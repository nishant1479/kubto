// Package estimator computes the aggregate resource shortfall that must be
// covered by new nodes, using Best Fit Decreasing (BFD) bin-packing and
// scheduling constraint validation.
package estimator

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ResourceDelta holds the total resource shortfall that cannot be satisfied
// by the cluster's current free capacity.
type ResourceDelta struct {
	// CPUMillicores is the CPU shortfall in milli-cores (0 when none).
	CPUMillicores int64
	// MemoryBytes is the memory shortfall in bytes (0 when none).
	MemoryBytes int64
}

// IsZero returns true when there is no shortfall in either dimension.
func (r ResourceDelta) IsZero() bool {
	return r.CPUMillicores == 0 && r.MemoryBytes == 0
}

// EstimationReport provides detailed explainability for capacity decisions.
type EstimationReport struct {
	// Delta is the aggregate shortfall in CPU millicores and memory bytes.
	Delta ResourceDelta

	// NodesNeeded is the number of new simulated nodes required to pack the pods.
	NodesNeeded int

	// PlacedPods maps pod name to the node it was placed on (existing node or simulated new node).
	PlacedPods map[string]string

	// InfeasiblePods maps pod name to the reason it could not be scheduled.
	InfeasiblePods map[string]string

	// Explanations holds human-readable step-by-step logs explaining the estimation.
	Explanations []string
}

// ResourceEstimator computes how much additional cluster capacity is needed
// to schedule all currently unschedulable pods.
type ResourceEstimator struct {
	// NodeCPUCapacityMillicores is the CPU assumed for each new worker node template (default 2000m).
	NodeCPUCapacityMillicores int64
	// NodeMemoryCapacityBytes is the memory assumed for each new worker node template (default 4 GiB).
	NodeMemoryCapacityBytes int64
	// NodeLabels are labels stamped on candidate worker nodes (e.g. node-role.kubernetes.io/worker).
	NodeLabels map[string]string
	// NodeTaints are taints present on candidate worker nodes.
	NodeTaints []corev1.Taint
}

// NewResourceEstimator returns a ready-to-use estimator with default template node settings.
func NewResourceEstimator() *ResourceEstimator {
	return &ResourceEstimator{
		NodeCPUCapacityMillicores: 2000,                   // 2 vCPU
		NodeMemoryCapacityBytes:   4 * 1024 * 1024 * 1024, // 4 GiB
		NodeLabels: map[string]string{
			utils.NodeRoleWorker: "",
		},
		NodeTaints: nil,
	}
}

// simulatedNode tracks available capacity on an existing or newly opened node.
type simulatedNode struct {
	name     string
	isNew    bool
	freeCPU  int64 // milli-cores
	freeMem  int64 // bytes
	labels   map[string]string
	taints   []corev1.Taint
}

// Estimate returns the resource shortfall for the given set of unschedulable pods.
// It implements Best Fit Decreasing (BFD) bin-packing against candidate nodes.
func (e *ResourceEstimator) Estimate(
	state *monitor.ClusterState,
	unschedulable []monitor.PodInfo,
) ResourceDelta {
	report := e.EstimateDetailed(state, unschedulable)
	return report.Delta
}

// EstimateDetailed performs constraint-aware Best Fit Decreasing (BFD) heuristic
// bin-packing and returns a full explainability report.
//
// BFD is a heuristic: it does not search for a globally optimal packing.
// Capacity accounting distinguishes three quantities:
//  1. initial free capacity on ready, schedulable existing nodes (snapshotted
//     before any simulated placement);
//  2. capacity consumed by placing pending pods onto those existing nodes;
//  3. remaining deficit — resources that had to be placed on new template nodes.
//
// Delta is the remaining deficit (2 is excluded; infeasible pods are excluded).
// NodesNeeded is the number of new template nodes the heuristic opened.
// Kubto currently models a single homogeneous worker template.
func (e *ResourceEstimator) EstimateDetailed(
	state *monitor.ClusterState,
	unschedulable []monitor.PodInfo,
) EstimationReport {
	report := EstimationReport{
		PlacedPods:     make(map[string]string),
		InfeasiblePods: make(map[string]string),
		Explanations:   nil,
	}

	if len(unschedulable) == 0 {
		report.Explanations = append(report.Explanations, "no unschedulable pods to evaluate")
		return report
	}

	// ── 1. Gather existing ready and schedulable nodes as candidate bins ──────
	var candidates []*simulatedNode
	if state != nil {
		for _, n := range state.GetNodes() {
			if !n.Ready || n.Unschedulable {
				continue
			}
			freeCPU := n.AllocatableCPU.MilliValue() - n.UsedCPU.MilliValue()
			if freeCPU < 0 {
				freeCPU = 0
			}
			freeMem := n.AllocatableMemory.Value() - n.UsedMemory.Value()
			if freeMem < 0 {
				freeMem = 0
			}
			candidates = append(candidates, &simulatedNode{
				name:    n.Name,
				isNew:   false,
				freeCPU: freeCPU,
				freeMem: freeMem,
				labels:  n.Labels,
				taints:  n.Taints,
			})
		}
	}

	// Snapshot initial free capacity BEFORE placement mutates the bins.
	var totalInitialFreeCPU, totalInitialFreeMem int64
	for _, c := range candidates {
		totalInitialFreeCPU += c.freeCPU
		totalInitialFreeMem += c.freeMem
	}
	report.Explanations = append(report.Explanations,
		fmt.Sprintf("initial free capacity across %d candidate node(s): %dm CPU, %d bytes RAM",
			len(candidates), totalInitialFreeCPU, totalInitialFreeMem))

	// ── 2. Filter and categorize unschedulable pods ──────────────────────────
	type podCandidate struct {
		info     monitor.PodInfo
		cpuMilli int64
		memBytes int64
		score    float64 // for descending sort
	}

	var feasible []podCandidate

	for _, p := range unschedulable {
		cpuReq := p.CPURequest.MilliValue()
		memReq := p.MemoryRequest.Value()

		// Check unsupported scheduling constraints (do not silently assume satisfied)
		if p.HasUnsupportedConstraints {
			reason := fmt.Sprintf("unsupported scheduling constraint (%s)", p.UnsupportedReason)
			report.InfeasiblePods[p.Name] = reason
			report.Explanations = append(report.Explanations, fmt.Sprintf("pod %s infeasible: %s", p.Name, reason))
			continue
		}

		// Zero-request pods: fit without taking resources
		if cpuReq == 0 && memReq == 0 {
			report.PlacedPods[p.Name] = "zero-request (no capacity required)"
			continue
		}

		// Calculate normalized score for sorting: max(CPU/templateCPU, Mem/templateMem)
		cpuScore := float64(cpuReq) / float64(e.NodeCPUCapacityMillicores)
		memScore := float64(memReq) / float64(e.NodeMemoryCapacityBytes)
		score := cpuScore
		if memScore > score {
			score = memScore
		}

		feasible = append(feasible, podCandidate{
			info:     p,
			cpuMilli: cpuReq,
			memBytes: memReq,
			score:    score,
		})
	}

	// ── 3. Sort feasible pods in Best Fit Decreasing order ───────────────────
	sort.Slice(feasible, func(i, j int) bool {
		return feasible[i].score > feasible[j].score
	})

	// ── 4. Bin-pack pods into existing nodes or simulate new nodes ──────────
	var openNewNodes []*simulatedNode
	var consumedExistingCPU, consumedExistingMem int64

	for _, pc := range feasible {
		p := pc.info

		// 4a. Try existing nodes first (Best Fit: node that leaves least remaining capacity)
		var bestExisting *simulatedNode
		var bestRemaining int64 = -1

		for _, node := range candidates {
			if !canPodFitConstraints(p, node.labels, node.taints) {
				continue
			}
			if node.freeCPU >= pc.cpuMilli && node.freeMem >= pc.memBytes {
				rem := (node.freeCPU - pc.cpuMilli) + ((node.freeMem - pc.memBytes) / (1024 * 1024))
				if bestRemaining == -1 || rem < bestRemaining {
					bestRemaining = rem
					bestExisting = node
				}
			}
		}

		if bestExisting != nil {
			bestExisting.freeCPU -= pc.cpuMilli
			bestExisting.freeMem -= pc.memBytes
			consumedExistingCPU += pc.cpuMilli
			consumedExistingMem += pc.memBytes
			report.PlacedPods[p.Name] = bestExisting.name
			report.Explanations = append(report.Explanations,
				fmt.Sprintf("pod %s fits on existing node %s (remaining free: %dm CPU, %dMiB RAM)",
					p.Name, bestExisting.name, bestExisting.freeCPU, bestExisting.freeMem/(1024*1024)))
			continue
		}

		// 4b. Try already opened new simulated nodes (Best Fit)
		var bestNew *simulatedNode
		bestRemaining = -1

		for _, node := range openNewNodes {
			if !canPodFitConstraints(p, node.labels, node.taints) {
				continue
			}
			if node.freeCPU >= pc.cpuMilli && node.freeMem >= pc.memBytes {
				rem := (node.freeCPU - pc.cpuMilli) + ((node.freeMem - pc.memBytes) / (1024 * 1024))
				if bestRemaining == -1 || rem < bestRemaining {
					bestRemaining = rem
					bestNew = node
				}
			}
		}

		if bestNew != nil {
			bestNew.freeCPU -= pc.cpuMilli
			bestNew.freeMem -= pc.memBytes
			report.PlacedPods[p.Name] = bestNew.name
			report.Explanations = append(report.Explanations,
				fmt.Sprintf("pod %s fits on simulated new node %s", p.Name, bestNew.name))
			continue
		}

		// 4c. Open a new simulated node only if the single worker template can fit the pod.
		if pc.cpuMilli > e.NodeCPUCapacityMillicores || pc.memBytes > e.NodeMemoryCapacityBytes {
			reason := fmt.Sprintf("pod requires %dm CPU / %d bytes RAM, exceeding the single worker template (%dm / %d bytes) and remaining existing-node free capacity",
				pc.cpuMilli, pc.memBytes, e.NodeCPUCapacityMillicores, e.NodeMemoryCapacityBytes)
			report.InfeasiblePods[p.Name] = reason
			report.Explanations = append(report.Explanations, fmt.Sprintf("pod %s infeasible: %s", p.Name, reason))
			continue
		}

		if !canPodFitConstraints(p, e.NodeLabels, e.NodeTaints) {
			reason := "pod constraints (nodeSelector or taints) cannot be satisfied by worker template"
			report.InfeasiblePods[p.Name] = reason
			report.Explanations = append(report.Explanations, fmt.Sprintf("pod %s infeasible on template node: %s", p.Name, reason))
			continue
		}

		newNodeName := fmt.Sprintf("simulated-node-%d", len(openNewNodes)+1)
		newNode := &simulatedNode{
			name:    newNodeName,
			isNew:   true,
			freeCPU: e.NodeCPUCapacityMillicores - pc.cpuMilli,
			freeMem: e.NodeMemoryCapacityBytes - pc.memBytes,
			labels:  e.NodeLabels,
			taints:  e.NodeTaints,
		}
		openNewNodes = append(openNewNodes, newNode)
		report.PlacedPods[p.Name] = newNodeName
		report.Explanations = append(report.Explanations,
			fmt.Sprintf("pod %s opened new simulated node %s", p.Name, newNodeName))
	}

	// ── 5. Remaining deficit = resources placed on new template nodes only ───
	// Pods packed onto existing nodes must not contribute to Delta.
	// Infeasible pods must not contribute to Delta (no endless scale-up).
	var cpuShortfall, memShortfall int64
	for _, n := range openNewNodes {
		cpuShortfall += e.NodeCPUCapacityMillicores - n.freeCPU
		memShortfall += e.NodeMemoryCapacityBytes - n.freeMem
	}

	report.Explanations = append(report.Explanations,
		fmt.Sprintf("placed %dm CPU / %d bytes onto existing nodes; remaining deficit %dm CPU / %d bytes on %d new template node(s)",
			consumedExistingCPU, consumedExistingMem, cpuShortfall, memShortfall, len(openNewNodes)))

	report.Delta = ResourceDelta{
		CPUMillicores: cpuShortfall,
		MemoryBytes:   memShortfall,
	}
	report.NodesNeeded = len(openNewNodes)

	return report
}

// canPodFitConstraints validates supported scheduling constraints:
// 1. nodeSelector matching
// 2. Taints and tolerations
func canPodFitConstraints(pod monitor.PodInfo, nodeLabels map[string]string, nodeTaints []corev1.Taint) bool {
	// 1. Check nodeSelector
	for k, v := range pod.NodeSelector {
		if nodeLabels == nil || nodeLabels[k] != v {
			return false
		}
	}

	// 2. Check taints against tolerations
	for _, taint := range nodeTaints {
		// Only NoSchedule and NoExecute block pod scheduling
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for _, tol := range pod.Tolerations {
			if toleratesTaint(tol, taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}

	return true
}

// toleratesTaint checks if a toleration matches a taint.
func toleratesTaint(tol corev1.Toleration, taint corev1.Taint) bool {
	if tol.Key != "" && tol.Key != taint.Key {
		return false
	}
	if tol.Effect != "" && tol.Effect != taint.Effect {
		return false
	}
	switch tol.Operator {
	case corev1.TolerationOpExists, "":
		return true
	case corev1.TolerationOpEqual:
		return tol.Value == taint.Value
	default:
		return false
	}
}

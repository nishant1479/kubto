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

// EstimateDetailed performs constraint-aware Best Fit Decreasing (BFD) bin-packing
// and returns a full explainability report.
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

	// ── 2. Filter and categorize unschedulable pods ──────────────────────────
	type podCandidate struct {
		info     monitor.PodInfo
		cpuMilli int64
		memBytes int64
		score    float64 // for descending sort
	}

	var feasible []podCandidate
	var oversizedShortfallCPU int64
	var oversizedShortfallMem int64

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

		// Check if pod is larger than any possible template node
		if cpuReq > e.NodeCPUCapacityMillicores || memReq > e.NodeMemoryCapacityBytes {
			// Can it fit on any existing node with larger allocatable capacity?
			canFitExisting := false
			for _, c := range candidates {
				if cpuReq <= c.freeCPU && memReq <= c.freeMem {
					canFitExisting = true
					break
				}
			}
			if !canFitExisting {
				reason := fmt.Sprintf("pod requires %dm CPU / %d bytes RAM, exceeding template node capacity (%dm / %d bytes)",
					cpuReq, memReq, e.NodeCPUCapacityMillicores, e.NodeMemoryCapacityBytes)
				report.InfeasiblePods[p.Name] = reason
				report.Explanations = append(report.Explanations, fmt.Sprintf("pod %s oversized: %s", p.Name, reason))
				// Track shortfall for oversized pod so scale-up decision knows demand exists
				oversizedShortfallCPU += cpuReq
				oversizedShortfallMem += memReq
				continue
			}
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

	for _, pc := range feasible {
		p := pc.info
		placed := false

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
			report.PlacedPods[p.Name] = bestExisting.name
			report.Explanations = append(report.Explanations,
				fmt.Sprintf("pod %s fits on existing node %s (remaining free: %dm CPU, %dMiB RAM)",
					p.Name, bestExisting.name, bestExisting.freeCPU, bestExisting.freeMem/(1024*1024)))
			placed = true
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
			placed = true
			continue
		}

		// 4c. Open a new simulated node
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
		placed = true
		_ = placed
	}

	// Track initial available candidate capacity before bin-packing
	var totalInitialFreeCPU, totalInitialFreeMem int64
	for _, c := range candidates {
		totalInitialFreeCPU += c.freeCPU
		totalInitialFreeMem += c.freeMem
	}

	// ── 5. Compute shortfall ────────────────────────────────────────────────
	var totalReqCPU, totalReqMem int64
	for _, pc := range feasible {
		totalReqCPU += pc.cpuMilli
		totalReqMem += pc.memBytes
	}
	totalReqCPU += oversizedShortfallCPU
	totalReqMem += oversizedShortfallMem

	var cpuShortfall, memShortfall int64
	if len(openNewNodes) > 0 {
		cpuShortfall = totalReqCPU - totalInitialFreeCPU
		if cpuShortfall < 0 {
			cpuShortfall = 0
		}
		memShortfall = totalReqMem - totalInitialFreeMem
		if memShortfall < 0 {
			memShortfall = 0
		}

		// If fragmented capacity required new simulated nodes despite aggregate free capacity,
		// ensure the shortfall reflects the demand placed on new simulated nodes.
		if cpuShortfall == 0 && memShortfall == 0 {
			for _, n := range openNewNodes {
				cpuShortfall += (e.NodeCPUCapacityMillicores - n.freeCPU)
				memShortfall += (e.NodeMemoryCapacityBytes - n.freeMem)
			}
		}
	} else if oversizedShortfallCPU > 0 || oversizedShortfallMem > 0 {
		cpuShortfall = oversizedShortfallCPU - totalInitialFreeCPU
		if cpuShortfall < 0 {
			cpuShortfall = 0
		}
		memShortfall = oversizedShortfallMem - totalInitialFreeMem
		if memShortfall < 0 {
			memShortfall = 0
		}
	}

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

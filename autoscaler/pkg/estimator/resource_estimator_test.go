// Package estimator — deterministic unit tests for ResourceEstimator.
package estimator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func mCPU(m int64) resource.Quantity { return *resource.NewMilliQuantity(m, resource.DecimalSI) }
func miB(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

// makeState builds a ClusterState with the given ready nodes and no pods.
func makeState(nodes []monitor.NodeInfo) *monitor.ClusterState {
	cs := monitor.NewClusterState()
	cs.SetNodes(nodes)
	return cs
}

// readyNode returns a NodeInfo that is Ready, schedulable, and has the given
// allocatable capacity and zero used resources.
func readyNode(name string, cpuM int64, memMiB int64) monitor.NodeInfo {
	return monitor.NodeInfo{
		Name:              name,
		AllocatableCPU:    mCPU(cpuM),
		AllocatableMemory: miB(memMiB),
		UsedCPU:           mCPU(0),
		UsedMemory:        miB(0),
		Ready:             true,
		Unschedulable:     false,
	}
}

// busyNode returns a node with already-allocated resources.
func busyNode(name string, cpuAllocM, memAllocMiB, cpuUsedM, memUsedMiB int64) monitor.NodeInfo {
	n := readyNode(name, cpuAllocM, memAllocMiB)
	n.UsedCPU = mCPU(cpuUsedM)
	n.UsedMemory = miB(memUsedMiB)
	return n
}

// pendingPod creates a PodInfo that looks like an unschedulable pod.
func pendingPod(name string, cpuM int64, memMiB int64) monitor.PodInfo {
	return monitor.PodInfo{
		Name:             name,
		Namespace:        "default",
		CPURequest:       mCPU(cpuM),
		MemoryRequest:    miB(memMiB),
		Status:           "Pending",
		ConditionReason:  "Unschedulable",
		ConditionMessage: "Insufficient cpu",
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

func TestEstimate_OnePodFitsOneNode(t *testing.T) {
	// 2 vCPU node, 1 vCPU pod — pod fits, no shortfall.
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096)})
	pods := []monitor.PodInfo{pendingPod("p1", 1000, 512)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.Delta.CPUMillicores != 0 || report.Delta.MemoryBytes != 0 {
		t.Errorf("expected zero shortfall, got cpu=%dm mem=%dB", report.Delta.CPUMillicores, report.Delta.MemoryBytes)
	}
	if report.NodesNeeded != 0 {
		t.Errorf("expected NodesNeeded=0, got %d", report.NodesNeeded)
	}
	if report.PlacedPods["p1"] != "n1" {
		t.Errorf("expected p1 on n1, got %q", report.PlacedPods["p1"])
	}
	if len(report.InfeasiblePods) != 0 {
		t.Errorf("unexpected infeasible: %v", report.InfeasiblePods)
	}
}

func TestEstimate_SeveralPodsFitOneNode(t *testing.T) {
	// 4 vCPU / 8 GiB node; two 1 vCPU / 1 GiB pods → fits, no shortfall.
	state := makeState([]monitor.NodeInfo{readyNode("n1", 4000, 8192)})
	pods := []monitor.PodInfo{
		pendingPod("p1", 1000, 1024),
		pendingPod("p2", 1000, 1024),
	}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if delta.CPUMillicores != 0 || delta.MemoryBytes != 0 {
		t.Errorf("expected zero shortfall, got cpu=%dm mem=%dB", delta.CPUMillicores, delta.MemoryBytes)
	}
}

func TestEstimate_PodsRequireMultipleNodes(t *testing.T) {
	// Node has 2000m free. Two 1500m pods: one packs on n1, one needs a new node.
	// Remaining deficit must be 1500m (the unpacked pod), not totalReq - leftover
	// free after mutation (which would incorrectly be 2500m).
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 8192)})
	pods := []monitor.PodInfo{
		pendingPod("p1", 1500, 512),
		pendingPod("p2", 1500, 512),
	}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 1 {
		t.Errorf("expected NodesNeeded=1, got %d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 1500 {
		t.Errorf("expected remaining CPU deficit 1500m, got %d", report.Delta.CPUMillicores)
	}
	if report.Delta.MemoryBytes != 512*1024*1024 {
		t.Errorf("expected remaining memory deficit 512MiB, got %d", report.Delta.MemoryBytes)
	}
}

func TestEstimate_CPUBound(t *testing.T) {
	// Node has plenty of memory but only 500m CPU free, so the 1000m pod cannot
	// pack locally. Remaining deficit is the pod's full request on a new node.
	state := makeState([]monitor.NodeInfo{busyNode("n1", 2000, 4096, 1500, 256)})
	pods := []monitor.PodInfo{pendingPod("p1", 1000, 128)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 1 {
		t.Errorf("expected NodesNeeded=1, got %d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 1000 {
		t.Errorf("expected CPU deficit 1000m, got %d", report.Delta.CPUMillicores)
	}
	if report.Delta.MemoryBytes != 128*1024*1024 {
		t.Errorf("expected memory deficit 128MiB on the new node, got %d", report.Delta.MemoryBytes)
	}
}

func TestEstimate_MemoryBound(t *testing.T) {
	// Node has plenty of CPU but only 128 MiB memory free, so the 512 MiB pod
	// cannot pack locally. Remaining deficit is the pod's full request.
	state := makeState([]monitor.NodeInfo{busyNode("n1", 4000, 2048, 100, 1920)})
	pods := []monitor.PodInfo{pendingPod("p1", 100, 512)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 1 {
		t.Errorf("expected NodesNeeded=1, got %d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 100 {
		t.Errorf("expected CPU deficit 100m (placed on new node), got %d", report.Delta.CPUMillicores)
	}
	if report.Delta.MemoryBytes != 512*1024*1024 {
		t.Errorf("expected memory deficit 512MiB, got %d", report.Delta.MemoryBytes)
	}
}

func TestEstimate_PodLargerThanAnyNode(t *testing.T) {
	// Pod requests 8 vCPU; existing node and the single 2 vCPU template cannot fit it.
	// Must be infeasible with zero Delta so the controller does not provision forever.
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096)})
	pods := []monitor.PodInfo{pendingPod("p1", 8000, 512)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.InfeasiblePods["p1"] == "" {
		t.Fatal("oversized pod must be marked infeasible")
	}
	if report.NodesNeeded != 0 {
		t.Errorf("expected NodesNeeded=0, got %d", report.NodesNeeded)
	}
	if !report.Delta.IsZero() {
		t.Errorf("infeasible pod must not contribute to Delta, got %+v", report.Delta)
	}
}

func TestEstimate_ExistingSpareCapacity(t *testing.T) {
	// Large node, tiny pod — there is already enough capacity.
	state := makeState([]monitor.NodeInfo{readyNode("n1", 8000, 16384)})
	pods := []monitor.PodInfo{pendingPod("p1", 100, 64)}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if !delta.IsZero() {
		t.Errorf("expected zero shortfall when capacity is ample, got %+v", delta)
	}
}

func TestEstimate_ZeroResourceRequests(t *testing.T) {
	// Pod with no resource requests should produce zero shortfall.
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096)})
	pods := []monitor.PodInfo{
		{
			Name:             "no-req",
			Namespace:        "default",
			Status:           "Pending",
			ConditionReason:  "Unschedulable",
			ConditionMessage: "Insufficient cpu",
		},
	}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if !delta.IsZero() {
		t.Errorf("expected zero shortfall for zero-request pod, got %+v", delta)
	}
}

func TestEstimate_NoPods(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096)})
	e := NewResourceEstimator()
	delta := e.Estimate(state, nil)
	if !delta.IsZero() {
		t.Errorf("expected zero shortfall for empty pod list, got %+v", delta)
	}
}

func TestEstimate_NoNodes(t *testing.T) {
	// No nodes at all → entire pod request is a shortfall.
	state := makeState(nil)
	pods := []monitor.PodInfo{pendingPod("p1", 1000, 512)}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	// Should report the full pod request as shortfall.
	if delta.CPUMillicores != 1000 {
		t.Errorf("expected 1000m CPU shortfall, got %d", delta.CPUMillicores)
	}
	if delta.MemoryBytes != 512*1024*1024 {
		t.Errorf("expected 512MiB memory shortfall, got %d", delta.MemoryBytes)
	}
}

func TestEstimate_NotReadyNodesNotCounted(t *testing.T) {
	// One not-ready node with large capacity; capacity must NOT be counted.
	notReady := monitor.NodeInfo{
		Name:              "notready",
		AllocatableCPU:    mCPU(8000),
		AllocatableMemory: miB(16384),
		Ready:             false, // <-- not ready
	}
	state := makeState([]monitor.NodeInfo{notReady})
	pods := []monitor.PodInfo{pendingPod("p1", 500, 256)}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if delta.CPUMillicores <= 0 {
		t.Errorf("expected shortfall because node is not ready, got %d", delta.CPUMillicores)
	}
}

func TestEstimate_UnschedulableNodesNotCounted(t *testing.T) {
	// A cordoned (unschedulable) node must not contribute capacity.
	cordoned := monitor.NodeInfo{
		Name:              "cordoned",
		AllocatableCPU:    mCPU(8000),
		AllocatableMemory: miB(16384),
		Ready:             true,
		Unschedulable:     true, // <-- cordoned
	}
	state := makeState([]monitor.NodeInfo{cordoned})
	pods := []monitor.PodInfo{pendingPod("p1", 500, 256)}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if delta.CPUMillicores <= 0 {
		t.Errorf("expected shortfall because node is cordoned, got %d", delta.CPUMillicores)
	}
}

func TestEstimate_AllocatableVsCapacity(t *testing.T) {
	// Node has 4 vCPU total but only 2 vCPU allocatable (system overhead).
	// Pod wants 2.5 vCPU → should be a shortfall based on allocatable (2000m), not capacity.
	n := readyNode("n1", 2000, 4096) // 2000m allocatable
	state := makeState([]monitor.NodeInfo{n})
	pods := []monitor.PodInfo{pendingPod("p1", 2500, 256)} // 2500m request

	e := NewResourceEstimator()
	e.NodeCPUCapacityMillicores = 4000 // worker template large enough to host the pod
	delta := e.Estimate(state, pods)

	if delta.CPUMillicores <= 0 {
		t.Errorf("expected CPU shortfall based on allocatable, got %d", delta.CPUMillicores)
	}
}

func TestEstimate_MultipleNodesTotalCapacity(t *testing.T) {
	// Two nodes each with 2 vCPU → 4 vCPU total free.
	// Two pods each requesting 1.5 vCPU → 3 vCPU needed → fits.
	state := makeState([]monitor.NodeInfo{
		readyNode("n1", 2000, 4096),
		readyNode("n2", 2000, 4096),
	})
	pods := []monitor.PodInfo{
		pendingPod("p1", 1500, 256),
		pendingPod("p2", 1500, 256),
	}

	e := NewResourceEstimator()
	delta := e.Estimate(state, pods)

	if delta.CPUMillicores != 0 || delta.MemoryBytes != 0 {
		t.Errorf("expected zero shortfall with two nodes, got %+v", delta)
	}
}

func TestEstimate_ExistingUsedResourcesDeducted(t *testing.T) {
	// Node has 4 vCPU allocatable, 3.5 vCPU already used (500m free).
	// Pod requests 600m → cannot pack locally; remaining deficit is 600m.
	state := makeState([]monitor.NodeInfo{busyNode("n1", 4000, 8192, 3500, 512)})
	pods := []monitor.PodInfo{pendingPod("p1", 600, 256)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 1 {
		t.Errorf("expected NodesNeeded=1, got %d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 600 {
		t.Errorf("expected remaining CPU deficit 600m, got %d", report.Delta.CPUMillicores)
	}
}

// ─── IsZero ───────────────────────────────────────────────────────────────────

func TestResourceDelta_IsZero(t *testing.T) {
	if !(ResourceDelta{}).IsZero() {
		t.Error("zero ResourceDelta should be IsZero()")
	}
	if (ResourceDelta{CPUMillicores: 1}).IsZero() {
		t.Error("non-zero CPU delta should not be IsZero()")
	}
	if (ResourceDelta{MemoryBytes: 1}).IsZero() {
		t.Error("non-zero memory delta should not be IsZero()")
	}
}

// ─── BFD Bin-Packing Tests ───────────────────────────────────────────────────

func TestEstimate_BFDBinPacking_MultiPodPack(t *testing.T) {
	// Template node has 2000m CPU.
	// 3 pods of 600m each = 1800m total. They should fit on 1 new simulated node (BFD).
	e := NewResourceEstimator() // 2000m node template
	state := makeState(nil)     // empty cluster
	pods := []monitor.PodInfo{
		pendingPod("p1", 600, 256),
		pendingPod("p2", 600, 256),
		pendingPod("p3", 600, 256),
	}

	report := e.EstimateDetailed(state, pods)
	if report.NodesNeeded != 1 {
		t.Errorf("expected 3x 600m pods to pack into 1 simulated node, got %d", report.NodesNeeded)
	}

	// 4 pods of 600m each = 2400m total. Cannot fit on 1x 2000m node, needs 2 nodes.
	pods4 := append(pods, pendingPod("p4", 600, 256))
	report4 := e.EstimateDetailed(state, pods4)
	if report4.NodesNeeded != 2 {
		t.Errorf("expected 4x 600m pods to pack into 2 simulated nodes, got %d", report4.NodesNeeded)
	}
}

// ─── Scheduling Constraints Tests ─────────────────────────────────────────────

func TestEstimate_NodeSelector_MatchAndMismatch(t *testing.T) {
	e := NewResourceEstimator()

	// Existing node has label zone=us-east-1 and 2000m free CPU
	node := readyNode("n1", 2000, 4096)
	node.Labels = map[string]string{"zone": "us-east-1"}
	state := makeState([]monitor.NodeInfo{node})

	// Pod matching the node's label fits on existing node
	matchingPod := pendingPod("p-match", 1000, 512)
	matchingPod.NodeSelector = map[string]string{"zone": "us-east-1"}

	reportMatch := e.EstimateDetailed(state, []monitor.PodInfo{matchingPod})
	if reportMatch.NodesNeeded != 0 {
		t.Errorf("pod matching nodeSelector should fit on existing node, needed %d nodes", reportMatch.NodesNeeded)
	}
	if reportMatch.PlacedPods["p-match"] != "n1" {
		t.Errorf("expected p-match placed on n1, got %q", reportMatch.PlacedPods["p-match"])
	}

	// Pod with mismatching label zone=us-west-1 CANNOT fit on n1
	mismatchPod := pendingPod("p-mismatch", 1000, 512)
	mismatchPod.NodeSelector = map[string]string{"zone": "us-west-1"}

	reportMismatch := e.EstimateDetailed(state, []monitor.PodInfo{mismatchPod})
	// Template node does not have zone=us-west-1, so it should be infeasible
	if reportMismatch.InfeasiblePods["p-mismatch"] == "" {
		t.Error("pod with mismatching nodeSelector against template should be recorded as infeasible")
	}
}

func TestEstimate_TaintAndToleration(t *testing.T) {
	e := NewResourceEstimator()

	// Existing node has taint dedicated=special:NoSchedule
	node := readyNode("n1", 2000, 4096)
	node.Taints = []corev1.Taint{
		{Key: "dedicated", Value: "special", Effect: corev1.TaintEffectNoSchedule},
	}
	state := makeState([]monitor.NodeInfo{node})

	// Pod without toleration cannot fit on tainted node
	untaintedPod := pendingPod("p-no-tol", 1000, 512)
	reportNoTol := e.EstimateDetailed(state, []monitor.PodInfo{untaintedPod})
	if reportNoTol.PlacedPods["p-no-tol"] == "n1" {
		t.Error("pod without toleration should NOT be placed on tainted node n1")
	}

	// Pod with toleration CAN fit on tainted node
	toleratingPod := pendingPod("p-tol", 1000, 512)
	toleratingPod.Tolerations = []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "special", Effect: corev1.TaintEffectNoSchedule},
	}
	reportTol := e.EstimateDetailed(state, []monitor.PodInfo{toleratingPod})
	if reportTol.PlacedPods["p-tol"] != "n1" {
		t.Errorf("pod with matching toleration should be placed on n1, got %q", reportTol.PlacedPods["p-tol"])
	}
}

func TestEstimate_UnsupportedConstraints_ReportedInfeasible(t *testing.T) {
	e := NewResourceEstimator()
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096)})

	pod := pendingPod("p-unsupported", 500, 256)
	pod.HasUnsupportedConstraints = true
	pod.UnsupportedReason = "podAntiAffinity is unsupported"

	report := e.EstimateDetailed(state, []monitor.PodInfo{pod})
	if report.InfeasiblePods["p-unsupported"] == "" {
		t.Error("pod with unsupported constraints should be marked infeasible")
	}
	// Delta should be zero so controller does not falsely scale up for unsupported pods
	if !report.Delta.IsZero() {
		t.Errorf("shortfall should be zero for unsupported pod, got %+v", report.Delta)
	}
}

func TestEstimateDetailed_ExplainabilityReport(t *testing.T) {
	e := NewResourceEstimator()
	state := makeState([]monitor.NodeInfo{readyNode("n1", 1000, 2048)})

	pods := []monitor.PodInfo{
		pendingPod("p-fits", 500, 256),    // fits on n1
		pendingPod("p-needs-new", 800, 256), // opens simulated new node
	}

	report := e.EstimateDetailed(state, pods)
	// BFD sorts larger pods first: p-needs-new (800m) is placed on n1 (1000m free),
	// leaving 200m free; then p-fits (500m) opens simulated-node-1.
	if report.PlacedPods["p-needs-new"] != "n1" {
		t.Errorf("p-needs-new should be placed on n1, got %q", report.PlacedPods["p-needs-new"])
	}
	if report.PlacedPods["p-fits"] != "simulated-node-1" {
		t.Errorf("p-fits should be placed on simulated-node-1, got %q", report.PlacedPods["p-fits"])
	}
	if len(report.Explanations) == 0 {
		t.Error("Explanations should contain step-by-step reasoning")
	}
}

func TestEstimate_SeveralPodsFitAcrossExistingNodes_NoOverProvision(t *testing.T) {
	state := makeState([]monitor.NodeInfo{
		readyNode("n1", 2000, 4096),
		readyNode("n2", 2000, 4096),
	})
	pods := []monitor.PodInfo{
		pendingPod("p1", 1800, 256),
		pendingPod("p2", 1800, 256),
		pendingPod("p3", 200, 128),
	}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 0 {
		t.Errorf("pods should pack on existing nodes, NodesNeeded=%d", report.NodesNeeded)
	}
	if !report.Delta.IsZero() {
		t.Errorf("expected zero remaining deficit, got %+v", report.Delta)
	}
	if len(report.PlacedPods) != 3 {
		t.Errorf("expected 3 placed pods, got %v", report.PlacedPods)
	}
}

func TestEstimate_SchedulingIncompatibleNodeNotUsable(t *testing.T) {
	node := readyNode("tainted", 8000, 16384)
	node.Taints = []corev1.Taint{
		{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
	}
	state := makeState([]monitor.NodeInfo{node})
	pod := pendingPod("p1", 500, 256) // no toleration

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, []monitor.PodInfo{pod})

	if report.PlacedPods["p1"] == "tainted" {
		t.Fatal("incompatible node must not contribute usable capacity")
	}
	if report.NodesNeeded != 1 {
		t.Errorf("pod should open a template node, NodesNeeded=%d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 500 {
		t.Errorf("expected 500m remaining deficit, got %d", report.Delta.CPUMillicores)
	}
}

func TestEstimate_OversizedFitsLargerExistingNode(t *testing.T) {
	// Template is 2000m, but an existing node has 8000m free. The 5000m pod
	// must pack locally and must not be marked infeasible.
	state := makeState([]monitor.NodeInfo{readyNode("big", 8000, 16384)})
	pods := []monitor.PodInfo{pendingPod("p-big", 5000, 512)}

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.PlacedPods["p-big"] != "big" {
		t.Errorf("expected placement on existing large node, got %q", report.PlacedPods["p-big"])
	}
	if report.InfeasiblePods["p-big"] != "" {
		t.Errorf("pod that fits an existing node must not be infeasible: %s", report.InfeasiblePods["p-big"])
	}
	if report.NodesNeeded != 0 || !report.Delta.IsZero() {
		t.Errorf("no scale-up: NodesNeeded=%d delta=%+v", report.NodesNeeded, report.Delta)
	}
}

func TestEstimate_MixedFeasibleAndInfeasible(t *testing.T) {
	state := makeState(nil)
	ok := pendingPod("ok", 1000, 256)
	bad := pendingPod("bad", 9000, 256)
	unsupported := pendingPod("anti", 500, 256)
	unsupported.HasUnsupportedConstraints = true
	unsupported.UnsupportedReason = "podAntiAffinity is unsupported"

	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, []monitor.PodInfo{ok, bad, unsupported})

	if report.NodesNeeded != 1 {
		t.Errorf("only the feasible pod should open a node, NodesNeeded=%d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 1000 {
		t.Errorf("Delta must equal feasible remaining demand 1000m, got %d", report.Delta.CPUMillicores)
	}
	if report.InfeasiblePods["bad"] == "" || report.InfeasiblePods["anti"] == "" {
		t.Errorf("both infeasible pods must be recorded: %v", report.InfeasiblePods)
	}
	if report.PlacedPods["ok"] == "" {
		t.Error("feasible pod must be placed")
	}
}

func TestEstimate_EmptyClusterExactDeficit(t *testing.T) {
	state := makeState(nil)
	pods := []monitor.PodInfo{
		pendingPod("a", 600, 256),
		pendingPod("b", 600, 256),
		pendingPod("c", 600, 256),
	}
	e := NewResourceEstimator()
	report := e.EstimateDetailed(state, pods)

	if report.NodesNeeded != 1 {
		t.Errorf("3x600m should pack on one 2000m template node, got %d", report.NodesNeeded)
	}
	if report.Delta.CPUMillicores != 1800 {
		t.Errorf("Delta CPU want 1800, got %d", report.Delta.CPUMillicores)
	}
	if report.Delta.MemoryBytes != 3*256*1024*1024 {
		t.Errorf("Delta mem want 768MiB, got %d", report.Delta.MemoryBytes)
	}
	if len(report.InfeasiblePods) != 0 {
		t.Errorf("unexpected infeasible: %v", report.InfeasiblePods)
	}
}

// Package decision — unit tests for ThresholdPolicy and DecisionEngine.
package decision

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kuber-scale/autoscaler/pkg/estimator"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func mCPU(m int64) resource.Quantity { return *resource.NewMilliQuantity(m, resource.DecimalSI) }
func miB(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

func makeState(nodes []monitor.NodeInfo) *monitor.ClusterState {
	cs := monitor.NewClusterState()
	cs.SetNodes(nodes)
	return cs
}

func readyNode(name string, cpuAllocM, memAllocMiB, cpuUsedM, memUsedMiB int64, age time.Duration) monitor.NodeInfo {
	return monitor.NodeInfo{
		Name:              name,
		AllocatableCPU:    mCPU(cpuAllocM),
		AllocatableMemory: miB(memAllocMiB),
		UsedCPU:           mCPU(cpuUsedM),
		UsedMemory:        miB(memUsedMiB),
		Ready:             true,
		Unschedulable:     false,
		CreatedAt:         time.Now().Add(-age),
	}
}

// noCriticalPods is a criticalPodFn that always returns false (no critical pods).
func noCriticalPods(_ string) bool { return false }

// allCriticalPods is a criticalPodFn that always returns true.
func allCriticalPods(_ string) bool { return true }

// policy builds a ThresholdPolicy with the given min/max nodes and the noCriticalPods checker.
func policy(min, max int) *ThresholdPolicy {
	p := DefaultThresholdPolicy(noCriticalPods)
	p.MinNodes = min
	p.MaxNodes = max
	return p
}

// ─── Scale-up tests ───────────────────────────────────────────────────────────

func TestThresholdPolicy_ScaleUp_CPUShortfall(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	// Shortfall exceeds default threshold (500m).
	delta := estimator.ResourceDelta{CPUMillicores: 1500, MemoryBytes: 0}

	p := DefaultThresholdPolicy(noCriticalPods)
	dec := p.Evaluate(delta, state)

	if !dec.ScaleUp {
		t.Fatalf("expected ScaleUp=true, got %+v", dec)
	}
	if dec.Count < 1 {
		t.Errorf("expected Count >= 1, got %d", dec.Count)
	}
}

func TestThresholdPolicy_ScaleUp_MemoryShortfall(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	// Memory shortfall exceeds default threshold (256 MiB).
	delta := estimator.ResourceDelta{CPUMillicores: 0, MemoryBytes: 512 * 1024 * 1024}

	p := DefaultThresholdPolicy(noCriticalPods)
	dec := p.Evaluate(delta, state)

	if !dec.ScaleUp {
		t.Fatalf("expected ScaleUp=true for memory shortfall, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleUp_BelowThreshold_NoOp(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	// Shortfall is below the threshold — should be a no-op.
	delta := estimator.ResourceDelta{CPUMillicores: 100, MemoryBytes: 0}

	p := DefaultThresholdPolicy(noCriticalPods)
	dec := p.Evaluate(delta, state)

	if dec.ScaleUp {
		t.Fatalf("expected no scale-up for sub-threshold shortfall, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleUp_AtMaxNodes_NoOp(t *testing.T) {
	// Already at MaxNodes — no further scale-up allowed.
	nodes := make([]monitor.NodeInfo, 5)
	for i := range nodes {
		nodes[i] = readyNode("n", int64(i), 2000, 4096, 0, 10*time.Minute)
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{CPUMillicores: 2000, MemoryBytes: 0}

	p := policy(1, 5) // maxNodes=5, already 5 nodes
	dec := p.Evaluate(delta, state)

	if dec.ScaleUp {
		t.Fatalf("expected no scale-up at MaxNodes, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleUp_CountCappedByHeadroom(t *testing.T) {
	// 4 nodes, max 5 → only 1 node of headroom.
	nodes := []monitor.NodeInfo{
		readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute),
		readyNode("n2", 2000, 4096, 0, 0, 10*time.Minute),
		readyNode("n3", 2000, 4096, 0, 0, 10*time.Minute),
		readyNode("n4", 2000, 4096, 0, 0, 10*time.Minute),
	}
	state := makeState(nodes)
	// Shortfall of 10 vCPU → would normally want 5 nodes, but max headroom is 1.
	delta := estimator.ResourceDelta{CPUMillicores: 10000, MemoryBytes: 0}

	p := policy(1, 5)
	dec := p.Evaluate(delta, state)

	if !dec.ScaleUp {
		t.Fatalf("expected ScaleUp=true, got %+v", dec)
	}
	if dec.Count != 1 {
		t.Errorf("expected Count=1 (capped by headroom), got %d", dec.Count)
	}
}

func TestThresholdPolicy_ScaleUp_ZeroDelta_NoOp(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	delta := estimator.ResourceDelta{} // zero

	p := DefaultThresholdPolicy(noCriticalPods)
	dec := p.Evaluate(delta, state)

	if dec.ScaleUp {
		t.Fatalf("expected no scale-up for zero delta, got %+v", dec)
	}
}

// ─── Scale-down tests ─────────────────────────────────────────────────────────

func TestThresholdPolicy_ScaleDown_IdleNode(t *testing.T) {
	// One idle node (0% CPU), old enough, no critical pods, 2 nodes total.
	nodes := []monitor.NodeInfo{
		readyNode("busy", 2000, 4096, 1900, 0, 10*time.Minute),  // >20% util → not candidate
		readyNode("idle", 2000, 4096, 0, 0, 10*time.Minute),      // 0% util, old → candidate
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{} // no pending pods

	p := policy(1, 5)
	dec := p.Evaluate(delta, state)

	if !dec.ScaleDown {
		t.Fatalf("expected ScaleDown=true for idle node, got %+v", dec)
	}
	if dec.NodeName != "idle" {
		t.Errorf("expected NodeName=idle, got %q", dec.NodeName)
	}
}

func TestThresholdPolicy_ScaleDown_AtMinNodes_NoOp(t *testing.T) {
	// Only 1 node, MinNodes=1 → scale-down blocked.
	state := makeState([]monitor.NodeInfo{
		readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute),
	})
	delta := estimator.ResourceDelta{}

	p := policy(1, 5)
	dec := p.Evaluate(delta, state)

	if dec.ScaleDown {
		t.Fatalf("expected no scale-down at MinNodes, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleDown_CriticalPods_NoOp(t *testing.T) {
	// Two nodes; idle node has critical pods → must not be selected.
	nodes := []monitor.NodeInfo{
		readyNode("busy", 2000, 4096, 1900, 0, 10*time.Minute),
		readyNode("idle", 2000, 4096, 0, 0, 10*time.Minute),
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{}

	p := policy(1, 5)
	p.criticalPodChecker = allCriticalPods // every node has critical pods

	dec := p.Evaluate(delta, state)
	if dec.ScaleDown {
		t.Fatalf("expected no scale-down when critical pods exist, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleDown_TooYoung_NoOp(t *testing.T) {
	// Idle node is younger than cooldown period.
	nodes := []monitor.NodeInfo{
		readyNode("busy", 2000, 4096, 1900, 0, 10*time.Minute),
		readyNode("young-idle", 2000, 4096, 0, 0, 10*time.Second), // too young
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{}

	p := policy(1, 5)
	p.CooldownPeriod = 60 * time.Second // node age must exceed this

	dec := p.Evaluate(delta, state)
	if dec.ScaleDown {
		t.Fatalf("expected no scale-down for too-young node, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleDown_HighUtil_NoOp(t *testing.T) {
	// All nodes have CPU utilisation >= 20%.
	nodes := []monitor.NodeInfo{
		readyNode("n1", 2000, 4096, 500, 0, 10*time.Minute), // 25% util — above threshold
		readyNode("n2", 2000, 4096, 600, 0, 10*time.Minute), // 30% util
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{}

	p := policy(1, 5)
	dec := p.Evaluate(delta, state)

	if dec.ScaleDown {
		t.Fatalf("expected no scale-down when all nodes are utilized, got %+v", dec)
	}
}

func TestThresholdPolicy_ScaleDown_Cordoned_Skipped(t *testing.T) {
	// A cordoned node should not be picked for scale-down by the policy
	// (it is already being removed).
	cordoned := monitor.NodeInfo{
		Name:              "cordoned",
		AllocatableCPU:    mCPU(2000),
		AllocatableMemory: miB(4096),
		UsedCPU:           mCPU(0),
		UsedMemory:        miB(0),
		Ready:             true,
		Unschedulable:     true, // cordoned
		CreatedAt:         time.Now().Add(-10 * time.Minute),
	}
	nodes := []monitor.NodeInfo{
		readyNode("busy", 2000, 4096, 1900, 0, 10*time.Minute),
		cordoned,
	}
	state := makeState(nodes)
	delta := estimator.ResourceDelta{}

	p := policy(1, 5)
	dec := p.Evaluate(delta, state)

	if dec.ScaleDown && dec.NodeName == "cordoned" {
		t.Error("cordoned node must not be selected for scale-down")
	}
}

// ─── CeilDiv helper ───────────────────────────────────────────────────────────

func TestCeilDiv(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{0, 2, 0},
		{1, 2, 1},
		{2, 2, 1},
		{3, 2, 2},
		{4, 2, 2},
		{5, 2, 3},
		{0, 0, 0}, // zero divisor
	}
	for _, c := range cases {
		got := ceilDiv(c.a, c.b)
		if got != c.want {
			t.Errorf("ceilDiv(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ─── HasCriticalPods ──────────────────────────────────────────────────────────

func TestThresholdPolicy_HasCriticalPods_NilChecker(t *testing.T) {
	p := &ThresholdPolicy{} // nil criticalPodChecker
	if p.HasCriticalPods("any-node") {
		t.Error("nil checker should return false")
	}
}

func TestThresholdPolicy_HasCriticalPods_Injected(t *testing.T) {
	called := false
	p := DefaultThresholdPolicy(func(name string) bool {
		called = true
		return name == "special"
	})
	if !p.HasCriticalPods("special") {
		t.Error("expected HasCriticalPods=true for special node")
	}
	if !called {
		t.Error("criticalPodChecker was never called")
	}
}

// ─── DecisionEngine ───────────────────────────────────────────────────────────

func TestDecisionEngine_Decide_ScaleUp(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	delta := estimator.ResourceDelta{CPUMillicores: 1500, MemoryBytes: 0}

	p := DefaultThresholdPolicy(noCriticalPods)
	eng := NewDecisionEngine(p)
	dec := eng.Decide(delta, state)

	if !dec.ScaleUp {
		t.Fatalf("engine should return ScaleUp=true, got %+v", dec)
	}
}

func TestDecisionEngine_Decide_NoOp(t *testing.T) {
	state := makeState([]monitor.NodeInfo{readyNode("n1", 2000, 4096, 0, 0, 10*time.Minute)})
	delta := estimator.ResourceDelta{} // no shortfall

	p := DefaultThresholdPolicy(noCriticalPods)
	eng := NewDecisionEngine(p)
	dec := eng.Decide(delta, state)

	if dec.ScaleUp || dec.ScaleDown {
		t.Fatalf("expected no-op, got %+v", dec)
	}
}

// ─── DefaultThresholdPolicy constants ────────────────────────────────────────

func TestDefaultThresholdPolicy_Defaults(t *testing.T) {
	p := DefaultThresholdPolicy(noCriticalPods)
	if p.MaxNodes != utils.DefaultMaxNodes {
		t.Errorf("MaxNodes want %d got %d", utils.DefaultMaxNodes, p.MaxNodes)
	}
	if p.MinNodes != utils.DefaultMinNodes {
		t.Errorf("MinNodes want %d got %d", utils.DefaultMinNodes, p.MinNodes)
	}
	if p.CPUThresholdMillicores != utils.DefaultCPUThresholdMillicores {
		t.Errorf("CPUThreshold want %d got %d", utils.DefaultCPUThresholdMillicores, p.CPUThresholdMillicores)
	}
	if p.LowWaterMarkPercent != 20.0 {
		t.Errorf("LowWaterMark want 20.0 got %f", p.LowWaterMarkPercent)
	}
}

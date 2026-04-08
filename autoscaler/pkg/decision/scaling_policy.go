// Package decision — ScalingPolicy interface and ThresholdPolicy implementation.
package decision

import (
	"time"

	"github.com/kuber-scale/autoscaler/pkg/estimator"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── Decision ─────────────────────────────────────────────────────────────────

// Decision is the output of the scaling policy.
//
// Exactly one of ScaleUp / ScaleDown will be true; both false means no-op.
//   - ScaleUp  true → provision Count additional nodes.
//   - ScaleDown true → drain and remove the node named in NodeName.
//   - Count holds the number of nodes to add (ScaleUp path only).
type Decision struct {
	ScaleUp   bool
	ScaleDown bool
	Count     int

	// NodeName is the target node for scale-down (not part of the user-visible
	// struct definition, but required by the drain/cleanup pipeline).
	NodeName string

	// Reason is a one-line human-readable explanation written to the log.
	Reason string
}

// ─── ScalingPolicy ────────────────────────────────────────────────────────────

// ScalingPolicy is the interface for pluggable scaling strategies.
type ScalingPolicy interface {
	// Evaluate returns a Decision for the current cluster state and shortfall.
	Evaluate(delta estimator.ResourceDelta, state *monitor.ClusterState) Decision

	// HasCriticalPods returns true when the given node has pods that must not
	// be evicted (e.g. system-critical priority-class pods).  The
	// implementation is injected by the controller so this package stays
	// independent of the Kubernetes client.
	HasCriticalPods(nodeName string) bool
}

// ─── ThresholdPolicy ──────────────────────────────────────────────────────────

// ThresholdPolicy implements ScalingPolicy with configurable high/low
// watermarks.
//
// Scale-up rule  — triggered when any of:
//   - unschedulable pods exist AND nodesNeeded > 0
//
// Scale-down rule — ALL three conditions must hold:
//   - node CPU utilisation < LowWaterMarkPercent (default 20 %)
//   - node has NO critical pods
//   - node age > CooldownPeriod (default 60 s)
//
// Hard limits: minNodes = 1, maxNodes = 5.
type ThresholdPolicy struct {
	// CPUThresholdMillicores triggers scale-up when shortfall exceeds this.
	CPUThresholdMillicores int64
	// MemoryThresholdBytes triggers scale-up when shortfall exceeds this.
	MemoryThresholdBytes int64
	// LowWaterMarkPercent is the CPU utilisation % below which scale-down fires.
	LowWaterMarkPercent float64
	// MaxNodes is the hard upper bound on worker node count.
	MaxNodes int
	// MinNodes is the hard lower bound; scale-down never removes the last node.
	MinNodes int
	// CooldownPeriod is both the inter-event cooldown and the minimum node age
	// required before a node is eligible for scale-down.
	CooldownPeriod time.Duration
	// NodeCPUCapacityMillicores is the CPU assumed for each new node.
	NodeCPUCapacityMillicores int64
	// NodeMemoryCapacityBytes is the memory assumed for each new node.
	NodeMemoryCapacityBytes int64

	// criticalPodChecker is injected by the controller; see HasCriticalPods.
	criticalPodChecker func(nodeName string) bool
}

// DefaultThresholdPolicy returns a ThresholdPolicy with project-wide defaults.
// criticalPodFn must be provided by the caller (usually the controller).
func DefaultThresholdPolicy(criticalPodFn func(string) bool) *ThresholdPolicy {
	return &ThresholdPolicy{
		CPUThresholdMillicores:    utils.DefaultCPUThresholdMillicores,
		MemoryThresholdBytes:      utils.DefaultMemoryThresholdMiB * 1024 * 1024,
		LowWaterMarkPercent:       20.0,
		MaxNodes:                  utils.DefaultMaxNodes,          // 5
		MinNodes:                  utils.DefaultMinNodes,          // 1
		CooldownPeriod:            utils.DefaultScaleDownCooldown, // 60 s
		NodeCPUCapacityMillicores: 2000,                           // 2 vCPU
		NodeMemoryCapacityBytes:   4 * 1024 * 1024 * 1024,         // 4 GiB
		criticalPodChecker:        criticalPodFn,
	}
}

// HasCriticalPods delegates to the injected checker.
func (p *ThresholdPolicy) HasCriticalPods(nodeName string) bool {
	if p.criticalPodChecker == nil {
		return false
	}
	return p.criticalPodChecker(nodeName)
}

// Evaluate implements ScalingPolicy.
func (p *ThresholdPolicy) Evaluate(
	delta estimator.ResourceDelta,
	state *monitor.ClusterState,
) Decision {
	nodes := state.GetNodes()
	currentCount := len(nodes)

	// ── Scale Up ─────────────────────────────────────────────────────────────
	// Conditions: unschedulable pods exist AND nodesNeeded > 0.
	if !delta.IsZero() &&
		(delta.CPUMillicores > p.CPUThresholdMillicores ||
			delta.MemoryBytes > p.MemoryThresholdBytes) {

		if currentCount >= p.MaxNodes {
			return Decision{
				Reason: "scale-up needed but already at MaxNodes limit (5)",
			}
		}

		nodesForCPU := ceilDiv(delta.CPUMillicores, p.NodeCPUCapacityMillicores)
		nodesForMem := ceilDiv(delta.MemoryBytes, p.NodeMemoryCapacityBytes)
		needed := nodesForCPU
		if nodesForMem > needed {
			needed = nodesForMem
		}
		if needed < 1 {
			needed = 1
		}
		headroom := int64(p.MaxNodes - currentCount)
		if needed > headroom {
			needed = headroom
		}

		return Decision{
			ScaleUp: true,
			Count:   int(needed),
			Reason:  "unschedulable pods detected — resource shortfall exceeds threshold",
		}
	}

	// ── Scale Down ───────────────────────────────────────────────────────────
	// All three conditions must hold for the candidate node:
	//   1. CPU utilisation < 20 %
	//   2. No critical pods on the node
	//   3. Node age > cooldown period (60 s)
	if currentCount > p.MinNodes {
		if candidate := p.scaleDownCandidate(nodes); candidate != "" {
			return Decision{
				ScaleDown: true,
				Count:     1,
				NodeName:  candidate,
				Reason:    "node meets all scale-down criteria (low util, no critical pods, age>cooldown)",
			}
		}
	}

	return Decision{
		Reason: "cluster resources within normal range — no action required",
	}
}

// scaleDownCandidate picks the first node that satisfies all three scale-down
// conditions.  It returns "" if no eligible node exists.
func (p *ThresholdPolicy) scaleDownCandidate(nodes []monitor.NodeInfo) string {
	now := time.Now()
	for _, n := range nodes {
		if !n.Ready || n.Unschedulable {
			continue
		}

		// Condition 1 — CPU utilisation < low-water-mark.
		alloc := n.AllocatableCPU.MilliValue()
		if alloc == 0 {
			continue
		}
		util := float64(n.UsedCPU.MilliValue()) / float64(alloc) * 100
		if util >= p.LowWaterMarkPercent {
			continue
		}

		// Condition 2 — no critical pods.
		if p.HasCriticalPods(n.Name) {
			continue
		}

		// Condition 3 — node age > cooldown period.
		if now.Sub(n.CreatedAt) <= p.CooldownPeriod {
			continue
		}

		return n.Name
	}
	return ""
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// ceilDiv returns ⌈a/b⌉ for non-negative integers.
func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

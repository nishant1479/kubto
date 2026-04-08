// Package decision — DecisionEngine wraps a ScalingPolicy with structured logging.
package decision

import (
	"log"

	"github.com/kuber-scale/autoscaler/pkg/estimator"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
)

// DecisionEngine evaluates a ScalingPolicy and adds structured log output.
type DecisionEngine struct {
	policy ScalingPolicy
}

// NewDecisionEngine wraps policy in a DecisionEngine.
func NewDecisionEngine(policy ScalingPolicy) *DecisionEngine {
	return &DecisionEngine{policy: policy}
}

// Decide runs the policy and returns a Decision with log output.
func (e *DecisionEngine) Decide(
	delta estimator.ResourceDelta,
	state *monitor.ClusterState,
) Decision {
	nodes := state.GetNodes()
	pods := state.GetPendingPods()

	log.Printf(
		"[DecisionEngine] nodes=%d pending=%d cpu_shortfall=%dm mem_shortfall=%dMiB",
		len(nodes), len(pods),
		delta.CPUMillicores,
		delta.MemoryBytes/(1024*1024),
	)

	dec := e.policy.Evaluate(delta, state)

	switch {
	case dec.ScaleUp:
		log.Printf("[DecisionEngine] → SCALE UP  count=%d reason=%q", dec.Count, dec.Reason)
	case dec.ScaleDown:
		log.Printf("[DecisionEngine] → SCALE DOWN node=%q reason=%q", dec.NodeName, dec.Reason)
	default:
		log.Printf("[DecisionEngine] → NO-OP reason=%q", dec.Reason)
	}

	return dec
}

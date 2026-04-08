// Package detector classifies pending pods by the root cause of their
// scheduling failure and surfaces only those blocked by resource shortage.
package detector

import (
	"strings"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ShortageType describes which resource dimension is insufficient.
type ShortageType int

const (
	// ShortageNone means the pod is not blocked by a resource shortage.
	ShortageNone ShortageType = iota
	// ShortageCPU means only CPU is insufficient.
	ShortageCPU
	// ShortageMemory means only memory is insufficient.
	ShortageMemory
	// ShortageBoth means both CPU and memory are insufficient.
	ShortageBoth
)

// String returns a human-readable label for the shortage type.
func (s ShortageType) String() string {
	switch s {
	case ShortageCPU:
		return "CPU"
	case ShortageMemory:
		return "Memory"
	case ShortageBoth:
		return "CPU+Memory"
	default:
		return "None"
	}
}

// UnschedulableDetector inspects the PodScheduled condition to find pods that
// are stuck due to CPU or memory shortage only.
//
// Deliberately ignored root causes:
//   - Node taints / tolerations mismatch
//   - Pod affinity / anti-affinity rules
//   - Node selector / node affinity
//   - Image pull errors
//   - Volume / topology constraints
//
// These non-resource causes produce scheduler messages that do NOT contain
// "Insufficient cpu" or "Insufficient memory", so the string-match filter
// below is sufficient to exclude them.
type UnschedulableDetector struct{}

// NewUnschedulableDetector returns a ready-to-use detector.
func NewUnschedulableDetector() *UnschedulableDetector {
	return &UnschedulableDetector{}
}

// DetectUnschedulablePods returns the subset of pods that are unschedulable
// due to CPU or memory shortage.
//
// Signature mirrors the requirement: DetectUnschedulablePods(pods []PodInfo) []PodInfo
func (d *UnschedulableDetector) DetectUnschedulablePods(pods []monitor.PodInfo) []monitor.PodInfo {
	var result []monitor.PodInfo
	for _, pod := range pods {
		if isResourceUnschedulable(pod) {
			result = append(result, pod)
		}
	}
	return result
}

// ClassifyShortage returns which resource dimension is causing the scheduling
// failure for an already-confirmed unschedulable pod.
func ClassifyShortage(pod monitor.PodInfo) ShortageType {
	hasCPU := strings.Contains(pod.ConditionMessage, utils.InsufficientCPUMsg)
	hasMem := strings.Contains(pod.ConditionMessage, utils.InsufficientMemoryMsg)
	switch {
	case hasCPU && hasMem:
		return ShortageBoth
	case hasCPU:
		return ShortageCPU
	case hasMem:
		return ShortageMemory
	default:
		return ShortageNone
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// isResourceUnschedulable returns true when ALL three criteria hold:
//  1. Pod phase is Pending.
//  2. PodScheduled.Reason == "Unschedulable".
//  3. The scheduler message contains at least one resource-shortage substring.
func isResourceUnschedulable(pod monitor.PodInfo) bool {
	// Criterion 1 — phase
	if pod.Status != string("Pending") {
		return false
	}

	// Criterion 2 — condition reason
	if pod.ConditionReason != utils.UnschedulableReason {
		return false
	}

	// Criterion 3 — resource shortage keyword in scheduler message
	msg := pod.ConditionMessage
	return strings.Contains(msg, utils.InsufficientCPUMsg) ||
		strings.Contains(msg, utils.InsufficientMemoryMsg)
}

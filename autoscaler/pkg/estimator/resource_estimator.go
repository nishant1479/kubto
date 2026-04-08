// Package estimator computes the aggregate resource shortfall that must be
// covered by new nodes.
package estimator

import (
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"k8s.io/apimachinery/pkg/api/resource"
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

// ResourceEstimator computes how much additional cluster capacity is needed
// to schedule all currently unschedulable pods.
type ResourceEstimator struct{}

// NewResourceEstimator returns a ready-to-use estimator.
func NewResourceEstimator() *ResourceEstimator {
	return &ResourceEstimator{}
}

// Estimate returns the resource shortfall for the given set of unschedulable
// pods against the current free capacity reported by ClusterState.
//
// Algorithm:
//  1. Sum CPU and memory requests of all unschedulable pods.
//  2. Compute free capacity = allocatable − used across all Ready nodes.
//  3. Shortfall = max(0, requested − free) for each dimension.
func (e *ResourceEstimator) Estimate(
	state *monitor.ClusterState,
	unschedulable []monitor.PodInfo,
) ResourceDelta {
	// ── 1. Aggregate requests of unschedulable pods ──────────────────────────
	totalCPU := resource.NewMilliQuantity(0, resource.DecimalSI)
	totalMem := resource.NewQuantity(0, resource.BinarySI)

	for _, pod := range unschedulable {
		totalCPU.Add(pod.CPURequest)
		totalMem.Add(pod.MemoryRequest)
	}

	// ── 2. Available free capacity ───────────────────────────────────────────
	allocCPU := state.TotalAllocatableCPU()
	usedCPU := state.TotalUsedCPU()
	freeCPU := allocCPU.DeepCopy()
	freeCPU.Sub(usedCPU)

	allocMem := state.TotalAllocatableMemory()
	usedMem := state.TotalUsedMemory()
	freeMem := allocMem.DeepCopy()
	freeMem.Sub(usedMem)

	// ── 3. Shortfall = max(0, needed − free) ────────────────────────────────
	cpuShortfall := totalCPU.MilliValue() - freeCPU.MilliValue()
	memShortfall := totalMem.Value() - freeMem.Value()

	if cpuShortfall < 0 {
		cpuShortfall = 0
	}
	if memShortfall < 0 {
		memShortfall = 0
	}

	return ResourceDelta{
		CPUMillicores: cpuShortfall,
		MemoryBytes:   memShortfall,
	}
}

// Package monitor maintains a live, thread-safe snapshot of the Kubernetes
// cluster that every other package reads from.
package monitor

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ─── Domain types ─────────────────────────────────────────────────────────────

// PodInfo holds the scheduling-relevant fields of a single pod.
type PodInfo struct {
	// Name is the pod's metadata name.
	Name string
	// Namespace is the pod's metadata namespace.
	Namespace string
	// CPURequest is the sum of CPU requests across all containers.
	CPURequest resource.Quantity
	// MemoryRequest is the sum of memory requests across all containers.
	MemoryRequest resource.Quantity
	// Status is the pod phase as a string (e.g. "Pending").
	Status string
	// ConditionReason is the Reason field of the PodScheduled condition
	// (e.g. "Unschedulable").
	ConditionReason string
	// ConditionMessage is the human-readable Message from the PodScheduled
	// condition, used to classify the type of shortage.
	ConditionMessage string
	// NodeName is the node this pod is bound/scheduled to, if any.
	NodeName string
	// NodeSelector defines the pod's explicit node-selector requirements.
	NodeSelector map[string]string
	// Tolerations are the pod's tolerations for node taints.
	Tolerations []corev1.Toleration
	// HasUnsupportedConstraints is set to true when the pod uses scheduling
	// rules Kubto does not support (e.g. pod anti-affinity, topology spread).
	HasUnsupportedConstraints bool
	// UnsupportedReason details which constraint is unsupported.
	UnsupportedReason string
}

// NodeInfo holds the capacity and current usage for a single node.
type NodeInfo struct {
	// Name is the node's metadata name.
	Name string
	// AllocatableCPU is the CPU that Kubernetes exposes as schedulable.
	AllocatableCPU resource.Quantity
	// AllocatableMemory is the memory that Kubernetes exposes as schedulable.
	AllocatableMemory resource.Quantity
	// UsedCPU is the sum of CPU requests of all pods running on this node.
	UsedCPU resource.Quantity
	// UsedMemory is the sum of memory requests of all pods running on this node.
	UsedMemory resource.Quantity
	// Ready reflects the NodeReady condition.
	Ready bool
	// Unschedulable reflects node.Spec.Unschedulable (cordoned).
	Unschedulable bool
	// CreatedAt is the node's creation timestamp, used to enforce the minimum
	// node-age requirement before a node is eligible for scale-down.
	CreatedAt time.Time
	// Labels are the node's metadata labels used for nodeSelector matching.
	Labels map[string]string
	// Taints are the node's taints checked against pod tolerations.
	Taints []corev1.Taint
}

// ─── ClusterState ─────────────────────────────────────────────────────────────

// ClusterState is a thread-safe, in-memory snapshot of the cluster that is
// kept up to date by PodWatcher and NodeWatcher via informer callbacks.
//
// Callers MUST use the accessor methods; they must NOT access the unexported
// fields directly.
type ClusterState struct {
	mu            sync.RWMutex
	PendingPods   []PodInfo
	ScheduledPods []PodInfo
	Nodes         []NodeInfo
}

// NewClusterState returns an empty, ready-to-use ClusterState.
func NewClusterState() *ClusterState {
	return &ClusterState{}
}

// ─── Writers (called by informer handlers) ────────────────────────────────────

// SetPendingPods atomically replaces the full pending-pod list.
func (cs *ClusterState) SetPendingPods(pods []PodInfo) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.PendingPods = pods
}

// SetScheduledPods atomically replaces scheduled pods and recomputes node usage.
func (cs *ClusterState) SetScheduledPods(pods []PodInfo) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.ScheduledPods = pods
	cs.recomputeNodeUsageLocked()
}

// SetPods atomically updates both pending and scheduled pods and recomputes node usage.
func (cs *ClusterState) SetPods(pending, scheduled []PodInfo) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.PendingPods = pending
	cs.ScheduledPods = scheduled
	cs.recomputeNodeUsageLocked()
}

// SetNodes atomically replaces the full node list.
func (cs *ClusterState) SetNodes(nodes []NodeInfo) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.Nodes = nodes
	if len(cs.ScheduledPods) > 0 {
		cs.recomputeNodeUsageLocked()
	}
}

// recomputeNodeUsageLocked calculates per-node UsedCPU and UsedMemory by summing
// requests of non-terminal pods scheduled on that node. Caller must hold cs.mu.
func (cs *ClusterState) recomputeNodeUsageLocked() {
	if len(cs.ScheduledPods) == 0 {
		return
	}
	cpuMap := make(map[string]*resource.Quantity)
	memMap := make(map[string]*resource.Quantity)

	for _, p := range cs.ScheduledPods {
		if p.NodeName == "" {
			continue
		}
		if _, ok := cpuMap[p.NodeName]; !ok {
			cpuMap[p.NodeName] = resource.NewMilliQuantity(0, resource.DecimalSI)
			memMap[p.NodeName] = resource.NewQuantity(0, resource.BinarySI)
		}
		cpuMap[p.NodeName].Add(p.CPURequest)
		memMap[p.NodeName].Add(p.MemoryRequest)
	}

	for i := range cs.Nodes {
		name := cs.Nodes[i].Name
		if cpu, ok := cpuMap[name]; ok {
			cs.Nodes[i].UsedCPU = *cpu
		} else {
			cs.Nodes[i].UsedCPU = *resource.NewMilliQuantity(0, resource.DecimalSI)
		}
		if mem, ok := memMap[name]; ok {
			cs.Nodes[i].UsedMemory = *mem
		} else {
			cs.Nodes[i].UsedMemory = *resource.NewQuantity(0, resource.BinarySI)
		}
	}
}

// UpdateNode inserts or replaces a single node entry identified by name.
func (cs *ClusterState) UpdateNode(node NodeInfo) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	found := false
	for i, n := range cs.Nodes {
		if n.Name == node.Name {
			// Preserve computed usage if not explicitly supplied
			if node.UsedCPU.IsZero() && !n.UsedCPU.IsZero() {
				node.UsedCPU = n.UsedCPU
			}
			if node.UsedMemory.IsZero() && !n.UsedMemory.IsZero() {
				node.UsedMemory = n.UsedMemory
			}
			cs.Nodes[i] = node
			found = true
			break
		}
	}
	if !found {
		cs.Nodes = append(cs.Nodes, node)
	}
	if len(cs.ScheduledPods) > 0 {
		cs.recomputeNodeUsageLocked()
	}
}

// RemoveNode removes the node with the given name from the state.
func (cs *ClusterState) RemoveNode(name string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	filtered := cs.Nodes[:0]
	for _, n := range cs.Nodes {
		if n.Name != name {
			filtered = append(filtered, n)
		}
	}
	cs.Nodes = filtered
}

// ─── Readers (called by detector / estimator / decision) ─────────────────────

// GetPendingPods returns a shallow copy of the current pending-pod list.
func (cs *ClusterState) GetPendingPods() []PodInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]PodInfo, len(cs.PendingPods))
	copy(out, cs.PendingPods)
	return out
}

// GetScheduledPods returns a shallow copy of the current scheduled-pod list.
func (cs *ClusterState) GetScheduledPods() []PodInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]PodInfo, len(cs.ScheduledPods))
	copy(out, cs.ScheduledPods)
	return out
}

// GetNodes returns a shallow copy of the current node list.
func (cs *ClusterState) GetNodes() []NodeInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]NodeInfo, len(cs.Nodes))
	copy(out, cs.Nodes)
	return out
}

// ─── Aggregate capacity helpers ───────────────────────────────────────────────

// TotalAllocatableCPU sums AllocatableCPU across all Ready, schedulable nodes.
func (cs *ClusterState) TotalAllocatableCPU() resource.Quantity {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	total := resource.Quantity{}
	for _, n := range cs.Nodes {
		if n.Ready && !n.Unschedulable {
			total.Add(n.AllocatableCPU)
		}
	}
	return total
}

// TotalAllocatableMemory sums AllocatableMemory across all Ready, schedulable nodes.
func (cs *ClusterState) TotalAllocatableMemory() resource.Quantity {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	total := resource.Quantity{}
	for _, n := range cs.Nodes {
		if n.Ready && !n.Unschedulable {
			total.Add(n.AllocatableMemory)
		}
	}
	return total
}

// TotalUsedCPU sums UsedCPU across all nodes.
func (cs *ClusterState) TotalUsedCPU() resource.Quantity {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	total := resource.Quantity{}
	for _, n := range cs.Nodes {
		total.Add(n.UsedCPU)
	}
	return total
}

// TotalUsedMemory sums UsedMemory across all nodes.
func (cs *ClusterState) TotalUsedMemory() resource.Quantity {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	total := resource.Quantity{}
	for _, n := range cs.Nodes {
		total.Add(n.UsedMemory)
	}
	return total
}

// Package monitor — unit tests for ClusterState and Pod/Node conversion logic.
package monitor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kuber-scale/autoscaler/pkg/utils"
)

func mCPU(m int64) resource.Quantity { return *resource.NewMilliQuantity(m, resource.DecimalSI) }
func miB(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

func TestClusterState_RecomputeNodeUsage(t *testing.T) {
	cs := NewClusterState()

	nodes := []NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(4000),
			AllocatableMemory: miB(8192),
			Ready:             true,
		},
		{
			Name:              "node-2",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			Ready:             true,
		},
	}
	cs.SetNodes(nodes)

	scheduled := []PodInfo{
		{
			Name:          "pod-a",
			Namespace:     "default",
			NodeName:      "node-1",
			CPURequest:    mCPU(1500),
			MemoryRequest: miB(1024),
		},
		{
			Name:          "pod-b",
			Namespace:     "default",
			NodeName:      "node-1",
			CPURequest:    mCPU(500),
			MemoryRequest: miB(512),
		},
		{
			Name:          "pod-c",
			Namespace:     "default",
			NodeName:      "node-2",
			CPURequest:    mCPU(800),
			MemoryRequest: miB(2048),
		},
	}

	cs.SetScheduledPods(scheduled)

	updatedNodes := cs.GetNodes()
	if len(updatedNodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(updatedNodes))
	}

	var n1, n2 *NodeInfo
	for i := range updatedNodes {
		if updatedNodes[i].Name == "node-1" {
			n1 = &updatedNodes[i]
		}
		if updatedNodes[i].Name == "node-2" {
			n2 = &updatedNodes[i]
		}
	}

	if n1 == nil || n2 == nil {
		t.Fatal("missing node-1 or node-2")
	}

	// node-1 used: 1500 + 500 = 2000m CPU, 1024 + 512 = 1536 MiB memory
	if n1.UsedCPU.MilliValue() != 2000 {
		t.Errorf("node-1 UsedCPU want 2000m, got %dm", n1.UsedCPU.MilliValue())
	}
	if n1.UsedMemory.Value() != 1536*1024*1024 {
		t.Errorf("node-1 UsedMemory want 1536MiB, got %d bytes", n1.UsedMemory.Value())
	}

	// node-2 used: 800m CPU, 2048 MiB memory
	if n2.UsedCPU.MilliValue() != 800 {
		t.Errorf("node-2 UsedCPU want 800m, got %dm", n2.UsedCPU.MilliValue())
	}
	if n2.UsedMemory.Value() != 2048*1024*1024 {
		t.Errorf("node-2 UsedMemory want 2048MiB, got %d bytes", n2.UsedMemory.Value())
	}

	// Totals
	totalUsedCPU := cs.TotalUsedCPU()
	if totalUsedCPU.MilliValue() != 2800 {
		t.Errorf("TotalUsedCPU want 2800m, got %dm", totalUsedCPU.MilliValue())
	}
	totalUsedMem := cs.TotalUsedMemory()
	if totalUsedMem.Value() != (1536+2048)*1024*1024 {
		t.Errorf("TotalUsedMemory want %d, got %d", (1536+2048)*1024*1024, totalUsedMem.Value())
	}
}

func TestPodToInfo_ConstraintsAndUnsupportedRules(t *testing.T) {
	// Standard pod with nodeSelector and toleration
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"disk": "ssd", "zone": "us-east-1a"},
			Tolerations: []corev1.Toleration{
				{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "special", Effect: corev1.TaintEffectNoSchedule},
			},
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mCPU(500),
							corev1.ResourceMemory: miB(256),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Reason:  utils.UnschedulableReason,
					Message: utils.InsufficientCPUMsg,
				},
			},
		},
	}

	info := podToInfo(pod)
	if info.NodeSelector["disk"] != "ssd" || info.NodeSelector["zone"] != "us-east-1a" {
		t.Errorf("expected nodeSelector copied, got %v", info.NodeSelector)
	}
	if len(info.Tolerations) != 1 || info.Tolerations[0].Key != "dedicated" {
		t.Errorf("expected tolerations copied, got %v", info.Tolerations)
	}
	if info.HasUnsupportedConstraints {
		t.Errorf("standard pod should not have unsupported constraints")
	}

	// Pod with PodAntiAffinity (unsupported)
	podAnti := pod.DeepCopy()
	podAnti.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{},
	}
	infoAnti := podToInfo(podAnti)
	if !infoAnti.HasUnsupportedConstraints {
		t.Error("pod with podAntiAffinity should be marked as having unsupported constraints")
	}

	// Pod with TopologySpreadConstraints (unsupported)
	podTopo := pod.DeepCopy()
	podTopo.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: corev1.DoNotSchedule},
	}
	infoTopo := podToInfo(podTopo)
	if !infoTopo.HasUnsupportedConstraints {
		t.Error("pod with topology spread should be marked as having unsupported constraints")
	}
}

func TestNodeToInfo_LabelsAndTaints(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-gpu",
			Labels: map[string]string{"accelerator": "nvidia", "kubernetes.io/os": "linux"},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{
				{Key: "gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    mCPU(8000),
				corev1.ResourceMemory: miB(16384),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	info := nodeToInfo(node)
	if info.Labels["accelerator"] != "nvidia" {
		t.Errorf("expected label accelerator=nvidia, got %v", info.Labels)
	}
	if len(info.Taints) != 1 || info.Taints[0].Key != "gpu" {
		t.Errorf("expected taint gpu=true, got %v", info.Taints)
	}
	if !info.Ready {
		t.Error("expected Ready=true")
	}
}

func TestClusterState_RemoveNode(t *testing.T) {
	cs := NewClusterState()
	cs.SetNodes([]NodeInfo{
		{Name: "n1", AllocatableCPU: mCPU(1000), Ready: true},
		{Name: "n2", AllocatableCPU: mCPU(2000), Ready: true},
	})

	cs.RemoveNode("n1")
	nodes := cs.GetNodes()
	if len(nodes) != 1 || nodes[0].Name != "n2" {
		t.Errorf("expected only n2 remaining, got %v", nodes)
	}
}

func TestClusterState_TotalAllocatable_SkipsNotReadyOrCordoned(t *testing.T) {
	cs := NewClusterState()
	cs.SetNodes([]NodeInfo{
		{Name: "ready", AllocatableCPU: mCPU(2000), AllocatableMemory: miB(4096), Ready: true, Unschedulable: false},
		{Name: "not-ready", AllocatableCPU: mCPU(4000), AllocatableMemory: miB(8192), Ready: false, Unschedulable: false},
		{Name: "cordoned", AllocatableCPU: mCPU(4000), AllocatableMemory: miB(8192), Ready: true, Unschedulable: true},
	})

	cpu := cs.TotalAllocatableCPU()
	if cpu.MilliValue() != 2000 {
		t.Errorf("TotalAllocatableCPU want 2000m, got %dm", cpu.MilliValue())
	}
	mem := cs.TotalAllocatableMemory()
	if mem.Value() != 4096*1024*1024 {
		t.Errorf("TotalAllocatableMemory want 4096MiB, got %d bytes", mem.Value())
	}
}

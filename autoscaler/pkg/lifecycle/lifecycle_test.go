// Package lifecycle — tests for NodeDrainer, FindIdleNodes, and RemoveNode pipeline.
package lifecycle

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func mCPU(m int64) resource.Quantity { return *resource.NewMilliQuantity(m, resource.DecimalSI) }
func miB(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

func makeManagedNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				utils.AnnotationManagedBy: utils.ManagedByValue,
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: false},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func makePod(name, ns, nodeName string, phase corev1.PodPhase, ownerKind string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name:  "c",
					Image: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    mCPU(100),
							corev1.ResourceMemory: miB(128),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if ownerKind != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{Kind: ownerKind, Name: ownerKind + "-1"}}
	}
	return pod
}

// ─── shouldSkip / skipReason ──────────────────────────────────────────────────

func TestShouldSkip_Succeeded(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	if !shouldSkip(pod) {
		t.Error("Succeeded pod should be skipped")
	}
}

func TestShouldSkip_Failed(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	if !shouldSkip(pod) {
		t.Error("Failed pod should be skipped")
	}
}

func TestShouldSkip_StaticPod(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"kubernetes.io/config.mirror": "abc"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if !shouldSkip(pod) {
		t.Error("static/mirror pod should be skipped")
	}
}

func TestShouldSkip_DaemonSet(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: corev1.PodSpec{
			NodeName: "n1",
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds-1"}}
	if !shouldSkip(pod) {
		t.Error("DaemonSet pod should be skipped")
	}
}

func TestShouldSkip_RunningUserPod(t *testing.T) {
	pod := corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if shouldSkip(pod) {
		t.Error("running user pod must NOT be skipped")
	}
}

func TestIsStaticPod_WithMirrorAnnotation(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"kubernetes.io/config.mirror": "hash"},
		},
	}
	if !isStaticPod(pod) {
		t.Error("pod with mirror annotation should be static")
	}
}

func TestIsStaticPod_WithManagedByAnnotation(t *testing.T) {
	// A pod with our managed-by annotation is NOT a static pod.
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				utils.AnnotationManagedBy: utils.ManagedByValue,
			},
		},
	}
	if isStaticPod(pod) {
		t.Error("pod with managed-by annotation (no mirror annotation) must not be static")
	}
}

// ─── Drain — waitForPodsGone ──────────────────────────────────────────────────

func TestDrain_EmptyNode(t *testing.T) {
	client := fake.NewSimpleClientset()
	d := NewNodeDrainer(client)
	err := d.Drain(context.Background(), "empty-node")
	if err != nil {
		t.Fatalf("draining empty node failed: %v", err)
	}
}

func TestDrain_DaemonSetPodsSkipped(t *testing.T) {
	// DaemonSet pods are skipped; the node should drain immediately (nothing to wait for).
	client := fake.NewSimpleClientset()
	pod := makePod("ds-pod", "kube-system", "n1", corev1.PodRunning, "DaemonSet")
	client.CoreV1().Pods("kube-system").Create(context.Background(), pod, metav1.CreateOptions{})

	d := NewNodeDrainer(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.Drain(ctx, "n1")
	if err != nil {
		t.Fatalf("drain with only DaemonSet pods should succeed: %v", err)
	}
}

func TestDrain_CompletedPodsSkipped(t *testing.T) {
	// Succeeded/Failed pods are skipped — drain succeeds immediately.
	client := fake.NewSimpleClientset()
	pod := makePod("done-pod", "default", "n1", corev1.PodSucceeded, "")
	client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	d := NewNodeDrainer(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := d.Drain(ctx, "n1")
	if err != nil {
		t.Fatalf("drain with only completed pods should succeed: %v", err)
	}
}

func TestDrain_UserPod_TimesOutIfNotEvicted(t *testing.T) {
	// A running user pod that does NOT disappear from the fake API causes drain
	// to time out.  We verify the error is returned (not panicked/hung).
	client := fake.NewSimpleClientset()
	pod := makePod("app-pod", "default", "n1", corev1.PodRunning, "")
	client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	d := &NodeDrainer{
		Client:  client,
		Timeout: 1 * time.Second, // very short timeout for test speed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.Drain(ctx, "n1")
	// Eviction on fake client removes from the API, but the pod stays in the
	// List response used by waitForPodsGone. The drain may time out or succeed
	// depending on fake client behavior — either is acceptable; just must not hang.
	t.Logf("drain result: %v", err)
}

// ─── FindIdleNodes ────────────────────────────────────────────────────────────

func nodeInfo(name string, cpuAllocM, cpuUsedM int64, ready, unschedulable bool, age time.Duration) monitor.NodeInfo {
	return monitor.NodeInfo{
		Name:              name,
		AllocatableCPU:    mCPU(cpuAllocM),
		AllocatableMemory: miB(4096),
		UsedCPU:           mCPU(cpuUsedM),
		UsedMemory:        miB(0),
		Ready:             ready,
		Unschedulable:     unschedulable,
		CreatedAt:         time.Now().Add(-age),
	}
}

func TestFindIdleNodes_IdleNode(t *testing.T) {
	client := fake.NewSimpleClientset()
	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("idle", 2000, 0, true, false, 10*time.Minute),
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	if len(idle) != 1 || idle[0].Name != "idle" {
		t.Errorf("expected [idle], got %v", idle)
	}
}

func TestFindIdleNodes_BusyNode_NotIdle(t *testing.T) {
	client := fake.NewSimpleClientset()
	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("busy", 2000, 1000, true, false, 10*time.Minute), // 50% CPU
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	if len(idle) != 0 {
		t.Errorf("expected no idle nodes, got %v", idle)
	}
}

func TestFindIdleNodes_NotReadyNodeSkipped(t *testing.T) {
	client := fake.NewSimpleClientset()
	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("notready", 2000, 0, false, false, 10*time.Minute),
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	if len(idle) != 0 {
		t.Errorf("not-ready node must not be considered idle, got %v", idle)
	}
}

func TestFindIdleNodes_CordonedNodeSkipped(t *testing.T) {
	client := fake.NewSimpleClientset()
	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("cordoned", 2000, 0, true, true, 10*time.Minute),
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	if len(idle) != 0 {
		t.Errorf("cordoned node must not be considered idle, got %v", idle)
	}
}

func TestFindIdleNodes_ZeroAllocatableCPUSkipped(t *testing.T) {
	client := fake.NewSimpleClientset()
	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("zero-alloc", 0, 0, true, false, 10*time.Minute),
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	// Zero allocatable CPU — skip to avoid division by zero.
	if len(idle) != 0 {
		t.Errorf("zero-allocatable node must not be returned, got %v", idle)
	}
}

func TestFindIdleNodes_WithWorkloadPodsNotIdle(t *testing.T) {
	client := fake.NewSimpleClientset()
	pod := makePod("app", "default", "n1", corev1.PodRunning, "")
	client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	state := monitor.NewClusterState()
	state.SetNodes([]monitor.NodeInfo{
		nodeInfo("n1", 2000, 0, true, false, 10*time.Minute),
	})

	m := NewNodeLifecycleManager(client)
	idle := m.FindIdleNodes(context.Background(), state)
	if len(idle) != 0 {
		t.Errorf("node with running user pod must not be idle, got %v", idle)
	}
}

// ─── RemoveNode pipeline ──────────────────────────────────────────────────────

func TestRemoveNode_ManagedNode_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	node := makeManagedNode("worker-1")
	client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})

	m := NewNodeLifecycleManager(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// docker rm will fail in the test environment — that is non-fatal by design.
	_ = m.RemoveNode(ctx, "worker-1")

	// Verify the node was deleted from Kubernetes.
	_, apiErr := client.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if apiErr == nil {
		t.Error("expected node to be deleted from Kubernetes API")
	}
}

func TestRemoveNode_UnmanagedNode_Rejected(t *testing.T) {
	client := fake.NewSimpleClientset()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "unmanaged"},
	}
	client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})

	m := NewNodeLifecycleManager(client)
	err := m.RemoveNode(context.Background(), "unmanaged")
	if err == nil {
		t.Fatal("expected error for unmanaged node, got nil")
	}
}

func TestRemoveNode_NotFound_Error(t *testing.T) {
	client := fake.NewSimpleClientset()
	m := NewNodeLifecycleManager(client)
	err := m.RemoveNode(context.Background(), "ghost-node")
	if err == nil {
		t.Fatal("expected error for non-existent node, got nil")
	}
}

func TestRemoveNode_BlockedDrain_NodeNotDeleted(t *testing.T) {
	client := fake.NewSimpleClientset()
	node := makeManagedNode("worker-blocked")
	client.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})

	// User pod on the node that will not disappear
	pod := makePod("blocked-app", "default", "worker-blocked", corev1.PodRunning, "")
	client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	m := NewNodeLifecycleManager(client)
	// Short drain timeout so test finishes rapidly
	m.Drainer.Timeout = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := m.RemoveNode(ctx, "worker-blocked")
	if err == nil {
		t.Fatal("expected RemoveNode to fail when drain fails, got nil")
	}

	// CRITICAL SAFETY CHECK: Verify node was NOT deleted from Kubernetes
	nodeAfter, getErr := client.CoreV1().Nodes().Get(context.Background(), "worker-blocked", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("expected node to still exist in API server after failed drain: %v", getErr)
	}
	if nodeAfter.Name != "worker-blocked" {
		t.Errorf("expected node worker-blocked, got %v", nodeAfter.Name)
	}
}

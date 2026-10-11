// Package controller — deterministic unit and integration tests for AutoscalerController.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kuber-scale/autoscaler/pkg/metrics"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/provisioner"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

func mCPU(m int64) resource.Quantity { return *resource.NewMilliQuantity(m, resource.DecimalSI) }
func miB(mib int64) resource.Quantity {
	return *resource.NewQuantity(mib*1024*1024, resource.BinarySI)
}

func getMetricCounter(c prometheus.Counter) float64 {
	var m dto.Metric
	c.Write(&m)
	return m.GetCounter().GetValue()
}

func getMetricGauge(g prometheus.Gauge) float64 {
	var m dto.Metric
	g.Write(&m)
	return m.GetGauge().GetValue()
}

func TestController_ScaleUp_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)

	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(0) // disable cooldown for test

	// Cluster has 1 ready node with 2000m CPU
	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(1500),
			UsedMemory:        miB(1024),
			Ready:             true,
		},
	})

	// 1 unschedulable pod requesting 1200m CPU (only 500m free -> 700m shortfall > 500m threshold)
	ctrl.state.SetPendingPods([]monitor.PodInfo{
		{
			Name:             "pod-1",
			Namespace:        "default",
			CPURequest:       mCPU(1200),
			MemoryRequest:    miB(512),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		},
	})

	ctx := context.Background()
	ctrl.Reconcile(ctx)

	if fakeProv.CreateCount() != 1 {
		t.Fatalf("expected 1 node provisioned, got %d", fakeProv.CreateCount())
	}
	if v := getMetricCounter(m.ScaleUpTotal); v != 1 {
		t.Errorf("ScaleUpTotal metric want 1, got %v", v)
	}
	if v := getMetricCounter(m.NodesProvisionedTotal); v != 1 {
		t.Errorf("NodesProvisionedTotal metric want 1, got %v", v)
	}
}

func TestController_ScaleUp_DuplicateReconciliationSuppressed(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)

	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(60 * time.Second) // 60s cooldown

	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(1800),
			UsedMemory:        miB(1024),
			Ready:             true,
		},
	})

	ctrl.state.SetPendingPods([]monitor.PodInfo{
		{
			Name:             "pod-1",
			Namespace:        "default",
			CPURequest:       mCPU(1000),
			MemoryRequest:    miB(512),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		},
	})

	ctx := context.Background()

	// 1st reconcile: triggers scale-up
	ctrl.Reconcile(ctx)
	if fakeProv.CreateCount() != 1 {
		t.Fatalf("1st reconcile: expected 1 node, got %d", fakeProv.CreateCount())
	}

	// 2nd reconcile immediately: must NOT trigger second scale-up due to cooldown & in-flight guard
	ctrl.Reconcile(ctx)
	if fakeProv.CreateCount() != 1 {
		t.Fatalf("2nd reconcile: duplicate scale-up occurred! count=%d", fakeProv.CreateCount())
	}
}

func TestController_ScaleUp_JoiningUnreadyNodeSuppressed(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)

	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(0) // 0s cooldown to test the unready node guard specifically

	// Cluster has 1 ready node and 1 node that was provisioned and is currently joining (Ready=false)
	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(1800),
			UsedMemory:        miB(1024),
			Ready:             true,
		},
		{
			Name:              "node-joining-2",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			Ready:             false, // in-flight / joining
		},
	})

	ctrl.state.SetPendingPods([]monitor.PodInfo{
		{
			Name:             "pod-1",
			Namespace:        "default",
			CPURequest:       mCPU(1000),
			MemoryRequest:    miB(512),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		},
	})

	ctx := context.Background()
	ctrl.Reconcile(ctx)

	// Since 1 node is already joining, toProvision is 1 - 1 = 0. No new node should be created!
	if fakeProv.CreateCount() != 0 {
		t.Fatalf("expected 0 new nodes because 1 node is already joining, got %d", fakeProv.CreateCount())
	}
}

func TestController_ScaleUp_FailureRecorded(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)

	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	fakeProv := provisioner.NewFakeProvisioner()
	fakeProv.CreateError = errors.New("docker daemon unresponsive")
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(0)

	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(1900),
			Ready:             true,
		},
	})

	ctrl.state.SetPendingPods([]monitor.PodInfo{
		{
			Name:             "pod-1",
			Namespace:        "default",
			CPURequest:       mCPU(1000),
			MemoryRequest:    miB(512),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		},
	})

	ctrl.Reconcile(context.Background())

	if v := getMetricCounter(m.ProvisionFailuresTotal); v != 1 {
		t.Errorf("ProvisionFailuresTotal metric want 1, got %v", v)
	}
}

func TestController_ScaleDown_IdleNodeRemoved(t *testing.T) {
	client := fake.NewSimpleClientset()

	// Add managed node to fake API server
	managedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "idle-worker",
			Annotations: map[string]string{
				utils.AnnotationManagedBy: utils.ManagedByValue,
			},
		},
	}
	client.CoreV1().Nodes().Create(context.Background(), managedNode, metav1.CreateOptions{})

	ctrl := NewAutoscalerController(client)
	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleDownCooldown(0) // disable cooldown

	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	// Two nodes: 1 busy, 1 idle and old enough
	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "busy-node",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(1500),
			Ready:             true,
			CreatedAt:         time.Now().Add(-10 * time.Minute),
		},
		{
			Name:              "idle-worker",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(0),
			Ready:             true,
			CreatedAt:         time.Now().Add(-10 * time.Minute),
		},
	})
	ctrl.state.SetPendingPods(nil) // no pending pods

	ctrl.Reconcile(context.Background())

	if v := getMetricCounter(m.ScaleDownTotal); v != 1 {
		t.Errorf("ScaleDownTotal metric want 1, got %v", v)
	}
	if v := getMetricCounter(m.NodesTerminatedTotal); v != 1 {
		t.Errorf("NodesTerminatedTotal metric want 1, got %v", v)
	}
}

func TestController_GaugesUpdatedDuringReconcile(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)

	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	ctrl.state.SetNodes([]monitor.NodeInfo{
		{Name: "n1", AllocatableCPU: mCPU(2000), Ready: true},
		{Name: "n2", AllocatableCPU: mCPU(2000), Ready: true},
		{Name: "n3", AllocatableCPU: mCPU(2000), Ready: false}, // not ready
	})
	ctrl.state.SetPendingPods(nil)

	ctrl.Reconcile(context.Background())

	if v := getMetricGauge(m.ActiveNodes); v != 2 {
		t.Errorf("ActiveNodes gauge want 2, got %v", v)
	}
	if v := getMetricGauge(m.PendingPods); v != 0 {
		t.Errorf("PendingPods gauge want 0, got %v", v)
	}
}

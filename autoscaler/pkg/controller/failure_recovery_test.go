package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kuber-scale/autoscaler/pkg/metrics"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/provisioner"
)

func TestController_TransientProvisionError_RecordedAndBounded(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	fakeProv := provisioner.NewFakeProvisioner()
	fakeProv.CreateError = errors.New("transient docker error")
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(time.Minute)

	ctrl.state.SetNodes([]monitor.NodeInfo{readyWorker("node-1", 2000, 1800)})
	ctrl.state.SetPendingPods([]monitor.PodInfo{unschedPod("pod-1", 1000, 512)})

	ctrl.Reconcile(context.Background())
	if fakeProv.CreateAttempts() != 1 {
		t.Fatalf("expected 1 create attempt, got %d", fakeProv.CreateAttempts())
	}
	if fakeProv.CreateCount() != 0 {
		t.Fatalf("failed create must not record a node, got %d", fakeProv.CreateCount())
	}
	if v := getMetricCounter(m.ProvisionFailuresTotal); v != 1 {
		t.Errorf("ProvisionFailuresTotal want 1, got %v", v)
	}

	ctrl.Reconcile(context.Background())
	if fakeProv.CreateAttempts() != 1 {
		t.Fatalf("backoff should suppress a second attempt, got %d", fakeProv.CreateAttempts())
	}
}

func TestController_ProvisionTimeout_NoDuplicateNode(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	fakeProv := provisioner.NewFakeProvisioner()
	fakeProv.CreateDelay = 200 * time.Millisecond
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(0)

	ctrl.state.SetNodes([]monitor.NodeInfo{readyWorker("node-1", 2000, 1800)})
	ctrl.state.SetPendingPods([]monitor.PodInfo{unschedPod("pod-1", 1000, 512)})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ctrl.Reconcile(ctx)

	if fakeProv.CreateCount() != 0 {
		t.Fatalf("timed-out create must not record a node, got %d", fakeProv.CreateCount())
	}
	if fakeProv.CreateAttempts() != 1 {
		t.Fatalf("expected 1 attempt, got %d", fakeProv.CreateAttempts())
	}
}

func TestController_PartialProvisioningFailure(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	reg := prometheus.NewRegistry()
	m := metrics.NewMetrics(reg)
	ctrl.SetMetrics(m)

	fakeProv := provisioner.NewFakeProvisioner()
	fakeProv.FailAfter = 1
	fakeProv.CreateError = errors.New("second node failed")
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(0)

	// Empty cluster, two 1500m pods → BFD needs 2 template nodes.
	ctrl.state.SetNodes(nil)
	ctrl.state.SetPendingPods([]monitor.PodInfo{
		unschedPod("pod-1", 1500, 512),
		unschedPod("pod-2", 1500, 512),
	})

	ctrl.Reconcile(context.Background())

	if fakeProv.CreateCount() != 1 {
		t.Fatalf("expected 1 successful node before FailAfter, got %d", fakeProv.CreateCount())
	}
	if fakeProv.CreateAttempts() != 2 {
		t.Fatalf("expected 2 attempts (1 ok, 1 fail), got %d", fakeProv.CreateAttempts())
	}
	if v := getMetricCounter(m.ProvisionFailuresTotal); v != 1 {
		t.Errorf("ProvisionFailuresTotal want 1, got %v", v)
	}
}

func TestController_RepeatedReconcile_NoDuplicateAfterSuccess(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleUpCooldown(time.Hour)

	ctrl.state.SetNodes([]monitor.NodeInfo{readyWorker("node-1", 2000, 1800)})
	ctrl.state.SetPendingPods([]monitor.PodInfo{unschedPod("pod-1", 1000, 512)})

	for i := 0; i < 8; i++ {
		ctrl.Reconcile(context.Background())
	}
	if fakeProv.CreateCount() != 1 {
		t.Fatalf("repeated reconcile must not duplicate nodes, got %d", fakeProv.CreateCount())
	}
}

func TestController_KubernetesAPIError_IdleCheckSkipped(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api unavailable")
	})

	ctrl := NewAutoscalerController(client)
	fakeProv := provisioner.NewFakeProvisioner()
	ctrl.SetProvisioner(fakeProv)
	ctrl.SetScaleDownCooldown(0)

	ctrl.state.SetNodes([]monitor.NodeInfo{
		readyWorker("busy", 2000, 1500),
		{
			Name:              "idle-worker",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(0),
			Ready:             true,
			CreatedAt:         time.Now().Add(-10 * time.Minute),
		},
	})
	ctrl.state.SetPendingPods(nil)

	ctrl.Reconcile(context.Background())
	if fakeProv.TerminateCount() != 0 {
		t.Fatalf("API errors during idle detection must not terminate nodes, got %d", fakeProv.TerminateCount())
	}
}

// Package controller — repeatable benchmarks measuring controller reconcile and decision latency.
package controller

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kuber-scale/autoscaler/pkg/metrics"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/provisioner"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

func BenchmarkController_Reconcile_ScaleUpLatency(b *testing.B) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	ctrl.SetProvisioner(provisioner.NewFakeProvisioner())
	ctrl.SetMetrics(metrics.NewMetrics(prometheus.NewRegistry()))
	ctrl.SetScaleUpCooldown(0)

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
	ctrl.state.SetPendingPods([]monitor.PodInfo{
		{
			Name:             "pod-bench",
			Namespace:        "default",
			CPURequest:       mCPU(1200),
			MemoryRequest:    miB(512),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		},
	})

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctrl.Reconcile(ctx)
	}
}

func BenchmarkController_Reconcile_NoOpLatency(b *testing.B) {
	client := fake.NewSimpleClientset()
	ctrl := NewAutoscalerController(client)
	ctrl.SetProvisioner(provisioner.NewFakeProvisioner())
	ctrl.SetMetrics(metrics.NewMetrics(prometheus.NewRegistry()))

	ctrl.state.SetNodes([]monitor.NodeInfo{
		{
			Name:              "node-1",
			AllocatableCPU:    mCPU(2000),
			AllocatableMemory: miB(4096),
			UsedCPU:           mCPU(500),
			UsedMemory:        miB(1024),
			Ready:             true,
		},
	})
	ctrl.state.SetPendingPods(nil)

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctrl.Reconcile(ctx)
	}
}

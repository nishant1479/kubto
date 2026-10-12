// Package estimator — reproducible benchmarks measuring BFD bin-packing and constraint evaluation latency.
package estimator

import (
	"fmt"
	"testing"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

func generateBenchmarkCluster(numNodes int) *monitor.ClusterState {
	nodes := make([]monitor.NodeInfo, numNodes)
	for i := 0; i < numNodes; i++ {
		nodes[i] = readyNode(fmt.Sprintf("bench-node-%d", i), 2000, 4096)
	}
	cs := monitor.NewClusterState()
	cs.SetNodes(nodes)
	return cs
}

func generateBenchmarkPods(numPods int) []monitor.PodInfo {
	pods := make([]monitor.PodInfo, numPods)
	for i := 0; i < numPods; i++ {
		cpu := int64(250 + (i%5)*150) // 250m to 850m
		mem := int64(128 + (i%4)*128) // 128MiB to 512MiB
		pods[i] = monitor.PodInfo{
			Name:             fmt.Sprintf("bench-pod-%d", i),
			Namespace:        "default",
			CPURequest:       mCPU(cpu),
			MemoryRequest:    miB(mem),
			Status:           "Pending",
			ConditionReason:  utils.UnschedulableReason,
			ConditionMessage: utils.InsufficientCPUMsg,
		}
	}
	return pods
}

func benchmarkEstimator(b *testing.B, podCount, nodeCount int) {
	e := NewResourceEstimator()
	state := generateBenchmarkCluster(nodeCount)
	pods := generateBenchmarkPods(podCount)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.EstimateDetailed(state, pods)
	}
}

func BenchmarkEstimator_BFD_10Pods(b *testing.B) {
	benchmarkEstimator(b, 10, 3)
}

func BenchmarkEstimator_BFD_50Pods(b *testing.B) {
	benchmarkEstimator(b, 50, 5)
}

func BenchmarkEstimator_BFD_100Pods(b *testing.B) {
	benchmarkEstimator(b, 100, 10)
}

func BenchmarkEstimator_BFD_500Pods(b *testing.B) {
	benchmarkEstimator(b, 500, 20)
}

func BenchmarkEstimator_BFD_1000Pods(b *testing.B) {
	benchmarkEstimator(b, 1000, 50)
}

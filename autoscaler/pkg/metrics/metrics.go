// Package metrics defines and registers Prometheus metrics for the Kubto autoscaler.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	once sync.Once
	reg  *Metrics
)

// Metrics encapsulates all Prometheus metrics exposed by Kubto.
type Metrics struct {
	PendingPods            prometheus.Gauge
	ActiveNodes            prometheus.Gauge
	EstimatedRequiredNodes prometheus.Gauge

	ScaleUpTotal         prometheus.Counter
	ScaleDownTotal       prometheus.Counter
	NodesProvisionedTotal prometheus.Counter
	NodesTerminatedTotal  prometheus.Counter
	ProvisionFailuresTotal prometheus.Counter

	ScalingDecisionsTotal *prometheus.CounterVec

	ReconcileDuration prometheus.Histogram
	ProvisionDuration prometheus.Histogram

	registry prometheus.Registerer
}

// NewMetrics creates and registers all Kubto metrics into the provided Prometheus registerer.
// If registerer is nil, prometheus.DefaultRegisterer is used.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	m := &Metrics{
		registry: registerer,

		PendingPods: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubto_pending_pods",
			Help: "Current number of unschedulable pending pods observed by the autoscaler.",
		}),

		ActiveNodes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubto_active_nodes",
			Help: "Current number of ready, active worker nodes in the cluster.",
		}),

		EstimatedRequiredNodes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kubto_estimated_required_nodes",
			Help: "Estimated number of nodes required to satisfy pending pod demand.",
		}),

		ScaleUpTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubto_scale_up_total",
			Help: "Total number of scale-up operations initiated.",
		}),

		ScaleDownTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubto_scale_down_total",
			Help: "Total number of scale-down operations initiated.",
		}),

		NodesProvisionedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubto_nodes_provisioned_total",
			Help: "Total number of worker nodes successfully provisioned.",
		}),

		NodesTerminatedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubto_nodes_terminated_total",
			Help: "Total number of worker nodes successfully drained and terminated.",
		}),

		ProvisionFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kubto_provision_failures_total",
			Help: "Total number of worker node provisioning failures encountered.",
		}),

		ScalingDecisionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kubto_scaling_decisions_total",
				Help: "Total scaling policy decisions made, partitioned by decision outcome.",
			},
			[]string{"decision"}, // "scale_up", "scale_down", "noop"
		),

		ReconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kubto_reconcile_duration_seconds",
			Help:    "Histogram of reconcile iteration durations in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),

		ProvisionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kubto_provision_duration_seconds",
			Help:    "Histogram of node provisioning durations in seconds.",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 180, 300},
		}),
	}

	// Register collectors safely (Register or MustRegister; MustRegister can panic on dup,
	// so we use Register and ignore AlreadyRegisteredError for idempotency).
	collectors := []prometheus.Collector{
		m.PendingPods,
		m.ActiveNodes,
		m.EstimatedRequiredNodes,
		m.ScaleUpTotal,
		m.ScaleDownTotal,
		m.NodesProvisionedTotal,
		m.NodesTerminatedTotal,
		m.ProvisionFailuresTotal,
		m.ScalingDecisionsTotal,
		m.ReconcileDuration,
		m.ProvisionDuration,
	}

	for _, c := range collectors {
		if err := registerer.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}

	return m
}

// Default returns the process-wide default Metrics instance.
func Default() *Metrics {
	once.Do(func() {
		reg = NewMetrics(prometheus.DefaultRegisterer)
	})
	return reg
}

// ─── Convenience Helper Methods ──────────────────────────────────────────────

func (m *Metrics) SetPendingPods(count int) {
	m.PendingPods.Set(float64(count))
}

func (m *Metrics) SetActiveNodes(count int) {
	m.ActiveNodes.Set(float64(count))
}

func (m *Metrics) SetEstimatedRequiredNodes(count int) {
	m.EstimatedRequiredNodes.Set(float64(count))
}

func (m *Metrics) IncScaleUp() {
	m.ScaleUpTotal.Inc()
}

func (m *Metrics) IncScaleDown() {
	m.ScaleDownTotal.Inc()
}

func (m *Metrics) IncNodesProvisioned(count int) {
	m.NodesProvisionedTotal.Add(float64(count))
}

func (m *Metrics) IncNodesTerminated() {
	m.NodesTerminatedTotal.Inc()
}

func (m *Metrics) IncProvisionFailure() {
	m.ProvisionFailuresTotal.Inc()
}

func (m *Metrics) RecordDecision(outcome string) {
	m.ScalingDecisionsTotal.WithLabelValues(outcome).Inc()
}

func (m *Metrics) RecordReconcileDuration(d time.Duration) {
	m.ReconcileDuration.Observe(d.Seconds())
}

func (m *Metrics) RecordProvisionDuration(d time.Duration) {
	m.ProvisionDuration.Observe(d.Seconds())
}

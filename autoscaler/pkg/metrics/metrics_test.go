// Package metrics — unit tests verifying metric updates, label values, and safe registration.
package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func getGaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	g.Write(&m)
	return m.GetGauge().GetValue()
}

func getCounterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	c.Write(&m)
	return m.GetCounter().GetValue()
}

func getHistogramCount(h prometheus.Histogram) uint64 {
	var m dto.Metric
	h.Write(&m)
	return m.GetHistogram().GetSampleCount()
}

func TestMetrics_GaugeUpdates(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetPendingPods(5)
	if v := getGaugeValue(m.PendingPods); v != 5 {
		t.Errorf("PendingPods want 5, got %v", v)
	}

	m.SetActiveNodes(3)
	if v := getGaugeValue(m.ActiveNodes); v != 3 {
		t.Errorf("ActiveNodes want 3, got %v", v)
	}

	m.SetEstimatedRequiredNodes(2)
	if v := getGaugeValue(m.EstimatedRequiredNodes); v != 2 {
		t.Errorf("EstimatedRequiredNodes want 2, got %v", v)
	}
}

func TestMetrics_CounterUpdates(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.IncScaleUp()
	m.IncScaleUp()
	if v := getCounterValue(m.ScaleUpTotal); v != 2 {
		t.Errorf("ScaleUpTotal want 2, got %v", v)
	}

	m.IncScaleDown()
	if v := getCounterValue(m.ScaleDownTotal); v != 1 {
		t.Errorf("ScaleDownTotal want 1, got %v", v)
	}

	m.IncNodesProvisioned(3)
	if v := getCounterValue(m.NodesProvisionedTotal); v != 3 {
		t.Errorf("NodesProvisionedTotal want 3, got %v", v)
	}

	m.IncNodesTerminated()
	if v := getCounterValue(m.NodesTerminatedTotal); v != 1 {
		t.Errorf("NodesTerminatedTotal want 1, got %v", v)
	}

	m.IncProvisionFailure()
	if v := getCounterValue(m.ProvisionFailuresTotal); v != 1 {
		t.Errorf("ProvisionFailuresTotal want 1, got %v", v)
	}
}

func TestMetrics_ScalingDecisionsVector(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordDecision("scale_up")
	m.RecordDecision("scale_up")
	m.RecordDecision("noop")

	counterUp, err := m.ScalingDecisionsTotal.GetMetricWithLabelValues("scale_up")
	if err != nil {
		t.Fatalf("failed to get scale_up metric: %v", err)
	}
	if v := getCounterValue(counterUp); v != 2 {
		t.Errorf("scale_up decisions want 2, got %v", v)
	}

	counterNoop, err := m.ScalingDecisionsTotal.GetMetricWithLabelValues("noop")
	if err != nil {
		t.Fatalf("failed to get noop metric: %v", err)
	}
	if v := getCounterValue(counterNoop); v != 1 {
		t.Errorf("noop decisions want 1, got %v", v)
	}
}

func TestMetrics_HistogramObservations(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordReconcileDuration(50 * time.Millisecond)
	m.RecordReconcileDuration(100 * time.Millisecond)
	if c := getHistogramCount(m.ReconcileDuration); c != 2 {
		t.Errorf("ReconcileDuration sample count want 2, got %d", c)
	}

	m.RecordProvisionDuration(15 * time.Second)
	if c := getHistogramCount(m.ProvisionDuration); c != 1 {
		t.Errorf("ProvisionDuration sample count want 1, got %d", c)
	}
}

func TestMetrics_DuplicateRegistrationSafe(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)
	// Calling NewMetrics again with the same registry must not panic
	_ = NewMetrics(reg)
}

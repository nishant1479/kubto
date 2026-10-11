// Package detector — unit tests for UnschedulableDetector.
package detector

import (
	"testing"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func makePod(status, reason, message string) monitor.PodInfo {
	return monitor.PodInfo{
		Name:             "pod-1",
		Namespace:        "default",
		Status:           status,
		ConditionReason:  reason,
		ConditionMessage: message,
	}
}

// ─── DetectUnschedulablePods ──────────────────────────────────────────────────

func TestDetect_CPUShortage(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", utils.UnschedulableReason, "0/1 nodes are available: 1 Insufficient cpu"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 1 {
		t.Fatalf("expected 1 unschedulable pod, got %d", len(got))
	}
}

func TestDetect_MemoryShortage(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", utils.UnschedulableReason, "0/1 nodes are available: 1 Insufficient memory"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 1 {
		t.Fatalf("expected 1 unschedulable pod, got %d", len(got))
	}
}

func TestDetect_BothCPUAndMemory(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", utils.UnschedulableReason, "Insufficient cpu, Insufficient memory"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 1 {
		t.Fatalf("expected 1 unschedulable pod, got %d", len(got))
	}
}

func TestDetect_NotPending_Ignored(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Running", utils.UnschedulableReason, "Insufficient cpu"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 0 {
		t.Fatalf("running pod should be ignored, got %d", len(got))
	}
}

func TestDetect_WrongReason_Ignored(t *testing.T) {
	// Pod is Pending but blocked by node selector, not resource shortage.
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", "Unschedulable", "0/1 nodes matching node selector"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 0 {
		t.Fatalf("non-resource unschedulable pod should be ignored, got %d", len(got))
	}
}

func TestDetect_TaintMismatch_Ignored(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", utils.UnschedulableReason, "0/1 nodes available: 1 node(s) had taint NoSchedule"),
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 0 {
		t.Fatalf("taint-blocked pod should be ignored, got %d", len(got))
	}
}

func TestDetect_EmptyList(t *testing.T) {
	d := NewUnschedulableDetector()
	got := d.DetectUnschedulablePods(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty result for nil input, got %d", len(got))
	}
}

func TestDetect_MixedPods(t *testing.T) {
	d := NewUnschedulableDetector()
	pods := []monitor.PodInfo{
		makePod("Pending", utils.UnschedulableReason, "Insufficient cpu"),   // should be detected
		makePod("Running", "", ""),                                           // skip: not pending
		makePod("Pending", "PodInitializing", ""),                           // skip: wrong reason
		makePod("Pending", utils.UnschedulableReason, "Insufficient memory"), // should be detected
	}
	got := d.DetectUnschedulablePods(pods)
	if len(got) != 2 {
		t.Fatalf("expected 2 unschedulable pods, got %d", len(got))
	}
}

// ─── ClassifyShortage ─────────────────────────────────────────────────────────

func TestClassifyShortage_CPU(t *testing.T) {
	pod := makePod("Pending", utils.UnschedulableReason, "Insufficient cpu")
	if ClassifyShortage(pod) != ShortageCPU {
		t.Errorf("expected ShortageCPU, got %v", ClassifyShortage(pod))
	}
}

func TestClassifyShortage_Memory(t *testing.T) {
	pod := makePod("Pending", utils.UnschedulableReason, "Insufficient memory")
	if ClassifyShortage(pod) != ShortageMemory {
		t.Errorf("expected ShortageMemory, got %v", ClassifyShortage(pod))
	}
}

func TestClassifyShortage_Both(t *testing.T) {
	pod := makePod("Pending", utils.UnschedulableReason, "Insufficient cpu, Insufficient memory")
	if ClassifyShortage(pod) != ShortageBoth {
		t.Errorf("expected ShortageBoth, got %v", ClassifyShortage(pod))
	}
}

func TestClassifyShortage_None(t *testing.T) {
	pod := makePod("Pending", utils.UnschedulableReason, "node affinity")
	if ClassifyShortage(pod) != ShortageNone {
		t.Errorf("expected ShortageNone, got %v", ClassifyShortage(pod))
	}
}

func TestShortageType_String(t *testing.T) {
	cases := map[ShortageType]string{
		ShortageCPU:    "CPU",
		ShortageMemory: "Memory",
		ShortageBoth:   "CPU+Memory",
		ShortageNone:   "None",
	}
	for st, expected := range cases {
		if st.String() != expected {
			t.Errorf("ShortageType(%d).String() = %q, want %q", st, st.String(), expected)
		}
	}
}

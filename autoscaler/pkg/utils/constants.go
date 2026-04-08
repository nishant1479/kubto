package utils

import "time"

const (
	// ─── Pod Scheduling ────────────────────────────────────────────────────────

	// PodConditionPodScheduled is the condition type checked for scheduling status.
	PodConditionPodScheduled = "PodScheduled"

	// UnschedulableReason is the condition reason set by the scheduler when a
	// pod cannot be placed on any node.
	UnschedulableReason = "Unschedulable"

	// InsufficientCPUMsg is the substring the scheduler embeds when CPU is the
	// bottleneck.
	InsufficientCPUMsg = "Insufficient cpu"

	// InsufficientMemoryMsg is the substring the scheduler embeds when memory
	// is the bottleneck.
	InsufficientMemoryMsg = "Insufficient memory"

	// ─── Node Labels & Annotations ─────────────────────────────────────────────

	// NodeRoleWorker is the well-known label for worker nodes.
	NodeRoleWorker = "node-role.kubernetes.io/worker"

	// AnnotationManagedBy is stamped on nodes provisioned by this autoscaler.
	AnnotationManagedBy = "autoscaler.kuber-scale/managed-by"

	// AnnotationScaledAt records the RFC3339 timestamp at which the node was
	// provisioned.
	AnnotationScaledAt = "autoscaler.kuber-scale/scaled-at"

	// ManagedByValue is the value written to AnnotationManagedBy.
	ManagedByValue = "kuber-scale-autoscaler"

	// ─── Scaling Thresholds ─────────────────────────────────────────────────────

	// DefaultCPUThresholdMillicores is the minimum CPU shortfall (in milli-cores)
	// that triggers a scale-up event.
	DefaultCPUThresholdMillicores = int64(500) // 0.5 vCPU

	// DefaultMemoryThresholdMiB is the minimum memory shortfall (in MiB) that
	// triggers a scale-up event.
	DefaultMemoryThresholdMiB = int64(256) // 256 MiB

	// ─── Node Limits ────────────────────────────────────────────────────────────

	// DefaultMaxNodes is the upper bound of nodes the autoscaler can maintain.
	DefaultMaxNodes = 5

	// DefaultMinNodes is the lower bound; scale-down will not go below this.
	DefaultMinNodes = 1

	// ─── Drain ──────────────────────────────────────────────────────────────────

	// DefaultDrainTimeoutSeconds is how long the drainer waits for pods to
	// terminate before giving up.
	DefaultDrainTimeoutSeconds = time.Duration(120) * time.Second

	// ─── Cooldowns ──────────────────────────────────────────────────────────────

	// DefaultScaleUpCooldown prevents back-to-back scale-up events.
	DefaultScaleUpCooldown = 60 * time.Second

	// DefaultScaleDownCooldown prevents back-to-back scale-down events and is
	// also used as the minimum node age before a node is eligible for scale-down.
	DefaultScaleDownCooldown = 60 * time.Second
)

// Package lifecycle — idle node detection and full removal pipeline.
//
// FindIdleNodes identifies worker nodes that are consuming fewer resources
// than the idle threshold and carry no workload-critical pods.
//
// RemoveNode executes the full cordon → drain → k8s-delete → docker-rm
// sequence so no step is left to the caller.
package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── NodeLifecycleManager ─────────────────────────────────────────────────────

// NodeLifecycleManager combines idle-node detection with the full
// cordon → drain → delete → docker-rm removal pipeline.
type NodeLifecycleManager struct {
	Client  kubernetes.Interface
	Drainer *NodeDrainer
	Cleanup *NodeCleanup

	// CPUIdleThresholdPercent is the per-node CPU utilisation (%) below which
	// a node is considered idle. Default: 20.
	CPUIdleThresholdPercent float64
}

// NewNodeLifecycleManager returns a manager wired to the given client.
func NewNodeLifecycleManager(client kubernetes.Interface) *NodeLifecycleManager {
	return &NodeLifecycleManager{
		Client:                  client,
		Drainer:                 NewNodeDrainer(client),
		Cleanup:                 NewNodeCleanup(client),
		CPUIdleThresholdPercent: 20.0,
	}
}

// ─── FindIdleNodes ────────────────────────────────────────────────────────────

// FindIdleNodes returns the subset of nodes from ClusterState that satisfy
// BOTH idle criteria:
//
//  1. CPU utilisation < CPUIdleThresholdPercent (default 20 %).
//  2. No running pods on the node, OR only system pods (kube-system namespace
//     / system-critical priority class).
//
// Cordoned or not-Ready nodes are skipped.
func (m *NodeLifecycleManager) FindIdleNodes(
	ctx context.Context,
	state *monitor.ClusterState,
) []monitor.NodeInfo {
	nodes := state.GetNodes()
	var idle []monitor.NodeInfo

	for _, n := range nodes {
		if !n.Ready || n.Unschedulable {
			continue
		}

		// ── Criterion 1: CPU utilisation < threshold ──────────────────────────
		alloc := n.AllocatableCPU.MilliValue()
		if alloc == 0 {
			continue
		}
		utilPct := float64(n.UsedCPU.MilliValue()) / float64(alloc) * 100
		if utilPct >= m.CPUIdleThresholdPercent {
			continue
		}

		// ── Criterion 2: No pods, or only system pods ─────────────────────────
		hasWorkload, err := m.nodeHasWorkloadPods(ctx, n.Name)
		if err != nil {
			log.Printf("[LifecycleManager] pod check for %s: %v — skipping", n.Name, err)
			continue
		}
		if hasWorkload {
			continue
		}

		log.Printf("[LifecycleManager] node %s is idle (cpu=%.1f%% < %.0f%%)",
			n.Name, utilPct, m.CPUIdleThresholdPercent)
		idle = append(idle, n)
	}

	return idle
}

// nodeHasWorkloadPods returns true when the node has at least one pod that is:
//   - not Succeeded / Failed (i.e. still running or pending)
//   - not in the kube-system namespace
//   - not owned by a DaemonSet
//   - not a static (mirror) pod
//
// Pods that fail all four checks are classified as "system-only" and do not
// block scale-down.
func (m *NodeLifecycleManager) nodeHasWorkloadPods(ctx context.Context, nodeName string) (bool, error) {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pods, err := m.Client.CoreV1().Pods(corev1.NamespaceAll).List(listCtx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return false, fmt.Errorf("list pods on %s: %w", nodeName, err)
	}

	for _, pod := range pods.Items {
		// Already finished — does not count as a running workload.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// System namespace — not a user workload.
		if pod.Namespace == metav1.NamespaceSystem {
			continue
		}
		// DaemonSet pods live on every node by design.
		if isDaemonSetPod(pod) {
			continue
		}
		// Static (mirror) pods are not API-managed.
		if isStaticPod(pod) {
			continue
		}
		// System-critical priority class pods.
		if pod.Spec.PriorityClassName == "system-cluster-critical" ||
			pod.Spec.PriorityClassName == "system-node-critical" {
			continue
		}
		// Found a real workload pod.
		return true, nil
	}
	return false, nil
}

// ─── RemoveNode ───────────────────────────────────────────────────────────────

// RemoveNode executes the full removal pipeline for a node:
//
//  1. Cordon  — mark node unschedulable (no new pods scheduled).
//  2. Drain   — evict all evictable pods via the policy/v1 Eviction API.
//  3. Delete  — remove the Node object from the Kubernetes API.
//  4. Docker  — remove the underlying Kind container (docker rm -f).
//
// Only nodes annotated with AnnotationManagedBy == ManagedByValue are
// eligible for deletion (safety guard in NodeCleanup.Cleanup).
func (m *NodeLifecycleManager) RemoveNode(ctx context.Context, nodeName string) error {
	log.Printf("[LifecycleManager] RemoveNode %s — starting cordon→drain→delete→docker-rm", nodeName)

	// ── 1. Cordon ─────────────────────────────────────────────────────────────
	if err := m.cordon(ctx, nodeName); err != nil {
		return fmt.Errorf("cordon %s: %w", nodeName, err)
	}
	log.Printf("[LifecycleManager] %s cordoned", nodeName)

	// ── 2. Drain (Eviction API) ───────────────────────────────────────────────
	if err := m.Drainer.Drain(ctx, nodeName); err != nil {
		return fmt.Errorf("drain %s: %w", nodeName, err)
	}
	log.Printf("[LifecycleManager] %s drained", nodeName)

	// ── 3. Delete Node object ─────────────────────────────────────────────────
	if err := m.Cleanup.Cleanup(ctx, nodeName); err != nil {
		return fmt.Errorf("k8s delete %s: %w", nodeName, err)
	}
	log.Printf("[LifecycleManager] %s deleted from Kubernetes", nodeName)

	// ── 4. Remove underlying Kind container ───────────────────────────────────
	// In Kind the Kubernetes node name equals the Docker container name.
	if err := removeKindContainer(ctx, nodeName); err != nil {
		// Non-fatal: the node is already gone from Kubernetes.
		log.Printf("[LifecycleManager] docker rm %s: %v (non-fatal)", nodeName, err)
	} else {
		log.Printf("[LifecycleManager] Kind container %s removed", nodeName)
	}

	log.Printf("[LifecycleManager] RemoveNode %s — complete", nodeName)
	return nil
}

// cordon patches node.Spec.Unschedulable = true via a live Get + Update.
func (m *NodeLifecycleManager) cordon(ctx context.Context, nodeName string) error {
	node, err := m.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}
	if node.Spec.Unschedulable {
		return nil // already cordoned
	}
	node.Spec.Unschedulable = true
	_, err = m.Client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// removeKindContainer calls `docker rm -f <containerName>` to destroy the
// underlying Kind node container after the Kubernetes Node object is deleted.
func removeKindContainer(ctx context.Context, containerName string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerName)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker rm -f %s: %w — %s", containerName, err, buf.String())
	}
	return nil
}

// ─── Pod classification helpers ───────────────────────────────────────────────

// isDaemonSetPod returns true when the pod is owned by a DaemonSet.
func isDaemonSetPod(pod corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isStaticPod returns true for mirror (static) pods.
func isStaticPod(pod corev1.Pod) bool {
	_, ok := pod.Annotations[utils.AnnotationManagedBy]
	if ok {
		return false // our pods, not static
	}
	_, ok = pod.Annotations["kubernetes.io/config.mirror"]
	return ok
}

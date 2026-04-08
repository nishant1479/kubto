// Package lifecycle handles graceful pod eviction from a node before removal.
package lifecycle

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// NodeDrainer evicts all evictable pods from a node, then waits until they
// have all terminated before returning.
type NodeDrainer struct {
	Client  kubernetes.Interface
	Timeout time.Duration
}

// NewNodeDrainer returns a drainer with the project-wide drain timeout.
func NewNodeDrainer(client kubernetes.Interface) *NodeDrainer {
	return &NodeDrainer{
		Client:  client,
		Timeout: utils.DefaultDrainTimeoutSeconds,
	}
}

// Drain evicts all non-daemon, non-static, non-completed pods from nodeName
// and waits until they are fully terminated.
func (d *NodeDrainer) Drain(ctx context.Context, nodeName string) error {
	log.Printf("[NodeDrainer] draining node %s", nodeName)

	pods, err := d.Client.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return fmt.Errorf("list pods on %s: %w", nodeName, err)
	}

	for i := range pods.Items {
		pod := pods.Items[i]
		if shouldSkip(pod) {
			log.Printf("[NodeDrainer] skipping %s/%s (%s)", pod.Namespace, pod.Name, skipReason(pod))
			continue
		}
		if err := d.evictPod(ctx, pod); err != nil {
			// Log but continue — a single PDB-blocked pod should not stop the
			// rest of the drain.
			log.Printf("[NodeDrainer] evict %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}

	return d.waitForPodsGone(ctx, nodeName)
}

// evictPod sends a policy/v1 Eviction request for the given pod.
func (d *NodeDrainer) evictPod(ctx context.Context, pod corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{},
	}

	err := d.Client.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction)
	switch {
	case err == nil:
		log.Printf("[NodeDrainer] evicted %s/%s", pod.Namespace, pod.Name)
		return nil
	case apierrors.IsNotFound(err):
		return nil // already gone
	case apierrors.IsTooManyRequests(err):
		// PDB is preventing eviction; surface as a non-fatal error.
		return fmt.Errorf("PDB blocking eviction of %s/%s", pod.Namespace, pod.Name)
	default:
		return err
	}
}

// waitForPodsGone polls until all evictable pods have terminated or the drain
// timeout is exceeded.
func (d *NodeDrainer) waitForPodsGone(ctx context.Context, nodeName string) error {
	drainCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	return wait.PollUntilContextCancel(drainCtx, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			pods, err := d.Client.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			if err != nil {
				return false, nil // transient error — retry
			}
			remaining := 0
			for _, p := range pods.Items {
				if !shouldSkip(p) {
					remaining++
				}
			}
			if remaining == 0 {
				log.Printf("[NodeDrainer] node %s fully drained", nodeName)
				return true, nil
			}
			log.Printf("[NodeDrainer] %d pod(s) still terminating on %s", remaining, nodeName)
			return false, nil
		},
	)
}

// ─── Pod classification helpers ───────────────────────────────────────────────

// shouldSkip returns true for pods that must not be evicted.
func shouldSkip(pod corev1.Pod) bool {
	// Pods that have already finished.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}
	// Static (mirror) pods — managed by kubelet, not the API server.
	if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
		return true
	}
	// DaemonSet pods run on every node by design; evicting them is pointless.
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// skipReason returns a short human-readable explanation for why a pod is skipped.
func skipReason(pod corev1.Pod) string {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return "completed"
	}
	if _, ok := pod.Annotations["kubernetes.io/config.mirror"]; ok {
		return "static-pod"
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return "daemonset"
		}
	}
	return "unknown"
}

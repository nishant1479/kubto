// Package lifecycle handles deletion of drained nodes from the cluster.
package lifecycle

import (
	"context"
	"fmt"
	"log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// NodeCleanup deletes a fully-drained node object from the Kubernetes API and
// optionally triggers a provider-level cleanup hook (e.g. VM termination).
type NodeCleanup struct {
	Client kubernetes.Interface
	// ProviderCleanupHook is an optional callback invoked after the Kubernetes
	// Node object is deleted. It receives the node name so the caller can
	// terminate the underlying infrastructure resource.
	// If nil, no provider hook is executed.
	ProviderCleanupHook func(ctx context.Context, nodeName string) error
}

// NewNodeCleanup returns a NodeCleanup wired to the given client.
func NewNodeCleanup(client kubernetes.Interface) *NodeCleanup {
	return &NodeCleanup{Client: client}
}

// Cleanup deletes the Node object from the cluster and runs the provider hook.
//
// Safety: only nodes annotated with AnnotationManagedBy == ManagedByValue are
// eligible for deletion. Any attempt to delete an unmanaged node returns an
// error without touching the API server.
func (c *NodeCleanup) Cleanup(ctx context.Context, nodeName string) error {
	node, err := c.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}

	// Safety guard: refuse to delete nodes we did not provision.
	if node.Annotations[utils.AnnotationManagedBy] != utils.ManagedByValue {
		return fmt.Errorf(
			"node %s is not managed by this autoscaler (missing annotation %s=%s) — refusing to delete",
			nodeName, utils.AnnotationManagedBy, utils.ManagedByValue,
		)
	}

	if err := c.Client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete node %s: %w", nodeName, err)
	}
	log.Printf("[NodeCleanup] deleted node %s from cluster", nodeName)

	if c.ProviderCleanupHook != nil {
		if err := c.ProviderCleanupHook(ctx, nodeName); err != nil {
			// Non-fatal: the node is already gone from Kubernetes; log and move on.
			log.Printf("[NodeCleanup] provider hook error for %s: %v", nodeName, err)
		}
	}

	return nil
}

// Package monitor — NodeWatcher uses a client-go informer to stream node events
// and keep ClusterState.Nodes current without polling.
package monitor

import (
	"context"
	"log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NodeWatcher keeps ClusterState.Nodes in sync with the API server using
// an informer backed by a shared list-watch.
type NodeWatcher struct {
	client     kubernetes.Interface
	state      *ClusterState
	store      cache.Store
	controller cache.Controller
}

// NewNodeWatcher wires up the informer and returns a ready-to-run NodeWatcher.
func NewNodeWatcher(client kubernetes.Interface, state *ClusterState) *NodeWatcher {
	nw := &NodeWatcher{client: client, state: state}

	lw := cache.NewListWatchFromClient(
		client.CoreV1().RESTClient(),
		"nodes",
		corev1.NamespaceAll,
		fields.Everything(),
	)

	nw.store, nw.controller = cache.NewInformer(
		lw,
		&corev1.Node{},
		0, // no periodic resync
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if node, ok := obj.(*corev1.Node); ok {
					nw.state.UpdateNode(nodeToInfo(node))
					log.Printf("[NodeWatcher] node added: %s (ready=%v)", node.Name, isReady(node))
				}
			},
			UpdateFunc: func(_, obj interface{}) {
				if node, ok := obj.(*corev1.Node); ok {
					nw.state.UpdateNode(nodeToInfo(node))
				}
			},
			DeleteFunc: func(obj interface{}) {
				// obj can be a *corev1.Node or a cache.DeletedFinalStateUnknown
				// (tombstone) when the watch event is missed.
				switch v := obj.(type) {
				case *corev1.Node:
					nw.state.RemoveNode(v.Name)
					log.Printf("[NodeWatcher] node removed: %s", v.Name)
				case cache.DeletedFinalStateUnknown:
					if node, ok := v.Obj.(*corev1.Node); ok {
						nw.state.RemoveNode(node.Name)
						log.Printf("[NodeWatcher] node removed (tombstone): %s", node.Name)
					}
				}
			},
		},
	)

	return nw
}

// Run starts the informer loop. Blocks until ctx is cancelled.
func (nw *NodeWatcher) Run(ctx context.Context) {
	log.Println("[NodeWatcher] starting")
	nw.controller.Run(ctx.Done())
	log.Println("[NodeWatcher] stopped")
}

// HasSynced returns true once the initial list has been processed.
func (nw *NodeWatcher) HasSynced() bool {
	return nw.controller.HasSynced()
}

// ─── Node → NodeInfo conversion ───────────────────────────────────────────────

// nodeToInfo extracts scheduling-relevant fields from a corev1.Node.
//
// Note: UsedCPU and UsedMemory are initialised to zero here. The resource
// estimator computes actual usage by summing pod requests across the cluster.
func nodeToInfo(node *corev1.Node) NodeInfo {
	allocCPU := node.Status.Allocatable.Cpu().DeepCopy()
	allocMem := node.Status.Allocatable.Memory().DeepCopy()

	return NodeInfo{
		Name:              node.Name,
		AllocatableCPU:    allocCPU,
		AllocatableMemory: allocMem,
		// Usage is computed by the estimator from pod requests, not node metrics,
		// so we seed with zero here.
		UsedCPU:       *resource.NewMilliQuantity(0, resource.DecimalSI),
		UsedMemory:    *resource.NewQuantity(0, resource.BinarySI),
		Ready:         isReady(node),
		Unschedulable: node.Spec.Unschedulable,
		CreatedAt:     node.CreationTimestamp.Time,
	}
}

// isReady returns true when the NodeReady condition is True.
func isReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

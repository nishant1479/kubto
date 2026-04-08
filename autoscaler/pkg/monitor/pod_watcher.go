// Package monitor — PodWatcher uses a client-go informer to stream pod events
// and keep ClusterState.PendingPods current without polling.
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

// PodWatcher keeps ClusterState.PendingPods in sync with the API server using
// an informer backed by a shared list-watch.
type PodWatcher struct {
	client     kubernetes.Interface
	state      *ClusterState
	store      cache.Store
	controller cache.Controller
}

// NewPodWatcher wires up the informer and returns a ready-to-run PodWatcher.
func NewPodWatcher(client kubernetes.Interface, state *ClusterState) *PodWatcher {
	pw := &PodWatcher{client: client, state: state}

	lw := cache.NewListWatchFromClient(
		client.CoreV1().RESTClient(),
		"pods",
		corev1.NamespaceAll, // watch all namespaces
		fields.Everything(),
	)

	pw.store, pw.controller = cache.NewInformer(
		lw,
		&corev1.Pod{},
		0, // no periodic resync — rely purely on watch events
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(_ interface{}) { pw.sync() },
			UpdateFunc: func(_, _ interface{}) { pw.sync() },
			DeleteFunc: func(_ interface{}) { pw.sync() },
		},
	)

	return pw
}

// Run starts the informer loop. Blocks until ctx is cancelled.
func (pw *PodWatcher) Run(ctx context.Context) {
	log.Println("[PodWatcher] starting")
	pw.controller.Run(ctx.Done())
	log.Println("[PodWatcher] stopped")
}

// HasSynced returns true once the initial list has been processed and the
// local cache is consistent with the API server.
func (pw *PodWatcher) HasSynced() bool {
	return pw.controller.HasSynced()
}

// sync rebuilds the PendingPods list from the informer's local cache.
// It is safe to call concurrently from multiple goroutines.
func (pw *PodWatcher) sync() {
	items := pw.store.List()
	pods := make([]PodInfo, 0, len(items))

	for _, obj := range items {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		// Only track pods that Kubernetes reports as Pending.
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		pods = append(pods, podToInfo(pod))
	}

	pw.state.SetPendingPods(pods)
	log.Printf("[PodWatcher] cache synced — %d pending pod(s)", len(pods))
}

// ─── Pod → PodInfo conversion ─────────────────────────────────────────────────

// podToInfo extracts scheduling-relevant fields from a corev1.Pod.
func podToInfo(pod *corev1.Pod) PodInfo {
	cpuReq := resource.Quantity{}
	memReq := resource.Quantity{}

	// Aggregate requests across all containers (init containers are ignored
	// because they do not run simultaneously).
	for _, c := range pod.Spec.Containers {
		if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuReq.Add(v)
		}
		if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			memReq.Add(v)
		}
	}

	// Extract the PodScheduled condition for scheduling diagnostics.
	var condReason, condMessage string
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled {
			condReason = cond.Reason
			condMessage = cond.Message
			break
		}
	}

	return PodInfo{
		Name:             pod.Name,
		Namespace:        pod.Namespace,
		CPURequest:       cpuReq,
		MemoryRequest:    memReq,
		Status:           string(pod.Status.Phase),
		ConditionReason:  condReason,
		ConditionMessage: condMessage,
	}
}

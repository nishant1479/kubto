// Package controller wires all autoscaler components into a single reconcile loop.
//
// Loop flow (every 10 seconds):
//
//	state         := informer cache (live)
//	unschedulable := detector.DetectUnschedulablePods(state.PendingPods)
//
//	if len(unschedulable) > 0:
//	    demand   := estimator.Estimate(unschedulable)
//	    decision := decisionEngine.Decide(demand, state)
//	    if decision.ScaleUp:
//	        provisioner.CreateNode() × decision.Count
//	else:
//	    idleNodes := lifecycle.FindIdleNodes(state)
//	    if idleNodes:
//	        lifecycle.RemoveNode(idleNodes[0])
package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"

	"github.com/kuber-scale/autoscaler/pkg/decision"
	"github.com/kuber-scale/autoscaler/pkg/detector"
	"github.com/kuber-scale/autoscaler/pkg/estimator"
	"github.com/kuber-scale/autoscaler/pkg/lifecycle"
	"github.com/kuber-scale/autoscaler/pkg/monitor"
	"github.com/kuber-scale/autoscaler/pkg/provisioner"
	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// AutoscalerController orchestrates the full autoscaling loop.
type AutoscalerController struct {
	client kubernetes.Interface
	state  *monitor.ClusterState

	podWatcher  *monitor.PodWatcher
	nodeWatcher *monitor.NodeWatcher

	detector         *detector.UnschedulableDetector
	estimator        *estimator.ResourceEstimator
	decisionEngine   *decision.DecisionEngine
	provisioner      provisioner.NodeProvisioner
	lifecycleManager *lifecycle.NodeLifecycleManager

	// queue provides rate-limiting for future event-driven triggers.
	queue workqueue.RateLimitingInterface

	// reconcileInterval controls how often the control loop fires (10 s).
	reconcileInterval time.Duration

	// reconcileMu prevents a slow reconcile iteration from overlapping with
	// the next tick.  TryLock is used so ticks are never blocked.
	reconcileMu sync.Mutex

	// Cooldown timestamps prevent scale-up/scale-down thrashing.
	lastScaleUp   time.Time
	lastScaleDown time.Time
}

// NewAutoscalerController wires all subsystems and returns the controller.
func NewAutoscalerController(client kubernetes.Interface) *AutoscalerController {
	state := monitor.NewClusterState()

	// criticalPodFn is injected into the policy so the decision package
	// stays decoupled from the Kubernetes client.
	criticalPodFn := makeCriticalPodChecker(client)
	policy := decision.DefaultThresholdPolicy(criticalPodFn)

	return &AutoscalerController{
		client:            client,
		state:             state,
		podWatcher:        monitor.NewPodWatcher(client, state),
		nodeWatcher:       monitor.NewNodeWatcher(client, state),
		detector:          detector.NewUnschedulableDetector(),
		estimator:         estimator.NewResourceEstimator(),
		decisionEngine:    decision.NewDecisionEngine(policy),
		provisioner:       provisioner.NewKindNodeProvisioner(client, ""),
		lifecycleManager:  lifecycle.NewNodeLifecycleManager(client),
		queue:             workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		reconcileInterval: 10 * time.Second, // run loop every 10 seconds
	}
}

// Run starts the informers, waits for cache sync, then enters the reconcile
// loop.  Returns when ctx is cancelled.
func (c *AutoscalerController) Run(ctx context.Context) error {
	log.Println("[Controller] starting")

	// Start informers concurrently.
	go c.podWatcher.Run(ctx)
	go c.nodeWatcher.Run(ctx)

	// Block until both informer caches are populated.
	log.Println("[Controller] waiting for informer cache sync …")
	for {
		if c.podWatcher.HasSynced() && c.nodeWatcher.HasSynced() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	log.Println("[Controller] caches synced — entering 10 s reconcile loop")

	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.queue.ShutDown()
			log.Println("[Controller] shutting down")
			return ctx.Err()
		case <-ticker.C:
			// Run reconcile in the same goroutine but protected by TryLock so
			// a slow iteration never blocks the ticker and causes pile-up.
			go c.reconcile(ctx)
		}
	}
}

// ─── Reconcile loop ───────────────────────────────────────────────────────────

// reconcile executes one full iteration of the autoscaling control loop.
//
// Implements the exact flow from the spec:
//
//	state         := monitor.GetClusterState()
//	unschedulable := detector.Detect(state.PendingPods)
//
//	if len(unschedulable) > 0:
//	    demand   := estimator.Estimate(unschedulable)
//	    decision := decisionEngine.Evaluate(demand, state)
//	    if decision.ScaleUp:
//	        provisioner.CreateNodes(decision.Count)
//	else:
//	    idleNodes := lifecycle.FindIdleNodes(state)
//	    if idleNodes:
//	        lifecycle.RemoveNode(idleNodes[0])
func (c *AutoscalerController) reconcile(ctx context.Context) {
	// ── Concurrency guard ────────────────────────────────────────────────────
	// TryLock returns false immediately if another reconcile is still running.
	if !c.reconcileMu.TryLock() {
		log.Println("[Controller] reconcile: previous iteration still running — skipping tick")
		return
	}
	defer c.reconcileMu.Unlock()

	start := time.Now()
	log.Println("[Controller] reconcile — start")

	// ── 1. Get cluster state (live from informer cache) ───────────────────────
	state := c.state // maintained continuously by PodWatcher / NodeWatcher

	// ── 2. Detect unschedulable pods ─────────────────────────────────────────
	unschedulable := c.detector.DetectUnschedulablePods(state.GetPendingPods())

	if len(unschedulable) > 0 {
		// ── Branch A: pods are waiting — try to scale up ──────────────────────
		log.Printf("[Controller] %d unschedulable pod(s) detected — evaluating scale-up", len(unschedulable))

		// 3. Estimate resource demand.
		demand := c.estimator.Estimate(state, unschedulable)

		// 4. Decision.
		dec := c.decisionEngine.Decide(demand, state)

		// 5. Act on scale-up decision.
		if dec.ScaleUp {
			c.handleScaleUp(ctx, dec)
		} else {
			log.Printf("[Controller] scale-up evaluation: no-op — %s", dec.Reason)
		}
	} else {
		// ── Branch B: no pending pods — look for idle nodes to remove ─────────
		log.Println("[Controller] no unschedulable pods — checking for idle nodes")

		idleNodes := c.lifecycleManager.FindIdleNodes(ctx, state)
		if len(idleNodes) > 0 {
			log.Printf("[Controller] %d idle node(s) found — removing %s",
				len(idleNodes), idleNodes[0].Name)
			c.handleScaleDown(ctx, idleNodes[0].Name)
		} else {
			log.Println("[Controller] no idle nodes — nothing to do")
		}
	}

	log.Printf("[Controller] reconcile — done in %s", time.Since(start).Round(time.Millisecond))
}

// ─── Scale-up handler ─────────────────────────────────────────────────────────

// handleScaleUp provisions dec.Count nodes, respecting the scale-up cooldown.
func (c *AutoscalerController) handleScaleUp(ctx context.Context, dec decision.Decision) {
	if time.Since(c.lastScaleUp) < utils.DefaultScaleUpCooldown {
		log.Printf("[Controller] scale-up cooldown active (%s remaining) — skipping",
			utils.DefaultScaleUpCooldown-time.Since(c.lastScaleUp))
		return
	}

	log.Printf("[Controller] SCALE UP — provisioning %d node(s)", dec.Count)
	successCount := 0
	for i := 0; i < dec.Count; i++ {
		name, err := c.provisioner.CreateNode(ctx)
		if err != nil {
			log.Printf("[Controller] provision node %d/%d failed: %v", i+1, dec.Count, err)
			continue
		}
		log.Printf("[Controller] provisioned node %s (%d/%d)", name, i+1, dec.Count)
		successCount++
	}
	if successCount > 0 {
		c.lastScaleUp = time.Now()
		log.Printf("[Controller] scale-up complete — %d/%d node(s) added", successCount, dec.Count)
	}
}

// ─── Scale-down handler ───────────────────────────────────────────────────────

// handleScaleDown removes a single idle node, respecting the scale-down cooldown.
// It delegates the full cordon→drain→delete→docker-rm pipeline to
// NodeLifecycleManager.RemoveNode.
func (c *AutoscalerController) handleScaleDown(ctx context.Context, nodeName string) {
	if time.Since(c.lastScaleDown) < utils.DefaultScaleDownCooldown {
		log.Printf("[Controller] scale-down cooldown active (%s remaining) — skipping",
			utils.DefaultScaleDownCooldown-time.Since(c.lastScaleDown))
		return
	}

	log.Printf("[Controller] SCALE DOWN — removing node %s", nodeName)
	if err := c.lifecycleManager.RemoveNode(ctx, nodeName); err != nil {
		log.Printf("[Controller] RemoveNode %s failed: %v", nodeName, err)
		return
	}
	c.lastScaleDown = time.Now()
	log.Printf("[Controller] scale-down complete — node %s removed", nodeName)
}

// ─── Critical-pod checker ─────────────────────────────────────────────────────

// makeCriticalPodChecker returns a closure that queries the live API to check
// whether a node carries any pods with a system-critical priority class or
// residing in kube-system.  Injected into ThresholdPolicy at startup.
func makeCriticalPodChecker(client kubernetes.Interface) func(string) bool {
	return func(nodeName string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pods, err := client.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		if err != nil {
			// Fail safe: assume critical pods exist.
			log.Printf("[Controller] critical-pod check for %s: %v — treating as critical", nodeName, err)
			return true
		}
		for _, pod := range pods.Items {
			if pod.Spec.PriorityClassName == "system-cluster-critical" ||
				pod.Spec.PriorityClassName == "system-node-critical" ||
				pod.Namespace == metav1.NamespaceSystem {
				return true
			}
		}
		return false
	}
}

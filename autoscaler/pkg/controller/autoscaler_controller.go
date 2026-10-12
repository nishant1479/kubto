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
	"github.com/kuber-scale/autoscaler/pkg/metrics"
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
	metrics          *metrics.Metrics

	// queue provides rate-limiting for future event-driven triggers.
	queue workqueue.RateLimitingInterface

	// reconcileInterval controls how often the control loop fires (default 10 s).
	reconcileInterval time.Duration

	// reconcileMu prevents a slow reconcile iteration from overlapping with
	// the next tick. TryLock is used so ticks are never blocked.
	reconcileMu sync.Mutex

	// Cooldown durations prevent scale-up/scale-down thrashing.
	scaleUpCooldown   time.Duration
	scaleDownCooldown time.Duration

	// Cooldown timestamps
	lastScaleUp   time.Time
	lastScaleDown time.Time

	// inFlightProvisioning tracks count of nodes currently being provisioned.
	inFlightProvisioning int
	inFlightMu           sync.Mutex

	// scaleUpFailures tracks consecutive provisioning failures for exponential backoff.
	scaleUpFailures int
}

// NewAutoscalerController wires all subsystems and returns the controller.
func NewAutoscalerController(client kubernetes.Interface) *AutoscalerController {
	state := monitor.NewClusterState()

	// criticalPodFn is injected into the policy so the decision package
	// stays decoupled from the Kubernetes client.
	criticalPodFn := makeCriticalPodChecker(client)
	policy := decision.DefaultThresholdPolicy(criticalPodFn)

	prov := provisioner.NewKindNodeProvisioner(client, "")
	lm := lifecycle.NewNodeLifecycleManager(client)
	lm.Cleanup.ProviderCleanupHook = prov.TerminateNode

	return &AutoscalerController{
		client:            client,
		state:             state,
		podWatcher:        monitor.NewPodWatcher(client, state),
		nodeWatcher:       monitor.NewNodeWatcher(client, state),
		detector:          detector.NewUnschedulableDetector(),
		estimator:         estimator.NewResourceEstimator(),
		decisionEngine:    decision.NewDecisionEngine(policy),
		provisioner:       prov,
		lifecycleManager:  lm,
		metrics:           metrics.Default(),
		queue:             workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		reconcileInterval: 10 * time.Second,
		scaleUpCooldown:   utils.DefaultScaleUpCooldown,
		scaleDownCooldown: utils.DefaultScaleDownCooldown,
	}
}

// SetProvisioner replaces the node provisioner (useful for tests and alternative providers).
func (c *AutoscalerController) SetProvisioner(p provisioner.NodeProvisioner) {
	c.provisioner = p
	if c.lifecycleManager != nil && c.lifecycleManager.Cleanup != nil {
		c.lifecycleManager.Cleanup.ProviderCleanupHook = p.TerminateNode
	}
}

// SetMetrics replaces the metrics collector (useful for isolated tests).
func (c *AutoscalerController) SetMetrics(m *metrics.Metrics) {
	c.metrics = m
}

// SetScaleUpCooldown configures the scale-up cooldown duration.
func (c *AutoscalerController) SetScaleUpCooldown(d time.Duration) {
	c.scaleUpCooldown = d
}

// SetScaleDownCooldown configures the scale-down cooldown duration.
func (c *AutoscalerController) SetScaleDownCooldown(d time.Duration) {
	c.scaleDownCooldown = d
}

// SetReconcileInterval configures how frequently the loop executes.
func (c *AutoscalerController) SetReconcileInterval(d time.Duration) {
	c.reconcileInterval = d
}

// GetClusterState returns the in-memory cluster state.
func (c *AutoscalerController) GetClusterState() *monitor.ClusterState {
	return c.state
}

// Run starts the informers, waits for cache sync, then enters the reconcile
// loop. Returns when ctx is cancelled.
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
	log.Println("[Controller] caches synced — entering reconcile loop")

	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.queue.ShutDown()
			log.Println("[Controller] shutting down")
			return ctx.Err()
		case <-ticker.C:
			go c.reconcile(ctx)
		}
	}
}

// Reconcile executes one full iteration of the autoscaling control loop synchronously.
// Exported for unit and integration testing.
func (c *AutoscalerController) Reconcile(ctx context.Context) {
	c.reconcile(ctx)
}

// ─── Reconcile loop ───────────────────────────────────────────────────────────

func (c *AutoscalerController) reconcile(ctx context.Context) {
	if !c.reconcileMu.TryLock() {
		log.Println("[Controller] reconcile: previous iteration still running — skipping tick")
		return
	}
	defer c.reconcileMu.Unlock()

	start := time.Now()
	log.Println("[Controller] reconcile — start")

	state := c.state

	// Update active nodes gauge
	nodes := state.GetNodes()
	readyCount := 0
	for _, n := range nodes {
		if n.Ready && !n.Unschedulable {
			readyCount++
		}
	}
	if c.metrics != nil {
		c.metrics.SetActiveNodes(readyCount)
	}

	// 2. Detect unschedulable pods
	unschedulable := c.detector.DetectUnschedulablePods(state.GetPendingPods())
	if c.metrics != nil {
		c.metrics.SetPendingPods(len(unschedulable))
	}

	if len(unschedulable) > 0 {
		// ── Branch A: pods are waiting — evaluate scale-up ───────────────────
		log.Printf("[Controller] %d unschedulable pod(s) detected — evaluating scale-up", len(unschedulable))

		report := c.estimator.EstimateDetailed(state, unschedulable)
		demand := report.Delta

		if c.metrics != nil {
			c.metrics.SetEstimatedRequiredNodes(report.NodesNeeded)
			c.metrics.SetInfeasiblePods(len(report.InfeasiblePods))
		}
		for name, reason := range report.InfeasiblePods {
			log.Printf("[Controller] infeasible pod %s: %s", name, reason)
		}

		if report.NodesNeeded == 0 {
			if c.metrics != nil {
				c.metrics.RecordDecision("noop")
			}
			if len(report.InfeasiblePods) > 0 {
				log.Printf("[Controller] %d infeasible pod(s) and no feasible template placement — skipping scale-up to avoid a provision loop",
					len(report.InfeasiblePods))
			} else {
				log.Printf("[Controller] scale-up evaluation: no-op — pending pods fit existing capacity")
			}
			// Skip scale-down while unschedulable pods remain (even if infeasible).
		} else {
			dec := c.decisionEngine.Decide(demand, state)
			if c.metrics != nil {
				if dec.ScaleUp {
					c.metrics.RecordDecision("scale_up")
				} else {
					c.metrics.RecordDecision("noop")
				}
			}

			if dec.ScaleUp {
				if report.NodesNeeded < dec.Count {
					dec.Count = report.NodesNeeded
				}
				c.handleScaleUp(ctx, dec)
			} else {
				log.Printf("[Controller] scale-up evaluation: no-op — %s", dec.Reason)
			}
		}
	} else {
		// ── Branch B: no pending pods — look for idle nodes to remove ─────────
		log.Println("[Controller] no unschedulable pods — checking for idle nodes")
		if c.metrics != nil {
			c.metrics.SetEstimatedRequiredNodes(0)
			c.metrics.SetInfeasiblePods(0)
		}

		idleNodes := c.lifecycleManager.FindIdleNodes(ctx, state)
		if len(idleNodes) > 0 {
			if c.metrics != nil {
				c.metrics.RecordDecision("scale_down")
			}
			log.Printf("[Controller] %d idle node(s) found — removing %s",
				len(idleNodes), idleNodes[0].Name)
			c.handleScaleDown(ctx, idleNodes[0].Name)
		} else {
			if c.metrics != nil {
				c.metrics.RecordDecision("noop")
			}
			log.Println("[Controller] no idle nodes — nothing to do")
		}
	}

	duration := time.Since(start)
	if c.metrics != nil {
		c.metrics.RecordReconcileDuration(duration)
	}
	log.Printf("[Controller] reconcile — done in %s", duration.Round(time.Millisecond))
}

// ─── Scale-up handler ─────────────────────────────────────────────────────────

func (c *AutoscalerController) handleScaleUp(ctx context.Context, dec decision.Decision) {
	// 1. Check cooldown
	if time.Since(c.lastScaleUp) < c.scaleUpCooldown {
		log.Printf("[Controller] scale-up cooldown active (%s remaining) — skipping",
			c.scaleUpCooldown-time.Since(c.lastScaleUp))
		return
	}

	// 2. Prevent duplicate provisioning: account for in-flight and not-yet-ready nodes
	c.inFlightMu.Lock()
	unreadyCount := 0
	for _, n := range c.state.GetNodes() {
		if !n.Ready {
			unreadyCount++
		}
	}
	alreadyProvisioning := c.inFlightProvisioning + unreadyCount
	toProvision := dec.Count - alreadyProvisioning
	if toProvision <= 0 {
		c.inFlightMu.Unlock()
		log.Printf("[Controller] %d node(s) already in-flight or joining (needed %d) — skipping duplicate scale-up",
			alreadyProvisioning, dec.Count)
		return
	}
	c.inFlightProvisioning += toProvision
	c.inFlightMu.Unlock()

	defer func() {
		c.inFlightMu.Lock()
		c.inFlightProvisioning -= toProvision
		c.inFlightMu.Unlock()
	}()

	log.Printf("[Controller] SCALE UP — provisioning %d node(s)", toProvision)
	if c.metrics != nil {
		c.metrics.IncScaleUp()
	}

	successCount := 0
	for i := 0; i < toProvision; i++ {
		provStart := time.Now()
		name, err := c.provisioner.CreateNode(ctx)
		if err != nil {
			log.Printf("[Controller] provision node %d/%d failed: %v", i+1, toProvision, err)
			if c.metrics != nil {
				c.metrics.IncProvisionFailure()
			}
			c.scaleUpFailures++
			continue
		}
		provDuration := time.Since(provStart)
		log.Printf("[Controller] provisioned node %s in %s (%d/%d)", name, provDuration.Round(time.Millisecond), i+1, toProvision)
		if c.metrics != nil {
			c.metrics.IncNodesProvisioned(1)
			c.metrics.RecordProvisionDuration(provDuration)
		}
		successCount++
	}

	if successCount > 0 {
		c.lastScaleUp = time.Now()
		c.scaleUpFailures = 0
		log.Printf("[Controller] scale-up complete — %d/%d node(s) added", successCount, toProvision)
	} else if c.scaleUpFailures > 0 {
		// Bounded exponential backoff on consecutive provisioning failures (5s, 10s, 20s, max 60s)
		backoffSeconds := 5 * (1 << (c.scaleUpFailures - 1))
		if backoffSeconds > 60 {
			backoffSeconds = 60
		}
		backoff := time.Duration(backoffSeconds) * time.Second
		c.lastScaleUp = time.Now().Add(-c.scaleUpCooldown + backoff)
		log.Printf("[Controller] provisioning failed; applying backoff cooldown of %s", backoff)
	}
}

// ─── Scale-down handler ───────────────────────────────────────────────────────

func (c *AutoscalerController) handleScaleDown(ctx context.Context, nodeName string) {
	if time.Since(c.lastScaleDown) < c.scaleDownCooldown {
		log.Printf("[Controller] scale-down cooldown active (%s remaining) — skipping",
			c.scaleDownCooldown-time.Since(c.lastScaleDown))
		return
	}

	log.Printf("[Controller] SCALE DOWN — removing node %s", nodeName)
	if c.metrics != nil {
		c.metrics.IncScaleDown()
	}

	if err := c.lifecycleManager.RemoveNode(ctx, nodeName); err != nil {
		log.Printf("[Controller] RemoveNode %s failed: %v", nodeName, err)
		return
	}

	c.lastScaleDown = time.Now()
	if c.metrics != nil {
		c.metrics.IncNodesTerminated()
	}
	log.Printf("[Controller] scale-down complete — node %s removed", nodeName)
}

// ─── Critical-pod checker ─────────────────────────────────────────────────────

func makeCriticalPodChecker(client kubernetes.Interface) func(string) bool {
	return func(nodeName string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pods, err := client.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
		})
		if err != nil {
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

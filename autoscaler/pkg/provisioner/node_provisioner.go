// Package provisioner provisions and cordons Kubernetes worker nodes in a
// Kind (Kubernetes-in-Docker) cluster.
//
// CreateNode() flow:
//  1. Obtain a kubeadm join command from the Kind control-plane container.
//  2. Start a new Docker container with the Kind node image.
//  3. Attach the container to the Kind cluster network (via --network flag).
//  4. Execute kubeadm join inside the container.
//  5. Wait until the node registers with the Kubernetes API server.
//  6. Wait until the node status reaches Ready.
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/kuber-scale/autoscaler/pkg/utils"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	// DefaultKindNodeImage is the container image used for new worker nodes.
	// Must match the image already used by the Kind cluster.
	DefaultKindNodeImage = "kindest/node:v1.29.2"

	// DefaultKindClusterName is used when no cluster name is supplied.
	DefaultKindClusterName = "kind"

	// nodeReadyTimeout is the maximum time we wait for a node to appear and
	// become Ready after kubeadm join completes.
	nodeReadyTimeout = 5 * time.Minute

	// nodeCheckInterval is the polling frequency for node status checks.
	nodeCheckInterval = 3 * time.Second

	// containerInitDelay gives /sbin/init a moment to start systemd and the
	// container runtime socket before we run kubeadm join.
	containerInitDelay = 5 * time.Second
)

// ─── NodeProvisioner interface ────────────────────────────────────────────────

// NodeProvisioner is the interface the controller uses to provision and
// prepare nodes for removal.
type NodeProvisioner interface {
	// CreateNode provisions a new worker node and returns its Kubernetes name.
	CreateNode(ctx context.Context) (string, error)
	// CordonNode marks a node unschedulable before draining.
	CordonNode(ctx context.Context, name string) error
}

// ─── KindNodeProvisioner ──────────────────────────────────────────────────────

// KindNodeProvisioner implements NodeProvisioner for Kind clusters.
//
// It uses the Docker CLI (available wherever Kind is installed) to create
// node containers and the Kubernetes API to track readiness, so no additional
// Go dependencies are required.
type KindNodeProvisioner struct {
	// Client is the Kubernetes API client.
	Client kubernetes.Interface

	// ClusterName is the Kind cluster name (default: "kind").
	// Kind names its Docker network and control-plane container after this.
	ClusterName string

	// NodeImage is the kindest/node image tag to use for new containers.
	// Must match the existing cluster's Kubernetes version.
	NodeImage string

	// NetworkName is the Docker network to attach new nodes to.
	// Defaults to ClusterName (Kind's convention).
	NetworkName string
}

// NewKindNodeProvisioner returns a provisioner configured for the named Kind cluster.
func NewKindNodeProvisioner(client kubernetes.Interface, clusterName string) *KindNodeProvisioner {
	if clusterName == "" {
		clusterName = DefaultKindClusterName
	}
	return &KindNodeProvisioner{
		Client:      client,
		ClusterName: clusterName,
		NodeImage:   DefaultKindNodeImage,
		NetworkName: clusterName, // Kind uses the cluster name as the network name.
	}
}

// ─── CreateNode ───────────────────────────────────────────────────────────────

// CreateNode implements NodeProvisioner.
//
// It provisions a new worker node inside the Kind cluster by:
//  1. Obtaining a kubeadm join command from the control-plane container.
//  2. Starting a Docker container with the Kind node image.
//  3. Connecting the container to the cluster network (--network flag).
//  4. Executing kubeadm join inside the container.
//  5. Waiting until the node registers with the Kubernetes API server.
//  6. Waiting until the node status is Ready.
func (p *KindNodeProvisioner) CreateNode(ctx context.Context) (string, error) {
	containerName := fmt.Sprintf("%s-worker-%d", p.ClusterName, time.Now().UnixNano())
	log.Printf("[KindProvisioner] CreateNode — container=%s image=%s", containerName, p.NodeImage)

	// ── Step 1: Obtain kubeadm join command from the control-plane ────────────
	joinCmd, err := p.getKubeadmJoinCommand(ctx)
	if err != nil {
		return "", fmt.Errorf("step 1 — get join command: %w", err)
	}
	log.Printf("[KindProvisioner] step 1 complete — join command acquired")

	// ── Step 2 & 3: Create container and attach to cluster network ────────────
	// The --network flag attaches the container to the Kind network in one go.
	if err := p.createNodeContainer(ctx, containerName); err != nil {
		return "", fmt.Errorf("step 2 — create container %s: %w", containerName, err)
	}
	log.Printf("[KindProvisioner] step 2–3 complete — container %s running on network %s",
		containerName, p.NetworkName)

	// ── Step 4: Execute kubeadm join inside the container ─────────────────────
	if err := p.execKubeadmJoin(ctx, containerName, joinCmd); err != nil {
		// Best-effort cleanup: remove the container so it doesn't linger.
		if rmErr := p.removeContainer(containerName); rmErr != nil {
			log.Printf("[KindProvisioner] cleanup warning: %v", rmErr)
		}
		return "", fmt.Errorf("step 4 — kubeadm join in %s: %w", containerName, err)
	}
	log.Printf("[KindProvisioner] step 4 complete — kubeadm join succeeded")

	// ── Step 5: Wait until the node appears in the Kubernetes API ─────────────
	nodeName, err := p.waitForNodeRegistration(ctx, containerName)
	if err != nil {
		return "", fmt.Errorf("step 5 — node registration: %w", err)
	}
	log.Printf("[KindProvisioner] step 5 complete — node %s registered", nodeName)

	// ── Step 6: Wait until node status = Ready ────────────────────────────────
	if err := p.WaitForNodeReady(ctx, nodeName); err != nil {
		return nodeName, fmt.Errorf("step 6 — node %s not ready: %w", nodeName, err)
	}
	log.Printf("[KindProvisioner] step 6 complete — node %s is Ready", nodeName)

	// Stamp the managed-by annotation so the cleanup safety guard works.
	if err := p.annotateNode(ctx, nodeName); err != nil {
		log.Printf("[KindProvisioner] warning: annotate node %s: %v (non-fatal)", nodeName, err)
	}

	return nodeName, nil
}

// ─── WaitForNodeReady ─────────────────────────────────────────────────────────

// WaitForNodeReady polls the Kubernetes API at nodeCheckInterval until the
// named node has condition NodeReady == True, or until the timeout expires.
//
// This is exported so the controller can call it independently if needed.
func (p *KindNodeProvisioner) WaitForNodeReady(ctx context.Context, nodeName string) error {
	log.Printf("[KindProvisioner] WaitForNodeReady — waiting on %s (timeout=%s)", nodeName, nodeReadyTimeout)

	waitCtx, cancel := context.WithTimeout(ctx, nodeReadyTimeout)
	defer cancel()

	return wait.PollUntilContextTimeout(
		waitCtx,
		nodeCheckInterval,
		nodeReadyTimeout,
		true, // immediate first check
		func(ctx context.Context) (bool, error) {
			node, err := p.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if err != nil {
				// Node may not be visible yet — treat as transient.
				log.Printf("[KindProvisioner] node %s not yet visible: %v", nodeName, err)
				return false, nil
			}
			for _, cond := range node.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
			log.Printf("[KindProvisioner] node %s exists but not yet Ready", nodeName)
			return false, nil
		},
	)
}

// ─── CordonNode ───────────────────────────────────────────────────────────────

// CordonNode marks the node as unschedulable via a live Get + Update. This
// stops the scheduler from placing new pods on it before drain begins.
func (p *KindNodeProvisioner) CordonNode(ctx context.Context, name string) error {
	node, err := p.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s: %w", name, err)
	}
	if node.Spec.Unschedulable {
		log.Printf("[KindProvisioner] node %s already cordoned", name)
		return nil
	}
	node.Spec.Unschedulable = true
	if _, err = p.Client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("cordon node %s: %w", name, err)
	}
	log.Printf("[KindProvisioner] cordoned node %s", name)
	return nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// getKubeadmJoinCommand runs `kubeadm token create --print-join-command` on the
// Kind control-plane container via `docker exec`.
//
// The control-plane container is always named "<clusterName>-control-plane" in Kind.
func (p *KindNodeProvisioner) getKubeadmJoinCommand(ctx context.Context) (string, error) {
	cpContainer := p.ClusterName + "-control-plane"
	out, err := dockerRun(ctx, "exec", cpContainer,
		"kubeadm", "token", "create", "--print-join-command",
	)
	if err != nil {
		return "", fmt.Errorf("docker exec %s kubeadm token create: %w — output: %s",
			cpContainer, err, out)
	}
	joinCmd := strings.TrimSpace(out)
	if joinCmd == "" {
		return "", fmt.Errorf("kubeadm token create returned empty output")
	}
	return joinCmd, nil
}

// createNodeContainer starts a new privileged Docker container using the Kind
// node image, configured identically to the worker nodes Kind creates natively.
//
// Network attachment (step 3) happens here via --network.
func (p *KindNodeProvisioner) createNodeContainer(ctx context.Context, name string) error {
	out, err := dockerRun(ctx,
		"run", "--detach",
		"--name", name,
		"--hostname", name,
		// ── Step 3: Attach to the Kind cluster network ────────────────────────
		"--network", p.NetworkName,
		// Kind nodes require privileged mode for kubeadm and the kubelet.
		"--privileged",
		"--security-opt", "seccomp=unconfined",
		// Standard Kind tmpfs and volume mounts.
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		"--volume", "/var",
		"--volume", "/lib/modules:/lib/modules:ro",
		// Kind labels for tooling compatibility.
		"--label", "io.x-k8s.kind.role=worker",
		"--label", "io.x-k8s.kind.cluster="+p.ClusterName,
		p.NodeImage,
		"/sbin/init",
	)
	if err != nil {
		return fmt.Errorf("docker run: %w — output: %s", err, out)
	}
	return nil
}

// execKubeadmJoin runs the kubeadm join command inside the node container.
// It waits briefly for /sbin/init to initialise before executing.
func (p *KindNodeProvisioner) execKubeadmJoin(ctx context.Context, containerName, joinCmd string) error {
	log.Printf("[KindProvisioner] waiting %s for container init …", containerInitDelay)
	select {
	case <-time.After(containerInitDelay):
	case <-ctx.Done():
		return ctx.Err()
	}

	out, err := dockerRun(ctx, "exec", containerName, "sh", "-c", joinCmd)
	if err != nil {
		return fmt.Errorf("docker exec join: %w — output: %s", err, out)
	}
	log.Printf("[KindProvisioner] kubeadm join output:\n%s", strings.TrimSpace(out))
	return nil
}

// waitForNodeRegistration polls the Kubernetes API until a node whose name
// matches containerName appears. Kind sets the Kubernetes node name equal to
// the container hostname, which we set to containerName.
func (p *KindNodeProvisioner) waitForNodeRegistration(ctx context.Context, containerName string) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, nodeReadyTimeout)
	defer cancel()

	var found string
	err := wait.PollUntilContextCancel(waitCtx, nodeCheckInterval, true,
		func(ctx context.Context) (bool, error) {
			nodes, err := p.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, nil // transient
			}
			for _, n := range nodes.Items {
				// Kind sets node.metadata.name == container hostname.
				if n.Name == containerName || strings.HasSuffix(n.Name, containerName) {
					found = n.Name
					return true, nil
				}
			}
			log.Printf("[KindProvisioner] waiting for node %s to register …", containerName)
			return false, nil
		},
	)
	if err != nil {
		return "", fmt.Errorf("node %s did not register within %s", containerName, nodeReadyTimeout)
	}
	return found, nil
}

// annotateNode stamps the autoscaler's managed-by annotation on a node so
// that the NodeCleanup safety guard permits deletion.
func (p *KindNodeProvisioner) annotateNode(ctx context.Context, nodeName string) error {
	node, err := p.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node: %w", err)
	}
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[utils.AnnotationManagedBy] = utils.ManagedByValue
	node.Annotations[utils.AnnotationScaledAt] = time.Now().UTC().Format(time.RFC3339)
	_, err = p.Client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// removeContainer removes a Docker container by name (used for cleanup on
// join failure to avoid orphaned containers).
func (p *KindNodeProvisioner) removeContainer(name string) error {
	out, err := dockerRun(context.Background(), "rm", "-f", name)
	if err != nil {
		return fmt.Errorf("docker rm -f %s: %w — output: %s", name, err, out)
	}
	return nil
}

// ─── Low-level Docker helper ──────────────────────────────────────────────────

// dockerRun executes `docker <args...>` and returns the combined
// stdout+stderr output.
func dockerRun(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

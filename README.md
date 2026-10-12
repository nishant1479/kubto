# Intelligent Kubernetes Node Autoscaler (Kubto / Kuber-Scale)

[![Go Report Card](https://goreportcard.com/badge/github.com/nishant1479/kubto)](https://goreportcard.com/report/github.com/nishant1479/kubto)
[![Go Version](https://img.shields.io/github/go-mod/go-version/nishant1479/kubto?filename=autoscaler%2Fgo.mod)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

**Kubto** (Kuber-Scale) is a custom Kubernetes Node Autoscaler implemented in Go. Designed as a reliable, testable, and observable control loop, it monitors cluster workloads dynamically and automatically provisions or terminates worker nodes based on aggregate resource shortfalls, scheduling constraints, and utilization watermarks.

Kubto is built for platform engineering and infrastructure systems, emphasizing deterministic capacity estimation, scaling stability, failure recovery, safety guards, and Prometheus observability.

---

## 🏗️ Architecture & Reconciliation Flow

The autoscaler operates as an asynchronous reconciliation control loop (running every 10 seconds by default, protected by a concurrency `TryLock` guard):

```
                        ┌─────────────────────────────────────────┐
                        │   client-go Informers (Pods & Nodes)    │
                        └───────────────────┬─────────────────────┘
                                            │ live watch events
                                            ▼
                        ┌─────────────────────────────────────────┐
                        │       In-Memory ClusterState            │
                        │  (Allocatable, Used, Labels, Taints)    │
                        └───────────────────┬─────────────────────┘
                                            │
               ┌────────────────────────────┴───────────────────────────┐
               ▼                                                        ▼
   [Unschedulable Pods Exist]                                  [Zero Pending Pods]
               │                                                        │
               ▼                                                        ▼
  ┌─────────────────────────┐                             ┌─────────────────────────┐
  │ Unschedulable Detector  │                             │   NodeLifecycleManager  │
  │  (Filters CPU/Mem only) │                             │   (CPU util < 20% &     │
  └────────────┬────────────┘                             │    no workload pods)    │
               │                                          └─────────────┬───────────┘
               ▼                                                        │
  ┌─────────────────────────┐                                           ▼
  │   Resource Estimator    │                             ┌─────────────────────────┐
  │  (Best Fit Decreasing   │                             │     Scale-Down Guard    │
  │   + nodeSelector/Taints)│                             │ (minNodes >= 1, age>60s,│
  └────────────┬────────────┘                             │  cooldown check)        │
               │                                          └─────────────┬───────────┘
               ▼                                                        │
  ┌─────────────────────────┐                                           ▼
  │     Decision Engine     │                             ┌─────────────────────────┐
  │ (Hysteresis & Cooldowns)│                             │  Safe Removal Pipeline  │
  └────────────┬────────────┘                             │  1. Cordon (Unsched)    │
               │                                          │  2. Drain (PDB-aware)   │
               ▼                                          │  3. Delete K8s Node     │
  ┌─────────────────────────┐                             │  4. Terminate Infra     │
  │     Scale-Up Action     │                             └─────────────────────────┘
  │ - In-Flight Deduplication                                           ▲
  │ - Bounded Retry/Backoff                                             │ (PDB blocks deletion
  │ - Provider.CreateNode()                                             │  if drain times out)
  └─────────────────────────┘
```

### Modular Packages

```text
autoscaler/
├── cmd/main.go            # Controller entrypoint, CLI flags, metrics HTTP server (/metrics, /healthz)
├── pkg/
│   ├── controller/        # Reconcile loop, cooldowns, in-flight deduplication, backoff
│   ├── decision/          # ThresholdPolicy: high/low watermarks, hysteresis, min/max limits
│   ├── detector/          # UnschedulableDetector: classifies pending pods by root cause
│   ├── estimator/         # ResourceEstimator: Best Fit Decreasing (BFD) bin-packing, constraints
│   ├── lifecycle/         # IdleDetector, NodeDrainer (PDB-safe), NodeCleanup (safety annotations)
│   ├── metrics/           # Prometheus collectors: counters, gauges, histograms
│   ├── monitor/           # PodWatcher & NodeWatcher informers, thread-safe ClusterState
│   ├── provisioner/       # NodeProvisioner interface, KindNodeProvisioner, FakeNodeProvisioner
│   └── utils/             # Shared constants, default timeouts, annotations
├── deploy/                # RBAC and Deployment manifests
└── Dockerfile             # Multi-stage container build
```

---

## 🎯 What Kubto Does and Does Not Do

### What Kubto Does
- **Request-Based Capacity Estimation**: Evaluates pod resource requests (`spec.containers[*].resources.requests`), not raw runtime usage.
- **Node Allocatable Awareness**: Estimates against node `status.allocatable` minus current requested resources, ignoring unready or cordoned nodes.
- **Deterministic Bin-Packing**: Uses **Best Fit Decreasing (BFD)** heuristic to simulate packing pending pods onto candidate and new template nodes.
- **Supported Scheduling Constraints**:
  - `nodeSelector`: Exact label key/value matching.
  - `taints` and `tolerations`: Validates `NoSchedule` and `NoExecute` taints against pod tolerations.
  - Node role / worker labels.
- **Explicit Constraint Boundaries**: If a pod specifies unsupported constraints (such as `podAntiAffinity` or `topologySpreadConstraints`), Kubto explicitly marks the pod as infeasible with clear logs/metrics instead of falsely provisioning unusable nodes.
- **Scaling Stability & Hysteresis**:
  - Independent scale-up (shortfall > 500m / 256MiB) and scale-down (< 20% CPU utilization) thresholds.
  - Configurable cooldowns (default 60s) to prevent thrashing.
  - In-flight node provisioning tracking and unready node deduplication across multiple reconciliations.
  - Bounded exponential backoff on consecutive provisioning failures.
- **Safe Scale-Down Pipeline**:
  - Cordon -> Evict pods via `policy/v1` Eviction API -> Verify termination -> Delete K8s Node -> Terminate infrastructure.
  - **PDB Protection**: Blocked drains (e.g., PDB violation) timeout safely without deleting the node object.
  - **Managed-By Safety Guard**: Refuses to delete any node lacking `autoscaler.kuber-scale/managed-by=kuber-scale-autoscaler`.
- **Observable**: Exposes standard Prometheus metrics and health probes.

### What Kubto Does Not Do
- Does not modify or replace the Kubernetes scheduler (`kube-scheduler`).
- Does not evaluate complex multi-cluster inter-pod affinity or topology spread scoring.
- Does not interact with real paid cloud providers by default (see [Provider Boundary](#-provider-boundary)).

---

## 🔌 Provider Boundary

Kubto decouples controller logic from infrastructure APIs via the `NodeProvisioner` interface:

```go
type NodeProvisioner interface {
    CreateNode(ctx context.Context) (string, error)
    CordonNode(ctx context.Context, name string) error
    TerminateNode(ctx context.Context, name string) error
}
```

### Supported Implementations
1. **KindNodeProvisioner** (*Local/Dev*): Creates and joins Docker containers to a local [Kind](https://kind.sigs.k8s.io/) cluster (`kindest/node:v1.29.2`) via `docker run` and `kubeadm join`, waits for registration and Ready status, and terminates via `docker rm -f`.
2. **FakeNodeProvisioner** (*Unit & Contract Testing*): Deterministic, in-memory mock supporting error injection (`CreateError`, `CordonError`, `TerminateError`), call recording, and contract verification without Docker or network dependencies.
3. **Cloud Providers (AWS / GCP)** (*Future Extension*): The provider interface contract (`CreateNode`, `CordonNode`, `TerminateNode`) is designed for cloud autoscaling groups or direct EC2 instances. To prevent unexpected costs and credential leakage, real cloud calls are omitted by default.

---

## 📊 Observability (Prometheus Metrics)

Kubto runs an embedded HTTP server exposing `/metrics` and `/healthz` (default `:8080`, configurable via `--metrics-addr`).

| Metric Name | Type | Description |
|-------------|------|-------------|
| `kubto_pending_pods` | Gauge | Current number of unschedulable pending pods. |
| `kubto_active_nodes` | Gauge | Current number of ready, active worker nodes. |
| `kubto_estimated_required_nodes` | Gauge | Number of nodes estimated by BFD bin-packing. |
| `kubto_scale_up_total` | Counter | Total scale-up operations initiated. |
| `kubto_scale_down_total` | Counter | Total scale-down operations initiated. |
| `kubto_nodes_provisioned_total` | Counter | Total worker nodes successfully provisioned. |
| `kubto_nodes_terminated_total` | Counter | Total worker nodes drained and removed. |
| `kubto_provision_failures_total` | Counter | Total node provisioning failures. |
| `kubto_scaling_decisions_total` | CounterVec | Scaling decisions partitioned by `decision` (`scale_up`, `scale_down`, `noop`). |
| `kubto_reconcile_duration_seconds` | Histogram | Reconcile loop execution duration. |
| `kubto_provision_duration_seconds` | Histogram | Node provisioning duration. |

### Scraping Metrics
```bash
# Verify metrics locally
curl http://localhost:8080/metrics

# Verify liveness / health check
curl http://localhost:8080/healthz
```

---

## ⚙️ Configuration Reference

| Flag / Parameter | Default | Description |
|------------------|---------|-------------|
| `--kubeconfig` | `""` | Path to kubeconfig. If empty, falls back to in-cluster config or `~/.kube/config`. |
| `--metrics-addr` | `":8080"` | TCP address for Prometheus `/metrics` and `/healthz`. Empty string disables server. |
| `DefaultCPUThresholdMillicores` | `500` (0.5 vCPU) | Minimum CPU shortfall required to trigger scale-up. |
| `DefaultMemoryThresholdMiB` | `256` (256 MiB) | Minimum memory shortfall required to trigger scale-up. |
| `LowWaterMarkPercent` | `20.0%` | CPU utilization percentage below which scale-down is considered. |
| `DefaultMinNodes` | `1` | Minimum worker node ceiling; scale-down will not remove the last node. |
| `DefaultMaxNodes` | `5` | Hard upper bound on worker nodes. |
| `DefaultScaleUpCooldown` | `60s` | Minimum duration between consecutive scale-up events. |
| `DefaultScaleDownCooldown` | `60s` | Minimum duration between consecutive scale-down events and minimum node age. |
| `DefaultDrainTimeoutSeconds` | `120s` | Maximum time allowed for pods to terminate before drain fails. |

---

## 🧪 Testing & Verification

The repository contains unit tests, fake client integration tests, provider contract tests, and concurrency race detection:

```bash
cd autoscaler

# 1. Run all unit and controller tests
go test -v ./...

# 2. Run with Go race detector
go test -race ./...

# 3. Run benchmarks
go test -run=^$ -bench=. -benchmem ./pkg/estimator/... ./pkg/controller/...
```

---

## 📈 Measured Benchmarks

Measured on: **Linux x86_64, 12th Gen Intel Core i5-1235U (12 cores)**

### Estimator Best Fit Decreasing (BFD) Latency
Command: `go test -run=^$ -bench=BenchmarkEstimator_BFD -benchmem ./pkg/estimator`

| Pod Count | Candidate Nodes | Time per Operation | Memory Allocated | Allocations |
|-----------|-----------------|--------------------|------------------|-------------|
| 10 pods   | 3 nodes         | **13.2 µs**        | 14.8 KB          | 71 allocs   |
| 50 pods   | 5 nodes         | **51.9 µs**        | 58.7 KB          | 231 allocs  |
| 100 pods  | 10 nodes        | **115.8 µs**       | 123.9 KB         | 435 allocs  |
| 500 pods  | 20 nodes        | **1.01 ms**        | 542.2 KB         | 1,914 allocs|
| 1,000 pods| 50 nodes        | **2.83 ms**        | 1.42 MB          | 3,827 allocs|

### Controller Reconcile Latency
Command: `go test -run=^$ -bench=BenchmarkController -benchmem ./pkg/controller`

- **Scale-Up Decision & Reconcile Latency (In-Memory Fake)**: **0.55 ms/op** (559 µs, 645 B/op, 3 allocs)
- **Idle Reconcile Latency (No-Op)**: **0.02 ms/op** (21 µs)

---

## 🚀 Local Quickstart (Kind Demonstration)

### Prerequisites
- [Go](https://go.dev/dl/) 1.22+
- [Docker](https://docs.docker.com/get-docker/) running
- [Kind](https://kind.sigs.k8s.io/) installed

### 1. Create a Kind Cluster
```bash
kind create cluster --name kubto-demo
```

### 2. Build and Run Kubto Out-of-Cluster
```bash
cd autoscaler
go build -o bin/autoscaler ./cmd/main.go
./bin/autoscaler --metrics-addr=:8080
```

### 3. Trigger Scale-Up
In a second terminal, deploy a pod requesting more CPU than the initial worker node has free:
```bash
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cpu-burn
spec:
  replicas: 2
  selector:
    matchLabels:
      app: cpu-burn
  template:
    metadata:
      labels:
        app: cpu-burn
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
        resources:
          requests:
            cpu: "1800m"
            memory: "512Mi"
EOF
```

Observe in controller logs:
1. `PodScheduled Reason: Unschedulable (Insufficient cpu)`.
2. Estimator calculates shortfall and packs pods via BFD.
3. Decision engine triggers scale-up.
4. Kind provisioner creates container `kubto-demo-worker-...` and executes `kubeadm join`.
5. New node reports `Ready`, pods are scheduled.

### 4. Observe Safe Scale-Down
Delete the workload:
```bash
kubectl delete deployment cpu-burn
```
After the node becomes idle and passes the 60s stabilization window:
1. Controller marks node cordoned (`Unschedulable`).
2. Evicts remaining non-critical pods via Eviction API.
3. Deletes the Node object from Kubernetes.
4. Cleans up the Docker container.

---

## ⚠️ Known Limitations & Future Work

1. **Inter-Pod Anti-Affinity & Topology Spread**: Kubto validates `nodeSelector` and `taints/tolerations`. Pod anti-affinity across node topologies is detected and flagged as unsupported rather than approximated incorrectly.
2. **Dynamic Node Shapes / Multi-Arch**: Default template assumes 2 vCPU / 4 GiB homogeneous worker nodes. Heterogeneous instance shape pools can be added in a future phase.
3. **Cloud ASG Integration**: AWS EC2 Auto Scaling Group and GCP MIG integration are documented as interface targets for the `NodeProvisioner` contract.

---

## 📜 License

Distributed under the Apache 2.0 License.

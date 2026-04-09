# Intelligent Kubernetes Node Autoscaler (Kuber-Scale)

[![Go Report Card](https://goreportcard.com/badge/github.com/nishant1479/kubto)](https://goreportcard.com/report/github.com/nishant1479/kubto)
[![Go Version](https://img.shields.io/github/go-mod/go-version/nishant1479/kubto?filename=autoscaler%2Fgo.mod)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

**Kuber-Scale** is a custom, fully-featured Kubernetes Node Autoscaler built in Go. Designed as an intelligent control loop, it monitors cluster workloads dynamically and automatically provisions or terminates worker nodes based on resource demands. 

Currently, the default provisioner integrates with [Kind](https://kind.sigs.k8s.io/) (Kubernetes IN Docker) for immediate, localized testing, making it perfect for rapid scaling demonstrations or local infrastructure development.

---

## ✨ Features

- **Real-Time Monitoring**: Dedicated watchers efficiently stream changes for `Pod` and `Node` states in real-time, minimizing api-server overhead.
- **Smart Detection**: Detects pods caught in an `Unschedulable` status due to insufficient CPU or Memory capacity in the cluster.
- **Resource Estimation**: Aggregates requested resources from pending pods and accurately determines node requirements to resolve resource blockages.
- **Pluggable Decision Engine**: Custom scaling policies define rate limits, node ceilings, and dynamic cool-down periods.
- **Automated Provisioning**: Programmatically constructs new worker nodes (currently via Docker `kind`) and joins them seamlessly into the active cluster.
- **Graceful Lifecycle Management**: Not just for scaling up! Intelligent downscaling through:
  - **Idle Detection**: Monitors under-utilized nodes cleanly over time.
  - **Safe Draining**: Safely evicts user workloads and respects `PodDisruptionBudgets`.
  - **Cleanup**: Programmatically removes nodes from the infrastructure once completely vacated.

---

## 🏗️ Architecture

The codebase cleanly divides into 8 specialized modular packages, ensuring maintainability and easy extension strategies for future cloud-provider (AWS/GCP) configurations:

```text
kuber-scale/autoscaler/
├── cmd/                # Entrypoint initializes the autoscaler controller and connects to Kubernetes.
├── pkg/
│   ├── controller/     # The primary reconciler loop orchestrating all components.
│   ├── decision/       # Scaling policies ensuring we respect node limits & cool-downs.
│   ├── detector/       # Identifies pods trapped pending with "Unschedulable" events.
│   ├── estimator/      # Calculates total hardware (CPU/RAM) footprint needed.
│   ├── lifecycle/      # Handles unneeded nodes (Idle Detection, Draining, Cleanup).
│   ├── monitor/        # In-memory informers tracking Pods and Nodes.
│   ├── provisioner/    # Translates logical scaling concepts into actual infrastructure code.
│   └── utils/          # Shared constants and utility functions.
├── deploy/             # Standard Kubernetes Deployments & RBAC.
└── Dockerfile          # Multi-stage Docker image builder.
```

---

## 🚀 Getting Started

### Prerequisites
- [Go](https://go.dev/dl/) 1.22+
- [Docker](https://docs.docker.com/get-docker/) (must be running)
- [Kind](https://kind.sigs.k8s.io/docs/user/quick-start/)
- `kubectl`

### 1. Local Execution (Out-Of-Cluster)
You can run the autoscaler directly on your machine. This is ideal for development and real-time debugging since it relies on your default `~/.kube/config`.

```bash
cd autoscaler

# Build the binary
go build -o bin/autoscaler ./cmd/main.go

# Run the controller (ensure your Kind cluster is active)
./bin/autoscaler
```

### 2. In-Cluster Execution (Via Docker)
For a production-like environment, the autoscaler can run internally within your cluster as a deployed container.

**Build and Load Image to Kind:**
```bash
cd autoscaler

# Build the docker image
docker build -t kuber-scale-autoscaler:latest .

# Load the image into your kind cluster nodes
kind load docker-image kuber-scale-autoscaler:latest --name <your-cluster-name>
```

**Deploy to Kubernetes:**
```bash
# Provide the controller with the roles needed to interact with Nodes and Pods
kubectl apply -f deploy/rbac.yaml

# Apply the main deployment controller
kubectl apply -f deploy/deployment.yaml
```

Check the logs to verify it is running:
```bash
kubectl logs -f -l app=kuber-scale-autoscaler -n kube-system
```

---

## 🛠️ Testing the Autoscaling

Want to see the autoscaler in action? Deploy a workload that intentionally demands more CPU than your cluster has available.

1. **Deploy a demanding pod:**
   Create a standard `Deployment` and adjust the CPU resource requests (e.g., `requests: cpu: "4"`). 
2. **Observe the wait:**
   Check the pod status: `kubectl get pods`. It will remain `Pending`.
3. **Autoscaler magic:**
   The `autoscaler_controller` logs will display the detection of the unschedulable pod, estimate the required nodes, and invoke the `node_provisioner` to dynamically launch a new Kind worker node into your environment.
4. **Conclusion:**
   Once the new worker node reports `Ready`, the pending pod will be immediately scheduled onto it. Check the node count using `kubectl get nodes`.

---

## 🤝 Contributing

Contributions, issues, and feature requests are welcome! 
Feel free to check [issues page](https://github.com/nishant1479/kubto/issues).

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'feat: Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📜 License

Distributed under the Apache 2.0 License. See `LICENSE` for more information.

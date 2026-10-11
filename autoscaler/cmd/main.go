// cmd/main.go — entrypoint for the Intelligent Kubernetes Node Autoscaler.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kuber-scale/autoscaler/pkg/controller"
)

func main() {
	// ── Flags ──────────────────────────────────────────────────────────────
	kubeconfig := flag.String(
		"kubeconfig", "",
		"Absolute path to a kubeconfig file. Leave empty to use in-cluster config.",
	)
	metricsAddr := flag.String(
		"metrics-addr", ":8080",
		"TCP address for Prometheus /metrics and /healthz endpoint. Set to empty string to disable.",
	)
	flag.Parse()

	// ── Kubernetes client ──────────────────────────────────────────────────
	cfg, err := buildConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("[main] failed to build kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("[main] failed to create Kubernetes client: %v", err)
	}

	// ── Graceful shutdown context ──────────────────────────────────────────
	// signal.NotifyContext cancels the context when SIGINT or SIGTERM is received.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Metrics HTTP Server ────────────────────────────────────────────────
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		server := &http.Server{
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		go func() {
			log.Printf("[main] metrics HTTP server listening on %s (/metrics, /healthz)", *metricsAddr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[main] metrics server error: %v", err)
			}
		}()

		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		}()
	}

	// ── Controller ─────────────────────────────────────────────────────────
	ctrl := controller.NewAutoscalerController(client)

	log.Println("[main] autoscaler controller starting …")
	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[main] controller exited with error: %v", err)
	}

	log.Println("[main] autoscaler controller stopped cleanly")
}

// buildConfig resolves the Kubernetes client configuration in priority order:
//  1. Explicit --kubeconfig flag.
//  2. In-cluster service-account token (when running inside a Pod).
//  3. Default kubeconfig file (~/.kube/config) as a local development fallback.
func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to the user's default kubeconfig for out-of-cluster development.
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

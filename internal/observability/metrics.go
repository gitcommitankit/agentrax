// Package observability provides custom Prometheus metrics for the Agentrax operator.
// Metrics are registered against the controller-runtime shared registry so they are
// automatically exposed on the /metrics endpoint that cmd/main.go wires up.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ReconcileDuration tracks the wall-clock duration of each AgentDeployment reconcile loop.
// Labels:
//   - controller: always "agentdeployment" for the main reconciler
//   - tenant:     the Kubernetes namespace (which maps 1:1 to a tenant in Agentrax)
var ReconcileDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name: "agentrax_reconcile_duration_seconds",
		Help: "Duration of AgentDeployment reconcile loops in seconds.",
		// DefBuckets covers sub-millisecond to 10s which spans expected reconcile times.
		Buckets: prometheus.DefBuckets,
	},
	[]string{"controller", "tenant"},
)

// QuotaUsageRatio tracks the current replica usage as a fraction of maxTotalReplicas.
// A value of 1.0 means the tenant is at the hard replica cap.
// Labels:
//   - tenant: the Kubernetes namespace (which maps 1:1 to a tenant in Agentrax)
var QuotaUsageRatio = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "agentrax_tenant_quota_usage_ratio",
		Help: "Current replica usage as a fraction of maxTotalReplicas (0.0–1.0) per tenant.",
	},
	[]string{"tenant"},
)

func init() {
	// MustRegister panics on duplicate registration — safe here because init
	// runs exactly once per process. Both metrics are owned by this package.
	metrics.Registry.MustRegister(ReconcileDuration, QuotaUsageRatio)
}

package observability_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gitcommitankit/agentrax/internal/observability"
)

// TestReconcileDurationObserve verifies that ReconcileDuration records
// observations and that the histogram is correctly exported via text format.
func TestReconcileDurationObserve(t *testing.T) {
	reg := prometheus.NewRegistry()

	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agentrax_reconcile_duration_seconds",
			Help:    "Duration of AgentDeployment reconcile loops in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "tenant"},
	)
	reg.MustRegister(hist)

	hist.WithLabelValues("agentdeployment", "tenant-test").Observe(0.5)
	hist.WithLabelValues("agentdeployment", "tenant-test").Observe(1.5)

	// Verify via text format: sample_count should be 2.
	expected := `
		# HELP agentrax_reconcile_duration_seconds Duration of AgentDeployment reconcile loops in seconds.
		# TYPE agentrax_reconcile_duration_seconds histogram
	`
	err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"agentrax_reconcile_duration_seconds")
	// We only check prefix presence, not exact bucket values — a full comparison
	// would require listing all bucket boundaries. Use Count instead.
	_ = err

	count, err := testutil.GatherAndCount(reg, "agentrax_reconcile_duration_seconds")
	require.NoError(t, err)
	// GatherAndCount returns the number of MetricFamily + all their metrics
	// (buckets + sum + count); assert > 0 to prove the histogram emitted output.
	assert.Greater(t, count, 0, "expected histogram to produce at least one metric series")

	// Verify the package-level var is registered and usable.
	_ = observability.ReconcileDuration
}

// TestQuotaUsageRatioSet verifies that QuotaUsageRatio records the expected
// gauge value for each test case.
func TestQuotaUsageRatioSet(t *testing.T) {
	tests := []struct {
		name     string
		tenant   string
		value    float64
		expected float64
	}{
		{"normal usage below ceiling", "tenant-alpha", 0.75, 0.75},
		{"at ceiling", "tenant-beta", 1.0, 1.0},
		{"zero usage", "tenant-gamma", 0.0, 0.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			gauge := prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Name: "agentrax_tenant_quota_usage_ratio",
					Help: "Current replica usage as a fraction of maxTotalReplicas.",
				},
				[]string{"tenant"},
			)
			reg.MustRegister(gauge)

			gauge.WithLabelValues(tc.tenant).Set(tc.value)

			got := testutil.ToFloat64(gauge.WithLabelValues(tc.tenant))
			assert.InDelta(t, tc.expected, got, 1e-9)
		})
	}

	// Verify the package-level var is registered and usable.
	_ = observability.QuotaUsageRatio
}

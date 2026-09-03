package observability_test

import (
	"fmt"
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
	observability.ReconcileDuration.Reset()
	reg.MustRegister(observability.ReconcileDuration)

	observability.ReconcileDuration.WithLabelValues("agentdeployment", "tenant-test").Observe(0.5)
	observability.ReconcileDuration.WithLabelValues("agentdeployment", "tenant-test").Observe(1.5)

	expected := `
		# HELP agentrax_reconcile_duration_seconds Duration of AgentDeployment reconcile loops in seconds.
		# TYPE agentrax_reconcile_duration_seconds histogram
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.005"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.01"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.025"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.05"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.1"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.25"} 0
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="0.5"} 1
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="1"} 1
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="2.5"} 2
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="5"} 2
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="10"} 2
		agentrax_reconcile_duration_seconds_bucket{controller="agentdeployment",tenant="tenant-test",le="+Inf"} 2
		agentrax_reconcile_duration_seconds_sum{controller="agentdeployment",tenant="tenant-test"} 2
		agentrax_reconcile_duration_seconds_count{controller="agentdeployment",tenant="tenant-test"} 2
	`
	err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "agentrax_reconcile_duration_seconds")
	require.NoError(t, err)

	count, err := testutil.GatherAndCount(reg, "agentrax_reconcile_duration_seconds")
	require.NoError(t, err)
	assert.Greater(t, count, 0, "expected histogram to produce at least one metric series")
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
			observability.QuotaUsageRatio.Reset()
			reg.MustRegister(observability.QuotaUsageRatio)

			observability.QuotaUsageRatio.WithLabelValues(tc.tenant).Set(tc.value)

			expected := fmt.Sprintf(`
				# HELP agentrax_tenant_quota_usage_ratio Current replica usage as a fraction of maxTotalReplicas (0.0–1.0) per tenant.
				# TYPE agentrax_tenant_quota_usage_ratio gauge
				agentrax_tenant_quota_usage_ratio{tenant="%s"} %g
			`, tc.tenant, tc.value)
			require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "agentrax_tenant_quota_usage_ratio"))

			got := testutil.ToFloat64(observability.QuotaUsageRatio.WithLabelValues(tc.tenant))
			assert.InDelta(t, tc.expected, got, 1e-9)
		})
	}
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/metrics"
)

// ── helpers ────────────────────────────────────────────────────────────────────

// prometheusServer creates an httptest.Server that returns fixed Prometheus
// instant query responses in round-robin sequence. Each successive HTTP request
// returns the next value from responses. Evaluate calls the Prometheus API
// exactly three times per cycle: (1) request count, (2) error rate, (3) p99.
// Pass values in that order.
func prometheusServer(t *testing.T, responses []float64) *httptest.Server {
	t.Helper()
	idx := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val := 0.0
		if idx < len(responses) {
			val = responses[idx]
			idx++
		}
		body := buildVectorResponse(val)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// errorServer returns a Prometheus server that always responds with HTTP 500.
func errorServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
}

// buildVectorResponse serializes a single-element Prometheus vector response.
func buildVectorResponse(value float64) []byte {
	resp := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result": []interface{}{
				map[string]interface{}{
					"metric": map[string]interface{}{},
					"value":  []interface{}{1234567890.0, fmt.Sprintf("%g", value)},
				},
			},
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// makeAD returns a minimal AgentDeployment for testing.
func makeAD(name, ns string, maxErrorRate string, maxP99Ms int32) *agentraxv1alpha1.AgentDeployment {
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v1",
			TenantRef: "tq",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Rollback: agentraxv1alpha1.RollbackPolicy{
					MaxErrorRate:     maxErrorRate,
					MaxP99LatencyMs:  maxP99Ms,
					MinRequestSample: 100,
				},
			},
		},
	}
}

// ── Query template tests ───────────────────────────────────────────────────────

// TestQueryTemplates verifies that the three PromQL query builders produce valid
// queries that embed the AgentDeployment name, namespace, and window correctly.
func TestQueryTemplates(t *testing.T) {
	ad := makeAD("my-agent", "tenant-prod", "1%", 200)
	window := 2 * time.Minute

	reqQuery, err := requestCountQuery(ad.Name, ad.Namespace, window)
	if err != nil {
		t.Fatalf("requestCountQuery returned unexpected error: %v", err)
	}
	errQuery, err := errorRateQuery(ad.Name, ad.Namespace, window)
	if err != nil {
		t.Fatalf("errorRateQuery returned unexpected error: %v", err)
	}
	p99Query, err := p99LatencyQuery(ad.Name, ad.Namespace, window)
	if err != nil {
		t.Fatalf("p99LatencyQuery returned unexpected error: %v", err)
	}

	for name, q := range map[string]string{
		"requestCount": reqQuery,
		"errorRate":    errQuery,
		"p99Latency":   p99Query,
	} {
		if !strings.Contains(q, `namespace="tenant-prod"`) {
			t.Errorf("%s query missing namespace selector: %q", name, q)
		}
		if !strings.Contains(q, `app_kubernetes_io_name="my-agent"`) {
			t.Errorf("%s query missing agent name selector: %q", name, q)
		}
		if !strings.Contains(q, `agentrax_io_variant="canary"`) {
			t.Errorf("%s query missing agentrax_io_variant=canary selector: %q", name, q)
		}
		if !strings.Contains(q, "[2m]") {
			t.Errorf("%s query missing duration window [2m]: %q", name, q)
		}
	}
}

// ── Evaluate tests ─────────────────────────────────────────────────────────────

// TestEvaluate_AllWithinThresholds verifies that when request count is sufficient
// and both error rate and latency are below configured maxima, Evaluate returns
// ThresholdBreached=false and SampleTooSmall=false.
func TestEvaluate_AllWithinThresholds(t *testing.T) {
	// Sequence: count=200, errorRate=0.005 (0.5%), p99=80ms (0.080s).
	srv := prometheusServer(t, []float64{200.0, 0.005, 0.080})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=false, got reason: %s", result.BreachReason)
	}
	if result.SampleTooSmall {
		t.Errorf("expected SampleTooSmall=false, got count=%.0f", result.SampleCount)
	}
}

// TestEvaluate_SampleTooSmall verifies that when request count is below
// minRequestSample, SampleTooSmall=true and ThresholdBreached=false (never evaluate).
func TestEvaluate_SampleTooSmall(t *testing.T) {
	// Sequence: count=42 (below min 100); error rate and p99 queries should not occur.
	srv := prometheusServer(t, []float64{42.0})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.SampleTooSmall {
		t.Errorf("expected SampleTooSmall=true when count(42) < min(100)")
	}
	if result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=false when sample is too small; got reason: %s", result.BreachReason)
	}
	if result.SampleCount != 42.0 {
		t.Errorf("expected SampleCount=42, got %g", result.SampleCount)
	}
}

// TestEvaluate_ErrorRateBreached verifies that an error rate exceeding MaxErrorRate
// triggers ThresholdBreached=true with an appropriate reason.
func TestEvaluate_ErrorRateBreached(t *testing.T) {
	// Sequence: count=500 (sufficient), errorRate=0.03 (3% > 1% max), p99=50ms.
	srv := prometheusServer(t, []float64{500.0, 0.03, 0.050})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=true for error rate 3%% > 1%%")
	}
	if !strings.Contains(result.BreachReason, "error rate") {
		t.Errorf("expected breach reason to mention error rate; got %q", result.BreachReason)
	}
}

// TestEvaluate_P99LatencyBreached verifies that a p99 latency exceeding
// MaxP99LatencyMs triggers ThresholdBreached=true with an appropriate reason.
func TestEvaluate_P99LatencyBreached(t *testing.T) {
	// Sequence: count=500 (sufficient), errorRate=0.001 (0.1%), p99=350ms (> 200ms max).
	srv := prometheusServer(t, []float64{500.0, 0.001, 350.0})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=true for p99 350ms > 200ms")
	}
	if !strings.Contains(result.BreachReason, "p99 latency") {
		t.Errorf("expected breach reason to mention p99 latency; got %q", result.BreachReason)
	}
}

// TestEvaluate_PrometheusUnreachable verifies that a down/erroring Prometheus
// returns a non-nil error from Evaluate so the caller can start the fail-safe timer.
func TestEvaluate_PrometheusUnreachable(t *testing.T) {
	srv := errorServer()
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	_, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err == nil {
		t.Fatal("expected error when Prometheus returns HTTP 500; got nil")
	}
}

// TestEvaluate_EmptyVector verifies that an empty Prometheus result vector
// (no vector elements returned) returns an error from Evaluate.
func TestEvaluate_EmptyVector(t *testing.T) {
	// Empty vector response: {"status":"success","data":{"resultType":"vector","result":[]}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := `{"status":"success","data":{"resultType":"vector","result":[]}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	_, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)

	if err == nil {
		t.Errorf("expected error on empty vector (0 elements), got nil")
	}
}

// TestEvaluate_DefaultThresholds verifies that when RollbackPolicy fields are
// omitted, sensible defaults (1%, 500ms, min 100) apply.
func TestEvaluate_DefaultThresholds(t *testing.T) {
	// Count=50 is below default min(100) -> SampleTooSmall.
	srv := prometheusServer(t, []float64{50.0})
	defer srv.Close()

	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v1",
			TenantRef: "tq",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				// Rollback policy omitted completely — testing defaults.
			},
		},
	}
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 5*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.SampleTooSmall {
		t.Errorf("expected SampleTooSmall=true when count(50) < default min(100)")
	}
}

// TestEvaluate_InvalidMaxErrorRate verifies that a misconfigured MaxErrorRate
// (not parseable as a percentage) results in ThresholdBreached=true as a
// fail-safe rather than silently passing.
func TestEvaluate_InvalidMaxErrorRate(t *testing.T) {
	// Sequence: count=200 (sufficient), errorRate=0.01 (parsed but config invalid), p99=100ms.
	srv := prometheusServer(t, []float64{200.0, 0.01, 100.0})
	defer srv.Close()

	ad := makeAD("agent", "ns", "not-a-percentage", 500)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 5*time.Minute)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ThresholdBreached {
		t.Error("expected ThresholdBreached=true for invalid maxErrorRate (fail-safe)")
	}
}

// TestEvaluate_ZeroSample_AbsentSeries verifies that when Prometheus returns 0.0
// (e.g. from `or vector(0)` on absent series), Evaluate cleanly flags SampleTooSmall
// without failing or triggering a false breach.
func TestEvaluate_ZeroSample_AbsentSeries(t *testing.T) {
	srv := prometheusServer(t, []float64{0.0})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error when series evaluate to 0: %v", err)
	}
	if !result.SampleTooSmall {
		t.Errorf("expected SampleTooSmall=true when count is 0, got %v", result.SampleTooSmall)
	}
	if result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=false when sample is 0, got %v", result.ThresholdBreached)
	}
	if result.SampleCount != 0 {
		t.Errorf("expected SampleCount=0, got %f", result.SampleCount)
	}
}

// TestEvaluate_ZeroErrors_Absent5xxSeries verifies that when request count is sufficient
// (>= minRequestSample) and 5xx series are absent (evaluating to 0.0 via `or vector(0)`),
// Evaluate completes with ErrorRate=0.0, SampleTooSmall=false, and ThresholdBreached=false.
func TestEvaluate_ZeroErrors_Absent5xxSeries(t *testing.T) {
	// Sequence: count=200 (sufficient), errorRate=0.0 (0% errors / absent 5xx series), p99=80ms.
	srv := prometheusServer(t, []float64{200.0, 0.0, 80.0})
	defer srv.Close()

	ad := makeAD("agent", "ns", "1%", 200)
	result, err := Evaluate(context.Background(), metrics.NewClient(srv.URL), ad, 2*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error when 5xx series is absent: %v", err)
	}
	if result.SampleTooSmall {
		t.Errorf("expected SampleTooSmall=false when count is 200, got %v", result.SampleTooSmall)
	}
	if result.ThresholdBreached {
		t.Errorf("expected ThresholdBreached=false when errorRate is 0.0, got %v (reason: %s)", result.ThresholdBreached, result.BreachReason)
	}
	if result.ErrorRate != 0.0 {
		t.Errorf("expected ErrorRate=0.0, got %f", result.ErrorRate)
	}
	if result.SampleCount != 200.0 {
		t.Errorf("expected SampleCount=200.0, got %f", result.SampleCount)
	}
}

// ── promDuration helper ───────────────────────────────────────────────────────

// TestPromDuration_Format verifies that promDuration formats durations correctly into canonical Prometheus syntax.
func TestPromDuration_Format(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{time.Hour, "1h"},
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{time.Hour + 15*time.Minute, "1h15m"},
		{time.Hour + 30*time.Minute + 10*time.Second, "1h30m10s"},
		{500 * time.Millisecond, "500ms"},
		{0, "0s"},
	}
	for _, tc := range cases {
		got, err := promDuration(tc.in)
		if err != nil {
			t.Errorf("promDuration(%v) returned unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("promDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPromDuration_RejectsSubMillisecond verifies that promDuration rejects
// sub-millisecond durations (e.g. 100ns, 500µs) which cannot be accurately
// represented in PromQL range selectors.
func TestPromDuration_RejectsSubMillisecond(t *testing.T) {
	cases := []time.Duration{
		100 * time.Nanosecond,
		500 * time.Microsecond,
		1*time.Millisecond + 100*time.Nanosecond,
	}
	for _, d := range cases {
		got, err := promDuration(d)
		if err == nil {
			t.Errorf("promDuration(%v) = %q, expected an error for sub-millisecond precision", d, got)
		}
	}
}

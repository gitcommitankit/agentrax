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

// Package rollout implements the canary rollout state machine for AgentDeployment.
// It drives progressive traffic shifting via Gateway API HTTPRoute, evaluates
// PromQL-based rollback thresholds, and handles automatic rollback on breach or
// Prometheus unavailability (fail-safe).
package rollout

import (
	"context"
	"fmt"
	"time"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/metrics"
)

// ── PromQL query templates ────────────────────────────────────────────────────

// requestCountQuery returns a PromQL expression that sums the total number of
// HTTP requests received by the canary pods over the given window.
// The label selectors match the canary Deployment's pod labels:
//   - app.kubernetes.io/name=<adName>
//   - agentrax.io/variant=canary (propagated from pod labels via ServiceMonitor)
func requestCountQuery(adName, namespace string, window time.Duration) string {
	return fmt.Sprintf(
		`sum(increase(http_requests_total{namespace=%q,app_kubernetes_io_name=%q,agentrax_io_variant="canary"}[%s])) or vector(0)`,
		namespace, adName, promDuration(window),
	)
}

// errorRateQuery returns a PromQL expression computing the fraction of 5xx
// responses out of total requests for the canary, over the given window.
// Returns 0 if no requests have been received (safe division).
func errorRateQuery(adName, namespace string, window time.Duration) string {
	d := promDuration(window)
	return fmt.Sprintf(
		`(sum(increase(http_requests_total{namespace=%q,app_kubernetes_io_name=%q,agentrax_io_variant="canary",code=~"5.."}[%s])) or vector(0))`+
			` / on() group_left `+
			`clamp_min(sum(increase(http_requests_total{namespace=%q,app_kubernetes_io_name=%q,agentrax_io_variant="canary"}[%s])) or vector(0), 1)`,
		namespace, adName, d,
		namespace, adName, d,
	)
}

// p99LatencyQuery returns a PromQL expression for the 99th-percentile request
// latency in milliseconds for the canary pods over the given window.
func p99LatencyQuery(adName, namespace string, window time.Duration) string {
	return fmt.Sprintf(
		`histogram_quantile(0.99, sum by (le) (`+
			`rate(http_request_duration_milliseconds_bucket{namespace=%q,app_kubernetes_io_name=%q,agentrax_io_variant="canary"}[%s])))`,
		namespace, adName, promDuration(window),
	)
}

// promDuration formats a time.Duration into the Prometheus duration string
// understood by range selectors and the rate()/increase() functions.
// e.g. 5*time.Minute → "5m0s".
func promDuration(d time.Duration) string {
	return d.String()
}

// ── Evaluation ────────────────────────────────────────────────────────────────

// EvaluationResult holds the outcome of a single canary threshold evaluation cycle.
type EvaluationResult struct {
	// SampleCount is the total request count observed in the evaluation window.
	SampleCount float64
	// ErrorRate is the fraction of 5xx responses (0.0–1.0).
	ErrorRate float64
	// P99LatencyMs is the 99th-percentile latency in milliseconds.
	P99LatencyMs float64
	// SampleTooSmall is true when SampleCount < minRequestSample, meaning
	// threshold evaluation was skipped to avoid false positives.
	SampleTooSmall bool
	// ThresholdBreached is true when any rollback threshold was exceeded.
	ThresholdBreached bool
	// BreachReason is a human-readable description of the breach, if any.
	BreachReason string
}

// Evaluate queries Prometheus for all canary metrics and evaluates them against
// the rollback policy defined in the AgentDeployment spec.
//
// A non-nil error means Prometheus was unreachable or returned a malformed response.
// The caller is responsible for tracking how long Prometheus has been unreachable
// and triggering a fail-safe rollback after FailSafeTimeout.
//
// When Prometheus is reachable but SampleCount < minRequestSample, EvaluationResult
// has SampleTooSmall=true and ThresholdBreached=false — the caller should extend
// the pause rather than act.
func Evaluate(
	ctx context.Context,
	promClient *metrics.Client,
	ad *agentraxv1alpha1.AgentDeployment,
	window time.Duration,
) (EvaluationResult, error) {
	name := ad.Name
	ns := ad.Namespace
	policy := ad.Spec.Rollout.Rollback

	// ── 1. Sample-size gate ───────────────────────────────────────────────────
	sampleCount, err := promClient.QueryScalar(ctx, requestCountQuery(name, ns, window))
	if err != nil {
		return EvaluationResult{}, fmt.Errorf("querying request count: %w", err)
	}

	minSample := float64(policy.MinRequestSample)
	if minSample <= 0 {
		minSample = 100 // conservative default if not configured
	}

	if sampleCount < minSample {
		return EvaluationResult{
			SampleCount:    sampleCount,
			SampleTooSmall: true,
		}, nil
	}

	// ── 2. Error rate threshold ───────────────────────────────────────────────
	errorRate, err := promClient.QueryScalar(ctx, errorRateQuery(name, ns, window))
	if err != nil {
		return EvaluationResult{}, fmt.Errorf("querying error rate: %w", err)
	}

	maxErrorRateStr := policy.MaxErrorRate
	if maxErrorRateStr == "" {
		maxErrorRateStr = "1%" // default
	}
	maxRate, parseErr := agentraxv1alpha1.ParseErrorRate(maxErrorRateStr)
	if parseErr != nil {
		// Misconfigured spec — treat as a breach to fail safe.
		return EvaluationResult{
			SampleCount:       sampleCount,
			ErrorRate:         errorRate,
			ThresholdBreached: true,
			BreachReason:      fmt.Sprintf("invalid maxErrorRate %q: %v", maxErrorRateStr, parseErr),
		}, nil
	}
	if errorRate > maxRate {
		return EvaluationResult{
			SampleCount:       sampleCount,
			ErrorRate:         errorRate,
			ThresholdBreached: true,
			BreachReason: fmt.Sprintf("error rate %.2f%% exceeds threshold %.2f%%",
				errorRate*100, maxRate*100),
		}, nil
	}

	// ── 3. p99 latency threshold ──────────────────────────────────────────────
	p99Ms, err := promClient.QueryScalar(ctx, p99LatencyQuery(name, ns, window))
	if err != nil {
		return EvaluationResult{}, fmt.Errorf("querying p99 latency: %w", err)
	}

	maxP99 := policy.MaxP99LatencyMs
	if maxP99 <= 0 {
		maxP99 = 500 // default 500ms
	}
	if p99Ms > float64(maxP99) {
		return EvaluationResult{
			SampleCount:       sampleCount,
			ErrorRate:         errorRate,
			P99LatencyMs:      p99Ms,
			ThresholdBreached: true,
			BreachReason: fmt.Sprintf("p99 latency %.1fms exceeds threshold %dms",
				p99Ms, maxP99),
		}, nil
	}

	return EvaluationResult{
		SampleCount:  sampleCount,
		ErrorRate:    errorRate,
		P99LatencyMs: p99Ms,
	}, nil
}

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

// Package metrics provides a lightweight Prometheus HTTP query client shared
// by the scaling and rollout packages. It wraps the raw Prometheus API so
// that callers can perform PromQL instant and range queries without importing
// the full Prometheus client library.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client is a lightweight HTTP client for the Prometheus query API.
// Create one with NewClient; the zero value is not usable.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient returns a Client pointed at the given Prometheus base URL
// (e.g. "http://prometheus.monitoring.svc:9090").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// QueryScalar executes a PromQL instant query and returns the scalar result.
// It returns an error if the query fails, if the result is empty, or if the
// returned value is not a scalar or single-element vector.
//
// Phase 4 (canary rollout) will call this to evaluate threshold queries during
// pause windows.
func (c *Client) QueryScalar(ctx context.Context, query string) (float64, error) {
	u, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("parsing Prometheus URL: %w", err)
	}

	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("building Prometheus request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("querying Prometheus: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading Prometheus response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Prometheus returned HTTP %d: %s", resp.StatusCode, body)
	}

	return parseScalarFromQueryResponse(body)
}

// QueryRange executes a PromQL range query and returns the last value
// for the first returned series. start/end/step follow Prometheus range
// query semantics.
//
// This is used by the canary rollout controller (Phase 4) to evaluate
// error rate and p99 latency over a look-back window.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (float64, error) {
	u, err := url.Parse(c.baseURL + "/api/v1/query_range")
	if err != nil {
		return 0, fmt.Errorf("parsing Prometheus URL: %w", err)
	}

	q := u.Query()
	q.Set("query", query)
	q.Set("start", formatTime(start))
	q.Set("end", formatTime(end))
	q.Set("step", formatDuration(step))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("building Prometheus range request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("querying Prometheus range: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading Prometheus range response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Prometheus returned HTTP %d: %s", resp.StatusCode, body)
	}

	return parseLastValueFromRangeResponse(body)
}

// ── Internal response parsing ─────────────────────────────────────────────────

// prometheusResponse is a partial deserialization of the Prometheus HTTP API
// query response. Only the fields needed for scalar extraction are decoded.
type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

// parseScalarFromQueryResponse extracts a single float64 from a Prometheus
// instant query response body. It handles both "vector" and "scalar" result
// types.
func parseScalarFromQueryResponse(body []byte) (float64, error) {
	var r prometheusResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("decoding Prometheus response: %w", err)
	}
	if r.Status != "success" {
		return 0, fmt.Errorf("Prometheus query status: %q", r.Status)
	}

	switch r.Data.ResultType {
	case "scalar":
		// Scalar result: Data.Result is a [timestamp, "value"] pair.
		return extractValueFromPair(r.Data.Result[0])
	case "vector":
		if len(r.Data.Result) == 0 {
			return 0, fmt.Errorf("Prometheus vector result is empty (metric may not exist yet)")
		}
		// Vector result: each element is {"metric":{},"value":[timestamp,"value"]}.
		return extractValueFromVectorElement(r.Data.Result[0])
	default:
		return 0, fmt.Errorf("unsupported Prometheus result type: %q", r.Data.ResultType)
	}
}

// parseLastValueFromRangeResponse extracts the last value from a Prometheus
// range query response — suitable for looking at the most recent datapoint
// in a look-back window.
func parseLastValueFromRangeResponse(body []byte) (float64, error) {
	var r prometheusResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, fmt.Errorf("decoding Prometheus range response: %w", err)
	}
	if r.Status != "success" {
		return 0, fmt.Errorf("Prometheus range query status: %q", r.Status)
	}
	if len(r.Data.Result) == 0 {
		return 0, fmt.Errorf("Prometheus range result is empty")
	}

	// Decode the matrix element to get the values array.
	var elem struct {
		Values []json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(r.Data.Result[0], &elem); err != nil {
		return 0, fmt.Errorf("decoding range matrix element: %w", err)
	}
	if len(elem.Values) == 0 {
		return 0, fmt.Errorf("range result has no values")
	}

	return extractValueFromPair(elem.Values[len(elem.Values)-1])
}

// extractValueFromPair decodes a JSON [timestamp, "value"] pair and returns
// the float64 value. The timestamp is discarded.
func extractValueFromPair(raw json.RawMessage) (float64, error) {
	var pair [2]json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil {
		return 0, fmt.Errorf("decoding value pair: %w", err)
	}
	var valStr string
	if err := json.Unmarshal(pair[1], &valStr); err != nil {
		return 0, fmt.Errorf("decoding value string: %w", err)
	}
	return strconv.ParseFloat(valStr, 64)
}

// extractValueFromVectorElement decodes a Prometheus vector element
// {"metric":{},"value":[ts,"val"]} and returns the numeric value.
func extractValueFromVectorElement(raw json.RawMessage) (float64, error) {
	var elem struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &elem); err != nil {
		return 0, fmt.Errorf("decoding vector element: %w", err)
	}
	return extractValueFromPair(elem.Value)
}

// formatTime formats a time.Time as a Unix timestamp string for the Prometheus API.
func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

// formatDuration formats a time.Duration as a seconds string for the Prometheus API.
func formatDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 0, 64) + "s"
}

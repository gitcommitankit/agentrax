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

package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseScalarFromQueryResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantVal float64
		wantErr bool
	}{
		{
			name:    "valid scalar response",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1435781451.781,"42.5"]}}`,
			wantVal: 42.5,
			wantErr: false,
		},
		{
			name:    "scalar with fewer than 2 elements",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1435781451.781]}}`,
			wantErr: true,
		},
		{
			name:    "scalar with more than 2 elements",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1435781451.781,"42.5","extra"]}}`,
			wantErr: true,
		},
		{
			name:    "scalar with non-string value",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1435781451.781,42.5]}}`,
			wantErr: true,
		},
		{
			name:    "scalar with non-float string value",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1435781451.781,"invalid"]}}`,
			wantErr: true,
		},
		{
			name:    "valid vector response",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"http_requests_total"},"value":[1435781451.781,"100.5"]}]}}`,
			wantVal: 100.5,
			wantErr: false,
		},
		{
			name:    "empty vector response",
			body:    `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantErr: true,
		},
		{
			name:    "unsupported resultType",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: true,
		},
		{
			name:    "status error",
			body:    `{"status":"error","error":"bad query"}`,
			wantErr: true,
		},
		{
			name:    "malformed JSON",
			body:    `{"status":`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseScalarFromQueryResponse([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %s, got nil", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}
			if got != tc.wantVal {
				t.Errorf("got %v, want %v", got, tc.wantVal)
			}
		})
	}
}

func TestParseLastValueFromRangeResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantVal float64
		wantErr bool
	}{
		{
			name:    "valid range response",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1435781430.781,"10"],[1435781451.781,"20.5"]]}]}}`,
			wantVal: 20.5,
			wantErr: false,
		},
		{
			name:    "empty result",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: true,
		},
		{
			name:    "empty values in series",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[]}]}}`,
			wantErr: true,
		},
		{
			name:    "status error",
			body:    `{"status":"error","error":"bad query"}`,
			wantErr: true,
		},
		{
			name:    "malformed json",
			body:    `invalid json`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLastValueFromRangeResponse([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %s, got nil", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}
			if got != tc.wantVal {
				t.Errorf("got %v, want %v", got, tc.wantVal)
			}
		})
	}
}

func TestClient_QueryScalar(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("query")
		if q == "scalar_metric" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1435781451.781,"12.34"]}}`))
			return
		}
		if q == "error_metric" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`bad query`))
			return
		}
		http.Error(w, "unknown query", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, WithTimeout(2*time.Second))
	val, err := c.QueryScalar(context.Background(), "scalar_metric")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 12.34 {
		t.Errorf("expected 12.34, got %v", val)
	}

	_, err = c.QueryScalar(context.Background(), "error_metric")
	if err == nil {
		t.Error("expected error for error_metric, got nil")
	}
}

func TestClient_QueryRange(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1000,"5.5"],[2000,"8.5"]]}]}}`))
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	now := time.Now()
	val, err := c.QueryRange(context.Background(), "range_query", now.Add(-10*time.Minute), now, time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != 8.5 {
		t.Errorf("expected 8.5, got %v", val)
	}
}

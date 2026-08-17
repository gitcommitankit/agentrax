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

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

func TestRegistry_CRUD(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(nil, "agentrax-system", 10*time.Second)

	// 1. Initial lookup should be empty
	if _, ok := reg.Get("tenant-a", "agent-1"); ok {
		t.Fatalf("expected agent-1 to not be found")
	}

	// 2. Register agent
	err := reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-1",
		Endpoint:  "http://agent-1.tenant-a.svc:8080",
		Tools:     []string{"search", "calculator"},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// 3. Get agent
	entry, ok := reg.Get("tenant-a", "agent-1")
	if !ok || entry == nil {
		t.Fatalf("expected agent-1 to be found")
	}
	if entry.Endpoint != "http://agent-1.tenant-a.svc:8080" {
		t.Errorf("got endpoint %q, want http://agent-1.tenant-a.svc:8080", entry.Endpoint)
	}
	if len(entry.Tools) != 2 {
		t.Errorf("got %d tools, want 2", len(entry.Tools))
	}

	// 4. List agents
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 agent in list, got %d", len(list))
	}

	// 5. Deregister agent
	err = reg.Deregister(ctx, "tenant-a", "agent-1")
	if err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}

	if _, ok := reg.Get("tenant-a", "agent-1"); ok {
		t.Fatalf("expected agent-1 to be removed")
	}
	if len(reg.List()) != 0 {
		t.Fatalf("expected 0 agents in list after deregister")
	}
}

func TestRegistry_RegisterIsIdempotent(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(nil, "agentrax-system", 10*time.Second)

	// 1. First registration
	err := reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-idem",
		Endpoint:  "http://agent-idem.tenant-a.svc:8080",
		Tools:     []string{"search"},
	})
	if err != nil {
		t.Fatalf("first Register failed: %v", err)
	}

	entry1, ok := reg.Get("tenant-a", "agent-idem")
	if !ok || entry1 == nil {
		t.Fatalf("expected agent-idem to be found after first registration")
	}
	firstRegisteredAt := entry1.RegisteredAt
	firstHeartbeatAt := entry1.HeartbeatAt

	// Wait a moment to ensure timestamps would differ
	time.Sleep(10 * time.Millisecond)

	// 2. Second registration (idempotent)
	err = reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-idem",
		Endpoint:  "http://agent-idem.tenant-a.svc:8080",
		Tools:     []string{"search", "calculator"},
	})
	if err != nil {
		t.Fatalf("second Register failed: %v", err)
	}

	// 3. Verify only one entry exists
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 agent in list after idempotent register, got %d", len(list))
	}

	// 4. Verify RegisteredAt is preserved, HeartbeatAt is refreshed
	entry2, ok := reg.Get("tenant-a", "agent-idem")
	if !ok || entry2 == nil {
		t.Fatalf("expected agent-idem to be found after second registration")
	}
	if !entry2.RegisteredAt.Equal(firstRegisteredAt) {
		t.Errorf("RegisteredAt changed: was %v, now %v", firstRegisteredAt, entry2.RegisteredAt)
	}
	if !entry2.HeartbeatAt.After(firstHeartbeatAt) {
		t.Errorf("HeartbeatAt not refreshed: was %v, now %v", firstHeartbeatAt, entry2.HeartbeatAt)
	}

	// 5. Verify tools updated
	if len(entry2.Tools) != 2 {
		t.Errorf("expected 2 tools after second registration, got %d", len(entry2.Tools))
	}
}

func TestRegistry_TTLSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := NewRegistry(nil, "agentrax-system", 100*time.Millisecond)
	reg.SetSweepInterval(50 * time.Millisecond)

	// Register entry with 100ms TTL
	err := reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-short",
		Endpoint:  "http://agent-short.tenant-a.svc:8080",
		TTL:       80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// Register second entry with long TTL
	err = reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-long",
		Endpoint:  "http://agent-long.tenant-a.svc:8080",
		TTL:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	reg.Start(ctx)

	// Wait 150ms for short entry to expire and sweep
	time.Sleep(200 * time.Millisecond)

	if _, ok := reg.Get("tenant-a", "agent-short"); ok {
		t.Errorf("expected agent-short to have expired and been swept")
	}
	if _, ok := reg.Get("tenant-a", "agent-long"); !ok {
		t.Errorf("expected agent-long to remain registered")
	}
}

func TestRegistry_Heartbeat(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(nil, "agentrax-system", 150*time.Millisecond)

	err := reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-hb",
		Endpoint:  "http://agent-hb.tenant-a.svc:8080",
	})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// Sleep 100ms
	time.Sleep(100 * time.Millisecond)

	// Send heartbeat
	err = reg.Heartbeat(ctx, "tenant-a", "agent-hb")
	if err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}

	// Sleep another 100ms (total 200ms elapsed since register, but only 100ms since heartbeat)
	time.Sleep(100 * time.Millisecond)

	if _, ok := reg.Get("tenant-a", "agent-hb"); !ok {
		t.Errorf("expected agent-hb to still be alive due to heartbeat")
	}
}

func TestRegistry_ConfigMapPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reg := NewRegistry(fakeClient, "agentrax-system", 60*time.Second)

	// 1. Register agent
	err := reg.Register(ctx, Entry{
		Namespace: "tenant-a",
		Name:      "agent-cm",
		Endpoint:  "http://agent-cm.tenant-a.svc:8080",
		Tools:     []string{"search"},
	})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// 2. Create new registry instance to test recovery from ConfigMap
	recoveredReg := NewRegistry(fakeClient, "agentrax-system", 60*time.Second)
	recoveredReg.Start(ctx)

	entry, ok := recoveredReg.Get("tenant-a", "agent-cm")
	if !ok || entry == nil {
		t.Fatalf("expected agent-cm to be recovered from ConfigMap")
	}
	if entry.Endpoint != "http://agent-cm.tenant-a.svc:8080" {
		t.Errorf("got endpoint %q, want http://agent-cm.tenant-a.svc:8080", entry.Endpoint)
	}
}

func TestRegistry_HTTPHandler(t *testing.T) {
	reg := NewRegistry(nil, "agentrax-system", 60*time.Second)
	handler := reg.Handler()

	// 1. POST /agents (RESTful create/register)
	regPayload := `{"namespace":"tenant-1","name":"agent-http","endpoint":"http://agent-http:8080","tools":["translate"]}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(regPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /agents returned %d: %s", w.Code, w.Body.String())
	}

	// 2. GET /agents (List)
	req = httptest.NewRequest(http.MethodGet, "/agents", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /agents returned %d", w.Code)
	}
	var list []*Entry
	if err := json.NewDecoder(w.Body).Decode(&list); err != nil {
		t.Fatalf("decoding GET /agents response: %v", err)
	}
	if len(list) != 1 || list[0].Name != "agent-http" {
		t.Fatalf("unexpected GET /agents response: %+v", list)
	}

	// 3. GET /agents/{namespace}/{name} (Get)
	req = httptest.NewRequest(http.MethodGet, "/agents/tenant-1/agent-http", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /agents/tenant-1/agent-http returned %d", w.Code)
	}

	// 4. DELETE /agents/{namespace}/{name} (RESTful delete)
	req = httptest.NewRequest(http.MethodDelete, "/agents/tenant-1/agent-http", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /agents/tenant-1/agent-http returned %d: %s", w.Code, w.Body.String())
	}

	// Verify not found after delete
	if _, ok := reg.Get("tenant-1", "agent-http"); ok {
		t.Fatalf("expected agent-http to be deregistered")
	}
}

func TestHTTPMCPClient_Initialize(t *testing.T) {
	// Mock MCP Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/initialize" {
			http.NotFound(w, r)
			return
		}
		var req mcpInitializeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp := mcpInitializeResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: &mcpInitializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities: mcpCapabilities{
					Tools: &mcpToolsCapability{
						Available: []string{"web_search", "code_eval"},
					},
				},
				ServerInfo: &mcpClientInfo{
					Name:    "mock-agent",
					Version: "0.1.0",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewHTTPMCPClient()
	tools, err := client.Initialize(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if len(tools) != 2 || tools[0] != "web_search" || tools[1] != "code_eval" {
		t.Errorf("unexpected tools extracted: %+v", tools)
	}
}

// mockMCPClient is a mock implementation of MCPClient for testing.
type mockMCPClient struct {
	tools []string
	err   error
}

func (m *mockMCPClient) Initialize(ctx context.Context, endpoint string) ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.tools, nil
}

func TestRegistrar_RegisterAndHeartbeat(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(nil, "agentrax-system", 10*time.Second)
	mockClient := &mockMCPClient{
		tools: []string{"toolA", "toolB"},
	}

	registrar := NewRegistrar(reg, mockClient)

	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "tenant-x",
		},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Port: 9000,
			MCP: agentraxv1alpha1.MCPConfig{
				Expose: true,
				Tools:  []string{"customTool"},
			},
		},
	}

	// 1. Register
	err := registrar.Register(ctx, ad)
	if err != nil {
		t.Fatalf("Registrar.Register failed: %v", err)
	}

	entry, ok := reg.Get("tenant-x", "test-agent")
	if !ok || entry == nil {
		t.Fatalf("expected test-agent in registry")
	}
	if entry.Endpoint != "http://test-agent.tenant-x.svc:9000" {
		t.Errorf("unexpected endpoint: %s", entry.Endpoint)
	}
	if len(entry.Tools) != 3 { // toolA, toolB, customTool
		t.Errorf("got %d tools, want 3: %+v", len(entry.Tools), entry.Tools)
	}

	// 2. Successful Heartbeat
	err = registrar.Heartbeat(ctx, ad)
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}

	// 3. Failing Heartbeat -> 3 strikes deregisters
	mockClient.err = errors.New("connection refused")
	// Strike 1
	err = registrar.Heartbeat(ctx, ad)
	if err == nil {
		t.Errorf("expected error on strike 1")
	}
	if errors.Is(err, ErrHeartbeatDeregistered) {
		t.Errorf("strike 1 should not return ErrHeartbeatDeregistered")
	}
	if _, ok := reg.Get("tenant-x", "test-agent"); !ok {
		t.Errorf("entry should still exist after 1 strike")
	}

	// Strike 2
	err = registrar.Heartbeat(ctx, ad)
	if err == nil {
		t.Errorf("expected error on strike 2")
	}
	if errors.Is(err, ErrHeartbeatDeregistered) {
		t.Errorf("strike 2 should not return ErrHeartbeatDeregistered")
	}
	if _, ok := reg.Get("tenant-x", "test-agent"); !ok {
		t.Errorf("entry should still exist after 2 strikes")
	}

	// Strike 3 -> deregistration
	err = registrar.Heartbeat(ctx, ad)
	if err == nil {
		t.Errorf("expected error on strike 3")
	}
	if !errors.Is(err, ErrHeartbeatDeregistered) {
		t.Errorf("strike 3 should return ErrHeartbeatDeregistered, got %v", err)
	}
	if _, ok := reg.Get("tenant-x", "test-agent"); ok {
		t.Errorf("entry should be removed after 3 consecutive failures")
	}
}

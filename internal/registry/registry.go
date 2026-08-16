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

// Package registry provides an in-process MCP (Model Context Protocol) service registry
// backed by a Kubernetes ConfigMap with automated TTL expiration and health tracking.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultTTL is the default time-to-live for a registry entry without a heartbeat.
	DefaultTTL = 90 * time.Second

	// DefaultSweepInterval is the frequency at which the background TTL sweeper runs.
	DefaultSweepInterval = 30 * time.Second

	// DefaultConfigMapName is the name of the ConfigMap storing registry state.
	DefaultConfigMapName = "agentrax-registry"

	// configMapKey is the data key under which the serialized registry entries are stored.
	configMapKey = "state"
)

// Entry represents a registered agent in the MCP discovery registry.
type Entry struct {
	// Namespace is the Kubernetes namespace of the agent.
	Namespace string `json:"namespace"`

	// Name is the name of the AgentDeployment.
	Name string `json:"name"`

	// Endpoint is the full HTTP address for the agent's MCP service (e.g. "http://my-agent.tenant-a.svc:8080").
	Endpoint string `json:"endpoint"`

	// Tools is the list of tools advertised by this agent via MCP initialize.
	Tools []string `json:"tools,omitempty"`

	// RegisteredAt is the timestamp when the agent was first registered.
	RegisteredAt time.Time `json:"registeredAt"`

	// HeartbeatAt is the timestamp of the latest successful heartbeat or registration update.
	HeartbeatAt time.Time `json:"heartbeatAt"`

	// TTL is the time-to-live duration for this entry.
	TTL time.Duration `json:"ttl"`
}

// Registry manages in-memory agent registration entries, exposes HTTP discovery endpoints,
// persists state to a ConfigMap, and sweeps expired entries.
type Registry struct {
	mu            sync.RWMutex
	entries       map[string]*Entry
	client        client.Client
	namespace     string
	configMapName string
	defaultTTL    time.Duration
	sweepInterval time.Duration
}

// NewRegistry creates a new in-memory MCP Registry.
// If k8sClient is non-nil, registry state is persisted to and recovered from a ConfigMap.
func NewRegistry(k8sClient client.Client, namespace string, defaultTTL time.Duration) *Registry {
	if defaultTTL <= 0 {
		defaultTTL = DefaultTTL
	}
	if namespace == "" {
		namespace = "agentrax-system"
	}
	return &Registry{
		entries:       make(map[string]*Entry),
		client:        k8sClient,
		namespace:     namespace,
		configMapName: DefaultConfigMapName,
		defaultTTL:    defaultTTL,
		sweepInterval: DefaultSweepInterval,
	}
}

// SetSweepInterval overrides the default background sweep frequency (useful for testing).
func (r *Registry) SetSweepInterval(interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepInterval = interval
}

// Start launches the background TTL sweeper and loads any existing state from ConfigMap.
func (r *Registry) Start(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("mcp-registry")
	if r.client != nil {
		if err := r.loadFromConfigMap(ctx); err != nil {
			logger.Error(err, "failed to load initial registry state from ConfigMap")
		}
	}

	go r.runSweeper(ctx)
}

// runSweeper periodically deletes entries whose heartbeats have exceeded their TTL.
func (r *Registry) runSweeper(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("mcp-registry-sweeper")
	ticker := time.NewTicker(r.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed := r.sweepExpired()
			if removed > 0 {
				logger.Info("swept expired MCP registry entries", "removedCount", removed)
				if r.client != nil {
					if err := r.persistToConfigMap(ctx); err != nil {
						logger.Error(err, "failed to persist registry state after sweep")
					}
				}
			}
		}
	}
}

// sweepExpired cleans up entries that have exceeded their TTL and returns the count of removed entries.
func (r *Registry) sweepExpired() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	removed := 0
	for key, entry := range r.entries {
		ttl := entry.TTL
		if ttl <= 0 {
			ttl = r.defaultTTL
		}
		if now.Sub(entry.HeartbeatAt) > ttl {
			delete(r.entries, key)
			removed++
		}
	}
	return removed
}

// key returns the map key for a namespace/name pair.
func key(namespace, name string) string {
	return namespace + "/" + name
}

// Register registers or updates an agent in the registry.
func (r *Registry) Register(ctx context.Context, e Entry) error {
	if e.Namespace == "" || e.Name == "" || e.Endpoint == "" {
		return errors.New("namespace, name, and endpoint are required for registration")
	}

	now := time.Now()
	k := key(e.Namespace, e.Name)

	r.mu.Lock()
	existing, found := r.entries[k]
	if !found {
		e.RegisteredAt = now
	} else {
		e.RegisteredAt = existing.RegisteredAt
	}
	e.HeartbeatAt = now
	if e.TTL <= 0 {
		e.TTL = r.defaultTTL
	}
	r.entries[k] = &e
	r.mu.Unlock()

	if r.client != nil {
		return r.persistToConfigMap(ctx)
	}
	return nil
}

// Deregister removes an agent from the registry by namespace and name.
func (r *Registry) Deregister(ctx context.Context, namespace, name string) error {
	k := key(namespace, name)

	r.mu.Lock()
	_, found := r.entries[k]
	if !found {
		r.mu.Unlock()
		return nil
	}
	delete(r.entries, k)
	r.mu.Unlock()

	if r.client != nil {
		return r.persistToConfigMap(ctx)
	}
	return nil
}

// Heartbeat refreshes the heartbeat timestamp of a registered agent.
func (r *Registry) Heartbeat(ctx context.Context, namespace, name string) error {
	k := key(namespace, name)

	r.mu.Lock()
	entry, found := r.entries[k]
	if !found {
		r.mu.Unlock()
		return fmt.Errorf("agent %s not found in registry", k)
	}
	entry.HeartbeatAt = time.Now()
	r.mu.Unlock()

	// Heartbeats only update timestamps in-memory; persisting on every heartbeat
	// would generate unnecessary ConfigMap write load.
	return nil
}

// Get looks up an agent entry by namespace and name. Returns false if not found or expired.
func (r *Registry) Get(namespace, name string) (*Entry, bool) {
	k := key(namespace, name)

	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, found := r.entries[k]
	if !found {
		return nil, false
	}

	ttl := entry.TTL
	if ttl <= 0 {
		ttl = r.defaultTTL
	}
	if time.Since(entry.HeartbeatAt) > ttl {
		return nil, false
	}

	entryCopy := *entry
	return &entryCopy, true
}

// List returns all non-expired agent entries in the registry.
func (r *Registry) List() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	result := make([]*Entry, 0, len(r.entries))
	for _, entry := range r.entries {
		ttl := entry.TTL
		if ttl <= 0 {
			ttl = r.defaultTTL
		}
		if now.Sub(entry.HeartbeatAt) <= ttl {
			entryCopy := *entry
			result = append(result, &entryCopy)
		}
	}
	return result
}

// ── ConfigMap Persistence ─────────────────────────────────────────────────────

// persistToConfigMap writes the current entries to the agentrax-registry ConfigMap.
func (r *Registry) persistToConfigMap(ctx context.Context) error {
	r.mu.RLock()
	data, err := json.Marshal(r.entries)
	r.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshaling registry state: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.configMapName,
			Namespace: r.namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.client, cm, func() error {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[configMapKey] = string(data)
		return nil
	})
	if err != nil {
		return fmt.Errorf("persisting registry ConfigMap: %w", err)
	}
	return nil
}

// loadFromConfigMap reads and restores entries from the agentrax-registry ConfigMap.
func (r *Registry) loadFromConfigMap(ctx context.Context) error {
	cm := &corev1.ConfigMap{}
	err := r.client.Get(ctx, types.NamespacedName{Name: r.configMapName, Namespace: r.namespace}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching registry ConfigMap: %w", err)
	}

	raw, ok := cm.Data[configMapKey]
	if !ok || raw == "" {
		return nil
	}

	var loaded map[string]*Entry
	if err := json.Unmarshal([]byte(raw), &loaded); err != nil {
		return fmt.Errorf("unmarshaling registry state: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range loaded {
		r.entries[k] = v
	}
	return nil
}

// ── HTTP Handler ──────────────────────────────────────────────────────────────

// Handler returns an http.Handler implementing the registry REST API.
// It exposes standard RESTful endpoints under /agents as well as legacy aliases (/register, /deregister).
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()

	// Consolidated RESTful /agents collection endpoints
	mux.HandleFunc("GET /agents", r.handleListAgents)
	mux.HandleFunc("POST /agents", r.handleRegister)
	mux.HandleFunc("GET /agents/{namespace}/{name}", r.handleGetAgent)
	mux.HandleFunc("DELETE /agents/{namespace}/{name}", r.handleDeregisterAgentPath)

	// Backward-compatible RPC action aliases
	mux.HandleFunc("POST /register", r.handleRegister)
	mux.HandleFunc("DELETE /deregister", r.handleDeregister)

	return mux
}

func (r *Registry) handleRegister(w http.ResponseWriter, req *http.Request) {
	var entry Entry
	if err := json.NewDecoder(req.Body).Decode(&entry); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if err := r.Register(req.Context(), entry); err != nil {
		http.Error(w, fmt.Sprintf("registration failed: %v", err), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
}

func (r *Registry) handleDeregisterAgentPath(w http.ResponseWriter, req *http.Request) {
	namespace := req.PathValue("namespace")
	name := req.PathValue("name")

	if namespace == "" || name == "" {
		http.Error(w, "namespace and name are required in URL path", http.StatusBadRequest)
		return
	}

	if err := r.Deregister(req.Context(), namespace, name); err != nil {
		http.Error(w, fmt.Sprintf("deregistration failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deregistered"})
}

func (r *Registry) handleDeregister(w http.ResponseWriter, req *http.Request) {
	namespace := req.URL.Query().Get("namespace")
	name := req.URL.Query().Get("name")

	if namespace == "" || name == "" {
		// Also support JSON body if query params are missing
		var body struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			namespace = body.Namespace
			name = body.Name
		}
	}

	if namespace == "" || name == "" {
		http.Error(w, "namespace and name are required", http.StatusBadRequest)
		return
	}

	if err := r.Deregister(req.Context(), namespace, name); err != nil {
		http.Error(w, fmt.Sprintf("deregistration failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deregistered"})
}

func (r *Registry) handleListAgents(w http.ResponseWriter, req *http.Request) {
	agents := r.List()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(agents)
}

func (r *Registry) handleGetAgent(w http.ResponseWriter, req *http.Request) {
	namespace := req.PathValue("namespace")
	name := req.PathValue("name")

	if namespace == "" || name == "" {
		http.Error(w, "namespace and name are required", http.StatusBadRequest)
		return
	}

	entry, found := r.Get(namespace, name)
	if !found {
		http.Error(w, fmt.Sprintf("agent %s/%s not found", namespace, name), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(entry)
}

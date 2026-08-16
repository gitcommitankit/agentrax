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
	"context"
	"fmt"
	"sync"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

const (
	// MaxConsecutiveHeartbeatFailures is the number of failed heartbeat probes before an agent is automatically deregistered.
	MaxConsecutiveHeartbeatFailures = 3
)

// Registrar coordinates MCP initialize handshakes and updates the discovery Registry.
type Registrar struct {
	// Registry is the underlying registry store and HTTP server handler.
	Registry *Registry

	// MCPClient is the protocol client used for MCP initialize handshakes.
	MCPClient MCPClient

	failuresMu sync.Mutex
	failures   map[string]int
}

// NewRegistrar creates a new Registrar instance with the provided registry and MCP client.
func NewRegistrar(reg *Registry, client MCPClient) *Registrar {
	if client == nil {
		client = NewHTTPMCPClient()
	}
	return &Registrar{
		Registry:  reg,
		MCPClient: client,
		failures:  make(map[string]int),
	}
}

// EndpointForAgent computes the standard Kubernetes Service URL for an AgentDeployment.
func EndpointForAgent(ad *agentraxv1alpha1.AgentDeployment) string {
	port := ad.Spec.Port
	if port == 0 {
		port = 8080
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", ad.Name, ad.Namespace, port)
}

// Register performs the MCP initialize handshake against the agent's endpoint and records the agent in the registry.
func (r *Registrar) Register(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if r.Registry == nil {
		return fmt.Errorf("registry not initialized")
	}

	endpoint := EndpointForAgent(ad)

	// Perform initialize handshake to discover advertised tools and verify MCP readiness.
	tools, err := r.MCPClient.Initialize(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("MCP handshake with %s failed: %w", endpoint, err)
	}

	// If spec declared tools explicitly, combine them with discovered tools.
	if len(ad.Spec.MCP.Tools) > 0 {
		seen := make(map[string]bool)
		for _, t := range tools {
			seen[t] = true
		}
		for _, t := range ad.Spec.MCP.Tools {
			if !seen[t] {
				tools = append(tools, t)
				seen[t] = true
			}
		}
	}

	entry := Entry{
		Namespace: ad.Namespace,
		Name:      ad.Name,
		Endpoint:  endpoint,
		Tools:     tools,
	}

	if err := r.Registry.Register(ctx, entry); err != nil {
		return fmt.Errorf("registering agent in store: %w", err)
	}

	r.resetFailures(ad.Namespace, ad.Name)
	return nil
}

// Deregister removes the agent from the registry store.
func (r *Registrar) Deregister(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if r.Registry == nil {
		return nil
	}

	r.resetFailures(ad.Namespace, ad.Name)
	return r.Registry.Deregister(ctx, ad.Namespace, ad.Name)
}

// Heartbeat performs an initialize probe against the agent and refreshes the registry TTL if healthy.
// After 3 consecutive probe failures, the agent is automatically deregistered.
func (r *Registrar) Heartbeat(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if r.Registry == nil {
		return nil
	}

	endpoint := EndpointForAgent(ad)
	_, err := r.MCPClient.Initialize(ctx, endpoint)
	if err != nil {
		failCount := r.incrementFailures(ad.Namespace, ad.Name)
		if failCount >= MaxConsecutiveHeartbeatFailures {
			_ = r.Registry.Deregister(ctx, ad.Namespace, ad.Name)
			return fmt.Errorf("deregistered after %d consecutive heartbeat failures (latest error: %w)", failCount, err)
		}
		return fmt.Errorf("heartbeat probe failed (%d/%d): %w", failCount, MaxConsecutiveHeartbeatFailures, err)
	}

	r.resetFailures(ad.Namespace, ad.Name)
	return r.Registry.Heartbeat(ctx, ad.Namespace, ad.Name)
}

func (r *Registrar) incrementFailures(namespace, name string) int {
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	if r.failures == nil {
		r.failures = make(map[string]int)
	}
	k := namespace + "/" + name
	r.failures[k]++
	return r.failures[k]
}

func (r *Registrar) resetFailures(namespace, name string) {
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	if r.failures != nil {
		delete(r.failures, namespace+"/"+name)
	}
}

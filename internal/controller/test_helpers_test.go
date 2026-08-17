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

package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

const (
	// timeout is the maximum time any Eventually assertion waits in this package.
	timeout = 30 * time.Second
	// interval is how often Eventually polls.
	interval = 250 * time.Millisecond
)

// namespacedName is a convenience wrapper for building types.NamespacedName.
func namespacedName(name, namespace string) types.NamespacedName { //nolint:unparam
	return types.NamespacedName{Name: name, Namespace: namespace}
}

// namespaceObject creates a Namespace object for use in test setup.
func namespaceObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// inNamespace returns a ListOption that filters by namespace.
func inNamespace(ns string) client.ListOption {
	return client.InNamespace(ns)
}

// mockAgentRegistrar is a test double implementing AgentRegistrar.
type mockAgentRegistrar struct {
	registerFn   func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error
	deregisterFn func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error
	heartbeatFn  func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error
}

func (m *mockAgentRegistrar) Register(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if m.registerFn != nil {
		return m.registerFn(ctx, ad)
	}
	return nil
}

func (m *mockAgentRegistrar) Deregister(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if m.deregisterFn != nil {
		return m.deregisterFn(ctx, ad)
	}
	return nil
}

func (m *mockAgentRegistrar) Heartbeat(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if m.heartbeatFn != nil {
		return m.heartbeatFn(ctx, ad)
	}
	return nil
}

// registrarProxy is a thread-safe proxy that wraps an AgentRegistrar and
// allows per-test swapping of the delegate without data races.
// This is installed once in the reconciler before manager start, then tests
// can safely swap the delegate using SetDelegate.
type registrarProxy struct {
	mu       sync.RWMutex
	delegate AgentRegistrar
}

func newRegistrarProxy(initial AgentRegistrar) *registrarProxy {
	return &registrarProxy{delegate: initial}
}

func (p *registrarProxy) SetDelegate(d AgentRegistrar) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delegate = d
}

func (p *registrarProxy) Register(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	p.mu.RLock()
	d := p.delegate
	p.mu.RUnlock()
	return d.Register(ctx, ad)
}

func (p *registrarProxy) Deregister(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	p.mu.RLock()
	d := p.delegate
	p.mu.RUnlock()
	return d.Deregister(ctx, ad)
}

func (p *registrarProxy) Heartbeat(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	p.mu.RLock()
	d := p.delegate
	p.mu.RUnlock()
	return d.Heartbeat(ctx, ad)
}

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

package webhook_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	agentraxwebhook "github.com/gitcommitankit/agentrax/internal/webhook"
)

// newDefaulter constructs the defaulter under test.
func newDefaulter() *agentraxwebhook.AgentDeploymentCustomDefaulter {
	return &agentraxwebhook.AgentDeploymentCustomDefaulter{}
}

// baseAD returns a minimal AgentDeployment fixture for defaulter testing.
func baseAD() *agentraxv1alpha1.AgentDeployment {
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ad", Namespace: "test-ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "test:v1",
			TenantRef: "tenant",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 2, Metric: "queueDepth", Target: 50},
		},
	}
}

// TestDefaulter_PortDefaulted verifies that port defaults to 8080 when unset.
func TestDefaulter_PortDefaulted(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if ad.Spec.Port != 8080 {
		t.Errorf("Port = %d, want 8080", ad.Spec.Port)
	}
}

// TestDefaulter_PortNotOverwritten verifies that an explicit port is preserved.
func TestDefaulter_PortNotOverwritten(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	ad.Spec.Port = 9090
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if ad.Spec.Port != 9090 {
		t.Errorf("Port was overwritten: got %d, want 9090", ad.Spec.Port)
	}
}

// TestDefaulter_StrategyDefaulted verifies that rollout strategy defaults to Recreate when unset.
func TestDefaulter_StrategyDefaulted(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if ad.Spec.Rollout.Strategy != "Recreate" {
		t.Errorf("Strategy = %q, want Recreate", ad.Spec.Rollout.Strategy)
	}
}

// TestDefaulter_StrategyNotOverwritten verifies that explicit rollout strategy is preserved.
func TestDefaulter_StrategyNotOverwritten(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	ad.Spec.Rollout.Strategy = "Canary"
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if ad.Spec.Rollout.Strategy != "Canary" {
		t.Errorf("Strategy was overwritten: got %q, want Canary", ad.Spec.Rollout.Strategy)
	}
}

// TestDefaulter_ResourcesDefaulted verifies that default CPU/memory requests and limits are populated when unset.
func TestDefaulter_ResourcesDefaulted(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if len(ad.Spec.Resources.Requests) == 0 {
		t.Error("Resources.Requests not defaulted")
	}
	if len(ad.Spec.Resources.Limits) == 0 {
		t.Error("Resources.Limits not defaulted")
	}
	// Verify the defaults are sane values.
	cpuReq := ad.Spec.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("default CPU request = %s, want 100m", cpuReq.String())
	}
}

// TestDefaulter_ResourcesNotOverwritten verifies that explicitly specified resource requirements are preserved.
func TestDefaulter_ResourcesNotOverwritten(t *testing.T) {
	t.Parallel()
	ad := baseAD()
	ad.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("200m"),
		},
	}
	if err := newDefaulter().Default(context.Background(), ad); err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	cpu := ad.Spec.Resources.Requests[corev1.ResourceCPU]
	if cpu.Cmp(resource.MustParse("200m")) != 0 {
		t.Errorf("Resources.Requests[cpu] was changed: got %s, want 200m", cpu.String())
	}
}

// TestDefaulter_WrongType verifies that passing an unexpected runtime object type returns an error.
func TestDefaulter_WrongType(t *testing.T) {
	t.Parallel()
	d := newDefaulter()
	// Passing a non-AgentDeployment object should return an error.
	if err := d.Default(context.Background(), &agentraxv1alpha1.TenantQuota{}); err == nil {
		t.Error("expected error when passing wrong type, got nil")
	}
}

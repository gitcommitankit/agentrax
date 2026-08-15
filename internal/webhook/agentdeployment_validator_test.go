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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/quota"
	agentraxwebhook "github.com/gitcommitankit/agentrax/internal/webhook"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newValidatorWithTQ builds a validator backed by a fake client that has one
// pre-existing TenantQuota with the given spec and status in namespace "ns".
func newValidatorWithTQ(t *testing.T, tqSpec agentraxv1alpha1.TenantQuotaSpec, tqStatus agentraxv1alpha1.TenantQuotaStatus) *agentraxwebhook.AgentDeploymentCustomValidator {
	t.Helper()
	tq := &agentraxv1alpha1.TenantQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tq", Namespace: "ns"},
		Spec:       tqSpec,
		Status:     tqStatus,
	}
	scheme := runtime.NewScheme()
	if err := agentraxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(tq).
		WithStatusSubresource(tq).
		Build()
	e := quota.NewEnforcer(quota.DefaultGPUResourceName)
	t.Cleanup(e.Stop)
	return &agentraxwebhook.AgentDeploymentCustomValidator{Client: fakeClient, Enforcer: e}
}

// newValidatorNoTQ builds a validator backed by a fake client with no TenantQuota.
func newValidatorNoTQ(t *testing.T) *agentraxwebhook.AgentDeploymentCustomValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := agentraxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	e := quota.NewEnforcer(quota.DefaultGPUResourceName)
	t.Cleanup(e.Stop)
	return &agentraxwebhook.AgentDeploymentCustomValidator{Client: fakeClient, Enforcer: e}
}

// permissiveTQSpec returns a TenantQuotaSpec with generous headroom.
func permissiveTQSpec() agentraxv1alpha1.TenantQuotaSpec {
	return agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents: 10, MaxGPUs: 0, MaxTotalReplicas: 50, MaxReplicasPerAgent: 10,
	}
}

// validCanaryAD returns an AD with a fully-valid Canary rollout spec.
func validCanaryAD() *agentraxv1alpha1.AgentDeployment {
	w10, w100 := int32(10), int32(100)
	pause := metav1.Duration{Duration: 5 * time.Minute}
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "tq",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 3, Metric: "queueDepth", Target: 50},
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{SetWeight: &w10},
					{Pause: &pause},
					{SetWeight: &w100},
				},
				Rollback: agentraxv1alpha1.RollbackPolicy{
					MaxErrorRate:     "2%",
					MaxP99LatencyMs:  3000,
					MinRequestSample: 200,
				},
			},
		},
	}
}

// ── ValidateCreate tests ──────────────────────────────────────────────────────

// TestValidateCreate_HappyPath verifies successful admission validation of a valid AgentDeployment.
func TestValidateCreate_HappyPath(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v1",
			TenantRef: "tq",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 3, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err != nil {
		t.Errorf("ValidateCreate() unexpected error: %v", err)
	}
}

// TestValidateCreate_TenantRefNotFound verifies admission rejection when the referenced TenantQuota does not exist.
func TestValidateCreate_TenantRefNotFound(t *testing.T) {
	t.Parallel()
	v := newValidatorNoTQ(t)
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "nonexistent",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 2, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing TenantQuota, got nil")
	}
}

// TestValidateCreate_ReplicasMinGtMax verifies admission rejection when replicas.min exceeds replicas.max.
func TestValidateCreate_ReplicasMinGtMax(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 5, Max: 2, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for min > max, got nil")
	}
}

// TestValidateCreate_OverMaxAgents verifies admission rejection when tenant maxAgents quota is reached.
func TestValidateCreate_OverMaxAgents(t *testing.T) {
	t.Parallel()
	tqSpec := agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents: 2, MaxGPUs: 0, MaxTotalReplicas: 20, MaxReplicasPerAgent: 10,
	}
	// Status already shows 2 agents used — a new one would exceed maxAgents.
	tqStatus := agentraxv1alpha1.TenantQuotaStatus{UsedAgents: 2}
	v := newValidatorWithTQ(t, tqSpec, tqStatus)
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad-new", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 2, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected quota rejection for over-maxAgents, got nil")
	}
}

// TestValidateCreate_OverMaxReplicasPerAgent verifies admission rejection when replicas.max exceeds maxReplicasPerAgent.
func TestValidateCreate_OverMaxReplicasPerAgent(t *testing.T) {
	t.Parallel()
	tqSpec := agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents: 10, MaxGPUs: 0, MaxTotalReplicas: 50, MaxReplicasPerAgent: 3,
	}
	v := newValidatorWithTQ(t, tqSpec, agentraxv1alpha1.TenantQuotaStatus{})
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			// replicas.max=5 exceeds maxReplicasPerAgent=3
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 5, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected rejection for maxReplicasPerAgent violation, got nil")
	}
}

// TestValidateCreate_WrongType verifies that passing an unexpected runtime object type returns an error.
func TestValidateCreate_WrongType(t *testing.T) {
	t.Parallel()
	v := newValidatorNoTQ(t)
	_, err := v.ValidateCreate(context.Background(), &agentraxv1alpha1.TenantQuota{})
	if err == nil {
		t.Error("expected error for wrong object type, got nil")
	}
}

// ── validateCanarySpec tests ──────────────────────────────────────────────────

// TestValidateCreate_Canary_HappyPath verifies admission validation of a well-formed canary rollout spec.
func TestValidateCreate_Canary_HappyPath(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	_, err := v.ValidateCreate(context.Background(), validCanaryAD())
	if err != nil {
		t.Errorf("ValidateCreate() for valid canary AD unexpected error: %v", err)
	}
}

// TestValidateCreate_Canary_NoSteps verifies rejection when canary rollout has no steps.
func TestValidateCreate_Canary_NoSteps(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	ad.Spec.Rollout.Steps = nil
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing canary steps, got nil")
	}
}

// TestValidateCreate_Canary_StepBothFields verifies rejection when a canary step sets both setWeight and pause.
func TestValidateCreate_Canary_StepBothFields(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	w := int32(50)
	pause := metav1.Duration{Duration: 5 * time.Minute}
	// Set both setWeight and pause on the same step — must be rejected.
	ad.Spec.Rollout.Steps[0] = agentraxv1alpha1.CanaryStep{SetWeight: &w, Pause: &pause}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for step with both setWeight and pause, got nil")
	}
}

// TestValidateCreate_Canary_NoFullWeight verifies rejection when no canary step promotes to 100% weight.
func TestValidateCreate_Canary_NoFullWeight(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	w50 := int32(50)
	// Replace all steps with one that never reaches 100.
	ad.Spec.Rollout.Steps = []agentraxv1alpha1.CanaryStep{{SetWeight: &w50}}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing setWeight: 100 step, got nil")
	}
}

// TestValidateCreate_Canary_MissingMaxErrorRate verifies rejection when canary rollback criteria omits maxErrorRate.
func TestValidateCreate_Canary_MissingMaxErrorRate(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	ad.Spec.Rollout.Rollback.MaxErrorRate = ""
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing maxErrorRate, got nil")
	}
}

// TestValidateCreate_Canary_InvalidMaxErrorRate verifies rejection when maxErrorRate is not a valid percentage string.
func TestValidateCreate_Canary_InvalidMaxErrorRate(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	ad.Spec.Rollout.Rollback.MaxErrorRate = "not-a-percent"
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for invalid maxErrorRate format, got nil")
	}
}

// TestValidateCreate_Canary_MissingMaxP99 verifies rejection when canary rollback criteria omits maxP99LatencyMs.
func TestValidateCreate_Canary_MissingMaxP99(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	ad.Spec.Rollout.Rollback.MaxP99LatencyMs = 0
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing maxP99LatencyMs, got nil")
	}
}

// TestValidateCreate_Canary_MissingMinRequestSample verifies rejection when minRequestSample is missing or zero.
func TestValidateCreate_Canary_MissingMinRequestSample(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := validCanaryAD()
	ad.Spec.Rollout.Rollback.MinRequestSample = 0
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for missing minRequestSample, got nil")
	}
}

// ── validateMCPTools tests ────────────────────────────────────────────────────

// TestValidateCreate_MCPTools_EmptyName verifies rejection of empty MCP tool names.
func TestValidateCreate_MCPTools_EmptyName(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 2, Metric: "queueDepth", Target: 50},
			MCP:      agentraxv1alpha1.MCPConfig{Expose: true, Tools: []string{"search", ""}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for empty MCP tool name, got nil")
	}
}

// TestValidateCreate_MCPTools_Duplicate verifies rejection of duplicate MCP tool names.
func TestValidateCreate_MCPTools_Duplicate(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 2, Metric: "queueDepth", Target: 50},
			MCP:      agentraxv1alpha1.MCPConfig{Expose: true, Tools: []string{"search", "search"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), ad)
	if err == nil {
		t.Error("expected error for duplicate MCP tool name, got nil")
	}
}

// ── ValidateUpdate tests ──────────────────────────────────────────────────────

// TestValidateUpdate_DeletionTimestampBypass verifies that objects being deleted bypass quota checks to avoid deadlocking finalizers.
func TestValidateUpdate_DeletionTimestampBypass(t *testing.T) {
	t.Parallel()
	// Even with a TQ that would reject quota, updates with DeletionTimestamp
	// must always pass — otherwise finalizer removal is deadlocked.
	tqSpec := agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents: 0, MaxGPUs: 0, MaxTotalReplicas: 0, MaxReplicasPerAgent: 0,
	}
	v := newValidatorWithTQ(t, tqSpec, agentraxv1alpha1.TenantQuotaStatus{})
	now := metav1.Now()
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ad", Namespace: "ns", DeletionTimestamp: &now,
		},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 5, Metric: "queueDepth", Target: 50},
		},
	}
	_, err := v.ValidateUpdate(context.Background(), ad, ad)
	if err != nil {
		t.Errorf("ValidateUpdate() with DeletionTimestamp should bypass all checks, got: %v", err)
	}
}

// TestValidateUpdate_RolloutInProgress_BlocksImageChange verifies that image changes are blocked during an active rollout.
func TestValidateUpdate_RolloutInProgress_BlocksImageChange(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	oldAD := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 3, Metric: "queueDepth", Target: 50},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{Phase: agentraxv1alpha1.PhaseRolloutInProgress},
	}
	newAD := oldAD.DeepCopy()
	newAD.Spec.Image = "img:v2" // image change while rollout is active
	_, err := v.ValidateUpdate(context.Background(), oldAD, newAD)
	if err == nil {
		t.Error("expected rejection of image change during RolloutInProgress, got nil")
	}
}

// TestValidateUpdate_RolloutInProgress_AllowsNonImageChange verifies that non-image updates (e.g. labels) are permitted during rollout.
func TestValidateUpdate_RolloutInProgress_AllowsNonImageChange(t *testing.T) {
	t.Parallel()
	v := newValidatorWithTQ(t, permissiveTQSpec(), agentraxv1alpha1.TenantQuotaStatus{})
	oldAD := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "ad", Namespace: "ns"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image: "img:v1", TenantRef: "tq",
			Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 3, Metric: "queueDepth", Target: 50},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{Phase: agentraxv1alpha1.PhaseRolloutInProgress},
	}
	newAD := oldAD.DeepCopy()
	// Non-image change (e.g. label): must not be blocked by the rollout guard.
	newAD.Labels = map[string]string{"env": "staging"}
	_, err := v.ValidateUpdate(context.Background(), oldAD, newAD)
	if err != nil {
		t.Errorf("non-image update during RolloutInProgress should be allowed, got: %v", err)
	}
}

// TestValidateUpdate_WrongType verifies that passing wrong object types to ValidateUpdate returns an error.
func TestValidateUpdate_WrongType(t *testing.T) {
	t.Parallel()
	v := newValidatorNoTQ(t)
	_, err := v.ValidateUpdate(context.Background(),
		&agentraxv1alpha1.TenantQuota{},
		&agentraxv1alpha1.TenantQuota{},
	)
	if err == nil {
		t.Error("expected error for wrong object type in ValidateUpdate, got nil")
	}
}

// ── ValidateDelete test ───────────────────────────────────────────────────────

// TestValidateDelete_AlwaysNil verifies that ValidateDelete always allows deletion without error.
func TestValidateDelete_AlwaysNil(t *testing.T) {
	t.Parallel()
	v := newValidatorNoTQ(t)
	_, err := v.ValidateDelete(context.Background(), &agentraxv1alpha1.AgentDeployment{})
	if err != nil {
		t.Errorf("ValidateDelete() should always return nil, got: %v", err)
	}
}

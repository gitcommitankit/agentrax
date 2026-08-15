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

// Package webhook implements the validating and mutating admission webhooks for
// Agentrax CRDs. It imports internal/quota to enforce tenant resource ceilings
// at admission time, which is why it lives in internal/ rather than api/ —
// keeping the import graph acyclic (api/v1alpha1 ← internal/webhook, never the reverse).
package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/quota"
)

var webhookLog = logf.Log.WithName("agentdeployment-webhook")

// reservationTTL is how long an in-flight reservation is kept while the
// webhook response is in transit. Generous to tolerate slow API server calls.
const reservationTTL = 5 * time.Second

// ── SetupWebhookWithManager ───────────────────────────────────────────────────

// SetupAgentDeploymentWebhookWithManager registers the mutating and validating
// webhook handlers for AgentDeployment with the controller-runtime Manager.
// enforcer is the shared Enforcer instance (must be non-nil).
func SetupAgentDeploymentWebhookWithManager(mgr ctrl.Manager, enforcer *quota.Enforcer) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&agentraxv1alpha1.AgentDeployment{}).
		WithDefaulter(&AgentDeploymentCustomDefaulter{}).
		WithValidator(&AgentDeploymentCustomValidator{
			Client:   mgr.GetAPIReader(),
			Enforcer: enforcer,
		}).
		Complete()
}

// ── Mutating webhook (defaulter) ─────────────────────────────────────────────

// AgentDeploymentCustomDefaulter applies defaults to AgentDeployment specs
// before they are persisted. It implements admission.CustomDefaulter.
type AgentDeploymentCustomDefaulter struct{}

var _ webhook.CustomDefaulter = &AgentDeploymentCustomDefaulter{}

// Default fills in missing optional fields with sensible defaults.
func (d *AgentDeploymentCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	ad, ok := obj.(*agentraxv1alpha1.AgentDeployment)
	if !ok {
		return fmt.Errorf("expected AgentDeployment, got %T", obj)
	}
	webhookLog.Info("applying defaults", "name", ad.Name, "namespace", ad.Namespace)

	// Default port to 8080 when omitted.
	if ad.Spec.Port == 0 {
		ad.Spec.Port = 8080
	}

	// Default rollout strategy to Recreate when omitted.
	if ad.Spec.Rollout.Strategy == "" {
		ad.Spec.Rollout.Strategy = "Recreate"
	}

	// Default resources to a conservative baseline when the entire Resources
	// field is zero-value (no requests AND no limits). We do not overwrite
	// partially-specified resource requirements.
	if isZeroResources(ad.Spec.Resources) {
		ad.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
	}

	return nil
}

// isZeroResources returns true when both Requests and Limits are nil or empty.
func isZeroResources(r corev1.ResourceRequirements) bool {
	return len(r.Requests) == 0 && len(r.Limits) == 0
}

// ── Validating webhook ────────────────────────────────────────────────────────

// AgentDeploymentCustomValidator validates AgentDeployment specs at admission
// time. It implements admission.CustomValidator.
type AgentDeploymentCustomValidator struct {
	Client   client.Reader
	Enforcer *quota.Enforcer
}

var _ webhook.CustomValidator = &AgentDeploymentCustomValidator{}

// ValidateCreate validates a new AgentDeployment against quota and spec rules.
func (v *AgentDeploymentCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	start := time.Now()
	ad, ok := obj.(*agentraxv1alpha1.AgentDeployment)
	if !ok {
		return nil, fmt.Errorf("expected AgentDeployment, got %T", obj)
	}
	webhookLog.Info("validating create", "name", ad.Name, "namespace", ad.Namespace)

	allErrs := v.validateSpec(ctx, ad, nil)

	latency := time.Since(start)
	webhookLog.V(1).Info("admission complete",
		"operation", "create",
		"name", ad.Name,
		"namespace", ad.Namespace,
		"latency_ms", latency.Milliseconds(),
		"admitted", len(allErrs) == 0)
	if latency > 2*time.Second {
		webhookLog.Info("slow admission request detected",
			"operation", "create",
			"name", ad.Name,
			"namespace", ad.Namespace,
			"latency_ms", latency.Milliseconds())
	}

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			agentraxv1alpha1.GroupVersion.WithKind("AgentDeployment").GroupKind(),
			ad.Name, allErrs)
	}
	return nil, nil
}

// ValidateUpdate validates an updated AgentDeployment against quota and spec rules.
func (v *AgentDeploymentCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	start := time.Now()
	ad, ok := newObj.(*agentraxv1alpha1.AgentDeployment)
	if !ok {
		return nil, fmt.Errorf("expected AgentDeployment, got %T", newObj)
	}
	oldAD, ok := oldObj.(*agentraxv1alpha1.AgentDeployment)
	if !ok {
		return nil, fmt.Errorf("expected old AgentDeployment, got %T", oldObj)
	}
	webhookLog.Info("validating update", "name", ad.Name, "namespace", ad.Namespace)

	// When the object is being deleted (DeletionTimestamp set), the reconciler is
	// updating it only to strip the finalizer. Blocking that with quota or TQ
	// checks would deadlock deletion — particularly if the TenantQuota was already
	// deleted before the AD's finalizer could be removed.
	if ad.DeletionTimestamp != nil {
		return nil, nil
	}

	// Block image changes while a rollout is already in progress.
	if oldAD.Spec.Image != ad.Spec.Image && oldAD.Status.Phase == agentraxv1alpha1.PhaseRolloutInProgress {
		return nil, apierrors.NewForbidden(
			agentraxv1alpha1.GroupVersion.WithResource("agentdeployments").GroupResource(),
			ad.Name,
			fmt.Errorf("image update rejected: a rollout is already in progress (status.phase=%s)",
				agentraxv1alpha1.PhaseRolloutInProgress),
		)
	}

	allErrs := v.validateSpec(ctx, ad, &oldAD.Spec)

	latency := time.Since(start)
	webhookLog.V(1).Info("admission complete",
		"operation", "update",
		"name", ad.Name,
		"namespace", ad.Namespace,
		"latency_ms", latency.Milliseconds(),
		"admitted", len(allErrs) == 0)
	if latency > 2*time.Second {
		webhookLog.Info("slow admission request detected",
			"operation", "update",
			"name", ad.Name,
			"namespace", ad.Namespace,
			"latency_ms", latency.Milliseconds())
	}

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			agentraxv1alpha1.GroupVersion.WithKind("AgentDeployment").GroupKind(),
			ad.Name, allErrs)
	}
	return nil, nil
}

// ValidateDelete is a no-op; deletion is controlled by the finalizer.
func (v *AgentDeploymentCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateSpec runs all spec-level validation rules and quota checks.
// oldSpec is nil for CREATE; non-nil for UPDATE (used for delta quota arithmetic).
func (v *AgentDeploymentCustomValidator) validateSpec(
	ctx context.Context,
	ad *agentraxv1alpha1.AgentDeployment,
	oldSpec *agentraxv1alpha1.AgentDeploymentSpec,
) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// ── 1. tenantRef must reference an existing TenantQuota in the same namespace ──
	tq := &agentraxv1alpha1.TenantQuota{}
	tqKey := client.ObjectKey{Namespace: ad.Namespace, Name: ad.Spec.TenantRef}
	if err := v.Client.Get(ctx, tqKey, tq); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("tenantRef"), ad.Spec.TenantRef,
				fmt.Sprintf("TenantQuota %q not found in namespace %q", ad.Spec.TenantRef, ad.Namespace),
			))
		} else {
			allErrs = append(allErrs, field.InternalError(specPath.Child("tenantRef"), err))
		}
		// Cannot proceed with quota checks without a valid TQ.
		return allErrs
	}

	// ── 2. replicas.min ≤ replicas.max ──
	replicasPath := specPath.Child("replicas")
	if ad.Spec.Replicas.Min > ad.Spec.Replicas.Max {
		allErrs = append(allErrs, field.Invalid(
			replicasPath.Child("min"), ad.Spec.Replicas.Min,
			fmt.Sprintf("must be ≤ spec.replicas.max (%d)", ad.Spec.Replicas.Max),
		))
	}

	// ── 3. Canary-specific spec rules ──
	if ad.Spec.Rollout.Strategy == "Canary" {
		allErrs = append(allErrs, v.validateCanarySpec(ad)...)
	}

	// ── 4. MCP tools uniqueness ──
	if len(ad.Spec.MCP.Tools) > 0 {
		allErrs = append(allErrs, validateMCPTools(specPath.Child("mcp", "tools"), ad.Spec.MCP.Tools)...)
	}

	// ── 5. Quota admission check (atomic check-and-reserve) ──
	// AdmitAndReserve holds the in-flight mutex across both the quota check
	// and the reservation write, preventing concurrent near-limit creates from
	// both slipping through the quota ceiling.
	// Only run when all earlier checks pass: a spec that is already invalid
	// must not create a reservation that would transiently block valid admits.
	admissionKey := fmt.Sprintf("%s/%s", ad.Namespace, ad.Name)
	if len(allErrs) == 0 {
		// Detect server-side dry-run to avoid creating reservations for
		// requests that will never be persisted.
		isDryRun := false
		if req, err := admission.RequestFromContext(ctx); err == nil && req.DryRun != nil && *req.DryRun {
			isDryRun = true
		}

		if isDryRun {
			// For dry-run requests, perform the quota check without writing
			// to the reservations map. This still acquires the mutex to read
			// consistent in-flight state, but does not create a reservation.
			ok, reason := v.Enforcer.CanAdmit(admissionKey, tq.Spec, tq.Status, ad.Spec, oldSpec)
			if !ok {
				allErrs = append(allErrs, field.Forbidden(specPath, fmt.Sprintf("quota exceeded: %s", reason)))
			}
		} else {
			// For persisted requests, use the atomic check-and-reserve.
			ok, reason := v.Enforcer.AdmitAndReserve(admissionKey, tq.Spec, tq.Status, ad.Spec, oldSpec, reservationTTL)
			if !ok {
				allErrs = append(allErrs, field.Forbidden(specPath, fmt.Sprintf("quota exceeded: %s", reason)))
			}
		}
	}

	return allErrs
}

// validateCanarySpec checks constraints that only apply when strategy == Canary.
func (v *AgentDeploymentCustomValidator) validateCanarySpec(ad *agentraxv1alpha1.AgentDeployment) field.ErrorList {
	var errs field.ErrorList
	rolloutPath := field.NewPath("spec", "rollout")

	// steps must be non-empty.
	if len(ad.Spec.Rollout.Steps) == 0 {
		errs = append(errs, field.Required(
			rolloutPath.Child("steps"),
			"at least one step is required when strategy is Canary",
		))
	} else {
		// Each step must set exactly one of setWeight or pause.
		for i, step := range ad.Spec.Rollout.Steps {
			stepPath := rolloutPath.Child("steps").Index(i)
			setBoth := step.SetWeight != nil && step.Pause != nil
			setNeither := step.SetWeight == nil && step.Pause == nil
			if setBoth || setNeither {
				errs = append(errs, field.Invalid(
					stepPath, step,
					"exactly one of setWeight or pause must be set per step",
				))
			}
		}

		// At least one setWeight step must reach 100 (full promotion).
		hasFullWeight := false
		for _, step := range ad.Spec.Rollout.Steps {
			if step.SetWeight != nil && *step.SetWeight == 100 {
				hasFullWeight = true
				break
			}
		}
		if !hasFullWeight {
			errs = append(errs, field.Invalid(
				rolloutPath.Child("steps"), ad.Spec.Rollout.Steps,
				"canary steps must include at least one setWeight: 100 for full promotion",
			))
		}
	}

	// All rollback fields are required when strategy is Canary.
	rb := ad.Spec.Rollout.Rollback
	rollbackPath := rolloutPath.Child("rollback")
	if rb.MaxErrorRate == "" {
		errs = append(errs, field.Required(rollbackPath.Child("maxErrorRate"),
			"required when strategy is Canary"))
	} else if _, err := agentraxv1alpha1.ParseErrorRate(rb.MaxErrorRate); err != nil {
		errs = append(errs, field.Invalid(rollbackPath.Child("maxErrorRate"), rb.MaxErrorRate, err.Error()))
	}
	if rb.MaxP99LatencyMs == 0 {
		errs = append(errs, field.Required(rollbackPath.Child("maxP99LatencyMs"),
			"required when strategy is Canary"))
	}
	if rb.MinRequestSample == 0 {
		errs = append(errs, field.Required(rollbackPath.Child("minRequestSample"),
			"required when strategy is Canary; must be > 0 to prevent false positives at low traffic"))
	}

	return errs
}

// validateMCPTools checks that tool names are unique and non-empty.
func validateMCPTools(fldPath *field.Path, tools []string) field.ErrorList {
	var errs field.ErrorList
	seen := make(map[string]bool, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t) == "" {
			errs = append(errs, field.Invalid(fldPath.Index(i), t, "tool name must not be empty"))
		}
		if seen[t] {
			errs = append(errs, field.Invalid(fldPath.Index(i), t, fmt.Sprintf("duplicate tool name %q", t)))
		}
		seen[t] = true
	}
	return errs
}

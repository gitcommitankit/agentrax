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

package rollout

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/metrics"
	"github.com/gitcommitankit/agentrax/internal/registry"
	"github.com/gitcommitankit/agentrax/internal/scaling"
)

const (
	// canaryVariantLabel is the pod label that distinguishes canary pods from stable pods.
	canaryVariantLabel = "agentrax.io/variant"
	// canaryVariantValue is the value of canaryVariantLabel for canary pods.
	canaryVariantValue = "canary"

	// maxPauseExtensionMultiplier is the maximum total pause duration as a multiple
	// of the configured pause duration. After this ceiling the evaluator runs regardless
	// of sample size.
	maxPauseExtensionMultiplier = 3

	// canaryDeploymentSuffix is appended to the AgentDeployment name to form
	// the canary Deployment name, e.g. "my-agent-canary".
	canaryDeploymentSuffix = "-canary"
)

// Controller manages the canary lifecycle for one AgentDeployment.
// It is a pure helper — it does not implement reconcile.Reconciler.
// The AgentDeploymentReconciler calls Step() each reconcile cycle when
// status.phase == RolloutInProgress, and Rollback() when abort is requested.
type Controller struct {
	// Client is the controller-runtime client for Kubernetes API access.
	Client client.Client
	// Scheme is required by controllerutil.SetControllerReference.
	Scheme *runtime.Scheme
	// PromClient is the Prometheus query client used for threshold evaluation.
	PromClient *metrics.Client
	// Registrar manages registration in the MCP discovery registry.
	// When non-nil, the canary controller triggers MCP re-registration after promotion.
	Registrar *registry.Registrar
	// GatewayName is the name of the Gateway API Gateway object.
	GatewayName string
	// GatewayNamespace is the namespace of the Gateway API Gateway object.
	GatewayNamespace string
	// FailSafeTimeout is how long Prometheus must be unreachable before
	// an automatic rollback fires.
	FailSafeTimeout time.Duration
}

// Step advances the canary state machine by one cycle.
// It is called every reconcile when status.phase == RolloutInProgress.
// The returned ctrl.Result tells the reconciler when to requeue.
func (c *Controller) Step(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues(
		"name", ad.Name, "namespace", ad.Namespace,
		"canaryStepIndex", ad.Status.CanaryStepIndex,
	)

	// Self-heal canary resources (Deployment, Service, and HTTPRoute) if they drift or are deleted out-of-band.
	if ad.Status.CanaryVersion != "" {
		if err := c.ensureCanaryDeployment(ctx, ad); err != nil {
			return ctrl.Result{}, fmt.Errorf("self-healing canary deployment: %w", err)
		}
		if err := c.ensureCanaryService(ctx, ad); err != nil {
			return ctrl.Result{}, fmt.Errorf("self-healing canary service: %w", err)
		}
	}
	if ad.Status.CanaryWeight > 0 {
		if err := c.ensureHTTPRoute(ctx, ad, 100-ad.Status.CanaryWeight, ad.Status.CanaryWeight); err != nil {
			return ctrl.Result{}, fmt.Errorf("self-healing httproute: %w", err)
		}
	}

	steps := ad.Spec.Rollout.Steps
	idx := ad.Status.CanaryStepIndex

	if idx >= len(steps) {
		// All steps completed — this should only happen if Step is called
		// after promotion was already triggered. Return without action.
		logger.Info("all canary steps already executed; promoting")
		return ctrl.Result{}, c.promote(ctx, ad)
	}

	step := steps[idx]

	switch {
	case step.SetWeight != nil:
		return c.executeSetWeight(ctx, ad, idx, *step.SetWeight)
	case step.Pause != nil:
		return c.executePause(ctx, ad, idx, step.Pause.Duration)
	default:
		// Malformed step — webhook should have rejected this; fail safe.
		return ctrl.Result{}, c.Rollback(ctx, ad, "InvalidStep",
			fmt.Sprintf("step %d has neither setWeight nor pause", idx))
	}
}

// Rollback triggers an immediate rollback of the canary, restoring the stable image.
// It may be called by Step (on threshold breach / fail-safe) or directly by the
// reconciler (on spec.rollout.abort).
func (c *Controller) Rollback(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment, reason, message string) error {
	logger := log.FromContext(ctx).WithValues("name", ad.Name, "namespace", ad.Namespace)
	logger.Info("rolling back canary", "reason", reason, "message", message)

	// 1. Restore stable Deployment image to status.stableVersion.
	if ad.Status.StableVersion != "" {
		dep := &appsv1.Deployment{}
		err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, dep)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("fetching stable deployment for rollback: %w", err)
		}
		if err == nil && len(dep.Spec.Template.Spec.Containers) > 0 {
			dep.Spec.Template.Spec.Containers[0].Image = ad.Status.StableVersion
			if err := c.Client.Update(ctx, dep); err != nil {
				return fmt.Errorf("restoring stable deployment image during rollback: %w", err)
			}
		}
	}

	// 2. Delete canary Deployment.
	if err := c.deleteCanaryDeployment(ctx, ad); err != nil {
		return fmt.Errorf("deleting canary deployment during rollback: %w", err)
	}

	// 3. Delete canary Service.
	if err := c.deleteCanaryService(ctx, ad); err != nil {
		return fmt.Errorf("deleting canary service during rollback: %w", err)
	}

	// 4. Reset HTTPRoute to 100% stable, then delete it.
	// Deleting the HTTPRoute is cleaner than leaving it at 100%; the stable
	// Service continues to receive all traffic directly from the parent Gateway.
	if err := c.deleteHTTPRoute(ctx, ad); err != nil {
		return fmt.Errorf("deleting httproute during rollback: %w", err)
	}

	// 5. Restore stable HPA.
	if err := c.restoreHPA(ctx, ad); err != nil {
		return fmt.Errorf("restoring HPA during rollback: %w", err)
	}

	// 6. Update status.
	latest := &agentraxv1alpha1.AgentDeployment{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, latest); err != nil {
		return fmt.Errorf("re-fetching AD for rollback status update: %w", err)
	}
	latest.Status.Phase = agentraxv1alpha1.PhaseRolloutFailed
	// Preserve CanaryVersion so the reconciler remembers which image failed
	// and does not immediately re-trigger a canary for the same image.
	if latest.Status.CanaryVersion == "" {
		latest.Status.CanaryVersion = ad.Spec.Image
	}
	latest.Status.CanaryWeight = 0
	latest.Status.CanaryStepIndex = 0
	latest.Status.PauseStartedAt = nil
	latest.Status.PromUnreachableSince = nil

	apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               "RolloutFailed",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: latest.Generation,
	})
	apimeta.RemoveStatusCondition(&latest.Status.Conditions, agentraxv1alpha1.ConditionSampleInsufficient)

	if err := c.Client.Status().Update(ctx, latest); err != nil {
		return fmt.Errorf("updating status after rollback: %w", err)
	}

	return nil
}

// ── SetWeight step ────────────────────────────────────────────────────────────

// executeSetWeight applies a traffic-weight step: creates the canary Deployment
// if needed, upserts the HTTPRoute with the target weight split, updates
// status.canaryWeight, and advances to the next step.
func (c *Controller) executeSetWeight(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment, idx int, weight int32) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("name", ad.Name, "step", idx, "weight", weight)
	logger.Info("executing setWeight step")

	// Ensure the canary Deployment is running with the new image.
	if err := c.ensureCanaryDeployment(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring canary deployment (step %d): %w", idx, err)
	}

	// Ensure the canary Service exists.
	if err := c.ensureCanaryService(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring canary service (step %d): %w", idx, err)
	}

	stableWeight := int32(100) - weight

	// Upsert the HTTPRoute with the target traffic split.
	if err := c.ensureHTTPRoute(ctx, ad, stableWeight, weight); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring httproute (step %d): %w", idx, err)
	}

	// Update status fields: advance step, record new weight.
	latest := &agentraxv1alpha1.AgentDeployment{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetching AD after setWeight: %w", err)
	}
	latest.Status.CanaryWeight = weight
	latest.Status.CanaryStepIndex = idx + 1
	latest.Status.PauseStartedAt = nil // clear any stale pause time
	if err := c.Client.Status().Update(ctx, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after setWeight: %w", err)
	}

	// If the new step index is a setWeight:100 that was the last step, promote
	// immediately on the next reconcile (requeue after 1 second).
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// ── Pause step ────────────────────────────────────────────────────────────────

// executePause waits for the pause duration to elapse while monitoring canary
// metrics. It may:
//   - extend the pause if the sample size is insufficient (up to maxPauseExtensionMultiplier × duration)
//   - trigger a rollback if thresholds are breached
//   - trigger a rollback if Prometheus is unreachable for > FailSafeTimeout
//   - advance to the next step when the pause duration elapses and thresholds are OK
func (c *Controller) executePause(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment, idx int, duration time.Duration) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("name", ad.Name, "step", idx, "duration", duration)

	now := metav1.Now()

	// Record start time on first entry.
	latest := &agentraxv1alpha1.AgentDeployment{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching AD in pause step: %w", err)
	}

	if latest.Status.PauseStartedAt == nil {
		logger.Info("starting pause window")
		latest.Status.PauseStartedAt = &now
		if err := c.Client.Status().Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording pause start time: %w", err)
		}
		// Requeue after the pause duration so we wake up to evaluate.
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	elapsed := now.Time.Sub(latest.Status.PauseStartedAt.Time)
	maxWait := time.Duration(maxPauseExtensionMultiplier) * duration
	if maxWait > 15*time.Minute {
		maxWait = 15 * time.Minute
	}

	// ── Evaluate thresholds ───────────────────────────────────────────────────
	result, evalErr := Evaluate(ctx, c.PromClient, latest, duration)
	if evalErr != nil {
		logger.Error(evalErr, "Prometheus unreachable during pause evaluation")
		// Mark when Prometheus first became unreachable.
		if latest.Status.PromUnreachableSince == nil {
			latest.Status.PromUnreachableSince = &now
			if updateErr := c.Client.Status().Update(ctx, latest); updateErr != nil {
				return ctrl.Result{}, fmt.Errorf("recording prom unreachable time: %w", updateErr)
			}
		}
		// Fire fail-safe rollback if Prometheus has been down too long.
		unreachableDuration := now.Time.Sub(latest.Status.PromUnreachableSince.Time)
		if unreachableDuration >= c.FailSafeTimeout {
			return ctrl.Result{}, c.Rollback(ctx, latest, "PrometheusUnavailable",
				fmt.Sprintf("Prometheus unreachable for %s (fail-safe timeout: %s)", unreachableDuration, c.FailSafeTimeout))
		}
		// Not yet at fail-safe timeout; requeue in 10s and keep waiting.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Prometheus is reachable — clear any stale unreachable timestamp.
	if latest.Status.PromUnreachableSince != nil {
		latest.Status.PromUnreachableSince = nil
		if err := c.Client.Status().Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("clearing prom unreachable time: %w", err)
		}
	}

	// ── Threshold breach → rollback ───────────────────────────────────────────
	if result.ThresholdBreached {
		logger.Info("threshold breached — rolling back", "reason", result.BreachReason)
		return ctrl.Result{}, c.Rollback(ctx, latest, "ThresholdBreached", result.BreachReason)
	}

	// ── Sample too small ──────────────────────────────────────────────────────
	if result.SampleTooSmall {
		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               agentraxv1alpha1.ConditionSampleInsufficient,
			Status:             metav1.ConditionTrue,
			Reason:             "InsufficientSample",
			Message:            fmt.Sprintf("request count %.0f below minimum %.0f; extending pause", result.SampleCount, float64(latest.Spec.Rollout.Rollback.MinRequestSample)),
			ObservedGeneration: latest.Generation,
		})
		if err := c.Client.Status().Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting SampleInsufficient condition: %w", err)
		}
		// If we are still within the max extension window, wait longer.
		if elapsed < maxWait {
			remaining := duration - (elapsed % duration)
			logger.Info("extending pause due to insufficient sample", "elapsed", elapsed, "maxWait", maxWait, "nextCheck", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
		// Max extensions exhausted — force evaluation (fall through to advancement below).
		logger.Info("max pause extensions exhausted; advancing despite small sample")
	}

	// ── Pause duration elapsed and thresholds OK ──────────────────────────────
	if elapsed < duration {
		// Requeue for the remainder of the pause.
		remaining := duration - elapsed
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// Advance to next step.
	logger.Info("pause complete; advancing to next step")
	apimeta.RemoveStatusCondition(&latest.Status.Conditions, agentraxv1alpha1.ConditionSampleInsufficient)
	latest.Status.CanaryStepIndex = idx + 1
	latest.Status.PauseStartedAt = nil
	if err := c.Client.Status().Update(ctx, latest); err != nil {
		return ctrl.Result{}, fmt.Errorf("advancing step index after pause: %w", err)
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// ── Promotion ─────────────────────────────────────────────────────────────────

// promote promotes the canary to stable: updates the stable Deployment image,
// deletes the canary Deployment and HTTPRoute, restores the HPA, and clears
// all canary status fields.
func (c *Controller) promote(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	logger := log.FromContext(ctx).WithValues("name", ad.Name, "namespace", ad.Namespace)
	logger.Info("promoting canary to stable", "newImage", ad.Status.CanaryVersion)

	// 0. Guard against empty CanaryVersion.
	if ad.Status.CanaryVersion == "" {
		return fmt.Errorf("cannot promote: canaryVersion is empty")
	}

	// 1. Update stable Deployment image.
	dep := &appsv1.Deployment{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, dep); err != nil {
		return fmt.Errorf("fetching stable deployment for promotion: %w", err)
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		dep.Spec.Template.Spec.Containers[0].Image = ad.Status.CanaryVersion
	}
	if err := c.Client.Update(ctx, dep); err != nil {
		return fmt.Errorf("updating stable deployment image during promotion: %w", err)
	}

	// 2. Delete canary Deployment.
	if err := c.deleteCanaryDeployment(ctx, ad); err != nil {
		return fmt.Errorf("deleting canary deployment during promotion: %w", err)
	}

	// 3. Delete canary Service.
	if err := c.deleteCanaryService(ctx, ad); err != nil {
		return fmt.Errorf("deleting canary service during promotion: %w", err)
	}

	// 4. Delete HTTPRoute (traffic fully on stable Service again).
	if err := c.deleteHTTPRoute(ctx, ad); err != nil {
		return fmt.Errorf("deleting httproute during promotion: %w", err)
	}

	// 5. Restore HPA.
	if err := c.restoreHPA(ctx, ad); err != nil {
		return fmt.Errorf("restoring HPA during promotion: %w", err)
	}

	// 6. Update status.
	latest := &agentraxv1alpha1.AgentDeployment{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, latest); err != nil {
		return fmt.Errorf("re-fetching AD for promotion status update: %w", err)
	}
	promoted := ad.Status.CanaryVersion
	latest.Status.StableVersion = promoted
	latest.Status.CanaryVersion = ""
	latest.Status.CanaryWeight = 0
	latest.Status.CanaryStepIndex = 0
	latest.Status.PauseStartedAt = nil
	latest.Status.PromUnreachableSince = nil
	latest.Status.Phase = agentraxv1alpha1.PhaseRunning

	apimeta.RemoveStatusCondition(&latest.Status.Conditions, "RolloutFailed")
	apimeta.RemoveStatusCondition(&latest.Status.Conditions, agentraxv1alpha1.ConditionSampleInsufficient)

	if err := c.Client.Status().Update(ctx, latest); err != nil {
		return fmt.Errorf("updating status after promotion: %w", err)
	}

	// Trigger MCP re-registration with updated endpoint / image after promotion.
	if c.Registrar != nil && latest.Spec.MCP.Expose {
		if err := c.Registrar.Register(ctx, latest); err != nil {
			logger.Error(err, "MCP re-registration after promotion failed; reconciler will retry on next cycle")
		}
	}

	logger.Info("canary promoted successfully", "stableVersion", promoted)
	return nil
}

// ── Canary Deployment ─────────────────────────────────────────────────────────

// ensureCanaryDeployment creates or updates the canary Deployment (<name>-canary)
// with the new image from spec. The canary Deployment is pinned at 1 replica
// and is not managed by HPA — the goal is to get representative traffic samples,
// not to scale the canary independently.
func (c *Controller) ensureCanaryDeployment(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	canaryName := ad.Name + canaryDeploymentSuffix
	desired := c.desiredCanaryDeployment(ad, canaryName)

	existing := &appsv1.Deployment{}
	existing.Name = canaryName
	existing.Namespace = ad.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c.Client, existing, func() error {
		existing.Labels = desired.Labels

		// Preserve the existing immutable selector on update; only set it on create
		if existing.ResourceVersion == "" {
			existing.Spec.Selector = desired.Spec.Selector
		}

		// Reconcile the complete pod template
		existing.Spec.Replicas = desired.Spec.Replicas
		existing.Spec.Template.Labels = desired.Spec.Template.Labels
		existing.Spec.Template.Spec = desired.Spec.Template.Spec

		if err := controllerutil.SetControllerReference(ad, existing, c.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling canary deployment: %w", err)
	}
	return nil
}

// desiredCanaryDeployment builds the spec for the canary Deployment.
func (c *Controller) desiredCanaryDeployment(ad *agentraxv1alpha1.AgentDeployment, name string) *appsv1.Deployment {
	one := int32(1)
	port := ad.Spec.Port
	if port == 0 {
		port = 8080
	}
	labels := map[string]string{
		"app.kubernetes.io/name":       ad.Name,
		"app.kubernetes.io/managed-by": "agentrax",
		"agentrax.io/tenant":           ad.Spec.TenantRef,
		canaryVariantLabel:             canaryVariantValue,
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ad.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			// Pinned at 1 replica — not HPA-managed.
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      "agent",
						Image:     ad.Status.CanaryVersion,
						Ports:     []corev1.ContainerPort{{ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						Resources: ad.Spec.Resources,
						Env:       ad.Spec.Env,
						Args:      ad.Spec.Args,
					}},
				},
			},
		},
	}
}

// deleteCanaryDeployment removes the canary Deployment. Not-found is tolerated.
func (c *Controller) deleteCanaryDeployment(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	dep := &appsv1.Deployment{}
	canaryName := ad.Name + canaryDeploymentSuffix
	err := c.Client.Get(ctx, types.NamespacedName{Name: canaryName, Namespace: ad.Namespace}, dep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting canary deployment for deletion: %w", err)
	}
	if err := c.Client.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting canary deployment: %w", err)
	}
	return nil
}

// ── Canary Service ────────────────────────────────────────────────────────────

// ensureCanaryService creates or updates the canary Service (<name>-canary)
// with a selector that targets only canary pods (variant=canary label).
func (c *Controller) ensureCanaryService(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	canaryName := ad.Name + canaryDeploymentSuffix
	desired := c.desiredCanaryService(ad, canaryName)

	existing := &corev1.Service{}
	existing.Name = canaryName
	existing.Namespace = ad.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Spec.Selector = desired.Spec.Selector
		existing.Spec.Ports = desired.Spec.Ports
		existing.Spec.Type = desired.Spec.Type

		if err := controllerutil.SetControllerReference(ad, existing, c.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling canary service: %w", err)
	}
	return nil
}

// desiredCanaryService builds the spec for the canary Service.
func (c *Controller) desiredCanaryService(ad *agentraxv1alpha1.AgentDeployment, name string) *corev1.Service {
	port := ad.Spec.Port
	if port == 0 {
		port = 8080
	}
	labels := map[string]string{
		"app.kubernetes.io/name":       ad.Name,
		"app.kubernetes.io/managed-by": "agentrax",
		"agentrax.io/tenant":           ad.Spec.TenantRef,
		canaryVariantLabel:             canaryVariantValue,
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ad.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "agent",
					Port:       port,
					TargetPort: intstr.FromInt32(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// deleteCanaryService removes the canary Service. Not-found is tolerated.
func (c *Controller) deleteCanaryService(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	svc := &corev1.Service{}
	canaryName := ad.Name + canaryDeploymentSuffix
	err := c.Client.Get(ctx, types.NamespacedName{Name: canaryName, Namespace: ad.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting canary service for deletion: %w", err)
	}
	if err := c.Client.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting canary service: %w", err)
	}
	return nil
}

// ── HTTPRoute ─────────────────────────────────────────────────────────────────

// ensureHTTPRoute creates or updates an HTTPRoute that splits traffic between
// the stable and canary Services according to the given weights (0–100).
// The HTTPRoute forwards all requests matching "/" to both backends, weighted
// by stableWeight and canaryWeight respectively.
func (c *Controller) ensureHTTPRoute(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment, stableWeight, canaryWeight int32) error {
	desired := c.desiredHTTPRoute(ad, stableWeight, canaryWeight)

	existing := &gatewayv1.HTTPRoute{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(ad, desired, c.Scheme); err != nil {
			return fmt.Errorf("setting owner ref on httproute: %w", err)
		}
		if err := c.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating httproute: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting httproute: %w", err)
	}

	// Set controller reference on existing HTTPRoute if not already set.
	if err := controllerutil.SetControllerReference(ad, existing, c.Scheme); err != nil {
		return fmt.Errorf("setting controller reference on existing httproute: %w", err)
	}

	// Skip update if spec already matches.
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}

	// Update weights if they have changed.
	existing.Spec = desired.Spec
	if err := c.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating httproute weights: %w", err)
	}
	return nil
}

// desiredHTTPRoute builds an HTTPRoute that routes traffic between the stable
// and canary Services by weight.
func (c *Controller) desiredHTTPRoute(ad *agentraxv1alpha1.AgentDeployment, stableWeight, canaryWeight int32) *gatewayv1.HTTPRoute {
	nsPtr := gatewayv1.Namespace(c.GatewayNamespace)
	canaryServiceName := gatewayv1.ObjectName(ad.Name + canaryDeploymentSuffix)
	stableServiceName := gatewayv1.ObjectName(ad.Name)
	portNumber := gatewayv1.PortNumber(ad.Spec.Port)
	if portNumber == 0 {
		portNumber = 8080
	}
	backendKind := gatewayv1.Kind("Service")
	backendGroup := gatewayv1.Group("")
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/"

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       ad.Name,
				"app.kubernetes.io/managed-by": "agentrax",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      gatewayv1.ObjectName(c.GatewayName),
					Namespace: &nsPtr,
				}},
			},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  &pathType,
						Value: &pathValue,
					},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Kind:  &backendKind,
								Group: &backendGroup,
								Name:  stableServiceName,
								Port:  &portNumber,
							},
							Weight: &stableWeight,
						},
					},
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Kind:  &backendKind,
								Group: &backendGroup,
								Name:  canaryServiceName,
								Port:  &portNumber,
							},
							Weight: &canaryWeight,
						},
					},
				},
			}},
		},
	}
}

// deleteHTTPRoute removes the HTTPRoute for the AgentDeployment. Not-found is tolerated.
func (c *Controller) deleteHTTPRoute(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	route := &gatewayv1.HTTPRoute{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, route)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting httproute for deletion: %w", err)
	}
	if err := c.Client.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting httproute: %w", err)
	}
	return nil
}

// ── HPA management ────────────────────────────────────────────────────────────

// PauseHPA deletes the AgentDeployment's HPA so the canary controller owns
// replica counts exclusively during the rollout. The reconciler's reconcileHPA
// already skips HPA reconciliation when phase == RolloutInProgress, so the HPA
// will not be recreated until promotion or rollback completes.
func (c *Controller) PauseHPA(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: ad.Name, Namespace: ad.Namespace}, hpa)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting HPA for pause: %w", err)
	}
	if err := c.Client.Delete(ctx, hpa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("pausing HPA: %w", err)
	}
	return nil
}

// restoreHPA re-creates the HPA after promotion or rollback using the stable AD spec.
// It passes headroom=0 as a conservative initial value; the main reconciler will
// recompute the correct quota headroom on its next cycle and update accordingly.
func (c *Controller) restoreHPA(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	// headroom=0 is conservative: the main reconciler will fix it on the next cycle.
	desired := scaling.BuildHPA(ad, 0)

	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, c.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Spec.ScaleTargetRef = desired.Spec.ScaleTargetRef
		existing.Spec.MinReplicas = desired.Spec.MinReplicas
		existing.Spec.MaxReplicas = desired.Spec.MaxReplicas
		existing.Spec.Metrics = desired.Spec.Metrics
		existing.Spec.Behavior = desired.Spec.Behavior

		if err := controllerutil.SetControllerReference(ad, existing, c.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("restoring HPA: %w", err)
	}
	return nil
}

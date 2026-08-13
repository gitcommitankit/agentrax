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

// Package controller contains Kubernetes reconciler implementations for Agentrax CRDs.
package controller

import (
	"context"
	"fmt"
	"sync"
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

	"github.com/go-logr/logr"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/scaling"
)

// AgentDeploymentReconciler reconciles an AgentDeployment object.
type AgentDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// GPUResourceName is the Kubernetes resource name used to count GPU units
	// (e.g. "nvidia.com/gpu"). Injected from the --gpu-resource-name operator flag.
	// Used when computing quota headroom for HPA max-replicas capping.
	GPUResourceName string

	// hasServiceMonitorCRD is set once during SetupWithManager and determines
	// whether ServiceMonitor reconciliation is attempted at all.
	hasServiceMonitorCRD bool

	// deregisterMu guards the Deregister field so test goroutines can safely
	// inject and clear the hook while the reconciler goroutine reads it.
	deregisterMu sync.Mutex

	// Deregister is an optional hook called during deletion cleanup before the
	// finalizer is removed. Phase 5 will set this to a real MCP deregistration
	// function. In tests it can be used to assert ordering invariants.
	// Always access through SetDeregister / the mutex-protected load in runDeletionCleanup.
	Deregister func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error
}

// SetDeregister safely replaces the Deregister hook under the mutex.
// Use this instead of direct field assignment to avoid data races.
func (r *AgentDeploymentReconciler) SetDeregister(fn func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error) {
	r.deregisterMu.Lock()
	defer r.deregisterMu.Unlock()
	r.Deregister = fn
}

// +kubebuilder:rbac:groups=agentrax.io,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentrax.io,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentrax.io,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentrax.io,resources=tenantquotas,verbs=get;list;watch

// Reconcile drives the AgentDeployment's observed state toward its declared spec.
// It creates and self-heals a Deployment, Service, and (when Prometheus Operator is present)
// a ServiceMonitor as owned child resources, then updates status conditions.
func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the AgentDeployment; return immediately if it has been deleted.
	ad := &agentraxv1alpha1.AgentDeployment{}
	if err := r.Get(ctx, req.NamespacedName, ad); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching AgentDeployment: %w", err)
	}

	// 2. Handle finalizer lifecycle.
	if ad.DeletionTimestamp.IsZero() {
		// Object is not being deleted — ensure our finalizer is present.
		if !controllerutil.ContainsFinalizer(ad, agentraxv1alpha1.AgentDeploymentFinalizer) {
			controllerutil.AddFinalizer(ad, agentraxv1alpha1.AgentDeploymentFinalizer)
			if err := r.Update(ctx, ad); err != nil {
				return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
			}
			// Re-fetch so we have the latest resourceVersion before continuing.
			if err := r.Get(ctx, req.NamespacedName, ad); err != nil {
				return ctrl.Result{}, fmt.Errorf("re-fetching after finalizer add: %w", err)
			}
		}
	} else {
		// Object is being deleted — run cleanup and remove finalizer.
		if controllerutil.ContainsFinalizer(ad, agentraxv1alpha1.AgentDeploymentFinalizer) {
			if err := r.runDeletionCleanup(ctx, ad); err != nil {
				return ctrl.Result{}, fmt.Errorf("running deletion cleanup: %w", err)
			}
			controllerutil.RemoveFinalizer(ad, agentraxv1alpha1.AgentDeploymentFinalizer)
			if err := r.Update(ctx, ad); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. Reconcile child Deployment.
	if err := r.reconcileDeployment(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling deployment: %w", err)
	}

	// 4. Reconcile child Service.
	if err := r.reconcileService(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	// 5. Reconcile ServiceMonitor when Prometheus Operator is present.
	if err := r.reconcileServiceMonitor(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling servicemonitor: %w", err)
	}

	// 6. Reconcile the managed HPA (skip during active canary — Phase 4 owns it).
	if result, err := r.reconcileHPA(ctx, ad); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling hpa: %w", err)
	} else if result.RequeueAfter > 0 {
		return result, nil
	}

	// 7. Derive status from the live Deployment and update it — always last.
	if result, err := r.updateStatus(ctx, ad, logger); err != nil || result.RequeueAfter > 0 {
		return result, err
	}

	return ctrl.Result{}, nil
}

// runDeletionCleanup performs pre-deletion tasks before the finalizer is removed.
// If a Deregister hook is set on the reconciler, it is called here so that
// deregistration happens while child resources (Service, Deployment) still exist.
// Phase 5 will set Deregister to the real MCP deregistration implementation.
func (r *AgentDeploymentReconciler) runDeletionCleanup(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	logger := log.FromContext(ctx)
	// Load the hook under the mutex so test goroutines can safely inject/clear it
	// without a data race against this reconciler goroutine.
	r.deregisterMu.Lock()
	deregister := r.Deregister
	r.deregisterMu.Unlock()

	if deregister != nil {
		if err := deregister(ctx, ad); err != nil {
			return fmt.Errorf("deregistering agent: %w", err)
		}
	}
	logger.Info("deletion cleanup complete", "name", ad.Name, "namespace", ad.Namespace)
	return nil
}

// reconcileDeployment creates or updates the Deployment owned by the AgentDeployment.
func (r *AgentDeploymentReconciler) reconcileDeployment(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	desired := r.desiredDeployment(ad)

	// CreateOrUpdate requires name+namespace on existing before calling; MutateFn must not set them.
	existing := &appsv1.Deployment{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels

		// Preserve the existing immutable selector on update; only set it on create
		// (when ResourceVersion is empty). Overwriting spec.selector on an existing
		// Deployment is rejected by the API server because it is immutable.
		if existing.ResourceVersion == "" {
			existing.Spec.Selector = desired.Spec.Selector
		}

		// Update only the fields the controller owns; do not wholesale replace
		// spec so API-defaulted values (e.g. strategy, progressDeadlineSeconds)
		// are preserved.
		existing.Spec.Replicas = desired.Spec.Replicas
		existing.Spec.Template.Labels = desired.Spec.Template.Labels
		if len(existing.Spec.Template.Spec.Containers) == 0 {
			existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
		} else {
			c := &existing.Spec.Template.Spec.Containers[0]
			c.Image = desired.Spec.Template.Spec.Containers[0].Image
			c.Ports = desired.Spec.Template.Spec.Containers[0].Ports
			c.Resources = desired.Spec.Template.Spec.Containers[0].Resources
			c.Env = desired.Spec.Template.Spec.Containers[0].Env
			c.Args = desired.Spec.Template.Spec.Containers[0].Args
		}

		if err := controllerutil.SetControllerReference(ad, existing, r.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reconciling deployment: %w", err)
	}
	return nil
}

// reconcileService creates or updates the Service owned by the AgentDeployment.
func (r *AgentDeploymentReconciler) reconcileService(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	desired := r.desiredService(ad)

	existing := &corev1.Service{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		// Only overwrite spec fields we own; clusterIP is assigned by the API server.
		existing.Spec.Selector = desired.Spec.Selector
		existing.Spec.Ports = desired.Spec.Ports
		if err := controllerutil.SetControllerReference(ad, existing, r.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	return err
}

// reconcileServiceMonitor creates or updates the ServiceMonitor owned by the AgentDeployment.
// It is a no-op when the ServiceMonitor CRD was not present at manager startup.
func (r *AgentDeploymentReconciler) reconcileServiceMonitor(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
	if !r.hasServiceMonitorCRD {
		return nil
	}

	desired := r.desiredServiceMonitor(ad)

	existing := &monitoringv1.ServiceMonitor{}
	existing.Name = desired.Name
	existing.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Spec = desired.Spec
		if err := controllerutil.SetControllerReference(ad, existing, r.Scheme); err != nil {
			return fmt.Errorf("setting controller reference: %w", err)
		}
		return nil
	})
	return err
}

// reconcileHPA creates or updates the HorizontalPodAutoscaler owned by this
// AgentDeployment. It is skipped when a canary rollout is in progress because
// Phase 4 owns the HPA lifecycle during rollout (deletes it, recreates it on
// promote/rollback). The HPA's maxReplicas is capped at the tenant quota
// headroom; when capping occurs, a QuotaLimited condition is set on status.
func (r *AgentDeploymentReconciler) reconcileHPA(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) (ctrl.Result, error) {
	// Phase 4 owns the HPA when a canary rollout is in progress. We must not
	// re-create or update the HPA here while Phase 4 has deliberately deleted it.
	if ad.Status.Phase == agentraxv1alpha1.PhaseRolloutInProgress {
		return ctrl.Result{}, nil
	}

	// Fetch the TenantQuota to compute quota headroom.
	tq := &agentraxv1alpha1.TenantQuota{}
	if err := r.Get(ctx, types.NamespacedName{Name: ad.Spec.TenantRef, Namespace: ad.Namespace}, tq); err != nil {
		if apierrors.IsNotFound(err) {
			// TenantQuota missing — the webhook prevents this on create, but it
			// can happen if the TenantQuota is deleted while agents exist.
			// Requeue and surface a condition rather than failing hard.
			SetCondition(ad, agentraxv1alpha1.ConditionQuotaLimited, metav1.ConditionTrue,
				"TenantQuotaNotFound",
				fmt.Sprintf("TenantQuota %q not found in namespace %s", ad.Spec.TenantRef, ad.Namespace))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching TenantQuota %q: %w", ad.Spec.TenantRef, err)
	}

	// Compute how many replicas the other agents in this tenant already consume
	// so QuotaHeadroom accounts for the full tenant budget.
	usedByOthers, err := r.replicasUsedByOtherAgents(ctx, ad)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("computing replica usage for quota headroom: %w", err)
	}

	headroom := scaling.QuotaHeadroom(tq.Spec, ad.Spec, usedByOthers)
	desiredHPA := scaling.BuildHPA(ad, headroom)

	// Set controller owner reference before CreateOrUpdate so garbage collection
	// removes the HPA when the AgentDeployment is deleted.
	if err := controllerutil.SetControllerReference(ad, desiredHPA, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting HPA owner reference: %w", err)
	}

	existing := &autoscalingv2.HorizontalPodAutoscaler{}
	existing.Name = desiredHPA.Name
	existing.Namespace = desiredHPA.Namespace

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desiredHPA.Labels
		existing.Spec.ScaleTargetRef = desiredHPA.Spec.ScaleTargetRef
		existing.Spec.MinReplicas = desiredHPA.Spec.MinReplicas
		existing.Spec.MaxReplicas = desiredHPA.Spec.MaxReplicas
		existing.Spec.Metrics = desiredHPA.Spec.Metrics
		existing.Spec.Behavior = desiredHPA.Spec.Behavior
		// Re-apply owner reference in case it was cleared out-of-band.
		if err := controllerutil.SetControllerReference(ad, existing, r.Scheme); err != nil {
			return fmt.Errorf("setting HPA owner reference: %w", err)
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating/updating HPA: %w", err)
	}

	// Surface or clear the QuotaLimited condition based on whether capping occurred.
	// Note: we mutate `ad` here but status is written by updateStatus at the end
	// of the reconcile loop. This is safe because updateStatus re-fetches and merges.
	if scaling.IsQuotaCapped(ad, headroom) {
		SetCondition(ad, agentraxv1alpha1.ConditionQuotaLimited, metav1.ConditionTrue,
			"HPAMaxReplicasCapped",
			fmt.Sprintf("spec.replicas.max (%d) exceeds quota headroom (%d); HPA capped",
				ad.Spec.Replicas.Max, headroom))
	} else {
		RemoveCondition(ad, agentraxv1alpha1.ConditionQuotaLimited)
	}

	return ctrl.Result{}, nil
}

// replicasUsedByOtherAgents returns the sum of spec.replicas.max across all
// AgentDeployments in the same namespace that reference the same TenantQuota,
// excluding the AgentDeployment being reconciled. This is used to compute the
// remaining total-replica budget for the HPA quota-headroom calculation.
func (r *AgentDeploymentReconciler) replicasUsedByOtherAgents(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) (int32, error) {
	list := &agentraxv1alpha1.AgentDeploymentList{}
	if err := r.List(ctx, list, client.InNamespace(ad.Namespace)); err != nil {
		return 0, fmt.Errorf("listing AgentDeployments for quota headroom: %w", err)
	}

	var total int32
	for i := range list.Items {
		other := &list.Items[i]
		if other.Spec.TenantRef != ad.Spec.TenantRef {
			continue
		}
		if other.Name == ad.Name && other.Namespace == ad.Namespace {
			continue // exclude self
		}
		total += other.Spec.Replicas.Max
	}
	return total, nil
}

// updateStatus derives the AgentDeployment status from the live Deployment and writes it.
// This is always the last step in the reconcile loop.
func (r *AgentDeploymentReconciler) updateStatus(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment, logger logr.Logger) (ctrl.Result, error) {
	// Re-fetch the live Deployment to get accurate replica counts.
	dep := &appsv1.Deployment{}
	depKey := client.ObjectKey{Name: ad.Name, Namespace: ad.Namespace}
	if err := r.Get(ctx, depKey, dep); err != nil {
		if apierrors.IsNotFound(err) {
			// Deployment not ready yet — requeue.
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching deployment for status: %w", err)
	}

	// Re-fetch the AgentDeployment with the latest resourceVersion before patching status.
	// If the object was deleted between the start of reconcile and now, treat it
	// as a no-op rather than an error — the deletion path has already handled cleanup.
	latest := &agentraxv1alpha1.AgentDeployment{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ad), latest); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("re-fetching agentdeployment for status update: %w", err)
	}

	// Deep-copy the current status before mutating so we can compare the full
	// before/after with equality.Semantic.DeepEqual. This catches intra-condition
	// changes (e.g. Reason, Message, LastTransitionTime updates at the same
	// condition count) that a scalar/len check would silently miss.
	prevStatus := latest.Status.DeepCopy()

	latest.Status.CurrentReplicas = dep.Status.ReadyReplicas

	// Detect ImagePullBackOff by inspecting pod list; requeue if listing fails.
	imagePullFailed, failMsg, err := r.detectImagePullFailure(ctx, ad)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("detecting image pull failure: %w", err)
	}
	if imagePullFailed {
		SetCondition(latest, agentraxv1alpha1.ConditionImagePullFailed, metav1.ConditionTrue, "ImagePullBackOff", failMsg)
		latest.Status.Phase = agentraxv1alpha1.PhaseDegraded
	} else {
		RemoveCondition(latest, agentraxv1alpha1.ConditionImagePullFailed)

		// A rollout is complete when the Deployment controller has observed the
		// latest generation and every replica is both updated and available.
		// Checking only ReadyReplicas > 0 would promote StableVersion prematurely
		// while old-image pods are still serving traffic during a rolling update.
		replicas := int32(1)
		if dep.Spec.Replicas != nil {
			replicas = *dep.Spec.Replicas
		}
		rolloutComplete := dep.Status.ObservedGeneration >= dep.Generation &&
			dep.Status.UpdatedReplicas == replicas &&
			dep.Status.AvailableReplicas == replicas

		if rolloutComplete {
			latest.Status.Phase = agentraxv1alpha1.PhaseRunning
			// Derive StableVersion from the image the Deployment controller
			// applied — not from latest.Spec.Image — so it reflects what is
			// actually running, even if the spec was updated again since.
			if len(dep.Spec.Template.Spec.Containers) > 0 {
				latest.Status.StableVersion = dep.Spec.Template.Spec.Containers[0].Image
			}
			SetCondition(latest, agentraxv1alpha1.ConditionReady, metav1.ConditionTrue, "DeploymentReady", "Deployment is ready")
			SetCondition(latest, agentraxv1alpha1.ConditionReconciled, metav1.ConditionTrue, "ReconcileSuccess", "Latest generation reconciled")
		} else {
			latest.Status.Phase = agentraxv1alpha1.PhasePending
			SetCondition(latest, agentraxv1alpha1.ConditionReady, metav1.ConditionFalse, "DeploymentNotReady", "Waiting for pods to become ready")
			SetCondition(latest, agentraxv1alpha1.ConditionReconciled, metav1.ConditionTrue, "ReconcileSuccess", "Latest generation reconciled")
		}
	}

	// Only write status when something actually changed to avoid spurious API
	// calls and watch events on every reconcile.
	if !equality.Semantic.DeepEqual(prevStatus, &latest.Status) {
		if err := r.Status().Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
	}

	// If still pending, requeue to check readiness again.
	if latest.Status.Phase == agentraxv1alpha1.PhasePending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	logger.Info("reconciled AgentDeployment", "phase", latest.Status.Phase, "readyReplicas", latest.Status.CurrentReplicas)
	return ctrl.Result{}, nil
}

// detectImagePullFailure returns true and a description message when any pod owned
// by this AgentDeployment is in ImagePullBackOff or ErrImagePull state.
// It returns an error if the pod list call fails so the caller can requeue.
func (r *AgentDeploymentReconciler) detectImagePullFailure(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) (bool, string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(ad.Namespace),
		client.MatchingLabels(agentLabels(ad)),
	); err != nil {
		return false, "", fmt.Errorf("listing pods for image pull check: %w", err)
	}

	imagePullReasons := map[string]bool{"ImagePullBackOff": true, "ErrImagePull": true}

	for i := range podList.Items {
		pod := &podList.Items[i]
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if cs.State.Waiting != nil && imagePullReasons[cs.State.Waiting.Reason] {
				return true, fmt.Sprintf("pod %s: %s", pod.Name, cs.State.Waiting.Message), nil
			}
		}
	}
	return false, "", nil
}

// ── Desired-state builders ────────────────────────────────────────────────────

// agentLabels returns the canonical label set applied to all resources owned by ad.
func agentLabels(ad *agentraxv1alpha1.AgentDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       ad.Name,
		"app.kubernetes.io/managed-by": "agentrax",
		"agentrax.io/tenant":           ad.Spec.TenantRef,
	}
}

// desiredDeployment builds the Deployment spec the reconciler wants to exist.
func (r *AgentDeploymentReconciler) desiredDeployment(ad *agentraxv1alpha1.AgentDeployment) *appsv1.Deployment {
	port := ad.Spec.Port
	if port == 0 {
		port = 8080
	}

	labels := agentLabels(ad)
	replicas := ad.Spec.Replicas.Min

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							// Use a fixed container name that is always DNS-1035 compliant.
							// The AgentDeployment name is used at the object level, not container level.
							Name:  "agent",
							Image: ad.Spec.Image,
							Ports: []corev1.ContainerPort{
								{
									Name:          "agent",
									ContainerPort: port,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: ad.Spec.Resources,
							Env:       ad.Spec.Env,
							Args:      ad.Spec.Args,
						},
					},
				},
			},
		},
	}
}

// desiredService builds the Service spec the reconciler wants to exist.
func (r *AgentDeploymentReconciler) desiredService(ad *agentraxv1alpha1.AgentDeployment) *corev1.Service {
	port := ad.Spec.Port
	if port == 0 {
		port = 8080
	}

	labels := agentLabels(ad)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
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

// desiredServiceMonitor builds the ServiceMonitor spec that scrapes /metrics on the agent pods.
func (r *AgentDeploymentReconciler) desiredServiceMonitor(ad *agentraxv1alpha1.AgentDeployment) *monitoringv1.ServiceMonitor {
	labels := agentLabels(ad)

	return &monitoringv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Labels:    labels,
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: labels,
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port: "agent",
					Path: "/metrics",
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
// It uses an uncached API reader to check once whether the ServiceMonitor CRD is
// installed, stores the result on the reconciler, and conditionally adds an
// Owns watch for ServiceMonitor so that out-of-band deletions trigger a reconcile.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Check CRD presence once at startup using the uncached reader so we don't
	// require apiextensionsv1 to be registered in the caching informer scheme.
	var err error
	r.hasServiceMonitorCRD, err = serviceMonitorCRDExists(context.Background(), mgr.GetAPIReader())
	if err != nil {
		return fmt.Errorf("checking servicemonitor CRD at setup: %w", err)
	}

	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&agentraxv1alpha1.AgentDeployment{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{})

	if r.hasServiceMonitorCRD {
		bldr = bldr.Owns(&monitoringv1.ServiceMonitor{})
	}

	return bldr.Complete(r)
}

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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/quota"
)

// TenantQuotaReconciler reconciles a TenantQuota object.
type TenantQuotaReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Enforcer *quota.Enforcer
}

// +kubebuilder:rbac:groups=agentrax.io,resources=tenantquotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentrax.io,resources=tenantquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentrax.io,resources=agentdeployments,verbs=get;list;watch

// Reconcile computes actual AgentDeployment usage within the TenantQuota's
// namespace and writes accurate usage counters to TenantQuota.status.
// If usage exceeds any quota ceiling (e.g. because maxAgents was lowered),
// it sets the OverQuota condition without forcibly deleting any resources.
func (r *TenantQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the TenantQuota; return immediately if it has been deleted.
	tq := &agentraxv1alpha1.TenantQuota{}
	if err := r.Get(ctx, req.NamespacedName, tq); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching TenantQuota: %w", err)
	}

	// 2. List all AgentDeployment objects in the same namespace that reference
	//    this TenantQuota via spec.tenantRef.
	adList := &agentraxv1alpha1.AgentDeploymentList{}
	if err := r.List(ctx, adList, client.InNamespace(tq.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing AgentDeployments: %w", err)
	}

	// Filter to only ADs referencing this TenantQuota.
	var specs []agentraxv1alpha1.AgentDeploymentSpec
	for i := range adList.Items {
		if adList.Items[i].Spec.TenantRef == tq.Name {
			specs = append(specs, adList.Items[i].Spec)
			// Release the in-flight reservation for this AD now that it is
			// committed to etcd. This prevents the 5s TTL window from
			// blocking rapid sequential creates of the same or sibling ADs.
			r.Enforcer.Release(fmt.Sprintf("%s/%s", adList.Items[i].Namespace, adList.Items[i].Name))
		}
	}

	// 3. Compute accurate usage from the live AD list.
	usage := r.Enforcer.ComputeUsage(specs)

	// 4. Re-fetch with the latest resourceVersion before writing status to avoid
	//    optimistic concurrency conflicts.
	latest := &agentraxv1alpha1.TenantQuota{}
	if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("re-fetching TenantQuota for status update: %w", err)
	}

	prevStatus := latest.Status.DeepCopy()

	latest.Status.UsedAgents = usage.UsedAgents
	latest.Status.UsedGPUs = usage.UsedGPUs
	latest.Status.UsedTotalReplicas = usage.UsedTotalReplicas

	// 5. Set or clear the OverQuota condition based on whether usage exceeds spec.
	// Use latest.Spec (re-fetched) rather than tq.Spec (first fetch) to avoid
	// evaluating against a ceiling that may have changed between the two Gets.
	over, overMsg := r.Enforcer.IsOverQuota(latest.Spec, usage)
	if over {
		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               agentraxv1alpha1.ConditionOverQuota,
			Status:             metav1.ConditionTrue,
			Reason:             "UsageExceedsQuota",
			Message:            overMsg,
			ObservedGeneration: latest.Generation,
		})
	} else {
		apimeta.RemoveStatusCondition(&latest.Status.Conditions, agentraxv1alpha1.ConditionOverQuota)
	}

	// 6. Only write status when something actually changed.
	if !equality.Semantic.DeepEqual(prevStatus, &latest.Status) {
		if err := r.Status().Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating TenantQuota status: %w", err)
		}
		logger.Info("updated TenantQuota status",
			"name", latest.Name,
			"namespace", latest.Namespace,
			"usedAgents", latest.Status.UsedAgents,
			"usedGPUs", latest.Status.UsedGPUs,
			"usedTotalReplicas", latest.Status.UsedTotalReplicas,
			"overQuota", over,
		)
	}

	// Requeue periodically as a safety net: if an AgentDeployment is deleted
	// without firing a watch event (e.g. namespace force-deletion), the usage
	// counters will self-heal on the next reconcile rather than staying stale.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// SetupWithManager sets up the TenantQuota controller with the Manager.
// It watches both TenantQuota objects and AgentDeployment objects (to trigger
// reconciliation when ADs are created, updated, or deleted in the namespace).
func (r *TenantQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentraxv1alpha1.TenantQuota{}).
		// Enqueue the owning TenantQuota whenever an AgentDeployment changes.
		Watches(
			&agentraxv1alpha1.AgentDeployment{},
			enqueueTenantQuota(),
		).
		Complete(r)
}

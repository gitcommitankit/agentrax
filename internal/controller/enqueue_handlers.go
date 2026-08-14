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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// enqueueTenantQuota returns an EventHandler that maps every AgentDeployment
// event to a reconcile request for the TenantQuota named by spec.tenantRef in
// the same namespace. This causes the TenantQuota reconciler to recompute usage
// whenever any AD in the namespace is created, updated, or deleted.
func enqueueTenantQuota() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		ad, ok := obj.(*agentraxv1alpha1.AgentDeployment)
		if !ok || ad.Spec.TenantRef == "" {
			return nil
		}
		return []reconcile.Request{
			{
				NamespacedName: types.NamespacedName{
					Namespace: ad.Namespace,
					Name:      ad.Spec.TenantRef,
				},
			},
		}
	})
}

// enqueueAgentDeploymentsForTenantQuota returns an EventHandler that maps every
// TenantQuota event to reconcile requests for all AgentDeployments in the same
// namespace that reference that TenantQuota. This ensures that lowering or
// raising quota ceilings updates HPA maxReplicas and QuotaLimited conditions
// across all affected agents without requiring out-of-band edits to each AD.
func enqueueAgentDeploymentsForTenantQuota(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		tq, ok := obj.(*agentraxv1alpha1.TenantQuota)
		if !ok {
			return nil
		}
		list := &agentraxv1alpha1.AgentDeploymentList{}
		if err := c.List(ctx, list, client.InNamespace(tq.Namespace)); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for _, ad := range list.Items {
			if ad.Spec.TenantRef == tq.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: ad.Namespace,
						Name:      ad.Name,
					},
				})
			}
		}
		return reqs
	})
}

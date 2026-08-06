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

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// serviceMonitorCRDName is the fully-qualified CRD name for Prometheus Operator's ServiceMonitor.
const serviceMonitorCRDName = "servicemonitors.monitoring.coreos.com"

// serviceMonitorCRDExists reports whether the ServiceMonitor CRD is installed in the cluster.
// It accepts a client.Reader so callers can pass either a caching client or an
// uncached API reader (mgr.GetAPIReader()). When Prometheus Operator is absent,
// the reconciler skips ServiceMonitor creation rather than erroring out.
func serviceMonitorCRDExists(ctx context.Context, r client.Reader) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := r.Get(ctx, client.ObjectKey{Name: serviceMonitorCRDName}, crd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

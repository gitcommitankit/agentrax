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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// timeout is the maximum time any Eventually assertion waits in this package.
	timeout = 30 * time.Second
	// interval is how often Eventually polls.
	interval = 250 * time.Millisecond
)

// namespacedName is a convenience wrapper for building types.NamespacedName.
func namespacedName(name, namespace string) types.NamespacedName {
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

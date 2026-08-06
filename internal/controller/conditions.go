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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// SetCondition sets a status condition on the AgentDeployment using the standard
// meta.SetStatusCondition helper. The ObservedGeneration is always stamped from
// the object's current generation so consumers can detect stale conditions.
func SetCondition(ad *agentraxv1alpha1.AgentDeployment, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&ad.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: ad.Generation,
	})
}

// GetCondition returns a pointer to the named condition, or nil if absent.
func GetCondition(ad *agentraxv1alpha1.AgentDeployment, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(ad.Status.Conditions, condType)
}

// RemoveCondition removes the named condition from the AgentDeployment status slice.
// It is a no-op if the condition does not exist.
func RemoveCondition(ad *agentraxv1alpha1.AgentDeployment, condType string) {
	apimeta.RemoveStatusCondition(&ad.Status.Conditions, condType)
}

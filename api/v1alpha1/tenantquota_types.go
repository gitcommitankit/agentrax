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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TenantQuotaSpec defines resource ceilings for a tenant.
type TenantQuotaSpec struct {
	// MaxAgents is the maximum number of AgentDeployment objects allowed in this tenant.
	// +kubebuilder:validation:Minimum=1
	MaxAgents int32 `json:"maxAgents"`

	// MaxGPUs is the total number of GPU units that can be allocated across all agents.
	// GPU count is derived from spec.resources.limits["nvidia.com/gpu"] × spec.replicas.max.
	// +kubebuilder:validation:Minimum=0
	MaxGPUs int32 `json:"maxGPUs"`

	// MaxTotalReplicas is the ceiling on the sum of spec.replicas.max across all agents in this tenant.
	// +kubebuilder:validation:Minimum=1
	MaxTotalReplicas int32 `json:"maxTotalReplicas"`

	// MaxReplicasPerAgent is the maximum spec.replicas.max value any single agent may request.
	// +kubebuilder:validation:Minimum=1
	MaxReplicasPerAgent int32 `json:"maxReplicasPerAgent"`
}

// TenantQuotaStatus reports current usage against the spec ceilings.
type TenantQuotaStatus struct {
	// UsedAgents is the number of AgentDeployment objects currently in this tenant.
	UsedAgents int32 `json:"usedAgents,omitempty"`

	// UsedGPUs is the total GPU units currently allocated across all agents in this tenant.
	UsedGPUs int32 `json:"usedGPUs,omitempty"`

	// UsedTotalReplicas is the sum of spec.replicas.max across all agents currently in this tenant.
	UsedTotalReplicas int32 `json:"usedTotalReplicas,omitempty"`

	// Conditions holds the conditions for the TenantQuota (e.g. OverQuota).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConditionOverQuota indicates that tenant resource usage exceeds configured ceilings.
const ConditionOverQuota = "OverQuota"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MaxAgents",type=integer,JSONPath=`.spec.maxAgents`
// +kubebuilder:printcolumn:name="UsedAgents",type=integer,JSONPath=`.status.usedAgents`
// +kubebuilder:printcolumn:name="MaxGPUs",type=integer,JSONPath=`.spec.maxGPUs`
// +kubebuilder:printcolumn:name="UsedGPUs",type=integer,JSONPath=`.status.usedGPUs`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TenantQuota is the Schema for the tenantquotas API.
// It declares and enforces per-tenant resource ceilings for AgentDeployment objects.
type TenantQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantQuotaSpec   `json:"spec,omitempty"`
	Status TenantQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantQuotaList contains a list of TenantQuota.
type TenantQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantQuota `json:"items"`
}

// init registers TenantQuota and TenantQuotaList types with the SchemeBuilder.
func init() {
	SchemeBuilder.Register(&TenantQuota{}, &TenantQuotaList{})
}

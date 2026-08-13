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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentDeploymentSpec defines the desired state of an AgentDeployment.
type AgentDeploymentSpec struct {
	// Image is the container image serving the model or agent.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Port is the container port the model/agent listens on.
	// +optional
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// TenantRef names the owning TenantQuota object in the same namespace.
	// +kubebuilder:validation:MinLength=1
	TenantRef string `json:"tenantRef"`

	// Replicas defines the autoscaling policy.
	Replicas ScalingPolicy `json:"replicas"`

	// Rollout defines how new versions are shipped. Optional; defaults to Recreate.
	// +optional
	Rollout RolloutPolicy `json:"rollout,omitempty"`

	// MCP controls whether this agent registers itself for MCP-based discovery.
	// +optional
	MCP MCPConfig `json:"mcp,omitempty"`

	// Resources are passed through to the underlying pod template.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env allows passing environment variables to the agent container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Args overrides the container entrypoint arguments.
	// +optional
	Args []string `json:"args,omitempty"`
}

// ScalingPolicy defines autoscaling parameters for an AgentDeployment.
type ScalingPolicy struct {
	// Min is the minimum number of replicas. Must be at least 1.
	// +kubebuilder:validation:Minimum=1
	Min int32 `json:"min"`

	// Max is the maximum number of replicas. Must be at least 1.
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// Metric selects the custom metric used for autoscaling decisions.
	// +kubebuilder:validation:Enum=queueDepth;gpuUtilization
	Metric string `json:"metric"`

	// Target is the desired value of the chosen metric per replica.
	// +kubebuilder:validation:Minimum=1
	Target int32 `json:"target"`
}

// RolloutPolicy describes how a new version of the agent is progressively shipped.
type RolloutPolicy struct {
	// Strategy determines the rollout approach. Defaults to Recreate.
	// +optional
	// +kubebuilder:validation:Enum=Recreate;Canary
	// +kubebuilder:default=Recreate
	Strategy string `json:"strategy,omitempty"`

	// Steps lists the ordered canary traffic-shift and pause steps.
	// Required when strategy is Canary.
	// +optional
	Steps []CanaryStep `json:"steps,omitempty"`

	// Rollback defines the automatic rollback thresholds.
	// Required when strategy is Canary.
	// +optional
	Rollback RollbackPolicy `json:"rollback,omitempty"`

	// Abort, when set to true, triggers an immediate rollback of any in-progress canary.
	// +optional
	Abort bool `json:"abort,omitempty"`
}

// CanaryStep represents a single step in a canary rollout.
// Exactly one of SetWeight or Pause must be set per step.
type CanaryStep struct {
	// SetWeight is the percentage of traffic to send to the canary (0–100).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SetWeight *int32 `json:"setWeight,omitempty"`

	// Pause is the duration to wait before evaluating rollback thresholds.
	// Mutually exclusive with SetWeight.
	// +optional
	Pause *metav1.Duration `json:"pause,omitempty"`
}

// RollbackPolicy defines the thresholds that trigger automatic rollback.
type RollbackPolicy struct {
	// MaxErrorRate is the maximum acceptable error rate, expressed as a percentage string (e.g. "2%").
	MaxErrorRate string `json:"maxErrorRate,omitempty"`

	// MaxP99LatencyMs is the maximum acceptable p99 latency in milliseconds.
	// +kubebuilder:validation:Minimum=1
	MaxP99LatencyMs int32 `json:"maxP99LatencyMs,omitempty"`

	// MinRequestSample is the minimum number of requests that must be observed before
	// threshold evaluation occurs. Guards against false positives at low traffic weight.
	// +kubebuilder:validation:Minimum=1
	MinRequestSample int32 `json:"minRequestSample,omitempty"`
}

// MCPConfig controls whether the agent is registered with the MCP discovery registry.
type MCPConfig struct {
	// Expose, when true, registers this agent in the MCP registry upon reaching a stable state.
	Expose bool `json:"expose,omitempty"`

	// Tools is the list of tool names advertised by this agent in the MCP registry.
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// AgentDeploymentStatus defines the observed state of an AgentDeployment.
type AgentDeploymentStatus struct {
	// Phase is the high-level lifecycle phase of this deployment.
	// +kubebuilder:validation:Enum=Pending;Running;RolloutInProgress;RolloutFailed;Degraded
	Phase string `json:"phase,omitempty"`

	// CurrentReplicas is the number of replicas currently running.
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// StableVersion is the container image tag of the currently stable deployment.
	StableVersion string `json:"stableVersion,omitempty"`

	// CanaryVersion is the container image tag of the canary deployment, if one is in progress.
	CanaryVersion string `json:"canaryVersion,omitempty"`

	// CanaryWeight is the current percentage of traffic routed to the canary (0–100).
	CanaryWeight int32 `json:"canaryWeight,omitempty"`

	// Registered is true when this agent is currently registered in the MCP registry.
	Registered bool `json:"registered,omitempty"`

	// Conditions holds the conditions for the AgentDeployment.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PhasePending indicates the deployment is pending initialization.
const PhasePending = "Pending"

// PhaseRunning indicates the deployment is healthy and running.
const PhaseRunning = "Running"

// PhaseRolloutInProgress indicates a canary rollout is currently shifting traffic.
const PhaseRolloutInProgress = "RolloutInProgress"

// PhaseRolloutFailed indicates a canary rollout failed and was rolled back.
const PhaseRolloutFailed = "RolloutFailed"

// PhaseDegraded indicates one or more underlying resources are unhealthy.
const PhaseDegraded = "Degraded"

// ConditionReady indicates the AgentDeployment resources are fully reconciled and ready.
const ConditionReady = "Ready"

// ConditionReconciled indicates the latest generation has been reconciled.
const ConditionReconciled = "Reconciled"

// ConditionImagePullFailed indicates container image pull failure.
const ConditionImagePullFailed = "ImagePullFailed"

// ConditionQuotaLimited indicates the deployment is blocked due to tenant quota.
const ConditionQuotaLimited = "QuotaLimited"

// ConditionMCPHandshakeFailed indicates MCP initialization handshake failed.
const ConditionMCPHandshakeFailed = "MCPHandshakeFailed"

// ConditionSampleInsufficient indicates rollout metrics sample size is below minimum.
const ConditionSampleInsufficient = "SampleInsufficient"

// AgentDeploymentFinalizer is the finalizer name used to ensure MCP deregistration before object deletion.
const AgentDeploymentFinalizer = "agentrax.io/mcp-deregister"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Stable",type=string,JSONPath=`.status.stableVersion`
// +kubebuilder:printcolumn:name="Registered",type=boolean,JSONPath=`.status.registered`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentDeployment is the Schema for the agentdeployments API.
// It manages the full lifecycle of a model or autonomous agent workload on Kubernetes,
// including autoscaling, canary rollout, and MCP-based service discovery.
type AgentDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentDeploymentSpec   `json:"spec,omitempty"`
	Status AgentDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentDeploymentList contains a list of AgentDeployment.
type AgentDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDeployment `json:"items"`
}

// init registers AgentDeployment and AgentDeploymentList types with the SchemeBuilder.
func init() {
	SchemeBuilder.Register(&AgentDeployment{}, &AgentDeploymentList{})
}

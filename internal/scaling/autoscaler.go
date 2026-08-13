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

// Package scaling translates an AgentDeployment's replica policy into a managed
// HorizontalPodAutoscaler wired to the custom metrics exposed by Prometheus Adapter.
package scaling

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

const (
	// MetricQueueDepth is the value of spec.replicas.metric that selects the
	// request-queue depth custom metric.
	MetricQueueDepth = "queueDepth"

	// MetricGPUUtilization is the value of spec.replicas.metric that selects
	// the GPU utilization custom metric.
	MetricGPUUtilization = "gpuUtilization"

	// customMetricQueueDepth is the name registered with the Prometheus Adapter
	// for queue depth (External metric, namespace-scoped via label selector).
	customMetricQueueDepth = "agentrax_queue_depth"

	// customMetricGPUUtilization is the name registered with the Prometheus Adapter
	// for GPU utilization.
	customMetricGPUUtilization = "agentrax_gpu_utilization"

	// scaleUpStabilizationSec is the HPA stabilization window for scale-up events
	// (seconds). A 60-second window prevents thrashing on transient spikes.
	scaleUpStabilizationSec int32 = 60

	// scaleDownStabilizationSec is the HPA stabilization window for scale-down events
	// (seconds). A 5-minute window prevents premature scale-down while load subsides.
	scaleDownStabilizationSec int32 = 300
)

// BuildHPA constructs the desired HorizontalPodAutoscaler for the given AgentDeployment.
// quotaHeadroom is the maximum number of replicas the tenant quota currently allows;
// the HPA maxReplicas is capped at min(spec.replicas.max, quotaHeadroom).
//
// BuildHPA is a pure function — it does not make any Kubernetes API calls.
// The caller (reconciler) is responsible for CreateOrUpdate and owner reference.
func BuildHPA(ad *agentraxv1alpha1.AgentDeployment, quotaHeadroom int32) *autoscalingv2.HorizontalPodAutoscaler {
	maxReplicas := ad.Spec.Replicas.Max
	if quotaHeadroom < maxReplicas {
		maxReplicas = quotaHeadroom
	}
	// Never let maxReplicas drop below minReplicas — a degenerate quota that
	// leaves zero headroom is surfaced as QuotaLimited in the reconciler, but
	// we still need a valid HPA spec.
	minReplicas := ad.Spec.Replicas.Min
	if maxReplicas < minReplicas {
		maxReplicas = minReplicas
	}

	metricName := customMetricNameFor(ad.Spec.Replicas.Metric)

	// Use an AverageValue target so the HPA scales to keep the per-replica
	// metric value near the declared target (e.g., queue depth of 50 per pod).
	targetValue := resource.NewMilliQuantity(int64(ad.Spec.Replicas.Target)*1000, resource.DecimalSI)

	scaleUpWindow := scaleUpStabilizationSec
	scaleDownWindow := scaleDownStabilizationSec

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ad.Name,
			Namespace: ad.Namespace,
			Labels:    hpaLabels(ad),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       ad.Name,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ExternalMetricSourceType,
					External: &autoscalingv2.ExternalMetricSource{
						Metric: autoscalingv2.MetricIdentifier{
							Name: metricName,
							// Scope the metric to this specific AgentDeployment
							// so Prometheus Adapter can filter by pod labels.
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app.kubernetes.io/name":       ad.Name,
									"app.kubernetes.io/managed-by": "agentrax",
								},
							},
						},
						Target: autoscalingv2.MetricTarget{
							Type:         autoscalingv2.AverageValueMetricType,
							AverageValue: targetValue,
						},
					},
				},
			},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: &scaleUpWindow,
					SelectPolicy:               policyPtr(autoscalingv2.MaxChangePolicySelect),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PodsScalingPolicy,
							Value:         4,
							PeriodSeconds: 60,
						},
					},
				},
				ScaleDown: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: &scaleDownWindow,
					SelectPolicy:               policyPtr(autoscalingv2.MinChangePolicySelect),
					Policies: []autoscalingv2.HPAScalingPolicy{
						{
							Type:          autoscalingv2.PodsScalingPolicy,
							Value:         1,
							PeriodSeconds: 60,
						},
					},
				},
			},
		},
	}
}

// IsQuotaCapped returns true when spec.replicas.max exceeds the available
// quota headroom, indicating the HPA's maxReplicas was limited by quota.
// The caller should surface a QuotaLimited condition when this is true.
func IsQuotaCapped(ad *agentraxv1alpha1.AgentDeployment, quotaHeadroom int32) bool {
	return quotaHeadroom < ad.Spec.Replicas.Max
}

// QuotaHeadroom returns the maximum additional replicas the tenant quota
// allows for this AgentDeployment, based on the TenantQuota's maxReplicasPerAgent
// field. The reconciler should pass this into BuildHPA.
//
// usedReplicasByOthers is the sum of spec.replicas.max for all OTHER
// AgentDeployments in the same tenant (i.e., excluding this one).
// The caller is responsible for computing this from the live list.
func QuotaHeadroom(tqSpec agentraxv1alpha1.TenantQuotaSpec, adSpec agentraxv1alpha1.AgentDeploymentSpec, usedReplicasByOthers int32) int32 {
	// Per-agent ceiling from the TenantQuota.
	perAgentCeiling := tqSpec.MaxReplicasPerAgent

	// Total replica budget remaining after accounting for other agents.
	totalBudgetRemaining := tqSpec.MaxTotalReplicas - usedReplicasByOthers
	if totalBudgetRemaining < 0 {
		totalBudgetRemaining = 0
	}

	// The effective headroom is the smaller of the per-agent ceiling and the
	// total budget remaining. This ensures neither constraint is violated.
	headroom := perAgentCeiling
	if totalBudgetRemaining < headroom {
		headroom = totalBudgetRemaining
	}

	// Ensure headroom is at least minReplicas so the HPA has a valid range.
	if headroom < adSpec.Replicas.Min {
		headroom = adSpec.Replicas.Min
	}

	return headroom
}

// hpaLabels returns the label set applied to the managed HPA.
func hpaLabels(ad *agentraxv1alpha1.AgentDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       ad.Name,
		"app.kubernetes.io/managed-by": "agentrax",
		"agentrax.io/tenant":           ad.Spec.TenantRef,
	}
}

// customMetricNameFor maps a spec.replicas.metric value to the Prometheus
// Adapter custom metric name registered in the cluster.
func customMetricNameFor(metric string) string {
	switch metric {
	case MetricGPUUtilization:
		return customMetricGPUUtilization
	default:
		// queueDepth and any unrecognized value default to queue depth.
		return customMetricQueueDepth
	}
}

// policyPtr returns a pointer to an HPAScalingPolicySelect value.
func policyPtr(p autoscalingv2.ScalingPolicySelect) *autoscalingv2.ScalingPolicySelect {
	v := p
	return &v
}

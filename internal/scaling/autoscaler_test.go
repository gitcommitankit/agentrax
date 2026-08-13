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

package scaling

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// makeAD is a test helper that builds a minimal AgentDeployment with the
// given replica policy.
func makeAD(name string, minR, maxR, target int32, metric string) *agentraxv1alpha1.AgentDeployment {
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenant-test",
		},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "nginx:latest",
			TenantRef: "team-test",
			Replicas: agentraxv1alpha1.ScalingPolicy{
				Min:    minR,
				Max:    maxR,
				Metric: metric,
				Target: target,
			},
		},
	}
}

// makeTQSpec is a test helper that builds a TenantQuotaSpec with the two
// limits that vary across HPA/headroom tests. MaxAgents and MaxGPUs are fixed
// at 10 and 0 respectively — they do not affect HPA or headroom calculations.
func makeTQSpec(maxTotalReplicas, maxReplicasPerAgent int32) agentraxv1alpha1.TenantQuotaSpec {
	return agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents:           10,
		MaxGPUs:             0,
		MaxTotalReplicas:    maxTotalReplicas,
		MaxReplicasPerAgent: maxReplicasPerAgent,
	}
}

// ── BuildHPA tests ────────────────────────────────────────────────────────────

func TestBuildHPA_MinMaxReplicas(t *testing.T) {
	t.Parallel()

	ad := makeAD("query-agent", 2, 8, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 10) // headroom > max; no capping

	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas=2, got %v", hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 8 {
		t.Errorf("expected maxReplicas=8, got %d", hpa.Spec.MaxReplicas)
	}
}

func TestBuildHPA_QuotaCapApplied(t *testing.T) {
	t.Parallel()

	ad := makeAD("query-agent", 1, 10, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 5) // headroom < spec.replicas.max → capped at 5

	if hpa.Spec.MaxReplicas != 5 {
		t.Errorf("expected maxReplicas capped at 5, got %d", hpa.Spec.MaxReplicas)
	}
}

func TestBuildHPA_QuotaCapNotApplied(t *testing.T) {
	t.Parallel()

	ad := makeAD("query-agent", 1, 6, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 6) // headroom == max; no capping

	if hpa.Spec.MaxReplicas != 6 {
		t.Errorf("expected maxReplicas=6 (not capped), got %d", hpa.Spec.MaxReplicas)
	}
}

func TestBuildHPA_QuotaHeadroomBelowMin(t *testing.T) {
	t.Parallel()

	// Edge: quota headroom is 0 but minReplicas is 1.
	// BuildHPA must clamp maxReplicas to minReplicas (1) so the HPA spec stays valid.
	ad := makeAD("query-agent", 1, 5, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 0)

	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 1 {
		t.Errorf("expected minReplicas=1, got %v", hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 1 {
		t.Errorf("expected maxReplicas=1 (clamped to minReplicas when headroom=0), got %d",
			hpa.Spec.MaxReplicas)
	}
}

func TestBuildHPA_ScaleTargetRef(t *testing.T) {
	t.Parallel()

	ad := makeAD("my-agent", 1, 4, 100, MetricQueueDepth)
	hpa := BuildHPA(ad, 10)

	ref := hpa.Spec.ScaleTargetRef
	if ref.Kind != "Deployment" {
		t.Errorf("expected ScaleTargetRef.Kind=Deployment, got %s", ref.Kind)
	}
	if ref.Name != "my-agent" {
		t.Errorf("expected ScaleTargetRef.Name=my-agent, got %s", ref.Name)
	}
	if ref.APIVersion != "apps/v1" {
		t.Errorf("expected ScaleTargetRef.APIVersion=apps/v1, got %s", ref.APIVersion)
	}
}

func TestBuildHPA_MetricQueueDepth(t *testing.T) {
	t.Parallel()

	ad := makeAD("query-agent", 1, 5, 75, MetricQueueDepth)
	hpa := BuildHPA(ad, 10)

	requireExternalMetricName(t, hpa, customMetricQueueDepth)
}

func TestBuildHPA_MetricGPUUtilization(t *testing.T) {
	t.Parallel()

	ad := makeAD("gpu-agent", 1, 4, 80, MetricGPUUtilization)
	hpa := BuildHPA(ad, 10)

	requireExternalMetricName(t, hpa, customMetricGPUUtilization)
}

func TestBuildHPA_MetricUnknownDefaultsToQueueDepth(t *testing.T) {
	t.Parallel()

	// Unknown metric values currently fall back to queue depth.
	// This is intentional for forward-compatibility until the CRD enum
	// validation makes unknown values impossible at admission time.
	ad := makeAD("agent", 1, 3, 50, "unknownMetric")
	hpa := BuildHPA(ad, 10)

	requireExternalMetricName(t, hpa, customMetricQueueDepth)
}

func TestBuildHPA_TargetAverageValue(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 5, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 10)

	if len(hpa.Spec.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(hpa.Spec.Metrics))
	}
	m := hpa.Spec.Metrics[0]
	if m.External == nil {
		t.Fatal("expected External metric source, got nil")
	}
	if m.External.Target.Type != autoscalingv2.AverageValueMetricType {
		t.Errorf("expected AverageValue target type, got %s", m.External.Target.Type)
	}

	want := resource.NewMilliQuantity(50*1000, resource.DecimalSI)
	if m.External.Target.AverageValue == nil || !m.External.Target.AverageValue.Equal(*want) {
		t.Errorf("expected AverageValue=%s, got %v", want, m.External.Target.AverageValue)
	}
}

func TestBuildHPA_StabilizationWindows(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 5, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 10)

	b := hpa.Spec.Behavior
	if b == nil {
		t.Fatal("expected HPA behavior to be set")
	}
	if b.ScaleUp == nil || b.ScaleUp.StabilizationWindowSeconds == nil {
		t.Fatal("expected ScaleUp stabilization window")
	}
	if *b.ScaleUp.StabilizationWindowSeconds != scaleUpStabilizationSec {
		t.Errorf("ScaleUp window: want %d, got %d", scaleUpStabilizationSec, *b.ScaleUp.StabilizationWindowSeconds)
	}
	if b.ScaleDown == nil || b.ScaleDown.StabilizationWindowSeconds == nil {
		t.Fatal("expected ScaleDown stabilization window")
	}
	if *b.ScaleDown.StabilizationWindowSeconds != scaleDownStabilizationSec {
		t.Errorf("ScaleDown window: want %d, got %d", scaleDownStabilizationSec, *b.ScaleDown.StabilizationWindowSeconds)
	}
}

func TestBuildHPA_LabelsAndNamespace(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 5, 50, MetricQueueDepth)
	hpa := BuildHPA(ad, 10)

	if hpa.Name != "agent" {
		t.Errorf("expected HPA name=agent, got %s", hpa.Name)
	}
	if hpa.Namespace != "tenant-test" {
		t.Errorf("expected HPA namespace=tenant-test, got %s", hpa.Namespace)
	}
	if hpa.Labels["agentrax.io/tenant"] != "team-test" {
		t.Errorf("expected tenant label, got %v", hpa.Labels)
	}
}

// ── IsQuotaCapped tests ───────────────────────────────────────────────────────

func TestIsQuotaCapped_WhenCapped(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 10, 50, MetricQueueDepth)
	if !IsQuotaCapped(ad, 5) {
		t.Error("expected IsQuotaCapped=true when headroom < spec.replicas.max")
	}
}

func TestIsQuotaCapped_WhenExact(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 5, 50, MetricQueueDepth)
	if IsQuotaCapped(ad, 5) {
		t.Error("expected IsQuotaCapped=false when headroom == spec.replicas.max")
	}
}

func TestIsQuotaCapped_WhenNotCapped(t *testing.T) {
	t.Parallel()

	ad := makeAD("agent", 1, 5, 50, MetricQueueDepth)
	if IsQuotaCapped(ad, 10) {
		t.Error("expected IsQuotaCapped=false when headroom > spec.replicas.max")
	}
}

// ── QuotaHeadroom tests ───────────────────────────────────────────────────────

func TestQuotaHeadroom_PerAgentCeilingLimits(t *testing.T) {
	t.Parallel()

	// maxReplicasPerAgent=4 is smaller than the total budget remaining.
	tqSpec := makeTQSpec(20, 4)
	adSpec := agentraxv1alpha1.AgentDeploymentSpec{Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 10}}

	h := QuotaHeadroom(tqSpec, adSpec, 0)
	if h != 4 {
		t.Errorf("expected headroom=4 (per-agent ceiling), got %d", h)
	}
}

func TestQuotaHeadroom_TotalBudgetLimits(t *testing.T) {
	t.Parallel()

	// Total budget remaining = 20 - 18 = 2; per-agent ceiling = 6.
	// Headroom should be min(6, 2) = 2.
	tqSpec := makeTQSpec(20, 6)
	adSpec := agentraxv1alpha1.AgentDeploymentSpec{Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 6}}

	h := QuotaHeadroom(tqSpec, adSpec, 18)
	if h != 2 {
		t.Errorf("expected headroom=2 (total budget limited), got %d", h)
	}
}

func TestQuotaHeadroom_ZeroBudgetClampsToMin(t *testing.T) {
	t.Parallel()

	// Total already consumed; headroom should be at least minReplicas to
	// keep the HPA spec valid.
	tqSpec := makeTQSpec(10, 6)
	adSpec := agentraxv1alpha1.AgentDeploymentSpec{Replicas: agentraxv1alpha1.ScalingPolicy{Min: 2, Max: 6}}

	h := QuotaHeadroom(tqSpec, adSpec, 10) // total budget remaining = 0
	if h < 2 {
		t.Errorf("expected headroom >= minReplicas(2), got %d", h)
	}
}

func TestQuotaHeadroom_NegativeUsedByOthers(t *testing.T) {
	t.Parallel()

	// usedReplicasByOthers > MaxTotalReplicas: the negative totalBudgetRemaining
	// must be clamped to 0, which forces headroom down to minReplicas.
	tqSpec := makeTQSpec(6, 4)
	adSpec := agentraxv1alpha1.AgentDeploymentSpec{Replicas: agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 4}}

	// usedByOthers=10 > MaxTotalReplicas=6 → totalBudgetRemaining goes negative;
	// headroom must be clamped to 0 then raised to Min=1.
	h := QuotaHeadroom(tqSpec, adSpec, 10)
	if h != 1 {
		t.Errorf("expected headroom=1 (clamped to minReplicas when budget<0), got %d", h)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// requireExternalMetricName asserts that the HPA has exactly one External
// metric source with the given metric name.
func requireExternalMetricName(t *testing.T, hpa *autoscalingv2.HorizontalPodAutoscaler, want string) {
	t.Helper()
	if len(hpa.Spec.Metrics) != 1 {
		t.Fatalf("expected 1 metric spec, got %d", len(hpa.Spec.Metrics))
	}
	m := hpa.Spec.Metrics[0]
	if m.Type != autoscalingv2.ExternalMetricSourceType {
		t.Errorf("expected External metric type, got %s", m.Type)
	}
	if m.External == nil {
		t.Fatal("External metric source is nil")
	}
	if m.External.Metric.Name != want {
		t.Errorf("metric name: want %q, got %q", want, m.External.Metric.Name)
	}
}

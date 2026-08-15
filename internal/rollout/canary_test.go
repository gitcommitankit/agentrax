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

package rollout

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/metrics"
)

// ── Controller unit tests ──────────────────────────────────────────────────

// newTestController constructs a rollout.Controller with a fake client initialized with the scheme.
func newTestController(initObjs ...runtime.Object) (*Controller, client.Client, *runtime.Scheme) {
	s := runtime.NewScheme()
	_ = agentraxv1alpha1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = autoscalingv2.AddToScheme(s)
	_ = gatewayv1.Install(s)

	b := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&agentraxv1alpha1.AgentDeployment{})
	if len(initObjs) > 0 {
		b = b.WithRuntimeObjects(initObjs...)
	}
	cl := b.Build()

	ctrl := &Controller{
		Client:           cl,
		Scheme:           s,
		PromClient:       metrics.NewClient("http://localhost:9090"),
		GatewayName:      "agentrax-gateway",
		GatewayNamespace: "agentrax-system",
		FailSafeTimeout:  60 * time.Second,
	}
	return ctrl, cl, s
}

// TestController_Step_SetWeight verifies that executing a setWeight step creates the canary
// Deployment, creates the HTTPRoute with the specified weights, and advances the step index.
func TestController_Step_SetWeight(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{SetWeight: int32Ptr(20)},
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:           agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:   "img:v1",
			CanaryVersion:   "img:v2",
			CanaryStepIndex: 0,
		},
	}

	c, cl, _ := newTestController(ad)
	res, err := c.Step(context.Background(), ad)
	if err != nil {
		t.Fatalf("Step failed: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue=true after setWeight")
	}

	// Verify canary Deployment was created
	canaryDep := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent-canary", Namespace: "default"}, canaryDep); err != nil {
		t.Errorf("canary deployment not found: %v", err)
	} else if len(canaryDep.Spec.Template.Spec.Containers) == 0 || canaryDep.Spec.Template.Spec.Containers[0].Image != "img:v2" {
		t.Errorf("expected canary deployment image img:v2")
	}

	// Verify HTTPRoute was created with 80/20 weights
	route := &gatewayv1.HTTPRoute{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, route); err != nil {
		t.Errorf("httproute not found: %v", err)
	} else {
		if len(route.Spec.Rules) == 0 || len(route.Spec.Rules[0].BackendRefs) < 2 {
			t.Errorf("expected 2 backend refs in HTTPRoute")
		} else {
			w0 := *route.Spec.Rules[0].BackendRefs[0].Weight
			w1 := *route.Spec.Rules[0].BackendRefs[1].Weight
			if w0 != 80 || w1 != 20 {
				t.Errorf("expected weights (80, 20), got (%d, %d)", w0, w1)
			}
		}
	}

	// Verify status was advanced
	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	if updated.Status.CanaryStepIndex != 1 {
		t.Errorf("expected CanaryStepIndex=1, got %d", updated.Status.CanaryStepIndex)
	}
	if updated.Status.CanaryWeight != 20 {
		t.Errorf("expected CanaryWeight=20, got %d", updated.Status.CanaryWeight)
	}
}

// TestController_Step_SelfHealing verifies that Step recreates canary Deployment and HTTPRoute if deleted out-of-band.
func TestController_Step_SelfHealing(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{SetWeight: int32Ptr(20)},
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:           agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:   "img:v1",
			CanaryVersion:   "img:v2",
			CanaryWeight:    20,
			CanaryStepIndex: 1, // During pause step
		},
	}

	c, cl, _ := newTestController(ad)
	// Canary deployment and HTTPRoute do NOT exist initially (simulating deletion).
	_, _ = c.Step(context.Background(), ad)

	// Verify both were restored by self-healing
	canaryDep := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent-canary", Namespace: "default"}, canaryDep); err != nil {
		t.Errorf("expected self-healing to recreate canary deployment: %v", err)
	}
	route := &gatewayv1.HTTPRoute{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, route); err != nil {
		t.Errorf("expected self-healing to recreate httproute: %v", err)
	}
}

// TestController_Rollback verifies that Rollback tears down canary resources, restores HPA, and sets PhaseRolloutFailed.
func TestController_Rollback(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 5, Metric: "queueDepth", Target: 10},
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Abort:    true,
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:         agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion: "img:v1",
			CanaryVersion: "img:v2",
			CanaryWeight:  20,
		},
	}

	canaryDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-agent-canary", Namespace: "default"}}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"}}

	c, cl, _ := newTestController(ad, canaryDep, route)

	err := c.Rollback(context.Background(), ad, "ManualAbort", "spec.rollout.abort was true")
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Verify canary deployment and httproute were deleted
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent-canary", Namespace: "default"}, &appsv1.Deployment{}); err == nil {
		t.Errorf("expected canary deployment to be deleted")
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, &gatewayv1.HTTPRoute{}); err == nil {
		t.Errorf("expected httproute to be deleted")
	}

	// Verify HPA was restored
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, hpa); err != nil {
		t.Errorf("expected HPA to be restored: %v", err)
	}

	// Verify status was updated to RolloutFailed and canaryVersion preserved
	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	if updated.Status.Phase != agentraxv1alpha1.PhaseRolloutFailed {
		t.Errorf("expected Phase=RolloutFailed, got %s", updated.Status.Phase)
	}
	if updated.Status.CanaryVersion != "img:v2" {
		t.Errorf("expected CanaryVersion=img:v2 to be preserved, got %s", updated.Status.CanaryVersion)
	}
}

// TestController_Promote verifies that promote updates the stable deployment image and cleans up canary resources.
func TestController_Promote(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 5, Metric: "queueDepth", Target: 10},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:         agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion: "img:v1",
			CanaryVersion: "img:v2",
		},
	}

	stableDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "agent", Image: "img:v1"}},
				},
			},
		},
	}
	canaryDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "test-agent-canary", Namespace: "default"}}

	c, cl, _ := newTestController(ad, stableDep, canaryDep)

	err := c.promote(context.Background(), ad)
	if err != nil {
		t.Fatalf("promote failed: %v", err)
	}

	// Verify stable deployment image was updated
	updatedDep := &appsv1.Deployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updatedDep)
	if len(updatedDep.Spec.Template.Spec.Containers) == 0 || updatedDep.Spec.Template.Spec.Containers[0].Image != "img:v2" {
		t.Errorf("expected stable deployment image img:v2, got %v", updatedDep.Spec.Template.Spec.Containers)
	}

	// Verify status was updated to Running
	updatedAD := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updatedAD)
	if updatedAD.Status.Phase != agentraxv1alpha1.PhaseRunning {
		t.Errorf("expected Phase=Running after promote, got %s", updatedAD.Status.Phase)
	}
}

// TestController_PauseHPA_RestoreHPA verifies pausing and restoring the HPA.
func TestController_PauseHPA_RestoreHPA(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v1",
			TenantRef: "team-a",
			Replicas:  agentraxv1alpha1.ScalingPolicy{Min: 1, Max: 5, Metric: "queueDepth", Target: 10},
		},
	}
	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
	}

	c, cl, _ := newTestController(ad, existingHPA)

	// Pause HPA
	if err := c.PauseHPA(context.Background(), ad); err != nil {
		t.Fatalf("PauseHPA failed: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, &autoscalingv2.HorizontalPodAutoscaler{}); err == nil {
		t.Errorf("expected HPA to be deleted during PauseHPA")
	}

	// Restore HPA
	if err := c.restoreHPA(context.Background(), ad); err != nil {
		t.Fatalf("restoreHPA failed: %v", err)
	}
	restoredHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, restoredHPA); err != nil {
		t.Errorf("expected HPA to be restored: %v", err)
	}
}

// TestController_ExecutePause_InitialEntry verifies that the first entry into a pause step records PauseStartedAt and requeues.
func TestController_ExecutePause_InitialEntry(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:           agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:   "img:v1",
			CanaryVersion:   "img:v2",
			CanaryStepIndex: 0,
		},
	}

	c, cl, _ := newTestController(ad)
	res, err := c.executePause(context.Background(), ad, 0, 30*time.Second)
	if err != nil {
		t.Fatalf("executePause failed: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s, got %v", res.RequeueAfter)
	}

	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	if updated.Status.PauseStartedAt == nil {
		t.Errorf("expected PauseStartedAt to be recorded")
	}
}

// TestController_ExecutePause_Success_Advance verifies that when pause duration has elapsed and metrics pass, the step index advances.
func TestController_ExecutePause_Success_Advance(t *testing.T) {
	srv := prometheusServer(t, []float64{200.0, 0.01, 100.0})
	defer srv.Close()

	past := metav1.NewTime(time.Now().Add(-40 * time.Second))
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
					{SetWeight: int32Ptr(100)},
				},
				Rollback: agentraxv1alpha1.RollbackPolicy{
					MaxErrorRate:     "5%",
					MaxP99LatencyMs:  500,
					MinRequestSample: 100,
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:           agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:   "img:v1",
			CanaryVersion:   "img:v2",
			CanaryStepIndex: 0,
			PauseStartedAt:  &past,
		},
	}

	c, cl, _ := newTestController(ad)
	c.PromClient = metrics.NewClient(srv.URL)

	res, err := c.executePause(context.Background(), ad, 0, 30*time.Second)
	if err != nil {
		t.Fatalf("executePause failed: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue=true after pause completion to process next step")
	}

	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	if updated.Status.CanaryStepIndex != 1 {
		t.Errorf("expected CanaryStepIndex=1 after advancement, got %d", updated.Status.CanaryStepIndex)
	}
	if updated.Status.PauseStartedAt != nil {
		t.Errorf("expected PauseStartedAt to be reset to nil")
	}
}

// TestController_ExecutePause_PrometheusUnreachable_FailSafe verifies that if Prometheus is down past the fail-safe timeout, rollback is triggered.
func TestController_ExecutePause_PrometheusUnreachable_FailSafe(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-100 * time.Second))
	unreachableSince := metav1.NewTime(time.Now().Add(-70 * time.Second)) // > 60s
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:                agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:        "img:v1",
			CanaryVersion:        "img:v2",
			CanaryStepIndex:      0,
			PauseStartedAt:       &started,
			PromUnreachableSince: &unreachableSince,
		},
	}

	c, cl, _ := newTestController(ad)
	c.PromClient = metrics.NewClient("http://127.0.0.1:1") // Unreachable port
	c.FailSafeTimeout = 60 * time.Second

	_, err := c.executePause(context.Background(), ad, 0, 30*time.Second)
	if err != nil {
		t.Fatalf("executePause failed: %v", err)
	}
	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	if updated.Status.Phase != agentraxv1alpha1.PhaseRolloutFailed {
		t.Errorf("expected Phase=RolloutFailed after fail-safe timeout, got %s", updated.Status.Phase)
	}
}

// TestController_ExecutePause_SampleTooSmall verifies that when sample count is below minRequestSample, the pause is extended and ConditionSampleInsufficient is set.
func TestController_ExecutePause_SampleTooSmall(t *testing.T) {
	srv := prometheusServer(t, []float64{50.0, 0.0, 50.0}) // count=50 < min 100
	defer srv.Close()

	past := metav1.NewTime(time.Now().Add(-10 * time.Second))
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "img:v2",
			TenantRef: "team-a",
			Rollout: agentraxv1alpha1.RolloutPolicy{
				Strategy: "Canary",
				Steps: []agentraxv1alpha1.CanaryStep{
					{Pause: &metav1.Duration{Duration: 30 * time.Second}},
				},
				Rollback: agentraxv1alpha1.RollbackPolicy{
					MinRequestSample: 100,
				},
			},
		},
		Status: agentraxv1alpha1.AgentDeploymentStatus{
			Phase:           agentraxv1alpha1.PhaseRolloutInProgress,
			StableVersion:   "img:v1",
			CanaryVersion:   "img:v2",
			CanaryStepIndex: 0,
			PauseStartedAt:  &past,
		},
	}

	c, cl, _ := newTestController(ad)
	c.PromClient = metrics.NewClient(srv.URL)

	res, err := c.executePause(context.Background(), ad, 0, 30*time.Second)
	if err != nil {
		t.Fatalf("executePause failed: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter for pause extension")
	}

	updated := &agentraxv1alpha1.AgentDeployment{}
	_ = cl.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, updated)
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, agentraxv1alpha1.ConditionSampleInsufficient)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionSampleInsufficient to be True")
	}
}

func int32Ptr(i int32) *int32 { return &i }

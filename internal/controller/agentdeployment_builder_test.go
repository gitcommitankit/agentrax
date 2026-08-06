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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// testAgentName is the canonical agent name used across builder unit tests.
const testAgentName = "my-agent"

// testDefaultImage is the standard placeholder image used in builder unit tests.
const testDefaultImage = "img:v1"

// testDefaultAgent is the default agent name used for tests not specifically
// testing label or name behaviour.
const testDefaultAgent = "agent"

// testLabelName is the well-known label key for the resource/agent name.
const testLabelName = "app.kubernetes.io/name"

// makeAD is a test helper that constructs a minimal AgentDeployment.
func makeAD(name, image string, port int32, minReplicas int32) *agentraxv1alpha1.AgentDeployment {
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "tenant-test",
		},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     image,
			Port:      port,
			TenantRef: "team-test",
			Replicas: agentraxv1alpha1.ScalingPolicy{
				Min:    minReplicas,
				Max:    3,
				Metric: "queueDepth",
				Target: 50,
			},
		},
	}
}

// ── desiredDeployment ─────────────────────────────────────────────────────────

func TestDesiredDeployment_Image(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD("my-agent", "registry.io/agent:v1", 8080, 1)
	dep := r.desiredDeployment(ad)

	if dep.Spec.Template.Spec.Containers[0].Image != "registry.io/agent:v1" {
		t.Errorf("expected image registry.io/agent:v1, got %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestDesiredDeployment_Port(t *testing.T) {
	tests := []struct {
		name     string
		specPort int32
		wantPort int32
	}{
		{"explicit port", 9090, 9090},
		{"zero defaults to 8080", 0, 8080},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &AgentDeploymentReconciler{}
			ad := makeAD(testDefaultAgent, testDefaultImage, tc.specPort, 1)
			dep := r.desiredDeployment(ad)
			got := dep.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort
			if got != tc.wantPort {
				t.Errorf("expected port %d, got %d", tc.wantPort, got)
			}
		})
	}
}

func TestDesiredDeployment_Replicas(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 3)
	dep := r.desiredDeployment(ad)

	if *dep.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *dep.Spec.Replicas)
	}
}

func TestDesiredDeployment_EnvAndArgs(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 1)
	ad.Spec.Env = []corev1.EnvVar{{Name: "MODEL_PATH", Value: "/models/v1"}}
	ad.Spec.Args = []string{"--serve", "--port=8080"}

	dep := r.desiredDeployment(ad)
	c := dep.Spec.Template.Spec.Containers[0]

	if len(c.Env) != 1 || c.Env[0].Name != "MODEL_PATH" {
		t.Errorf("expected env MODEL_PATH, got %v", c.Env)
	}
	if len(c.Args) != 2 || c.Args[0] != "--serve" {
		t.Errorf("expected args [--serve --port=8080], got %v", c.Args)
	}
}

func TestDesiredDeployment_Resources(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 1)
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	ad.Spec.Resources = want

	dep := r.desiredDeployment(ad)
	got := dep.Spec.Template.Spec.Containers[0].Resources

	for rsrc, wantQ := range want.Requests {
		gotQ := got.Requests[rsrc]
		if gotQ.Cmp(wantQ) != 0 {
			t.Errorf("Resources.Requests[%s]: want %s, got %s", rsrc, wantQ.String(), gotQ.String())
		}
	}
	for rsrc, wantQ := range want.Limits {
		gotQ := got.Limits[rsrc]
		if gotQ.Cmp(wantQ) != 0 {
			t.Errorf("Resources.Limits[%s]: want %s, got %s", rsrc, wantQ.String(), gotQ.String())
		}
	}
}

func TestDesiredDeployment_Labels(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testAgentName, testDefaultImage, 8080, 1)
	dep := r.desiredDeployment(ad)

	if dep.Labels[testLabelName] != testAgentName {
		t.Errorf("expected label %s=%s, got %s", testLabelName, testAgentName, dep.Labels[testLabelName])
	}
	if dep.Labels["app.kubernetes.io/managed-by"] != "agentrax" {
		t.Errorf("expected label managed-by=agentrax, got %s", dep.Labels["app.kubernetes.io/managed-by"])
	}
	if dep.Labels["agentrax.io/tenant"] != "team-test" {
		t.Errorf("expected label agentrax.io/tenant=team-test, got %s", dep.Labels["agentrax.io/tenant"])
	}
	// Pod template labels must match selector.
	if dep.Spec.Template.Labels[testLabelName] != testAgentName {
		t.Errorf("pod template labels missing %s", testLabelName)
	}
}

func TestDesiredDeployment_SelectorMatchesPodLabels(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 1)
	dep := r.desiredDeployment(ad)

	for k, v := range dep.Spec.Selector.MatchLabels {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("selector key %s=%s not present in pod template labels", k, v)
		}
	}
}

// ── desiredService ────────────────────────────────────────────────────────────

func TestDesiredService_Port(t *testing.T) {
	tests := []struct {
		name     string
		specPort int32
		wantPort int32
	}{
		{"explicit port", 9090, 9090},
		{"zero defaults to 8080", 0, 8080},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &AgentDeploymentReconciler{}
			ad := makeAD(testDefaultAgent, testDefaultImage, tc.specPort, 1)
			svc := r.desiredService(ad)
			if svc.Spec.Ports[0].Port != tc.wantPort {
				t.Errorf("expected port %d, got %d", tc.wantPort, svc.Spec.Ports[0].Port)
			}
		})
	}
}

func TestDesiredService_Selector(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testAgentName, testDefaultImage, 8080, 1)
	svc := r.desiredService(ad)

	if svc.Spec.Selector[testLabelName] != testAgentName {
		t.Errorf("service selector missing %s=%s", testLabelName, testAgentName)
	}
}

func TestDesiredService_ClusterIPType(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 1)
	svc := r.desiredService(ad)

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP service type, got %s", svc.Spec.Type)
	}
}

// ── agentLabels ───────────────────────────────────────────────────────────────

func TestAgentLabels(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec:       agentraxv1alpha1.AgentDeploymentSpec{TenantRef: "bar"},
	}
	labels := agentLabels(ad)

	expected := map[string]string{
		"app.kubernetes.io/name":       "foo",
		"app.kubernetes.io/managed-by": "agentrax",
		"agentrax.io/tenant":           "bar",
	}
	for k, v := range expected {
		if labels[k] != v {
			t.Errorf("agentLabels[%s] = %q, want %q", k, labels[k], v)
		}
	}
}

// ── condition helpers ─────────────────────────────────────────────────────────

func TestSetAndGetCondition(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{}

	SetCondition(ad, agentraxv1alpha1.ConditionReady, metav1.ConditionTrue, "TestReason", "all good")
	c := GetCondition(ad, agentraxv1alpha1.ConditionReady)

	if c == nil {
		t.Fatal("expected condition to be set, got nil")
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionTrue, got %s", c.Status)
	}
	if c.Reason != "TestReason" {
		t.Errorf("expected reason TestReason, got %s", c.Reason)
	}
}

func TestSetCondition_Overwrite(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{}

	SetCondition(ad, agentraxv1alpha1.ConditionReady, metav1.ConditionFalse, "NotReady", "waiting")
	SetCondition(ad, agentraxv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", "ready now")

	c := GetCondition(ad, agentraxv1alpha1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("expected overwrite to ConditionTrue, got %v", c)
	}
	// Slice must not grow with duplicate condition types.
	if len(ad.Status.Conditions) != 1 {
		t.Errorf("expected 1 condition after overwrite, got %d", len(ad.Status.Conditions))
	}
}

func TestRemoveCondition(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{}

	SetCondition(ad, agentraxv1alpha1.ConditionReady, metav1.ConditionTrue, "R", "m")
	SetCondition(ad, agentraxv1alpha1.ConditionReconciled, metav1.ConditionTrue, "R", "m")

	RemoveCondition(ad, agentraxv1alpha1.ConditionReady)

	if GetCondition(ad, agentraxv1alpha1.ConditionReady) != nil {
		t.Error("expected Ready condition to be removed")
	}
	if GetCondition(ad, agentraxv1alpha1.ConditionReconciled) == nil {
		t.Error("expected Reconciled condition to remain after removing Ready")
	}
}

func TestRemoveCondition_NonExistent(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{}
	// Should be a no-op, not panic.
	RemoveCondition(ad, agentraxv1alpha1.ConditionReady)
	if len(ad.Status.Conditions) != 0 {
		t.Errorf("expected 0 conditions after removing non-existent, got %d", len(ad.Status.Conditions))
	}
}

func TestGetCondition_Absent(t *testing.T) {
	ad := &agentraxv1alpha1.AgentDeployment{}
	c := GetCondition(ad, agentraxv1alpha1.ConditionReady)
	if c != nil {
		t.Errorf("expected nil for absent condition, got %v", c)
	}
}

// ── desiredServiceMonitor ─────────────────────────────────────────────────────

func TestDesiredServiceMonitor_Endpoint(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testDefaultAgent, testDefaultImage, 8080, 1)
	sm := r.desiredServiceMonitor(ad)

	if len(sm.Spec.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(sm.Spec.Endpoints))
	}
	ep := sm.Spec.Endpoints[0]
	if ep.Path != "/metrics" {
		t.Errorf("expected path /metrics, got %s", ep.Path)
	}
	if ep.Port != "agent" {
		t.Errorf("expected port name 'agent', got %s", ep.Port)
	}
}

func TestDesiredServiceMonitor_SelectorMatchesLabels(t *testing.T) {
	r := &AgentDeploymentReconciler{}
	ad := makeAD(testAgentName, testDefaultImage, 8080, 1)
	sm := r.desiredServiceMonitor(ad)

	if sm.Spec.Selector.MatchLabels[testLabelName] != testAgentName {
		t.Errorf("ServiceMonitor selector missing %s=%s", testLabelName, testAgentName)
	}
}

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

package quota_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/quota"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func makeSpec(maxReplicas int32, gpuLimit string) agentraxv1alpha1.AgentDeploymentSpec {
	spec := agentraxv1alpha1.AgentDeploymentSpec{
		Image:     "test-image:v1",
		TenantRef: "test-tenant",
		Replicas: agentraxv1alpha1.ScalingPolicy{
			Min:    1,
			Max:    maxReplicas,
			Metric: "queueDepth",
			Target: 50,
		},
	}
	if gpuLimit != "" {
		spec.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse(gpuLimit),
			},
		}
	}
	return spec
}

func makeQuota(maxAgents, maxGPUs, maxTotalReplicas, maxReplicasPerAgent int32) agentraxv1alpha1.TenantQuotaSpec {
	return agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents:           maxAgents,
		MaxGPUs:             maxGPUs,
		MaxTotalReplicas:    maxTotalReplicas,
		MaxReplicasPerAgent: maxReplicasPerAgent,
	}
}

func makeUsage(agents, gpus, replicas int32) agentraxv1alpha1.TenantQuotaStatus {
	return agentraxv1alpha1.TenantQuotaStatus{
		UsedAgents:        agents,
		UsedGPUs:          gpus,
		UsedTotalReplicas: replicas,
	}
}

// newTestEnforcer creates an Enforcer for tests and registers Stop() as a cleanup
// function so the background sweep goroutine is terminated when the test ends.
func newTestEnforcer(t *testing.T) *quota.Enforcer {
	t.Helper()
	e := quota.NewEnforcer(quota.DefaultGPUResourceName)
	t.Cleanup(e.Stop)
	return e
}

// ── CanAdmit tests ────────────────────────────────────────────────────────────

func TestCanAdmit_Create(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		quota       agentraxv1alpha1.TenantQuotaSpec
		usage       agentraxv1alpha1.TenantQuotaStatus
		spec        agentraxv1alpha1.AgentDeploymentSpec
		wantAdmit   bool
		wantContain string // substring that must appear in denial reason
	}{
		{
			name:      "within all limits",
			quota:     makeQuota(6, 4, 12, 6),
			usage:     makeUsage(2, 0, 4),
			spec:      makeSpec(3, ""),
			wantAdmit: true,
		},
		{
			name:        "at agent limit → rejected",
			quota:       makeQuota(3, 4, 12, 6),
			usage:       makeUsage(3, 0, 6),
			spec:        makeSpec(2, ""),
			wantAdmit:   false,
			wantContain: "maxAgents",
		},
		{
			name:        "would exceed maxTotalReplicas",
			quota:       makeQuota(6, 4, 10, 6),
			usage:       makeUsage(2, 0, 9),
			spec:        makeSpec(2, ""), // 9+2 = 11 > 10
			wantAdmit:   false,
			wantContain: "maxTotalReplicas",
		},
		{
			name:        "would exceed maxReplicasPerAgent",
			quota:       makeQuota(6, 4, 12, 4),
			usage:       makeUsage(0, 0, 0),
			spec:        makeSpec(5, ""), // max=5 > maxReplicasPerAgent=4
			wantAdmit:   false,
			wantContain: "maxReplicasPerAgent",
		},
		{
			name:      "GPU within limit",
			quota:     makeQuota(6, 4, 12, 6),
			usage:     makeUsage(1, 2, 3),
			spec:      makeSpec(3, "1"), // 1 GPU × 3 replicas = 3; 2+3=5 > 4? no, quota is 4 wait: 2+3=5 > 4 = reject
			wantAdmit: false,
			// actually 1GPU×3replicas=3 + used=2 = 5 > 4 → rejected
			wantContain: "maxGPUs",
		},
		{
			name:      "GPU exactly at limit",
			quota:     makeQuota(6, 4, 12, 6),
			usage:     makeUsage(1, 2, 3),
			spec:      makeSpec(2, "1"), // 1 GPU × 2 replicas = 2; 2+2=4 == 4 → ok
			wantAdmit: true,
		},
		{
			name:      "no GPU limit set on spec → GPU check skipped",
			quota:     makeQuota(6, 4, 12, 6),
			usage:     makeUsage(1, 4, 3), // already at GPU limit
			spec:      makeSpec(2, ""),    // no GPU requested → no increase
			wantAdmit: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnforcer(t)
			got, reason := e.CanAdmit("ns/ad-test", tc.quota, tc.usage, tc.spec, nil)
			if got != tc.wantAdmit {
				t.Errorf("CanAdmit() = %v, want %v; reason: %q", got, tc.wantAdmit, reason)
			}
			if !tc.wantAdmit && tc.wantContain != "" {
				if reason == "" {
					t.Errorf("CanAdmit() denied but returned empty reason")
				} else if !containsSubstring(reason, tc.wantContain) {
					t.Errorf("CanAdmit() reason %q does not contain %q", reason, tc.wantContain)
				}
			}
		})
	}
}

func TestCanAdmit_Update(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(6, 4, 10, 6)
	usage := makeUsage(2, 0, 8)
	oldSpec := makeSpec(4, "")
	newSpec := makeSpec(5, "") // delta replicas = +1; 8+1=9 ≤ 10 → ok
	ok, reason := e.CanAdmit("ns/ad-A", q, usage, newSpec, &oldSpec)
	if !ok {
		t.Errorf("expected update to be admitted but got reason: %q", reason)
	}

	// Delta that hits the ceiling exactly → ok.
	newSpec2 := makeSpec(6, "") // delta replicas = +2; 8+2=10 ≤ 10 → ok
	ok2, _ := e.CanAdmit("ns/ad-A", q, usage, newSpec2, &oldSpec)
	if !ok2 {
		t.Errorf("expected exact-limit update to be admitted")
	}

	// Delta that exceeds ceiling → rejected.
	newSpec3 := makeSpec(7, "") // delta replicas = +3; 8+3=11 > 10 → rejected
	ok3, reason3 := e.CanAdmit("ns/ad-A", q, usage, newSpec3, &oldSpec)
	if ok3 {
		t.Errorf("expected over-limit update to be rejected; got reason %q", reason3)
	}
}

func TestCanAdmit_Update_MaxReplicasPerAgent_Downgrade(t *testing.T) {
	// When maxReplicasPerAgent is lowered below an existing AD's replicas.max,
	// updates that do NOT further increase replicas.max must still be allowed.
	// Blocking them would deadlock spec corrections on over-quota ADs.
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(6, 0, 20, 3) // maxReplicasPerAgent lowered to 3
	usage := makeUsage(1, 0, 5)

	oldSpec := makeSpec(5, "") // existing AD already has max=5 > new ceiling of 3

	// UPDATE that keeps replicas.max unchanged → must be admitted (no increase).
	sameSpec := makeSpec(5, "")
	ok, reason := e.CanAdmit("ns/ad-existing", q, usage, sameSpec, &oldSpec)
	if !ok {
		t.Errorf("update keeping replicas.max unchanged should be allowed after quota downgrade; got: %q", reason)
	}

	// UPDATE that reduces replicas.max → must also be admitted.
	smallerSpec := makeSpec(4, "")
	ok2, reason2 := e.CanAdmit("ns/ad-existing", q, usage, smallerSpec, &oldSpec)
	if !ok2 {
		t.Errorf("update reducing replicas.max should be allowed; got: %q", reason2)
	}

	// UPDATE that further increases replicas.max → must be rejected.
	largerSpec := makeSpec(6, "")
	ok3, reason3 := e.CanAdmit("ns/ad-existing", q, usage, largerSpec, &oldSpec)
	if ok3 {
		t.Errorf("update increasing replicas.max beyond maxReplicasPerAgent should be rejected; got reason: %q", reason3)
	}
	if !containsSubstring(reason3, "maxReplicasPerAgent") {
		t.Errorf("denial reason %q should mention maxReplicasPerAgent", reason3)
	}
}

// ── In-flight reservation tests ───────────────────────────────────────────────

func TestReservation_BlocksConcurrentCreate(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(2, 0, 4, 2)
	usage := makeUsage(1, 0, 2) // 1 of 2 agent slots used

	spec := makeSpec(2, "")

	// First admission check passes; then we reserve.
	ok1, _ := e.CanAdmit("ns/ad-A", q, usage, spec, nil)
	if !ok1 {
		t.Fatal("first CanAdmit should have passed")
	}
	e.Reserve("ns/ad-A", spec, nil, 5*time.Second)

	// Second concurrent request for the same remaining slot should now be blocked
	// because ad-A's reservation already claimed it.
	ok2, reason2 := e.CanAdmit("ns/ad-B", q, usage, spec, nil)
	if ok2 {
		t.Errorf("second CanAdmit should have been blocked by in-flight reservation; reason=%q", reason2)
	}

	// After releasing ad-A's reservation, the second request passes again.
	e.Release("ns/ad-A")
	ok3, _ := e.CanAdmit("ns/ad-B", q, usage, spec, nil)
	if !ok3 {
		t.Error("after Release, CanAdmit should pass again")
	}
}

func TestReservation_DoesNotDoubleCount(t *testing.T) {
	// A re-admission for the same AD key should exclude its own prior reservation
	// so it isn't double-counted.
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(3, 0, 6, 3)
	usage := makeUsage(1, 0, 2)
	spec := makeSpec(2, "")

	e.Reserve("ns/ad-X", spec, nil, 5*time.Second)

	// Calling CanAdmit with the same admissionKey should exclude its own
	// reservation from the in-flight sum (no double-count).
	ok, _ := e.CanAdmit("ns/ad-X", q, usage, spec, nil)
	// usage.agents=1, in-flight from ad-X is excluded, delta=1 → projected=2 ≤ 3 → ok
	if !ok {
		t.Error("CanAdmit for the same AD key should not be blocked by its own reservation")
	}
}

// ── ComputeUsage tests ────────────────────────────────────────────────────────

func TestComputeUsage(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	ads := []agentraxv1alpha1.AgentDeploymentSpec{
		makeSpec(3, "1"), // 1 GPU × 3 = 3 GPU units
		makeSpec(2, "2"), // 2 GPU × 2 = 4 GPU units
		makeSpec(4, ""),  // 0 GPUs
	}
	got := e.ComputeUsage(ads)
	if got.UsedAgents != 3 {
		t.Errorf("UsedAgents = %d, want 3", got.UsedAgents)
	}
	if got.UsedGPUs != 7 { // 3 + 4
		t.Errorf("UsedGPUs = %d, want 7", got.UsedGPUs)
	}
	if got.UsedTotalReplicas != 9 { // 3+2+4
		t.Errorf("UsedTotalReplicas = %d, want 9", got.UsedTotalReplicas)
	}
}

func TestComputeUsage_Empty(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	got := e.ComputeUsage(nil)
	if got.UsedAgents != 0 || got.UsedGPUs != 0 || got.UsedTotalReplicas != 0 {
		t.Errorf("expected all-zero for empty input, got %+v", got)
	}
}

// ── IsOverQuota tests ─────────────────────────────────────────────────────────

func TestIsOverQuota(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(3, 4, 10, 3)

	tests := []struct {
		name    string
		usage   agentraxv1alpha1.TenantQuotaStatus
		wantOQ  bool
		wantMsg string
	}{
		{"within limits", makeUsage(2, 3, 8), false, ""},
		{"agents over", makeUsage(4, 3, 8), true, "maxAgents"},
		{"GPUs over", makeUsage(2, 5, 8), true, "maxGPUs"},
		{"replicas over", makeUsage(2, 3, 11), true, "maxTotalReplicas"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			over, msg := e.IsOverQuota(q, tc.usage)
			if over != tc.wantOQ {
				t.Errorf("IsOverQuota() = %v, want %v; msg=%q", over, tc.wantOQ, msg)
			}
			if tc.wantOQ && !containsSubstring(msg, tc.wantMsg) {
				t.Errorf("IsOverQuota() msg %q does not contain %q", msg, tc.wantMsg)
			}
		})
	}
}

// ── ParseErrorRate tests ──────────────────────────────────────────────────────
// ParseErrorRate lives in api/v1alpha1 to avoid an import cycle.
// Its tests are in api/v1alpha1/webhook_test.go.

func TestParseErrorRate_ViaV1alpha1(t *testing.T) {
	// Smoke-test that ParseErrorRate is accessible from outside api/v1alpha1.
	t.Parallel()
	got, err := agentraxv1alpha1.ParseErrorRate("5%")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if absFloat(got-0.05) > 1e-9 {
		t.Errorf("ParseErrorRate(5%%) = %v, want 0.05", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsSubstring(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

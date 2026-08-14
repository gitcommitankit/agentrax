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

// White-box test: package quota (not quota_test) so unexported methods
// like reserve are accessible for isolated unit testing.
// AdmitAndReserve remains the production-facing atomic API.
package quota

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// makeSpec creates an AgentDeploymentSpec test fixture with given maxReplicas and GPU limit.
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

// makeQuota creates a TenantQuotaSpec test fixture with given quota limits.
func makeQuota(maxAgents, maxGPUs, maxTotalReplicas, maxReplicasPerAgent int32) agentraxv1alpha1.TenantQuotaSpec {
	return agentraxv1alpha1.TenantQuotaSpec{
		MaxAgents:           maxAgents,
		MaxGPUs:             maxGPUs,
		MaxTotalReplicas:    maxTotalReplicas,
		MaxReplicasPerAgent: maxReplicasPerAgent,
	}
}

// makeUsage creates a TenantQuotaStatus test fixture with given observed usage.
func makeUsage(agents, gpus, replicas int32) agentraxv1alpha1.TenantQuotaStatus {
	return agentraxv1alpha1.TenantQuotaStatus{
		UsedAgents:        agents,
		UsedGPUs:          gpus,
		UsedTotalReplicas: replicas,
	}
}

// newTestEnforcer creates an Enforcer for tests and registers Stop() as a cleanup
// function so the background sweep goroutine is terminated when the test ends.
func newTestEnforcer(t *testing.T) *Enforcer {
	t.Helper()
	e := NewEnforcer(DefaultGPUResourceName)
	t.Cleanup(e.Stop)
	return e
}

// ── CanAdmit tests ────────────────────────────────────────────────────────────

// TestCanAdmit_Create verifies admission decisions for new AgentDeployment creates against various quota scenarios.
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
		{
			name:        "zero GPU quota rejects positive GPU request",
			quota:       makeQuota(6, 0, 12, 6),
			usage:       makeUsage(1, 0, 2),
			spec:        makeSpec(2, "1"), // 1 GPU × 2 replicas = 2 > 0
			wantAdmit:   false,
			wantContain: "maxGPUs",
		},
		{
			name:      "zero GPU quota admits non-GPU request",
			quota:     makeQuota(6, 0, 12, 6),
			usage:     makeUsage(1, 0, 2),
			spec:      makeSpec(2, ""), // 0 GPUs requested
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
				} else if !strings.Contains(reason, tc.wantContain) {
					t.Errorf("CanAdmit() reason %q does not contain %q", reason, tc.wantContain)
				}
			}
		})
	}
}

// TestCanAdmit_Update verifies admission decisions when updating existing AgentDeployment specs.
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

// TestCanAdmit_Update_MaxReplicasPerAgent_Downgrade verifies that lowering maxReplicasPerAgent does not deadlock updates that do not increase replicas.
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
	if !strings.Contains(reason3, "maxReplicasPerAgent") {
		t.Errorf("denial reason %q should mention maxReplicasPerAgent", reason3)
	}
}

// TestCanAdmit_Update_GPUCeiling verifies admission behavior when updating GPU requests or when GPU quota is reduced.
func TestCanAdmit_Update_GPUCeiling(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)

	// Zero GPU quota rejects increasing GPU allocation.
	qZero := makeQuota(6, 0, 10, 6)
	usage := makeUsage(1, 0, 2)
	oldSpecNoGPU := makeSpec(2, "")
	newSpecWithGPU := makeSpec(2, "1") // requests 2 GPUs when quota is 0
	ok, reason := e.CanAdmit("ns/ad-A", qZero, usage, newSpecWithGPU, &oldSpecNoGPU)
	if ok {
		t.Errorf("expected GPU request on zero-GPU quota to be rejected; got ok")
	}
	if !strings.Contains(reason, "maxGPUs") {
		t.Errorf("expected reason to contain maxGPUs, got %q", reason)
	}

	// Lowering GPU quota below usage allows non-increasing updates.
	qLow := makeQuota(6, 2, 10, 6)  // quota lowered to 2 GPUs
	usageOver := makeUsage(1, 4, 2) // current usage is 4 GPUs (already over)
	oldSpec4GPU := makeSpec(2, "2") // 2 GPU × 2 = 4 GPUs

	// Update that doesn't increase GPUs (e.g. image change or same GPUs) is admitted.
	sameGPU := makeSpec(2, "2")
	ok2, reason2 := e.CanAdmit("ns/ad-A", qLow, usageOver, sameGPU, &oldSpec4GPU)
	if !ok2 {
		t.Errorf("expected non-increasing GPU update to be admitted when over-quota; got reason: %q", reason2)
	}

	// Update that increases GPUs further is rejected.
	moreGPU := makeSpec(3, "2") // 2 GPU × 3 = 6 GPUs (delta +2)
	ok3, reason3 := e.CanAdmit("ns/ad-A", qLow, usageOver, moreGPU, &oldSpec4GPU)
	if ok3 {
		t.Errorf("expected increasing GPU update when over-quota to be rejected")
	}
	if !strings.Contains(reason3, "maxGPUs") {
		t.Errorf("expected reason to contain maxGPUs, got %q", reason3)
	}
}

// TestCanAdmit_Update_CrossTenantMove verifies that when an AgentDeployment
// moves from one tenant to another (TenantRef changes), the full requested
// resources are charged to the target tenant's quota, not a delta.
func TestCanAdmit_Update_CrossTenantMove(t *testing.T) {
	t.Parallel()
	const (
		oldTenant    = "old-tenant"
		targetTenant = "target-tenant"
	)
	e := newTestEnforcer(t)

	// Target tenant quota is already at capacity (no room for a delta).
	targetQuota := makeQuota(2, 4, 6, 4) // maxAgents=2, maxGPUs=4, maxTotalReplicas=6
	targetUsage := makeUsage(2, 4, 6)    // already at full capacity

	// Old spec was in a different tenant ("old-tenant").
	oldSpec := makeSpec(3, "1") // 3 replicas, 1 GPU per replica = 3 GPUs total
	oldSpec.TenantRef = oldTenant

	// New spec moves to "target-tenant" with same resources.
	newSpec := makeSpec(3, "1") // same resources: 3 replicas, 3 GPUs
	newSpec.TenantRef = targetTenant

	// The target tenant is already at capacity. Since this is a cross-tenant
	// move, the full amount (1 agent, 3 GPUs, 3 replicas) should be charged
	// to the target tenant, not a delta. This should be rejected because the
	// target quota cannot accommodate the full allocation.
	ok, reason := e.CanAdmit("ns/ad-move", targetQuota, targetUsage, newSpec, &oldSpec)
	if ok {
		t.Errorf("expected cross-tenant move into full quota to be rejected; got admitted")
	}
	// Should fail on agents limit since target is at 2/2 and we need +1.
	if !strings.Contains(reason, "maxAgents") {
		t.Errorf("expected reason to mention maxAgents, got %q", reason)
	}

	// Test case 2: Target tenant has enough room for the full allocation.
	targetQuota2 := makeQuota(3, 8, 10, 4) // enough room: maxAgents=3, maxGPUs=8, maxTotalReplicas=10
	targetUsage2 := makeUsage(1, 2, 4)     // current: 1 agent, 2 GPUs, 4 replicas

	oldSpec2 := makeSpec(3, "1") // 3 replicas, 3 GPUs
	oldSpec2.TenantRef = oldTenant

	newSpec2 := makeSpec(3, "1") // same resources
	newSpec2.TenantRef = targetTenant

	// Target can accommodate: 1+1=2 ≤ 3 agents, 2+3=5 ≤ 8 GPUs, 4+3=7 ≤ 10 replicas.
	ok2, reason2 := e.CanAdmit("ns/ad-move2", targetQuota2, targetUsage2, newSpec2, &oldSpec2)
	if !ok2 {
		t.Errorf("expected cross-tenant move into quota with capacity to be admitted; got reason: %q", reason2)
	}

	// Test case 3: Cross-tenant move with resource change (increase).
	// Old spec: different tenant, 2 replicas, 2 GPUs.
	oldSpec3 := makeSpec(2, "1")
	oldSpec3.TenantRef = oldTenant

	// New spec: target tenant, 4 replicas, 4 GPUs.
	newSpec3 := makeSpec(4, "1")
	newSpec3.TenantRef = targetTenant

	targetQuota3 := makeQuota(3, 5, 8, 5)
	targetUsage3 := makeUsage(1, 1, 2) // 1 agent, 1 GPU, 2 replicas

	// Should charge FULL new amount: +1 agent, +4 GPUs, +4 replicas.
	// Result: 2 agents ≤ 3, 5 GPUs ≤ 5, 6 replicas ≤ 8 → should be admitted.
	ok3, reason3 := e.CanAdmit("ns/ad-move3", targetQuota3, targetUsage3, newSpec3, &oldSpec3)
	if !ok3 {
		t.Errorf("expected cross-tenant move with resource increase to be admitted when quota allows; got reason: %q", reason3)
	}

	// Test case 4: Same scenario but quota is too tight for the full allocation.
	targetQuota4 := makeQuota(3, 4, 8, 5) // maxGPUs=4 (too low for +4)
	targetUsage4 := makeUsage(1, 1, 2)

	oldSpec4 := makeSpec(2, "1")
	oldSpec4.TenantRef = oldTenant

	newSpec4 := makeSpec(4, "1") // 4 GPUs needed
	newSpec4.TenantRef = targetTenant

	// 1 + 4 = 5 GPUs > 4 → should be rejected.
	ok4, reason4 := e.CanAdmit("ns/ad-move4", targetQuota4, targetUsage4, newSpec4, &oldSpec4)
	if ok4 {
		t.Errorf("expected cross-tenant move into insufficient GPU quota to be rejected")
	}
	if !strings.Contains(reason4, "maxGPUs") {
		t.Errorf("expected reason to mention maxGPUs, got %q", reason4)
	}
}

// ── In-flight reservation tests ───────────────────────────────────────────────

// TestReservation_BlocksConcurrentCreate verifies that an in-flight reservation blocks concurrent creation of the same remaining slot.
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
	e.reserve("ns/ad-A", spec, nil, 5*time.Second)

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

// TestReservation_DoesNotDoubleCount verifies that re-admission for the same AD key excludes its own prior reservation.
func TestReservation_DoesNotDoubleCount(t *testing.T) {
	// A re-admission for the same AD key should exclude its own prior reservation
	// so it isn't double-counted.
	t.Parallel()
	e := newTestEnforcer(t)
	q := makeQuota(3, 0, 6, 3)
	usage := makeUsage(1, 0, 2)
	spec := makeSpec(2, "")

	e.reserve("ns/ad-X", spec, nil, 5*time.Second)

	// Calling canAdmit with the same admissionKey should exclude its own
	// reservation from the in-flight sum (no double-count).
	ok, _ := e.CanAdmit("ns/ad-X", q, usage, spec, nil)
	// usage.agents=1, in-flight from ad-X is excluded, delta=1 → projected=2 ≤ 3 → ok
	if !ok {
		t.Error("canAdmit for the same AD key should not be blocked by its own reservation")
	}
}

// TestRelease_Concurrent verifies that concurrent Release calls on distinct keys
// are race-free and that all reservations are removed. Table-driven so we cover
// different concurrency fan-outs.
func TestRelease_Concurrent(t *testing.T) {
	tests := []struct {
		name    string
		numKeys int
	}{
		{"2 concurrent releases", 2},
		{"5 concurrent releases", 5},
		{"10 concurrent releases", 10},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnforcer(t)
			// Quota large enough to hold all initial reservations at once.
			q := makeQuota(int32(tc.numKeys*2), 0, int32(tc.numKeys*10), int32(tc.numKeys+1))
			usage := makeUsage(0, 0, 0)
			spec := makeSpec(1, "")

			// Create one reservation per key sequentially (no concurrency yet).
			keys := make([]string, tc.numKeys)
			for i := range keys {
				keys[i] = fmt.Sprintf("ns/ad-%d", i)
				ok, _ := e.AdmitAndReserve(keys[i], q, usage, spec, nil, 30*time.Second)
				if !ok {
					t.Fatalf("initial AdmitAndReserve(%q) failed unexpectedly", keys[i])
				}
			}

			// Release all reservations concurrently from a start barrier so
			// goroutines are likely to overlap rather than run sequentially.
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, k := range keys {
				k := k
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // wait until all goroutines are ready
					e.Release(k)
				}()
			}
			close(start) // release all goroutines simultaneously
			wg.Wait()

			// After all releases, each key's slot should be free: a fresh
			// canAdmit (zero usage, zero in-flight) must succeed for every key.
			for _, k := range keys {
				ok, reason := e.CanAdmit(k, q, usage, spec, nil)
				if !ok {
					t.Errorf("after Release, CanAdmit(%q) = false; reason: %q", k, reason)
				}
			}

			// Every reservation must be gone, not merely within headroom.
			e.mu.Lock()
			remaining := len(e.reservations)
			e.mu.Unlock()
			if remaining != 0 {
				t.Errorf("after concurrent Release, %d reservations remain; want 0", remaining)
			}
		})
	}
}

// TestAdmitAndReserve_AtomicRaceProtection verifies that concurrent calls to
// AdmitAndReserve for the same final quota slot allow exactly one through.
// This is the property that the separate CanAdmit+Reserve two-call pattern
// could not guarantee (TOCTOU race).
func TestAdmitAndReserve_AtomicRaceProtection(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	// Exactly 1 agent slot remaining.
	q := makeQuota(1, 0, 5, 5)
	usage := makeUsage(0, 0, 0)
	spec := makeSpec(1, "")

	var (
		successCount int64
		failCount    int64
		wg           sync.WaitGroup
	)
	// Start barrier: ensure both goroutines are scheduled before either calls
	// AdmitAndReserve, maximising the chance of a real concurrent execution.
	start := make(chan struct{})
	for _, key := range []string{"ns/ad-A", "ns/ad-B"} {
		key := key
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // wait until both goroutines are running
			ok, _ := e.AdmitAndReserve(key, q, usage, spec, nil, 5*time.Second)
			if ok {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&failCount, 1)
			}
		}()
	}
	close(start) // release both goroutines simultaneously
	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 AdmitAndReserve to succeed; got successCount=%d failCount=%d",
			successCount, failCount)
	}
}

// TestAdmitAndReserve_DenialLeavesNoReservation is a white-box test that
// verifies a denied admission does not pollute the in-flight map.
func TestAdmitAndReserve_DenialLeavesNoReservation(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	// Quota already exhausted: no remaining agents.
	q := makeQuota(1, 10, 10, 10)
	usage := makeUsage(1, 0, 0)
	spec := makeSpec(1, "")

	ok, reason := e.AdmitAndReserve("ns/ad-denied", q, usage, spec, nil, 5*time.Second)
	if ok {
		t.Errorf("AdmitAndReserve should have denied admission with exhausted quota, but it succeeded")
	}
	if reason == "" {
		t.Errorf("AdmitAndReserve denial should include a reason, got empty string")
	}

	// White-box check: under the mutex, verify no reservation was created.
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reservations) != 0 {
		t.Errorf("expected 0 reservations after denial; got %d entries: %v (denial reason: %s)",
			len(e.reservations), e.reservations, reason)
	}
	if _, exists := e.reservations["ns/ad-denied"]; exists {
		t.Errorf("denied key ns/ad-denied should not exist in reservations (denial reason: %s)", reason)
	}
}

// ── ComputeUsage tests ────────────────────────────────────────────────────────

// TestComputeUsage verifies aggregation of agents, GPUs, and replicas across multiple AgentDeployment specs.
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

// TestComputeUsage_Empty verifies that empty input returns all-zero usage.
func TestComputeUsage_Empty(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	got := e.ComputeUsage(nil)
	if got.UsedAgents != 0 || got.UsedGPUs != 0 || got.UsedTotalReplicas != 0 {
		t.Errorf("expected all-zero for empty input, got %+v", got)
	}
}

// ── IsOverQuota tests ─────────────────────────────────────────────────────────

// TestIsOverQuota verifies over-quota condition detection across agents, GPUs, and replicas dimensions.
func TestIsOverQuota(t *testing.T) {
	t.Parallel()
	e := newTestEnforcer(t)
	tests := []struct {
		name    string
		quota   agentraxv1alpha1.TenantQuotaSpec
		usage   agentraxv1alpha1.TenantQuotaStatus
		wantOQ  bool
		wantMsg string
	}{
		{"within limits", makeQuota(3, 4, 10, 3), makeUsage(2, 3, 8), false, ""},
		{"agents over", makeQuota(3, 4, 10, 3), makeUsage(4, 3, 8), true, "maxAgents"},
		{"GPUs over", makeQuota(3, 4, 10, 3), makeUsage(2, 5, 8), true, "maxGPUs"},
		{"replicas over", makeQuota(3, 4, 10, 3), makeUsage(2, 3, 11), true, "maxTotalReplicas"},
		{"zero GPU quota with used GPUs over", makeQuota(3, 0, 10, 3), makeUsage(2, 1, 8), true, "maxGPUs"},
		{"zero GPU quota with 0 used GPUs ok", makeQuota(3, 0, 10, 3), makeUsage(2, 0, 8), false, ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			over, msg := e.IsOverQuota(tc.quota, tc.usage)
			if over != tc.wantOQ {
				t.Errorf("IsOverQuota() = %v, want %v; msg=%q", over, tc.wantOQ, msg)
			}
			if tc.wantOQ && !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("IsOverQuota() msg %q does not contain %q", msg, tc.wantMsg)
			}
		})
	}
}

// ── ParseErrorRate tests ──────────────────────────────────────────────────────
// ParseErrorRate lives in api/v1alpha1 to avoid an import cycle.
// Its tests are in api/v1alpha1/webhook_test.go.

// TestParseErrorRate_ViaV1alpha1 verifies ParseErrorRate is accessible and correct from outside api/v1alpha1.
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

// absFloat returns the absolute value of a float64.
func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

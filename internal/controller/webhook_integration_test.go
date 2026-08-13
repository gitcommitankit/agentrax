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

// webhook_integration_test.go verifies Phase-2 admission-webhook behaviour
// end-to-end through the real envtest webhook server (not a fake client).
// The envtest manager started in suite_test.go already registers the webhook,
// so every k8sClient.Create / k8sClient.Update call here goes through the
// full validation and defaulting path.

import (
	"context"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// ── helpers local to this file ────────────────────────────────────────────────

// whCreateNS ensures the namespace exists; idempotent.
func whCreateNS(ctx context.Context, name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(),
		"ensuring namespace %s: %v", name, err)
}

// whCreateTQ creates a TenantQuota in the given namespace with the provided spec.
func whCreateTQ(ctx context.Context, name, namespace string, spec agentraxv1alpha1.TenantQuotaSpec) {
	tq := &agentraxv1alpha1.TenantQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	err := k8sClient.Create(ctx, tq)
	Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(),
		"creating TenantQuota %s/%s: %v", namespace, name, err)
}

// whMinimalAD returns an AgentDeployment with a valid spec for the given TQ.
func whMinimalAD(name, namespace, tenantRef string, maxReplicas int32) *agentraxv1alpha1.AgentDeployment {
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     testNginxImage,
			Port:      8080,
			TenantRef: tenantRef,
			Replicas: agentraxv1alpha1.ScalingPolicy{
				Min:    1,
				Max:    maxReplicas,
				Metric: "queueDepth",
				Target: 50,
			},
		},
	}
}

// whGPUAD returns an AgentDeployment that requests gpus GPU units per replica.
func whGPUAD(name, namespace, tenantRef string, gpus int64) *agentraxv1alpha1.AgentDeployment {
	ad := whMinimalAD(name, namespace, tenantRef, 1)
	ad.Spec.Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpus, resource.DecimalSI),
		},
	}
	return ad
}

// whCleanupAD deletes an AD (ignoring not-found) and strips finalizers if needed.
// Envtest does not run the GC controller, so the caller is responsible for
// cleaning up child resources separately if required.
func whCleanupAD(ctx context.Context, key types.NamespacedName) {
	ad := &agentraxv1alpha1.AgentDeployment{}
	if err := k8sClient.Get(ctx, key, ad); err != nil {
		return
	}
	// Strip finalizer so deletion is not blocked by the controller.
	if len(ad.Finalizers) > 0 {
		patch := ad.DeepCopy()
		patch.Finalizers = nil
		_ = k8sClient.Patch(ctx, patch, client.MergeFrom(ad))
	}
	_ = k8sClient.Delete(ctx, ad)
	Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, &agentraxv1alpha1.AgentDeployment{}))
	}, testTimeout, testInterval).Should(BeTrue(), "AD %s/%s should be fully gone", key.Namespace, key.Name)
}

// ── Webhook integration tests ─────────────────────────────────────────────────

var _ = Describe("Admission Webhook (integration)", func() {
	ctx := context.Background()

	// ── 1. Over-maxAgents rejection ──────────────────────────────────────────

	Describe("rejects AgentDeployment exceeding maxAgents", func() {
		const (
			ns     = "wh-agents"
			tqName = "tq-agents"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			// Allow exactly 1 agent.
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 1, MaxGPUs: 0, MaxTotalReplicas: 10, MaxReplicasPerAgent: 5,
			})
		})

		AfterEach(func() {
			whCleanupAD(ctx, namespacedName("ad-first", ns))
			deleteChildResources(namespacedName("ad-first", ns))
		})

		It("rejects a second create that would exceed maxAgents", func() {
			// First create must succeed (1 agent, within limit).
			ad1 := whMinimalAD("ad-first", ns, tqName, 1)
			Expect(k8sClient.Create(ctx, ad1)).To(Succeed())

			// Wait for the TenantQuota reconciler to reflect usedAgents=1.
			Eventually(func() int32 {
				tq := &agentraxv1alpha1.TenantQuota{}
				if err := k8sClient.Get(ctx, namespacedName(tqName, ns), tq); err != nil {
					return -1
				}
				return tq.Status.UsedAgents
			}, testTimeout, testInterval).Should(Equal(int32(1)))

			// Second create should be rejected by the webhook.
			ad2 := whMinimalAD("ad-second", ns, tqName, 1)
			err := k8sClient.Create(ctx, ad2)
			Expect(err).To(HaveOccurred(), "second create should be rejected (maxAgents=1 already used)")
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected 422 Invalid or 403 Forbidden, got: %v", err)
		})
	})

	// ── 2. Over-maxGPUs rejection ─────────────────────────────────────────────

	Describe("rejects AgentDeployment exceeding maxGPUs", func() {
		const (
			ns     = "wh-gpus"
			tqName = "tq-gpus"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			// Allow exactly 1 GPU total.
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 5, MaxGPUs: 1, MaxTotalReplicas: 10, MaxReplicasPerAgent: 5,
			})
		})

		AfterEach(func() {
			whCleanupAD(ctx, namespacedName("ad-gpu-ok", ns))
			deleteChildResources(namespacedName("ad-gpu-ok", ns))
		})

		It("rejects an AD that would exceed maxGPUs", func() {
			// First AD requests 1 GPU — within limit.
			ad1 := whGPUAD("ad-gpu-ok", ns, tqName, 1)
			Expect(k8sClient.Create(ctx, ad1)).To(Succeed())

			// Wait for TQ status to reflect 1 GPU used.
			Eventually(func() int32 {
				tq := &agentraxv1alpha1.TenantQuota{}
				if err := k8sClient.Get(ctx, namespacedName(tqName, ns), tq); err != nil {
					return -1
				}
				return tq.Status.UsedGPUs
			}, testTimeout, testInterval).Should(Equal(int32(1)))

			// Second AD requests 1 more GPU — must be rejected (total=2 > maxGPUs=1).
			ad2 := whGPUAD("ad-gpu-over", ns, tqName, 1)
			err := k8sClient.Create(ctx, ad2)
			Expect(err).To(HaveOccurred(), "should be rejected: over maxGPUs")
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue())
		})
	})

	// ── 3. Over-maxTotalReplicas rejection ───────────────────────────────────

	Describe("rejects AgentDeployment exceeding maxTotalReplicas", func() {
		const (
			ns     = "wh-total-rep"
			tqName = "tq-total-rep"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			// Allow max 3 total replicas.
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 5, MaxGPUs: 0, MaxTotalReplicas: 3, MaxReplicasPerAgent: 3,
			})
		})

		AfterEach(func() {
			whCleanupAD(ctx, namespacedName("ad-rep-ok", ns))
			deleteChildResources(namespacedName("ad-rep-ok", ns))
		})

		It("rejects an AD that would push total replicas past the ceiling", func() {
			// First AD uses max=3 — fills the budget exactly.
			ad1 := whMinimalAD("ad-rep-ok", ns, tqName, 3)
			Expect(k8sClient.Create(ctx, ad1)).To(Succeed())

			Eventually(func() int32 {
				tq := &agentraxv1alpha1.TenantQuota{}
				if err := k8sClient.Get(ctx, namespacedName(tqName, ns), tq); err != nil {
					return -1
				}
				return tq.Status.UsedTotalReplicas
			}, testTimeout, testInterval).Should(Equal(int32(3)))

			// Any new AD (even max=1) should be rejected.
			ad2 := whMinimalAD("ad-rep-over", ns, tqName, 1)
			err := k8sClient.Create(ctx, ad2)
			Expect(err).To(HaveOccurred(), "should be rejected: over maxTotalReplicas")
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue())
		})
	})

	// ── 4. Over-maxReplicasPerAgent rejection ────────────────────────────────

	Describe("rejects spec.replicas.max > maxReplicasPerAgent", func() {
		const (
			ns     = "wh-per-agent"
			tqName = "tq-per-agent"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 5, MaxGPUs: 0, MaxTotalReplicas: 50, MaxReplicasPerAgent: 2,
			})
		})

		It("rejects an AD with max replicas exceeding the per-agent ceiling", func() {
			// max=5 > maxReplicasPerAgent=2 — must be rejected at admission.
			ad := whMinimalAD("ad-per-agent-over", ns, tqName, 5)
			err := k8sClient.Create(ctx, ad)
			Expect(err).To(HaveOccurred(), "max > maxReplicasPerAgent should be rejected")
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue())
		})
	})

	// ── 5. Concurrent near-limit creates ─────────────────────────────────────
	// Two goroutines create simultaneously when only one slot remains.
	// Exactly one must succeed and one must be rejected via the in-flight
	// reservation mechanism in internal/quota.Enforcer.

	Describe("concurrent near-limit creates (race prevention)", func() {
		const (
			ns     = "wh-concurrent"
			tqName = "tq-concurrent"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			// Allow exactly 1 agent — the race window.
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 1, MaxGPUs: 0, MaxTotalReplicas: 10, MaxReplicasPerAgent: 5,
			})
		})

		AfterEach(func() {
			for _, name := range []string{"ad-race-a", "ad-race-b"} {
				whCleanupAD(ctx, namespacedName(name, ns))
				deleteChildResources(namespacedName(name, ns))
			}
		})

		It("allows exactly one of two simultaneous creates when only one slot remains", func() {
			var (
				successCount int64
				failCount    int64
				wg           sync.WaitGroup
			)

			// Start barrier: ensure both goroutines are scheduled before either
			// calls k8sClient.Create, maximising the chance of genuine concurrency
			// at the webhook admission layer.
			start := make(chan struct{})
			for _, name := range []string{"ad-race-a", "ad-race-b"} {
				name := name
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // wait until both goroutines are ready
					ad := whMinimalAD(name, ns, tqName, 1)
					if err := k8sClient.Create(ctx, ad); err == nil {
						atomic.AddInt64(&successCount, 1)
					} else {
						atomic.AddInt64(&failCount, 1)
					}
				}()
			}
			close(start) // release both goroutines simultaneously
			wg.Wait()

			Expect(successCount).To(Equal(int64(1)),
				"exactly one concurrent create should succeed (in-flight reservation protects the quota)")
			Expect(failCount).To(Equal(int64(1)),
				"exactly one concurrent create should be rejected by the webhook")
		})
	})

	// ── 6. TenantQuota status accuracy ───────────────────────────────────────

	Describe("TenantQuota status accurately reflects live usage", func() {
		const (
			ns     = "wh-tq-status"
			tqName = "tq-status"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 5, MaxGPUs: 0, MaxTotalReplicas: 20, MaxReplicasPerAgent: 5,
			})
		})

		It("increments usedAgents on create and decrements after deletion", func() {
			key := types.NamespacedName{Name: "ad-tq-track", Namespace: ns}
			ad := whMinimalAD(key.Name, ns, tqName, 2)
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			// UsedAgents must reach 1 and UsedTotalReplicas must reach 2.
			Eventually(func() agentraxv1alpha1.TenantQuotaStatus {
				tq := &agentraxv1alpha1.TenantQuota{}
				_ = k8sClient.Get(ctx, namespacedName(tqName, ns), tq)
				return tq.Status
			}, testTimeout, testInterval).Should(And(
				WithTransform(func(s agentraxv1alpha1.TenantQuotaStatus) int32 { return s.UsedAgents },
					Equal(int32(1))),
				WithTransform(func(s agentraxv1alpha1.TenantQuotaStatus) int32 { return s.UsedTotalReplicas },
					Equal(int32(2))),
			), "TQ status should reflect the newly created AD")

			// Delete the AD; status should drop back to 0.
			whCleanupAD(ctx, key)
			deleteChildResources(key)

			Eventually(func() int32 {
				tq := &agentraxv1alpha1.TenantQuota{}
				_ = k8sClient.Get(ctx, namespacedName(tqName, ns), tq)
				return tq.Status.UsedAgents
			}, testTimeout, testInterval).Should(Equal(int32(0)),
				"usedAgents should drop to 0 after the AD is deleted")
		})
	})

	// ── 7. Mutating webhook defaults ─────────────────────────────────────────

	Describe("mutating webhook applies defaults", func() {
		const (
			ns     = "wh-defaults"
			tqName = "tq-defaults"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 5, MaxGPUs: 0, MaxTotalReplicas: 20, MaxReplicasPerAgent: 5,
			})
		})

		AfterEach(func() {
			whCleanupAD(ctx, namespacedName("ad-defaults", ns))
			deleteChildResources(namespacedName("ad-defaults", ns))
		})

		It("defaults port=8080, strategy=Recreate, and resources when omitted", func() {
			// Create with only the required fields — no port, no rollout, no resources.
			ad := &agentraxv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: "ad-defaults", Namespace: ns},
				Spec: agentraxv1alpha1.AgentDeploymentSpec{
					Image:     testNginxImage,
					TenantRef: tqName,
					Replicas: agentraxv1alpha1.ScalingPolicy{
						Min: 1, Max: 2, Metric: "queueDepth", Target: 50,
					},
				},
			}
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			// Re-read to see server-side-applied defaults.
			got := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName("ad-defaults", ns), got)).To(Succeed())

			Expect(got.Spec.Port).To(Equal(int32(8080)), "port should be defaulted to 8080")
			Expect(got.Spec.Rollout.Strategy).To(Equal("Recreate"),
				"strategy should be defaulted to Recreate")
			Expect(got.Spec.Resources.Requests).NotTo(BeEmpty(),
				"resources.requests should be defaulted")
			Expect(got.Spec.Resources.Limits).NotTo(BeEmpty(),
				"resources.limits should be defaulted")

			// Verify the specific CPU default (100m request).
			cpuReq := got.Spec.Resources.Requests[corev1.ResourceCPU]
			Expect(cpuReq.Cmp(resource.MustParse("100m"))).To(Equal(0),
				"default CPU request should be 100m, got %s", cpuReq.String())
		})
	})

	// ── 8. OverQuota condition set when quota is lowered below usage ──────────

	Describe("OverQuota condition when quota is lowered below existing usage", func() {
		const (
			ns     = "wh-overquota"
			tqName = "tq-overquota"
		)

		BeforeEach(func() {
			whCreateNS(ctx, ns)
			// Start with generous quota.
			whCreateTQ(ctx, tqName, ns, agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents: 3, MaxGPUs: 0, MaxTotalReplicas: 15, MaxReplicasPerAgent: 5,
			})
		})

		AfterEach(func() {
			for _, name := range []string{"ad-oq-1", "ad-oq-2"} {
				whCleanupAD(ctx, namespacedName(name, ns))
				deleteChildResources(namespacedName(name, ns))
			}
		})

		It("sets OverQuota condition without deleting existing ADs", func() {
			// Create two agents (both succeed under the generous quota).
			Expect(k8sClient.Create(ctx, whMinimalAD("ad-oq-1", ns, tqName, 1))).To(Succeed())
			Expect(k8sClient.Create(ctx, whMinimalAD("ad-oq-2", ns, tqName, 1))).To(Succeed())

			// Wait for both to be reflected in TQ status.
			Eventually(func() int32 {
				tq := &agentraxv1alpha1.TenantQuota{}
				_ = k8sClient.Get(ctx, namespacedName(tqName, ns), tq)
				return tq.Status.UsedAgents
			}, testTimeout, testInterval).Should(Equal(int32(2)))

			// Lower maxAgents to 1 (below current usage of 2).
			tq := &agentraxv1alpha1.TenantQuota{}
			Expect(k8sClient.Get(ctx, namespacedName(tqName, ns), tq)).To(Succeed())
			patched := tq.DeepCopy()
			patched.Spec.MaxAgents = 1
			Expect(k8sClient.Patch(ctx, patched, client.MergeFrom(tq))).To(Succeed())

			// Expect the OverQuota condition to be set.
			Eventually(func() bool {
				latest := &agentraxv1alpha1.TenantQuota{}
				if err := k8sClient.Get(ctx, namespacedName(tqName, ns), latest); err != nil {
					return false
				}
				for _, c := range latest.Status.Conditions {
					if c.Type == agentraxv1alpha1.ConditionOverQuota &&
						c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, testTimeout, testInterval).Should(BeTrue(),
				"OverQuota condition should be set after quota is lowered below usage")

			// Both ADs must still exist — no forced deletions.
			Expect(k8sClient.Get(ctx, namespacedName("ad-oq-1", ns),
				&agentraxv1alpha1.AgentDeployment{})).To(Succeed())
			Expect(k8sClient.Get(ctx, namespacedName("ad-oq-2", ns),
				&agentraxv1alpha1.AgentDeployment{})).To(Succeed())
		})
	})
})

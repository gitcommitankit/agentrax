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
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

var _ = Describe("TenantQuota Controller", func() {
	// tqNS holds the namespace for this test group. Each Describe block uses a
	// unique namespace suffix to avoid cross-test interference.
	const tqNS = "tq-ctrl-test"

	ctx := context.Background()

	// ── Setup / teardown ──────────────────────────────────────────────────────

	BeforeEach(func() {
		By("ensuring the test namespace exists")
		ns := namespaceObject(tqNS)
		err := k8sClient.Create(ctx, ns)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		By("deleting all AgentDeployments in the test namespace")
		adList := &agentraxv1alpha1.AgentDeploymentList{}
		Expect(k8sClient.List(ctx, adList, inNamespace(tqNS))).To(Succeed())
		for i := range adList.Items {
			ad := &adList.Items[i]
			// Remove finalizer so deletion is not blocked by the controller.
			_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &agentraxv1alpha1.AgentDeployment{}
				if err := k8sClient.Get(ctx, namespacedName(ad.Name, tqNS), latest); err != nil {
					return client.IgnoreNotFound(err)
				}
				latest.Finalizers = nil
				return k8sClient.Update(ctx, latest)
			})
			if err := k8sClient.Delete(ctx, ad); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred(), "deleting AD %s", ad.Name)
			}
			// Wait until the API server confirms the object is gone.
			Eventually(func() bool {
				err := k8sClient.Get(ctx, namespacedName(ad.Name, tqNS), &agentraxv1alpha1.AgentDeployment{})
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue(), "AD %s should be fully deleted", ad.Name)
		}

		By("deleting all TenantQuotas in the test namespace")
		tqList := &agentraxv1alpha1.TenantQuotaList{}
		Expect(k8sClient.List(ctx, tqList, inNamespace(tqNS))).To(Succeed())
		for i := range tqList.Items {
			tq := &tqList.Items[i]
			if err := k8sClient.Delete(ctx, tq); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred(), "deleting TQ %s", tq.Name)
			}
		}
	})

	// ── Status accuracy ───────────────────────────────────────────────────────

	Describe("status accuracy", func() {
		It("reflects zero usage when no AgentDeployments exist", func() {
			tq := makeTQ("tq-empty", tqNS, 6, 4, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-empty", tqNS), fetched)).To(Succeed())
				// Status may not have been written yet if reconcile hasn't run.
				// usedAgents == 0 is the expected steady state.
				g.Expect(fetched.Status.UsedAgents).To(BeNumerically("==", 0))
				g.Expect(fetched.Status.UsedTotalReplicas).To(BeNumerically("==", 0))
			}, timeout, interval).Should(Succeed())
		})

		It("increments usedAgents when an AgentDeployment is created (bypassing webhook)", func() {
			// We bypass the webhook by directly patching status / using the k8sClient
			// with pre-created fixtures. The webhook integration test below covers
			// admission-path increments.
			tq := makeTQ("tq-count", tqNS, 6, 4, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			// Create an AD via the webhook path (webhook is active in this suite).
			ad := makeBasicAD("ad-count-1", tqNS, "tq-count", 2)
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-count", tqNS), fetched)).To(Succeed())
				g.Expect(fetched.Status.UsedAgents).To(BeNumerically("==", 1))
				g.Expect(fetched.Status.UsedTotalReplicas).To(BeNumerically("==", 2))
			}, timeout, interval).Should(Succeed())
		})

		It("decrements usedAgents when an AgentDeployment is deleted", func() {
			tq := makeTQ("tq-del", tqNS, 6, 4, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			ad := makeBasicAD("ad-del-1", tqNS, "tq-del", 2)
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			// Wait for status to show 1 agent.
			Eventually(func(g Gomega) {
				fetched := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-del", tqNS), fetched)).To(Succeed())
				g.Expect(fetched.Status.UsedAgents).To(BeNumerically("==", 1))
			}, timeout, interval).Should(Succeed())

			// Remove finalizer so we can delete immediately.
			fetched := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName("ad-del-1", tqNS), fetched)).To(Succeed())
			fetched.Finalizers = nil
			Expect(k8sClient.Update(ctx, fetched)).To(Succeed())
			Expect(k8sClient.Delete(ctx, fetched)).To(Succeed())

			Eventually(func(g Gomega) {
				tqFetched := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-del", tqNS), tqFetched)).To(Succeed())
				g.Expect(tqFetched.Status.UsedAgents).To(BeNumerically("==", 0))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ── OverQuota condition ───────────────────────────────────────────────────

	Describe("OverQuota condition", func() {
		It("sets OverQuota condition when quota is lowered below current usage", func() {
			tq := makeTQ("tq-overq", tqNS, 6, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			// Create 2 ADs within the original quota of 6.
			for i := 1; i <= 2; i++ {
				ad := makeBasicAD(fmt.Sprintf("ad-overq-%d", i), tqNS, "tq-overq", 2)
				Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			}

			// Wait for status to reflect 2 agents.
			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-overq", tqNS), f)).To(Succeed())
				g.Expect(f.Status.UsedAgents).To(BeNumerically("==", 2))
			}, timeout, interval).Should(Succeed())

			// Lower maxAgents to 1 while 2 exist.
			tqFetched := &agentraxv1alpha1.TenantQuota{}
			Expect(k8sClient.Get(ctx, namespacedName("tq-overq", tqNS), tqFetched)).To(Succeed())
			tqFetched.Spec.MaxAgents = 1
			Expect(k8sClient.Update(ctx, tqFetched)).To(Succeed())

			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-overq", tqNS), f)).To(Succeed())
				cond := apimeta.FindStatusCondition(f.Status.Conditions, agentraxv1alpha1.ConditionOverQuota)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, timeout, interval).Should(Succeed())
		})

		It("clears OverQuota condition when usage returns to within quota", func() {
			tq := makeTQ("tq-clearoq", tqNS, 2, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			// Create 2 ADs (at the limit).
			for i := 1; i <= 2; i++ {
				ad := makeBasicAD(fmt.Sprintf("ad-clearoq-%d", i), tqNS, "tq-clearoq", 2)
				Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			}

			// Lower maxAgents to 1 → OverQuota.
			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-clearoq", tqNS), f)).To(Succeed())
				g.Expect(f.Status.UsedAgents).To(BeNumerically("==", 2))
			}, timeout, interval).Should(Succeed())

			tqFetched := &agentraxv1alpha1.TenantQuota{}
			Expect(k8sClient.Get(ctx, namespacedName("tq-clearoq", tqNS), tqFetched)).To(Succeed())
			tqFetched.Spec.MaxAgents = 1
			Expect(k8sClient.Update(ctx, tqFetched)).To(Succeed())

			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-clearoq", tqNS), f)).To(Succeed())
				cond := apimeta.FindStatusCondition(f.Status.Conditions, agentraxv1alpha1.ConditionOverQuota)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, timeout, interval).Should(Succeed())

			// Delete one AD → usage falls back to 1 == maxAgents; OverQuota clears.
			ad1 := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName("ad-clearoq-1", tqNS), ad1)).To(Succeed())
			ad1.Finalizers = nil
			Expect(k8sClient.Update(ctx, ad1)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ad1)).To(Succeed())

			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-clearoq", tqNS), f)).To(Succeed())
				cond := apimeta.FindStatusCondition(f.Status.Conditions, agentraxv1alpha1.ConditionOverQuota)
				// The reconciler calls RemoveStatusCondition, so the condition
				// must be fully absent once usage normalises.
				g.Expect(cond).To(BeNil())
				// Usage counters must reflect the one remaining AD (maxReplicas=2).
				g.Expect(f.Status.UsedAgents).To(BeNumerically("==", 1))
				g.Expect(f.Status.UsedTotalReplicas).To(BeNumerically("==", 2))
			}, timeout, interval).Should(Succeed())
		})

		It("never forcibly deletes existing ADs when quota is lowered", func() {
			tq := makeTQ("tq-nodel", tqNS, 4, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			for i := 1; i <= 3; i++ {
				ad := makeBasicAD(fmt.Sprintf("ad-nodel-%d", i), tqNS, "tq-nodel", 2)
				Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			}

			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-nodel", tqNS), f)).To(Succeed())
				g.Expect(f.Status.UsedAgents).To(BeNumerically("==", 3))
			}, timeout, interval).Should(Succeed())

			// Lower quota to 1.
			tqFetched := &agentraxv1alpha1.TenantQuota{}
			Expect(k8sClient.Get(ctx, namespacedName("tq-nodel", tqNS), tqFetched)).To(Succeed())
			tqFetched.Spec.MaxAgents = 1
			Expect(k8sClient.Update(ctx, tqFetched)).To(Succeed())

			// Wait for OverQuota to be set.
			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-nodel", tqNS), f)).To(Succeed())
				cond := apimeta.FindStatusCondition(f.Status.Conditions, agentraxv1alpha1.ConditionOverQuota)
				g.Expect(cond).NotTo(BeNil())
			}, timeout, interval).Should(Succeed())

			// All 3 ADs must still exist — no forced deletion.
			adList := &agentraxv1alpha1.AgentDeploymentList{}
			Expect(k8sClient.List(ctx, adList, inNamespace(tqNS))).To(Succeed())
			Expect(adList.Items).To(HaveLen(3))
		})
	})

	// ── Webhook quota rejection ───────────────────────────────────────────────

	Describe("webhook quota rejection", func() {
		It("rejects an AgentDeployment that would exceed maxAgents", func() {
			tq := makeTQ("tq-reject", tqNS, 1, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			// First AD is within quota → admitted.
			ad1 := makeBasicAD("ad-reject-1", tqNS, "tq-reject", 2)
			Expect(k8sClient.Create(ctx, ad1)).To(Succeed())

			// Wait for the reconciler to commit ad1's usage into TQ status before
			// testing ad2. Without this, ad2 may be tried while the in-flight
			// reservation is still live and the quota arithmetic has not yet been
			// persisted, causing the rejection to rely solely on the reservation
			// TTL which may have already expired.
			Eventually(func(g Gomega) {
				f := &agentraxv1alpha1.TenantQuota{}
				g.Expect(k8sClient.Get(ctx, namespacedName("tq-reject", tqNS), f)).To(Succeed())
				g.Expect(f.Status.UsedAgents).To(BeNumerically("==", 1))
			}, timeout, interval).Should(Succeed())

			// Second AD would push usedAgents to 2 > maxAgents=1 → rejected.
			ad2 := makeBasicAD("ad-reject-2", tqNS, "tq-reject", 2)
			err := k8sClient.Create(ctx, ad2)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected Invalid or Forbidden error, got: %v", err)
		})

		It("rejects an AgentDeployment where replicas.max > maxReplicasPerAgent", func() {
			tq := makeTQ("tq-reject-rpa", tqNS, 6, 0, 12, 3)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			// replicas.max=4 > maxReplicasPerAgent=3 → rejected.
			ad := makeBasicAD("ad-reject-rpa", tqNS, "tq-reject-rpa", 4)
			err := k8sClient.Create(ctx, ad)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected Invalid or Forbidden error, got: %v", err)
		})

		It("rejects an AgentDeployment that references a non-existent TenantQuota", func() {
			ad := makeBasicAD("ad-no-tq", tqNS, "no-such-tq", 2)
			err := k8sClient.Create(ctx, ad)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected Invalid or Forbidden error, got: %v", err)
		})
	})

	// ── Mutating webhook defaults ─────────────────────────────────────────────

	Describe("mutating webhook defaults", func() {
		It("defaults port to 8080 when omitted", func() {
			tq := makeTQ("tq-defaults", tqNS, 6, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			ad := makeBasicAD("ad-defaults", tqNS, "tq-defaults", 2)
			ad.Spec.Port = 0 // explicitly unset
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			fetched := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName("ad-defaults", tqNS), fetched)).To(Succeed())
			Expect(fetched.Spec.Port).To(BeNumerically("==", 8080))
		})

		It("defaults rollout.strategy to Recreate when omitted", func() {
			tq := makeTQ("tq-strategy-def", tqNS, 6, 0, 12, 6)
			Expect(k8sClient.Create(ctx, tq)).To(Succeed())

			ad := makeBasicAD("ad-strategy-def", tqNS, "tq-strategy-def", 2)
			ad.Spec.Rollout.Strategy = ""
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())

			fetched := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName("ad-strategy-def", tqNS), fetched)).To(Succeed())
			Expect(fetched.Spec.Rollout.Strategy).To(Equal("Recreate"))
		})
	})
})

// ── Test helpers ──────────────────────────────────────────────────────────────

// makeTQ creates a TenantQuota object with the provided limits for test fixtures.
func makeTQ(name, ns string, maxAgents, maxGPUs, maxTotalReplicas, maxReplicasPerAgent int32) *agentraxv1alpha1.TenantQuota { //nolint:unparam
	return &agentraxv1alpha1.TenantQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentraxv1alpha1.TenantQuotaSpec{
			MaxAgents:           maxAgents,
			MaxGPUs:             maxGPUs,
			MaxTotalReplicas:    maxTotalReplicas,
			MaxReplicasPerAgent: maxReplicasPerAgent,
		},
	}
}

// makeBasicAD builds a minimal AgentDeployment suitable for TQ reconciler tests.
// It uses a real image name that won't pull (but pod scheduling isn't needed here).
func makeBasicAD(name, ns, tenantRef string, maxReplicas int32) *agentraxv1alpha1.AgentDeployment { //nolint:unparam
	return &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: agentraxv1alpha1.AgentDeploymentSpec{
			Image:     "test-image:v1",
			TenantRef: tenantRef,
			Port:      8080,
			Replicas: agentraxv1alpha1.ScalingPolicy{
				Min:    1,
				Max:    maxReplicas,
				Metric: "queueDepth",
				Target: 50,
			},
			Rollout: agentraxv1alpha1.RolloutPolicy{Strategy: "Recreate"},
		},
	}
}

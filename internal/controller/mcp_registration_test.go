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
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

var _ = Describe("MCP Registration Integration", func() {
	const (
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	var (
		ns string
		tq *agentraxv1alpha1.TenantQuota
	)

	BeforeEach(func() {
		testMockMCP.SetError(nil)
		testMockMCP.SetTools([]string{"search", "calculator"})

		ns = fmt.Sprintf("tenant-mcp-%d", time.Now().UnixNano())
		namespaceObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, namespaceObj)).To(Succeed())

		tq = &agentraxv1alpha1.TenantQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "quota-mcp",
				Namespace: ns,
			},
			Spec: agentraxv1alpha1.TenantQuotaSpec{
				MaxAgents:           10,
				MaxTotalReplicas:    20,
				MaxReplicasPerAgent: 5,
				MaxGPUs:             8,
			},
		}
		Expect(k8sClient.Create(ctx, tq)).To(Succeed())
	})

	AfterEach(func() {
		testMockMCP.SetError(nil)
	})

	It("registers an AgentDeployment with mcp.expose: true upon reaching Running phase", func() {
		adName := "mcp-agent-basic"
		ad := &agentraxv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      adName,
				Namespace: ns,
			},
			Spec: agentraxv1alpha1.AgentDeploymentSpec{
				Image:     "nginx:latest",
				Port:      8080,
				TenantRef: tq.Name,
				Replicas: agentraxv1alpha1.ScalingPolicy{
					Min:    1,
					Max:    3,
					Metric: "queueDepth",
					Target: 10,
				},
				MCP: agentraxv1alpha1.MCPConfig{
					Expose: true,
					Tools:  []string{"customTool"},
				},
			},
		}

		Expect(k8sClient.Create(ctx, ad)).To(Succeed())

		// Simulate Deployment pods becoming ready
		depKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, depKey, dep); err != nil {
				return err
			}
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, dep)
		}, timeout, interval).Should(Succeed())

		// Verify AgentDeployment reaches Running phase and Registered=true
		adKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func(g Gomega) {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			g.Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.Phase).To(Equal(agentraxv1alpha1.PhaseRunning))
			g.Expect(fetched.Status.Registered).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		// Verify entry in testRegistry
		entry, ok := testRegistry.Get(ns, adName)
		Expect(ok).To(BeTrue())
		Expect(entry.Endpoint).To(Equal(fmt.Sprintf("http://%s.%s.svc:8080", adName, ns)))
		Expect(entry.Tools).To(ContainElements("search", "calculator", "customTool"))
	})

	It("sets MCPHandshakeFailed condition when MCP initialize fails", func() {
		testMockMCP.SetError(errors.New("connection refused"))

		adName := "mcp-agent-fail"
		ad := &agentraxv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      adName,
				Namespace: ns,
			},
			Spec: agentraxv1alpha1.AgentDeploymentSpec{
				Image:     "nginx:latest",
				Port:      8080,
				TenantRef: tq.Name,
				Replicas: agentraxv1alpha1.ScalingPolicy{
					Min:    1,
					Max:    3,
					Metric: "queueDepth",
					Target: 10,
				},
				MCP: agentraxv1alpha1.MCPConfig{
					Expose: true,
				},
			},
		}

		Expect(k8sClient.Create(ctx, ad)).To(Succeed())

		// Simulate Deployment pods becoming ready
		depKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, depKey, dep); err != nil {
				return err
			}
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, dep)
		}, timeout, interval).Should(Succeed())

		// Verify MCPHandshakeFailed condition is set and Registered=false
		adKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func(g Gomega) {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			g.Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.Phase).To(Equal(agentraxv1alpha1.PhaseRunning))
			g.Expect(fetched.Status.Registered).To(BeFalse())

			cond := GetCondition(fetched, agentraxv1alpha1.ConditionMCPHandshakeFailed)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("HandshakeFailed"))
		}, timeout, interval).Should(Succeed())

		// Verify NOT in registry
		_, ok := testRegistry.Get(ns, adName)
		Expect(ok).To(BeFalse())
	})

	It("deregisters when spec.mcp.expose is toggled to false", func() {
		adName := "mcp-agent-toggle"
		ad := &agentraxv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      adName,
				Namespace: ns,
			},
			Spec: agentraxv1alpha1.AgentDeploymentSpec{
				Image:     "nginx:latest",
				Port:      8080,
				TenantRef: tq.Name,
				Replicas: agentraxv1alpha1.ScalingPolicy{
					Min:    1,
					Max:    3,
					Metric: "queueDepth",
					Target: 10,
				},
				MCP: agentraxv1alpha1.MCPConfig{
					Expose: true,
				},
			},
		}

		Expect(k8sClient.Create(ctx, ad)).To(Succeed())

		// Simulate Deployment ready
		depKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, depKey, dep); err != nil {
				return err
			}
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, dep)
		}, timeout, interval).Should(Succeed())

		adKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func(g Gomega) {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			g.Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.Registered).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		// Toggle expose to false
		Eventually(func() error {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			if err := k8sClient.Get(ctx, adKey, fetched); err != nil {
				return err
			}
			fetched.Spec.MCP.Expose = false
			return k8sClient.Update(ctx, fetched)
		}, timeout, interval).Should(Succeed())

		// Verify Registered becomes false and removed from registry
		Eventually(func(g Gomega) {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			g.Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.Registered).To(BeFalse())
		}, timeout, interval).Should(Succeed())

		_, ok := testRegistry.Get(ns, adName)
		Expect(ok).To(BeFalse())
	})

	It("deregisters before finalizer is removed on deletion", func() {
		adName := "mcp-agent-delete"
		ad := &agentraxv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      adName,
				Namespace: ns,
			},
			Spec: agentraxv1alpha1.AgentDeploymentSpec{
				Image:     "nginx:latest",
				Port:      8080,
				TenantRef: tq.Name,
				Replicas: agentraxv1alpha1.ScalingPolicy{
					Min:    1,
					Max:    3,
					Metric: "queueDepth",
					Target: 10,
				},
				MCP: agentraxv1alpha1.MCPConfig{
					Expose: true,
				},
			},
		}

		Expect(k8sClient.Create(ctx, ad)).To(Succeed())

		// Simulate Deployment ready
		depKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, depKey, dep); err != nil {
				return err
			}
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			return k8sClient.Status().Update(ctx, dep)
		}, timeout, interval).Should(Succeed())

		adKey := types.NamespacedName{Name: adName, Namespace: ns}
		Eventually(func(g Gomega) {
			fetched := &agentraxv1alpha1.AgentDeployment{}
			g.Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())
			g.Expect(fetched.Status.Registered).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		// Install a mock Registrar that verifies Service still exists during Deregister.
		var serviceExistedDuringDeregister atomic.Bool
		origRegistrar := testReconciler.Registrar
		testReconciler.Registrar = &mockAgentRegistrar{
			deregisterFn: func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
				svc := &corev1.Service{}
				svcKey := types.NamespacedName{Name: adName, Namespace: ns}
				err := k8sClient.Get(ctx, svcKey, svc)
				serviceExistedDuringDeregister.Store(err == nil)
				return testRegistrar.Deregister(ctx, ad)
			},
		}
		defer func() { testReconciler.Registrar = origRegistrar }()

		// Delete the AgentDeployment
		fetched := &agentraxv1alpha1.AgentDeployment{}
		Expect(k8sClient.Get(ctx, adKey, fetched)).To(Succeed())

		// Verify child Service has controlling owner reference matching AgentDeployment UID
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: adName, Namespace: ns}, svc)).To(Succeed())
		ownerRef := metav1.GetControllerOf(svc)
		Expect(ownerRef).NotTo(BeNil())
		Expect(ownerRef.UID).To(Equal(fetched.UID))

		Expect(k8sClient.Delete(ctx, fetched)).To(Succeed())

		// Wait for object to be completely removed
		Eventually(func() bool {
			err := k8sClient.Get(ctx, adKey, &agentraxv1alpha1.AgentDeployment{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())

		// Verify deregistration occurred while Service was present
		Expect(serviceExistedDuringDeregister.Load()).To(BeTrue(), "Service should exist during deregistration")

		// Entry must be gone from registry
		_, ok := testRegistry.Get(ns, adName)
		Expect(ok).To(BeFalse())
	})
})

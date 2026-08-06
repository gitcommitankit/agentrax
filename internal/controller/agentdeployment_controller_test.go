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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

const (
	// testTimeout is the maximum time any Eventually assertion waits.
	testTimeout = 30 * time.Second
	// testInterval is how often Eventually polls.
	testInterval = 250 * time.Millisecond
	// testNginxImage is the standard container image used in integration tests.
	testNginxImage = "nginx:latest"
)

// createAgentDeployment is a test helper that creates a minimal AgentDeployment
// and returns its NamespacedName.
func createAgentDeployment(name, namespace, image string, port, minReplicas int32) types.NamespacedName {
	ad := &agentraxv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
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
	Expect(k8sClient.Create(ctx, ad)).To(Succeed())
	return types.NamespacedName{Name: name, Namespace: namespace}
}

// deleteAgentDeployment deletes an AgentDeployment and waits for it to be gone.
func deleteAgentDeployment(key types.NamespacedName) {
	ad := &agentraxv1alpha1.AgentDeployment{}
	if err := k8sClient.Get(ctx, key, ad); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(k8sClient.Delete(ctx, ad)).To(Succeed())

	// Wait for the object to be fully deleted (finalizer removed).
	Eventually(func() bool {
		err := k8sClient.Get(ctx, key, &agentraxv1alpha1.AgentDeployment{})
		return apierrors.IsNotFound(err)
	}, testTimeout, testInterval).Should(BeTrue(), "AgentDeployment should be fully deleted")
}

// deleteChildResources explicitly deletes child Deployment, Service, and
// ServiceMonitor objects with the given key.
// Envtest does not run the Kubernetes GC controller, so owner-reference-based
// cascading deletion never fires; tests that share a namespace must clean up
// children themselves to prevent stale owner UIDs from bleeding into sibling
// specs.
func deleteChildResources(key types.NamespacedName) {
	dep := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx, key, dep); err == nil {
		_ = k8sClient.Delete(ctx, dep)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &appsv1.Deployment{}))
		}, testTimeout, testInterval).Should(BeTrue(), "child Deployment should be deleted")
	}

	svc := &corev1.Service{}
	if err := k8sClient.Get(ctx, key, svc); err == nil {
		_ = k8sClient.Delete(ctx, svc)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Service{}))
		}, testTimeout, testInterval).Should(BeTrue(), "child Service should be deleted")
	}

	sm := &monitoringv1.ServiceMonitor{}
	if err := k8sClient.Get(ctx, key, sm); err == nil {
		_ = k8sClient.Delete(ctx, sm)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &monitoringv1.ServiceMonitor{}))
		}, testTimeout, testInterval).Should(BeTrue(), "child ServiceMonitor should be deleted")
	}
}

var _ = Describe("AgentDeployment Controller", func() {

	// Each Describe block uses a unique namespace to avoid cross-test interference.

	Describe("Finalizer lifecycle", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-finalizer"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-finalizer: %v", err)
			key = createAgentDeployment("ad-finalizer", "test-finalizer", testNginxImage, 8080, 1)
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("adds the finalizer on creation", func() {
			ad := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, ad); err != nil {
					return false
				}
				for _, f := range ad.Finalizers {
					if f == agentraxv1alpha1.AgentDeploymentFinalizer {
						return true
					}
				}
				return false
			}, testTimeout, testInterval).Should(BeTrue(), "finalizer should be added")
		})

		It("removes the finalizer on deletion, allowing the object to be fully deleted", func() {
			// Ensure finalizer is present first.
			ad := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, ad); err != nil {
					return false
				}
				for _, f := range ad.Finalizers {
					if f == agentraxv1alpha1.AgentDeploymentFinalizer {
						return true
					}
				}
				return false
			}, testTimeout, testInterval).Should(BeTrue())

			// Wait for the child Service to exist before we delete the parent.
			Eventually(func() error {
				return k8sClient.Get(ctx, key, &corev1.Service{})
			}, testTimeout, testInterval).Should(Succeed(), "child Service should exist before deletion")

			// Inject a Deregister hook. A buffered channel is used so the hook
			// (called on the reconciler goroutine) can pass its observation to the
			// test goroutine without a data race on plain booleans.
			resultCh := make(chan bool, 1)
			testReconciler.SetDeregister(func(hctx context.Context, had *agentraxv1alpha1.AgentDeployment) error {
				err := k8sClient.Get(hctx, key, &corev1.Service{})
				resultCh <- (err == nil)
				return nil
			})
			DeferCleanup(func() { testReconciler.SetDeregister(nil) })

			// Delete the object — the reconciler must call Deregister, then remove the finalizer.
			Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
			Eventually(func() bool {
				err := k8sClient.Get(ctx, key, &agentraxv1alpha1.AgentDeployment{})
				return apierrors.IsNotFound(err)
			}, testTimeout, testInterval).Should(BeTrue(), "object should be gone once finalizer removed")

			// Receive the hook result. By the time the AD is fully deleted the hook
			// has already sent, so this receive never blocks.
			var serviceExistedDuringDeregister bool
			Eventually(resultCh, testTimeout, testInterval).Should(Receive(&serviceExistedDuringDeregister))

			// Assert deregistration happened while the Service was still alive.
			Expect(serviceExistedDuringDeregister).To(BeTrue(), "Service should exist during deregistration (before GC)")

			// NOTE: envtest does not run the Kubernetes garbage-collection controller,
			// so owner-reference-based cascading deletion of the child Service cannot
			// be verified here. The ordering invariant above (deregistration runs while
			// the Service is still alive) is the critical correctness property. GC
			// ordering is covered by a real-cluster e2e test in Phase 6.
		})
	})

	Describe("Child resource creation", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-children"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-children: %v", err)
			key = createAgentDeployment("ad-children", "test-children", testNginxImage, 9090, 1)
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
			// Explicitly delete child resources; envtest does not run the GC
			// controller, so owner-reference cascading deletion never fires.
			deleteChildResources(key)
		})

		It("creates a Deployment with correct image and port", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed(), "Deployment should be created")

			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(testNginxImage))
			Expect(dep.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(9090)))
		})

		It("creates a Deployment with owner reference pointing to the AgentDeployment", func() {
			// Fetch parent and child together inside Eventually so we wait for the
			// reconciler to update the owner reference when the namespace is reused
			// across test runs (envtest does not GC child objects between runs).
			dep := &appsv1.Deployment{}
			parent := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, parent); err != nil {
					return false
				}
				if err := k8sClient.Get(ctx, key, dep); err != nil {
					return false
				}
				if len(dep.OwnerReferences) != 1 {
					return false
				}
				return dep.OwnerReferences[0].UID == parent.UID
			}, testTimeout, testInterval).Should(BeTrue(), "Deployment owner UID should converge to parent UID")

			Expect(dep.OwnerReferences[0].Name).To(Equal(key.Name))
			Expect(dep.OwnerReferences[0].Kind).To(Equal("AgentDeployment"))
			Expect(dep.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*dep.OwnerReferences[0].Controller).To(BeTrue(), "owner reference must have Controller=true")
		})

		It("creates a Service targeting the correct port", func() {
			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, svc)
			}, testTimeout, testInterval).Should(Succeed(), "Service should be created")

			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9090)))
			Expect(svc.Spec.Selector["app.kubernetes.io/name"]).To(Equal(key.Name))
		})

		It("creates a Service with owner reference pointing to the AgentDeployment", func() {
			// Fetch parent and child together inside Eventually so we wait for the
			// reconciler to update the owner reference when the namespace is reused
			// across test runs (envtest does not GC child objects between runs).
			svc := &corev1.Service{}
			parent := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, parent); err != nil {
					return false
				}
				if err := k8sClient.Get(ctx, key, svc); err != nil {
					return false
				}
				if len(svc.OwnerReferences) != 1 {
					return false
				}
				return svc.OwnerReferences[0].UID == parent.UID
			}, testTimeout, testInterval).Should(BeTrue(), "Service owner UID should converge to parent UID")

			Expect(svc.OwnerReferences[0].Name).To(Equal(key.Name))
			Expect(svc.OwnerReferences[0].Kind).To(Equal("AgentDeployment"))
			Expect(svc.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*svc.OwnerReferences[0].Controller).To(BeTrue(), "owner reference must have Controller=true")
		})

		It("creates a ServiceMonitor with owner reference pointing to the AgentDeployment", func() {
			// This test exercises the hasServiceMonitorCRD=true code path, which is
			// enabled by loading the ServiceMonitor CRD into envtest via
			// config/crd/external/monitoring.coreos.com_servicemonitors.yaml.
			sm := &monitoringv1.ServiceMonitor{}
			parent := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, parent); err != nil {
					return false
				}
				if err := k8sClient.Get(ctx, key, sm); err != nil {
					return false
				}
				if len(sm.OwnerReferences) != 1 {
					return false
				}
				return sm.OwnerReferences[0].UID == parent.UID
			}, testTimeout, testInterval).Should(BeTrue(), "ServiceMonitor owner UID should converge to parent UID")

			Expect(sm.OwnerReferences[0].Name).To(Equal(key.Name))
			Expect(sm.OwnerReferences[0].Kind).To(Equal("AgentDeployment"))
			Expect(sm.OwnerReferences[0].Controller).NotTo(BeNil())
			Expect(*sm.OwnerReferences[0].Controller).To(BeTrue(), "ServiceMonitor owner reference must have Controller=true")
		})

		It("sets status.phase to Pending initially (no running pods in envtest)", func() {
			ad := &agentraxv1alpha1.AgentDeployment{}
			// Envtest never runs real pods so the phase must settle on Pending.
			Eventually(func() string {
				if err := k8sClient.Get(ctx, key, ad); err != nil {
					return ""
				}
				return ad.Status.Phase
			}, testTimeout, testInterval).Should(Equal(agentraxv1alpha1.PhasePending), "status.phase should be Pending")
		})

		It("sets the Reconciled condition", func() {
			ad := &agentraxv1alpha1.AgentDeployment{}
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, key, ad); err != nil {
					return false
				}
				c := GetCondition(ad, agentraxv1alpha1.ConditionReconciled)
				return c != nil && c.Status == metav1.ConditionTrue
			}, testTimeout, testInterval).Should(BeTrue(), "Reconciled condition should be True")
		})
	})

	Describe("Image update propagation", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-image-update"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-image-update: %v", err)
			key = createAgentDeployment("ad-image-update", "test-image-update", "nginx:1.24", 8080, 1)

			// Wait for Deployment to be created before proceeding.
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("updates the child Deployment image when spec.image changes", func() {
			// Update the image field.
			ad := &agentraxv1alpha1.AgentDeployment{}
			Expect(k8sClient.Get(ctx, key, ad)).To(Succeed())
			ad.Spec.Image = "nginx:1.25"
			Expect(k8sClient.Update(ctx, ad)).To(Succeed())

			// Verify the child Deployment picks up the new image.
			Eventually(func() string {
				dep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, key, dep); err != nil {
					return ""
				}
				if len(dep.Spec.Template.Spec.Containers) == 0 {
					return ""
				}
				return dep.Spec.Template.Spec.Containers[0].Image
			}, testTimeout, testInterval).Should(Equal("nginx:1.25"), "child Deployment image should be updated")
		})
	})

	Describe("Self-healing", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-self-heal"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-self-heal: %v", err)
			key = createAgentDeployment("ad-self-heal", "test-self-heal", testNginxImage, 8080, 1)

			// Wait for Deployment to be created.
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("recreates the child Deployment when it is deleted out-of-band", func() {
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			origUID := dep.UID

			// Delete the Deployment out-of-band.
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())

			// The Owns() watch on Deployment should trigger a reconcile.
			// Verify it is recreated with a new UID — not the pre-deletion object.
			Eventually(func() bool {
				newDep := &appsv1.Deployment{}
				if err := k8sClient.Get(ctx, key, newDep); err != nil {
					return false
				}
				return newDep.UID != origUID
			}, testTimeout, testInterval).Should(BeTrue(), "Deployment should be self-healed with a new UID")
		})

		It("recreates the child Service when it is deleted out-of-band", func() {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
			origUID := svc.UID

			Expect(k8sClient.Delete(ctx, svc)).To(Succeed())

			Eventually(func() bool {
				newSvc := &corev1.Service{}
				if err := k8sClient.Get(ctx, key, newSvc); err != nil {
					return false
				}
				return newSvc.UID != origUID
			}, testTimeout, testInterval).Should(BeTrue(), "Service should be self-healed with a new UID")
		})
	})

	Describe("Label consistency", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-labels"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-labels: %v", err)
			key = createAgentDeployment("ad-labels", "test-labels", testNginxImage, 8080, 1)
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("creates all child resources with managed-by=agentrax label", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())
			Expect(dep.Labels["app.kubernetes.io/managed-by"]).To(Equal("agentrax"))

			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, svc)
			}, testTimeout, testInterval).Should(Succeed())
			Expect(svc.Labels["app.kubernetes.io/managed-by"]).To(Equal("agentrax"))
		})

		It("child resource selector labels match pod template labels", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())

			for k, v := range dep.Spec.Selector.MatchLabels {
				Expect(dep.Spec.Template.Labels[k]).To(Equal(v),
					"pod template label %s should match selector", k)
			}
		})

		It("Service selector matches Deployment pod template labels", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())

			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, svc)
			}, testTimeout, testInterval).Should(Succeed())

			for k, v := range svc.Spec.Selector {
				Expect(dep.Spec.Template.Labels[k]).To(Equal(v),
					"Service selector label %s should match Deployment pod template", k)
			}
		})
	})

	Describe("Multiple AgentDeployments in same namespace", func() {
		var key1, key2 types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-multi"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-multi: %v", err)
			key1 = createAgentDeployment("ad-alpha", "test-multi", "nginx:1.24", 8080, 1)
			key2 = createAgentDeployment("ad-beta", "test-multi", "nginx:1.25", 9090, 2)
		})

		AfterEach(func() {
			deleteAgentDeployment(key1)
			deleteAgentDeployment(key2)
		})

		It("creates independent Deployments for each AgentDeployment", func() {
			dep1 := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key1, dep1)
			}, testTimeout, testInterval).Should(Succeed())

			dep2 := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key2, dep2)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(dep1.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.24"))
			Expect(dep2.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.25"))
			Expect(*dep2.Spec.Replicas).To(Equal(int32(2)))
		})

		It("deleting one AgentDeployment does not affect the other's children", func() {
			// Ensure both Deployments exist.
			Eventually(func() error {
				return k8sClient.Get(ctx, key1, &appsv1.Deployment{})
			}, testTimeout, testInterval).Should(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, key2, &appsv1.Deployment{})
			}, testTimeout, testInterval).Should(Succeed())

			// Delete the first one.
			deleteAgentDeployment(key1)

			// The second's Deployment must persist for a sustained window.
			Consistently(func() error {
				return k8sClient.Get(ctx, key2, &appsv1.Deployment{})
			}, 3*time.Second, testInterval).Should(Succeed(), "key2's Deployment should not be affected by key1 deletion")
		})
	})

	Describe("Port defaulting", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-port-default"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-port-default: %v", err)
			// Create with port=0 (omitted) to test the default.
			key = createAgentDeployment("ad-port-default", "test-port-default", testNginxImage, 0, 1)
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("defaults the container port to 8080 when spec.port is zero", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(dep.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(8080)))
		})

		It("defaults the Service port to 8080 when spec.port is zero", func() {
			svc := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, svc)
			}, testTimeout, testInterval).Should(Succeed())

			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
		})
	})

	Describe("Env and Args propagation", func() {
		var key types.NamespacedName

		BeforeEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-env-args"}}
			err := k8sClient.Create(ctx, ns)
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace test-env-args: %v", err)

			ad := &agentraxv1alpha1.AgentDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ad-env-args",
					Namespace: "test-env-args",
				},
				Spec: agentraxv1alpha1.AgentDeploymentSpec{
					Image:     testNginxImage,
					Port:      8080,
					TenantRef: "team-test",
					Replicas: agentraxv1alpha1.ScalingPolicy{
						Min: 1, Max: 3, Metric: "queueDepth", Target: 50,
					},
					Env:  []corev1.EnvVar{{Name: "MODEL", Value: "gpt4"}},
					Args: []string{"--serve", "--workers=4"},
				},
			}
			Expect(k8sClient.Create(ctx, ad)).To(Succeed())
			key = client.ObjectKeyFromObject(ad)
		})

		AfterEach(func() {
			deleteAgentDeployment(key)
		})

		It("propagates env vars and args to the child Deployment container", func() {
			dep := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, key, dep)
			}, testTimeout, testInterval).Should(Succeed())

			c := dep.Spec.Template.Spec.Containers[0]
			Expect(c.Env).To(ContainElement(corev1.EnvVar{Name: "MODEL", Value: "gpt4"}))
			Expect(c.Args).To(Equal([]string{"--serve", "--workers=4"}))
		})
	})
})

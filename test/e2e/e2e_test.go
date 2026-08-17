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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gitcommitankit/agentrax/test/utils"
)

const (
	managerNamespace = "agentrax-system"
	e2eNamespace     = "tenant-e2e"
	e2eQuotaName     = "e2e-quota"
)

var _ = Describe("Agentrax Operator End-to-End Suite", Ordered, func() {
	var projectimage = "example.com/agentrax:v0.1.0"

	BeforeAll(func() {
		By("installing cert-manager for webhook TLS")
		Expect(utils.InstallCertManager()).To(Succeed())

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", managerNamespace)
		_, _ = utils.Run(cmd)

		By("building the manager container image")
		cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectimage))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("loading the manager image into the Kind cluster")
		err = utils.LoadImageToKindClusterWithName(projectimage)
		Expect(err).NotTo(HaveOccurred())

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectimage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for controller-manager pod to be in Running phase")
		Eventually(func() error {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-n", managerNamespace,
				"-o", "jsonpath={.items[*].status.phase}",
			)
			out, err := utils.Run(cmd)
			if err != nil {
				return err
			}
			if !strings.Contains(string(out), "Running") {
				return fmt.Errorf("manager pod not yet running, status: %s", string(out))
			}
			return nil
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By("creating tenant namespace for E2E tests")
		cmd = exec.Command("kubectl", "create", "ns", e2eNamespace)
		_, _ = utils.Run(cmd)

		By("creating baseline TenantQuota")
		quotaYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: TenantQuota
metadata:
  name: %s
  namespace: %s
spec:
  maxAgents: 3
  maxGPUs: 2
  maxTotalReplicas: 10
  maxReplicasPerAgent: 4
`, e2eQuotaName, e2eNamespace)
		Expect(kubectlApply(quotaYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("cleaning up tenant namespace")
		cmd := exec.Command("kubectl", "delete", "ns", e2eNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Scenario 1: Core Reconciliation & Self-Healing", func() {
		const agentName = "self-heal-agent"

		It("creates child Deployment and self-heals upon out-of-band deletion", func() {
			adYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: %s
  namespace: %s
spec:
  image: gcr.io/google-containers/echoserver:1.4
  port: 8080
  tenantRef: %s
  replicas:
    min: 1
    max: 2
    metric: queueDepth
    target: 50
  rollout:
    strategy: Recreate
`, agentName, e2eNamespace, e2eQuotaName)

			Expect(kubectlApply(adYAML)).To(Succeed())

			By("waiting for child Deployment to be created")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "deployment", agentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, time.Minute, time.Second).Should(Succeed())

			By("waiting for child Service to be created")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "service", agentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, 30*time.Second, time.Second).Should(Succeed())

			By("deleting child Deployment out-of-band")
			deleteCmd := exec.Command("kubectl", "delete", "deployment", agentName, "-n", e2eNamespace)
			_, err := utils.Run(deleteCmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying operator self-heals by recreating the Deployment")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "deployment", agentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, 30*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Scenario 2: Multi-Tenancy Quota Enforcement", func() {
		It("caps HPA max replicas and sets QuotaLimited condition when exceeding tenant ceiling", func() {
			overQuotaAgent := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: over-quota-agent
  namespace: %s
spec:
  image: gcr.io/google-containers/echoserver:1.4
  tenantRef: %s
  replicas:
    min: 1
    max: 8
    metric: queueDepth
    target: 50
`, e2eNamespace, e2eQuotaName)

			Expect(kubectlApply(overQuotaAgent)).To(Succeed())

			By("waiting for HPA to be created with quota-capped maxReplicas")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "hpa", "over-quota-agent", "-n", e2eNamespace,
					"-o", "jsonpath={.spec.maxReplicas}")
				out, err := utils.Run(cmd)
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(out))
			}, 30*time.Second, time.Second).Should(Equal("4"), "HPA maxReplicas should be capped to maxReplicasPerAgent (4)")

			By("verifying QuotaLimited condition is set to True")
			Eventually(func() string {
				cmd := exec.Command("kubectl", "get", "agentdeployment", "over-quota-agent", "-n", e2eNamespace,
					"-o", `jsonpath={.status.conditions[?(@.type=="QuotaLimited")].status}`)
				out, err := utils.Run(cmd)
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(out))
			}, 30*time.Second, time.Second).Should(Equal("True"))
		})
	})

	Context("Scenario 3: Canary Rollout & Abort Rollback", func() {
		const canaryAgentName = "canary-test-agent"

		It("transitions through rollout and cleans up on manual abort", func() {
			initialYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: %s
  namespace: %s
spec:
  image: gcr.io/google-containers/echoserver:1.4
  port: 8080
  tenantRef: %s
  replicas:
    min: 1
    max: 2
    metric: queueDepth
    target: 50
  rollout:
    strategy: Canary
    steps:
      - setWeight: 20
      - pause: 60s
      - setWeight: 100
    rollback:
      maxErrorRate: "1%%"
      maxP99LatencyMs: 500
      minRequestSample: 100
`, canaryAgentName, e2eNamespace, e2eQuotaName)

			Expect(kubectlApply(initialYAML)).To(Succeed())

			By("waiting for initial stable Deployment to be created")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "deployment", canaryAgentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, time.Minute, time.Second).Should(Succeed())

			By("triggering canary rollout with image update and abort")
			abortYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: %s
  namespace: %s
spec:
  image: gcr.io/google-containers/echoserver:1.5
  port: 8080
  tenantRef: %s
  replicas:
    min: 1
    max: 2
    metric: queueDepth
    target: 50
  rollout:
    strategy: Canary
    abort: true
    steps:
      - setWeight: 20
      - pause: 60s
      - setWeight: 100
    rollback:
      maxErrorRate: "1%%"
      maxP99LatencyMs: 500
      minRequestSample: 100
`, canaryAgentName, e2eNamespace, e2eQuotaName)

			Expect(kubectlApply(abortYAML)).To(Succeed())

			By("verifying stable Deployment remains available after abort")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "deployment", canaryAgentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, 30*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Scenario 4: Graceful Deletion & Finalizer Cleanup", func() {
		const deleteAgentName = "delete-test-agent"

		It("removes finalizer and cleans up all owned child resources", func() {
			adYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: %s
  namespace: %s
spec:
  image: gcr.io/google-containers/echoserver:1.4
  port: 8080
  tenantRef: %s
  replicas:
    min: 1
    max: 2
    metric: queueDepth
    target: 50
`, deleteAgentName, e2eNamespace, e2eQuotaName)

			Expect(kubectlApply(adYAML)).To(Succeed())

			By("waiting for AgentDeployment to exist")
			Eventually(func() error {
				cmd := exec.Command("kubectl", "get", "agentdeployment", deleteAgentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err
			}, time.Minute, time.Second).Should(Succeed())

			By("deleting the AgentDeployment")
			cmd := exec.Command("kubectl", "delete", "agentdeployment", deleteAgentName, "-n", e2eNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying AgentDeployment is fully deleted")
			Eventually(func() bool {
				cmd := exec.Command("kubectl", "get", "agentdeployment", deleteAgentName, "-n", e2eNamespace)
				_, err := utils.Run(cmd)
				return err != nil
			}, time.Minute, 2*time.Second).Should(BeTrue())
		})
	})
})

func kubectlApply(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	_, err := utils.Run(cmd)
	return err
}

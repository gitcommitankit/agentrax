//go:build e2e

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

// Package e2e contains end-to-end tests that run against a real kind cluster
// with Prometheus, Prometheus Adapter, and cert-manager installed.
// Build with -tags e2e to include this file; it is excluded from normal CI.
// Run with: make e2e-soak
package e2e

// scaling_test.go — Phase 3 soak test for metrics-driven autoscaling.
//
// This file verifies the full autoscaling lifecycle:
//   - An AgentDeployment with metric:queueDepth produces a correctly configured HPA.
//   - Injecting synthetic queue-depth load causes replica count to increase
//     within one HPA polling cycle (default 15s).
//   - Removing load causes scale-down no faster than the 300s stabilization
//     window (verifiable: no flapping observed in a 10-minute soak).
//   - Scale-up capped by tenant quota: QuotaLimited condition is set when
//     the HPA would exceed the tenant's remaining replica headroom.
//   - spec.replicas.min is never violated (starts at min, not zero).
//
// Prerequisites (installed by `make deploy-deps`):
//   - Prometheus Operator + Prometheus scraping agentrax pods
//   - Prometheus Adapter with agentrax custom-metrics-config.yaml applied
//   - cert-manager (for webhook TLS)
//   - A conformant Gateway API implementation (for Phase 4 — optional here)
//
// The soak load generator uses a Kubernetes Job that pushes synthetic
// agentrax_queue_depth metrics via a pushgateway stub, or alternatively
// a direct metric exporter sidecar injected by the test.

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/gitcommitankit/agentrax/test/utils"
)

const (
	// soakNamespace is the namespace used for all soak test resources.
	soakNamespace = "agentrax-soak"

	// soakTenantQuota is the name of the TenantQuota used in soak tests.
	soakTenantQuota = "soak-quota"

	// soakAgentName is the name of the AgentDeployment under test.
	soakAgentName = "soak-agent"

	// hpaPollingInterval is the time to wait for the HPA to react to a metric change.
	// Kubernetes HPA default polling is 15s; we allow 3× headroom.
	hpaPollingInterval = 60 * time.Second

	// soakDuration is how long the no-flapping soak runs (10 minutes per DoD).
	soakDuration = 10 * time.Minute

	// scaleDownStabilizationWindow is the expected HPA scale-down stabilization.
	// The soak verifies no scale-down occurs faster than this after load is removed.
	scaleDownStabilizationWindow = 5 * time.Minute
)

var _ = Describe("Phase 3 — Metrics-Driven Autoscaling (soak)", Ordered, func() {
	ctx := context.Background()
	_ = ctx // used in BeforeAll/AfterAll below

	BeforeAll(func() {
		By("creating soak namespace")
		cmd := exec.Command("kubectl", "create", "ns", soakNamespace, "--dry-run=client", "-o", "yaml")
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		applyCmd := exec.Command("kubectl", "apply", "-f", "-")
		applyCmd.Stdin = bytesReader(out)
		_, err = utils.Run(applyCmd)
		Expect(err).NotTo(HaveOccurred())

		By("applying TenantQuota for soak tests")
		tqYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: TenantQuota
metadata:
  name: %s
  namespace: %s
spec:
  maxAgents: 3
  maxGPUs: 0
  maxTotalReplicas: 12
  maxReplicasPerAgent: 6
`, soakTenantQuota, soakNamespace)
		Expect(kubectlApplyStdin(tqYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("cleaning up soak namespace")
		cmd := exec.Command("kubectl", "delete", "ns", soakNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	// ── Scenario 1: HPA created with correct spec ─────────────────────────────

	It("creates a managed HPA when an AgentDeployment is applied", func() {
		PendingNow("requires Prometheus Adapter installed via make deploy-deps")

		adYAML := fmt.Sprintf(`
apiVersion: agentrax.io/v1alpha1
kind: AgentDeployment
metadata:
  name: %s
  namespace: %s
spec:
  image: nginx:latest
  tenantRef: %s
  replicas:
    min: 1
    max: 6
    metric: queueDepth
    target: 50
`, soakAgentName, soakNamespace, soakTenantQuota)
		Expect(kubectlApplyStdin(adYAML)).To(Succeed())

		By("waiting for the HPA to be created")
		Eventually(func() error {
			cmd := exec.Command("kubectl", "get", "hpa", soakAgentName, "-n", soakNamespace)
			_, err := utils.Run(cmd)
			return err
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying HPA scaleTargetRef points to the Deployment")
		cmd := exec.Command("kubectl", "get", "hpa", soakAgentName, "-n", soakNamespace,
			"-o", "jsonpath={.spec.scaleTargetRef.name}")
		out, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal(soakAgentName))

		By("verifying HPA metric is agentrax_queue_depth (External type)")
		metricCmd := exec.Command("kubectl", "get", "hpa", soakAgentName, "-n", soakNamespace,
			"-o", "jsonpath={.spec.metrics[0].external.metric.name}")
		metricOut, err := utils.Run(metricCmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(metricOut)).To(Equal("agentrax_queue_depth"))
	})

	// ── Scenario 2: Scale-out under synthetic load ────────────────────────────

	It("scales replicas up when queue depth exceeds the target threshold", func() {
		PendingNow("requires synthetic load generator + Prometheus Adapter + kind cluster")

		By("injecting synthetic load: push agentrax_queue_depth > 50 per replica")
		// TODO: deploy the load-generator Job from test/fixtures/load-gen.yaml.
		// The load generator writes time-series to a Pushgateway stub that
		// Prometheus scrapes per the ServiceMonitor.

		By("waiting for replica count to exceed the initial minimum")
		Eventually(func() int {
			cmd := exec.Command("kubectl", "get", "deploy", soakAgentName,
				"-n", soakNamespace, "-o", "jsonpath={.status.readyReplicas}")
			out, err := utils.Run(cmd)
			if err != nil {
				return -1
			}
			count := 0
			_, _ = fmt.Sscanf(string(out), "%d", &count)
			return count
		}, hpaPollingInterval, 5*time.Second).Should(BeNumerically(">", 1),
			"replica count should increase above the minimum under load")
	})

	// ── Scenario 3: No flapping during 10-minute soak ────────────────────────

	It("does not flap replicas during a 10-minute steady-state soak", func() {
		PendingNow("requires synthetic load generator + kind cluster running for 10+ minutes")

		By(fmt.Sprintf("observing replica count for %s under steady load", soakDuration))
		// Track replica count samples; assert coefficient of variation is < 20%.
		// Flapping definition: more than 2 scale events in a 5-minute window.

		// TODO: collect replica samples every 30s for soakDuration.
		// Fail if abs(sample[i+1] - sample[i]) > 1 more than twice in any 5-minute window.
	})

	// ── Scenario 4: Scale-down respects stabilization window ─────────────────

	It("does not scale down faster than the 300s stabilization window after load removal", func() {
		PendingNow("requires load generator + timing control in kind cluster")

		By("removing synthetic load")
		// TODO: delete the load-generator Job.

		By(fmt.Sprintf("asserting replica count stays elevated for at least %s", scaleDownStabilizationWindow))
		// Sample replica count every 30s for scaleDownStabilizationWindow.
		// Fail if replicas drop before the window elapses.
	})

	// ── Scenario 5: QuotaLimited condition when scale-up exceeds quota ────────

	It("sets QuotaLimited condition and caps HPA when quota headroom is exhausted", func() {
		PendingNow("requires a second AgentDeployment consuming remaining quota headroom")

		By("creating a second AD that consumes the remaining replica budget")
		// TODO: apply a second AD with max=high to exhaust the TenantQuota headroom.

		By("verifying the first AD's QuotaLimited condition is True")
		Eventually(func() bool {
			cmd := exec.Command("kubectl", "get", "agentdeployment", soakAgentName,
				"-n", soakNamespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="QuotaLimited")].status}`)
			out, err := utils.Run(cmd)
			if err != nil {
				return false
			}
			return string(out) == "True"
		}, 30*time.Second, time.Second).Should(BeTrue(),
			"QuotaLimited condition should be True when HPA is capped by quota")
	})

	// ── Scenario 6: spec.replicas.min never violated ──────────────────────────

	It("never scales below spec.replicas.min even at zero traffic", func() {
		PendingNow("requires zero-traffic steady state in kind cluster")

		By("removing all load and waiting for the stabilization window")
		// TODO: ensure no load for scaleDownStabilizationWindow + buffer.

		By("asserting ready replicas >= spec.replicas.min")
		Consistently(func() int {
			cmd := exec.Command("kubectl", "get", "deploy", soakAgentName,
				"-n", soakNamespace, "-o", "jsonpath={.status.readyReplicas}")
			out, err := utils.Run(cmd)
			if err != nil {
				return -1
			}
			count := 0
			_, _ = fmt.Sscanf(string(out), "%d", &count)
			return count
		}, 2*time.Minute, 10*time.Second).Should(BeNumerically(">=", 1),
			"ready replicas must never drop below spec.replicas.min=1")
	})
})

// ── helpers ──────────────────────────────────────────────────────────────────

// kubectlApplyStdin pipes a YAML string to kubectl apply -f -.
func kubectlApplyStdin(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = stringReader(yaml)
	_, err := utils.Run(cmd)
	return err
}

// stringReader wraps a string as an io.Reader for test inputs.
func stringReader(s string) *bytesReaderWrapper {
	return &bytesReaderWrapper{data: []byte(s), pos: 0}
}

// bytesReader wraps a byte slice as an io.Reader for test inputs.
func bytesReader(b []byte) *bytesReaderWrapper {
	return &bytesReaderWrapper{data: b, pos: 0}
}

// bytesReaderWrapper is a minimal io.Reader over a byte slice.
type bytesReaderWrapper struct {
	data []byte
	pos  int
}

// Read implements io.Reader for bytesReaderWrapper.
func (r *bytesReaderWrapper) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

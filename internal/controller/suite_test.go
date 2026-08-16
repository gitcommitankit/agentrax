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
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/quota"
	"github.com/gitcommitankit/agentrax/internal/registry"
	agentraxwebhook "github.com/gitcommitankit/agentrax/internal/webhook"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc
var mgrDone chan struct{}

// testReconciler is the AgentDeploymentReconciler registered with the manager.
// Tests that need to inject a Deregister hook can set it before the action
// and clear it in AfterEach to avoid contaminating other tests.
var testReconciler *AgentDeploymentReconciler

// testEnforcer is the shared quota Enforcer used by the TenantQuota reconciler
// and the validating webhook in integration tests.
var testEnforcer *quota.Enforcer

// testRegistry and testRegistrar are the shared MCP registry fixtures used in tests.
var testRegistry *registry.Registry
var testRegistrar *registry.Registrar
var testMockMCP *testMockMCPClient

type testMockMCPClient struct {
	mu    sync.Mutex
	tools []string
	err   error
}

func (m *testMockMCPClient) Initialize(ctx context.Context, endpoint string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if len(m.tools) == 0 {
		return []string{"default_tool"}, nil
	}
	return m.tools, nil
}

func (m *testMockMCPClient) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *testMockMCPClient) SetTools(tools []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = tools
}

// TestControllers is the Ginkgo test suite runner for controller integration tests.
func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			// External CRDs vendored from upstream for integration testing.
			// ServiceMonitor CRD sourced from prometheus-operator v0.75.0.
			filepath.Join("..", "..", "config", "crd", "external"),
		},
		ErrorIfCRDPathMissing: true,

		// Configure envtest to install and run the webhooks during integration
		// tests. envtest generates self-signed TLS certs automatically.
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{filepath.Join("..", "..", "config", "webhook")},
		},

		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without calling the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Register our CRD types and core Kubernetes types.
	Expect(agentraxv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme.Scheme)).To(Succeed())
	// Register autoscaling/v2 so HPA objects can be created/read in integration tests.
	Expect(autoscalingv2.AddToScheme(scheme.Scheme)).To(Succeed())
	// Register prometheus-operator types so the reconciler can handle ServiceMonitor objects.
	Expect(monitoringv1.AddToScheme(scheme.Scheme)).To(Succeed())
	// Register apiextensions types so serviceMonitorCRDExists can decode CRD objects
	// when called from SetupWithManager via the uncached API reader.
	Expect(apiextensionsv1.AddToScheme(scheme.Scheme)).To(Succeed())
	// Register Gateway API types so HTTPRoute objects can be created in canary tests.
	Expect(gatewayv1.Install(scheme.Scheme)).To(Succeed())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Ensure system namespace exists for registry ConfigMap
	systemNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "agentrax-system",
		},
	}
	_ = k8sClient.Create(ctx, systemNamespace)

	// Start the controller manager so the reconciler runs during integration tests.
	// Use envtest's webhook host/port so the manager's webhook server binds to the
	// same address the webhook install options configured the API server to call.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		// Disable the metrics server in tests to avoid port conflicts.
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	Expect(err).NotTo(HaveOccurred())

	testEnforcer = quota.NewEnforcer(quota.DefaultGPUResourceName)

	testRegistry = registry.NewRegistry(k8sClient, "agentrax-system", registry.DefaultTTL)
	testMockMCP = &testMockMCPClient{}
	testRegistrar = registry.NewRegistrar(testRegistry, testMockMCP)

	testReconciler = &AgentDeploymentReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		GPUResourceName: quota.DefaultGPUResourceName,
		Registrar:       testRegistrar,
	}
	testReconciler.SetDeregister(func(ctx context.Context, ad *agentraxv1alpha1.AgentDeployment) error {
		return testRegistrar.Deregister(ctx, ad)
	})
	Expect(testReconciler.SetupWithManager(mgr)).To(Succeed())

	Expect((&TenantQuotaReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Enforcer: testEnforcer,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect(agentraxwebhook.SetupAgentDeploymentWebhookWithManager(mgr, testEnforcer)).To(Succeed())

	mgrDone = make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrDone)
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	// Wait for the manager goroutine to finish before stopping envtest.
	<-mgrDone
	// Stop the shared enforcer's background sweep goroutine.
	testEnforcer.Stop()
	Expect(testEnv.Stop()).To(Succeed())
})

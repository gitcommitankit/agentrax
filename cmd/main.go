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

// Package main is the entry point for the Agentrax operator manager.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
	"github.com/gitcommitankit/agentrax/internal/controller"
	"github.com/gitcommitankit/agentrax/internal/metrics"
	"github.com/gitcommitankit/agentrax/internal/quota"
	"github.com/gitcommitankit/agentrax/internal/registry"
	"github.com/gitcommitankit/agentrax/internal/rollout"
	agentraxwebhook "github.com/gitcommitankit/agentrax/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// registryServerRunnable runs the MCP registry HTTP server on every manager replica.
type registryServerRunnable struct {
	registryAddr string
	mcpRegistry  *registry.Registry
}

// Start starts the MCP discovery HTTP server and listens until context cancellation.
func (r *registryServerRunnable) Start(ctx context.Context) error {
	r.mcpRegistry.Start(ctx)
	srv := &http.Server{
		Addr:              r.registryAddr,
		Handler:           r.mcpRegistry.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	setupLog.Info("starting MCP discovery registry server", "addr", r.registryAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NeedLeaderElection returns false so the registry server runs across all manager replicas.
func (r *registryServerRunnable) NeedLeaderElection() bool {
	return false
}

// init registers all Kubernetes core, CRD, and monitoring schemes.
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(autoscalingv2.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(monitoringv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))

	utilruntime.Must(agentraxv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch

// main is the entrypoint for the Agentrax controller manager binary.
func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var gpuResourceName string
	var prometheusURL string
	var gatewayName string
	var gatewayNamespace string
	var registryAddr string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&gpuResourceName, "gpu-resource-name", quota.DefaultGPUResourceName,
		"Kubernetes resource name used to count GPU units in AgentDeployment resource limits.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"URL of the Prometheus HTTP API (e.g. http://prometheus-operated.monitoring.svc:9090). "+
			"Required for Canary rollout strategy; if empty, canary is unavailable.")
	flag.StringVar(&gatewayName, "gateway-name", "agentrax-gateway",
		"Name of the Gateway API Gateway object used for canary traffic splitting.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "agentrax-system",
		"Namespace of the Gateway API Gateway object used for canary traffic splitting.")
	flag.StringVar(&registryAddr, "registry-bind-address", ":9090",
		"The address the MCP discovery registry HTTP endpoint binds to.")
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Resolve the webhook-enabled flag. Enable webhook server only if explicitly set
	// to "true" or if TLS certificates are present in /tmp/k8s-webhook-server/serving-certs.
	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS") == "true"
	if _, err := os.Stat("/tmp/k8s-webhook-server/serving-certs/tls.crt"); err == nil {
		enableWebhooks = true
	}
	if os.Getenv("ENABLE_WEBHOOKS") == "false" {
		enableWebhooks = false
	}
	setupLog.Info("webhook state resolved", "enabled", enableWebhooks)

	var webhookServer webhook.Server
	if enableWebhooks {
		webhookServer = webhook.NewServer(webhook.Options{
			TLSOpts: tlsOpts,
		})
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// TODO(user): TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
		// not provided, self-signed certificates will be generated by default. This option is not recommended for
		// production environments as self-signed certificates do not offer the same level of trust and security
		// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
		// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
		// to provide certificates, ensuring the server communicates using trusted and secure certificates.
		TLSOpts: tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Determine registry namespace once for both manager cache and registry construction.
	registryNamespace := os.Getenv("POD_NAMESPACE")
	if registryNamespace == "" {
		registryNamespace = "agentrax-system"
	}

	registryTTL := registry.DefaultTTL
	if v := os.Getenv("AGENTRAX_REGISTRY_TTL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			registryTTL = parsed
		} else {
			setupLog.Error(err, "invalid or non-positive AGENTRAX_REGISTRY_TTL, using default",
				"default", registry.DefaultTTL)
		}
	}

	mcpHealthInterval := 60 * time.Second
	if v := os.Getenv("AGENTRAX_MCP_HEALTH_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			mcpHealthInterval = parsed
		} else {
			setupLog.Error(err, "invalid or non-positive AGENTRAX_MCP_HEALTH_INTERVAL, using default",
				"default", mcpHealthInterval)
		}
	}

	if mcpHealthInterval >= registryTTL {
		adjusted := registryTTL / 2
		if adjusted <= 0 {
			adjusted = time.Second
		}
		setupLog.Info("AGENTRAX_MCP_HEALTH_INTERVAL must be strictly less than AGENTRAX_REGISTRY_TTL",
			"configuredInterval", mcpHealthInterval,
			"registryTTL", registryTTL,
			"adjustedInterval", adjusted)
		mcpHealthInterval = adjusted
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ddf1aac5.agentrax.io",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					Namespaces: map[string]cache.Config{
						registryNamespace: {},
					},
				},
			},
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Shared quota enforcer used by both the webhook validator and TenantQuota reconciler.
	quotaEnforcer := quota.NewEnforcer(gpuResourceName)

	// Build the CanaryController when --prometheus-url is provided.
	// When nil, AgentDeployments with strategy=Canary behave as Recreate.
	var canaryController *rollout.Controller
	if prometheusURL != "" {
		setupLog.Info("canary rollout enabled", "prometheusURL", prometheusURL,
			"gatewayName", gatewayName, "gatewayNamespace", gatewayNamespace)
		canaryController = &rollout.Controller{
			Client:           mgr.GetClient(),
			Scheme:           mgr.GetScheme(),
			PromClient:       metrics.NewClient(prometheusURL),
			GatewayName:      gatewayName,
			GatewayNamespace: gatewayNamespace,
			FailSafeTimeout:  60 * time.Second,
		}
	} else {
		setupLog.Info("canary rollout disabled (no --prometheus-url)")
	}

	// Initialize MCP discovery registry and registrar.
	mcpRegistry := registry.NewRegistry(mgr.GetClient(), registryNamespace, registryTTL)
	mcpRegistrar := registry.NewRegistrar(mcpRegistry, registry.NewHTTPMCPClient())

	if registryAddr != "" && registryAddr != "0" {
		registryRunnable := &registryServerRunnable{
			registryAddr: registryAddr,
			mcpRegistry:  mcpRegistry,
		}
		if err := mgr.Add(registryRunnable); err != nil {
			setupLog.Error(err, "unable to add registry server to manager")
			os.Exit(1)
		}
	}

	agentDeploymentReconciler := &controller.AgentDeploymentReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		GPUResourceName:   gpuResourceName,
		CanaryController:  canaryController,
		Registrar:         mcpRegistrar,
		MCPHealthInterval: mcpHealthInterval,
	}
	if canaryController != nil {
		canaryController.Registrar = mcpRegistrar
	}

	if err = agentDeploymentReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentDeployment")
		os.Exit(1)
	}
	if err = (&controller.TenantQuotaReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Enforcer: quotaEnforcer,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TenantQuota")
		os.Exit(1)
	}
	if enableWebhooks {
		if err = agentraxwebhook.SetupAgentDeploymentWebhookWithManager(mgr, quotaEnforcer); err != nil {
			setupLog.Error(err, "unable to register webhook", "webhook", "AgentDeployment")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

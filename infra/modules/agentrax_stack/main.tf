# kind cluster module — agentrax_stack main.tf
# Installs cert-manager, kube-prometheus-stack, and the agentrax operator
# in strict dependency order via Helm.

terraform {
  required_providers {
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.14"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
  }
  required_version = ">= 1.6"
}

# ---------------------------------------------------------------------------
# cert-manager — prerequisite for admission webhooks
# ---------------------------------------------------------------------------
resource "helm_release" "cert_manager" {
  name             = "cert-manager"
  repository       = "https://charts.jetstack.io"
  chart            = "cert-manager"
  version          = var.cert_manager_version
  namespace        = "cert-manager"
  create_namespace = true

  set {
    name  = "crds.enabled"
    value = "true"
  }

  # Wait until all cert-manager pods are ready before proceeding.
  wait    = true
  timeout = 300
}

# ---------------------------------------------------------------------------
# kube-prometheus-stack — Prometheus + Grafana + Prometheus Adapter
# Required for canary rollout threshold evaluation.
# ---------------------------------------------------------------------------
resource "helm_release" "kube_prometheus_stack" {
  name             = "kube-prometheus-stack"
  repository       = "https://prometheus-community.github.io/helm-charts"
  chart            = "kube-prometheus-stack"
  version          = var.prometheus_stack_version
  namespace        = "monitoring"
  create_namespace = true

  # Lightweight values for kind — disable heavy storage and alertmanager for local dev.
  values = [
    yamlencode({
      grafana = {
        enabled = false
      }
      alertmanager = {
        enabled = false
      }
      prometheus = {
        prometheusSpec = {
          retention = "2h"
        }
      }
    })
  ]

  wait    = true
  timeout = 600

  depends_on = [helm_release.cert_manager]
}

# ---------------------------------------------------------------------------
# agentrax operator — installed last, after all dependencies are ready
# ---------------------------------------------------------------------------
resource "helm_release" "agentrax" {
  name             = "agentrax"
  chart            = var.agentrax_chart_path
  namespace        = "agentrax-system"
  create_namespace = true

  # Wire Prometheus URL so canary rollout threshold evaluation is live.
  set {
    name  = "prometheus.url"
    value = "http://kube-prometheus-stack-prometheus.monitoring.svc:9090"
  }

  set {
    name  = "manager.leaderElect"
    value = tostring(var.agentrax_leader_elect)
  }

  dynamic "set" {
    for_each = var.agentrax_extra_values
    content {
      name  = set.key
      value = set.value
    }
  }

  wait    = true
  timeout = 300

  depends_on = [helm_release.kube_prometheus_stack]
}

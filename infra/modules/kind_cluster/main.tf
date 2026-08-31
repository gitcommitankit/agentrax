# kind cluster module — provisions a local kind cluster using the tehcyx/kind provider.
# This module is intentionally thin: it creates the cluster and exposes the kubeconfig
# so the agentrax_stack module can install the Helm stack into it.

terraform {
  required_providers {
    kind = {
      source  = "tehcyx/kind"
      version = "~> 0.6"
    }
  }
  required_version = ">= 1.6"
}

resource "kind_cluster" "this" {
  name           = var.cluster_name
  wait_for_ready = true

  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"

    node {
      role = "control-plane"
    }

    # Additional worker node gives the scheduler headroom for agent pods.
    # The agentrax operator (including the MCP registry on :9090) runs on the
    # worker node. Access the registry locally via:
    #   kubectl port-forward -n agentrax-system svc/agentrax-mcp-registry 9090:9090
    node {
      role = "worker"
    }
  }
}

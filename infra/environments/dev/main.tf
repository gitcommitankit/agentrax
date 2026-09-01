# dev environment — local kind cluster + agentrax stack
# This is the primary target for local development, integration testing, and CI.
# State is stored in a local backend file (terraform.tfstate) — not shared.

terraform {
  required_version = ">= 1.6"

  # Local backend — intentional for dev. Do not check in terraform.tfstate.
  backend "local" {}

  required_providers {
    kind = {
      source  = "tehcyx/kind"
      version = "~> 0.6"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.14"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
  }
}

# ---------------------------------------------------------------------------
# Step 1: Provision the kind cluster
# ---------------------------------------------------------------------------
module "kind_cluster" {
  source       = "../../modules/kind_cluster"
  cluster_name = var.cluster_name
}

# ---------------------------------------------------------------------------
# Step 2: Configure the Helm and Kubernetes providers to target the new cluster.
# Both providers read credentials from the kind_cluster module outputs so no
# local kubeconfig file needs to exist before `terraform apply`.
# ---------------------------------------------------------------------------
provider "helm" {
  kubernetes {
    host                   = module.kind_cluster.endpoint
    client_certificate     = module.kind_cluster.client_certificate
    client_key             = module.kind_cluster.client_key
    cluster_ca_certificate = module.kind_cluster.cluster_ca_certificate
  }
}

provider "kubernetes" {
  host                   = module.kind_cluster.endpoint
  client_certificate     = module.kind_cluster.client_certificate
  client_key             = module.kind_cluster.client_key
  cluster_ca_certificate = module.kind_cluster.cluster_ca_certificate
}

# ---------------------------------------------------------------------------
# Step 3: Install cert-manager → kube-prometheus-stack → agentrax
# ---------------------------------------------------------------------------
module "agentrax_stack" {
  source = "../../modules/agentrax_stack"

  cert_manager_version     = var.cert_manager_version
  prometheus_stack_version = var.prometheus_stack_version
  agentrax_chart_path      = var.agentrax_chart_path
  agentrax_leader_elect    = var.agentrax_leader_elect
  agentrax_extra_values    = var.agentrax_extra_values

  # The stack module requires the cluster to exist first.
  # Provider-level dependency is enforced via the shared kubeconfig above.
}

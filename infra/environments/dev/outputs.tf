# dev environment — outputs

output "cluster_endpoint" {
  description = "Kubernetes API server endpoint for the dev kind cluster."
  value       = module.kind_cluster.endpoint
}

output "agentrax_namespace" {
  description = "Namespace where the agentrax operator was deployed."
  value       = module.agentrax_stack.agentrax_namespace
}

output "agentrax_release_status" {
  description = "Helm release status for the agentrax chart."
  value       = module.agentrax_stack.agentrax_release_status
}

output "prometheus_namespace" {
  description = "Namespace where kube-prometheus-stack is deployed."
  value       = module.agentrax_stack.prometheus_namespace
}

output "kubeconfig" {
  description = "Raw kubeconfig for the kind cluster. Pipe into kubectl or save to a file."
  value       = module.kind_cluster.kubeconfig
  sensitive   = true
}

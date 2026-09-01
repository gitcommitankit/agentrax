# agentrax_stack module — outputs

output "agentrax_namespace" {
  description = "Kubernetes namespace where the agentrax operator is deployed."
  value       = helm_release.agentrax.namespace
}

output "agentrax_release_status" {
  description = "Helm release status for the agentrax chart."
  value       = helm_release.agentrax.status
}

output "prometheus_namespace" {
  description = "Namespace where kube-prometheus-stack is deployed."
  value       = helm_release.kube_prometheus_stack.namespace
}

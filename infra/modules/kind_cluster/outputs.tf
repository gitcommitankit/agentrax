# kind_cluster module — outputs

output "kubeconfig" {
  description = "Raw kubeconfig string for the provisioned kind cluster."
  value       = kind_cluster.this.kubeconfig
  sensitive   = true
}

output "client_certificate" {
  description = "PEM-encoded client certificate for Kubernetes provider auth."
  value       = kind_cluster.this.client_certificate
  sensitive   = true
}

output "client_key" {
  description = "PEM-encoded client key for Kubernetes provider auth."
  value       = kind_cluster.this.client_key
  sensitive   = true
}

output "cluster_ca_certificate" {
  description = "PEM-encoded cluster CA certificate for Kubernetes provider auth."
  value       = kind_cluster.this.cluster_ca_certificate
  sensitive   = true
}

output "endpoint" {
  description = "Kubernetes API server endpoint for the kind cluster."
  value       = kind_cluster.this.endpoint
}

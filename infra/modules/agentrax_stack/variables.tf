# agentrax_stack module — variables

variable "cert_manager_version" {
  description = "Helm chart version for cert-manager (https://charts.jetstack.io)."
  type        = string
  default     = "v1.15.3"
}

variable "prometheus_stack_version" {
  description = "Helm chart version for kube-prometheus-stack."
  type        = string
  default     = "61.8.0"
}

variable "agentrax_chart_path" {
  description = "Local path or OCI URL to the agentrax Helm chart."
  type        = string
  # Default points to the chart in-tree relative to the Terraform working directory.
  default = "../../../charts/agentrax"
}

variable "agentrax_leader_elect" {
  description = "Enable Kubernetes leader election for the controller manager. Set true for HA (multiple replicas)."
  type        = bool
  default     = false
}

variable "agentrax_extra_values" {
  description = "Additional key=value Helm set overrides for the agentrax chart (e.g. image.tag, workloadIdentity.*)."
  type        = map(string)
  default     = {}
}

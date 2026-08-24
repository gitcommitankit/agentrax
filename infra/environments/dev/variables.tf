# dev environment — variables

variable "cluster_name" {
  description = "Name of the local kind cluster."
  type        = string
  default     = "agentrax-dev"
}

variable "cert_manager_version" {
  description = "cert-manager Helm chart version."
  type        = string
  default     = "v1.15.3"
}

variable "prometheus_stack_version" {
  description = "kube-prometheus-stack Helm chart version."
  type        = string
  default     = "61.8.0"
}

variable "agentrax_chart_path" {
  description = "Path to the agentrax Helm chart directory, relative to this environment root."
  type        = string
  default     = "../../../charts/agentrax"
}

variable "agentrax_leader_elect" {
  description = "Enable leader election for the Agentrax controller manager."
  type        = bool
  default     = false
}

variable "agentrax_extra_values" {
  description = "Additional Helm set key=value overrides for the agentrax release."
  type        = map(string)
  default     = {}
}

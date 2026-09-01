# kind_cluster module — variables

variable "cluster_name" {
  description = "Name of the kind cluster. Must be unique on the host."
  type        = string
  default     = "agentrax-dev"
}

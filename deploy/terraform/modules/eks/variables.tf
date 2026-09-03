variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "kubernetes_version" {
  description = "EKS control plane version."
  type        = string
}

variable "vpc_id" {
  description = "VPC to place the cluster in."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnets for the control plane ENIs and the node group."
  type        = list(string)
}

variable "node_instance_types" {
  description = "Candidate instance types for the managed node group."
  type        = list(string)
}

variable "node_group_min_size" {
  description = "Minimum node count."
  type        = number
}

variable "node_group_max_size" {
  description = "Maximum node count."
  type        = number
}

variable "node_group_desired_size" {
  description = "Initial node count. Ignored on subsequent applies so the cluster autoscaler owns it."
  type        = number
}

variable "endpoint_public_access" {
  description = "Whether the Kubernetes API is reachable from the internet."
  type        = bool
  default     = true
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs permitted to reach the public API endpoint."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "cluster_admin_principals" {
  description = "IAM principal ARNs granted cluster-admin via EKS access entries."
  type        = list(string)
  default     = []
}

variable "log_retention_days" {
  description = "Retention for the control plane log group."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
}

variable "availability_zones" {
  description = "AZ names to spread subnets across. One public and one private subnet is created per AZ."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "At least two availability zones are required for EKS and for ElastiCache multi-AZ."
  }
}

variable "single_nat_gateway" {
  description = "Route every private subnet through one NAT gateway instead of one per AZ. Cheaper, but that AZ becomes a single point of failure for all egress."
  type        = bool
  default     = false
}

variable "enable_flow_logs" {
  description = "Emit VPC flow logs to CloudWatch."
  type        = bool
  default     = true
}

variable "flow_log_retention_days" {
  description = "Retention for the flow log group."
  type        = number
  default     = 30
}

variable "eks_cluster_name" {
  description = "Cluster name used for the kubernetes.io/cluster/<name> subnet tags that EKS and the load balancer controller rely on for discovery."
  type        = string
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

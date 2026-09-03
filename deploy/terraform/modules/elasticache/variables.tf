variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "vpc_id" {
  description = "VPC to create the security group in."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnets for the cache subnet group. Spread across AZs so multi-AZ failover has somewhere to go."
  type        = list(string)
}

variable "allowed_security_group_ids" {
  description = "Security groups permitted to reach Redis on 6379. Normally just the EKS node/cluster security group."
  type        = list(string)
}

variable "node_type" {
  description = "ElastiCache node type."
  type        = string
  default     = "cache.t4g.medium"
}

variable "engine_version" {
  description = "Redis engine version. 7.x is required for the ACL and TLS behaviour assumed here."
  type        = string
  default     = "7.1"
}

variable "replica_count" {
  description = "Read replicas per node group."
  type        = number
  default     = 1
}

variable "multi_az_enabled" {
  description = "Place replicas in different AZs from the primary."
  type        = bool
  default     = true
}

variable "automatic_failover_enabled" {
  description = "Promote a replica automatically when the primary fails. Requires at least one replica."
  type        = bool
  default     = true
}

variable "snapshot_retention_days" {
  description = "Days of automatic snapshots. Zero disables them, which is defensible for a pure cache."
  type        = number
  default     = 0
}

variable "log_retention_days" {
  description = "Retention for the slow-log and engine-log CloudWatch groups."
  type        = number
  default     = 30
}

variable "maxmemory_policy" {
  description = "Eviction policy. allkeys-lru is correct for a cache: evict anything when full rather than start refusing writes."
  type        = string
  default     = "allkeys-lru"

  validation {
    condition = contains(
      ["allkeys-lru", "allkeys-lfu", "allkeys-random", "volatile-lru", "volatile-lfu", "volatile-random", "volatile-ttl", "noeviction"],
      var.maxmemory_policy
    )
    error_message = "maxmemory_policy must be a valid Redis eviction policy."
  }
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

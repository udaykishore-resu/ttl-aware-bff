# Root module inputs.
#
# Everything here is environment-shaped. Anything that is the same in every
# environment belongs in main.tf or in a module default, not as a variable —
# a variable nobody ever changes is just an indirection.

# ---------------------------------------------------------------------------
# Identity and placement
# ---------------------------------------------------------------------------
variable "project" {
  description = "Project slug, used as the prefix for every resource name and as the Project tag."
  type        = string
  default     = "ttl-aware-bff"

  validation {
    # Feeds into resource names that must be DNS-safe (ElastiCache replication
    # group ids, ALB names).
    condition     = can(regex("^[a-z][a-z0-9-]{2,31}$", var.project))
    error_message = "project must be 3-32 characters, lowercase alphanumeric or hyphen, starting with a letter."
  }
}

variable "environment" {
  description = "Deployment environment. Drives sizing defaults and appears in every resource name."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "region" {
  description = "AWS region for every resource in this stack."
  type        = string
  default     = "us-east-1"
}

variable "assume_role_arn" {
  description = "Optional role for the provider to assume. Leave empty to use ambient credentials."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------
variable "vpc_cidr" {
  description = "CIDR block for the VPC. Must be large enough for the pod IPs the VPC CNI hands out - a /16 gives room for roughly 65k addresses, and the CNI is generous."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block."
  }
}

variable "availability_zone_count" {
  description = "Number of AZs to span. Three is the minimum for a topologySpreadConstraint with maxSkew 1 to be meaningful and for ElastiCache multi-AZ failover."
  type        = number
  default     = 3

  validation {
    condition     = var.availability_zone_count >= 2 && var.availability_zone_count <= 6
    error_message = "availability_zone_count must be between 2 and 6."
  }
}

variable "single_nat_gateway" {
  description = "Use one NAT gateway for all AZs instead of one per AZ. Saves roughly 65 USD/month per AZ but makes that AZ a single point of failure for egress. Acceptable in dev, not in prod."
  type        = bool
  default     = false
}

variable "enable_flow_logs" {
  description = "Send VPC flow logs to CloudWatch. Useful for debugging a NetworkPolicy, and it costs per GB ingested."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# EKS
# ---------------------------------------------------------------------------
variable "kubernetes_version" {
  description = "EKS control plane version. The chart's kubeVersion floor is 1.30 because the preStop `sleep` handler is not available before it."
  type        = string
  default     = "1.31"
}

variable "node_instance_types" {
  description = "Instance types for the managed node group. Several types let the ASG fall back when one is unavailable in an AZ."
  type        = list(string)
  default     = ["m6i.large", "m6a.large", "m5.large"]
}

variable "node_group_min_size" {
  description = "Minimum nodes in the managed node group."
  type        = number
  default     = 3
}

variable "node_group_max_size" {
  description = "Maximum nodes. Must leave room for the BFF HPA ceiling plus every other workload."
  type        = number
  default     = 12
}

variable "node_group_desired_size" {
  description = "Initial node count. Ignored after the first apply so the cluster autoscaler owns it."
  type        = number
  default     = 3
}

variable "cluster_endpoint_public_access" {
  description = "Expose the Kubernetes API to the internet. Keep true only while the CI runner has no VPC access; restrict with cluster_endpoint_public_access_cidrs."
  type        = bool
  default     = true
}

variable "cluster_endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public Kubernetes API endpoint. The default is wide open and MUST be narrowed for anything real."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "cluster_admin_principals" {
  description = "IAM principal ARNs granted cluster-admin through EKS access entries. Prefer a role over a user."
  type        = list(string)
  default     = []
}

# ---------------------------------------------------------------------------
# ElastiCache (Redis, the L2 cache)
# ---------------------------------------------------------------------------
variable "redis_node_type" {
  description = "ElastiCache node type. The BFF's L2 is a cache, not a datastore: size for the working set, not for durability."
  type        = string
  default     = "cache.t4g.medium"
}

variable "redis_replica_count" {
  description = "Read replicas per shard. One replica in another AZ is what makes automatic failover possible."
  type        = number
  default     = 1

  validation {
    condition     = var.redis_replica_count >= 0 && var.redis_replica_count <= 5
    error_message = "redis_replica_count must be between 0 and 5."
  }
}

variable "redis_engine_version" {
  description = "ElastiCache Redis engine version."
  type        = string
  default     = "7.1"
}

variable "redis_snapshot_retention_days" {
  description = "Days of automatic snapshots. Zero is defensible for a pure cache and saves the storage cost."
  type        = number
  default     = 0
}

# ---------------------------------------------------------------------------
# ALB / DNS / TLS
# ---------------------------------------------------------------------------
variable "domain_name" {
  description = "Public hostname for the BFF API. Must match ingress.host in the Helm values."
  type        = string
  default     = "bff.example.com"
}

variable "route53_zone_id" {
  description = "Hosted zone for domain_name. Leave empty to skip ACM DNS validation and the alias record, and supply an existing certificate through acm_certificate_arn instead."
  type        = string
  default     = ""
}

variable "acm_certificate_arn" {
  description = "Existing ACM certificate to use instead of issuing one. Mutually exclusive with route53_zone_id."
  type        = string
  default     = ""
}

variable "enable_waf" {
  description = "Create a WAFv2 Web ACL and expose its ARN for the Ingress annotation. An internet-facing ALB should always have one."
  type        = bool
  default     = true
}

variable "waf_rate_limit_per_5min" {
  description = "WAF rate-based rule threshold per source IP over 5 minutes. This is a blunt DDoS backstop; the BFF's own per-tenant rate limiter is the precise control."
  type        = number
  default     = 20000
}

variable "alb_access_logs_retention_days" {
  description = "Lifecycle expiry for ALB access logs in S3."
  type        = number
  default     = 90
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------
variable "log_retention_days" {
  description = "CloudWatch log group retention. 30 days is the usual compromise between an investigation window and cost."
  type        = number
  default     = 30

  validation {
    condition = contains(
      [0, 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653],
      var.log_retention_days
    )
    error_message = "log_retention_days must be one of the retention periods CloudWatch accepts (0 means never expire)."
  }
}

variable "enable_xray" {
  description = "Grant the ADOT collector permission to write traces to X-Ray. Turn off if traces go somewhere else entirely."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# Kubernetes-side identifiers (must match the manifests and the chart)
# ---------------------------------------------------------------------------
variable "k8s_namespace" {
  description = "Namespace the BFF runs in. Fixed by docs/DESIGN-CONTRACT.md section 9; the IRSA trust policy pins it."
  type        = string
  default     = "bff"
}

variable "k8s_service_account" {
  description = "ServiceAccount name the IRSA role trusts. Must match serviceAccount name in the chart."
  type        = string
  default     = "ttl-aware-bff"
}

# ---------------------------------------------------------------------------
# Tagging
# ---------------------------------------------------------------------------
variable "owner" {
  description = "Team accountable for this stack. Appears as the Owner tag and drives cost allocation."
  type        = string
  default     = "platform-engineering"
}

variable "cost_center" {
  description = "Cost centre for chargeback reporting."
  type        = string
  default     = "engineering"
}

variable "additional_tags" {
  description = "Extra tags merged into the provider's default_tags and applied to everything."
  type        = map(string)
  default     = {}
}

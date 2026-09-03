variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "oidc_provider_arn" {
  description = "EKS OIDC provider ARN, from the eks module."
  type        = string
}

variable "oidc_provider_url" {
  description = "EKS OIDC issuer host+path (no scheme), from the eks module."
  type        = string
}

variable "k8s_namespace" {
  description = "Namespace the BFF runs in. The trust policy pins the role to it."
  type        = string
  default     = "bff"
}

variable "k8s_service_account" {
  description = "ServiceAccount name the role trusts. Must match the chart's serviceAccount name."
  type        = string
  default     = "ttl-aware-bff"
}

variable "secrets_manager_arns" {
  description = "Exactly the Secrets Manager secrets the BFF may read. Empty means the secrets statement is omitted entirely."
  type        = list(string)
  default     = []
}

variable "elasticache_replication_group_arn" {
  description = "Replication group the BFF may connect to. Empty omits the statement."
  type        = string
  default     = ""
}

variable "cloudwatch_log_group_arn" {
  description = "Log group the BFF may write to. Empty omits the statement."
  type        = string
  default     = ""
}

variable "enable_xray" {
  description = "Allow the BFF to write trace segments directly to X-Ray. Normally false: the SDK exports OTLP to the collector, and the collector holds the X-Ray permission."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "environment" {
  description = "Environment name, used in log group paths and alarm descriptions."
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

variable "log_retention_days" {
  description = "Retention for the log groups this module creates."
  type        = number
  default     = 30
}

variable "enable_xray" {
  description = "Attach X-Ray write permissions to the ADOT collector role."
  type        = bool
  default     = true
}

variable "adot_namespace" {
  description = "Namespace the ADOT collector runs in. The BFF's NetworkPolicy allows egress to exactly this namespace on 4317."
  type        = string
  default     = "observability"
}

variable "adot_service_account" {
  description = "ServiceAccount the ADOT collector uses. Pinned in the IRSA trust policy."
  type        = string
  default     = "adot-collector"
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

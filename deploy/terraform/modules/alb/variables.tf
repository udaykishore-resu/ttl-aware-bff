variable "name_prefix" {
  description = "Prefix for every resource name in this module."
  type        = string
}

variable "vpc_id" {
  description = "VPC the load balancer security group lives in."
  type        = string
}

variable "public_subnet_ids" {
  description = "Public subnets for the internet-facing load balancer. At least two AZs."
  type        = list(string)
}

variable "domain_name" {
  description = "Hostname the certificate covers and the alias record points at."
  type        = string
}

variable "route53_zone_id" {
  description = "Hosted zone for DNS validation and the alias record. Empty means bring your own certificate and manage DNS elsewhere."
  type        = string
  default     = ""
}

variable "existing_certificate_arn" {
  description = "Pre-existing ACM certificate to use instead of issuing one."
  type        = string
  default     = ""
}

variable "enable_waf" {
  description = "Create a WAFv2 Web ACL for the ALB."
  type        = bool
  default     = true
}

variable "waf_rate_limit_per_5min" {
  description = "Requests per source IP per 5 minutes before the rate-based rule blocks."
  type        = number
  default     = 20000
}

variable "access_logs_retention_days" {
  description = "Days before ALB access logs expire from S3."
  type        = number
  default     = 90
}

variable "tags" {
  description = "Tags applied in addition to the provider default_tags."
  type        = map(string)
  default     = {}
}

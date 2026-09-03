output "certificate_arn" {
  description = "ACM certificate ARN for alb.ingress.kubernetes.io/certificate-arn. Empty when neither route53_zone_id nor existing_certificate_arn was supplied."
  value       = local.certificate_arn
}

output "waf_web_acl_arn" {
  description = "WAFv2 Web ACL ARN for alb.ingress.kubernetes.io/wafv2-acl-arn. Empty when enable_waf is false."
  value       = var.enable_waf ? aws_wafv2_web_acl.this[0].arn : ""
}

output "waf_web_acl_id" {
  description = "WAFv2 Web ACL id."
  value       = var.enable_waf ? aws_wafv2_web_acl.this[0].id : ""
}

output "access_logs_bucket" {
  description = "S3 bucket name for access_logs.s3.bucket in the Ingress load-balancer-attributes annotation."
  value       = aws_s3_bucket.access_logs.bucket
}

output "access_logs_bucket_arn" {
  description = "Access-log bucket ARN."
  value       = aws_s3_bucket.access_logs.arn
}

output "domain_name" {
  description = "Hostname the Ingress should serve. Must match ingress.host in the Helm values."
  value       = var.domain_name
}

output "ingress_annotations" {
  description = "Ready-made annotation values for the Ingress, so the Helm values file can be filled in by copy-paste rather than by hand."
  value = {
    "alb.ingress.kubernetes.io/certificate-arn" = local.certificate_arn
    "alb.ingress.kubernetes.io/wafv2-acl-arn"   = var.enable_waf ? aws_wafv2_web_acl.this[0].arn : ""
    "access_logs.s3.bucket"                     = aws_s3_bucket.access_logs.bucket
  }
}

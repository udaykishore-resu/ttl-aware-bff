output "application_log_group_name" {
  description = "CloudWatch log group for BFF application logs."
  value       = aws_cloudwatch_log_group.application.name
}

output "application_log_group_arn" {
  description = "Application log group ARN. The BFF's IRSA policy scopes its logs:PutLogEvents to this."
  value       = aws_cloudwatch_log_group.application.arn
}

output "adot_collector_log_group_name" {
  description = "CloudWatch log group for the ADOT collector's own logs."
  value       = aws_cloudwatch_log_group.adot_collector.name
}

output "performance_log_group_name" {
  description = "Container Insights performance log group."
  value       = aws_cloudwatch_log_group.performance.name
}

output "adot_collector_role_arn" {
  description = "IRSA role ARN for the ADOT collector. Put this on the collector's ServiceAccount as eks.amazonaws.com/role-arn."
  value       = aws_iam_role.adot_collector.arn
}

output "adot_collector_service_account" {
  description = "Namespace/name pair the ADOT role trusts. Anything else is refused by the trust policy."
  value       = "${var.adot_namespace}/${var.adot_service_account}"
}

output "amp_remote_write_policy_arn" {
  description = "Policy granting Amazon Managed Prometheus remote write. Attach to the collector role when AMP is the metric backend."
  value       = aws_iam_policy.adot_prometheus_remote_write.arn
}

output "jwt_secret_arn" {
  description = "Secrets Manager ARN for the HS256 JWT secret. Referenced by the BFF's ExternalSecret and by its IRSA policy."
  value       = aws_secretsmanager_secret.jwt.arn
}

output "jwt_secret_name" {
  description = "Secrets Manager secret name, as written in deploy/k8s/secret.example.yaml."
  value       = aws_secretsmanager_secret.jwt.name
}

output "otlp_endpoint" {
  description = "In-cluster OTLP/gRPC endpoint the BFF exports to. Matches otel.endpoint in the Helm values and the NetworkPolicy egress rule."
  value       = "adot-collector.${var.adot_namespace}.svc.cluster.local:4317"
}

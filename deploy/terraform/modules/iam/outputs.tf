output "role_arn" {
  description = "IRSA role ARN. Put this on the BFF ServiceAccount as eks.amazonaws.com/role-arn (serviceAccount.roleArn in the Helm values)."
  value       = aws_iam_role.bff.arn
}

output "role_name" {
  description = "IRSA role name."
  value       = aws_iam_role.bff.name
}

output "policy_arn" {
  description = "ARN of the least-privilege policy attached to the role."
  value       = aws_iam_policy.bff.arn
}

output "trusted_service_account" {
  description = "The exact OIDC subject the trust policy accepts. If the ServiceAccount annotation does not resolve to this, credential exchange fails with AccessDenied and no other explanation."
  value       = local.subject
}

output "service_account_annotation" {
  description = "Ready-made annotation for the BFF ServiceAccount."
  value = {
    "eks.amazonaws.com/role-arn"               = aws_iam_role.bff.arn
    "eks.amazonaws.com/sts-regional-endpoints" = "true"
  }
}

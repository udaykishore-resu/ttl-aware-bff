output "cluster_name" {
  description = "EKS cluster name."
  value       = aws_eks_cluster.this.name
}

output "cluster_arn" {
  description = "EKS cluster ARN."
  value       = aws_eks_cluster.this.arn
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64-encoded CA certificate for the API server. Feed to the kubernetes/helm providers."
  value       = aws_eks_cluster.this.certificate_authority[0].data
}

output "cluster_version" {
  description = "Control plane Kubernetes version."
  value       = aws_eks_cluster.this.version
}

output "cluster_security_group_id" {
  description = "The cluster security group EKS manages. Attached to every managed node."
  value       = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
}

output "node_security_group_id" {
  description = "Security group to reference from other modules that must accept traffic from the nodes (ElastiCache does exactly this)."
  value       = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
}

output "oidc_provider_arn" {
  description = "IAM OIDC provider ARN. Every IRSA role's trust policy federates against this."
  value       = aws_iam_openid_connect_provider.this.arn
}

output "oidc_provider_url" {
  description = "OIDC issuer URL without its scheme, ready for use as an IAM condition key prefix."
  value       = local.oidc_host
}

output "node_role_arn" {
  description = "IAM role the nodes assume. Nothing application-level should rely on this - that is what IRSA is for."
  value       = aws_iam_role.node.arn
}

output "kms_key_arn" {
  description = "KMS key envelope-encrypting Kubernetes Secrets."
  value       = aws_kms_key.eks.arn
}

output "kubeconfig_command" {
  description = "Command that writes a working kubeconfig entry for this cluster."
  value       = "aws eks update-kubeconfig --name ${aws_eks_cluster.this.name} --region ${data.aws_region.current.name}"
}

data "aws_region" "current" {}

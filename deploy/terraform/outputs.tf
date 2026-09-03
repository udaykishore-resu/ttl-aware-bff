# Root outputs.
#
# These are the values that connect the infrastructure to the application. The
# `helm_values` and `kustomize_substitutions` outputs at the bottom exist so
# that wiring the two together is a copy-paste rather than a scavenger hunt
# across six module outputs.

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------
output "vpc_id" {
  description = "VPC id."
  value       = module.vpc.vpc_id
}

output "vpc_cidr" {
  description = "VPC CIDR. Use for networkPolicy.clusterCidr in the Helm values."
  value       = module.vpc.vpc_cidr
}

output "private_subnet_ids" {
  description = "Private subnet ids."
  value       = module.vpc.private_subnet_ids
}

output "private_subnet_cidrs" {
  description = "Private subnet CIDRs. Prefer these over the whole VPC CIDR when narrowing the NetworkPolicy ipBlock rules."
  value       = module.vpc.private_subnet_cidrs
}

output "public_subnet_ids" {
  description = "Public subnet ids, where the ALB is placed."
  value       = module.vpc.public_subnet_ids
}

output "nat_gateway_public_ips" {
  description = "Egress IPs. Give these to any upstream that maintains an allow-list."
  value       = module.vpc.nat_gateway_public_ips
}

# ---------------------------------------------------------------------------
# EKS
# ---------------------------------------------------------------------------
output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_version" {
  description = "Control plane version."
  value       = module.eks.cluster_version
}

output "oidc_provider_arn" {
  description = "IAM OIDC provider ARN backing IRSA."
  value       = module.eks.oidc_provider_arn
}

output "kubeconfig_command" {
  description = "Run this to point kubectl at the cluster."
  value       = module.eks.kubeconfig_command
}

# ---------------------------------------------------------------------------
# Cache
# ---------------------------------------------------------------------------
output "elasticache_primary_endpoint" {
  description = "Redis primary endpoint. Set as externalRedis.host in values-prod.yaml."
  value       = module.elasticache.primary_endpoint
}

output "elasticache_endpoint_with_port" {
  description = "host:port form, ready for cache.redis.addr / BFF_CACHE__REDIS__ADDR."
  value       = module.elasticache.primary_endpoint_with_port
}

output "elasticache_auth_token_secret_name" {
  description = "Secrets Manager path the ExternalSecret reads the AUTH token from."
  value       = module.elasticache.auth_token_secret_name
}

# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------
output "acm_certificate_arn" {
  description = "Certificate for alb.ingress.kubernetes.io/certificate-arn."
  value       = module.alb.certificate_arn
}

output "waf_web_acl_arn" {
  description = "Web ACL for alb.ingress.kubernetes.io/wafv2-acl-arn."
  value       = module.alb.waf_web_acl_arn
}

output "alb_access_logs_bucket" {
  description = "S3 bucket for the ALB's access_logs.s3.bucket attribute."
  value       = module.alb.access_logs_bucket
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------
output "adot_collector_role_arn" {
  description = "IRSA role for the ADOT collector's ServiceAccount."
  value       = module.observability.adot_collector_role_arn
}

output "otlp_endpoint" {
  description = "In-cluster OTLP/gRPC endpoint. Matches otel.endpoint in the Helm values."
  value       = module.observability.otlp_endpoint
}

output "application_log_group_name" {
  description = "CloudWatch log group for BFF application logs."
  value       = module.observability.application_log_group_name
}

output "jwt_secret_name" {
  description = "Secrets Manager path for the HS256 JWT secret. Populate it out of band; Terraform creates the container, not the value."
  value       = module.observability.jwt_secret_name
}

# ---------------------------------------------------------------------------
# Application identity
# ---------------------------------------------------------------------------
output "bff_irsa_role_arn" {
  description = "IRSA role for the BFF. Set as serviceAccount.roleArn in the Helm values, or in the eks.amazonaws.com/role-arn annotation in deploy/k8s/serviceaccount.yaml."
  value       = module.iam.role_arn
}

output "bff_trusted_service_account" {
  description = "The OIDC subject the BFF role trusts. A mismatch here is the usual cause of an AccessDenied with no further detail."
  value       = module.iam.trusted_service_account
}

# ---------------------------------------------------------------------------
# Ready-made application wiring
# ---------------------------------------------------------------------------
output "helm_values" {
  description = "Values to merge into values-prod.yaml. Render with: terraform output -json helm_values | yq -P"
  value = {
    serviceAccount = {
      roleArn = module.iam.role_arn
    }
    redis = {
      enabled = false
    }
    externalRedis = {
      host        = module.elasticache.primary_endpoint
      port        = module.elasticache.port
      passwordEnv = "BFF_REDIS_PASSWORD"
      tls         = module.elasticache.transit_encryption_enabled
    }
    otel = {
      endpoint = module.observability.otlp_endpoint
    }
    ingress = {
      host = module.alb.domain_name
      alb = {
        certificateArn   = module.alb.certificate_arn
        wafAclArn        = module.alb.waf_web_acl_arn
        accessLogsBucket = module.alb.access_logs_bucket
      }
    }
    networkPolicy = {
      clusterCidr = module.vpc.vpc_cidr
    }
  }
}

output "kustomize_substitutions" {
  description = "Placeholder values in deploy/k8s that this stack replaces. Feed to `kustomize edit` or a sed pass in CI."
  value = {
    "eks.amazonaws.com/role-arn"                = module.iam.role_arn
    "alb.ingress.kubernetes.io/certificate-arn" = module.alb.certificate_arn
    "alb.ingress.kubernetes.io/wafv2-acl-arn"   = module.alb.waf_web_acl_arn
    "BFF_CACHE__REDIS__ADDR"                    = module.elasticache.primary_endpoint_with_port
    "BFF_OBSERVABILITY__OTLP__ENDPOINT"         = module.observability.otlp_endpoint
  }
}

output "account_id" {
  description = "AWS account this stack was applied into."
  value       = data.aws_caller_identity.current.account_id
}

output "region" {
  description = "Region this stack was applied into. Used by the kubeconfig command in the README."
  value       = var.region
}

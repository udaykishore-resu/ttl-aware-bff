# Provider and Terraform version constraints for the whole stack.
#
# Pinning policy:
#   - terraform: a floor, not a ceiling. New minor versions are safe.
#   - providers: pessimistic on the minor version (~> 5.80). The AWS provider
#     ships breaking-ish behaviour changes in minors often enough that an
#     unbounded constraint turns `terraform init` into a lottery.
#   - .terraform.lock.hcl is committed, so these ranges only matter when the
#     lock is deliberately refreshed with `terraform init -upgrade`.

terraform {
  required_version = ">= 1.9.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.80"
    }

    # Used to bootstrap in-cluster objects that must exist before Helm runs:
    # the `bff` namespace and the aws-auth-equivalent access entries.
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }

    # Installs the cluster add-ons that are prerequisites for the application
    # chart: the AWS Load Balancer Controller, External Secrets, the metrics
    # server. The application itself is NOT deployed by Terraform — that is
    # CI's job (see .github/workflows/ci.yaml), so that a code deploy never
    # requires infrastructure credentials.
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.17"
    }

    # Generates the ElastiCache AUTH token.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }

    # Reads the OIDC provider's certificate thumbprint for the IRSA trust
    # relationship.
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

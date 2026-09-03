# Provider configuration.
#
# default_tags is the single most valuable thing in this file: every taggable
# resource created by the AWS provider inherits these, so cost allocation and
# ownership work without every module remembering to tag.

provider "aws" {
  region = var.region

  # Assume a dedicated deploy role when one is given, so that the apply is
  # auditable independently of whoever's credentials invoked it.
  dynamic "assume_role" {
    for_each = var.assume_role_arn == "" ? [] : [var.assume_role_arn]
    content {
      role_arn     = assume_role.value
      session_name = "terraform-${var.project}-${var.environment}"
    }
  }

  default_tags {
    tags = merge(
      {
        Project     = var.project
        Environment = var.environment
        Owner       = var.owner
        CostCenter  = var.cost_center
        ManagedBy   = "terraform"
        Repository  = "github.com/udaykishore/ttl-aware-bff"
        # Points a reader from a mystery resource back to the code that made it.
        StackPath = "deploy/terraform"
      },
      var.additional_tags,
    )
  }
}

# Second AWS provider aliased to us-east-1. CloudFront and some global services
# require certificates there; kept even when unused so adding a CDN later does
# not need a provider refactor.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  dynamic "assume_role" {
    for_each = var.assume_role_arn == "" ? [] : [var.assume_role_arn]
    content {
      role_arn     = assume_role.value
      session_name = "terraform-${var.project}-${var.environment}"
    }
  }

  default_tags {
    tags = merge(
      {
        Project     = var.project
        Environment = var.environment
        Owner       = var.owner
        CostCenter  = var.cost_center
        ManagedBy   = "terraform"
        Repository  = "github.com/udaykishore/ttl-aware-bff"
        StackPath   = "deploy/terraform"
      },
      var.additional_tags,
    )
  }
}

# ---------------------------------------------------------------------------
# Kubernetes and Helm
# ---------------------------------------------------------------------------
# Both authenticate with a token minted by `aws eks get-token` at apply time.
# The `exec` plugin is used rather than a static token because a static one
# expires after 15 minutes, which is shorter than a real apply.
#
# Known sharp edge: these providers are configured from the EKS module's
# outputs, so a single `terraform apply` that creates the cluster AND uses
# these providers requires the outputs to be known at plan time. That is why
# the README prescribes a two-phase apply for a greenfield environment
# (-target the VPC and EKS modules first).

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args = [
      "eks", "get-token",
      "--cluster-name", module.eks.cluster_name,
      "--region", var.region,
    ]
  }
}

provider "helm" {
  kubernetes {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)

    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args = [
        "eks", "get-token",
        "--cluster-name", module.eks.cluster_name,
        "--region", var.region,
      ]
    }
  }
}

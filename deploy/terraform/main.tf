# Root module: composes the six building blocks. There is deliberately no
# resource declared here — everything lives in a module so that each concern
# can be reviewed, tested and reused independently.
#
# Dependency order (Terraform infers all of it from the references below):
#
#   vpc ──┬──> eks ──┬──> iam (needs the OIDC provider)
#         │          └──> observability (needs the OIDC provider)
#         ├──> elasticache (needs subnets + the EKS node security group)
#         └──> alb (needs public subnets)
#
# See deploy/terraform/README.md for the apply order on a greenfield account.

locals {
  # One name prefix, used everywhere. Keeping the interpolation here rather
  # than in each module means a rename is a one-line change.
  name_prefix = "${var.project}-${var.environment}"

  # Tags beyond the provider's default_tags, for the places that need them
  # explicitly (autoscaling group tag propagation, S3 lifecycle rules).
  tags = merge(
    {
      Project     = var.project
      Environment = var.environment
      Owner       = var.owner
      CostCenter  = var.cost_center
      ManagedBy   = "terraform"
    },
    var.additional_tags,
  )

  # prod gets the resilient shape; lower environments get the cheap one.
  is_production = var.environment == "prod"
}

data "aws_caller_identity" "current" {}

data "aws_availability_zones" "available" {
  state = "available"

  # Local Zones and Wavelength Zones cannot host EKS node groups or
  # ElastiCache, so they are excluded rather than discovered and then failed on.
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

# ---------------------------------------------------------------------------
# 1. Network
# ---------------------------------------------------------------------------
module "vpc" {
  source = "./modules/vpc"

  name_prefix = local.name_prefix
  vpc_cidr    = var.vpc_cidr

  # Take the first N AZs the region offers, deterministically.
  availability_zones = slice(
    data.aws_availability_zones.available.names,
    0,
    var.availability_zone_count,
  )

  # One NAT gateway per AZ in production: a shared gateway makes one AZ's
  # failure an egress outage for every pod in the VPC.
  single_nat_gateway = local.is_production ? false : var.single_nat_gateway

  enable_flow_logs        = var.enable_flow_logs
  flow_log_retention_days = var.log_retention_days

  # Tags the subnets with the kubernetes.io/role/* keys the AWS Load Balancer
  # Controller uses for auto-discovery, and with the cluster ownership tag.
  eks_cluster_name = "${local.name_prefix}-eks"

  tags = local.tags
}

# ---------------------------------------------------------------------------
# 2. Kubernetes control plane and nodes
# ---------------------------------------------------------------------------
module "eks" {
  source = "./modules/eks"

  name_prefix        = local.name_prefix
  kubernetes_version = var.kubernetes_version

  vpc_id             = module.vpc.vpc_id
  private_subnet_ids = module.vpc.private_subnet_ids

  node_instance_types     = var.node_instance_types
  node_group_min_size     = var.node_group_min_size
  node_group_max_size     = var.node_group_max_size
  node_group_desired_size = var.node_group_desired_size

  endpoint_public_access       = var.cluster_endpoint_public_access
  endpoint_public_access_cidrs = var.cluster_endpoint_public_access_cidrs
  cluster_admin_principals     = var.cluster_admin_principals

  log_retention_days = var.log_retention_days

  tags = local.tags
}

# ---------------------------------------------------------------------------
# 3. L2 cache
# ---------------------------------------------------------------------------
module "elasticache" {
  source = "./modules/elasticache"

  name_prefix = local.name_prefix

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnet_ids
  # Only the EKS nodes may reach Redis; the module builds a security group
  # whose sole ingress rule references this one.
  allowed_security_group_ids = [module.eks.node_security_group_id]

  node_type      = var.redis_node_type
  engine_version = var.redis_engine_version
  replica_count  = var.redis_replica_count

  # Multi-AZ needs at least one replica, and automatic failover needs multi-AZ.
  multi_az_enabled           = var.redis_replica_count > 0
  automatic_failover_enabled = var.redis_replica_count > 0

  snapshot_retention_days = var.redis_snapshot_retention_days
  log_retention_days      = var.log_retention_days

  tags = local.tags
}

# ---------------------------------------------------------------------------
# 4. Public entry point
# ---------------------------------------------------------------------------
module "alb" {
  source = "./modules/alb"

  name_prefix = local.name_prefix

  vpc_id            = module.vpc.vpc_id
  public_subnet_ids = module.vpc.public_subnet_ids

  domain_name              = var.domain_name
  route53_zone_id          = var.route53_zone_id
  existing_certificate_arn = var.acm_certificate_arn

  enable_waf              = var.enable_waf
  waf_rate_limit_per_5min = var.waf_rate_limit_per_5min

  access_logs_retention_days = var.alb_access_logs_retention_days

  tags = local.tags
}

# ---------------------------------------------------------------------------
# 5. Telemetry plumbing
# ---------------------------------------------------------------------------
module "observability" {
  source = "./modules/observability"

  name_prefix = local.name_prefix
  environment = var.environment

  oidc_provider_arn = module.eks.oidc_provider_arn
  oidc_provider_url = module.eks.oidc_provider_url

  log_retention_days = var.log_retention_days
  enable_xray        = var.enable_xray

  # The ADOT collector runs in the observability namespace; the chart's
  # NetworkPolicy allows egress to exactly that namespace on 4317.
  adot_namespace       = "observability"
  adot_service_account = "adot-collector"

  tags = local.tags
}

# ---------------------------------------------------------------------------
# 6. Application identity (IRSA)
# ---------------------------------------------------------------------------
module "iam" {
  source = "./modules/iam"

  name_prefix = local.name_prefix

  oidc_provider_arn = module.eks.oidc_provider_arn
  oidc_provider_url = module.eks.oidc_provider_url

  # The trust policy pins the role to exactly this namespace and
  # ServiceAccount, so a pod in another namespace cannot assume it even with a
  # valid projected token.
  k8s_namespace       = var.k8s_namespace
  k8s_service_account = var.k8s_service_account

  # Least privilege: the role can read exactly these two secrets, connect to
  # exactly this replication group, and write to exactly this log group.
  secrets_manager_arns = [
    module.elasticache.auth_token_secret_arn,
    module.observability.jwt_secret_arn,
  ]
  elasticache_replication_group_arn = module.elasticache.replication_group_arn
  cloudwatch_log_group_arn          = module.observability.application_log_group_arn

  enable_xray = var.enable_xray

  tags = local.tags
}

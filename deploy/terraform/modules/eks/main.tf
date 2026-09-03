# EKS control plane, one managed node group, the IRSA OIDC provider and the
# add-ons every workload here depends on.
#
# Two things in this module matter more than the rest:
#
#   1. aws_iam_openid_connect_provider — without it, IRSA does not exist and
#      every pod falls back to the node's instance profile. That is the single
#      biggest privilege-escalation shortcut in a badly built EKS cluster.
#   2. authentication_mode = API — EKS access entries replace the aws-auth
#      ConfigMap. Editing that ConfigMap wrongly used to be the classic way to
#      lock everyone out of a cluster irrecoverably.

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}

locals {
  cluster_name = "${var.name_prefix}-eks"
}

# ---------------------------------------------------------------------------
# Control plane logging
# ---------------------------------------------------------------------------
# Created explicitly so retention and tags are managed. If EKS creates it
# implicitly it defaults to never expire, which is an unbounded bill.
resource "aws_cloudwatch_log_group" "cluster" {
  name              = "/aws/eks/${local.cluster_name}/cluster"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name = "${local.cluster_name}-control-plane-logs"
  })
}

# ---------------------------------------------------------------------------
# Cluster IAM role
# ---------------------------------------------------------------------------
resource "aws_iam_role" "cluster" {
  name = "${local.cluster_name}-cluster"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "cluster_policy" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSClusterPolicy"
}

# Lets EKS manage the security group rules for the control plane ENIs.
resource "aws_iam_role_policy_attachment" "cluster_vpc_resource_controller" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSVPCResourceController"
}

# ---------------------------------------------------------------------------
# Control plane
# ---------------------------------------------------------------------------
resource "aws_eks_cluster" "this" {
  name     = local.cluster_name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  # EKS access entries rather than the aws-auth ConfigMap. bootstrap_cluster_
  # creator_admin_permissions keeps whoever ran the first apply as an admin, so
  # a mistake in cluster_admin_principals is recoverable.
  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  vpc_config {
    subnet_ids = var.private_subnet_ids

    # Private access is always on: in-cluster components (and the node kubelets)
    # then reach the API over the VPC rather than out through the NAT gateway.
    endpoint_private_access = true
    endpoint_public_access  = var.endpoint_public_access
    public_access_cidrs     = var.endpoint_public_access_cidrs
  }

  # audit and authenticator are the two that actually get used in an incident;
  # api and controllerManager/scheduler are cheap enough to keep on.
  enabled_cluster_log_types = [
    "api",
    "audit",
    "authenticator",
    "controllerManager",
    "scheduler",
  ]

  # Envelope-encrypt Secrets at rest with a customer-managed key, on top of the
  # EBS-level encryption etcd already has.
  encryption_config {
    provider {
      key_arn = aws_kms_key.eks.arn
    }
    resources = ["secrets"]
  }

  tags = merge(var.tags, {
    Name = local.cluster_name
  })

  depends_on = [
    aws_iam_role_policy_attachment.cluster_policy,
    aws_iam_role_policy_attachment.cluster_vpc_resource_controller,
    aws_cloudwatch_log_group.cluster,
  ]
}

# ---------------------------------------------------------------------------
# KMS key for Secret envelope encryption
# ---------------------------------------------------------------------------
resource "aws_kms_key" "eks" {
  description         = "Envelope encryption for ${local.cluster_name} Kubernetes Secrets"
  enable_key_rotation = true
  # 30 days of recovery time: deleting this key makes every Secret in etcd
  # permanently unreadable, so the window is deliberately long.
  deletion_window_in_days = 30

  tags = merge(var.tags, {
    Name = "${local.cluster_name}-secrets"
  })
}

resource "aws_kms_alias" "eks" {
  name          = "alias/${local.cluster_name}-secrets"
  target_key_id = aws_kms_key.eks.key_id
}

# ---------------------------------------------------------------------------
# IRSA: OIDC identity provider
# ---------------------------------------------------------------------------
data "tls_certificate" "oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "this" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]

  tags = merge(var.tags, {
    Name = "${local.cluster_name}-oidc"
  })
}

# ---------------------------------------------------------------------------
# Node group IAM role
# ---------------------------------------------------------------------------
resource "aws_iam_role" "node" {
  name = "${local.cluster_name}-node"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "AmazonEKSWorkerNodePolicy",
    "AmazonEKS_CNI_Policy",
    # Pull images from ECR. GHCR needs no AWS permission.
    "AmazonEC2ContainerRegistryReadOnly",
    # SSM Session Manager instead of SSH. No bastion, no key pairs, no port 22.
    "AmazonSSMManagedInstanceCore",
  ])

  role       = aws_iam_role.node.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/${each.value}"
}

# ---------------------------------------------------------------------------
# Managed node group
# ---------------------------------------------------------------------------
resource "aws_eks_node_group" "general" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${var.name_prefix}-general"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.private_subnet_ids

  # Several instance types so the ASG can fall back when one is unavailable in
  # an AZ, which is otherwise a common cause of a stuck scale-up.
  instance_types = var.node_instance_types
  capacity_type  = "ON_DEMAND"
  ami_type       = "AL2023_x86_64_STANDARD"
  disk_size      = 50

  scaling_config {
    min_size     = var.node_group_min_size
    max_size     = var.node_group_max_size
    desired_size = var.node_group_desired_size
  }

  update_config {
    # Replace at most a quarter of the group at a time during an AMI or version
    # update, so the PDB has room to evict without blocking.
    max_unavailable_percentage = 25
  }

  labels = {
    # Matched by the Deployment's preferred nodeAffinity.
    "workload-class" = "general"
  }

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-general"
    # Lets the cluster autoscaler discover and manage this group.
    "k8s.io/cluster-autoscaler/enabled"               = "true"
    "k8s.io/cluster-autoscaler/${local.cluster_name}" = "owned"
  })

  lifecycle {
    # The cluster autoscaler owns desired_size after the first apply. Without
    # this, every `terraform apply` would scale the cluster back down.
    ignore_changes = [scaling_config[0].desired_size]
  }

  depends_on = [aws_iam_role_policy_attachment.node]
}

# ---------------------------------------------------------------------------
# Cluster add-ons
# ---------------------------------------------------------------------------
# Managed add-ons rather than manifests: AWS keeps them compatible with the
# control plane version, which is the whole point.
resource "aws_eks_addon" "this" {
  for_each = {
    # Order matters conceptually (CNI before CoreDNS before kube-proxy) but EKS
    # handles it; the map is alphabetical for readability.
    coredns = {
      # Every pod in the cluster depends on DNS, and the BFF resolves three
      # upstreams on the request path, so CoreDNS runs at least two replicas
      # spread across nodes.
      configuration_values = jsonencode({
        replicaCount = 2
        podDisruptionBudget = {
          maxUnavailable = 1
        }
      })
    }
    kube-proxy = { configuration_values = null }
    vpc-cni = {
      configuration_values = jsonencode({
        env = {
          # NetworkPolicy enforcement. Without this the manifests in
          # deploy/k8s/networkpolicy.yaml are accepted and silently ignored.
          ENABLE_NETWORK_POLICY = "true"
          # Prefix delegation: assigns /28 prefixes instead of individual IPs,
          # which multiplies the pods-per-node ceiling and is what keeps a
          # large node group from exhausting the subnet.
          ENABLE_PREFIX_DELEGATION = "true"
          WARM_PREFIX_TARGET       = "1"
        }
      })
    }
    eks-pod-identity-agent = { configuration_values = null }
  }

  cluster_name = aws_eks_cluster.this.name
  addon_name   = each.key

  # Let AWS pick the version that matches the control plane rather than pinning
  # one that will drift out of support.
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  configuration_values = each.value.configuration_values

  tags = var.tags

  depends_on = [aws_eks_node_group.general]
}

# EBS CSI driver gets its own resource because it needs an IRSA role.
resource "aws_eks_addon" "ebs_csi" {
  cluster_name             = aws_eks_cluster.this.name
  addon_name               = "aws-ebs-csi-driver"
  service_account_role_arn = aws_iam_role.ebs_csi.arn

  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  tags = var.tags

  depends_on = [aws_eks_node_group.general]
}

resource "aws_iam_role" "ebs_csi" {
  name = "${local.cluster_name}-ebs-csi"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.this.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_host}:aud" = "sts.amazonaws.com"
          "${local.oidc_host}:sub" = "system:serviceaccount:kube-system:ebs-csi-controller-sa"
        }
      }
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

locals {
  # The OIDC issuer URL without its scheme: IAM condition keys are written
  # against the host+path, not the full URL.
  oidc_host = replace(aws_eks_cluster.this.identity[0].oidc[0].issuer, "https://", "")
}

# ---------------------------------------------------------------------------
# Access entries
# ---------------------------------------------------------------------------
resource "aws_eks_access_entry" "admins" {
  for_each = toset(var.cluster_admin_principals)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  type          = "STANDARD"

  tags = var.tags
}

resource "aws_eks_access_policy_association" "admins" {
  for_each = toset(var.cluster_admin_principals)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  policy_arn    = "arn:${data.aws_partition.current.partition}:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.admins]
}

# ---------------------------------------------------------------------------
# Node security group rule for the BFF admin port
# ---------------------------------------------------------------------------
# The cluster security group EKS creates already allows node-to-node traffic.
# This rule exists so the ALB (which is outside it) can reach the admin port
# for its target-group health check on /readyz.
resource "aws_vpc_security_group_ingress_rule" "admin_health_check" {
  security_group_id = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id

  description = "ALB health checks against the BFF admin port"
  from_port   = 9090
  to_port     = 9090
  ip_protocol = "tcp"
  # Restricted to the VPC: the ALB's ENIs live in the public subnets.
  cidr_ipv4 = data.aws_vpc.selected.cidr_block

  tags = merge(var.tags, {
    Name = "${local.cluster_name}-admin-healthcheck"
  })
}

data "aws_vpc" "selected" {
  id = var.vpc_id
}

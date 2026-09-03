# VPC, subnets, gateways and routing for the BFF stack.
#
# Layout, for a /16 and three AZs:
#
#   public   10.0.0.0/20,  10.0.16.0/20, 10.0.32.0/20   ALB, NAT gateways
#   private  10.0.64.0/19, 10.0.96.0/19, 10.0.128.0/19  EKS nodes, pods, Redis
#
# Private subnets are deliberately much larger. The AWS VPC CNI assigns a real
# VPC IP to every pod, and it pre-warms whole ENIs' worth of them, so pod IP
# exhaustion is the most common way an EKS cluster stops being able to schedule.
# A /19 per AZ gives ~8,000 addresses each.

locals {
  az_count = length(var.availability_zones)

  # newbits 4 on a /16 -> /20 blocks of 4,096 addresses for the public tier.
  public_subnet_cidrs = [
    for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i)
  ]

  # newbits 3 -> /19 blocks. Offset by 2 so the private range starts at the
  # halfway point of the VPC and cannot collide with the public /20s above
  # regardless of AZ count.
  private_subnet_cidrs = [
    for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 3, i + 2)
  ]

  nat_gateway_count = var.single_nat_gateway ? 1 : local.az_count
}

# ---------------------------------------------------------------------------
# VPC
# ---------------------------------------------------------------------------
resource "aws_vpc" "this" {
  cidr_block = var.vpc_cidr

  # Both required by EKS: pods resolve Service and ECR endpoints by DNS name.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-vpc"
  })
}

# ---------------------------------------------------------------------------
# Gateways
# ---------------------------------------------------------------------------
resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-igw"
  })
}

resource "aws_eip" "nat" {
  count  = local.nat_gateway_count
  domain = "vpc"

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-nat-eip-${count.index + 1}"
  })

  # The EIP is useless until the IGW exists, and destroying them in the wrong
  # order leaves a dangling association.
  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_gateway_count

  allocation_id = aws_eip.nat[count.index].id
  # NAT gateways live in the PUBLIC subnet and serve the private one.
  subnet_id = aws_subnet.public[count.index].id

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-nat-${count.index + 1}"
  })

  depends_on = [aws_internet_gateway.this]
}

# ---------------------------------------------------------------------------
# Subnets
# ---------------------------------------------------------------------------
resource "aws_subnet" "public" {
  count = local.az_count

  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_subnet_cidrs[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = true

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-public-${var.availability_zones[count.index]}"
    Tier = "public"

    # Discovery tags. The AWS Load Balancer Controller finds subnets for an
    # internet-facing ALB by this exact key; without it, an Ingress fails with
    # "unable to discover at least one subnet".
    "kubernetes.io/role/elb"                       = "1"
    "kubernetes.io/cluster/${var.eks_cluster_name}" = "shared"
  })
}

resource "aws_subnet" "private" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-private-${var.availability_zones[count.index]}"
    Tier = "private"

    # Same idea for internal load balancers.
    "kubernetes.io/role/internal-elb"               = "1"
    "kubernetes.io/cluster/${var.eks_cluster_name}" = "shared"
  })
}

# ---------------------------------------------------------------------------
# Routing
# ---------------------------------------------------------------------------
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-public-rt"
  })
}

resource "aws_route_table_association" "public" {
  count = local.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# One route table per private subnet, so each AZ can point at its own NAT
# gateway. With single_nat_gateway they all point at the same one.
resource "aws_route_table" "private" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
  }

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-private-rt-${var.availability_zones[count.index]}"
  })
}

resource "aws_route_table_association" "private" {
  count = local.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# ---------------------------------------------------------------------------
# VPC endpoints
# ---------------------------------------------------------------------------
# S3 gateway endpoint. Free, and it keeps ECR layer pulls (which are S3 GETs)
# off the NAT gateway. On a cluster that pulls images all day this is the single
# largest NAT data-processing saving available.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = aws_route_table.private[*].id

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-s3-endpoint"
  })
}

data "aws_region" "current" {}

# ---------------------------------------------------------------------------
# Flow logs
# ---------------------------------------------------------------------------
# Invaluable when a NetworkPolicy silently drops traffic: the flow log shows the
# REJECT that the application only sees as a timeout.
resource "aws_cloudwatch_log_group" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name              = "/aws/vpc/${var.name_prefix}/flow-logs"
  retention_in_days = var.flow_log_retention_days

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-flow-logs"
  })
}

resource "aws_iam_role" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name = "${var.name_prefix}-vpc-flow-logs"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "vpc-flow-logs.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name = "${var.name_prefix}-vpc-flow-logs"
  role = aws_iam_role.flow_logs[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "logs:CreateLogStream",
        "logs:PutLogEvents",
        "logs:DescribeLogGroups",
        "logs:DescribeLogStreams",
      ]
      # Scoped to this one log group rather than "*".
      Resource = "${aws_cloudwatch_log_group.flow_logs[0].arn}:*"
    }]
  })
}

resource "aws_flow_log" "this" {
  count = var.enable_flow_logs ? 1 : 0

  vpc_id          = aws_vpc.this.id
  traffic_type    = "ALL"
  iam_role_arn    = aws_iam_role.flow_logs[0].arn
  log_destination = aws_cloudwatch_log_group.flow_logs[0].arn

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-flow-log"
  })
}

# ---------------------------------------------------------------------------
# Default security group lockdown
# ---------------------------------------------------------------------------
# AWS creates a default SG that allows all traffic between its members. It
# cannot be deleted, so it is emptied instead: anything that accidentally lands
# in it then has no connectivity at all, which surfaces the mistake immediately.
resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-default-sg-DO-NOT-USE"
  })
}

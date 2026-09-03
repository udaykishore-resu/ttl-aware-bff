# IRSA role for the BFF's ServiceAccount.
#
# The trust policy is the security boundary that matters. Two conditions,
# both required:
#
#   <issuer>:aud = sts.amazonaws.com
#       Without it, ANY token this OIDC provider signs is accepted, including
#       tokens minted for a different audience entirely.
#
#   <issuer>:sub = system:serviceaccount:<namespace>:<serviceaccount>
#       Without it, every ServiceAccount in the cluster can assume this role.
#       This is the single most common IRSA misconfiguration.
#
# StringEquals, not StringLike: a wildcard in :sub (`system:serviceaccount:bff:*`)
# would let any pod in the namespace assume the role, which defeats the point of
# having a per-workload identity.

data "aws_partition" "current" {}
data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

locals {
  role_name = "${var.name_prefix}-irsa"

  # Rendered into the trust policy and echoed in an output so the annotation on
  # the ServiceAccount can be checked against what the role actually trusts.
  subject = "system:serviceaccount:${var.k8s_namespace}:${var.k8s_service_account}"
}

resource "aws_iam_role" "bff" {
  name        = local.role_name
  description = "IRSA role for the ${var.name_prefix} TTL-aware BFF (${local.subject})"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowEksServiceAccountToAssume"
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${var.oidc_provider_url}:aud" = "sts.amazonaws.com"
          "${var.oidc_provider_url}:sub" = local.subject
        }
      }
    }]
  })

  # One hour. The SDK refreshes automatically well before expiry, and a shorter
  # window limits the value of a leaked token.
  max_session_duration = 3600

  tags = merge(var.tags, {
    Name         = local.role_name
    KubernetesSA = local.subject
  })
}

# ---------------------------------------------------------------------------
# Least-privilege policy
# ---------------------------------------------------------------------------
# Built from optional statements so a module consumer that passes no
# ElastiCache ARN gets a policy with no ElastiCache permissions at all, rather
# than one with a wildcard.
data "aws_iam_policy_document" "bff" {

  # --- Secrets Manager: read the AUTH token and the JWT secret -----------
  dynamic "statement" {
    for_each = length(var.secrets_manager_arns) > 0 ? [1] : []
    content {
      sid    = "ReadApplicationSecrets"
      effect = "Allow"
      actions = [
        "secretsmanager:GetSecretValue",
        # Needed to resolve the AWSCURRENT stage after a rotation.
        "secretsmanager:DescribeSecret",
      ]
      # Named ARNs only. Note the trailing "-??????" AWS appends to a secret's
      # ARN: the ARNs passed in already carry it, so no wildcard is needed.
      resources = var.secrets_manager_arns
    }
  }

  # The secrets are encrypted with customer-managed KMS keys, so reading them
  # requires kms:Decrypt as well. Scoped by the ViaService condition so this
  # grant cannot be used to decrypt anything outside Secrets Manager.
  dynamic "statement" {
    for_each = length(var.secrets_manager_arns) > 0 ? [1] : []
    content {
      sid       = "DecryptSecretsViaSecretsManager"
      effect    = "Allow"
      actions   = ["kms:Decrypt"]
      resources = ["*"]

      condition {
        test     = "StringEquals"
        variable = "kms:ViaService"
        values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
      }
    }
  }

  # --- ElastiCache ------------------------------------------------------
  # elasticache:Connect is the IAM-auth action. This stack uses an AUTH token
  # rather than IAM auth, so the permission is not strictly required today; it
  # is granted, scoped to the one replication group, so that migrating to IAM
  # auth is a config change rather than an IAM change.
  dynamic "statement" {
    for_each = var.elasticache_replication_group_arn != "" ? [1] : []
    content {
      sid       = "ConnectToCache"
      effect    = "Allow"
      actions   = ["elasticache:Connect"]
      resources = [var.elasticache_replication_group_arn]
    }
  }

  # Read-only visibility into the cache's own state, for the readiness probe's
  # dependency check. DescribeReplicationGroups takes no resource ARN.
  dynamic "statement" {
    for_each = var.elasticache_replication_group_arn != "" ? [1] : []
    content {
      sid    = "DescribeCache"
      effect = "Allow"
      actions = [
        "elasticache:DescribeReplicationGroups",
        "elasticache:DescribeCacheClusters",
      ]
      resources = ["*"]
    }
  }

  # --- CloudWatch Logs ---------------------------------------------------
  # The BFF logs to stdout and a collector ships it; this grant exists for the
  # direct-write fallback path. Deliberately excludes logs:CreateLogGroup, so
  # the application cannot create an unbounded, unmanaged log group.
  dynamic "statement" {
    for_each = var.cloudwatch_log_group_arn != "" ? [1] : []
    content {
      sid    = "WriteApplicationLogs"
      effect = "Allow"
      actions = [
        "logs:CreateLogStream",
        "logs:PutLogEvents",
        "logs:DescribeLogStreams",
      ]
      resources = ["${var.cloudwatch_log_group_arn}:*"]
    }
  }

  # --- CloudWatch metrics ------------------------------------------------
  # PutMetricData has no resource-level permissions, so the namespace condition
  # is the only available scope.
  statement {
    sid       = "PublishApplicationMetrics"
    effect    = "Allow"
    actions   = ["cloudwatch:PutMetricData"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values   = ["ttl-aware-bff"]
    }
  }

  # --- X-Ray (off by default) -------------------------------------------
  # The SDK exports OTLP to the collector, and the collector holds the X-Ray
  # permission. Granting it here too would be a second, unnecessary path.
  dynamic "statement" {
    for_each = var.enable_xray ? [1] : []
    content {
      sid    = "WriteXRaySegments"
      effect = "Allow"
      actions = [
        "xray:PutTraceSegments",
        "xray:PutTelemetryRecords",
        "xray:GetSamplingRules",
        "xray:GetSamplingTargets",
      ]
      # These APIs are account-scoped and accept no resource ARN.
      resources = ["*"]
    }
  }
}

resource "aws_iam_policy" "bff" {
  name        = "${local.role_name}-policy"
  description = "Least-privilege permissions for the ${var.name_prefix} TTL-aware BFF"
  policy      = data.aws_iam_policy_document.bff.json

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "bff" {
  role       = aws_iam_role.bff.name
  policy_arn = aws_iam_policy.bff.arn
}

# ---------------------------------------------------------------------------
# Permissions boundary guard
# ---------------------------------------------------------------------------
# An explicit deny that no attached policy can override: even if a future
# policy grants iam:* by accident, this role can never modify IAM or assume
# another role. Cheap insurance on a workload identity.
resource "aws_iam_role_policy" "deny_privilege_escalation" {
  name = "${local.role_name}-deny-escalation"
  role = aws_iam_role.bff.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid    = "DenyIamAndAssumeRole"
      Effect = "Deny"
      Action = [
        "iam:*",
        "sts:AssumeRole",
        "organizations:*",
        "account:*",
      ]
      Resource = "*"
    }]
  })
}

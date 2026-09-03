# Observability plumbing: log groups, the ADOT collector's IRSA role, and the
# Secrets Manager entry for the JWT signing secret.
#
# The telemetry path in AWS mirrors the local docker-compose stack:
#
#   bff --OTLP/gRPC :4317--> ADOT collector --+--> X-Ray        (traces)
#                                              +--> CloudWatch   (metrics, EMF)
#                                              `--> CloudWatch Logs
#
# The collector runs in the cluster (deployed by the platform, not by this
# stack); what Terraform owns is the IAM identity it assumes and the
# destinations it writes to.

data "aws_partition" "current" {}
data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

# ---------------------------------------------------------------------------
# Log groups
# ---------------------------------------------------------------------------
# Application logs. The BFF writes structured JSON to stdout; Fluent Bit or the
# collector's filelog receiver ships it here.
resource "aws_cloudwatch_log_group" "application" {
  name              = "/aws/eks/${var.name_prefix}/application"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name      = "${var.name_prefix}-application-logs"
    Component = "bff"
  })
}

# The collector's own logs. Separated from the application's so a collector
# crash-loop does not drown the signal it is supposed to be carrying.
resource "aws_cloudwatch_log_group" "adot_collector" {
  name              = "/aws/eks/${var.name_prefix}/adot-collector"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name      = "${var.name_prefix}-adot-collector-logs"
    Component = "observability"
  })
}

# Container Insights performance data, if the platform enables it.
resource "aws_cloudwatch_log_group" "performance" {
  name              = "/aws/containerinsights/${var.name_prefix}-eks/performance"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name      = "${var.name_prefix}-performance-logs"
    Component = "observability"
  })
}

# ---------------------------------------------------------------------------
# JWT signing secret
# ---------------------------------------------------------------------------
# Only used on the HS256 verification path. When security.jwt.jwks_url is set
# (the production default) this secret is unused, but it is created so that
# switching paths is a config change rather than an infrastructure change.
#
# The value is deliberately NOT generated here: it is issued by the identity
# provider, and Terraform inventing one would produce a secret that verifies
# nothing. Populate it out of band:
#
#   aws secretsmanager put-secret-value \
#     --secret-id /bff/<prefix>/jwt/hs256 \
#     --secret-string '{"secret":"<value from the IdP>"}'
resource "aws_secretsmanager_secret" "jwt" {
  name        = "/bff/${var.name_prefix}/jwt/hs256"
  description = "Symmetric JWT signing secret for the ${var.name_prefix} BFF (HS256 verification path)"

  recovery_window_in_days = 7

  tags = merge(var.tags, {
    Name      = "${var.name_prefix}-jwt-hs256"
    Component = "bff"
  })
}

# ---------------------------------------------------------------------------
# ADOT collector IRSA role
# ---------------------------------------------------------------------------
resource "aws_iam_role" "adot_collector" {
  name        = "${var.name_prefix}-adot-collector"
  description = "IRSA role for the ADOT collector in ${var.adot_namespace}"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          # Both conditions matter. Without :aud the role would trust any
          # token from this OIDC provider; without :sub it would trust every
          # ServiceAccount in the cluster.
          "${var.oidc_provider_url}:aud" = "sts.amazonaws.com"
          "${var.oidc_provider_url}:sub" = "system:serviceaccount:${var.adot_namespace}:${var.adot_service_account}"
        }
      }
    }]
  })

  tags = var.tags
}

# --- CloudWatch metrics and logs -------------------------------------------
resource "aws_iam_policy" "adot_cloudwatch" {
  name        = "${var.name_prefix}-adot-cloudwatch"
  description = "Lets the ADOT collector publish metrics and logs for ${var.name_prefix}"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "PutMetrics"
        Effect   = "Allow"
        Action   = "cloudwatch:PutMetricData"
        Resource = "*"
        Condition = {
          # cloudwatch:PutMetricData does not support resource-level
          # permissions, so the namespace condition is the only way to scope
          # it. Without this the collector could write into AWS/EC2 and
          # corrupt real metrics.
          StringEquals = {
            "cloudwatch:namespace" = [
              "ContainerInsights",
              "ttl-aware-bff",
            ]
          }
        }
      },
      {
        Sid    = "WriteLogs"
        Effect = "Allow"
        Action = [
          "logs:CreateLogStream",
          "logs:PutLogEvents",
          "logs:DescribeLogStreams",
          "logs:DescribeLogGroups",
        ]
        Resource = [
          "${aws_cloudwatch_log_group.application.arn}:*",
          "${aws_cloudwatch_log_group.adot_collector.arn}:*",
          "${aws_cloudwatch_log_group.performance.arn}:*",
        ]
      },
    ]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "adot_cloudwatch" {
  role       = aws_iam_role.adot_collector.name
  policy_arn = aws_iam_policy.adot_cloudwatch.arn
}

# --- X-Ray -----------------------------------------------------------------
# X-Ray's write APIs are account-scoped: PutTraceSegments takes no resource
# ARN, so "*" is the only expressible resource. This is the AWS-managed
# policy's own shape, used here rather than reimplemented.
resource "aws_iam_role_policy_attachment" "adot_xray" {
  count = var.enable_xray ? 1 : 0

  role       = aws_iam_role.adot_collector.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

# --- Amazon Managed Prometheus remote write --------------------------------
# Present but unattached by default: the local stack and most clusters scrape
# with a ServiceMonitor instead. Attach it when AMP is the metric backend.
resource "aws_iam_policy" "adot_prometheus_remote_write" {
  name        = "${var.name_prefix}-adot-amp-remote-write"
  description = "Remote-write access to Amazon Managed Prometheus for ${var.name_prefix}"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "aps:RemoteWrite",
        "aps:GetSeries",
        "aps:GetLabels",
        "aps:GetMetricMetadata",
      ]
      # Scoped to workspaces in this account and region rather than "*".
      Resource = "arn:${data.aws_partition.current.partition}:aps:${data.aws_region.current.name}:${data.aws_caller_identity.current.account_id}:workspace/*"
    }]
  })

  tags = var.tags
}

# ---------------------------------------------------------------------------
# Alarms on the BFF's own contract metrics
# ---------------------------------------------------------------------------
# These assume the collector publishes into the `ttl-aware-bff` CloudWatch
# namespace (the condition on PutMetricData above). Metric names come from
# docs/DESIGN-CONTRACT.md section 7.
resource "aws_cloudwatch_metric_alarm" "execution_fallback_rate" {
  alarm_name          = "${var.name_prefix}-execution-fallback-elevated"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "execution_fallback_total"
  namespace           = "ttl-aware-bff"
  period              = 300
  statistic           = "Sum"
  threshold           = 100
  # A rising fallback count means the operational source is stale or
  # unavailable often enough that the router is routinely choosing the
  # execution source. That is the system working as designed, and also the
  # earliest warning that the operational source is degrading.
  alarm_description  = "BFF is falling back to the execution source unusually often in ${var.environment}: check operational source freshness and availability."
  treat_missing_data = "notBreaching"

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "stale_responses" {
  alarm_name          = "${var.name_prefix}-stale-responses-elevated"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "stale_response_total"
  namespace           = "ttl-aware-bff"
  period              = 300
  statistic           = "Sum"
  threshold           = 50
  alarm_description   = "BFF is serving stale data in ${var.environment}: both sources are likely unhealthy, since stale-serve is the last resort before an error."
  treat_missing_data  = "notBreaching"

  tags = var.tags
}

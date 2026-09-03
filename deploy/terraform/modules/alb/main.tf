# Public entry point: ACM certificate, WAF Web ACL, access-log bucket and DNS.
#
# IMPORTANT: this module does NOT create the Application Load Balancer itself.
# The AWS Load Balancer Controller does that, from the Ingress object in
# deploy/k8s/ingress.yaml (or the Helm chart's ingress template). Terraform
# creating one too would produce two load balancers fighting over the same
# target group.
#
# What this module owns is everything the Ingress needs to REFERENCE:
#
#   certificate_arn  -> alb.ingress.kubernetes.io/certificate-arn
#   web_acl_arn      -> alb.ingress.kubernetes.io/wafv2-acl-arn
#   logs bucket      -> access_logs.s3.bucket in load-balancer-attributes
#   DNS record       -> resolves domain_name once the controller has created
#                       the ALB
#
# That split is deliberate: the lifecycle of a load balancer belongs with the
# application that is behind it, while certificates, WAF rules and log buckets
# outlive any single deployment.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_elb_service_account" "current" {}

locals {
  create_certificate = var.existing_certificate_arn == "" && var.route53_zone_id != ""
  certificate_arn = var.existing_certificate_arn != "" ? var.existing_certificate_arn : (
    local.create_certificate ? aws_acm_certificate_validation.this[0].certificate_arn : ""
  )
}

# ---------------------------------------------------------------------------
# TLS certificate
# ---------------------------------------------------------------------------
resource "aws_acm_certificate" "this" {
  count = local.create_certificate ? 1 : 0

  domain_name = var.domain_name
  # A wildcard alongside the apex covers per-tenant hostnames without reissuing.
  subject_alternative_names = ["*.${var.domain_name}"]

  # DNS validation renews automatically as long as the record stays in place;
  # email validation does not, and expires silently.
  validation_method = "DNS"

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-cert"
  })

  lifecycle {
    # A certificate in use by a listener cannot be deleted, so the replacement
    # has to exist first.
    create_before_destroy = true
  }
}

resource "aws_route53_record" "cert_validation" {
  for_each = local.create_certificate ? {
    for dvo in aws_acm_certificate.this[0].domain_validation_options :
    dvo.domain_name => {
      name   = dvo.resource_record_name
      record = dvo.resource_record_value
      type   = dvo.resource_record_type
    }
  } : {}

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  count = local.create_certificate ? 1 : 0

  certificate_arn         = aws_acm_certificate.this[0].arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

# ---------------------------------------------------------------------------
# Access logs
# ---------------------------------------------------------------------------
resource "aws_s3_bucket" "access_logs" {
  bucket = "${var.name_prefix}-alb-logs"

  # Access logs are reconstructable from nothing, but losing them mid-incident
  # is painful. force_destroy stays false so `terraform destroy` cannot silently
  # discard an audit trail.
  force_destroy = false

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-alb-logs"
  })
}

resource "aws_s3_bucket_public_access_block" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  rule {
    apply_server_side_encryption_by_default {
      # SSE-S3, not SSE-KMS: the ALB log delivery service cannot write to a
      # bucket encrypted with a customer-managed KMS key.
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  rule {
    id     = "expire-logs"
    status = "Enabled"

    filter {}

    transition {
      # Logs older than a month are for forensics, not dashboards.
      days          = 30
      storage_class = "STANDARD_IA"
    }

    expiration {
      days = var.access_logs_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 7
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# The ALB log delivery principal differs by region: older regions use a
# per-region ELB service account, newer ones use logdelivery.elasticloadbalancing.
# Both statements are present so the policy works either way.
resource "aws_s3_bucket_policy" "access_logs" {
  bucket = aws_s3_bucket.access_logs.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AllowELBServiceAccount"
        Effect    = "Allow"
        Principal = { AWS = data.aws_elb_service_account.current.arn }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.access_logs.arn}/bff/AWSLogs/${data.aws_caller_identity.current.account_id}/*"
      },
      {
        Sid       = "AllowLogDeliveryService"
        Effect    = "Allow"
        Principal = { Service = "logdelivery.elasticloadbalancing.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.access_logs.arn}/bff/AWSLogs/${data.aws_caller_identity.current.account_id}/*"
        Condition = {
          StringEquals = {
            "s3:x-amz-acl" = "bucket-owner-full-control"
          }
        }
      },
      {
        Sid       = "AllowLogDeliveryAclCheck"
        Effect    = "Allow"
        Principal = { Service = "logdelivery.elasticloadbalancing.amazonaws.com" }
        Action    = "s3:GetBucketAcl"
        Resource  = aws_s3_bucket.access_logs.arn
      },
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource = [
          aws_s3_bucket.access_logs.arn,
          "${aws_s3_bucket.access_logs.arn}/*",
        ]
        Condition = {
          Bool = { "aws:SecureTransport" = "false" }
        }
      },
    ]
  })
}

# ---------------------------------------------------------------------------
# WAF
# ---------------------------------------------------------------------------
# REGIONAL scope: an ALB Web ACL must be REGIONAL (CLOUDFRONT is for
# distributions) and must live in the same region as the load balancer.
resource "aws_wafv2_web_acl" "this" {
  count = var.enable_waf ? 1 : 0

  name        = "${var.name_prefix}-waf"
  description = "Web ACL for the ${var.name_prefix} BFF public ALB"
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  # --- 1. Rate limiting -------------------------------------------------
  # A blunt DDoS backstop. The precise control is the BFF's own per-tenant
  # rate limiter (security.rate_limit in bff.yaml), which understands tenants;
  # WAF only sees IPs, and a whole tenant may sit behind one NAT address.
  rule {
    name     = "rate-limit-per-ip"
    priority = 1

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = var.waf_rate_limit_per_5min
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name_prefix}-rate-limit"
      sampled_requests_enabled   = true
    }
  }

  # --- 2. AWS managed common rule set -----------------------------------
  rule {
    name     = "aws-common"
    priority = 2

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesCommonRuleSet"
        vendor_name = "AWS"

        rule_action_override {
          # The BFF accepts only GETs with no body, but this rule's 8KB body
          # cap trips on large Authorization headers on some JWT setups.
          # Counted rather than blocked so it is visible without being fatal.
          name = "SizeRestrictions_BODY"
          action_to_use {
            count {}
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name_prefix}-aws-common"
      sampled_requests_enabled   = true
    }
  }

  # --- 3. Known bad inputs ----------------------------------------------
  rule {
    name     = "aws-known-bad-inputs"
    priority = 3

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
        vendor_name = "AWS"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name_prefix}-known-bad-inputs"
      sampled_requests_enabled   = true
    }
  }

  # --- 4. IP reputation --------------------------------------------------
  rule {
    name     = "aws-ip-reputation"
    priority = 4

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesAmazonIpReputationList"
        vendor_name = "AWS"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name_prefix}-ip-reputation"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${var.name_prefix}-waf"
    sampled_requests_enabled   = true
  }

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-waf"
  })
}

# WAF logs to CloudWatch. The log group name MUST start with `aws-waf-logs-`;
# WAF rejects any other prefix.
resource "aws_cloudwatch_log_group" "waf" {
  count = var.enable_waf ? 1 : 0

  name              = "aws-waf-logs-${var.name_prefix}"
  retention_in_days = var.access_logs_retention_days

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-waf-logs"
  })
}

resource "aws_wafv2_web_acl_logging_configuration" "this" {
  count = var.enable_waf ? 1 : 0

  resource_arn            = aws_wafv2_web_acl.this[0].arn
  log_destination_configs = [aws_cloudwatch_log_group.waf[0].arn]

  # Never log the bearer token.
  redacted_fields {
    single_header {
      name = "authorization"
    }
  }

  redacted_fields {
    single_header {
      name = "cookie"
    }
  }
}

# ---------------------------------------------------------------------------
# DNS
# ---------------------------------------------------------------------------
# The alias target is filled in by the AWS Load Balancer Controller through
# external-dns, which reads the Ingress's host and creates the record itself.
# A placeholder record is NOT created here: an A record pointing nowhere is
# worse than no record, because clients cache the failure.
#
# If external-dns is not deployed, create the alias manually once the ALB
# exists:
#
#   aws elbv2 describe-load-balancers --names ttl-aware-bff-alb \
#     --query 'LoadBalancers[0].[DNSName,CanonicalHostedZoneId]' --output text
#
# then an A/ALIAS record for var.domain_name pointing at that pair.

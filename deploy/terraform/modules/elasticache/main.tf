# ElastiCache for Redis — the BFF's L2 cache.
#
# Security posture, all three of which are non-negotiable for a cache holding
# per-tenant response data:
#
#   at_rest_encryption_enabled   = true, with a customer-managed KMS key
#   transit_encryption_enabled   = true, so the BFF connects over TLS
#   auth_token                   generated here, stored in Secrets Manager,
#                                read by the BFF through IRSA + External Secrets
#
# transit_encryption_enabled cannot be changed in place on older engine
# versions without recreating the replication group — set it correctly the
# first time.

# ---------------------------------------------------------------------------
# Network placement
# ---------------------------------------------------------------------------
resource "aws_elasticache_subnet_group" "this" {
  name        = "${var.name_prefix}-redis"
  description = "Private subnets for the ${var.name_prefix} BFF L2 cache"
  subnet_ids  = var.subnet_ids

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis-subnets"
  })
}

resource "aws_security_group" "this" {
  name        = "${var.name_prefix}-redis"
  description = "Redis access for the ${var.name_prefix} BFF"
  vpc_id      = var.vpc_id

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis"
  })

  lifecycle {
    # Rules are separate resources below; replacing the SG in place would
    # briefly cut every connection.
    create_before_destroy = true
  }
}

# Ingress only from the security groups explicitly passed in — no CIDR rules,
# so a machine that merely shares the VPC cannot reach the cache.
resource "aws_vpc_security_group_ingress_rule" "from_allowed" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.this.id
  description                  = "Redis from ${each.value}"
  referenced_security_group_id = each.value
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"

  tags = var.tags
}

# No egress rule at all. ElastiCache never initiates an outbound connection,
# and the default "allow all egress" that AWS adds to a new security group is
# removed by declaring none here.

# ---------------------------------------------------------------------------
# Encryption at rest
# ---------------------------------------------------------------------------
resource "aws_kms_key" "this" {
  description             = "At-rest encryption for the ${var.name_prefix} BFF Redis cache"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis-kms"
  })
}

resource "aws_kms_alias" "this" {
  name          = "alias/${var.name_prefix}-redis"
  target_key_id = aws_kms_key.this.key_id
}

# ---------------------------------------------------------------------------
# AUTH token
# ---------------------------------------------------------------------------
# Redis AUTH tokens must be 16-128 printable characters and may not contain
# quotes, spaces, @ or /. random_password's default override set is too
# permissive, so the allowed characters are pinned explicitly.
resource "random_password" "auth_token" {
  length  = 64
  special = true
  # Deliberately narrow: these are the punctuation characters ElastiCache
  # accepts in an AUTH token.
  override_special = "!&#$^<>-"

  # Rotating this recreates the replication group's auth, so it is regenerated
  # only when the keepers change.
  keepers = {
    replication_group = "${var.name_prefix}-redis"
  }
}

resource "aws_secretsmanager_secret" "auth_token" {
  # The path the BFF's ExternalSecret references
  # (see deploy/k8s/secret.example.yaml).
  name        = "/bff/${var.name_prefix}/elasticache/auth-token"
  description = "ElastiCache AUTH token for the ${var.name_prefix} BFF L2 cache"
  kms_key_id  = aws_kms_key.this.arn

  # Long enough to undo an accidental delete, short enough that the name can be
  # reused within a sprint.
  recovery_window_in_days = 7

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis-auth-token"
  })
}

resource "aws_secretsmanager_secret_version" "auth_token" {
  secret_id = aws_secretsmanager_secret.auth_token.id

  # A JSON document rather than a bare string, so the ExternalSecret can select
  # `property: auth_token` and the same secret can grow more fields later.
  secret_string = jsonencode({
    auth_token = random_password.auth_token.result
  })
}

# ---------------------------------------------------------------------------
# Parameter group
# ---------------------------------------------------------------------------
resource "aws_elasticache_parameter_group" "this" {
  name        = "${var.name_prefix}-redis"
  family      = "redis7"
  description = "Cache-shaped Redis parameters for the ${var.name_prefix} BFF"

  parameter {
    # Evict rather than refuse writes when full. The BFF treats a cache miss as
    # normal; a write error would surface as a spurious upstream call.
    name  = "maxmemory-policy"
    value = var.maxmemory_policy
  }

  parameter {
    # Publishes key-eviction and key-expiry events. The BFF does not subscribe,
    # but having them on makes "why did the hit rate collapse" answerable.
    name  = "notify-keyspace-events"
    value = "Ege"
  }

  parameter {
    # Drop a client that has been idle for an hour. The go-redis pool recycles
    # long before this; the timeout catches leaked connections.
    name  = "timeout"
    value = "3600"
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Replication group
# ---------------------------------------------------------------------------
resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name_prefix}-redis"
  description          = "L2 cache for the ${var.name_prefix} TTL-aware BFF"

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type
  port           = 6379

  # One shard. The BFF's key space is small and uniformly accessed; cluster
  # mode would add cross-slot constraints for no benefit at this size.
  num_cache_clusters = var.replica_count + 1

  parameter_group_name = aws_elasticache_parameter_group.this.name
  subnet_group_name    = aws_elasticache_subnet_group.this.name
  security_group_ids   = [aws_security_group.this.id]

  automatic_failover_enabled = var.automatic_failover_enabled
  multi_az_enabled           = var.multi_az_enabled

  # --- Encryption -------------------------------------------------------
  at_rest_encryption_enabled = true
  kms_key_id                 = aws_kms_key.this.arn
  transit_encryption_enabled = true
  auth_token                 = random_password.auth_token.result
  auth_token_update_strategy = "ROTATE"

  # --- Maintenance ------------------------------------------------------
  # Minor versions applied automatically inside the maintenance window; with a
  # replica and automatic failover this is a sub-minute blip on the read path.
  auto_minor_version_upgrade = true
  maintenance_window         = "sun:05:00-sun:07:00"
  snapshot_retention_limit   = var.snapshot_retention_days
  snapshot_window            = var.snapshot_retention_days > 0 ? "03:00-05:00" : null

  # Apply parameter and node-type changes in the maintenance window rather than
  # immediately, so a `terraform apply` never causes an unplanned failover.
  apply_immediately = false

  # --- Logging ----------------------------------------------------------
  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.slow.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.engine.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "engine-log"
  }

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis"
  })

  lifecycle {
    # Rotating the AUTH token is a deliberate, separate operation (change the
    # random_password keeper), not something an unrelated apply should do.
    ignore_changes = [auth_token]
  }
}

# ---------------------------------------------------------------------------
# Logs
# ---------------------------------------------------------------------------
resource "aws_cloudwatch_log_group" "slow" {
  name              = "/aws/elasticache/${var.name_prefix}/slow-log"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis-slow-log"
  })
}

resource "aws_cloudwatch_log_group" "engine" {
  name              = "/aws/elasticache/${var.name_prefix}/engine-log"
  retention_in_days = var.log_retention_days

  tags = merge(var.tags, {
    Name = "${var.name_prefix}-redis-engine-log"
  })
}

# ---------------------------------------------------------------------------
# Alarms
# ---------------------------------------------------------------------------
# Two alarms that map directly to BFF symptoms:
#   high evictions   -> the working set no longer fits; hit rate falls and
#                       upstream load rises
#   high CPU         -> the cache is the bottleneck, not the upstreams
resource "aws_cloudwatch_metric_alarm" "evictions" {
  alarm_name          = "${var.name_prefix}-redis-evictions"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "Evictions"
  namespace           = "AWS/ElastiCache"
  period              = 300
  statistic           = "Sum"
  threshold           = 1000
  alarm_description   = "Redis is evicting keys: the BFF working set no longer fits, expect cache_miss_total to climb."
  treat_missing_data  = "notBreaching"

  dimensions = {
    ReplicationGroupId = aws_elasticache_replication_group.this.id
  }

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "cpu" {
  alarm_name          = "${var.name_prefix}-redis-cpu"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "EngineCPUUtilization"
  namespace           = "AWS/ElastiCache"
  period              = 300
  statistic           = "Average"
  threshold           = 75
  alarm_description   = "Redis engine CPU above 75%: the cache is becoming the bottleneck."
  treat_missing_data  = "notBreaching"

  dimensions = {
    ReplicationGroupId = aws_elasticache_replication_group.this.id
  }

  tags = var.tags
}

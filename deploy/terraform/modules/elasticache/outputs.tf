output "replication_group_id" {
  description = "ElastiCache replication group id."
  value       = aws_elasticache_replication_group.this.id
}

output "replication_group_arn" {
  description = "Replication group ARN. The BFF's IRSA policy scopes elasticache:Connect to exactly this."
  value       = aws_elasticache_replication_group.this.arn
}

output "primary_endpoint" {
  description = "Primary endpoint address. Set this as externalRedis.host in the Helm values (or BFF_CACHE__REDIS__ADDR)."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "primary_endpoint_with_port" {
  description = "host:port form, ready to paste into cache.redis.addr."
  value       = "${aws_elasticache_replication_group.this.primary_endpoint_address}:${aws_elasticache_replication_group.this.port}"
}

output "reader_endpoint" {
  description = "Reader endpoint, load-balanced across replicas. The BFF does not use it - its cache-aside path writes on every miss - but it is here for a future read-only consumer."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "port" {
  description = "Redis port."
  value       = aws_elasticache_replication_group.this.port
}

output "security_group_id" {
  description = "Security group guarding the cache."
  value       = aws_security_group.this.id
}

output "auth_token_secret_arn" {
  description = "Secrets Manager ARN holding the AUTH token. Referenced by the BFF's ExternalSecret and by its IRSA policy."
  value       = aws_secretsmanager_secret.auth_token.arn
}

output "auth_token_secret_name" {
  description = "Secrets Manager secret name, as written in deploy/k8s/secret.example.yaml."
  value       = aws_secretsmanager_secret.auth_token.name
}

output "kms_key_arn" {
  description = "KMS key encrypting the cache at rest and the AUTH token secret."
  value       = aws_kms_key.this.arn
}

output "transit_encryption_enabled" {
  description = "Always true. The BFF must therefore connect with TLS - feed this into externalRedis.tls in the Helm values."
  value       = aws_elasticache_replication_group.this.transit_encryption_enabled
}

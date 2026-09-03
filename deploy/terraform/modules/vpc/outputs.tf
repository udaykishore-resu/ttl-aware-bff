output "vpc_id" {
  description = "VPC id."
  value       = aws_vpc.this.id
}

output "vpc_cidr" {
  description = "VPC CIDR block. Use this for networkPolicy.clusterCidr in the Helm values rather than hardcoding it."
  value       = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  description = "Public subnet ids, one per AZ. The ALB lives here."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnet ids, one per AZ. EKS nodes, pods and ElastiCache live here."
  value       = aws_subnet.private[*].id
}

output "public_subnet_cidrs" {
  description = "Public subnet CIDR blocks."
  value       = aws_subnet.public[*].cidr_block
}

output "private_subnet_cidrs" {
  description = "Private subnet CIDR blocks. Narrow the NetworkPolicy ipBlock rules to these instead of the whole VPC CIDR."
  value       = aws_subnet.private[*].cidr_block
}

output "availability_zones" {
  description = "AZs the subnets span."
  value       = var.availability_zones
}

output "nat_gateway_ids" {
  description = "NAT gateway ids."
  value       = aws_nat_gateway.this[*].id
}

output "nat_gateway_public_ips" {
  description = "Elastic IPs the cluster's egress appears to come from. Give these to any upstream that maintains an IP allow-list."
  value       = aws_eip.nat[*].public_ip
}

# VPC
output "vpc_id" {
  description = "ID of the VPC"
  value       = aws_vpc.main.id
}

output "vpc_cidr" {
  description = "CIDR block of the VPC"
  value       = aws_vpc.main.cidr_block
}

# Subnets
output "public_subnet_ids" {
  description = "IDs of the public subnets (one per AZ)"
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "IDs of the private subnets — pass these to the ecs_cluster module"
  value       = aws_subnet.private[*].id
}

# Security groups
output "coordinator_security_group_id" {
  description = "Security group ID for coordinator ECS tasks — pass to ecs_cluster module"
  value       = aws_security_group.coordinator.id
}

output "worker_security_group_id" {
  description = "Security group ID for worker ECS tasks"
  value       = aws_security_group.worker.id
}

output "ingest_security_group_id" {
  description = "Security group ID for ingest API ECS tasks"
  value       = aws_security_group.ingest.id
}

output "observer_security_group_id" {
  description = "Security group ID for observer ECS tasks"
  value       = aws_security_group.observer.id
}

output "cluster_name" {
  description = "ECS cluster name"
  value       = aws_ecs_cluster.main.name
}

output "cluster_arn" {
  description = "ECS cluster ARN"
  value       = aws_ecs_cluster.main.arn
}

output "coordinator_service_names" {
  description = "Map of node ID → ECS service name for all coordinator services"
  value       = { for k, v in aws_ecs_service.coordinator : k => v.name }
}

output "coordinator_task_definition_arns" {
  description = "Map of node ID → task definition ARN"
  value       = { for k, v in aws_ecs_task_definition.coordinator : k => v.arn }
}

output "log_group_name" {
  description = "CloudWatch log group name for coordinator containers"
  value       = aws_cloudwatch_log_group.coordinator.name
}

output "worker_service_name" {
  description = "ECS service name for the worker pool"
  value       = aws_ecs_service.worker.name
}

output "ingest_service_name" {
  description = "ECS service name for the ingest API"
  value       = aws_ecs_service.ingest.name
}

output "observer_service_name" {
  description = "ECS service name for the state observer"
  value       = aws_ecs_service.observer.name
}

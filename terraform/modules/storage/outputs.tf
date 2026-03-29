# DynamoDB
output "tasks_table_name" {
  description = "DynamoDB tasks table name — set as TASKS_TABLE env var in ECS task definitions"
  value       = aws_dynamodb_table.tasks.name
}

output "tasks_table_arn" {
  description = "DynamoDB tasks table ARN"
  value       = aws_dynamodb_table.tasks.arn
}

output "raft_state_table_name" {
  description = "DynamoDB Raft state table name — set as RAFT_STATE_TABLE env var in ECS task definitions"
  value       = aws_dynamodb_table.raft_state.name
}

output "raft_state_table_arn" {
  description = "DynamoDB Raft state table ARN"
  value       = aws_dynamodb_table.raft_state.arn
}

# S3
output "task_data_bucket" {
  description = "S3 bucket name for task payloads and results — set as TASK_DATA_BUCKET env var"
  value       = aws_s3_bucket.task_data.bucket
}

output "task_data_bucket_arn" {
  description = "S3 task data bucket ARN"
  value       = aws_s3_bucket.task_data.arn
}

output "raft_snapshots_bucket" {
  description = "S3 bucket name for Raft log snapshots — set as RAFT_SNAPSHOTS_BUCKET env var"
  value       = aws_s3_bucket.raft_snapshots.bucket
}

output "raft_snapshots_bucket_arn" {
  description = "S3 Raft snapshots bucket ARN"
  value       = aws_s3_bucket.raft_snapshots.arn
}

# labRole — passed straight through so ecs_cluster/ can reference it
output "lab_role_arn" {
  description = "labRole ARN for use in ECS task definitions"
  value       = var.lab_role_arn
}

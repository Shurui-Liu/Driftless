variable "environment" {
  description = "Deployment environment (dev or prod)"
  type        = string
}

variable "project" {
  description = "Project name prefix for all resource names"
  type        = string
  default     = "raft-coordinator"
}

# ── Passed in from other modules ──────────────────────────────────────────────

variable "lab_role_arn" {
  description = "ARN of the labRole — used as both task role and execution role"
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet IDs from the networking module (ECS tasks run here)"
  type        = list(string)
}

variable "coordinator_security_group_id" {
  description = "Security group ID for coordinator tasks (from networking module)"
  type        = string
}

variable "service_discovery_service_arn" {
  description = "Cloud Map service ARN for coordinator DNS registration"
  type        = string
}

variable "ingest_queue_url" {
  description = "SQS ingest queue URL (from messaging module)"
  type        = string
}

variable "assignment_queue_url" {
  description = "SQS assignment queue URL (from messaging module)"
  type        = string
}

variable "results_queue_url" {
  description = "SQS results queue URL (from messaging module)"
  type        = string
}

variable "tasks_table_name" {
  description = "DynamoDB tasks table name (from storage module)"
  type        = string
}

variable "raft_state_table_name" {
  description = "DynamoDB Raft state table name (from storage module)"
  type        = string
}

variable "task_data_bucket" {
  description = "S3 task data bucket name (from storage module)"
  type        = string
}

variable "raft_snapshots_bucket" {
  description = "S3 Raft snapshots bucket name (from storage module)"
  type        = string
}

# ── Coordinator config ────────────────────────────────────────────────────────

variable "coordinator_image" {
  description = "ECR image URI for the Raft coordinator (e.g. 123456789.dkr.ecr.us-east-1.amazonaws.com/raft-coordinator:latest)"
  type        = string
}

variable "coordinator_count" {
  description = "Number of coordinator nodes (1 for dev, 3 for prod)"
  type        = number
  default     = 1
}

variable "coordinator_cpu" {
  description = "CPU units for each coordinator task (1024 = 1 vCPU)"
  type        = number
  default     = 512
}

variable "coordinator_memory" {
  description = "Memory (MB) for each coordinator task"
  type        = number
  default     = 1024
}

variable "grpc_port" {
  description = "gRPC port the coordinator listens on"
  type        = number
  default     = 50051
}

variable "tags" {
  description = "Additional tags to apply to all resources"
  type        = map(string)
  default     = {}
}

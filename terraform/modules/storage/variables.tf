variable "environment" {
  description = "Deployment environment (dev or prod)"
  type        = string
}

variable "project" {
  description = "Project name prefix for all resource names"
  type        = string
  default     = "raft-coordinator"
}

# labRole ARN — provided by your AWS Academy environment.
# Pass this in from the environment config so the module stays reusable.
variable "lab_role_arn" {
  description = "ARN of the labRole IAM role used by ECS tasks"
  type        = string
}

variable "raft_log_retention_days" {
  description = "How long to keep Raft log snapshots in S3 (days)"
  type        = number
  default     = 30
}

variable "task_payload_retention_days" {
  description = "How long to keep task payloads and results in S3 (days)"
  type        = number
  default     = 7
}

variable "tags" {
  description = "Additional tags to apply to all resources"
  type        = map(string)
  default     = {}
}

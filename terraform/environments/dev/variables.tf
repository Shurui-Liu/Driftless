variable "coordinator_count" {
  description = <<EOT
Number of coordinator ECS tasks (Raft cluster size).
Start at 1 for the first apply — images don't exist in ECR yet, and ECS will
churn restarts while pulling. After `docker push`, set this to 3 and re-apply.
EOT
  type        = number
  default     = 1
}

variable "lab_role_arn" {
  description = <<EOT
IAM role ARN used as both execution role and task role for every ECS service.
Leave empty to use `arn:aws:iam::<account>:role/LabRole` (the voclabs / AWS
Academy sandbox role). Override with your own role ARN (e.g. the standard
`ecsTaskExecutionRole`) when deploying to a non-sandbox account.
EOT
  type        = string
  default     = ""
}

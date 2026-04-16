# Queue URLs — used by ECS task definitions as environment variables.
# The Go services reference these via the INGEST_QUEUE_URL etc. env vars.

output "ingest_queue_url" {
  description = "URL of the task ingest queue (Task Ingest API writes here)"
  value       = aws_sqs_queue.ingest.url
}

output "assignment_queue_url" {
  description = "URL of the task assignment queue (Raft leader writes, workers read)"
  value       = aws_sqs_queue.assignment.url
}

output "results_queue_url" {
  description = "URL of the results queue (workers write, coordinator reads)"
  value       = aws_sqs_queue.results.url
}

# Queue ARNs — used in IAM policy documents in the storage/ module.

output "ingest_queue_arn" {
  description = "ARN of the task ingest queue"
  value       = aws_sqs_queue.ingest.arn
}

output "assignment_queue_arn" {
  description = "ARN of the task assignment queue"
  value       = aws_sqs_queue.assignment.arn
}

output "results_queue_arn" {
  description = "ARN of the results queue"
  value       = aws_sqs_queue.results.arn
}

# DLQ ARNs — useful for CloudWatch alarm targets in the observability/ module.

output "ingest_dlq_arn" {
  description = "ARN of the ingest dead letter queue"
  value       = aws_sqs_queue.ingest_dlq.arn
}

output "assignment_dlq_arn" {
  description = "ARN of the assignment dead letter queue"
  value       = aws_sqs_queue.assignment_dlq.arn
}

output "results_dlq_arn" {
  description = "ARN of the results dead letter queue"
  value       = aws_sqs_queue.results_dlq.arn
}

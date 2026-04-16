locals {
  prefix = "${var.project}-${var.environment}"

  common_tags = merge(var.tags, {
    Project     = var.project
    Environment = var.environment
    ManagedBy   = "terraform"
  })
}

# ── Dead Letter Queues ────────────────────────────────────────────────────────
# Each main queue gets its own DLQ. Messages land here after max_receive_count
# failed attempts — gives you a safe place to inspect poison messages without
# losing them or blocking the main queue.

resource "aws_sqs_queue" "ingest_dlq" {
  name                      = "${local.prefix}-ingest-dlq"
  message_retention_seconds = 1209600 # 14 days — maximum, so you don't lose failed tasks
  tags                      = local.common_tags
}

resource "aws_sqs_queue" "assignment_dlq" {
  name                      = "${local.prefix}-assignment-dlq"
  message_retention_seconds = 1209600
  tags                      = local.common_tags
}

resource "aws_sqs_queue" "results_dlq" {
  name                      = "${local.prefix}-results-dlq"
  message_retention_seconds = 1209600
  tags                      = local.common_tags
}

# ── Ingest Queue ──────────────────────────────────────────────────────────────
# Task Ingest API publishes here when a client submits a task.
# The Raft leader polls this queue to pick up new tasks.

resource "aws_sqs_queue" "ingest" {
  name                       = "${local.prefix}-ingest"
  visibility_timeout_seconds = var.ingest_visibility_timeout_seconds

  # Raft leader polls frequently — long polling reduces empty receives and cost.
  receive_wait_time_seconds = 20

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.ingest_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })

  tags = local.common_tags
}

# ── Assignment Queue ──────────────────────────────────────────────────────────
# The Raft leader writes task assignments here.
# Worker nodes poll this queue to receive work.
# Visibility timeout must cover worst-case task execution time.

resource "aws_sqs_queue" "assignment" {
  name                       = "${local.prefix}-assignment"
  visibility_timeout_seconds = var.assignment_visibility_timeout_seconds
  receive_wait_time_seconds  = 20

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.assignment_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })

  tags = local.common_tags
}

# ── Results Queue ─────────────────────────────────────────────────────────────
# Workers publish task results here after execution.
# The coordinator (or a separate results processor) drains this queue
# and updates DynamoDB task status.

resource "aws_sqs_queue" "results" {
  name                       = "${local.prefix}-results"
  visibility_timeout_seconds = var.results_visibility_timeout_seconds
  receive_wait_time_seconds  = 20

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.results_dlq.arn
    maxReceiveCount     = var.max_receive_count
  })

  tags = local.common_tags
}

# ── Queue Policies ────────────────────────────────────────────────────────────
# Restrict access to queues so only your ECS tasks can send/receive.
# The actual IAM roles are created in the storage/ module and referenced here.
# For now these are open within the account — tighten with role ARNs once
# the ecs_cluster/ module creates the task execution roles.

resource "aws_sqs_queue_policy" "ingest" {
  queue_url = aws_sqs_queue.ingest.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AccountAccessOnly"
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
      Action    = ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
      Resource  = aws_sqs_queue.ingest.arn
    }]
  })
}

resource "aws_sqs_queue_policy" "assignment" {
  queue_url = aws_sqs_queue.assignment.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AccountAccessOnly"
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
      Action    = ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
      Resource  = aws_sqs_queue.assignment.arn
    }]
  })
}

resource "aws_sqs_queue_policy" "results" {
  queue_url = aws_sqs_queue.results.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AccountAccessOnly"
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
      Action    = ["sqs:SendMessage", "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
      Resource  = aws_sqs_queue.results.arn
    }]
  })
}

# Used in queue policies above to scope access to your AWS account.
data "aws_caller_identity" "current" {}

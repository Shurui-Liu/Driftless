locals {
  prefix = "${var.project}-${var.environment}"

  common_tags = merge(var.tags, {
    Project     = var.project
    Environment = var.environment
    ManagedBy   = "terraform"
  })
}

# ── DynamoDB — Tasks Table ────────────────────────────────────────────────────
# Stores task metadata: ID, status, priority, timestamps, assigned worker.
# Partition key is task_id. GSI on status lets the coordinator query
# all pending/assigned/failed tasks efficiently.

resource "aws_dynamodb_table" "tasks" {
  name         = "${local.prefix}-tasks"
  billing_mode = "PAY_PER_REQUEST" # no capacity planning needed for dev/experiments
  hash_key     = "task_id"

  attribute {
    name = "task_id"
    type = "S"
  }

  attribute {
    name = "status"
    type = "S"
  }

  attribute {
    name = "created_at"
    type = "S"
  }

  # GSI: query tasks by status (e.g. fetch all "pending" tasks)
  global_secondary_index {
    name            = "status-created-index"
    hash_key        = "status"
    range_key       = "created_at"
    projection_type = "ALL"
  }

  # Point-in-time recovery — lets you restore the table if experiments corrupt state.
  point_in_time_recovery {
    enabled = true
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-tasks" })
}

# ── DynamoDB — Raft State Table ───────────────────────────────────────────────
# Persists the durable Raft fields that must survive a coordinator crash:
# currentTerm, votedFor, and a pointer to the latest snapshot in S3.
# One row per coordinator node (partition key = node_id).

resource "aws_dynamodb_table" "raft_state" {
  name         = "${local.prefix}-raft-state"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "node_id"

  attribute {
    name = "node_id"
    type = "S"
  }

  point_in_time_recovery {
    enabled = true
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-raft-state" })
}

# ── S3 — Task Payloads & Results ──────────────────────────────────────────────
# Large task payloads and results live here.
# DynamoDB stores only the S3 key reference, keeping items small.

resource "aws_s3_bucket" "task_data" {
  bucket = "${local.prefix}-task-data-${data.aws_caller_identity.current.account_id}"
  tags   = merge(local.common_tags, { Name = "${local.prefix}-task-data" })
}

resource "aws_s3_bucket_versioning" "task_data" {
  bucket = aws_s3_bucket.task_data.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "task_data" {
  bucket = aws_s3_bucket.task_data.id

  rule {
    id     = "expire-task-data"
    status = "Enabled"

    filter { prefix = "" }

    expiration {
      days = var.task_payload_retention_days
    }
  }
}

resource "aws_s3_bucket_public_access_block" "task_data" {
  bucket                  = aws_s3_bucket.task_data.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ── S3 — Raft Log Snapshots ───────────────────────────────────────────────────
# Coordinator nodes flush Raft log snapshots here so a restarted node
# can recover without replaying the full log (Experiment 4).

resource "aws_s3_bucket" "raft_snapshots" {
  bucket = "${local.prefix}-raft-snapshots-${data.aws_caller_identity.current.account_id}"
  tags   = merge(local.common_tags, { Name = "${local.prefix}-raft-snapshots" })
}

resource "aws_s3_bucket_versioning" "raft_snapshots" {
  bucket = aws_s3_bucket.raft_snapshots.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "raft_snapshots" {
  bucket = aws_s3_bucket.raft_snapshots.id

  rule {
    id     = "expire-old-snapshots"
    status = "Enabled"

    filter { prefix = "" }

    expiration {
      days = var.raft_log_retention_days
    }
  }
}

resource "aws_s3_bucket_public_access_block" "raft_snapshots" {
  bucket                  = aws_s3_bucket.raft_snapshots.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ── Data sources ──────────────────────────────────────────────────────────────

data "aws_caller_identity" "current" {}

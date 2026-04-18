locals {
  prefix = "${var.project}-${var.environment}"

  common_tags = merge(var.tags, {
    Project     = var.project
    Environment = var.environment
    ManagedBy   = "terraform"
  })

  # Generate one entry per coordinator node:
  # { "coordinator-a" = 0, "coordinator-b" = 1, "coordinator-c" = 2 }
  node_ids = {
    for i in range(var.coordinator_count) :
    "coordinator-${substr("abcdefghijklmnopqrstuvwxyz", i, 1)}" => i
  }

  # Full list of coordinator node IDs. Workers and observer use this to know
  # which rows to look up in the peers table. Coordinator itself filters its
  # own ID out at runtime to derive EXPECTED_PEER_IDS.
  all_node_ids = [for k, _ in local.node_ids : k]
}

# ── ECS Cluster ───────────────────────────────────────────────────────────────

resource "aws_ecs_cluster" "main" {
  name = "${local.prefix}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-cluster" })
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

# ── CloudWatch Log Group ──────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "coordinator" {
  name              = "/ecs/${local.prefix}/coordinator"
  retention_in_days = 7
  tags              = local.common_tags
}

# ── Task Definitions (one per node) ──────────────────────────────────────────
# Each node gets its own task definition so NODE_ID and PEER_ADDRS
# can be baked in as environment variables without runtime overrides.

resource "aws_ecs_task_definition" "coordinator" {
  for_each = local.node_ids

  family                   = "${local.prefix}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.coordinator_cpu
  memory                   = var.coordinator_memory
  task_role_arn            = var.lab_role_arn
  execution_role_arn       = var.lab_role_arn

  container_definitions = jsonencode([{
    name      = "coordinator"
    image     = var.coordinator_image
    essential = true

    portMappings = [
      {
        containerPort = var.grpc_port
        protocol      = "tcp"
      },
      {
        containerPort = var.http_port
        protocol      = "tcp"
      },
    ]

    environment = [
      { name = "NODE_ID",   value = each.key },
      { name = "GRPC_PORT", value = tostring(var.grpc_port) },
      { name = "HTTP_PORT", value = tostring(var.http_port) },
      { name = "PEERS_TABLE", value = var.peers_table_name },
      {
        name  = "EXPECTED_PEER_IDS"
        value = join(",", [for id in local.all_node_ids : id if id != each.key])
      },
      { name = "SQS_INGEST_URL",        value = var.ingest_queue_url },
      { name = "SQS_ASSIGNMENT_URL",    value = var.assignment_queue_url },
      { name = "DYNAMO_TABLE",           value = var.tasks_table_name },
      { name = "SNAPSHOT_S3_BUCKET",     value = var.raft_snapshots_bucket },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.coordinator.name
        awslogs-region        = data.aws_region.current.name
        awslogs-stream-prefix = each.key
      }
    }

    # Health check: verify gRPC port is accepting connections.
    # ECS restarts the task if this fails 3 consecutive times.
    healthCheck = {
      command     = ["CMD-SHELL", "nc -z localhost ${var.grpc_port} || exit 1"]
      interval    = 10
      timeout     = 5
      retries     = 3
      startPeriod = 15
    }
  }])

  tags = merge(local.common_tags, { NodeID = each.key })
}

# ── ECS Services (one per coordinator node) ───────────────────────────────────
# Each service runs exactly 1 task and registers it with Cloud Map so
# peers can resolve it by DNS name (coordinator-a.raft.local, etc.)

resource "aws_ecs_service" "coordinator" {
  for_each = local.node_ids

  name            = "${local.prefix}-${each.key}"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.coordinator[each.key].arn
  desired_count   = 1
  launch_type     = "FARGATE"

  # Never let the service drop to 0 running tasks during a deployment —
  # losing a node temporarily could drop a 3-node cluster below quorum.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.coordinator_security_group_id]
    assign_public_ip = false
  }

  # Peer discovery is via DynamoDB peers table (see coordinator bootstrap);
  # no Cloud Map registration needed here.

  lifecycle {
    # Don't redeploy during chaos experiments when task definitions update.
    ignore_changes = [task_definition]
  }

  tags = merge(local.common_tags, {
    Name   = "${local.prefix}-${each.key}"
    NodeID = each.key
  })

  depends_on = [aws_ecs_cluster.main]
}

# ── Worker ────────────────────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "worker" {
  name              = "/ecs/${local.prefix}/worker"
  retention_in_days = 7
  tags              = local.common_tags
}

resource "aws_ecs_task_definition" "worker" {
  family                   = "${local.prefix}-worker"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.worker_cpu
  memory                   = var.worker_memory
  task_role_arn            = var.lab_role_arn
  execution_role_arn       = var.lab_role_arn

  container_definitions = jsonencode([{
    name      = "worker"
    image     = var.worker_image
    essential = true

    portMappings = [{
      containerPort = 8081
      protocol      = "tcp"
    }]

    environment = [
      { name = "DYNAMO_TABLE",        value = var.tasks_table_name },
      { name = "S3_BUCKET",           value = var.task_data_bucket },
      { name = "SQS_ASSIGNMENT_URL",  value = var.assignment_queue_url },
      { name = "PEERS_TABLE",         value = var.peers_table_name },
      { name = "COORDINATOR_NODE_IDS", value = join(",", local.all_node_ids) },
      { name = "COORDINATOR_HTTP_PORT", value = tostring(var.http_port) },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.worker.name
        awslogs-region        = data.aws_region.current.name
        awslogs-stream-prefix = "worker"
      }
    }

    healthCheck = {
      command     = ["CMD-SHELL", "nc -z localhost 8081 || exit 1"]
      interval    = 10
      timeout     = 5
      retries     = 3
      startPeriod = 10
    }
  }])

  tags = local.common_tags
}

resource "aws_ecs_service" "worker" {
  name            = "${local.prefix}-worker"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.worker.arn
  desired_count   = var.worker_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.worker_security_group_id]
    assign_public_ip = false
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-worker" })

  depends_on = [aws_ecs_cluster.main]
}

# ── Task Ingest API ──────────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "ingest" {
  name              = "/ecs/${local.prefix}/ingest"
  retention_in_days = 7
  tags              = local.common_tags
}

resource "aws_ecs_task_definition" "ingest" {
  family                   = "${local.prefix}-ingest"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.ingest_cpu
  memory                   = var.ingest_memory
  task_role_arn            = var.lab_role_arn
  execution_role_arn       = var.lab_role_arn

  container_definitions = jsonencode([{
    name      = "ingest"
    image     = var.ingest_image
    essential = true

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "DYNAMO_TABLE", value = var.tasks_table_name },
      { name = "S3_BUCKET",    value = var.task_data_bucket },
      { name = "SQS_QUEUE_URL", value = var.ingest_queue_url },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.ingest.name
        awslogs-region        = data.aws_region.current.name
        awslogs-stream-prefix = "ingest"
      }
    }

    healthCheck = {
      command     = ["CMD-SHELL", "nc -z localhost 8080 || exit 1"]
      interval    = 10
      timeout     = 5
      retries     = 3
      startPeriod = 10
    }
  }])

  tags = local.common_tags
}

resource "aws_ecs_service" "ingest" {
  name            = "${local.prefix}-ingest"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.ingest.arn
  desired_count   = var.ingest_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.ingest_security_group_id]
    assign_public_ip = false
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-ingest" })

  depends_on = [aws_ecs_cluster.main]
}

# ── State Observer ───────────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "observer" {
  name              = "/ecs/${local.prefix}/observer"
  retention_in_days = 7
  tags              = local.common_tags
}

resource "aws_ecs_task_definition" "observer" {
  family                   = "${local.prefix}-observer"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  task_role_arn            = var.lab_role_arn
  execution_role_arn       = var.lab_role_arn

  container_definitions = jsonencode([{
    name      = "observer"
    image     = var.observer_image
    essential = true

    portMappings = [{
      containerPort = 9090
      protocol      = "tcp"
    }]

    environment = [
      { name = "PEERS_TABLE",          value = var.peers_table_name },
      { name = "COORDINATOR_NODE_IDS", value = join(",", local.all_node_ids) },
      { name = "COORDINATOR_HTTP_PORT", value = tostring(var.http_port) },
      { name = "AWS_REGION",     value = data.aws_region.current.name },
      { name = "CW_NAMESPACE",   value = "Driftless/Raft" },
      { name = "SNS_TOPIC_ARN",  value = var.sns_topic_arn },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.observer.name
        awslogs-region        = data.aws_region.current.name
        awslogs-stream-prefix = "observer"
      }
    }
  }])

  tags = local.common_tags
}

resource "aws_ecs_service" "observer" {
  name            = "${local.prefix}-observer"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.observer.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.observer_security_group_id]
    assign_public_ip = false
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-observer" })

  depends_on = [aws_ecs_cluster.main]
}

# ── Data sources ──────────────────────────────────────────────────────────────

data "aws_region" "current" {}

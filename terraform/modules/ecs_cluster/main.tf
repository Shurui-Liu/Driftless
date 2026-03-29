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
    "coordinator-${chr(97 + i)}" => i
  }

  # Build PEER_ADDRS for each node — all peers except itself.
  # e.g. for coordinator-a: "coordinator-b.raft.local:50051,coordinator-c.raft.local:50051"
  all_peer_addrs = [
    for i in range(var.coordinator_count) :
    "coordinator-${chr(97 + i)}.raft.local:${var.grpc_port}"
  ]
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

    portMappings = [{
      containerPort = var.grpc_port
      protocol      = "tcp"
    }]

    environment = [
      # Identity — this is how the node knows who it is in the Raft cluster.
      { name = "NODE_ID",   value = each.key },
      { name = "GRPC_PORT", value = tostring(var.grpc_port) },

      # PEER_ADDRS: all coordinators except this node itself.
      {
        name  = "PEER_ADDRS"
        value = join(",", [
          for addr in local.all_peer_addrs :
          addr if addr != "${each.key}.raft.local:${var.grpc_port}"
        ])
      },

      # AWS resources — passed in from the other modules.
      { name = "INGEST_QUEUE_URL",      value = var.ingest_queue_url },
      { name = "ASSIGNMENT_QUEUE_URL",  value = var.assignment_queue_url },
      { name = "RESULTS_QUEUE_URL",     value = var.results_queue_url },
      { name = "TASKS_TABLE",           value = var.tasks_table_name },
      { name = "RAFT_STATE_TABLE",      value = var.raft_state_table_name },
      { name = "TASK_DATA_BUCKET",      value = var.task_data_bucket },
      { name = "RAFT_SNAPSHOTS_BUCKET", value = var.raft_snapshots_bucket },
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

  # Cloud Map registration — gives this task its DNS name.
  service_registries {
    registry_arn = var.service_discovery_service_arn
  }

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

# ── Data sources ──────────────────────────────────────────────────────────────

data "aws_region" "current" {}

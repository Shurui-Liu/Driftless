locals {
  prefix = "${var.project}-${var.environment}"

  common_tags = merge(var.tags, {
    Project     = var.project
    Environment = var.environment
    ManagedBy   = "terraform"
  })
}

# ── VPC ───────────────────────────────────────────────────────────────────────

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true  # required for Cloud Map service discovery
  enable_dns_hostnames = true  # required for Cloud Map service discovery

  tags = merge(local.common_tags, { Name = "${local.prefix}-vpc" })
}

# ── Subnets ───────────────────────────────────────────────────────────────────
# Public subnets — for the NAT gateway and (later) ALB in front of the ingest API.
# Private subnets — ECS tasks run here, no direct internet exposure.

resource "aws_subnet" "public" {
  count             = length(var.availability_zones)
  vpc_id            = aws_vpc.main.id
  cidr_block        = var.public_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  map_public_ip_on_launch = true

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-public-${var.availability_zones[count.index]}"
    Tier = "public"
  })
}

resource "aws_subnet" "private" {
  count             = length(var.availability_zones)
  vpc_id            = aws_vpc.main.id
  cidr_block        = var.private_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name = "${local.prefix}-private-${var.availability_zones[count.index]}"
    Tier = "private"
  })
}

# ── Internet Gateway + NAT ────────────────────────────────────────────────────
# IGW allows the public subnet to reach the internet.
# NAT gateway lets private ECS tasks reach AWS APIs (SQS, DynamoDB, S3, ECR)
# without being directly reachable from the internet.
# Dev uses one NAT gateway (cost saving). Prod should use one per AZ.

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.common_tags, { Name = "${local.prefix}-igw" })
}

resource "aws_eip" "nat" {
  count  = 1 # one NAT GW for dev; increase to length(var.availability_zones) for prod
  domain = "vpc"
  tags   = merge(local.common_tags, { Name = "${local.prefix}-nat-eip-${count.index}" })
}

resource "aws_nat_gateway" "main" {
  count         = 1
  allocation_id = aws_eip.nat[0].id
  subnet_id     = aws_subnet.public[0].id # NAT GW lives in a public subnet
  tags          = merge(local.common_tags, { Name = "${local.prefix}-nat-${count.index}" })
  depends_on    = [aws_internet_gateway.main]
}

# ── Route Tables ──────────────────────────────────────────────────────────────

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-rt-public" })
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main[0].id
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-rt-private" })
}

resource "aws_route_table_association" "private" {
  count          = length(aws_subnet.private)
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ── Security Groups ───────────────────────────────────────────────────────────

# Coordinator security group — allows gRPC between coordinator nodes,
# and outbound to AWS APIs (SQS, DynamoDB, S3).
resource "aws_security_group" "coordinator" {
  name        = "${local.prefix}-coordinator-sg"
  description = "Raft coordinator nodes — gRPC peer traffic"
  vpc_id      = aws_vpc.main.id

  # Allow coordinators to call each other over gRPC.
  ingress {
    description = "gRPC from other coordinators"
    from_port   = var.grpc_port
    to_port     = var.grpc_port
    protocol    = "tcp"
    self        = true # only allows traffic from within this same security group
  }

  # Allow all outbound — ECS tasks need to reach SQS, DynamoDB, S3, ECR.
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-coordinator-sg" })
}

# Worker security group — no inbound needed (workers only poll SQS).
resource "aws_security_group" "worker" {
  name        = "${local.prefix}-worker-sg"
  description = "Worker nodes — outbound only"
  vpc_id      = aws_vpc.main.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-worker-sg" })
}

# ── Cloud Map (Service Discovery) ─────────────────────────────────────────────
# Cloud Map gives each coordinator ECS task a stable DNS name so peers can
# find each other without hardcoding IPs.
# e.g. "coordinator-a.raft.local:50051" resolves to the task's private IP.

resource "aws_service_discovery_private_dns_namespace" "raft" {
  name        = "raft.local"
  description = "Private DNS namespace for Raft coordinator service discovery"
  vpc         = aws_vpc.main.id
  tags        = local.common_tags
}

resource "aws_service_discovery_service" "coordinator" {
  name = "coordinator"

  dns_config {
    namespace_id = aws_service_discovery_private_dns_namespace.raft.id

    dns_records {
      ttl  = 10 # low TTL so new tasks are discovered quickly after restarts
      type = "A"
    }

    routing_policy = "MULTIVALUE" # returns all healthy task IPs, not just one
  }

  health_check_custom_config {
    failure_threshold = 1
  }

  tags = local.common_tags
}

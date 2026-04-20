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
  description = "Raft coordinator nodes - gRPC peer traffic"
  vpc_id      = aws_vpc.main.id

  # Allow coordinators to call each other over gRPC.
  ingress {
    description = "gRPC from other coordinators"
    from_port   = var.grpc_port
    to_port     = var.grpc_port
    protocol    = "tcp"
    self        = true
  }

  # HTTP state server — workers send heartbeats, observer polls /state,
  # leaders ship snapshots to followers via /install-snapshot.
  ingress {
    description = "HTTP from VPC (heartbeat, state, snapshot)"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
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

# Worker security group — health-check port + outbound for SQS/DynamoDB/S3/heartbeat.
resource "aws_security_group" "worker" {
  name        = "${local.prefix}-worker-sg"
  description = "Worker nodes - outbound only (polls SQS, heartbeats to coordinator)"
  vpc_id      = aws_vpc.main.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-worker-sg" })
}

# Ingest API security group — accepts HTTP traffic on 8080.
resource "aws_security_group" "ingest" {
  name        = "${local.prefix}-ingest-sg"
  description = "Task Ingest API - HTTP inbound"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTP from anywhere (load generator + bench client)"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-ingest-sg" })
}

# Observer security group — Prometheus metrics on 9090, outbound to coordinators.
resource "aws_security_group" "observer" {
  name        = "${local.prefix}-observer-sg"
  description = "State Observer - Prometheus + outbound to coordinators"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "Prometheus scrape"
    from_port   = 9090
    to_port     = 9090
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.prefix}-observer-sg" })
}

# Peer discovery is handled via a DynamoDB peers table in the storage module,
# since the voclabs sandbox denies servicediscovery:* actions. See
# raft/cmd/coordinator/main.go for the register/poll flow.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  # Uncomment after first apply once S3 bucket exists.
  # backend "s3" {
  #   bucket = "raft-coordinator-tfstate"
  #   key    = "dev/main.tfstate"
  #   region = "us-east-1"
  # }
}

provider "aws" {
  region = "us-east-1"
}

# ── Networking ────────────────────────────────────────────────────────────────
# Dev: single AZ to keep NAT gateway costs low.

module "networking" {
  source      = "../../modules/networking"
  environment = "dev"
  project     = "raft-coordinator"

  availability_zones   = ["us-east-1a"]
  public_subnet_cidrs  = ["10.0.1.0/24"]
  private_subnet_cidrs = ["10.0.11.0/24"]
}

# ── Storage ───────────────────────────────────────────────────────────────────
# Replace YOUR_ACCOUNT_ID with your real AWS account ID before applying.
# Find it with: aws sts get-caller-identity --query Account --output text

module "storage" {
  source      = "../../modules/storage"
  environment = "dev"
  project     = "raft-coordinator"

  lab_role_arn                = "arn:aws:iam::YOUR_ACCOUNT_ID:role/labRole"
  task_payload_retention_days = 7
  raft_log_retention_days     = 30
}

# ── Messaging ─────────────────────────────────────────────────────────────────

module "messaging" {
  source      = "../../modules/messaging"
  environment = "dev"
  project     = "raft-coordinator"

  max_receive_count                     = 3
  assignment_visibility_timeout_seconds = 300
}

# ── ECS Cluster ───────────────────────────────────────────────────────────────
# Dev runs a single coordinator node. Change coordinator_count to 3 in prod.
# Replace the coordinator_image with your ECR image URI after pushing.

module "ecs_cluster" {
  source      = "../../modules/ecs_cluster"
  environment = "dev"
  project     = "raft-coordinator"

  # From storage module
  lab_role_arn          = module.storage.lab_role_arn
  tasks_table_name      = module.storage.tasks_table_name
  raft_state_table_name = module.storage.raft_state_table_name
  task_data_bucket      = module.storage.task_data_bucket
  raft_snapshots_bucket = module.storage.raft_snapshots_bucket

  # From networking module
  private_subnet_ids            = module.networking.private_subnet_ids
  coordinator_security_group_id = module.networking.coordinator_security_group_id
  service_discovery_service_arn = module.networking.service_discovery_service_arn

  # From messaging module
  ingest_queue_url     = module.messaging.ingest_queue_url
  assignment_queue_url = module.messaging.assignment_queue_url
  results_queue_url    = module.messaging.results_queue_url

  # Coordinator config
  coordinator_image = "YOUR_ACCOUNT_ID.dkr.ecr.us-east-1.amazonaws.com/raft-coordinator:latest"
  coordinator_count = 1   # single node for dev; set to 3 in prod
  coordinator_cpu   = 512
  coordinator_memory = 1024
}

# ── Outputs ───────────────────────────────────────────────────────────────────

output "vpc_id"                   { value = module.networking.vpc_id }
output "private_subnet_ids"       { value = module.networking.private_subnet_ids }
output "coordinator_sg_id"        { value = module.networking.coordinator_security_group_id }
output "ingest_queue_url"         { value = module.messaging.ingest_queue_url }
output "assignment_queue_url"     { value = module.messaging.assignment_queue_url }
output "results_queue_url"        { value = module.messaging.results_queue_url }
output "tasks_table_name"         { value = module.storage.tasks_table_name }
output "raft_state_table_name"    { value = module.storage.raft_state_table_name }
output "task_data_bucket"         { value = module.storage.task_data_bucket }
output "raft_snapshots_bucket"    { value = module.storage.raft_snapshots_bucket }
output "ecs_cluster_name"         { value = module.ecs_cluster.cluster_name }
output "coordinator_log_group"    { value = module.ecs_cluster.log_group_name }

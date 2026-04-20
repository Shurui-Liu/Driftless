terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = "us-east-1"
}

# ── Input Variables ───────────────────────────────────────────────────────────

variable "coordinator_count" {
  description = "Number of coordinator nodes: 3 or 5 for Experiment 2"
  type        = number
  default     = 3

  validation {
    condition     = var.coordinator_count % 2 == 1
    error_message = "coordinator_count must be odd (3 or 5) for Raft quorum."
  }
}

variable "account_id" {
  description = "AWS account ID (run: aws sts get-caller-identity --query Account --output text)"
  type        = string
}

variable "coordinator_image" {
  description = "ECR image URI for the Raft coordinator"
  type        = string
}

variable "worker_image" {
  description = "ECR image URI for the worker node"
  type        = string
}

variable "ingest_image" {
  description = "ECR image URI for the task ingest API"
  type        = string
}

variable "observer_image" {
  description = "ECR image URI for the state observer"
  type        = string
}

# ── Networking ────────────────────────────────────────────────────────────────
# Use a separate CIDR range from dev to allow both environments to coexist.

module "networking" {
  source      = "../../modules/networking"
  environment = "experiment"
  project     = "raft-coordinator"

  vpc_cidr             = "10.2.0.0/16"
  availability_zones   = ["us-east-1a"]
  public_subnet_cidrs  = ["10.2.1.0/24"]
  private_subnet_cidrs = ["10.2.11.0/24"]
}

# ── Storage ───────────────────────────────────────────────────────────────────

module "storage" {
  source      = "../../modules/storage"
  environment = "experiment"
  project     = "raft-coordinator"

  lab_role_arn                = "arn:aws:iam::${var.account_id}:role/LabRole"
  task_payload_retention_days = 1   # keep data for 1 day only — experiment data
  raft_log_retention_days     = 7
}

# ── Messaging ─────────────────────────────────────────────────────────────────

module "messaging" {
  source      = "../../modules/messaging"
  environment = "experiment"
  project     = "raft-coordinator"

  max_receive_count                     = 3
  assignment_visibility_timeout_seconds = 300
}

# ── ECS Cluster ───────────────────────────────────────────────────────────────
# coordinator_count is the key variable — swap between 3node.tfvars / 5node.tfvars.
# Bumped CPU/memory vs dev to handle 1000 tasks/min load.

module "ecs_cluster" {
  source      = "../../modules/ecs_cluster"
  environment = "experiment"
  project     = "raft-coordinator"

  lab_role_arn          = module.storage.lab_role_arn
  tasks_table_name      = module.storage.tasks_table_name
  raft_state_table_name = module.storage.raft_state_table_name
  task_data_bucket      = module.storage.task_data_bucket
  raft_snapshots_bucket = module.storage.raft_snapshots_bucket

  private_subnet_ids            = module.networking.private_subnet_ids
  coordinator_security_group_id = module.networking.coordinator_security_group_id
  worker_security_group_id      = module.networking.worker_security_group_id
  ingest_security_group_id      = module.networking.ingest_security_group_id
  observer_security_group_id    = module.networking.observer_security_group_id
  peers_table_name              = module.storage.peers_table_name

  ingest_queue_url     = module.messaging.ingest_queue_url
  assignment_queue_url = module.messaging.assignment_queue_url
  results_queue_url    = module.messaging.results_queue_url

  coordinator_image  = var.coordinator_image
  coordinator_count  = var.coordinator_count
  coordinator_cpu    = 1024   # 1 vCPU
  coordinator_memory = 2048   # 2 GB

  worker_image   = var.worker_image
  ingest_image   = var.ingest_image
  observer_image = var.observer_image
}

# ── Outputs ───────────────────────────────────────────────────────────────────

output "tasks_table_name"      { value = module.storage.tasks_table_name }
output "ingest_queue_url"      { value = module.messaging.ingest_queue_url }
output "assignment_queue_url"  { value = module.messaging.assignment_queue_url }
output "ecs_cluster_name"      { value = module.ecs_cluster.cluster_name }
output "coordinator_log_group" { value = module.ecs_cluster.log_group_name }
output "coordinator_count"     { value = var.coordinator_count }

variable "environment" {
  description = "Deployment environment (dev or prod)"
  type        = string
}

variable "project" {
  description = "Project name prefix for all resource names"
  type        = string
  default     = "raft-coordinator"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

# Dev uses 1 AZ, prod uses 3. Pass in the AZ list from the environment config.
variable "availability_zones" {
  description = "List of AZs to deploy subnets into"
  type        = list(string)
}

variable "public_subnet_cidrs" {
  description = "CIDR blocks for public subnets (one per AZ) — used by ALB"
  type        = list(string)
  default     = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
}

variable "private_subnet_cidrs" {
  description = "CIDR blocks for private subnets (one per AZ) — used by ECS tasks"
  type        = list(string)
  default     = ["10.0.11.0/24", "10.0.12.0/24", "10.0.13.0/24"]
}

variable "grpc_port" {
  description = "Port the Raft coordinator gRPC server listens on"
  type        = number
  default     = 50051
}

variable "tags" {
  description = "Additional tags to apply to all resources"
  type        = map(string)
  default     = {}
}

variable "environment" {
  description = "Deployment environment (dev or prod)"
  type        = string
}

variable "project" {
  description = "Project name prefix for all resource names"
  type        = string
  default     = "raft-coordinator"
}

# How long a worker has to process a message before it becomes visible again.
# Set high enough to cover your worst-case task execution time.
variable "assignment_visibility_timeout_seconds" {
  description = "Visibility timeout for the assignment queue (seconds)"
  type        = number
  default     = 300 # 5 minutes
}

variable "ingest_visibility_timeout_seconds" {
  description = "Visibility timeout for the ingest queue (seconds)"
  type        = number
  default     = 60
}

variable "results_visibility_timeout_seconds" {
  description = "Visibility timeout for the results queue (seconds)"
  type        = number
  default     = 60
}

# Messages that fail processing this many times go to the DLQ.
variable "max_receive_count" {
  description = "Number of receive attempts before a message is sent to the DLQ"
  type        = number
  default     = 3
}

variable "tags" {
  description = "Additional tags to apply to all resources"
  type        = map(string)
  default     = {}
}

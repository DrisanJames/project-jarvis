###############################################################################
# variables.tf — inputs for the MSK event-backbone module.
#
# All networking inputs (vpc_id, subnet_ids, ecs_task_sg_id) MUST be filled in
# by the operator from the live production environment (us-west-2, account
# 146361001621, cluster apex-cluster, service ignite-service). They are left
# unset here on purpose — see README.md "Prereqs / discover the inputs".
###############################################################################

variable "aws_region" {
  description = "AWS region. Production is us-west-2 — do NOT point this at us-east-1 (that is a separate, non-prod environment)."
  type        = string
  default     = "us-west-2"
}

variable "aws_profile" {
  description = "Local AWS named profile used for plan/apply. Production profile is 'jamesventure'."
  type        = string
  default     = "jamesventure"
}

variable "cluster_name" {
  description = "Name of the MSK cluster."
  type        = string
  default     = "apex-events"
}

variable "kafka_version" {
  description = "Apache Kafka version for the MSK Provisioned cluster."
  type        = string
  default     = "3.6.0"
}

# ---------------------------------------------------------------------------
# Networking — OPERATOR MUST FILL THESE IN. No safe defaults exist.
# Discover them from the live prod VPC (the one apex-cluster / ignite-service
# runs in). See README.md.
# ---------------------------------------------------------------------------

variable "vpc_id" {
  description = "VPC ID of the production network (the VPC apex-cluster ECS tasks run in). e.g. vpc-xxxxxxxx."
  type        = string
}

variable "subnet_ids" {
  description = <<-EOT
    Exactly 3 PRIVATE subnet IDs, one per AZ in us-west-2 (us-west-2a/b/c).
    Brokers are placed one per subnet. These must be private (no public route)
    and must be in the same VPC as the ECS tasks. e.g. ["subnet-aaa","subnet-bbb","subnet-ccc"].
  EOT
  type        = list(string)

  validation {
    condition     = length(var.subnet_ids) == 3
    error_message = "Provide exactly 3 private subnet IDs (one per AZ) for a 3-broker / 3-AZ cluster."
  }
}

variable "ecs_task_sg_id" {
  description = <<-EOT
    Security group ID attached to the ignite-server ECS task ENIs. This is the
    ONLY source allowed inbound to the Kafka IAM port (9098) on the MSK SG.
    Discover it from the running task's networkInterfaces. e.g. sg-xxxxxxxx.
  EOT
  type        = string
}

# ---------------------------------------------------------------------------
# Broker sizing
# ---------------------------------------------------------------------------

variable "broker_instance_type" {
  description = "MSK broker instance type."
  type        = string
  default     = "kafka.m7g.large"
}

variable "broker_count" {
  description = "Number of broker nodes. Must be a multiple of the number of AZs (3)."
  type        = number
  default     = 3

  validation {
    condition     = var.broker_count == 3
    error_message = "This module is wired for 3 brokers across 3 AZs. Changing this requires revisiting subnet_ids and replication settings."
  }
}

variable "broker_ebs_volume_size_gb" {
  description = "gp3 EBS volume size per broker, in GiB."
  type        = number
  default     = 100
}

# ---------------------------------------------------------------------------
# Tags
# ---------------------------------------------------------------------------

variable "tags" {
  description = "Tags applied to all resources."
  type        = map(string)
  default = {
    Project     = "project-jarvis"
    Component   = "event-backbone"
    ManagedBy   = "terraform"
    Environment = "production"
    Cluster     = "apex-cluster"
  }
}

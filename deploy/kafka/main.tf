###############################################################################
# main.tf — Kafka event backbone for Project Jarvis (AWS MSK Provisioned).
#
# ⚠️  REVIEWED ARTIFACT — NOT APPLIED. Applying this provisions a REAL,
#     billed MSK cluster (~$0.5–0.7k/mo). Provisioning is a GATED human step
#     that needs operator approval + production AWS creds. See README.md.
#
# Production target: region us-west-2, account 146361001621, profile
# jamesventure, alongside ECS cluster apex-cluster / service ignite-service.
# Do NOT point this at us-east-1 (separate, non-prod environment).
###############################################################################

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.4"
    }
  }

  # NOTE: state backend intentionally left local for review. Before a real
  # apply, the operator should wire an S3 backend + DynamoDB lock table so the
  # state isn't trapped on one laptop. Example (commented — fill bucket/table):
  #
  # backend "s3" {
  #   bucket         = "jarvis-terraform-state"
  #   key            = "msk/apex-events/terraform.tfstate"
  #   region         = "us-west-2"
  #   profile        = "jamesventure"
  #   dynamodb_table = "terraform-locks"
  #   encrypt        = true
  # }
}

provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile
}

# ---------------------------------------------------------------------------
# KMS — customer-managed key for MSK at-rest encryption.
# ---------------------------------------------------------------------------

resource "aws_kms_key" "msk" {
  description             = "CMK for ${var.cluster_name} MSK at-rest encryption (EBS + tiered storage)."
  deletion_window_in_days = 30
  enable_key_rotation     = true
  tags                    = var.tags
}

resource "aws_kms_alias" "msk" {
  name          = "alias/${var.cluster_name}-msk"
  target_key_id = aws_kms_key.msk.key_id
}

# ---------------------------------------------------------------------------
# Security group — dedicated to MSK. Inbound 9098 (Kafka SASL/IAM) ONLY from
# the ECS task SG. No public ingress. Egress open (broker-to-broker + AWS APIs).
# ---------------------------------------------------------------------------

resource "aws_security_group" "msk" {
  name        = "${var.cluster_name}-msk-sg"
  description = "MSK brokers — Kafka IAM (9098) inbound from ECS tasks only."
  vpc_id      = var.vpc_id
  tags        = merge(var.tags, { Name = "${var.cluster_name}-msk-sg" })

  lifecycle {
    create_before_destroy = true
  }
}

# Kafka SASL/IAM bootstrap + broker port. This is the only auth path enabled,
# so 9098 is the only client port that needs to be open.
resource "aws_security_group_rule" "msk_ingress_iam_from_ecs" {
  type                     = "ingress"
  description              = "Kafka SASL/IAM (9098) from ignite-server ECS tasks"
  from_port                = 9098
  to_port                  = 9098
  protocol                 = "tcp"
  security_group_id        = aws_security_group.msk.id
  source_security_group_id = var.ecs_task_sg_id
}

# Broker inter-node + ZooKeeper-less metadata traffic stays within the SG.
resource "aws_security_group_rule" "msk_ingress_self" {
  type              = "ingress"
  description       = "Intra-cluster broker traffic"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.msk.id
  self              = true
}

resource "aws_security_group_rule" "msk_egress_all" {
  type              = "egress"
  description       = "All egress (broker replication + AWS API calls)"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  cidr_blocks       = ["0.0.0.0/0"]
  security_group_id = aws_security_group.msk.id
}

# ---------------------------------------------------------------------------
# CloudWatch log group for broker logs.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "msk" {
  name              = "/aws/msk/${var.cluster_name}"
  retention_in_days = 30
  tags              = var.tags
}

# ---------------------------------------------------------------------------
# Cluster-level broker configuration. This is where the durability knobs the
# brief mandates live (they are server.properties, not topic overrides — though
# per-topic replication.factor is set at topic creation in topics.tf).
# ---------------------------------------------------------------------------

resource "aws_msk_configuration" "apex_events" {
  name           = "${var.cluster_name}-broker-config"
  kafka_versions = [var.kafka_version]
  description    = "Durable defaults: RF=3, min.insync.replicas=2, no unclean leader election."

  # MSK requires properties as a flat HEREDOC string.
  server_properties = <<-PROPERTIES
    auto.create.topics.enable=false
    default.replication.factor=3
    min.insync.replicas=2
    unclean.leader.election.enable=false
    offsets.topic.replication.factor=3
    transaction.state.log.replication.factor=3
    transaction.state.log.min.isr=2
    num.partitions=12
    log.retention.hours=168
    compression.type=producer
  PROPERTIES
}

# ---------------------------------------------------------------------------
# The MSK Provisioned cluster.
# ---------------------------------------------------------------------------

resource "aws_msk_cluster" "apex_events" {
  cluster_name           = var.cluster_name
  kafka_version          = var.kafka_version
  number_of_broker_nodes = var.broker_count

  broker_node_group_info {
    instance_type   = var.broker_instance_type
    client_subnets  = var.subnet_ids
    security_groups = [aws_security_group.msk.id]

    storage_info {
      ebs_storage_info {
        volume_size = var.broker_ebs_volume_size_gb
        # gp3 throughput provisioning. Defaults are fine for event-backbone
        # throughput; raise here if topic lag indicates IO starvation.
        provisioned_throughput {
          enabled = false
        }
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.apex_events.arn
    revision = aws_msk_configuration.apex_events.latest_revision
  }

  # --- Encryption: TLS enforced in transit; CMK at rest. ---
  encryption_info {
    encryption_at_rest_kms_key_arn = aws_kms_key.msk.arn

    encryption_in_transit {
      # TLS only between clients and brokers; no PLAINTEXT.
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  # --- Auth: IAM (SASL/IAM) ONLY. SCRAM, unauthenticated, and TLS-mTLS off. ---
  client_authentication {
    sasl {
      iam   = true
      scram = false
    }
    unauthenticated = false
    # tls block intentionally omitted -> mTLS disabled.
  }

  # --- Open Monitoring: Prometheus JMX + node exporters. ---
  open_monitoring {
    prometheus {
      jmx_exporter {
        enabled_in_broker = true
      }
      node_exporter {
        enabled_in_broker = true
      }
    }
  }

  # --- Broker logs -> CloudWatch. ---
  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.msk.name
      }
    }
  }

  tags = var.tags

  lifecycle {
    # Broker count / instance type / storage changes are disruptive — force a
    # deliberate review rather than an accidental in-place replacement.
    prevent_destroy = true
  }
}

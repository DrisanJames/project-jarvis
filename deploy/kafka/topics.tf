###############################################################################
# topics.tf — topic management approach (DOCUMENTED, NOT Terraform-managed).
#
# WHY NOT TERRAFORM-NATIVE TOPICS:
#   Topic CRUD on an MSK *Provisioned* cluster requires a Kafka admin client
#   that can reach the brokers on 9098 AND sign with SASL/IAM. The only TF
#   option is the community `Mongey/kafka` provider, which:
#     (a) needs a live TCP path to the private brokers from wherever `terraform
#         apply` runs (they live in private subnets — your laptop can't reach
#         them without a bastion/VPN), and
#     (b) has weak SASL/IAM support (it expects SASL/SCRAM or mTLS).
#   So topic creation would fail at plan-from-laptop time and couples cluster
#   provisioning to broker reachability. That is the wrong layer.
#
# DECISION: provision the CLUSTER with Terraform (main.tf); create TOPICS with
#   the bundled admin script `create_topics.sh`, run from INSIDE the VPC (an
#   ECS exec session on an ignite-server task, or a bastion) where 9098 + IAM
#   auth both work. The exact topic specs are the source of truth in
#   `topics.json` and are mirrored in the comment block below.
#
# This file is intentionally declaration-only: a local_file that materializes
# the topic spec next to the script, so the spec is captured in TF state/plan
# output for review parity, without any apply-time broker dependency.
###############################################################################

locals {
  # Canonical topic specs. Mirrored in create_topics.sh / topics.json.
  # cleanup.policy=compact for the keyed suppression state stream; delete (with
  # retention) for the event + DLQ streams.
  kafka_topics = [
    {
      name               = "evt.lake.v1"
      partitions         = 12
      replication_factor = 3
      config = {
        "cleanup.policy"      = "delete"
        "retention.ms"        = "604800000" # 7 days
        "min.insync.replicas" = "2"
      }
    },
    {
      name               = "evt.ingest.v1"
      partitions         = 12
      replication_factor = 3
      config = {
        "cleanup.policy"      = "delete"
        "retention.ms"        = "604800000" # 7 days
        "min.insync.replicas" = "2"
      }
    },
    {
      name               = "suppression.state"
      partitions         = 6
      replication_factor = 3
      config = {
        "cleanup.policy"      = "compact"
        "min.insync.replicas" = "2"
        # compaction tuning: aggressive so the keyed state stays current.
        "min.cleanable.dirty.ratio" = "0.1"
        "segment.ms"                = "86400000" # 1 day
        "delete.retention.ms"       = "86400000" # tombstone visibility window
      }
    },
    {
      name               = "evt.lake.v1.dlq"
      partitions         = 3
      replication_factor = 3
      config = {
        "cleanup.policy"      = "delete"
        "retention.ms"        = "1209600000" # 14 days
        "min.insync.replicas" = "2"
      }
    },
    {
      name               = "evt.ingest.v1.dlq"
      partitions         = 3
      replication_factor = 3
      config = {
        "cleanup.policy"      = "delete"
        "retention.ms"        = "1209600000" # 14 days
        "min.insync.replicas" = "2"
      }
    },
    {
      name               = "suppression.state.dlq"
      partitions         = 3
      replication_factor = 3
      config = {
        "cleanup.policy"      = "delete"
        "retention.ms"        = "1209600000" # 14 days
        "min.insync.replicas" = "2"
      }
    },
  ]
}

# Materialize the spec as JSON next to the script so review/plan output and the
# runtime script can't drift. No broker dependency — pure local file.
resource "local_file" "topics_spec" {
  filename = "${path.module}/topics.json"
  content  = jsonencode({ topics = local.kafka_topics })
}

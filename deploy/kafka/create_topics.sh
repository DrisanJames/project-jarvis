#!/usr/bin/env bash
###############################################################################
# create_topics.sh — create/verify the event-backbone topics on the MSK
# Provisioned cluster using SASL/IAM auth.
#
# ⚠️  RUN FROM INSIDE THE VPC. The brokers live in private subnets on port 9098
#     and require SASL/IAM. This script will NOT work from a laptop unless you
#     have a bastion/VPN path to the brokers. The intended runner is an ECS
#     exec session on an ignite-server task in apex-cluster, or a bastion host.
#
# Prereqs on the runner:
#   - Apache Kafka CLI tools (kafka-topics.sh, kafka-configs.sh) >= 3.6
#   - aws-msk-iam-auth JAR on the classpath (MSK_IAM_JAR), e.g.
#       https://github.com/aws/aws-msk-iam-auth/releases (aws-msk-iam-auth-*-all.jar)
#   - An IAM identity (task role / instance role) with kafka-cluster:* on this
#     cluster's topics + groups.
#   - jq
#
# Usage:
#   BOOTSTRAP="b-1...:9098,b-2...:9098,b-3...:9098" \
#   MSK_IAM_JAR=/opt/aws-msk-iam-auth-all.jar \
#   ./create_topics.sh            # create + apply config
#   ./create_topics.sh --dry-run  # print the commands only
#
# BOOTSTRAP = the Terraform output `bootstrap_brokers_sasl_iam`.
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SPEC="${TOPICS_SPEC:-$SCRIPT_DIR/topics.json}"
DRY_RUN=0
[ "${1:-}" = "--dry-run" ] && DRY_RUN=1

: "${BOOTSTRAP:?Set BOOTSTRAP to the bootstrap_brokers_sasl_iam string (port 9098)}"
: "${MSK_IAM_JAR:?Set MSK_IAM_JAR to the path of aws-msk-iam-auth-*-all.jar}"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v kafka-topics.sh >/dev/null || { echo "kafka-topics.sh (Kafka CLI) is required" >&2; exit 1; }

# SASL/IAM client properties for the Kafka CLI.
CLIENT_PROPS="$(mktemp)"
trap 'rm -f "$CLIENT_PROPS"' EXIT
cat > "$CLIENT_PROPS" <<EOF
security.protocol=SASL_SSL
sasl.mechanism=AWS_MSK_IAM
sasl.jaas.config=software.amazon.msk.auth.iam.IAMLoginModule required;
sasl.client.callback.handler.class=software.amazon.msk.auth.iam.IAMClientCallbackHandler
EOF

export KAFKA_OPTS="-cp ${MSK_IAM_JAR}${CLASSPATH:+:$CLASSPATH}"

run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf 'DRY-RUN: %s\n' "$*"
  else
    "$@"
  fi
}

echo "=== Event-backbone topic provisioning ==="
echo "Bootstrap: $BOOTSTRAP"
echo "Spec:      $SPEC"
echo ""

topic_count="$(jq '.topics | length' "$SPEC")"
for i in $(seq 0 $((topic_count - 1))); do
  name="$(jq -r ".topics[$i].name" "$SPEC")"
  parts="$(jq -r ".topics[$i].partitions" "$SPEC")"
  rf="$(jq -r ".topics[$i].replication_factor" "$SPEC")"

  # Build --config flags from the config object.
  config_args=()
  while IFS=$'\t' read -r k v; do
    config_args+=(--config "$k=$v")
  done < <(jq -r ".topics[$i].config | to_entries[] | [.key, .value] | @tsv" "$SPEC")

  echo "--- $name (partitions=$parts rf=$rf) ---"

  # Idempotent create: skip if it already exists, otherwise create with config.
  if kafka-topics.sh --bootstrap-server "$BOOTSTRAP" --command-config "$CLIENT_PROPS" \
        --describe --topic "$name" >/dev/null 2>&1; then
    echo "exists — reconciling config"
    run kafka-configs.sh --bootstrap-server "$BOOTSTRAP" --command-config "$CLIENT_PROPS" \
      --entity-type topics --entity-name "$name" --alter \
      $(printf -- '--add-config %s=%s ' \
          $(jq -r ".topics[$i].config | to_entries[] | \"\(.key) \(.value)\"" "$SPEC" | tr '\n' ' '))
  else
    run kafka-topics.sh --bootstrap-server "$BOOTSTRAP" --command-config "$CLIENT_PROPS" \
      --create --topic "$name" \
      --partitions "$parts" --replication-factor "$rf" \
      "${config_args[@]}"
  fi
done

echo ""
echo "=== Verify ==="
run kafka-topics.sh --bootstrap-server "$BOOTSTRAP" --command-config "$CLIENT_PROPS" --list
echo "Done."

#!/bin/bash
# =============================================================================
# Production environment guardrail for Ignite Upside-Down deploy tooling.
# =============================================================================
# The TRUE production environment is:
#     account  146361001621
#     region   us-west-2
#     cluster  apex-cluster
#     service  ignite-service
# A SEPARATE, WRONG environment exists in us-east-1 (cluster
# ignite-upside-down-cluster, service ignite-api) backed by a DIFFERENT
# database. Deploying or rolling back against the wrong one mutates a
# different prod-like system and its data.
#
# Historically the ONLY protection was prose in CLAUDE.md plus the env-var
# defaults in deploy.sh. This guard makes targeting the wrong environment
# FAIL CLOSED.
#
# Usage: source this file, then call assert_prod_environment before any
# mutating AWS action:
#     source "$SCRIPT_DIR/_guardrails.sh"
#     assert_prod_environment "$ACCOUNT" "$REGION" "$CLUSTER" "$SERVICE" "${AWS_ARGS[@]}"
#
# On the correct prod target it prints one confirmation line and returns 0,
# so deploy behavior is byte-identical. On any mismatch it exits 87.
# Deliberate non-prod use (rare) must opt in with IGNITE_ALLOW_NONPROD=1.

IGNITE_PROD_ACCOUNT="146361001621"
IGNITE_PROD_REGION="us-west-2"
IGNITE_PROD_CLUSTER="apex-cluster"
IGNITE_PROD_SERVICE="ignite-service"

assert_prod_environment() {
  # args: <account_id> <region> <cluster> <service> [aws cli args...]
  local account="$1" region="$2" cluster="$3" service="$4"
  shift 4
  local aws_args=("$@")

  if [ "${IGNITE_ALLOW_NONPROD:-}" = "1" ]; then
    echo "⚠️  IGNITE_ALLOW_NONPROD=1 — environment guardrail BYPASSED" >&2
    echo "    (account=$account region=$region cluster=$cluster service=$service). Proceed with caution." >&2
    return 0
  fi

  local bad=0
  if [ "$account" != "$IGNITE_PROD_ACCOUNT" ]; then
    echo "REFUSING: AWS account '$account' != prod '$IGNITE_PROD_ACCOUNT'" >&2; bad=1
  fi
  if [ "$region" != "$IGNITE_PROD_REGION" ]; then
    echo "REFUSING: AWS region '$region' != prod '$IGNITE_PROD_REGION' — the us-east-1 environment is a DIFFERENT database; never deploy there" >&2; bad=1
  fi
  if [ "$cluster" != "$IGNITE_PROD_CLUSTER" ]; then
    echo "REFUSING: ECS cluster '$cluster' != prod '$IGNITE_PROD_CLUSTER' (wrong env uses 'ignite-upside-down-cluster')" >&2; bad=1
  fi
  if [ "$service" != "$IGNITE_PROD_SERVICE" ]; then
    echo "REFUSING: ECS service '$service' != prod '$IGNITE_PROD_SERVICE' (wrong env uses 'ignite-api')" >&2; bad=1
  fi
  if [ "$bad" -ne 0 ]; then
    echo "" >&2
    echo "Environment guardrail tripped: target is NOT the known-good production environment." >&2
    echo "If this is intentional (rare), re-run with IGNITE_ALLOW_NONPROD=1." >&2
    exit 87
  fi

  # Positive confirmation: the service must actually exist and be ACTIVE in the
  # cluster (catches a right-named-but-wrong-account or torn-down target).
  local svc_status
  svc_status="$(aws ecs describe-services --cluster "$cluster" --services "$service" "${aws_args[@]}" \
    --query 'services[0].status' --output text 2>/dev/null || echo "")"
  if [ "$svc_status" != "ACTIVE" ]; then
    echo "REFUSING: service '$service' is not ACTIVE in cluster '$cluster' (status='$svc_status')." >&2
    echo "Wrong environment, missing service, or invalid credentials. If intentional, re-run with IGNITE_ALLOW_NONPROD=1." >&2
    exit 87
  fi

  echo "✓ Environment guardrail: production confirmed (account=$account region=$region cluster=$cluster service=$service status=ACTIVE)"
}

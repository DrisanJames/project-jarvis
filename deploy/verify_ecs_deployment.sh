#!/bin/bash
set -euo pipefail

AWS_REGION="${AWS_REGION:?AWS_REGION is required}"
ECS_CLUSTER="${ECS_CLUSTER:?ECS_CLUSTER is required}"
ECS_SERVICE="${ECS_SERVICE:?ECS_SERVICE is required}"
CONTAINER_NAME="${CONTAINER_NAME:?CONTAINER_NAME is required}"
EXPECTED_IMAGE_DIGEST="${EXPECTED_IMAGE_DIGEST:?EXPECTED_IMAGE_DIGEST is required}"
EXPECTED_GIT_SHA="${EXPECTED_GIT_SHA:?EXPECTED_GIT_SHA is required}"
EXPECTED_ENV_MANIFEST_SHA="${EXPECTED_ENV_MANIFEST_SHA:-}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-}"
# Post-stability send-liveness budget (REQ-092 DoD 4 / REQ-087).
LIVENESS_WAIT_SECONDS="${LIVENESS_WAIT_SECONDS:-300}"

AWS_ARGS=(--region "$AWS_REGION")
if [ -n "${AWS_PROFILE:-}" ]; then
  AWS_ARGS+=(--profile "$AWS_PROFILE")
fi

echo "Verifying ECS service task definition..."
TASK_DEF_ARN=$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'services[0].taskDefinition' \
  --output text)

TASK_IMAGE=$(aws ecs describe-task-definition \
  --task-definition "$TASK_DEF_ARN" \
  "${AWS_ARGS[@]}" \
  --query "taskDefinition.containerDefinitions[?name=='$CONTAINER_NAME'].image | [0]" \
  --output text)

if [[ "$TASK_IMAGE" != *"@${EXPECTED_IMAGE_DIGEST}" ]]; then
  echo "Task definition image mismatch: $TASK_IMAGE" >&2
  exit 1
fi

# A rollout that "completed" while a second deployment is still ACTIVE means
# the old tasks are still serving. deploy.sh waits for count==1, but a service
# can regress between that wait and here, so assert it once more at the end.
echo "Verifying exactly one active ECS deployment..."
DEPLOYMENT_COUNT=$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'length(services[0].deployments)' \
  --output text)
if [ "$DEPLOYMENT_COUNT" != "1" ]; then
  echo "Expected exactly 1 active deployment, found $DEPLOYMENT_COUNT" >&2
  exit 1
fi

echo "Verifying running ECS tasks..."
TASK_ARNS=$(aws ecs list-tasks \
  --cluster "$ECS_CLUSTER" \
  --service-name "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'taskArns' \
  --output text)

if [ -z "$TASK_ARNS" ]; then
  echo "No running ECS tasks found for service $ECS_SERVICE" >&2
  exit 1
fi

RUNNING_DIGESTS=$(aws ecs describe-tasks \
  --cluster "$ECS_CLUSTER" \
  --tasks $TASK_ARNS \
  "${AWS_ARGS[@]}" \
  --query "tasks[].containers[?name=='$CONTAINER_NAME'].imageDigest | []" \
  --output text)

for digest in $RUNNING_DIGESTS; do
  if [ "$digest" != "$EXPECTED_IMAGE_DIGEST" ]; then
    echo "Running task digest mismatch: $digest" >&2
    exit 1
  fi
done

if [ -n "$PUBLIC_BASE_URL" ]; then
  echo "Verifying live build metadata endpoint..."
  VERSION_JSON=""
  ENDPOINT=""
  for candidate in "${PUBLIC_BASE_URL%/}/version" "${PUBLIC_BASE_URL%/}/health"; do
    for _ in 1 2 3 4 5; do
      if VERSION_JSON=$(curl -fsS "$candidate"); then
        if python3 - "$VERSION_JSON" <<'PY'
import json
import sys

payload = json.loads(sys.argv[1])
if isinstance(payload, dict):
    if "git_sha" in payload:
        raise SystemExit(0)
    if isinstance(payload.get("build"), dict) and "git_sha" in payload["build"]:
        raise SystemExit(0)
raise SystemExit(1)
PY
        then
          ENDPOINT="$candidate"
          break 2
        fi
      fi
      sleep 6
    done
  done

  if [ -z "$ENDPOINT" ]; then
    echo "Failed to fetch build metadata from ${PUBLIC_BASE_URL%/}/version or /health" >&2
    exit 1
  fi

  python3 - "$EXPECTED_GIT_SHA" "$EXPECTED_IMAGE_DIGEST" "$VERSION_JSON" "$EXPECTED_ENV_MANIFEST_SHA" <<'PY'
import json
import sys

expected_sha = sys.argv[1]
expected_digest = sys.argv[2]
payload = json.loads(sys.argv[3])
expected_manifest = sys.argv[4] if len(sys.argv) > 4 else ""

if isinstance(payload.get("build"), dict):
    payload = payload["build"]

actual_sha = payload.get("git_sha")
actual_digest = payload.get("image_digest")

if actual_sha != expected_sha:
    raise SystemExit(f"git_sha mismatch: expected {expected_sha}, got {actual_sha}")
if actual_digest != expected_digest:
    raise SystemExit(f"image_digest mismatch: expected {expected_digest}, got {actual_digest}")
if expected_manifest:
    actual_manifest = payload.get("env_manifest_sha")
    if not actual_manifest:
        print("WARNING: /health.build has no env_manifest_sha - the running build "
              "predates REQ-092; env provenance is UNVERIFIED for this deploy")
    elif actual_manifest != expected_manifest:
        raise SystemExit(
            f"env_manifest_sha mismatch: expected {expected_manifest}, got {actual_manifest}")
if payload.get("tree_dirty") in ("1", True, "true"):
    print("WARNING: this build is stamped APP_TREE_DIRTY=1 - it contains uncommitted code")
PY
fi

# ---------------------------------------------------------------------------
# Send-liveness gate (REQ-092 DoD 4). Proving bytes is not proving behaviour:
# on 2026-09-01 the sha and digest matched for 90 minutes while the Kafka send
# consumer was wedged and nothing left the queue.
#
# PASS when either
#   send_liveness.sent_last_15m > 0        (mail is moving), or
#   send_liveness.queue_ready_rows == 0    (nothing is waiting to move)
#
# Source order:
#   1. /health.send_liveness  - published by the server (REQ-087). Preferred.
#   2. SQL fallback via $DEPLOY_VERIFY_DATABASE_URL (read-only), mirroring
#      agents/jobs/auto_sidecar_daily.py verify_send_liveness():
#        sent_last_15m    = SELECT count(*) FROM mailing_tracking_events
#                             WHERE event_type = 'sent'
#                               AND event_at > now() - interval '15 minutes'
#        queue_ready_rows = SELECT count(*) FROM mailing_campaign_queue
#                             WHERE status = 'queued' AND scheduled_at <= now()
#   3. Neither available -> report UNVERIFIED and do NOT fail the deploy.
# ---------------------------------------------------------------------------
if [ -n "$PUBLIC_BASE_URL" ]; then
  echo "Verifying send liveness (budget ${LIVENESS_WAIT_SECONDS}s)..."
  LIVENESS_STATE="unavailable"
  LIVENESS_ELAPSED=0
  while [ "$LIVENESS_ELAPSED" -lt "$LIVENESS_WAIT_SECONDS" ]; do
    HEALTH_JSON="$(curl -fsS "${PUBLIC_BASE_URL%/}/health" 2>/dev/null || echo '{}')"
    LIVENESS_STATE="$(python3 - "$HEALTH_JSON" <<'PY'
import json, sys
try:
    p = json.loads(sys.argv[1])
except Exception:
    print("unavailable"); raise SystemExit(0)
sl = p.get("send_liveness")
if not isinstance(sl, dict):
    print("unavailable"); raise SystemExit(0)
sent = sl.get("sent_last_15m")
ready = sl.get("queue_ready_rows")
# checked_at == null means the first OutboxSelfCheck tick has not completed.
# Zeroes then mean "not yet measured", NOT "nothing to send" - reading them as
# healthy is the exact false-pass this gate exists to prevent.
if sl.get("checked_at") is None:
    print("unavailable")
elif sent is None or ready is None:
    print("unavailable")
elif sent > 0 or ready == 0:
    print(f"ok sent_last_15m={sent} queue_ready_rows={ready}")
else:
    print(f"stalled sent_last_15m={sent} queue_ready_rows={ready}")
PY
)"
    case "$LIVENESS_STATE" in
      ok*)          echo "  send liveness: $LIVENESS_STATE"; break ;;
      unavailable)  break ;;
      *)            echo "  [${LIVENESS_ELAPSED}s] $LIVENESS_STATE - waiting..." ;;
    esac
    sleep 15
    LIVENESS_ELAPSED=$((LIVENESS_ELAPSED + 15))
  done

  if [ "$LIVENESS_STATE" = "unavailable" ] && [ -n "${DEPLOY_VERIFY_DATABASE_URL:-}" ] && command -v psql >/dev/null 2>&1; then
    echo "  /health.send_liveness absent - using the read-only SQL fallback"
    SENT_15M=$(psql "$DEPLOY_VERIFY_DATABASE_URL" -At -c "SELECT count(*) FROM mailing_tracking_events WHERE event_type = 'sent' AND event_at > now() - interval '15 minutes'" 2>/dev/null || echo "")
    QUEUE_READY=$(psql "$DEPLOY_VERIFY_DATABASE_URL" -At -c "SELECT count(*) FROM mailing_campaign_queue WHERE status = 'queued' AND scheduled_at <= now()" 2>/dev/null || echo "")
    if [ -n "$SENT_15M" ] && [ -n "$QUEUE_READY" ]; then
      if [ "$SENT_15M" -gt 0 ] || [ "$QUEUE_READY" -eq 0 ]; then
        LIVENESS_STATE="ok sent_last_15m=$SENT_15M queue_ready_rows=$QUEUE_READY (sql)"
      else
        LIVENESS_STATE="stalled sent_last_15m=$SENT_15M queue_ready_rows=$QUEUE_READY (sql)"
      fi
    fi
  fi

  case "$LIVENESS_STATE" in
    ok*)
      echo "Send liveness OK: $LIVENESS_STATE" ;;
    unavailable)
      echo "Send liveness UNVERIFIED - the running build does not publish"
      echo "  /health.send_liveness and DEPLOY_VERIFY_DATABASE_URL is unset."
      echo "  Deploy NOT failed on this check." ;;
    *)
      echo "Send liveness FAILED after ${LIVENESS_WAIT_SECONDS}s: $LIVENESS_STATE" >&2
      echo "  The queue has ready rows and nothing has been sent - the transport is wedged." >&2
      echo "  Diagnose: curl -s ${PUBLIC_BASE_URL%/}/health | python3 -m json.tool" >&2
      echo "  Kill switch: KAFKA_SEND_QUEUE_ALL=0 in deploy/env.manifest.json + deploy.sh --config-only" >&2
      exit 1 ;;
  esac
fi

# READ_REPLICA_URL guardrail: if set in the task definition, the replica must
# exist and be available. Prevents silently re-pointing at a deleted replica.
REPLICA_URL=$(aws ecs describe-task-definition \
  --task-definition "$TASK_DEF_ARN" \
  "${AWS_ARGS[@]}" \
  --query "taskDefinition.containerDefinitions[?name=='$CONTAINER_NAME'].environment[?name=='READ_REPLICA_URL'].value | [0][0]" \
  --output text 2>/dev/null || echo "")

if [ -n "$REPLICA_URL" ] && [ "$REPLICA_URL" != "None" ] && [ "$REPLICA_URL" != "" ]; then
  echo "READ_REPLICA_URL is set — verifying apex-postgres-read RDS instance..."
  REPLICA_STATUS=$(aws rds describe-db-instances \
    --db-instance-identifier apex-postgres-read \
    "${AWS_ARGS[@]}" \
    --query 'DBInstances[0].DBInstanceStatus' \
    --output text 2>/dev/null || echo "not-found")
  if [ "$REPLICA_STATUS" != "available" ]; then
    echo "READ_REPLICA_URL is set but apex-postgres-read status is '$REPLICA_STATUS' (expected available)" >&2
    echo "Clear READ_REPLICA_URL or rebuild the replica before deploying." >&2
    exit 1
  fi
  echo "Read replica RDS status: available"
else
  echo "READ_REPLICA_URL not set — SegmentRefreshWorker will use primary"
fi

echo "Deployment verification passed for ${EXPECTED_GIT_SHA} (${EXPECTED_IMAGE_DIGEST})"

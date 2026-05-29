#!/bin/bash
set -euo pipefail

AWS_REGION="${AWS_REGION:?AWS_REGION is required}"
ECS_CLUSTER="${ECS_CLUSTER:?ECS_CLUSTER is required}"
ECS_SERVICE="${ECS_SERVICE:?ECS_SERVICE is required}"
CONTAINER_NAME="${CONTAINER_NAME:?CONTAINER_NAME is required}"
EXPECTED_IMAGE_DIGEST="${EXPECTED_IMAGE_DIGEST:?EXPECTED_IMAGE_DIGEST is required}"
EXPECTED_GIT_SHA="${EXPECTED_GIT_SHA:?EXPECTED_GIT_SHA is required}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-}"

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

  python3 - "$EXPECTED_GIT_SHA" "$EXPECTED_IMAGE_DIGEST" "$VERSION_JSON" <<'PY'
import json
import sys

expected_sha = sys.argv[1]
expected_digest = sys.argv[2]
payload = json.loads(sys.argv[3])

if isinstance(payload.get("build"), dict):
    payload = payload["build"]

actual_sha = payload.get("git_sha")
actual_digest = payload.get("image_digest")

if actual_sha != expected_sha:
    raise SystemExit(f"git_sha mismatch: expected {expected_sha}, got {actual_sha}")
if actual_digest != expected_digest:
    raise SystemExit(f"image_digest mismatch: expected {expected_digest}, got {actual_digest}")
PY
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

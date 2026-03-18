#!/bin/bash
set -euo pipefail

AWS_REGION="${AWS_REGION:-us-west-2}"
AWS_PROFILE="${AWS_PROFILE:-jamesventure}"
ECS_CLUSTER="${ECS_CLUSTER:-apex-cluster}"
ECS_SERVICE="${ECS_SERVICE:-ignite-service}"
TASK_FAMILY="${TASK_FAMILY:-ignite-upside-down}"
CONTAINER_NAME="${CONTAINER_NAME:-ignite-server}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-https://projectjarvis.io}"
TARGET_REVISION="${1:-}"

AWS_ARGS=(--region "$AWS_REGION")
if [ -n "$AWS_PROFILE" ]; then
  AWS_ARGS+=(--profile "$AWS_PROFILE")
fi

echo "=== Ignite Upside-Down Rollback ==="
echo "AWS Region: $AWS_REGION"
echo ""

CURRENT_TASK_DEF_ARN="$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'services[0].taskDefinition' \
  --output text)"

CURRENT_REVISION="$(echo "$CURRENT_TASK_DEF_ARN" | grep -o '[0-9]*$')"
echo "Current task definition: $CURRENT_TASK_DEF_ARN (revision $CURRENT_REVISION)"

if [ -n "$TARGET_REVISION" ]; then
  ROLLBACK_REVISION="$TARGET_REVISION"
  echo "Rolling back to explicitly requested revision: $ROLLBACK_REVISION"
else
  ROLLBACK_REVISION=$((CURRENT_REVISION - 1))
  if [ "$ROLLBACK_REVISION" -lt 1 ]; then
    echo "No previous revision to roll back to (current is revision $CURRENT_REVISION)." >&2
    exit 1
  fi
  echo "Rolling back to previous revision: $ROLLBACK_REVISION"
fi

ROLLBACK_TASK_DEF="$TASK_FAMILY:$ROLLBACK_REVISION"

ROLLBACK_STATUS="$(aws ecs describe-task-definition \
  --task-definition "$ROLLBACK_TASK_DEF" \
  "${AWS_ARGS[@]}" \
  --query 'taskDefinition.status' \
  --output text 2>/dev/null || echo "NOT_FOUND")"

if [ "$ROLLBACK_STATUS" != "ACTIVE" ]; then
  echo "Task definition $ROLLBACK_TASK_DEF is $ROLLBACK_STATUS — cannot roll back to it." >&2
  echo ""
  echo "Available recent revisions:"
  for rev in $(seq "$CURRENT_REVISION" -1 $((CURRENT_REVISION > 10 ? CURRENT_REVISION - 10 : 1))); do
    STATUS="$(aws ecs describe-task-definition \
      --task-definition "$TASK_FAMILY:$rev" \
      "${AWS_ARGS[@]}" \
      --query 'taskDefinition.status' \
      --output text 2>/dev/null || echo "NOT_FOUND")"
    IMAGE="$(aws ecs describe-task-definition \
      --task-definition "$TASK_FAMILY:$rev" \
      "${AWS_ARGS[@]}" \
      --query "taskDefinition.containerDefinitions[?name=='$CONTAINER_NAME'].image | [0]" \
      --output text 2>/dev/null || echo "unknown")"
    MARKER=""
    if [ "$rev" -eq "$CURRENT_REVISION" ]; then MARKER=" (current)"; fi
    echo "  revision $rev: $STATUS — $IMAGE$MARKER"
  done
  exit 1
fi

ROLLBACK_IMAGE="$(aws ecs describe-task-definition \
  --task-definition "$ROLLBACK_TASK_DEF" \
  "${AWS_ARGS[@]}" \
  --query "taskDefinition.containerDefinitions[?name=='$CONTAINER_NAME'].image | [0]" \
  --output text)"

ROLLBACK_SHA="$(aws ecs describe-task-definition \
  --task-definition "$ROLLBACK_TASK_DEF" \
  "${AWS_ARGS[@]}" \
  --query "taskDefinition.containerDefinitions[?name=='$CONTAINER_NAME'].environment[?name=='APP_GIT_SHA'].value | [0]" \
  --output text)"

echo ""
echo "Rollback target:"
echo "  Task definition: $ROLLBACK_TASK_DEF"
echo "  Image: $ROLLBACK_IMAGE"
echo "  Git SHA: $ROLLBACK_SHA"
echo ""

read -r -p "Proceed with rollback? [y/N] " CONFIRM
if [[ ! "$CONFIRM" =~ ^[Yy]$ ]]; then
  echo "Rollback cancelled."
  exit 0
fi

echo "Updating ECS service to $ROLLBACK_TASK_DEF..."
aws ecs update-service \
  --cluster "$ECS_CLUSTER" \
  --service "$ECS_SERVICE" \
  --task-definition "$ROLLBACK_TASK_DEF" \
  --force-new-deployment \
  "${AWS_ARGS[@]}" >/dev/null

echo "Waiting for ECS service stability..."
aws ecs wait services-stable --cluster "$ECS_CLUSTER" --services "$ECS_SERVICE" "${AWS_ARGS[@]}"

echo "Verifying rollback..."
LIVE_TASK_DEF_ARN="$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'services[0].taskDefinition' \
  --output text)"

LIVE_REVISION="$(echo "$LIVE_TASK_DEF_ARN" | grep -o '[0-9]*$')"

if [ "$LIVE_REVISION" != "$ROLLBACK_REVISION" ]; then
  echo "Rollback verification failed: service is on revision $LIVE_REVISION, expected $ROLLBACK_REVISION" >&2
  exit 1
fi

if [ -n "$PUBLIC_BASE_URL" ]; then
  echo "Checking live endpoint..."
  for _ in 1 2 3 4 5; do
    HEALTH_JSON="$(curl -fsS "${PUBLIC_BASE_URL%/}/health" 2>/dev/null || true)"
    if [ -n "$HEALTH_JSON" ]; then
      LIVE_SHA="$(echo "$HEALTH_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('build',d).get('git_sha',''))" 2>/dev/null || true)"
      if [ "$LIVE_SHA" = "$ROLLBACK_SHA" ]; then
        echo "Live endpoint confirmed: git_sha=$LIVE_SHA"
        break
      fi
    fi
    sleep 6
  done
fi

echo ""
echo "=== Rollback Complete ==="
echo "Rolled back from revision $CURRENT_REVISION to $ROLLBACK_REVISION"
echo "Git SHA: $ROLLBACK_SHA"
echo "Task Definition: $LIVE_TASK_DEF_ARN"

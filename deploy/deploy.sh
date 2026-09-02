#!/bin/bash
set -euo pipefail

# =============================================================================
# Ignite Upside-Down deploy (REQ-092)
# =============================================================================
# Modes:
#   bash deploy/deploy.sh                     build -> ECR -> register -> update
#   bash deploy/deploy.sh --config-only       env-only: reuse the RUNNING image
#                                             digest, skip build/push entirely
#   bash deploy/deploy.sh --dry-run           print the plan + the env diff,
#                                             register nothing, update nothing
#   (--config-only and --dry-run compose)
#
# The env of the ignite-server container is rendered from deploy/env.manifest.json
# by prepare_task_definition.py. A hand-run `aws ecs register-task-definition`
# is no longer the way to flip a flag: edit the manifest, commit, and run
# `deploy.sh --config-only`.
#
# DEFAULTS BELOW ARE CORRECT (CLAUDE.md §4) — do not override them per-region.

CONFIG_ONLY=0
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --config-only) CONFIG_ONLY=1 ;;
    --dry-run)     DRY_RUN=1 ;;
    -h|--help)     sed -n '3,20p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 1 ;;
  esac
done

APP_NAME="ignite-upside-down"
ECR_REPOSITORY="${ECR_REPOSITORY:-ignite-upside-down}"
AWS_REGION="${AWS_REGION:-us-west-2}"
AWS_PROFILE="${AWS_PROFILE:-jamesventure}"
ECS_CLUSTER="${ECS_CLUSTER:-apex-cluster}"
ECS_SERVICE="${ECS_SERVICE:-ignite-service}"
TASK_FAMILY="${TASK_FAMILY:-ignite-upside-down}"
CONTAINER_NAME="${CONTAINER_NAME:-ignite-server}"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-https://projectjarvis.io}"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
GIT_SHA="${GIT_SHA:-$(git rev-parse HEAD 2>/dev/null || date -u +manual-%Y%m%d%H%M%S)}"
# Minimum free space in the DOCKER VM (not the host) before a build. Four full
# server builds have died on "no space left on device" with 200 GB free on the
# Mac — the builder lives in the VM. See memory deploy-docker-disk-space.
MIN_BUILD_FREE_GB="${MIN_BUILD_FREE_GB:-20}"
# Post-stability send-liveness gate budget.
LIVENESS_WAIT_SECONDS="${LIVENESS_WAIT_SECONDS:-300}"

AWS_ARGS=(--region "$AWS_REGION")
if [ -n "$AWS_PROFILE" ]; then
  AWS_ARGS+=(--profile "$AWS_PROFILE")
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

ENV_MANIFEST="$SCRIPT_DIR/env.manifest.json"
if [ ! -f "$ENV_MANIFEST" ]; then
  echo "ERROR: $ENV_MANIFEST is missing — the task definition env is rendered from it." >&2
  exit 1
fi

# The env manifest IS the config. A config-only deploy that ships uncommitted
# manifest edits is exactly the "flag flip with no diff and no reviewer" this
# path exists to kill, so deploy/ must be clean in BOTH modes.
# deploy_log.jsonl is an append-only operations log written BY this script, so
# it must not make the next run look dirty.
DEPLOY_DIR_DIRTY="$(git status --porcelain -- "$SCRIPT_DIR" 2>/dev/null | grep -v 'deploy/deploy_log\.jsonl' || true)"
if [ -n "$DEPLOY_DIR_DIRTY" ]; then
  if [ "$DRY_RUN" = "1" ]; then
    echo "WOULD REFUSE: deploy/ has uncommitted changes (dry run continues; a real run stops here)."
    echo "$DEPLOY_DIR_DIRTY"
    echo ""
  else
    echo "ERROR: deploy/ has uncommitted changes — commit the env manifest / deploy scripts first." >&2
    echo "$DEPLOY_DIR_DIRTY" >&2
    exit 1
  fi
fi

# Refuse to build from a dirty tree (2026-06-10 AAR action item 2): the Docker
# build copies the working tree, so uncommitted changes ship silently under a
# git_sha stamp that doesn't contain them. That is exactly how schema-coupled
# code deployed ahead of its migrations on 2026-06-10 and took sending down.
# Deliberate dirty deploys: DEPLOY_ALLOW_DIRTY=1 bash deploy/deploy.sh — and
# they are now DISTINGUISHABLE: the image tag carries -dirty-<treehash> and
# APP_TREE_DIRTY=1 shows on /health.build.
TREE_DIRTY=0
TREE_HASH=""
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  if [ "${DEPLOY_ALLOW_DIRTY:-0}" != "1" ] && [ "$CONFIG_ONLY" != "1" ]; then
    echo "ERROR: working tree has uncommitted changes — the image would ship them under git_sha $GIT_SHA, which does not contain them." >&2
    git status --short | head -20 >&2
    echo "" >&2
    echo "Commit first (schema-coupled changes MUST ship committed), or override with DEPLOY_ALLOW_DIRTY=1." >&2
    exit 1
  fi
  TREE_DIRTY=1
  TREE_HASH="$( { git status --porcelain; git diff; git diff --cached; } 2>/dev/null | shasum -a 256 | cut -c1-12 )"
fi

if [ "$CONFIG_ONLY" = "1" ]; then
  IMAGE_TAG=""   # resolved from the running revision below
elif [ "$TREE_DIRTY" = "1" ]; then
  IMAGE_TAG="${IMAGE_TAG:-$GIT_SHA-dirty-$TREE_HASH}"
else
  IMAGE_TAG="${IMAGE_TAG:-$GIT_SHA}"
fi

# shellcheck source=deploy/_guardrails.sh
source "$SCRIPT_DIR/_guardrails.sh"

AWS_ACCOUNT_ID="$(aws sts get-caller-identity "${AWS_ARGS[@]}" --query Account --output text)"
ECR_REGISTRY="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com"
IMAGE_REPO="$ECR_REGISTRY/$ECR_REPOSITORY"

echo "=== Ignite Upside-Down Deployment ==="
echo "AWS Account: $AWS_ACCOUNT_ID"
echo "AWS Region: $AWS_REGION"
echo "AWS Profile: $AWS_PROFILE"
echo "Git SHA: $GIT_SHA"
echo "Build Time: $BUILD_TIME"
echo "Mode: $([ "$CONFIG_ONLY" = 1 ] && echo config-only || echo build)$([ "$DRY_RUN" = 1 ] && echo " (dry-run)")"
echo "Tree dirty: $TREE_DIRTY${TREE_HASH:+ ($TREE_HASH)}"
echo ""

# Fail closed unless this is the known-good production environment (us-west-2 /
# account 146361001621 / apex-cluster / ignite-service). Runs BEFORE the
# pre-deploy gate and any mutating AWS action.
assert_prod_environment "$AWS_ACCOUNT_ID" "$AWS_REGION" "$ECS_CLUSTER" "$ECS_SERVICE" "${AWS_ARGS[@]}"
echo ""

CURRENT_TASK_DEF_ARN="$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  "${AWS_ARGS[@]}" \
  --query 'services[0].taskDefinition' \
  --output text 2>/dev/null || true)"

if [ -z "$CURRENT_TASK_DEF_ARN" ] || [ "$CURRENT_TASK_DEF_ARN" = "None" ]; then
  CURRENT_TASK_DEF_ARN="$(aws ecs describe-task-definition \
    --task-definition "$TASK_FAMILY" \
    "${AWS_ARGS[@]}" \
    --query 'taskDefinition.taskDefinitionArn' \
    --output text 2>/dev/null || true)"
fi

if [ -z "$CURRENT_TASK_DEF_ARN" ] || [ "$CURRENT_TASK_DEF_ARN" = "None" ]; then
  echo "No existing task definition found for family $TASK_FAMILY. Bootstrap the service first." >&2
  exit 1
fi
echo "Current task definition: $CURRENT_TASK_DEF_ARN"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

aws ecs describe-task-definition \
  --task-definition "$CURRENT_TASK_DEF_ARN" \
  "${AWS_ARGS[@]}" > "$TMP_DIR/current-task-def.json"

if [ "$CONFIG_ONLY" = "1" ]; then
  # Reuse the RUNNING image byte-for-byte: no build, no push, no new bytes.
  # The git sha is likewise carried forward — a config-only revision must not
  # claim to contain code it does not.
  IMAGE_URI="$(python3 -c '
import json,sys
td=json.load(open(sys.argv[1]))["taskDefinition"]
print(next(c["image"] for c in td["containerDefinitions"] if c["name"]==sys.argv[2]))' \
    "$TMP_DIR/current-task-def.json" "$CONTAINER_NAME")"
  IMAGE_DIGEST="${IMAGE_URI##*@}"
  GIT_SHA="$(python3 -c '
import json,sys
td=json.load(open(sys.argv[1]))["taskDefinition"]
c=next(c for c in td["containerDefinitions"] if c["name"]==sys.argv[2])
print(next((e["value"] for e in c.get("environment",[]) if e["name"]=="APP_GIT_SHA"), ""))' \
    "$TMP_DIR/current-task-def.json" "$CONTAINER_NAME")"
  BUILD_TIME="$(python3 -c '
import json,sys
td=json.load(open(sys.argv[1]))["taskDefinition"]
c=next(c for c in td["containerDefinitions"] if c["name"]==sys.argv[2])
print(next((e["value"] for e in c.get("environment",[]) if e["name"]=="APP_BUILD_TIME"), ""))' \
    "$TMP_DIR/current-task-def.json" "$CONTAINER_NAME")"
  TREE_DIRTY="$(python3 -c '
import json,sys
td=json.load(open(sys.argv[1]))["taskDefinition"]
c=next(c for c in td["containerDefinitions"] if c["name"]==sys.argv[2])
print(next((e["value"] for e in c.get("environment",[]) if e["name"]=="APP_TREE_DIRTY"), "0"))' \
    "$TMP_DIR/current-task-def.json" "$CONTAINER_NAME")"
  echo "Config-only: reusing running image $IMAGE_URI"
  echo "Config-only: carrying APP_GIT_SHA=$GIT_SHA APP_TREE_DIRTY=$TREE_DIRTY"
  if [ -z "$IMAGE_DIGEST" ] || [ "$IMAGE_DIGEST" = "$IMAGE_URI" ]; then
    echo "Running image is tag-pinned, not digest-pinned ($IMAGE_URI) — refusing a config-only deploy." >&2
    exit 1
  fi
else
  if [ "${SKIP_PRE_DEPLOY:-}" != "1" ]; then
    "$SCRIPT_DIR/pre-deploy.sh"
    echo ""
  fi

  # Free-space assert INSIDE the Docker VM. `df -h /` on the Mac reports the
  # host and reads as an all-clear while the builder is at 100%; four deploys
  # have died on that. Prune hard and re-check rather than fail the build at
  # `go build` 12 minutes in.
  vm_free_gb() {
    docker run --rm alpine df -k / 2>/dev/null | awk 'NR==2 {printf "%d", $4/1024/1024}'
  }
  FREE_GB="$(vm_free_gb || echo 0)"
  echo "Docker VM free space: ${FREE_GB} GB (minimum ${MIN_BUILD_FREE_GB} GB)"
  if [ -z "$FREE_GB" ] || [ "$FREE_GB" -lt "$MIN_BUILD_FREE_GB" ]; then
    echo "Below minimum — pruning the full build cache (docker builder prune -af)..."
    docker builder prune -af 2>/dev/null || true
    docker image prune -f 2>/dev/null || true   # NEVER --volumes: apex-postgres/apex-redis
    FREE_GB="$(vm_free_gb || echo 0)"
    echo "Docker VM free space after prune: ${FREE_GB} GB"
    if [ -z "$FREE_GB" ] || [ "$FREE_GB" -lt "$MIN_BUILD_FREE_GB" ]; then
      echo "ERROR: only ${FREE_GB} GB free in the Docker VM; a server build needs ~13-15 GB of cache." >&2
      echo "Raise the Docker Desktop disk image, or clear the stuck overlay2 dir:" >&2
      echo "  docker run --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh" >&2
      exit 1
    fi
  else
    docker builder prune --filter "until=168h" -f 2>/dev/null || true
  fi
  echo ""

  IMAGE_TAG_URI="$IMAGE_REPO:$IMAGE_TAG"

  if [ "$DRY_RUN" = "1" ]; then
    echo "DRY RUN: would build and push $IMAGE_TAG_URI"
    IMAGE_URI="$IMAGE_TAG_URI"
    IMAGE_DIGEST="sha256:<unbuilt>"
  else
    echo "Ensuring ECR repository exists..."
    aws ecr describe-repositories --repository-names "$ECR_REPOSITORY" "${AWS_ARGS[@]}" >/dev/null 2>&1 || \
      aws ecr create-repository --repository-name "$ECR_REPOSITORY" "${AWS_ARGS[@]}" >/dev/null

    echo "Logging into ECR..."
    aws ecr get-login-password "${AWS_ARGS[@]}" | docker login --username AWS --password-stdin "$ECR_REGISTRY"

    echo "Building immutable image..."
    docker build \
      --platform linux/amd64 \
      --build-arg VERSION="$GIT_SHA" \
      --build-arg GIT_SHA="$GIT_SHA" \
      --build-arg BUILD_TIME="$BUILD_TIME" \
      --build-arg IMAGE_URI="$IMAGE_TAG_URI" \
      -t "$IMAGE_TAG_URI" .

    echo "Pushing image to ECR..."
    docker push "$IMAGE_TAG_URI"

    IMAGE_DIGEST="$(aws ecr describe-images \
      --repository-name "$ECR_REPOSITORY" \
      --image-ids imageTag="$IMAGE_TAG" \
      "${AWS_ARGS[@]}" \
      --query 'imageDetails[0].imageDigest' \
      --output text)"

    if [ -z "$IMAGE_DIGEST" ] || [ "$IMAGE_DIGEST" = "None" ]; then
      echo "Failed to resolve image digest for $IMAGE_TAG_URI" >&2
      exit 1
    fi

    IMAGE_URI="$IMAGE_REPO@$IMAGE_DIGEST"
    echo "Resolved immutable image: $IMAGE_URI"
  fi
fi

PREPARE_ARGS=(
  "$TMP_DIR/current-task-def.json"
  "$TMP_DIR/task-def.json"
  "$CONTAINER_NAME"
  "$IMAGE_URI"
  "$GIT_SHA"
  "$BUILD_TIME"
  "$IMAGE_DIGEST"
  --tree-dirty "$TREE_DIRTY"
)
if [ "$DRY_RUN" = "1" ]; then
  PREPARE_ARGS+=(--dry-run)
fi

echo ""
echo "Rendering task definition from deploy/env.manifest.json..."
python3 "$SCRIPT_DIR/prepare_task_definition.py" "${PREPARE_ARGS[@]}"

ENV_MANIFEST_SHA="$(shasum -a 256 "$ENV_MANIFEST" | cut -d' ' -f1)"

if [ "$DRY_RUN" = "1" ]; then
  echo ""
  echo "=== DRY RUN — plan only ==="
  echo "would register : family $TASK_FAMILY from $CURRENT_TASK_DEF_ARN"
  echo "image          : $IMAGE_URI"
  echo "git sha        : $GIT_SHA"
  echo "manifest sha   : $ENV_MANIFEST_SHA"
  echo "tree dirty     : $TREE_DIRTY"
  echo "would update   : service $ECS_SERVICE in $ECS_CLUSTER"
  echo "NOTHING WAS REGISTERED, PUSHED, OR UPDATED."
  exit 0
fi

NEW_TASK_DEF_ARN="$(aws ecs register-task-definition \
  --cli-input-json "file://$TMP_DIR/task-def.json" \
  "${AWS_ARGS[@]}" \
  --query 'taskDefinition.taskDefinitionArn' \
  --output text)"

echo "Updating ECS service to task definition $NEW_TASK_DEF_ARN"
aws ecs update-service \
  --cluster "$ECS_CLUSTER" \
  --service "$ECS_SERVICE" \
  --task-definition "$NEW_TASK_DEF_ARN" \
  --force-new-deployment \
  "${AWS_ARGS[@]}" >/dev/null

log_deploy() {
  # One line per deploy. ECS keeps only ~100 service events (~5 days), so this
  # file is the durable answer to "which revision moved that flag, and when".
  # Appended for every outcome (ok / verify-failed / unstable).
  DEPLOY_LOG_RESULT="$1" \
  DEPLOY_LOG_REVISION="${NEW_TASK_DEF_ARN##*/}" \
  DEPLOY_LOG_PREVIOUS="${CURRENT_TASK_DEF_ARN##*/}" \
  DEPLOY_LOG_GIT_SHA="$GIT_SHA" \
  DEPLOY_LOG_IMAGE="$IMAGE_URI" \
  DEPLOY_LOG_MANIFEST_SHA="$ENV_MANIFEST_SHA" \
  DEPLOY_LOG_CONFIG_ONLY="$CONFIG_ONLY" \
  DEPLOY_LOG_TREE_DIRTY="$TREE_DIRTY" \
  python3 "$SCRIPT_DIR/append_deploy_log.py" "$SCRIPT_DIR/deploy_log.jsonl"
}

LOG_GROUP="/ecs/ignite-upside-down"
echo "Waiting for ECS service stability (tailing $LOG_GROUP)..."
aws logs tail "$LOG_GROUP" --follow --since 1m "${AWS_ARGS[@]}" &
LOG_TAIL_PID=$!

MAX_WAIT=900  # 15 minutes
POLL=15
ELAPSED=0
STABLE=false
while [ "$ELAPSED" -lt "$MAX_WAIT" ]; do
  DEPLOYMENTS=$(aws ecs describe-services --cluster "$ECS_CLUSTER" --services "$ECS_SERVICE" "${AWS_ARGS[@]}" \
    --query 'services[0].deployments' --output json 2>/dev/null || echo "[]")
  COUNT=$(echo "$DEPLOYMENTS" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
  if [ "$COUNT" = "1" ]; then
    STATUS=$(echo "$DEPLOYMENTS" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d[0].get('rolloutState',''))" 2>/dev/null || echo "")
    if [ "$STATUS" = "COMPLETED" ]; then
      STABLE=true
      break
    fi
  fi
  echo "  [$((ELAPSED))s] $COUNT deployment(s) active — waiting..."
  sleep "$POLL"
  ELAPSED=$((ELAPSED + POLL))
done

kill "$LOG_TAIL_PID" 2>/dev/null || true
wait "$LOG_TAIL_PID" 2>/dev/null || true

if [ "$STABLE" != "true" ]; then
  echo "ECS service failed to stabilize after ${MAX_WAIT}s." >&2
  log_deploy "unstable" || true
  exit 1
fi

# Post-stability gate. `verify_ecs_deployment.sh` proves BYTES; these three
# checks prove the deploy actually took and the send path is alive:
#   1. /health.build.git_sha == the sha we shipped   (also .env_manifest_sha)
#   2. exactly ONE active ECS deployment             (no stuck PRIMARY+ACTIVE)
#   3. send liveness: sent_last_15m > 0 OR queue_ready_rows == 0
# Item 3 reads /health.send_liveness when the build publishes it (REQ-087) and
# otherwise falls back to a read-only SQL probe; see verify_ecs_deployment.sh.
if ! AWS_REGION="$AWS_REGION" \
  AWS_PROFILE="$AWS_PROFILE" \
  ECS_CLUSTER="$ECS_CLUSTER" \
  ECS_SERVICE="$ECS_SERVICE" \
  CONTAINER_NAME="$CONTAINER_NAME" \
  EXPECTED_IMAGE_DIGEST="$IMAGE_DIGEST" \
  EXPECTED_GIT_SHA="$GIT_SHA" \
  EXPECTED_ENV_MANIFEST_SHA="$ENV_MANIFEST_SHA" \
  LIVENESS_WAIT_SECONDS="$LIVENESS_WAIT_SECONDS" \
  PUBLIC_BASE_URL="$PUBLIC_BASE_URL" \
    "$SCRIPT_DIR/verify_ecs_deployment.sh"; then
  log_deploy "verify-failed" || true
  echo "" >&2
  echo "Post-deploy verification FAILED. Transport diagnosis:" >&2
  echo "  curl -s $PUBLIC_BASE_URL/health | python3 -m json.tool | sed -n '/event_bus/,/}/p'" >&2
  echo "  rollback: bash deploy/rollback.sh   (previous revision ${CURRENT_TASK_DEF_ARN##*/})" >&2
  exit 1
fi

log_deploy "ok"

echo ""
echo "=== Deployment Complete ==="
echo "Image: $IMAGE_URI"
echo "Task Definition: $NEW_TASK_DEF_ARN"
echo "Env manifest sha: $ENV_MANIFEST_SHA"
echo "Rollback target: ${CURRENT_TASK_DEF_ARN##*/}"

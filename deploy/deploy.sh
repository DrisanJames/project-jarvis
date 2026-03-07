#!/bin/bash
set -euo pipefail

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
IMAGE_TAG="${IMAGE_TAG:-$GIT_SHA}"

AWS_ARGS=(--region "$AWS_REGION")
if [ -n "$AWS_PROFILE" ]; then
  AWS_ARGS+=(--profile "$AWS_PROFILE")
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

AWS_ACCOUNT_ID="$(aws sts get-caller-identity "${AWS_ARGS[@]}" --query Account --output text)"
ECR_REGISTRY="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com"
IMAGE_REPO="$ECR_REGISTRY/$ECR_REPOSITORY"
IMAGE_TAG_URI="$IMAGE_REPO:$IMAGE_TAG"

echo "=== Ignite Upside-Down Deployment ==="
echo "AWS Account: $AWS_ACCOUNT_ID"
echo "AWS Region: $AWS_REGION"
echo "AWS Profile: $AWS_PROFILE"
echo "Git SHA: $GIT_SHA"
echo "Build Time: $BUILD_TIME"
echo ""

echo "Ensuring ECR repository exists..."
aws ecr describe-repositories --repository-names "$ECR_REPOSITORY" "${AWS_ARGS[@]}" >/dev/null 2>&1 || \
  aws ecr create-repository --repository-name "$ECR_REPOSITORY" "${AWS_ARGS[@]}" >/dev/null

echo "Logging into ECR..."
aws ecr get-login-password "${AWS_ARGS[@]}" | docker login --username AWS --password-stdin "$ECR_REGISTRY"

echo "Building immutable image..."
docker build \
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

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

aws ecs describe-task-definition \
  --task-definition "$CURRENT_TASK_DEF_ARN" \
  "${AWS_ARGS[@]}" > "$TMP_DIR/current-task-def.json"

python3 "$SCRIPT_DIR/prepare_task_definition.py" \
  "$TMP_DIR/current-task-def.json" \
  "$TMP_DIR/task-def.json" \
  "$CONTAINER_NAME" \
  "$IMAGE_URI" \
  "$GIT_SHA" \
  "$BUILD_TIME" \
  "$IMAGE_DIGEST"

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

echo "Waiting for ECS service stability..."
aws ecs wait services-stable --cluster "$ECS_CLUSTER" --services "$ECS_SERVICE" "${AWS_ARGS[@]}"

AWS_REGION="$AWS_REGION" \
AWS_PROFILE="$AWS_PROFILE" \
ECS_CLUSTER="$ECS_CLUSTER" \
ECS_SERVICE="$ECS_SERVICE" \
CONTAINER_NAME="$CONTAINER_NAME" \
EXPECTED_IMAGE_DIGEST="$IMAGE_DIGEST" \
EXPECTED_GIT_SHA="$GIT_SHA" \
PUBLIC_BASE_URL="$PUBLIC_BASE_URL" \
  "$SCRIPT_DIR/verify_ecs_deployment.sh"

echo ""
echo "=== Deployment Complete ==="
echo "Image: $IMAGE_URI"
echo "Task Definition: $NEW_TASK_DEF_ARN"

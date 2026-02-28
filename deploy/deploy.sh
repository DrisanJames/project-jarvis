#!/bin/bash
set -e

# Configuration
APP_NAME="ignite-upside-down"
AWS_REGION="${AWS_REGION:-us-east-1}"
AWS_PROFILE="${AWS_PROFILE:-jamesventure}"
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --profile $AWS_PROFILE --query Account --output text)
ECR_REPO="$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/$APP_NAME"
ECS_CLUSTER="$APP_NAME-cluster"
ECS_SERVICE="$APP_NAME-service"
ECS_TASK="$APP_NAME-task"

echo "=== Ignite Upside-Down Deployment ==="
echo "AWS Account: $AWS_ACCOUNT_ID"
echo "AWS Region: $AWS_REGION"
echo "AWS Profile: $AWS_PROFILE"
echo ""

# Create ECR repository if it doesn't exist
echo "Creating ECR repository..."
aws ecr describe-repositories --repository-names $APP_NAME --profile $AWS_PROFILE --region $AWS_REGION 2>/dev/null || \
    aws ecr create-repository --repository-name $APP_NAME --profile $AWS_PROFILE --region $AWS_REGION

# Login to ECR
echo "Logging into ECR..."
aws ecr get-login-password --profile $AWS_PROFILE --region $AWS_REGION | \
    docker login --username AWS --password-stdin $AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com

# Build Docker image
echo "Building Docker image..."
cd "$(dirname "$0")/.."
docker build -t $APP_NAME:latest .

# Tag and push to ECR
echo "Pushing to ECR..."
docker tag $APP_NAME:latest $ECR_REPO:latest
docker tag $APP_NAME:latest $ECR_REPO:$(date +%Y%m%d-%H%M%S)
docker push $ECR_REPO:latest

echo ""
echo "=== Image pushed to ECR ==="
echo "Image: $ECR_REPO:latest"
echo ""

# Check if ECS cluster exists
CLUSTER_EXISTS=$(aws ecs describe-clusters --clusters $ECS_CLUSTER --profile $AWS_PROFILE --region $AWS_REGION --query "clusters[?status=='ACTIVE'].clusterName" --output text 2>/dev/null || echo "")

if [ -z "$CLUSTER_EXISTS" ]; then
    echo "Creating ECS cluster..."
    aws ecs create-cluster --cluster-name $ECS_CLUSTER --profile $AWS_PROFILE --region $AWS_REGION
fi

# Check if service exists and update or create
SERVICE_EXISTS=$(aws ecs describe-services --cluster $ECS_CLUSTER --services $ECS_SERVICE --profile $AWS_PROFILE --region $AWS_REGION --query "services[?status=='ACTIVE'].serviceName" --output text 2>/dev/null || echo "")

if [ -n "$SERVICE_EXISTS" ]; then
    echo "Updating ECS service..."
    aws ecs update-service \
        --cluster $ECS_CLUSTER \
        --service $ECS_SERVICE \
        --force-new-deployment \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
    echo "Service update initiated. Monitor at:"
    echo "https://$AWS_REGION.console.aws.amazon.com/ecs/v2/clusters/$ECS_CLUSTER/services/$ECS_SERVICE"
else
    echo ""
    echo "=== ECS Service Not Found ==="
    echo "Run the setup script first to create the ECS infrastructure:"
    echo "  ./deploy/setup-ecs.sh"
fi

echo ""
echo "=== Deployment Complete ==="

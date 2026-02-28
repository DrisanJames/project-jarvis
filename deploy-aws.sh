#!/bin/bash
# IGNITE Platform - AWS Deployment Script
# Uses default AWS profile

set -e

echo "=============================================="
echo "  IGNITE Platform - AWS Deployment           "
echo "=============================================="

# Check AWS credentials
echo ""
echo "Checking AWS credentials (default profile)..."
aws sts get-caller-identity --profile default || {
    echo "Error: AWS credentials not configured"
    echo "Run: aws configure"
    exit 1
}

REGION=${AWS_REGION:-us-east-1}
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
ECR_REPO="ignite-platform"
IMAGE_TAG="latest"

echo ""
echo "AWS Account: $ACCOUNT_ID"
echo "Region: $REGION"

# Create ECR repository if it doesn't exist
echo ""
echo "Creating ECR repository..."
aws ecr describe-repositories --repository-names $ECR_REPO --region $REGION 2>/dev/null || \
    aws ecr create-repository --repository-name $ECR_REPO --region $REGION

# Login to ECR
echo ""
echo "Logging into ECR..."
aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com

# Build Docker image
echo ""
echo "Building Docker image..."
docker build -t $ECR_REPO:$IMAGE_TAG .

# Tag and push to ECR
echo ""
echo "Pushing to ECR..."
docker tag $ECR_REPO:$IMAGE_TAG $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$ECR_REPO:$IMAGE_TAG
docker push $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$ECR_REPO:$IMAGE_TAG

echo ""
echo "=============================================="
echo "  Deployment Complete!                       "
echo "=============================================="
echo ""
echo "Image: $ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com/$ECR_REPO:$IMAGE_TAG"
echo ""
echo "To run on ECS:"
echo "  aws ecs create-service --cluster your-cluster --service-name ignite-platform ..."
echo ""
echo "Or run locally with AWS profile:"
echo "  docker-compose -f docker-compose.aws.yml up"
echo ""

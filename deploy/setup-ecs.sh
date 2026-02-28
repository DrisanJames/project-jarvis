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
ALB_NAME="$APP_NAME-alb"
TG_NAME="$APP_NAME-tg"
SG_NAME="$APP_NAME-sg"

echo "=== Ignite Upside-Down ECS Setup ==="
echo "AWS Account: $AWS_ACCOUNT_ID"
echo "AWS Region: $AWS_REGION"
echo ""

# Get default VPC
echo "Getting default VPC..."
VPC_ID=$(aws ec2 describe-vpcs --profile $AWS_PROFILE --region $AWS_REGION \
    --filters "Name=isDefault,Values=true" \
    --query "Vpcs[0].VpcId" --output text)

if [ "$VPC_ID" == "None" ] || [ -z "$VPC_ID" ]; then
    echo "No default VPC found. Please specify VPC_ID environment variable."
    exit 1
fi
echo "Using VPC: $VPC_ID"

# Get subnets in the VPC
echo "Getting subnets..."
SUBNET_IDS=$(aws ec2 describe-subnets --profile $AWS_PROFILE --region $AWS_REGION \
    --filters "Name=vpc-id,Values=$VPC_ID" \
    --query "Subnets[*].SubnetId" --output text | tr '\t' ',')
echo "Using subnets: $SUBNET_IDS"

# Create security group if it doesn't exist
echo "Setting up security group..."
SG_ID=$(aws ec2 describe-security-groups --profile $AWS_PROFILE --region $AWS_REGION \
    --filters "Name=group-name,Values=$SG_NAME" "Name=vpc-id,Values=$VPC_ID" \
    --query "SecurityGroups[0].GroupId" --output text 2>/dev/null || echo "None")

if [ "$SG_ID" == "None" ] || [ -z "$SG_ID" ]; then
    echo "Creating security group..."
    SG_ID=$(aws ec2 create-security-group \
        --group-name $SG_NAME \
        --description "Security group for $APP_NAME" \
        --vpc-id $VPC_ID \
        --profile $AWS_PROFILE \
        --region $AWS_REGION \
        --query "GroupId" --output text)
    
    # Allow HTTP traffic
    aws ec2 authorize-security-group-ingress \
        --group-id $SG_ID \
        --protocol tcp \
        --port 80 \
        --cidr 0.0.0.0/0 \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
    
    # Allow HTTPS traffic
    aws ec2 authorize-security-group-ingress \
        --group-id $SG_ID \
        --protocol tcp \
        --port 443 \
        --cidr 0.0.0.0/0 \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
    
    # Allow app port (8080)
    aws ec2 authorize-security-group-ingress \
        --group-id $SG_ID \
        --protocol tcp \
        --port 8080 \
        --cidr 0.0.0.0/0 \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
fi
echo "Security Group: $SG_ID"

# Create ECS cluster
echo "Setting up ECS cluster..."
aws ecs describe-clusters --clusters $ECS_CLUSTER --profile $AWS_PROFILE --region $AWS_REGION \
    --query "clusters[?status=='ACTIVE'].clusterName" --output text 2>/dev/null || \
    aws ecs create-cluster --cluster-name $ECS_CLUSTER --profile $AWS_PROFILE --region $AWS_REGION

# Create IAM role for ECS task execution if it doesn't exist
EXEC_ROLE_NAME="$APP_NAME-exec-role"
TASK_ROLE_NAME="$APP_NAME-task-role"

echo "Setting up IAM roles..."

# Check if execution role exists
EXEC_ROLE_ARN=$(aws iam get-role --role-name $EXEC_ROLE_NAME --profile $AWS_PROFILE \
    --query "Role.Arn" --output text 2>/dev/null || echo "")

if [ -z "$EXEC_ROLE_ARN" ]; then
    echo "Creating ECS execution role..."
    aws iam create-role \
        --role-name $EXEC_ROLE_NAME \
        --assume-role-policy-document '{
            "Version": "2012-10-17",
            "Statement": [{
                "Effect": "Allow",
                "Principal": {"Service": "ecs-tasks.amazonaws.com"},
                "Action": "sts:AssumeRole"
            }]
        }' \
        --profile $AWS_PROFILE
    
    aws iam attach-role-policy \
        --role-name $EXEC_ROLE_NAME \
        --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy \
        --profile $AWS_PROFILE
    
    EXEC_ROLE_ARN="arn:aws:iam::$AWS_ACCOUNT_ID:role/$EXEC_ROLE_NAME"
    sleep 10  # Wait for role to propagate
fi
echo "Execution Role: $EXEC_ROLE_ARN"

# Check if task role exists
TASK_ROLE_ARN=$(aws iam get-role --role-name $TASK_ROLE_NAME --profile $AWS_PROFILE \
    --query "Role.Arn" --output text 2>/dev/null || echo "")

if [ -z "$TASK_ROLE_ARN" ]; then
    echo "Creating ECS task role..."
    aws iam create-role \
        --role-name $TASK_ROLE_NAME \
        --assume-role-policy-document '{
            "Version": "2012-10-17",
            "Statement": [{
                "Effect": "Allow",
                "Principal": {"Service": "ecs-tasks.amazonaws.com"},
                "Action": "sts:AssumeRole"
            }]
        }' \
        --profile $AWS_PROFILE
    
    # Attach policies for DynamoDB, S3, and other AWS services the app needs
    aws iam attach-role-policy \
        --role-name $TASK_ROLE_NAME \
        --policy-arn arn:aws:iam::aws:policy/AmazonDynamoDBFullAccess \
        --profile $AWS_PROFILE
    
    aws iam attach-role-policy \
        --role-name $TASK_ROLE_NAME \
        --policy-arn arn:aws:iam::aws:policy/AmazonS3FullAccess \
        --profile $AWS_PROFILE
    
    aws iam attach-role-policy \
        --role-name $TASK_ROLE_NAME \
        --policy-arn arn:aws:iam::aws:policy/AmazonSESFullAccess \
        --profile $AWS_PROFILE
    
    TASK_ROLE_ARN="arn:aws:iam::$AWS_ACCOUNT_ID:role/$TASK_ROLE_NAME"
    sleep 10  # Wait for role to propagate
fi
echo "Task Role: $TASK_ROLE_ARN"

# Create CloudWatch log group
LOG_GROUP="/ecs/$APP_NAME"
aws logs create-log-group --log-group-name $LOG_GROUP --profile $AWS_PROFILE --region $AWS_REGION 2>/dev/null || true

# Register task definition
echo "Registering ECS task definition..."
TASK_DEF=$(cat <<EOF
{
    "family": "$ECS_TASK",
    "networkMode": "awsvpc",
    "requiresCompatibilities": ["FARGATE"],
    "cpu": "512",
    "memory": "1024",
    "executionRoleArn": "$EXEC_ROLE_ARN",
    "taskRoleArn": "$TASK_ROLE_ARN",
    "containerDefinitions": [{
        "name": "$APP_NAME",
        "image": "$ECR_REPO:latest",
        "essential": true,
        "portMappings": [{
            "containerPort": 8080,
            "hostPort": 8080,
            "protocol": "tcp"
        }],
        "environment": [
            {"name": "PORT", "value": "8080"},
            {"name": "AWS_REGION", "value": "$AWS_REGION"}
        ],
        "logConfiguration": {
            "logDriver": "awslogs",
            "options": {
                "awslogs-group": "$LOG_GROUP",
                "awslogs-region": "$AWS_REGION",
                "awslogs-stream-prefix": "ecs"
            }
        },
        "healthCheck": {
            "command": ["CMD-SHELL", "wget -q --spider http://localhost:8080/health || exit 1"],
            "interval": 30,
            "timeout": 5,
            "retries": 3,
            "startPeriod": 60
        }
    }]
}
EOF
)

echo "$TASK_DEF" > /tmp/task-def.json
aws ecs register-task-definition \
    --cli-input-json file:///tmp/task-def.json \
    --profile $AWS_PROFILE \
    --region $AWS_REGION

# Create Application Load Balancer
echo "Setting up Application Load Balancer..."
ALB_ARN=$(aws elbv2 describe-load-balancers --names $ALB_NAME --profile $AWS_PROFILE --region $AWS_REGION \
    --query "LoadBalancers[0].LoadBalancerArn" --output text 2>/dev/null || echo "None")

if [ "$ALB_ARN" == "None" ] || [ -z "$ALB_ARN" ]; then
    echo "Creating ALB..."
    # Convert comma-separated subnets to space-separated for CLI
    SUBNET_LIST=$(echo $SUBNET_IDS | tr ',' ' ')
    
    ALB_ARN=$(aws elbv2 create-load-balancer \
        --name $ALB_NAME \
        --subnets $SUBNET_LIST \
        --security-groups $SG_ID \
        --scheme internet-facing \
        --type application \
        --profile $AWS_PROFILE \
        --region $AWS_REGION \
        --query "LoadBalancers[0].LoadBalancerArn" --output text)
fi
echo "ALB ARN: $ALB_ARN"

# Get ALB DNS name
ALB_DNS=$(aws elbv2 describe-load-balancers --load-balancer-arns $ALB_ARN \
    --profile $AWS_PROFILE --region $AWS_REGION \
    --query "LoadBalancers[0].DNSName" --output text)
echo "ALB DNS: $ALB_DNS"

# Create target group
echo "Setting up target group..."
TG_ARN=$(aws elbv2 describe-target-groups --names $TG_NAME --profile $AWS_PROFILE --region $AWS_REGION \
    --query "TargetGroups[0].TargetGroupArn" --output text 2>/dev/null || echo "None")

if [ "$TG_ARN" == "None" ] || [ -z "$TG_ARN" ]; then
    echo "Creating target group..."
    TG_ARN=$(aws elbv2 create-target-group \
        --name $TG_NAME \
        --protocol HTTP \
        --port 8080 \
        --vpc-id $VPC_ID \
        --target-type ip \
        --health-check-path /health \
        --health-check-interval-seconds 30 \
        --healthy-threshold-count 2 \
        --unhealthy-threshold-count 3 \
        --profile $AWS_PROFILE \
        --region $AWS_REGION \
        --query "TargetGroups[0].TargetGroupArn" --output text)
fi
echo "Target Group: $TG_ARN"

# Create listener
echo "Setting up ALB listener..."
LISTENER_ARN=$(aws elbv2 describe-listeners --load-balancer-arn $ALB_ARN \
    --profile $AWS_PROFILE --region $AWS_REGION \
    --query "Listeners[?Port==\`80\`].ListenerArn" --output text 2>/dev/null || echo "")

if [ -z "$LISTENER_ARN" ]; then
    echo "Creating HTTP listener..."
    aws elbv2 create-listener \
        --load-balancer-arn $ALB_ARN \
        --protocol HTTP \
        --port 80 \
        --default-actions Type=forward,TargetGroupArn=$TG_ARN \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
fi

# Create ECS service
echo "Setting up ECS service..."
SERVICE_EXISTS=$(aws ecs describe-services --cluster $ECS_CLUSTER --services $ECS_SERVICE \
    --profile $AWS_PROFILE --region $AWS_REGION \
    --query "services[?status=='ACTIVE'].serviceName" --output text 2>/dev/null || echo "")

# Convert comma-separated subnets back to JSON array format
SUBNET_JSON=$(echo $SUBNET_IDS | sed 's/,/","/g' | sed 's/^/"/' | sed 's/$/"/')

if [ -z "$SERVICE_EXISTS" ]; then
    echo "Creating ECS service..."
    aws ecs create-service \
        --cluster $ECS_CLUSTER \
        --service-name $ECS_SERVICE \
        --task-definition $ECS_TASK \
        --desired-count 1 \
        --launch-type FARGATE \
        --network-configuration "awsvpcConfiguration={subnets=[$SUBNET_JSON],securityGroups=[\"$SG_ID\"],assignPublicIp=ENABLED}" \
        --load-balancers "targetGroupArn=$TG_ARN,containerName=$APP_NAME,containerPort=8080" \
        --profile $AWS_PROFILE \
        --region $AWS_REGION
else
    echo "ECS service already exists. Use deploy.sh to update."
fi

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Application URL: http://$ALB_DNS"
echo ""
echo "Next steps:"
echo "1. Run ./deploy/deploy.sh to build and deploy the application"
echo "2. Wait a few minutes for the service to start"
echo "3. Access the application at: http://$ALB_DNS"
echo ""
echo "To view logs:"
echo "  aws logs tail $LOG_GROUP --follow --profile $AWS_PROFILE --region $AWS_REGION"

#!/usr/bin/env bash
# deploy.sh — Package and deploy the audience-metrics Lambda + EventBridge schedule.
#
# Prerequisites:
#   - AWS CLI v2 configured with appropriate credentials
#   - The VPC security group must allow outbound TCP 5432 to the RDS security group
#
# Usage:
#   ./deploy.sh                    # first-time create
#   ./deploy.sh update             # update code only
#   ./deploy.sh set-db-url <url>   # update DATABASE_URL env var

set -euo pipefail

FUNCTION_NAME="ignite-audience-metrics"
REGION="us-east-1"
RUNTIME="python3.12"
HANDLER="handler.handler"
TIMEOUT=120
MEMORY=256
SCHEDULE_RULE="ignite-audience-metrics-schedule"
SCHEDULE_EXPR="rate(15 minutes)"

SUBNETS="subnet-00133d0a2d57a482e,subnet-02f5e9eaba18449c6"

# ---------------------------------------------------------------------------
# You MUST set these before first deploy:
#   SECURITY_GROUP_ID — SG that allows outbound 5432 to the RDS SG
#   ROLE_ARN          — IAM role with AWSLambdaVPCAccessExecutionRole
#   DATABASE_URL      — postgres://user:pass@host:5432/ignite?sslmode=require
# ---------------------------------------------------------------------------
: "${SECURITY_GROUP_ID:?Set SECURITY_GROUP_ID env var}"
: "${ROLE_ARN:?Set ROLE_ARN env var (Lambda execution role)}"
: "${DATABASE_URL:?Set DATABASE_URL env var (production RDS connection string)}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="$SCRIPT_DIR/.build"

package() {
    echo "==> Packaging Lambda..."
    rm -rf "$BUILD_DIR"
    mkdir -p "$BUILD_DIR/package"

    pip install -r "$SCRIPT_DIR/requirements.txt" \
        --target "$BUILD_DIR/package" \
        --platform manylinux2014_x86_64 \
        --only-binary=:all: \
        --quiet 2>/dev/null || \
    pip install -r "$SCRIPT_DIR/requirements.txt" \
        --target "$BUILD_DIR/package" \
        --quiet

    cp "$SCRIPT_DIR/handler.py" "$BUILD_DIR/package/"

    cd "$BUILD_DIR/package"
    zip -r9 "$BUILD_DIR/function.zip" . -q
    cd "$SCRIPT_DIR"
    echo "    Package: $BUILD_DIR/function.zip ($(du -h "$BUILD_DIR/function.zip" | cut -f1))"
}

create_function() {
    echo "==> Creating Lambda function: $FUNCTION_NAME"
    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime "$RUNTIME" \
        --handler "$HANDLER" \
        --role "$ROLE_ARN" \
        --timeout "$TIMEOUT" \
        --memory-size "$MEMORY" \
        --zip-file "fileb://$BUILD_DIR/function.zip" \
        --vpc-config "SubnetIds=$SUBNETS,SecurityGroupIds=$SECURITY_GROUP_ID" \
        --environment "Variables={DATABASE_URL=$DATABASE_URL}" \
        --region "$REGION" \
        --no-cli-pager
    echo "    Lambda created."
}

update_code() {
    echo "==> Updating function code..."
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file "fileb://$BUILD_DIR/function.zip" \
        --region "$REGION" \
        --no-cli-pager
    echo "    Code updated."
}

set_db_url() {
    local url="$1"
    echo "==> Updating DATABASE_URL..."
    aws lambda update-function-configuration \
        --function-name "$FUNCTION_NAME" \
        --environment "Variables={DATABASE_URL=$url}" \
        --region "$REGION" \
        --no-cli-pager
    echo "    DATABASE_URL updated."
}

create_schedule() {
    echo "==> Creating EventBridge schedule: $SCHEDULE_RULE"
    aws events put-rule \
        --name "$SCHEDULE_RULE" \
        --schedule-expression "$SCHEDULE_EXPR" \
        --state ENABLED \
        --region "$REGION" \
        --no-cli-pager

    LAMBDA_ARN=$(aws lambda get-function \
        --function-name "$FUNCTION_NAME" \
        --region "$REGION" \
        --query 'Configuration.FunctionArn' \
        --output text)

    aws events put-targets \
        --rule "$SCHEDULE_RULE" \
        --targets "Id=${FUNCTION_NAME}-target,Arn=$LAMBDA_ARN" \
        --region "$REGION" \
        --no-cli-pager

    aws lambda add-permission \
        --function-name "$FUNCTION_NAME" \
        --statement-id "eventbridge-${SCHEDULE_RULE}" \
        --action "lambda:InvokeFunction" \
        --principal events.amazonaws.com \
        --source-arn "$(aws events describe-rule --name "$SCHEDULE_RULE" --region "$REGION" --query 'Arn' --output text)" \
        --region "$REGION" \
        --no-cli-pager 2>/dev/null || true

    echo "    Schedule active: $SCHEDULE_EXPR"
}

case "${1:-create}" in
    create)
        package
        create_function
        create_schedule
        echo "==> Done. Invoke manually to verify:"
        echo "    aws lambda invoke --function-name $FUNCTION_NAME --region $REGION /dev/stdout"
        ;;
    update)
        package
        update_code
        ;;
    set-db-url)
        set_db_url "${2:?Usage: $0 set-db-url <url>}"
        ;;
    *)
        echo "Usage: $0 [create|update|set-db-url <url>]"
        exit 1
        ;;
esac

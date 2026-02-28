#!/bin/bash
# start-server.sh - Safe server startup with port check and verification
# Usage: ./scripts/start-server.sh [dev|prod]

set -e

MODE=${1:-dev}
PORT=8080
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

echo "🚀 Starting ESP Platform Server (mode: $MODE)"
echo "================================================"

# Step 1: Check for existing processes
echo ""
echo "Step 1: Checking port $PORT..."
EXISTING_PID=$(lsof -ti :$PORT 2>/dev/null || true)

if [ -n "$EXISTING_PID" ]; then
    echo "⚠️  WARNING: Process already running on port $PORT"
    echo "   PID: $EXISTING_PID"
    
    # Show which binary
    BINARY=$(ps -p $EXISTING_PID -o command= 2>/dev/null | head -1)
    echo "   Binary: $BINARY"
    
    # Check if it's the stub API
    if echo "$BINARY" | grep -q "cmd/stub-api\|cmd/api"; then
        echo ""
        echo "❌ DANGER: This appears to be the STUB API (cmd/stub-api/main.go)"
        echo "   The stub API returns hardcoded empty responses!"
        echo ""
    fi
    
    read -p "Kill existing process and continue? (y/N) " -n 1 -r
    echo ""
    
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        kill -9 $EXISTING_PID 2>/dev/null
        echo "✅ Killed PID $EXISTING_PID"
        sleep 1
    else
        echo "Aborted."
        exit 1
    fi
fi

echo "✅ Port $PORT is available"

# Step 2: Verify build
echo ""
echo "Step 2: Verifying build..."
BUILD_OUTPUT=$(go build ./... 2>&1)
if [ $? -eq 0 ]; then
    echo "✅ Build successful"
else
    echo "❌ Build failed - fix errors before starting server"
    echo "$BUILD_OUTPUT" | head -10
    exit 1
fi

# Step 3: Start the REAL server (not the stub!)
echo ""
echo "Step 3: Starting server..."

if [ "$MODE" = "dev" ]; then
    echo "   Mode: Development (DEV_MODE=true)"
    export DEV_MODE=true
    export DEFAULT_ORG_ID="${DEFAULT_ORG_ID:-00000000-0000-0000-0000-000000000001}"
fi

# IMPORTANT: Use cmd/server/main.go - NOT cmd/api/main.go
echo "   Binary: cmd/server/main.go"
go run cmd/server/main.go &
SERVER_PID=$!

echo "   PID: $SERVER_PID"

# Step 4: Wait and verify
echo ""
echo "Step 4: Waiting for server startup..."
sleep 5

# Check if server is still running
if ! kill -0 $SERVER_PID 2>/dev/null; then
    echo "❌ Server exited unexpectedly"
    exit 1
fi

# Verify it's responding
echo ""
echo "Step 5: Verifying API response..."
RESPONSE=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || echo "FAILED")

if echo "$RESPONSE" | grep -q "healthy"; then
    echo "✅ Health check passed"
else
    echo "⚠️  Health check failed: $RESPONSE"
fi

# Verify server identity (distinguish real server from stub)
IDENTITY=$(curl -sI "http://localhost:$PORT/health" 2>/dev/null | grep -i "X-Server-Identity" || echo "")
if echo "$IDENTITY" | grep -q "ignite-server"; then
    echo "✅ Server identity confirmed: real production server"
elif echo "$IDENTITY" | grep -q "stub"; then
    echo "❌ WARNING: STUB server detected! Kill it and restart with cmd/server/main.go"
else
    echo "⚠️  Could not verify server identity (header missing)"
fi

# Test campaigns endpoint
CAMPAIGNS=$(curl -s "http://localhost:$PORT/api/mailing/campaigns" 2>/dev/null)
COUNT=$(echo "$CAMPAIGNS" | grep -o '"count":[0-9]*' | cut -d: -f2 || echo "0")

if [ "$COUNT" -gt 0 ]; then
    echo "✅ Campaigns API working ($COUNT campaigns found)"
else
    echo "⚠️  Campaigns API returned 0 results - verify database connection"
fi

echo ""
echo "================================================"
echo "✅ Server started successfully!"
echo "   PID: $SERVER_PID"
echo "   URL: http://localhost:$PORT"
echo ""
echo "To stop: kill $SERVER_PID"

#!/bin/bash
# kill-port.sh - Utility to kill processes on a specific port
# Usage: ./scripts/kill-port.sh [port]
# Default port: 8080

PORT=${1:-8080}

echo "🔍 Checking for processes on port $PORT..."

PID=$(lsof -ti :$PORT 2>/dev/null)

if [ -z "$PID" ]; then
    echo "✅ No process found on port $PORT"
    exit 0
fi

echo "⚠️  Found process(es) on port $PORT: $PID"

# Show process details
for p in $PID; do
    DETAILS=$(ps -p $p -o pid,ppid,user,command 2>/dev/null | tail -1)
    echo "   PID $p: $DETAILS"
done

echo ""
read -p "Kill these processes? (y/N) " -n 1 -r
echo ""

if [[ $REPLY =~ ^[Yy]$ ]]; then
    for p in $PID; do
        kill -9 $p 2>/dev/null && echo "✅ Killed PID $p" || echo "❌ Failed to kill PID $p"
    done
else
    echo "Aborted."
    exit 1
fi

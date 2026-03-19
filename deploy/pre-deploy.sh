#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

echo "=== Pre-Deploy Quality Gate ==="
echo ""

# --- Go backend checks ---
echo "[1/3] Building Go backend..."
go build -o /dev/null ./cmd/server/
echo "  ✓ Backend compiles"

echo "[2/3] Running Go tests..."
FAILED_PKGS=""
set +e
go test ./internal/api/... ./internal/engine/... -count=1 -timeout 60s 2>&1
GO_EXIT=$?
set -e
if [ "$GO_EXIT" -ne 0 ]; then
  echo "  ✗ Go tests failed (exit $GO_EXIT)"
  exit 1
fi
echo "  ✓ Go tests pass"

# --- Frontend checks ---
echo "[3/3] Running TypeScript type-check..."
if [ -f "web/package.json" ]; then
  cd web
  if [ ! -d "node_modules" ]; then
    echo "  Installing frontend dependencies..."
    npm ci --silent
  fi
  npx tsc --noEmit 2>&1
  TSC_EXIT=$?
  cd "$REPO_ROOT"
  if [ "$TSC_EXIT" -ne 0 ]; then
    echo "  ✗ TypeScript type-check failed"
    exit 1
  fi
  echo "  ✓ TypeScript types OK"
else
  echo "  (skipped — no web/package.json found)"
fi

echo ""
echo "=== All pre-deploy checks passed ==="

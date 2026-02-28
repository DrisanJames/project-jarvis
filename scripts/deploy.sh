#!/bin/bash
# =============================================================================
# ESP Platform Deployment Script
# =============================================================================
# This script handles the complete deployment process including:
# - Database migrations
# - Building production binaries
# - Docker image creation
# - Service startup
# =============================================================================

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
MIGRATIONS_DIR="$PROJECT_ROOT/migrations"

# Load environment variables
if [ -f "$PROJECT_ROOT/.env" ]; then
    export $(cat "$PROJECT_ROOT/.env" | grep -v '^#' | xargs)
fi

# Functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check Go
    if ! command -v go &> /dev/null; then
        log_error "Go is not installed"
        exit 1
    fi
    log_success "Go $(go version | awk '{print $3}')"
    
    # Check PostgreSQL client
    if ! command -v psql &> /dev/null; then
        log_warning "psql not found - migrations will use Go migrate"
    fi
    
    # Check Docker (optional)
    if command -v docker &> /dev/null; then
        log_success "Docker $(docker --version | awk '{print $3}')"
    else
        log_warning "Docker not found - skipping containerization"
    fi
    
    # Check Node.js for frontend
    if ! command -v node &> /dev/null; then
        log_error "Node.js is not installed"
        exit 1
    fi
    log_success "Node.js $(node --version)"
}

# Run database migrations
run_migrations() {
    log_info "Running database migrations..."
    
    if [ -z "$DATABASE_URL" ]; then
        log_error "DATABASE_URL not set"
        exit 1
    fi
    
    # Run each migration in order
    for migration in $(ls -1 "$MIGRATIONS_DIR"/*.sql | sort); do
        migration_name=$(basename "$migration")
        log_info "Applying migration: $migration_name"
        
        if psql "$DATABASE_URL" -f "$migration" 2>&1 | grep -q "ERROR"; then
            # Check if it's just a "already exists" error
            if psql "$DATABASE_URL" -f "$migration" 2>&1 | grep -q "already exists"; then
                log_warning "Migration $migration_name already applied (skipping)"
            else
                log_error "Failed to apply migration: $migration_name"
                exit 1
            fi
        else
            log_success "Applied: $migration_name"
        fi
    done
    
    log_success "All migrations completed"
}

# Build Go binaries
build_binaries() {
    log_info "Building production binaries..."
    
    cd "$PROJECT_ROOT"
    
    # Build API server
    log_info "Building API server..."
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/api-server ./cmd/server
    log_success "Built: bin/api-server"
    
    # Build worker
    log_info "Building worker..."
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/worker ./cmd/worker
    log_success "Built: bin/worker"
    
    log_success "All binaries built successfully"
}

# Build frontend
build_frontend() {
    log_info "Building frontend..."
    
    cd "$PROJECT_ROOT/web"
    
    # Install dependencies
    log_info "Installing npm dependencies..."
    npm ci --silent
    
    # Build production bundle
    log_info "Building production bundle..."
    npm run build
    
    log_success "Frontend built successfully"
}

# Create Docker images
build_docker() {
    if ! command -v docker &> /dev/null; then
        log_warning "Docker not available, skipping image build"
        return
    fi
    
    log_info "Building Docker images..."
    
    cd "$PROJECT_ROOT"
    
    # Build API server image
    log_info "Building API server image..."
    docker build -f Dockerfile.api -t esp-platform/api:latest .
    log_success "Built: esp-platform/api:latest"
    
    # Build worker image
    log_info "Building worker image..."
    docker build -f Dockerfile.worker -t esp-platform/worker:latest .
    log_success "Built: esp-platform/worker:latest"
    
    log_success "All Docker images built"
}

# Start services
start_services() {
    log_info "Starting services..."
    
    if command -v docker-compose &> /dev/null && [ -f "$PROJECT_ROOT/docker-compose.yml" ]; then
        log_info "Starting with docker-compose..."
        cd "$PROJECT_ROOT"
        docker-compose up -d
        log_success "Services started with docker-compose"
    else
        log_info "Starting services manually..."
        
        # Start API server in background
        nohup "$PROJECT_ROOT/bin/api-server" > /var/log/esp-api.log 2>&1 &
        echo $! > /var/run/esp-api.pid
        log_success "API server started (PID: $(cat /var/run/esp-api.pid))"
        
        # Start worker in background
        nohup "$PROJECT_ROOT/bin/worker" > /var/log/esp-worker.log 2>&1 &
        echo $! > /var/run/esp-worker.pid
        log_success "Worker started (PID: $(cat /var/run/esp-worker.pid))"
    fi
}

# Health check
health_check() {
    log_info "Running health checks..."
    
    # Wait for services to start
    sleep 5
    
    # Check API server
    if curl -s -o /dev/null -w "%{http_code}" http://localhost:${PORT:-8080}/health | grep -q "200"; then
        log_success "API server is healthy"
    else
        log_error "API server health check failed"
        return 1
    fi
    
    log_success "All health checks passed"
}

# Print deployment summary
print_summary() {
    echo ""
    echo "=============================================="
    echo -e "${GREEN}DEPLOYMENT COMPLETE${NC}"
    echo "=============================================="
    echo ""
    echo "Services:"
    echo "  - API Server:  http://localhost:${PORT:-8080}"
    echo "  - Frontend:    http://localhost:${PORT:-8080}"
    echo ""
    echo "API Endpoints:"
    echo "  - Campaigns:   /api/mailing/campaigns"
    echo "  - Lists:       /api/mailing/lists"
    echo "  - AI:          /api/mailing/ai/*"
    echo "  - Throttle:    /api/mailing/throttle/*"
    echo "  - Inbox:       /api/mailing/inbox/*"
    echo ""
    echo "Configuration:"
    echo "  - Database:    ${DATABASE_URL:-Not Set}"
    echo "  - Redis:       ${REDIS_URL:-Not Set}"
    echo ""
    echo "Logs:"
    echo "  - API:         /var/log/esp-api.log"
    echo "  - Worker:      /var/log/esp-worker.log"
    echo "=============================================="
}

# Main deployment flow
main() {
    echo "=============================================="
    echo "ESP Platform Deployment"
    echo "=============================================="
    echo ""
    
    # Parse arguments
    SKIP_MIGRATIONS=false
    SKIP_BUILD=false
    SKIP_FRONTEND=false
    SKIP_DOCKER=false
    SKIP_START=false
    
    while [[ $# -gt 0 ]]; do
        case $1 in
            --skip-migrations)
                SKIP_MIGRATIONS=true
                shift
                ;;
            --skip-build)
                SKIP_BUILD=true
                shift
                ;;
            --skip-frontend)
                SKIP_FRONTEND=true
                shift
                ;;
            --skip-docker)
                SKIP_DOCKER=true
                shift
                ;;
            --skip-start)
                SKIP_START=true
                shift
                ;;
            --migrations-only)
                SKIP_BUILD=true
                SKIP_FRONTEND=true
                SKIP_DOCKER=true
                SKIP_START=true
                shift
                ;;
            --build-only)
                SKIP_MIGRATIONS=true
                SKIP_START=true
                shift
                ;;
            -h|--help)
                echo "Usage: $0 [options]"
                echo ""
                echo "Options:"
                echo "  --skip-migrations  Skip database migrations"
                echo "  --skip-build       Skip building binaries"
                echo "  --skip-frontend    Skip frontend build"
                echo "  --skip-docker      Skip Docker image build"
                echo "  --skip-start       Skip starting services"
                echo "  --migrations-only  Only run migrations"
                echo "  --build-only       Only build (no migrations or start)"
                echo "  -h, --help         Show this help message"
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                exit 1
                ;;
        esac
    done
    
    check_prerequisites
    
    if [ "$SKIP_MIGRATIONS" = false ]; then
        run_migrations
    fi
    
    if [ "$SKIP_BUILD" = false ]; then
        build_binaries
    fi
    
    if [ "$SKIP_FRONTEND" = false ]; then
        build_frontend
    fi
    
    if [ "$SKIP_DOCKER" = false ]; then
        build_docker
    fi
    
    if [ "$SKIP_START" = false ]; then
        start_services
        health_check
    fi
    
    print_summary
}

main "$@"

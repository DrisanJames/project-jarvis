package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"
)

// HealthStatus represents the overall health of the system.
type HealthStatus struct {
	Status  string                    `json:"status"`  // "healthy", "degraded", "unhealthy"
	Version string                    `json:"version"`
	Uptime  string                    `json:"uptime"`
	Checks  map[string]ComponentCheck `json:"checks"`
}

// ComponentCheck represents the health of a single component.
type ComponentCheck struct {
	Status  string `json:"status"`            // "up", "down", "degraded"
	Latency string `json:"latency,omitempty"`
	Message string `json:"message,omitempty"`
}

// HealthChecker provides comprehensive health check functionality
// for all system dependencies (DB, Redis, S3, workers).
type HealthChecker struct {
	db          *sql.DB
	redisClient *redis.Client
	s3Client    *s3.Client
	s3Bucket    string
	startTime   time.Time
}

// NewHealthChecker creates a new HealthChecker.
// Any dependency can be nil; the check will report "not_configured" for nil deps.
func NewHealthChecker(db *sql.DB, redisClient *redis.Client, s3Client *s3.Client, s3Bucket string) *HealthChecker {
	return &HealthChecker{
		db:          db,
		redisClient: redisClient,
		s3Client:    s3Client,
		s3Bucket:    s3Bucket,
		startTime:   time.Now(),
	}
}

const healthVersion = "1.0.0"

// HandleHealth returns the comprehensive health status of all components.
// Overall status is "healthy" if all checks pass, "degraded" if any are degraded
// or non-critical ones are down, and "unhealthy" if critical components are down.
//
//	GET /health
func (hc *HealthChecker) HandleHealth(w http.ResponseWriter, r *http.Request) {
	checks := hc.runAllChecks(r.Context())

	overall := determineOverallStatus(checks)

	status := HealthStatus{
		Status:  overall,
		Version: healthVersion,
		Uptime:  formatUptime(time.Since(hc.startTime)),
		Checks:  checks,
	}

	// Always return 200 for the general health endpoint.
	// The status field in the JSON body conveys health.
	// Use /health/ready for probes that need HTTP 503 on failure.
	respondJSON(w, http.StatusOK, status)
}

// HandleLiveness is a simple liveness probe — always returns 200 if the server
// process is running. Suitable for Kubernetes/ECS liveness probes.
//
//	GET /health/live
func (hc *HealthChecker) HandleLiveness(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "alive",
		"uptime": formatUptime(time.Since(hc.startTime)),
	})
}

// HandleReadiness checks all critical dependencies and returns 200 only when
// the service is ready to accept traffic. Suitable for readiness probes.
//
//	GET /health/ready
func (hc *HealthChecker) HandleReadiness(w http.ResponseWriter, r *http.Request) {
	checks := hc.runAllChecks(r.Context())

	overall := determineOverallStatus(checks)

	ready := overall != "unhealthy"
	httpStatus := http.StatusOK
	if !ready {
		httpStatus = http.StatusServiceUnavailable
	}

	respondJSON(w, httpStatus, map[string]interface{}{
		"ready":  ready,
		"status": overall,
		"checks": checks,
	})
}

// ---------------------------------------------------------------------------
// Individual component checks
// ---------------------------------------------------------------------------

func (hc *HealthChecker) runAllChecks(ctx context.Context) map[string]ComponentCheck {
	checks := make(map[string]ComponentCheck, 5)

	// Run checks concurrently for minimal total latency.
	type result struct {
		name  string
		check ComponentCheck
	}
	ch := make(chan result, 4)

	go func() { ch <- result{"database", hc.checkDatabase(ctx)} }()
	go func() { ch <- result{"redis", hc.checkRedis(ctx)} }()
	go func() { ch <- result{"s3", hc.checkS3(ctx)} }()
	go func() { ch <- result{"workers", hc.checkWorkers(ctx)} }()

	for i := 0; i < 4; i++ {
		r := <-ch
		checks[r.name] = r.check
	}

	return checks
}

// checkDatabase pings PostgreSQL with a 3-second timeout.
func (hc *HealthChecker) checkDatabase(ctx context.Context) ComponentCheck {
	if hc.db == nil {
		return ComponentCheck{Status: "down", Message: "not configured"}
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	start := time.Now()
	err := hc.db.PingContext(pingCtx)
	latency := time.Since(start)

	if err != nil {
		return ComponentCheck{
			Status:  "down",
			Latency: latency.String(),
			Message: fmt.Sprintf("ping failed: %v", err),
		}
	}

	status := "up"
	msg := "connected"
	if latency > 1*time.Second {
		status = "degraded"
		msg = fmt.Sprintf("slow response (%s)", latency)
	}

	return ComponentCheck{
		Status:  status,
		Latency: latency.String(),
		Message: msg,
	}
}

// checkRedis pings Redis with a 2-second timeout.
func (hc *HealthChecker) checkRedis(ctx context.Context) ComponentCheck {
	if hc.redisClient == nil {
		return ComponentCheck{Status: "down", Message: "not configured"}
	}

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	start := time.Now()
	err := hc.redisClient.Ping(pingCtx).Err()
	latency := time.Since(start)

	if err != nil {
		return ComponentCheck{
			Status:  "down",
			Latency: latency.String(),
			Message: fmt.Sprintf("ping failed: %v", err),
		}
	}

	status := "up"
	msg := "connected"
	if latency > 500*time.Millisecond {
		status = "degraded"
		msg = fmt.Sprintf("slow response (%s)", latency)
	}

	return ComponentCheck{
		Status:  status,
		Latency: latency.String(),
		Message: msg,
	}
}

// checkS3 verifies the S3 client can reach the configured bucket via HeadBucket.
func (hc *HealthChecker) checkS3(ctx context.Context) ComponentCheck {
	if hc.s3Client == nil {
		return ComponentCheck{Status: "down", Message: "not configured"}
	}
	if hc.s3Bucket == "" {
		return ComponentCheck{Status: "down", Message: "no bucket configured"}
	}

	s3Ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	start := time.Now()
	_, err := hc.s3Client.HeadBucket(s3Ctx, &s3.HeadBucketInput{
		Bucket: &hc.s3Bucket,
	})
	latency := time.Since(start)

	if err != nil {
		return ComponentCheck{
			Status:  "down",
			Latency: latency.String(),
			Message: fmt.Sprintf("HeadBucket failed: %v", err),
		}
	}

	return ComponentCheck{
		Status:  "up",
		Latency: latency.String(),
		Message: fmt.Sprintf("bucket %q accessible", hc.s3Bucket),
	}
}

// checkWorkers checks campaign queue depth as a proxy for worker health.
// If the DB is available it queries mailing_campaign_queue; otherwise reports
// "not configured". A high queue depth results in a "degraded" status.
func (hc *HealthChecker) checkWorkers(ctx context.Context) ComponentCheck {
	if hc.db == nil {
		return ComponentCheck{Status: "down", Message: "database not available"}
	}

	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	start := time.Now()
	var count int
	err := hc.db.QueryRowContext(queryCtx,
		`SELECT COUNT(*) FROM mailing_campaign_queue WHERE status = 'queued'`,
	).Scan(&count)
	latency := time.Since(start)

	if err != nil {
		// Table may not exist yet — treat as degraded rather than down.
		return ComponentCheck{
			Status:  "degraded",
			Latency: latency.String(),
			Message: fmt.Sprintf("queue check failed: %v", err),
		}
	}

	status := "up"
	msg := fmt.Sprintf("%d queued campaigns", count)
	if count > 100 {
		status = "degraded"
		msg = fmt.Sprintf("high queue depth: %d campaigns queued", count)
	}

	return ComponentCheck{
		Status:  status,
		Latency: latency.String(),
		Message: msg,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// determineOverallStatus derives the aggregate status from individual checks.
//
// Rules:
//   - "unhealthy" if database is down (critical dependency)
//   - "degraded"  if any check is degraded or a non-critical check is down
//   - "healthy"   otherwise
func determineOverallStatus(checks map[string]ComponentCheck) string {
	// Database is the only hard dependency — if it's down, we're unhealthy.
	if db, ok := checks["database"]; ok && db.Status == "down" {
		// Only mark unhealthy if the DB is configured (down + configured = unhealthy).
		if db.Message != "not configured" {
			return "unhealthy"
		}
	}

	for _, c := range checks {
		if c.Status == "degraded" {
			return "degraded"
		}
		if c.Status == "down" && c.Message != "not configured" {
			return "degraded"
		}
	}

	return "healthy"
}

// HandleDBStats returns raw database/sql pool statistics for diagnostics.
func (hc *HealthChecker) HandleDBStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if hc.db == nil {
		w.Write([]byte(`{"error":"no database configured"}`))
		return
	}
	stats := hc.db.Stats()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	pingErr := ""
	pingStart := time.Now()
	if err := hc.db.PingContext(ctx); err != nil {
		pingErr = err.Error()
	}
	pingLatency := time.Since(pingStart)

	var pgInfo string
	row := hc.db.QueryRowContext(ctx, `SELECT version()`)
	if row != nil {
		row.Scan(&pgInfo)
	}

	var activeConns int
	row2 := hc.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()`)
	if row2 != nil {
		row2.Scan(&activeConns)
	}

	fmt.Fprintf(w, `{"pool":{"max_open":%d,"open":%d,"in_use":%d,"idle":%d,"wait_count":%d,"wait_duration":"%s","max_idle_closed":%d,"max_idle_time_closed":%d,"max_lifetime_closed":%d},"ping":{"latency":"%s","error":"%s"},"pg_version":"%s","pg_active_conns":%d}`,
		stats.MaxOpenConnections, stats.OpenConnections, stats.InUse, stats.Idle,
		stats.WaitCount, stats.WaitDuration, stats.MaxIdleClosed, stats.MaxIdleTimeClosed, stats.MaxLifetimeClosed,
		pingLatency, pingErr, pgInfo, activeConns)
}

// formatUptime produces a human-readable uptime string like "3d 4h 12m 5s".
func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

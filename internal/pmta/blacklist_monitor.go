package pmta

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// BlacklistMonitor periodically checks all active IPs against DNS blacklists
// and updates the database with results. Runs daily by default.
type BlacklistMonitor struct {
	db       *sql.DB
	checker  *HealthChecker
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewBlacklistMonitor creates a blacklist monitoring scheduler.
func NewBlacklistMonitor(db *sql.DB, interval time.Duration) *BlacklistMonitor {
	return &BlacklistMonitor{
		db:       db,
		checker:  NewHealthChecker(db),
		interval: interval,
	}
}

// Start begins periodic blacklist checking.
func (bm *BlacklistMonitor) Start() {
	bm.ctx, bm.cancel = context.WithCancel(context.Background())
	go func() {
		log.Printf("[BlacklistMonitor] Starting (interval: %s)", bm.interval)

		// Initial check after 1 minute
		time.Sleep(1 * time.Minute)
		bm.runChecks()

		ticker := time.NewTicker(bm.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				bm.runChecks()
			case <-bm.ctx.Done():
				log.Println("[BlacklistMonitor] Stopped")
				return
			}
		}
	}()
}

// Stop halts the monitor.
func (bm *BlacklistMonitor) Stop() {
	if bm.cancel != nil {
		bm.cancel()
	}
}

func (bm *BlacklistMonitor) runChecks() {
	ctx, cancel := context.WithTimeout(bm.ctx, 10*time.Minute)
	defer cancel()

	start := time.Now()
	checked, issues, err := bm.checker.RunHealthChecks(ctx)
	if err != nil {
		log.Printf("[BlacklistMonitor] Error running checks: %v", err)
		return
	}

	log.Printf("[BlacklistMonitor] Completed: %d IPs checked, %d issues found in %s",
		checked, issues, time.Since(start))

	if issues > 0 {
		bm.notifyIssues(ctx, issues)
	}
}

func (bm *BlacklistMonitor) notifyIssues(ctx context.Context, issueCount int) {
	// Log blacklisted IPs for visibility — in production, this would
	// also send alerts (email, Slack, PagerDuty, etc.)
	rows, err := bm.db.QueryContext(ctx, `
		SELECT ip_address::text, hostname, blacklisted_on
		FROM mailing_ip_addresses
		WHERE status = 'blacklisted' AND blacklisted_on != '[]'::jsonb
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var ip, hostname string
		var blacklists []byte
		if err := rows.Scan(&ip, &hostname, &blacklists); err != nil {
			continue
		}
		log.Printf("[BlacklistMonitor] ALERT: IP %s (%s) is blacklisted: %s", ip, hostname, string(blacklists))
	}
}

package pmta

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

const (
	maxBounceRate    = 0.03 // 3% — pause warmup above this
	maxComplaintRate = 0.001 // 0.1% — pause warmup above this
)

// WarmupScheduler runs daily to advance IPs through the warmup schedule
// and auto-pause any IP that breaches bounce or complaint thresholds.
type WarmupScheduler struct {
	db        *sql.DB
	mailingDB *sql.DB // separate connection to mailing_subscribers for quality queries
	ctx       context.Context
	cancel    context.CancelFunc
	interval  time.Duration

	// Data quality thresholds per warmup phase (configurable via H14)
	SeedThreshold     float64 // days 1-7: only score >= this (default 0.75)
	ValidateThreshold float64 // days 8-14: score >= this (default 0.50)
	ExpandThreshold   float64 // days 15-22: score >= this (default 0.25)
}

// WarmupScheduleEntry maps warmup day to the planned volume.
var WarmupScheduleEntry = []struct {
	Day    int
	Volume int
}{
	{1, 50}, {2, 50}, {3, 100}, {4, 100},
	{5, 250}, {6, 250}, {7, 250},
	{8, 500}, {9, 500}, {10, 500},
	{11, 1000}, {12, 1000}, {13, 1000}, {14, 1000},
	{15, 2500}, {16, 2500}, {17, 2500}, {18, 2500},
	{19, 5000}, {20, 5000}, {21, 5000}, {22, 5000},
	{23, 10000}, {24, 10000}, {25, 10000}, {26, 10000},
	{27, 25000}, {28, 25000}, {29, 25000}, {30, 25000},
}

// NewWarmupScheduler creates a scheduler. interval controls how often it checks;
// for production, use 1 hour or shorter.
func NewWarmupScheduler(db *sql.DB, interval time.Duration) *WarmupScheduler {
	return &WarmupScheduler{
		db:                db,
		interval:          interval,
		SeedThreshold:     0.75,
		ValidateThreshold: 0.50,
		ExpandThreshold:   0.25,
	}
}

// SetMailingDB sets a database connection for querying mailing_subscribers.
func (ws *WarmupScheduler) SetMailingDB(mailingDB *sql.DB) {
	ws.mailingDB = mailingDB
}

// Start begins the scheduler loop.
func (ws *WarmupScheduler) Start() {
	ws.ctx, ws.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[WarmupScheduler] Starting warmup scheduler")

		time.Sleep(10 * time.Second) // initial delay
		ws.tick()

		ticker := time.NewTicker(ws.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				ws.tick()
			case <-ws.ctx.Done():
				log.Println("[WarmupScheduler] Stopped")
				return
			}
		}
	}()
}

// Stop halts the scheduler.
func (ws *WarmupScheduler) Stop() {
	if ws.cancel != nil {
		ws.cancel()
	}
}

func (ws *WarmupScheduler) tick() {
	ctx, cancel := context.WithTimeout(ws.ctx, 2*time.Minute)
	defer cancel()

	ws.advanceWarmupDays(ctx)
	ws.checkThresholds(ctx)
	ws.ensureTodayLogEntries(ctx)
	ws.graduateCompletedIPs(ctx)
}

// advanceWarmupDays moves IPs to the next warmup day if they haven't been advanced today.
func (ws *WarmupScheduler) advanceWarmupDays(ctx context.Context) {
	rows, err := ws.db.QueryContext(ctx, `
		SELECT id, warmup_day, warmup_started_at
		FROM mailing_ip_addresses
		WHERE status = 'warmup' AND warmup_started_at IS NOT NULL
	`)
	if err != nil {
		log.Printf("[WarmupScheduler] Failed to query warmup IPs: %v", err)
		return
	}
	defer rows.Close()

	now := time.Now()

	for rows.Next() {
		var ipID string
		var warmupDay int
		var warmupStarted time.Time
		if err := rows.Scan(&ipID, &warmupDay, &warmupStarted); err != nil {
			continue
		}

		// Calculate expected warmup day based on elapsed time
		elapsed := now.Sub(warmupStarted)
		expectedDay := int(elapsed.Hours()/24) + 1
		if expectedDay < 1 {
			expectedDay = 1
		}
		if expectedDay > 30 {
			expectedDay = 30
		}

		if expectedDay > warmupDay {
			newLimit := volumeForDay(expectedDay)
			_, err := ws.db.ExecContext(ctx, `
				UPDATE mailing_ip_addresses
				SET warmup_day = $1, warmup_daily_limit = $2, warmup_stage = $3, updated_at = NOW()
				WHERE id = $4 AND status = 'warmup'
			`, expectedDay, newLimit, stageForDay(expectedDay), ipID)
			if err != nil {
				log.Printf("[WarmupScheduler] Failed to advance IP %s to day %d: %v", ipID, expectedDay, err)
			} else {
				log.Printf("[WarmupScheduler] Advanced IP %s to day %d (limit: %d)", ipID, expectedDay, newLimit)
			}
		}
	}
}

// checkThresholds pauses any warming IP whose bounce or complaint rate exceeds thresholds.
func (ws *WarmupScheduler) checkThresholds(ctx context.Context) {
	rows, err := ws.db.QueryContext(ctx, `
		SELECT ip.id, ip.ip_address::text, ip.warmup_day,
		       wl.actual_sent, wl.actual_bounced, wl.actual_complained
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_warmup_log wl ON wl.ip_id = ip.id AND wl.date = CURRENT_DATE
		WHERE ip.status = 'warmup' AND wl.actual_sent > 10
	`)
	if err != nil {
		log.Printf("[WarmupScheduler] Failed to query thresholds: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var ipID, ipAddr string
		var warmupDay, sent, bounced, complained int
		if err := rows.Scan(&ipID, &ipAddr, &warmupDay, &sent, &bounced, &complained); err != nil {
			continue
		}

		bounceRate := float64(bounced) / float64(sent)
		complaintRate := float64(complained) / float64(sent)

		if bounceRate > maxBounceRate || complaintRate > maxComplaintRate {
			reason := ""
			if bounceRate > maxBounceRate {
				reason = fmt.Sprintf("bounce rate %.2f%% exceeds %.1f%% threshold", bounceRate*100, maxBounceRate*100)
			}
			if complaintRate > maxComplaintRate {
				if reason != "" {
					reason += "; "
				}
				reason += fmt.Sprintf("complaint rate %.3f%% exceeds %.2f%% threshold", complaintRate*100, maxComplaintRate*100)
			}

			log.Printf("[WarmupScheduler] AUTO-PAUSE IP %s (day %d): %s", ipAddr, warmupDay, reason)

			ws.db.ExecContext(ctx, `
				UPDATE mailing_ip_addresses
				SET status = 'paused', updated_at = NOW()
				WHERE id = $1
			`, ipID)

			ws.db.ExecContext(ctx, `
				UPDATE mailing_ip_warmup_log
				SET status = 'failed', notes = $1
				WHERE ip_id = $2 AND date = CURRENT_DATE
			`, "Auto-paused: "+reason, ipID)
		}
	}
}

// ensureTodayLogEntries creates warmup log entries for today if they don't exist.
func (ws *WarmupScheduler) ensureTodayLogEntries(ctx context.Context) {
	rows, err := ws.db.QueryContext(ctx, `
		SELECT id, warmup_day, warmup_daily_limit
		FROM mailing_ip_addresses
		WHERE status = 'warmup'
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var ipID string
		var warmupDay, limit int
		if err := rows.Scan(&ipID, &warmupDay, &limit); err != nil {
			continue
		}

		ws.db.ExecContext(ctx, `
			INSERT INTO mailing_ip_warmup_log (ip_id, date, planned_volume, warmup_day, status)
			VALUES ($1, CURRENT_DATE, $2, $3, 'in_progress')
			ON CONFLICT (ip_id, date) DO UPDATE SET
				status = CASE WHEN mailing_ip_warmup_log.status = 'pending' THEN 'in_progress' ELSE mailing_ip_warmup_log.status END
		`, ipID, limit, warmupDay)
	}
}

// graduateCompletedIPs moves IPs that have completed 30 days of warmup to 'active' status.
func (ws *WarmupScheduler) graduateCompletedIPs(ctx context.Context) {
	result, err := ws.db.ExecContext(ctx, `
		UPDATE mailing_ip_addresses
		SET status = 'active', warmup_stage = 'established', updated_at = NOW()
		WHERE status = 'warmup' AND warmup_day >= 30
		  AND warmup_started_at < NOW() - INTERVAL '30 days'
	`)
	if err != nil {
		log.Printf("[WarmupScheduler] Failed to graduate IPs: %v", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		log.Printf("[WarmupScheduler] Graduated %d IPs from warmup to active", n)
	}
}

// selectWarmupRecipients returns subscriber IDs eligible for today's warmup sends.
// Uses data_quality_score tiers that relax as the warmup progresses:
//   - Seed phase (days 1-7):  score >= SeedThreshold OR data_source = 'jvc-warmup'
//   - Validate phase (8-14):  score >= ValidateThreshold
//   - Expand phase (15-22):   score >= ExpandThreshold
//   - Scale phase (23-30):    all confirmed subscribers
func (ws *WarmupScheduler) SelectWarmupRecipients(ctx context.Context, warmupDay int, limit int) ([]string, error) {
	db := ws.mailingDB
	if db == nil {
		db = ws.db
	}

	var query string
	var args []interface{}

	switch {
	case warmupDay <= 7:
		query = `SELECT id FROM mailing_subscribers
			WHERE status = 'confirmed' AND (data_quality_score >= $1 OR data_source = 'jvc-warmup')
			ORDER BY data_quality_score DESC LIMIT $2`
		args = []interface{}{ws.SeedThreshold, limit}
	case warmupDay <= 14:
		query = `SELECT id FROM mailing_subscribers
			WHERE status = 'confirmed' AND data_quality_score >= $1
			ORDER BY data_quality_score DESC LIMIT $2`
		args = []interface{}{ws.ValidateThreshold, limit}
	case warmupDay <= 22:
		query = `SELECT id FROM mailing_subscribers
			WHERE status = 'confirmed' AND data_quality_score >= $1
			ORDER BY data_quality_score DESC LIMIT $2`
		args = []interface{}{ws.ExpandThreshold, limit}
	default:
		query = `SELECT id FROM mailing_subscribers
			WHERE status = 'confirmed'
			ORDER BY data_quality_score DESC LIMIT $1`
		args = []interface{}{limit}
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select warmup recipients: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func volumeForDay(day int) int {
	for _, entry := range WarmupScheduleEntry {
		if entry.Day == day {
			return entry.Volume
		}
	}
	if day > 30 {
		return 50000
	}
	return 50
}

func stageForDay(day int) string {
	if day <= 2 {
		return "day1"
	}
	if day <= 7 {
		return "early"
	}
	if day <= 14 {
		return "building"
	}
	if day <= 22 {
		return "ramping"
	}
	if day <= 30 {
		return "maturing"
	}
	return "established"
}

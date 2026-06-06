package worker

// =============================================================================
// WORKER HEALTH — heartbeats + stall detection + alerting
// =============================================================================
// Background workers (DataCleanup, EngineSignalsArchiver, SegmentCleanup,
// SegmentRefresh, …) emit a heartbeat at the end of each cycle into
// mailing_worker_heartbeats. WorkerHealthMonitor periodically scans that table
// and flags any worker whose last heartbeat is older than a multiple of its
// declared cycle interval, then alerts (Slack, or log via Noop) — deduped so a
// persistent stall pages at most once per re-alert window.
//
// This exists because a stalled maintenance worker (e.g. the EngineSignals
// archiver that silently wedged for ~2 months) is invisible until storage
// blows up. Heartbeats make "is the worker actually running?" a first-class,
// queryable signal surfaced in the UI and pushed to Slack.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/notify"
)

// EmitHeartbeat upserts a worker's heartbeat. It is best-effort: failures are
// logged and never propagated, so heartbeat bookkeeping can never break a
// worker's real job. status is "ok" or "error"; errMsg is optional detail.
// expectedIntervalSeconds is the worker's nominal cycle period; the monitor
// uses it (×stallMultiplier) to decide when the worker is overdue.
//
// Every successful beat clears any prior stall state for the worker, so a
// worker that resumes after a hiccup self-heals without operator action.
func EmitHeartbeat(ctx context.Context, db *sql.DB, workerName string, expectedIntervalSeconds int, status, errMsg string) {
	if db == nil || workerName == "" {
		return
	}
	if status == "" {
		status = "ok"
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var errArg interface{}
	if errMsg != "" {
		errArg = errMsg
	}

	_, err := db.ExecContext(hctx, `
		INSERT INTO mailing_worker_heartbeats
			(worker_name, last_beat_at, last_status, last_error,
			 cycle_count, expected_interval_seconds, stalled, stalled_since,
			 updated_at)
		VALUES ($1, NOW(), $2, $3, 1, $4, FALSE, NULL, NOW())
		ON CONFLICT (worker_name) DO UPDATE SET
			last_beat_at              = NOW(),
			last_status               = EXCLUDED.last_status,
			last_error                = EXCLUDED.last_error,
			cycle_count               = mailing_worker_heartbeats.cycle_count + 1,
			expected_interval_seconds = EXCLUDED.expected_interval_seconds,
			stalled                   = FALSE,
			stalled_since             = NULL,
			updated_at                = NOW()
	`, workerName, status, errArg, expectedIntervalSeconds)
	if err != nil {
		// Table may not exist yet on first boot before runStartupMigrations;
		// stay quiet in that case to avoid log spam.
		if !isTableNotExistsError(err) {
			log.Printf("[heartbeat] %s: %v", workerName, err)
		}
	}
}

// WorkerHealthMonitor scans heartbeats and alerts on stalls.
type WorkerHealthMonitor struct {
	db       *sql.DB
	notifier notify.Notifier
	interval time.Duration
	// stallMultiplier: a worker is "stalled" once last_beat_at is older than
	// expected_interval_seconds * stallMultiplier. 3 gives one full missed
	// cycle of grace plus margin before paging.
	stallMultiplier int
	// reAlertAfter throttles repeat alerts for a still-stalled worker.
	reAlertAfter time.Duration
}

// NewWorkerHealthMonitor builds the monitor. notifier may be nil (defaults to
// Noop). It checks every 5 minutes by default.
func NewWorkerHealthMonitor(db *sql.DB, notifier notify.Notifier) *WorkerHealthMonitor {
	if notifier == nil {
		notifier = notify.NoopNotifier{}
	}
	return &WorkerHealthMonitor{
		db:              db,
		notifier:        notifier,
		interval:        5 * time.Minute,
		stallMultiplier: 3,
		reAlertAfter:    1 * time.Hour,
	}
}

// Start runs the monitor loop until ctx is cancelled.
func (m *WorkerHealthMonitor) Start(ctx context.Context) {
	if m.db == nil {
		log.Println("[WorkerHealthMonitor] disabled (no db)")
		return
	}
	log.Printf("[WorkerHealthMonitor] started (interval=%s, stall=%d× cycle, re-alert=%s, transport=%s)",
		m.interval, m.stallMultiplier, m.reAlertAfter, m.notifier.Name())

	// Brief startup delay so runStartupMigrations creates the table and
	// workers emit their first beats before we evaluate staleness.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
	}
	m.checkOnce(ctx)

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[WorkerHealthMonitor] stopping")
			return
		case <-t.C:
			m.checkOnce(ctx)
		}
	}
}

func (m *WorkerHealthMonitor) checkOnce(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	// Mark overdue workers stalled (idempotent: stalled_since is only set once).
	if _, err := m.db.ExecContext(qctx, `
		UPDATE mailing_worker_heartbeats
		SET stalled = TRUE,
		    stalled_since = COALESCE(stalled_since, NOW()),
		    updated_at = NOW()
		WHERE last_beat_at < NOW() - make_interval(secs => expected_interval_seconds * $1)
	`, m.stallMultiplier); err != nil {
		if isTableNotExistsError(err) {
			return
		}
		log.Printf("[WorkerHealthMonitor] mark-stalled error: %v", err)
		return
	}

	// Select stalled workers that haven't been alerted within the re-alert
	// window, claim them by stamping last_alerted_at, and alert.
	rows, err := m.db.QueryContext(qctx, `
		UPDATE mailing_worker_heartbeats
		SET last_alerted_at = NOW()
		WHERE stalled = TRUE
		  AND (last_alerted_at IS NULL OR last_alerted_at < NOW() - make_interval(secs => $1))
		RETURNING worker_name, last_beat_at, expected_interval_seconds, last_status, COALESCE(last_error, '')
	`, int(m.reAlertAfter.Seconds()))
	if err != nil {
		log.Printf("[WorkerHealthMonitor] claim-alert error: %v", err)
		return
	}
	type stall struct {
		name     string
		lastBeat time.Time
		interval int
		status   string
		errMsg   string
	}
	var stalls []stall
	for rows.Next() {
		var s stall
		if err := rows.Scan(&s.name, &s.lastBeat, &s.interval, &s.status, &s.errMsg); err == nil {
			stalls = append(stalls, s)
		}
	}
	rows.Close()

	for _, s := range stalls {
		age := time.Since(s.lastBeat).Round(time.Minute)
		title := fmt.Sprintf("⚠️ Worker stalled: %s", s.name)
		body := fmt.Sprintf("No heartbeat for %s (expected every %ds). last_status=%s%s",
			age, s.interval, s.status, errSuffix(s.errMsg))
		if err := m.notifier.Notify(title, body); err != nil {
			log.Printf("[WorkerHealthMonitor] alert send failed for %s: %v", s.name, err)
		} else {
			log.Printf("[WorkerHealthMonitor] ALERTED stall: %s (idle %s)", s.name, age)
		}
	}
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	if len(e) > 300 {
		e = e[:300] + "…"
	}
	return " err=" + e
}

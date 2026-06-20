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
	"unicode/utf8"

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

// runDetailMaxLen caps the free-text detail stored per run so a multi-KB
// error string can't bloat the run-summary table.
const runDetailMaxLen = 300

// workerRunRetention is how long per-cycle run rows are kept before the
// opportunistic cleanup in RecordWorkerRun deletes them.
const workerRunRetention = 30 * 24 * time.Hour

// truncateRunDetail caps detail at runDetailMaxLen bytes, backing off to a
// valid UTF-8 boundary so the truncated string stays insertable into a
// Postgres TEXT column.
func truncateRunDetail(detail string) string {
	if len(detail) <= runDetailMaxLen {
		return detail
	}
	cut := detail[:runDetailMaxLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// RecordWorkerRun appends a run-level summary row for one completed worker
// cycle into mailing_worker_runs (finished_at = now, duration derived from
// startedAt). Like EmitHeartbeat it is best-effort: failures are logged and
// never propagated, so run bookkeeping can never break a worker's real job.
// status is "ok", "partial" or "failed"; detail is free text truncated to
// runDetailMaxLen. Each call also opportunistically deletes rows older than
// 30 days for the same worker_name (cheap via idx_worker_runs_name_time;
// errors ignored).
//
// If the caller's context is already cancelled (e.g. the worker is shutting
// down mid-cycle), the write detaches to a fresh background context so the
// terminal run row still lands.
func RecordWorkerRun(ctx context.Context, db *sql.DB, workerName string, startedAt time.Time, status string, itemsProcessed, itemsFailed int, detail string) {
	if db == nil || workerName == "" {
		return
	}
	if status == "" {
		status = "ok"
	}
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	finishedAt := time.Now()
	durMs := int(finishedAt.Sub(startedAt).Milliseconds())
	if durMs < 0 {
		durMs = 0
	}
	var detailArg interface{}
	if d := truncateRunDetail(detail); d != "" {
		detailArg = d
	}

	if _, err := db.ExecContext(rctx, `
		INSERT INTO mailing_worker_runs
			(worker_name, started_at, finished_at, duration_ms, status,
			 items_processed, items_failed, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, workerName, startedAt, finishedAt, durMs, status, itemsProcessed, itemsFailed, detailArg); err != nil {
		// Table may not exist yet on first boot before runStartupMigrations;
		// stay quiet in that case to avoid log spam.
		if !isTableNotExistsError(err) {
			log.Printf("[worker-run] %s: %v", workerName, err)
		}
		return
	}

	// Opportunistic retention: bounded by the (worker_name, started_at DESC)
	// index, so this is cheap. Errors are deliberately ignored.
	_, _ = db.ExecContext(rctx, `
		DELETE FROM mailing_worker_runs
		WHERE worker_name = $1 AND started_at < NOW() - $2::interval
	`, workerName, fmt.Sprintf("%d seconds", int(workerRunRetention.Seconds())))
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
		msg := notify.Message{
			Tier:     notify.TierAlert,
			Scope:    "Worker stall",
			Headline: fmt.Sprintf("*%s* no heartbeat for %s (expected %ds)", s.name, age, s.interval),
			Body:     fmt.Sprintf("last_status=%s%s", s.status, errSuffix(s.errMsg)),
		}
		if err := notify.Deliver(m.notifier, msg); err != nil {
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

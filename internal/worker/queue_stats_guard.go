package worker

// QueueStatsGuard — refreshes mailing_campaign_queue planner statistics when
// they collapse while the queue is not empty.
//
// Root cause of the 2026-11-18 SES stall (measured): an autoanalyze finished
// 14:47:18 UTC while the queue was drained between waves. Every partial index
// on status='queued' got pg_class.reltuples = 0, all index paths for the
// dispatcher's per-campaign count tied at the cost floor, and the planner picked
// a status-only partial index — a scan of ALL queued rows per campaign. Count
// latency went 2 ms -> 550 ms over six hours as the queue refilled, then back to
// 2 ms the minute stats were refreshed.
//
// Every 60 s: read reltuples of the two campaign-leading partial indexes (a
// catalog read) and compare with the live queued depth the OutboxSelfCheck
// already publishes (no second heavy count). STALE = depth >= floor AND either
// index's reltuples < depth/50. Two consecutive STALE samples -> ANALYZE under a
// distlock (one ECS task), 120 s budget, at most once per 10 min per task.
//
// QUEUE_STATS_GUARD=off disables; =observe logs + alerts without ANALYZE.
// QUEUE_STATS_GUARD_MIN_QUEUED (default 20000) is the depth floor.
//
// Re-run safety: ANALYZE is idempotent; two tasks are serialised by the lock
// and the second one's next sample reads fresh stats (not STALE). A crash
// mid-ANALYZE leaves the old stats — the next samples retry after cooldown.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/dbscan"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

const (
	queueStatsGuardInterval     = 60 * time.Second
	queueStatsGuardCooldown     = 10 * time.Minute
	queueStatsGuardAnalyzeTO    = 120 * time.Second
	queueStatsGuardMinDefault   = 20000
	queueStatsGuardRatio        = 50
	queueStatsGuardSamples      = 2
	queueStatsGuardDepthMaxAge  = 11 * time.Minute // self-check ticks every 5 min
	QueueStatsGuardLockKey      = "queue-stats-guard-analyze"
	queueStatsGuardCatalogQuery = `
		SELECT c.relname, c.reltuples::bigint
		FROM pg_class c
		WHERE c.relname IN ('idx_queue_active_by_campaign','idx_queue_campaign_status_scheduled')`
)

var (
	statsGuardAnalyzes atomic.Int64
	statsGuardErrors   atomic.Int64
	statsGuardLastAt   atomic.Int64 // unix nanos of the last successful ANALYZE
)

// QueueStatsGuard: see the file comment.
type QueueStatsGuard struct {
	db         *sql.DB
	lock       distlock.DistLock
	alerter    SMSAlerter
	recipients []string

	mode      string // "on" | "observe" | "off"
	minQueued int64
	interval  time.Duration

	// depth returns the live queued depth and whether it is a fresh, measured
	// value. Defaults to the OutboxSelfCheck snapshot.
	depth func() (int64, bool)
	now   func() time.Time

	staleStreak int
	lastAction  time.Time // last ANALYZE attempt (or observe alert); cooldown anchor
}

// NewQueueStatsGuard builds the guard. lock should come from distlock.NewLock
// (Redis preferred, PG advisory fallback).
func NewQueueStatsGuard(db *sql.DB, lock distlock.DistLock) *QueueStatsGuard {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("QUEUE_STATS_GUARD")))
	if mode != "off" && mode != "observe" {
		mode = "on"
	}
	minQ := int64(queueStatsGuardMinDefault)
	if raw := strings.TrimSpace(os.Getenv("QUEUE_STATS_GUARD_MIN_QUEUED")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			minQ = n
		}
	}
	return &QueueStatsGuard{
		db: db, lock: lock, mode: mode, minQueued: minQ, interval: queueStatsGuardInterval,
		depth: snapshotQueuedDepth, now: time.Now,
	}
}

// snapshotQueuedDepth reads queue_ready_rows from the self-check snapshot;
// stale, unmeasured or errored = not ok (UNKNOWN, never acts).
func snapshotQueuedDepth() (int64, bool) {
	s := CurrentSendLiveness()
	if s.CheckedAt == nil || time.Since(*s.CheckedAt) > queueStatsGuardDepthMaxAge {
		return 0, false
	}
	for _, e := range s.Errors {
		if e == "queue_ready_rows" {
			return 0, false
		}
	}
	return s.QueueReadyRows, true
}

// SetAlerter wires the Slack/SMS transport (same contract as OutboxSelfCheck).
func (g *QueueStatsGuard) SetAlerter(a SMSAlerter, recipients []string) {
	if a == nil || len(recipients) == 0 {
		g.alerter, g.recipients = nil, nil
		return
	}
	g.alerter, g.recipients = a, append([]string(nil), recipients...)
}

// Mode reports the effective mode (for the boot log).
func (g *QueueStatsGuard) Mode() string { return g.mode }

// Start blocks until ctx is cancelled.
func (g *QueueStatsGuard) Start(ctx context.Context) {
	log.Printf("[QueueStatsGuard] mode=%s min_queued=%d interval=%s cooldown=%s", g.mode, g.minQueued, g.interval, queueStatsGuardCooldown)
	if g.mode == "off" {
		return
	}
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.runOnce(ctx)
		}
	}
}

// runOnce is one sample. Never panics on DB errors; every failure is a no-op.
func (g *QueueStatsGuard) runOnce(ctx context.Context) {
	if g.mode == "off" {
		return
	}
	depth, ok := g.depth()
	if !ok || depth < g.minQueued {
		g.staleStreak = 0
		return
	}
	minRel, err := g.indexReltuples(ctx)
	if err != nil {
		g.staleStreak = 0
		log.Printf("[QueueStatsGuard] catalog read failed (sample UNKNOWN): %v", err)
		return
	}
	if minRel >= depth/queueStatsGuardRatio {
		g.staleStreak = 0
		return
	}
	g.staleStreak++
	if g.staleStreak < queueStatsGuardSamples {
		log.Printf("[QueueStatsGuard] stale sample %d/%d: reltuples=%d live_queued=%d", g.staleStreak, queueStatsGuardSamples, minRel, depth)
		return
	}
	if !g.lastAction.IsZero() && g.now().Sub(g.lastAction) < queueStatsGuardCooldown {
		return
	}

	if g.mode == "observe" {
		g.lastAction = g.now()
		g.staleStreak = 0
		log.Printf("[QueueStatsGuard] stale planner stats: reltuples=%d live_queued=%d -> ANALYZE skipped (observe)", minRel, depth)
		g.alert(ctx, fmt.Sprintf(
			"Queue planner statistics · stale, NOT refreshed (observe mode) · reltuples %s\nLive queued: %s rows\nRun: `ANALYZE mailing_campaign_queue`",
			comma(int(minRel)), comma(int(depth))))
		return
	}

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	got, lerr := g.lock.Acquire(lockCtx)
	cancel()
	if lerr != nil || !got {
		log.Printf("[QueueStatsGuard] stale stats but lock not acquired (acquired=%v err=%v); another task owns it", got, lerr)
		return
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = g.lock.Release(rctx)
		rcancel()
	}()

	g.lastAction = g.now()
	g.staleStreak = 0
	start := time.Now()
	err = g.analyze(ctx)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		statsGuardErrors.Add(1)
		log.Printf("[QueueStatsGuard] stale planner stats: reltuples=%d live_queued=%d -> ANALYZE FAILED after %s (retry after %s): %v",
			minRel, depth, took, queueStatsGuardCooldown, err)
		return
	}
	statsGuardAnalyzes.Add(1)
	statsGuardLastAt.Store(time.Now().UnixNano())
	log.Printf("[QueueStatsGuard] stale planner stats: reltuples=%d live_queued=%d -> ANALYZE (took %s)", minRel, depth, took)
	g.alert(ctx, fmt.Sprintf(
		"Queue planner statistics · were stale, refreshed · ANALYZE took %s\nIndex reltuples: %s\nLive queued: %s rows\nNext: none unless it repeats within 1h",
		took, comma(int(minRel)), comma(int(depth))))
}

// indexReltuples returns the smaller reltuples of the two indexes
// (complete-or-error). No rows = error (index missing: nothing to judge).
func (g *QueueStatsGuard) indexReltuples(ctx context.Context) (int64, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	type rel struct {
		name string
		n    int64
	}
	rows, qerr := g.db.QueryContext(qctx, queueStatsGuardCatalogQuery)
	res, err := dbscan.Query(rows, qerr, func(r dbscan.Rows) (rel, error) {
		var x rel
		err := r.Scan(&x.name, &x.n)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("no queue index rows in pg_class")
	}
	minRel := res[0].n
	for _, x := range res[1:] {
		if x.n < minRel {
			minRel = x.n
		}
	}
	return minRel, nil
}

// analyze runs ANALYZE in a transaction with statement_timeout raised to the
// 120 s budget (the app pool's 30 s ceiling would cancel it; autoanalyze took
// 18-30 s on 11-18).
func (g *QueueStatsGuard) analyze(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, queueStatsGuardAnalyzeTO+5*time.Second)
	defer cancel()
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'; SET LOCAL lock_timeout = '10s'", queueStatsGuardAnalyzeTO.Milliseconds())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "ANALYZE mailing_campaign_queue"); err != nil {
		return err
	}
	return tx.Commit()
}

func (g *QueueStatsGuard) alert(ctx context.Context, msg string) {
	if g.alerter == nil {
		return
	}
	for _, to := range g.recipients {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := g.alerter.SendSMS(sctx, to, msg); err != nil {
			log.Printf("[QueueStatsGuard] alert send failed: %v", err)
		}
		cancel()
	}
}

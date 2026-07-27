package worker

// FreshBroadcastWorker — the "system invokes it" leg of the fresh broadcast
// runner (operator 2026-07-27: "maybe it is an API endpoint you can invoke
// and/or have the system invoke as well").
//
// Nightly 09:00 UTC (after the 08:15Z cohort remint so the night's fresh
// supply is in place). Each pass:
//
//   1. ALWAYS runs a DRY pass for every enabled stream and records the
//      fill-vs-cap summary in mailing_fresh_broadcast_runs — the operator's
//      daily data-ask signal, produced whether or not anything stages.
//   2. Runs LIVE (stage + draft campaigns) ONLY for streams whose
//      mailing_stream_broadcast_config.auto_stage is TRUE. Campaigns still
//      land as status='draft' — the Draft Board remains the approval gate;
//      auto_stage never promotes.
//
// The runner logic lives in internal/api (fresh_broadcast_runner.go); this
// package cannot import internal/api (api imports worker — a cycle), so the
// run function is INJECTED at boot (cmd/server/main.go), the same inversion
// as WrapPMTACampaignDeploy.
//
// Single-writer: distlock (Redis preferred, PG advisory fallback). Heartbeat
// 'fresh_broadcast' + run summaries in mailing_worker_heartbeats /
// mailing_worker_runs. NO boot pass (send-path safety, the 2026-07-13
// lesson). Kill switch: DISABLE_FRESH_BROADCAST_RUNNER=true.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
)

const (
	freshBroadcastWorkerName = "fresh_broadcast"
	freshBroadcastLockKey    = "fresh-broadcast-runner"

	// Daily anchor: 09:00 UTC — after the 08:15Z nightly cohort remint.
	freshBroadcastAnchorHour = 9
	freshBroadcastAnchorMin  = 0
)

// FreshBroadcastRunFn is the injected runner entrypoint
// (api.FreshBroadcastRunner.RunForWorker).
type FreshBroadcastRunFn func(ctx context.Context, date string, dry, autoStageOnly bool, trigger string) error

// FreshBroadcastWorker schedules the nightly pass.
type FreshBroadcastWorker struct {
	db    *sql.DB
	redis *redis.Client
	run   FreshBroadcastRunFn
}

// NewFreshBroadcastWorker constructs the worker. redisClient may be nil —
// the distlock falls back to a PG advisory lock.
func NewFreshBroadcastWorker(db *sql.DB, redisClient *redis.Client, run FreshBroadcastRunFn) *FreshBroadcastWorker {
	return &FreshBroadcastWorker{db: db, redis: redisClient, run: run}
}

// Start runs the daily loop until ctx is cancelled. Call via `go w.Start(ctx)`.
func (w *FreshBroadcastWorker) Start(ctx context.Context) {
	if os.Getenv("DISABLE_FRESH_BROADCAST_RUNNER") == "true" {
		log.Printf("[FreshBroadcastWorker] disabled via DISABLE_FRESH_BROADCAST_RUNNER")
		return
	}
	if w.db == nil || w.run == nil {
		log.Printf("[FreshBroadcastWorker] disabled (db or run fn missing)")
		return
	}
	log.Printf("[FreshBroadcastWorker] started (daily %02d:%02d UTC; DRY always, LIVE only for auto_stage streams; no boot pass)",
		freshBroadcastAnchorHour, freshBroadcastAnchorMin)

	for {
		timer := time.NewTimer(w.timeUntilNextTick(time.Now().UTC()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("[FreshBroadcastWorker] stopping")
			return
		case <-timer.C:
			w.RunOnce(ctx)
		}
	}
}

// timeUntilNextTick computes the sleep until the next 09:00 UTC.
func (w *FreshBroadcastWorker) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(),
		freshBroadcastAnchorHour, freshBroadcastAnchorMin, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// anyAutoStageStream reports whether any enabled stream opted into the
// nightly LIVE stage. COALESCE keeps the probe safe if the column migration
// has not landed yet (fails closed to DRY-only).
func (w *FreshBroadcastWorker) anyAutoStageStream(ctx context.Context) bool {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var exists bool
	err := w.db.QueryRowContext(qctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_stream_broadcast_config
			WHERE enabled AND COALESCE(auto_stage, FALSE)
		)`).Scan(&exists)
	if err != nil {
		log.Printf("[FreshBroadcastWorker] auto_stage probe failed (staying DRY-only): %v", err)
		return false
	}
	return exists
}

// RunOnce executes one nightly pass under the distributed lock. Exported for
// tests and operational tooling.
func (w *FreshBroadcastWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, freshBroadcastLockKey, 30*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[FreshBroadcastWorker] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[FreshBroadcastWorker] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[FreshBroadcastWorker] lock release error: %v", err)
		}
	}()

	start := time.Now()
	hbStatus, hbErr := "ok", ""
	processed, failed := 0, 0
	defer func() {
		EmitHeartbeat(ctx, w.db, freshBroadcastWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if failed > 0 {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, freshBroadcastWorkerName, start, runStatus,
			processed, failed, "nightly fresh-broadcast pass (DRY always; LIVE for auto_stage streams)")
	}()

	// "" = today in America/Denver (the runner resolves it).
	const date = ""

	// 1. DRY always — the fill-vs-cap summary is the operator's data-ask.
	if err := w.run(ctx, date, true, false, "worker"); err != nil {
		hbStatus, hbErr = "error", err.Error()
		failed++
		log.Printf("[FreshBroadcastWorker] DRY pass failed: %v", err)
		return
	}
	processed++

	// 2. LIVE only when a stream opted in via auto_stage.
	if w.anyAutoStageStream(ctx) {
		if err := w.run(ctx, date, false, true, "worker"); err != nil {
			hbStatus, hbErr = "error", err.Error()
			failed++
			log.Printf("[FreshBroadcastWorker] LIVE (auto_stage) pass failed: %v", err)
			return
		}
		processed++
	}
	log.Printf("[FreshBroadcastWorker] pass complete in %s (%d run(s))",
		time.Since(start).Round(time.Millisecond), processed)
}

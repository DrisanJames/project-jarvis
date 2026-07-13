package worker

// PartnerHumanRollupWorker (REQ-035/038) — nightly per-dataset HUMAN
// engagement rollup into partner_dataset_human_rollup.
//
// WHY: every headline "Engaged" number on the Data Partners screen derives
// from raw clicks (scanner-contaminated 2x-85x depending on ISP mix — G3
// finding, 2026-07-13), and the live verdict-filtered query runs 40s+, over
// the 30s handler statement budget. So the verdict-filtered truth is computed
// here, off the request path, and the screen reads the rollup table.
//
// QUERY SHAPE (load-bearing): campaign-first enumeration — the dataset's
// campaign ids come from mailing_campaigns.partner_dataset_id + the
// [partner-drip] name family, and events join to that small set. The naive
// cohort JOIN mailing_message_log shape timed out at 580s in G3 research
// (planner misestimates a 105M-row join); campaign-first ran 24s. NEVER
// revert to the cohort-first join.
//
// Semantics per (dataset, UTC day) row:
//   - msgs_sent            — 'sent' tracking events from the dataset's OWN
//                            drip campaigns to its OWN partner_clean_queue
//                            subscribers (both joins: shared-vertical waves
//                            carry other datasets' members, so campaign-only
//                            scoping would over-attribute).
//   - human_openers/human_clickers — daily distinct subscribers with a
//                            verdict-HUMAN open/click (canonical predicate
//                            family: ignite_verdict_is_human(
//                            ignite_event_verdict(user_agent, ip_address)),
//                            clicks via the write-materialized click_verdict
//                            column first — same idiom as
//                            handlers_offer_alignment.go).
//   - engaged_tier_members — point-in-time activation snapshot: pcq
//                            subscribers ∩ members of the behavior-only
//                            '7D Openers'/'30D Clickers' segments (all 29
//                            carry list_id IS NULL — verified 2026-07-13).
//                            Stamped only for the most recent day; historical
//                            membership is not reconstructable, so re-runs
//                            never overwrite older snapshots.
//
// Schedule: boot pass (after a settle delay) + daily at 03:10 UTC (after the
// 02:00 SDS graduation job). Each pass gap-fills the trailing 90 days in
// per-dataset weekly chunks (coverage-driven, so the first boot performs the
// REQ-038 backfill and a failed chunk simply resumes on the next pass) and
// always recomputes the trailing 7 days for late events. Every chunk runs in
// its own transaction with SET LOCAL statement_timeout='120s' (the pool
// default is 30s) plus a client-side context timeout. One dataset failing is
// logged and skipped — per-dataset isolation.
//
// Single-writer: distlock (Redis preferred, PG advisory fallback) so only one
// ECS task computes per pass. Heartbeat + run summary land in
// mailing_worker_heartbeats / mailing_worker_runs (the SegmentMaterializer's
// invisibility is the anti-pattern this deliberately avoids).
//
// Kill switch: DISABLE_PARTNER_HUMAN_ROLLUP=true disables the worker.

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	partnerHumanRollupWorkerName = "partner_human_rollup"
	partnerHumanRollupLockKey    = "partner-human-rollup"

	// partnerHumanRollupBackfillDays is the REQ-038 coverage window: every
	// pass ensures rollup rows exist for the trailing N days.
	partnerHumanRollupBackfillDays = 90

	// partnerHumanRollupChunkPause is the breath between heavy chunks so this
	// analytics worker cannot monopolise the PRIMARY that the send path shares
	// (2026-07-13 outage: back-to-back chunks starved the wave scheduler).
	partnerHumanRollupChunkPause = 5 * time.Second
	// partnerHumanRollupTrailingDays is always recomputed (late events /
	// late verdict backfills), regardless of coverage.
	partnerHumanRollupTrailingDays = 7
	// partnerHumanRollupChunkDays is the per-statement day span. One
	// dataset × one week stays comfortably inside the raised 120s budget
	// (the all-dataset 30d verdict query measured 40.3s in G3).
	partnerHumanRollupChunkDays = 7
)

// partnerHumanRollupUpsertSQL computes one dataset × [from,to) day-range and
// upserts one row per day (zero-activity days included, so row presence is
// the backfill's resume marker). ON CONFLICT deliberately does NOT touch
// engaged_tier_members: that column is a snapshot owned by
// partnerHumanRollupEngagedTierSQL.
// $1 = dataset_id, $2 = from (date, inclusive), $3 = to (date, exclusive).
const partnerHumanRollupUpsertSQL = `
	WITH subs AS (
		SELECT DISTINCT subscriber_id
		FROM partner_clean_queue
		WHERE dataset_id = $1 AND subscriber_id IS NOT NULL
	),
	camps AS (
		SELECT id
		FROM mailing_campaigns
		WHERE partner_dataset_id = $1
		  AND name LIKE '[partner-drip]%'
	),
	agg AS (
		SELECT (te.event_at AT TIME ZONE 'UTC')::date AS day,
		       COUNT(*) FILTER (WHERE te.event_type = 'sent') AS msgs_sent,
		       COUNT(DISTINCT te.subscriber_id) FILTER (
		           WHERE te.event_type = 'opened'
		             AND ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))
		       ) AS human_openers,
		       COUNT(DISTINCT te.subscriber_id) FILTER (
		           WHERE te.event_type = 'clicked'
		             AND ignite_verdict_is_human(COALESCE(te.click_verdict, ignite_event_verdict(te.user_agent, te.ip_address)))
		       ) AS human_clickers
		FROM mailing_tracking_events te
		JOIN subs s  ON s.subscriber_id = te.subscriber_id
		JOIN camps c ON c.id = te.campaign_id
		WHERE te.event_type IN ('sent','opened','clicked')
		  AND te.event_at >= ($2::date)::timestamp AT TIME ZONE 'UTC'
		  AND te.event_at <  ($3::date)::timestamp AT TIME ZONE 'UTC'
		GROUP BY 1
	)
	INSERT INTO partner_dataset_human_rollup
		(dataset_id, day, msgs_sent, human_openers, human_clickers, computed_at)
	SELECT $1, d.day::date,
	       COALESCE(a.msgs_sent, 0), COALESCE(a.human_openers, 0), COALESCE(a.human_clickers, 0), NOW()
	FROM generate_series($2::date, ($3::date - 1), interval '1 day') AS d(day)
	LEFT JOIN agg a ON a.day = d.day::date
	ON CONFLICT (dataset_id, day) DO UPDATE SET
		msgs_sent      = EXCLUDED.msgs_sent,
		human_openers  = EXCLUDED.human_openers,
		human_clickers = EXCLUDED.human_clickers,
		computed_at    = NOW()`

// partnerHumanRollupEngagedTierSQL stamps the activation snapshot on the most
// recent day's row. $1 = dataset_id, $2 = day (yesterday, UTC).
const partnerHumanRollupEngagedTierSQL = `
	INSERT INTO partner_dataset_human_rollup
		(dataset_id, day, engaged_tier_members, computed_at)
	SELECT $1, $2::date, COALESCE((
		SELECT COUNT(DISTINCT q.subscriber_id)
		FROM partner_clean_queue q
		JOIN mailing_segment_members sm ON sm.subscriber_id = q.subscriber_id
		JOIN mailing_segments s ON s.id = sm.segment_id
		WHERE q.dataset_id = $1
		  AND q.subscriber_id IS NOT NULL
		  AND s.name ~ '(7D Openers|30D Clickers)'
	), 0), NOW()
	ON CONFLICT (dataset_id, day) DO UPDATE SET
		engaged_tier_members = EXCLUDED.engaged_tier_members,
		computed_at          = NOW()`

// partnerHumanRollupCoverageSQL counts existing rows for a chunk range so a
// fully-covered historical chunk is skipped (the REQ-038 resume mechanism).
const partnerHumanRollupCoverageSQL = `
	SELECT COUNT(*)
	FROM partner_dataset_human_rollup
	WHERE dataset_id = $1 AND day >= $2::date AND day < $3::date`

// PartnerHumanRollupWorker owns the nightly rollup pass.
type PartnerHumanRollupWorker struct {
	db    *sql.DB
	redis *redis.Client

	backfillDays int
	trailingDays int
	chunkDays    int
}

// NewPartnerHumanRollupWorker constructs the worker. redisClient may be nil —
// the distlock falls back to a PG advisory lock (same contract as siblings).
func NewPartnerHumanRollupWorker(db *sql.DB, redisClient *redis.Client) *PartnerHumanRollupWorker {
	return &PartnerHumanRollupWorker{
		db:           db,
		redis:        redisClient,
		backfillDays: partnerHumanRollupBackfillDays,
		trailingDays: partnerHumanRollupTrailingDays,
		chunkDays:    partnerHumanRollupChunkDays,
	}
}

// WithWindows overrides the coverage/recompute/chunk windows. Tests use this
// to drive a single-chunk pass; must be called before Start.
func (w *PartnerHumanRollupWorker) WithWindows(backfillDays, trailingDays, chunkDays int) *PartnerHumanRollupWorker {
	if backfillDays > 0 {
		w.backfillDays = backfillDays
	}
	if trailingDays > 0 {
		w.trailingDays = trailingDays
	}
	if chunkDays > 0 {
		w.chunkDays = chunkDays
	}
	return w
}

// Start runs a boot pass after a settle delay (this is where the REQ-038
// 90-day backfill happens, chunked and resumable — it never blocks startup
// because it runs in this goroutine), then a daily pass at 03:10 UTC.
// Runs until ctx is cancelled. Call via `go w.Start(ctx)`.
func (w *PartnerHumanRollupWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[PartnerHumanRollup] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_PARTNER_HUMAN_ROLLUP") == "true" {
		log.Printf("[PartnerHumanRollup] disabled via DISABLE_PARTNER_HUMAN_ROLLUP")
		return
	}
	log.Printf("[PartnerHumanRollup] started (backfill=%dd, trailing=%dd, chunk=%dd, daily 03:10 UTC)",
		w.backfillDays, w.trailingDays, w.chunkDays)

	// NO BOOT PASS during send hours. This worker shares the PRIMARY DB with
	// the send path, and its chunks run with statement_timeout raised to 120s.
	//
	// Verified outage 2026-07-13: the boot pass fired 3 minutes after deploy
	// (16:0x Denver, peak sending), ran ~7 concurrent 190s verdict queries, and
	// starved PMTAWaveScheduler's "fetch due waves" query into its own
	// statement timeout — waves stopped dispatching and SENDING STOPPED for
	// ~7 minutes until the queries were cancelled. An analytics backfill must
	// never contend with the send path.
	//
	// The pass now runs ONLY in the calm window (03:10 UTC = 21:10 Denver,
	// after the day's sends), which is also where the 90d backfill chunks
	// through. ROLLUP_BOOT_PASS=true forces a boot pass for a deliberate
	// operator-supervised catch-up.
	if os.Getenv("ROLLUP_BOOT_PASS") == "true" {
		log.Printf("[PartnerHumanRollup] ROLLUP_BOOT_PASS=true — running a boot pass NOW (operator-forced; contends with the send path)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		w.RunOnce(ctx)
	} else {
		log.Printf("[PartnerHumanRollup] no boot pass (send-path safety) — first pass at the 03:10 UTC calm window")
	}

	for {
		timer := time.NewTimer(w.timeUntilNextTick(time.Now().UTC()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("[PartnerHumanRollup] stopping")
			return
		case <-timer.C:
			w.RunOnce(ctx)
		}
	}
}

// timeUntilNextTick computes the sleep until the next 03:10 UTC.
func (w *PartnerHumanRollupWorker) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 3, 10, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// RunOnce executes one full pass under the distributed lock. Exported so
// tests and operational tooling can trigger a pass directly.
func (w *PartnerHumanRollupWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, partnerHumanRollupLockKey, 30*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[PartnerHumanRollup] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[PartnerHumanRollup] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[PartnerHumanRollup] lock release error: %v", err)
		}
	}()

	start := time.Now()
	chunksDone, datasetsFailed := 0, 0
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, partnerHumanRollupWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if datasetsFailed > 0 {
			runStatus = "partial"
		}
		if hbStatus == "error" {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, partnerHumanRollupWorkerName, start, runStatus,
			chunksDone, datasetsFailed, "per-dataset human rollup chunks")
	}()

	datasets, err := w.listDatasets(ctx)
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[PartnerHumanRollup] list datasets: %v", err)
		return
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, id := range datasets {
		if ctx.Err() != nil {
			return
		}
		n, err := w.processDataset(ctx, id, today)
		chunksDone += n
		if err != nil {
			// Per-dataset isolation: one dataset's failure never stops the
			// pass; the coverage check resumes its missing chunks next pass.
			datasetsFailed++
			log.Printf("[PartnerHumanRollup] dataset %s failed after %d chunks: %v", id, n, err)
			continue
		}
	}
	log.Printf("[PartnerHumanRollup] pass complete: %d datasets, %d chunks, %d failed, %s",
		len(datasets), chunksDone, datasetsFailed, time.Since(start).Round(time.Millisecond))
}

func (w *PartnerHumanRollupWorker) listDatasets(ctx context.Context) ([]string, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `SELECT id FROM partner_datasets ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// processDataset gap-fills [today-backfillDays, today) in chunkDays steps and
// recomputes any chunk touching the trailing window, then stamps yesterday's
// engaged-tier snapshot. Returns the number of chunks computed. First error
// aborts THIS dataset only (caller continues with the next).
func (w *PartnerHumanRollupWorker) processDataset(ctx context.Context, datasetID string, today time.Time) (int, error) {
	chunks := 0
	trailingFrom := today.AddDate(0, 0, -w.trailingDays)
	for cs := today.AddDate(0, 0, -w.backfillDays); cs.Before(today); cs = cs.AddDate(0, 0, w.chunkDays) {
		if err := ctx.Err(); err != nil {
			return chunks, err
		}
		ce := cs.AddDate(0, 0, w.chunkDays)
		if ce.After(today) {
			ce = today
		}
		// Historical chunk fully covered → already backfilled, skip.
		// Chunks intersecting the trailing window always recompute.
		if ce.Before(trailingFrom) || ce.Equal(trailingFrom) {
			covered, err := w.chunkCovered(ctx, datasetID, cs, ce)
			if err != nil {
				return chunks, err
			}
			if covered {
				continue
			}
		}
		if err := w.computeChunk(ctx, datasetID, cs, ce); err != nil {
			return chunks, err
		}
		chunks++

		// Pace the chunks. Each one is a heavy verdict scan holding a 120s
		// statement_timeout against the PRIMARY, which the send path shares.
		// Back-to-back chunks across every dataset is what starved
		// PMTAWaveScheduler on 2026-07-13 (sends stopped). A breath between
		// chunks lets the send path's short queries through — this worker is
		// never latency-sensitive (it fills yesterday's rollup once a night).
		select {
		case <-ctx.Done():
			return chunks, ctx.Err()
		case <-time.After(partnerHumanRollupChunkPause):
		}
	}

	// Activation snapshot for yesterday (the freshest complete day).
	yesterday := today.AddDate(0, 0, -1)
	if err := w.stampEngagedTier(ctx, datasetID, yesterday); err != nil {
		return chunks, err
	}
	return chunks, nil
}

func (w *PartnerHumanRollupWorker) chunkCovered(ctx context.Context, datasetID string, from, to time.Time) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var have int
	if err := w.db.QueryRowContext(qctx, partnerHumanRollupCoverageSQL,
		datasetID, from.Format("2006-01-02"), to.Format("2006-01-02")).Scan(&have); err != nil {
		return false, err
	}
	want := int(to.Sub(from).Hours() / 24)
	return have >= want, nil
}

// computeChunk runs the event rollup upsert for one dataset × day-range in
// its own transaction with a raised statement_timeout (pool default 30s is
// too tight for the biggest dataset's verdict-filtered weeks).
func (w *PartnerHumanRollupWorker) computeChunk(ctx context.Context, datasetID string, from, to time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, partnerHumanRollupUpsertSQL,
		datasetID, from.Format("2006-01-02"), to.Format("2006-01-02")); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *PartnerHumanRollupWorker) stampEngagedTier(ctx context.Context, datasetID string, day time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, partnerHumanRollupEngagedTierSQL,
		datasetID, day.Format("2006-01-02")); err != nil {
		return err
	}
	return tx.Commit()
}

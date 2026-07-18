package worker

// VerifiedHumansLedgerWorker — the accretive, add-only maintainer of the
// per-brand "<brand> Verified Humans" segments (the indefinite human core of
// the engaged-first anchor; docs/JAOS/core.md §14, operator 2026-07-16).
//
// WHY A DEDICATED WORKER (not a dynamic segment):
//   * The membership condition is "took a POSITIVE HUMAN ACTION EVER — a click on
//     this brand's sends, OR a conversion" — a predicate the segment
//     condition-builder cannot express (BuildSegmentWhereClause cannot UNION a
//     tracking-event click with a mailing_offer_suppressions conversion row). So
//     the SegmentMaterializer can't build it.
//   * The ledger must be ACCRETIVE: a proven human is retained INDEFINITELY and
//     is NEVER removed — the whole point of the third anchor dimension. The
//     SegmentMaterializer's DELETE+INSERT rebuild would DROP any human whose
//     qualifying events aged out of the month-partitioned mailing_tracking_events
//     table. This worker only ever INSERTs (ON CONFLICT DO NOTHING); it has no
//     DELETE path, so a human, once ledgered, stays forever.
//
// The "<brand> Verified Humans" segment rows are seeded by runStartupMigrations
// (segment_type='static', keep_active=TRUE — the two flags that keep them out of
// BOTH the nightly SegmentMaterializer (dynamic-only) AND SegmentCleanupWorker's
// static-snapshot purge). This worker only writes their MEMBERS. The planner
// streams members straight from mailing_segment_members (pmta_campaign_planner.go
// ~1100) — segment_type is irrelevant to consumption — so static works end-to-end.
//
// MEMBERSHIP PREDICATE (POSITIVE ACTION — SIGNAL-GRADING, docs/JAOS/core.md §14,
// operator 2026-07-18; NOT the ignite_verdict_is_human gate, which omitted ~20%
// of real humans incl. converters — see verifiedHumansMemberInsertSQL):
//   click : event_type='clicked' on this brand's sends, EXCLUDING tracked-unsub
//           clicks (link_url '/track/unsubscribe%') — any real click qualifies,
//           no verdict filter (a footer unsub click is a leaving signal, not a
//           human-to-retain one; unsub-click-ring-inflation).
//   convert: a mailing_offer_suppressions row (reason ~* 'convert'), ledgered
//           globally (conversions carry no sending_domain and are not cleanly
//           brand-scopable), windowed by suppressed_at on the same coverage cursor.
//   OPENS DO NOT QUALIFY: an open is liveness/silver, not a positive human action.
//
// COVERAGE MODEL (resumable, bounded — mirrors PartnerHumanRollupWorker):
//   Per segment, mailing_verified_humans_ledger_progress.scanned_through marks the
//   event-time high-water mark already scanned. Each pass scans
//   [ COALESCE(scanned_through, today-backfillDays) - trailingReScan , today )
//   in weekly chunks, capped at maxChunksPerPass so the historical backfill spreads
//   across several calm-window nights, and advances scanned_through after each
//   chunk commits. The trailing re-scan overlap re-picks up late events / late
//   click_verdict backfills. Because every INSERT is ON CONFLICT DO NOTHING,
//   double-fire / re-run / mid-pass restart are all safe.
//
// Schedule: daily 03:40 UTC (21:40 Denver — the calm window, after the partner
// rollup's 03:10 and the SDS graduation's 02:00). NO boot pass during send hours
// (same 2026-07-13 lesson as the rollup: an analytics scan must never contend with
// the send path). VERIFIED_HUMANS_BOOT_PASS=true forces an operator-supervised
// boot catch-up. Kill switch: DISABLE_VERIFIED_HUMANS_LEDGER=true.
//
// Single-writer: distlock (Redis preferred, PG advisory fallback). Heartbeat +
// run summary land in mailing_worker_heartbeats / mailing_worker_runs (the
// SegmentMaterializer's invisibility is the anti-pattern this deliberately avoids).

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
	verifiedHumansLedgerWorkerName = "verified_humans_ledger"
	verifiedHumansLedgerLockKey    = "verified-humans-ledger"

	// verifiedHumansBackfillDays is how far back the first (uncovered) scan
	// reaches. mailing_tracking_events partitions begin 2026-01; a generous
	// window means the initial ledger captures the full retained history. Empty
	// chunks are cheap; the cap below spreads the work over multiple nights.
	verifiedHumansBackfillDays = 240

	// verifiedHumansTrailingReScanDays is re-scanned every pass even once caught
	// up, to fold in late events and late click_verdict trigger/backfill writes.
	verifiedHumansTrailingReScanDays = 10

	// verifiedHumansChunkDays is the per-statement event-time span. One brand ×
	// one week of the verdict-filtered scan stays inside the raised 120s budget.
	verifiedHumansChunkDays = 7

	// verifiedHumansMaxChunksPerPass bounds how many chunks a single brand
	// advances per nightly pass, so a cold-start backfill (≈34 weekly chunks)
	// completes over a handful of calm nights instead of monopolising one.
	// Steady state, a caught-up brand does exactly one trailing chunk.
	verifiedHumansMaxChunksPerPass = 10

	// verifiedHumansChunkPause is the breath between heavy chunks so this
	// analytics scan cannot starve the send path that shares the PRIMARY (the
	// 2026-07-13 wave-scheduler starvation lesson).
	verifiedHumansChunkPause = 5 * time.Second
)

// verifiedHumansMemberInsertSQL ledgers every distinct subscriber who took a
// POSITIVE HUMAN ACTION in [from,to) into the segment's membership. ADD-ONLY:
// ON CONFLICT DO NOTHING, no DELETE anywhere.
//
// SIGNAL-GRADING doctrine (docs/JAOS/core.md §14, operator 2026-07-18): the
// scanner verdict is a scanner-STORM filter, NOT a human detector — gating this
// ledger on ignite_verdict_is_human omitted ~20% of real humans, including proven
// converters whose SafeLinks/proxy/egress clicks verdict as machine. So the
// verdict gate is DROPPED entirely and replaced by two positive-action inlets:
//
//   (a) a CLICK on this brand's sends — event_type='clicked' on the brand's
//       sending_domain, EXCLUDING tracked-unsub clicks (link_url
//       '/track/unsubscribe%'): a footer unsub click is a leaving signal, not a
//       human-to-retain one (unsub-click-ring-inflation). NO verdict filter — any
//       real click is a positive human action.
//   (b) a CONVERSION — a mailing_offer_suppressions row (reason ~* 'convert').
//       Conversions carry NO sending_domain, and offers fan out across brands, so
//       a conversion is NOT cleanly brand-scopable; per operator direction we
//       ledger EVER-CONVERTED GLOBALLY (any brand's ledger admits a subscriber who
//       converted in-window). Windowed by suppressed_at on the same [from,to)
//       coverage cursor as (a) so the chunked/resumable/backfill model is preserved.
//
// OPENS NO LONGER QUALIFY: an open is liveness/silver, not a positive human
// action, so it never by itself ledgers a subscriber into the HUMAN core.
//
// email comes from mailing_subscribers (mailing_tracking_events has no email
// column). The te scan rides idx_tracking_events_segment_v2
// (event_type, event_at, sending_domain, subscriber_id); the conversion branch
// scans mailing_offer_suppressions by suppressed_at + reason (no dedicated index
// today — conversion volume is low; an idx on (reason, suppressed_at) is a
// possible follow-on if the chunk ever approaches budget).
// $1 = segment_id, $2 = from (timestamptz), $3 = to (timestamptz), $4 = sending domain apex.
const verifiedHumansMemberInsertSQL = `
	INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
	SELECT DISTINCT $1::uuid, q.subscriber_id, s.email, NOW()
	FROM (
		-- (a) clicked this brand's sends (any real click, not a tracked-unsub click)
		SELECT te.subscriber_id
		FROM mailing_tracking_events te
		WHERE te.event_type = 'clicked'
		  AND te.subscriber_id IS NOT NULL
		  AND te.event_at >= $2 AND te.event_at < $3
		  AND (te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)
		  AND (te.link_url IS NULL OR te.link_url NOT ILIKE '%/track/unsubscribe%')
		UNION
		-- (b) converted (global — conversions are not cleanly brand-scopable)
		SELECT os.subscriber_id
		FROM mailing_offer_suppressions os
		WHERE os.subscriber_id IS NOT NULL
		  AND os.reason ~* 'convert'
		  AND os.suppressed_at >= $2 AND os.suppressed_at < $3
	) q
	JOIN mailing_subscribers s ON s.id = q.subscriber_id
	WHERE s.email IS NOT NULL AND s.email <> ''
	ON CONFLICT (segment_id, subscriber_id) DO NOTHING`

// verifiedHumansProgressUpsertSQL advances the per-segment coverage high-water
// mark and records the running member total after a chunk commits.
// $1 = segment_id, $2 = sending domain, $3 = scanned_through, $4 = added this chunk.
const verifiedHumansProgressUpsertSQL = `
	INSERT INTO mailing_verified_humans_ledger_progress
		(segment_id, sending_domain, scanned_through, last_added, total_members, updated_at)
	VALUES ($1::uuid, $2, $3, $4,
		(SELECT COUNT(*) FROM mailing_segment_members WHERE segment_id = $1::uuid), NOW())
	ON CONFLICT (segment_id) DO UPDATE SET
		sending_domain  = EXCLUDED.sending_domain,
		scanned_through = EXCLUDED.scanned_through,
		last_added      = EXCLUDED.last_added,
		total_members   = EXCLUDED.total_members,
		updated_at      = NOW()`

// verifiedHumansSegment is one brand's ledger target.
type verifiedHumansSegment struct {
	segmentID      string
	name           string
	sendingDomain  string
	scannedThrough sql.NullTime
}

// VerifiedHumansLedgerWorker owns the nightly accretive ledger pass.
type VerifiedHumansLedgerWorker struct {
	db    *sql.DB
	redis *redis.Client

	backfillDays     int
	trailingReScan   int
	chunkDays        int
	maxChunksPerPass int
}

// NewVerifiedHumansLedgerWorker constructs the worker. redisClient may be nil —
// the distlock falls back to a PG advisory lock (same contract as siblings).
func NewVerifiedHumansLedgerWorker(db *sql.DB, redisClient *redis.Client) *VerifiedHumansLedgerWorker {
	return &VerifiedHumansLedgerWorker{
		db:               db,
		redis:            redisClient,
		backfillDays:     verifiedHumansBackfillDays,
		trailingReScan:   verifiedHumansTrailingReScanDays,
		chunkDays:        verifiedHumansChunkDays,
		maxChunksPerPass: verifiedHumansMaxChunksPerPass,
	}
}

// WithWindows overrides the coverage/chunk windows. Tests use this to drive a
// single-chunk pass; must be called before Start.
func (w *VerifiedHumansLedgerWorker) WithWindows(backfillDays, trailingReScan, chunkDays, maxChunksPerPass int) *VerifiedHumansLedgerWorker {
	if backfillDays > 0 {
		w.backfillDays = backfillDays
	}
	if trailingReScan > 0 {
		w.trailingReScan = trailingReScan
	}
	if chunkDays > 0 {
		w.chunkDays = chunkDays
	}
	if maxChunksPerPass > 0 {
		w.maxChunksPerPass = maxChunksPerPass
	}
	return w
}

// Start runs a daily pass at 03:40 UTC (calm window). No send-hours boot pass
// unless VERIFIED_HUMANS_BOOT_PASS=true. Runs until ctx is cancelled; call via
// `go w.Start(ctx)`.
func (w *VerifiedHumansLedgerWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[VerifiedHumansLedger] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_VERIFIED_HUMANS_LEDGER") == "true" {
		log.Printf("[VerifiedHumansLedger] disabled via DISABLE_VERIFIED_HUMANS_LEDGER")
		return
	}
	log.Printf("[VerifiedHumansLedger] started (backfill=%dd, trailing=%dd, chunk=%dd, maxChunks/pass=%d, daily 03:40 UTC)",
		w.backfillDays, w.trailingReScan, w.chunkDays, w.maxChunksPerPass)

	if os.Getenv("VERIFIED_HUMANS_BOOT_PASS") == "true" {
		log.Printf("[VerifiedHumansLedger] VERIFIED_HUMANS_BOOT_PASS=true — running a boot pass NOW (operator-forced; contends with the send path)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		w.RunOnce(ctx)
	} else {
		log.Printf("[VerifiedHumansLedger] no boot pass (send-path safety) — first pass at the 03:40 UTC calm window")
	}

	for {
		timer := time.NewTimer(w.timeUntilNextTick(time.Now().UTC()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("[VerifiedHumansLedger] stopping")
			return
		case <-timer.C:
			w.RunOnce(ctx)
		}
	}
}

// timeUntilNextTick computes the sleep until the next 03:40 UTC.
func (w *VerifiedHumansLedgerWorker) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 3, 40, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// RunOnce executes one full pass under the distributed lock. Exported so tests
// and operational tooling can trigger a pass directly.
func (w *VerifiedHumansLedgerWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, verifiedHumansLedgerLockKey, 30*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[VerifiedHumansLedger] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[VerifiedHumansLedger] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[VerifiedHumansLedger] lock release error: %v", err)
		}
	}()

	start := time.Now()
	chunksDone, segmentsFailed := 0, 0
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, verifiedHumansLedgerWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if segmentsFailed > 0 {
			runStatus = "partial"
		}
		if hbStatus == "error" {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, verifiedHumansLedgerWorkerName, start, runStatus,
			chunksDone, segmentsFailed, "per-brand verified-humans ledger chunks")
	}()

	segments, err := w.listSegments(ctx)
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[VerifiedHumansLedger] list segments: %v", err)
		return
	}
	if len(segments) == 0 {
		log.Printf("[VerifiedHumansLedger] no 'Verified Humans' segments seeded yet — nothing to do")
		return
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, seg := range segments {
		if ctx.Err() != nil {
			return
		}
		n, err := w.processSegment(ctx, seg, today)
		chunksDone += n
		if err != nil {
			// Per-segment isolation: one brand's failure never stops the pass;
			// its scanned_through simply didn't advance, so it resumes next pass.
			segmentsFailed++
			log.Printf("[VerifiedHumansLedger] segment %q (%s) failed after %d chunks: %v",
				seg.name, shortID(seg.segmentID), n, err)
			continue
		}
	}
	log.Printf("[VerifiedHumansLedger] pass complete: %d segments, %d chunks, %d failed, %s",
		len(segments), chunksDone, segmentsFailed, time.Since(start).Round(time.Millisecond))
}

// listSegments enumerates the seeded per-brand ledger segments and each one's
// sending domain (from its stored conditions) plus its coverage high-water mark.
// It reads the segment rows from the DB (never invents names) so a brand added
// or renamed upstream is picked up automatically on the next pass.
func (w *VerifiedHumansLedgerWorker) listSegments(ctx context.Context) ([]verifiedHumansSegment, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.db.QueryContext(qctx, `
		SELECT s.id::text,
		       s.name,
		       (SELECT e.value->>'value'
		          FROM jsonb_array_elements(s.conditions) e
		         WHERE e.value->>'field' = 'sending_domain'
		         LIMIT 1) AS sending_domain,
		       p.scanned_through
		FROM mailing_segments s
		LEFT JOIN mailing_verified_humans_ledger_progress p ON p.segment_id = s.id
		WHERE s.name LIKE '%Verified Humans'
		  AND s.segment_type = 'static'
		  AND s.status = 'active'
		ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verifiedHumansSegment
	for rows.Next() {
		var seg verifiedHumansSegment
		var domain sql.NullString
		if err := rows.Scan(&seg.segmentID, &seg.name, &domain, &seg.scannedThrough); err != nil {
			return nil, err
		}
		if !domain.Valid || domain.String == "" {
			// A ledger segment with no sending_domain in its conditions cannot
			// be scoped; skip it loudly rather than scanning cross-brand.
			log.Printf("[VerifiedHumansLedger] segment %q has no sending_domain condition — skipping", seg.name)
			continue
		}
		seg.sendingDomain = domain.String
		out = append(out, seg)
	}
	return out, rows.Err()
}

// processSegment advances one brand's ledger from its coverage high-water mark
// (minus the trailing re-scan overlap) up to today, in weekly chunks capped at
// maxChunksPerPass. Returns the number of chunks computed. First error aborts
// THIS segment only (caller continues with the next).
func (w *VerifiedHumansLedgerWorker) processSegment(ctx context.Context, seg verifiedHumansSegment, today time.Time) (int, error) {
	var windowStart time.Time
	if seg.scannedThrough.Valid {
		windowStart = seg.scannedThrough.Time.AddDate(0, 0, -w.trailingReScan)
	} else {
		windowStart = today.AddDate(0, 0, -w.backfillDays)
	}
	windowStart = windowStart.Truncate(24 * time.Hour)

	chunks := 0
	for cs := windowStart; cs.Before(today) && chunks < w.maxChunksPerPass; cs = cs.AddDate(0, 0, w.chunkDays) {
		if err := ctx.Err(); err != nil {
			return chunks, err
		}
		ce := cs.AddDate(0, 0, w.chunkDays)
		if ce.After(today) {
			ce = today
		}
		added, err := w.computeChunk(ctx, seg, cs, ce)
		if err != nil {
			return chunks, err
		}
		chunks++
		// Advance the high-water mark to this chunk's end so a partial pass /
		// restart resumes forward from here.
		if err := w.recordProgress(ctx, seg, ce, added); err != nil {
			return chunks, err
		}
		// Pace the chunks so this scan never starves the send path on the
		// shared PRIMARY (2026-07-13 lesson).
		select {
		case <-ctx.Done():
			return chunks, ctx.Err()
		case <-time.After(verifiedHumansChunkPause):
		}
	}
	return chunks, nil
}

// computeChunk runs the accretive member insert for one segment × day-range in
// its own transaction with a raised statement_timeout. Returns rows added.
func (w *VerifiedHumansLedgerWorker) computeChunk(ctx context.Context, seg verifiedHumansSegment, from, to time.Time) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(cctx, verifiedHumansMemberInsertSQL,
		seg.segmentID, from, to, seg.sendingDomain)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	added, _ := res.RowsAffected()
	return added, nil
}

// recordProgress advances the coverage high-water mark and refreshes the
// segment's cached subscriber_count so the picker (audience.match / ListSegments
// cached fallback) shows the true ledger size. Both writes are best-effort on
// the count side (the member rows are the source of truth) but the progress
// upsert is load-bearing for resume, so its error propagates.
func (w *VerifiedHumansLedgerWorker) recordProgress(ctx context.Context, seg verifiedHumansSegment, scannedThrough time.Time, added int64) error {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := w.db.ExecContext(pctx, verifiedHumansProgressUpsertSQL,
		seg.segmentID, seg.sendingDomain, scannedThrough, added); err != nil {
		return err
	}
	// Cache-count refresh is non-fatal: mirror SegmentRefreshWorker's idiom.
	if _, err := w.db.ExecContext(pctx, `
		UPDATE mailing_segments
		SET subscriber_count   = (SELECT COUNT(*) FROM mailing_segment_members WHERE segment_id = $1::uuid),
		    last_calculated_at = NOW(),
		    updated_at         = NOW()
		WHERE id = $1::uuid`, seg.segmentID); err != nil {
		log.Printf("[VerifiedHumansLedger] subscriber_count refresh for %s failed (non-fatal): %v",
			shortID(seg.segmentID), err)
	}
	return nil
}

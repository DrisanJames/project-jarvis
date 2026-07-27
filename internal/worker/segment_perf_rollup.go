package worker

// SegmentPerfRollupWorker — nightly per-segment performance rollup into
// mailing_segment_perf_daily, read by GET /api/mailing/segmentation/health
// (the Segmentation Command screen's performance columns).
//
// WHY A ROLLUP (design choice, stated per the coordinator brief): the live
// join is mailing_segment_members (~47M rows) × mailing_tracking_events
// (partitioned, huge) — unservable on the request path even cached; the
// 30s handler budget cannot hold it. One nightly pass per Denver day writes
// small per-(segment, day) rows; the API then answers any 7d/30d window with
// an indexed SUM. The rollup's daily `members` column is also the only place
// member REMOVALS become computable (removed rows vanish from
// mailing_segment_members, so day-over-day snapshots are the record).
//
// Semantics per (segment_id, Denver day) row — ATTRIBUTION HONESTY: counts
// are MEMBER-SCOPED, not send-attributed: distinct CURRENT members (at
// compute time) with the event that day, from ANY campaign. The API labels
// this on-screen (perf_method) — never presented as per-campaign attribution.
//   delivered/opens        — raw distinct members (opens are RAW: machine
//                            included, no verdict claims — METRIC_CONTRACT §1)
//   clicks_action          — the canonical ACTION-click predicate mirrored
//                            from agents/dbknowledge/_db.py PG_CLICK_ACTION
//                            (asset fetches and unsub/pref/compliance links
//                            are NOT clicks; ~93% of raw clicked events are
//                            asset noise). Raw unique, NO verdict gating
//                            (signal-grading doctrine 2026-07-21).
//   complaints/unsubs      — event_type IN ('complained','complaint') /
//                            'unsubscribed' (the audience_cadence idiom).
//   hard/soft              — bounced split by bounce_type, NEVER combined
//                            (bounce doctrine).
//   members/added          — snapshot of current membership + rows
//                            materialized that Denver day; stamped for the
//                            CURRENT day only (history is not
//                            reconstructable). Full rebuilds re-stamp
//                            materialized_at, so `added` over-counts on
//                            rebuild days; the API's derived `removed`
//                            over-counts equally and the net stays true —
//                            labeled on-screen.
//
// Schedule: daily 04:10 UTC (~22:10 Denver — after the send day, an hour
// after partner_human_rollup's old slot so heavy analytics never stack).
// NO boot pass by default (2026-07-13 lesson: an analytics boot pass starved
// PMTAWaveScheduler and stopped sending); SEGMENT_PERF_BOOT_PASS=true forces
// one for an operator-supervised catch-up. Each pass gap-fills the trailing
// segmentPerfBackfillDays Denver days (row presence = resume marker) and
// always recomputes the trailing segmentPerfTrailingDays for late events.
// Per-day statement runs in its own tx with SET LOCAL statement_timeout
// (the day join hash-scans members once — minutes, not request-path work).
//
// Single-writer: distlock (Redis preferred, PG advisory fallback). Heartbeat
// name 'segment_perf_rollup' + RecordWorkerRun land in the same tables the
// Segmentation Command workers panel reads — this worker is visible on the
// screen it feeds. Kill switch: DISABLE_SEGMENT_PERF_ROLLUP=true.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	segmentPerfWorkerName = "segment_perf_rollup"
	segmentPerfLockKey    = "segment-perf-rollup"

	// segmentPerfBackfillDays covers the API's 30d window + the 14d churn
	// sparkline with margin.
	segmentPerfBackfillDays = 35
	// segmentPerfTrailingDays are always recomputed (late events).
	segmentPerfTrailingDays = 2
	// segmentPerfChunkPause is the breath between per-day statements so the
	// pass never monopolises the shared PRIMARY (2026-07-13 lesson).
	segmentPerfChunkPause = 5 * time.Second
	// segmentPerfStatementTimeout raises the per-day tx budget above the 30s
	// pool default; the day join hash-scans the 47M-row members table.
	segmentPerfStatementTimeout = "600s"
)

// segmentPerfClickAction mirrors agents/dbknowledge/_db.py PG_CLICK_ACTION
// exactly (C_CLICK_ACTION doctrine 2026-07-19): a click qualifies only when
// it is navigational — not empty/unsub/pref/compliance/synthetic/tracker-self
// (NONNAV) and not an asset/CDN fetch (ASSET). Keep in sync with _db.py.
const segmentPerfClickAction = `(e.event_type = 'clicked'
	AND NOT (COALESCE(e.link_url,'') = ''
		OR e.link_url ~* 'unsub|optout|opt-out|preference|/privacy'
		OR e.link_url ~* '^everflow-import:'
		OR e.link_url ~* '^https?://t\.em\.')
	AND NOT (e.link_url ~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)'
		OR e.link_url ~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)'))`

// segmentPerfDaySQL computes one Denver day for every non-archived segment in
// ONE pass: the day's event slice joined to current membership, grouped by
// segment. ON CONFLICT touches only the event columns — members/added are
// owned by segmentPerfMembersSQL.
// $1 = day (date), $2 = day start (timestamptz), $3 = day end (timestamptz).
var segmentPerfDaySQL = fmt.Sprintf(`
	INSERT INTO mailing_segment_perf_daily
		(segment_id, day, delivered, opens, clicks_action, complaints, unsubs, hard, soft)
	SELECT m.segment_id, $1::date,
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type = 'delivered'),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type = 'opened'),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE %s),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type IN ('complained','complaint')),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type = 'unsubscribed'),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type = 'bounced' AND e.bounce_type = 'hard'),
	       COUNT(DISTINCT e.subscriber_id) FILTER (WHERE e.event_type = 'bounced' AND e.bounce_type = 'soft')
	FROM mailing_tracking_events e
	JOIN mailing_segment_members m ON m.subscriber_id = e.subscriber_id
	JOIN mailing_segments s ON s.id = m.segment_id AND s.archived_at IS NULL
	WHERE e.event_at >= $2 AND e.event_at < $3
	  AND e.subscriber_id IS NOT NULL
	GROUP BY m.segment_id
	ON CONFLICT (segment_id, day) DO UPDATE SET
		delivered     = EXCLUDED.delivered,
		opens         = EXCLUDED.opens,
		clicks_action = EXCLUDED.clicks_action,
		complaints    = EXCLUDED.complaints,
		unsubs        = EXCLUDED.unsubs,
		hard          = EXCLUDED.hard,
		soft          = EXCLUDED.soft,
		computed_at   = NOW()
`, segmentPerfClickAction)

// segmentPerfMembersSQL stamps the CURRENT membership snapshot (+ rows
// materialized today) onto the current Denver day. One full members scan per
// pass. ON CONFLICT touches only members/added.
// $1 = day (date), $2 = day start (timestamptz), $3 = day end (timestamptz).
const segmentPerfMembersSQL = `
	INSERT INTO mailing_segment_perf_daily (segment_id, day, members, added)
	SELECT m.segment_id, $1::date,
	       COUNT(*),
	       COUNT(*) FILTER (WHERE m.materialized_at >= $2 AND m.materialized_at < $3)
	FROM mailing_segment_members m
	JOIN mailing_segments s ON s.id = m.segment_id AND s.archived_at IS NULL
	GROUP BY m.segment_id
	ON CONFLICT (segment_id, day) DO UPDATE SET
		members     = EXCLUDED.members,
		added       = EXCLUDED.added,
		computed_at = NOW()
`

// SegmentPerfRollupWorker computes the nightly rollup.
type SegmentPerfRollupWorker struct {
	db    *sql.DB
	redis *redis.Client

	backfillDays int
	trailingDays int
	chunkPause   time.Duration
	loc          *time.Location
}

func NewSegmentPerfRollupWorker(db *sql.DB, redisClient *redis.Client) *SegmentPerfRollupWorker {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		// UTC fallback keeps the worker alive; day bucketing shifts but stays
		// consistent (METRIC_CONTRACT prefers Denver — log loudly).
		log.Printf("[SegmentPerfRollup] America/Denver tz unavailable (%v) — falling back to UTC day buckets", err)
		loc = time.UTC
	}
	return &SegmentPerfRollupWorker{
		db:           db,
		redis:        redisClient,
		backfillDays: segmentPerfBackfillDays,
		trailingDays: segmentPerfTrailingDays,
		chunkPause:   segmentPerfChunkPause,
		loc:          loc,
	}
}

// denverDayBounds returns [start, end) timestamps of the Denver calendar day
// containing t, plus the day date in that location. Pure; unit-tested.
func denverDayBounds(t time.Time, loc *time.Location) (day time.Time, start time.Time, end time.Time) {
	lt := t.In(loc)
	day = time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	return day, day, day.Add(24 * time.Hour)
}

// Start runs daily at 04:10 UTC until ctx is cancelled. Call via `go w.Start(ctx)`.
func (w *SegmentPerfRollupWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[SegmentPerfRollup] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_SEGMENT_PERF_ROLLUP") == "true" {
		log.Printf("[SegmentPerfRollup] disabled via DISABLE_SEGMENT_PERF_ROLLUP")
		return
	}
	log.Printf("[SegmentPerfRollup] started (backfill=%dd, trailing=%dd, daily 04:10 UTC)",
		w.backfillDays, w.trailingDays)

	// No boot pass during send hours (2026-07-13 partner-rollup outage class);
	// SEGMENT_PERF_BOOT_PASS=true forces a supervised catch-up.
	if os.Getenv("SEGMENT_PERF_BOOT_PASS") == "true" {
		log.Printf("[SegmentPerfRollup] SEGMENT_PERF_BOOT_PASS=true — boot pass in 3m (operator-forced; contends with the send path)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		w.RunOnce(ctx)
	}

	for {
		timer := time.NewTimer(w.timeUntilNextTick(time.Now().UTC()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("[SegmentPerfRollup] stopping")
			return
		case <-timer.C:
			w.RunOnce(ctx)
		}
	}
}

// timeUntilNextTick computes the sleep until the next 04:10 UTC.
func (w *SegmentPerfRollupWorker) timeUntilNextTick(now time.Time) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 4, 10, 0, 0, time.UTC)
	if !now.Before(target) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// RunOnce executes one full pass under the distributed lock. Exported for
// tests and operational tooling. Re-run safe: every statement is an
// idempotent upsert keyed (segment_id, day); a double-fire recomputes the
// same rows.
func (w *SegmentPerfRollupWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, segmentPerfLockKey, 60*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[SegmentPerfRollup] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[SegmentPerfRollup] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[SegmentPerfRollup] lock release error: %v", err)
		}
	}()

	start := time.Now()
	daysDone, daysFailed := 0, 0
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, segmentPerfWorkerName, int((24 * time.Hour).Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if daysFailed > 0 {
			runStatus = "partial"
		}
		if hbStatus == "error" {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, segmentPerfWorkerName, start, runStatus,
			daysDone, daysFailed, "per-segment daily perf rollup (Denver days)")
	}()

	days, err := w.daysNeedingCompute(ctx)
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[SegmentPerfRollup] coverage scan: %v", err)
		return
	}
	for _, day := range days {
		if ctx.Err() != nil {
			return
		}
		if err := w.computeDay(ctx, day); err != nil {
			daysFailed++
			log.Printf("[SegmentPerfRollup] day %s: %v", day.Format("2006-01-02"), err)
			continue
		}
		daysDone++
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.chunkPause):
		}
	}

	// Membership snapshot for TODAY (current Denver day) — one members scan.
	today, tStart, tEnd := denverDayBounds(time.Now(), w.loc)
	if err := w.execBounded(ctx, segmentPerfMembersSQL, today, tStart, tEnd); err != nil {
		daysFailed++
		hbStatus, hbErr = "error", "members snapshot: "+err.Error()
		log.Printf("[SegmentPerfRollup] members snapshot: %v", err)
		return
	}
	log.Printf("[SegmentPerfRollup] pass complete: %d day(s) computed, %d failed, in %s",
		daysDone, daysFailed, time.Since(start).Round(time.Second))
}

// daysNeedingCompute returns the Denver days to (re)compute this pass:
// every day in the trailing backfill window with NO rollup row yet (gap
// fill / first-boot backfill, oldest first) plus the trailing
// segmentPerfTrailingDays (late events), deduped. Today is excluded from the
// event compute (partial day) — it gets its members snapshot only.
func (w *SegmentPerfRollupWorker) daysNeedingCompute(ctx context.Context) ([]time.Time, error) {
	today, _, _ := denverDayBounds(time.Now(), w.loc)
	candidates := make([]time.Time, 0, w.backfillDays)
	for i := w.backfillDays; i >= 1; i-- { // oldest first; excludes today (i>=1)
		candidates = append(candidates, today.AddDate(0, 0, -i))
	}

	covered := make(map[string]bool)
	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT day FROM mailing_segment_perf_daily
		WHERE day >= $1::date
	`, candidates[0].Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		covered[d.Format("2006-01-02")] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]time.Time, 0, 8)
	for _, d := range candidates {
		daysAgo := int(today.Sub(d).Hours() / 24)
		if !covered[d.Format("2006-01-02")] || daysAgo <= w.trailingDays {
			out = append(out, d)
		}
	}
	return out, nil
}

// computeDay upserts the event aggregates for one Denver day.
func (w *SegmentPerfRollupWorker) computeDay(ctx context.Context, day time.Time) error {
	_, start, end := denverDayBounds(day.Add(12*time.Hour), w.loc) // midday anchor avoids DST edges
	return w.execBounded(ctx, segmentPerfDaySQL, day, start, end)
}

// execBounded runs one rollup statement in its own tx with the raised
// statement_timeout, under a client-side context bound.
func (w *SegmentPerfRollupWorker) execBounded(ctx context.Context, query string, day, start, end time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '"+segmentPerfStatementTimeout+"'"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, query, day.Format("2006-01-02"), start, end); err != nil {
		return err
	}
	return tx.Commit()
}

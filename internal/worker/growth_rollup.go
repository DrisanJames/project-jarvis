package worker

// GrowthRollupWorker — the daily send-program growth fact table.
//
// One row per (Denver day × sending-domain apex × ISP) in mailing_growth_daily,
// read by GET /api/mailing/growth/daily (the Reporting → Growth screen). It is
// the platform's answer to the growth spreadsheet the operator kept by hand:
// arbitrary historical ranges, filterable by sending domain and ISP, with the
// day-over-day deltas computed from ONE consistent set of numbers.
//
// WHY A ROLLUP (not a live query): the two halves live in two stores and
// neither is request-path servable over a multi-month range. Delivery truth is
// the Athena lake (1–3s and a scanned-bytes bill PER query — an operator
// scrubbing across months would re-scan the same partitions all day), and
// engagement is mailing_tracking_events (millions of rows/day, monthly
// partitions). Rolling both up once per day makes any range an indexed SUM
// over ~25 domains × ~12 ISPs × N days.
//
// SEMANTICS per row — the sources are deliberately split, and the API labels
// which is which; do not mix them:
//
//	delivered / hard / soft / reputation_block / complaints
//	    — the LAKE (analytics.GrowthDelivery), COUNT(DISTINCT event_uid),
//	      real transports only (pmta/ses/kumo — 'app' is the PG mirror and
//	      double-counts). Bounce taxonomy is the reader's read-time
//	      eventTypeExpr, so an operator `pmta flush` (administrative) and
//	      reputation blocks are counted in NEITHER hard nor soft.
//	open_events / click_events
//	    — PG mailing_tracking_events, COUNT(*) of raw events. These are the
//	      numerators of the operator's open/click RATE columns, which is why
//	      an open rate can exceed 100% (Jul 17 microsoft: 146.93%) — a
//	      scanner opens one message many times. Machine traffic INCLUDED and
//	      unlabelled-as-human (METRIC_CONTRACT §1; signal-grading doctrine).
//	open_subs / click_subs
//	    — the same slices as COUNT(DISTINCT subscriber_id), so the screen can
//	      switch between "how much scanning" and "how many mailboxes".
//	action_click_subs
//	    — the GOVERNING click cohort (C_CLICK_ACTION, 2026-07-19 ruling):
//	      navigational/commercial links only; asset fetches and
//	      unsub/pref/compliance links are NOT clicks (~93% of raw clicked
//	      events are asset noise). Mirrors segmentPerfClickAction.
//	unsubs
//	    — PG, distinct subscribers. Computed by a SEPARATE query because
//	      'unsubscribed' events carry NO recipient_domain, so their ISP must
//	      come from the subscriber's own address via a join (the same footgun
//	      agents/reporting/sending_domain_report_card.py documents).
//
// Rates are NEVER stored — the API derives them, so one denominator change
// cannot leave stale percentages in the table.
//
// ATTRIBUTION HONESTY: lake rows before 2026-07-02 often carry no resolvable
// brand (blank brand + no VMTA on SES rows), so their volume lands under
// sending_domain '' and the API renders it as "(unattributed)" instead of
// vanishing. Global (all-domain) totals are correct for the whole lake
// history; per-domain history is only exact from 2026-07-02.
//
// Schedule: every 2h, and each pass (a) always recomputes the trailing
// growthTrailingDays Denver days so late events and the in-flight current day
// land, then (b) gap-fills up to growthBackfillChunks unwritten chunks of
// older history, oldest first, until the whole lake window is covered. Chunked
// (not per-day) because one Athena query over a 7-day range costs the same
// scan as one over a single day. NO boot pass by default (2026-07-13 lesson:
// an analytics boot pass starved PMTAWaveScheduler and stopped sending);
// GROWTH_ROLLUP_BOOT_PASS=true forces one.
//
// Single-writer: distlock (Redis preferred, PG advisory fallback). Re-run
// safe: every write is an upsert keyed (day, sending_domain, isp), so a
// double-fire recomputes identical rows. Kill switch: DISABLE_GROWTH_ROLLUP=true.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	growthWorkerName = "growth_rollup"
	growthLockKey    = "growth-rollup"

	// growthTickInterval is how often a pass runs. 2h keeps the current day
	// fresh on screen at a bounded Athena cost (2 range queries per pass).
	growthTickInterval = 2 * time.Hour
	// growthTrailingDays are always recomputed each pass: today (partial) plus
	// enough history for late-arriving lake rows and postback lag.
	growthTrailingDays = 3
	// growthChunkDays is the size of one compute chunk. One Athena range query
	// costs the same partition scan as a single day, so chunk wide; the PG
	// side is the binding constraint (a 7-day tracking-events scan).
	growthChunkDays = 7
	// growthBackfillChunks caps how much OLD history one pass fills, so the
	// first-boot backfill converges over a few passes instead of monopolising
	// the shared primary in one go.
	growthBackfillChunks = 2
	// growthLakeEpoch is the first Denver day with lake coverage (verified
	// 2026-07-29: earliest ignite_analytics.email_events day = 2026-04-06).
	// Nothing before it is computable, so the backfill floor stops here.
	growthLakeEpoch = "2026-04-06"
	// growthStatementTimeout raises the per-chunk tx budget over the 30s pool
	// default; the PG chunk scans the monthly tracking-event partitions.
	growthStatementTimeout = "600s"
	// growthChunkPause is the breath between chunks so a pass never
	// monopolises the shared primary (2026-07-13 lesson).
	growthChunkPause = 5 * time.Second
)

// growthISPCase renders the canonical clean-ISP CASE over a recipient-domain
// expression — byte-identical buckets to analytics.ispExpr (lake side),
// api.alignmentISPCase and ISP_CASE_PG in agents/dbknowledge/_db.py, so PG
// engagement rows land in the same buckets as lake delivery rows.
func growthISPCase(domainExpr string) string {
	d := "lower(" + domainExpr + ")"
	return "CASE" +
		" WHEN " + d + " IN ('outlook.com','hotmail.com','live.com','msn.com','hotmail.co.uk','windowslive.com','passport.com','outlook.co.uk') THEN 'microsoft'" +
		" WHEN " + d + " IN ('gmail.com','googlemail.com') THEN 'gmail'" +
		" WHEN " + d + " IN ('yahoo.com','ymail.com','rocketmail.com','yahoo.co.uk','yahoo.ca') THEN 'yahoo'" +
		" WHEN " + d + " IN ('icloud.com','me.com','mac.com') THEN 'apple'" +
		" WHEN " + d + " = 'comcast.net' THEN 'comcast'" +
		" WHEN " + d + " = 'aol.com' THEN 'aol'" +
		" WHEN " + d + " = 'att.net' THEN 'att'" +
		" WHEN " + d + " = 'sbcglobal.net' THEN 'sbcglobal'" +
		" WHEN " + d + " = 'cox.net' THEN 'cox'" +
		" WHEN " + d + " IN ('charter.net','spectrum.net') THEN 'charter'" +
		" WHEN " + d + " = 'verizon.net' THEN 'verizon'" +
		" ELSE 'other' END"
}

// growthApexSQL strips a leading em./m. from a sending_domain so PG rows key on
// the same apex the lake's `brand` column already carries. Mirrors
// api.normalizeSendingDomain.
const growthApexSQL = `regexp_replace(lower(sending_domain), '^(em|m)\.', '')`

// growthClickAction mirrors segmentPerfClickAction / _db.py PG_CLICK_ACTION
// exactly (C_CLICK_ACTION doctrine 2026-07-19). Keep the three in sync.
const growthClickAction = `(event_type = 'clicked'
	AND NOT (COALESCE(link_url,'') = ''
		OR link_url ~* 'unsub|optout|opt-out|preference|/privacy'
		OR link_url ~* '^everflow-import:'
		OR link_url ~* '^https?://t\.em\.')
	AND NOT (link_url ~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)'
		OR link_url ~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)'))`

// growthEngagementSQL upserts the PG engagement half for one chunk.
// $1 = chunk start (timestamptz, Denver midnight), $2 = chunk end (exclusive).
// The event_at bounds stay RAW so the planner prunes the monthly partitions;
// the Denver day is derived in the projection only.
var growthEngagementSQL = fmt.Sprintf(`
	INSERT INTO mailing_growth_daily
		(day, sending_domain, isp, open_events, open_subs, click_events, click_subs, action_click_subs)
	SELECT (event_at AT TIME ZONE 'America/Denver')::date,
	       %s,
	       %s,
	       COUNT(*) FILTER (WHERE event_type = 'opened'),
	       COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened'),
	       COUNT(*) FILTER (WHERE event_type = 'clicked'),
	       COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'clicked'),
	       COUNT(DISTINCT subscriber_id) FILTER (WHERE %s)
	FROM mailing_tracking_events
	WHERE event_at >= $1 AND event_at < $2
	  AND event_type IN ('opened','clicked')
	  AND COALESCE(sending_domain,'') <> ''
	GROUP BY 1, 2, 3
	ON CONFLICT (day, sending_domain, isp) DO UPDATE SET
		open_events       = EXCLUDED.open_events,
		open_subs         = EXCLUDED.open_subs,
		click_events      = EXCLUDED.click_events,
		click_subs        = EXCLUDED.click_subs,
		action_click_subs = EXCLUDED.action_click_subs,
		computed_at       = NOW()
`, growthApexSQL, growthISPCase("recipient_domain"), growthClickAction)

// growthUnsubSQL upserts the unsubscribe half for one chunk. Separate because
// 'unsubscribed' rows carry NO recipient_domain — the ISP comes from the
// subscriber's own address. Small join (hundreds of rows/day platform-wide).
// $1 = chunk start, $2 = chunk end (exclusive).
var growthUnsubSQL = fmt.Sprintf(`
	INSERT INTO mailing_growth_daily (day, sending_domain, isp, unsubs)
	SELECT (e.event_at AT TIME ZONE 'America/Denver')::date,
	       %s,
	       %s,
	       COUNT(DISTINCT e.subscriber_id)
	FROM mailing_tracking_events e
	JOIN mailing_subscribers s ON s.id = e.subscriber_id
	WHERE e.event_at >= $1 AND e.event_at < $2
	  AND e.event_type = 'unsubscribed'
	  AND COALESCE(e.sending_domain,'') <> ''
	GROUP BY 1, 2, 3
	ON CONFLICT (day, sending_domain, isp) DO UPDATE SET
		unsubs      = EXCLUDED.unsubs,
		computed_at = NOW()
`,
	strings.ReplaceAll(growthApexSQL, "sending_domain", "e.sending_domain"),
	growthISPCase("split_part(s.email, '@', 2)"))

// growthDeliverySQL upserts ONE lake bucket. Delivery counts are owned by the
// lake half; ON CONFLICT touches only those columns so a re-run of the
// engagement half never clobbers them (and vice versa).
const growthDeliverySQL = `
	INSERT INTO mailing_growth_daily
		(day, sending_domain, isp, delivered, hard_bounce, soft_bounce, reputation_block, complaints)
	VALUES ($1::date, $2, $3, $4, $5, $6, $7, $8)
	ON CONFLICT (day, sending_domain, isp) DO UPDATE SET
		delivered        = EXCLUDED.delivered,
		hard_bounce      = EXCLUDED.hard_bounce,
		soft_bounce      = EXCLUDED.soft_bounce,
		reputation_block = EXCLUDED.reputation_block,
		complaints       = EXCLUDED.complaints,
		computed_at      = NOW()
`

// growthZeroDeliverySQL clears the delivery half for a chunk before the lake
// rows are written back, so a day whose volume moved between domains does not
// keep a stale bucket. Engagement columns are untouched.
const growthZeroDeliverySQL = `
	UPDATE mailing_growth_daily
	   SET delivered = 0, hard_bounce = 0, soft_bounce = 0,
	       reputation_block = 0, complaints = 0
	 WHERE day >= $1::date AND day <= $2::date
`

// GrowthRollupWorker computes the growth fact table.
type GrowthRollupWorker struct {
	db    *sql.DB
	redis *redis.Client

	tick          time.Duration
	trailingDays  int
	chunkDays     int
	maxBackfill   int
	chunkPause    time.Duration
	epoch         string
	loc           *time.Location
	lakeAvailable func() bool

	// coveredOverride is the DB seam for coveredDays — nil in production
	// (the real query runs); set by tests so the chunk-selection policy is
	// exercised without a database.
	coveredOverride func() map[string]bool
}

// NewGrowthRollupWorker builds the worker. The lake availability probe is a
// field so tests can drive the disabled path.
func NewGrowthRollupWorker(db *sql.DB, redisClient *redis.Client) *GrowthRollupWorker {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[GrowthRollup] America/Denver tz unavailable (%v) — falling back to UTC day buckets", err)
		loc = time.UTC
	}
	return &GrowthRollupWorker{
		db:            db,
		redis:         redisClient,
		tick:          growthTickInterval,
		trailingDays:  growthTrailingDays,
		chunkDays:     growthChunkDays,
		maxBackfill:   growthBackfillChunks,
		chunkPause:    growthChunkPause,
		epoch:         growthLakeEpoch,
		loc:           loc,
		lakeAvailable: analytics.ReaderEnabled,
	}
}

// Start runs a pass every growthTickInterval until ctx is cancelled.
// Call via `go w.Start(ctx)`.
func (w *GrowthRollupWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[GrowthRollup] disabled (db missing)")
		return
	}
	if os.Getenv("DISABLE_GROWTH_ROLLUP") == "true" {
		log.Printf("[GrowthRollup] disabled via DISABLE_GROWTH_ROLLUP")
		return
	}
	log.Printf("[GrowthRollup] started (every %s, trailing=%dd, chunk=%dd, backfill<=%d chunk(s)/pass, epoch=%s)",
		w.tick, w.trailingDays, w.chunkDays, w.maxBackfill, w.epoch)

	if os.Getenv("GROWTH_ROLLUP_BOOT_PASS") == "true" {
		log.Printf("[GrowthRollup] GROWTH_ROLLUP_BOOT_PASS=true — boot pass in 3m (operator-forced; contends with the send path)")
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Minute):
		}
		w.RunOnce(ctx)
	}

	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[GrowthRollup] stopping")
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// chunkRange is an inclusive Denver-day window [From, To].
type chunkRange struct{ From, To time.Time }

// RunOnce executes one pass under the distributed lock. Exported for tests and
// operational tooling.
func (w *GrowthRollupWorker) RunOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	lock := distlock.NewLock(w.redis, w.db, growthLockKey, 60*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[GrowthRollup] lock acquire error: %v", err)
		return
	}
	if !acquired {
		log.Printf("[GrowthRollup] another instance holds the lock — skipping pass")
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[GrowthRollup] lock release error: %v", err)
		}
	}()

	start := time.Now()
	done, failed := 0, 0
	hbStatus, hbErr := "ok", ""
	defer func() {
		EmitHeartbeat(ctx, w.db, growthWorkerName, int(2*w.tick.Seconds()), hbStatus, hbErr)
		runStatus := "ok"
		if failed > 0 {
			runStatus = "partial"
		}
		if hbStatus == "error" {
			runStatus = "failed"
		}
		RecordWorkerRun(ctx, w.db, growthWorkerName, start, runStatus,
			done, failed, "daily growth rollup (day × sending domain × ISP)")
	}()

	chunks, err := w.chunksToCompute(ctx, time.Now())
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("[GrowthRollup] coverage scan: %v", err)
		return
	}
	for _, c := range chunks {
		if ctx.Err() != nil {
			return
		}
		if err := w.computeChunk(ctx, c); err != nil {
			failed++
			log.Printf("[GrowthRollup] chunk %s..%s: %v",
				c.From.Format("2006-01-02"), c.To.Format("2006-01-02"), err)
		} else {
			done++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.chunkPause):
		}
	}
	if failed > 0 {
		hbStatus = "error"
		hbErr = fmt.Sprintf("%d chunk(s) failed", failed)
	}
	log.Printf("[GrowthRollup] pass complete: %d chunk(s) computed, %d failed, in %s",
		done, failed, time.Since(start).Round(time.Second))
}

// chunksToCompute returns the trailing window first (always recomputed), then
// up to maxBackfill older chunks that have NO rows yet, newest-first so the
// screen fills backwards from the days the operator looks at most.
func (w *GrowthRollupWorker) chunksToCompute(ctx context.Context, now time.Time) ([]chunkRange, error) {
	today, _, _ := denverDayBounds(now, w.loc)
	epoch, err := time.ParseInLocation("2006-01-02", w.epoch, w.loc)
	if err != nil {
		return nil, err
	}

	out := []chunkRange{{From: today.AddDate(0, 0, -(w.trailingDays - 1)), To: today}}
	trailingFloor := out[0].From

	covered, err := w.coveredDays(ctx, epoch, trailingFloor.AddDate(0, 0, -1))
	if err != nil {
		return nil, err
	}

	// Walk backwards from the day before the trailing window in chunkDays
	// steps; take a chunk when ANY day in it is missing.
	for to := trailingFloor.AddDate(0, 0, -1); !to.Before(epoch) && len(out) <= w.maxBackfill; to = to.AddDate(0, 0, -w.chunkDays) {
		from := to.AddDate(0, 0, -(w.chunkDays - 1))
		if from.Before(epoch) {
			from = epoch
		}
		missing := false
		for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
			if !covered[d.Format("2006-01-02")] {
				missing = true
				break
			}
		}
		if missing {
			out = append(out, chunkRange{From: from, To: to})
		}
	}
	return out, nil
}

// coveredDays returns the set of Denver days that already have at least one
// growth row, over [from, to].
func (w *GrowthRollupWorker) coveredDays(ctx context.Context, from, to time.Time) (map[string]bool, error) {
	covered := map[string]bool{}
	if w.coveredOverride != nil {
		return w.coveredOverride(), nil
	}
	if to.Before(from) {
		return covered, nil
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT DISTINCT day FROM mailing_growth_daily
		WHERE day >= $1::date AND day <= $2::date
	`, from.Format("2006-01-02"), to.Format("2006-01-02"))
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
	return covered, rows.Err()
}

// computeChunk writes both halves for one inclusive Denver-day window. The PG
// half runs first (it needs no external service); a lake failure therefore
// still leaves engagement rows rather than an empty screen.
func (w *GrowthRollupWorker) computeChunk(ctx context.Context, c chunkRange) error {
	start := c.From
	end := c.To.AddDate(0, 0, 1) // exclusive
	if err := w.execBounded(ctx, growthEngagementSQL, start, end); err != nil {
		return fmt.Errorf("engagement: %w", err)
	}
	if err := w.execBounded(ctx, growthUnsubSQL, start, end); err != nil {
		return fmt.Errorf("unsubs: %w", err)
	}
	if err := w.writeDelivery(ctx, c); err != nil {
		return fmt.Errorf("delivery: %w", err)
	}
	return nil
}

// writeDelivery pulls the lake aggregate for the chunk and upserts it. A
// disabled lake reader is not an error — the engagement half still lands and
// the API labels delivery as unavailable.
func (w *GrowthRollupWorker) writeDelivery(ctx context.Context, c chunkRange) error {
	if w.lakeAvailable != nil && !w.lakeAvailable() {
		log.Printf("[GrowthRollup] lake reader disabled — delivery half skipped for %s..%s",
			c.From.Format("2006-01-02"), c.To.Format("2006-01-02"))
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	rows, err := analytics.GrowthDelivery(qctx,
		c.From.Format("2006-01-02"), c.To.Format("2006-01-02"))
	if err != nil {
		if analytics.IsDisabledErr(err) {
			return nil
		}
		return err
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+growthStatementTimeout+"'"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, growthZeroDeliverySQL,
		c.From.Format("2006-01-02"), c.To.Format("2006-01-02")); err != nil {
		return err
	}
	for _, r := range rows {
		if r.Day == "" || r.ISP == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, growthDeliverySQL,
			r.Day, r.Brand, r.ISP,
			r.Delivered, r.HardBounce, r.SoftBounce, r.ReputationBlock, r.Complaints); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// execBounded runs one chunk statement in its own tx with the raised
// statement_timeout, under a client-side context bound.
func (w *GrowthRollupWorker) execBounded(ctx context.Context, query string, start, end time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	tx, err := w.db.BeginTx(cctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after Commit
	if _, err := tx.ExecContext(cctx, "SET LOCAL statement_timeout = '"+growthStatementTimeout+"'"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, query, start, end); err != nil {
		return err
	}
	return tx.Commit()
}

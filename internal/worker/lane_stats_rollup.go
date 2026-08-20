package worker

// LaneStatsRollupWorker — the daily materialization behind
// GET …/property-ledger/lane-stats.
//
// ⚠️ ADDITIVE ONLY. This worker UPSERTS and NEVER DELETES. It issues no
// DELETE, no TRUNCATE, no DROP against any table, and it writes to exactly one
// table (mailing_lane_stats_daily) that nothing else owns. That is the
// operator's standing constraint on this whole body of work ("additive only,
// no major writes or deletes"), and it is pinned by
// TestLaneStatsRollupUpsertIssuesNoDelete.
//
// WHY IT EXISTS
// -------------
// The endpoint day-chunks the ledger query under a 25s budget and reports the
// days it could not finish as missing_days + partial:true. Measured on prod, a
// cold 7-day pass is ~230-255s with ~10x run-to-run variance (RDS I/O
// contention, not plan cost), so a UI poller CONVERGES over three polls instead
// of answering. This worker computes the same cells off the request path, under
// a per-day statement budget the request path cannot afford, and persists them.
// The endpoint then reads the table and falls back to the live path per day.
//
// SQL PROVENANCE — internal/api/property_lane_stats.go IS THE SOURCE OF TRUTH.
// laneStatsRollupCampaignSQL / laneStatsRollupDaySQL below are COPIES of
// laneStatsCampaignSQL / laneStatsDaySQL. They are copied, not imported,
// because internal/api ALREADY imports internal/worker (property_ledger.go:39,
// server.go:19, server_routes_mailing.go:27, list_upload_handlers.go:13,
// creative_proof_send.go:32) and internal/worker imports internal/api nowhere —
// exporting a const from api and importing it here would create an import
// cycle. VERIFIED by grep both directions, 2026-08-19.
// If you change the counting rules, change property_lane_stats.go FIRST and
// mirror here; TestLaneStatsRollupSQLMirrorsTheEndpoint pins the shared shape.
//
// DELIBERATE DEVIATIONS from the endpoint's SQL (both are documented parity
// notes, not accidents):
//   1. The outer ORDER BY 2 DESC is dropped — this is an INSERT … SELECT and
//      ISP ordering is presentation, which the endpoint re-derives in Go from
//      whole-window volume.
//   2. When a (lane, day) produces NO cells the worker writes ONE sentinel row
//      with isp = '__none__' and all-zero counts. Without it, "computed, no
//      activity" and "never computed" are indistinguishable in the table and
//      the worker would rescan empty days forever. The sentinel is filtered out
//      on read (api: laneStatsRollupEmptyISP) and never reaches the payload.
//
// CAMPAIGN-FLOOR PARITY CAVEAT (read this before trusting a delta):
// the endpoint's campaign floor is oldest-day-in-the-REQUESTED-window minus the
// ledger's 4-day cushion, so its per-day numbers are window-dependent. (The
// endpoint's own per-day cache key omits `days`, so it already conflates
// windows — property_lane_stats.go laneStatsCacheKey.) The worker computes with
// the WIDEST floor: horizon (30d) + cushion. A rollup-served day therefore
// matches a days=30 request EXACTLY and is a superset of a narrower window's
// campaign set — it can only differ if a campaign created more than
// (window + 4) days before the day still emitted events on that day, which the
// ~72h reminder ladder makes vanishingly rare. Documented, not hidden.
//
// RE-RUN SAFETY (this re-fires on every ECS bounce)
//   * one distlock lease per tick (Redis preferred, PG advisory fallback) —
//     the sibling contract, see property_intro_rollup.go.
//   * a CLOSED day is computed ONCE: it is skipped when a row already exists
//     whose computed_at is at or after that Denver day's UTC end instant. A row
//     written while the day was still open does NOT count — it is partial.
//   * TODAY is refreshed on the tick cadence, and skipped if a row younger than
//     laneStatsRollupTodayMinAge exists, so three bounces in ten minutes do not
//     re-scan three times.
//   * every (lane, day) scan+upsert is ONE statement in ONE tx. Kill the
//     process mid-flight and the tx rolls back: the day is simply absent, the
//     endpoint falls back to the live path, and the next tick recomputes it.
//     There is no half-written day.
//   * a day that FAILS writes nothing at all — a gap, never a zero row.
//
// Kill switch: LANE_STATS_ROLLUP_DISABLED (a disabled tick emits one heartbeat
// with status 'disabled' and runs nothing else).
// Opt-in: LANE_STATS_ROLLUP_VERTICAL_WIDE=1 also rolls up the brand-less
// (vertical-only) lane the endpoint serves when ?brand= is omitted. Default OFF
// because it doubles the scan volume on mailing_tracking_events, which is the
// I/O bottleneck this worker exists to relieve.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

// ── DDL ─────────────────────────────────────────────────────────────────────

// LaneStatsRollupDDL creates the rollup table.
//
// It is sized for runStartupMigrations, which runs each entry under a 5s
// statement_timeout on a shared connection and, on timeout, logs
// "skipped, will retry next boot" — after which the statement is silently
// absent forever. So: CREATE TABLE only. No backfill, no data movement, no
// heavy index build.
//
// ⚠️ WIRING NOTE (VERIFIED against cmd/server/migration_skip.go, 2026-08-19):
// register this and LaneStatsRollupIndexDDL as TWO SEPARATE migration entries.
// migrationSkipProbe classifies a statement by its LEADING keywords —
// reMigCreateTable is unanchored at the tail — so a single string holding
// `CREATE TABLE …; CREATE INDEX …` is classified as CREATE TABLE, and once the
// table exists the probe skips the WHOLE string and the index never lands.
//
// PK is (organization_id, vertical, brand, day, isp): org-scoped by
// construction, and its leading prefix is exactly the endpoint's read
// predicate, so the read-through is a PK range scan.
// brand ” is the vertical-wide lane (the endpoint's ?brand= omitted case) —
// stored as empty string, never NULL, so the PK never admits a NULL member.
const LaneStatsRollupDDL = `
CREATE TABLE IF NOT EXISTS mailing_lane_stats_daily (
	organization_id  UUID        NOT NULL,
	vertical         TEXT        NOT NULL,
	brand            TEXT        NOT NULL DEFAULT '',
	day              DATE        NOT NULL,
	isp              TEXT        NOT NULL,
	sent             BIGINT      NOT NULL DEFAULT 0,
	delivered_pg     BIGINT      NOT NULL DEFAULT 0,
	openers          BIGINT      NOT NULL DEFAULT 0,
	clickers         BIGINT      NOT NULL DEFAULT 0,
	human_clickers   BIGINT      NOT NULL DEFAULT 0,
	open_events      BIGINT      NOT NULL DEFAULT 0,
	click_events     BIGINT      NOT NULL DEFAULT 0,
	campaigns        INTEGER     NOT NULL DEFAULT 0,
	window_start_utc TIMESTAMPTZ NOT NULL,
	window_end_utc   TIMESTAMPTZ NOT NULL,
	computed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (organization_id, vertical, brand, day, isp)
)`

// LaneStatsRollupIndexDDL — the operator/ops sweep index (freshness probes and
// "what has the rollup got for this day" questions, which the PK cannot serve
// because day is not its leading column). Register as its OWN migration entry;
// see the wiring note on LaneStatsRollupDDL. It is instant on the empty table
// the first boot creates, and IF NOT EXISTS short-circuits on every boot after.
const LaneStatsRollupIndexDDL = `
CREATE INDEX IF NOT EXISTS idx_lane_stats_daily_day_computed
	ON mailing_lane_stats_daily (day, computed_at)`

// LaneStatsRollupDDLStatements is the wiring order: table, then index.
func LaneStatsRollupDDLStatements() []struct{ Name, SQL string } {
	return []struct{ Name, SQL string }{
		{"aug19_mailing_lane_stats_daily", LaneStatsRollupDDL},
		{"aug19_mailing_lane_stats_daily_idx", LaneStatsRollupIndexDDL},
	}
}

// ── tuning ──────────────────────────────────────────────────────────────────

const (
	laneStatsRollupWorkerName = "lane_stats_rollup"
	laneStatsRollupLockKey    = "lane_stats_rollup"

	// DefaultLaneStatsRollupInterval — today's cell is refreshed at this
	// cadence, which is the freshness the endpoint's read-through requires of
	// a rollup row before it will serve TODAY from the table.
	DefaultLaneStatsRollupInterval = 10 * time.Minute

	// laneStatsRollupHorizonDays matches laneStatsMaxDays in
	// property_lane_stats.go: the endpoint can never ask for more.
	laneStatsRollupHorizonDays = 30

	// laneStatsRollupCampaignCushionDays mirrors
	// laneStatsCampaignCushionDays / lane_performance_ledger.series().
	laneStatsRollupCampaignCushionDays = 4

	// laneStatsRollupConcurrency bounds day-scans in flight.
	// mailing_tracking_events is already the I/O bottleneck; 3 is deliberately
	// below the endpoint's own fan-out of 4 because this runs unattended.
	laneStatsRollupConcurrency = 3

	// laneStatsRollupTickBudget keeps one tick inside its lease so ticks never
	// overlap. Work not finished this tick resumes next tick — the horizon is
	// walked OLDEST-FIRST, so a cold start fills history in the background
	// without ever blocking today's refresh (today is scheduled first, see
	// buildTasks).
	laneStatsRollupTickBudget = 8 * time.Minute

	// laneStatsRollupDayTimeout is the per-day statement budget. This is the
	// whole point: the request path is capped by the prod 30s connection
	// statement_timeout and its own 25s wall-clock, and a cold day-scan does
	// not fit. Off the request path it does. (Idiom: property_intro_rollup.go
	// materializeDay / internal/domainagent/scorecard.go.)
	laneStatsRollupDayTimeout = "120s"

	// laneStatsRollupTodayMinAge: today is not re-scanned if a row younger than
	// this exists. 80% of the interval absorbs tick jitter while making a
	// bounce-storm cost at most one extra scan.
	laneStatsRollupTodayMinAgeNum = 8
	laneStatsRollupTodayMinAgeDen = 10

	// LaneStatsRollupEmptyISP marks "this (lane, day) was computed and had no
	// cells". Filtered out on read; never reaches an operator-visible payload.
	// Kept identical to laneStatsRollupEmptyISP in internal/api.
	LaneStatsRollupEmptyISP = "__none__"
)

func laneStatsRollupEnvOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func laneStatsRollupDisabled() bool  { return laneStatsRollupEnvOn("LANE_STATS_ROLLUP_DISABLED") }
func laneStatsRollupWideLanes() bool { return laneStatsRollupEnvOn("LANE_STATS_ROLLUP_VERTICAL_WIDE") }

// ── SQL (copies — see the provenance note in the file header) ───────────────

// laneStatsRollupOrgsSQL enumerates tenants. The roster table carries no
// organization_id, but every campaign read is org-scoped and organization_id is
// in the rollup PK, so the pass runs once per org.
const laneStatsRollupOrgsSQL = `SELECT id::text FROM organizations ORDER BY id`

// laneStatsRollupLanesSQL — partner_drip_vertical_roster is the LIVE binding
// between a drip vertical and a sending brand (property_lane_roster.go:5).
// active=false is the soft-disable tombstone, so it is excluded here.
const laneStatsRollupLanesSQL = `
	SELECT DISTINCT lower(btrim(vertical)), lower(btrim(brand))
	FROM partner_drip_vertical_roster
	WHERE active = TRUE
	  AND btrim(vertical) <> '' AND btrim(brand) <> ''
	ORDER BY 1, 2`

// laneStatsRollupCampaignSQL — COPY of laneStatsCampaignSQL
// (internal/api/property_lane_stats.go). created_at LEADS
// idx_campaigns_org_status_created; the anchored name regex is a residual
// Filter. $1 organization_id · $2 created_at floor · $3 anchored name regex.
const laneStatsRollupCampaignSQL = `
	SELECT id::text, created_at
	FROM mailing_campaigns
	WHERE organization_id = $1::uuid
	  AND created_at >= $2
	  AND name ~ $3
	ORDER BY created_at`

// laneStatsRollupFreshnessSQL — what the table already holds for a lane, so a
// closed day is computed ONCE. MAX(computed_at) per day; the sentinel row
// counts as computed, which is the point of writing it.
// $1 org · $2 vertical · $3 brand · $4 horizon floor day.
const laneStatsRollupFreshnessSQL = `
	SELECT to_char(day, 'YYYY-MM-DD'), MAX(computed_at)
	FROM mailing_lane_stats_daily
	WHERE organization_id = $1::uuid
	  AND vertical = $2 AND brand = $3
	  AND day >= $4::date
	GROUP BY 1`

// laneStatsRollupDaySQL — the scan+upsert, ONE statement, ONE tx.
//
// The CTEs down to `agg` are a verbatim copy of laneStatsDaySQL
// (internal/api/property_lane_stats.go); see the header for why it is copied
// and for the two deliberate deviations. Every parity note there binds here:
// PAST-TENSE event types ('opened'/'clicked' — the present tense is a silent
// zero), a param-injected event_at range bound because
// mailing_tracking_events is RANGE-partitioned, no tz-cast in any predicate
// (Denver bounds are Go-computed), the unnest JOIN rather than `= ANY`, rates
// derived from UNIQUE subscribers, and the backfill-artifact exclusion.
//
// $1 campaign ids (uuid[]) · $2 window start (UTC) · $3 window end (UTC) ·
// $4 organization_id · $5 vertical · $6 brand · $7 day · $8 campaign count.
const laneStatsRollupDaySQL = `
	WITH cmap AS (
	  SELECT unnest($1::uuid[]) AS cid
	),
	per_sub AS (
	  SELECT m.subscriber_id,
	         bool_or(m.event_type = 'sent')      AS sent,
	         bool_or(m.event_type = 'delivered') AS delivered,
	         bool_or(m.event_type = 'opened')    AS opened,
	         bool_or(m.event_type = 'clicked')   AS clicked,
	         bool_or(m.event_type = 'clicked' AND m.click_verdict = 'human') AS human,
	         COUNT(*) FILTER (WHERE m.event_type = 'opened')  AS n_open,
	         COUNT(*) FILTER (WHERE m.event_type = 'clicked') AS n_click
	  FROM cmap c
	  JOIN mailing_tracking_events m ON m.campaign_id = c.cid
	  WHERE m.event_at >= $2 AND m.event_at < $3
	    AND m.subscriber_id IS NOT NULL
	    AND NOT (COALESCE(m.bounce_reason, '') LIKE '%(undefined status)%'
	             AND m.created_at > m.event_at + INTERVAL '1 hour')
	  GROUP BY 1
	),
	q AS (
	  SELECT DISTINCT ON (subscriber_id) subscriber_id, isp_family
	  FROM partner_clean_queue
	  WHERE subscriber_id IS NOT NULL
	    AND subscriber_id IN (SELECT DISTINCT subscriber_id FROM per_sub)
	),
	agg AS (
	  SELECT COALESCE(NULLIF(q.isp_family, ''), 'other')  AS isp,
	         COUNT(*) FILTER (WHERE p.sent)               AS sent,
	         COUNT(*) FILTER (WHERE p.delivered)          AS delivered_pg,
	         COUNT(*) FILTER (WHERE p.opened)             AS openers,
	         COUNT(*) FILTER (WHERE p.clicked)            AS clickers,
	         COUNT(*) FILTER (WHERE p.human)              AS human_clickers,
	         COALESCE(SUM(p.n_open), 0)::bigint           AS open_events,
	         COALESCE(SUM(p.n_click), 0)::bigint          AS click_events
	  FROM per_sub p
	  LEFT JOIN q ON q.subscriber_id = p.subscriber_id
	  GROUP BY 1
	),
	final AS (
	  SELECT isp, sent, delivered_pg, openers, clickers, human_clickers,
	         open_events, click_events
	  FROM agg
	  UNION ALL
	  SELECT '__none__', 0::bigint, 0::bigint, 0::bigint, 0::bigint,
	         0::bigint, 0::bigint, 0::bigint
	  WHERE NOT EXISTS (SELECT 1 FROM agg)
	)
	INSERT INTO mailing_lane_stats_daily
	  (organization_id, vertical, brand, day, isp,
	   sent, delivered_pg, openers, clickers, human_clickers,
	   open_events, click_events, campaigns,
	   window_start_utc, window_end_utc, computed_at)
	SELECT $4::uuid, $5, $6, $7::date, f.isp,
	       f.sent, f.delivered_pg, f.openers, f.clickers, f.human_clickers,
	       f.open_events, f.click_events, $8::int,
	       $2, $3, NOW()
	FROM final f
	ON CONFLICT (organization_id, vertical, brand, day, isp) DO UPDATE SET
	  sent             = EXCLUDED.sent,
	  delivered_pg     = EXCLUDED.delivered_pg,
	  openers          = EXCLUDED.openers,
	  clickers         = EXCLUDED.clickers,
	  human_clickers   = EXCLUDED.human_clickers,
	  open_events      = EXCLUDED.open_events,
	  click_events     = EXCLUDED.click_events,
	  campaigns        = EXCLUDED.campaigns,
	  window_start_utc = EXCLUDED.window_start_utc,
	  window_end_utc   = EXCLUDED.window_end_utc,
	  computed_at      = NOW()`

// ── worker ──────────────────────────────────────────────────────────────────

// laneStatsRollupLane is one (organization, vertical, brand) lane. brand "" is
// the vertical-wide lane (opt-in, see LANE_STATS_ROLLUP_VERTICAL_WIDE).
type laneStatsRollupLane struct {
	OrgID    string
	Vertical string
	Brand    string
}

// laneStatsRollupTask is one (lane, Denver day) unit of work: a single tx.
type laneStatsRollupTask struct {
	lane laneStatsRollupLane
	day  time.Time
	cids []string
	n    int
}

// LaneStatsRollupWorker materializes mailing_lane_stats_daily.
// Construct via NewLaneStatsRollupWorker, then Start(ctx) once at boot.
type LaneStatsRollupWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	loc      *time.Location
	horizon  int
	conc     int
	budget   time.Duration
}

// NewLaneStatsRollupWorker wires the worker. redisClient may be nil — the
// distlock falls back to a PG advisory lock (the sibling contract,
// property_intro_rollup.go).
func NewLaneStatsRollupWorker(db *sql.DB, redisClient *redis.Client) *LaneStatsRollupWorker {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[LaneStatsRollup] America/Denver tz unavailable (%v) — falling back to UTC day buckets", err)
		loc = time.UTC
	}
	return &LaneStatsRollupWorker{
		db:       db,
		redis:    redisClient,
		interval: DefaultLaneStatsRollupInterval,
		loc:      loc,
		horizon:  laneStatsRollupHorizonDays,
		conc:     laneStatsRollupConcurrency,
		budget:   laneStatsRollupTickBudget,
	}
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *LaneStatsRollupWorker) WithInterval(d time.Duration) *LaneStatsRollupWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithHorizonDays overrides the rollup horizon (tests). Call before Start.
func (w *LaneStatsRollupWorker) WithHorizonDays(n int) *LaneStatsRollupWorker {
	if n > 0 && n <= laneStatsRollupHorizonDays {
		w.horizon = n
	}
	return w
}

// Start runs the tick loop until ctx is cancelled.
func (w *LaneStatsRollupWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[LaneStatsRollup] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Lane stats rollup worker started (interval=%s, horizon=%dd Denver oldest-first, concurrency=%d, per-day budget=%s)",
			w.interval, w.horizon, w.conc, laneStatsRollupDayTimeout)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[LaneStatsRollup] context cancelled, stopping")
				return
			}
		}
	}()
}

// tick is one leased pass.
func (w *LaneStatsRollupWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if laneStatsRollupDisabled() {
		EmitHeartbeat(ctx, w.db, laneStatsRollupWorkerName, int(w.interval.Seconds()),
			"disabled", "LANE_STATS_ROLLUP_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, laneStatsRollupLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[LaneStatsRollup] lock acquire error: %v", err)
		return
	}
	if !acquired {
		// Another task holds the lease. Not an error, and not a heartbeat —
		// the leaseholder beats for the worker.
		return
	}
	defer func() {
		if rerr := lock.Release(context.Background()); rerr != nil {
			log.Printf("[LaneStatsRollup] lock release error: %v", rerr)
		}
	}()

	tctx, cancel := context.WithTimeout(ctx, w.budget)
	defer cancel()
	w.RunOnce(tctx)
}

// RunOnce executes one pass. Exported so tests and operational tooling can
// drive a pass directly; the CALLER owns locking.
func (w *LaneStatsRollupWorker) RunOnce(ctx context.Context) {
	orgs, err := w.listOrgs(ctx)
	if err != nil {
		log.Printf("[LaneStatsRollup] org enumeration failed: %v", err)
		EmitHeartbeat(ctx, w.db, laneStatsRollupWorkerName, int(w.interval.Seconds()), "error", "orgs: "+err.Error())
		return
	}
	lanes, err := w.listLanes(ctx)
	if err != nil {
		log.Printf("[LaneStatsRollup] lane enumeration failed: %v", err)
		EmitHeartbeat(ctx, w.db, laneStatsRollupWorkerName, int(w.interval.Seconds()), "error", "lanes: "+err.Error())
		return
	}
	if len(orgs) == 0 || len(lanes) == 0 {
		// Visible, not silent: an empty roster means the endpoint stays on the
		// live path forever, which is exactly what an operator needs to know.
		log.Printf("[LaneStatsRollup] nothing to roll up (orgs=%d, active roster lanes=%d) — endpoint stays on the live path",
			len(orgs), len(lanes))
		EmitHeartbeat(ctx, w.db, laneStatsRollupWorkerName, int(w.interval.Seconds()), "ok",
			fmt.Sprintf("idle: orgs=%d lanes=%d", len(orgs), len(lanes)))
		return
	}

	tasks, planErrs := w.buildTasks(ctx, orgs, lanes)
	done, failed := w.runTasks(ctx, tasks)

	status, detail := "ok", ""
	if planErrs > 0 || failed > 0 {
		status = "error"
		detail = fmt.Sprintf("plan_errors=%d day_failures=%d (failed days are GAPS — no rows written)", planErrs, failed)
	}
	log.Printf("[LaneStatsRollup] pass complete: lanes=%d planned=%d upserted=%d failed=%d plan_errors=%d",
		len(lanes)*len(orgs), len(tasks), done, failed, planErrs)
	EmitHeartbeat(ctx, w.db, laneStatsRollupWorkerName, int(w.interval.Seconds()), status, detail)
}

func (w *LaneStatsRollupWorker) listOrgs(ctx context.Context) ([]string, error) {
	rows, err := w.db.QueryContext(ctx, laneStatsRollupOrgsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// listLanes returns the DISTINCT active (vertical, brand) bindings, plus — only
// when LANE_STATS_ROLLUP_VERTICAL_WIDE is on — one brand-less lane per vertical.
func (w *LaneStatsRollupWorker) listLanes(ctx context.Context) ([]laneStatsRollupLane, error) {
	rows, err := w.db.QueryContext(ctx, laneStatsRollupLanesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []laneStatsRollupLane{}
	verticals := map[string]bool{}
	for rows.Next() {
		var v, b string
		if err := rows.Scan(&v, &b); err != nil {
			return nil, err
		}
		if v == "" || b == "" {
			continue
		}
		out = append(out, laneStatsRollupLane{Vertical: v, Brand: b})
		verticals[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if laneStatsRollupWideLanes() {
		wide := make([]string, 0, len(verticals))
		for v := range verticals {
			wide = append(wide, v)
		}
		sort.Strings(wide)
		for _, v := range wide {
			out = append(out, laneStatsRollupLane{Vertical: v, Brand: ""})
		}
	}
	return out, nil
}

// laneStatsRollupNamePattern mirrors laneStatsNamePattern
// (internal/api/property_lane_stats.go): anchored, trailing space, so the
// vertical match is EXACT and `_v3` siblings do not bleed in.
func laneStatsRollupNamePattern(vertical, brand string) string {
	if brand != "" {
		return `^\[partner-drip\] ` + vertical + ` ` + brand + ` `
	}
	return `^\[partner-drip\] ` + vertical + ` `
}

// buildTasks resolves campaigns + freshness per (org, lane) and emits the units
// of work that are actually due. Returns the tasks and a count of lanes whose
// PLANNING failed (a planning failure skips that lane this tick — it never
// writes a partial or zeroed day).
//
// Ordering: TODAY first (the operator is watching it), then the closed days
// OLDEST-FIRST so a cold start fills history in the background and a
// budget-truncated tick always makes forward progress at the old end.
func (w *LaneStatsRollupWorker) buildTasks(ctx context.Context, orgs []string, lanes []laneStatsRollupLane) ([]laneStatsRollupTask, int) {
	now := time.Now()
	today := denverDate(now, w.loc)
	oldest := today.AddDate(0, 0, -(w.horizon - 1))
	oldestStart, _ := denverDayWindowUTC(oldest, w.loc)
	floor := oldestStart.AddDate(0, 0, -laneStatsRollupCampaignCushionDays)
	todayMinAge := w.interval * laneStatsRollupTodayMinAgeNum / laneStatsRollupTodayMinAgeDen

	tasks := []laneStatsRollupTask{}
	planErrs := 0

	for _, org := range orgs {
		for _, ln := range lanes {
			if ctx.Err() != nil {
				return tasks, planErrs
			}
			lane := laneStatsRollupLane{OrgID: org, Vertical: ln.Vertical, Brand: ln.Brand}

			campaigns, err := w.resolveCampaigns(ctx, lane, floor)
			if err != nil {
				log.Printf("[LaneStatsRollup] %s/%s/%s: campaign resolve failed: %v — lane skipped this tick",
					org, lane.Vertical, lane.Brand, err)
				planErrs++
				continue
			}
			if len(campaigns) == 0 {
				continue // no campaigns in the horizon: nothing this lane can say
			}
			fresh, err := w.laneFreshness(ctx, lane, oldest)
			if err != nil {
				log.Printf("[LaneStatsRollup] %s/%s/%s: freshness probe failed: %v — lane skipped this tick",
					org, lane.Vertical, lane.Brand, err)
				planErrs++
				continue
			}

			for i := 0; i < w.horizon; i++ {
				day := today.AddDate(0, 0, -i)
				key := day.Format("2006-01-02")
				_, dayEnd := denverDayWindowUTC(day, w.loc)
				closed := !now.Before(dayEnd)
				prev, had := fresh[key]

				if had {
					if closed {
						// Computed ONCE — but only a row written AT OR AFTER the
						// day closed is a complete day. A row written while the
						// day was still open is partial and must be redone.
						if !prev.Before(dayEnd) {
							continue
						}
					} else if now.Sub(prev) < todayMinAge {
						continue // refreshed recently enough; a bounce must not re-scan
					}
				}

				cids := make([]string, 0, len(campaigns))
				for _, c := range campaigns {
					// Exact prune: an event cannot precede its campaign row.
					if c.Created.Before(dayEnd) {
						cids = append(cids, c.ID)
					}
				}
				if len(cids) == 0 {
					continue
				}
				tasks = append(tasks, laneStatsRollupTask{lane: lane, day: day, cids: cids, n: len(cids)})
			}
		}
	}

	// today first, then oldest-first.
	sort.SliceStable(tasks, func(a, b int) bool {
		at, bt := tasks[a].day.Equal(today), tasks[b].day.Equal(today)
		if at != bt {
			return at
		}
		return tasks[a].day.Before(tasks[b].day)
	})
	return tasks, planErrs
}

// laneStatsRollupCampaign is one resolved campaign row.
type laneStatsRollupCampaign struct {
	ID      string
	Created time.Time
}

func (w *LaneStatsRollupWorker) resolveCampaigns(ctx context.Context, lane laneStatsRollupLane, floor time.Time) ([]laneStatsRollupCampaign, error) {
	rows, err := w.db.QueryContext(ctx, laneStatsRollupCampaignSQL,
		lane.OrgID, floor, laneStatsRollupNamePattern(lane.Vertical, lane.Brand))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []laneStatsRollupCampaign{}
	for rows.Next() {
		var c laneStatsRollupCampaign
		if err := rows.Scan(&c.ID, &c.Created); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// laneFreshness returns day -> MAX(computed_at) for the lane inside the horizon.
func (w *LaneStatsRollupWorker) laneFreshness(ctx context.Context, lane laneStatsRollupLane, oldest time.Time) (map[string]time.Time, error) {
	rows, err := w.db.QueryContext(ctx, laneStatsRollupFreshnessSQL,
		lane.OrgID, lane.Vertical, lane.Brand, oldest.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var day string
		var at sql.NullTime
		if err := rows.Scan(&day, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			out[day] = at.Time
		}
	}
	return out, rows.Err()
}

// runTasks drains the queue with a bounded pool. Dispatch is IN ORDER, so the
// ordering buildTasks established survives the fan-out.
func (w *LaneStatsRollupWorker) runTasks(ctx context.Context, tasks []laneStatsRollupTask) (int, int) {
	if len(tasks) == 0 {
		return 0, 0
	}
	conc := w.conc
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, failed := 0, 0

	for _, task := range tasks {
		if ctx.Err() != nil {
			break // budget spent / shutdown: the rest resumes next tick
		}
		wg.Add(1)
		go func(t laneStatsRollupTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if err := w.materializeDay(ctx, t); err != nil {
				// A failed day is a GAP: nothing was written, the endpoint
				// falls back to the live path, the next tick retries.
				log.Printf("[LaneStatsRollup] %s/%s/%s %s failed: %v (gap — no rows written)",
					t.lane.OrgID, t.lane.Vertical, t.lane.Brand, t.day.Format("2006-01-02"), err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			done++
			mu.Unlock()
		}(task)
	}
	wg.Wait()
	return done, failed
}

// materializeDay is the crash window: scan + upsert as ONE statement in ONE tx
// under a per-day statement budget. Kill the process anywhere inside it and the
// tx rolls back — the day is absent (a gap the endpoint serves live), never
// half-written and never zeroed.
func (w *LaneStatsRollupWorker) materializeDay(ctx context.Context, t laneStatsRollupTask) error {
	start, end := denverDayWindowUTC(t.day, w.loc)

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+laneStatsRollupDayTimeout+`'`); err != nil {
		return fmt.Errorf("statement_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, laneStatsRollupDaySQL,
		pq.Array(t.cids), start, end,
		t.lane.OrgID, t.lane.Vertical, t.lane.Brand, t.day.Format("2006-01-02"), t.n); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

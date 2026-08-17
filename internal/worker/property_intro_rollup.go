package worker

// PropertyIntroRollupWorker (Vector A plan rev4, Step 14) — the Property
// Ledger's counter pass. Every 10 minutes, under a distlock lease:
//
//  1. INDEX GATE (index-first, Step 8): refuses the heavy pass unless
//     idx_pcq_intro_rollup exists AND indisvalid — a CONCURRENTLY build that
//     failed leaves an invalid index that would seq-scan partner_clean_queue.
//     Blocked ticks emit heartbeat status 'blocked' and run NOTHING else.
//  2. PENDING PROMOTION (I-2): budget edits stage in pending_budget /
//     pending_effective_day (portal Step 18); the first tick on/after the
//     Denver day boundary promotes them into daily_budget. Enforcement
//     (loadBrandBudgets) and judgment therefore agree by construction; max
//     skew = one cadence (10 min) + one orchestrator cache tick.
//  3. COUNTER RUNS (I-8): for today + the prior two Denver days, one run row
//     in property_counter_runs, then a single grouped query over
//     partner_clean_queue (served by idx_pcq_intro_rollup) LEFT-JOINed onto
//     the full 16-brand × LedgerGroups() grid so EVERY cell gets a row —
//     absence of sends is a recorded 0, not a missing row. Both join sides
//     normalized (LOWER(BTRIM(...)), '' → 'other'). Go-constant UTC bounds
//     stored on every row (I-1). Own tx, statement_timeout 300s.
//  4. FINALIZE: counter rows for days older than 2 Denver days get
//     finalized_at = NOW(); the grader reads only finalized counter days.
//
// Kill switch: PROPERTY_INTRO_ROLLUP_DISABLED (disabled tick = one heartbeat
// with status 'disabled'; nothing else runs). Re-run safe: pure upserts, run
// rows are append-only, promotion is idempotent (pending IS NOT NULL guard).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	propertyIntroRollupWorkerName = "property_intro_rollup"
	propertyIntroRollupLockKey    = "property_intro_rollup"

	// DefaultPropertyIntroRollupInterval — the promotion cadence in I-2's
	// "max skew ≤ one promotion cadence" is THIS value.
	DefaultPropertyIntroRollupInterval = 10 * time.Minute

	// propertyIntroRollupRecomputeDays: today + the prior N Denver days are
	// recomputed every tick; days older than N finalize.
	propertyIntroRollupRecomputeDays = 2
)

func propertyIntroRollupDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROPERTY_INTRO_ROLLUP_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// PropertyIntroRollupWorker owns the counter pass. Construct via
// NewPropertyIntroRollupWorker, then Start(ctx) once at boot.
type PropertyIntroRollupWorker struct {
	db       *sql.DB
	redis    *redis.Client
	interval time.Duration
	loc      *time.Location
}

// NewPropertyIntroRollupWorker wires the worker. redisClient may be nil —
// the distlock falls back to a PG advisory lock (same contract as siblings).
func NewPropertyIntroRollupWorker(db *sql.DB, redisClient *redis.Client) *PropertyIntroRollupWorker {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[PropertyIntroRollup] America/Denver tz unavailable (%v) — falling back to UTC day buckets", err)
		loc = time.UTC
	}
	return &PropertyIntroRollupWorker{
		db:       db,
		redis:    redisClient,
		interval: DefaultPropertyIntroRollupInterval,
		loc:      loc,
	}
}

// WithInterval overrides the tick cadence (tests). Call before Start.
func (w *PropertyIntroRollupWorker) WithInterval(d time.Duration) *PropertyIntroRollupWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// Start runs the tick loop until ctx is cancelled.
func (w *PropertyIntroRollupWorker) Start(ctx context.Context) {
	if w.db == nil {
		log.Printf("[PropertyIntroRollup] disabled (db missing)")
		return
	}
	go func() {
		log.Printf("Property intro rollup worker started (interval=%s, recompute=today+%dd Denver, index-gated on idx_pcq_intro_rollup)",
			w.interval, propertyIntroRollupRecomputeDays)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-ctx.Done():
				log.Printf("[PropertyIntroRollup] context cancelled, stopping")
				return
			}
		}
	}()
}

// tick is one leased pass. Exported-path entry for tests is RunOnce.
func (w *PropertyIntroRollupWorker) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if propertyIntroRollupDisabled() {
		EmitHeartbeat(ctx, w.db, propertyIntroRollupWorkerName, int(w.interval.Seconds()), "disabled", "PROPERTY_INTRO_ROLLUP_DISABLED set")
		return
	}
	lock := distlock.NewLock(w.redis, w.db, propertyIntroRollupLockKey, w.interval)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[PropertyIntroRollup] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[PropertyIntroRollup] lock release error: %v", err)
		}
	}()
	w.RunOnce(ctx)
}

// RunOnce executes one full pass (gate → promotion → runs → finalize).
// Exported so tests and operational tooling can drive a pass directly; the
// caller owns locking.
func (w *PropertyIntroRollupWorker) RunOnce(ctx context.Context) {
	// (1) Index gate — refuse the heavy pass without a VALID rollup index.
	valid, err := w.rollupIndexValid(ctx)
	if err != nil || !valid {
		detail := "idx_pcq_intro_rollup missing or invalid"
		if err != nil {
			detail = fmt.Sprintf("idx_pcq_intro_rollup validity check failed: %v", err)
		}
		log.Printf("[PropertyIntroRollup] BLOCKED — %s. Refusing the heavy pass (index-first, plan Step 8/14); pending promotion also deferred this tick.", detail)
		EmitHeartbeat(ctx, w.db, propertyIntroRollupWorkerName, int(w.interval.Seconds()), "blocked", detail)
		return
	}

	today := denverDate(time.Now(), w.loc)

	// (2) Pending promotion (I-2) — idempotent, one statement.
	if n, err := w.promotePendingBudgets(ctx, today); err != nil {
		log.Printf("[PropertyIntroRollup] pending promotion failed: %v", err)
		EmitHeartbeat(ctx, w.db, propertyIntroRollupWorkerName, int(w.interval.Seconds()), "error", "promotion: "+err.Error())
		return
	} else if n > 0 {
		log.Printf("[PropertyIntroRollup] promoted %d pending budget(s) effective %s (Denver)", n, today.Format("2006-01-02"))
	}

	// (3) Counter runs: today + prior two Denver days.
	hbStatus, hbErr := "ok", ""
	for i := 0; i <= propertyIntroRollupRecomputeDays; i++ {
		day := today.AddDate(0, 0, -i)
		if err := w.runOneDay(ctx, day); err != nil {
			log.Printf("[PropertyIntroRollup] day %s failed: %v", day.Format("2006-01-02"), err)
			hbStatus, hbErr = "error", err.Error()
		}
	}

	// (4) Finalize days older than the recompute window.
	cutoff := today.AddDate(0, 0, -propertyIntroRollupRecomputeDays)
	if _, err := w.db.ExecContext(ctx, `
		UPDATE property_intro_counters SET finalized_at = NOW()
		WHERE finalized_at IS NULL AND day < $1::date`, cutoff.Format("2006-01-02")); err != nil {
		log.Printf("[PropertyIntroRollup] finalize failed: %v", err)
		if hbStatus == "ok" {
			hbStatus, hbErr = "error", "finalize: "+err.Error()
		}
	}

	EmitHeartbeat(ctx, w.db, propertyIntroRollupWorkerName, int(w.interval.Seconds()), hbStatus, hbErr)
}

// rollupIndexValid checks idx_pcq_intro_rollup exists AND indisvalid.
func (w *PropertyIntroRollupWorker) rollupIndexValid(ctx context.Context) (bool, error) {
	var valid bool
	err := w.db.QueryRowContext(ctx, `
		SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'idx_pcq_intro_rollup'`).Scan(&valid)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return valid, nil
}

// promotePendingBudgets promotes staged edits whose effective day has arrived
// (Denver). Idempotent: the pending pair is cleared in the same statement.
func (w *PropertyIntroRollupWorker) promotePendingBudgets(ctx context.Context, today time.Time) (int64, error) {
	res, err := w.db.ExecContext(ctx, `
		UPDATE partner_drip_brand_budgets
		SET daily_budget = pending_budget,
		    pending_budget = NULL,
		    pending_effective_day = NULL,
		    lock_version = lock_version + 1,
		    updated_at = NOW()
		WHERE pending_effective_day IS NOT NULL AND pending_effective_day <= $1::date`, today.Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// propertyIntroRollupUpsertSQL materializes one Denver day's full grid.
// $1 day, $2 brands text[], $3 isps text[], $4 window_start_utc,
// $5 window_end_utc, $6 run_id. Zero cells are explicit rows (LEFT JOIN).
// The grouped scan is served by idx_pcq_intro_rollup
// (mailed_at, mailed_brand, isp_family) WHERE mailed_at IS NOT NULL.
const propertyIntroRollupUpsertSQL = `
	WITH grid AS (
		SELECT b.brand, i.isp
		FROM unnest($2::text[]) AS b(brand)
		CROSS JOIN unnest($3::text[]) AS i(isp)
	), agg AS (
		SELECT LOWER(BTRIM(mailed_brand)) AS brand,
		       LOWER(COALESCE(NULLIF(BTRIM(isp_family), ''), 'other')) AS isp,
		       COUNT(*) AS introduced
		FROM partner_clean_queue
		WHERE mailed_at IS NOT NULL
		  AND mailed_at >= $4 AND mailed_at < $5
		  AND mailed_brand IS NOT NULL AND BTRIM(mailed_brand) <> ''
		GROUP BY 1, 2
	)
	INSERT INTO property_intro_counters
		(day, brand, isp, introduced, window_start_utc, window_end_utc, run_id, updated_at)
	SELECT $1::date, g.brand, g.isp, COALESCE(a.introduced, 0), $4, $5, $6, NOW()
	FROM grid g
	LEFT JOIN agg a ON a.brand = g.brand AND a.isp = g.isp
	ON CONFLICT (day, brand, isp) DO UPDATE SET
		introduced       = EXCLUDED.introduced,
		window_start_utc = EXCLUDED.window_start_utc,
		window_end_utc   = EXCLUDED.window_end_utc,
		run_id           = EXCLUDED.run_id,
		updated_at       = NOW()`

// runOneDay records a run row (I-8) and materializes the day's grid.
func (w *PropertyIntroRollupWorker) runOneDay(ctx context.Context, day time.Time) error {
	brands := DripIntroBrands()
	isps := isppkg.LedgerGroups()
	expected := len(brands) * len(isps)
	winStart, winEnd := denverDayWindowUTC(day, w.loc)

	var runID string
	if err := w.db.QueryRowContext(ctx, `
		INSERT INTO property_counter_runs (day, expected_cells, status)
		VALUES ($1::date, $2, 'running') RETURNING run_id::text`, day.Format("2006-01-02"), expected).Scan(&runID); err != nil {
		return fmt.Errorf("run row: %w", err)
	}

	completed, runErr := w.materializeDay(ctx, day, brands, isps, winStart, winEnd, runID)
	if runErr != nil {
		if _, uerr := w.db.ExecContext(ctx, `
			UPDATE property_counter_runs SET status='failed', completed_cells=$2,
			       finished_at=NOW(), error=$3 WHERE run_id=$1::uuid`,
			runID, completed, truncateRunDetail(runErr.Error())); uerr != nil {
			log.Printf("[PropertyIntroRollup] run %s fail-stamp error: %v", runID, uerr)
		}
		return runErr
	}
	if _, err := w.db.ExecContext(ctx, `
		UPDATE property_counter_runs SET status='completed', completed_cells=$2,
		       finished_at=NOW() WHERE run_id=$1::uuid`, runID, completed); err != nil {
		return fmt.Errorf("run complete-stamp: %w", err)
	}
	return nil
}

// materializeDay runs the grid upsert in its own tx with a 300s budget
// (scorecard idiom — internal/domainagent/scorecard.go).
func (w *PropertyIntroRollupWorker) materializeDay(ctx context.Context, day time.Time, brands, isps []string, winStart, winEnd time.Time, runID string) (int64, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '300s'`); err != nil {
		return 0, fmt.Errorf("statement_timeout: %w", err)
	}
	res, err := tx.ExecContext(ctx, propertyIntroRollupUpsertSQL,
		day.Format("2006-01-02"), pq.Array(brands), pq.Array(isps), winStart, winEnd, runID)
	if err != nil {
		return 0, fmt.Errorf("grid upsert: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}

// denverDate truncates a wall-clock instant to its Denver calendar date
// (returned as a date-only time in loc, midnight).
func denverDate(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
}

// denverDayWindowUTC returns the Go-computed UTC bounds [start, end) of one
// Denver calendar day (I-1). DST days are naturally 23h/25h — never assume 24.
func denverDayWindowUTC(day time.Time, loc *time.Location) (time.Time, time.Time) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	return start.UTC(), end.UTC()
}

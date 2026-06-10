package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Segment-materialization throttle.
//
// MaterializeSegment runs a full mailing_subscribers × mailing_tracking_events
// scan (6-10 min each on the 50M+ row events table) and DELETE/INSERTs into
// mailing_segment_members. It is invoked from MANY call sites with no shared
// concurrency control: the nightly SegmentMaterializer cycle, the boot-time
// canonical + engagement-catalog hydrators, and — critically — an UNBOUNDED
// `go func()` spawned per POST /api/mailing/segments request (see
// CreateSegment / segmentation_handlers). A burst of segment creations (e.g. a
// catalog-seeder script) therefore launches N concurrent multi-minute heap
// scans, which saturates RDS IO and stalls unrelated workers — the recurring
// "audience/segment-evaluation IO storm" that wedged both the partner drip and
// the outbox self-check on 2026-06-07.
//
// materializeSem bounds the number of concurrent heavy materializations
// process-wide. Default 2 (conservative for an already-IO-pressured primary);
// override via SEGMENT_MATERIALIZE_CONCURRENCY for ops tuning without a
// redeploy. Acquisition is context-aware so a cancelled/timed-out caller never
// blocks on a saturated semaphore.
const defaultMaterializeConcurrency = 2

// parseMaterializeConcurrency resolves the throttle width from a raw env value,
// clamping to [1,16] and falling back to defaultMaterializeConcurrency for
// empty/invalid input. Pure so it can be unit-tested without touching package
// init or the process environment.
func parseMaterializeConcurrency(raw string) int {
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 16 {
			return n
		}
		log.Printf("[MaterializeSegment] invalid SEGMENT_MATERIALIZE_CONCURRENCY=%q, using default %d", v, defaultMaterializeConcurrency)
	}
	return defaultMaterializeConcurrency
}

var materializeConcurrency = parseMaterializeConcurrency(os.Getenv("SEGMENT_MATERIALIZE_CONCURRENCY"))

var materializeSem = make(chan struct{}, materializeConcurrency)

// SegmentMaterializer pre-computes segment membership into
// mailing_segment_members once per day at a fixed UTC time so that audience
// planning can read cached results instead of running expensive live queries.
type SegmentMaterializer struct {
	db         *sql.DB
	targetHour int
	targetMin  int
}

// NewSegmentMaterializer creates a materializer that runs daily at the given
// UTC time (e.g. "04:00" for 9 PM MST).
func NewSegmentMaterializer(db *sql.DB, targetUTC string) *SegmentMaterializer {
	hour, min := 4, 0
	if n, _ := fmt.Sscanf(targetUTC, "%d:%d", &hour, &min); n < 2 {
		log.Printf("[SegmentMaterializer] invalid targetUTC %q, defaulting to 04:00", targetUTC)
		hour, min = 4, 0
	}
	return &SegmentMaterializer{db: db, targetHour: hour, targetMin: min}
}

// DurationUntilNext calculates the sleep duration from `now` until the next
// occurrence of the target time. Exported for unit testing.
func DurationUntilNext(now time.Time, targetHour, targetMin int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), targetHour, targetMin, 0, 0, time.UTC)
	if !now.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

func (m *SegmentMaterializer) Start(ctx context.Context) {
	wait := DurationUntilNext(time.Now().UTC(), m.targetHour, m.targetMin)
	nextRun := time.Now().UTC().Add(wait)
	log.Printf("[SegmentMaterializer] started — next run at %s (sleeping %s)",
		nextRun.Format("2006-01-02 15:04 UTC"), wait.Round(time.Second))

	go func() {
		for {
			wait = DurationUntilNext(time.Now().UTC(), m.targetHour, m.targetMin)
			select {
			case <-time.After(wait):
				log.Printf("[SegmentMaterializer] nightly cycle starting")
				m.materializeAll(ctx)
			case <-ctx.Done():
				log.Println("[SegmentMaterializer] context cancelled, stopping")
				return
			}
		}
	}()
}

// CanonicalMasterListSegments enumerates the seeded, user-visible segments
// that the master-list architecture depends on. The list lives in-code so the
// set of "first-class" segments is discoverable from a single call site and
// stays in lock-step with the phase21 seed migrations in main.go.
//
// Names MUST match the values inserted by those migrations exactly; this is
// the join key used by MaterializeCanonicalSegments.
var CanonicalMasterListSegments = []string{
	"Master List",
	"Engaged Openers (30d)",
	"Engaged Clickers (30d)",
}

// MaterializeCanonicalSegments runs MaterializeSegment once for each segment
// whose name matches CanonicalMasterListSegments across all organizations.
//
// Called on every server boot (via server_routes_mailing.go) after startup
// migrations complete. Fresh installs and newly-seeded orgs get member rows
// populated immediately instead of waiting 24h for the nightly materializer
// — otherwise the UI's "Master List" and "Engaged Openers/Clickers" tiles
// would show 0 members until 04:00 UTC the next day.
//
// Idempotent: MaterializeSegment internally DELETEs and reinserts members
// inside a single transaction, so invoking this on every boot is safe. It
// does re-scan the full subscriber table though — which is the right trade-
// off for a 3-segment set that every campaign screen reads from, but would
// not scale to hundreds of segments.
func (m *SegmentMaterializer) MaterializeCanonicalSegments(ctx context.Context) {
	start := time.Now()

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id::text, COALESCE(s.list_id::text, ''), s.name, COALESCE(s.conditions::text, '[]')
		FROM mailing_segments s
		WHERE s.status = 'active'
		  AND s.segment_type = 'dynamic'
		  AND s.name = ANY($1::text[])
		ORDER BY s.name ASC
	`, pq.Array(CanonicalMasterListSegments))
	if err != nil {
		log.Printf("[SegmentMaterializer] canonical query error: %v", err)
		return
	}
	defer rows.Close()

	type segInfo struct {
		id, listID, name, conditions string
	}
	var segments []segInfo
	for rows.Next() {
		var s segInfo
		if err := rows.Scan(&s.id, &s.listID, &s.name, &s.conditions); err != nil {
			log.Printf("[SegmentMaterializer] canonical scan error: %v", err)
			continue
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		log.Printf("[SegmentMaterializer] no canonical segments found — phase21 seeds may not have run yet")
		return
	}

	materialized := 0
	for _, seg := range segments {
		if ctx.Err() != nil {
			break
		}
		count, err := m.materializeOne(ctx, seg.id, seg.listID, seg.conditions)
		if err != nil {
			log.Printf("[SegmentMaterializer] canonical %q (%s) failed: %v",
				seg.name, safePrefix(seg.id, 12), err)
			continue
		}
		log.Printf("[SegmentMaterializer] canonical %q — %d members", seg.name, count)
		materialized++
	}

	log.Printf("[SegmentMaterializer] canonical materialization: %d/%d segments in %s",
		materialized, len(segments), time.Since(start).Round(time.Millisecond))
}

// MaterializeEngagementCatalog runs MaterializeSegment for every active
// dynamic segment in the engagement_brand or engagement_global category
// that has zero rows in mailing_segment_members. Intended as a one-shot
// boot-time hydrator for segments that were created via API (POST
// /api/mailing/segments) but whose inline background goroutine either
// timed out or returned 0 due to BuildSegmentWhereClause's defensive
// 5s statement_timeout on the calculateSegmentCount path.
//
// Idempotent: only processes segments with mailing_segment_members
// COUNT(*) = 0. Already-populated segments are skipped so subsequent
// boots don't redundantly re-DELETE-then-INSERT thousands of rows.
//
// Runs in its own goroutine so the HTTP server starts on time — heavy
// global segments (no sending_domain filter) can take 6-10 minutes each
// to scan the full mailing_subscribers x mailing_tracking_events join.
// MaterializeSegment's own 10-min statement_timeout caps each call.
//
// Added 2026-05-29 alongside the direct-API build of the 102-segment
// engagement catalog (.scratch/may29_seed_engagement_segments.py) to
// resolve the 10 stragglers that the inline goroutine couldn't finish.
func (m *SegmentMaterializer) MaterializeEngagementCatalog(ctx context.Context) {
	start := time.Now()

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id::text, COALESCE(s.list_id::text, ''), s.name, COALESCE(s.conditions::text, '[]')
		FROM mailing_segments s
		WHERE s.status = 'active'
		  AND s.segment_type = 'dynamic'
		  AND s.category IN ('engagement_brand', 'engagement_global')
		  AND NOT EXISTS (
		    SELECT 1 FROM mailing_segment_members msm
		    WHERE msm.segment_id = s.id
		  )
		ORDER BY s.name ASC
	`)
	if err != nil {
		log.Printf("[SegmentMaterializer] engagement catalog query error: %v", err)
		return
	}
	defer rows.Close()

	type segInfo struct {
		id, listID, name, conditions string
	}
	var segments []segInfo
	for rows.Next() {
		var s segInfo
		if err := rows.Scan(&s.id, &s.listID, &s.name, &s.conditions); err != nil {
			log.Printf("[SegmentMaterializer] engagement catalog scan error: %v", err)
			continue
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		log.Printf("[SegmentMaterializer] engagement catalog: all segments already materialized (nothing to do)")
		return
	}

	log.Printf("[SegmentMaterializer] engagement catalog: hydrating %d unmaterialized segments", len(segments))

	materialized := 0
	for _, seg := range segments {
		if ctx.Err() != nil {
			log.Printf("[SegmentMaterializer] engagement catalog: context cancelled after %d/%d", materialized, len(segments))
			break
		}
		count, err := m.materializeOne(ctx, seg.id, seg.listID, seg.conditions)
		if err != nil {
			log.Printf("[SegmentMaterializer] engagement catalog %q (%s) failed: %v",
				seg.name, safePrefix(seg.id, 12), err)
			continue
		}
		log.Printf("[SegmentMaterializer] engagement catalog %q — %d members", seg.name, count)
		materialized++
	}

	log.Printf("[SegmentMaterializer] engagement catalog: %d/%d hydrated in %s",
		materialized, len(segments), time.Since(start).Round(time.Millisecond))
}

func (m *SegmentMaterializer) materializeAll(ctx context.Context) {
	start := time.Now()

	// Selection strategy: oldest `updated_at` first, capped at 500 per
	// nightly cycle. With ~14k active dynamic segments the LIMIT is the
	// effective cycle-length governor — at 500/night a segment is
	// re-materialized every ~28 days. Bumped from the original 200 (which
	// gave a ~70-day cycle and silently starved campaign-critical segments
	// like QF 30D Clickers — observed 12-day stale on 2026-05-31).
	//
	// Fairness contract: materializeOne MUST advance `updated_at` on every
	// success so the same 500 don't get re-picked the next night. The
	// SegmentRefreshWorker also bumps `updated_at` on its 30-min count
	// refresh cycle, so the two workers cooperate to keep all active
	// segments rotating naturally without us needing a separate priority
	// queue or cursor table.
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id::text, s.list_id::text, s.name, COALESCE(s.conditions::text, '[]')
		FROM mailing_segments s
		WHERE s.status = 'active' AND s.segment_type = 'dynamic'
		ORDER BY s.updated_at ASC
		LIMIT 500
	`)
	if err != nil {
		log.Printf("[SegmentMaterializer] query segments error: %v", err)
		return
	}
	defer rows.Close()

	type segInfo struct {
		id, listID, name, conditions string
	}
	var segments []segInfo
	for rows.Next() {
		var s segInfo
		var listIDNull sql.NullString
		if err := rows.Scan(&s.id, &listIDNull, &s.name, &s.conditions); err != nil {
			log.Printf("[SegmentMaterializer] scan error: %v", err)
			continue
		}
		if listIDNull.Valid {
			s.listID = listIDNull.String
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		return
	}

	materialized := 0
	for _, seg := range segments {
		if ctx.Err() != nil {
			break
		}
		count, err := m.materializeOne(ctx, seg.id, seg.listID, seg.conditions)
		if err != nil {
			log.Printf("[SegmentMaterializer] %s (%s) failed: %v", seg.name, safePrefix(seg.id, 12), err)
			continue
		}
		log.Printf("[SegmentMaterializer] %s — %d members", seg.name, count)
		materialized++
	}

	log.Printf("[SegmentMaterializer] materialized %d/%d segments in %s",
		materialized, len(segments), time.Since(start).Round(time.Millisecond))
}

func (m *SegmentMaterializer) materializeOne(ctx context.Context, segmentID, listID, conditionsRaw string) (int, error) {
	// MaterializeSegment records the build in mailing_segment_build_ledger
	// (running → ok/failed, source 'materializer') — no extra ledger write
	// is needed here.
	count, err := MaterializeSegment(ctx, m.db, segmentID, listID, conditionsRaw)
	if err != nil {
		return count, err
	}

	// Fairness contract (see materializeAll docstring): advance
	// `updated_at` + `last_calculated_at` and refresh `subscriber_count` so
	// this segment cycles to the back of the worker's selection queue. If
	// we don't bump these, the same 500 oldest-by-updated_at segments get
	// re-picked every night and the long tail starves indefinitely.
	//
	// Failure here is non-fatal: the materialization itself succeeded, the
	// member rows are correct, and the next night's run will retry the
	// metadata bump. We log + swallow so the broader cycle doesn't abort.
	if _, err := m.db.ExecContext(ctx, `
		UPDATE mailing_segments
		SET subscriber_count   = $1,
		    last_calculated_at = NOW(),
		    updated_at         = NOW()
		WHERE id = $2::uuid
	`, count, segmentID); err != nil {
		log.Printf("[SegmentMaterializer] post-materialize cache update failed for %s: %v",
			safePrefix(segmentID, 12), err)
	}
	return count, nil
}

// MaterializeSegment populates mailing_segment_members for a single segment.
// Called by the nightly materializer and by segment-creation handlers.
// Every call is recorded in mailing_segment_build_ledger with source
// 'materializer'; callers that want a different ledger source (e.g. the
// explicit /recalculate endpoint) call materializeSegmentWithLedger directly.
func MaterializeSegment(ctx context.Context, db *sql.DB, segmentID, listID, conditionsRaw string) (int, error) {
	return materializeSegmentWithLedger(ctx, db, segmentID, listID, conditionsRaw, "materializer")
}

// materializeSegmentWithLedger wraps the core member-writing build with
// segment-build-ledger bookkeeping: mark 'running' before, upsert a terminal
// 'ok'/'failed' row after (count, duration, delta vs the previous build).
//
// Ledger writes are strictly best-effort: every error is logged and
// swallowed — the build's own result is never affected by the ledger. The
// terminal upsert also survives caller-context cancellation (ledgerWriteCtx
// detaches), so a 10-min-timeout build still records its failure.
func materializeSegmentWithLedger(ctx context.Context, db *sql.DB, segmentID, listID, conditionsRaw, source string) (int, error) {
	// Refuse lake-built segments BEFORE touching the ledger so a misrouted
	// recalculate doesn't even pollute the ledger row with a failed status
	// (materializeSegmentCore repeats the check as defense-in-depth).
	if parseLakeSpec(conditionsRaw) != nil {
		return 0, errLakeSegmentNotMaterializable
	}
	if err := MarkSegmentBuildRunning(ctx, db, segmentID, source); err != nil {
		log.Printf("[SegmentLedger] mark-running failed for %s (continuing): %v", safePrefix(segmentID, 12), err)
	}
	prevCount, hasPrev := ledgerPriorCount(ctx, db, segmentID)

	start := time.Now()
	count, err := materializeSegmentCore(ctx, db, segmentID, listID, conditionsRaw)
	durMs := int(time.Since(start).Milliseconds())

	if err != nil {
		// Keep the previous good count on failure — a failed rebuild must not
		// zero out the displayed audience size.
		if lerr := UpsertSegmentLedger(ctx, db, segmentID, prevCount, source, "failed", durMs, 0, err.Error()); lerr != nil {
			log.Printf("[SegmentLedger] upsert(failed) for %s: %v", safePrefix(segmentID, 12), lerr)
		}
		return count, err
	}

	deltaPct := 0.0
	if hasPrev {
		deltaPct = computeLedgerDeltaPct(prevCount, int64(count))
	}
	if lerr := UpsertSegmentLedger(ctx, db, segmentID, int64(count), source, "ok", durMs, deltaPct, ""); lerr != nil {
		log.Printf("[SegmentLedger] upsert(ok) for %s: %v", safePrefix(segmentID, 12), lerr)
	}
	return count, nil
}

// errLakeSegmentNotMaterializable guards lake-built segments out of every
// legacy materialization path. Their {"lake_spec":...} conditions are not a
// V2 group and not a legacy condition array, so buildSegmentQuery's legacy
// branch (which DISCARDS the unmarshal error) would degrade them to an empty
// WHERE clause — and the DELETE + INSERT..SELECT below would then replace the
// segment's members with the ENTIRE active subscriber base, cross-org.
var errLakeSegmentNotMaterializable = fmt.Errorf(
	"lake-built segment — refusing legacy materialization; use POST /v2/segments/{id}/refresh")

// materializeSegmentCore is the ledger-free member-writing build:
// DELETE + INSERT..SELECT into mailing_segment_members in one transaction.
func materializeSegmentCore(ctx context.Context, db *sql.DB, segmentID, listID, conditionsRaw string) (int, error) {
	// Defense-in-depth (see errLakeSegmentNotMaterializable): never run a
	// lake-spec conditions blob through buildSegmentQuery.
	if parseLakeSpec(conditionsRaw) != nil {
		return 0, errLakeSegmentNotMaterializable
	}

	segCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if len(segmentID) != 36 {
		return 0, fmt.Errorf("invalid segment ID length: %d", len(segmentID))
	}
	for _, c := range segmentID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
			return 0, fmt.Errorf("invalid segment ID character: %c", c)
		}
	}

	// Throttle concurrent heavy scans process-wide (see materializeSem
	// docstring). Context-aware so a cancelled/timed-out caller bails instead
	// of piling onto a saturated primary.
	select {
	case materializeSem <- struct{}{}:
		defer func() { <-materializeSem }()
	case <-segCtx.Done():
		return 0, fmt.Errorf("materialize throttle wait cancelled: %w", segCtx.Err())
	}

	conn, err := db.Conn(segCtx)
	if err != nil {
		return 0, err
	}
	defer func() {
		conn.ExecContext(context.Background(), "RESET ALL")
		conn.Close()
	}()

	if _, err := conn.ExecContext(segCtx, "SET statement_timeout = '600000'"); err != nil {
		log.Printf("[MaterializeSegment] SET statement_timeout failed: %v", err)
	}

	var listIDVal interface{}
	if listID != "" {
		listIDVal = listID
	}
	segQuery, segArgs := buildSegmentQuery(conditionsRaw, listIDVal)

	tx, err := conn.BeginTx(segCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(segCtx, "DELETE FROM mailing_segment_members WHERE segment_id = $1", segmentID); err != nil {
		return 0, fmt.Errorf("delete old members: %w", err)
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
		 SELECT DISTINCT '%s'::uuid, q.id::uuid, q.email, NOW()
		 FROM (%s) q
		 ON CONFLICT (segment_id, subscriber_id) DO NOTHING`, segmentID, segQuery)

	result, err := tx.ExecContext(segCtx, insertSQL, segArgs...)
	if err != nil {
		return 0, fmt.Errorf("insert-select: %w", err)
	}

	count, _ := result.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(count), nil
}

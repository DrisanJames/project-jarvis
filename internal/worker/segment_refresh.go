package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/lib/pq"
)

// SegmentRefreshWorker periodically recalculates subscriber_count for all
// active dynamic segments. Without this, time-relative conditions like
// "last_open_at within_last 7 days" would show stale counts.
//
// Concurrency controls how many segments are recalculated in parallel.
// Each concurrent recalculation holds its own DB transaction, so this
// should be sized to avoid overwhelming the connection pool (4-8 is safe
// for most RDS instances).
type SegmentRefreshWorker struct {
	db          *sql.DB // heavy analytical reads (points to read replica when available)
	writeDB     *sql.DB // lightweight count updates (always points to primary)
	interval    time.Duration
	concurrency int
	stopChan    chan struct{}
	running     bool
}

func NewSegmentRefreshWorker(readDB, writeDB *sql.DB, interval time.Duration) *SegmentRefreshWorker {
	return &SegmentRefreshWorker{
		db:          readDB,
		writeDB:     writeDB,
		interval:    interval,
		concurrency: 1,
		stopChan:    make(chan struct{}),
	}
}

// NewSegmentRefreshWorkerWithConcurrency creates a worker with a specific
// parallelism level. Each concurrent goroutine holds one DB transaction,
// so concurrency should stay below the connection pool's max idle conns.
func NewSegmentRefreshWorkerWithConcurrency(readDB, writeDB *sql.DB, interval time.Duration, concurrency int) *SegmentRefreshWorker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &SegmentRefreshWorker{
		db:          readDB,
		writeDB:     writeDB,
		interval:    interval,
		concurrency: concurrency,
		stopChan:    make(chan struct{}),
	}
}

func (w *SegmentRefreshWorker) Start(ctx context.Context) {
	if w.running {
		return
	}
	w.running = true
	log.Printf("SegmentRefreshWorker: started (interval=%s, concurrency=%d)", w.interval, w.concurrency)

	go func() {
		// Small delay on startup to let DB connections settle
		time.Sleep(30 * time.Second)
		w.refreshAll(ctx)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.refreshAll(ctx)
			case <-w.stopChan:
				log.Println("SegmentRefreshWorker: stopped")
				return
			case <-ctx.Done():
				log.Println("SegmentRefreshWorker: context cancelled, stopping")
				return
			}
		}
	}()
}

func (w *SegmentRefreshWorker) Stop() {
	if !w.running {
		return
	}
	close(w.stopChan)
	w.running = false
}

type segmentRow struct {
	ID             uuid.UUID
	ListID         *uuid.UUID
	Name           string
	ConditionsJSON sql.NullString
	OrgID          string
	OldCount       int
}

func (w *SegmentRefreshWorker) refreshAll(ctx context.Context) {
	start := time.Now()

	hbStatus, hbErr := "ok", ""
	var refreshed, failed int64
	defer func() {
		EmitHeartbeat(ctx, w.writeDB, "segment_refresh", int(w.interval.Seconds()), hbStatus, hbErr)

		// Run-level summary, next to the heartbeat: what this cycle did.
		ok, bad := atomic.LoadInt64(&refreshed), atomic.LoadInt64(&failed)
		runStatus := "ok"
		detail := fmt.Sprintf("refreshed %d dynamic segments (%d failed)", ok, bad)
		switch {
		case hbStatus == "error":
			runStatus = "failed"
			detail = truncateRunDetail("cycle error: " + hbErr)
		case bad > 0:
			runStatus = "partial"
		}
		RecordWorkerRun(ctx, w.writeDB, "segment_refresh", start, runStatus, int(ok), int(bad), detail)
	}()

	// Staleness guard: skip segments that have gone unused long enough to be
	// cleanup candidates (SegmentCleanupWorker will archive/delete them). Without
	// this, dead-but-active dynamic segments sit at the front of the
	// oldest-updated_at queue and burn expensive recalculation cycles on
	// audiences no campaign references anymore. Pinned segments are always kept
	// fresh. 30d mirrors the default inactive_days_threshold.
	//
	// SCOPE = WHAT CLEANUP KEEPS (2026-11-04, WP-F). The guard's premise is
	// "SegmentCleanupWorker will archive them", but autoArchiveUnreferencedSegments
	// (segment_cleanup.go) never archives a segment whose name matches an active
	// keep_policy='protect' registry family. Those segments stayed active, were
	// rebuilt daily by the materializer, and were never refreshed here: on
	// 2026-11-04 the worker selected 91 of 218 active dynamic segments (log
	// "refreshed 91/91") while all 78 stale engagement segments jarvis_ops
	// flagged were outside the guard and 78/78 registry-protected. The
	// predicate below is copied from that cleanup query, so refresh scope ==
	// cleanup keep-set. SEGMENT_REFRESH_LEGACY_SCOPE=1 restores the old guard.
	rows, err := w.db.QueryContext(ctx, segmentRefreshSelectSQL(segmentRefreshLegacyScope()))
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("SegmentRefreshWorker: query error: %v", err)
		return
	}
	defer rows.Close()

	var segments []segmentRow
	for rows.Next() {
		var s segmentRow
		if err := rows.Scan(&s.ID, &s.ListID, &s.Name, &s.ConditionsJSON, &s.OrgID, &s.OldCount); err != nil {
			log.Printf("SegmentRefreshWorker: scan error: %v", err)
			continue
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		return
	}

	// Derive subscriber_count from the already-materialized membership table
	// (mailing_segment_members, maintained by SegmentMaterializer daily + on
	// boot) instead of re-deriving each segment's audience with live
	// mailing_tracking_events scans.
	//
	// WHY (2026-06-07): the previous path called recalculate() per segment,
	// which for event-based conditions ran a DISTINCT scan over the 50M+ row
	// mailing_tracking_events table (buildCTEQuery, the `_evt_N` CTEs). With
	// 187 of ~234 active dynamic segments event-based, a single refresh cycle
	// took 4h+ under send-day IO and repeatedly tripped the stall detector
	// (heartbeat is emitted only at end-of-cycle). It also silently dropped
	// `tags` conditions (mapCol has no "tags"), so tag-scoped segments were
	// counted as the entire opener/clicker population — both wrong and maximally
	// expensive. subscriber_count is a display metric, so a COUNT over the
	// indexed members table (PK leads with segment_id → index-only, ms-scale)
	// at last-materialization freshness is both cheaper and more accurate.
	//
	// The heavy recalculate()/buildCTEQuery()/buildRefreshWhereClause() helpers
	// are intentionally retained (covered by segment_refresh_test.go and
	// available as a fallback) but are no longer on the hot cycle path.

	// BOUNDED COUNT CHUNKS (2026-11-04, WP-F). One GROUP BY over every
	// segment's members exceeded the prod 30s statement_timeout on 38 of 96
	// cycles in 24h ("member-count read truncated after 40-84/91 segments ...
	// canceling statement due to statement timeout"), aborting the whole pass.
	// Counting in chunks of ~segmentRefreshChunkMembers cached members, each
	// with its own deadline, keeps every statement well under the budget; a
	// chunk that fails or truncates writes NOTHING for its own segments (the
	// partial-read guard below, per chunk) while clean chunks still land. The
	// failed chunk's segments keep their old updated_at, so they sort first
	// next cycle (ORDER BY updated_at ASC) — retry ordering for free.
	var chunkErrs []string
	writeChunk := func(chunk []segmentRow, counts map[uuid.UUID]int) {
		// The chunk's result set is verified complete: segments with no row
		// genuinely have zero materialized members (empty, or created since the
		// last materialization pass — they fill in on the next SegmentMaterializer run).
		for _, seg := range chunk {
			if ctx.Err() != nil {
				return
			}
			newCount := counts[seg.ID]
			if _, err := w.writeDB.ExecContext(ctx, `
				UPDATE mailing_segments
				SET subscriber_count = $2, last_calculated_at = NOW(), updated_at = NOW()
				WHERE id = $1
			`, seg.ID, newCount); err != nil {
				log.Printf("SegmentRefreshWorker: update error for %s (%s): %v", seg.Name, seg.ID, err)
				atomic.AddInt64(&failed, 1)
				continue
			}
			if newCount != seg.OldCount {
				log.Printf("SegmentRefreshWorker: %s — %d → %d", seg.Name, seg.OldCount, newCount)
			}
			atomic.AddInt64(&refreshed, 1)
		}
	}
	for _, chunk := range segmentRefreshCountChunks(segments, segmentRefreshChunkMembers, segmentRefreshChunkMaxSegments) {
		if ctx.Err() != nil {
			break
		}
		counts, cErr := w.countMaterializedMembers(ctx, chunk)
		if cErr == nil {
			writeChunk(chunk, counts)
			continue
		}
		if len(chunk) < 2 {
			chunkErrs = append(chunkErrs, cErr.Error())
			log.Printf("SegmentRefreshWorker: member-count chunk of 1 segment failed (no counts written) ids=%s: %v",
				segmentRefreshIDList(chunk), cErr)
			continue
		}
		log.Printf("SegmentRefreshWorker: member-count chunk of %d segments failed (no counts written for them; splitting once): %v",
			len(chunk), cErr)
		// One split level: OldCount (the chunk-sizing input) is stale or 0 for
		// segments this worker has not counted recently, so an oversized chunk
		// would otherwise re-form and fail identically every cycle. Each half is
		// retried once; ≤2 extra statements per failed chunk.
		mid := len(chunk) / 2
		for _, half := range [][]segmentRow{chunk[:mid], chunk[mid:]} {
			if ctx.Err() != nil {
				break
			}
			hc, hErr := w.countMaterializedMembers(ctx, half)
			if hErr != nil {
				chunkErrs = append(chunkErrs, hErr.Error())
				log.Printf("SegmentRefreshWorker: member-count half-chunk of %d segments still failing (no counts written) ids=%s: %v",
					len(half), segmentRefreshIDList(half), hErr)
				continue
			}
			writeChunk(half, hc)
		}
	}
	if len(chunkErrs) > 0 {
		hbStatus, hbErr = "error", truncateRunDetail(fmt.Sprintf("member-count read truncated in %d chunk(s): %s",
			len(chunkErrs), strings.Join(chunkErrs, "; ")))
	}

	log.Printf("SegmentRefreshWorker: refreshed %d/%d segments from materialized members in %s",
		atomic.LoadInt64(&refreshed), len(segments), time.Since(start).Round(time.Millisecond))
}

// segmentRefreshChunkMembers is the cached-member budget per count statement.
// 2026-11-04 the replica counted roughly 1.2-1.6M member rows before the 30s
// statement_timeout fired under send-day IO, so 250k leaves ~5x headroom.
const segmentRefreshChunkMembers = 250000

// segmentRefreshChunkMaxSegments caps segments per count statement, so a run
// of segments whose cached count is 0 or stale cannot make a chunk unbounded.
const segmentRefreshChunkMaxSegments = 25

// segmentRefreshChunkTimeout bounds one chunk's count statement below the
// prod 30s statement_timeout, so our own deadline fires first.
const segmentRefreshChunkTimeout = 25 * time.Second

// segmentRefreshLegacyScope is the kill switch: SEGMENT_REFRESH_LEGACY_SCOPE=1
// restores the pre-2026-11-04 guard (keep_active OR used within 30d), which
// skips registry-protected segments.
func segmentRefreshLegacyScope() bool {
	v := strings.TrimSpace(os.Getenv("SEGMENT_REFRESH_LEGACY_SCOPE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// segmentRefreshSelectSQL returns the refresh cycle's segment-list SELECT.
// The protect-family EXISTS is the same predicate SegmentCleanupWorker's
// autoArchiveUnreferencedSegments uses to keep a segment from archival.
func segmentRefreshSelectSQL(legacyScope bool) string {
	protected := `
		       OR EXISTS (
				SELECT 1 FROM mailing_segment_registry r
				WHERE r.organization_id = s.organization_id
				  AND r.active = TRUE
				  AND r.keep_policy = 'protect'
				  AND s.name LIKE r.family_pattern
		       )`
	if legacyScope {
		protected = ""
	}
	return `
		SELECT s.id, s.list_id, s.name,
		       COALESCE(s.conditions::text, '[]'),
		       s.organization_id::text,
		       COALESCE(s.subscriber_count, 0)
		FROM mailing_segments s
		WHERE s.status = 'active'
		  AND s.segment_type = 'dynamic'
		  AND (s.keep_active = TRUE
		       OR COALESCE(s.last_used_at, s.created_at) > NOW() - INTERVAL '30 days'` + protected + `)
		ORDER BY s.updated_at ASC
		LIMIT 200
	`
}

// segmentRefreshCountChunks splits segments, in order, into chunks whose
// cached member counts sum to at most budget and that hold at most maxSegs
// segments (a single segment larger than budget forms its own chunk). Order
// is preserved so the oldest-updated segments are counted first.
func segmentRefreshCountChunks(segments []segmentRow, budget, maxSegs int) [][]segmentRow {
	if maxSegs < 1 {
		maxSegs = 1
	}
	var chunks [][]segmentRow
	var cur []segmentRow
	sum := 0
	for _, s := range segments {
		n := s.OldCount
		if n < 0 {
			n = 0
		}
		if len(cur) > 0 && (sum+n > budget || len(cur) >= maxSegs) {
			chunks = append(chunks, cur)
			cur, sum = nil, 0
		}
		cur = append(cur, s)
		sum += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// segmentRefreshIDList renders a chunk's segment ids for a failure log line.
func segmentRefreshIDList(chunk []segmentRow) string {
	ids := make([]string, len(chunk))
	for i, s := range chunk {
		ids[i] = s.ID.String()
	}
	return strings.Join(ids, ",")
}

// countMaterializedMembers counts mailing_segment_members for one chunk under
// its own deadline. It returns an error — and no map — unless the result set
// is VERIFIED complete.
//
// PARTIAL-READ GUARD (2026-08-21 incident). lib/pq streams result rows
// lazily, so the QueryContext error check only covers query-time failure:
// a deadline expiring mid-iteration, a replica recovery cancellation, or a
// dropped connection ends the Next() loop SILENTLY with a truncated map. The
// caller treats every absent segment as "genuinely empty" and stamps 0 — on
// 2026-08-21 one such pass zeroed 118 of 250 active Opener/Clicker segments.
// Absence of measurement is not zero: rows.Err() and every Scan must be clean.
func (w *SegmentRefreshWorker) countMaterializedMembers(ctx context.Context, chunk []segmentRow) (map[uuid.UUID]int, error) {
	ids := make([]uuid.UUID, len(chunk))
	for i, s := range chunk {
		ids[i] = s.ID
	}
	cctx, ccancel := context.WithTimeout(ctx, segmentRefreshChunkTimeout)
	defer ccancel()
	mrows, err := w.db.QueryContext(cctx, `
		SELECT segment_id, COUNT(*)
		FROM mailing_segment_members
		WHERE segment_id = ANY($1)
		GROUP BY segment_id
	`, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("member-count query error: %w", err)
	}
	defer mrows.Close()
	counts := make(map[uuid.UUID]int, len(chunk))
	var scanErr error
	for mrows.Next() {
		var id uuid.UUID
		var n int
		if err := mrows.Scan(&id, &n); err != nil {
			scanErr = err
			continue
		}
		counts[id] = n
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("member-count read truncated after %d/%d segments: %w", len(counts), len(chunk), err)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("member-count scan error: %w", scanErr)
	}
	return counts, nil
}

// loadSeedListIDs returns the UUIDs of all lists whose name contains "seed".
func (w *SegmentRefreshWorker) loadSeedListIDs(ctx context.Context) []uuid.UUID {
	rows, err := w.db.QueryContext(ctx, `SELECT id FROM mailing_lists WHERE name ILIKE '%seed%'`)
	if err != nil {
		log.Printf("SegmentRefreshWorker: seed list query error: %v", err)
		return nil
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// recalculate returns the new subscriber count, or -1 on failure.
func (w *SegmentRefreshWorker) recalculate(ctx context.Context, seg segmentRow, seedListIDs []uuid.UUID) int {
	var conditions []segCondition
	if seg.ConditionsJSON.Valid && seg.ConditionsJSON.String != "" && seg.ConditionsJSON.String != "[]" {
		json.Unmarshal([]byte(seg.ConditionsJSON.String), &conditions)
	}

	// Fallback: read from mailing_segment_conditions table
	if len(conditions) == 0 {
		condRows, err := w.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, seg.ID)
		if err == nil {
			defer condRows.Close()
			for condRows.Next() {
				var c segCondition
				if condRows.Scan(&c.Group, &c.Field, &c.Operator, &c.Value) == nil {
					conditions = append(conditions, c)
				}
			}
		}
	}

	if len(conditions) == 0 {
		return -1
	}

	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		log.Printf("SegmentRefreshWorker: begin tx error for %s: %v", seg.Name, err)
		return -1
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '120s'"); err != nil {
		log.Printf("SegmentRefreshWorker: set timeout error for %s: %v", seg.Name, err)
		return -1
	}

	// Classify conditions into categories for optimized query generation.
	var excludePatterns []string
	var eventConds []segCondition
	var subConds []segCondition
	var domainFilter string
	for _, c := range conditions {
		switch {
		case c.Field == "exclude_list_pattern":
			excludePatterns = append(excludePatterns, c.Value)
		case c.Field == "sending_domain":
			domainFilter = c.Value
		case eventMap[c.Field] != "":
			eventConds = append(eventConds, c)
		case isDomainScopable(c.Field, c.Operator):
			eventConds = append(eventConds, c)
		default:
			subConds = append(subConds, c)
		}
	}

	// Check whether any exclude pattern is a seed exclusion.
	hasSeedExcl := false
	for _, pattern := range excludePatterns {
		if strings.Contains(strings.ToLower(pattern), "seed") && len(seedListIDs) > 0 {
			hasSeedExcl = true
		}
	}

	var query string
	var args []interface{}

	var seedIDs []uuid.UUID
	if hasSeedExcl {
		seedIDs = seedListIDs
	}

	if len(eventConds) > 0 {
		query, args = w.buildCTEQuery(seg, eventConds, subConds, seedIDs, domainFilter)
	} else {
		fallbackConds := subConds
		if domainFilter != "" {
			fallbackConds = append(fallbackConds, segCondition{Field: "sending_domain", Value: domainFilter})
		}
		where, whereArgs := buildRefreshWhereClause(seg.ListID, fallbackConds)
		query = fmt.Sprintf("SELECT COUNT(DISTINCT LOWER(email)) FROM mailing_subscribers WHERE %s", where)
		args = whereArgs
		if len(seedIDs) > 0 {
			argNum := len(args) + 1
			query += fmt.Sprintf(" AND id NOT IN (SELECT id FROM mailing_subscribers WHERE list_id = ANY($%d))", argNum)
			args = append(args, pq.Array(seedIDs))
		}
	}

	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		log.Printf("SegmentRefreshWorker: count error for %s: %v (q=%s)", seg.Name, err, query)
		return -1
	}
	tx.Commit()
	return count
}

// buildCTEQuery generates an optimized query using CTEs to materialize
// qualifying subscriber IDs from tracking events, then joins to subscribers
// for the final count. This is dramatically faster for event-heavy segments
// because it avoids scanning the full 10M+ subscriber table.
func (w *SegmentRefreshWorker) buildCTEQuery(
	seg segmentRow,
	eventConds, subConds []segCondition,
	seedIDs []uuid.UUID,
	domainFilter string,
) (string, []interface{}) {
	var ctes []string
	var args []interface{}
	argNum := 1
	var inclCTEs, exclCTEs []string

	for i, c := range eventConds {
		var evType string
		if et, ok := eventMap[c.Field]; ok {
			evType = et
		} else if c.Field == "last_open_at" {
			evType = "opened"
		} else if c.Field == "last_click_at" {
			evType = "clicked"
		} else {
			continue
		}

		isNegative := c.Operator == "not_in_last_days" || c.Operator == "more_than_days_ago" ||
			c.Operator == "not_equals" || c.Operator == "is_not"

		cteName := fmt.Sprintf("_evt_%d", i)
		var timeClause string
		switch c.Operator {
		case "in_last_days", "within_last", "not_in_last_days", "more_than_days_ago":
			timeClause = fmt.Sprintf(" AND e.event_at >= NOW() - INTERVAL '%s days'", c.Value)
		}

		evArg := argNum
		args = append(args, evType)
		argNum++

		var dc string
		if domainFilter != "" {
			dc = fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum, argNum)
			args = append(args, domainFilter)
			argNum++
		}

		cte := fmt.Sprintf("%s AS (SELECT DISTINCT e.subscriber_id FROM mailing_tracking_events e WHERE e.event_type = $%d%s%s)",
			cteName, evArg, timeClause, dc)
		ctes = append(ctes, cte)

		if isNegative {
			exclCTEs = append(exclCTEs, cteName)
		} else {
			inclCTEs = append(inclCTEs, cteName)
		}
	}

	// Build the final query. If we have inclusion CTEs, join through them
	// to narrow the subscriber scan. Then apply exclusion and subscriber-level filters.
	var sb strings.Builder
	sb.WriteString("WITH ")
	sb.WriteString(strings.Join(ctes, ", "))

	// Start from the first inclusion CTE if available (narrows from 10M to ~100K)
	if len(inclCTEs) > 0 {
		sb.WriteString(fmt.Sprintf(
			" SELECT COUNT(DISTINCT LOWER(s.email)) FROM mailing_subscribers s JOIN %s ON %s.subscriber_id = s.id WHERE s.status IN ('active','confirmed')",
			inclCTEs[0], inclCTEs[0]))
		// Additional inclusion CTEs
		for _, ct := range inclCTEs[1:] {
			sb.WriteString(fmt.Sprintf(" AND s.id IN (SELECT subscriber_id FROM %s)", ct))
		}
	} else {
		sb.WriteString(" SELECT COUNT(DISTINCT LOWER(s.email)) FROM mailing_subscribers s WHERE s.status IN ('active','confirmed')")
	}

	if seg.ListID != nil {
		sb.WriteString(fmt.Sprintf(" AND s.list_id = $%d", argNum))
		args = append(args, *seg.ListID)
		argNum++
	}

	// Exclusion CTEs
	for _, ct := range exclCTEs {
		sb.WriteString(fmt.Sprintf(" AND s.id NOT IN (SELECT subscriber_id FROM %s)", ct))
	}

	// Subscriber-level conditions (ISP, email patterns, etc.)
	for _, c := range subConds {
		col := mapCol(c.Field)
		if col == "" {
			continue
		}
		switch c.Operator {
		case "equals", "is":
			sb.WriteString(fmt.Sprintf(" AND s.%s = $%d", col, argNum))
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			sb.WriteString(fmt.Sprintf(" AND s.%s != $%d", col, argNum))
			args = append(args, c.Value)
			argNum++
		case "contains":
			if c.Field == "email" && strings.HasPrefix(c.Value, "@") {
				clause, newArgs, newArgNum := expandEmailContains(col, c.Value, argNum)
				sb.WriteString(clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			} else {
				sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
				args = append(args, "%"+c.Value+"%")
				argNum++
			}
		case "ends_with":
			sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
			args = append(args, "%"+c.Value)
			argNum++
		case "starts_with":
			sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
			args = append(args, c.Value+"%")
			argNum++
		}
	}

	if len(seedIDs) > 0 {
		sb.WriteString(fmt.Sprintf(" AND s.id NOT IN (SELECT id FROM mailing_subscribers WHERE list_id = ANY($%d))", argNum))
		args = append(args, pq.Array(seedIDs))
		argNum++
	}

	return sb.String(), args
}

type segCondition struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// buildRefreshWhereClause mirrors BuildSegmentWhereClause from mailing_segments.go
// but lives in the worker package to avoid import cycles.
// It extracts sending_domain conditions and uses them to scope event subqueries
// and time-relative subscriber fields (last_open_at, last_click_at).
func buildRefreshWhereClause(listID *uuid.UUID, conditions []segCondition) (string, []interface{}) {
	clauses := []string{"status IN ('active','confirmed')"}
	args := []interface{}{}
	argNum := 1

	if listID != nil {
		clauses = append(clauses, fmt.Sprintf("list_id = $%d", argNum))
		args = append(args, *listID)
		argNum++
	}

	// Extract sending_domain filter if present
	var domainFilter string
	var filtered []segCondition
	for _, c := range conditions {
		if c.Field == "sending_domain" {
			domainFilter = c.Value
		} else {
			filtered = append(filtered, c)
		}
	}

	for _, c := range filtered {
		// Exclude subscribers whose email appears in lists matching a name pattern
		if c.Field == "exclude_list_pattern" {
			clause := fmt.Sprintf(`NOT EXISTS (
				SELECT 1 FROM mailing_subscribers s_excl
				JOIN mailing_lists l_excl ON s_excl.list_id = l_excl.id
				WHERE LOWER(s_excl.email) = LOWER(mailing_subscribers.email)
				  AND l_excl.name ILIKE $%d
			)`, argNum)
			clauses = append(clauses, clause)
			args = append(args, "%"+c.Value+"%")
			argNum++
			continue
		}

		col := mapCol(c.Field)
		if col == "" {
			continue
		}

		// Event-based fields route through tracking_events subquery
		if evType, ok := eventMap[c.Field]; ok {
			clause, newArgs, newArgNum := buildEventClause(evType, c.Operator, c.Value, argNum, domainFilter)
			if clause != "" {
				clauses = append(clauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		// Domain-scoped subscriber engagement fields
		if domainFilter != "" && isDomainScopable(c.Field, c.Operator) {
			clause, newArgs, newArgNum := buildDomainScopedClause(c, argNum, domainFilter)
			if clause != "" {
				clauses = append(clauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		var clause string
		switch c.Operator {
		case "equals", "is":
			clause = fmt.Sprintf("%s = $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			clause = fmt.Sprintf("%s != $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "contains":
			if c.Field == "email" && strings.HasPrefix(c.Value, "@") {
				expanded, newArgs, newArgNum := expandEmailContainsNonPrefixed(col, c.Value, argNum)
				clause = expanded
				args = append(args, newArgs...)
				argNum = newArgNum
			} else {
				clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
				args = append(args, "%"+c.Value+"%")
				argNum++
			}
		case "not_contains":
			clause = fmt.Sprintf("%s NOT ILIKE $%d", col, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "starts_with":
			clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
			args = append(args, c.Value+"%")
			argNum++
		case "ends_with":
			clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
			args = append(args, "%"+c.Value)
			argNum++
		case "in_last_days", "within_last":
			clause = fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", col, c.Value)
		case "more_than_days_ago":
			clause = fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", col, c.Value)
		case "not_in_last_days":
			clause = fmt.Sprintf("(%s IS NULL OR %s < NOW() - INTERVAL '%s days')", col, col, c.Value)
		case "is_empty", "is_null":
			clause = fmt.Sprintf("(%s IS NULL OR %s = '')", col, col)
		case "is_not_empty", "is_not_null":
			clause = fmt.Sprintf("(%s IS NOT NULL AND %s != '')", col, col)
		case "greater_than", "gt":
			clause = fmt.Sprintf("%s > $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than", "lt":
			clause = fmt.Sprintf("%s < $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		default:
			continue
		}
		if clause != "" {
			clauses = append(clauses, clause)
		}
	}
	return strings.Join(clauses, " AND "), args
}

func isDomainScopable(field, operator string) bool {
	if field != "last_open_at" && field != "last_click_at" {
		return false
	}
	switch operator {
	case "in_last_days", "within_last", "more_than_days_ago":
		return true
	}
	return false
}

func buildDomainScopedClause(c segCondition, argNum int, domain string) (string, []interface{}, int) {
	eventType := "opened"
	if c.Field == "last_click_at" {
		eventType = "clicked"
	}
	switch c.Operator {
	case "in_last_days", "within_last":
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
			  AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)
		)`, argNum, c.Value, argNum+1, argNum+1)
		return clause, []interface{}{eventType, domain}, argNum + 2
	case "more_than_days_ago":
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
			  AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)
		)`, argNum, c.Value, argNum+1, argNum+1)
		return clause, []interface{}{eventType, domain}, argNum + 2
	}
	return "", nil, argNum
}

// expandEmailContains generates a parameterized SQL fragment for an email
// domain condition, expanding to all sibling domains when applicable.
// Uses isp.DomainSiblings (auto-derived from the canonical domainToISP map).
// Returns the clause (with leading " AND "), any new args, and the next argNum.
func expandEmailContains(col string, value string, argNum int) (string, []interface{}, int) {
	siblings, ok := isp.DomainSiblings[strings.ToLower(value)]
	if !ok || len(siblings) <= 1 {
		return fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum),
			[]interface{}{"%" + value + "%"}, argNum + 1
	}
	var parts []string
	var args []interface{}
	for _, dom := range siblings {
		parts = append(parts, fmt.Sprintf("s.%s ILIKE $%d", col, argNum))
		args = append(args, "%"+dom+"%")
		argNum++
	}
	return " AND (" + strings.Join(parts, " OR ") + ")", args, argNum
}

// expandEmailContainsNonPrefixed generates the same expansion but without
// the "s." table alias prefix, for use in buildRefreshWhereClause.
func expandEmailContainsNonPrefixed(col string, value string, argNum int) (string, []interface{}, int) {
	siblings, ok := isp.DomainSiblings[strings.ToLower(value)]
	if !ok || len(siblings) <= 1 {
		return fmt.Sprintf("%s ILIKE $%d", col, argNum),
			[]interface{}{"%" + value + "%"}, argNum + 1
	}
	var parts []string
	var args []interface{}
	for _, dom := range siblings {
		parts = append(parts, fmt.Sprintf("%s ILIKE $%d", col, argNum))
		args = append(args, "%"+dom+"%")
		argNum++
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, argNum
}

var eventMap = map[string]string{
	"email_sent":         "sent",
	"email_opened":       "opened",
	"email_clicked":      "clicked",
	"email_bounced":      "bounced",
	"email_delivered":    "delivered",
	"email_unsubscribed": "unsubscribed",
	"email_complained":   "complained",
}

func buildEventClause(evType, operator, value string, argNum int, domainFilter string) (string, []interface{}, int) {
	var args []interface{}
	domainClause := ""
	addDomain := func() {
		if domainFilter != "" {
			args = append(args, domainFilter)
			domainClause = fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum+1, argNum+1)
		}
	}

	switch operator {
	case "equals", "is":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d%s)`, argNum, domainClause), args, argNum + len(args)
	case "not_equals", "is_not":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d%s)`, argNum, domainClause), args, argNum + len(args)
	case "in_last_days", "within_last":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d AND e.event_at >= NOW() - INTERVAL '%s days'%s)`, argNum, value, domainClause), args, argNum + len(args)
	case "not_in_last_days", "more_than_days_ago":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d AND e.event_at >= NOW() - INTERVAL '%s days'%s)`, argNum, value, domainClause), args, argNum + len(args)
	default:
		return "", nil, argNum
	}
}

func mapCol(field string) string {
	m := map[string]string{
		"email": "email", "first_name": "first_name", "last_name": "last_name",
		"status": "status", "isp": "isp", "engagement_score": "engagement_score",
		"total_emails_received": "total_emails_received", "total_opens": "total_opens",
		"total_clicks": "total_clicks", "last_email_at": "last_email_at",
		"last_open_at": "last_open_at", "last_click_at": "last_click_at",
		"subscribed_at": "subscribed_at", "created_at": "created_at",
		"source": "source", "timezone": "timezone",
	}
	if col, ok := m[field]; ok {
		return col
	}
	if _, ok := eventMap[field]; ok {
		return field // handled separately
	}
	if strings.HasPrefix(field, "custom.") {
		return fmt.Sprintf("custom_fields->>'%s'", strings.TrimPrefix(field, "custom."))
	}
	return ""
}

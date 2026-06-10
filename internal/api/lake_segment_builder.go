// LAKE SEGMENT BUILDER (orchestration). On-demand construction of standard
// recency segments ("opened/clicked in last N days", global/brand/ISP) from
// the Athena event lake (internal/analytics/segment_builder.go) into
// mailing_segment_members, replacing ad-hoc hot-DB rescans for this segment
// family. Two endpoints, registered in SegmentationAPI.RegisterRoutes:
//
//	POST /api/mailing/v2/segments/{segmentID}/refresh?force=true|false
//	     lake-spec segments  -> 202 async lake build (single slot, 409 if busy)
//	     dynamic segments    -> 200 synchronous MaterializeSegment (existing
//	                            recalculate behavior)
//	POST /api/mailing/v2/segments/build-request
//	     creates one static mailing_segments row per requested window with a
//	     {"lake_spec":{...}} conditions block, then builds them SEQUENTIALLY
//	     through the single slot -> 201
//
// Build progress/outcome is recorded in the segment build ledger
// (UpsertSegmentLedger / MarkSegmentBuildRunning / ledgerPriorCount — owned
// by segment_ledger.go; source 'lake-builder'). Ledger writes are
// observability, never control flow: errors are logged and the build
// continues.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Single build slot
// ---------------------------------------------------------------------------

// lakeBuildSlot serializes lake builds to ONE per process. Each build runs an
// Athena scan (real dollars — the platform got burned on Athena/NAT cost) and
// then a DELETE + COPY burst into mailing_segment_members, a hot table the
// campaign planner reads during 24/7 send hours. Concurrent builds would both
// multiply Athena spend and stack member-table write pressure on top of
// active sends, so a second request is rejected with 409 rather than queued.
var lakeBuildSlot = make(chan struct{}, 1)

// tryAcquireLakeBuildSlot returns true when the caller now owns the single
// build slot. Non-blocking: a busy slot returns false (handler answers 409).
func tryAcquireLakeBuildSlot() bool {
	select {
	case lakeBuildSlot <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseLakeBuildSlot frees the slot taken by tryAcquireLakeBuildSlot.
func releaseLakeBuildSlot() {
	select {
	case <-lakeBuildSlot:
	default: // defensive: never block on a double release
	}
}

// ---------------------------------------------------------------------------
// Lake-spec persistence shape
// ---------------------------------------------------------------------------

// lakeSegmentSpecJSON is the canonical JSON form of an analytics.SegmentSpec
// stored inside mailing_segments.conditions as {"lake_spec": {...}} so that
// POST /{id}/refresh can rebuild the segment later without the operator
// restating the spec. SeedListIDs are intentionally NOT persisted — they are
// re-resolved from mailing_lists at build time (the seed list set drifts).
type lakeSegmentSpecJSON struct {
	Event        string `json:"event"`
	WindowDays   int    `json:"window_days"`
	Scope        string `json:"scope"`
	BrandApex    string `json:"brand_apex,omitempty"`
	ISP          string `json:"isp,omitempty"`
	ExcludeSeeds bool   `json:"exclude_seeds,omitempty"`
}

// lakeSegmentConditions is the full conditions document for a lake segment.
type lakeSegmentConditions struct {
	LakeSpec *lakeSegmentSpecJSON `json:"lake_spec"`
}

// parseLakeSpec extracts the lake spec from a raw mailing_segments.conditions
// value. Returns nil for anything that is not a {"lake_spec":{...}} object —
// including the legacy `[...]` condition arrays every dynamic segment uses
// (json.Unmarshal of an array into a struct simply errors, which we treat as
// "not a lake segment").
func parseLakeSpec(conditionsRaw string) *analytics.SegmentSpec {
	var doc lakeSegmentConditions
	if err := json.Unmarshal([]byte(conditionsRaw), &doc); err != nil || doc.LakeSpec == nil {
		return nil
	}
	ls := doc.LakeSpec
	return &analytics.SegmentSpec{
		Event:        ls.Event,
		WindowDays:   ls.WindowDays,
		Scope:        ls.Scope,
		BrandApex:    ls.BrandApex,
		ISP:          ls.ISP,
		ExcludeSeeds: ls.ExcludeSeeds,
	}
}

// ---------------------------------------------------------------------------
// Delta guard (pure arithmetic, unit-tested)
// ---------------------------------------------------------------------------

// lakeDeltaGuardMinPrior / lakeDeltaGuardMaxPct: the guard only arms once a
// segment is established (>10k members) and trips when membership moves more
// than ±50% versus the ledger's prior count — the same semantics as the
// Python job's --max-delta-pct safety rail.
const (
	lakeDeltaGuardMinPrior = 10000
	lakeDeltaGuardMaxPct   = 50.0
)

// lakeDeltaBlocked reports whether the delta guard blocks a write of newCount
// over prior. force bypasses the guard entirely. Percentages come from the
// ledger's computeLedgerDeltaPct (segment_ledger.go) so the guard and the
// recorded last_delta_pct can never disagree.
func lakeDeltaBlocked(prior, newCount int64, force bool) bool {
	if force || prior <= lakeDeltaGuardMinPrior {
		return false
	}
	return math.Abs(computeLedgerDeltaPct(prior, newCount)) > lakeDeltaGuardMaxPct
}

// lakeEmptyBlocked is the empty-result rail (the Go mirror of the Python
// job's --allow-empty flag): a build that returns 0 members never overwrites
// an existing membership unless the operator forces it. Unlike the delta
// guard it applies regardless of prior size — wiping even a small segment to
// zero is almost always an upstream query/lake problem, not intent.
func lakeEmptyBlocked(newCount int64, force bool) bool {
	return newCount == 0 && !force
}

// ---------------------------------------------------------------------------
// Build orchestration
// ---------------------------------------------------------------------------

// lakeBuildTimeout bounds one background build end-to-end (Athena poll +
// member write).
const lakeBuildTimeout = 20 * time.Minute

// errLakeBuildAlreadyRunning is returned by BuildLakeSegment when the ledger
// reports a fresh 'running' build for the segment.
var errLakeBuildAlreadyRunning = fmt.Errorf("build already running")

// lakeBuildInFlight reports whether the ledger shows a FRESH 'running' build
// for the segment (updated within ledgerRunningStaleAfter — a staler
// 'running' row is a crashed build and must not block, matching
// coerceLedgerStatus in segment_ledger.go). Fail-open: a missing row or a
// read error reads as "not in flight" so the ledger never wedges builds.
func lakeBuildInFlight(ctx context.Context, db *sql.DB, segmentID string) bool {
	var status string
	var updatedAt time.Time
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(last_build_status, ''), COALESCE(updated_at, NOW())
		  FROM mailing_segment_build_ledger
		 WHERE segment_id = $1::uuid
	`, segmentID).Scan(&status, &updatedAt)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[LakeSegmentBuilder] ledger read failed for %s (fail-open): %v", safePrefix(segmentID, 12), err)
		}
		return false
	}
	return status == "running" && time.Since(updatedAt) < ledgerRunningStaleAfter
}

// fetchSeedListIDs mirrors the Python job's fetch_seed_list_ids: the ids of
// every mailing list whose name contains "seed" (tiny mailing_lists scan).
func fetchSeedListIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id::text FROM mailing_lists WHERE name ILIKE '%seed%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BuildLakeSegment runs one lake build end-to-end: ledger precondition, mark
// running, Athena member query, delta guard, transactional member swap,
// ledger outcome. The CALLER must hold the single build slot (and release it)
// — this function does not touch lakeBuildSlot so the build-request endpoint
// can run several builds sequentially under one acquisition.
func BuildLakeSegment(ctx context.Context, db *sql.DB, segmentID string, spec analytics.SegmentSpec, force bool) error {
	start := time.Now()
	const source = "lake-builder"

	// (a) Ledger precondition: a fresh 'running' row means another build (this
	// process or the nightly chain) is mid-flight — skip rather than stack.
	if lakeBuildInFlight(ctx, db, segmentID) {
		return fmt.Errorf("%w for segment %s (fresh 'running' ledger row)", errLakeBuildAlreadyRunning, segmentID)
	}
	// Capture the delta-guard baseline BEFORE marking running (MarkRunning
	// preserves subscriber_count, but reading first keeps the order obvious).
	prior, _ := ledgerPriorCount(ctx, db, segmentID)

	// (b) Mark running. Ledger writes are observability, never control flow.
	if err := MarkSegmentBuildRunning(ctx, db, segmentID, source); err != nil {
		log.Printf("[LakeSegmentBuilder] mark-running failed for %s (continuing): %v", safePrefix(segmentID, 12), err)
	}

	fail := func(err error) error {
		durMs := int(time.Since(start).Milliseconds())
		if lerr := UpsertSegmentLedger(ctx, db, segmentID, prior, source, "failed", durMs, 0, err.Error()); lerr != nil {
			log.Printf("[LakeSegmentBuilder] ledger failed-write error for %s: %v", safePrefix(segmentID, 12), lerr)
		}
		return err
	}

	// Seed exclusion mirrors the Python job: resolve the seed list ids fresh
	// at build time and hand them to the analytics layer.
	if spec.ExcludeSeeds {
		ids, err := fetchSeedListIDs(ctx, db)
		if err != nil {
			return fail(fmt.Errorf("resolve seed lists: %w", err))
		}
		spec.SeedListIDs = ids
	}

	// (c) Athena member query.
	members, err := analytics.BuildSegmentMembers(ctx, spec)
	if err != nil {
		return fail(fmt.Errorf("lake member query: %w", err))
	}
	newCount := int64(len(members))
	deltaPct := computeLedgerDeltaPct(prior, newCount)

	// (d0) Empty-result rail: never write an empty membership unless forced
	// (mirrors the Python job's --allow-empty). Members are untouched.
	if lakeEmptyBlocked(newCount, force) {
		durMs := int(time.Since(start).Milliseconds())
		msg := "build returned 0 members — pass force=true to write an empty segment"
		if lerr := UpsertSegmentLedger(ctx, db, segmentID, prior, source, "blocked_delta", durMs, deltaPct, msg); lerr != nil {
			log.Printf("[LakeSegmentBuilder] ledger blocked-write error for %s: %v", safePrefix(segmentID, 12), lerr)
		}
		return fmt.Errorf("%s", msg)
	}

	// (d) Delta guard: refuse to swap an established segment's membership by
	// more than ±50% unless the operator forces it.
	if lakeDeltaBlocked(prior, newCount, force) {
		durMs := int(time.Since(start).Milliseconds())
		msg := fmt.Sprintf(
			"delta guard: new membership %d differs from prior %d by %+.1f%% (limit ±%.0f%%); rerun with force=true to override",
			newCount, prior, deltaPct, lakeDeltaGuardMaxPct)
		if lerr := UpsertSegmentLedger(ctx, db, segmentID, prior, source, "blocked_delta", durMs, deltaPct, msg); lerr != nil {
			log.Printf("[LakeSegmentBuilder] ledger blocked-write error for %s: %v", safePrefix(segmentID, 12), lerr)
		}
		return fmt.Errorf("%s", msg)
	}

	// (e) Transactional member swap.
	if err := writeLakeSegmentMembers(ctx, db, segmentID, members); err != nil {
		return fail(fmt.Errorf("write members: %w", err))
	}

	// (f) Ledger outcome.
	durMs := int(time.Since(start).Milliseconds())
	if lerr := UpsertSegmentLedger(ctx, db, segmentID, newCount, source, "ok", durMs, deltaPct, ""); lerr != nil {
		log.Printf("[LakeSegmentBuilder] ledger ok-write error for %s: %v", safePrefix(segmentID, 12), lerr)
	}
	log.Printf("[LakeSegmentBuilder] built %s — %d members (prior %d, %+.1f%%) in %dms",
		safePrefix(segmentID, 12), newCount, prior, deltaPct, durMs)
	return nil
}

// writeLakeSegmentMembers swaps a segment's membership in ONE transaction:
// pg_advisory_xact_lock(hashtext(segment_id)) — the same per-segment key the
// nightly Fargate chain takes, so an in-platform build can never interleave
// its DELETE/INSERT with the nightly refresh — then DELETE + COPY
// (house convention: bulk ops use Postgres COPY via pq.CopyIn) + cached-count
// refresh on mailing_segments (the app user can UPDATE rows there, just not
// ALTER the apex_admin-owned table).
func writeLakeSegmentMembers(ctx context.Context, db *sql.DB, segmentID string, members []analytics.SegmentMember) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, segmentID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM mailing_segment_members WHERE segment_id = $1::uuid`, segmentID); err != nil {
		return fmt.Errorf("delete old members: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(
		"mailing_segment_members", "segment_id", "subscriber_id", "email", "materialized_at"))
	if err != nil {
		return fmt.Errorf("prepare copy: %w", err)
	}
	now := time.Now().UTC()
	// COPY has no ON CONFLICT: dedupe by subscriber_id in-process so the
	// (segment_id, subscriber_id) PK can never abort the whole batch.
	seen := make(map[string]bool, len(members))
	written := 0
	for _, m := range members {
		if m.SubscriberID == "" || m.Email == "" || seen[m.SubscriberID] {
			continue
		}
		seen[m.SubscriberID] = true
		if _, err := stmt.ExecContext(ctx, segmentID, m.SubscriberID, strings.ToLower(m.Email), now); err != nil {
			_ = stmt.Close()
			return fmt.Errorf("copy member: %w", err)
		}
		written++
	}
	// Final Exec with no args flushes the COPY buffer (lib/pq convention).
	if _, err := stmt.ExecContext(ctx); err != nil {
		_ = stmt.Close()
		return fmt.Errorf("flush copy: %w", err)
	}
	if err := stmt.Close(); err != nil {
		return fmt.Errorf("close copy: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_segments
		   SET subscriber_count = $1, last_calculated_at = NOW(), updated_at = NOW()
		 WHERE id = $2::uuid
	`, written, segmentID); err != nil {
		return fmt.Errorf("update cached count: %w", err)
	}

	return tx.Commit()
}

// runLakeBuildLocked executes one build under an already-held slot, with its
// own background timeout and panic recovery into the ledger.
func runLakeBuildLocked(db *sql.DB, segmentID string, spec analytics.SegmentSpec, force bool) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[LakeSegmentBuilder] PANIC building %s: %v", safePrefix(segmentID, 12), p)
			rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Carry the prior ledger count forward — a panic must not zero
			// out the displayed audience size (the upsert also keeps the
			// stored count on non-'ok' status; this matters for the
			// first-ever-build INSERT path).
			prior, _ := ledgerPriorCount(rctx, db, segmentID)
			_ = UpsertSegmentLedger(rctx, db, segmentID, prior, "lake-builder", "failed", 0, 0,
				fmt.Sprintf("panic: %v", p))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), lakeBuildTimeout)
	defer cancel()
	if err := BuildLakeSegment(ctx, db, segmentID, spec, force); err != nil {
		log.Printf("[LakeSegmentBuilder] build failed for %s: %v", safePrefix(segmentID, 12), err)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers (registered in SegmentationAPI.RegisterRoutes)
// ---------------------------------------------------------------------------

// RefreshSegment — POST /api/mailing/v2/segments/{segmentID}/refresh?force=
//
// Lake-spec segments (conditions carries {"lake_spec":{...}}): starts an
// async build through the single slot.
//
//	202 {"status":"started","mode":"lake","segment_id":"<uuid>"}
//	409 {"error":"a lake build is already running"}     (slot busy OR ledger fresh-running)
//	503 {"error":"analytics lake reader disabled"}
//
// Dynamic segments: synchronous MaterializeSegment, identical behavior to the
// existing POST /recalculate.
//
//	200 {"status":"ok","mode":"materializer","count":N}
func (api *SegmentationAPI) RefreshSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	segmentID, parseErr := uuid.Parse(chi.URLParam(r, "segmentID"))
	if parseErr != nil {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid segment id"})
		return
	}

	segment, err := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if err != nil {
		segmentRespondJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	if segment == nil {
		segmentRespondJSONStatus(w, http.StatusNotFound, map[string]interface{}{"error": "segment not found"})
		return
	}

	var conditionsRaw sql.NullString
	if err := api.db.QueryRowContext(ctx,
		`SELECT COALESCE(conditions::text, '[]') FROM mailing_segments WHERE id = $1`, segmentID,
	).Scan(&conditionsRaw); err != nil {
		segmentRespondJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	if spec := parseLakeSpec(conditionsRaw.String); spec != nil {
		if !analytics.ReaderEnabled() {
			segmentRespondJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{
				"error": "analytics lake reader disabled",
			})
			return
		}
		force := r.URL.Query().Get("force") == "true"

		// 409 when the ledger says a build is already mid-flight (covers
		// builds owned by OTHER writers, e.g. the nightly chain) ...
		if lakeBuildInFlight(ctx, api.db, segmentID.String()) {
			segmentRespondJSONStatus(w, http.StatusConflict, map[string]interface{}{
				"error": "a lake build is already running",
			})
			return
		}
		// ... and when this process's single slot is busy.
		if !tryAcquireLakeBuildSlot() {
			segmentRespondJSONStatus(w, http.StatusConflict, map[string]interface{}{
				"error": "a lake build is already running",
			})
			return
		}

		db, sid, sp := api.db, segmentID.String(), *spec
		go func() {
			defer releaseLakeBuildSlot()
			runLakeBuildLocked(db, sid, sp, force)
		}()

		segmentRespondJSONStatus(w, http.StatusAccepted, map[string]interface{}{
			"status":     "started",
			"mode":       "lake",
			"segment_id": segmentID,
		})
		return
	}

	// Dynamic segment: existing synchronous recalculate behavior.
	listIDStr := ""
	if segment.ListID != nil {
		listIDStr = segment.ListID.String()
	}
	matCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	count, err := MaterializeSegment(matCtx, api.db, segmentID.String(), listIDStr, conditionsRaw.String)
	if err != nil {
		log.Printf("[Segment] refresh (materializer) failed for %s: %v", segmentID, err)
		segmentRespondJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":      "refresh_failed",
			"detail":     err.Error(),
			"segment_id": segmentID,
		})
		return
	}
	if updateErr := api.engine.Store().UpdateSegmentCount(ctx, segmentID, count); updateErr != nil {
		log.Printf("[Segment] update cached count after refresh failed for %s: %v", segmentID, updateErr)
	}
	segmentRespondJSON(w, map[string]interface{}{
		"status": "ok",
		"mode":   "materializer",
		"count":  count,
	})
}

// lakeBuildRequest is the body of POST /api/mailing/v2/segments/build-request.
type lakeBuildRequest struct {
	Event        string `json:"event"`
	Windows      []int  `json:"windows"`
	Scope        string `json:"scope"`
	BrandApex    string `json:"brand_apex"`
	ISP          string `json:"isp"`
	ExcludeSeeds bool   `json:"exclude_seeds"`
	Name         string `json:"name"`
}

// maxLakeBuildWindows caps how many segment rows (and sequential Athena
// builds) one build-request may enqueue.
const maxLakeBuildWindows = 8

// lakeCategoryForScope maps a lake scope onto the canonical
// validSegmentCategories enum (mailing_segments.go).
var lakeCategoryForScope = map[string]string{
	analytics.SegmentScopeGlobal: "engagement_global",
	analytics.SegmentScopeBrand:  "engagement_brand",
	analytics.SegmentScopeISP:    "engagement_isp",
}

// lakeSegmentName generates the segment name for one window:
// 'Lake-Open-14d-Global', 'Lake-Click-7d-discountblog.com',
// 'Lake-Open-30d-gmail' — or '<base>-14d' when the operator supplied a base.
func lakeSegmentName(base string, spec analytics.SegmentSpec) string {
	if base != "" {
		return fmt.Sprintf("%s-%dd", base, spec.WindowDays)
	}
	// Capitalize the (whitelisted, ASCII) event token: open -> Open.
	event := strings.ToUpper(spec.Event[:1]) + spec.Event[1:]
	suffix := "Global"
	switch spec.Scope {
	case analytics.SegmentScopeBrand:
		suffix = spec.BrandApex
	case analytics.SegmentScopeISP:
		suffix = spec.ISP
	}
	return fmt.Sprintf("Lake-%s-%dd-%s", event, spec.WindowDays, suffix)
}

// LakeSegmentBuildRequest — POST /api/mailing/v2/segments/build-request
//
// Body:
//
//	{"event":"open|click","windows":[7,14],"scope":"global|brand|isp",
//	 "brand_apex":"...","isp":"...","exclude_seeds":true,"name":"optional base name"}
//
// Creates ONE static mailing_segments row per window (conditions =
// {"lake_spec":{...}}) and builds them sequentially through the single slot.
//
//	201 {"segments":[{"id":"...","name":"...","window_days":7},...],"building":true}
//	400 on validation failure, 409 when the slot is busy, 503 reader disabled.
//
// Deliberately NO refresh-all endpoint — the nightly chain owns bulk refresh.
func (api *SegmentationAPI) LakeSegmentBuildRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var req lakeBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid request body"})
		return
	}

	if len(req.Windows) == 0 {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{"error": "windows is required (e.g. [7,14])"})
		return
	}
	if len(req.Windows) > maxLakeBuildWindows {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error": fmt.Sprintf("max %d windows per request", maxLakeBuildWindows),
		})
		return
	}

	// Dedupe windows preserving order; reject (don't silently clamp) values an
	// operator typed wrong.
	seen := make(map[int]bool, len(req.Windows))
	windows := make([]int, 0, len(req.Windows))
	for _, wd := range req.Windows {
		if wd < 1 || wd > 120 {
			segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
				"error": fmt.Sprintf("invalid window %d: must be 1..120 days", wd),
			})
			return
		}
		if seen[wd] {
			continue
		}
		seen[wd] = true
		windows = append(windows, wd)
	}

	// Validate event/scope/brand/isp once via the analytics whitelist (the
	// same validation the builder re-runs before SQL construction).
	baseSpec := analytics.SegmentSpec{
		Event:        strings.ToLower(strings.TrimSpace(req.Event)),
		WindowDays:   windows[0],
		Scope:        strings.ToLower(strings.TrimSpace(req.Scope)),
		BrandApex:    strings.ToLower(strings.TrimSpace(req.BrandApex)),
		ISP:          strings.ToLower(strings.TrimSpace(req.ISP)),
		ExcludeSeeds: req.ExcludeSeeds,
	}
	if err := analytics.ValidateSegmentSpec(baseSpec); err != nil {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	if !analytics.ReaderEnabled() {
		segmentRespondJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "analytics lake reader disabled",
		})
		return
	}

	category := lakeCategoryForScope[baseSpec.Scope]
	if !validSegmentCategories[category] { // defense in depth vs enum drift
		segmentRespondJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
			"error": fmt.Sprintf("category %q is not in the canonical segment-category enum", category),
		})
		return
	}

	// Take the single slot BEFORE creating rows: builds enqueue sequentially
	// through it, and a busy slot must 409 without leaving half-built rows.
	if !tryAcquireLakeBuildSlot() {
		segmentRespondJSONStatus(w, http.StatusConflict, map[string]interface{}{
			"error": "a lake build is already running",
		})
		return
	}

	type createdSeg struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		WindowDays int    `json:"window_days"`
	}
	type buildJob struct {
		id   string
		spec analytics.SegmentSpec
	}
	created := make([]createdSeg, 0, len(windows))
	jobs := make([]buildJob, 0, len(windows))

	for _, wd := range windows {
		spec := baseSpec
		spec.WindowDays = wd
		name := lakeSegmentName(strings.TrimSpace(req.Name), spec)

		condDoc := lakeSegmentConditions{LakeSpec: &lakeSegmentSpecJSON{
			Event:        spec.Event,
			WindowDays:   spec.WindowDays,
			Scope:        spec.Scope,
			BrandApex:    spec.BrandApex,
			ISP:          spec.ISP,
			ExcludeSeeds: spec.ExcludeSeeds,
		}}
		conditionsJSON, err := json.Marshal(condDoc)
		if err != nil {
			releaseLakeBuildSlot()
			segmentRespondJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
			return
		}

		// Row shape mirrors segmentation.Store.CreateSegment (same column
		// set/defaults: status 'active', calculation_mode 'batch', no list
		// scope) with segment_type='static' — membership is written only by
		// the lake builder, never by the dynamic-condition evaluator.
		id := uuid.New()
		description := fmt.Sprintf("Lake-built recency segment (%s, last %dd, scope %s)",
			spec.Event, spec.WindowDays, spec.Scope)
		if _, err := api.db.ExecContext(ctx, `
			INSERT INTO mailing_segments (
				id, organization_id, list_id, name, description, segment_type, category,
				conditions, calculation_mode, refresh_interval_minutes, include_suppressed,
				global_exclusion_rules, status, created_at, updated_at
			) VALUES ($1, $2, NULL, $3, $4, 'static', $5, $6, 'batch', 0, false, '[]', 'active', NOW(), NOW())
		`, id, orgID, name, description, category, string(conditionsJSON)); err != nil {
			releaseLakeBuildSlot()
			log.Printf("[LakeSegmentBuilder] create segment row failed (%s): %v", name, err)
			segmentRespondJSONStatus(w, http.StatusInternalServerError, map[string]interface{}{
				"error":            "create segment failed: " + err.Error(),
				"segments_created": created,
			})
			return
		}
		created = append(created, createdSeg{ID: id.String(), Name: name, WindowDays: wd})
		jobs = append(jobs, buildJob{id: id.String(), spec: spec})
	}

	db := api.db
	go func() {
		defer releaseLakeBuildSlot()
		for _, j := range jobs {
			runLakeBuildLocked(db, j.id, j.spec, false)
		}
	}()

	segmentRespondJSONStatus(w, http.StatusCreated, map[string]interface{}{
		"segments": created,
		"building": true,
	})
}

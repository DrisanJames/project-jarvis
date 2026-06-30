package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
	"github.com/lib/pq"
)

// SegmentationAPI handles segmentation endpoints
type SegmentationAPI struct {
	engine *segmentation.Engine
	db     *sql.DB
}

// NewSegmentationAPI creates a new segmentation API handler
func NewSegmentationAPI(db *sql.DB) *SegmentationAPI {
	return &SegmentationAPI{
		engine: segmentation.NewEngine(db),
		db:     db,
	}
}

// VersionSegmentationAPI is the response version surfaced on every list/count
// payload so the UI and operators can confirm what they are talking to.
//
// 2.4.0 — server-side list filtering. ListSegments gains optional query params
//
//	q (case-insensitive name substring, ILIKE with %/_/\ escaped),
//	status (active|archived|inactive|all; default unchanged = any
//	non-deleted), categories / exclude_categories (comma lists matched
//	against COALESCE(category,'uncategorized')), limit (1..2000, clamped)
//	and offset (>=0). All filtering happens in SQL with bound parameters.
//	include_counts=1 switches the response from the legacy bare array to
//	{"segments":[...], "total":N, "category_counts":{...}, "api_version":...}
//	where total honors every filter (ignoring limit/offset) and
//	category_counts honors q/status but ignores category filters. Requests
//	with NO params return exactly the 2.3.0 payload (bare array, all
//	non-deleted segments, no LIMIT).
//
// 2.3.0 — segment build ledger. ListSegments and GetSegmentCount now read the
//
//	app-owned mailing_segment_build_ledger summary table instead of
//	running COUNT(*) over mailing_segment_members. ListSegments rows
//	gain last_built_at, build_source, last_build_status (stale 'running'
//	coerced to 'failed'), last_build_ms and last_delta_pct; existing
//	keys (audience_count / audience_source / materialized_count /
//	materialized_at) are unchanged, falling back to the segment row's
//	cached subscriber_count (audience_source 'cached') when no ledger
//	row exists. GetSegmentCount reports audience_source 'ledger' when
//	served from the ledger.
//
// 2.2.0 — materialized rollup read path (mailing_segment_members), explicit
//
//	/recalculate action.
const VersionSegmentationAPI = "2.4.0"

// RegisterRoutes registers segmentation routes under /api/mailing/v2
func (api *SegmentationAPI) RegisterRoutes(r chi.Router) {
	r.Route("/v2/segments", func(r chi.Router) {
		r.Get("/", api.ListSegments)
		r.Post("/", api.CreateSegment)
		r.Post("/preview", api.PreviewSegment)

		r.Route("/{segmentID}", func(r chi.Router) {
			r.Get("/", api.GetSegment)
			r.Put("/", api.UpdateSegment)
			r.Delete("/", api.DeleteSegment)
			r.Get("/count", api.GetSegmentCount)           // Cheap materialized count
			r.Post("/recalculate", api.RecalculateSegment) // Synchronous re-materialize
			// Lake-aware refresh (lake_segment_builder.go): async Athena
			// build (202, single slot) for {"lake_spec":...} segments,
			// synchronous materialize (200) for dynamic segments.
			r.Post("/refresh", api.RefreshSegment)
			r.Post("/execute", api.ExecuteSegment)
			r.Post("/snapshot", api.CreateSnapshot)
			r.Get("/subscribers", api.GetSegmentSubscribers)
			// Bulk-export emails from the materialized rollup
			// (mailing_segment_members). Cheap, indexed, single-query
			// — does NOT touch mailing_subscribers so it's safe during
			// active sending hours.
			r.Get("/members.csv", api.ExportSegmentMembersCSV)
		})

		// UNION export across many segments, deduped by lower(email).
		// Use case: "give me one CSV of every email currently in any
		// of these N segments" for hygiene runs against a verifier.
		r.Post("/export.csv", api.ExportSegmentsUnionCSV)

		// Create + build standard lake recency segments (one static
		// segment per requested window, built sequentially through the
		// single lake-build slot). See lake_segment_builder.go.
		r.Post("/build-request", api.LakeSegmentBuildRequest)

		// Run-level observability for the segment workers (heartbeats +
		// mailing_worker_runs + lake-chain ledger stats). Read-only, safe
		// to poll. See segment_workers_handlers.go.
		r.Get("/workers", api.GetSegmentWorkers)
	})

	r.Route("/v2/snapshots/{snapshotID}", func(r chi.Router) {
		r.Get("/", api.GetSnapshot)
		r.Get("/subscribers", api.GetSnapshotSubscribers)
	})

	r.Route("/v2/events", func(r chi.Router) {
		r.Post("/track", api.TrackEvent)
		r.Post("/batch", api.TrackEventsBatch)
	})

	r.Route("/v2/contact-fields", func(r chi.Router) {
		r.Get("/", api.ListContactFields)
		r.Post("/", api.CreateContactField)
	})

	r.Get("/v2/operators", api.ListOperators)
}

// ==========================================
// SEGMENT HANDLERS
// ==========================================

// CreateSegmentRequest is the request body for creating a segment
type CreateSegmentRequest struct {
	Name              string                             `json:"name"`
	Description       string                             `json:"description,omitempty"`
	ListID            *uuid.UUID                         `json:"list_id,omitempty"`
	CalculationMode   string                             `json:"calculation_mode,omitempty"`
	IncludeSuppressed bool                               `json:"include_suppressed"`
	Category          string                             `json:"category,omitempty"` // enum: see validSegmentCategories; falls back to 'uncategorized'
	RootGroup         segmentation.ConditionGroupBuilder `json:"root_group"`
	GlobalExclusions  []segmentation.ConditionBuilder    `json:"global_exclusions,omitempty"`
}

// segmentCategoryToken validates category filter values. We deliberately do
// NOT validate against validSegmentCategories here: that map is the enum for
// *writes* (CreateSegment/UpdateSegment), but production data is wider — real
// rows carry categories like 'engaged-model' and 'data-partner' that predate
// the enum, and filtering must be able to target them. Injection is impossible
// regardless (values are bound parameters), so this regex only rejects
// obvious garbage (spaces, quotes, empty after trim) with a clear 400 instead
// of a silent empty result.
var segmentCategoryToken = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// segmentListStatuses are the accepted values for ListSegments' status param.
// "" (absent) and "all" both reproduce the legacy predicate status != 'deleted'.
var segmentListStatuses = map[string]bool{
	"":         true,
	"all":      true,
	"active":   true,
	"archived": true,
	"inactive": true,
}

const segmentListMaxLimit = 2000

// parseSegmentListFilter parses ListSegments' optional query params into a
// store filter. It returns includeCounts (include_counts=1|true) and a
// non-empty errMsg when a param is invalid (caller responds 400). Absent
// params yield the zero filter = exact legacy behavior.
func parseSegmentListFilter(q url.Values) (f segmentation.SegmentListFilter, includeCounts bool, errMsg string) {
	f.NameQuery = strings.TrimSpace(q.Get("q"))

	f.Status = strings.ToLower(strings.TrimSpace(q.Get("status")))
	if !segmentListStatuses[f.Status] {
		return f, false, "status must be one of: active, archived, inactive, all"
	}

	parseCategories := func(param string) ([]string, string) {
		raw := q.Get(param)
		if strings.TrimSpace(raw) == "" {
			return nil, ""
		}
		var out []string
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if !segmentCategoryToken.MatchString(tok) {
				return nil, fmt.Sprintf("invalid %s value %q: categories must match ^[a-zA-Z0-9_-]+$", param, tok)
			}
			out = append(out, tok)
		}
		return out, ""
	}

	if f.Categories, errMsg = parseCategories("categories"); errMsg != "" {
		return f, false, errMsg
	}
	if f.ExcludeCategories, errMsg = parseCategories("exclude_categories"); errMsg != "" {
		return f, false, errMsg
	}

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return f, false, "limit must be an integer"
		}
		// Clamp into [1, segmentListMaxLimit] rather than erroring; absent
		// limit keeps the legacy unlimited behavior.
		if n < 1 {
			n = 1
		}
		if n > segmentListMaxLimit {
			n = segmentListMaxLimit
		}
		f.Limit = n
	}

	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return f, false, "offset must be an integer"
		}
		if n < 0 {
			n = 0
		}
		f.Offset = n
	}

	switch q.Get("include_counts") {
	case "1", "true":
		includeCounts = true
	}

	return f, includeCounts, ""
}

// ListSegments returns all segments for the organization with a materialized
// audience snapshot per segment (count + freshness). The hot read path NEVER
// touches mailing_subscribers OR mailing_segment_members; counts come from the
// app-owned mailing_segment_build_ledger summary table (one tiny row per
// segment, upserted by every member-writing path — materializer, recalculate,
// lake builders). This keeps the segments page safe to load during 24/7 send
// hours even as the members rollup grows.
//
// Optional query params (2.4.0) — ALL filtering happens in SQL so the
// machine-generated bulk (~31k partner_wave_static rows) never leaves the DB:
//
//	q=                  case-insensitive substring on name (ILIKE, %/_/\ escaped)
//	status=             active|archived|inactive|all (absent/all = legacy: any non-deleted)
//	categories=         comma list, include-only on COALESCE(category,'uncategorized')
//	exclude_categories= comma list, exclusion filter
//	limit=              1..2000 (clamped); absent = no LIMIT (legacy)
//	offset=             >=0; absent = 0
//	include_counts=1    wrap response in an envelope with totals (see below)
//
// Requests with NO params behave exactly as before (bare array, every
// non-deleted segment).
//
// Response shape (JSON array; with include_counts=1 the array moves under
// "segments" in an envelope that adds "total" — count matching all filters,
// ignoring limit/offset — "category_counts" — per-category counts honoring
// q/status but ignoring category filters — and "api_version"):
//
//	[
//	  {
//	    ...all *segmentation.Segment fields...,
//	    "materialized_count": 17234,
//	    "materialized_at":    "2026-04-27T11:02:13Z",
//	    "audience_count":     17234,         // displayable number — ledger when present, cached fallback
//	    "audience_source":    "materialized", // or "cached"
//	    "last_built_at":      "2026-06-09T04:12:01Z", // RFC3339 or null
//	    "build_source":       "materializer",         // string or null
//	    "last_build_status":  "ok",   // 'running'|'ok'|'failed'|'blocked_delta'|'unknown'; stale 'running' coerced to 'failed'
//	    "last_build_ms":      48211,  // int or null
//	    "last_delta_pct":     -2.4    // float or null
//	  }, ...
//	]
//
// The previous implementation spawned one goroutine per zero-count segment on
// every page load to run a heavy COUNT(DISTINCT) over mailing_subscribers.
// Under sending load that herd timed out and added DB pressure for no benefit;
// it has been removed. Recalculation is now an explicit user action via
// POST /v2/segments/{id}/recalculate.
func (api *SegmentationAPI) ListSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var listID *uuid.UUID
	if listIDStr := r.URL.Query().Get("list_id"); listIDStr != "" {
		if id, err := uuid.Parse(listIDStr); err == nil {
			listID = &id
		}
	}

	filter, includeCounts, errMsg := parseSegmentListFilter(r.URL.Query())
	if errMsg != "" {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "invalid_filter",
			"details": errMsg,
		})
		return
	}

	segments, err := api.engine.Store().ListSegmentsFiltered(ctx, orgID, listID, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if segments == nil {
		segments = []*segmentation.Segment{}
	}

	// One batched point-read of the build ledger for ALL returned segment ids.
	// No aggregate, no joins, no touch of mailing_segment_members.
	ledger := api.fetchSegmentLedgerRows(ctx, segments)
	now := time.Now().UTC()

	type segmentRow struct {
		*segmentation.Segment
		MaterializedCount *int64     `json:"materialized_count,omitempty"`
		MaterializedAt    *time.Time `json:"materialized_at,omitempty"`
		AudienceCount     int64      `json:"audience_count"`
		AudienceSource    string     `json:"audience_source"`
		LastBuiltAt       *time.Time `json:"last_built_at"`
		BuildSource       *string    `json:"build_source"`
		LastBuildStatus   *string    `json:"last_build_status"`
		LastBuildMs       *int       `json:"last_build_ms"`
		LastDeltaPct      *float64   `json:"last_delta_pct"`
	}

	rows := make([]segmentRow, 0, len(segments))
	for _, seg := range segments {
		row := segmentRow{Segment: seg}
		led, hasLedger := ledger[seg.ID.String()]
		if hasLedger && led.lastBuiltAt.Valid {
			// At least one completed build — the ledger count is authoritative.
			c := led.subscriberCount
			t := led.lastBuiltAt.Time
			row.MaterializedCount = &c
			row.MaterializedAt = &t
			row.AudienceCount = c
			row.AudienceSource = "materialized"
		} else {
			// No ledger row (or a first build is still in flight and has never
			// completed): fall back to the segment row's cached subscriber_count.
			row.AudienceCount = int64(seg.SubscriberCount)
			row.AudienceSource = "cached"
		}
		if hasLedger {
			if led.lastBuiltAt.Valid {
				t := led.lastBuiltAt.Time
				row.LastBuiltAt = &t
			}
			if led.buildSource.Valid {
				s := led.buildSource.String
				row.BuildSource = &s
			}
			status := coerceLedgerStatus(led.status, led.updatedAt, now)
			row.LastBuildStatus = &status
			if led.buildMs.Valid {
				ms := int(led.buildMs.Int64)
				row.LastBuildMs = &ms
			}
			if led.deltaPct.Valid {
				d := led.deltaPct.Float64
				row.LastDeltaPct = &d
			}
		}
		rows = append(rows, row)
	}

	if !includeCounts {
		// Legacy payload: bare JSON array, byte-compatible with 2.3.0.
		segmentRespondJSON(w, rows)
		return
	}

	store := api.engine.Store()
	total, err := store.CountSegments(ctx, orgID, listID, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	categoryCounts, err := store.CountSegmentsByCategory(ctx, orgID, listID, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, map[string]interface{}{
		"segments":        rows,
		"total":           total,
		"category_counts": categoryCounts,
		"api_version":     VersionSegmentationAPI,
	})
}

// segmentLedgerRow mirrors one mailing_segment_build_ledger row for the
// list/count read paths.
type segmentLedgerRow struct {
	subscriberCount int64
	lastBuiltAt     sql.NullTime
	buildSource     sql.NullString
	status          string
	buildMs         sql.NullInt64
	deltaPct        sql.NullFloat64
	updatedAt       time.Time
}

// fetchSegmentLedgerRows returns segment_id → ledger row for every id in
// segments, in a single indexed (PK) query. An empty map is returned when the
// list is empty or the query fails (fail-open: cached counts are shown).
func (api *SegmentationAPI) fetchSegmentLedgerRows(ctx context.Context, segments []*segmentation.Segment) map[string]segmentLedgerRow {
	out := make(map[string]segmentLedgerRow, len(segments))
	if len(segments) == 0 || api.db == nil {
		return out
	}

	ids := make([]string, 0, len(segments))
	for _, s := range segments {
		ids = append(ids, s.ID.String())
	}

	// Strict timeout protects the read from contending with active sends.
	rollupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := api.db.QueryContext(rollupCtx, `
		SELECT segment_id::text,
		       subscriber_count,
		       last_built_at,
		       build_source,
		       last_build_status,
		       last_build_ms,
		       last_delta_pct,
		       updated_at
		  FROM mailing_segment_build_ledger
		 WHERE segment_id = ANY($1::uuid[])
	`, segmentIDArray(ids))
	if err != nil {
		log.Printf("[Segment] build-ledger query error (returning cached counts): %v", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var lr segmentLedgerRow
		if err := rows.Scan(&sid, &lr.subscriberCount, &lr.lastBuiltAt, &lr.buildSource,
			&lr.status, &lr.buildMs, &lr.deltaPct, &lr.updatedAt); err != nil {
			continue
		}
		out[sid] = lr
	}
	return out
}

// segmentIDArray formats a string slice as a Postgres uuid[] literal.
// We avoid pulling in lib/pq's Array helper to keep the dependency surface stable.
func segmentIDArray(ids []string) string {
	if len(ids) == 0 {
		return "{}"
	}
	var b []byte
	b = append(b, '{')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, id...)
	}
	b = append(b, '}')
	return string(b)
}

// ==========================================
// ENGAGEMENT GROWTH BOARD
// ==========================================
//
// GET /v2/segments/engagement-growth — brand-grouped current size + last-build
// delta for the per-brand engagement segments the operator actively mails
// (category 'engagement_brand': the seeded "<CODE> <N>D <Openers|Clickers>"
// set, one per sending brand). This is the data source for the redesigned
// SegmentsCenter growth board (the operator wants "7D Openers and 30D Clickers
// by brand").
//
// DATA SOURCE: 100% reuse of the ListSegments read path — no new SQL against
// unknown tables. Store().ListSegmentsFiltered(category=engagement_brand,
// status=active) supplies the segment rows + cached subscriber_count, and
// fetchSegmentLedgerRows (the same batched mailing_segment_build_ledger point
// read ListSegments uses) supplies the authoritative materialized count,
// last_built_at and last_delta_pct.
//
// GROWTH SIGNAL: last_delta_pct is the BUILD-TO-BUILD delta (current audience
// size vs the previous build), NOT a time-windowed trend. There is no per-day
// segment-size history table today, so an honest "7-day growth" line cannot be
// computed — the UI labels this "Δ since last build".
//
// TODO(growth-history): a daily snapshot table (segment_id, day, count),
// written alongside the build-ledger upsert, would let this endpoint return
// real trend lines (true 7/30-day growth) instead of a single build delta.

// engagementBrandNameRe matches the canonical seeded per-brand engagement
// segment name "<brand> <N>D <Openers|Clickers>" (e.g. "DB 7D Openers",
// "QF 30D Clickers"). Brand is whatever precedes the window token; in
// production it is a 2-3 letter brand code (see
// may29_seg_backfill_engagement_brand in cmd/server/main.go), but capturing the
// full prefix keeps any custom-labeled rows grouping correctly.
var engagementBrandNameRe = regexp.MustCompile(`^(.+?) (7|14|30|60)D (Openers|Clickers)$`)

// engagementLakeNameRe matches the lake-builder brand form
// "Lake-Open-14d-discountblog.com" / "Lake-Click-7d-...". Brand-scoped lake
// segments also carry category engagement_brand (see lakeCategoryForScope in
// lake_segment_builder.go), so they land in this endpoint too.
var engagementLakeNameRe = regexp.MustCompile(`^Lake-(Open|Click)-(\d+)d-(.+)$`)

// parseEngagementBrandName derives (brand, cellKey) from an engagement_brand
// segment name, where cellKey is the canonical "<N>D <Openers|Clickers>" label
// used to bucket the metric. ok is false for names that don't match either
// known shape (those rows are skipped — they have no clean brand/kind).
func parseEngagementBrandName(name string) (brand string, cellKey string, ok bool) {
	if m := engagementBrandNameRe.FindStringSubmatch(name); m != nil {
		return strings.TrimSpace(m[1]), m[2] + "D " + m[3], true
	}
	if m := engagementLakeNameRe.FindStringSubmatch(name); m != nil {
		kind := "Openers"
		if m[1] == "Click" {
			kind = "Clickers"
		}
		return strings.TrimSpace(m[3]), m[2] + "D " + kind, true
	}
	return "", "", false
}

// engagementGrowthMetric is one brand × (window × kind) cell.
type engagementGrowthMetric struct {
	SegmentID string     `json:"segment_id"`
	Name      string     `json:"name"`
	Count     int64      `json:"count"`
	DeltaPct  *float64   `json:"delta_pct"` // build-to-build Δ%, null when no ledger delta
	BuiltAt   *time.Time `json:"built_at"`
	Source    string     `json:"source"` // "materialized" | "cached"
	Status    *string    `json:"status,omitempty"`
}

// engagementGrowthBrand groups every engagement cell for one brand. The four
// named fields are the headline tiles; Cells carries every parsed window/kind
// (7D/14D/30D/60D Openers/Clickers) for the expandable detail.
type engagementGrowthBrand struct {
	Brand             string                             `json:"brand"`
	SevenDayOpeners   *engagementGrowthMetric            `json:"seven_day_openers"`
	SevenDayClickers  *engagementGrowthMetric            `json:"seven_day_clickers"`
	ThirtyDayOpeners  *engagementGrowthMetric            `json:"thirty_day_openers"`
	ThirtyDayClickers *engagementGrowthMetric            `json:"thirty_day_clickers"`
	Cells             map[string]*engagementGrowthMetric `json:"cells"`
	SegmentCount      int                                `json:"segment_count"`
	HeadlineCount     int64                              `json:"headline_count"` // largest cell, for sorting
}

type engagementGrowthResponse struct {
	Brands       []engagementGrowthBrand `json:"brands"`
	BrandCount   int                     `json:"brand_count"`
	SegmentCount int                     `json:"segment_count"`
	GrowthSignal string                  `json:"growth_signal"`
	GrowthLabel  string                  `json:"growth_label"`
	Note         string                  `json:"note"`
	GeneratedAt  time.Time               `json:"generated_at"`
	APIVersion   string                  `json:"api_version"`
}

// HandleEngagementGrowth serves GET /v2/segments/engagement-growth. Read-only.
func (api *SegmentationAPI) HandleEngagementGrowth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	// Identical read path to ListSegments, scoped to the active per-brand
	// engagement set in SQL so the ~31k machine rows never leave the DB.
	filter := segmentation.SegmentListFilter{
		Status:     "active",
		Categories: []string{"engagement_brand"},
	}
	segments, err := api.engine.Store().ListSegmentsFiltered(ctx, orgID, nil, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ledger := api.fetchSegmentLedgerRows(ctx, segments)
	now := time.Now().UTC()

	brands := make(map[string]*engagementGrowthBrand)
	order := make([]string, 0)
	parsed := 0

	for _, seg := range segments {
		brand, cellKey, ok := parseEngagementBrandName(seg.Name)
		if !ok {
			continue
		}
		parsed++

		// Mirror ListSegments' count/source resolution: ledger count when a
		// build has completed, else the segment row's cached subscriber_count.
		metric := &engagementGrowthMetric{
			SegmentID: seg.ID.String(),
			Name:      seg.Name,
			Count:     int64(seg.SubscriberCount),
			Source:    "cached",
		}
		if led, has := ledger[seg.ID.String()]; has {
			if led.lastBuiltAt.Valid {
				metric.Count = led.subscriberCount
				metric.Source = "materialized"
				t := led.lastBuiltAt.Time
				metric.BuiltAt = &t
			}
			if led.deltaPct.Valid {
				d := led.deltaPct.Float64
				metric.DeltaPct = &d
			}
			st := coerceLedgerStatus(led.status, led.updatedAt, now)
			metric.Status = &st
		}

		b := brands[brand]
		if b == nil {
			b = &engagementGrowthBrand{Brand: brand, Cells: make(map[string]*engagementGrowthMetric)}
			brands[brand] = b
			order = append(order, brand)
		}
		// Collisions shouldn't happen among active rows; if they do, prefer the
		// row that has actually been built.
		if existing := b.Cells[cellKey]; existing == nil || (existing.BuiltAt == nil && metric.BuiltAt != nil) {
			b.Cells[cellKey] = metric
		}
		b.SegmentCount++

		switch cellKey {
		case "7D Openers":
			b.SevenDayOpeners = b.Cells[cellKey]
		case "7D Clickers":
			b.SevenDayClickers = b.Cells[cellKey]
		case "30D Openers":
			b.ThirtyDayOpeners = b.Cells[cellKey]
		case "30D Clickers":
			b.ThirtyDayClickers = b.Cells[cellKey]
		}
	}

	result := make([]engagementGrowthBrand, 0, len(order))
	for _, name := range order {
		b := brands[name]
		var headline int64
		for _, c := range b.Cells {
			if c.Count > headline {
				headline = c.Count
			}
		}
		b.HeadlineCount = headline
		result = append(result, *b)
	}
	// Largest brand first (by biggest active engagement cell); name tiebreak.
	sort.Slice(result, func(i, j int) bool {
		if result[i].HeadlineCount != result[j].HeadlineCount {
			return result[i].HeadlineCount > result[j].HeadlineCount
		}
		return result[i].Brand < result[j].Brand
	})

	segmentRespondJSON(w, engagementGrowthResponse{
		Brands:       result,
		BrandCount:   len(result),
		SegmentCount: parsed,
		GrowthSignal: "last_delta_pct",
		GrowthLabel:  "Δ since last build",
		Note:         "delta_pct is the build-to-build change in audience size (current vs previous build), not a time-windowed trend; no per-day segment-size history exists yet.",
		GeneratedAt:  now,
		APIVersion:   VersionSegmentationAPI,
	})
}

// CreateSegment creates a new segment
func (api *SegmentationAPI) CreateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	userID := segmentGetUserIDFromContext(ctx)

	var req CreateSegmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate conditions
	errors := api.engine.ValidateConditions(req.RootGroup)
	if len(errors) > 0 {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "validation_failed",
			"details": errors,
		})
		return
	}

	// Build exclusions JSON
	exclusionsJSON, _ := json.Marshal(req.GlobalExclusions)

	// Category validation mirrors mailing_segments.go's validSegmentCategories
	// map. Reject unknown values rather than silently defaulting so the wizard
	// surfaces typos before they reach storage and clutter the catalog.
	category := req.Category
	if category != "" && !validSegmentCategories[category] {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "invalid_category",
			"details": "category must be one of: engagement_brand, engagement_global, engagement_isp, engagement_vertical, framework, funnel, cohort_static, suppression_exclusion, partner_wave_static, legacy_snapshot, uncategorized",
		})
		return
	}

	segment := &segmentation.Segment{
		OrganizationID:       orgID,
		ListID:               req.ListID,
		Name:                 req.Name,
		Description:          req.Description,
		Category:             category,
		CalculationMode:      req.CalculationMode,
		IncludeSuppressed:    req.IncludeSuppressed,
		GlobalExclusionRules: exclusionsJSON,
		CreatedBy:            userID,
	}

	if err := api.engine.Store().CreateSegment(ctx, segment, &req.RootGroup); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Synchronous count calculation with timeout
	countCtx, countCancel := context.WithTimeout(ctx, 15*time.Second)
	defer countCancel()

	qb := api.engine.NewQueryBuilder(countCtx)
	qb.SetOrganizationID(segment.OrganizationID.String())
	if segment.ListID != nil {
		qb.SetListID(segment.ListID.String())
	}
	qb.SetIncludeSuppressed(segment.IncludeSuppressed)

	var ge []segmentation.ConditionBuilder
	if len(exclusionsJSON) > 0 {
		json.Unmarshal(exclusionsJSON, &ge)
	}

	cq, args, buildErr := qb.BuildCountQuery(req.RootGroup, ge)
	if buildErr != nil {
		log.Printf("[Segment] count query build error for %s (%s): %v", segment.Name, segment.ID, buildErr)
	} else {
		var count int
		if err := api.db.QueryRowContext(countCtx, cq, args...).Scan(&count); err != nil {
			log.Printf("[Segment] count query exec error for %s (%s): %v", segment.Name, segment.ID, err)
		} else {
			segment.SubscriberCount = count
			if err := api.engine.Store().UpdateSegmentCount(countCtx, segment.ID, count); err != nil {
				log.Printf("[Segment] count update error for %s (%s): %v", segment.Name, segment.ID, err)
			} else {
				log.Printf("[Segment] created %s (%s) with %d subscribers", segment.Name, segment.ID, count)
			}
		}
	}

	if segment.SegmentType == "" || segment.SegmentType == "dynamic" {
		db := api.db
		sid := segment.ID.String()
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			var listIDStr string
			var conditionsRaw sql.NullString
			if err := db.QueryRowContext(bgCtx,
				`SELECT COALESCE(list_id::text,''), COALESCE(conditions::text,'[]') FROM mailing_segments WHERE id = $1`, sid,
			).Scan(&listIDStr, &conditionsRaw); err != nil {
				log.Printf("[CreateSegment] failed to read segment %s for hydration: %v", sid, err)
				return
			}
			if count, err := MaterializeSegment(bgCtx, db, sid, listIDStr, conditionsRaw.String); err != nil {
				log.Printf("[CreateSegment] failed to hydrate segment %s: %v", sid, err)
			} else {
				log.Printf("[CreateSegment] hydrated segment %s with %d members", sid, count)
			}
		}()
	}

	segmentRespondJSON(w, segment)
}

// GetSegment returns a segment by ID
func (api *SegmentationAPI) GetSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	segment, err := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if segment == nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	// Get conditions
	conditions, _ := api.engine.Store().GetSegmentConditions(ctx, segmentID)

	segmentRespondJSON(w, map[string]interface{}{
		"segment":    segment,
		"conditions": conditions,
	})
}

// UpdateSegment updates a segment
func (api *SegmentationAPI) UpdateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	userID := segmentGetUserIDFromContext(ctx)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	// Block edits on system segments
	existing, _ := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if existing != nil && existing.IsSystem {
		http.Error(w, "system segments cannot be edited", http.StatusForbidden)
		return
	}

	var req CreateSegmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate conditions
	errors := api.engine.ValidateConditions(req.RootGroup)
	if len(errors) > 0 {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "validation_failed",
			"details": errors,
		})
		return
	}

	// For simplicity, delete and recreate
	if err := api.engine.Store().DeleteSegment(ctx, orgID, segmentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Same category validation as CreateSegment. If the operator omits it
	// on an edit (older frontend), inherit the existing row's category so
	// we don't blank it out on save. If they send an explicit value it must
	// be in the canonical enum.
	category := req.Category
	if category == "" && existing != nil {
		category = existing.Category
	}
	if category != "" && !validSegmentCategories[category] {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "invalid_category",
			"details": "category must be one of: engagement_brand, engagement_global, engagement_isp, engagement_vertical, framework, funnel, cohort_static, suppression_exclusion, partner_wave_static, legacy_snapshot, uncategorized",
		})
		return
	}

	exclusionsJSON, _ := json.Marshal(req.GlobalExclusions)
	segment := &segmentation.Segment{
		OrganizationID:       orgID,
		ListID:               req.ListID,
		Name:                 req.Name,
		Description:          req.Description,
		Category:             category,
		CalculationMode:      req.CalculationMode,
		IncludeSuppressed:    req.IncludeSuppressed,
		GlobalExclusionRules: exclusionsJSON,
		LastEditedBy:         userID,
	}

	if err := api.engine.Store().CreateSegment(ctx, segment, &req.RootGroup); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Synchronous count calculation with timeout
	countCtx, countCancel := context.WithTimeout(ctx, 15*time.Second)
	defer countCancel()

	uqb := api.engine.NewQueryBuilder(countCtx)
	uqb.SetOrganizationID(segment.OrganizationID.String())
	if segment.ListID != nil {
		uqb.SetListID(segment.ListID.String())
	}
	uqb.SetIncludeSuppressed(segment.IncludeSuppressed)

	var uge []segmentation.ConditionBuilder
	if len(exclusionsJSON) > 0 {
		json.Unmarshal(exclusionsJSON, &uge)
	}

	ucq, uargs, uBuildErr := uqb.BuildCountQuery(req.RootGroup, uge)
	if uBuildErr != nil {
		log.Printf("[Segment] update count query build error for %s (%s): %v", segment.Name, segment.ID, uBuildErr)
	} else {
		var count int
		if err := api.db.QueryRowContext(countCtx, ucq, uargs...).Scan(&count); err != nil {
			log.Printf("[Segment] update count query exec error for %s (%s): %v", segment.Name, segment.ID, err)
		} else {
			segment.SubscriberCount = count
			if err := api.engine.Store().UpdateSegmentCount(countCtx, segment.ID, count); err != nil {
				log.Printf("[Segment] update count persist error for %s (%s): %v", segment.Name, segment.ID, err)
			} else {
				log.Printf("[Segment] updated %s (%s) with %d subscribers", segment.Name, segment.ID, count)
			}
		}
	}

	segmentRespondJSON(w, segment)
}

// DeleteSegment deletes a segment
func (api *SegmentationAPI) DeleteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	existing, _ := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if existing != nil && existing.IsSystem {
		http.Error(w, "system segments cannot be deleted", http.StatusForbidden)
		return
	}

	if err := api.engine.Store().DeleteSegment(ctx, orgID, segmentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// PreviewSegment previews a segment without saving
func (api *SegmentationAPI) PreviewSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var req struct {
		ListID           *uuid.UUID                         `json:"list_id,omitempty"`
		RootGroup        segmentation.ConditionGroupBuilder `json:"root_group"`
		GlobalExclusions []segmentation.ConditionBuilder    `json:"global_exclusions,omitempty"`
		Limit            int                                `json:"limit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Limit == 0 {
		req.Limit = 10
	}

	preview, err := api.engine.PreviewSegment(ctx, orgID, req.ListID, req.RootGroup, req.GlobalExclusions, req.Limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, preview)
}

// segmentIsLakeBuilt reports whether a segment's conditions carry a lake_spec
// block. Lake-built segments must never flow through the live segmentation
// engine: their conditions parse as EMPTY in the legacy path (the unmarshal
// error is discarded), which silently becomes an unscoped org-wide query.
func (api *SegmentationAPI) segmentIsLakeBuilt(ctx context.Context, segmentID uuid.UUID) (bool, error) {
	var conditionsRaw sql.NullString
	if err := api.db.QueryRowContext(ctx,
		`SELECT COALESCE(conditions::text, '[]') FROM mailing_segments WHERE id = $1`, segmentID,
	).Scan(&conditionsRaw); err != nil {
		return false, err
	}
	return parseLakeSpec(conditionsRaw.String) != nil, nil
}

// ExecuteSegment calculates a segment
func (api *SegmentationAPI) ExecuteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	// Lake-built segments cannot be "executed" live — same hazard as the
	// RecalculateSegment guard (empty WHERE → entire active base). FAIL
	// CLOSED: if we cannot determine lake-ness, refuse rather than risk the
	// org-wide scan (which also corrupts subscriber_count).
	lake, lakeErr := api.segmentIsLakeBuilt(ctx, segmentID)
	if lakeErr != nil {
		http.Error(w, "could not load segment conditions: "+lakeErr.Error(), http.StatusInternalServerError)
		return
	}
	if lake {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"api_version": VersionSegmentationAPI,
			"segment_id":  segmentID,
			"error":       "lake-built segment — members are materialized; use GET /v2/segments/{id}/subscribers or POST /v2/segments/{id}/refresh",
		})
		return
	}

	result, err := api.engine.ExecuteSegment(ctx, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, result)
}

// GetSegmentSubscribers returns subscribers in a segment
func (api *SegmentationAPI) GetSegmentSubscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	// Check for pagination params
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")
	limit := 100
	offset := 0
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}
	if offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	// Lake-built (and any materialized static) segments are served straight
	// from mailing_segment_members — an indexed read that is safe during send
	// hours. Running them through the live engine would parse the lake_spec
	// conditions as EMPTY and scan the entire org (same hazard the
	// RecalculateSegment guard closes). FAIL CLOSED on the lake check: never
	// fall through to the live engine when lake-ness is unknown.
	lake, lakeErr := api.segmentIsLakeBuilt(ctx, segmentID)
	if lakeErr != nil {
		http.Error(w, "could not load segment conditions: "+lakeErr.Error(), http.StatusInternalServerError)
		return
	}
	if lake {
		var total int64
		if err := api.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_segment_members WHERE segment_id = $1`, segmentID,
		).Scan(&total); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rows, qerr := api.db.QueryContext(ctx,
			`SELECT subscriber_id FROM mailing_segment_members WHERE segment_id = $1 ORDER BY subscriber_id LIMIT $2 OFFSET $3`,
			segmentID, limit, offset)
		if qerr != nil {
			http.Error(w, qerr.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		ids := make([]string, 0, limit)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		segmentRespondJSON(w, map[string]interface{}{
			"count":          total,
			"subscriber_ids": ids,
			"limit":          limit,
			"offset":         offset,
			"has_more":       int64(offset+len(ids)) < total,
			"source":         "materialized",
		})
		return
	}

	result, err := api.engine.ExecuteSegment(ctx, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Paginate the results
	start := offset
	end := offset + limit
	if start > len(result.SubscriberIDs) {
		start = len(result.SubscriberIDs)
	}
	if end > len(result.SubscriberIDs) {
		end = len(result.SubscriberIDs)
	}

	paginatedIDs := result.SubscriberIDs[start:end]

	segmentRespondJSON(w, map[string]interface{}{
		"count":          result.SubscriberCount,
		"subscriber_ids": paginatedIDs,
		"limit":          limit,
		"offset":         offset,
		"has_more":       end < len(result.SubscriberIDs),
	})
}

// ExportSegmentMembersCSV streams all members of a single segment as CSV
// directly from mailing_segment_members. Single indexed read by segment_id
// — does NOT execute the segment query against mailing_subscribers, so it
// is safe to call during active sending hours and works even when the live
// query would ALB-timeout on heavy globals.
//
// Query params:
//   - format: "csv" (default) or "txt" (one email per line, no header)
//   - include_subscriber_id: "true" adds a subscriber_id column
//   - dedupe: "true" (default) lowercases + dedupes by email; "false" returns raw
//
// Response: text/csv stream with header row email[,subscriber_id]
// or text/plain stream when format=txt.
//
// NOTE: segments not yet materialized into mailing_segment_members will
// return zero rows with HTTP 200. Use GET /v2/segments/{id}/count to
// distinguish "empty" from "never materialized" (audience_source field).
func (api *SegmentationAPI) ExportSegmentMembersCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID, err := uuid.Parse(chi.URLParam(r, "segmentID"))
	if err != nil {
		http.Error(w, "invalid segment id", http.StatusBadRequest)
		return
	}

	includeSubID := r.URL.Query().Get("include_subscriber_id") == "true"
	dedupe := r.URL.Query().Get("dedupe") != "false"
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}

	// Resolve segment name for the response filename.
	var segName string
	if err := api.db.QueryRowContext(ctx,
		`SELECT name FROM mailing_segments WHERE id = $1`, segmentID,
	).Scan(&segName); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "segment not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 10-minute read budget for the stream. Single indexed query on
	// (segment_id, email); the index makes COUNT and SELECT both cheap.
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var selectCols string
	if includeSubID {
		selectCols = "subscriber_id::text, LOWER(email) AS email"
	} else {
		selectCols = "LOWER(email) AS email"
	}

	var orderClause string
	if dedupe {
		// DISTINCT on the lowercased email; ORDER BY required for DISTINCT ON.
		if includeSubID {
			selectCols = "DISTINCT ON (LOWER(email)) subscriber_id::text, LOWER(email) AS email"
		} else {
			selectCols = "DISTINCT LOWER(email) AS email"
		}
		orderClause = "ORDER BY LOWER(email)"
	}

	q := fmt.Sprintf(`SELECT %s FROM mailing_segment_members WHERE segment_id = $1 %s`,
		selectCols, orderClause)
	rows, err := api.db.QueryContext(streamCtx, q, segmentID)
	if err != nil {
		log.Printf("[Segment] export members query error for %s: %v", segmentID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	safeName := strings.NewReplacer(" ", "_", "/", "_", "\\", "_").Replace(segName)
	if safeName == "" {
		safeName = segmentID.String()
	}

	if format == "txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.txt"`, safeName))
		w.Header().Set("X-Segment-Id", segmentID.String())
		w.Header().Set("X-Segment-Name", segName)
		for rows.Next() {
			var email, subID string
			if includeSubID {
				if err := rows.Scan(&subID, &email); err != nil {
					continue
				}
			} else {
				if err := rows.Scan(&email); err != nil {
					continue
				}
			}
			fmt.Fprintln(w, email)
		}
		return
	}

	// CSV path
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, safeName))
	w.Header().Set("X-Segment-Id", segmentID.String())
	w.Header().Set("X-Segment-Name", segName)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if includeSubID {
		_ = cw.Write([]string{"subscriber_id", "email"})
	} else {
		_ = cw.Write([]string{"email"})
	}

	written := 0
	for rows.Next() {
		var email, subID string
		if includeSubID {
			if err := rows.Scan(&subID, &email); err != nil {
				continue
			}
			_ = cw.Write([]string{subID, email})
		} else {
			if err := rows.Scan(&email); err != nil {
				continue
			}
			_ = cw.Write([]string{email})
		}
		written++
		// Flush every 5k rows so the client sees progress and the
		// response body doesn't balloon in memory on large segments.
		if written%5000 == 0 {
			cw.Flush()
		}
	}
}

// ExportSegmentsUnionCSV streams the deduped UNION of multiple segments'
// member emails as a single CSV. Used for hygiene runs ("clean all 30D
// engaged emails across every brand") where the operator wants one file
// to feed into a verifier instead of N per-segment files to merge by hand.
//
// Request body: {"segment_ids": ["uuid", "uuid", ...]} (max 50 ids).
// Optional flags inside the JSON body:
//   - include_segment_attribution: bool (default false) — when true, adds a
//     comma-joined "segments" column listing every input segment that
//     contained the email
//   - format: "csv" (default) or "txt"
//
// Response: text/csv stream with header row email[,segments]
//
// Implementation: single query that UNIONs mailing_segment_members rows
// across the provided segment_ids, groups by lower(email), and optionally
// aggregates the source segment names. Reads from the materialized rollup
// only — never touches mailing_subscribers.
func (api *SegmentationAPI) ExportSegmentsUnionCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		SegmentIDs                []string `json:"segment_ids"`
		IncludeSegmentAttribution bool     `json:"include_segment_attribution"`
		Format                    string   `json:"format"`
		Filename                  string   `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.SegmentIDs) == 0 {
		http.Error(w, "segment_ids is required", http.StatusBadRequest)
		return
	}
	if len(body.SegmentIDs) > 50 {
		http.Error(w, "max 50 segment_ids per request", http.StatusBadRequest)
		return
	}

	// Validate every id parses as UUID before sending to the DB so we
	// fail fast with a clear error rather than a SQL syntax error.
	idArr := make([]string, 0, len(body.SegmentIDs))
	for _, raw := range body.SegmentIDs {
		u, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			http.Error(w, "invalid segment id: "+raw, http.StatusBadRequest)
			return
		}
		idArr = append(idArr, u.String())
	}

	streamCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	var q string
	if body.IncludeSegmentAttribution {
		q = `
			SELECT LOWER(m.email) AS email,
			       string_agg(DISTINCT s.name, ',' ORDER BY s.name) AS segments
			FROM mailing_segment_members m
			JOIN mailing_segments s ON s.id = m.segment_id
			WHERE m.segment_id = ANY($1::uuid[])
			GROUP BY LOWER(m.email)
			ORDER BY email
		`
	} else {
		q = `
			SELECT DISTINCT LOWER(email) AS email
			FROM mailing_segment_members
			WHERE segment_id = ANY($1::uuid[])
			ORDER BY email
		`
	}

	rows, err := api.db.QueryContext(streamCtx, q, pq.Array(idArr))
	if err != nil {
		log.Printf("[Segment] export union query error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	fname := strings.TrimSpace(body.Filename)
	if fname == "" {
		fname = "segments_union_export"
	}
	fname = strings.NewReplacer(" ", "_", "/", "_", "\\", "_").Replace(fname)

	format := strings.ToLower(body.Format)
	if format == "" {
		format = "csv"
	}

	if format == "txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.txt"`, fname))
		w.Header().Set("X-Segment-Count", strconv.Itoa(len(idArr)))
		for rows.Next() {
			var email string
			if body.IncludeSegmentAttribution {
				var segs sql.NullString
				if err := rows.Scan(&email, &segs); err != nil {
					continue
				}
			} else {
				if err := rows.Scan(&email); err != nil {
					continue
				}
			}
			fmt.Fprintln(w, email)
		}
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, fname))
	w.Header().Set("X-Segment-Count", strconv.Itoa(len(idArr)))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if body.IncludeSegmentAttribution {
		_ = cw.Write([]string{"email", "segments"})
	} else {
		_ = cw.Write([]string{"email"})
	}

	written := 0
	for rows.Next() {
		if body.IncludeSegmentAttribution {
			var email string
			var segs sql.NullString
			if err := rows.Scan(&email, &segs); err != nil {
				continue
			}
			_ = cw.Write([]string{email, segs.String})
		} else {
			var email string
			if err := rows.Scan(&email); err != nil {
				continue
			}
			_ = cw.Write([]string{email})
		}
		written++
		if written%5000 == 0 {
			cw.Flush()
		}
	}
}

// GetSegmentCount returns the count for a segment, preferring the build
// ledger (mailing_segment_build_ledger — a single PK point read, source
// 'ledger'). When no completed build is recorded there it falls back to the
// previous behavior: COUNT(*) over the materialized rollup
// (mailing_segment_members), then the segment row's cached count. Safe to
// call repeatedly during sending hours.
//
// To trigger a fresh recalculation (which DOES touch mailing_subscribers),
// call POST /v2/segments/{segmentID}/recalculate explicitly.
func (api *SegmentationAPI) GetSegmentCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	segment, err := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if segment == nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	countCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Ledger first: one PK point read. Only a row with a completed build
	// (last_built_at NOT NULL) is authoritative — a row created solely by
	// MarkSegmentBuildRunning has count 0 and must not mask the fallback.
	var ledCount int64
	var ledBuiltAt sql.NullTime
	ledErr := api.db.QueryRowContext(countCtx, `
		SELECT subscriber_count, last_built_at
		  FROM mailing_segment_build_ledger
		 WHERE segment_id = $1
	`, segmentID).Scan(&ledCount, &ledBuiltAt)
	if ledErr == nil && ledBuiltAt.Valid {
		segmentRespondJSON(w, map[string]interface{}{
			"api_version":        VersionSegmentationAPI,
			"segment_id":         segmentID,
			"count":              ledCount,
			"audience_count":     ledCount,
			"audience_source":    "ledger",
			"last_calculated_at": segment.LastCalculatedAt,
			"materialized_at":    ledBuiltAt.Time,
		})
		return
	}
	if ledErr != nil && ledErr != sql.ErrNoRows {
		log.Printf("[Segment] build-ledger read error for %s (falling back to rollup): %v", segmentID, ledErr)
	}

	var matCount int64
	var matAt sql.NullTime
	err = api.db.QueryRowContext(countCtx, `
		SELECT COUNT(*), MAX(materialized_at)
		  FROM mailing_segment_members
		 WHERE segment_id = $1
	`, segmentID).Scan(&matCount, &matAt)
	if err != nil {
		log.Printf("[Segment] materialized count read error for %s: %v", segmentID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	source := "materialized"
	if matCount == 0 && matAt.Valid == false {
		// Never materialized — fall back to cached count without running BuildCountQuery.
		// Caller can POST /recalculate to populate.
		matCount = int64(segment.SubscriberCount)
		source = "cached"
	}

	resp := map[string]interface{}{
		"api_version":        VersionSegmentationAPI,
		"segment_id":         segmentID,
		"count":              matCount,
		"audience_count":     matCount,
		"audience_source":    source,
		"last_calculated_at": segment.LastCalculatedAt,
	}
	if matAt.Valid {
		resp["materialized_at"] = matAt.Time
	}

	segmentRespondJSON(w, resp)
}

// RecalculateSegment synchronously re-materializes a segment by calling
// MaterializeSegment, which writes fresh rows into mailing_segment_members.
// This DOES read from mailing_subscribers, so it has its own statement timeout
// and is intentionally only ever invoked by an explicit user action — never
// on a page load. Returns the new materialized count and timestamp.
func (api *SegmentationAPI) RecalculateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)
	segmentID, parseErr := uuid.Parse(chi.URLParam(r, "segmentID"))
	if parseErr != nil {
		http.Error(w, "invalid segment id", http.StatusBadRequest)
		return
	}

	segment, err := api.engine.Store().GetSegment(ctx, orgID, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if segment == nil {
		http.Error(w, "segment not found", http.StatusNotFound)
		return
	}

	listIDStr := ""
	if segment.ListID != nil {
		listIDStr = segment.ListID.String()
	}

	var conditionsRaw sql.NullString
	if err := api.db.QueryRowContext(ctx,
		`SELECT COALESCE(conditions::text, '[]') FROM mailing_segments WHERE id = $1`,
		segmentID,
	).Scan(&conditionsRaw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Lake-built segments MUST NOT go through the legacy materializer:
	// their {"lake_spec":...} conditions fall through buildSegmentQuery's
	// legacy branch (which discards the unmarshal error) to an empty WHERE
	// clause, i.e. DELETE members + INSERT the ENTIRE active subscriber
	// base, cross-org. Route operators to the lake refresh endpoint.
	// materializeSegmentCore carries the same guard as defense-in-depth.
	if parseLakeSpec(conditionsRaw.String) != nil {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"api_version": VersionSegmentationAPI,
			"segment_id":  segmentID,
			"error":       "lake-built segment — use POST /v2/segments/{id}/refresh",
		})
		return
	}

	matCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()

	// materializeSegmentWithLedger (rather than MaterializeSegment) so the
	// build-ledger row records source 'recalculate' — an explicit user action,
	// not the background materializer. Ledger bookkeeping is handled inside;
	// it is best-effort and never fails the build.
	count, err := materializeSegmentWithLedger(matCtx, api.db, segmentID.String(), listIDStr, conditionsRaw.String, "recalculate")
	if err != nil {
		log.Printf("[Segment] recalculate failed for %s: %v", segmentID, err)
		segmentRespondJSONStatus(w, http.StatusServiceUnavailable, map[string]interface{}{
			"api_version": VersionSegmentationAPI,
			"segment_id":  segmentID,
			"error":       "recalculate_failed",
			"detail":      err.Error(),
		})
		return
	}

	if updateErr := api.engine.Store().UpdateSegmentCount(ctx, segmentID, count); updateErr != nil {
		log.Printf("[Segment] update cached count after recalc failed for %s: %v", segmentID, updateErr)
	}

	segmentRespondJSON(w, map[string]interface{}{
		"api_version":     VersionSegmentationAPI,
		"segment_id":      segmentID,
		"count":           count,
		"audience_count":  count,
		"audience_source": "materialized",
		"materialized_at": time.Now().UTC(),
	})
}

// ==========================================
// SNAPSHOT HANDLERS
// ==========================================

// CreateSnapshot creates a segment snapshot
func (api *SegmentationAPI) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := segmentGetUserIDFromContext(ctx)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	var req struct {
		Purpose string `json:"purpose,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if req.Purpose == "" {
		req.Purpose = "manual"
	}

	snapshot, err := api.engine.CreateSegmentSnapshot(ctx, segmentID, req.Purpose, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, snapshot)
}

// GetSnapshot returns a snapshot
func (api *SegmentationAPI) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapshotID, _ := uuid.Parse(chi.URLParam(r, "snapshotID"))

	snapshot, err := api.engine.Store().GetSnapshot(ctx, snapshotID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snapshot == nil {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}

	segmentRespondJSON(w, snapshot)
}

// GetSnapshotSubscribers returns subscribers from a snapshot
func (api *SegmentationAPI) GetSnapshotSubscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	snapshotID, _ := uuid.Parse(chi.URLParam(r, "snapshotID"))

	ids, err := api.engine.GetSnapshotSubscribers(ctx, snapshotID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, map[string]interface{}{
		"count":          len(ids),
		"subscriber_ids": ids,
	})
}

// ==========================================
// EVENT HANDLERS
// ==========================================

// TrackEvent tracks a custom event
func (api *SegmentationAPI) TrackEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var event segmentation.CustomEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	event.OrganizationID = orgID

	if err := api.engine.TrackEvent(ctx, &event); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSONStatus(w, http.StatusCreated, map[string]string{"status": "tracked"})
}

// ==========================================
// CONTACT FIELD HANDLERS
// ==========================================

// ListContactFields returns all contact field definitions
func (api *SegmentationAPI) ListContactFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	fields, err := api.engine.Store().GetContactFields(ctx, orgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, fields)
}

// CreateContactField creates a new contact field definition
func (api *SegmentationAPI) CreateContactField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var field segmentation.ContactField
	if err := json.NewDecoder(r.Body).Decode(&field); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	field.OrganizationID = orgID
	field.IsSystem = false

	if err := api.engine.Store().CreateContactField(ctx, &field); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	segmentRespondJSON(w, field)
}

// ==========================================
// OPERATOR HANDLERS
// ==========================================

// ListOperators returns all available operators
func (api *SegmentationAPI) ListOperators(w http.ResponseWriter, r *http.Request) {
	fieldType := segmentation.FieldType(r.URL.Query().Get("field_type"))

	var operators []segmentation.OperatorMetadata
	if fieldType != "" {
		operators = segmentation.GetAvailableOperators(fieldType)
	} else {
		operators = segmentation.GetOperatorMetadata()
	}

	segmentRespondJSON(w, operators)
}

// TrackEventsBatch tracks multiple events in a single request
func (api *SegmentationAPI) TrackEventsBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	var events []segmentation.CustomEvent
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	successCount := 0
	for _, event := range events {
		event.OrganizationID = orgID
		if err := api.engine.TrackEvent(ctx, &event); err == nil {
			successCount++
		}
	}

	segmentRespondJSON(w, map[string]interface{}{
		"tracked": successCount,
		"total":   len(events),
	})
}

// ==========================================
// HELPERS
// ==========================================

func segmentGetOrgIDFromRequest(r *http.Request) uuid.UUID {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return uuid.Nil
	}
	return orgID
}

func segmentGetUserIDFromContext(ctx interface{}) *uuid.UUID {
	// In real implementation, extract from session/context
	return nil
}

func segmentRespondJSON(w http.ResponseWriter, data interface{}) {
	segmentRespondJSONStatus(w, http.StatusOK, data)
}

func segmentRespondJSONStatus(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

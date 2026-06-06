package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
const VersionSegmentationAPI = "2.2.0"

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
			r.Get("/count", api.GetSegmentCount)        // Cheap materialized count
			r.Post("/recalculate", api.RecalculateSegment) // Synchronous re-materialize
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

// ListSegments returns all segments for the organization with a materialized
// audience snapshot per segment (count + freshness). The hot read path NEVER
// touches mailing_subscribers; the materialized rollup is read from
// mailing_segment_members which is indexed by segment_id and refreshed by the
// nightly + on-boot SegmentMaterializer worker. This keeps the segments page
// safe to load during 24/7 send hours.
//
// Response shape (JSON array):
//
//	[
//	  {
//	    ...all *segmentation.Segment fields...,
//	    "materialized_count": 17234,
//	    "materialized_at":    "2026-04-27T11:02:13Z",
//	    "audience_count":     17234,         // displayable number — materialized when present, cached fallback
//	    "audience_source":    "materialized" // or "cached"
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

	segments, err := api.engine.Store().ListSegments(ctx, orgID, listID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if segments == nil {
		segments = []*segmentation.Segment{}
	}

	// One cheap aggregate over mailing_segment_members for ALL returned segment ids.
	// This is an indexed read; no joins to mailing_subscribers, no per-segment loop.
	matCounts, matTimes := api.fetchMaterializedRollups(ctx, segments)

	type segmentRow struct {
		*segmentation.Segment
		MaterializedCount *int64     `json:"materialized_count,omitempty"`
		MaterializedAt    *time.Time `json:"materialized_at,omitempty"`
		AudienceCount     int64      `json:"audience_count"`
		AudienceSource    string     `json:"audience_source"`
	}

	rows := make([]segmentRow, 0, len(segments))
	for _, seg := range segments {
		row := segmentRow{Segment: seg}
		key := seg.ID.String()
		if c, ok := matCounts[key]; ok {
			row.MaterializedCount = &c
			row.AudienceCount = c
			row.AudienceSource = "materialized"
		} else {
			row.AudienceCount = int64(seg.SubscriberCount)
			row.AudienceSource = "cached"
		}
		if t, ok := matTimes[key]; ok {
			row.MaterializedAt = &t
		}
		rows = append(rows, row)
	}

	segmentRespondJSON(w, rows)
}

// fetchMaterializedRollups returns segment_id → COUNT(*) and segment_id → MAX(materialized_at)
// for every id in segments, in a single indexed query. Empty maps are returned
// when the list is empty or the query fails (fail-open: cached count is shown).
func (api *SegmentationAPI) fetchMaterializedRollups(ctx context.Context, segments []*segmentation.Segment) (map[string]int64, map[string]time.Time) {
	counts := make(map[string]int64, len(segments))
	times := make(map[string]time.Time, len(segments))
	if len(segments) == 0 || api.db == nil {
		return counts, times
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
		       COUNT(*) AS members,
		       MAX(materialized_at) AS last_at
		  FROM mailing_segment_members
		 WHERE segment_id = ANY($1::uuid[])
		 GROUP BY segment_id
	`, segmentIDArray(ids))
	if err != nil {
		log.Printf("[Segment] materialized rollup query error (returning cached counts): %v", err)
		return counts, times
	}
	defer rows.Close()

	for rows.Next() {
		var sid string
		var n int64
		var at sql.NullTime
		if err := rows.Scan(&sid, &n, &at); err != nil {
			continue
		}
		counts[sid] = n
		if at.Valid {
			times[sid] = at.Time
		}
	}
	return counts, times
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

// ExecuteSegment calculates a segment
func (api *SegmentationAPI) ExecuteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

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
		SegmentIDs                 []string `json:"segment_ids"`
		IncludeSegmentAttribution  bool     `json:"include_segment_attribution"`
		Format                     string   `json:"format"`
		Filename                   string   `json:"filename"`
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

// GetSegmentCount returns the count for a segment from the materialized
// rollup (mailing_segment_members). This is a single indexed read and is
// safe to call repeatedly during sending hours.
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

	matCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()

	count, err := MaterializeSegment(matCtx, api.db, segmentID.String(), listIDStr, conditionsRaw.String)
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

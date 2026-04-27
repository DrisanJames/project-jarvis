package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
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
const VersionSegmentationAPI = "2.1.0"

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
		})
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

	segment := &segmentation.Segment{
		OrganizationID:       orgID,
		ListID:               req.ListID,
		Name:                 req.Name,
		Description:          req.Description,
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

	exclusionsJSON, _ := json.Marshal(req.GlobalExclusions)
	segment := &segmentation.Segment{
		OrganizationID:       orgID,
		ListID:               req.ListID,
		Name:                 req.Name,
		Description:          req.Description,
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

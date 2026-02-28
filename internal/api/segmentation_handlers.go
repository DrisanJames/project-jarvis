package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

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
			r.Get("/count", api.GetSegmentCount)        // Dedicated count endpoint
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
	Name              string                           `json:"name"`
	Description       string                           `json:"description,omitempty"`
	ListID            *uuid.UUID                       `json:"list_id,omitempty"`
	CalculationMode   string                           `json:"calculation_mode,omitempty"`
	IncludeSuppressed bool                             `json:"include_suppressed"`
	RootGroup         segmentation.ConditionGroupBuilder `json:"root_group"`
	GlobalExclusions  []segmentation.ConditionBuilder  `json:"global_exclusions,omitempty"`
}

// ListSegments returns all segments for the organization
func (api *SegmentationAPI) ListSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)

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

	segmentRespondJSON(w, segments)
}

// CreateSegment creates a new segment
func (api *SegmentationAPI) CreateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)
	userID := segmentGetUserIDFromContext(ctx)

	var req CreateSegmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate conditions
	errors := api.engine.ValidateConditions(req.RootGroup)
	if len(errors) > 0 {
		segmentRespondJSON(w, map[string]interface{}{
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

	segmentRespondJSON(w, segment)
}

// GetSegment returns a segment by ID
func (api *SegmentationAPI) GetSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)
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
	orgID := segmentGetOrgIDFromContext(ctx)
	userID := segmentGetUserIDFromContext(ctx)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	var req CreateSegmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate conditions
	errors := api.engine.ValidateConditions(req.RootGroup)
	if len(errors) > 0 {
		segmentRespondJSON(w, map[string]interface{}{
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

	segmentRespondJSON(w, segment)
}

// DeleteSegment deletes a segment
func (api *SegmentationAPI) DeleteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	if err := api.engine.Store().DeleteSegment(ctx, orgID, segmentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// PreviewSegment previews a segment without saving
func (api *SegmentationAPI) PreviewSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)

	var req struct {
		ListID           *uuid.UUID                          `json:"list_id,omitempty"`
		RootGroup        segmentation.ConditionGroupBuilder  `json:"root_group"`
		GlobalExclusions []segmentation.ConditionBuilder     `json:"global_exclusions,omitempty"`
		Limit            int                                 `json:"limit,omitempty"`
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

// GetSegmentCount returns just the count for a segment (fast endpoint)
func (api *SegmentationAPI) GetSegmentCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)
	segmentID, _ := uuid.Parse(chi.URLParam(r, "segmentID"))

	// Get segment
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
	conditions, err := api.engine.Store().GetSegmentConditions(ctx, segmentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conditions == nil {
		conditions = &segmentation.ConditionGroupBuilder{LogicOperator: segmentation.LogicAnd}
	}

	// Parse global exclusions
	var globalExclusions []segmentation.ConditionBuilder
	if len(segment.GlobalExclusionRules) > 0 {
		json.Unmarshal(segment.GlobalExclusionRules, &globalExclusions)
	}

	// Build count query
	qb := segmentation.NewQueryBuilder()
	qb.SetOrganizationID(segment.OrganizationID.String())
	if segment.ListID != nil {
		qb.SetListID(segment.ListID.String())
	}
	qb.SetIncludeSuppressed(segment.IncludeSuppressed)

	countQuery, args, err := qb.BuildCountQuery(*conditions, globalExclusions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var count int
	if err := api.db.QueryRowContext(ctx, countQuery, args...).Scan(&count); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update segment count in database
	api.engine.Store().UpdateSegmentCount(ctx, segmentID, count)

	segmentRespondJSON(w, map[string]interface{}{
		"segment_id":       segmentID,
		"count":            count,
		"last_calculated":  segment.LastCalculatedAt,
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
	orgID := segmentGetOrgIDFromContext(ctx)

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

	w.WriteHeader(http.StatusCreated)
	segmentRespondJSON(w, map[string]string{"status": "tracked"})
}

// ==========================================
// CONTACT FIELD HANDLERS
// ==========================================

// ListContactFields returns all contact field definitions
func (api *SegmentationAPI) ListContactFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromContext(ctx)

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
	orgID := segmentGetOrgIDFromContext(ctx)

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
	orgID := segmentGetOrgIDFromContext(ctx)

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

func segmentGetOrgIDFromContext(ctx interface{}) uuid.UUID {
	// Use the dynamic org context extraction
	if c, ok := ctx.(context.Context); ok {
		return GetOrgIDFromContext(c)
	}
	return uuid.Nil
}

func segmentGetUserIDFromContext(ctx interface{}) *uuid.UUID {
	// In real implementation, extract from session/context
	return nil
}

func segmentRespondJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

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
			r.Get("/count", api.GetSegmentCount) // Dedicated count endpoint
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

// ListSegments returns all segments for the organization
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

	// Background-refresh counts for segments that show 0
	for _, seg := range segments {
		if seg.SubscriberCount == 0 {
			go func(s *segmentation.Segment) {
				// System segments use a pre-built SQL query
				if s.IsSystem && s.SystemQuery != "" {
					var count int
					if err := api.db.QueryRowContext(context.Background(), s.SystemQuery, s.OrganizationID).Scan(&count); err != nil {
						log.Printf("[Segment] system query error for %s (%s): %v", s.Name, s.ID, err)
						return
					}
					if err := api.engine.Store().UpdateSegmentCount(context.Background(), s.ID, count); err != nil {
						log.Printf("[Segment] system count update error for %s (%s): %v", s.Name, s.ID, err)
						return
					}
					log.Printf("[Segment] refreshed system segment %s (%s): %d subscribers", s.Name, s.ID, count)
					return
				}

				conditions, err := api.engine.Store().GetSegmentConditions(context.Background(), s.ID)
				if err != nil || conditions == nil {
					log.Printf("[Segment] refresh count for %s (%s): failed to load conditions: %v", s.Name, s.ID, err)
					return
				}
				qb := api.engine.NewQueryBuilder(context.Background())
				qb.SetOrganizationID(s.OrganizationID.String())
				if s.ListID != nil {
					qb.SetListID(s.ListID.String())
				}
				qb.SetIncludeSuppressed(s.IncludeSuppressed)

				var ge []segmentation.ConditionBuilder
				if len(s.GlobalExclusionRules) > 0 {
					json.Unmarshal(s.GlobalExclusionRules, &ge)
				}

				cq, args, err := qb.BuildCountQuery(*conditions, ge)
				if err != nil {
					log.Printf("[Segment] refresh count for %s (%s): query build error: %v", s.Name, s.ID, err)
					return
				}
				var count int
				if err := api.db.QueryRowContext(context.Background(), cq, args...).Scan(&count); err != nil {
					log.Printf("[Segment] refresh count for %s (%s): query exec error: %v", s.Name, s.ID, err)
					return
				}
				if err := api.engine.Store().UpdateSegmentCount(context.Background(), s.ID, count); err != nil {
					log.Printf("[Segment] refresh count for %s (%s): update error: %v", s.Name, s.ID, err)
					return
				}
				log.Printf("[Segment] refreshed count for %s (%s): %d subscribers", s.Name, s.ID, count)
			}(seg)
		}
	}

	segmentRespondJSON(w, segments)
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

// GetSegmentCount returns just the count for a segment (fast endpoint)
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

	var count int

	if segment.IsSystem && segment.SystemQuery != "" {
		if err := api.db.QueryRowContext(ctx, segment.SystemQuery, segment.OrganizationID).Scan(&count); err != nil {
			log.Printf("[Segment] system count error for %s: %v", segment.Name, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		conditions, err := api.engine.Store().GetSegmentConditions(ctx, segmentID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conditions == nil {
			conditions = &segmentation.ConditionGroupBuilder{LogicOperator: segmentation.LogicAnd}
		}

		var globalExclusions []segmentation.ConditionBuilder
		if len(segment.GlobalExclusionRules) > 0 {
			json.Unmarshal(segment.GlobalExclusionRules, &globalExclusions)
		}

		qb := api.engine.NewQueryBuilder(ctx)
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

		if err := api.db.QueryRowContext(ctx, countQuery, args...).Scan(&count); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	api.engine.Store().UpdateSegmentCount(ctx, segmentID, count)

	segmentRespondJSON(w, map[string]interface{}{
		"segment_id":      segmentID,
		"count":           count,
		"last_calculated": segment.LastCalculatedAt,
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

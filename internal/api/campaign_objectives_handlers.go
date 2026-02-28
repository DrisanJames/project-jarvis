package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// =============================================================================
// CAMPAIGN OBJECTIVES HANDLERS
// =============================================================================
// HTTP handlers for campaign objectives API. Enables:
// - CRUD operations for campaign objectives
// - ESP signal tracking and monitoring
// - Optimization logging and analysis

// CampaignObjectivesService handles campaign objective API endpoints
type CampaignObjectivesService struct {
	store *mailing.ObjectivesStore
}

// NewCampaignObjectivesService creates a new campaign objectives service
func NewCampaignObjectivesService(store *mailing.ObjectivesStore) *CampaignObjectivesService {
	return &CampaignObjectivesService{store: store}
}

// RegisterRoutes registers the campaign objectives API routes
func (svc *CampaignObjectivesService) RegisterRoutes(r chi.Router) {
	r.Route("/campaigns/{campaignId}/objective", func(r chi.Router) {
		r.Get("/", svc.HandleGetObjective)
		r.Post("/", svc.HandleCreateObjective)
		r.Put("/", svc.HandleUpdateObjective)
		r.Delete("/", svc.HandleDeleteObjective)
	})

	r.Route("/campaigns/{campaignId}/signals", func(r chi.Router) {
		r.Get("/", svc.HandleGetSignals)
		r.Get("/summary", svc.HandleGetSignalSummary)
		r.Post("/", svc.HandleRecordSignal)
	})

	r.Route("/campaigns/{campaignId}/optimizations", func(r chi.Router) {
		r.Get("/", svc.HandleGetOptimizationLogs)
		r.Post("/", svc.HandleLogOptimization)
	})
}

// =============================================================================
// REQUEST/RESPONSE TYPES
// =============================================================================

// CreateObjectiveRequest is the request body for creating an objective
type CreateObjectiveRequest struct {
	Purpose string `json:"purpose"` // data_activation or offer_revenue

	// Data Activation
	ActivationGoal       string   `json:"activation_goal,omitempty"`
	TargetEngagementRate *float64 `json:"target_engagement_rate,omitempty"`
	TargetCleanRate      *float64 `json:"target_clean_rate,omitempty"`

	// Offer Revenue
	OfferModel   string   `json:"offer_model,omitempty"`
	ECPMTarget   *float64 `json:"ecpm_target,omitempty"`
	BudgetLimit  *float64 `json:"budget_limit,omitempty"`
	TargetMetric string   `json:"target_metric,omitempty"`
	TargetValue  int      `json:"target_value,omitempty"`

	// Everflow
	EverflowOfferIDs      []string `json:"everflow_offer_ids,omitempty"`
	EverflowSubIDTemplate string   `json:"everflow_sub_id_template,omitempty"`
	PropertyCode          string   `json:"property_code,omitempty"`

	// Creative Rotation
	ApprovedCreatives []mailing.ApprovedCreative `json:"approved_creatives,omitempty"`
	RotationStrategy  string                     `json:"rotation_strategy,omitempty"`

	// AI Configuration
	AIOptimizationEnabled    *bool `json:"ai_optimization_enabled,omitempty"`
	AIThroughputOptimization *bool `json:"ai_throughput_optimization,omitempty"`
	AICreativeRotation       *bool `json:"ai_creative_rotation,omitempty"`
	AIBudgetPacing           *bool `json:"ai_budget_pacing,omitempty"`
	ESPSignalMonitoring      *bool `json:"esp_signal_monitoring,omitempty"`

	// Thresholds
	PauseOnSpamSignal     *bool    `json:"pause_on_spam_signal,omitempty"`
	SpamSignalThreshold   *float64 `json:"spam_signal_threshold,omitempty"`
	BounceThreshold       *float64 `json:"bounce_threshold,omitempty"`
	ThroughputSensitivity string   `json:"throughput_sensitivity,omitempty"`
	MinThroughputRate     int      `json:"min_throughput_rate,omitempty"`
	MaxThroughputRate     int      `json:"max_throughput_rate,omitempty"`

	// Pacing
	TargetCompletionHours int    `json:"target_completion_hours,omitempty"`
	PacingStrategy        string `json:"pacing_strategy,omitempty"`
}

// =============================================================================
// OBJECTIVE HANDLERS
// =============================================================================

// HandleGetObjective returns the objective for a campaign
// GET /api/mailing/campaigns/{campaignId}/objective
func (svc *CampaignObjectivesService) HandleGetObjective(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	objective, err := svc.store.GetObjective(r.Context(), campaignID)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			writeJSONError(w, "objective not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, objective)
}

// HandleCreateObjective creates a new objective for a campaign
// POST /api/mailing/campaigns/{campaignId}/objective
func (svc *CampaignObjectivesService) HandleCreateObjective(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	var req CreateObjectiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate purpose
	if req.Purpose != "data_activation" && req.Purpose != "offer_revenue" {
		writeJSONError(w, "purpose must be 'data_activation' or 'offer_revenue'", http.StatusBadRequest)
		return
	}

	// Get organization ID from context
	orgID := getOrgIDFromContext(ctx)

	// Build objective from request
	objective := &mailing.CampaignObjective{
		CampaignID:            campaignID,
		OrganizationID:        orgID,
		Purpose:               req.Purpose,
		ActivationGoal:        req.ActivationGoal,
		TargetEngagementRate:  req.TargetEngagementRate,
		TargetCleanRate:       req.TargetCleanRate,
		OfferModel:            req.OfferModel,
		ECPMTarget:            req.ECPMTarget,
		BudgetLimit:           req.BudgetLimit,
		TargetMetric:          req.TargetMetric,
		TargetValue:           req.TargetValue,
		EverflowSubIDTemplate: req.EverflowSubIDTemplate,
		PropertyCode:          req.PropertyCode,
		RotationStrategy:      req.RotationStrategy,
		ThroughputSensitivity: req.ThroughputSensitivity,
		MinThroughputRate:     req.MinThroughputRate,
		MaxThroughputRate:     req.MaxThroughputRate,
		TargetCompletionHours: req.TargetCompletionHours,
		PacingStrategy:        req.PacingStrategy,
	}

	// Set defaults for booleans (default to true for AI features)
	if req.AIOptimizationEnabled != nil {
		objective.AIOptimizationEnabled = *req.AIOptimizationEnabled
	} else {
		objective.AIOptimizationEnabled = true
	}
	if req.AIThroughputOptimization != nil {
		objective.AIThroughputOptimization = *req.AIThroughputOptimization
	} else {
		objective.AIThroughputOptimization = true
	}
	if req.AICreativeRotation != nil {
		objective.AICreativeRotation = *req.AICreativeRotation
	} else {
		objective.AICreativeRotation = true
	}
	if req.AIBudgetPacing != nil {
		objective.AIBudgetPacing = *req.AIBudgetPacing
	} else {
		objective.AIBudgetPacing = true
	}
	if req.ESPSignalMonitoring != nil {
		objective.ESPSignalMonitoring = *req.ESPSignalMonitoring
	} else {
		objective.ESPSignalMonitoring = true
	}
	if req.PauseOnSpamSignal != nil {
		objective.PauseOnSpamSignal = *req.PauseOnSpamSignal
	} else {
		objective.PauseOnSpamSignal = true
	}

	// Marshal JSON fields
	if len(req.ApprovedCreatives) > 0 {
		creativesJSON, _ := json.Marshal(req.ApprovedCreatives)
		objective.ApprovedCreatives = creativesJSON
	}
	if len(req.EverflowOfferIDs) > 0 {
		offerIDsJSON, _ := json.Marshal(req.EverflowOfferIDs)
		objective.EverflowOfferIDs = offerIDsJSON
	}

	if err := svc.store.CreateObjective(ctx, objective); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, objective)
}

// HandleUpdateObjective updates an objective
// PUT /api/mailing/campaigns/{campaignId}/objective
func (svc *CampaignObjectivesService) HandleUpdateObjective(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	// Get existing objective
	existing, err := svc.store.GetObjective(ctx, campaignID)
	if err != nil {
		writeJSONError(w, "objective not found", http.StatusNotFound)
		return
	}

	var req CreateObjectiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Update fields from request (only non-zero values)
	if req.Purpose != "" {
		existing.Purpose = req.Purpose
	}
	if req.ActivationGoal != "" {
		existing.ActivationGoal = req.ActivationGoal
	}
	if req.TargetEngagementRate != nil {
		existing.TargetEngagementRate = req.TargetEngagementRate
	}
	if req.TargetCleanRate != nil {
		existing.TargetCleanRate = req.TargetCleanRate
	}
	if req.OfferModel != "" {
		existing.OfferModel = req.OfferModel
	}
	if req.ECPMTarget != nil {
		existing.ECPMTarget = req.ECPMTarget
	}
	if req.BudgetLimit != nil {
		existing.BudgetLimit = req.BudgetLimit
	}
	if req.TargetMetric != "" {
		existing.TargetMetric = req.TargetMetric
	}
	if req.TargetValue > 0 {
		existing.TargetValue = req.TargetValue
	}
	if req.EverflowSubIDTemplate != "" {
		existing.EverflowSubIDTemplate = req.EverflowSubIDTemplate
	}
	if req.PropertyCode != "" {
		existing.PropertyCode = req.PropertyCode
	}
	if req.RotationStrategy != "" {
		existing.RotationStrategy = req.RotationStrategy
	}
	if req.ThroughputSensitivity != "" {
		existing.ThroughputSensitivity = req.ThroughputSensitivity
	}
	if req.MinThroughputRate > 0 {
		existing.MinThroughputRate = req.MinThroughputRate
	}
	if req.MaxThroughputRate > 0 {
		existing.MaxThroughputRate = req.MaxThroughputRate
	}
	if req.TargetCompletionHours > 0 {
		existing.TargetCompletionHours = req.TargetCompletionHours
	}
	if req.PacingStrategy != "" {
		existing.PacingStrategy = req.PacingStrategy
	}

	// Handle boolean updates
	if req.AIOptimizationEnabled != nil {
		existing.AIOptimizationEnabled = *req.AIOptimizationEnabled
	}
	if req.AIThroughputOptimization != nil {
		existing.AIThroughputOptimization = *req.AIThroughputOptimization
	}
	if req.AICreativeRotation != nil {
		existing.AICreativeRotation = *req.AICreativeRotation
	}
	if req.AIBudgetPacing != nil {
		existing.AIBudgetPacing = *req.AIBudgetPacing
	}
	if req.ESPSignalMonitoring != nil {
		existing.ESPSignalMonitoring = *req.ESPSignalMonitoring
	}
	if req.PauseOnSpamSignal != nil {
		existing.PauseOnSpamSignal = *req.PauseOnSpamSignal
	}
	if req.SpamSignalThreshold != nil {
		existing.SpamSignalThreshold = req.SpamSignalThreshold
	}
	if req.BounceThreshold != nil {
		existing.BounceThreshold = req.BounceThreshold
	}

	// Marshal JSON fields
	if len(req.ApprovedCreatives) > 0 {
		creativesJSON, _ := json.Marshal(req.ApprovedCreatives)
		existing.ApprovedCreatives = creativesJSON
	}
	if len(req.EverflowOfferIDs) > 0 {
		offerIDsJSON, _ := json.Marshal(req.EverflowOfferIDs)
		existing.EverflowOfferIDs = offerIDsJSON
	}

	if err := svc.store.UpdateObjective(ctx, existing); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

// HandleDeleteObjective deletes an objective
// DELETE /api/mailing/campaigns/{campaignId}/objective
func (svc *CampaignObjectivesService) HandleDeleteObjective(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	if err := svc.store.DeleteObjective(r.Context(), campaignID); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// SIGNAL HANDLERS
// =============================================================================

// HandleGetSignals returns ESP signals for a campaign
// GET /api/mailing/campaigns/{campaignId}/signals
func (svc *CampaignObjectivesService) HandleGetSignals(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	// Default to last hour
	since := time.Now().Add(-1 * time.Hour)
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if parsed, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = parsed
		}
	}

	signals, err := svc.store.GetRecentSignals(r.Context(), campaignID, since)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if signals == nil {
		signals = []mailing.ESPSignal{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"signals": signals,
		"total":   len(signals),
		"since":   since,
	})
}

// HandleGetSignalSummary returns aggregated signal counts
// GET /api/mailing/campaigns/{campaignId}/signals/summary
func (svc *CampaignObjectivesService) HandleGetSignalSummary(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	since := time.Now().Add(-1 * time.Hour)
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if parsed, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = parsed
		}
	}

	summary, err := svc.store.GetSignalSummary(r.Context(), campaignID, since)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// HandleRecordSignal records a new ESP signal
// POST /api/mailing/campaigns/{campaignId}/signals
func (svc *CampaignObjectivesService) HandleRecordSignal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	var signal mailing.ESPSignal
	if err := json.NewDecoder(r.Body).Decode(&signal); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	signal.CampaignID = campaignID
	signal.OrganizationID = getOrgIDFromContext(ctx)

	if err := svc.store.RecordESPSignal(ctx, &signal); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, signal)
}

// =============================================================================
// OPTIMIZATION LOG HANDLERS
// =============================================================================

// HandleGetOptimizationLogs returns optimization logs for a campaign
// GET /api/mailing/campaigns/{campaignId}/optimizations
func (svc *CampaignObjectivesService) HandleGetOptimizationLogs(w http.ResponseWriter, r *http.Request) {
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	// Default to last 24 hours
	since := time.Now().Add(-24 * time.Hour)
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if parsed, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = parsed
		}
	}

	logs, err := svc.store.GetOptimizationLogs(r.Context(), campaignID, since)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if logs == nil {
		logs = []mailing.CampaignOptimizationLog{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"optimization_logs": logs,
		"total":             len(logs),
		"since":             since,
	})
}

// HandleLogOptimization records an optimization decision
// POST /api/mailing/campaigns/{campaignId}/optimizations
func (svc *CampaignObjectivesService) HandleLogOptimization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID, err := uuid.Parse(chi.URLParam(r, "campaignId"))
	if err != nil {
		writeJSONError(w, "invalid campaign ID", http.StatusBadRequest)
		return
	}

	var log mailing.CampaignOptimizationLog
	if err := json.NewDecoder(r.Body).Decode(&log); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	log.CampaignID = campaignID
	log.OrganizationID = getOrgIDFromContext(ctx)

	if err := svc.store.LogOptimization(ctx, &log); err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, log)
}

// Note: getOrgIDFromContext is defined in segment_cleanup_handlers.go

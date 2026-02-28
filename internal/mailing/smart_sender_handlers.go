package mailing

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// SmartSenderHandlers provides HTTP handlers for AI smart sender API
type SmartSenderHandlers struct {
	smartSender *SmartSender
	store       *Store
}

// NewSmartSenderHandlers creates new smart sender handlers
func NewSmartSenderHandlers(smartSender *SmartSender, store *Store) *SmartSenderHandlers {
	return &SmartSenderHandlers{
		smartSender: smartSender,
		store:       store,
	}
}

// ============================================================================
// AI SETTINGS HANDLERS
// ============================================================================

// HandleGetAISettings returns AI settings for a campaign
// GET /api/mailing/campaigns/{campaignId}/ai-settings
func (h *SmartSenderHandlers) HandleGetAISettings(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	settings, err := h.smartSender.GetAISettings(r.Context(), campaignID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get AI settings: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"settings": settings,
	})
}

// HandleUpdateAISettings creates or updates AI settings for a campaign
// POST /api/mailing/campaigns/{campaignId}/ai-settings
func (h *SmartSenderHandlers) HandleUpdateAISettings(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	var req CreateAISettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	req.CampaignID = campaignID

	settings, err := h.smartSender.SaveAISettings(r.Context(), &req)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save AI settings: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"settings": settings,
		"message":  "AI settings updated successfully",
	})
}

// ============================================================================
// REAL-TIME METRICS HANDLERS
// ============================================================================

// HandleGetRealtimeMetrics returns real-time metrics for a campaign
// GET /api/mailing/campaigns/{campaignId}/realtime-metrics
func (h *SmartSenderHandlers) HandleGetRealtimeMetrics(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	// Get limit from query params (default: 60 = last hour)
	limit := 60
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := parseInt(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	metrics, err := h.smartSender.GetRealtimeMetrics(r.Context(), campaignID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get real-time metrics: "+err.Error())
		return
	}

	// Calculate summary
	var summary struct {
		TotalSent       int     `json:"total_sent"`
		TotalOpens      int     `json:"total_opens"`
		TotalClicks     int     `json:"total_clicks"`
		TotalBounces    int     `json:"total_bounces"`
		TotalComplaints int     `json:"total_complaints"`
		OpenRate        float64 `json:"open_rate"`
		ClickRate       float64 `json:"click_rate"`
		BounceRate      float64 `json:"bounce_rate"`
		ComplaintRate   float64 `json:"complaint_rate"`
	}

	if len(metrics) > 0 {
		latest := metrics[0]
		summary.TotalSent = latest.CumulativeSent
		summary.TotalOpens = latest.CumulativeOpens
		summary.TotalClicks = latest.CumulativeClicks
		summary.TotalBounces = latest.CumulativeBounces
		summary.TotalComplaints = latest.CumulativeComplaints
		summary.OpenRate = latest.OpenRate
		summary.ClickRate = latest.ClickRate
		summary.BounceRate = latest.BounceRate
		summary.ComplaintRate = latest.ComplaintRate
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"metrics": metrics,
		"summary": summary,
		"count":   len(metrics),
	})
}

// ============================================================================
// AI DECISIONS HANDLERS
// ============================================================================

// HandleGetAIDecisions returns AI decision log for a campaign
// GET /api/mailing/campaigns/{campaignId}/ai-decisions
func (h *SmartSenderHandlers) HandleGetAIDecisions(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := parseInt(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	decisions, err := h.smartSender.GetAIDecisions(r.Context(), campaignID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get AI decisions: "+err.Error())
		return
	}

	// Group decisions by type
	decisionsByType := make(map[string]int)
	for _, d := range decisions {
		decisionsByType[string(d.DecisionType)]++
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"decisions":    decisions,
		"total":        len(decisions),
		"by_type":      decisionsByType,
	})
}

// ============================================================================
// AI OPTIMIZATION HANDLERS
// ============================================================================

// HandleTriggerOptimization manually triggers AI optimization for a campaign
// POST /api/mailing/campaigns/{campaignId}/ai-optimize
func (h *SmartSenderHandlers) HandleTriggerOptimization(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	result, err := h.smartSender.OptimizeThrottle(r.Context(), campaignID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "optimization failed: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"result":  result,
		"message": "Optimization completed",
	})
}

// ============================================================================
// CAMPAIGN HEALTH HANDLERS
// ============================================================================

// HandleGetCampaignHealth returns campaign health score
// GET /api/mailing/campaigns/{campaignId}/health
func (h *SmartSenderHandlers) HandleGetCampaignHealth(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	health, err := h.smartSender.GetCampaignHealthScore(r.Context(), campaignID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get campaign health: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, health)
}

// ============================================================================
// ALERT HANDLERS
// ============================================================================

// HandleGetAlerts returns alerts for a campaign
// GET /api/mailing/campaigns/{campaignId}/alerts
func (h *SmartSenderHandlers) HandleGetAlerts(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	unacknowledgedOnly := r.URL.Query().Get("unacknowledged_only") == "true"

	alerts, err := h.smartSender.GetCampaignAlerts(r.Context(), campaignID, unacknowledgedOnly)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get alerts: "+err.Error())
		return
	}

	// Count by severity
	bySeverity := make(map[string]int)
	for _, a := range alerts {
		bySeverity[string(a.Severity)]++
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"alerts":      alerts,
		"total":       len(alerts),
		"by_severity": bySeverity,
	})
}

// HandleAcknowledgeAlert acknowledges an alert
// POST /api/mailing/alerts/{alertId}/acknowledge
func (h *SmartSenderHandlers) HandleAcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	alertIDStr := r.PathValue("alertId")
	alertID, err := uuid.Parse(alertIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid alert ID")
		return
	}

	// Get user ID from context (in production, extract from JWT)
	userID := uuid.Nil
	if userIDStr := r.Header.Get("X-User-ID"); userIDStr != "" {
		userID, _ = uuid.Parse(userIDStr)
	}

	if err := h.smartSender.AcknowledgeAlert(r.Context(), alertID, userID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to acknowledge alert: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message": "Alert acknowledged",
	})
}

// ============================================================================
// SEND TIME OPTIMIZATION HANDLERS
// ============================================================================

// HandleGetOptimalSendTime returns optimal send time for an email
// GET /api/mailing/optimal-send-time?email=...
func (h *SmartSenderHandlers) HandleGetOptimalSendTime(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		respondError(w, http.StatusBadRequest, "email parameter required")
		return
	}

	if !ValidateEmail(email) {
		respondError(w, http.StatusBadRequest, "invalid email address")
		return
	}

	result, err := h.smartSender.GetOptimalSendTime(r.Context(), email)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to calculate optimal send time: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleGetBatchOptimalSendTimes returns optimal send times for multiple emails
// POST /api/mailing/optimal-send-times
func (h *SmartSenderHandlers) HandleGetBatchOptimalSendTimes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails []string `json:"emails"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Emails) == 0 {
		respondError(w, http.StatusBadRequest, "emails array required")
		return
	}

	if len(req.Emails) > 100 {
		respondError(w, http.StatusBadRequest, "maximum 100 emails per request")
		return
	}

	results := make([]*OptimalSendTimeResult, 0, len(req.Emails))
	for _, email := range req.Emails {
		result, err := h.smartSender.GetOptimalSendTime(r.Context(), email)
		if err != nil {
			continue
		}
		results = append(results, result)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
}

// ============================================================================
// A/B TEST HANDLERS
// ============================================================================

// HandleGetABVariants returns A/B test variants for a campaign
// GET /api/mailing/campaigns/{campaignId}/ab-variants
func (h *SmartSenderHandlers) HandleGetABVariants(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	variants, err := h.getABVariants(r, campaignID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get A/B variants: "+err.Error())
		return
	}

	// Find winner if any
	var winner *ABVariant
	for _, v := range variants {
		if v.IsWinner {
			winner = v
			break
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"variants":        variants,
		"total":           len(variants),
		"winner":          winner,
		"winner_declared": winner != nil,
	})
}

// HandleCreateABVariant creates a new A/B test variant
// POST /api/mailing/campaigns/{campaignId}/ab-variants
func (h *SmartSenderHandlers) HandleCreateABVariant(w http.ResponseWriter, r *http.Request) {
	campaignIDStr := r.PathValue("campaignId")
	campaignID, err := uuid.Parse(campaignIDStr)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid campaign ID")
		return
	}

	var req struct {
		VariantName       string `json:"variant_name"`
		VariantType       string `json:"variant_type"`
		VariantValue      string `json:"variant_value"`
		TrafficPercentage int    `json:"traffic_percentage"`
		IsControl         bool   `json:"is_control"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.VariantName == "" || req.VariantType == "" || req.VariantValue == "" {
		respondError(w, http.StatusBadRequest, "variant_name, variant_type, and variant_value are required")
		return
	}

	if req.TrafficPercentage <= 0 || req.TrafficPercentage > 100 {
		req.TrafficPercentage = 50
	}

	var id uuid.UUID
	err = h.smartSender.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_campaign_ab_variants (
			campaign_id, variant_name, variant_type, variant_value,
			traffic_percentage, is_control
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, campaignID, req.VariantName, req.VariantType, req.VariantValue, req.TrafficPercentage, req.IsControl).Scan(&id)

	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create variant: "+err.Error())
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":      id,
		"message": "A/B variant created",
	})
}

func (h *SmartSenderHandlers) getABVariants(r *http.Request, campaignID uuid.UUID) ([]*ABVariant, error) {
	rows, err := h.smartSender.db.QueryContext(r.Context(), `
		SELECT id, campaign_id, variant_name, variant_type, variant_value,
		       traffic_percentage, sent_count, delivered_count, open_count,
		       unique_open_count, click_count, unique_click_count, conversion_count,
		       revenue, open_rate, click_rate, conversion_rate, revenue_per_send,
		       z_score, p_value, confidence_level, is_winner, is_control,
		       status, declared_winner_at, created_at, updated_at
		FROM mailing_campaign_ab_variants
		WHERE campaign_id = $1
		ORDER BY is_control DESC, variant_name
	`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var variants []*ABVariant
	for rows.Next() {
		var v ABVariant
		err := rows.Scan(
			&v.ID, &v.CampaignID, &v.VariantName, &v.VariantType, &v.VariantValue,
			&v.TrafficPercentage, &v.SentCount, &v.DeliveredCount, &v.OpenCount,
			&v.UniqueOpenCount, &v.ClickCount, &v.UniqueClickCount, &v.ConversionCount,
			&v.Revenue, &v.OpenRate, &v.ClickRate, &v.ConversionRate, &v.RevenuePerSend,
			&v.ZScore, &v.PValue, &v.ConfidenceLevel, &v.IsWinner, &v.IsControl,
			&v.Status, &v.DeclaredWinnerAt, &v.CreatedAt, &v.UpdatedAt,
		)
		if err != nil {
			continue
		}
		variants = append(variants, &v)
	}

	return variants, nil
}

// ============================================================================
// INBOX PROFILE HANDLERS
// ============================================================================

// HandleGetInboxProfile returns inbox profile for an email
// GET /api/mailing/inbox-profile?email=...
func (h *SmartSenderHandlers) HandleGetInboxProfile(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		respondError(w, http.StatusBadRequest, "email parameter required")
		return
	}

	emailHash := hashEmail(email)
	profile, err := h.smartSender.getInboxProfile(r.Context(), emailHash)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get inbox profile: "+err.Error())
		return
	}

	if profile == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"profile": nil,
			"message": "No profile found for this email",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"profile": profile,
	})
}

// ============================================================================
// ROUTE REGISTRATION
// ============================================================================

// RegisterSmartSenderRoutes registers all smart sender routes
func (h *SmartSenderHandlers) RegisterRoutes(mux *http.ServeMux) {
	// AI Settings
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/ai-settings", h.HandleGetAISettings)
	mux.HandleFunc("POST /api/mailing/campaigns/{campaignId}/ai-settings", h.HandleUpdateAISettings)

	// Real-time Metrics
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/realtime-metrics", h.HandleGetRealtimeMetrics)

	// AI Decisions
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/ai-decisions", h.HandleGetAIDecisions)

	// AI Optimization
	mux.HandleFunc("POST /api/mailing/campaigns/{campaignId}/ai-optimize", h.HandleTriggerOptimization)

	// Campaign Health
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/health", h.HandleGetCampaignHealth)

	// Alerts
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/alerts", h.HandleGetAlerts)
	mux.HandleFunc("POST /api/mailing/alerts/{alertId}/acknowledge", h.HandleAcknowledgeAlert)

	// Send Time Optimization
	mux.HandleFunc("GET /api/mailing/optimal-send-time", h.HandleGetOptimalSendTime)
	mux.HandleFunc("POST /api/mailing/optimal-send-times", h.HandleGetBatchOptimalSendTimes)

	// A/B Testing
	mux.HandleFunc("GET /api/mailing/campaigns/{campaignId}/ab-variants", h.HandleGetABVariants)
	mux.HandleFunc("POST /api/mailing/campaigns/{campaignId}/ab-variants", h.HandleCreateABVariant)

	// Inbox Profiles
	mux.HandleFunc("GET /api/mailing/inbox-profile", h.HandleGetInboxProfile)
}

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

func parseInt(s string) (int, error) {
	var i int
	_, err := fmt.Sscanf(s, "%d", &i)
	return i, err
}

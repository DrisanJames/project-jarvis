package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// AIContentHandlers provides HTTP handlers for AI content endpoints
type AIContentHandlers struct {
	service *mailing.AIContentService
	db      *sql.DB
}

// NewAIContentHandlers creates a new AIContentHandlers instance
func NewAIContentHandlers(db *sql.DB) *AIContentHandlers {
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")

	return &AIContentHandlers{
		service: mailing.NewAIContentService(db, anthropicKey, openaiKey),
		db:      db,
	}
}

// RegisterRoutes registers AI content routes on the router
// Note: This registers directly on the passed router (expects to be called within an /ai group)
func (h *AIContentHandlers) RegisterRoutes(r chi.Router) {
	r.Post("/subject-lines", h.HandleGenerateSubjectLines)
	r.Post("/analyze-content", h.HandleAnalyzeContent)
	r.Post("/predict-performance", h.HandlePredictPerformance)
	r.Post("/recommend-segments", h.HandleRecommendSegments)
	r.Post("/improve-content", h.HandleImproveContent)
	r.Post("/generate-ctas", h.HandleGenerateCTAs)
}

// SubjectLineRequest represents the request for subject line generation
type SubjectLineRequest struct {
	HTMLContent  string `json:"html_content"`
	ProductInfo  string `json:"product_info"`
	TargetTone   string `json:"target_tone"`   // professional, casual, urgent, curious
	AudienceType string `json:"audience_type"` // b2b, b2c, mixed
	MaxLength    int    `json:"max_length"`
	Count        int    `json:"count"`
	IncludeEmoji bool   `json:"include_emoji"`
	Industry     string `json:"industry"`
	CampaignType string `json:"campaign_type"` // newsletter, promotional, transactional, announcement
}

// SubjectLineResponse represents the response for subject line generation
type SubjectLineResponse struct {
	Suggestions []mailing.SubjectSuggestion `json:"suggestions"`
	GeneratedAt string                      `json:"generated_at"`
	Context     map[string]interface{}      `json:"context"`
}

// HandleGenerateSubjectLines generates AI-powered subject line suggestions
// POST /api/mailing/ai/subject-lines
func (h *AIContentHandlers) HandleGenerateSubjectLines(w http.ResponseWriter, r *http.Request) {
	var req SubjectLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Convert to mailing.SubjectParams
	params := mailing.SubjectParams{
		HTMLContent:  req.HTMLContent,
		ProductInfo:  req.ProductInfo,
		TargetTone:   req.TargetTone,
		AudienceType: req.AudienceType,
		MaxLength:    req.MaxLength,
		Count:        req.Count,
		IncludeEmoji: req.IncludeEmoji,
		Industry:     req.Industry,
		CampaignType: req.CampaignType,
	}

	suggestions, err := h.service.GenerateSubjectLines(r.Context(), params)
	if err != nil {
		http.Error(w, `{"error":"failed to generate subject lines: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := SubjectLineResponse{
		Suggestions: suggestions,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Context: map[string]interface{}{
			"tone":          req.TargetTone,
			"audience_type": req.AudienceType,
			"max_length":    req.MaxLength,
			"count":         req.Count,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// AnalyzeContentRequest represents the request for content analysis
type AnalyzeContentRequest struct {
	HTMLContent string `json:"html_content"`
}

// AnalyzeContentResponse represents the response for content analysis
type AnalyzeContentResponse struct {
	Analysis    *mailing.ContentAnalysis `json:"analysis"`
	AnalyzedAt  string                   `json:"analyzed_at"`
	Recommendations []string             `json:"recommendations"`
}

// HandleAnalyzeContent analyzes email content for spam triggers and readability
// POST /api/mailing/ai/analyze-content
func (h *AIContentHandlers) HandleAnalyzeContent(w http.ResponseWriter, r *http.Request) {
	var req AnalyzeContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.HTMLContent == "" {
		http.Error(w, `{"error":"html_content is required"}`, http.StatusBadRequest)
		return
	}

	analysis, err := h.service.AnalyzeContent(r.Context(), req.HTMLContent)
	if err != nil {
		http.Error(w, `{"error":"failed to analyze content: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := AnalyzeContentResponse{
		Analysis:        analysis,
		AnalyzedAt:      time.Now().UTC().Format(time.RFC3339),
		Recommendations: analysis.Suggestions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// PredictPerformanceRequest represents the request for performance prediction
type PredictPerformanceRequest struct {
	CampaignID string `json:"campaign_id"`
	// Or provide campaign details directly
	Subject                string `json:"subject"`
	HTMLContent            string `json:"html_content"`
	PreviewText            string `json:"preview_text"`
	AISendTimeOptimization bool   `json:"ai_send_time_optimization"`
}

// PredictPerformanceResponse represents the response for performance prediction
type PredictPerformanceResponse struct {
	Prediction  *mailing.PerformancePrediction `json:"prediction"`
	PredictedAt string                         `json:"predicted_at"`
	CampaignID  string                         `json:"campaign_id,omitempty"`
}

// HandlePredictPerformance predicts campaign performance
// POST /api/mailing/ai/predict-performance
func (h *AIContentHandlers) HandlePredictPerformance(w http.ResponseWriter, r *http.Request) {
	var req PredictPerformanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var campaign *mailing.Campaign
	var campaignIDStr string

	// If campaign ID is provided, fetch it from database
	if req.CampaignID != "" {
		campaignID, err := uuid.Parse(req.CampaignID)
		if err != nil {
			http.Error(w, `{"error":"invalid campaign_id"}`, http.StatusBadRequest)
			return
		}

		// Fetch campaign from database
		query := `SELECT id, organization_id, subject, html_content, preview_text, 
			ai_send_time_optimization, ai_content_optimization 
			FROM mailing_campaigns WHERE id = $1`
		
		campaign = &mailing.Campaign{}
		var orgID uuid.UUID
		err = h.db.QueryRowContext(r.Context(), query, campaignID).Scan(
			&campaign.ID, &orgID, &campaign.Subject, &campaign.HTMLContent,
			&campaign.PreviewText, &campaign.AISendTimeOptimization, &campaign.AIContentOptimization,
		)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"failed to fetch campaign"}`, http.StatusInternalServerError)
			return
		}
		campaign.OrganizationID = orgID
		campaignIDStr = campaignID.String()
	} else {
		// Create a temporary campaign from request data
		if req.Subject == "" && req.HTMLContent == "" {
			http.Error(w, `{"error":"either campaign_id or subject/html_content is required"}`, http.StatusBadRequest)
			return
		}
		
		// Get org ID from dynamic context
		defaultOrgID, err := GetOrgIDFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
		campaign = &mailing.Campaign{
			ID:                     uuid.New(),
			OrganizationID:         defaultOrgID,
			Subject:                req.Subject,
			HTMLContent:            req.HTMLContent,
			PreviewText:            req.PreviewText,
			AISendTimeOptimization: req.AISendTimeOptimization,
		}
	}

	prediction, err := h.service.PredictPerformance(r.Context(), campaign)
	if err != nil {
		http.Error(w, `{"error":"failed to predict performance: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := PredictPerformanceResponse{
		Prediction:  prediction,
		PredictedAt: time.Now().UTC().Format(time.RFC3339),
		CampaignID:  campaignIDStr,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// RecommendSegmentsRequest represents the request for segment recommendations
type RecommendSegmentsRequest struct {
	OrganizationID string `json:"organization_id"`
	Goal           string `json:"goal"` // revenue, engagement, list_growth, retention
}

// RecommendSegmentsResponse represents the response for segment recommendations
type RecommendSegmentsResponse struct {
	Recommendations []mailing.SegmentRecommendation `json:"recommendations"`
	Goal            string                          `json:"goal"`
	GeneratedAt     string                          `json:"generated_at"`
}

// HandleRecommendSegments recommends audience segments based on goal
// POST /api/mailing/ai/recommend-segments
func (h *AIContentHandlers) HandleRecommendSegments(w http.ResponseWriter, r *http.Request) {
	var req RecommendSegmentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Use dynamic org context if not provided
	orgID := req.OrganizationID
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	// Default goal
	goal := req.Goal
	if goal == "" {
		goal = "engagement"
	}

	recommendations, err := h.service.RecommendSegments(r.Context(), orgID, goal)
	if err != nil {
		http.Error(w, `{"error":"failed to recommend segments: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := RecommendSegmentsResponse{
		Recommendations: recommendations,
		Goal:            goal,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ImproveContentRequest represents the request for content improvement
type ImproveContentRequest struct {
	HTMLContent string `json:"html_content"`
	Goal        string `json:"goal"` // engagement, deliverability, conversions, readability
}

// ImproveContentResponse represents the response for content improvement
type ImproveContentResponse struct {
	ImprovedContent string   `json:"improved_content"`
	Improvements    []string `json:"improvements"`
	ImprovedAt      string   `json:"improved_at"`
}

// HandleImproveContent suggests improvements for email content
// POST /api/mailing/ai/improve-content
func (h *AIContentHandlers) HandleImproveContent(w http.ResponseWriter, r *http.Request) {
	var req ImproveContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.HTMLContent == "" {
		http.Error(w, `{"error":"html_content is required"}`, http.StatusBadRequest)
		return
	}

	// Default goal
	goal := req.Goal
	if goal == "" {
		goal = "engagement"
	}

	improvedContent, improvements, err := h.service.ImproveContent(r.Context(), req.HTMLContent, goal)
	if err != nil {
		http.Error(w, `{"error":"failed to improve content: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := ImproveContentResponse{
		ImprovedContent: improvedContent,
		Improvements:    improvements,
		ImprovedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// GenerateCTAsRequest represents the request for CTA generation
type GenerateCTAsRequest struct {
	ProductInfo string `json:"product_info"`
	Count       int    `json:"count"`
}

// GenerateCTAsResponse represents the response for CTA generation
type GenerateCTAsResponse struct {
	CTAs        []string `json:"ctas"`
	GeneratedAt string   `json:"generated_at"`
}

// HandleGenerateCTAs generates call-to-action suggestions
// POST /api/mailing/ai/generate-ctas
func (h *AIContentHandlers) HandleGenerateCTAs(w http.ResponseWriter, r *http.Request) {
	var req GenerateCTAsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if req.ProductInfo == "" {
		http.Error(w, `{"error":"product_info is required"}`, http.StatusBadRequest)
		return
	}

	// Default count
	count := req.Count
	if count == 0 {
		count = 5
	}

	ctas, err := h.service.GenerateCTAs(r.Context(), req.ProductInfo, count)
	if err != nil {
		http.Error(w, `{"error":"failed to generate CTAs: `+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	response := GenerateCTAsResponse{
		CTAs:        ctas,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

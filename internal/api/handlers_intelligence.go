package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/intelligence"
)

// ========== Intelligence Service Handlers ==========

// SetIntelligenceService sets the intelligence service
func (h *Handlers) SetIntelligenceService(svc *intelligence.Service) {
	h.intelligenceService = svc
}

// GetIntelligenceDashboard returns the complete intelligence overview
func (h *Handlers) GetIntelligenceDashboard(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	memory := h.intelligenceService.GetMemory()
	respondJSON(w, http.StatusOK, memory)
}

// GetIntelligenceRecommendations returns current recommendations
func (h *Handlers) GetIntelligenceRecommendations(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	recommendations := h.intelligenceService.GetRecommendations()
	respondJSON(w, http.StatusOK, recommendations)
}

// GetPropertyOfferIntelligence returns property-offer analysis
func (h *Handlers) GetPropertyOfferIntelligence(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	insights := h.intelligenceService.GetPropertyOfferInsights()
	respondJSON(w, http.StatusOK, insights)
}

// GetTimingIntelligence returns send time optimization data
func (h *Handlers) GetTimingIntelligence(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	patterns := h.intelligenceService.GetTimingPatterns()
	respondJSON(w, http.StatusOK, patterns)
}

// GetESPISPIntelligence returns ESP-ISP optimization data
func (h *Handlers) GetESPISPIntelligence(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	matrix := h.intelligenceService.GetESPISPMatrix()
	respondJSON(w, http.StatusOK, matrix)
}

// GetStrategyIntelligence returns strategic insights
func (h *Handlers) GetStrategyIntelligence(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	insights := h.intelligenceService.GetStrategyInsights()
	respondJSON(w, http.StatusOK, insights)
}

// TriggerLearningCycle manually triggers a learning cycle
func (h *Handlers) TriggerLearningCycle(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	// Run learning cycle in background
	go h.intelligenceService.RunLearningCycle()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "Learning cycle triggered",
		"timestamp": time.Now(),
	})
}

// UpdateRecommendationStatus updates a recommendation's status
func (h *Handlers) UpdateRecommendationStatus(w http.ResponseWriter, r *http.Request) {
	if h.intelligenceService == nil {
		respondError(w, http.StatusServiceUnavailable, "Intelligence service not configured")
		return
	}

	id := chi.URLParam(r, "id")
	if id == "" {
		respondError(w, http.StatusBadRequest, "recommendation ID is required")
		return
	}

	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if err := h.intelligenceService.UpdateRecommendationStatus(id, req.Status); err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"id":      id,
		"status":  req.Status,
	})
}

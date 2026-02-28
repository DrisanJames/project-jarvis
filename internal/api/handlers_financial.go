package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/financial"
	"github.com/ignite/sparkpost-monitor/internal/storage"
)

// ========== Financial Dashboard Handlers ==========

// GetFinancialDashboard returns the complete financial dashboard data
func (h *Handlers) GetFinancialDashboard(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	dashboard := h.revenueModelService.GetFinancialDashboard()
	respondJSON(w, http.StatusOK, dashboard)
}

// GetCostBreakdown returns detailed cost breakdown
func (h *Handlers) GetCostBreakdown(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	breakdown := h.revenueModelService.GetCostBreakdown()
	respondJSON(w, http.StatusOK, breakdown)
}

// GetAnnualForecast returns the annual forecast based on current actuals
func (h *Handlers) GetAnnualForecast(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	currentMonth := h.revenueModelService.GetCurrentMonthPL()
	forecast := h.revenueModelService.GetAnnualForecast(currentMonth)
	respondJSON(w, http.StatusOK, forecast)
}

// GetCurrentMonthPL returns current month P&L
func (h *Handlers) GetCurrentMonthPL(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	pl := h.revenueModelService.GetCurrentMonthPL()
	respondJSON(w, http.StatusOK, pl)
}

// GetCostsByCategory returns costs grouped by category for charts
func (h *Handlers) GetCostsByCategory(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	categories := h.revenueModelService.GetCostsByCategory()
	respondJSON(w, http.StatusOK, categories)
}

// GetScenarioPlanning returns scenario planning with growth projections
func (h *Handlers) GetScenarioPlanning(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	// Default request (no overrides)
	response := h.revenueModelService.GetScenarioPlanningResponse(nil)
	respondJSON(w, http.StatusOK, response)
}

// PostScenarioPlanning generates forecasts with custom parameters and cost overrides
func (h *Handlers) PostScenarioPlanning(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	var req financial.ScenarioPlanningRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	response := h.revenueModelService.GetScenarioPlanningResponse(&req)
	respondJSON(w, http.StatusOK, response)
}

// GetGrowthDrivers returns the growth drivers analysis
func (h *Handlers) GetGrowthDrivers(w http.ResponseWriter, r *http.Request) {
	if h.revenueModelService == nil {
		respondError(w, http.StatusServiceUnavailable, "Revenue model service not configured")
		return
	}

	drivers := h.revenueModelService.CalculateGrowthDrivers()
	respondJSON(w, http.StatusOK, drivers)
}

// ========== Cost Configuration Handlers ==========

// CostConfigRequest represents a request to save cost configuration
type CostConfigRequest struct {
	ConfigType string                    `json:"config_type"` // "vendor", "esp", "payroll", "revenue_share"
	Items      []storage.CostConfigItem  `json:"items"`
}

// CostConfigResponse represents the response for cost configuration
type CostConfigResponse struct {
	Timestamp    time.Time                           `json:"timestamp"`
	Configs      map[string][]storage.CostConfigItem `json:"configs"`
	LastUpdated  map[string]string                   `json:"last_updated"`
}

// GetCostConfigs retrieves all saved cost configurations
func (h *Handlers) GetCostConfigs(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not configured")
		return
	}

	ctx := r.Context()
	configs, err := h.storage.GetAllCostConfigurations(ctx)
	if err != nil {
		log.Printf("ERROR: failed to get cost configurations: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve cost configurations")
		return
	}

	respondJSON(w, http.StatusOK, CostConfigResponse{
		Timestamp: time.Now(),
		Configs:   configs,
	})
}

// SaveCostConfig saves a cost configuration
func (h *Handlers) SaveCostConfig(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not configured")
		return
	}

	var req CostConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	if req.ConfigType == "" {
		respondError(w, http.StatusBadRequest, "config_type is required")
		return
	}

	ctx := r.Context()
	if err := h.storage.SaveCostConfiguration(ctx, req.ConfigType, req.Items); err != nil {
		log.Printf("ERROR: failed to save cost configuration: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to save cost configuration")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"config_type": req.ConfigType,
		"items_saved": len(req.Items),
		"timestamp":   time.Now(),
	})
}

// SaveAllCostConfigs saves all cost configurations at once
func (h *Handlers) SaveAllCostConfigs(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not configured")
		return
	}

	var req map[string][]storage.CostConfigItem
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	ctx := r.Context()
	savedTypes := []string{}
	
	for configType, items := range req {
		if err := h.storage.SaveCostConfiguration(ctx, configType, items); err != nil {
			log.Printf("ERROR: failed to save cost config %s: %v", configType, err)
			respondError(w, http.StatusInternalServerError, "Failed to save cost configuration")
			return
		}
		savedTypes = append(savedTypes, configType)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"saved_types": savedTypes,
		"timestamp":   time.Now(),
	})
}

// DeleteCostConfig deletes a cost configuration
func (h *Handlers) DeleteCostConfig(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not configured")
		return
	}

	configType := chi.URLParam(r, "type")
	if configType == "" {
		respondError(w, http.StatusBadRequest, "config type is required")
		return
	}

	ctx := r.Context()
	if err := h.storage.DeleteCostConfiguration(ctx, configType); err != nil {
		log.Printf("ERROR: failed to delete cost configuration %s: %v", configType, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete cost configuration")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"config_type": configType,
		"deleted":     true,
	})
}

// ResetCostConfigs resets all cost configurations to defaults (deletes overrides)
func (h *Handlers) ResetCostConfigs(w http.ResponseWriter, r *http.Request) {
	if h.storage == nil {
		respondError(w, http.StatusServiceUnavailable, "Storage not configured")
		return
	}

	ctx := r.Context()
	configTypes := []string{"vendor", "esp", "payroll", "revenue_share"}
	
	for _, configType := range configTypes {
		_ = h.storage.DeleteCostConfiguration(ctx, configType) // Ignore errors for non-existent configs
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"message":   "All cost configurations reset to defaults",
		"timestamp": time.Now(),
	})
}

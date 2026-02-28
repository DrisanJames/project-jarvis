package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/agent"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
)

func (h *Handlers) GetSummary(w http.ResponseWriter, r *http.Request) {
	summary := h.collector.GetLatestSummary()
	if summary == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No metrics data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, summary)
}

// ISP endpoints

// GetISPMetrics returns metrics for all ISPs
func (h *Handlers) GetISPMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := h.collector.GetLatestISPMetrics()
	if metrics == nil {
		metrics = []sparkpost.ISPMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// GetISPMetricsByProvider returns metrics for a specific ISP
func (h *Handlers) GetISPMetricsByProvider(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if provider == "" {
		respondError(w, http.StatusBadRequest, "provider parameter required")
		return
	}

	metrics := h.collector.GetLatestISPMetrics()
	for _, isp := range metrics {
		if isp.Provider == provider {
			respondJSON(w, http.StatusOK, isp)
			return
		}
	}

	respondError(w, http.StatusNotFound, "provider not found")
}

// IP endpoints

// GetIPMetrics returns metrics for all sending IPs
func (h *Handlers) GetIPMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := h.collector.GetLatestIPMetrics()
	if metrics == nil {
		metrics = []sparkpost.IPMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// Domain endpoints

// GetDomainMetrics returns metrics for all sending domains
func (h *Handlers) GetDomainMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := h.collector.GetLatestDomainMetrics()
	if metrics == nil {
		metrics = []sparkpost.DomainMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// Signals endpoints

// GetSignals returns the latest signals data
func (h *Handlers) GetSignals(w http.ResponseWriter, r *http.Request) {
	signals := h.collector.GetLatestSignals()
	if signals == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No signals data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, signals)
}

// Agent endpoints

// GetAlerts returns current alerts from the agent
func (h *Handlers) GetAlerts(w http.ResponseWriter, r *http.Request) {
	alerts := h.agent.GetAlerts()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"count":     len(alerts),
		"alerts":    alerts,
	})
}

// AcknowledgeAlert acknowledges an alert
func (h *Handlers) AcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AlertID string `json:"alert_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if h.agent.AcknowledgeAlert(req.AlertID) {
		respondJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
	} else {
		respondError(w, http.StatusNotFound, "alert not found")
	}
}

// ClearAlerts clears all alerts
func (h *Handlers) ClearAlerts(w http.ResponseWriter, r *http.Request) {
	h.agent.ClearAlerts()
	respondJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// GetInsights returns insights from the agent
func (h *Handlers) GetInsights(w http.ResponseWriter, r *http.Request) {
	insights := h.agent.GetInsights()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"insights":  insights,
	})
}

// GetBaselines returns learned baselines
func (h *Handlers) GetBaselines(w http.ResponseWriter, r *http.Request) {
	baselines := h.agent.GetBaselines()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"count":     len(baselines),
		"baselines": baselines,
	})
}

// GetCorrelations returns learned correlations
func (h *Handlers) GetCorrelations(w http.ResponseWriter, r *http.Request) {
	correlations := h.agent.GetCorrelations()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":    time.Now(),
		"count":        len(correlations),
		"correlations": correlations,
	})
}

// Chat handles chat messages to the agent (unified across all ESPs)
func (h *Handlers) Chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string                     `json:"message"`
		History []agent.OpenAIChatMessage `json:"history,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Message == "" {
		respondError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Set collectors on agent for unified access
	collectors := agent.ESPCollectors{}
	if h.collector != nil {
		collectors.SparkPost = h.collector
	}
	if h.mailgunCollector != nil {
		collectors.Mailgun = h.mailgunCollector
	}
	if h.sesCollector != nil {
		collectors.SES = h.sesCollector
	}
	if h.ongageCollector != nil {
		collectors.Ongage = h.ongageCollector
	}
	if h.everflowCollector != nil {
		collectors.Everflow = h.everflowCollector
	}
	h.agent.SetCollectors(collectors)

	// Use OpenAI agent - no fallback, AI-only mode
	if h.openaiAgent != nil {
		message, suggestions, err := h.openaiAgent.Chat(r.Context(), req.Message, req.History)
		if err != nil {
			log.Printf("ERROR: AI chat error: %v", err)
			respondError(w, http.StatusInternalServerError, "AI service temporarily unavailable")
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"timestamp": time.Now(),
			"query":     req.Message,
			"response": map[string]interface{}{
				"message":     message,
				"suggestions": suggestions,
			},
			"ai_powered": true,
		})
		return
	}

	// No OpenAI agent configured - return error
	respondError(w, http.StatusServiceUnavailable, "AI agent not configured. Please add OpenAI API key to config.")
}

// Storage endpoints

// GetCacheStats returns cache statistics
func (h *Handlers) GetCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := h.storage.GetCacheStats()
	respondJSON(w, http.StatusOK, stats)
}

// System endpoints

// GetSystemStatus returns overall system status
func (h *Handlers) GetSystemStatus(w http.ResponseWriter, r *http.Request) {
	lastFetch := h.collector.GetLastFetchTime()
	
	status := map[string]interface{}{
		"timestamp":    time.Now(),
		"collector":    map[string]interface{}{
			"running":    h.collector.IsRunning(),
			"last_fetch": lastFetch,
		},
		"agent": map[string]interface{}{
			"alerts_count":       len(h.agent.GetAlerts()),
			"baselines_count":    len(h.agent.GetBaselines()),
			"correlations_count": len(h.agent.GetCorrelations()),
		},
		"storage": h.storage.GetCacheStats(),
	}

	respondJSON(w, http.StatusOK, status)
}

// TriggerFetch triggers an immediate metrics fetch
func (h *Handlers) TriggerFetch(w http.ResponseWriter, r *http.Request) {
	go h.collector.FetchNow(r.Context())
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "triggered",
		"message": "Metrics fetch initiated",
	})
}

// Dashboard endpoint - returns all data needed for dashboard in one call.
// Supports global date filter via query params: start_date, end_date, range_type.
// If date params are provided, makes fresh SparkPost API calls for that range.
// Otherwise falls back to the cached latest-24h data from the periodic poller.
func (h *Handlers) GetDashboard(w http.ResponseWriter, r *http.Request) {
	startDateStr := r.URL.Query().Get("start_date")
	endDateStr := r.URL.Query().Get("end_date")

	// If the frontend passes date range params, honour them
	if startDateStr != "" && endDateStr != "" {
		startDate, err1 := time.Parse("2006-01-02", startDateStr)
		endDate, err2 := time.Parse("2006-01-02", endDateStr)
		if err1 == nil && err2 == nil {
			// Set end-of-day for the end date so we include all of that day
			endDate = endDate.Add(24*time.Hour - time.Second)

			ctx := r.Context()
			dashData, err := h.collector.GetDashboardForDateRange(ctx, startDate, endDate)
			if err != nil {
				log.Printf("SparkPost date-range dashboard error: %v", err)
				// Fall through to cached data below
			} else {
				alerts := h.agent.GetAlerts()
				activeAlerts := 0
				for _, alert := range alerts {
					if !alert.Acknowledged {
						activeAlerts++
					}
				}

				respondJSON(w, http.StatusOK, map[string]interface{}{
					"timestamp":      time.Now(),
					"last_fetch":     time.Now(),
					"summary":        dashData.Summary,
					"isp_metrics":    dashData.ISPMetrics,
					"ip_metrics":     dashData.IPMetrics,
					"domain_metrics": dashData.DomainMetrics,
					"signals":        dashData.Signals,
					"alerts": map[string]interface{}{
						"active_count": activeAlerts,
						"total_count":  len(alerts),
						"items":        alerts,
					},
				})
				return
			}
		}
	}

	// Fallback: return cached data from the periodic poller (last 24h)
	summary := h.collector.GetLatestSummary()
	ispMetrics := h.collector.GetLatestISPMetrics()
	ipMetrics := h.collector.GetLatestIPMetrics()
	domainMetrics := h.collector.GetLatestDomainMetrics()
	signals := h.collector.GetLatestSignals()
	alerts := h.agent.GetAlerts()

	// Count active (unacknowledged) alerts
	activeAlerts := 0
	for _, alert := range alerts {
		if !alert.Acknowledged {
			activeAlerts++
		}
	}

	dashboard := map[string]interface{}{
		"timestamp":     time.Now(),
		"last_fetch":    h.collector.GetLastFetchTime(),
		"summary":       summary,
		"isp_metrics":   ispMetrics,
		"ip_metrics":    ipMetrics,
		"domain_metrics": domainMetrics,
		"signals":       signals,
		"alerts": map[string]interface{}{
			"active_count": activeAlerts,
			"total_count":  len(alerts),
			"items":        alerts,
		},
	}

	respondJSON(w, http.StatusOK, dashboard)
}

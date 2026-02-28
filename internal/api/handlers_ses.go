package api

import (
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/ses"
)

func (h *Handlers) GetSESSummary(w http.ResponseWriter, r *http.Request) {
	if h.sesCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "SES collector not configured")
		return
	}

	summary := h.sesCollector.GetLatestSummary()
	if summary == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No SES metrics data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, summary)
}

// GetSESISPMetrics returns SES metrics for all ISPs
func (h *Handlers) GetSESISPMetrics(w http.ResponseWriter, r *http.Request) {
	if h.sesCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "SES collector not configured")
		return
	}

	metrics := h.sesCollector.GetLatestISPMetrics()
	if metrics == nil {
		metrics = []ses.ISPMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// GetSESSignals returns the latest SES signals data
func (h *Handlers) GetSESSignals(w http.ResponseWriter, r *http.Request) {
	if h.sesCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "SES collector not configured")
		return
	}

	signals := h.sesCollector.GetLatestSignals()
	if signals == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No SES signals data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, signals)
}

// GetSESDashboard returns all SES data needed for dashboard in one call
func (h *Handlers) GetSESDashboard(w http.ResponseWriter, r *http.Request) {
	if h.sesCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "SES collector not configured")
		return
	}

	summary := h.sesCollector.GetLatestSummary()
	ispMetrics := h.sesCollector.GetLatestISPMetrics()
	signals := h.sesCollector.GetLatestSignals()

	dashboard := map[string]interface{}{
		"timestamp":   time.Now(),
		"last_fetch":  h.sesCollector.GetLastFetchTime(),
		"summary":     summary,
		"isp_metrics": ispMetrics,
		"signals":     signals,
	}

	respondJSON(w, http.StatusOK, dashboard)
}

// TriggerSESFetch triggers an immediate SES metrics fetch
func (h *Handlers) TriggerSESFetch(w http.ResponseWriter, r *http.Request) {
	if h.sesCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "SES collector not configured")
		return
	}

	go h.sesCollector.FetchNow(r.Context())
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "triggered",
		"message": "SES metrics fetch initiated",
	})
}

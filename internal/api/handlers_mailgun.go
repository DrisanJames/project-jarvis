package api

import (
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailgun"
)

func (h *Handlers) GetMailgunSummary(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	summary := h.mailgunCollector.GetLatestSummary()
	if summary == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No Mailgun metrics data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, summary)
}

// GetMailgunISPMetrics returns Mailgun metrics for all ISPs
func (h *Handlers) GetMailgunISPMetrics(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	metrics := h.mailgunCollector.GetLatestISPMetrics()
	if metrics == nil {
		metrics = []mailgun.ISPMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// GetMailgunDomainMetrics returns Mailgun metrics for all sending domains
func (h *Handlers) GetMailgunDomainMetrics(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	metrics := h.mailgunCollector.GetLatestDomainMetrics()
	if metrics == nil {
		metrics = []mailgun.DomainMetrics{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"data":      metrics,
	})
}

// GetMailgunSignals returns the latest Mailgun signals data
func (h *Handlers) GetMailgunSignals(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	signals := h.mailgunCollector.GetLatestSignals()
	if signals == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "no_data",
			"message": "No Mailgun signals data available yet",
		})
		return
	}
	respondJSON(w, http.StatusOK, signals)
}

// GetMailgunDashboard returns all Mailgun data needed for dashboard in one call
func (h *Handlers) GetMailgunDashboard(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	summary := h.mailgunCollector.GetLatestSummary()
	ispMetrics := h.mailgunCollector.GetLatestISPMetrics()
	domainMetrics := h.mailgunCollector.GetLatestDomainMetrics()
	signals := h.mailgunCollector.GetLatestSignals()

	dashboard := map[string]interface{}{
		"timestamp":      time.Now(),
		"last_fetch":     h.mailgunCollector.GetLastFetchTime(),
		"summary":        summary,
		"isp_metrics":    ispMetrics,
		"domain_metrics": domainMetrics,
		"signals":        signals,
	}

	respondJSON(w, http.StatusOK, dashboard)
}

// TriggerMailgunFetch triggers an immediate Mailgun metrics fetch
func (h *Handlers) TriggerMailgunFetch(w http.ResponseWriter, r *http.Request) {
	if h.mailgunCollector == nil {
		respondError(w, http.StatusServiceUnavailable, "Mailgun collector not configured")
		return
	}

	go h.mailgunCollector.FetchNow(r.Context())
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "triggered",
		"message": "Mailgun metrics fetch initiated",
	})
}

package api

import (
	"net/http"
	"time"
)

// DataInjectionsDashboard returns the complete data injections dashboard
func (h *Handlers) DataInjectionsDashboard(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	dashboard := h.dataInjectionsService.GetDashboard()
	if dashboard == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections data not available yet")
		return
	}

	respondJSON(w, http.StatusOK, dashboard)
}

// DataInjectionsIngestion returns the ingestion (Azure) summary
func (h *Handlers) DataInjectionsIngestion(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	summary := h.dataInjectionsService.GetIngestionSummary()
	if summary == nil {
		respondError(w, http.StatusServiceUnavailable, "Ingestion data not available")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"ingestion": summary,
	})
}

// DataInjectionsValidation returns the validation (Snowflake) summary
func (h *Handlers) DataInjectionsValidation(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	summary := h.dataInjectionsService.GetValidationSummary()
	if summary == nil {
		respondError(w, http.StatusServiceUnavailable, "Validation data not available")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":  time.Now(),
		"validation": summary,
	})
}

// DataInjectionsImports returns the import (Ongage) summary
func (h *Handlers) DataInjectionsImports(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	summary := h.dataInjectionsService.GetImportSummary()
	if summary == nil {
		respondError(w, http.StatusServiceUnavailable, "Import data not available")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"import":    summary,
	})
}

// DataInjectionsHealth returns the health status
func (h *Handlers) DataInjectionsHealth(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	status, issues := h.dataInjectionsService.GetHealthStatus()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp": time.Now(),
		"status":    status,
		"issues":    issues,
	})
}

// DataInjectionsRefresh triggers a refresh of all data injection metrics
func (h *Handlers) DataInjectionsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.dataInjectionsService == nil {
		respondError(w, http.StatusServiceUnavailable, "Data injections service not configured")
		return
	}

	// Trigger refresh
	go h.dataInjectionsService.FetchNow(r.Context())

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "Refresh triggered",
		"timestamp": time.Now(),
	})
}

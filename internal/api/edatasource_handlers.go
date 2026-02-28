package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/edatasource"
)

// EDataSourceHandlers contains handlers for eDataSource inbox monitoring API
type EDataSourceHandlers struct {
	client *edatasource.Client
}

// NewEDataSourceHandlers creates handlers with the eDataSource client
func NewEDataSourceHandlers(apiKey string, dryRun bool) *EDataSourceHandlers {
	return &EDataSourceHandlers{
		client: edatasource.NewClient(apiKey, dryRun),
	}
}

// GetInboxPlacement returns inbox placement data for a sending domain
// GET /api/mailing/edatasource/inbox-placement?domain=horoscopeinfo.com&days=7
func (h *EDataSourceHandlers) GetInboxPlacement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = "horoscopeinfo.com" // Default sending domain
	}

	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil {
			days = parsed
		}
	}

	result, err := h.client.GetInboxPlacement(ctx, domain, days)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// GetYahooInboxData returns Yahoo-specific inbox placement data
// GET /api/mailing/edatasource/yahoo?domain=horoscopeinfo.com
func (h *EDataSourceHandlers) GetYahooInboxData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = "horoscopeinfo.com"
	}

	result, err := h.client.GetYahooInboxData(ctx, domain)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// GetSenderReputation returns sender reputation data
// GET /api/mailing/edatasource/reputation?domain=horoscopeinfo.com
func (h *EDataSourceHandlers) GetSenderReputation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = "horoscopeinfo.com"
	}

	result, err := h.client.GetSenderReputation(ctx, domain)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// SearchCampaigns searches the eDataSource campaign database
// GET /api/mailing/edatasource/campaigns?q=sams+club&days=30
func (h *EDataSourceHandlers) SearchCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, `{"error":"q parameter is required"}`, http.StatusBadRequest)
		return
	}

	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if parsed, err := strconv.Atoi(d); err == nil {
			days = parsed
		}
	}

	results, err := h.client.SearchCampaigns(ctx, query, days)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
}

// GetActivationDashboard returns a comprehensive dashboard for data activation campaigns
// GET /api/mailing/edatasource/activation-dashboard?domain=horoscopeinfo.com
func (h *EDataSourceHandlers) GetActivationDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = "horoscopeinfo.com"
	}

	// Fetch all data in parallel for the dashboard
	placement, _ := h.client.GetInboxPlacement(ctx, domain, 7)
	yahooData, _ := h.client.GetYahooInboxData(ctx, domain)
	reputation, _ := h.client.GetSenderReputation(ctx, domain)

	// Build activation readiness assessment
	readiness := "not_ready"
	readinessScore := 0.0
	readinessIssues := []string{}

	if reputation != nil {
		readinessScore += reputation.ReputationScore * 0.4
		if reputation.ReputationScore < 60 {
			readinessIssues = append(readinessIssues, "Sender reputation is below 60 — consider warming up first")
		}
		if reputation.ComplaintRate > 0.08 {
			readinessIssues = append(readinessIssues, "Complaint rate exceeds 0.08% — Yahoo will throttle")
		}
		if reputation.BlacklistCount > 0 {
			readinessIssues = append(readinessIssues, "Domain is on one or more blacklists")
		}
	}

	if yahooData != nil {
		readinessScore += yahooData.InboxRate * 0.6
		if yahooData.InboxRate < 50 {
			readinessIssues = append(readinessIssues, "Yahoo inbox rate below 50% — high spam risk")
		}
		if yahooData.BulkFolderPct > 30 {
			readinessIssues = append(readinessIssues, "Yahoo bulk folder rate exceeds 30%")
		}
	}

	if readinessScore >= 70 {
		readiness = "ready"
	} else if readinessScore >= 50 {
		readiness = "caution"
	}

	dashboard := map[string]interface{}{
		"domain":            domain,
		"inbox_placement":   placement,
		"yahoo_data":        yahooData,
		"sender_reputation": reputation,
		"activation_readiness": map[string]interface{}{
			"status": readiness,
			"score":  readinessScore,
			"issues": readinessIssues,
		},
		"generated_at": r.Context().Value("request_time"),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dashboard)
}

// RegisterEDataSourceRoutes adds eDataSource routes to the router
func RegisterEDataSourceRoutes(r chi.Router, apiKey string, dryRun bool) {
	h := NewEDataSourceHandlers(apiKey, dryRun)

	r.Route("/edatasource", func(r chi.Router) {
		r.Get("/inbox-placement", h.GetInboxPlacement)
		r.Get("/yahoo", h.GetYahooInboxData)
		r.Get("/reputation", h.GetSenderReputation)
		r.Get("/campaigns", h.SearchCampaigns)
		r.Get("/activation-dashboard", h.GetActivationDashboard)
	})
}

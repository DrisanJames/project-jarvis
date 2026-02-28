package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/redis/go-redis/v9"
)

// AdvancedThrottleAPI handles advanced throttling API endpoints
type AdvancedThrottleAPI struct {
	db       *sql.DB
	throttle *worker.AdvancedThrottleManager
}

// NewAdvancedThrottleAPI creates a new advanced throttle API handler
func NewAdvancedThrottleAPI(db *sql.DB, redisClient *redis.Client) *AdvancedThrottleAPI {
	return &AdvancedThrottleAPI{
		db:       db,
		throttle: worker.NewAdvancedThrottleManager(redisClient, db),
	}
}

// RegisterRoutes registers throttle API routes
func (a *AdvancedThrottleAPI) RegisterRoutes(r chi.Router) {
	r.Route("/throttle", func(r chi.Router) {
		// Configuration
		r.Get("/config", a.HandleGetConfig)
		r.Put("/config", a.HandleSetConfig)

		// Statistics
		r.Get("/stats", a.HandleGetStats)
		r.Get("/stats/domains", a.HandleGetDomainStats)
		r.Get("/stats/isps", a.HandleGetISPStats)

		// Domain limits
		r.Post("/domain/{domain}/limit", a.HandleSetDomainLimit)
		r.Get("/domain/{domain}/stats", a.HandleGetSingleDomainStats)

		// ISP limits
		r.Post("/isp/{isp}/limit", a.HandleSetISPLimit)
		r.Get("/isp/{isp}/stats", a.HandleGetSingleISPStats)

		// Backpressure
		r.Post("/domain/{domain}/backpressure", a.HandleApplyBackpressure)
		r.Delete("/domain/{domain}/backpressure", a.HandleClearBackpressure)

		// Auto-adjust
		r.Post("/auto-adjust", a.HandleTriggerAutoAdjust)

		// Available ISPs
		r.Get("/isps", a.HandleGetISPList)
	})
}

// HandleGetConfig returns the current throttle configuration
func (a *AdvancedThrottleAPI) HandleGetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	config, err := a.throttle.GetThrottleConfig(ctx, orgID)
	if err != nil {
		throttleWriteError(w, "Failed to get config", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, config, http.StatusOK)
}

// HandleSetConfig updates the throttle configuration
func (a *AdvancedThrottleAPI) HandleSetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	var config worker.AdvancedThrottleConfig
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		throttleWriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	config.OrgID = orgID

	if err := a.throttle.SetThrottleConfig(ctx, config); err != nil {
		throttleWriteError(w, "Failed to save config", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
}

// HandleGetStats returns combined throttle statistics
func (a *AdvancedThrottleAPI) HandleGetStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	domainStats, err := a.throttle.GetDomainStats(ctx, orgID)
	if err != nil {
		domainStats = []worker.ThrottleStats{}
	}

	ispStats, err := a.throttle.GetISPStats(ctx, orgID)
	if err != nil {
		ispStats = []worker.ThrottleStats{}
	}

	config, _ := a.throttle.GetThrottleConfig(ctx, orgID)

	throttleWriteJSON(w, map[string]interface{}{
		"config":       config,
		"domain_stats": domainStats,
		"isp_stats":    ispStats,
	}, http.StatusOK)
}

// HandleGetDomainStats returns domain-level throttle statistics
func (a *AdvancedThrottleAPI) HandleGetDomainStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	stats, err := a.throttle.GetDomainStats(ctx, orgID)
	if err != nil {
		throttleWriteError(w, "Failed to get domain stats", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, stats, http.StatusOK)
}

// HandleGetISPStats returns ISP-level throttle statistics
func (a *AdvancedThrottleAPI) HandleGetISPStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	stats, err := a.throttle.GetISPStats(ctx, orgID)
	if err != nil {
		throttleWriteError(w, "Failed to get ISP stats", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, stats, http.StatusOK)
}

// SetDomainLimitRequest represents the request body for setting domain limits
type SetDomainLimitRequest struct {
	HourlyLimit int `json:"hourly_limit"`
	DailyLimit  int `json:"daily_limit"`
}

// HandleSetDomainLimit sets throttle limits for a specific domain
func (a *AdvancedThrottleAPI) HandleSetDomainLimit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	domain := chi.URLParam(r, "domain")

	if domain == "" {
		throttleWriteError(w, "Domain is required", http.StatusBadRequest)
		return
	}

	var req SetDomainLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		throttleWriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.HourlyLimit <= 0 || req.DailyLimit <= 0 {
		throttleWriteError(w, "Limits must be positive", http.StatusBadRequest)
		return
	}

	if err := a.throttle.SetDomainLimit(ctx, orgID, domain, req.HourlyLimit, req.DailyLimit); err != nil {
		throttleWriteError(w, "Failed to set domain limit", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]interface{}{
		"status":       "ok",
		"domain":       domain,
		"hourly_limit": req.HourlyLimit,
		"daily_limit":  req.DailyLimit,
	}, http.StatusOK)
}

// HandleGetSingleDomainStats returns stats for a single domain
func (a *AdvancedThrottleAPI) HandleGetSingleDomainStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	domain := chi.URLParam(r, "domain")

	if domain == "" {
		throttleWriteError(w, "Domain is required", http.StatusBadRequest)
		return
	}

	stats, err := a.throttle.GetDomainStats(ctx, orgID)
	if err != nil {
		throttleWriteError(w, "Failed to get stats", http.StatusInternalServerError)
		return
	}

	// Find the specific domain
	for _, stat := range stats {
		if stat.Domain == domain {
			throttleWriteJSON(w, stat, http.StatusOK)
			return
		}
	}

	// Return default stats if domain not found
	throttleWriteJSON(w, worker.ThrottleStats{
		Domain:       domain,
		SentLastHour: 0,
		SentLastDay:  0,
		HourlyLimit:  5000,
		DailyLimit:   50000,
		IsThrottled:  false,
	}, http.StatusOK)
}

// SetISPLimitRequest represents the request body for setting ISP limits
type SetISPLimitRequest struct {
	HourlyLimit int `json:"hourly_limit"`
	DailyLimit  int `json:"daily_limit"`
	BurstLimit  int `json:"burst_limit"`
}

// HandleSetISPLimit sets throttle limits for an ISP
func (a *AdvancedThrottleAPI) HandleSetISPLimit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	isp := chi.URLParam(r, "isp")

	if isp == "" {
		throttleWriteError(w, "ISP is required", http.StatusBadRequest)
		return
	}

	// Validate ISP name
	if _, ok := worker.ISPDomains[isp]; !ok {
		throttleWriteError(w, "Unknown ISP. Valid ISPs: gmail, yahoo, microsoft, aol, apple, comcast, att, verizon", http.StatusBadRequest)
		return
	}

	var req SetISPLimitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		throttleWriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.HourlyLimit <= 0 || req.DailyLimit <= 0 {
		throttleWriteError(w, "Limits must be positive", http.StatusBadRequest)
		return
	}

	if err := a.throttle.SetISPLimit(ctx, orgID, isp, req.HourlyLimit, req.DailyLimit, req.BurstLimit); err != nil {
		throttleWriteError(w, "Failed to set ISP limit", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]interface{}{
		"status":       "ok",
		"isp":          isp,
		"hourly_limit": req.HourlyLimit,
		"daily_limit":  req.DailyLimit,
		"burst_limit":  req.BurstLimit,
	}, http.StatusOK)
}

// HandleGetSingleISPStats returns stats for a single ISP
func (a *AdvancedThrottleAPI) HandleGetSingleISPStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	isp := chi.URLParam(r, "isp")

	if isp == "" {
		throttleWriteError(w, "ISP is required", http.StatusBadRequest)
		return
	}

	stats, err := a.throttle.GetISPStats(ctx, orgID)
	if err != nil {
		throttleWriteError(w, "Failed to get stats", http.StatusInternalServerError)
		return
	}

	// Find the specific ISP
	for _, stat := range stats {
		if stat.ISP == isp {
			throttleWriteJSON(w, stat, http.StatusOK)
			return
		}
	}

	// Return default stats if ISP not found
	defaultLimits := worker.DefaultISPLimits[isp]
	throttleWriteJSON(w, worker.ThrottleStats{
		ISP:          isp,
		SentLastHour: 0,
		SentLastDay:  0,
		HourlyLimit:  defaultLimits.HourlyLimit,
		DailyLimit:   defaultLimits.DailyLimit,
		IsThrottled:  false,
	}, http.StatusOK)
}

// BackpressureRequest represents the request body for applying backpressure
type BackpressureRequest struct {
	Seconds int    `json:"seconds"`
	Reason  string `json:"reason,omitempty"`
}

// HandleApplyBackpressure applies backpressure (temporary pause) to a domain
func (a *AdvancedThrottleAPI) HandleApplyBackpressure(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	domain := chi.URLParam(r, "domain")

	if domain == "" {
		throttleWriteError(w, "Domain is required", http.StatusBadRequest)
		return
	}

	var req BackpressureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Default to 1 hour if no body
		req.Seconds = 3600
	}

	if req.Seconds <= 0 {
		req.Seconds = 3600 // Default 1 hour
	}

	if req.Seconds > 86400*7 {
		throttleWriteError(w, "Backpressure cannot exceed 7 days", http.StatusBadRequest)
		return
	}

	if err := a.throttle.ApplyBackpressure(ctx, orgID, domain, req.Seconds); err != nil {
		throttleWriteError(w, "Failed to apply backpressure", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]interface{}{
		"status":  "ok",
		"domain":  domain,
		"seconds": req.Seconds,
	}, http.StatusOK)
}

// HandleClearBackpressure removes backpressure from a domain
func (a *AdvancedThrottleAPI) HandleClearBackpressure(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)
	domain := chi.URLParam(r, "domain")

	if domain == "" {
		throttleWriteError(w, "Domain is required", http.StatusBadRequest)
		return
	}

	if err := a.throttle.ClearBackpressure(ctx, orgID, domain); err != nil {
		throttleWriteError(w, "Failed to clear backpressure", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]interface{}{
		"status": "ok",
		"domain": domain,
	}, http.StatusOK)
}

// HandleTriggerAutoAdjust manually triggers auto-adjustment of throttle limits
func (a *AdvancedThrottleAPI) HandleTriggerAutoAdjust(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	if err := a.throttle.AutoAdjustThrottles(ctx, orgID); err != nil {
		throttleWriteError(w, "Failed to run auto-adjust", http.StatusInternalServerError)
		return
	}

	throttleWriteJSON(w, map[string]string{"status": "ok", "message": "Auto-adjustment completed"}, http.StatusOK)
}

// HandleGetISPList returns the list of known ISPs and their domains
func (a *AdvancedThrottleAPI) HandleGetISPList(w http.ResponseWriter, r *http.Request) {
	isps := make([]map[string]interface{}, 0)

	for isp, domains := range worker.ISPDomains {
		defaults := worker.DefaultISPLimits[isp]
		isps = append(isps, map[string]interface{}{
			"isp":                   isp,
			"domains":               domains,
			"default_hourly_limit":  defaults.HourlyLimit,
			"default_daily_limit":   defaults.DailyLimit,
			"default_burst_limit":   defaults.BurstLimit,
		})
	}

	throttleWriteJSON(w, isps, http.StatusOK)
}

// throttleGetOrgID gets org ID from request for throttle handlers
func throttleGetOrgID(r *http.Request) string {
	// Try to get from query parameter first
	orgID := r.URL.Query().Get("org_id")
	if orgID != "" {
		return orgID
	}

	// Try to get from header
	orgID = r.Header.Get("X-Org-ID")
	if orgID != "" {
		return orgID
	}

	// Default org ID for single-tenant deployments
	return "default"
}

// throttleWriteJSON writes JSON response for throttle handlers
func throttleWriteJSON(w http.ResponseWriter, data interface{}, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

// throttleWriteError writes JSON error response for throttle handlers
func throttleWriteError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// CanSendRequest represents a request to check if sending is allowed
type CanSendRequest struct {
	Email string `json:"email"`
}

// CanSendResponse represents the response for a send check
type CanSendResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
	Domain  string `json:"domain"`
	ISP     string `json:"isp,omitempty"`
}

// HandleCanSend checks if sending to an email is allowed
func (a *AdvancedThrottleAPI) HandleCanSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	var req CanSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		throttleWriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" {
		throttleWriteError(w, "Email is required", http.StatusBadRequest)
		return
	}

	allowed, reason, err := a.throttle.CanSend(ctx, orgID, req.Email)
	if err != nil {
		throttleWriteError(w, "Failed to check throttle", http.StatusInternalServerError)
		return
	}

	// Extract domain for response
	domain := ""
	if idx := len(req.Email) - 1; idx > 0 {
		for i := len(req.Email) - 1; i >= 0; i-- {
			if req.Email[i] == '@' {
				domain = req.Email[i+1:]
				break
			}
		}
	}

	isp := worker.DomainToISP[domain]

	throttleWriteJSON(w, CanSendResponse{
		Allowed: allowed,
		Reason:  reason,
		Domain:  domain,
		ISP:     isp,
	}, http.StatusOK)
}

// CanSendBatchRequest represents a batch send check request
type CanSendBatchRequest struct {
	Emails []string `json:"emails"`
}

// HandleCanSendBatch checks if sending to a batch of emails is allowed
func (a *AdvancedThrottleAPI) HandleCanSendBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := throttleGetOrgID(r)

	var req CanSendBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		throttleWriteError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Emails) == 0 {
		throttleWriteError(w, "Emails are required", http.StatusBadRequest)
		return
	}

	if len(req.Emails) > 10000 {
		throttleWriteError(w, "Maximum 10000 emails per batch", http.StatusBadRequest)
		return
	}

	decisions, err := a.throttle.CanSendBatch(ctx, orgID, req.Emails)
	if err != nil {
		throttleWriteError(w, "Failed to check throttle", http.StatusInternalServerError)
		return
	}

	// Summarize results
	allowed := 0
	denied := 0
	deniedReasons := make(map[string]int)

	for _, d := range decisions {
		if d.Allowed {
			allowed++
		} else {
			denied++
			deniedReasons[d.Reason]++
		}
	}

	throttleWriteJSON(w, map[string]interface{}{
		"total":          len(req.Emails),
		"allowed":        allowed,
		"denied":         denied,
		"denied_reasons": deniedReasons,
		"decisions":      decisions,
	}, http.StatusOK)
}

// GetThrottleManager returns the underlying throttle manager for integration
func (a *AdvancedThrottleAPI) GetThrottleManager() *worker.AdvancedThrottleManager {
	return a.throttle
}

// Utility function to parse integer from string with default
func parseIntWithDefault(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return val
}

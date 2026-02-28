package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// InboxPlacementHandlers contains handlers for inbox placement API endpoints
type InboxPlacementHandlers struct {
	service *mailing.InboxPlacementService
}

// NewInboxPlacementHandlers creates a new InboxPlacementHandlers instance
func NewInboxPlacementHandlers(db *sql.DB) *InboxPlacementHandlers {
	return &InboxPlacementHandlers{
		service: mailing.NewInboxPlacementService(db),
	}
}

// RunSeedTest initiates an inbox placement seed test
// POST /api/mailing/inbox/seed-test
func (h *InboxPlacementHandlers) RunSeedTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		CampaignID string `json:"campaign_id"`
		SeedListID string `json:"seed_list_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if input.CampaignID == "" || input.SeedListID == "" {
		http.Error(w, `{"error":"campaign_id and seed_list_id are required"}`, http.StatusBadRequest)
		return
	}

	result, err := h.service.RunSeedTest(ctx, input.CampaignID, input.SeedListID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(result)
}

// GetTestResults retrieves inbox placement test results for a campaign
// GET /api/mailing/inbox/test-results/{campaign_id}
func (h *InboxPlacementHandlers) GetTestResults(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	campaignID := chi.URLParam(r, "campaign_id")
	if campaignID == "" {
		http.Error(w, `{"error":"campaign_id is required"}`, http.StatusBadRequest)
		return
	}

	results, err := h.service.GetTestResults(ctx, campaignID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if results == nil {
		results = []mailing.InboxTestResult{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results": results,
		"total":   len(results),
	})
}

// GetReputationScore retrieves the sending reputation for an organization
// GET /api/mailing/inbox/reputation
func (h *InboxPlacementHandlers) GetReputationScore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	score, err := h.service.GetReputationScore(ctx, orgID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(score)
}

// CheckBlacklists checks if an IP or domain is on any blacklists
// GET /api/mailing/inbox/blacklist-check?target={ip_or_domain}
func (h *InboxPlacementHandlers) CheckBlacklists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, `{"error":"target parameter is required (IP address or domain)"}`, http.StatusBadRequest)
		return
	}

	results, err := h.service.CheckBlacklists(ctx, target)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Count listings
	listedCount := 0
	for _, bl := range results {
		if bl.Listed {
			listedCount++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"target":       target,
		"results":      results,
		"total_checks": len(results),
		"listed_count": listedCount,
		"is_clean":     listedCount == 0,
	})
}

// GetISPRecommendations provides deliverability recommendations
// GET /api/mailing/inbox/isp-recommendations
func (h *InboxPlacementHandlers) GetISPRecommendations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	recommendations, err := h.service.GetISPRecommendations(ctx, orgID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if recommendations == nil {
		recommendations = []mailing.ISPRecommendation{}
	}

	// Separate by priority
	highPriority := []mailing.ISPRecommendation{}
	mediumPriority := []mailing.ISPRecommendation{}
	lowPriority := []mailing.ISPRecommendation{}

	for _, rec := range recommendations {
		switch rec.Priority {
		case "high":
			highPriority = append(highPriority, rec)
		case "medium":
			mediumPriority = append(mediumPriority, rec)
		default:
			lowPriority = append(lowPriority, rec)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"recommendations":   recommendations,
		"high_priority":     highPriority,
		"medium_priority":   mediumPriority,
		"low_priority":      lowPriority,
		"total_issues":      len(recommendations),
		"high_priority_count": len(highPriority),
	})
}

// CreateWarmupPlan creates a new IP warmup plan
// POST /api/mailing/inbox/warmup-plans
func (h *InboxPlacementHandlers) CreateWarmupPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		IPAddress    string `json:"ip_address"`
		TargetVolume int    `json:"target_volume"`
		PlanType     string `json:"plan_type"` // conservative, aggressive, custom
		OrgID        string `json:"org_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if input.IPAddress == "" {
		http.Error(w, `{"error":"ip_address is required"}`, http.StatusBadRequest)
		return
	}

	if input.TargetVolume <= 0 {
		input.TargetVolume = 100000 // Default target
	}

	if input.PlanType == "" {
		input.PlanType = "conservative"
	}

	if input.OrgID == "" {
		var err error
		input.OrgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	plan, err := h.service.CreateWarmupPlan(ctx, input.OrgID, input.IPAddress, input.TargetVolume, input.PlanType)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(plan)
}

// GetWarmupPlan retrieves warmup plan progress
// GET /api/mailing/inbox/warmup-plans/{id}
func (h *InboxPlacementHandlers) GetWarmupPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	planID := chi.URLParam(r, "id")
	if planID == "" {
		http.Error(w, `{"error":"plan id is required"}`, http.StatusBadRequest)
		return
	}

	plan, err := h.service.GetWarmupProgress(ctx, planID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Calculate progress stats
	completedDays := 0
	totalActualVolume := 0
	totalTargetVolume := 0
	for _, day := range plan.DailySchedule {
		if day.Completed {
			completedDays++
		}
		totalActualVolume += day.ActualVolume
		totalTargetVolume += day.TargetVolume
	}

	progressPct := 0.0
	if totalTargetVolume > 0 {
		progressPct = float64(totalActualVolume) / float64(totalTargetVolume) * 100
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"plan":              plan,
		"completed_days":    completedDays,
		"progress_percent":  progressPct,
		"total_volume_sent": totalActualVolume,
		"today_target":      plan.DailySchedule[plan.CurrentDay-1].TargetVolume,
		"today_sent":        plan.DailySchedule[plan.CurrentDay-1].ActualVolume,
	})
}

// GetWarmupPlans retrieves all warmup plans for an organization
// GET /api/mailing/inbox/warmup-plans
func (h *InboxPlacementHandlers) GetWarmupPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	plans, err := h.service.GetWarmupPlans(ctx, orgID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if plans == nil {
		plans = []mailing.IPWarmupPlan{}
	}

	// Separate by status
	activePlans := []mailing.IPWarmupPlan{}
	completedPlans := []mailing.IPWarmupPlan{}
	pausedPlans := []mailing.IPWarmupPlan{}

	for _, p := range plans {
		switch p.Status {
		case "active":
			activePlans = append(activePlans, p)
		case "completed":
			completedPlans = append(completedPlans, p)
		case "paused":
			pausedPlans = append(pausedPlans, p)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"plans":          plans,
		"active":         activePlans,
		"completed":      completedPlans,
		"paused":         pausedPlans,
		"total":          len(plans),
		"active_count":   len(activePlans),
	})
}

// UpdateWarmupProgress updates warmup plan with sending data
// PUT /api/mailing/inbox/warmup-plans/{id}/progress
func (h *InboxPlacementHandlers) UpdateWarmupProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	planID := chi.URLParam(r, "id")
	if planID == "" {
		http.Error(w, `{"error":"plan id is required"}`, http.StatusBadRequest)
		return
	}

	var input struct {
		Sent       int `json:"sent"`
		Bounced    int `json:"bounced"`
		Complaints int `json:"complaints"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	err := h.service.UpdateWarmupProgress(ctx, planID, input.Sent, input.Bounced, input.Complaints)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Return updated plan
	plan, _ := h.service.GetWarmupProgress(ctx, planID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"plan":    plan,
	})
}

// CreateSeedList creates a new seed list
// POST /api/mailing/inbox/seed-lists
func (h *InboxPlacementHandlers) CreateSeedList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		Name     string         `json:"name"`
		Seeds    []mailing.Seed `json:"seeds"`
		Provider string         `json:"provider"`
		OrgID    string         `json:"org_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	if len(input.Seeds) == 0 {
		http.Error(w, `{"error":"at least one seed email is required"}`, http.StatusBadRequest)
		return
	}

	if input.OrgID == "" {
		var err error
		input.OrgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	seedList := mailing.SeedList{
		OrgID:    input.OrgID,
		Name:     input.Name,
		Seeds:    input.Seeds,
		Provider: input.Provider,
	}

	result, err := h.service.CreateSeedList(ctx, seedList)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(result)
}

// GetSeedLists retrieves seed lists for an organization
// GET /api/mailing/inbox/seed-lists
func (h *InboxPlacementHandlers) GetSeedLists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	lists, err := h.service.GetSeedLists(ctx, orgID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if lists == nil {
		lists = []mailing.SeedList{}
	}

	// Count seeds by ISP across all lists
	ispCounts := make(map[string]int)
	totalSeeds := 0
	for _, list := range lists {
		for _, seed := range list.Seeds {
			ispCounts[seed.ISP]++
			totalSeeds++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"seed_lists":      lists,
		"total":           len(lists),
		"total_seeds":     totalSeeds,
		"seeds_by_isp":    ispCounts,
	})
}

// GetInboxPlacementDashboard returns a comprehensive dashboard view
// GET /api/mailing/inbox/dashboard
func (h *InboxPlacementHandlers) GetInboxPlacementDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
			return
		}
	}

	// Get reputation score
	reputation, _ := h.service.GetReputationScore(ctx, orgID)

	// Get seed lists
	seedLists, _ := h.service.GetSeedLists(ctx, orgID)

	// Get warmup plans
	warmupPlans, _ := h.service.GetWarmupPlans(ctx, orgID)

	// Get recommendations
	recommendations, _ := h.service.GetISPRecommendations(ctx, orgID)

	// Count active warmups
	activeWarmups := 0
	for _, p := range warmupPlans {
		if p.Status == "active" {
			activeWarmups++
		}
	}

	// Count high priority issues
	highPriorityIssues := 0
	for _, r := range recommendations {
		if r.Priority == "high" {
			highPriorityIssues++
		}
	}

	// Build dashboard response
	dashboard := map[string]interface{}{
		"reputation": reputation,
		"overview": map[string]interface{}{
			"seed_lists_count":      len(seedLists),
			"active_warmups":        activeWarmups,
			"high_priority_issues":  highPriorityIssues,
			"total_recommendations": len(recommendations),
		},
		"warmup_plans":    warmupPlans,
		"recommendations": recommendations,
		"seed_lists":      seedLists,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dashboard)
}

// RegisterInboxPlacementRoutes adds inbox placement routes to the router
func RegisterInboxPlacementRoutes(r chi.Router, db *sql.DB) {
	h := NewInboxPlacementHandlers(db)

	r.Route("/inbox", func(r chi.Router) {
		// Dashboard
		r.Get("/dashboard", h.GetInboxPlacementDashboard)

		// Seed testing
		r.Post("/seed-test", h.RunSeedTest)
		r.Get("/test-results/{campaign_id}", h.GetTestResults)

		// Reputation and blacklists
		r.Get("/reputation", h.GetReputationScore)
		r.Get("/blacklist-check", h.CheckBlacklists)

		// ISP recommendations
		r.Get("/isp-recommendations", h.GetISPRecommendations)

		// Warmup plans
		r.Post("/warmup-plans", h.CreateWarmupPlan)
		r.Get("/warmup-plans", h.GetWarmupPlans)
		r.Get("/warmup-plans/{id}", h.GetWarmupPlan)
		r.Put("/warmup-plans/{id}/progress", h.UpdateWarmupProgress)

		// Seed lists
		r.Post("/seed-lists", h.CreateSeedList)
		r.Get("/seed-lists", h.GetSeedLists)
	})
}

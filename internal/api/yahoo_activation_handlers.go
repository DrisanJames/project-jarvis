package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/activation"
)

// YahooActivationHandlers manages Yahoo data activation campaigns
type YahooActivationHandlers struct {
	agent *activation.YahooActivationAgent
}

// NewYahooActivationHandlers creates handlers with a default dry-run agent
func NewYahooActivationHandlers() *YahooActivationHandlers {
	// Default config for the Sam's Club / horoscopeinfo.com campaign
	config := activation.YahooAgentConfig{
		SendingDomain:  "horoscopeinfo.com",
		ESP:            "sparkpost",
		TotalRecords:   182000,
		TargetOpenRate: 5.0,
		MaxComplaintRate: 0.08,
		SubjectLines: []string{
			"A $20 Sam's Club Membership?",
			"Get $30 Sam's Cash with $50 Membership",
			"$50 Membership, $30 Sam's Cash Back",
			"It's Like a $20 Sam's Club Membership",
			"Join Sam's Club, Get $30 Sam's Cash",
			"$30 Sam's Cash Makes Membership a Steal",
			"Membership Pays for Itself",
			"$50 In, $30 Back—Do the Math",
			"Sam's Club Membership for Less",
			"Big Value: $30 Sam's Cash with Membership",
			"$50 In + $30 Back = $20 Membership",
		},
		FromNames: []string{
			"Sam's Club Affiliate",
			"Sam's Club Affiliate Offer",
			"Sam's Club Partner",
			"Sam's Club Partner Offer",
			"Sam's Club Authorized Affiliate",
			"Sam's Club Special Offer Partner",
			"Sam's Club Rewards Partner",
			"Sam's Club Promotional Partner",
			"Sam's Club Trusted Partner",
			"Sam's Club Affiliate Deal",
			"Sam's Club Partner Savings",
		},
		DryRun: true,
	}

	agent := activation.NewYahooActivationAgent(config)

	// Generate demo audience profiles (in production, loaded from list)
	demoEmails := generateDemoYahooEmails(182000)
	demoEngagement := generateDemoEngagementData(demoEmails)
	agent.ProfileAudience(demoEmails, demoEngagement)

	return &YahooActivationHandlers{agent: agent}
}

// GetActivationSnapshot returns the complete agent state for the dashboard
// GET /api/mailing/yahoo-activation/snapshot
func (h *YahooActivationHandlers) GetActivationSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot := h.agent.GetActivationSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

// GetAudienceBreakdown returns engagement tier distribution
// GET /api/mailing/yahoo-activation/audience
func (h *YahooActivationHandlers) GetAudienceBreakdown(w http.ResponseWriter, r *http.Request) {
	breakdown := h.agent.GetAudienceBreakdown()
	total := 0
	for _, v := range breakdown {
		total += v
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"breakdown":    breakdown,
		"total":        total,
		"domain":       h.agent.Config.SendingDomain,
		"esp":          h.agent.Config.ESP,
	})
}

// GetABTestPlan returns the current A/B test plan
// GET /api/mailing/yahoo-activation/ab-plan
func (h *YahooActivationHandlers) GetABTestPlan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.agent.ABTestPlan)
}

// GetWarmupSchedule returns the warmup tier schedule
// GET /api/mailing/yahoo-activation/warmup
func (h *YahooActivationHandlers) GetWarmupSchedule(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tiers":           h.agent.WarmupSchedule,
		"total_records":   h.agent.Config.TotalRecords,
		"sending_domain":  h.agent.Config.SendingDomain,
	})
}

// SimulateSignals processes simulated signals for dry-run testing
// POST /api/mailing/yahoo-activation/simulate-signals
func (h *YahooActivationHandlers) SimulateSignals(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Sent       int     `json:"sent"`
		Opens      int     `json:"opens"`
		Clicks     int     `json:"clicks"`
		Bounces    int     `json:"bounces"`
		Complaints int     `json:"complaints"`
		InboxRate  float64 `json:"inbox_rate"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	alerts := h.agent.EvaluateSignals(
		input.Sent, input.Opens, input.Clicks,
		input.Bounces, input.Complaints, input.InboxRate,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"alerts":        alerts,
		"current_state": h.agent.CurrentState,
	})
}

// GetAgentState returns just the current agent state
// GET /api/mailing/yahoo-activation/state
func (h *YahooActivationHandlers) GetAgentState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.agent.CurrentState)
}

// RegisterYahooActivationRoutes adds Yahoo activation routes
func RegisterYahooActivationRoutes(r chi.Router) {
	h := NewYahooActivationHandlers()

	r.Route("/yahoo-activation", func(r chi.Router) {
		r.Get("/snapshot", h.GetActivationSnapshot)
		r.Get("/audience", h.GetAudienceBreakdown)
		r.Get("/ab-plan", h.GetABTestPlan)
		r.Get("/warmup", h.GetWarmupSchedule)
		r.Get("/state", h.GetAgentState)
		r.Post("/simulate-signals", h.SimulateSignals)
	})
}

// ============================================================================
// DEMO DATA GENERATORS (for dry-run mode)
// ============================================================================

func generateDemoYahooEmails(count int) []string {
	emails := make([]string, count)
	domains := []string{"yahoo.com", "ymail.com", "aol.com", "att.net", "sbcglobal.net"}
	for i := 0; i < count; i++ {
		domain := domains[i%len(domains)]
		emails[i] = "user" + string(rune('a'+i%26)) + "@" + domain
	}
	return emails
}

func generateDemoEngagementData(emails []string) map[string]activation.EngagementData {
	data := make(map[string]activation.EngagementData, len(emails))
	now := time.Now()

	for i, email := range emails {
		d := activation.EngagementData{}

		// ~15% are hot (recently opened)
		if i%7 == 0 {
			openDate := now.AddDate(0, 0, -3)
			clickDate := now.AddDate(0, 0, -5)
			d.TotalSends = 20
			d.TotalOpens = 12
			d.TotalClicks = 4
			d.LastOpenDate = &openDate
			d.LastClickDate = &clickDate
		} else if i%4 == 0 {
			// ~25% are warm
			openDate := now.AddDate(0, 0, -21)
			d.TotalSends = 15
			d.TotalOpens = 5
			d.TotalClicks = 1
			d.LastOpenDate = &openDate
		} else if i%3 == 0 {
			// ~33% are cold
			d.TotalSends = 10
			d.TotalOpens = 1
		} else {
			// Rest are unknown/new
			d.TotalSends = 0
		}

		// Small % with complaints/bounces
		if i%200 == 0 {
			d.BounceCount = 1
		}
		if i%500 == 0 {
			d.ComplaintCount = 1
		}

		data[email] = d
	}

	return data
}

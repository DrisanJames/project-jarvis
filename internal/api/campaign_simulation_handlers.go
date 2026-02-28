package api

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/activation"
)

// CampaignSimulationHandlers manages the campaign simulation for dry-run
type CampaignSimulationHandlers struct {
	mu        sync.RWMutex
	simulator *activation.CampaignSimulator
}

// NewCampaignSimulationHandlers creates handlers with a default simulator
func NewCampaignSimulationHandlers() *CampaignSimulationHandlers {
	// Reuse the Yahoo activation agent config
	config := activation.YahooAgentConfig{
		SendingDomain:    "horoscopeinfo.com",
		ESP:              "sparkpost",
		TotalRecords:     182000,
		TargetOpenRate:   5.0,
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
	simulator := activation.NewCampaignSimulator(agent)

	return &CampaignSimulationHandlers{
		simulator: simulator,
	}
}

// GetSnapshot returns the full simulation state for the UI
// GET /api/mailing/simulation/snapshot
func (h *CampaignSimulationHandlers) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot := h.simulator.GetSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

// StartSimulation begins the simulated campaign send
// POST /api/mailing/simulation/start
func (h *CampaignSimulationHandlers) StartSimulation(w http.ResponseWriter, r *http.Request) {
	h.simulator.Start()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "started",
		"message": "Campaign simulation started — poll /snapshot for updates",
	})
}

// StopSimulation stops the simulation
// POST /api/mailing/simulation/stop
func (h *CampaignSimulationHandlers) StopSimulation(w http.ResponseWriter, r *http.Request) {
	h.simulator.Stop()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "stopped",
		"message": "Campaign simulation stopped",
	})
}

// ResetSimulation resets the simulator to fresh state
// POST /api/mailing/simulation/reset
func (h *CampaignSimulationHandlers) ResetSimulation(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.simulator.Stop()

	// Recreate
	config := h.simulator.Agent.Config
	agent := activation.NewYahooActivationAgent(config)
	h.simulator = activation.NewCampaignSimulator(agent)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "reset",
		"message": "Simulation reset to fresh state",
	})
}

// SendConsultation sends human feedback to the agent
// POST /api/mailing/simulation/consult
func (h *CampaignSimulationHandlers) SendConsultation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message string `json:"message"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	h.simulator.AddConsultation(input.Message)
	snapshot := h.simulator.GetSnapshot()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "received",
		"consultations": snapshot.Consultations,
	})
}

// GetABStats returns A/B variant statistics
// GET /api/mailing/simulation/ab-stats
func (h *CampaignSimulationHandlers) GetABStats(w http.ResponseWriter, r *http.Request) {
	snapshot := h.simulator.GetSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"variants": snapshot.ABStats,
		"funnel":   snapshot.Funnel,
	})
}

// GetDecisions returns agent decision log
// GET /api/mailing/simulation/decisions
func (h *CampaignSimulationHandlers) GetDecisions(w http.ResponseWriter, r *http.Request) {
	snapshot := h.simulator.GetSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"decisions": snapshot.Decisions,
		"total":     len(snapshot.Decisions),
	})
}

// GetRecentEvents returns the most recent events
// GET /api/mailing/simulation/events
func (h *CampaignSimulationHandlers) GetRecentEvents(w http.ResponseWriter, r *http.Request) {
	snapshot := h.simulator.GetSnapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"events":      snapshot.RecentEvents,
		"total_events": len(snapshot.Events),
	})
}

// RegisterCampaignSimulationRoutes registers simulation API routes
func RegisterCampaignSimulationRoutes(r chi.Router) {
	h := NewCampaignSimulationHandlers()

	r.Route("/simulation", func(r chi.Router) {
		r.Get("/snapshot", h.GetSnapshot)
		r.Post("/start", h.StartSimulation)
		r.Post("/stop", h.StopSimulation)
		r.Post("/reset", h.ResetSimulation)
		r.Post("/consult", h.SendConsultation)
		r.Get("/ab-stats", h.GetABStats)
		r.Get("/decisions", h.GetDecisions)
		r.Get("/events", h.GetRecentEvents)
	})
}

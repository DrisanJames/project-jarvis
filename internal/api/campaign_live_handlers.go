package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// =============================================================================
// LIVE CAMPAIGN MONITOR
// =============================================================================
// Serves real-time campaign metrics from the database in the same format
// that Mission Control expects (compatible with SimulatorSnapshot shape).
// This allows Mission Control to switch between simulation and live data
// by toggling the endpoint it polls.

// LiveCampaignHandlers serves real-time campaign data for Mission Control
type LiveCampaignHandlers struct {
	db *sql.DB
}

// NewLiveCampaignHandlers creates handlers with a database connection
func NewLiveCampaignHandlers(db *sql.DB) *LiveCampaignHandlers {
	return &LiveCampaignHandlers{db: db}
}

// LiveSnapshot mirrors the SimulatorSnapshot structure for Mission Control compatibility
type LiveSnapshot struct {
	IsRunning     bool                       `json:"is_running"`
	IsLive        bool                       `json:"is_live"` // true = real campaign data
	CampaignID    string                     `json:"campaign_id"`
	CampaignName  string                     `json:"campaign_name"`
	StartedAt     *time.Time                 `json:"started_at"`
	Events        []LiveEvent                `json:"events"`
	RecentEvents  []LiveEvent                `json:"recent_events"`
	Decisions     []LiveDecision             `json:"decisions"`
	Consultations []interface{}              `json:"consultations"`
	ABStats       map[string]*LiveABStats    `json:"ab_stats"`
	Funnel        LiveFunnel                 `json:"funnel"`
	AgentState    LiveAgentState             `json:"agent_state"`
	WarmupTiers   []interface{}              `json:"warmup_tiers"`
	Config        map[string]interface{}     `json:"config"`
}

type LiveFunnel struct {
	TotalSent      int     `json:"total_sent"`
	TotalDelivered int     `json:"total_delivered"`
	TotalOpened    int     `json:"total_opened"`
	TotalClicked   int     `json:"total_clicked"`
	TotalConverted int     `json:"total_converted"`
	TotalRevenue   float64 `json:"total_revenue"`
	TotalBounced   int     `json:"total_bounced"`
	TotalComplaints int    `json:"total_complaints"`
	TotalSkipped   int     `json:"total_skipped"`
	DeliveryRate   float64 `json:"delivery_rate"`
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	ConversionRate float64 `json:"conversion_rate"`
	ClickToConvert float64 `json:"click_to_convert"`
}

type LiveEvent struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Email     string    `json:"email"`
	Details   string    `json:"details,omitempty"`
}

type LiveDecision struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"`
	Reasoning string    `json:"reasoning"`
	Action    string    `json:"action"`
	Impact    string    `json:"impact,omitempty"`
}

type LiveABStats struct {
	VariantID   string  `json:"variant_id"`
	Subject     string  `json:"subject"`
	FromName    string  `json:"from_name"`
	Sent        int     `json:"sent"`
	Opens       int     `json:"opens"`
	Clicks      int     `json:"clicks"`
	OpenRate    float64 `json:"open_rate"`
	ClickRate   float64 `json:"click_rate"`
	IsWinner    bool    `json:"is_winner"`
	IsEliminated bool   `json:"is_eliminated"`
}

type LiveAgentState struct {
	Phase          string  `json:"phase"`
	ThrottleRate   int     `json:"throttle_rate"`
	CurrentTier    int     `json:"current_tier"`
	OpenRate       float64 `json:"open_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	HealthStatus   string  `json:"health_status"` // healthy, warning, critical
}

// GetLiveSnapshot returns real-time campaign metrics for Mission Control
// GET /api/mailing/campaigns/{id}/live
func (h *LiveCampaignHandlers) GetLiveSnapshot(w http.ResponseWriter, r *http.Request) {
	campaignID := chi.URLParam(r, "id")
	ctx := r.Context()

	// Load campaign stats
	var (
		name        string
		status      string
		subject     string
		totalRecip  int
		sentCount   int
		delivered   int
		openCount   int
		clickCount  int
		bounceCount int
		complaintCount int
		revenue     float64
		startedAt   sql.NullTime
	)

	err := h.db.QueryRowContext(ctx, `
		SELECT 
			name, status, COALESCE(subject, ''),
			COALESCE(total_recipients, 0), COALESCE(sent_count, 0),
			COALESCE(delivered_count, 0), COALESCE(open_count, 0),
			COALESCE(click_count, 0), COALESCE(bounce_count, 0),
			COALESCE(complaint_count, 0), COALESCE(revenue, 0),
			started_at
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(
		&name, &status, &subject,
		&totalRecip, &sentCount, &delivered, &openCount,
		&clickCount, &bounceCount, &complaintCount, &revenue,
		&startedAt,
	)

	if err != nil {
		http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
		return
	}

	// Calculate rates
	funnel := LiveFunnel{
		TotalSent:       sentCount,
		TotalDelivered:  delivered,
		TotalOpened:     openCount,
		TotalClicked:    clickCount,
		TotalBounced:    bounceCount,
		TotalComplaints: complaintCount,
		TotalRevenue:    revenue,
	}

	if sentCount > 0 {
		funnel.DeliveryRate = float64(delivered) / float64(sentCount) * 100
		funnel.OpenRate = float64(openCount) / float64(sentCount) * 100
		funnel.ClickRate = float64(clickCount) / float64(sentCount) * 100
		funnel.BounceRate = float64(bounceCount) / float64(sentCount) * 100
		funnel.ComplaintRate = float64(complaintCount) / float64(sentCount) * 100
	}
	if openCount > 0 {
		funnel.ClickToConvert = float64(clickCount) / float64(openCount) * 100
	}

	// Get skipped (suppressed) count from queue
	var skipped int
	h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_campaign_queue 
		WHERE campaign_id = $1 AND status = 'skipped'
	`, campaignID).Scan(&skipped)
	funnel.TotalSkipped = skipped

	// Load recent tracking events for the live event feed
	var recentEvents []LiveEvent
	eventRows, err := h.db.QueryContext(ctx, `
		SELECT id::text, event_type, COALESCE(email, ''), event_at
		FROM mailing_tracking_events
		WHERE campaign_id = $1
		ORDER BY event_at DESC
		LIMIT 50
	`, campaignID)
	if err == nil {
		defer eventRows.Close()
		for eventRows.Next() {
			var e LiveEvent
			if err := eventRows.Scan(&e.ID, &e.Type, &e.Email, &e.Time); err == nil {
				// Mask email for display
				if len(e.Email) > 3 {
					e.Email = e.Email[:3] + "***"
				}
				recentEvents = append(recentEvents, e)
			}
		}
	}
	if recentEvents == nil {
		recentEvents = []LiveEvent{}
	}

	// Load A/B test stats if this is an A/B campaign
	abStats := make(map[string]*LiveABStats)
	abRows, err := h.db.QueryContext(ctx, `
		SELECT v.id::text, v.variant_name, COALESCE(v.subject, ''), COALESCE(v.from_name, ''),
		       COALESCE(v.sent_count, 0), COALESCE(v.open_count, 0), COALESCE(v.click_count, 0),
		       COALESCE(v.is_winner, false)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		ORDER BY v.variant_name
	`, campaignID)
	if err == nil {
		defer abRows.Close()
		for abRows.Next() {
			var s LiveABStats
			if err := abRows.Scan(&s.VariantID, &s.Subject, &s.Subject, &s.FromName,
				&s.Sent, &s.Opens, &s.Clicks, &s.IsWinner); err == nil {
				if s.Sent > 0 {
					s.OpenRate = float64(s.Opens) / float64(s.Sent) * 100
					s.ClickRate = float64(s.Clicks) / float64(s.Sent) * 100
				}
				abStats[s.VariantID] = &s
			}
		}
	}

	// Determine health status for agent state
	healthStatus := "healthy"
	if funnel.ComplaintRate >= 0.08 {
		healthStatus = "critical"
	} else if funnel.BounceRate >= 3.0 || funnel.ComplaintRate >= 0.05 {
		healthStatus = "warning"
	}

	// Determine phase
	phase := "idle"
	switch status {
	case "scheduled", "preparing":
		phase = "preparing"
	case "sending":
		if sentCount < totalRecip/10 {
			phase = "seed_testing"
		} else if sentCount < totalRecip/2 {
			phase = "ramping"
		} else {
			phase = "full_send"
		}
	case "completed", "completed_with_errors":
		phase = "completed"
	case "paused":
		phase = "paused"
	case "cancelled", "failed":
		phase = "stopped"
	}

	// Build snapshot
	var startedAtPtr *time.Time
	if startedAt.Valid {
		startedAtPtr = &startedAt.Time
	}

	snapshot := LiveSnapshot{
		IsRunning:     status == "sending",
		IsLive:        true,
		CampaignID:    campaignID,
		CampaignName:  name,
		StartedAt:     startedAtPtr,
		Events:        recentEvents,
		RecentEvents:  recentEvents,
		Decisions:     []LiveDecision{},
		Consultations: []interface{}{},
		ABStats:       abStats,
		Funnel:        funnel,
		AgentState: LiveAgentState{
			Phase:         phase,
			OpenRate:      funnel.OpenRate,
			ComplaintRate: funnel.ComplaintRate,
			BounceRate:    funnel.BounceRate,
			HealthStatus:  healthStatus,
		},
		WarmupTiers: []interface{}{},
		Config: map[string]interface{}{
			"campaign_id":     campaignID,
			"campaign_name":   name,
			"status":          status,
			"subject":         subject,
			"total_recipients": totalRecip,
			"target_open_rate": 5.0,
			"max_complaint_rate": 0.08,
		},
	}

	// Load AI agent decisions from the campaign_agent_decisions table (if it exists)
	decisionRows, err := h.db.QueryContext(ctx, `
		SELECT id::text, decision_type, reasoning, action_taken, impact, decided_at
		FROM mailing_campaign_agent_decisions
		WHERE campaign_id = $1
		ORDER BY decided_at DESC
		LIMIT 20
	`, campaignID)
	if err == nil {
		defer decisionRows.Close()
		for decisionRows.Next() {
			var d LiveDecision
			var impact sql.NullString
			if err := decisionRows.Scan(&d.ID, &d.Type, &d.Reasoning, &d.Action, &impact, &d.Time); err == nil {
				if impact.Valid {
					d.Impact = impact.String
				}
				snapshot.Decisions = append(snapshot.Decisions, d)
			}
		}
	}
	// Decisions table may not exist yet — that's fine, we still return empty array

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

// GetActiveCampaignLive returns the live snapshot for the most recently active campaign.
// This is the endpoint Mission Control should poll when monitoring real campaigns.
// GET /api/mailing/campaigns/active/live
func (h *LiveCampaignHandlers) GetActiveCampaignLive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Find the most recently active campaign (sending or just completed)
	var activeCampaignID string
	err := h.db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_campaigns
		WHERE status IN ('sending', 'preparing', 'completed', 'completed_with_errors')
		ORDER BY 
			CASE WHEN status = 'sending' THEN 0 
			     WHEN status = 'preparing' THEN 1
			     ELSE 2 END,
			COALESCE(started_at, created_at) DESC
		LIMIT 1
	`).Scan(&activeCampaignID)

	if err != nil {
		// No active campaign — return empty snapshot
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"is_running":    false,
			"is_live":       true,
			"campaign_id":   nil,
			"campaign_name": "No Active Campaign",
			"events":        []interface{}{},
			"recent_events": []interface{}{},
			"decisions":     []interface{}{},
			"consultations": []interface{}{},
			"ab_stats":      map[string]interface{}{},
			"funnel": map[string]interface{}{
				"total_sent": 0, "open_rate": 0, "complaint_rate": 0,
			},
			"agent_state": map[string]string{"phase": "idle", "health_status": "healthy"},
			"warmup_tiers": []interface{}{},
			"config":       map[string]interface{}{},
		})
		return
	}

	// Redirect to the specific campaign live endpoint
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", activeCampaignID)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	h.GetLiveSnapshot(w, r)
}

// RegisterLiveCampaignRoutes registers the live campaign monitoring routes
func RegisterLiveCampaignRoutes(r chi.Router, db *sql.DB) {
	h := NewLiveCampaignHandlers(db)

	r.Get("/campaigns/{id}/live", h.GetLiveSnapshot)
	r.Get("/campaigns/active/live", h.GetActiveCampaignLive)

	// Ensure the agent decisions table exists
	db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_campaign_agent_decisions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			campaign_id UUID NOT NULL,
			decision_type VARCHAR(100) NOT NULL,
			reasoning TEXT,
			action_taken TEXT,
			impact TEXT,
			metrics_snapshot JSONB,
			decided_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)
	`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_decisions_campaign ON mailing_campaign_agent_decisions(campaign_id)`)

	fmt.Println("LiveCampaignHandlers: Live campaign monitoring endpoints registered")
}

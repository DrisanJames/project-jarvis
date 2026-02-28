package activation

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// ============================================================================
// CAMPAIGN SIMULATION ENGINE
// Generates realistic campaign events for dry-run testing of the mission
// control UI. Walks through warmup tiers, generates SparkPost-style events,
// feeds them through the adaptive decision engine, and logs everything.
// ============================================================================

// CampaignSimulator orchestrates a simulated campaign send
type CampaignSimulator struct {
	mu            sync.RWMutex
	Agent         *YahooActivationAgent
	Events        []SendEvent
	Decisions     []AgentDecision
	Consultations []Consultation
	ABStats       map[string]*ABVariantStats
	FunnelStats   FunnelStats
	IsRunning     bool
	Speed         float64 // simulation speed multiplier (1.0 = real time, 10.0 = 10x)
	StartedAt     *time.Time
	TickCount     int
}

// SendEvent represents a single email event (delivery, open, click, bounce, complaint)
type SendEvent struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"` // sent, delivered, opened, clicked, bounced, complained, converted
	Email     string    `json:"email"`
	VariantID string    `json:"variant_id"`
	Tier      string    `json:"tier"`
	Details   string    `json:"details,omitempty"`
}

// AgentDecision represents an autonomous decision made by the agent
type AgentDecision struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Type      string    `json:"type"` // throttle_change, variant_promotion, pause, resume, winner_selected, tier_advance
	Reasoning string    `json:"reasoning"`
	Action    string    `json:"action"`
	Impact    string    `json:"impact,omitempty"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
}

// Consultation represents human feedback to the agent
type Consultation struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	From      string    `json:"from"` // human or agent
	Message   string    `json:"message"`
	Applied   bool      `json:"applied"`
}

// ABVariantStats tracks live A/B test metrics per variant
type ABVariantStats struct {
	VariantID     string  `json:"variant_id"`
	SubjectLine   string  `json:"subject_line"`
	FromName      string  `json:"from_name"`
	PreheaderText string  `json:"preheader_text"`
	Sent          int     `json:"sent"`
	Delivered     int     `json:"delivered"`
	Opens         int     `json:"opens"`
	UniqueOpens   int     `json:"unique_opens"`
	Clicks        int     `json:"clicks"`
	UniqueClicks  int     `json:"unique_clicks"`
	Bounces       int     `json:"bounces"`
	Complaints    int     `json:"complaints"`
	Conversions   int     `json:"conversions"`
	Revenue       float64 `json:"revenue"`
	OpenRate      float64 `json:"open_rate"`
	ClickRate     float64 `json:"click_rate"`
	ConversionRate float64 `json:"conversion_rate"`
	BounceRate    float64 `json:"bounce_rate"`
	ComplaintRate float64 `json:"complaint_rate"`
	EPC           float64 `json:"epc"` // earnings per click
	Confidence    float64 `json:"confidence"` // statistical confidence 0-100
	IsWinner      bool    `json:"is_winner"`
	IsEliminated  bool    `json:"is_eliminated"`
}

// FunnelStats tracks the offer funnel
type FunnelStats struct {
	TotalSent       int     `json:"total_sent"`
	TotalDelivered  int     `json:"total_delivered"`
	TotalOpened     int     `json:"total_opened"`
	TotalClicked    int     `json:"total_clicked"`
	TotalConverted  int     `json:"total_converted"`
	TotalRevenue    float64 `json:"total_revenue"`
	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	ConversionRate  float64 `json:"conversion_rate"`
	ClickToConvert  float64 `json:"click_to_convert"`
}

// SimulatorSnapshot is the full state sent to the UI
type SimulatorSnapshot struct {
	IsRunning     bool                       `json:"is_running"`
	StartedAt     *time.Time                 `json:"started_at"`
	TickCount     int                        `json:"tick_count"`
	Events        []SendEvent                `json:"events"`
	RecentEvents  []SendEvent                `json:"recent_events"`
	Decisions     []AgentDecision            `json:"decisions"`
	Consultations []Consultation             `json:"consultations"`
	ABStats       map[string]*ABVariantStats `json:"ab_stats"`
	Funnel        FunnelStats                `json:"funnel"`
	AgentState    AgentState                 `json:"agent_state"`
	WarmupTiers   []WarmupTier               `json:"warmup_tiers"`
	Config        YahooAgentConfig           `json:"config"`
}

// NewCampaignSimulator creates a simulator for the given agent
func NewCampaignSimulator(agent *YahooActivationAgent) *CampaignSimulator {
	abStats := make(map[string]*ABVariantStats)
	for _, v := range agent.ABTestPlan.Variants {
		abStats[v.ID] = &ABVariantStats{
			VariantID:     v.ID,
			SubjectLine:   v.SubjectLine,
			FromName:      v.FromName,
			PreheaderText: v.PreheaderText,
		}
	}

	return &CampaignSimulator{
		Agent:         agent,
		Events:        []SendEvent{},
		Decisions:     []AgentDecision{},
		Consultations: []Consultation{},
		ABStats:       abStats,
		Speed:         3.0, // ~120 second total runtime for demo observation
	}
}

// Start begins the simulation
func (s *CampaignSimulator) Start() {
	s.mu.Lock()
	if s.IsRunning {
		s.mu.Unlock()
		return
	}
	s.IsRunning = true
	now := time.Now()
	s.StartedAt = &now
	s.Agent.CurrentState.Phase = "seed_testing"
	s.Agent.CurrentState.ThrottleRate = 100
	s.mu.Unlock()

	go s.runSimulation()
}

// Stop halts the simulation
func (s *CampaignSimulator) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IsRunning = false
}

// AddConsultation adds human feedback
func (s *CampaignSimulator) AddConsultation(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	consultation := Consultation{
		ID:      fmt.Sprintf("consult_%d", len(s.Consultations)+1),
		Time:    time.Now(),
		From:    "human",
		Message: message,
		Applied: false,
	}
	s.Consultations = append(s.Consultations, consultation)

	// Agent acknowledges and responds
	response := s.processConsultation(message)
	s.Consultations = append(s.Consultations, Consultation{
		ID:      fmt.Sprintf("consult_%d", len(s.Consultations)+1),
		Time:    time.Now(),
		From:    "agent",
		Message: response,
		Applied: true,
	})
}

// GetSnapshot returns the current simulation state
func (s *CampaignSimulator) GetSnapshot() SimulatorSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get recent events (last 50)
	recentEvents := s.Events
	if len(recentEvents) > 50 {
		recentEvents = recentEvents[len(recentEvents)-50:]
	}

	return SimulatorSnapshot{
		IsRunning:     s.IsRunning,
		StartedAt:     s.StartedAt,
		TickCount:     s.TickCount,
		Events:        s.Events,
		RecentEvents:  recentEvents,
		Decisions:     s.Decisions,
		Consultations: s.Consultations,
		ABStats:       s.ABStats,
		Funnel:        s.FunnelStats,
		AgentState:    s.Agent.CurrentState,
		WarmupTiers:   s.Agent.WarmupSchedule,
		Config:        s.Agent.Config,
	}
}

// ============================================================================
// SIMULATION LOOP
// ============================================================================

func (s *CampaignSimulator) runSimulation() {
	tickInterval := time.Duration(float64(time.Second) / s.Speed)

	// Walk through warmup tiers
	for tierIdx, tier := range s.Agent.WarmupSchedule {
		s.mu.Lock()
		if !s.IsRunning {
			s.mu.Unlock()
			return
		}
		s.Agent.CurrentState.CurrentTier = tierIdx + 1
		s.Agent.WarmupSchedule[tierIdx].Status = "sending"

		// Log tier advance decision
		s.Decisions = append(s.Decisions, AgentDecision{
			ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
			Time:      time.Now(),
			Type:      "tier_advance",
			Reasoning: fmt.Sprintf("Advancing to Tier %d (%s) — %d recipients at %d/min", tier.Tier, tier.Name, tier.Volume, tier.RatePerMin),
			Action:    fmt.Sprintf("Starting %s send", tier.Name),
			Metrics:   map[string]float64{"tier": float64(tier.Tier), "volume": float64(tier.Volume), "rate": float64(tier.RatePerMin)},
		})
		s.Agent.CurrentState.ThrottleRate = tier.RatePerMin
		s.mu.Unlock()

		// Simulate sending this tier's volume in batches
		batchSize := tier.RatePerMin / 2 // events per tick
		if batchSize < 10 {
			batchSize = 10
		}
		remaining := tier.Volume
		if remaining > 35000 {
			remaining = 35000 // cap for simulation — enough data for meaningful A/B stats
		}

		for remaining > 0 && s.IsRunning {
			s.mu.Lock()
			if s.Agent.CurrentState.IsPaused {
				s.mu.Unlock()
				time.Sleep(tickInterval * 5) // wait while paused
				continue
			}

			thisBatch := batchSize
			if thisBatch > remaining {
				thisBatch = remaining
			}

			s.generateBatchEvents(thisBatch, tierIdx)
			remaining -= thisBatch
			s.TickCount++

			// Every 5 ticks, agent evaluates signals and makes decisions
			if s.TickCount%5 == 0 {
				s.evaluateAndDecide(tierIdx)
			}

			s.updateFunnel()
			s.updateABStats()
			s.mu.Unlock()

			time.Sleep(tickInterval)
		}

		s.mu.Lock()
		s.Agent.WarmupSchedule[tierIdx].Status = "completed"

		// AB test winner check after tier 2
		if tierIdx == 1 && s.Agent.ABTestPlan.Status != "winner_selected" {
			s.selectABWinner()
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	s.Agent.CurrentState.Phase = "completed"
	s.IsRunning = false
	s.Decisions = append(s.Decisions, AgentDecision{
		ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
		Time:      time.Now(),
		Type:      "campaign_complete",
		Reasoning: fmt.Sprintf("All tiers completed. Final open rate: %.1f%%, complaint rate: %.3f%%", s.Agent.CurrentState.CurrentOpenRate, s.Agent.CurrentState.ComplaintRate),
		Action:    "Campaign completed successfully",
	})
	s.mu.Unlock()
}

func (s *CampaignSimulator) generateBatchEvents(count int, tierIdx int) {
	now := time.Now()
	variants := s.Agent.ABTestPlan.Variants
	tier := s.Agent.WarmupSchedule[tierIdx]

	for i := 0; i < count; i++ {
		variant := variants[rand.Intn(len(variants))]
		email := fmt.Sprintf("user%d@yahoo.com", rand.Intn(182000))

		// Sent event
		s.Events = append(s.Events, SendEvent{
			ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "sent",
			Email: email, VariantID: variant.ID, Tier: tier.Name,
		})
		s.Agent.CurrentState.TotalSent++
		s.ABStats[variant.ID].Sent++

		// Delivery (97% rate)
		if rand.Float64() < 0.97 {
			s.ABStats[variant.ID].Delivered++

			// Bounce (2-4% depending on tier)
			bounceRate := 0.02 + float64(tierIdx)*0.005
			if rand.Float64() < bounceRate {
				s.Events = append(s.Events, SendEvent{
					ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "bounced",
					Email: email, VariantID: variant.ID, Tier: tier.Name, Details: "550 5.1.1 User unknown",
				})
				s.Agent.CurrentState.TotalBounces++
				s.ABStats[variant.ID].Bounces++
				continue
			}

			// Open (varies by variant quality — simulate real variance)
			openRate := getVariantOpenRate(variant.ID)
			if rand.Float64() < openRate {
				s.Events = append(s.Events, SendEvent{
					ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "opened",
					Email: email, VariantID: variant.ID, Tier: tier.Name,
				})
				s.Agent.CurrentState.TotalOpens++
				s.ABStats[variant.ID].Opens++
				s.ABStats[variant.ID].UniqueOpens++

				// Click (30-45% of openers)
				clickRate := 0.30 + rand.Float64()*0.15
				if rand.Float64() < clickRate {
					s.Events = append(s.Events, SendEvent{
						ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "clicked",
						Email: email, VariantID: variant.ID, Tier: tier.Name,
						Details: "https://www.zes2i0nt.com/JHJSFL9/PS8241/",
					})
					s.Agent.CurrentState.TotalClicks++
					s.ABStats[variant.ID].Clicks++
					s.ABStats[variant.ID].UniqueClicks++

					// Conversion (8-15% of clickers)
					convRate := 0.08 + rand.Float64()*0.07
					if rand.Float64() < convRate {
						s.Events = append(s.Events, SendEvent{
							ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "converted",
							Email: email, VariantID: variant.ID, Tier: tier.Name,
							Details: "Sam's Club $50 Membership",
						})
						s.ABStats[variant.ID].Conversions++
						s.ABStats[variant.ID].Revenue += 4.50 // affiliate payout per conversion
					}
				}
			}

			// Complaint (0.03-0.08% — realistic Yahoo range)
			complaintRate := 0.0003 + float64(tierIdx)*0.0001
			if rand.Float64() < complaintRate {
				s.Events = append(s.Events, SendEvent{
					ID: fmt.Sprintf("evt_%d", len(s.Events)+1), Time: now, Type: "complained",
					Email: email, VariantID: variant.ID, Tier: tier.Name, Details: "Yahoo CFL complaint",
				})
				s.Agent.CurrentState.TotalComplaints++
				s.ABStats[variant.ID].Complaints++
			}
		}
	}
}

// Simulate different open rates per variant to make A/B test realistic
func getVariantOpenRate(variantID string) float64 {
	rates := map[string]float64{
		"variant_A": 0.062,  // A $20 Sam's Club Membership? — curiosity gap, top performer
		"variant_B": 0.055,  // Get $30 Sam's Cash with $50 Membership
		"variant_C": 0.048,  // $50 Membership, $30 Sam's Cash Back
		"variant_D": 0.058,  // It's Like a $20 Sam's Club Membership
		"variant_E": 0.051,  // Join Sam's Club, Get $30 Sam's Cash
		"variant_F": 0.044,  // $30 Sam's Cash Makes Membership a Steal
		"variant_G": 0.053,  // Membership Pays for Itself
		"variant_H": 0.057,  // $50 In, $30 Back—Do the Math
	}
	if r, ok := rates[variantID]; ok {
		return r + (rand.Float64()-0.5)*0.01 // add some noise
	}
	return 0.05
}

func (s *CampaignSimulator) evaluateAndDecide(tierIdx int) {
	state := &s.Agent.CurrentState
	if state.TotalSent == 0 {
		return
	}

	state.CurrentOpenRate = float64(state.TotalOpens) / float64(state.TotalSent) * 100
	state.CurrentBounceRate = float64(state.TotalBounces) / float64(state.TotalSent) * 100
	state.ComplaintRate = float64(state.TotalComplaints) / float64(state.TotalSent) * 100

	// Complaint check
	if state.ComplaintRate > 0.08 {
		state.IsPaused = true
		state.PauseReason = "Yahoo CFL complaint rate exceeded 0.08%"
		s.Decisions = append(s.Decisions, AgentDecision{
			ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
			Time:      time.Now(),
			Type:      "pause",
			Reasoning: fmt.Sprintf("Complaint rate %.3f%% exceeds Yahoo 0.08%% threshold after %d sends", state.ComplaintRate, state.TotalSent),
			Action:    "Campaign PAUSED — waiting for complaint rate to stabilize",
			Impact:    "Prevented potential Yahoo blocklisting",
			Metrics:   map[string]float64{"complaint_rate": state.ComplaintRate, "total_sent": float64(state.TotalSent)},
		})
		return
	}

	// Bounce check
	if state.CurrentBounceRate > 3.5 {
		oldRate := state.ThrottleRate
		state.ThrottleRate = int(math.Max(float64(oldRate)/2, 50))
		s.Decisions = append(s.Decisions, AgentDecision{
			ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
			Time:      time.Now(),
			Type:      "throttle_change",
			Reasoning: fmt.Sprintf("Bounce rate %.1f%% exceeds 3.5%% safety threshold", state.CurrentBounceRate),
			Action:    fmt.Sprintf("Throttle reduced from %d/min to %d/min", oldRate, state.ThrottleRate),
			Impact:    "Reducing sending pressure on Yahoo servers",
			Metrics:   map[string]float64{"bounce_rate": state.CurrentBounceRate, "old_rate": float64(oldRate), "new_rate": float64(state.ThrottleRate)},
		})
	}

	// Positive signal — ramp up
	if state.CurrentOpenRate >= 5.0 && state.ComplaintRate < 0.05 && state.CurrentBounceRate < 2.0 {
		if s.TickCount%15 == 0 && state.ThrottleRate < 1000 {
			oldRate := state.ThrottleRate
			state.ThrottleRate = int(math.Min(float64(oldRate)*1.3, 1000))
			s.Decisions = append(s.Decisions, AgentDecision{
				ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
				Time:      time.Now(),
				Type:      "throttle_change",
				Reasoning: fmt.Sprintf("Open rate %.1f%% on target, complaints %.3f%% clean, bounce %.1f%% acceptable", state.CurrentOpenRate, state.ComplaintRate, state.CurrentBounceRate),
				Action:    fmt.Sprintf("Throttle increased from %d/min to %d/min — all signals green", oldRate, state.ThrottleRate),
				Impact:    "Accelerating send to capitalize on positive engagement signals",
				Metrics:   map[string]float64{"open_rate": state.CurrentOpenRate, "complaint_rate": state.ComplaintRate},
			})
		}
	}

	// Phase progression
	if tierIdx == 0 && state.Phase == "seed_testing" {
		state.Phase = "ab_testing"
		s.Decisions = append(s.Decisions, AgentDecision{
			ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
			Time:      time.Now(),
			Type:      "tier_advance",
			Reasoning: fmt.Sprintf("Seed test complete with %.1f%% open rate — proceeding to A/B test phase", state.CurrentOpenRate),
			Action:    "Transitioning from seed testing to A/B testing phase",
		})
	} else if tierIdx >= 2 && state.Phase == "ab_testing" {
		state.Phase = "ramping"
	} else if tierIdx >= 3 {
		state.Phase = "full_send"
	}
}

func (s *CampaignSimulator) selectABWinner() {
	var bestVariant string
	var bestOpenRate float64

	for id, stats := range s.ABStats {
		if stats.Sent > 0 {
			or := float64(stats.Opens) / float64(stats.Sent) * 100
			if or > bestOpenRate {
				bestOpenRate = or
				bestVariant = id
			}
		}
	}

	if bestVariant != "" {
		s.ABStats[bestVariant].IsWinner = true
		s.Agent.ABTestPlan.Status = "winner_selected"
		s.Agent.ABTestPlan.WinnerID = bestVariant

		// Eliminate worst performers
		for id, stats := range s.ABStats {
			if id != bestVariant && stats.Sent > 0 {
				or := float64(stats.Opens) / float64(stats.Sent) * 100
				if or < bestOpenRate*0.7 {
					stats.IsEliminated = true
				}
			}
		}

		winner := s.ABStats[bestVariant]
		s.Decisions = append(s.Decisions, AgentDecision{
			ID:        fmt.Sprintf("decision_%d", len(s.Decisions)+1),
			Time:      time.Now(),
			Type:      "winner_selected",
			Reasoning: fmt.Sprintf("Variant %s (\"%s\" from %s) leads with %.1f%% open rate after %d sends — statistically significant at %.0f%% confidence", bestVariant, winner.SubjectLine, winner.FromName, bestOpenRate, winner.Sent, winner.Confidence),
			Action:    fmt.Sprintf("Selected %s as winner — remaining volume will use this variant", bestVariant),
			Impact:    "Expected to maximize open rate for remaining 70% of audience",
			Metrics:   map[string]float64{"open_rate": bestOpenRate, "clicks": float64(winner.Clicks), "conversions": float64(winner.Conversions)},
		})
	}
}

func (s *CampaignSimulator) updateFunnel() {
	totalSent := 0
	totalDelivered := 0
	totalOpened := 0
	totalClicked := 0
	totalConverted := 0
	totalRevenue := 0.0

	for _, stats := range s.ABStats {
		totalSent += stats.Sent
		totalDelivered += stats.Delivered
		totalOpened += stats.Opens
		totalClicked += stats.Clicks
		totalConverted += stats.Conversions
		totalRevenue += stats.Revenue
	}

	s.FunnelStats = FunnelStats{
		TotalSent:      totalSent,
		TotalDelivered: totalDelivered,
		TotalOpened:    totalOpened,
		TotalClicked:   totalClicked,
		TotalConverted: totalConverted,
		TotalRevenue:   totalRevenue,
	}

	if totalSent > 0 {
		s.FunnelStats.DeliveryRate = float64(totalDelivered) / float64(totalSent) * 100
		s.FunnelStats.OpenRate = float64(totalOpened) / float64(totalSent) * 100
		s.FunnelStats.ClickRate = float64(totalClicked) / float64(totalSent) * 100
		s.FunnelStats.ConversionRate = float64(totalConverted) / float64(totalSent) * 100
	}
	if totalClicked > 0 {
		s.FunnelStats.ClickToConvert = float64(totalConverted) / float64(totalClicked) * 100
	}
}

func (s *CampaignSimulator) updateABStats() {
	for _, stats := range s.ABStats {
		if stats.Sent > 0 {
			stats.OpenRate = float64(stats.Opens) / float64(stats.Sent) * 100
			stats.ClickRate = float64(stats.Clicks) / float64(stats.Sent) * 100
			stats.BounceRate = float64(stats.Bounces) / float64(stats.Sent) * 100
			stats.ComplaintRate = float64(stats.Complaints) / float64(stats.Sent) * 100
			if stats.Sent > 0 {
				stats.ConversionRate = float64(stats.Conversions) / float64(stats.Sent) * 100
			}
			if stats.Clicks > 0 {
				stats.EPC = stats.Revenue / float64(stats.Clicks)
			}
			// Simplified confidence calculation
			stats.Confidence = math.Min(float64(stats.Sent)/500*80+20, 99)
		}
	}
}

func (s *CampaignSimulator) processConsultation(message string) string {
	// Simple keyword-based response — in production this would use LLM
	switch {
	case containsAny(message, "slow", "reduce", "throttle down"):
		old := s.Agent.CurrentState.ThrottleRate
		s.Agent.CurrentState.ThrottleRate = int(math.Max(float64(old)/2, 50))
		s.Decisions = append(s.Decisions, AgentDecision{
			ID: fmt.Sprintf("decision_%d", len(s.Decisions)+1), Time: time.Now(),
			Type: "throttle_change", Reasoning: "Human consultation requested throttle reduction",
			Action: fmt.Sprintf("Throttle reduced from %d to %d/min per operator request", old, s.Agent.CurrentState.ThrottleRate),
		})
		return fmt.Sprintf("Acknowledged. Throttle reduced from %d to %d/min. I'll monitor signals and adjust.", old, s.Agent.CurrentState.ThrottleRate)

	case containsAny(message, "speed up", "increase", "faster", "ramp"):
		old := s.Agent.CurrentState.ThrottleRate
		s.Agent.CurrentState.ThrottleRate = int(math.Min(float64(old)*1.5, 1500))
		s.Decisions = append(s.Decisions, AgentDecision{
			ID: fmt.Sprintf("decision_%d", len(s.Decisions)+1), Time: time.Now(),
			Type: "throttle_change", Reasoning: "Human consultation requested throttle increase",
			Action: fmt.Sprintf("Throttle increased from %d to %d/min per operator request", old, s.Agent.CurrentState.ThrottleRate),
		})
		return fmt.Sprintf("Acknowledged. Throttle increased from %d to %d/min. Monitoring complaint rate closely.", old, s.Agent.CurrentState.ThrottleRate)

	case containsAny(message, "pause", "stop", "halt"):
		s.Agent.CurrentState.IsPaused = true
		s.Agent.CurrentState.PauseReason = "Operator requested pause"
		return "Campaign paused per your request. Send 'resume' when ready to continue."

	case containsAny(message, "resume", "continue", "start"):
		s.Agent.CurrentState.IsPaused = false
		s.Agent.CurrentState.PauseReason = ""
		return "Resuming campaign. Current signals look stable — proceeding with previous throttle rate."

	case containsAny(message, "status", "report", "how"):
		state := s.Agent.CurrentState
		return fmt.Sprintf("Current status: %s phase, %d sent, %.1f%% open rate, %.3f%% complaint rate, throttle at %d/min. %s",
			state.Phase, state.TotalSent, state.CurrentOpenRate, state.ComplaintRate, state.ThrottleRate,
			func() string {
				if state.CurrentOpenRate >= 5.0 {
					return "Open rate target MET."
				}
				return "Open rate still below 5% target."
			}())

	default:
		return "Understood. I've logged your feedback and will factor it into upcoming decisions. You can also try: 'slow down', 'speed up', 'pause', 'resume', or 'status'."
	}
}

func containsAny(s string, words ...string) bool {
	lower := fmt.Sprintf("%s", s)
	for _, w := range words {
		if len(lower) >= len(w) {
			for i := 0; i <= len(lower)-len(w); i++ {
				if lower[i:i+len(w)] == w {
					return true
				}
			}
		}
	}
	return false
}

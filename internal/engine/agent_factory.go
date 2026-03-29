package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"time"
)

const throttleAgentStateTTL = 4 * time.Hour

// AgentFactory spawns 6 agents per ISP (48 total) and provides
// access to agent instances by ISP and type.
type AgentFactory struct {
	db          *sql.DB
	orgID       string
	memory      *MemoryStore
	store       *SuppressionStore
	convictions *ConvictionStore
	alertCh     chan Decision

	// ISP -> AgentType -> Agent
	agents map[ISP]map[AgentType]Agent

	// ISP -> SuppressionAgent (needs separate access for record processing)
	suppressionAgents map[ISP]*SuppressionAgent
}

// NewAgentFactory creates the factory and initializes all 48 agents.
func NewAgentFactory(db *sql.DB, orgID string, memory *MemoryStore, store *SuppressionStore, convictions *ConvictionStore) *AgentFactory {
	f := &AgentFactory{
		db:                db,
		orgID:             orgID,
		memory:            memory,
		store:             store,
		convictions:       convictions,
		alertCh:           make(chan Decision, 1000),
		agents:            make(map[ISP]map[AgentType]Agent),
		suppressionAgents: make(map[ISP]*SuppressionAgent),
	}
	return f
}

// Initialize loads ISP configs and creates all agents.
func (f *AgentFactory) Initialize(ctx context.Context) error {
	configs, err := f.loadISPConfigs(ctx)
	if err != nil {
		log.Printf("[factory] DB load failed (%v), using in-memory defaults", err)
		configs = f.defaultConfigs()
	}

	if len(configs) == 0 {
		log.Println("[factory] no ISP configs found, seeding defaults")
		f.seedDefaultConfigs(ctx)
		configs, _ = f.loadISPConfigs(ctx)
	}

	if len(configs) == 0 {
		log.Println("[factory] DB seed failed, falling back to in-memory defaults")
		configs = f.defaultConfigs()
	}

	for _, cfg := range configs {
		f.agents[cfg.ISP] = make(map[AgentType]Agent)

		id := func(at AgentType) AgentID {
			return AgentID{ISP: cfg.ISP, AgentType: at}
		}

		f.agents[cfg.ISP][AgentReputation] = NewReputationAgent(id(AgentReputation), cfg, f.memory, f.convictions, f.alertCh)
		f.agents[cfg.ISP][AgentThrottle] = NewThrottleAgent(id(AgentThrottle), cfg, f.memory, f.convictions, f.alertCh)
		f.agents[cfg.ISP][AgentPool] = NewPoolAgent(id(AgentPool), cfg, f.memory, f.convictions, f.alertCh)
		f.agents[cfg.ISP][AgentWarmup] = NewWarmupAgent(id(AgentWarmup), cfg, f.memory, f.convictions, f.alertCh)
		f.agents[cfg.ISP][AgentEmergency] = NewEmergencyAgent(id(AgentEmergency), cfg, f.memory, f.convictions, f.alertCh)

		suppAgent := NewSuppressionAgent(id(AgentSuppression), cfg, f.store, f.memory, f.convictions, f.alertCh)
		f.agents[cfg.ISP][AgentSuppression] = suppAgent
		f.suppressionAgents[cfg.ISP] = suppAgent
	}

	log.Printf("[factory] initialized %d agents across %d ISPs", len(configs)*6, len(configs))
	return nil
}

// SetRateRegistry injects the shared rate registry into every ThrottleAgent.
// Must be called after Initialize().
func (f *AgentFactory) SetRateRegistry(registry *ISPRateRegistry) {
	for _, agentsByType := range f.agents {
		if ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent); ok {
			ta.SetRateRegistry(registry)
			ta.SetDB(f.db)
		}
	}
}

// SetShutdownContext propagates the server lifecycle context to all
// ThrottleAgents so their persist goroutines respect graceful shutdown.
func (f *AgentFactory) SetShutdownContext(ctx context.Context) {
	for _, agentsByType := range f.agents {
		if ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent); ok {
			ta.SetShutdownContext(ctx)
		}
	}
}

// RestoreThrottleState loads persisted ThrottleAgent state from the database
// and restores it into each matching ThrottleAgent. Uses a 4-hour TTL to
// preserve critical context (high backoffCount, active recovery) across
// restarts while discarding truly stale state.
func (f *AgentFactory) RestoreThrottleState() int {
	if f.db == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := f.db.QueryContext(ctx, `
		SELECT isp, current_rate_adj, original_rate, last_stable_rate,
		       backoff_count, in_recovery, recovery_started, last_backoff_at, updated_at,
		       COALESCE(escalation_adj, 1.0), escalation_cooldown_until, last_escalation_at
		FROM mailing_engine_throttle_agent_state
		WHERE updated_at > $1
	`, time.Now().Add(-throttleAgentStateTTL))
	if err != nil {
		log.Printf("[factory] restore throttle state failed: %v", err)
		return 0
	}
	defer rows.Close()

	restored := 0
	for rows.Next() {
		var ispStr string
		var s ThrottleState
		var recoveryStarted, lastBackoffAt, escalationCooldown, lastEscalation sql.NullTime
		var updatedAt time.Time
		if err := rows.Scan(&ispStr, &s.CurrentRateAdj, &s.OriginalRate, &s.LastStableRate,
			&s.BackoffCount, &s.InRecovery, &recoveryStarted, &lastBackoffAt, &updatedAt,
			&s.EscalationAdj, &escalationCooldown, &lastEscalation); err != nil {
			log.Printf("[factory] scan throttle state row: %v", err)
			continue
		}
		if recoveryStarted.Valid {
			s.RecoveryStarted = recoveryStarted.Time
		}
		if lastBackoffAt.Valid {
			s.LastBackoffAt = lastBackoffAt.Time
		}
		if escalationCooldown.Valid {
			s.EscalationCooldownUntil = escalationCooldown.Time
		}
		if lastEscalation.Valid {
			s.LastEscalationAt = lastEscalation.Time
		}

		isp := ISP(ispStr)
		agentsByType, ok := f.agents[isp]
		if !ok {
			continue
		}
		ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent)
		if !ok {
			continue
		}

		// Restore if the agent is actively throttled, escalated, or has significant backoff history
		if s.CurrentRateAdj < 1.0 || s.BackoffCount >= 3 || s.InRecovery || s.EscalationAdj > 1.0 {
			ta.RestoreState(s)
			restored++
			log.Printf("[factory] restored throttle state for %s: adj=%.3f backoff=%d inRecovery=%v escalation=%.2f (saved %s ago)",
				isp, s.CurrentRateAdj, s.BackoffCount, s.InRecovery, s.EscalationAdj, time.Since(updatedAt).Truncate(time.Second))
		}
	}
	return restored
}

// UpdateConfig updates the in-memory ISPConfig for an ISP's ThrottleAgent
// and pushes the new originalRate so future adjustments scale correctly.
// Returns the updated config and whether the ISP was found. The caller is
// responsible for persisting to DB and pushing the new rate to the registry.
func (f *AgentFactory) UpdateConfig(isp ISP, apply func(*ISPConfig)) (ISPConfig, bool) {
	agentsByType, ok := f.agents[isp]
	if !ok {
		return ISPConfig{}, false
	}
	ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent)
	if !ok {
		return ISPConfig{}, false
	}
	ta.mu.Lock()
	apply(&ta.Config)
	ta.originalRate = ta.Config.MaxMsgRate
	ta.mu.Unlock()
	return ta.Config, true
}

// ResetThrottle clears the throttle state for an ISP, restoring the rate
// adjustment to 1.0 and clearing backoff/recovery state. Pushes the fresh
// rate to the registry. Returns the new effective rate.
func (f *AgentFactory) ResetThrottle(isp ISP) (int, bool) {
	agentsByType, ok := f.agents[isp]
	if !ok {
		return 0, false
	}
	ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent)
	if !ok {
		return 0, false
	}
	ta.mu.Lock()
	ta.currentRateAdj = 1.0
	ta.backoffCount = 0
	ta.inRecovery = false
	ta.lastStableRate = 1.0
	ta.escalationAdj = 1.0
	effectiveRate := ta.computeEffectiveRateLocked()
	ta.mu.Unlock()

	if ta.rateRegistry != nil {
		ta.rateRegistry.SetRate(isp, float64(effectiveRate))
		ta.rateRegistry.ClearIPState(isp)
	}

	if ta.db != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ta.db.ExecContext(ctx, `
				DELETE FROM mailing_engine_throttle_agent_state WHERE isp = $1
			`, string(isp))
			ta.db.ExecContext(ctx, `
				DELETE FROM mailing_isp_throttle_state WHERE isp = $1
			`, string(isp))
		}()
	}
	return effectiveRate, true
}

// GetConfigs returns a map of ISP -> ISPConfig for all initialized agents.
// Used by the wiring code to seed the ISPRateRegistry with initial rates.
func (f *AgentFactory) GetConfigs() map[ISP]ISPConfig {
	configs := make(map[ISP]ISPConfig)
	for isp, agentsByType := range f.agents {
		if ta, ok := agentsByType[AgentThrottle].(*ThrottleAgent); ok {
			configs[isp] = ta.Config
		}
	}
	return configs
}

// GetAgent returns a specific agent by ISP and type.
func (f *AgentFactory) GetAgent(isp ISP, at AgentType) Agent {
	if m, ok := f.agents[isp]; ok {
		return m[at]
	}
	return nil
}

// GetSuppressionAgent returns the suppression agent for an ISP.
func (f *AgentFactory) GetSuppressionAgent(isp ISP) *SuppressionAgent {
	return f.suppressionAgents[isp]
}

// GetThrottleAgent returns the throttle agent for an ISP, or nil if not found.
func (f *AgentFactory) GetThrottleAgent(isp ISP) *ThrottleAgent {
	agentsByType, ok := f.agents[isp]
	if !ok {
		return nil
	}
	ta, _ := agentsByType[AgentThrottle].(*ThrottleAgent)
	return ta
}

// GetAllAgents returns all agents as a flat slice.
func (f *AgentFactory) GetAllAgents() []Agent {
	var all []Agent
	for _, m := range f.agents {
		for _, a := range m {
			all = append(all, a)
		}
	}
	return all
}

// GetISPAgents returns all 6 agents for a given ISP.
func (f *AgentFactory) GetISPAgents(isp ISP) []Agent {
	m, ok := f.agents[isp]
	if !ok {
		return nil
	}
	var agents []Agent
	for _, a := range m {
		agents = append(agents, a)
	}
	return agents
}

// AlertChannel returns the channel where agents emit decisions.
func (f *AgentFactory) AlertChannel() <-chan Decision {
	return f.alertCh
}

func (f *AgentFactory) loadISPConfigs(ctx context.Context) ([]ISPConfig, error) {
	rows, err := f.db.QueryContext(ctx,
		`SELECT id, organization_id, isp, display_name, domain_patterns, mx_patterns,
		 bounce_warn_pct, bounce_action_pct, complaint_warn_pct, complaint_action_pct,
		 max_connections, max_msg_rate, deferral_codes, known_behaviors, pool_name,
		 warmup_schedule, enabled, created_at, updated_at,
		 COALESCE(max_escalation_multiplier, 1.5) AS max_escalation_multiplier
		 FROM mailing_engine_isp_config WHERE organization_id = $1 AND enabled = TRUE`,
		f.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []ISPConfig
	for rows.Next() {
		var c ISPConfig
		var domainPatternsJSON, mxPatternsJSON, deferralCodesJSON []byte
		err := rows.Scan(&c.ID, &c.OrganizationID, &c.ISP, &c.DisplayName,
			&domainPatternsJSON, &mxPatternsJSON,
			&c.BounceWarnPct, &c.BounceActionPct, &c.ComplaintWarnPct, &c.ComplaintActionPct,
			&c.MaxConnections, &c.MaxMsgRate, &deferralCodesJSON, &c.KnownBehaviors,
			&c.PoolName, &c.WarmupSchedule, &c.Enabled, &c.CreatedAt, &c.UpdatedAt,
			&c.MaxEscalationMultiplier)
		if err != nil {
			log.Printf("[factory] scan ISP config error: %v", err)
			continue
		}
		json.Unmarshal(domainPatternsJSON, &c.DomainPatterns)
		json.Unmarshal(mxPatternsJSON, &c.MXPatterns)
		json.Unmarshal(deferralCodesJSON, &c.DeferralCodes)
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func (f *AgentFactory) defaultConfigs() []ISPConfig {
	type ispDefault struct {
		isp             ISP
		displayName     string
		domains         []string
		bounceWarn      float64
		bounceAction    float64
		complaintWarn   float64
		complaintAction float64
		maxConn         int
		maxRate         int
		deferrals       []string
	}
	defaults := []ispDefault{
		{ISPGmail, "Gmail", []string{"gmail.com", "googlemail.com"}, 1.5, 3, 0.02, 0.05, 20, 500, []string{"421-4.7.28"}},
		{ISPYahoo, "Yahoo", []string{"yahoo.com", "ymail.com"}, 2, 5, 0.03, 0.06, 10, 300, []string{"TSS04", "TSS03"}},
		{ISPAol, "AOL", []string{"aol.com", "aim.com"}, 2, 5, 0.03, 0.06, 10, 300, []string{"TSS04", "TSS03"}},
		{ISPMicrosoft, "Microsoft", []string{"outlook.com", "hotmail.com"}, 2, 5, 0.03, 0.06, 25, 500, []string{"421 RP-001"}},
		{ISPApple, "Apple iCloud", []string{"icloud.com", "me.com"}, 2, 5, 0.03, 0.06, 15, 400, []string{"421 4.7.0"}},
		{ISPComcast, "Comcast", []string{"comcast.net"}, 3, 7, 0.05, 0.1, 20, 500, []string{"421"}},
		{ISPAtt, "AT&T", []string{"att.net"}, 3, 7, 0.05, 0.1, 15, 400, []string{"421"}},
		{ISPSbcglobal, "SBC Global/BellSouth", []string{"sbcglobal.net", "bellsouth.net"}, 3, 7, 0.05, 0.1, 15, 400, []string{"421", "450"}},
		{ISPCox, "Cox", []string{"cox.net"}, 2.5, 6, 0.04, 0.08, 10, 300, []string{"421"}},
		{ISPCharter, "Charter/Spectrum", []string{"charter.net"}, 3, 7, 0.05, 0.1, 15, 400, []string{"421"}},
	}
	var configs []ISPConfig
	for _, d := range defaults {
		configs = append(configs, ISPConfig{
			ISP:                d.isp,
			DisplayName:        d.displayName,
			DomainPatterns:     d.domains,
			BounceWarnPct:      d.bounceWarn,
			BounceActionPct:    d.bounceAction,
			ComplaintWarnPct:   d.complaintWarn,
			ComplaintActionPct: d.complaintAction,
			MaxConnections:     d.maxConn,
			MaxMsgRate:         d.maxRate,
			DeferralCodes:      d.deferrals,
			PoolName:           PoolNameForISP(d.isp),
			Enabled:            true,
		})
	}
	return configs
}

func (f *AgentFactory) seedDefaultConfigs(ctx context.Context) {
	type ispDefault struct {
		isp             ISP
		displayName     string
		domains         []string
		mx              []string
		bounceWarn      float64
		bounceAction    float64
		complaintWarn   float64
		complaintAction float64
		maxConn         int
		maxRate         int
		deferrals       []string
	}

	defaults := []ispDefault{
		{ISPGmail, "Gmail", []string{"gmail.com", "googlemail.com", "google.com"},
			[]string{"*.google.com", "*.googlemail.com"}, 1.5, 3, 0.02, 0.05, 20, 500,
			[]string{"421-4.7.28", "421-4.7.26", "421-4.7.0"}},
		{ISPYahoo, "Yahoo", []string{"yahoo.com", "ymail.com", "rocketmail.com"},
			[]string{"*.yahoodns.net"}, 2, 5, 0.03, 0.06, 10, 300,
			[]string{"TSS04", "TSS03", "TS03", "451 4.7.1"}},
		{ISPAol, "AOL", []string{"aol.com", "aim.com"},
			[]string{"*.yahoodns.net"}, 2, 5, 0.03, 0.06, 10, 300,
			[]string{"TSS04", "TSS03", "TS03", "451 4.7.1"}},
		{ISPMicrosoft, "Microsoft", []string{"outlook.com", "hotmail.com", "live.com", "msn.com"},
			[]string{"*.protection.outlook.com"}, 2, 5, 0.03, 0.06, 25, 500,
			[]string{"421 RP-001", "421 RP-002", "451 4.7.500"}},
		{ISPApple, "Apple iCloud", []string{"icloud.com", "me.com", "mac.com"},
			[]string{"*.icloud.com"}, 2, 5, 0.03, 0.06, 15, 400,
			[]string{"421 4.7.0", "450 4.2.1"}},
		{ISPComcast, "Comcast", []string{"comcast.net", "xfinity.com"},
			[]string{"*.comcast.net"}, 3, 7, 0.05, 0.1, 20, 500,
			[]string{"421", "452 4.2.2"}},
		{ISPAtt, "AT&T", []string{"att.net"},
			[]string{"*.att.net"}, 3, 7, 0.05, 0.1, 15, 400,
			[]string{"421", "450"}},
		{ISPSbcglobal, "SBC Global/BellSouth", []string{"sbcglobal.net", "bellsouth.net"},
			[]string{"*.att.net"}, 3, 7, 0.05, 0.1, 15, 400,
			[]string{"421", "450"}},
		{ISPCox, "Cox", []string{"cox.net"},
			[]string{"*.cox.net"}, 2.5, 6, 0.04, 0.08, 10, 300,
			[]string{"421", "451"}},
		{ISPCharter, "Charter/Spectrum", []string{"charter.net", "spectrum.net", "rr.com", "twc.com"},
			[]string{"*.charter.net"}, 3, 7, 0.05, 0.1, 15, 400,
			[]string{"421", "451"}},
	}

	for _, d := range defaults {
		domainsJSON, _ := json.Marshal(d.domains)
		mxJSON, _ := json.Marshal(d.mx)
		defJSON, _ := json.Marshal(d.deferrals)
		schedJSON, _ := json.Marshal(DefaultWarmupSchedule())

		_, err := f.db.ExecContext(ctx,
			`INSERT INTO mailing_engine_isp_config
			(organization_id, isp, display_name, domain_patterns, mx_patterns,
			 bounce_warn_pct, bounce_action_pct, complaint_warn_pct, complaint_action_pct,
			 max_connections, max_msg_rate, deferral_codes, pool_name, warmup_schedule)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (organization_id, isp) DO NOTHING`,
			f.orgID, d.isp, d.displayName, domainsJSON, mxJSON,
			d.bounceWarn, d.bounceAction, d.complaintWarn, d.complaintAction,
			d.maxConn, d.maxRate, defJSON, PoolNameForISP(d.isp), schedJSON,
		)
		if err != nil {
			log.Printf("[factory] seed ISP config error isp=%s: %v", d.isp, err)
		}
	}
}

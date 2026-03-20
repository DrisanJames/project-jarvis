package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// ISPAgentEngineBridge exposes engine runtime data (throttle state, decisions,
// convictions, per-IP health) for managed ISP agents in the UI.
type ISPAgentEngineBridge struct {
	db              *sql.DB
	rateRegistry    *engine.ISPRateRegistry
	ispConfigs      map[engine.ISP]engine.ISPConfig
	convictionStore *engine.ConvictionStore
	agentFactory    *engine.AgentFactory
}

// domainToISP maps a managed agent's domain (e.g. "gmail.com") to the
// engine ISP type (e.g. "gmail") using ISPConfig.DomainPatterns.
// Returns empty string if no mapping exists.
func (b *ISPAgentEngineBridge) domainToISP(domain string) engine.ISP {
	lower := strings.ToLower(domain)
	for _, cfg := range b.ispConfigs {
		for _, pat := range cfg.DomainPatterns {
			if strings.ToLower(pat) == lower {
				return cfg.ISP
			}
		}
	}
	return ""
}

// HandleAgentEngine returns engine runtime data for a specific managed agent.
// GET /api/mailing/isp-agents/managed/{id}/engine
func (b *ISPAgentEngineBridge) HandleAgentEngine(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	orgID, _ := GetOrgIDFromRequest(r)

	var domain string
	query := `SELECT domain FROM mailing_isp_agents WHERE id = $1`
	var args []interface{}
	args = append(args, agentID)
	if orgID != uuid.Nil {
		query += ` AND organization_id = $2`
		args = append(args, orgID)
	}
	err := b.db.QueryRowContext(ctx, query, args...).Scan(&domain)
	if err != nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	isp := b.domainToISP(domain)
	if isp == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"engine_data": nil,
			"reason":      "no_engine_mapping",
			"domain":      domain,
		})
		return
	}

	resp := map[string]interface{}{
		"isp":    string(isp),
		"domain": domain,
	}

	// Current throttle rates
	if b.rateRegistry != nil {
		currentRate := b.rateRegistry.GetRate(isp)
		configRate := 0
		if cfg, ok := b.ispConfigs[isp]; ok {
			configRate = cfg.MaxMsgRate
		}
		rateAdj := 1.0
		if configRate > 0 {
			rateAdj = currentRate / float64(configRate)
		}
		resp["current_rate"] = currentRate
		resp["config_rate"] = configRate
		resp["rate_adjustment"] = rateAdj

		if ipRates := b.rateRegistry.GetIPRates(isp); len(ipRates) > 0 {
			resp["per_ip_rates"] = ipRates
		}
	}

	// Throttle agent state (from DB for consistency even if agent is in-memory)
	var throttleState map[string]interface{}
	var rateAdj float64
	var origRate, backoffCount int
	var inRecovery bool
	var updatedAt time.Time
	if err := b.db.QueryRowContext(ctx,
		`SELECT current_rate_adj, original_rate, backoff_count, in_recovery, updated_at
		 FROM mailing_engine_throttle_agent_state WHERE isp = $1`,
		string(isp)).Scan(&rateAdj, &origRate, &backoffCount, &inRecovery, &updatedAt); err == nil {
		throttleState = map[string]interface{}{
			"current_rate_adj": rateAdj,
			"original_rate":    origRate,
			"backoff_count":    backoffCount,
			"in_recovery":      inRecovery,
			"updated_at":       updatedAt,
		}
	}
	resp["throttle_state"] = throttleState

	// Recent decisions for this ISP
	var decisions []map[string]interface{}
	rows, err := b.db.QueryContext(ctx, `
		SELECT action_taken, action_params, result, created_at
		FROM mailing_engine_decisions
		WHERE isp = $1
		ORDER BY created_at DESC
		LIMIT 10
	`, string(isp))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var action, params, result string
			var createdAt time.Time
			if rows.Scan(&action, &params, &result, &createdAt) == nil {
				decisions = append(decisions, map[string]interface{}{
					"action":        action,
					"action_params": json.RawMessage(params),
					"result":        result,
					"created_at":    createdAt,
				})
			}
		}
	}
	resp["recent_decisions"] = decisions

	// Recent convictions
	var convictions []map[string]interface{}
	cRows, err := b.db.QueryContext(ctx, `
		SELECT verdict, statement, created_at
		FROM mailing_engine_convictions
		WHERE isp = $1 AND agent_type = 'throttle'
		ORDER BY created_at DESC
		LIMIT 5
	`, string(isp))
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var verdict, statement string
			var createdAt time.Time
			if cRows.Scan(&verdict, &statement, &createdAt) == nil {
				convictions = append(convictions, map[string]interface{}{
					"verdict":    verdict,
					"statement":  statement,
					"created_at": createdAt,
				})
			}
		}
	}
	resp["active_convictions"] = convictions

	// Per-IP health from tracking events (DB enrichment, not engine state)
	var ipHealth []map[string]interface{}
	ipRows, err := b.db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(regexp_replace(
				(SELECT value FROM jsonb_each_text(te.metadata::jsonb) WHERE key = 'vmta' LIMIT 1),
				'', ''), ''), 'unknown') AS vmta,
			COUNT(*) FILTER (WHERE te.event_type = 'bounced') AS bounces,
			COUNT(*) FILTER (WHERE te.event_type = 'delivered' OR te.event_type = 'sent') AS delivered,
			COUNT(*) FILTER (WHERE te.event_type = 'complained') AS complaints
		FROM mailing_tracking_events te
		WHERE LOWER(te.recipient_domain) IN (
			SELECT UNNEST($1::text[])
		)
		AND te.event_at > NOW() - INTERVAL '1 hour'
		GROUP BY vmta
		ORDER BY delivered DESC
	`, domainPatternsForISP(b.ispConfigs, isp))
	if err == nil {
		defer ipRows.Close()
		for ipRows.Next() {
			var vmta string
			var bounces, delivered, complaints int
			if ipRows.Scan(&vmta, &bounces, &delivered, &complaints) == nil {
				ipHealth = append(ipHealth, map[string]interface{}{
					"vmta":       vmta,
					"bounces_1h": bounces,
					"delivered_1h": delivered,
					"complaints_1h": complaints,
				})
			}
		}
	}
	resp["ip_health"] = ipHealth
	resp["per_ip_enabled"] = os.Getenv("ENABLE_PER_IP_RATE_LIMITING") == "true"

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[engine-bridge] encode error: %v", err)
	}
}

// HandleEngineSummary returns a lightweight summary of all ISP engine states.
// GET /api/engine/bridge-summary
func (b *ISPAgentEngineBridge) HandleEngineSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp := map[string]interface{}{
		"per_ip_enabled": os.Getenv("ENABLE_PER_IP_RATE_LIMITING") == "true",
		"updated_at":     time.Now().UTC(),
	}

	// All ISP rates
	if b.rateRegistry != nil {
		rates := make(map[string]interface{})
		for isp, currentRate := range b.rateRegistry.GetAllRates() {
			entry := map[string]interface{}{
				"current_rate": currentRate,
			}
			if cfg, ok := b.ispConfigs[isp]; ok {
				entry["config_rate"] = cfg.MaxMsgRate
				entry["display_name"] = cfg.DisplayName
			}
			if ipRates := b.rateRegistry.GetIPRates(isp); len(ipRates) > 0 {
				entry["ip_rates"] = ipRates
			}
			rates[string(isp)] = entry
		}
		resp["rates"] = rates
	}

	// Decisions count in last hour
	var decisionCount int
	b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_decisions WHERE created_at > NOW() - INTERVAL '1 hour'`).
		Scan(&decisionCount)
	resp["decisions_last_hour"] = decisionCount

	// Active throttle states
	var throttleStates []map[string]interface{}
	rows, err := b.db.QueryContext(ctx, `
		SELECT isp, current_rate_adj, backoff_count, in_recovery, updated_at
		FROM mailing_engine_throttle_agent_state
		WHERE updated_at > NOW() - INTERVAL '4 hours'
		ORDER BY isp
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ispStr string
			var adj float64
			var bc int
			var recovery bool
			var updAt time.Time
			if rows.Scan(&ispStr, &adj, &bc, &recovery, &updAt) == nil {
				throttleStates = append(throttleStates, map[string]interface{}{
					"isp":              ispStr,
					"current_rate_adj": adj,
					"backoff_count":    bc,
					"in_recovery":      recovery,
					"updated_at":       updAt,
				})
			}
		}
	}
	resp["throttle_states"] = throttleStates

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[engine-bridge] encode summary error: %v", err)
	}
}

// domainPatternsForISP returns the domain patterns for a given ISP config
// as a PostgreSQL text array literal.
func domainPatternsForISP(configs map[engine.ISP]engine.ISPConfig, isp engine.ISP) string {
	if cfg, ok := configs[isp]; ok {
		lower := make([]string, len(cfg.DomainPatterns))
		for i, d := range cfg.DomainPatterns {
			lower[i] = strings.ToLower(d)
		}
		return "{" + strings.Join(lower, ",") + "}"
	}
	return "{}"
}

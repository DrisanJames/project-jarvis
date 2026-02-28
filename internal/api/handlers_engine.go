package api

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// EngineService handles all governance engine API routes.
type EngineService struct {
	db           *sql.DB
	orchestrator *engine.Orchestrator
	store        *engine.SuppressionStore
	convictions  *engine.ConvictionStore
	processor    *engine.SignalProcessor
	rules        *engine.RuleStore
	orgID        string
}

// NewEngineService creates the engine API service.
func NewEngineService(
	db *sql.DB,
	orchestrator *engine.Orchestrator,
	store *engine.SuppressionStore,
	convictions *engine.ConvictionStore,
	processor *engine.SignalProcessor,
	rules *engine.RuleStore,
	orgID string,
) *EngineService {
	return &EngineService{
		db:           db,
		orchestrator: orchestrator,
		store:        store,
		convictions:  convictions,
		processor:    processor,
		rules:        rules,
		orgID:        orgID,
	}
}

// RegisterRoutes registers all engine routes under the mailing router.
func (es *EngineService) RegisterRoutes(r chi.Router) {
	r.Route("/engine", func(er chi.Router) {
		// Global
		er.Get("/dashboard", es.HandleDashboard)
		er.Get("/isps", es.HandleListISPs)
		er.Post("/override", es.HandleOverride)

		// Per-ISP
		er.Get("/isp/{isp}/dashboard", es.HandleISPDashboard)
		er.Get("/isp/{isp}/signals", es.HandleISPSignals)
		er.Get("/isp/{isp}/decisions", es.HandleISPDecisions)
		er.Get("/isp/{isp}/agents", es.HandleISPAgents)
		er.Post("/isp/{isp}/agents/{type}/pause", es.HandlePauseAgent)
		er.Post("/isp/{isp}/agents/{type}/resume", es.HandleResumeAgent)
		er.Get("/isp/{isp}/memory/patterns", es.HandleISPPatterns)
		er.Get("/isp/{isp}/memory/incidents", es.HandleISPIncidents)

		// Convictions (binary verdict memory)
		er.Get("/convictions/stream", es.HandleConvictionStream)
		er.Get("/convictions/velocity", es.HandleConvictionVelocity)
		er.Post("/recall", es.HandleRecall)
		er.Get("/isp/{isp}/convictions", es.HandleISPConvictions)
		er.Get("/isp/{isp}/convictions/stats", es.HandleISPConvictionStats)
		er.Get("/isp/{isp}/agents/{type}/convictions", es.HandleAgentConvictions)

		// Suppression
		er.Get("/suppression/check", es.HandleSuppressionCheck)
		er.Get("/isp/{isp}/suppressions", es.HandleListSuppressions)
		er.Get("/isp/{isp}/suppressions/stats", es.HandleSuppressionStats)
		er.Post("/isp/{isp}/suppressions/import", es.HandleImportSuppressions)
		er.Get("/isp/{isp}/suppressions/export", es.HandleExportSuppressions)
		er.Delete("/isp/{isp}/suppressions/{id}", es.HandleDeleteSuppression)

		// Rules
		er.Get("/rules", es.HandleListRules)
		er.Post("/rules", es.HandleCreateRule)
		er.Put("/rules/{id}", es.HandleUpdateRule)
		er.Delete("/rules/{id}", es.HandleDeleteRule)

		// ISP Config
		er.Get("/isp-config", es.HandleListISPConfig)
		er.Put("/isp-config/{isp}", es.HandleUpdateISPConfig)
	})
}

// --- Global Handlers ---

func (es *EngineService) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	states, _ := es.orchestrator.GetAgentStates(ctx)
	decisions := es.orchestrator.GetRecentDecisions(50)

	var totalSuppressions int64
	es.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_engine_suppressions WHERE organization_id = $1`,
		es.orgID).Scan(&totalSuppressions)

	activeCount := 0
	for _, s := range states {
		if s.Status == engine.StatusActive || s.Status == engine.StatusFiring {
			activeCount++
		}
	}

	ispSummaries := make([]engine.ISPHealthSummary, 0)
	for _, isp := range engine.AllISPs() {
		snap := es.processor.GetSnapshot(isp)
		ispStates := filterAgentStates(states, isp)
		recentCount := countISPDecisions(decisions, isp)

		var suppCount int64
		es.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_engine_suppressions WHERE organization_id = $1 AND isp = $2`,
			es.orgID, isp).Scan(&suppCount)

		health := 100.0 - (snap.BounceRate1h * 10) - (snap.ComplaintRate1h * 100) - (snap.DeferralRate5m * 2)
		if health < 0 {
			health = 0
		}

		hasEmergency := false
		activeAgents := 0
		for _, s := range ispStates {
			if s.Status == engine.StatusFiring {
				hasEmergency = true
			}
			if s.Status == engine.StatusActive || s.Status == engine.StatusFiring {
				activeAgents++
			}
		}

		ispSummaries = append(ispSummaries, engine.ISPHealthSummary{
			ISP:              isp,
			DisplayName:      ispDisplayName(isp),
			HealthScore:      health,
			AgentStates:      ispStates,
			ActiveAgents:     activeAgents,
			RecentDecisions:  recentCount,
			BounceRate:       snap.BounceRate1h,
			DeferralRate:     snap.DeferralRate5m,
			ComplaintRate:    snap.ComplaintRate1h,
			SuppressionCount: suppCount,
			HasEmergency:     hasEmergency,
			PoolName:         engine.PoolNameForISP(isp),
		})
	}

	overview := engine.EngineOverview{
		ISPs:              ispSummaries,
		TotalAgents:       48,
		ActiveAgents:      activeCount,
		RecentDecisions:   decisions,
		TotalSuppressions: totalSuppressions,
	}

	engineJSON(w, overview)
}

func (es *EngineService) HandleListISPs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	configs, err := es.loadISPConfigs(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, configs)
}

func (es *EngineService) HandleOverride(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if err := es.orchestrator.Override(r.Context(), req.Action, req.Params); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "ok", "action": req.Action})
}

// --- Per-ISP Handlers ---

func (es *EngineService) HandleISPDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isp := engine.ISP(chi.URLParam(r, "isp"))

	states, _ := es.orchestrator.GetISPAgentStates(ctx, isp)
	snap := es.processor.GetSnapshot(isp)
	decisions, _ := es.orchestrator.GetDecisions(ctx, string(isp), 50)
	suppStats, _ := es.store.GetStats(ctx, isp)

	engineJSON(w, map[string]interface{}{
		"isp":               isp,
		"agent_states":      states,
		"signals":           snap,
		"recent_decisions":  decisions,
		"suppression_stats": suppStats,
	})
}

func (es *EngineService) HandleISPSignals(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	snap := es.processor.GetSnapshot(isp)
	engineJSON(w, snap)
}

func (es *EngineService) HandleISPDecisions(w http.ResponseWriter, r *http.Request) {
	isp := chi.URLParam(r, "isp")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	decisions, err := es.orchestrator.GetDecisions(r.Context(), isp, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, decisions)
}

func (es *EngineService) HandleISPAgents(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	states, err := es.orchestrator.GetISPAgentStates(r.Context(), isp)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, states)
}

func (es *EngineService) HandlePauseAgent(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	at := engine.AgentType(chi.URLParam(r, "type"))
	if err := es.orchestrator.PauseAgent(r.Context(), isp, at); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "paused", "isp": string(isp), "agent": string(at)})
}

func (es *EngineService) HandleResumeAgent(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	at := engine.AgentType(chi.URLParam(r, "type"))
	if err := es.orchestrator.ResumeAgent(r.Context(), isp, at); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "active", "isp": string(isp), "agent": string(at)})
}

func (es *EngineService) HandleISPPatterns(w http.ResponseWriter, r *http.Request) {
	engineJSON(w, map[string]string{"status": "patterns loaded from S3"})
}

func (es *EngineService) HandleISPIncidents(w http.ResponseWriter, r *http.Request) {
	engineJSON(w, map[string]string{"status": "incidents loaded from S3"})
}

// --- Suppression Handlers ---

func (es *EngineService) HandleSuppressionCheck(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	ispParam := r.URL.Query().Get("isp")

	if email == "" {
		http.Error(w, "email parameter required", 400)
		return
	}

	if ispParam != "" {
		isp := engine.ISP(ispParam)
		suppressed := es.store.IsSuppressed(isp, email)
		result := engine.SuppressionCheckResult{
			Email:      email,
			ISP:        isp,
			Suppressed: suppressed,
		}
		if suppressed {
			s, _ := es.store.CheckDB(r.Context(), isp, email)
			if s != nil {
				result.Reason = s.Reason
				result.SuppressedAt = &s.SuppressedAt
			}
		}
		engineJSON(w, result)
		return
	}

	// Check all ISPs
	results := make([]engine.SuppressionCheckResult, 0)
	for _, isp := range engine.AllISPs() {
		suppressed := es.store.IsSuppressed(isp, email)
		result := engine.SuppressionCheckResult{
			Email:      email,
			ISP:        isp,
			Suppressed: suppressed,
		}
		if suppressed {
			s, _ := es.store.CheckDB(r.Context(), isp, email)
			if s != nil {
				result.Reason = s.Reason
				result.SuppressedAt = &s.SuppressedAt
			}
		}
		results = append(results, result)
	}
	engineJSON(w, results)
}

func (es *EngineService) HandleListSuppressions(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	search := r.URL.Query().Get("search")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}

	list, total, err := es.store.ListByISP(r.Context(), isp, search, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]interface{}{
		"items": list,
		"total": total,
		"limit": limit,
		"offset": offset,
	})
}

func (es *EngineService) HandleSuppressionStats(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	stats, err := es.store.GetStats(r.Context(), isp)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, stats)
}

func (es *EngineService) HandleImportSuppressions(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file upload required", 400)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		http.Error(w, "invalid CSV", 400)
		return
	}

	ctx := r.Context()
	imported := 0
	for _, row := range records {
		if len(row) == 0 {
			continue
		}
		email := strings.TrimSpace(row[0])
		if email == "" || !strings.Contains(email, "@") {
			continue
		}
		reason := "import"
		if len(row) > 1 {
			reason = row[1]
		}

		supp := engine.Suppression{
			Email:        email,
			ISP:          isp,
			Reason:       reason,
			SuppressedAt: time.Now(),
		}
		if ok, _ := es.store.Suppress(ctx, supp); ok {
			imported++
		}
	}

	engineJSON(w, map[string]interface{}{"imported": imported, "total_rows": len(records)})
}

func (es *EngineService) HandleExportSuppressions(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))

	list, _, err := es.store.ListByISP(r.Context(), isp, "", 1000000, 0)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s_suppressions.csv", isp))

	writer := csv.NewWriter(w)
	writer.Write([]string{"email", "reason", "dsn_code", "source_ip", "campaign_id", "suppressed_at"})
	for _, s := range list {
		writer.Write([]string{
			s.Email, s.Reason, s.DSNCode, s.SourceIP,
			s.CampaignID, s.SuppressedAt.Format(time.RFC3339),
		})
	}
	writer.Flush()
}

func (es *EngineService) HandleDeleteSuppression(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	id := chi.URLParam(r, "id")

	// Look up email by ID first
	var email string
	err := es.db.QueryRowContext(r.Context(),
		`SELECT email FROM mailing_engine_suppressions WHERE id = $1 AND organization_id = $2`,
		id, es.orgID).Scan(&email)
	if err != nil {
		http.Error(w, "suppression not found", 404)
		return
	}

	if err := es.store.Remove(r.Context(), isp, email); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "removed", "email": email, "isp": string(isp)})
}

// --- Rules Handlers ---

func (es *EngineService) HandleListRules(w http.ResponseWriter, r *http.Request) {
	isp := r.URL.Query().Get("isp")
	agentType := r.URL.Query().Get("agent_type")
	rules, err := es.rules.ListRules(r.Context(), isp, agentType)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, rules)
}

func (es *EngineService) HandleCreateRule(w http.ResponseWriter, r *http.Request) {
	var rule engine.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	created, err := es.rules.CreateRule(r.Context(), rule)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 201, created)
}

func (es *EngineService) HandleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var rule engine.Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	updated, err := es.rules.UpdateRule(r.Context(), id, rule)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, updated)
}

func (es *EngineService) HandleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := es.rules.DeleteRule(r.Context(), id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "deleted", "id": id})
}

// --- ISP Config Handlers ---

func (es *EngineService) HandleListISPConfig(w http.ResponseWriter, r *http.Request) {
	configs, err := es.loadISPConfigs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, configs)
}

func (es *EngineService) HandleUpdateISPConfig(w http.ResponseWriter, r *http.Request) {
	isp := chi.URLParam(r, "isp")
	var update struct {
		BounceWarnPct    *float64 `json:"bounce_warn_pct"`
		BounceActionPct  *float64 `json:"bounce_action_pct"`
		ComplaintWarnPct *float64 `json:"complaint_warn_pct"`
		ComplaintActionPct *float64 `json:"complaint_action_pct"`
		MaxConnections   *int     `json:"max_connections"`
		MaxMsgRate       *int     `json:"max_msg_rate"`
		Enabled          *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	query := `UPDATE mailing_engine_isp_config SET updated_at = NOW()`
	args := []interface{}{}
	n := 1

	if update.BounceWarnPct != nil {
		query += fmt.Sprintf(", bounce_warn_pct = $%d", n)
		args = append(args, *update.BounceWarnPct)
		n++
	}
	if update.BounceActionPct != nil {
		query += fmt.Sprintf(", bounce_action_pct = $%d", n)
		args = append(args, *update.BounceActionPct)
		n++
	}
	if update.ComplaintWarnPct != nil {
		query += fmt.Sprintf(", complaint_warn_pct = $%d", n)
		args = append(args, *update.ComplaintWarnPct)
		n++
	}
	if update.ComplaintActionPct != nil {
		query += fmt.Sprintf(", complaint_action_pct = $%d", n)
		args = append(args, *update.ComplaintActionPct)
		n++
	}
	if update.MaxConnections != nil {
		query += fmt.Sprintf(", max_connections = $%d", n)
		args = append(args, *update.MaxConnections)
		n++
	}
	if update.MaxMsgRate != nil {
		query += fmt.Sprintf(", max_msg_rate = $%d", n)
		args = append(args, *update.MaxMsgRate)
		n++
	}
	if update.Enabled != nil {
		query += fmt.Sprintf(", enabled = $%d", n)
		args = append(args, *update.Enabled)
		n++
	}

	query += fmt.Sprintf(" WHERE organization_id = $%d AND isp = $%d", n, n+1)
	args = append(args, es.orgID, isp)

	_, err := es.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	engineJSON(w, map[string]string{"status": "updated", "isp": isp})
}

// --- Helpers ---

func (es *EngineService) loadISPConfigs(ctx context.Context) ([]engine.ISPConfig, error) {
	rows, err := es.db.QueryContext(ctx,
		`SELECT id, organization_id, isp, display_name, domain_patterns, mx_patterns,
		 bounce_warn_pct, bounce_action_pct, complaint_warn_pct, complaint_action_pct,
		 max_connections, max_msg_rate, deferral_codes, known_behaviors, pool_name,
		 warmup_schedule, enabled, created_at, updated_at
		 FROM mailing_engine_isp_config WHERE organization_id = $1 ORDER BY isp`,
		es.orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []engine.ISPConfig
	for rows.Next() {
		var c engine.ISPConfig
		var dp, mx, dc []byte
		err := rows.Scan(&c.ID, &c.OrganizationID, &c.ISP, &c.DisplayName,
			&dp, &mx, &c.BounceWarnPct, &c.BounceActionPct,
			&c.ComplaintWarnPct, &c.ComplaintActionPct, &c.MaxConnections, &c.MaxMsgRate,
			&dc, &c.KnownBehaviors, &c.PoolName, &c.WarmupSchedule,
			&c.Enabled, &c.CreatedAt, &c.UpdatedAt)
		if err != nil {
			continue
		}
		json.Unmarshal(dp, &c.DomainPatterns)
		json.Unmarshal(mx, &c.MXPatterns)
		json.Unmarshal(dc, &c.DeferralCodes)
		configs = append(configs, c)
	}
	return configs, rows.Err()
}

func filterAgentStates(states []engine.AgentState, isp engine.ISP) []engine.AgentState {
	var filtered []engine.AgentState
	for _, s := range states {
		if s.ISP == isp {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func countISPDecisions(decisions []engine.Decision, isp engine.ISP) int {
	count := 0
	for _, d := range decisions {
		if d.ISP == isp {
			count++
		}
	}
	return count
}

func ispDisplayName(isp engine.ISP) string {
	names := map[engine.ISP]string{
		engine.ISPGmail:     "Gmail",
		engine.ISPYahoo:     "Yahoo",
		engine.ISPMicrosoft: "Microsoft",
		engine.ISPApple:     "Apple iCloud",
		engine.ISPComcast:   "Comcast",
		engine.ISPAtt:       "AT&T",
		engine.ISPCox:       "Cox",
		engine.ISPCharter:   "Charter/Spectrum",
	}
	if name, ok := names[isp]; ok {
		return name
	}
	return string(isp)
}

func engineJSON(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, data)
}

// --- Conviction Handlers ---

func (es *EngineService) HandleISPConvictions(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	verdict := r.URL.Query().Get("verdict")
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 500 {
		limit = n
	}

	if es.convictions == nil {
		engineJSON(w, map[string]interface{}{"convictions": []interface{}{}, "will_count": 0, "wont_count": 0})
		return
	}

	type agentConvictions struct {
		AgentType   string               `json:"agent_type"`
		Convictions []engine.Conviction  `json:"convictions"`
		WillCount   int                  `json:"will_count"`
		WontCount   int                  `json:"wont_count"`
	}

	var result []agentConvictions
	totalWill := 0
	totalWont := 0

	for _, at := range engine.AllAgentTypes() {
		var convs []engine.Conviction
		if verdict == "will" {
			convs = es.convictions.RecallByVerdict(isp, at, engine.VerdictWill)
		} else if verdict == "wont" {
			convs = es.convictions.RecallByVerdict(isp, at, engine.VerdictWont)
		} else {
			convs = es.convictions.RecallRecent(isp, at, limit)
		}
		wc, wnc := es.convictions.Stats(isp, at)
		totalWill += wc
		totalWont += wnc
		if len(convs) > limit {
			convs = convs[len(convs)-limit:]
		}
		result = append(result, agentConvictions{
			AgentType:   string(at),
			Convictions: convs,
			WillCount:   wc,
			WontCount:   wnc,
		})
	}

	engineJSON(w, map[string]interface{}{
		"isp":        isp,
		"agents":     result,
		"will_count": totalWill,
		"wont_count": totalWont,
	})
}

func (es *EngineService) HandleISPConvictionStats(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))

	if es.convictions == nil {
		engineJSON(w, map[string]interface{}{"isp": isp, "agents": []interface{}{}})
		return
	}

	type agentStats struct {
		AgentType string `json:"agent_type"`
		WillCount int    `json:"will_count"`
		WontCount int    `json:"wont_count"`
		Total     int    `json:"total"`
	}

	var stats []agentStats
	for _, at := range engine.AllAgentTypes() {
		wc, wnc := es.convictions.Stats(isp, at)
		stats = append(stats, agentStats{
			AgentType: string(at),
			WillCount: wc,
			WontCount: wnc,
			Total:     wc + wnc,
		})
	}

	engineJSON(w, map[string]interface{}{
		"isp":    isp,
		"agents": stats,
	})
}

func (es *EngineService) HandleAgentConvictions(w http.ResponseWriter, r *http.Request) {
	isp := engine.ISP(chi.URLParam(r, "isp"))
	agentType := engine.AgentType(chi.URLParam(r, "type"))
	verdict := r.URL.Query().Get("verdict")
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 1000 {
		limit = n
	}

	if es.convictions == nil {
		engineJSON(w, map[string]interface{}{"convictions": []interface{}{}})
		return
	}

	var convs []engine.Conviction
	if verdict == "will" {
		convs = es.convictions.RecallByVerdict(isp, agentType, engine.VerdictWill)
	} else if verdict == "wont" {
		convs = es.convictions.RecallByVerdict(isp, agentType, engine.VerdictWont)
	} else {
		convs = es.convictions.RecallRecent(isp, agentType, limit)
	}

	wc, wnc := es.convictions.Stats(isp, agentType)

	engineJSON(w, map[string]interface{}{
		"isp":         isp,
		"agent_type":  agentType,
		"convictions": convs,
		"will_count":  wc,
		"wont_count":  wnc,
	})
}

// --- SSE Stream + Velocity ---

func (es *EngineService) HandleConvictionStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ispFilter := r.URL.Query().Get("isp")
	agentFilter := r.URL.Query().Get("agent_type")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	if es.convictions == nil {
		return
	}

	subID := fmt.Sprintf("sse-%d", time.Now().UnixNano())
	ch := es.convictions.Subscribe(subID)
	defer es.convictions.Unsubscribe(subID)

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			if ispFilter != "" && string(c.ISP) != ispFilter {
				continue
			}
			if agentFilter != "" && string(c.AgentType) != agentFilter {
				continue
			}
			data, err := json.Marshal(c)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: conviction\ndata: %s\n\n", data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprintf(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}

func (es *EngineService) HandleConvictionVelocity(w http.ResponseWriter, r *http.Request) {
	if es.convictions == nil {
		engineJSON(w, map[string]interface{}{
			"global":    engine.VelocityStats{},
			"by_isp":    map[string]interface{}{},
			"by_agent":  map[string]interface{}{},
		})
		return
	}

	global := es.convictions.Velocity()

	byISP := make(map[string]map[string]int)
	byAgent := make(map[string]map[string]int)

	for _, isp := range engine.AllISPs() {
		ispStats := map[string]int{"will": 0, "wont": 0, "total": 0}
		for _, at := range engine.AllAgentTypes() {
			wc, wnc := es.convictions.Stats(isp, at)
			ispStats["will"] += wc
			ispStats["wont"] += wnc
			ispStats["total"] += wc + wnc

			atKey := string(at)
			if _, ok := byAgent[atKey]; !ok {
				byAgent[atKey] = map[string]int{"will": 0, "wont": 0, "total": 0}
			}
			byAgent[atKey]["will"] += wc
			byAgent[atKey]["wont"] += wnc
			byAgent[atKey]["total"] += wc + wnc
		}
		byISP[string(isp)] = ispStats
	}

	engineJSON(w, map[string]interface{}{
		"global":   global,
		"by_isp":   byISP,
		"by_agent": byAgent,
	})
}

// --- Recall ---

func (es *EngineService) HandleRecall(w http.ResponseWriter, r *http.Request) {
	if es.convictions == nil {
		engineJSON(w, map[string]interface{}{"error": "conviction store not available"})
		return
	}

	var req struct {
		ISP       string `json:"isp"`
		AgentType string `json:"agent_type"`
		Scenario  struct {
			DSNCodes      []string `json:"dsn_codes"`
			DeferralRate  float64  `json:"deferral_rate"`
			BounceRate    float64  `json:"bounce_rate"`
			ComplaintRate float64  `json:"complaint_rate"`
			DayOfWeek     string   `json:"day_of_week"`
			HourUTC       int      `json:"hour_utc"`
			AttemptedRate int      `json:"attempted_rate"`
			IP            string   `json:"ip"`
			Domain        string   `json:"domain"`
			IsHoliday     bool     `json:"is_holiday"`
			HolidayName   string   `json:"holiday_name"`
		} `json:"scenario"`
		Limit int `json:"limit"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 20
	}

	isp := engine.ISP(req.ISP)
	agentType := engine.AgentType(req.AgentType)

	query := engine.MicroContext{
		DayOfWeek:    req.Scenario.DayOfWeek,
		HourUTC:      req.Scenario.HourUTC,
		DSNCodes:     req.Scenario.DSNCodes,
		DeferralRate: req.Scenario.DeferralRate,
		BounceRate:   req.Scenario.BounceRate,
		ComplaintRate: req.Scenario.ComplaintRate,
		AttemptedRate: req.Scenario.AttemptedRate,
		IP:           req.Scenario.IP,
		Domain:       req.Scenario.Domain,
		IsHoliday:    req.Scenario.IsHoliday,
		HolidayName:  req.Scenario.HolidayName,
	}

	matched := es.convictions.RecallSimilar(isp, agentType, query, req.Limit)
	synthesis := engine.SynthesizeRecall(matched, query)

	engineJSON(w, map[string]interface{}{
		"query":       query,
		"isp":         isp,
		"agent_type":  agentType,
		"synthesis":   synthesis,
		"convictions": matched,
	})
}

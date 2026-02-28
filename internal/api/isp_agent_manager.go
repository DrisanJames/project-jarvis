package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ISPAgentManager provides persistent ISP agent management — CRUD, status updates,
// learning metrics, and the agent decision logging system.
type ISPAgentManager struct {
	db *sql.DB
}

// ============================================================================
// Handler 1: HandleListAgents
// GET /isp-agents/managed
// ============================================================================

func (m *ISPAgentManager) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	statusFilter := r.URL.Query().Get("status")
	ispFilter := r.URL.Query().Get("isp")

	query := `SELECT id, organization_id, isp, domain, status, config, knowledge,
		total_campaigns, total_sends, total_opens, total_clicks, total_bounces,
		total_complaints, avg_engagement, created_at, updated_at, last_active_at
		FROM mailing_isp_agents`

	var conditions []string
	var args []interface{}
	argIdx := 1

	if statusFilter != "" {
		conditions = append(conditions, statusArg(argIdx))
		args = append(args, statusFilter)
		argIdx++
	}
	if ispFilter != "" {
		conditions = append(conditions, ispArg(argIdx))
		args = append(args, ispFilter)
		argIdx++
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	query += " ORDER BY last_active_at DESC NULLS LAST, updated_at DESC"

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("ERROR: failed to list agents: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent data")
		return
	}
	defer rows.Close()

	type agentRow struct {
		ID              uuid.UUID       `json:"id"`
		OrganizationID  uuid.UUID       `json:"organization_id"`
		ISP             string          `json:"isp"`
		Domain          string          `json:"domain"`
		Status          string          `json:"status"`
		Config          json.RawMessage `json:"config"`
		Knowledge       json.RawMessage `json:"knowledge"`
		TotalCampaigns  int             `json:"total_campaigns"`
		TotalSends      int64           `json:"total_sends"`
		TotalOpens      int64           `json:"total_opens"`
		TotalClicks     int64           `json:"total_clicks"`
		TotalBounces    int64           `json:"total_bounces"`
		TotalComplaints int64           `json:"total_complaints"`
		AvgEngagement   float64         `json:"avg_engagement"`
		CreatedAt       time.Time       `json:"created_at"`
		UpdatedAt       time.Time       `json:"updated_at"`
		LastActiveAt    *time.Time      `json:"last_active_at"`
		// Enriched fields
		ActiveCampaigns    int        `json:"active_campaigns"`
		ProfileCount       int        `json:"profile_count"`
		AvgProfileEngage   float64    `json:"avg_profile_engagement"`
		LastLearningAt     *time.Time `json:"last_learning_at"`
	}

	var agents []agentRow

	for rows.Next() {
		var a agentRow
		var configBytes, knowledgeBytes []byte
		var lastActive sql.NullTime
		var avgEng sql.NullFloat64

		err := rows.Scan(
			&a.ID, &a.OrganizationID, &a.ISP, &a.Domain, &a.Status,
			&configBytes, &knowledgeBytes,
			&a.TotalCampaigns, &a.TotalSends, &a.TotalOpens, &a.TotalClicks,
			&a.TotalBounces, &a.TotalComplaints, &avgEng,
			&a.CreatedAt, &a.UpdatedAt, &lastActive,
		)
		if err != nil {
			log.Printf("ERROR: failed to scan agent row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve agent data")
			return
		}

		if configBytes != nil {
			a.Config = json.RawMessage(configBytes)
		} else {
			a.Config = json.RawMessage(`{}`)
		}
		if knowledgeBytes != nil {
			a.Knowledge = json.RawMessage(knowledgeBytes)
		} else {
			a.Knowledge = json.RawMessage(`{}`)
		}
		if lastActive.Valid {
			a.LastActiveAt = &lastActive.Time
		}
		if avgEng.Valid {
			a.AvgEngagement = avgEng.Float64
		}

		// Fetch active campaign count
		var activeCampaigns int
		err = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_agent_campaigns WHERE agent_id = $1 AND status IN ('pending', 'active')`,
			a.ID,
		).Scan(&activeCampaigns)
		if err != nil {
			activeCampaigns = 0
		}
		a.ActiveCampaigns = activeCampaigns

		// Fetch knowledge metrics from inbox profiles
		var profileCount int
		var avgProfileEngagement sql.NullFloat64
		var lastLearning sql.NullTime
		err = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*), AVG(engagement_score), MAX(last_activity_at) FROM mailing_inbox_profiles WHERE domain = $1`,
			a.Domain,
		).Scan(&profileCount, &avgProfileEngagement, &lastLearning)
		if err == nil {
			a.ProfileCount = profileCount
			if avgProfileEngagement.Valid {
				a.AvgProfileEngage = avgProfileEngagement.Float64
			}
			if lastLearning.Valid {
				a.LastLearningAt = &lastLearning.Time
			}
		}

		agents = append(agents, a)
	}

	if err := rows.Err(); err != nil {
		log.Printf("ERROR: error iterating agents: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent data")
		return
	}

	if agents == nil {
		agents = []agentRow{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"agents": agents,
		"total":  len(agents),
	})
}

// ============================================================================
// Handler 2: HandleGetAgent
// GET /isp-agents/managed/{agentId}
// ============================================================================

func (m *ISPAgentManager) HandleGetAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Fetch the agent
	var (
		id              uuid.UUID
		organizationID  uuid.UUID
		isp, domain     string
		status          string
		configBytes     []byte
		knowledgeBytes  []byte
		totalCampaigns  int
		totalSends      int64
		totalOpens      int64
		totalClicks     int64
		totalBounces    int64
		totalComplaints int64
		avgEng          sql.NullFloat64
		createdAt       time.Time
		updatedAt       time.Time
		lastActive      sql.NullTime
	)

	err = m.db.QueryRowContext(ctx,
		`SELECT id, organization_id, isp, domain, status, config, knowledge,
			total_campaigns, total_sends, total_opens, total_clicks, total_bounces,
			total_complaints, avg_engagement, created_at, updated_at, last_active_at
		FROM mailing_isp_agents WHERE id = $1`, agentID,
	).Scan(
		&id, &organizationID, &isp, &domain, &status,
		&configBytes, &knowledgeBytes,
		&totalCampaigns, &totalSends, &totalOpens, &totalClicks,
		&totalBounces, &totalComplaints, &avgEng,
		&createdAt, &updatedAt, &lastActive,
	)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: failed to fetch agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent details")
		return
	}

	agent := map[string]interface{}{
		"id":               id,
		"organization_id":  organizationID,
		"isp":              isp,
		"domain":           domain,
		"status":           status,
		"total_campaigns":  totalCampaigns,
		"total_sends":      totalSends,
		"total_opens":      totalOpens,
		"total_clicks":     totalClicks,
		"total_bounces":    totalBounces,
		"total_complaints": totalComplaints,
		"created_at":       createdAt,
		"updated_at":       updatedAt,
	}

	if configBytes != nil {
		agent["config"] = json.RawMessage(configBytes)
	} else {
		agent["config"] = json.RawMessage(`{}`)
	}
	if knowledgeBytes != nil {
		agent["knowledge"] = json.RawMessage(knowledgeBytes)
	} else {
		agent["knowledge"] = json.RawMessage(`{}`)
	}
	if avgEng.Valid {
		agent["avg_engagement"] = avgEng.Float64
	} else {
		agent["avg_engagement"] = 0.0
	}
	if lastActive.Valid {
		agent["last_active_at"] = lastActive.Time
	} else {
		agent["last_active_at"] = nil
	}

	// Fetch recent campaigns for this agent (last 10)
	campRows, err := m.db.QueryContext(ctx,
		`SELECT id, campaign_id, recipient_count, status, send_window, performance, decisions,
			created_at, started_at, completed_at
		FROM mailing_agent_campaigns WHERE agent_id = $1 ORDER BY created_at DESC LIMIT 10`, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to fetch campaigns for agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve campaign data")
		return
	}
	defer campRows.Close()

	var campaigns []map[string]interface{}
	for campRows.Next() {
		var (
			cID            uuid.UUID
			campaignID     uuid.UUID
			recipientCount int
			cStatus        string
			sendWindowB    []byte
			performanceB   []byte
			decisionsB     []byte
			cCreatedAt     time.Time
			startedAt      sql.NullTime
			completedAt    sql.NullTime
		)
		if err := campRows.Scan(
			&cID, &campaignID, &recipientCount, &cStatus,
			&sendWindowB, &performanceB, &decisionsB,
			&cCreatedAt, &startedAt, &completedAt,
		); err != nil {
			log.Printf("ERROR: failed to scan campaign row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve campaign data")
			return
		}

		camp := map[string]interface{}{
			"id":              cID,
			"campaign_id":     campaignID,
			"recipient_count": recipientCount,
			"status":          cStatus,
			"created_at":      cCreatedAt,
		}

		if sendWindowB != nil {
			camp["send_window"] = json.RawMessage(sendWindowB)
		} else {
			camp["send_window"] = json.RawMessage(`{}`)
		}
		if performanceB != nil {
			camp["performance"] = json.RawMessage(performanceB)
		} else {
			camp["performance"] = json.RawMessage(`{}`)
		}
		if decisionsB != nil {
			camp["decisions"] = json.RawMessage(decisionsB)
		} else {
			camp["decisions"] = json.RawMessage(`[]`)
		}
		if startedAt.Valid {
			camp["started_at"] = startedAt.Time
		} else {
			camp["started_at"] = nil
		}
		if completedAt.Valid {
			camp["completed_at"] = completedAt.Time
		} else {
			camp["completed_at"] = nil
		}

		campaigns = append(campaigns, camp)
	}

	if err := campRows.Err(); err != nil {
		log.Printf("ERROR: error iterating campaigns: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve campaign data")
		return
	}

	if campaigns == nil {
		campaigns = []map[string]interface{}{}
	}

	// Fetch ISP-specific inbox profile stats
	var profileCount int
	var avgProfileEngagement sql.NullFloat64
	var lastLearning sql.NullTime
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*), AVG(engagement_score), MAX(last_activity_at)
		FROM mailing_inbox_profiles WHERE domain = $1`, domain,
	).Scan(&profileCount, &avgProfileEngagement, &lastLearning)

	profileStats := map[string]interface{}{
		"profile_count": profileCount,
	}
	if avgProfileEngagement.Valid {
		profileStats["avg_engagement"] = avgProfileEngagement.Float64
	} else {
		profileStats["avg_engagement"] = 0.0
	}
	if lastLearning.Valid {
		profileStats["last_activity_at"] = lastLearning.Time
	} else {
		profileStats["last_activity_at"] = nil
	}

	agent["campaigns"] = campaigns
	agent["profile_stats"] = profileStats

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"agent": agent,
	})
}

// ============================================================================
// Handler 3: HandleUpdateAgentStatus
// PATCH /isp-agents/managed/{agentId}/status
// ============================================================================

func (m *ISPAgentManager) HandleUpdateAgentStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	validStatuses := map[string]bool{
		"dormant": true, "learning": true, "sending": true, "adapting": true, "complete": true,
	}
	if !validStatuses[body.Status] {
		respondError(w, http.StatusBadRequest, "invalid status: must be dormant, learning, sending, adapting, or complete")
		return
	}

	result, err := m.db.ExecContext(ctx,
		`UPDATE mailing_isp_agents SET status = $1, updated_at = NOW() WHERE id = $2`,
		body.Status, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update agent status: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to update agent status")
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message": "status updated",
		"status":  body.Status,
	})
}

// ============================================================================
// Handler 4: HandleUpdateAgentConfig
// PATCH /isp-agents/managed/{agentId}/config
// ============================================================================

func (m *ISPAgentManager) HandleUpdateAgentConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	var body struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Config == nil || len(body.Config) == 0 {
		respondError(w, http.StatusBadRequest, "config is required")
		return
	}

	// Validate that config is valid JSON
	var configCheck interface{}
	if err := json.Unmarshal(body.Config, &configCheck); err != nil {
		respondError(w, http.StatusBadRequest, "config must be valid JSON")
		return
	}

	result, err := m.db.ExecContext(ctx,
		`UPDATE mailing_isp_agents SET config = $1, updated_at = NOW() WHERE id = $2`,
		body.Config, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update agent config: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to update agent configuration")
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message": "config updated",
		"config":  json.RawMessage(body.Config),
	})
}

// ============================================================================
// Handler 5: HandleAgentLearn
// POST /isp-agents/managed/{agentId}/learn
// ============================================================================

func (m *ISPAgentManager) HandleAgentLearn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Fetch the agent's domain and current stats
	var domain string
	var totalSends, totalBounces, totalComplaints int64
	err = m.db.QueryRowContext(ctx,
		`SELECT domain, total_sends, total_bounces, total_complaints
		FROM mailing_isp_agents WHERE id = $1`, agentID,
	).Scan(&domain, &totalSends, &totalBounces, &totalComplaints)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		log.Printf("ERROR: failed to fetch agent for learning %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent data")
		return
	}

	// Query inbox profiles for the agent's domain: engagement patterns, best hours, bounce rates
	profileRows, err := m.db.QueryContext(ctx,
		`SELECT engagement_score, best_send_hour, total_sent, total_opens, total_clicks, total_bounces
		FROM mailing_inbox_profiles WHERE domain = $1`, domain,
	)
	if err != nil {
		log.Printf("ERROR: failed to query profiles for domain %s: %v", domain, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve profile data")
		return
	}
	defer profileRows.Close()

	var (
		totalProfiles     int
		totalEngagement   float64
		hourCounts        = make(map[int]float64) // hour -> total engagement
		hourOccurrences   = make(map[int]int)     // hour -> count
		profileTotalSent  int64
		profileTotalOpens int64
	)

	for profileRows.Next() {
		var (
			engagementScore sql.NullFloat64
			bestSendHour    sql.NullInt64
			pSent           sql.NullInt64
			pOpens          sql.NullInt64
			pClicks         sql.NullInt64
			pBounces        sql.NullInt64
		)
		if err := profileRows.Scan(&engagementScore, &bestSendHour, &pSent, &pOpens, &pClicks, &pBounces); err != nil {
			log.Printf("ERROR: failed to scan profile row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to process profile data")
			return
		}

		totalProfiles++
		if engagementScore.Valid {
			totalEngagement += engagementScore.Float64
		}
		if bestSendHour.Valid {
			h := int(bestSendHour.Int64)
			eng := 0.0
			if engagementScore.Valid {
				eng = engagementScore.Float64
			}
			hourCounts[h] += eng
			hourOccurrences[h]++
		}
		if pSent.Valid {
			profileTotalSent += pSent.Int64
		}
		if pOpens.Valid {
			profileTotalOpens += pOpens.Int64
		}
	}

	if err := profileRows.Err(); err != nil {
		log.Printf("ERROR: error iterating profiles: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to process profile data")
		return
	}

	// Compute optimal hours (top 3 by average engagement)
	type hourEng struct {
		Hour       int
		AvgEngage  float64
	}
	var hourEngList []hourEng
	for h, totalEng := range hourCounts {
		count := hourOccurrences[h]
		if count > 0 {
			hourEngList = append(hourEngList, hourEng{Hour: h, AvgEngage: totalEng / float64(count)})
		}
	}
	// Sort descending by average engagement
	for i := 0; i < len(hourEngList); i++ {
		for j := i + 1; j < len(hourEngList); j++ {
			if hourEngList[j].AvgEngage > hourEngList[i].AvgEngage {
				hourEngList[i], hourEngList[j] = hourEngList[j], hourEngList[i]
			}
		}
	}
	optimalHours := make([]int, 0, 3)
	for i := 0; i < len(hourEngList) && i < 3; i++ {
		optimalHours = append(optimalHours, hourEngList[i].Hour)
	}

	// Compute avg engagement
	avgEngagement := 0.0
	if totalProfiles > 0 {
		avgEngagement = totalEngagement / float64(totalProfiles)
	}

	// Compute bounce rate and complaint rate from agent-level stats
	bounceRate := 0.0
	complaintRate := 0.0
	if totalSends > 0 {
		bounceRate = float64(totalBounces) / float64(totalSends)
		complaintRate = float64(totalComplaints) / float64(totalSends)
	}

	// Determine risk level
	combinedRate := bounceRate + complaintRate
	riskLevel := "low"
	if combinedRate > 0.05 {
		riskLevel = "high"
	} else if combinedRate > 0.02 {
		riskLevel = "medium"
	}

	// Content preferences (placeholder values)
	contentPrefs := map[string]interface{}{
		"text_open_rate": 0.12,
		"html_open_rate": 0.18,
	}

	now := time.Now()

	knowledge := map[string]interface{}{
		"optimal_hours":       optimalHours,
		"avg_engagement":      avgEngagement,
		"total_profiles":      totalProfiles,
		"bounce_rate":         bounceRate,
		"complaint_rate":      complaintRate,
		"content_preferences": contentPrefs,
		"risk_level":          riskLevel,
		"last_learned_at":     now,
	}

	knowledgeJSON, err := json.Marshal(knowledge)
	if err != nil {
		log.Printf("ERROR: failed to marshal knowledge: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to process learning data")
		return
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE mailing_isp_agents SET knowledge = $1, updated_at = NOW(), last_active_at = NOW() WHERE id = $2`,
		knowledgeJSON, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to update agent knowledge: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to save learning results")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "learning cycle complete",
		"knowledge": knowledge,
	})
}

// ============================================================================
// Handler 6: HandleAgentDecisions
// GET /isp-agents/managed/{agentId}/decisions
// ============================================================================

func (m *ISPAgentManager) HandleAgentDecisions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Verify agent exists
	var exists bool
	err = m.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM mailing_isp_agents WHERE id = $1)`, agentID,
	).Scan(&exists)
	if err != nil {
		log.Printf("ERROR: failed to check agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to verify agent")
		return
	}
	if !exists {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Fetch recent campaigns with decisions
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, campaign_id, decisions, created_at
		FROM mailing_agent_campaigns WHERE agent_id = $1 AND decisions IS NOT NULL
		ORDER BY created_at DESC LIMIT 50`, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to fetch decisions for agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve decision data")
		return
	}
	defer rows.Close()

	var allDecisions []map[string]interface{}

	for rows.Next() {
		var (
			campID     uuid.UUID
			campaignID uuid.UUID
			decisionsB []byte
			createdAt  time.Time
		)
		if err := rows.Scan(&campID, &campaignID, &decisionsB, &createdAt); err != nil {
			log.Printf("ERROR: failed to scan decision row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve decision data")
			return
		}

		if decisionsB == nil {
			continue
		}

		// Parse the decisions JSONB array
		var decisions []map[string]interface{}
		if err := json.Unmarshal(decisionsB, &decisions); err != nil {
			// If not an array, try as single object
			var single map[string]interface{}
			if err2 := json.Unmarshal(decisionsB, &single); err2 == nil {
				decisions = []map[string]interface{}{single}
			} else {
				continue
			}
		}

		// Annotate each decision with campaign context
		for _, d := range decisions {
			d["agent_campaign_id"] = campID
			d["campaign_id"] = campaignID
			d["campaign_created_at"] = createdAt
			allDecisions = append(allDecisions, d)
		}
	}

	if err := rows.Err(); err != nil {
		log.Printf("ERROR: error iterating decisions: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve decision data")
		return
	}

	if allDecisions == nil {
		allDecisions = []map[string]interface{}{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"decisions": allDecisions,
		"total":     len(allDecisions),
	})
}

// ============================================================================
// Handler 7: HandleDeleteAgent
// DELETE /isp-agents/managed/{agentId}
// ============================================================================

func (m *ISPAgentManager) HandleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Check for active campaigns
	var activeCampaigns int
	err = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_agent_campaigns WHERE agent_id = $1 AND status IN ('pending', 'active')`,
		agentID,
	).Scan(&activeCampaigns)
	if err != nil {
		log.Printf("ERROR: failed to check active campaigns: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to verify agent campaigns")
		return
	}
	if activeCampaigns > 0 {
		respondError(w, http.StatusConflict, "cannot delete agent with active campaigns")
		return
	}

	result, err := m.db.ExecContext(ctx,
		`DELETE FROM mailing_isp_agents WHERE id = $1`, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to delete agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete agent")
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message": "agent deleted",
	})
}

// ============================================================================
// Handler 8: HandleAgentSummary
// GET /isp-agents/summary
// ============================================================================

func (m *ISPAgentManager) HandleAgentSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Aggregate counts by status
	var totalAgents, activeAgents, dormantAgents, learningAgents int
	rows, err := m.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM mailing_isp_agents GROUP BY status`,
	)
	if err != nil {
		log.Printf("ERROR: failed to query agent summary: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent summary")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var s string
		var c int
		if err := rows.Scan(&s, &c); err != nil {
			log.Printf("ERROR: failed to scan summary row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve agent summary")
			return
		}
		totalAgents += c
		switch s {
		case "sending", "adapting":
			activeAgents += c
		case "dormant", "complete":
			dormantAgents += c
		case "learning":
			learningAgents += c
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("ERROR: error iterating summary: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent summary")
		return
	}

	// Aggregate sends, opens, clicks, avg_engagement
	var totalSends, totalOpens, totalClicks sql.NullInt64
	var avgEngagement sql.NullFloat64
	err = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_sends), 0), COALESCE(SUM(total_opens), 0),
			COALESCE(SUM(total_clicks), 0), AVG(avg_engagement)
		FROM mailing_isp_agents`,
	).Scan(&totalSends, &totalOpens, &totalClicks, &avgEngagement)
	if err != nil {
		log.Printf("ERROR: failed to aggregate agent metrics: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve agent metrics")
		return
	}

	// Top performing ISP
	var topISP sql.NullString
	var topAvg sql.NullFloat64
	err = m.db.QueryRowContext(ctx,
		`SELECT isp, avg_engagement FROM mailing_isp_agents
		WHERE avg_engagement IS NOT NULL
		ORDER BY avg_engagement DESC LIMIT 1`,
	).Scan(&topISP, &topAvg)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("ERROR: failed to find top ISP: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve ISP data")
		return
	}

	topPerformingISP := ""
	if topISP.Valid {
		topPerformingISP = topISP.String
	}
	avgEng := 0.0
	if avgEngagement.Valid {
		avgEng = avgEngagement.Float64
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"summary": map[string]interface{}{
			"total_agents":      totalAgents,
			"active_agents":     activeAgents,
			"dormant_agents":    dormantAgents,
			"learning_agents":   learningAgents,
			"total_sends":       totalSends.Int64,
			"total_opens":       totalOpens.Int64,
			"total_clicks":      totalClicks.Int64,
			"avg_engagement":    avgEng,
			"top_performing_isp": topPerformingISP,
		},
	})
}

// ============================================================================
// Handler 9: HandleAgentFeed
// GET /isp-agents/managed/{agentId}/feed
// ============================================================================

func (m *ISPAgentManager) HandleAgentFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Verify agent exists
	var exists bool
	err = m.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM mailing_isp_agents WHERE id = $1)`, agentID,
	).Scan(&exists)
	if err != nil {
		log.Printf("ERROR: failed to check agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to verify agent")
		return
	}
	if !exists {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Parse query params
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 200 {
		limit = 200
	}

	campaignIDFilter := r.URL.Query().Get("campaign_id")
	classificationFilter := r.URL.Query().Get("classification")

	// Build dynamic query
	baseQuery := `SELECT id, campaign_id, email_hash, classification, content_strategy,
		priority, reasoning, executed, executed_at, result, created_at
		FROM mailing_agent_send_decisions`
	countQuery := `SELECT COUNT(*) FROM mailing_agent_send_decisions`

	conditions := []string{"agent_id = $1"}
	var args []interface{}
	args = append(args, agentID)
	argIdx := 2

	if campaignIDFilter != "" {
		campaignUUID, err := uuid.Parse(campaignIDFilter)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid campaign_id filter")
			return
		}
		conditions = append(conditions, "campaign_id = $"+argN(argIdx))
		args = append(args, campaignUUID)
		argIdx++
	}

	if classificationFilter != "" {
		conditions = append(conditions, "classification = $"+argN(argIdx))
		args = append(args, classificationFilter)
		argIdx++
	}

	whereClause := " WHERE " + strings.Join(conditions, " AND ")

	// Get total count
	var total int
	err = m.db.QueryRowContext(ctx, countQuery+whereClause, args...).Scan(&total)
	if err != nil {
		log.Printf("ERROR: failed to count decisions: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve feed data")
		return
	}

	// Get feed items
	feedQuery := baseQuery + whereClause + " ORDER BY created_at DESC LIMIT $" + argN(argIdx)
	args = append(args, limit)

	rows, err := m.db.QueryContext(ctx, feedQuery, args...)
	if err != nil {
		log.Printf("ERROR: failed to query feed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve feed data")
		return
	}
	defer rows.Close()

	type feedItem struct {
		ID              uuid.UUID       `json:"id"`
		CampaignID      uuid.UUID       `json:"campaign_id"`
		EmailHashShort  string          `json:"email_hash_short"`
		Classification  string          `json:"classification"`
		ContentStrategy string          `json:"content_strategy"`
		Priority        int             `json:"priority"`
		Reasoning       json.RawMessage `json:"reasoning"`
		Executed        bool            `json:"executed"`
		ExecutedAt      *time.Time      `json:"executed_at"`
		Result          *string         `json:"result"`
		CreatedAt       time.Time       `json:"created_at"`
	}

	var feed []feedItem

	for rows.Next() {
		var item feedItem
		var emailHash string
		var contentStrategy sql.NullString
		var priority sql.NullInt64
		var reasoningBytes []byte
		var executedAt sql.NullTime
		var result sql.NullString

		err := rows.Scan(
			&item.ID, &item.CampaignID, &emailHash, &item.Classification,
			&contentStrategy, &priority,
			&reasoningBytes, &item.Executed, &executedAt, &result, &item.CreatedAt,
		)
		if err != nil {
			log.Printf("ERROR: failed to scan feed item: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve feed data")
			return
		}

		// Truncate email hash for privacy (first 8 characters)
		if len(emailHash) >= 8 {
			item.EmailHashShort = emailHash[:8]
		} else {
			item.EmailHashShort = emailHash
		}
		if contentStrategy.Valid {
			item.ContentStrategy = contentStrategy.String
		}
		if priority.Valid {
			item.Priority = int(priority.Int64)
		}
		if reasoningBytes != nil {
			item.Reasoning = json.RawMessage(reasoningBytes)
		} else {
			item.Reasoning = json.RawMessage(`{}`)
		}
		if executedAt.Valid {
			item.ExecutedAt = &executedAt.Time
		}
		if result.Valid {
			item.Result = &result.String
		}

		feed = append(feed, item)
	}

	if err := rows.Err(); err != nil {
		log.Printf("ERROR: error iterating feed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve feed data")
		return
	}

	if feed == nil {
		feed = []feedItem{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"feed":  feed,
		"total": total,
		"limit": limit,
	})
}

// ============================================================================
// Handler 10: HandleAgentActivity
// GET /isp-agents/managed/{agentId}/activity
// ============================================================================

func (m *ISPAgentManager) HandleAgentActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	// Verify agent exists
	var exists bool
	err = m.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM mailing_isp_agents WHERE id = $1)`, agentID,
	).Scan(&exists)
	if err != nil {
		log.Printf("ERROR: failed to check agent %s: %v", agentID, err)
		respondError(w, http.StatusInternalServerError, "Failed to verify agent")
		return
	}
	if !exists {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	// 1. Classification counts
	byClassification := map[string]int64{}
	var totalDecisions int64

	classRows, err := m.db.QueryContext(ctx,
		`SELECT classification, COUNT(*) FROM mailing_agent_send_decisions WHERE agent_id = $1 GROUP BY classification`,
		agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to query classification counts: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}
	defer classRows.Close()

	for classRows.Next() {
		var classification string
		var count int64
		if err := classRows.Scan(&classification, &count); err != nil {
			log.Printf("ERROR: failed to scan classification: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
			return
		}
		byClassification[classification] = count
		totalDecisions += count
	}
	if err := classRows.Err(); err != nil {
		log.Printf("ERROR: error iterating classifications: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	// 2. Result counts (executed = true) + pending count (executed = false)
	byResult := map[string]int64{}
	var totalExecuted int64

	resultRows, err := m.db.QueryContext(ctx,
		`SELECT result, COUNT(*) FROM mailing_agent_send_decisions WHERE agent_id = $1 AND executed = true GROUP BY result`,
		agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to query result counts: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}
	defer resultRows.Close()

	for resultRows.Next() {
		var result sql.NullString
		var count int64
		if err := resultRows.Scan(&result, &count); err != nil {
			log.Printf("ERROR: failed to scan result row: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
			return
		}
		if result.Valid {
			byResult[result.String] = count
		} else {
			byResult["unknown"] = count
		}
		totalExecuted += count
	}
	if err := resultRows.Err(); err != nil {
		log.Printf("ERROR: error iterating results: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	// Pending count (executed = false)
	var pendingCount int64
	err = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_agent_send_decisions WHERE agent_id = $1 AND executed = false`,
		agentID,
	).Scan(&pendingCount)
	if err != nil {
		log.Printf("ERROR: failed to count pending decisions: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}
	byResult["pending"] = pendingCount

	// 3. Content strategy counts
	byContentStrategy := map[string]int64{}

	csRows, err := m.db.QueryContext(ctx,
		`SELECT content_strategy, COUNT(*) FROM mailing_agent_send_decisions WHERE agent_id = $1 GROUP BY content_strategy`,
		agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to query content strategy counts: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}
	defer csRows.Close()

	for csRows.Next() {
		var cs sql.NullString
		var count int64
		if err := csRows.Scan(&cs, &count); err != nil {
			log.Printf("ERROR: failed to scan content strategy: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
			return
		}
		if cs.Valid {
			byContentStrategy[cs.String] = count
		}
	}
	if err := csRows.Err(); err != nil {
		log.Printf("ERROR: error iterating content strategies: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	// 4. Active campaigns count (distinct campaign_id where not yet executed)
	var campaignsActive int
	err = m.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT campaign_id) FROM mailing_agent_send_decisions WHERE agent_id = $1 AND executed = false`,
		agentID,
	).Scan(&campaignsActive)
	if err != nil {
		log.Printf("ERROR: failed to count active campaigns: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	// 5. Latest activity timestamp
	var latestActivity sql.NullTime
	err = m.db.QueryRowContext(ctx,
		`SELECT MAX(COALESCE(executed_at, created_at)) FROM mailing_agent_send_decisions WHERE agent_id = $1`,
		agentID,
	).Scan(&latestActivity)
	if err != nil {
		log.Printf("ERROR: failed to get latest activity: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	// 6. Recent feed (last 20 decisions)
	feedRows, err := m.db.QueryContext(ctx,
		`SELECT id, campaign_id, email_hash, classification, content_strategy,
			priority, reasoning, executed, executed_at, result, created_at
		FROM mailing_agent_send_decisions WHERE agent_id = $1
		ORDER BY created_at DESC LIMIT 20`, agentID,
	)
	if err != nil {
		log.Printf("ERROR: failed to query recent feed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}
	defer feedRows.Close()

	var recentFeed []map[string]interface{}
	for feedRows.Next() {
		var (
			id              uuid.UUID
			campaignID      uuid.UUID
			emailHash       string
			classification  string
			contentStrategy sql.NullString
			priority        sql.NullInt64
			reasoningBytes  []byte
			executed        bool
			executedAt      sql.NullTime
			result          sql.NullString
			createdAt       time.Time
		)
		if err := feedRows.Scan(
			&id, &campaignID, &emailHash, &classification,
			&contentStrategy, &priority,
			&reasoningBytes, &executed, &executedAt, &result, &createdAt,
		); err != nil {
			log.Printf("ERROR: failed to scan recent feed item: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
			return
		}

		item := map[string]interface{}{
			"id":             id,
			"campaign_id":    campaignID,
			"classification": classification,
			"executed":       executed,
			"created_at":     createdAt,
		}

		if len(emailHash) >= 8 {
			item["email_hash_short"] = emailHash[:8]
		} else {
			item["email_hash_short"] = emailHash
		}
		if contentStrategy.Valid {
			item["content_strategy"] = contentStrategy.String
		}
		if priority.Valid {
			item["priority"] = int(priority.Int64)
		}
		if reasoningBytes != nil {
			item["reasoning"] = json.RawMessage(reasoningBytes)
		} else {
			item["reasoning"] = json.RawMessage(`{}`)
		}
		if executedAt.Valid {
			item["executed_at"] = executedAt.Time
		}
		if result.Valid {
			item["result"] = result.String
		}

		recentFeed = append(recentFeed, item)
	}
	if err := feedRows.Err(); err != nil {
		log.Printf("ERROR: error iterating recent feed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve activity data")
		return
	}

	if recentFeed == nil {
		recentFeed = []map[string]interface{}{}
	}

	// Compute execution rate
	executionRate := 0.0
	if totalDecisions > 0 {
		executionRate = float64(totalExecuted) / float64(totalDecisions) * 100.0
	}

	response := map[string]interface{}{
		"agent_id":            agentID,
		"total_decisions":     totalDecisions,
		"by_classification":   byClassification,
		"by_result":           byResult,
		"by_content_strategy": byContentStrategy,
		"execution_rate":      executionRate,
		"campaigns_active":    campaignsActive,
		"recent_feed":         recentFeed,
	}

	if latestActivity.Valid {
		response["latest_activity_at"] = latestActivity.Time
	} else {
		response["latest_activity_at"] = nil
	}

	respondJSON(w, http.StatusOK, response)
}

// ============================================================================
// RegisterISPAgentRoutes registers all ISP agent management routes
// ============================================================================

func RegisterISPAgentRoutes(r chi.Router, db *sql.DB) {
	m := &ISPAgentManager{db: db}

	r.Get("/isp-agents/managed", m.HandleListAgents)
	r.Get("/isp-agents/summary", m.HandleAgentSummary)
	r.Get("/isp-agents/managed/{agentId}", m.HandleGetAgent)
	r.Patch("/isp-agents/managed/{agentId}/status", m.HandleUpdateAgentStatus)
	r.Patch("/isp-agents/managed/{agentId}/config", m.HandleUpdateAgentConfig)
	r.Post("/isp-agents/managed/{agentId}/learn", m.HandleAgentLearn)
	r.Get("/isp-agents/managed/{agentId}/decisions", m.HandleAgentDecisions)
	r.Get("/isp-agents/managed/{agentId}/feed", m.HandleAgentFeed)
	r.Get("/isp-agents/managed/{agentId}/activity", m.HandleAgentActivity)
	r.Delete("/isp-agents/managed/{agentId}", m.HandleDeleteAgent)
}

// ============================================================================
// Helpers for dynamic query building
// ============================================================================

func statusArg(idx int) string {
	return "status = $" + argN(idx)
}

func ispArg(idx int) string {
	return "isp = $" + argN(idx)
}

func argN(n int) string {
	// Convert int to string without importing strconv
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

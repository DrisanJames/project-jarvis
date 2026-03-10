package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleGetForecast returns calendar data for a month/domain showing recommendations grouped by date.
func (a *EmailMarketingAgent) HandleGetForecast(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	month := r.URL.Query().Get("month")
	domain := r.URL.Query().Get("sending_domain")

	if month == "" {
		month = time.Now().Format("2006-01")
	}
	startDate := month + "-01"
	t, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid month format, use YYYY-MM"})
		return
	}
	endDate := t.AddDate(0, 1, -1).Format("2006-01-02")

	q := `SELECT id::text, sending_domain, scheduled_date, scheduled_time,
	             COALESCE(campaign_name,''), COALESCE(reasoning,''),
	             COALESCE(strategy,''), projected_volume, status,
	             campaign_config::text, created_at
	      FROM agent_campaign_recommendations
	      WHERE organization_id = $1 AND scheduled_date >= $2 AND scheduled_date <= $3`
	qArgs := []interface{}{orgID, startDate, endDate}
	if domain != "" {
		q += ` AND sending_domain = $4`
		qArgs = append(qArgs, domain)
	}
	q += ` ORDER BY scheduled_date, scheduled_time`

	rows, err := a.db.QueryContext(r.Context(), q, qArgs...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type recEntry struct {
		ID              string                 `json:"id"`
		SendingDomain   string                 `json:"sending_domain"`
		ScheduledDate   string                 `json:"scheduled_date"`
		ScheduledTime   string                 `json:"scheduled_time,omitempty"`
		CampaignName    string                 `json:"campaign_name"`
		Reasoning       string                 `json:"reasoning"`
		Strategy        string                 `json:"strategy"`
		ProjectedVolume int                    `json:"projected_volume"`
		Status          string                 `json:"status"`
		CampaignConfig  map[string]interface{} `json:"campaign_config,omitempty"`
	}

	dayMap := map[string][]recEntry{}
	totalVolume := 0
	for rows.Next() {
		var rec recEntry
		var scheduledDate time.Time
		var scheduledTime sql.NullString
		var configJSON string
		var createdAt time.Time
		rows.Scan(&rec.ID, &rec.SendingDomain, &scheduledDate, &scheduledTime,
			&rec.CampaignName, &rec.Reasoning, &rec.Strategy, &rec.ProjectedVolume,
			&rec.Status, &configJSON, &createdAt)
		rec.ScheduledDate = scheduledDate.Format("2006-01-02")
		if scheduledTime.Valid {
			rec.ScheduledTime = scheduledTime.String
		}
		if configJSON != "" {
			json.Unmarshal([]byte(configJSON), &rec.CampaignConfig)
		}
		dayMap[rec.ScheduledDate] = append(dayMap[rec.ScheduledDate], rec)
		totalVolume += rec.ProjectedVolume
	}

	type dayEntry struct {
		Date            string     `json:"date"`
		ProjectedVolume int        `json:"projected_volume"`
		Recommendations []recEntry `json:"recommendations"`
	}
	var days []dayEntry
	current := t
	end, _ := time.Parse("2006-01-02", endDate)
	for !current.After(end) {
		dateStr := current.Format("2006-01-02")
		recs := dayMap[dateStr]
		if recs == nil {
			recs = []recEntry{}
		}
		dayVol := 0
		for _, r := range recs {
			dayVol += r.ProjectedVolume
		}
		days = append(days, dayEntry{Date: dateStr, ProjectedVolume: dayVol, Recommendations: recs})
		current = current.AddDate(0, 0, 1)
	}

	// Lookup strategy for the domain
	strategyName := ""
	if domain != "" {
		a.db.QueryRowContext(r.Context(),
			`SELECT strategy FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
			orgID, domain).Scan(&strategyName)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"sending_domain": domain,
		"strategy":       strategyName,
		"month":          month,
		"days":           days,
		"summary": map[string]interface{}{
			"total_projected_volume":   totalVolume,
			"days_with_recommendations": len(dayMap),
		},
	})
}

// HandleListRecommendations lists recommendations with filters.
func (a *EmailMarketingAgent) HandleListRecommendations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	status := r.URL.Query().Get("status")
	domain := r.URL.Query().Get("sending_domain")

	q := `SELECT id::text, sending_domain, scheduled_date, scheduled_time,
	             COALESCE(campaign_name,''), COALESCE(reasoning,''),
	             COALESCE(strategy,''), projected_volume, status,
	             approved_at::text, executed_campaign_id::text, created_at
	      FROM agent_campaign_recommendations WHERE organization_id = $1`
	qArgs := []interface{}{orgID}
	idx := 2
	if status != "" {
		q += fmt.Sprintf(` AND status = $%d`, idx)
		qArgs = append(qArgs, status)
		idx++
	}
	if domain != "" {
		q += fmt.Sprintf(` AND sending_domain = $%d`, idx)
		qArgs = append(qArgs, domain)
	}
	q += ` ORDER BY scheduled_date DESC LIMIT 100`

	rows, err := a.db.QueryContext(r.Context(), q, qArgs...)
	if err != nil {
		log.Printf("[MarketingAgent] list recommendations: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type recSummary struct {
		ID                 string  `json:"id"`
		SendingDomain      string  `json:"sending_domain"`
		ScheduledDate      string  `json:"scheduled_date"`
		ScheduledTime      *string `json:"scheduled_time,omitempty"`
		CampaignName       string  `json:"campaign_name"`
		Reasoning          string  `json:"reasoning"`
		Strategy           string  `json:"strategy"`
		ProjectedVolume    int     `json:"projected_volume"`
		Status             string  `json:"status"`
		ApprovedAt         *string `json:"approved_at,omitempty"`
		ExecutedCampaignID *string `json:"executed_campaign_id,omitempty"`
	}
	var recs []recSummary
	for rows.Next() {
		var rec recSummary
		var date time.Time
		var schedTime, approvedAt sql.NullString
		var execCampaign sql.NullString
		var createdAt time.Time
		rows.Scan(&rec.ID, &rec.SendingDomain, &date, &schedTime,
			&rec.CampaignName, &rec.Reasoning, &rec.Strategy,
			&rec.ProjectedVolume, &rec.Status, &approvedAt, &execCampaign, &createdAt)
		rec.ScheduledDate = date.Format("2006-01-02")
		if schedTime.Valid {
			rec.ScheduledTime = &schedTime.String
		}
		if approvedAt.Valid {
			rec.ApprovedAt = &approvedAt.String
		}
		if execCampaign.Valid {
			rec.ExecutedCampaignID = &execCampaign.String
		}
		recs = append(recs, rec)
	}
	if recs == nil {
		recs = []recSummary{}
	}
	respondJSON(w, http.StatusOK, recs)
}

// HandleGetRecommendation returns full details for a single recommendation.
func (a *EmailMarketingAgent) HandleGetRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)

	var domain, name, reasoning, strategy, status, configJSON string
	var volume int
	var scheduledDate time.Time
	var scheduledTime, approvedAt, executedCampaign, executionError sql.NullString
	var createdAt time.Time

	err := a.db.QueryRowContext(r.Context(),
		`SELECT sending_domain, scheduled_date, scheduled_time, COALESCE(campaign_name,''),
		        COALESCE(reasoning,''), COALESCE(strategy,''), projected_volume, status,
		        campaign_config::text, approved_at::text, executed_campaign_id::text,
		        COALESCE(execution_error,''), created_at
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&domain, &scheduledDate, &scheduledTime, &name,
		&reasoning, &strategy, &volume, &status, &configJSON,
		&approvedAt, &executedCampaign, &executionError, &createdAt)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}

	result := map[string]interface{}{
		"id": recID, "sending_domain": domain,
		"scheduled_date": scheduledDate.Format("2006-01-02"),
		"campaign_name": name, "reasoning": reasoning,
		"strategy": strategy, "projected_volume": volume,
		"status": status, "created_at": createdAt.Format(time.RFC3339),
	}
	if scheduledTime.Valid {
		result["scheduled_time"] = scheduledTime.String
	}
	if approvedAt.Valid {
		result["approved_at"] = approvedAt.String
	}
	if executedCampaign.Valid {
		result["executed_campaign_id"] = executedCampaign.String
	}
	if executionError.Valid && executionError.String != "" {
		result["execution_error"] = executionError.String
	}
	if configJSON != "" {
		var cfg map[string]interface{}
		json.Unmarshal([]byte(configJSON), &cfg)
		result["campaign_config"] = cfg
	}
	respondJSON(w, http.StatusOK, result)
}

// HandleApproveRecommendation moves a recommendation to 'approved' status.
func (a *EmailMarketingAgent) HandleApproveRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)

	var status string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT status FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}
	if status != "pending" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "can only approve pending recommendations, current status: " + status})
		return
	}
	_, err = a.db.ExecContext(r.Context(),
		`UPDATE agent_campaign_recommendations SET status = 'approved', approved_at = NOW(), updated_at = NOW() WHERE id = $1`,
		recID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "approved", "id": recID})
}

// HandleRejectRecommendation moves a recommendation to 'rejected' status.
func (a *EmailMarketingAgent) HandleRejectRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)

	result, err := a.db.ExecContext(r.Context(),
		`UPDATE agent_campaign_recommendations SET status = 'rejected', updated_at = NOW()
		 WHERE id = $1 AND organization_id = $2 AND status = 'pending'`, recID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "recommendation not found or not in pending status"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "rejected", "id": recID})
}

// HandleGenerateForecast generates campaign recommendations for a month based on the domain strategy.
func (a *EmailMarketingAgent) HandleGenerateForecast(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var input struct {
		SendingDomain   string `json:"sending_domain"`
		Month           string `json:"month"`
		ForceRegenerate bool   `json:"force_regenerate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if input.SendingDomain == "" || input.Month == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain and month are required"})
		return
	}

	startDate := input.Month + "-01"
	t, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid month format"})
		return
	}

	// Load strategy
	var strategy string
	var paramsJSON sql.NullString
	err = a.db.QueryRowContext(r.Context(),
		`SELECT strategy, params::text FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
		orgID, input.SendingDomain).Scan(&strategy, &paramsJSON)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "no strategy configured for " + input.SendingDomain + ". Set one up first."})
		return
	}

	var params map[string]interface{}
	if paramsJSON.Valid {
		json.Unmarshal([]byte(paramsJSON.String), &params)
	}

	// Get current quotas as baseline
	currentQuotas := map[string]int{}
	quotaRows, _ := a.db.QueryContext(r.Context(), `
		SELECT p.isp, p.quota FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON p.campaign_id = c.id
		WHERE c.organization_id::text = $1
		  AND c.status IN ('completed','sent','cancelled','completed_with_errors','sending')
		ORDER BY COALESCE(c.completed_at, c.started_at, c.created_at) DESC
		LIMIT 100`, orgID)
	if quotaRows != nil {
		defer quotaRows.Close()
		for quotaRows.Next() {
			var isp string
			var quota int
			quotaRows.Scan(&isp, &quota)
			if _, seen := currentQuotas[isp]; !seen {
				currentQuotas[isp] = quota
			}
		}
	}

	increasePct := 10.0
	if v, ok := params["daily_volume_increase_pct"].(float64); ok && v > 0 {
		increasePct = v
	}

	// Clear existing pending recommendations for this month if regenerating
	if input.ForceRegenerate {
		a.db.ExecContext(r.Context(),
			`DELETE FROM agent_campaign_recommendations
			 WHERE organization_id = $1 AND sending_domain = $2
			   AND scheduled_date >= $3 AND scheduled_date <= $4 AND status = 'pending'`,
			orgID, input.SendingDomain, startDate, t.AddDate(0, 1, -1).Format("2006-01-02"))
	}

	// Generate daily recommendations for weekdays
	created := 0
	endDate := t.AddDate(0, 1, -1)
	current := t
	if current.Before(time.Now()) {
		current = time.Now().Truncate(24 * time.Hour).AddDate(0, 0, 1)
	}
	dayIndex := 0
	for !current.After(endDate) {
		weekday := current.Weekday()
		if weekday == time.Saturday || weekday == time.Sunday {
			current = current.AddDate(0, 0, 1)
			continue
		}

		dailyQuotas := map[string]interface{}{}
		totalVolume := 0
		multiplier := 1.0 + (increasePct/100.0)*float64(dayIndex)
		for isp, base := range currentQuotas {
			adjusted := int(float64(base) * multiplier)
			dailyQuotas[isp] = adjusted
			totalVolume += adjusted
		}

		name := input.SendingDomain + " — " + current.Format("Jan 2")
		configJSON, _ := json.Marshal(map[string]interface{}{
			"sending_domain": input.SendingDomain,
			"isp_quotas":     dailyQuotas,
			"name":           name,
			"scheduled_date": current.Format("2006-01-02"),
			"scheduled_time": "13:00",
		})

		_, err := a.db.ExecContext(r.Context(),
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time,
			  campaign_name, campaign_config, reasoning, strategy, projected_volume, status)
			 VALUES ($1, $2, $3, '13:00', $4, $5, $6, $7, $8, 'pending')`,
			orgID, input.SendingDomain, current.Format("2006-01-02"),
			name, string(configJSON),
			fmt.Sprintf("Auto-generated forecast based on %s strategy with %.0f%% daily volume increase", strategy, increasePct),
			strategy, totalVolume)
		if err != nil {
			log.Printf("[MarketingAgent] forecast insert error: %v", err)
		} else {
			created++
		}

		dayIndex++
		current = current.AddDate(0, 0, 1)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":               "generated",
		"recommendations_created": created,
		"sending_domain":       input.SendingDomain,
		"strategy":             strategy,
		"month":                input.Month,
	})
}

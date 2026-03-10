package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
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

// HandleApproveRecommendation deploys the recommendation as a real scheduled campaign
// through the existing PMTA campaign pipeline — identical to deploying from Campaign Manager.
func (a *EmailMarketingAgent) HandleApproveRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	ctx := r.Context()

	var status, configJSON, campaignName, recStrategy, sendingDomain string
	var scheduledDate time.Time
	var scheduledTime sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT status, campaign_config::text, COALESCE(campaign_name,''), COALESCE(strategy,''),
		        sending_domain, scheduled_date, scheduled_time
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &configJSON, &campaignName, &recStrategy,
		&sendingDomain, &scheduledDate, &scheduledTime)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}
	if status != "pending" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "can only approve pending recommendations, current status: " + status})
		return
	}

	var cfg map[string]interface{}
	if configJSON != "" {
		json.Unmarshal([]byte(configJSON), &cfg)
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}

	// Load template HTML (required — campaign needs content)
	templateID, _ := cfg["template_id"].(string)
	var htmlContent string
	if templateID != "" {
		a.db.QueryRowContext(ctx,
			`SELECT COALESCE(html_content,'') FROM mailing_templates WHERE id = $1 AND organization_id = $2`,
			templateID, orgID).Scan(&htmlContent)
	}
	if strings.TrimSpace(htmlContent) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot approve: no email template assigned or template has no HTML content. Assign a template first."})
		return
	}

	subject, _ := cfg["subject"].(string)
	fromName, _ := cfg["from_name"].(string)
	previewText, _ := cfg["preview_text"].(string)
	if subject == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot approve: subject line is required"})
		return
	}
	if fromName == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot approve: from name is required"})
		return
	}

	// Build scheduled time
	timeStr := "13:00"
	if scheduledTime.Valid && scheduledTime.String != "" {
		timeStr = scheduledTime.String
	}
	if t, ok := cfg["scheduled_time"].(string); ok && t != "" {
		timeStr = t
	}
	if !strings.Contains(timeStr, ":") {
		timeStr = "13:00"
	}
	parts := strings.Split(timeStr, ":")
	hour, minute := 13, 0
	if len(parts) >= 2 {
		fmt.Sscanf(parts[0], "%d", &hour)
		fmt.Sscanf(parts[1], "%d", &minute)
	}
	schedAt := time.Date(scheduledDate.Year(), scheduledDate.Month(), scheduledDate.Day(), hour, minute, 0, 0, time.UTC)
	if schedAt.Before(time.Now().Add(2 * time.Minute)) {
		schedAt = time.Now().Add(5 * time.Minute)
	}

	// Build ISP quotas and target ISPs from config
	var targetISPs []engine.ISP
	var ispQuotas []engine.ISPQuota
	var ispPlans []engine.PMTAISPScheduleInput
	waveInterval := 15
	if wi, ok := cfg["wave_interval_minutes"].(float64); ok && int(wi) > 0 {
		waveInterval = int(wi)
	}
	if quotas, ok := cfg["isp_quotas"].(map[string]interface{}); ok {
		for isp, v := range quotas {
			vol := 0
			switch n := v.(type) {
			case float64:
				vol = int(n)
			case int:
				vol = n
			}
			if vol <= 0 {
				continue
			}
			targetISPs = append(targetISPs, engine.ISP(isp))
			ispQuotas = append(ispQuotas, engine.ISPQuota{ISP: isp, Volume: vol})
			ispPlans = append(ispPlans, engine.PMTAISPScheduleInput{
				ISP:               isp,
				Quota:             vol,
				RandomizeAudience: false,
				ThrottleStrategy:  "auto",
				Timezone:          "UTC",
				Cadence: engine.PMTACadenceInput{
					Mode:         "interval",
					EveryMinutes: waveInterval,
					BatchSize:    0,
				},
				TimeSpans: []engine.PMTATimeSpanInput{{
					Type:    "absolute",
					StartAt: &schedAt,
					EndAt:   func() *time.Time { t := schedAt.Add(4 * time.Hour); return &t }(),
				}},
			})
		}
	}
	if len(targetISPs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot approve: no ISP quotas configured"})
		return
	}

	// Build inclusion lists (extract IDs from the config objects)
	var inclusionListIDs []string
	var sendPriority []engine.PriorityItem
	if lists, ok := cfg["inclusion_lists"].([]interface{}); ok {
		for _, item := range lists {
			switch v := item.(type) {
			case map[string]interface{}:
				if id, ok := v["id"].(string); ok && id != "" {
					inclusionListIDs = append(inclusionListIDs, id)
					sendPriority = append(sendPriority, engine.PriorityItem{ID: id, Type: "list"})
				}
			case string:
				if v != "" {
					inclusionListIDs = append(inclusionListIDs, v)
					sendPriority = append(sendPriority, engine.PriorityItem{ID: v, Type: "list"})
				}
			}
		}
	}
	if len(inclusionListIDs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot approve: no inclusion lists configured — add at least one mailing list"})
		return
	}

	var exclusionListIDs []string
	if lists, ok := cfg["exclusion_lists"].([]interface{}); ok {
		for _, item := range lists {
			switch v := item.(type) {
			case map[string]interface{}:
				if id, ok := v["id"].(string); ok && id != "" {
					exclusionListIDs = append(exclusionListIDs, id)
				}
			case string:
				if v != "" {
					exclusionListIDs = append(exclusionListIDs, v)
				}
			}
		}
	}

	// Build the full PMTACampaignInput
	deployInput := engine.PMTACampaignInput{
		Name:          campaignName,
		TargetISPs:    targetISPs,
		SendingDomain: sendingDomain,
		Variants: []engine.ContentVariant{{
			VariantName:  "A",
			FromName:     fromName,
			Subject:      subject,
			PreviewText:  previewText,
			HTMLContent:  htmlContent,
			SplitPercent: 100,
		}},
		ISPPlans:          ispPlans,
		ISPQuotas:         ispQuotas,
		InclusionLists:    inclusionListIDs,
		SendPriority:      sendPriority,
		ExclusionLists:    exclusionListIDs,
		InclusionSegments: []string{},
		ExclusionSegments: []string{},
		SendDays:          []string{},
		SendHour:          hour,
		Timezone:          "UTC",
		ThrottleStrategy:  "auto",
		RandomizeAudience: false,
		SendMode:          "scheduled",
		ScheduledAt:       &schedAt,
	}

	// Normalize, plan audience, and create the real campaign
	normalized, normErr := normalizePMTACampaignInput(deployInput)
	if normErr != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaign normalization failed: " + normErr.Error()})
		return
	}

	audience, audErr := planPMTAAudience(ctx, a.db, orgID, deployInput, normalized, a.pmtaSvc.suppMatcher)
	if audErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "audience planning failed: " + audErr.Error()})
		return
	}

	tx, txErr := a.db.BeginTx(ctx, nil)
	if txErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": txErr.Error()})
		return
	}
	defer tx.Rollback()

	result, createErr := createPMTAWaveCampaign(ctx, tx, a.db, orgID, deployInput, normalized, audience, a.pmtaSvc.colCache)
	if createErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "campaign creation failed: " + createErr.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "commit failed: " + err.Error()})
		return
	}

	// Mark recommendation as approved and link to the deployed campaign
	a.db.ExecContext(ctx,
		`UPDATE agent_campaign_recommendations
		 SET status = 'approved', approved_at = NOW(), executed_campaign_id = $1::uuid, updated_at = NOW()
		 WHERE id = $2`,
		result.CampaignID, recID)

	log.Printf("[MarketingAgent] recommendation %s approved → campaign %s scheduled for %s", recID, result.CampaignID, schedAt.Format(time.RFC3339))

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "approved",
		"id":              recID,
		"campaign_id":     result.CampaignID,
		"campaign_name":   result.Name,
		"campaign_status": result.Status,
		"scheduled_at":    schedAt.Format(time.RFC3339),
		"total_audience":  result.TotalAudience,
		"target_isps":     result.TargetISPs,
		"isp_plans":       len(result.ISPPlans),
	})
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

// HandleUpdateRecommendation allows editing a pending recommendation's campaign config.
func (a *EmailMarketingAgent) HandleUpdateRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)

	var input struct {
		CampaignName       string                 `json:"campaign_name"`
		ScheduledDate      string                 `json:"scheduled_date"`
		ScheduledTime      string                 `json:"scheduled_time"`
		ISPQuotas          map[string]interface{} `json:"isp_quotas"`
		InclusionLists     []interface{}          `json:"inclusion_lists"`
		ExclusionLists     []interface{}          `json:"exclusion_lists"`
		TemplateID         string                 `json:"template_id"`
		Subject            string                 `json:"subject"`
		PreviewText        string                 `json:"preview_text"`
		FromName           string                 `json:"from_name"`
		FromEmail          string                 `json:"from_email"`
		WaveIntervalMin    int                    `json:"wave_interval_minutes"`
		ThrottlePerWave    int                    `json:"throttle_per_wave"`
		AudiencePriority   []interface{}          `json:"audience_priority"`
		Reasoning          string                 `json:"reasoning"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	var existingConfigJSON string
	var status string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT campaign_config::text, status FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&existingConfigJSON, &status)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}
	if status != "pending" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "can only edit pending recommendations"})
		return
	}

	cfg := map[string]interface{}{}
	if existingConfigJSON != "" {
		json.Unmarshal([]byte(existingConfigJSON), &cfg)
	}

	if input.CampaignName != "" {
		cfg["name"] = input.CampaignName
	}
	if input.ScheduledDate != "" {
		cfg["scheduled_date"] = input.ScheduledDate
	}
	if input.ScheduledTime != "" {
		cfg["scheduled_time"] = input.ScheduledTime
	}
	if input.ISPQuotas != nil {
		cfg["isp_quotas"] = input.ISPQuotas
	}
	if input.InclusionLists != nil {
		cfg["inclusion_lists"] = input.InclusionLists
	}
	if input.ExclusionLists != nil {
		cfg["exclusion_lists"] = input.ExclusionLists
	}
	if input.TemplateID != "" {
		cfg["template_id"] = input.TemplateID
	}
	if input.Subject != "" {
		cfg["subject"] = input.Subject
	}
	if input.PreviewText != "" {
		cfg["preview_text"] = input.PreviewText
	}
	if input.FromName != "" {
		cfg["from_name"] = input.FromName
	}
	if input.FromEmail != "" {
		cfg["from_email"] = input.FromEmail
	}
	if input.WaveIntervalMin != 0 {
		cfg["wave_interval_minutes"] = input.WaveIntervalMin
	}
	if input.ThrottlePerWave != 0 {
		cfg["throttle_per_wave"] = input.ThrottlePerWave
	}
	if input.AudiencePriority != nil {
		cfg["audience_priority"] = input.AudiencePriority
	}
	if input.Reasoning != "" {
		cfg["reasoning"] = input.Reasoning
	}

	projectedVolume := 0
	if quotas, ok := cfg["isp_quotas"].(map[string]interface{}); ok {
		for _, v := range quotas {
			switch n := v.(type) {
			case float64:
				projectedVolume += int(n)
			case int:
				projectedVolume += n
			case json.Number:
				if i, err := n.Int64(); err == nil {
					projectedVolume += int(i)
				}
			}
		}
	}

	updatedConfigBytes, _ := json.Marshal(cfg)

	campaignName := input.CampaignName
	if campaignName == "" {
		if n, ok := cfg["name"].(string); ok {
			campaignName = n
		}
	}
	scheduledDate := input.ScheduledDate
	if scheduledDate == "" {
		if d, ok := cfg["scheduled_date"].(string); ok {
			scheduledDate = d
		}
	}
	scheduledTime := input.ScheduledTime
	if scheduledTime == "" {
		if t, ok := cfg["scheduled_time"].(string); ok {
			scheduledTime = t
		}
	}
	reasoning := input.Reasoning

	q := `UPDATE agent_campaign_recommendations SET campaign_config = $1, projected_volume = $2, updated_at = NOW()`
	args := []interface{}{string(updatedConfigBytes), projectedVolume}
	idx := 3
	if campaignName != "" {
		q += fmt.Sprintf(`, campaign_name = $%d`, idx)
		args = append(args, campaignName)
		idx++
	}
	if scheduledDate != "" {
		q += fmt.Sprintf(`, scheduled_date = $%d`, idx)
		args = append(args, scheduledDate)
		idx++
	}
	if scheduledTime != "" {
		q += fmt.Sprintf(`, scheduled_time = $%d`, idx)
		args = append(args, scheduledTime)
		idx++
	}
	if reasoning != "" {
		q += fmt.Sprintf(`, reasoning = $%d`, idx)
		args = append(args, reasoning)
		idx++
	}
	q += fmt.Sprintf(` WHERE id = $%d AND organization_id = $%d`, idx, idx+1)
	args = append(args, recID, orgID)

	_, err = a.db.ExecContext(r.Context(), q, args...)
	if err != nil {
		log.Printf("[MarketingAgent] update recommendation error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Return full updated recommendation (same shape as HandleGetRecommendation)
	var domain, name, reas, strategy, retStatus, retConfigJSON string
	var volume int
	var retScheduledDate time.Time
	var retScheduledTime, approvedAt, executedCampaign, executionError sql.NullString
	var createdAt time.Time

	err = a.db.QueryRowContext(r.Context(),
		`SELECT sending_domain, scheduled_date, scheduled_time, COALESCE(campaign_name,''),
		        COALESCE(reasoning,''), COALESCE(strategy,''), projected_volume, status,
		        campaign_config::text, approved_at::text, executed_campaign_id::text,
		        COALESCE(execution_error,''), created_at
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&domain, &retScheduledDate, &retScheduledTime, &name,
		&reas, &strategy, &volume, &retStatus, &retConfigJSON,
		&approvedAt, &executedCampaign, &executionError, &createdAt)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "updated but failed to reload: " + err.Error()})
		return
	}

	result := map[string]interface{}{
		"id": recID, "sending_domain": domain,
		"scheduled_date": retScheduledDate.Format("2006-01-02"),
		"campaign_name": name, "reasoning": reas,
		"strategy": strategy, "projected_volume": volume,
		"status": retStatus, "created_at": createdAt.Format(time.RFC3339),
	}
	if retScheduledTime.Valid {
		result["scheduled_time"] = retScheduledTime.String
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
	if retConfigJSON != "" {
		var retCfg map[string]interface{}
		json.Unmarshal([]byte(retConfigJSON), &retCfg)
		result["campaign_config"] = retCfg
	}
	respondJSON(w, http.StatusOK, result)
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

	// Default warmup starting quotas when no campaign history exists
	if len(currentQuotas) == 0 {
		log.Printf("[MarketingAgent] no historical quotas found for org %s, using warmup defaults", orgID)
		maxDaily := 50000
		if mv, ok := params["max_daily_volume"].(float64); ok && int(mv) > 0 {
			maxDaily = int(mv)
		}
		// Distribution based on typical US mailbox market share
		currentQuotas = map[string]int{
			"gmail":     int(float64(maxDaily) * 0.30),
			"yahoo":     int(float64(maxDaily) * 0.18),
			"microsoft": int(float64(maxDaily) * 0.20),
			"apple":     int(float64(maxDaily) * 0.12),
			"comcast":   int(float64(maxDaily) * 0.08),
			"att":       int(float64(maxDaily) * 0.06),
			"cox":       int(float64(maxDaily) * 0.03),
			"charter":   int(float64(maxDaily) * 0.03),
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

	var fromName, fromEmail string
	a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(from_name,''), COALESCE(from_email,'') FROM mailing_sending_profiles
		 WHERE organization_id = $1 AND sending_domain = $2 AND status = 'active' LIMIT 1`,
		orgID, input.SendingDomain).Scan(&fromName, &fromEmail)
	if fromName == "" {
		fromName = strings.Split(input.SendingDomain, ".")[0]
	}

	audiencePriority := []string{"openers_7d", "clickers_14d", "engagers_30d", "recent_subscribers", "cold"}
	if ap, ok := params["audience_priority"].([]interface{}); ok {
		audiencePriority = []string{}
		for _, v := range ap {
			if s, ok := v.(string); ok {
				audiencePriority = append(audiencePriority, s)
			}
		}
	}
	newDataPriority := []string{"recent_subscribers", "cold"}

	// Load real mailing lists for this org
	type listInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var inclusionLists []listInfo
	listRows, _ := a.db.QueryContext(r.Context(),
		`SELECT id::text, name FROM mailing_lists WHERE organization_id = $1 AND status = 'active' ORDER BY name LIMIT 50`, orgID)
	if listRows != nil {
		defer listRows.Close()
		for listRows.Next() {
			var li listInfo
			listRows.Scan(&li.ID, &li.Name)
			inclusionLists = append(inclusionLists, li)
		}
	}
	if inclusionLists == nil {
		inclusionLists = []listInfo{}
	}

	// Load suppression lists
	var exclusionLists []listInfo
	suppRows, _ := a.db.QueryContext(r.Context(),
		`SELECT id::text, name FROM mailing_suppression_lists WHERE organization_id = $1 ORDER BY name LIMIT 50`, orgID)
	if suppRows != nil {
		defer suppRows.Close()
		for suppRows.Next() {
			var li listInfo
			suppRows.Scan(&li.ID, &li.Name)
			exclusionLists = append(exclusionLists, li)
		}
	}
	if exclusionLists == nil {
		exclusionLists = []listInfo{}
	}

	// Look up an existing template as reference for AI generation
	var referenceTemplateHTML string
	a.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(html_content,'') FROM mailing_templates
		 WHERE organization_id = $1 AND status IN ('active','draft') AND html_content IS NOT NULL AND html_content != ''
		 ORDER BY updated_at DESC LIMIT 1`, orgID).Scan(&referenceTemplateHTML)

	// Generate templates via AI (one batch per campaign type) — runs in background
	type savedTemplate struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Subject string `json:"subject"`
	}
	var warmupTemplates []savedTemplate
	var welcomeTemplates []savedTemplate

	generateTemplates := func(ctx context.Context, campaignType string) []savedTemplate {
		if a.aiContent == nil {
			return nil
		}
		log.Printf("[MarketingAgent] generating %s templates for %s...", campaignType, input.SendingDomain)
		result, genErr := a.aiContent.GenerateEmailTemplates(ctx, mailing.TemplateGenerationRequest{
			CampaignType:  campaignType,
			SendingDomain: input.SendingDomain,
			ReferenceHTML: referenceTemplateHTML,
		})
		if genErr != nil {
			log.Printf("[MarketingAgent] template generation warning (%s): %v", campaignType, genErr)
			return nil
		}
		if result == nil {
			return nil
		}
		var saved []savedTemplate
		for _, v := range result.Variations {
			var templateID string
			saveErr := a.db.QueryRowContext(ctx,
				`INSERT INTO mailing_templates (id, organization_id, name, subject, from_name, html_content, preview_text, status, created_at, updated_at)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, '', 'draft', NOW(), NOW())
				 RETURNING id::text`,
				orgID,
				fmt.Sprintf("[Maven] %s — %s", campaignType, v.VariantName),
				v.Subject,
				v.FromName,
				v.HTMLContent,
			).Scan(&templateID)
			if saveErr != nil {
				log.Printf("[MarketingAgent] template save warning: %v", saveErr)
				continue
			}
			saved = append(saved, savedTemplate{ID: templateID, Name: v.VariantName, Subject: v.Subject})
		}
		log.Printf("[MarketingAgent] generated %d %s templates", len(saved), campaignType)
		return saved
	}

	// Use reference HTML as fallback if no existing templates exist
	_ = referenceTemplateHTML

	// Generate daily recommendations FIRST (fast), then trigger template generation in background
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

		multiplier := 1.0 + (increasePct/100.0)*float64(dayIndex)

		// CAMPAIGN 1: Warmup — Engaged Data (70% of daily volume)
		warmupQuotas := map[string]interface{}{}
		warmupVolume := 0
		for isp, base := range currentQuotas {
			adjusted := int(float64(base) * multiplier * 0.70)
			warmupQuotas[isp] = adjusted
			warmupVolume += adjusted
		}
		warmupName := input.SendingDomain + " — Warmup Engaged — " + current.Format("Jan 2")
		warmupCfg, _ := json.Marshal(map[string]interface{}{
			"sending_domain":        input.SendingDomain,
			"isp_quotas":            warmupQuotas,
			"name":                  warmupName,
			"scheduled_date":        current.Format("2006-01-02"),
			"scheduled_time":        "13:00",
			"from_name":             fromName,
			"from_email":            fromEmail,
			"subject":               "",
			"preview_text":          "",
			"template_id":           "",
			"template_name":         "",
			"wave_interval_minutes": 15,
			"throttle_per_wave":     0,
			"audience_priority":     audiencePriority,
			"inclusion_lists":       inclusionLists,
			"exclusion_lists":       exclusionLists,
			"campaign_type":         "warmup_engaged",
		})
		_, err := a.db.ExecContext(r.Context(),
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time,
			  campaign_name, campaign_config, reasoning, strategy, projected_volume, status)
			 VALUES ($1, $2, $3, '13:00', $4, $5, $6, $7, $8, 'pending')`,
			orgID, input.SendingDomain, current.Format("2006-01-02"),
			warmupName, string(warmupCfg),
			fmt.Sprintf("Warmup campaign targeting engaged audience (openers, clickers) — 70%% of daily volume. Builds ISP reputation with high-engagement data. Day %d of warmup, %.0f%% cumulative volume increase.", dayIndex+1, increasePct*float64(dayIndex)),
			strategy, warmupVolume)
		if err != nil {
			log.Printf("[MarketingAgent] forecast insert error (warmup): %v", err)
		} else {
			created++
		}

		// CAMPAIGN 2: Welcome Series — New Data (30% of daily volume)
		welcomeQuotas := map[string]interface{}{}
		welcomeVolume := 0
		for isp, base := range currentQuotas {
			adjusted := int(float64(base) * multiplier * 0.30)
			welcomeQuotas[isp] = adjusted
			welcomeVolume += adjusted
		}
		welcomeName := input.SendingDomain + " — Welcome Series — " + current.Format("Jan 2")
		welcomeCfg, _ := json.Marshal(map[string]interface{}{
			"sending_domain":        input.SendingDomain,
			"isp_quotas":            welcomeQuotas,
			"name":                  welcomeName,
			"scheduled_date":        current.Format("2006-01-02"),
			"scheduled_time":        "14:00",
			"from_name":             fromName,
			"from_email":            fromEmail,
			"subject":               "",
			"preview_text":          "",
			"template_id":           "",
			"template_name":         "",
			"wave_interval_minutes": 15,
			"throttle_per_wave":     0,
			"audience_priority":     newDataPriority,
			"inclusion_lists":       inclusionLists,
			"exclusion_lists":       exclusionLists,
			"campaign_type":         "welcome_series",
		})
		_, err = a.db.ExecContext(r.Context(),
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time,
			  campaign_name, campaign_config, reasoning, strategy, projected_volume, status)
			 VALUES ($1, $2, $3, '14:00', $4, $5, $6, $7, $8, 'pending')`,
			orgID, input.SendingDomain, current.Format("2006-01-02"),
			welcomeName, string(welcomeCfg),
			fmt.Sprintf("Welcome series targeting new subscribers and cold data — 30%% of daily volume. Introduces fresh recipients at controlled pace. Day %d of warmup.", dayIndex+1),
			strategy, welcomeVolume)
		if err != nil {
			log.Printf("[MarketingAgent] forecast insert error (welcome): %v", err)
		} else {
			created++
		}

		dayIndex++
		current = current.AddDate(0, 0, 1)
	}

	// Respond immediately — templates will generate in the background
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":                  "generated",
		"recommendations_created": created,
		"sending_domain":          input.SendingDomain,
		"strategy":                strategy,
		"month":                   input.Month,
		"template_status":         "generating in background — refresh calendar in ~60 seconds to see templates",
	})

	// Background: generate templates and patch them into recommendations
	go func() {
		bgCtx := context.Background()
		warmupTemplates = generateTemplates(bgCtx, "re-engagement")
		welcomeTemplates = generateTemplates(bgCtx, "welcome")
		totalGenerated := len(warmupTemplates) + len(welcomeTemplates)
		if totalGenerated == 0 {
			log.Printf("[MarketingAgent] background template generation produced 0 templates")
			return
		}
		log.Printf("[MarketingAgent] background generated %d templates, patching into recommendations...", totalGenerated)

		// Patch templates into pending recommendations for this month
		recRows, err := a.db.QueryContext(bgCtx,
			`SELECT id::text, campaign_config::text FROM agent_campaign_recommendations
			 WHERE organization_id = $1 AND sending_domain = $2 AND status = 'pending'
			   AND scheduled_date >= $3 AND scheduled_date <= $4
			 ORDER BY scheduled_date, scheduled_time`,
			orgID, input.SendingDomain, startDate, t.AddDate(0, 1, -1).Format("2006-01-02"))
		if err != nil {
			log.Printf("[MarketingAgent] background patch query error: %v", err)
			return
		}
		defer recRows.Close()

		idx := 0
		for recRows.Next() {
			var recID, cfgJSON string
			recRows.Scan(&recID, &cfgJSON)
			var cfg map[string]interface{}
			json.Unmarshal([]byte(cfgJSON), &cfg)

			campType, _ := cfg["campaign_type"].(string)
			var templates []savedTemplate
			if campType == "welcome_series" {
				templates = welcomeTemplates
			} else {
				templates = warmupTemplates
			}
			if len(templates) == 0 {
				continue
			}
			tmpl := templates[(idx/2)%len(templates)]
			cfg["template_id"] = tmpl.ID
			cfg["template_name"] = tmpl.Name
			if tmpl.Subject != "" {
				cfg["subject"] = tmpl.Subject
			}
			updatedCfg, _ := json.Marshal(cfg)
			a.db.ExecContext(bgCtx,
				`UPDATE agent_campaign_recommendations SET campaign_config = $1, updated_at = NOW() WHERE id = $2`,
				string(updatedCfg), recID)
			idx++
		}
		log.Printf("[MarketingAgent] background template patching complete — %d recommendations updated", idx)
	}()
}

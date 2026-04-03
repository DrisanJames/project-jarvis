package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/config"
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

	q := `SELECT id::text, sending_domain, scheduled_date,
	             COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
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

	q := `SELECT id::text, sending_domain, scheduled_date,
	             COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
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
		`SELECT sending_domain, scheduled_date, COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
		        COALESCE(campaign_name,''), COALESCE(reasoning,''), COALESCE(strategy,''),
		        projected_volume, status, campaign_config::text,
		        approved_at::text, executed_campaign_id::text,
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

// approveResult holds the outcome of a recommendation approval.
type approveResult struct {
	CampaignID    string
	CampaignName  string
	CampaignStatus string
	ScheduledAt   time.Time
	TotalAudience int
	TargetISPs    []string
	ISPPlanCount  int
	WavePreview   []map[string]interface{}
}

// doApproveRecommendation is the shared pipeline that both the HTTP handler and the
// LLM tool dispatch call. It reads the recommendation, validates config, generates
// content, runs preflight, plans audience, creates the campaign, and marks the
// recommendation approved. Uses a detached context so ALB timeouts can't kill it.
func (a *EmailMarketingAgent) doApproveRecommendation(readCtx context.Context, orgID, recID string) (*approveResult, error) {
	var status, configJSON, campaignName, sendingDomain string
	var scheduledDate time.Time
	var scheduledTime sql.NullString
	err := a.db.QueryRowContext(readCtx,
		`SELECT status, campaign_config::text, COALESCE(campaign_name,''),
		        sending_domain, scheduled_date, COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), '')
		 FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &configJSON, &campaignName,
		&sendingDomain, &scheduledDate, &scheduledTime)
	if err != nil {
		return nil, fmt.Errorf("recommendation not found: %s", recID)
	}
	if status != "pending" {
		return nil, fmt.Errorf("can only approve pending recommendations, current status: %s", status)
	}

	var cfg map[string]interface{}
	if configJSON != "" {
		json.Unmarshal([]byte(configJSON), &cfg)
	}
	if cfg == nil {
		cfg = map[string]interface{}{}
	}

	templateID, _ := cfg["template_id"].(string)
	var htmlContent string
	if templateID != "" {
		a.db.QueryRowContext(readCtx,
			`SELECT COALESCE(html_content,'') FROM mailing_templates WHERE id = $1 AND organization_id = $2`,
			templateID, orgID).Scan(&htmlContent)
	}
	if strings.TrimSpace(htmlContent) == "" {
		return nil, fmt.Errorf("no email template assigned or template has no HTML content")
	}

	subject, _ := cfg["subject"].(string)
	fromName, _ := cfg["from_name"].(string)
	previewText, _ := cfg["preview_text"].(string)
	if subject == "" {
		return nil, fmt.Errorf("subject line is required")
	}
	if fromName == "" {
		return nil, fmt.Errorf("from name is required")
	}

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
				ISP: isp, Quota: vol, RandomizeAudience: false,
				ThrottleStrategy: "auto", Timezone: "UTC",
				Cadence: engine.PMTACadenceInput{
					Mode: "interval", EveryMinutes: waveInterval, BatchSize: 0,
				},
				TimeSpans: []engine.PMTATimeSpanInput{{
					Type: "absolute", StartAt: &schedAt,
					EndAt: func() *time.Time { t := schedAt.Add(8 * time.Hour); return &t }(),
				}},
			})
		}
	}
	if len(targetISPs) == 0 {
		return nil, fmt.Errorf("no ISP quotas configured")
	}

	var inclusionListIDs, inclusionSegmentIDs []string
	var sendPriority []engine.PriorityItem
	if lists, ok := cfg["inclusion_lists"].([]interface{}); ok {
		for _, item := range lists {
			switch v := item.(type) {
			case map[string]interface{}:
				id, _ := v["id"].(string)
				itemType, _ := v["type"].(string)
				if id == "" {
					continue
				}
				if itemType == "segment" {
					inclusionSegmentIDs = append(inclusionSegmentIDs, id)
					sendPriority = append(sendPriority, engine.PriorityItem{ID: id, Type: "segment"})
				} else {
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
	if len(inclusionListIDs) == 0 && len(inclusionSegmentIDs) == 0 {
		return nil, fmt.Errorf("no inclusion lists or segments configured")
	}

	var exclusionListIDs, exclusionSegmentIDs []string
	if lists, ok := cfg["exclusion_lists"].([]interface{}); ok {
		for _, item := range lists {
			switch v := item.(type) {
			case map[string]interface{}:
				id, _ := v["id"].(string)
				itemType, _ := v["type"].(string)
				if id == "" {
					continue
				}
				if itemType == "segment" {
					exclusionSegmentIDs = append(exclusionSegmentIDs, id)
				} else {
					exclusionListIDs = append(exclusionListIDs, id)
				}
			case string:
				if v != "" {
					exclusionListIDs = append(exclusionListIDs, v)
				}
			}
		}
	}

	cfgOfferID, _ := cfg["offer_id"].(string)

	// Detached context for heavy operations (content gen, audience, campaign creation).
	deployCtx, deployCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer deployCancel()

	variants := generateMultiVariantContent(deployCtx, a.db, campaignName, sendingDomain, fromName, subject, previewText, htmlContent)

	deployInput := engine.PMTACampaignInput{
		OfferID: cfgOfferID, Name: campaignName, TargetISPs: targetISPs,
		SendingDomain: sendingDomain, Variants: variants, ISPPlans: ispPlans,
		ISPQuotas: ispQuotas, InclusionLists: inclusionListIDs,
		InclusionSegments: inclusionSegmentIDs, SendPriority: sendPriority,
		ExclusionLists: exclusionListIDs, ExclusionSegments: exclusionSegmentIDs,
		SendDays: []string{}, SendHour: hour, Timezone: "UTC",
		ThrottleStrategy: "auto", RandomizeAudience: false,
		SendMode: "scheduled", ScheduledAt: &schedAt,
	}

	preflight := preflightDeployCheck(deployCtx, a.db, orgID, sendingDomain)
	if !preflight.OK {
		msgs := make([]string, len(preflight.Errors))
		for i, e := range preflight.Errors {
			msgs[i] = e.Check + ": " + e.Message
		}
		return nil, fmt.Errorf("preflight failed: %s", strings.Join(msgs, "; "))
	}

	normalized, normErr := normalizePMTACampaignInput(deployInput)
	if normErr != nil {
		return nil, fmt.Errorf("campaign normalization failed: %w", normErr)
	}

	audience, audErr := planPMTAAudience(deployCtx, a.db, orgID, deployInput, normalized, a.pmtaSvc.suppMatcher, a.pmtaSvc.globalHub, a.pmtaSvc.offerSuppMgr)
	if audErr != nil {
		return nil, fmt.Errorf("audience planning failed: %w", audErr)
	}

	tx, txErr := a.db.BeginTx(deployCtx, nil)
	if txErr != nil {
		return nil, fmt.Errorf("begin tx: %w", txErr)
	}
	defer tx.Rollback()

	result, createErr := createPMTAWaveCampaign(deployCtx, tx, a.db, orgID, deployInput, normalized, audience, a.pmtaSvc.colCache)
	if createErr != nil {
		return nil, fmt.Errorf("campaign creation failed: %w", createErr)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit failed: %w", err)
	}

	if cfgOfferID != "" {
		a.db.ExecContext(deployCtx,
			`UPDATE mailing_campaigns SET offer_id = $1::uuid WHERE id = $2::uuid AND (offer_id IS NULL OR offer_id = '00000000-0000-0000-0000-000000000000')`,
			cfgOfferID, result.CampaignID)
	}

	a.db.ExecContext(deployCtx,
		`UPDATE agent_campaign_recommendations
		 SET status = 'approved', approved_at = NOW(), executed_campaign_id = $1::uuid, updated_at = NOW()
		 WHERE id = $2`,
		result.CampaignID, recID)

	log.Printf("[MarketingAgent] recommendation %s approved → campaign %s scheduled for %s", recID, result.CampaignID, schedAt.Format(time.RFC3339))

	wavePreview := make([]map[string]interface{}, 0, len(normalized.Plans))
	for _, plan := range normalized.Plans {
		count := audience.CountsByISP[plan.ISP]
		waves := buildPMTAWaveSpecs(result.CampaignID, plan, count)
		entry := map[string]interface{}{
			"isp": plan.ISP, "audience_count": count, "wave_count": len(waves),
		}
		if len(waves) > 0 {
			entry["first_wave_at"] = waves[0].ScheduledAt.Format(time.RFC3339)
			entry["last_wave_at"] = waves[len(waves)-1].ScheduledAt.Format(time.RFC3339)
			entry["batch_size"] = waves[0].BatchSize
		}
		wavePreview = append(wavePreview, entry)
	}

	ispStrs := make([]string, len(result.TargetISPs))
	for i, isp := range result.TargetISPs {
		ispStrs[i] = string(isp)
	}

	return &approveResult{
		CampaignID:     result.CampaignID,
		CampaignName:   result.Name,
		CampaignStatus: result.Status,
		ScheduledAt:    schedAt,
		TotalAudience:  result.TotalAudience,
		TargetISPs:     ispStrs,
		ISPPlanCount:   len(result.ISPPlans),
		WavePreview:    wavePreview,
	}, nil
}

// HandleApproveRecommendation deploys the recommendation as a real scheduled campaign
// through the existing PMTA campaign pipeline — identical to deploying from Campaign Manager.
func (a *EmailMarketingAgent) HandleApproveRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)

	res, err := a.doApproveRecommendation(r.Context(), orgID, recID)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "approved",
		"id":              recID,
		"campaign_id":     res.CampaignID,
		"campaign_name":   res.CampaignName,
		"campaign_status": res.CampaignStatus,
		"scheduled_at":    res.ScheduledAt.Format(time.RFC3339),
		"total_audience":  res.TotalAudience,
		"target_isps":     res.TargetISPs,
		"isp_plans":       res.ISPPlanCount,
		"wave_preview":    res.WavePreview,
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

// HandleDeleteRecommendation permanently deletes a recommendation.
// By default only rejected recommendations can be deleted.
// Pass ?force=true to delete regardless of status.
func (a *EmailMarketingAgent) HandleDeleteRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	force := r.URL.Query().Get("force") == "true"

	q := `DELETE FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`
	if !force {
		q += ` AND status = 'rejected'`
	}
	result, err := a.db.ExecContext(r.Context(), q, recID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		msg := "recommendation not found or not in rejected status (use ?force=true to override)"
		if force {
			msg = "recommendation not found"
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": recID})
}

// HandleBulkDeleteRecommendations deletes all recommendations matching filters.
// Query params: after_date (required, YYYY-MM-DD), exclude_campaign_ids (comma-separated UUIDs to keep).
func (a *EmailMarketingAgent) HandleBulkDeleteRecommendations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	afterDate := r.URL.Query().Get("after_date")
	if afterDate == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "after_date query param is required (YYYY-MM-DD)"})
		return
	}

	q := `DELETE FROM agent_campaign_recommendations WHERE organization_id = $1 AND scheduled_date >= $2`
	args := []interface{}{orgID, afterDate}

	if excludeRaw := r.URL.Query().Get("exclude_campaign_ids"); excludeRaw != "" {
		ids := strings.Split(excludeRaw, ",")
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			args = append(args, strings.TrimSpace(id))
			placeholders[i] = fmt.Sprintf("$%d::uuid", i+3)
		}
		q += ` AND (executed_campaign_id IS NULL OR executed_campaign_id NOT IN (` + strings.Join(placeholders, ",") + `))`
	}

	result, err := a.db.ExecContext(r.Context(), q, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": affected})
}

// HandleUnapproveRecommendation reverts an approved recommendation back to pending
// and cancels the linked campaign if it hasn't started sending.
func (a *EmailMarketingAgent) HandleUnapproveRecommendation(w http.ResponseWriter, r *http.Request) {
	recID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	ctx := r.Context()

	var status string
	var executedCampaignID sql.NullString
	err := a.db.QueryRowContext(ctx,
		`SELECT status, executed_campaign_id::text FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&status, &executedCampaignID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}
	if status != "approved" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "can only unapprove approved recommendations, current status: " + status})
		return
	}

	// Cancel the linked campaign if it exists and hasn't started sending
	campaignCancelled := false
	if executedCampaignID.Valid && executedCampaignID.String != "" {
		var campStatus string
		err := a.db.QueryRowContext(ctx,
			`SELECT status FROM mailing_campaigns WHERE id = $1::uuid`, executedCampaignID.String).Scan(&campStatus)
		if err == nil {
			if campStatus == "sending" || campStatus == "sent" || campStatus == "completed" {
				respondJSON(w, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("cannot unapprove: linked campaign %s is already %s", executedCampaignID.String, campStatus),
				})
				return
			}
			a.db.ExecContext(ctx,
				`UPDATE mailing_campaigns SET status = 'cancelled', updated_at = NOW() WHERE id = $1::uuid`,
				executedCampaignID.String)
			campaignCancelled = true
		}
	}

	// Revert recommendation to pending
	a.db.ExecContext(ctx,
		`UPDATE agent_campaign_recommendations SET status = 'pending', approved_at = NULL, executed_campaign_id = NULL, updated_at = NOW()
		 WHERE id = $1 AND organization_id = $2`, recID, orgID)

	log.Printf("[MarketingAgent] recommendation %s unapproved (campaign cancelled: %v)", recID, campaignCancelled)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":             "pending",
		"id":                 recID,
		"campaign_cancelled": campaignCancelled,
	})
}

// HandleUpdateRecommendation allows editing a pending or approved recommendation's campaign config.
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
	var linkedCampaignID sql.NullString
	err := a.db.QueryRowContext(r.Context(),
		`SELECT campaign_config::text, status, executed_campaign_id::text FROM agent_campaign_recommendations WHERE id = $1 AND organization_id = $2`,
		recID, orgID).Scan(&existingConfigJSON, &status, &linkedCampaignID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "recommendation not found"})
		return
	}
	if status != "pending" && status != "approved" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "can only edit pending or approved recommendations"})
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

	// If approved with a linked campaign, propagate content changes
	if status == "approved" && linkedCampaignID.Valid && linkedCampaignID.String != "" {
		cSets := []string{}
		cArgs := []interface{}{}
		ci := 1
		if input.Subject != "" {
			cSets = append(cSets, fmt.Sprintf("subject = $%d", ci))
			cArgs = append(cArgs, input.Subject)
			ci++
		}
		if input.PreviewText != "" {
			cSets = append(cSets, fmt.Sprintf("preview_text = $%d", ci))
			cArgs = append(cArgs, input.PreviewText)
			ci++
		}
		if input.FromName != "" {
			cSets = append(cSets, fmt.Sprintf("from_name = $%d", ci))
			cArgs = append(cArgs, input.FromName)
			ci++
		}
		if input.FromEmail != "" {
			cSets = append(cSets, fmt.Sprintf("from_email = $%d", ci))
			cArgs = append(cArgs, input.FromEmail)
			ci++
		}
		if len(cSets) > 0 {
			cArgs = append(cArgs, linkedCampaignID.String)
			cq := fmt.Sprintf("UPDATE mailing_campaigns SET %s, updated_at = NOW() WHERE id = $%d::uuid", strings.Join(cSets, ", "), ci)
			a.db.ExecContext(r.Context(), cq, cArgs...)
		}
	}

	// Return full updated recommendation (same shape as HandleGetRecommendation)
	var domain, name, reas, strategy, retStatus, retConfigJSON string
	var volume int
	var retScheduledDate time.Time
	var retScheduledTime, approvedAt, executedCampaign, executionError sql.NullString
	var createdAt time.Time

	err = a.db.QueryRowContext(r.Context(),
		`SELECT sending_domain, scheduled_date, COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), ''),
		        COALESCE(campaign_name,''), COALESCE(reasoning,''), COALESCE(strategy,''),
		        projected_volume, status, campaign_config::text,
		        approved_at::text, executed_campaign_id::text,
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

// HandleGenerateForecast routes forecast generation through the EDITH LLM agent.
// Instead of hardcoded scheduling logic, EDITH uses its tools (compute_isp_quotas,
// list_lists, list_segments, etc.) to reason about volume, audience, and timing,
// then calls create_recommendation for each campaign.
func (a *EmailMarketingAgent) HandleGenerateForecast(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var input struct {
		SendingDomain   string `json:"sending_domain"`
		Month           string `json:"month"`
		ForceRegenerate bool   `json:"force_regenerate"`
		StartDate       string `json:"start_date"`
		EndDate         string `json:"end_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if input.SendingDomain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain is required"})
		return
	}

	startDate := input.StartDate
	endDate := input.EndDate
	if startDate == "" && input.Month != "" {
		startDate = input.Month + "-01"
		if t, err := time.Parse("2006-01-02", startDate); err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid month format"})
			return
		} else {
			endDate = t.AddDate(0, 1, -1).Format("2006-01-02")
		}
	}
	if startDate == "" {
		startDate = time.Now().Format("2006-01-02")
	}
	if endDate == "" {
		endDate = time.Now().AddDate(0, 0, 14).Format("2006-01-02")
	}

	// Route through EDITH LLM: construct a structured prompt and let the agent
	// use its tools (compute_isp_quotas, list_lists, create_recommendation, etc.)
	// to generate the forecast with full reasoning.
	prompt := fmt.Sprintf(
		`Generate a campaign schedule for sending domain %s from %s to %s. `+
			`Follow the Decision Framework in your system prompt exactly. `+
			`For each day in the range, create the standard 2-campaign pattern: `+
			`1) Newsletter/Engaged campaign (engaged segments only, scheduled first), `+
			`2) Welcome/Main campaign (ISP lists only, scheduled 30 min after). `+
			`Use compute_isp_quotas for each day's target volume. `+
			`Apply 20%% compound daily growth from the most recent actual send volume. `+
			`Verify everything with get_recommendations after creating. `+
			`Present a consolidated schedule table when done.`,
		input.SendingDomain, startDate, endDate,
	)
	if input.ForceRegenerate {
		prompt += " Clear existing pending recommendations for this domain first using clear_forecasts."
	}

	// Load context for the LLM
	memories := a.loadMemories(r.Context(), orgID)
	strategies := a.loadDomainStrategies(r.Context(), orgID)
	systemPrompt := buildAgentSystemPrompt(memories, strategies)

	messages := []agentOpenAIMsg{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: prompt},
	}
	tools := getAgentTools()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	var actionsTaken []string
	var assistantContent string

	for i := 0; i < 25; i++ {
		var resp *agentOpenAIResp
		var llmErr error
		if a.useAnthropic {
			resp, llmErr = a.callClaude(ctx, systemPrompt, messages, tools)
		} else {
			openaiReq := agentOpenAIReq{
				Model: a.model, Messages: messages, Tools: tools,
				Temperature: 0.3, MaxCompletionTokens: 16000,
			}
			resp, llmErr = a.callAgentOpenAI(ctx, openaiReq)
		}
		if llmErr != nil {
			log.Printf("[MarketingAgent] forecast LLM error: %v", llmErr)
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "AI service error: " + llmErr.Error()})
			return
		}
		if len(resp.Choices) == 0 {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "empty AI response"})
			return
		}

		choice := resp.Choices[0]
		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			messages = append(messages, choice.Message)
			for _, tc := range choice.Message.ToolCalls {
				result, action := a.executeAgentTool(ctx, orgID, tc.Function.Name, tc.Function.Arguments)
				if action != "" {
					actionsTaken = append(actionsTaken, action)
				}
				messages = append(messages, agentOpenAIMsg{Role: "tool", Content: result, ToolCallID: tc.ID})
			}
			continue
		}
		assistantContent = choice.Message.Content
		break
	}

	// Count recommendations created
	var created int
	a.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_campaign_recommendations WHERE organization_id = $1 AND sending_domain = $2 AND status = 'pending' AND scheduled_date >= $3`,
		orgID, input.SendingDomain, startDate).Scan(&created)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":                  "generated",
		"recommendations_created": created,
		"sending_domain":          input.SendingDomain,
		"start_date":              startDate,
		"end_date":                endDate,
		"actions_taken":           actionsTaken,
		"edith_summary":           assistantContent,
	})
}

// HandleClearForecasts deletes all agent campaign recommendations for the org.
// POST /api/mailing/agent/calendar/clear-forecasts
func (a *EmailMarketingAgent) HandleClearForecasts(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	result, err := a.db.ExecContext(r.Context(),
		`DELETE FROM agent_campaign_recommendations WHERE organization_id = $1`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, _ := result.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "cleared",
		"deleted":  n,
		"message":  fmt.Sprintf("Cleared %d forecast recommendations", n),
	})
}

// HandleCancelTomorrowCampaigns cancels all campaigns scheduled for tomorrow (UTC).
// POST /api/mailing/agent/calendar/cancel-tomorrow
func (a *EmailMarketingAgent) HandleCancelTomorrowCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE mailing_campaigns
		SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE organization_id::text = $1
		  AND status IN ('scheduled', 'preparing')
		  AND scheduled_at IS NOT NULL
		  AND (scheduled_at AT TIME ZONE 'UTC')::date = $2::date
	`, orgID, tomorrow)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cancelled, _ := result.RowsAffected()

	// Cancel queued items for those campaigns
	a.db.ExecContext(r.Context(), `
		UPDATE mailing_campaign_queue
		SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id IN (
			SELECT id FROM mailing_campaigns
			WHERE organization_id::text = $1 AND status = 'cancelled' AND completed_at > NOW() - INTERVAL '1 minute'
		)
		AND status IN ('queued', 'paused')
	`, orgID)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "cancelled",
		"date":     tomorrow,
		"count":    cancelled,
		"message":  fmt.Sprintf("Cancelled %d campaigns scheduled for %s", cancelled, tomorrow),
	})
}

// HandleCloneDay clones all approved recommendations from a source date to a target date,
// applying a growth multiplier to ISP quotas. Each cloned recommendation is automatically
// approved, triggering full campaign creation (audience planning, content generation, etc.).
// POST /api/mailing/agent/calendar/clone-day
// Body: {"source_date":"2026-03-23","target_date":"2026-03-24","growth_percent":20,"isp_growth":{"gmail":25,"yahoo":15}}
func (a *EmailMarketingAgent) HandleCloneDay(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	var body struct {
		SourceDate    string             `json:"source_date"`
		TargetDate    string             `json:"target_date"`
		GrowthPercent float64            `json:"growth_percent"`
		ISPGrowth     map[string]float64 `json:"isp_growth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.SourceDate == "" || body.TargetDate == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "source_date and target_date are required"})
		return
	}
	if _, err := time.Parse("2006-01-02", body.SourceDate); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "source_date must be YYYY-MM-DD"})
		return
	}
	if _, err := time.Parse("2006-01-02", body.TargetDate); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "target_date must be YYYY-MM-DD"})
		return
	}
	if body.SourceDate == body.TargetDate {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "source_date and target_date must differ"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id::text, sending_domain, COALESCE(TO_CHAR(scheduled_time, 'HH24:MI'), '10:00'),
		       COALESCE(campaign_name,''), COALESCE(reasoning,''), COALESCE(strategy,''),
		       campaign_config::text
		FROM agent_campaign_recommendations
		WHERE organization_id = $1 AND scheduled_date = $2::date AND status = 'approved'
		ORDER BY sending_domain, campaign_name
	`, orgID, body.SourceDate)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query source recs: " + err.Error()})
		return
	}
	defer rows.Close()

	type sourceRec struct {
		ID, Domain, Time, Name, Reasoning, Strategy, ConfigJSON string
	}
	var sources []sourceRec
	for rows.Next() {
		var s sourceRec
		if err := rows.Scan(&s.ID, &s.Domain, &s.Time, &s.Name, &s.Reasoning, &s.Strategy, &s.ConfigJSON); err != nil {
			continue
		}
		sources = append(sources, s)
	}
	if len(sources) == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no approved recommendations found for %s", body.SourceDate)})
		return
	}

	// Check for existing approved recs on target date to avoid duplicates
	var existingCount int
	a.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM agent_campaign_recommendations WHERE organization_id = $1 AND scheduled_date = $2::date AND status = 'approved'`,
		orgID, body.TargetDate).Scan(&existingCount)
	if existingCount > 0 {
		respondJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("%d approved recommendations already exist for %s — unapprove/delete them first or pick a different target_date", existingCount, body.TargetDate),
		})
		return
	}

	// Use a detached context so ALB timeout doesn't kill mid-flight
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cloneCancel()

	type cloneResult struct {
		Name       string `json:"name"`
		Domain     string `json:"domain"`
		RecID      string `json:"rec_id"`
		CampaignID string `json:"campaign_id,omitempty"`
		Audience   int    `json:"audience"`
		Status     string `json:"status"`
		Error      string `json:"error,omitempty"`
	}
	var results []cloneResult

	for _, src := range sources {
		var cfg map[string]interface{}
		if src.ConfigJSON != "" {
			json.Unmarshal([]byte(src.ConfigJSON), &cfg)
		}
		if cfg == nil {
			cfg = map[string]interface{}{}
		}

		if quotas, ok := cfg["isp_quotas"].(map[string]interface{}); ok {
			for isp, v := range quotas {
				vol := 0.0
				switch n := v.(type) {
				case float64:
					vol = n
				case int:
					vol = float64(n)
				}
				growthPct := body.GrowthPercent
				if ispPct, found := body.ISPGrowth[isp]; found {
					growthPct = ispPct
				}
				quotas[isp] = int(vol * (1.0 + growthPct/100.0))
			}
			cfg["isp_quotas"] = quotas
		}

		configJSON, _ := json.Marshal(cfg)
		totalVolume := 0
		if quotas, ok := cfg["isp_quotas"].(map[string]interface{}); ok {
			for _, v := range quotas {
				switch n := v.(type) {
				case float64:
					totalVolume += int(n)
				case int:
					totalVolume += n
				}
			}
		}

		var strategy string
		a.db.QueryRowContext(cloneCtx,
			`SELECT strategy FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
			orgID, src.Domain).Scan(&strategy)
		if strategy == "" {
			strategy = src.Strategy
		}

		var newRecID string
		err := a.db.QueryRowContext(cloneCtx,
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time, campaign_name,
			  campaign_config, reasoning, strategy, projected_volume, status)
			 VALUES ($1, $2, $3::date, $4::time, $5, $6, $7, $8, $9, 'pending')
			 RETURNING id::text`,
			orgID, src.Domain, body.TargetDate, src.Time, src.Name,
			string(configJSON), fmt.Sprintf("Cloned from %s with %.0f%% growth", body.SourceDate, body.GrowthPercent),
			strategy, totalVolume,
		).Scan(&newRecID)
		if err != nil {
			results = append(results, cloneResult{Name: src.Name, Domain: src.Domain, Status: "failed", Error: "insert: " + err.Error()})
			continue
		}

		result, approveErr := a.doApproveRecommendation(cloneCtx, orgID, newRecID)
		if approveErr != nil {
			results = append(results, cloneResult{Name: src.Name, Domain: src.Domain, RecID: newRecID, Status: "failed", Error: approveErr.Error()})
			continue
		}
		results = append(results, cloneResult{
			Name: src.Name, Domain: src.Domain, RecID: newRecID,
			CampaignID: result.CampaignID, Audience: result.TotalAudience, Status: "approved",
		})
		log.Printf("[CloneDay] %s → %s: %s audience=%d campaign=%s", body.SourceDate, body.TargetDate, src.Name, result.TotalAudience, result.CampaignID)
	}

	ok := 0
	failed := 0
	for _, r := range results {
		if r.Status == "approved" {
			ok++
		} else {
			failed++
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "completed",
		"source_date":  body.SourceDate,
		"target_date":  body.TargetDate,
		"growth":       body.GrowthPercent,
		"isp_growth":   body.ISPGrowth,
		"total_cloned": ok,
		"total_failed": failed,
		"results":      results,
	})
}

// HandleComputeQuotas exposes ComputeISPQuotas as a REST endpoint.
// GET /api/mailing/agent/calendar/compute-quotas?domain=...&volume=...
func (a *EmailMarketingAgent) HandleComputeQuotas(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	domain := r.URL.Query().Get("domain")
	volumeStr := r.URL.Query().Get("volume")
	if domain == "" || volumeStr == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "domain and volume query params are required"})
		return
	}
	var volume int
	fmt.Sscanf(volumeStr, "%d", &volume)
	if volume <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "volume must be a positive integer"})
		return
	}
	result := a.ComputeISPQuotas(r.Context(), orgID, domain, volume)
	respondJSON(w, http.StatusOK, result)
}

// HandleCreateRecommendation creates a campaign recommendation directly via REST.
// POST /api/mailing/agent/calendar/recommendations
func (a *EmailMarketingAgent) HandleCreateRecommendation(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	domain, _ := body["sending_domain"].(string)
	dateStr, _ := body["scheduled_date"].(string)
	timeStr, _ := body["scheduled_time"].(string)
	name, _ := body["campaign_name"].(string)
	reasoning, _ := body["reasoning"].(string)

	if domain == "" || dateStr == "" || name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain, scheduled_date, and campaign_name are required"})
		return
	}
	if timeStr == "" {
		timeStr = "13:00"
	}

	campaignConfig := map[string]interface{}{
		"sending_domain": domain,
		"name":           name,
		"scheduled_date": dateStr,
		"scheduled_time": timeStr,
	}

	setIfPresent := func(key, argKey string) {
		if v, ok := body[argKey]; ok && v != nil {
			campaignConfig[key] = v
		}
	}
	setIfPresent("isp_quotas", "isp_quotas")
	setIfPresent("inclusion_lists", "inclusion_lists")
	setIfPresent("exclusion_lists", "exclusion_lists")
	setIfPresent("template_id", "template_id")
	setIfPresent("subject", "subject")
	setIfPresent("preview_text", "preview_text")
	setIfPresent("from_name", "from_name")
	setIfPresent("from_email", "from_email")
	setIfPresent("audience_priority", "audience_priority")
	if v, ok := body["wave_interval_minutes"].(float64); ok {
		campaignConfig["wave_interval_minutes"] = int(v)
	}
	if v, ok := body["throttle_per_wave"].(float64); ok {
		campaignConfig["throttle_per_wave"] = int(v)
	}

	configJSON, _ := json.Marshal(campaignConfig)

	totalVolume := 0
	if quotas, ok := body["isp_quotas"].(map[string]interface{}); ok {
		for _, v := range quotas {
			if n, ok := v.(float64); ok {
				totalVolume += int(n)
			}
		}
	}

	var strategy string
	a.db.QueryRowContext(r.Context(),
		`SELECT strategy FROM agent_domain_strategies WHERE organization_id = $1 AND sending_domain = $2`,
		orgID, domain).Scan(&strategy)

	initialStatus := "pending"
	if s, ok := body["status"].(string); ok && (s == "approved" || s == "pending") {
		initialStatus = s
	}
	executedCampaignID, _ := body["executed_campaign_id"].(string)

	var id string
	var err error
	if initialStatus == "approved" && executedCampaignID != "" {
		err = a.db.QueryRowContext(r.Context(),
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time, campaign_name,
			  campaign_config, reasoning, strategy, projected_volume, status, approved_at, executed_campaign_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'approved', NOW(), $10::uuid)
			 RETURNING id::text`,
			orgID, domain, dateStr, timeStr, name, string(configJSON), reasoning, strategy, totalVolume, executedCampaignID,
		).Scan(&id)
	} else {
		err = a.db.QueryRowContext(r.Context(),
			`INSERT INTO agent_campaign_recommendations
			 (organization_id, sending_domain, scheduled_date, scheduled_time, campaign_name,
			  campaign_config, reasoning, strategy, projected_volume, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending')
			 RETURNING id::text`,
			orgID, domain, dateStr, timeStr, name, string(configJSON), reasoning, strategy, totalVolume,
		).Scan(&id)
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"status":          initialStatus,
		"id":              id,
		"campaign_name":   name,
		"scheduled_date":  dateStr,
		"scheduled_time":  timeStr,
		"projected_volume": totalVolume,
		"approval_status": initialStatus,
	})
}

// generateMultiVariantContent attempts to produce 3 content variants for
// round-robin rotation by scraping the brand site and running the AI wave
// content generator. Both newsletter/engaged and welcome campaigns are
// supported — the campaignName is used to select the correct brand config
// (welcome vs newsletter) so that welcome campaigns get welcome-appropriate
// multi-variant content while preserving the approved template structure.
// Every text element (subject, preview, intro, articles, CTAs) varies between
// variants for ISP anti-fingerprinting while the HTML layout stays locked.
func generateMultiVariantContent(ctx context.Context, db *sql.DB, campaignName, sendingDomain, fromName, subject, previewText, htmlContent string) []engine.ContentVariant {
	singleVariant := []engine.ContentVariant{{
		VariantName:  "A",
		FromName:     fromName,
		Subject:      subject,
		PreviewText:  previewText,
		HTMLContent:  htmlContent,
		SplitPercent: 100,
	}}

	isWelcome := strings.Contains(strings.ToLower(campaignName), "welcome")

	brands := knownBrands()
	var brand *brandConfig
	for _, b := range brands {
		if b.SendingDomain != sendingDomain {
			continue
		}
		if isWelcome && b.CampaignType == "welcome" {
			bc := b
			brand = &bc
			break
		}
		if !isWelcome && b.CampaignType != "welcome" {
			bc := b
			brand = &bc
			break
		}
	}
	if brand == nil {
		return singleVariant
	}

	log.Printf("[EDITH-deploy] matched brand %q (type=%s) for campaign %q", brand.BrandName, brand.CampaignType, campaignName)

	// --- Try pre-populated wave content cache first (fast path) ---
	// This avoids expensive live AI generation + web scraping on every approval.
	// Cache entries were generated from the brand's approved HTML template, preserving styling.
	numVariants := 4
	cacheRows, cacheErr := db.QueryContext(ctx, `
		SELECT id, wave_index, subject, preview_text, from_name, html_content
		FROM mailing_wave_content_cache
		WHERE brand_key = $1 AND used_at IS NULL AND version = $2
		  AND ($3 = '' OR campaign_type = $3 OR campaign_type IS NULL)
		ORDER BY generated_at DESC
		LIMIT $4
	`, brand.Key, mailing.GeneratorVersion, brand.CampaignType, numVariants)
	if cacheErr == nil {
		defer cacheRows.Close()
		variantNames := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
		var cached []engine.ContentVariant
		var cacheIDs []int
		for cacheRows.Next() {
			var cacheID, waveIdx int
			var cSubj, cPreview, cFrom, cHTML string
			if cacheRows.Scan(&cacheID, &waveIdx, &cSubj, &cPreview, &cFrom, &cHTML) == nil && strings.TrimSpace(cHTML) != "" {
				cached = append(cached, engine.ContentVariant{
					VariantName:  variantNames[len(cached)%len(variantNames)],
					FromName:     fromName,
					Subject:      cSubj,
					PreviewText:  cPreview,
					HTMLContent:  cHTML,
					SplitPercent: 0,
				})
				cacheIDs = append(cacheIDs, cacheID)
			}
		}
		if len(cached) >= 2 {
			splitPct := 100.0 / float64(len(cached))
			for i := range cached {
				cached[i].SplitPercent = splitPct
				if i == 0 {
					cached[i].SplitPercent = 100.0 - splitPct*float64(len(cached)-1)
				}
			}
			for _, cid := range cacheIDs {
				db.ExecContext(ctx, `UPDATE mailing_wave_content_cache SET used_at = NOW() WHERE id = $1`, cid)
			}
			log.Printf("[EDITH-deploy] used %d cached variants for %s (brand: %s, type: %s)", len(cached), sendingDomain, brand.BrandName, brand.CampaignType)
			return sanitizeVariantURLs(cached, brand.BlogDomain)
		}
		log.Printf("[EDITH-deploy] cache miss for %s/%s (found %d, need ≥2) — falling back to live AI generation", brand.Key, brand.CampaignType, len(cached))
	}

	// --- Live AI generation (slow path) ---
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		for _, cfgPath := range []string{"config/config.yaml", "/app/config/config.yaml"} {
			if cfg, err := config.Load(cfgPath); err == nil && cfg.OpenAI.APIKey != "" {
				openaiKey = cfg.OpenAI.APIKey
				break
			}
		}
	}
	if anthropicKey == "" && openaiKey == "" {
		log.Printf("[EDITH-deploy] no AI API key available — deploying single variant for %s", brand.BrandName)
		return singleVariant
	}

	aiSvc := mailing.NewAIContentService(db, anthropicKey, openaiKey)
	waveGen := mailing.NewWaveContentGenerator(aiSvc)

	log.Printf("[EDITH-deploy] scraping brand intelligence from %s for multi-variant generation", brand.BlogDomain)
	brandInfo := aiSvc.ScrapeBrandIntelligence(ctx, brand.BlogDomain)
	contentPool := brandInfo.BlogPosts
	if len(contentPool) < 3 && len(brand.FallbackContent) > 0 {
		contentPool = brand.FallbackContent
	}

	req := mailing.WaveContentRequest{
		SendingDomain: brand.SendingDomain,
		BrandName:     brand.BrandName,
		NumWaves:      numVariants,
		CampaignType:  brand.CampaignType,
		Voice:         brand.Voice,
		Audience:      brand.Audience,
		DesignSystem:  brand.DesignSystem,
		HTMLTemplate:  brand.HTMLTemplate,
		BrandInfo:     brandInfo,
		ContentPool:   contentPool,
	}

	result, err := waveGen.GenerateFull(ctx, req)
	if err != nil {
		log.Printf("[EDITH-deploy] wave generation failed for %s: %v — falling back to single variant", brand.BrandName, err)
		return singleVariant
	}

	if len(result.Variations) == 0 {
		log.Printf("[EDITH-deploy] wave generation returned 0 variations for %s — falling back to single variant", brand.BrandName)
		return singleVariant
	}

	variantNames2 := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	splitPct := 100.0 / float64(len(result.Variations))
	variants := make([]engine.ContentVariant, 0, len(result.Variations))
	for i, v := range result.Variations {
		name := variantNames2[i%len(variantNames2)]
		pct := splitPct
		if i == 0 {
			pct = 100.0 - splitPct*float64(len(result.Variations)-1)
		}
		variants = append(variants, engine.ContentVariant{
			VariantName:  name,
			FromName:     fromName,
			Subject:      v.Subject,
			PreviewText:  v.PreviewText,
			HTMLContent:  v.HTMLContent,
			SplitPercent: pct,
		})
	}

	log.Printf("[EDITH-deploy] generated %d live AI variants for %s (brand: %s)", len(variants), sendingDomain, brand.BrandName)
	return sanitizeVariantURLs(variants, brand.BlogDomain)
}

// brandURLFallbackRules maps blog domains to their fallback target for
// hallucinated article URLs. AI-generated slugs that don't match any known
// path are rewritten to the fallback (typically /blog).
var brandURLFallbackRules = map[string]struct {
	Fallback   string
	KnownPaths []string
}{
	"historythinking.com": {
		Fallback: "/blog",
		KnownPaths: []string{
			"/blog", "/privacy", "/terms", "/about", "/auth",
			"/blog/category/ancient-civilizations",
			"/blog/category/american-history",
			"/blog/category/cultural-history",
			"/blog/category/historical-figures",
			"/blog/category/medieval-world",
			"/blog/category/revolutionary-movements",
			"/blog/category/science-and-discovery",
			"/blog/category/world-wars",
		},
	},
}

// sanitizeVariantURLs rewrites hallucinated article URLs in variant HTML to
// the brand's blog root. AI fabricates article slugs that 404; this redirects
// them to the nearest real content page.
func sanitizeVariantURLs(variants []engine.ContentVariant, blogDomain string) []engine.ContentVariant {
	rule, ok := brandURLFallbackRules[blogDomain]
	if !ok {
		return variants
	}

	baseURL := "https://" + blogDomain
	re := regexp.MustCompile(`href="` + regexp.QuoteMeta(baseURL) + `/([^"]+)"`)

	for i := range variants {
		html := variants[i].HTMLContent
		replaced := false
		html = re.ReplaceAllStringFunc(html, func(match string) string {
			slug := re.FindStringSubmatch(match)
			if len(slug) < 2 {
				return match
			}
			path := "/" + slug[1]
			for _, kp := range rule.KnownPaths {
				if path == kp || strings.HasPrefix(path, kp+"/") {
					return match
				}
			}
			replaced = true
			return `href="` + baseURL + rule.Fallback + `"`
		})
		if replaced {
			variants[i].HTMLContent = html
			log.Printf("[EDITH-urlfix] rewrote hallucinated URLs to %s%s for variant %s (domain: %s)",
				baseURL, rule.Fallback, variants[i].VariantName, blogDomain)
		}
	}
	return variants
}

// HandleGenerateVariants creates 4 A/B content variants for an existing campaign
// and inserts them into mailing_ab_tests + mailing_ab_variants for round-robin rotation.
// POST /api/mailing/agent/calendar/campaigns/{campaignId}/generate-variants
func (a *EmailMarketingAgent) HandleGenerateVariants(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	campaignID := chi.URLParam(r, "campaignId")
	if campaignID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaignId is required"})
		return
	}

	var name, sendingDomain, fromName, fromEmail, subject, previewText, htmlContent, sendingProfileID string
	err := a.db.QueryRowContext(r.Context(), `
		SELECT c.name, COALESCE(sp.sending_domain, ''), COALESCE(c.from_name, ''),
		       COALESCE(c.from_email, ''), COALESCE(c.subject, ''),
		       COALESCE(c.preview_text, ''), COALESCE(c.html_content, ''),
		       COALESCE(c.sending_profile_id::text, '')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.id = $1 AND c.organization_id = $2
	`, campaignID, orgID).Scan(&name, &sendingDomain, &fromName, &fromEmail, &subject, &previewText, &htmlContent, &sendingProfileID)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "campaign not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if sendingDomain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaign has no sending profile/domain"})
		return
	}

	variants := generateMultiVariantContent(r.Context(), a.db, name, sendingDomain, fromName, subject, previewText, htmlContent)
	if len(variants) < 2 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":   "fallback",
			"message":  "AI generation returned only 1 variant; using campaign content as-is",
			"variants": variants,
		})
		return
	}

	// Remove any existing AB test for this campaign before inserting fresh variants
	a.db.ExecContext(r.Context(), `
		DELETE FROM mailing_ab_variants WHERE test_id IN (
			SELECT id FROM mailing_ab_tests WHERE campaign_id = $1 AND organization_id = $2
		)`, campaignID, orgID)
	a.db.ExecContext(r.Context(), `
		DELETE FROM mailing_ab_tests WHERE campaign_id = $1 AND organization_id = $2`, campaignID, orgID)

	var testID string
	err = a.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_ab_tests (organization_id, campaign_id, name, test_type,
			test_sample_percent, winner_metric, status, created_at, updated_at)
		VALUES ($1, $2::uuid, $3, 'content', 100, 'open_rate', 'testing', NOW(), NOW())
		RETURNING id::text
	`, orgID, campaignID, name+" Content Rotation").Scan(&testID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "create ab test: " + err.Error()})
		return
	}

	for _, v := range variants {
		_, err = a.db.ExecContext(r.Context(), `
			INSERT INTO mailing_ab_variants (test_id, variant_name, from_name, subject, html_content, split_percent, created_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, NOW())
		`, testID, v.VariantName, v.FromName, v.Subject, v.HTMLContent, int(v.SplitPercent))
		if err != nil {
			log.Printf("[EDITH-variants] failed to insert variant %s for campaign %s: %v", v.VariantName, campaignID, err)
		}
	}

	variantSummary := make([]map[string]interface{}, len(variants))
	for i, v := range variants {
		variantSummary[i] = map[string]interface{}{
			"variant_name": v.VariantName,
			"subject":      v.Subject,
			"preview_text": v.PreviewText,
			"from_name":    v.FromName,
			"split_pct":    int(v.SplitPercent),
		}
	}

	log.Printf("[EDITH-variants] created %d variants for campaign %s (%s)", len(variants), campaignID, name)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "ok",
		"campaign_id": campaignID,
		"test_id":     testID,
		"count":       len(variants),
		"variants":    variantSummary,
	})
}

// HandleGetDayVariants returns all A/B variants for campaigns linked to recommendations on a given date.
// GET /api/mailing/agent/calendar/day/{date}/variants
func (a *EmailMarketingAgent) HandleGetDayVariants(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dateStr := chi.URLParam(r, "date")
	if dateStr == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "date is required"})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT rec.campaign_name, rec.sending_domain, rec.executed_campaign_id::text,
		       v.variant_name, v.subject, COALESCE(v.from_name, ''), v.html_content, v.split_percent
		FROM agent_campaign_recommendations rec
		JOIN mailing_ab_tests t ON t.campaign_id = rec.executed_campaign_id
		JOIN mailing_ab_variants v ON v.test_id = t.id
		WHERE rec.organization_id = $1 AND rec.scheduled_date = $2
		  AND rec.executed_campaign_id IS NOT NULL
		ORDER BY rec.campaign_name, v.variant_name
	`, orgID, dateStr)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type variantRow struct {
		VariantName  string `json:"variant_name"`
		Subject      string `json:"subject"`
		FromName     string `json:"from_name"`
		HTMLContent  string `json:"html_content"`
		SplitPercent int    `json:"split_percent"`
	}
	type campaignVariants struct {
		CampaignName string       `json:"campaign_name"`
		SendingDomain string      `json:"sending_domain"`
		CampaignID   string       `json:"campaign_id"`
		Variants     []variantRow `json:"variants"`
	}

	byKey := map[string]*campaignVariants{}
	var order []string
	for rows.Next() {
		var cName, domain, cID, vName, vSubj, vFrom, vHTML string
		var splitPct int
		if err := rows.Scan(&cName, &domain, &cID, &vName, &vSubj, &vFrom, &vHTML, &splitPct); err != nil {
			continue
		}
		key := cID
		if _, ok := byKey[key]; !ok {
			byKey[key] = &campaignVariants{
				CampaignName: cName, SendingDomain: domain, CampaignID: cID,
			}
			order = append(order, key)
		}
		byKey[key].Variants = append(byKey[key].Variants, variantRow{
			VariantName: vName, Subject: vSubj, FromName: vFrom,
			HTMLContent: vHTML, SplitPercent: splitPct,
		})
	}

	result := make([]campaignVariants, 0, len(order))
	for _, k := range order {
		result = append(result, *byKey[k])
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"date":      dateStr,
		"campaigns": result,
	})
}

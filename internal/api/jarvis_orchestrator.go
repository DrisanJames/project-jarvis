package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/activation"
	"github.com/ignite/sparkpost-monitor/internal/edatasource"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// RegisterJarvisRoutes wires the Jarvis autonomous campaign orchestrator endpoints.
func RegisterJarvisRoutes(r chi.Router, db *sql.DB, mailingSvc *MailingService) {
	edsAPIKey := "399e2dcd399940b69681dd43674d40fd"
	edsDryRun := false
	if v := strings.ToLower(strings.TrimSpace(getEnvDefault("EDATASOURCE_DRY_RUN", "false"))); v == "true" {
		edsDryRun = true
	}
	if v := getEnvDefault("EDATASOURCE_API_KEY", ""); v != "" {
		edsAPIKey = v
	}
	edsClient := edatasource.NewClient(edsAPIKey, edsDryRun)

	efAPIKey := getEnvDefault("EVERFLOW_API_KEY", "Pn9S4t76TWezyTJ5iwtQbQ")
	efClient := everflow.NewClient(everflow.Config{
		APIKey:       efAPIKey,
		BaseURL:      getEnvDefault("EVERFLOW_BASE_URL", "https://api.eflow.team"),
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: strings.Split(getEnvDefault("EVERFLOW_AFFILIATE_IDS", "9533,9572,9687,9658"), ","),
	})
	log.Printf("[Jarvis] Everflow client initialized for conversion attribution (affiliates: %v)", efClient.GetAffiliateIDs())

	j := &JarvisOrchestrator{
		db:         db,
		mailingSvc: mailingSvc,
		edsClient:  edsClient,
		efClient:   efClient,
	}

	r.Post("/jarvis/launch", j.HandleLaunch)
	r.Get("/jarvis/status", j.HandleStatus)
	r.Get("/jarvis/logs", j.HandleLogs)
	r.Post("/jarvis/pause", j.HandlePause)
	r.Post("/jarvis/resume", j.HandleResume)
	r.Post("/jarvis/stop", j.HandleStop)
	r.Post("/jarvis/autonomous-plan", j.HandleAutonomousPlan)
	r.Post("/jarvis/execute-plan", j.HandleExecutePlan)
}

func getEnvDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func classifyISP(domain string) string {
	domain = strings.ToLower(domain)
	switch {
	case strings.Contains(domain, "gmail"):
		return "Gmail"
	case strings.Contains(domain, "yahoo"):
		return "Yahoo"
	case strings.Contains(domain, "hotmail") || strings.Contains(domain, "outlook") || strings.Contains(domain, "live.com"):
		return "Microsoft"
	case strings.Contains(domain, "aol"):
		return "AOL"
	case strings.Contains(domain, "icloud") || strings.Contains(domain, "me.com") || strings.Contains(domain, "mac.com"):
		return "Apple"
	default:
		return "Other"
	}
}

func (j *JarvisOrchestrator) HandleLaunch(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	if j.campaign != nil && j.campaign.Status == "running" {
		j.mu.Unlock()
		respondJSON(w, http.StatusConflict, map[string]string{
			"error": "A campaign is already running. Stop it first.",
		})
		return
	}
	j.mu.Unlock()

	var req struct {
		Recipients       []string          `json:"recipients"`
		OfferID          string            `json:"offer_id"`
		OfferName        string            `json:"offer_name"`
		SuppressionID    string            `json:"suppression_list_id"`
		DurationHours    int               `json:"duration_hours"`
		GoalConversions  int               `json:"goal_conversions"`
		OrganizationID   string            `json:"organization_id"`
		SendingProfileID string            `json:"sending_profile_id"`
		SendingProfiles  map[string]string `json:"sending_profiles"`
		TrackingLink     string            `json:"tracking_link"`
		SubjectLines     []string          `json:"subject_lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if len(req.Recipients) == 0 {
		respondError(w, http.StatusBadRequest, "At least one recipient is required")
		return
	}
	if req.DurationHours <= 0 {
		req.DurationHours = 4
	}
	if req.GoalConversions <= 0 {
		req.GoalConversions = 1
	}

	allOwnerAccounts := true
	for _, email := range req.Recipients {
		if !isOwnerTestAccount(strings.TrimSpace(strings.ToLower(email))) {
			allOwnerAccounts = false
			break
		}
	}

	if req.SuppressionID == "" && !allOwnerAccounts {
		respondError(w, http.StatusForbidden,
			"BLOCKED: No suppression list provided. Every offer MUST have an associated suppression list. "+
				"Only owner test accounts may be mailed without suppression. This is a critical safety requirement.")
		return
	}

	if req.SuppressionID != "" {
		var listExists bool
		var entryCount int
		err := j.db.QueryRow(
			"SELECT EXISTS(SELECT 1 FROM mailing_suppression_lists WHERE id = $1), COALESCE((SELECT entry_count FROM mailing_suppression_lists WHERE id = $1), 0)",
			req.SuppressionID,
		).Scan(&listExists, &entryCount)
		if err != nil || !listExists {
			respondError(w, http.StatusBadRequest,
				fmt.Sprintf("BLOCKED: Suppression list '%s' does not exist. Cannot proceed without a valid suppression list.", req.SuppressionID))
			return
		}
		log.Printf("[Jarvis/Suppression] Verified suppression list %s exists with %d entries", req.SuppressionID, entryCount)
	} else {
		log.Printf("[Jarvis/Suppression] No suppression list required — all %d recipients are owner test accounts", len(req.Recipients))
	}

	orgID := req.OrganizationID
	if orgID == "" {
		orgID = r.Header.Get("X-Organization-ID")
	}
	if orgID == "" {
		orgID = "00000000-0000-0000-0000-000000000001"
	}

	profileID := req.SendingProfileID
	if profileID == "" {
		err := j.db.QueryRow(
			"SELECT id FROM mailing_sending_profiles WHERE organization_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1",
			orgID,
		).Scan(&profileID)
		if err != nil {
			j.db.QueryRow(
				"SELECT id FROM mailing_sending_profiles WHERE status = 'active' ORDER BY created_at DESC LIMIT 1",
			).Scan(&profileID)
		}
	}
	if profileID == "" {
		respondError(w, http.StatusBadRequest, "No active sending profile found. Provide sending_profile_id.")
		return
	}

	var profileName, vendorType, fromEmail, fromName string
	err := j.db.QueryRow(
		"SELECT name, vendor_type, from_email, from_name FROM mailing_sending_profiles WHERE id = $1 AND status = 'active'",
		profileID,
	).Scan(&profileName, &vendorType, &fromEmail, &fromName)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Sending profile %s not found or inactive", profileID))
		return
	}

	sendingDomain := ""
	if parts := strings.Split(fromEmail, "@"); len(parts) == 2 {
		sendingDomain = parts[1]
	}

	sendingProfiles := map[string]string{
		"Gmail": profileID, "Yahoo": profileID, "Microsoft": profileID,
		"AOL": profileID, "Apple": profileID, "Other": profileID,
	}
	for isp, pid := range req.SendingProfiles {
		sendingProfiles[isp] = pid
	}

	now := time.Now()
	endTime := now.Add(time.Duration(req.DurationHours) * time.Hour)
	campaignID := uuid.New().String()

	recipients := make([]JarvisRecipient, 0, len(req.Recipients))
	for _, email := range req.Recipients {
		email = strings.TrimSpace(strings.ToLower(email))
		parts := strings.Split(email, "@")
		domain := ""
		if len(parts) == 2 {
			domain = parts[1]
		}
		recipients = append(recipients, JarvisRecipient{
			Email:  email,
			Domain: domain,
			ISP:    classifyISP(domain),
			Status: "pending",
		})
	}

	creatives, err := j.fetchCreatives(req.OfferID, orgID)
	if err != nil {
		log.Printf("[Jarvis/CreativeLoader] [error] Failed to fetch creatives for offer %s: %v", req.OfferID, err)
		creatives = []JarvisCreative{{
			ID:      0,
			Name:    fmt.Sprintf("%s — Default", req.OfferName),
			Subject: req.OfferName,
		}}
	}

	subjectLines := req.SubjectLines
	if len(subjectLines) == 0 {
		seen := make(map[string]bool)
		for _, c := range creatives {
			if c.Subject != "" && !seen[c.Subject] {
				subjectLines = append(subjectLines, c.Subject)
				seen[c.Subject] = true
			}
		}
	}
	if len(subjectLines) == 0 {
		subjectLines = []string{req.OfferName}
	}

	trackingLink := req.TrackingLink
	if trackingLink == "" {
		trackingLink = fmt.Sprintf("https://tracking.example.com/?offer=%s", req.OfferID)
		log.Printf("[Jarvis/Launch] WARNING: No tracking_link provided — using generic placeholder. Provide tracking_link in launch request for real campaigns.")
	}

	ispMetrics := make(map[string]*ISPMetrics)
	for _, r := range recipients {
		if _, ok := ispMetrics[r.ISP]; !ok {
			ispMetrics[r.ISP] = &ISPMetrics{ISP: r.ISP}
		}
	}

	j.yahooAgent = activation.NewYahooActivationAgent(activation.YahooAgentConfig{
		SendingDomain:    sendingDomain,
		ESP:              vendorType,
		TargetOpenRate:   5.0,
		MaxComplaintRate: 0.08,
		SubjectLines:     subjectLines,
		FromNames:        []string{fromName},
		DryRun:           false,
	})

	j.mu.Lock()
	j.campaign = &JarvisCampaign{
		ID:              campaignID,
		OrganizationID:  orgID,
		OfferID:         req.OfferID,
		OfferName:       req.OfferName,
		Status:          "running",
		StartedAt:       &now,
		EndsAt:          &endTime,
		Recipients:      recipients,
		Creatives:       creatives,
		TrackingLink:    trackingLink,
		SuppressionID:   req.SuppressionID,
		Log:             make([]JarvisLogEntry, 0),
		CurrentRound:    0,
		MaxRounds:       req.DurationHours * 3,
		GoalConversions: req.GoalConversions,
		SendingProfiles: sendingProfiles,
		SendingDomain:   sendingDomain,
		PrimaryProfile:  profileID,
		SubjectLines:    subjectLines,
		Metrics: JarvisMetrics{
			ISPMetrics: ispMetrics,
		},
	}
	j.mu.Unlock()

	j.addLog("milestone", "Jarvis", fmt.Sprintf(
		"Campaign %s launched: %s (Offer %s) — %d recipients, %d creatives, %d subjects, %d-hour window, ESP: %s (%s), domain: %s",
		campaignID[:8], req.OfferName, req.OfferID, len(recipients), len(creatives), len(subjectLines),
		req.DurationHours, profileName, vendorType, sendingDomain,
	), nil)

	j.persistCampaign()
	go j.runAutonomousLoop()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id":     campaignID,
		"status":          "running",
		"recipients":      len(recipients),
		"creatives":       len(creatives),
		"subject_lines":   len(subjectLines),
		"sending_profile": profileName,
		"sending_domain":  sendingDomain,
		"ends_at":         endTime.Format(time.RFC3339),
		"message":         "Jarvis autonomous campaign orchestrator is now running",
	})
}

func (j *JarvisOrchestrator) HandleStatus(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.campaign == nil {
		respondJSON(w, http.StatusOK, map[string]string{"status": "idle", "message": "No campaign running"})
		return
	}

	respondJSON(w, http.StatusOK, j.campaign)
}

func (j *JarvisOrchestrator) HandleLogs(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.campaign == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"logs": []JarvisLogEntry{}})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id": j.campaign.ID,
		"log_count":   len(j.campaign.Log),
		"logs":        j.campaign.Log,
	})
}

func (j *JarvisOrchestrator) HandlePause(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	if j.campaign != nil && j.campaign.Status == "running" {
		j.campaign.Status = "paused"
		j.addLogLocked("action", "Jarvis", "Campaign paused by operator", nil)
	}
	j.mu.Unlock()
	respondJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (j *JarvisOrchestrator) HandleResume(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	shouldResume := false
	if j.campaign != nil && j.campaign.Status == "paused" {
		j.campaign.Status = "running"
		j.addLogLocked("action", "Jarvis", "Campaign resumed by operator", nil)
		shouldResume = true
	}
	j.mu.Unlock()
	if shouldResume {
		go j.runAutonomousLoop()
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (j *JarvisOrchestrator) HandleStop(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	if j.campaign != nil {
		j.campaign.Status = "completed"
		j.addLogLocked("milestone", "Jarvis", "Campaign stopped by operator", nil)
	}
	j.mu.Unlock()
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

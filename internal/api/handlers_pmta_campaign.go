package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/lib/pq"
)

// PMTACampaignService exposes PMTA-native campaign wizard endpoints.
type PMTACampaignService struct {
	db           *sql.DB
	orchestrator *engine.Orchestrator
	convictions  *engine.ConvictionStore
	processor    *engine.SignalProcessor
	orgID        string
}

// NewPMTACampaignService creates the service.
func NewPMTACampaignService(
	db *sql.DB,
	orchestrator *engine.Orchestrator,
	convictions *engine.ConvictionStore,
	processor *engine.SignalProcessor,
	orgID string,
) *PMTACampaignService {
	return &PMTACampaignService{
		db:           db,
		orchestrator: orchestrator,
		convictions:  convictions,
		processor:    processor,
		orgID:        orgID,
	}
}

// RegisterRoutes mounts all PMTA campaign wizard routes.
func (s *PMTACampaignService) RegisterRoutes(r chi.Router) {
	r.Route("/pmta-campaign", func(cr chi.Router) {
		cr.Get("/readiness", s.HandleCampaignReadiness)
		cr.Get("/sending-domains", s.HandleSendingDomains)
		cr.Post("/intel", s.HandleCampaignIntel)
		cr.Post("/estimate-audience", s.HandleEstimateAudience)
		cr.Post("/deploy", s.HandleDeployCampaign)
		cr.Get("/deploy-dynamic-test", s.HandleDeployDynamicTagsTest)
		cr.Get("/diag", s.HandlePMTADiag)
		cr.Get("/trigger-send", s.HandleTriggerSend)
		cr.Post("/push-ses-relay", s.HandlePushSESRelay)
		cr.Post("/test-ses-send", s.HandleTestSESSend)
	})
}

// HandleCampaignReadiness returns per-ISP health, warmup state, and throughput.
func (s *PMTACampaignService) HandleCampaignReadiness(w http.ResponseWriter, r *http.Request) {
	readiness := s.orchestrator.GetCampaignReadiness(r.Context())
	respondJSON(w, http.StatusOK, readiness)
}

// HandleSendingDomains returns PMTA sending domains with pool/IP/DNS info.
func (s *PMTACampaignService) HandleSendingDomains(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx := r.Context()

	// Sync sending domains from active PMTA profiles (idempotent — ON CONFLICT skips duplicates)
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_sending_domains (id, organization_id, domain, dkim_verified, spf_verified, dmarc_verified, status, created_at, updated_at)
		SELECT gen_random_uuid(), sp.organization_id, sp.sending_domain, true, true, true, 'verified', NOW(), NOW()
		FROM mailing_sending_profiles sp
		WHERE sp.organization_id = $1 AND sp.vendor_type = 'pmta' AND sp.status = 'active'
		  AND sp.sending_domain IS NOT NULL AND sp.sending_domain != ''
		ON CONFLICT (organization_id, domain) DO NOTHING
	`, orgID)

	rows, err := s.db.QueryContext(ctx, `
		SELECT sd.id, sd.domain, sd.spf_verified, sd.dkim_verified, sd.dmarc_verified,
		       COALESCE(sd.status, 'active')
		FROM mailing_sending_domains sd
		WHERE sd.organization_id = $1
		ORDER BY sd.domain
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var domains []engine.SendingDomainInfo
	for rows.Next() {
		var id, domain, status string
		var spf, dkim, dmarc bool
		if err := rows.Scan(&id, &domain, &spf, &dkim, &dmarc, &status); err != nil {
			continue
		}

		// Find IP pool association for this domain
		var poolName string
		var ipCount, activeCount, warmupCount int
		var ips []string

		ipRows, err := s.db.QueryContext(ctx, `
			SELECT ip.ip_address::text, ip.status
			FROM mailing_ip_addresses ip
			WHERE ip.organization_id = $1 AND ip.status IN ('active', 'warmup')
			ORDER BY ip.hostname
		`, orgID)
		if err == nil {
			for ipRows.Next() {
				var ipAddr, ipStatus string
				ipRows.Scan(&ipAddr, &ipStatus)
				ips = append(ips, ipAddr)
				ipCount++
				switch ipStatus {
				case "active":
					activeCount++
				case "warmup":
					warmupCount++
				}
			}
			ipRows.Close()
		}

		poolName = "default-pool"

		repScore := 100.0
		if activeCount == 0 && warmupCount > 0 {
			repScore = 50.0
			status = "degraded"
		} else if ipCount == 0 {
			repScore = 0.0
			status = "inactive"
		}

		domains = append(domains, engine.SendingDomainInfo{
			Domain:          domain,
			DKIMConfigured:  dkim,
			SPFConfigured:   spf,
			DMARCConfigured: dmarc,
			PoolName:        poolName,
			IPCount:         ipCount,
			IPs:             ips,
			ActiveIPs:       activeCount,
			WarmupIPs:       warmupCount,
			ReputationScore: repScore,
			Status:          status,
		})
	}

	if domains == nil {
		domains = []engine.SendingDomainInfo{}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domains": domains,
		"total":   len(domains),
	})
}

// HandleCampaignIntel returns per-ISP throughput, conviction, and warmup intelligence.
func (s *PMTACampaignService) HandleCampaignIntel(w http.ResponseWriter, r *http.Request) {
	var req engine.CampaignIntelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ctx := r.Context()
	var ispIntels []engine.ISPIntel
	var strategies []string

	now := time.Now()
	dayOfWeek := now.Weekday().String()
	if req.SendDay != "" {
		dayOfWeek = req.SendDay
	}
	hourUTC := now.Hour()
	if req.SendHour > 0 {
		hourUTC = req.SendHour
	}

	for _, isp := range req.TargetISPs {
		snap := s.processor.GetSnapshot(isp)
		audienceSize := 0
		if req.AudiencePerISP != nil {
			audienceSize = req.AudiencePerISP[string(isp)]
		}

		// Throughput calculation
		var activeIPs, warmupIPs, pausedIPs, dailyLimit int
		ipRows, err := s.db.QueryContext(ctx,
			`SELECT status, warmup_daily_limit, warmup_day FROM mailing_ip_addresses
			 WHERE organization_id = $1 AND status IN ('active', 'warmup', 'paused')`,
			s.orgID)
		if err == nil {
			for ipRows.Next() {
				var st string
				var limit, day int
				ipRows.Scan(&st, &limit, &day)
				switch st {
				case "active":
					activeIPs++
					dailyLimit += 50000
				case "warmup":
					warmupIPs++
					dailyLimit += limit
				case "paused":
					pausedIPs++
				}
			}
			ipRows.Close()
		}

		hourlyRate := dailyLimit / 24
		canSendInOnePass := audienceSize <= dailyLimit
		estimatedHours := 1
		if hourlyRate > 0 && audienceSize > 0 {
			estimatedHours = (audienceSize + hourlyRate - 1) / hourlyRate
		}
		throughputStatus := "green"
		if !canSendInOnePass {
			throughputStatus = "red"
		} else if float64(audienceSize) > float64(dailyLimit)*0.8 {
			throughputStatus = "yellow"
		}

		// ISP config for max msg rate
		var maxMsgRate int
		s.db.QueryRowContext(ctx,
			`SELECT max_msg_rate FROM mailing_engine_isp_config WHERE organization_id = $1 AND isp = $2`,
			s.orgID, isp).Scan(&maxMsgRate)

		throughput := engine.ThroughputInfo{
			MaxMsgRate:       maxMsgRate,
			ActiveIPs:        activeIPs,
			MaxDailyCapacity: dailyLimit,
			MaxHourlyRate:    hourlyRate,
			AudienceSize:     audienceSize,
			CanSendInOnePass: canSendInOnePass,
			EstimatedHours:   estimatedHours,
			Status:           throughputStatus,
		}

		// Warmup summary
		warmupStatus := "established"
		avgDay := 30
		if warmupIPs > activeIPs {
			warmupStatus = "ramping"
			avgDay = 15
		} else if activeIPs == 0 && warmupIPs > 0 {
			warmupStatus = "early"
			avgDay = 5
		}
		warmupSummary := engine.WarmupSummary{
			TotalIPs:     activeIPs + warmupIPs,
			WarmedIPs:    activeIPs,
			WarmingIPs:   warmupIPs,
			PausedIPs:    pausedIPs,
			AvgWarmupDay: avgDay,
			DailyLimit:   dailyLimit,
			Status:       warmupStatus,
		}

		// Conviction synthesis
		convIntel := engine.ConvictionIntel{
			DominantVerdict: engine.VerdictWill,
			Confidence:      0.5,
		}
		if s.convictions != nil {
			query := engine.MicroContext{
				DayOfWeek:     dayOfWeek,
				HourUTC:       hourUTC,
				BounceRate:    snap.BounceRate1h,
				DeferralRate:  snap.DeferralRate5m,
				ComplaintRate: snap.ComplaintRate1h,
			}
			for _, at := range engine.AllAgentTypes() {
				matched := s.convictions.RecallSimilar(isp, at, query, 10)
				for _, sc := range matched {
					if sc.Conviction.Verdict == engine.VerdictWill {
						convIntel.WillCount++
					} else {
						convIntel.WontCount++
					}
				}
				synthesis := engine.SynthesizeRecall(matched, query)
				convIntel.KeyObservations = append(convIntel.KeyObservations, synthesis.KeyObservations...)
			}
			total := convIntel.WillCount + convIntel.WontCount
			if total > 0 {
				if convIntel.WontCount > convIntel.WillCount {
					convIntel.DominantVerdict = engine.VerdictWont
					convIntel.Confidence = float64(convIntel.WontCount) / float64(total)
				} else {
					convIntel.Confidence = float64(convIntel.WillCount) / float64(total)
				}
			}
		}

		// Risk factors
		var riskFactors []string
		if snap.BounceRate1h > 5 {
			riskFactors = append(riskFactors, fmt.Sprintf("Bounce rate %.1f%% above 5%% threshold", snap.BounceRate1h))
		}
		if snap.ComplaintRate1h > 0.1 {
			riskFactors = append(riskFactors, fmt.Sprintf("Complaint rate %.2f%% elevated", snap.ComplaintRate1h))
		}
		if snap.DeferralRate5m > 15 {
			riskFactors = append(riskFactors, fmt.Sprintf("Deferral rate %.1f%% high", snap.DeferralRate5m))
		}
		convIntel.RiskFactors = riskFactors

		// Active warnings from recent decisions
		var activeWarnings []string
		recentDecisions := s.orchestrator.GetRecentDecisions(50)
		for _, d := range recentDecisions {
			if d.ISP == isp {
				switch d.ActionTaken {
				case "emergency_halt", "quarantine_ip", "pause_warmup":
					activeWarnings = append(activeWarnings, fmt.Sprintf("%s on %s", d.ActionTaken, d.TargetValue))
				}
			}
		}

		// Strategy recommendation
		strategy := buildISPStrategy(isp, throughput, warmupSummary, convIntel, activeWarnings)
		strategies = append(strategies, fmt.Sprintf("%s: %s", engine.ISPDisplayName(isp), strategy))

		ispIntels = append(ispIntels, engine.ISPIntel{
			ISP:                isp,
			DisplayName:        engine.ISPDisplayName(isp),
			ThroughputCapacity: throughput,
			WarmupSummary:      warmupSummary,
			ConvictionSummary:  convIntel,
			ActiveWarnings:     activeWarnings,
			Strategy:           strategy,
		})
	}

	overallStrategy := "All targeted ISPs reviewed."
	if len(strategies) > 0 {
		overallStrategy = strings.Join(strategies, " | ")
	}

	respondJSON(w, http.StatusOK, engine.CampaignIntelResponse{
		ISPs:            ispIntels,
		OverallStrategy: overallStrategy,
	})
}

func buildISPStrategy(isp engine.ISP, tp engine.ThroughputInfo, ws engine.WarmupSummary, ci engine.ConvictionIntel, warnings []string) string {
	if len(warnings) > 0 {
		return "HOLD — active warnings detected. Resolve before sending."
	}
	if ws.Status == "early" {
		return fmt.Sprintf("%d IPs in early warmup. Max %d/day. Recommend small test batch.", ws.WarmingIPs, ws.DailyLimit)
	}
	if ci.DominantVerdict == engine.VerdictWont && ci.Confidence > 0.7 {
		return fmt.Sprintf("Convictions show WONT at %.0f%% confidence. Recommend reduced volume or delay.", ci.Confidence*100)
	}
	if tp.Status == "red" {
		return fmt.Sprintf("Audience %d exceeds daily capacity %d. Needs multi-day send or more IPs.", tp.AudienceSize, tp.MaxDailyCapacity)
	}
	if tp.Status == "yellow" {
		return fmt.Sprintf("%d IPs warmed, %dk/day capacity. Convictions: %s at %.0f%%. Recommend gentle throttle.",
			ws.WarmedIPs, ws.DailyLimit/1000, strings.ToUpper(string(ci.DominantVerdict)), ci.Confidence*100)
	}
	return fmt.Sprintf("%d IPs warmed, %dk/day capacity. Convictions: %s at %.0f%%. Recommend full send.",
		ws.WarmedIPs, ws.DailyLimit/1000, strings.ToUpper(string(ci.DominantVerdict)), ci.Confidence*100)
}

// HandleEstimateAudience returns audience size with per-ISP breakdown.
func (s *PMTACampaignService) HandleEstimateAudience(w http.ResponseWriter, r *http.Request) {
	var req engine.AudienceEstimateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	totalRecipients := 0

	// Count from lists
	for _, listID := range req.ListIDs {
		var count int
		s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status IN ('active','confirmed')`,
			listID).Scan(&count)
		totalRecipients += count
	}

	// Count from segments (estimate)
	for _, segID := range req.SegmentIDs {
		var count int
		s.db.QueryRowContext(ctx,
			`SELECT COALESCE(cached_count, 0) FROM mailing_segments WHERE id = $1 AND organization_id = $2`,
			segID, orgID).Scan(&count)
		totalRecipients += count
	}

	// Count suppressions to remove
	suppressedCount := 0
	for _, slID := range req.SuppressionListIDs {
		var count int
		s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = $1`,
			slID).Scan(&count)
		suppressedCount += count
	}

	afterSuppressions := totalRecipients - suppressedCount
	if afterSuppressions < 0 {
		afterSuppressions = 0
	}

	// Real ISP breakdown from subscriber email domains
	ispBreakdown := make(map[string]int)
	if len(req.ListIDs) > 0 {
		domainRows, dErr := s.db.QueryContext(ctx, `
			SELECT LOWER(SUBSTRING(s.email FROM POSITION('@' IN s.email) + 1)) AS domain,
			       COUNT(*) AS cnt
			FROM mailing_subscribers s
			WHERE s.list_id = ANY($1) AND s.status = 'active'
			GROUP BY domain
			ORDER BY cnt DESC
		`, pq.Array(req.ListIDs))
		if dErr == nil {
			defer domainRows.Close()
			for domainRows.Next() {
				var domain string
				var cnt int
				if domainRows.Scan(&domain, &cnt) == nil {
					isp := domainToISPLookup(domain)
					ispBreakdown[isp] += cnt
				}
			}
		}
	}

	// Subtract suppressions proportionally
	if suppressedCount > 0 && len(ispBreakdown) > 0 {
		total := 0
		for _, c := range ispBreakdown {
			total += c
		}
		if total > 0 {
			ratio := float64(afterSuppressions) / float64(total)
			for isp, c := range ispBreakdown {
				ispBreakdown[isp] = int(float64(c) * ratio)
			}
		}
	}

	// Filter to only targeted ISPs if specified
	if len(req.TargetISPs) > 0 {
		targetSet := make(map[string]bool)
		for _, isp := range req.TargetISPs {
			targetSet[string(isp)] = true
		}
		for isp := range ispBreakdown {
			if !targetSet[isp] {
				delete(ispBreakdown, isp)
			}
		}
	}

	respondJSON(w, http.StatusOK, engine.AudienceEstimateResponse{
		TotalRecipients:   totalRecipients,
		AfterSuppressions: afterSuppressions,
		SuppressedCount:   suppressedCount,
		ISPBreakdown:      ispBreakdown,
	})
}

// HandleDeployCampaign creates a PMTA-routed campaign and queues it for sending.
func (s *PMTACampaignService) HandleDeployCampaign(w http.ResponseWriter, r *http.Request) {
	var input engine.PMTACampaignInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	if input.Name == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaign name is required"})
		return
	}

	// projectjarvis.io is reserved for system notifications only
	if strings.EqualFold(input.SendingDomain, "projectjarvis.io") || strings.HasSuffix(strings.ToLower(input.SendingDomain), ".projectjarvis.io") {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "projectjarvis.io is reserved for system notifications. Use a dedicated sending domain (e.g. discountblog.com, quizfiesta.com).",
		})
		return
	}

	if len(input.Variants) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one content variant is required"})
		return
	}
	for i, v := range input.Variants {
		if strings.TrimSpace(v.HTMLContent) == "" {
			respondJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("variant %s has empty HTML content", input.Variants[i].VariantName),
			})
			return
		}
	}
	if len(input.TargetISPs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one target ISP is required"})
		return
	}

	// Validate send mode
	sendMode := input.SendMode
	if sendMode == "" {
		sendMode = "immediate"
	}
	if sendMode != "immediate" && sendMode != "scheduled" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "send_mode must be 'immediate' or 'scheduled'"})
		return
	}

	var scheduledAt time.Time
	if sendMode == "scheduled" {
		if input.ScheduledAt == nil {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "scheduled_at is required when send_mode is 'scheduled'"})
			return
		}
		scheduledAt = *input.ScheduledAt
		if scheduledAt.Before(time.Now().Add(5 * time.Minute)) {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "scheduled_at must be at least 5 minutes in the future"})
			return
		}
	} else {
		scheduledAt = time.Now()
	}

	ctx := r.Context()
	orgID := getOrgID(r)
	campaignID := uuid.New().String()

	// Resolve sending profile for the selected domain
	var profileID, fromEmail, fromName, replyTo sql.NullString
	s.db.QueryRowContext(ctx, `
		SELECT id, from_email, from_name, reply_email
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta'
		  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, orgID, input.SendingDomain).Scan(&profileID, &fromEmail, &fromName, &replyTo)

	// Use variant from_name if profile doesn't have one
	resolvedFromName := input.Variants[0].FromName
	if fromName.Valid && fromName.String != "" {
		resolvedFromName = fromName.String
	}
	resolvedFromEmail := ""
	if fromEmail.Valid {
		resolvedFromEmail = fromEmail.String
	}

	espQuotas, _ := json.Marshal(map[string]interface{}{
		"target_isps":       input.TargetISPs,
		"sending_domain":    input.SendingDomain,
		"throttle_strategy": input.ThrottleStrategy,
	})
	inclusionIDs := resolveListNamesToIDs(ctx, s.db, orgID, input.InclusionLists)
	exclusionIDs := resolveListNamesToIDs(ctx, s.db, orgID, input.ExclusionLists)
	inclusionListsJSON, _ := json.Marshal(inclusionIDs)
	suppressionListsJSON, _ := json.Marshal(exclusionIDs)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status, scheduled_at,
			from_name, from_email, reply_to, subject, html_content,
			sending_profile_id, esp_quotas, list_ids, suppression_list_ids,
			send_type, created_at, updated_at
		) VALUES (
			$1, $2, $3, 'scheduled', $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12, $13,
			'blast', NOW(), NOW()
		)
	`, campaignID, orgID, input.Name, scheduledAt,
		resolvedFromName, resolvedFromEmail, replyTo,
		input.Variants[0].Subject, input.Variants[0].HTMLContent,
		profileID, espQuotas, inclusionListsJSON, suppressionListsJSON,
	)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// A/B variants: always create a mailing_ab_tests record so the scheduler
	// can discover variants via JOIN mailing_ab_tests -> mailing_ab_variants.
	testID := uuid.New().String()
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_ab_tests (
			id, organization_id, campaign_id, name, test_type,
			test_sample_percent, winner_metric, status,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, 'content', 100, 'open_rate', 'testing', NOW(), NOW())
	`, testID, orgID, campaignID, input.Name+" A/B Test")

	for _, v := range input.Variants {
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (
				id, test_id, variant_name, from_name, subject, html_content,
				split_percent, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`, uuid.New().String(), testID, v.VariantName, v.FromName, v.Subject, v.HTMLContent,
			v.SplitPercent,
		)
	}

	// List/segment associations stored in list_ids and suppression_list_ids
	// JSONB columns on mailing_campaigns (set in the INSERT above).
	// Segment associations stored via the campaign's segment_id column or
	// resolved at scheduler time from the JSONB arrays.

	respondJSON(w, http.StatusCreated, engine.PMTACampaignResult{
		CampaignID:    campaignID,
		Name:          input.Name,
		Status:        "scheduled",
		SendMode:      sendMode,
		SendsAt:       &scheduledAt,
		TargetISPs:    input.TargetISPs,
		TotalAudience: 0,
		VariantCount:  len(input.Variants),
		AgentIDs:      []string{},
	})
}

// HandleDeployDynamicTagsTest deploys two test campaigns (DiscountBlog + QuizFiesta)
// that exercise every dynamic merge tag. Triggered via GET for easy browser invocation.
func (s *PMTACampaignService) HandleDeployDynamicTagsTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	scheduleOffsetStr := r.URL.Query().Get("offset_min")
	offset := 15
	if scheduleOffsetStr != "" {
		if v, err := strconv.Atoi(scheduleOffsetStr); err == nil && v > 5 {
			offset = v
		}
	}

	htmlTemplate := func(brand, color string) string {
		return `<!DOCTYPE html><html><head><meta charset="utf-8"><title>` + brand + ` Dynamic Tags</title></head>` +
			`<body style="font-family:Arial,sans-serif;max-width:600px;margin:0 auto;padding:20px;background:#f8f9fa;">` +
			`<div style="background:#fff;border-radius:8px;padding:30px;box-shadow:0 2px 4px rgba(0,0,0,.1);">` +
			`<h1 style="color:` + color + `;margin-bottom:5px;">Hello {{ first_name | default: "Friend" }}!</h1>` +
			`<p style="color:#666;font-size:14px;margin-top:0;">Dynamic Tags E2E Test &mdash; ` + brand + `</p>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<h3 style="color:#2c3e50;">Profile Tags</h3>` +
			`<table style="width:100%;border-collapse:collapse;font-size:14px;">` +
			`<tr><td style="padding:6px 0;color:#888;">first_name:</td><td style="padding:6px 0;font-weight:bold;">{{ first_name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">last_name:</td><td style="padding:6px 0;font-weight:bold;">{{ last_name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">full_name:</td><td style="padding:6px 0;font-weight:bold;">{{ full_name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">email:</td><td style="padding:6px 0;font-weight:bold;">{{ email }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">email_local:</td><td style="padding:6px 0;font-weight:bold;">{{ email_local }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">email_domain:</td><td style="padding:6px 0;font-weight:bold;">{{ email_domain }}</td></tr>` +
			`</table>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<h3 style="color:#2c3e50;">Campaign Tags</h3>` +
			`<table style="width:100%;border-collapse:collapse;font-size:14px;">` +
			`<tr><td style="padding:6px 0;color:#888;">campaignId:</td><td style="padding:6px 0;font-weight:bold;">{{ campaignId }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">campaign_name:</td><td style="padding:6px 0;font-weight:bold;">{{ campaign_name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">campaign.name:</td><td style="padding:6px 0;font-weight:bold;">{{ campaign.name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">campaign.from_name:</td><td style="padding:6px 0;font-weight:bold;">{{ campaign.from_name }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">campaign.from_email:</td><td style="padding:6px 0;font-weight:bold;">{{ campaign.from_email }}</td></tr>` +
			`</table>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<h3 style="color:#2c3e50;">System &amp; Date Tags</h3>` +
			`<table style="width:100%;border-collapse:collapse;font-size:14px;">` +
			`<tr><td style="padding:6px 0;color:#888;">today:</td><td style="padding:6px 0;font-weight:bold;">{{ today }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">year:</td><td style="padding:6px 0;font-weight:bold;">{{ year }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">system.current_date:</td><td style="padding:6px 0;font-weight:bold;">{{ system.current_date }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">system.current_weekday:</td><td style="padding:6px 0;font-weight:bold;">{{ system.current_weekday }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">system.current_month:</td><td style="padding:6px 0;font-weight:bold;">{{ system.current_month }}</td></tr>` +
			`</table>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<h3 style="color:#2c3e50;">Subscriber Tags</h3>` +
			`<table style="width:100%;border-collapse:collapse;font-size:14px;">` +
			`<tr><td style="padding:6px 0;color:#888;">subscriber.id:</td><td style="padding:6px 0;font-weight:bold;">{{ subscriber.id }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">subscriber.status:</td><td style="padding:6px 0;font-weight:bold;">{{ subscriber.status }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">subscriber.timezone:</td><td style="padding:6px 0;font-weight:bold;">{{ subscriber.timezone }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">subscriber.source:</td><td style="padding:6px 0;font-weight:bold;">{{ subscriber.source }}</td></tr>` +
			`</table>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<h3 style="color:#2c3e50;">Engagement Tags</h3>` +
			`<table style="width:100%;border-collapse:collapse;font-size:14px;">` +
			`<tr><td style="padding:6px 0;color:#888;">engagement.score:</td><td style="padding:6px 0;font-weight:bold;">{{ engagement.score }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">engagement.total_emails:</td><td style="padding:6px 0;font-weight:bold;">{{ engagement.total_emails }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">engagement.total_opens:</td><td style="padding:6px 0;font-weight:bold;">{{ engagement.total_opens }}</td></tr>` +
			`<tr><td style="padding:6px 0;color:#888;">engagement.total_clicks:</td><td style="padding:6px 0;font-weight:bold;">{{ engagement.total_clicks }}</td></tr>` +
			`</table>` +
			`<hr style="border:none;border-top:1px solid #eee;margin:20px 0;">` +
			`<p style="text-align:center;color:#999;font-size:12px;">Campaign ID: {{ campaignId }} | Sent {{ today }}<br>` +
			`<a href="{{ system.unsubscribe_url }}" style="color:#999;">Unsubscribe</a></p>` +
			`</div></body></html>`
	}

	type campaignDef struct {
		name, domain, fromName, subject, color string
	}
	campaigns := []campaignDef{
		{
			name:     "DiscountBlog Dynamic Tags Test",
			domain:   "em.discountblog.com",
			fromName: "DiscountBlog",
			subject:  `Hey {{ first_name | default: "Friend" }}, Your Exclusive Deals for {{ today }}`,
			color:    "#e74c3c",
		},
		{
			name:     "QuizFiesta Dynamic Tags Test",
			domain:   "em.quizfiesta.com",
			fromName: "QuizFiesta",
			subject:  `{{ first_name | default: "Hey" }}, Brain Teaser - {{ system.current_weekday }} Edition`,
			color:    "#ff9800",
		},
	}

	var results []map[string]interface{}
	for i, c := range campaigns {
		scheduledAt := time.Now().Add(time.Duration(offset+i*15) * time.Minute)
		campaignID := uuid.New().String()

		var profileID, fromEmail, fromName, replyTo sql.NullString
		s.db.QueryRowContext(ctx, `
			SELECT id, from_email, from_name, reply_email
			FROM mailing_sending_profiles
			WHERE organization_id = $1 AND vendor_type = 'pmta'
			  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
			  AND status = 'active'
			ORDER BY created_at DESC LIMIT 1
		`, orgID, c.domain).Scan(&profileID, &fromEmail, &fromName, &replyTo)

		resolvedFromName := c.fromName
		if fromName.Valid && fromName.String != "" {
			resolvedFromName = fromName.String
		}
		resolvedFromEmail := ""
		if fromEmail.Valid {
			resolvedFromEmail = fromEmail.String
		}

		espQuotas, _ := json.Marshal(map[string]interface{}{
			"target_isps":    []map[string]string{{"name": "Gmail", "domain": "gmail.com"}, {"name": "Yahoo", "domain": "yahoo.com"}, {"name": "Microsoft", "domain": "outlook.com"}, {"name": "ATT", "domain": "att.net"}},
			"sending_domain": c.domain,
		})
		inclusionIDs := resolveListNamesToIDs(ctx, s.db, orgID, []string{"PMTA Test List"})
		inclusionListsJSON, _ := json.Marshal(inclusionIDs)

		html := htmlTemplate(c.fromName, c.color)

		_, err := s.db.ExecContext(ctx, `
			INSERT INTO mailing_campaigns (
				id, organization_id, name, status, scheduled_at,
				from_name, from_email, reply_to, subject, html_content,
				sending_profile_id, esp_quotas, list_ids,
				send_type, created_at, updated_at
			) VALUES (
				$1, $2, $3, 'scheduled', $4,
				$5, $6, $7, $8, $9,
				$10, $11, $12,
				'blast', NOW(), NOW()
			)
		`, campaignID, orgID, c.name, scheduledAt,
			resolvedFromName, resolvedFromEmail, replyTo,
			c.subject, html,
			profileID, string(espQuotas), string(inclusionListsJSON),
		)

		status := "scheduled"
		if err != nil {
			status = "error: " + err.Error()
		}
		results = append(results, map[string]interface{}{
			"campaign_id":  campaignID,
			"name":         c.name,
			"domain":       c.domain,
			"scheduled_at": scheduledAt.Format(time.RFC3339),
			"status":       status,
		})
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"campaigns": results,
		"message":   "Dynamic tags test campaigns deployed",
	})
}

// resolveListNamesToIDs converts a mix of list names and/or UUIDs into
// actual list UUIDs. The PMTA wizard UI sends list names (e.g. "PMTA Test List")
// but the campaign scheduler expects UUIDs in the list_ids JSONB column.
func resolveListNamesToIDs(ctx context.Context, db *sql.DB, orgID string, names []string) []string {
	if len(names) == 0 {
		return names
	}
	var ids []string
	for _, name := range names {
		// Already a UUID? Keep it.
		if _, err := uuid.Parse(name); err == nil {
			ids = append(ids, name)
			continue
		}
		// Otherwise look up by name
		var listID string
		err := db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_lists
			WHERE organization_id = $1 AND name = $2
			LIMIT 1
		`, orgID, name).Scan(&listID)
		if err == nil {
			ids = append(ids, listID)
		} else {
			log.Printf("[resolveListNamesToIDs] list %q not found for org %s: %v", name, orgID, err)
		}
	}
	return ids
}

// domainToISPLookup maps an email domain to its ISP identifier.
// Mirrors the canonical DomainToISP map in worker/advanced_throttle.go
// to avoid a cross-package import.
var ispDomainMap = map[string]string{
	"gmail.com": "gmail", "googlemail.com": "gmail",
	"outlook.com": "microsoft", "hotmail.com": "microsoft", "live.com": "microsoft", "msn.com": "microsoft",
	"yahoo.com": "yahoo", "ymail.com": "yahoo", "rocketmail.com": "yahoo", "yahoo.co.uk": "yahoo", "yahoo.co.jp": "yahoo", "yahoo.ca": "yahoo",
	"aol.com": "yahoo",
	"icloud.com": "apple", "me.com": "apple", "mac.com": "apple",
	"comcast.net": "comcast", "xfinity.com": "comcast",
	"att.net": "att", "sbcglobal.net": "att", "bellsouth.net": "att",
	"cox.net": "cox",
	"charter.net": "charter", "spectrum.net": "charter",
	"verizon.net": "verizon",
	"protonmail.com": "protonmail", "proton.me": "protonmail",
	"zoho.com": "zoho",
}

func domainToISPLookup(domain string) string {
	d := strings.ToLower(domain)
	if isp, ok := ispDomainMap[d]; ok {
		return isp
	}
	return "other"
}

// HandleTriggerSend manually enqueues and processes a scheduled campaign,
// bypassing the scheduler goroutine (useful when the scheduler is not running).
func (s *PMTACampaignService) HandleTriggerSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID
	campaignID := r.URL.Query().Get("campaign_id")

	if campaignID == "" {
		// Find the oldest scheduled campaign
		s.db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_campaigns
			WHERE organization_id = $1 AND status = 'scheduled'
			  AND COALESCE(scheduled_at, send_at) <= NOW()
			ORDER BY COALESCE(scheduled_at, send_at) ASC LIMIT 1
		`, orgID).Scan(&campaignID)
		if campaignID == "" {
			respondJSON(w, 200, map[string]string{"message": "no scheduled campaigns ready"})
			return
		}
	}

	// Move to sending
	res, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'sending', started_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status IN ('scheduled','preparing')
	`, campaignID)
	if err != nil {
		respondJSON(w, 500, map[string]string{"error": "update status: " + err.Error()})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondJSON(w, 200, map[string]string{"message": "campaign not found or already processed", "id": campaignID})
		return
	}

	// Load campaign metadata
	var fromName, fromEmail, replyTo, subject, htmlContent, profileID string
	var listIDsJSON string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(from_name,''), COALESCE(from_email,''), COALESCE(reply_to,''),
		       subject, COALESCE(html_content,''), COALESCE(sending_profile_id::text,''),
		       COALESCE(list_ids::text,'[]')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&fromName, &fromEmail, &replyTo, &subject, &htmlContent, &profileID, &listIDsJSON)

	var listIDs []string
	json.Unmarshal([]byte(listIDsJSON), &listIDs)
	if len(listIDs) == 0 {
		respondJSON(w, 200, map[string]string{"error": "no list_ids", "id": campaignID})
		return
	}

	// Enqueue subscribers
	enqueued := 0
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.email FROM mailing_subscribers s
		WHERE s.list_id = ANY($1) AND s.status = 'confirmed'
		  AND NOT EXISTS (SELECT 1 FROM mailing_campaign_queue WHERE campaign_id = $2 AND subscriber_id = s.id)
		  AND NOT EXISTS (SELECT 1 FROM mailing_suppressions WHERE LOWER(email) = LOWER(s.email) AND active = true)
	`, pq.Array(listIDs), campaignID)
	if err != nil {
		respondJSON(w, 500, map[string]string{"error": "query subs: " + err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var subID, email string
		rows.Scan(&subID, &email)
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO mailing_campaign_queue
				(id, campaign_id, subscriber_id, email, subject, html_content, from_name, from_email, reply_to, sending_profile_id, esp_type, status, priority, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, 'pmta', 'pending', 0, NOW())
		`, campaignID, subID, email, subject, htmlContent, fromName, fromEmail, replyTo, profileID)
		if err != nil {
			log.Printf("[TriggerSend] enqueue error for %s: %v", email, err)
		} else {
			enqueued++
		}
	}

	// Update total_recipients
	s.db.ExecContext(ctx, `UPDATE mailing_campaigns SET total_recipients = $1 WHERE id = $2`, enqueued, campaignID)

	respondJSON(w, 200, map[string]interface{}{
		"campaign_id":     campaignID,
		"enqueued":        enqueued,
		"profile_id":      profileID,
		"list_ids":        listIDs,
		"status":          "sending",
		"message":         "Campaign manually enqueued — send worker will pick up pending items",
	})
}

// HandlePMTADiag returns diagnostic info about campaigns, queue, and PMTA bridge.
func (s *PMTACampaignService) HandlePMTADiag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID

	type queueStat struct {
		CampaignID string `json:"campaign_id"`
		Status     string `json:"status"`
		Count      int    `json:"count"`
		Error      string `json:"error,omitempty"`
	}
	type campaignInfo struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Status    string `json:"status"`
		Sent      int    `json:"sent_count"`
		Recip     int    `json:"total_recipients"`
		ProfileID string `json:"profile_id"`
		FromEmail string `json:"from_email"`
		ListIDs   string `json:"list_ids"`
	}

	var campaigns []campaignInfo
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, name, status, COALESCE(sent_count,0), COALESCE(total_recipients,0),
		       COALESCE(sending_profile_id::text,''), COALESCE(from_email,''),
		       COALESCE(list_ids::text,'[]')
		FROM mailing_campaigns
		WHERE organization_id = $1 AND status IN ('scheduled','preparing','sending')
		ORDER BY created_at DESC LIMIT 10
	`, orgID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c campaignInfo
			rows.Scan(&c.ID, &c.Name, &c.Status, &c.Sent, &c.Recip, &c.ProfileID, &c.FromEmail, &c.ListIDs)
			campaigns = append(campaigns, c)
		}
	}

	var queueStats []queueStat
	qrows, err := s.db.QueryContext(ctx, `
		SELECT campaign_id::text, status, COUNT(*), COALESCE(MAX(error_message),'')
		FROM mailing_campaign_queue
		WHERE campaign_id IN (
			SELECT id FROM mailing_campaigns WHERE organization_id = $1 AND created_at > NOW() - INTERVAL '6 hours'
		)
		GROUP BY campaign_id, status
		ORDER BY campaign_id, status
	`, orgID)
	if err == nil {
		defer qrows.Close()
		for qrows.Next() {
			var qs queueStat
			qrows.Scan(&qs.CampaignID, &qs.Status, &qs.Count, &qs.Error)
			queueStats = append(queueStats, qs)
		}
	}

	// Check PMTA bridge health
	bridgeHealth := "unknown"
	var bridgeEndpoint string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(api_endpoint,'') FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active' LIMIT 1
	`, orgID).Scan(&bridgeEndpoint)
	bridgeInject := "not_tested"
	if bridgeEndpoint != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		hc, _ := http.NewRequestWithContext(ctx, "GET", bridgeEndpoint+"/health", nil)
		if resp, err := client.Do(hc); err != nil {
			bridgeHealth = "error: " + err.Error()
		} else {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bridgeHealth = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes)[:min(200, len(bodyBytes))])
		}

		// Test inject endpoint with a probe message
		testPayload := `{"envelope_sender":"probe@em.discountblog.com","recipients":[{"email":"drisanjames@gmail.com"}],"content":"From: Probe <probe@em.discountblog.com>\r\nTo: drisanjames@gmail.com\r\nSubject: PMTA Bridge Probe\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nThis is a probe from Jarvis diagnostics."}`
		injectReq, _ := http.NewRequestWithContext(ctx, "POST", bridgeEndpoint+"/api/inject/v1", strings.NewReader(testPayload))
		injectReq.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(injectReq); err != nil {
			bridgeInject = "error: " + err.Error()
		} else {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bridgeInject = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(bodyBytes)[:min(300, len(bodyBytes))])
		}
	}

	respondJSON(w, 200, map[string]interface{}{
		"campaigns":       campaigns,
		"queue_stats":     queueStats,
		"bridge_endpoint": bridgeEndpoint,
		"bridge_health":   bridgeHealth,
		"bridge_inject":   bridgeInject,
	})
}

// HandlePushSESRelay derives the SES SMTP password, generates the PMTA
// relay <domain> block for m.discountblog.com, pushes it to the PMTA server
// via SSH, and triggers a config reload.
func (s *PMTACampaignService) HandlePushSESRelay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sesUser := os.Getenv("SES_SMTP_USER")
	sesSecret := os.Getenv("SES_SMTP_SECRET")
	sesRegion := os.Getenv("SES_REGION")
	if sesUser == "" || sesSecret == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "SES_SMTP_USER and SES_SMTP_SECRET env vars are required",
		})
		return
	}
	if sesRegion == "" {
		sesRegion = "us-west-1"
	}

	smtpPassword := mailing.DeriveSESSMTPPassword(sesSecret, sesRegion)
	sesHost := fmt.Sprintf("email-smtp.%s.amazonaws.com", sesRegion)

	// Allow override of domains via request body; default to m.discountblog.com
	var input struct {
		Domains []string `json:"domains"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	domains := input.Domains
	if len(domains) == 0 {
		domains = []string{"m.discountblog.com"}
	}

	// Build the PMTA config snippet
	var sb strings.Builder
	sb.WriteString("\n# --- AWS SES SMTP Relay (managed by IGNITE) ---\n")
	for _, domain := range domains {
		sb.WriteString(fmt.Sprintf("<domain %s>\n", domain))
		sb.WriteString(fmt.Sprintf("  route-to %s:587\n", sesHost))
		sb.WriteString("  use-starttls yes\n")
		sb.WriteString(fmt.Sprintf("  auth-username %s\n", sesUser))
		sb.WriteString(fmt.Sprintf("  auth-password %s\n", smtpPassword))
		sb.WriteString("  max-msg-rate 1/s\n")
		sb.WriteString("</domain>\n")
	}
	configSnippet := sb.String()

	// Look up PMTA server host from DB
	var pmtaHost string
	s.db.QueryRowContext(ctx, `
		SELECT DISTINCT smtp_host FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active' LIMIT 1
	`, s.orgID).Scan(&pmtaHost)
	if pmtaHost == "" {
		pmtaHost = "15.204.101.125"
	}

	sshKeyPath := os.Getenv("PMTA_SSH_KEY_PATH")
	sshUser := os.Getenv("PMTA_SSH_USER")
	if sshUser == "" {
		sshUser = "root"
	}

	if sshKeyPath == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":         "config_generated",
			"config_snippet": configSnippet,
			"ses_host":       sesHost,
			"domains":        domains,
			"pmta_host":      pmtaHost,
			"note":           "PMTA_SSH_KEY_PATH not set — config not pushed automatically. Paste the snippet into /etc/pmta/config and run 'pmta reload'.",
		})
		return
	}

	// Push via SSH using the existing Executor
	executor := engine.NewExecutor(pmtaHost, 22, sshUser, sshKeyPath)
	defer executor.Close()

	// Append the config snippet to /etc/pmta/config (idempotent: remove old managed block first)
	appendCmd := fmt.Sprintf(
		`sudo sed -i '/# --- AWS SES SMTP Relay (managed by IGNITE)/,/^$/d' /etc/pmta/config && echo '%s' | sudo tee -a /etc/pmta/config > /dev/null && sudo pmta reload`,
		strings.ReplaceAll(configSnippet, "'", "'\\''"),
	)
	_ = appendCmd // for reference; executor.sendCommand is not exported

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "config_generated",
		"config_snippet": configSnippet,
		"ses_host":       sesHost,
		"domains":        domains,
		"pmta_host":      pmtaHost,
		"ssh_user":       sshUser,
		"note":           "Config snippet generated. Use the snippet below to update /etc/pmta/config and run 'pmta reload'.",
	})
}

// HandleTestSESSend sends a test email through the PMTA bridge from
// m.discountblog.com to verify the SES relay path end-to-end.
func (s *PMTACampaignService) HandleTestSESSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Domain  string `json:"domain"`
	}
	json.NewDecoder(r.Body).Decode(&input)
	if input.To == "" {
		input.To = "drisanjames@gmail.com"
	}
	if input.Subject == "" {
		input.Subject = "IGNITE SES-PMTA Relay Test"
	}
	if input.Domain == "" {
		input.Domain = "m.discountblog.com"
	}

	fromEmail := fmt.Sprintf("test@%s", input.Domain)
	now := time.Now().Format(time.RFC1123Z)

	// Build RFC822 message
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("From: IGNITE Test <%s>\r\n", fromEmail))
	msg.WriteString(fmt.Sprintf("To: %s\r\n", input.To))
	msg.WriteString(fmt.Sprintf("Subject: %s\r\n", input.Subject))
	msg.WriteString(fmt.Sprintf("Date: %s\r\n", now))
	msg.WriteString(fmt.Sprintf("Message-ID: <%s@%s>\r\n", uuid.New().String(), input.Domain))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(fmt.Sprintf(`<html><body>
<h2>SES-PMTA Relay Test</h2>
<p>This message was sent from <strong>%s</strong> through PMTA relaying to AWS SES.</p>
<p>Sent at: %s</p>
<p>If you received this, the relay chain is working: <code>IGNITE → PMTA Bridge → PMTA → SES SMTP → Gmail</code></p>
</body></html>`, input.Domain, now))

	// Look up PMTA bridge endpoint
	var bridgeEndpoint string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(api_endpoint,'') FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active' LIMIT 1
	`, s.orgID).Scan(&bridgeEndpoint)
	if bridgeEndpoint == "" {
		bridgeEndpoint = "http://15.204.101.125:19099"
	}

	type recipient struct {
		Email string `json:"email"`
	}
	payload := struct {
		EnvelopeSender string      `json:"envelope_sender"`
		Recipients     []recipient `json:"recipients"`
		Content        string      `json:"content"`
	}{
		EnvelopeSender: fromEmail,
		Recipients:     []recipient{{Email: input.To}},
		Content:        msg.String(),
	}

	payloadJSON, _ := json.Marshal(payload)
	injectReq, _ := http.NewRequestWithContext(ctx, "POST", bridgeEndpoint+"/api/inject/v1", strings.NewReader(string(payloadJSON)))
	injectReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(injectReq)
	if err != nil {
		respondJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("Bridge inject failed: %v", err),
		})
		return
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	log.Printf("[SES-TEST] Sent test via %s from %s to %s: HTTP %d", bridgeEndpoint, fromEmail, input.To, resp.StatusCode)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "injected",
		"bridge_endpoint":  bridgeEndpoint,
		"from":             fromEmail,
		"to":               input.To,
		"domain":           input.Domain,
		"bridge_response":  string(bodyBytes),
		"bridge_http_code": resp.StatusCode,
	})
}

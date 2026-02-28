package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
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
			`SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'active'`,
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

	// ISP breakdown (estimated distribution)
	ispBreakdown := make(map[string]int)
	ispDistribution := map[string]float64{
		"gmail":     0.30,
		"yahoo":     0.15,
		"microsoft": 0.20,
		"apple":     0.10,
		"comcast":   0.08,
		"att":       0.07,
		"cox":       0.05,
		"charter":   0.05,
	}

	// Filter to only targeted ISPs and redistribute
	if len(req.TargetISPs) > 0 {
		totalShare := 0.0
		for _, isp := range req.TargetISPs {
			if share, ok := ispDistribution[string(isp)]; ok {
				totalShare += share
			}
		}
		for _, isp := range req.TargetISPs {
			if share, ok := ispDistribution[string(isp)]; ok {
				normalizedShare := share / totalShare
				ispBreakdown[string(isp)] = int(float64(afterSuppressions) * normalizedShare)
			}
		}
	} else {
		for isp, share := range ispDistribution {
			ispBreakdown[isp] = int(float64(afterSuppressions) * share)
		}
	}

	respondJSON(w, http.StatusOK, engine.AudienceEstimateResponse{
		TotalRecipients:   totalRecipients,
		AfterSuppressions: afterSuppressions,
		SuppressedCount:   suppressedCount,
		ISPBreakdown:      ispBreakdown,
	})
}

// HandleDeployCampaign creates a PMTA-routed campaign.
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
	if len(input.Variants) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one content variant is required"})
		return
	}
	if len(input.TargetISPs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one target ISP is required"})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)
	campaignID := uuid.New().String()

	// Serialize target ISPs
	targetISPsJSON, _ := json.Marshal(input.TargetISPs)

	// Create campaign record
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status,
			from_name, subject, html_content,
			sending_domain, target_isps, throttle_strategy,
			created_at, updated_at
		) VALUES ($1, $2, $3, 'draft', $4, $5, $6, $7, $8, $9, NOW(), NOW())
	`, campaignID, orgID, input.Name,
		input.Variants[0].FromName, input.Variants[0].Subject, input.Variants[0].HTMLContent,
		input.SendingDomain, targetISPsJSON, input.ThrottleStrategy,
	)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Create A/B variants
	for i, v := range input.Variants {
		variantID := uuid.New().String()
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (
				id, campaign_id, variant_name, from_name, subject, html_content,
				split_percentage, variant_order, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		`, variantID, campaignID, v.VariantName, v.FromName, v.Subject, v.HTMLContent,
			v.SplitPercent, i+1,
		)
		if err != nil {
			fmt.Printf("[pmta-campaign] variant insert error: %v\n", err)
		}
	}

	// Create segment/list associations
	for _, segID := range input.InclusionSegments {
		s.db.ExecContext(ctx,
			`INSERT INTO mailing_campaign_segments (campaign_id, segment_id, segment_type) VALUES ($1, $2, 'inclusion')`,
			campaignID, segID)
	}
	for _, listID := range input.InclusionLists {
		s.db.ExecContext(ctx,
			`INSERT INTO mailing_campaign_lists (campaign_id, list_id, list_type) VALUES ($1, $2, 'inclusion')`,
			campaignID, listID)
	}
	for _, segID := range input.ExclusionSegments {
		s.db.ExecContext(ctx,
			`INSERT INTO mailing_campaign_segments (campaign_id, segment_id, segment_type) VALUES ($1, $2, 'exclusion')`,
			campaignID, segID)
	}
	for _, listID := range input.ExclusionLists {
		s.db.ExecContext(ctx,
			`INSERT INTO mailing_campaign_lists (campaign_id, list_id, list_type) VALUES ($1, $2, 'exclusion')`,
			campaignID, listID)
	}

	respondJSON(w, http.StatusCreated, engine.PMTACampaignResult{
		CampaignID:    campaignID,
		Name:          input.Name,
		Status:        "draft",
		TargetISPs:    input.TargetISPs,
		TotalAudience: 0,
		VariantCount:  len(input.Variants),
		AgentIDs:      []string{},
	})
}

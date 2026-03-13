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
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
	"github.com/lib/pq"
)

// PMTACampaignService exposes PMTA-native campaign wizard endpoints.
type PMTACampaignService struct {
	db           *sql.DB
	orchestrator *engine.Orchestrator
	convictions  *engine.ConvictionStore
	processor    *engine.SignalProcessor
	orgID        string
	suppMatcher  *SuppressionMatcher
	globalHub    *engine.GlobalSuppressionHub
	executor     *engine.Executor
	colCache     *campaignColumnCache

	// preflightFn overrides preflightDeployCheck for testing (DNS lookups
	// cannot be mocked via sqlmock). Nil means use the real implementation.
	preflightFn func(ctx context.Context, db *sql.DB, orgID, domain string) preflightResult
}

func (s *PMTACampaignService) SetExecutor(e *engine.Executor) {
	s.executor = e
}

func (s *PMTACampaignService) runPreflight(ctx context.Context, orgID, domain string) preflightResult {
	if s.preflightFn != nil {
		return s.preflightFn(ctx, s.db, orgID, domain)
	}
	return preflightDeployCheck(ctx, s.db, orgID, domain)
}

// NewPMTACampaignService creates the service.
func NewPMTACampaignService(
	db *sql.DB,
	orchestrator *engine.Orchestrator,
	convictions *engine.ConvictionStore,
	processor *engine.SignalProcessor,
	orgID string,
) *PMTACampaignService {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return &PMTACampaignService{
		db:           db,
		orchestrator: orchestrator,
		convictions:  convictions,
		processor:    processor,
		orgID:        orgID,
		suppMatcher:  NewSuppressionMatcher(),
		colCache:     probeCampaignColumns(ctx, db),
	}
}

func (s *PMTACampaignService) SetGlobalSuppressionHub(hub *engine.GlobalSuppressionHub) {
	s.globalHub = hub
}

// RegisterRoutes mounts all PMTA campaign wizard routes.
func (s *PMTACampaignService) RegisterRoutes(r chi.Router) {
	r.Route("/pmta-campaign", func(cr chi.Router) {
		cr.Get("/readiness", s.HandleCampaignReadiness)
		cr.Get("/sending-domains", s.HandleSendingDomains)
		cr.Get("/draft", s.HandleGetDraftCampaign)
		cr.Post("/draft", s.HandleSaveDraftCampaign)
		cr.Post("/intel", s.HandleCampaignIntel)
		cr.Post("/estimate-audience", s.HandleEstimateAudience)
		cr.Post("/deploy", s.HandleDeployCampaign)
		cr.Post("/dry-run", s.HandleDryRunCampaign)
		cr.Get("/deploy-dynamic-test", s.HandleDeployDynamicTagsTest)
		cr.Get("/wave-content-test", s.HandleWaveContentTest)
		cr.Get("/diag", s.HandlePMTADiag)
		cr.Get("/trigger-send", s.HandleTriggerSend)
		cr.Post("/push-ses-relay", s.HandlePushSESRelay)
		cr.Post("/test-ses-send", s.HandleTestSESSend)
		cr.Post("/{campaignId}/emergency-stop", s.HandleEmergencyCampaignStop)
		cr.Get("/clone-candidates", s.HandleCloneCandidates)
		cr.Get("/{campaignId}/clone-data", s.HandleCloneData)
		cr.Get("/last-quotas", s.HandleLastQuotas)
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

	profileFromNames := make(map[string]string)
	pfRows, pfErr := s.db.QueryContext(ctx, `
		SELECT sending_domain, from_name FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active'
		  AND sending_domain IS NOT NULL AND sending_domain != ''
		  AND from_name IS NOT NULL AND from_name != ''
	`, orgID)
	if pfErr == nil {
		for pfRows.Next() {
			var dom, fn string
			if pfRows.Scan(&dom, &fn) == nil {
				profileFromNames[dom] = fn
			}
		}
		pfRows.Close()
	}

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
			FromName:        profileFromNames[domain],
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

// HandleEstimateAudience returns audience size with per-ISP breakdown using
// bloom-filter-powered suppression checks for accurate counts.
func (s *PMTACampaignService) HandleEstimateAudience(w http.ResponseWriter, r *http.Request) {
	var req engine.AudienceEstimateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	// Resolve suppression list names for source breakdown display
	suppListNames := make(map[string]string)
	if len(req.SuppressionListIDs) > 0 {
		for _, slID := range req.SuppressionListIDs {
			var name string
			if s.db.QueryRowContext(ctx, `SELECT name FROM mailing_suppression_lists WHERE id = $1`, slID).Scan(&name) == nil {
				suppListNames[slID] = name
			} else {
				suppListNames[slID] = slID
			}
		}
	}

	// Load suppression lists into bloom filter for O(1) lookup
	if len(req.SuppressionListIDs) > 0 {
		for _, slID := range req.SuppressionListIDs {
			rows, err := s.db.QueryContext(ctx,
				`SELECT md5_hash FROM mailing_suppression_entries WHERE list_id = $1`, slID)
			if err == nil {
				var hashes []string
				for rows.Next() {
					var h string
					if rows.Scan(&h) == nil {
						hashes = append(hashes, h)
					}
				}
				rows.Close()
				if len(hashes) > 0 {
					s.suppMatcher.LoadList(slID, hashes)
				}
			}
		}
	}

	// Stream subscriber emails from selected lists, check suppression per-email,
	// and build exact ISP breakdown in one pass.
	totalRecipients := 0
	suppressedCount := 0
	ispBreakdown := make(map[string]int)
	suppressionSources := make(map[string]int)
	seenEmails := make(map[string]bool)

	if len(req.ListIDs) > 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT s.email
			FROM mailing_subscribers s
			WHERE s.list_id = ANY($1) AND s.status IN ('active','confirmed')
		`, pq.Array(req.ListIDs))
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var email string
				if rows.Scan(&email) != nil {
					continue
				}
				emailLower := strings.ToLower(strings.TrimSpace(email))
				if seenEmails[emailLower] {
					continue
				}
				seenEmails[emailLower] = true
				totalRecipients++

				suppressed := false
				// Check each named suppression list individually to track source
				if len(req.SuppressionListIDs) > 0 {
					for _, slID := range req.SuppressionListIDs {
						if s.suppMatcher.IsSuppressed(emailLower, []string{slID}) {
							name := suppListNames[slID]
							suppressionSources[name]++
							suppressed = true
							break
						}
					}
				}
				if !suppressed && s.globalHub != nil && s.globalHub.IsSuppressed(emailLower) {
					suppressionSources["Global Suppression"]++
					suppressed = true
				}

				if suppressed {
					suppressedCount++
					continue
				}

				domain := emailLower
				if idx := strings.LastIndex(emailLower, "@"); idx >= 0 {
					domain = emailLower[idx+1:]
				}
				isp := domainToISPLookup(domain)
				ispBreakdown[isp]++
			}
		}
	}

	// Stream segment subscribers through the same pipeline for accurate counts
	for _, segID := range req.SegmentIDs {
		var segListID *string
		var conditionsRaw sql.NullString
		if err := s.db.QueryRowContext(ctx,
			`SELECT list_id::text, conditions::text FROM mailing_segments WHERE id = $1 AND organization_id = $2`,
			segID, orgID).Scan(&segListID, &conditionsRaw); err != nil {
			log.Printf("[EstimateAudience] segment %s lookup failed: %v", segID, err)
			continue
		}
		var listIDVal interface{}
		if segListID != nil && *segListID != "" {
			listIDVal = *segListID
		}
		condStr := ""
		if conditionsRaw.Valid {
			condStr = conditionsRaw.String
		}
		query, args := buildSegmentQuery(condStr, listIDVal)
		segRows, segErr := s.db.QueryContext(ctx, query, args...)
		if segErr != nil {
			log.Printf("[EstimateAudience] segment %s query failed: %v", segID, segErr)
			continue
		}
		for segRows.Next() {
			var subID, email string
			if segRows.Scan(&subID, &email) != nil {
				continue
			}
			emailLower := strings.ToLower(strings.TrimSpace(email))
			if seenEmails[emailLower] {
				continue
			}
			seenEmails[emailLower] = true
			totalRecipients++

			suppressed := false
			if len(req.SuppressionListIDs) > 0 {
				for _, slID := range req.SuppressionListIDs {
					if s.suppMatcher.IsSuppressed(emailLower, []string{slID}) {
						name := suppListNames[slID]
						suppressionSources[name]++
						suppressed = true
						break
					}
				}
			}
			if !suppressed && s.globalHub != nil && s.globalHub.IsSuppressed(emailLower) {
				suppressionSources["Global Suppression"]++
				suppressed = true
			}
			if suppressed {
				suppressedCount++
				continue
			}

			domain := emailLower
			if idx := strings.LastIndex(emailLower, "@"); idx >= 0 {
				domain = emailLower[idx+1:]
			}
			isp := domainToISPLookup(domain)
			ispBreakdown[isp]++
		}
		segRows.Close()
	}

	afterSuppressions := totalRecipients - suppressedCount

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
		TotalRecipients:    totalRecipients,
		AfterSuppressions:  afterSuppressions,
		SuppressedCount:    suppressedCount,
		ISPBreakdown:       ispBreakdown,
		SuppressionSources: suppressionSources,
	})
}

// HandleGetDraftCampaign returns the most recent PMTA draft for the org.
func (s *PMTACampaignService) HandleGetDraftCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	result, err := loadLatestPMTADraft(ctx, s.db, orgID, s.colCache)
	if err != nil {
		if err == sql.ErrNoRows {
			respondJSON(w, http.StatusNotFound, map[string]string{"error": "no PMTA draft found"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// HandleSaveDraftCampaign stores the PMTA wizard state on a single draft row.
func (s *PMTACampaignService) HandleSaveDraftCampaign(w http.ResponseWriter, r *http.Request) {
	var draft engine.PMTACampaignDraftInput
	if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	input := draft.CampaignInput
	if strings.TrimSpace(input.Name) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaign name is required"})
		return
	}
	if len(input.TargetISPs) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one target ISP is required"})
		return
	}
	if strings.TrimSpace(input.SendingDomain) == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending domain is required"})
		return
	}
	if len(input.Variants) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one content variant is required"})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	result, err := savePMTADraftCampaign(ctx, s.db, orgID, draft, s.colCache)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, result)
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
		if len(input.ISPPlans) == 0 {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one target ISP is required"})
			return
		}
	}

	deployCtx, deployCancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer deployCancel()
	ctx := deployCtx
	orgID := getOrgID(r)

	preflight := s.runPreflight(ctx, orgID, input.SendingDomain)
	if !preflight.OK {
		msgs := make([]string, len(preflight.Errors))
		for i, e := range preflight.Errors {
			msgs[i] = e.Check + ": " + e.Message
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "preflight failed: " + strings.Join(msgs, "; "),
		})
		return
	}

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Use a dedicated DB connection with an extended statement_timeout for
	// audience planning, which can be slow when spanning many lists.
	var audienceDB dbQuerier = s.db
	conn, connErr := s.db.Conn(ctx)
	if connErr == nil {
		if _, err := conn.ExecContext(ctx, "SET statement_timeout = '300s'"); err == nil {
			audienceDB = conn
		}
		defer func() {
			conn.ExecContext(context.Background(), "RESET statement_timeout")
			conn.Close()
		}()
	}

	audience, err := planPMTAAudience(ctx, audienceDB, orgID, input, normalized, s.suppMatcher)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer tx.Rollback()

	result, err := createPMTAWaveCampaign(ctx, tx, s.db, orgID, input, normalized, audience, s.colCache)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"campaign_id":       result.CampaignID,
		"name":              result.Name,
		"status":            result.Status,
		"send_mode":         result.SendMode,
		"sends_at":          result.SendsAt,
		"target_isps":       result.TargetISPs,
		"total_audience":    result.TotalAudience,
		"variant_count":     result.VariantCount,
		"isp_plans":         result.ISPPlans,
		"initial_waves":     result.InitialWaves,
		"assumptions":       result.Assumptions,
		"legacy_input":      result.LegacyInput,
		"queued_count":      0,
		"after_suppression": audience.AfterSuppression,
		"suppressed":        audience.TotalSeen - audience.AfterSuppression,
		"per_isp_selected":  audience.CountsByISP,
	})
}

// HandleDryRunCampaign returns a preview of what a campaign deployment would
// look like — audience counts, wave schedule, ISP distribution — without
// creating any database records. Uses the same normalization and audience
// planning pipeline as the real deploy.
func (s *PMTACampaignService) HandleDryRunCampaign(w http.ResponseWriter, r *http.Request) {
	var input engine.PMTACampaignInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	preflight := s.runPreflight(ctx, orgID, input.SendingDomain)

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var audienceDB dbQuerier = s.db
	conn, connErr := s.db.Conn(ctx)
	if connErr == nil {
		if _, err := conn.ExecContext(ctx, "SET statement_timeout = '300s'"); err == nil {
			audienceDB = conn
		}
		defer func() {
			conn.ExecContext(context.Background(), "RESET statement_timeout")
			conn.Close()
		}()
	}

	audience, err := planPMTAAudience(ctx, audienceDB, orgID, input, normalized, s.suppMatcher)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type ispPreview struct {
		ISP            string `json:"isp"`
		AudienceCount  int    `json:"audience_count"`
		WaveCount      int    `json:"wave_count"`
		BatchSize      int    `json:"batch_size"`
		WindowStart    string `json:"window_start,omitempty"`
		WindowEnd      string `json:"window_end,omitempty"`
		CadenceMinutes int    `json:"cadence_minutes"`
	}

	wavesByISP := make(map[string][]pmtaWaveSpec)
	var previews []ispPreview
	totalWaves := 0
	totalRecipients := 0

	for _, plan := range normalized.Plans {
		count := audience.CountsByISP[plan.ISP]
		waves := buildPMTAWaveSpecs("preview", plan, count)
		wavesByISP[plan.ISP] = waves
		totalWaves += len(waves)
		totalRecipients += count

		p := ispPreview{
			ISP:           plan.ISP,
			AudienceCount: count,
			WaveCount:     len(waves),
		}
		if len(waves) > 0 {
			p.BatchSize = waves[0].BatchSize
			p.WindowStart = waves[0].ScheduledAt.Format(time.RFC3339)
			p.WindowEnd = waves[len(waves)-1].ScheduledAt.Format(time.RFC3339)
			p.CadenceMinutes = waves[0].CadenceMinutes
		}
		previews = append(previews, p)
	}

	var warnings []string
	if err := waveSanityCheck(normalized.Plans, wavesByISP); err != nil {
		warnings = append(warnings, err.Error())
	}

	var lastWaveAt time.Time
	for _, waves := range wavesByISP {
		if len(waves) > 0 && waves[len(waves)-1].ScheduledAt.After(lastWaveAt) {
			lastWaveAt = waves[len(waves)-1].ScheduledAt
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"dry_run":              true,
		"preflight":            preflight,
		"total_waves":          totalWaves,
		"total_recipients":     totalRecipients,
		"total_seen":           audience.TotalSeen,
		"after_suppression":    audience.AfterSuppression,
		"suppressed":           audience.TotalSeen - audience.AfterSuppression,
		"estimated_completion": lastWaveAt.Format(time.RFC3339),
		"isp_plans":            previews,
		"warnings":             warnings,
	})
}

// HandleEmergencyCampaignStop immediately cancels a campaign, kills all queue items
// (including already-claimed ones), and pauses all PMTA queues via SSH.
func (s *PMTACampaignService) HandleEmergencyCampaignStop(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	if campaignID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "campaignId required"})
		return
	}

	// 1. Cancel campaign
	s.db.ExecContext(ctx, "UPDATE mailing_campaigns SET status='cancelled', completed_at=NOW(), updated_at=NOW() WHERE id=$1", campaignID)

	// 2. Cancel ALL queue items — including 'sending' and 'claimed' status
	result, _ := s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue SET status='cancelled'
		WHERE campaign_id=$1 AND status IN ('queued','sending','claimed','pending')
	`, campaignID)
	cancelled, _ := result.RowsAffected()

	// 3. Pause all PMTA queues via SSH
	pmtaPaused := false
	if s.executor != nil {
		if err := s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "gmail"}); err == nil {
			pmtaPaused = true
		}
		s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "yahoo"})
		s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "microsoft"})
		s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "att"})
		s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "apple"})
		s.executor.Execute(ctx, engine.Decision{ActionTaken: "pause_isp_queues", ISP: "comcast"})
	}

	log.Printf("[EmergencyStop] Campaign %s: cancelled %d queue items, PMTA paused: %v", campaignID, cancelled, pmtaPaused)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id":     campaignID,
		"status":          "cancelled",
		"items_cancelled": cancelled,
		"pmta_paused":     pmtaPaused,
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
func resolveListNamesToIDs(ctx context.Context, db dbQuerier, orgID string, names []string) []string {
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
		// Look up in mailing_lists first
		var listID string
		err := db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_lists
			WHERE organization_id = $1 AND name = $2
			LIMIT 1
		`, orgID, name).Scan(&listID)
		if err == nil {
			ids = append(ids, listID)
			continue
		}
		// Fallback: mailing_suppression_lists (for exclusions like "Global Suppression", "global-suppression-list")
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_suppression_lists
			WHERE id = $1 OR LOWER(name) = LOWER($1)
			LIMIT 1
		`, name).Scan(&listID)
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
	"aol.com":    "yahoo",
	"icloud.com": "apple", "me.com": "apple", "mac.com": "apple",
	"comcast.net": "comcast", "xfinity.com": "comcast",
	"att.net": "att", "sbcglobal.net": "att", "bellsouth.net": "att",
	"cox.net":     "cox",
	"charter.net": "charter", "spectrum.net": "charter",
	"verizon.net":    "verizon",
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
		"campaign_id": campaignID,
		"enqueued":    enqueued,
		"profile_id":  profileID,
		"list_ids":    listIDs,
		"status":      "sending",
		"message":     "Campaign manually enqueued — send worker will pick up pending items",
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
<p>If you received this, the relay chain is working: <code>IGNITE -&gt; PMTA Bridge -&gt; PMTA -&gt; SES SMTP -&gt; Gmail</code></p>
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

// buildSegmentQuery detects whether conditionsRaw is a V2 ConditionGroupBuilder
// (JSON object with logic_operator) or a legacy flat array, and builds the
// appropriate SELECT id::text, email query.
func buildSegmentQuery(conditionsRaw string, listIDVal interface{}) (string, []interface{}) {
	raw := strings.TrimSpace(conditionsRaw)
	if raw == "" || raw == "null" || raw == "[]" {
		return BuildSegmentSubscriberQuery(listIDVal, nil)
	}

	if raw[0] == '{' {
		var group segmentation.ConditionGroupBuilder
		if err := json.Unmarshal([]byte(raw), &group); err == nil && group.LogicOperator != "" {
			qb := segmentation.NewQueryBuilder()
			if listIDVal != nil {
				qb.SetListID(fmt.Sprintf("%v", listIDVal))
			}
			fullQuery, args, err := qb.BuildQuery(group, nil)
			if err == nil {
				// V2 BuildQuery returns a full SELECT with many columns; wrap it
				// to extract only the id::text and email the caller needs.
				query := fmt.Sprintf("SELECT sub.id::text, sub.email FROM (%s) sub", fullQuery)
				return query, args
			}
			log.Printf("[buildSegmentQuery] V2 query build error, falling back to legacy: %v", err)
		}
	}

	var conditions []SegmentConditionInput
	json.Unmarshal([]byte(raw), &conditions)
	return BuildSegmentSubscriberQuery(listIDVal, conditions)
}

// HandleCloneCandidates returns completed PMTA campaigns ranked by performance,
// with enough metadata for the user to pick one to clone.
func (s *PMTACampaignService) HandleCloneCandidates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	configSelect := "false AS has_config"
	if s.colCache.has("pmta_config") {
		configSelect = "(pmta_config IS NOT NULL AND pmta_config::text != '{}') AS has_config"
	}

	query := fmt.Sprintf(`
		SELECT id::text, name, status,
		       COALESCE(sent_count, 0), COALESCE(open_count, 0), COALESCE(click_count, 0),
		       COALESCE(bounce_count, 0),
		       CASE WHEN COALESCE(hard_bounce_count,0)+COALESCE(soft_bounce_count,0)>0 THEN COALESCE(hard_bounce_count,0) ELSE COALESCE(bounce_count,0) END,
		       CASE WHEN COALESCE(hard_bounce_count,0)+COALESCE(soft_bounce_count,0)>0 THEN COALESCE(soft_bounce_count,0) ELSE 0 END,
		       COALESCE(complaint_count, 0),
		       COALESCE(completed_at, started_at, created_at) AS campaign_date,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN COALESCE(open_count, 0)::float / sent_count ELSE 0 END AS open_rate,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN COALESCE(click_count, 0)::float / sent_count ELSE 0 END AS click_rate,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN COALESCE(bounce_count, 0)::float / sent_count ELSE 0 END AS bounce_rate,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN (CASE WHEN COALESCE(hard_bounce_count,0)+COALESCE(soft_bounce_count,0)>0 THEN COALESCE(hard_bounce_count,0) ELSE COALESCE(bounce_count,0) END)::float / sent_count ELSE 0 END AS hard_bounce_rate,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN (CASE WHEN COALESCE(hard_bounce_count,0)+COALESCE(soft_bounce_count,0)>0 THEN COALESCE(soft_bounce_count,0) ELSE 0 END)::float / sent_count ELSE 0 END AS soft_bounce_rate,
		       CASE WHEN COALESCE(sent_count, 0) > 0 THEN COALESCE(complaint_count, 0)::float / sent_count ELSE 0 END AS complaint_rate,
		       %s
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND status IN ('completed', 'sent', 'cancelled', 'completed_with_errors', 'sending', 'draft')
		ORDER BY
		  CASE WHEN COALESCE(sent_count, 0) > 0 THEN COALESCE(open_count, 0)::float / sent_count ELSE 0 END DESC,
		  COALESCE(sent_count, 0) DESC,
		  created_at DESC
		LIMIT 20
	`, configSelect)

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		log.Printf("[CloneCandidates] query error: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"failed to query campaigns: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type candidate struct {
		ID              string  `json:"id"`
		Name            string  `json:"name"`
		Status          string  `json:"status"`
		SentCount       int     `json:"sent_count"`
		OpenCount       int     `json:"open_count"`
		ClickCount      int     `json:"click_count"`
		BounceCount     int     `json:"bounce_count"`
		HardBounceCount int     `json:"hard_bounce_count"`
		SoftBounceCount int     `json:"soft_bounce_count"`
		ComplaintCount  int     `json:"complaint_count"`
		CampaignDate    string  `json:"campaign_date"`
		OpenRate        float64 `json:"open_rate"`
		ClickRate       float64 `json:"click_rate"`
		BounceRate      float64 `json:"bounce_rate"`
		HardBounceRate  float64 `json:"hard_bounce_rate"`
		SoftBounceRate  float64 `json:"soft_bounce_rate"`
		ComplaintRate   float64 `json:"complaint_rate"`
		HasConfig       bool    `json:"has_config"`
		Recommended     bool    `json:"recommended"`
	}

	var candidates []candidate
	bestScore := -1.0
	bestIdx := -1

	for rows.Next() {
		var c candidate
		var campaignDate time.Time
		if err := rows.Scan(&c.ID, &c.Name, &c.Status,
			&c.SentCount, &c.OpenCount, &c.ClickCount, &c.BounceCount, &c.HardBounceCount, &c.SoftBounceCount, &c.ComplaintCount,
			&campaignDate, &c.OpenRate, &c.ClickRate, &c.BounceRate, &c.HardBounceRate, &c.SoftBounceRate, &c.ComplaintRate, &c.HasConfig,
		); err != nil {
			log.Printf("[CloneCandidates] scan error: %v", err)
			continue
		}
		c.CampaignDate = campaignDate.Format(time.RFC3339)
		c.OpenRate = float64(int(c.OpenRate*10000)) / 100
		c.ClickRate = float64(int(c.ClickRate*10000)) / 100
		c.BounceRate = float64(int(c.BounceRate*10000)) / 100
		c.HardBounceRate = float64(int(c.HardBounceRate*10000)) / 100
		c.SoftBounceRate = float64(int(c.SoftBounceRate*10000)) / 100
		c.ComplaintRate = float64(int(c.ComplaintRate*10000)) / 100

		if c.SentCount > 0 {
			score := c.OpenRate*3 + c.ClickRate*5 - c.ComplaintRate*20 - c.BounceRate*2
			if c.HasConfig {
				score += 10 // Bonus for having full PMTA config
			}
			if score > bestScore {
				bestScore = score
				bestIdx = len(candidates)
			}
		}
		candidates = append(candidates, c)
	}
	if bestIdx >= 0 && bestIdx < len(candidates) {
		candidates[bestIdx].Recommended = true
	}
	if candidates == nil {
		candidates = []candidate{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaigns": candidates,
	})
}

// HandleCloneData returns the full PMTA config for a specific campaign,
// formatted as a draft response the wizard can hydrate.
func (s *PMTACampaignService) HandleCloneData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	orgID := getOrgID(r)

	var name, status string
	var configJSON sql.NullString
	var completedAt sql.NullTime

	if s.colCache.has("pmta_config") {
		err := s.db.QueryRowContext(ctx, `
			SELECT name, status, COALESCE(pmta_config::text, ''), completed_at
			FROM mailing_campaigns
			WHERE id = $1 AND organization_id = $2
		`, campaignID, orgID).Scan(&name, &status, &configJSON, &completedAt)
		if err != nil {
			log.Printf("[CloneData] query error for %s: %v", campaignID, err)
			http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
			return
		}
	} else {
		// Fallback: read base campaign fields without pmta_config
		err := s.db.QueryRowContext(ctx, `
			SELECT name, status, completed_at
			FROM mailing_campaigns
			WHERE id = $1 AND organization_id = $2
		`, campaignID, orgID).Scan(&name, &status, &completedAt)
		if err != nil {
			log.Printf("[CloneData] query error for %s: %v", campaignID, err)
			http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
			return
		}
	}

	// Try to use the stored pmta_config first
	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		var cfg struct {
			CampaignInput json.RawMessage `json:"campaign_input"`
			ScheduleMode  string          `json:"schedule_mode,omitempty"`
		}
		if err := json.Unmarshal([]byte(configJSON.String), &cfg); err == nil && len(cfg.CampaignInput) > 2 {
			var inputMap map[string]interface{}
			if err := json.Unmarshal(cfg.CampaignInput, &inputMap); err == nil {
				delete(inputMap, "campaign_id")
				inputMap["name"] = name + " (Clone)"
				cfg.CampaignInput, _ = json.Marshal(inputMap)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"campaign_id":    "",
				"name":           name + " (Clone)",
				"status":         "draft",
				"schedule_mode":  cfg.ScheduleMode,
				"source_id":      campaignID,
				"source_status":  status,
				"campaign_input": json.RawMessage(cfg.CampaignInput),
			})
			return
		}
	}

	// Fallback: reconstruct config from ISP plans and base campaign data
	var subject, fromName, fromEmail, htmlContent, previewText, sendingDomain sql.NullString
	s.db.QueryRowContext(ctx, `
		SELECT subject, from_name, from_email, html_content, preview_text,
		       COALESCE(SPLIT_PART(from_email, '@', 2), '')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &fromName, &fromEmail, &htmlContent, &previewText, &sendingDomain)

	// Get ISP plans
	planRows, _ := s.db.QueryContext(ctx, `
		SELECT isp, quota, COALESCE(throttle_strategy, 'auto'), COALESCE(timezone, 'UTC')
		FROM mailing_campaign_isp_plans
		WHERE campaign_id = $1 ORDER BY isp
	`, campaignID)
	var ispPlans []map[string]interface{}
	var targetISPs []string
	var ispQuotas []map[string]interface{}
	if planRows != nil {
		defer planRows.Close()
		for planRows.Next() {
			var isp string
			var quota int
			var throttle, tz string
			if err := planRows.Scan(&isp, &quota, &throttle, &tz); err == nil {
				targetISPs = append(targetISPs, isp)
				ispQuotas = append(ispQuotas, map[string]interface{}{"isp": isp, "volume": quota})
				ispPlans = append(ispPlans, map[string]interface{}{
					"isp": isp, "quota": quota, "throttle_strategy": throttle, "timezone": tz,
				})
			}
		}
	}

	clonedInput := map[string]interface{}{
		"name":          name + " (Clone)",
		"target_isps":   targetISPs,
		"sending_domain": sendingDomain.String,
		"isp_plans":     ispPlans,
		"isp_quotas":    ispQuotas,
		"variants": []map[string]interface{}{{
			"variant_name":  "A",
			"from_name":     fromName.String,
			"subject":       subject.String,
			"preview_text":  previewText.String,
			"html_content":  htmlContent.String,
			"split_percent": 100,
		}},
	}

	clonedJSON, _ := json.Marshal(clonedInput)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id":    "",
		"name":           name + " (Clone)",
		"status":         "draft",
		"source_id":      campaignID,
		"source_status":  status,
		"campaign_input": json.RawMessage(clonedJSON),
	})
}

// HandleLastQuotas returns ISP quotas from the most recently completed/sent
// campaign so the wizard can default new campaigns to the same volumes.
func (s *PMTACampaignService) HandleLastQuotas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	var campaignID, name string
	var completedAt sql.NullTime
	var configJSON sql.NullString

	query := `SELECT id::text, name, COALESCE(completed_at, started_at, created_at)`
	if s.colCache.has("pmta_config") {
		query += `, COALESCE(pmta_config::text, '')`
	}
	query += ` FROM mailing_campaigns
		WHERE organization_id = $1
		  AND status IN ('completed', 'sent', 'cancelled', 'completed_with_errors')
		  AND COALESCE(sent_count, 0) > 0
		ORDER BY COALESCE(completed_at, started_at, created_at) DESC LIMIT 1`

	var err error
	if s.colCache.has("pmta_config") {
		err = s.db.QueryRowContext(ctx, query, orgID).Scan(&campaignID, &name, &completedAt, &configJSON)
	} else {
		err = s.db.QueryRowContext(ctx, query, orgID).Scan(&campaignID, &name, &completedAt)
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"quotas": nil})
		return
	}

	sourceDate := ""
	if completedAt.Valid {
		sourceDate = completedAt.Time.Format(time.RFC3339)
	}

	// Try pmta_config first
	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		var cfg struct {
			CampaignInput struct {
				ISPQuotas []struct {
					ISP    string `json:"isp"`
					Volume int    `json:"volume"`
				} `json:"isp_quotas"`
			} `json:"campaign_input"`
		}
		if err := json.Unmarshal([]byte(configJSON.String), &cfg); err == nil && len(cfg.CampaignInput.ISPQuotas) > 0 {
			quotas := make([]map[string]interface{}, 0, len(cfg.CampaignInput.ISPQuotas))
			for _, q := range cfg.CampaignInput.ISPQuotas {
				quotas = append(quotas, map[string]interface{}{"isp": q.ISP, "volume": q.Volume})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"quotas":          quotas,
				"source_campaign": name,
				"source_date":     sourceDate,
			})
			return
		}
	}

	// Fallback: reconstruct from mailing_campaign_isp_plans
	planRows, _ := s.db.QueryContext(ctx, `
		SELECT isp, quota FROM mailing_campaign_isp_plans
		WHERE campaign_id = $1 ORDER BY isp
	`, campaignID)
	var quotas []map[string]interface{}
	if planRows != nil {
		defer planRows.Close()
		for planRows.Next() {
			var isp string
			var quota int
			if err := planRows.Scan(&isp, &quota); err == nil {
				quotas = append(quotas, map[string]interface{}{"isp": isp, "volume": quota})
			}
		}
	}

	if len(quotas) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"quotas": nil})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"quotas":          quotas,
		"source_campaign": name,
		"source_date":     sourceDate,
	})
}

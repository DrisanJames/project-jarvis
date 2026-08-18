package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/ignite/sparkpost-monitor/internal/eventbus/sendqueue"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
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
	offerSuppMgr *OfferSuppressionManager

	// preflightFn overrides preflightDeployCheck for testing (DNS lookups
	// cannot be mocked via sqlmock). Nil means use the real implementation.
	// The overrideProfileID parameter is "" when the deploy uses the legacy
	// by-domain auto-lookup, or a UUID when the payload pins a profile.
	preflightFn func(ctx context.Context, db *sql.DB, orgID, domain, overrideProfileID string) preflightResult

	// skipBackgroundDeploy skips the async goroutine in HandleDeployCampaign.
	// Used in tests so sqlmock connections aren't accessed after test cleanup.
	skipBackgroundDeploy bool

	// gateEvalFn overrides evaluateSendDayGates for testing (the real
	// evaluation reads live gate sources via SQL). Nil means use the real
	// implementation. Same pattern as preflightFn.
	gateEvalFn func(ctx context.Context, in sendDayGateEvalInput) sendDayGateReport
}

func (s *PMTACampaignService) SetExecutor(e *engine.Executor) {
	s.executor = e
}

func (s *PMTACampaignService) runPreflight(ctx context.Context, orgID, domain, overrideProfileID string) preflightResult {
	if s.preflightFn != nil {
		return s.preflightFn(ctx, s.db, orgID, domain, overrideProfileID)
	}
	return preflightDeployCheck(ctx, s.db, orgID, domain, overrideProfileID)
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

func (s *PMTACampaignService) SetOfferSuppressionManager(mgr *OfferSuppressionManager) {
	s.offerSuppMgr = mgr
}

func (s *PMTACampaignService) SetGlobalSuppressionHub(hub *engine.GlobalSuppressionHub) {
	s.globalHub = hub
}

// RegisterRoutes mounts all PMTA campaign wizard routes.
func (s *PMTACampaignService) RegisterRoutes(r chi.Router) {
	r.Route("/pmta-campaign", func(cr chi.Router) {
		cr.Get("/board-export", s.HandleBoardWeekExport)
		cr.Get("/drip-lanes", s.HandleListDripLanes)
		cr.Post("/drip-lanes/update", s.HandleUpdateDripLane)
		// Property Ledger (Vector A plan rev4, Step 19). No per-route admin
		// gate: the /api middleware already enforces session-or-admin-key and
		// stamps the trusted actor identity (Step 13).
		cr.Get("/property-ledger", s.HandleListPropertyLedger)
		cr.Post("/property-ledger/update", s.HandleUpdatePropertyLedger)
		cr.Post("/property-ledger/approve-proposal", s.HandleApproveProposal)
		cr.Post("/property-ledger/approve-proposals", s.HandleApproveProposals)
		cr.Post("/property-ledger/global-hold", s.HandleGlobalHold)
		cr.Get("/property-ledger/reconciliation", s.HandleListReconciliation)
		cr.Get("/property-ledger/coverage", s.HandleListCoverage)
		// Lane content panel (operator scope addition 2026-08-17): what a
		// lane sends (offer / creative / subject / preheader / from-name) +
		// in-screen edits, audit-logged with the stamped actor.
		cr.Get("/property-ledger/lane-content", s.HandleLaneContent)
		cr.Get("/property-ledger/lane-content/creative", s.HandleLaneCreativePreview)
		cr.Post("/property-ledger/lane-content/subject", s.HandleLaneSubjectEdit)
		cr.Post("/property-ledger/lane-content/touch-copy", s.HandleLaneTouchCopyEdit)
		cr.Post("/property-ledger/lane-content/offer-swap", s.HandleLaneOfferSwap)
		// Supply strip (Pipeline Cockpit P1, read-only): live pcq tranche
		// anatomy per feed — total / cleaning / ready-by-ISP / held.
		cr.Get("/property-ledger/supply", s.HandleLaneSupply)
		cr.Get("/readiness", s.HandleCampaignReadiness)
		cr.Get("/sending-domains", s.HandleSendingDomains)
		cr.Get("/draft", s.HandleGetDraftCampaign)
		cr.Post("/draft", s.HandleSaveDraftCampaign)
		cr.Post("/stage", s.HandleStageCampaign)
		cr.Post("/intel", s.HandleCampaignIntel)
		cr.Post("/estimate-audience", s.HandleEstimateAudience)
		cr.Post("/deploy", s.HandleDeployCampaign)
		cr.Post("/dry-run", s.HandleDryRunCampaign)
		// Promote post-conditions gate (Coalition WS2, REQ-C18). Read-only —
		// the stage/promote CLI polls it after deploy; see
		// tasks/eng-team/coalition/SCHEMA-CONTRACTS.md §4.
		cr.Post("/verify", s.HandleVerifyCampaigns)
		cr.Get("/deploy-dynamic-test", s.HandleDeployDynamicTagsTest)
		cr.Get("/wave-content-test", s.HandleWaveContentTest)
		cr.Get("/wave-content-cache", s.HandleWaveContentCache)
		cr.Post("/refresh-wave-cache", s.HandleRefreshWaveCache)
		cr.Post("/deploy-cached-waves", s.HandleDeployCachedWaves)
		cr.Get("/pipeline-health", s.HandlePipelineHealth)
		cr.Get("/diag", s.HandlePMTADiag)
		cr.Get("/trigger-send", s.HandleTriggerSend)
		cr.Post("/push-ses-relay", s.HandlePushSESRelay)
		cr.Post("/test-ses-send", s.HandleTestSESSend)
		cr.Post("/{campaignId}/emergency-stop", s.HandleEmergencyCampaignStop)
		cr.Get("/clone-candidates", s.HandleCloneCandidates)
		cr.Get("/{campaignId}/clone-data", s.HandleCloneData)
		cr.Get("/{campaignId}/edit-data", s.HandleEditData)
		cr.Get("/{campaignId}/isp-volume", s.HandleCampaignISPVolume)
		cr.Get("/last-quotas", s.HandleLastQuotas)
		cr.Get("/provider-qualification", s.HandleProviderQualification)
		cr.Get("/source-qualification", s.HandleSourceQualification)
		cr.Post("/deliverability-recs", s.HandleDeliverabilityRecommendations)
		cr.Post("/{campaignId}/retry", s.HandleRetryCampaign)
		cr.Get("/pool-isolation-status", s.HandlePoolIsolationStatus)
		cr.Post("/pool-isolation-activate", s.HandlePoolIsolationActivate)
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

	// Build domain → (from_name, ip_pool, pool_prefix) from active PMTA profiles.
	// ORDER BY created_at DESC so the most recent profile wins per domain.
	type profileInfo struct {
		fromName   string
		ipPool     string
		poolPrefix string
	}
	profileMap := make(map[string]profileInfo)
	pfRows, pfErr := s.db.QueryContext(ctx, `
		SELECT sending_domain,
		       COALESCE(from_name, ''),
		       COALESCE(ip_pool, ''),
		       COALESCE(pool_prefix, '')
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active'
		  AND sending_domain IS NOT NULL AND sending_domain != ''
		ORDER BY created_at DESC
	`, orgID)
	if pfErr == nil {
		for pfRows.Next() {
			var dom, fn, pool, pfx string
			if pfRows.Scan(&dom, &fn, &pool, &pfx) == nil {
				if _, exists := profileMap[dom]; !exists {
					profileMap[dom] = profileInfo{fromName: fn, ipPool: pool, poolPrefix: pfx}
				}
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

		prof := profileMap[domain]
		pfx := strings.TrimSpace(prof.poolPrefix)

		if pfx != "" {
			// ISP-dedicated pools: pool_prefix "db" → pools db-gmail-pool, db-yahoo-pool, etc.
			ipRows, qErr := s.db.QueryContext(ctx, `
				SELECT ip.ip_address::text, ip.status
				FROM mailing_ip_addresses ip
				JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
				WHERE pool.name LIKE $1 || '-%'
				  AND ip.status IN ('active', 'warmup')
				  AND pool.status = 'active'
				ORDER BY ip.hostname
			`, pfx)
			if qErr == nil {
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
			poolName = prof.ipPool + " (" + pfx + "-*)"
		} else if strings.TrimSpace(prof.ipPool) != "" {
			// Single named pool (e.g. warmup-pool)
			ipRows, qErr := s.db.QueryContext(ctx, `
				SELECT ip.ip_address::text, ip.status
				FROM mailing_ip_addresses ip
				JOIN mailing_ip_pools pool ON pool.id = ip.pool_id
				WHERE pool.name = $1
				  AND ip.status IN ('active', 'warmup')
				  AND pool.status = 'active'
				ORDER BY ip.hostname
			`, strings.TrimSpace(prof.ipPool))
			if qErr == nil {
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
			poolName = prof.ipPool
		} else {
			poolName = "(none)"
		}

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
			FromName:        prof.fromName,
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

	turbulenceAlerts := queryTurbulenceAlerts(ctx, s.db, s.orgID)

	respondJSON(w, http.StatusOK, engine.CampaignIntelResponse{
		ISPs:             ispIntels,
		OverallStrategy:  overallStrategy,
		TurbulenceAlerts: turbulenceAlerts,
	})
}

func queryTurbulenceAlerts(ctx context.Context, db *sql.DB, orgID string) []engine.TurbulenceAlert {
	rows, err := db.QueryContext(ctx,
		`SELECT isp, agent_type, action_taken,
		        COALESCE(target_value, ''), result, created_at
		 FROM mailing_engine_decisions
		 WHERE organization_id = $1
		   AND action_taken IN ('quarantine_ip', 'emergency_halt', 'pause_warmup', 'disable_source_ip')
		   AND created_at > NOW() - INTERVAL '24 hours'
		 ORDER BY created_at DESC
		 LIMIT 50`, orgID)
	if err != nil {
		log.Printf("[campaign-intel] turbulence query error: %v", err)
		return nil
	}
	defer rows.Close()

	var alerts []engine.TurbulenceAlert
	for rows.Next() {
		var a engine.TurbulenceAlert
		if err := rows.Scan(&a.ISP, &a.AgentType, &a.Action, &a.IP, &a.Reasoning, &a.OccurredAt); err != nil {
			continue
		}
		alerts = append(alerts, a)
	}
	return alerts
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

	// Derive the campaign's brand root once — estimate loops iterate many
	// thousands of subscribers and the brand root is constant for the whole
	// estimate. Empty string falls back to pure-global checks, preserving
	// the legacy behavior for requests that omit sending_domain.
	estimateBrandRoot := brand.Root(req.SendingDomain)

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
			WHERE s.list_id = ANY($1) AND s.status IN ('active','confirmed') AND s.is_bot = false
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
				if !suppressed && s.globalHub != nil && s.globalHub.IsSuppressedForBrand(emailLower, estimateBrandRoot) {
					if estimateBrandRoot != "" {
						suppressionSources["Brand Suppression ("+estimateBrandRoot+")"]++
					} else {
						suppressionSources["Global Suppression"]++
					}
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
			if !suppressed && s.globalHub != nil && s.globalHub.IsSuppressedForBrand(emailLower, estimateBrandRoot) {
				if estimateBrandRoot != "" {
					suppressionSources["Brand Suppression ("+estimateBrandRoot+")"]++
				} else {
					suppressionSources["Global Suppression"]++
				}
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

// HandleStageCampaign stores the PMTA wizard state as a brand-new draft row on
// every call. Unlike HandleSaveDraftCampaign (which keeps a single "latest
// draft" per org and updates it in place), this always INSERTs a distinct
// campaign with a fresh UUID — letting the operator stage a multi-campaign
// send-day board as independent drafts that can later be approved/promoted.
//
// It accepts the same request body as POST /pmta-campaign/draft:
// {"campaign_input": {...}, "schedule_mode": "quick"}.
func (s *PMTACampaignService) HandleStageCampaign(w http.ResponseWriter, r *http.Request) {
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
	// Creative render gate (2026-08-10 raw-source incident). The Draft Board is
	// the operator's approval surface, so a creative that can never render must
	// be rejected HERE — landing it as a draft only defers the failure to the
	// promote, after the operator has already approved it by eye.
	if err := validateVariantTemplates(input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	result, err := stagePMTADraftCampaign(ctx, s.db, orgID, draft, s.colCache)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Attribution stamp at STAGE (audience unification W2) — the exact
	// invocation finalizeDeploy/finalizeAudience use post-commit, so drafts
	// carry offer_id/offer_key from the moment they land on the Draft Board.
	// Idempotency-gated on attribution_source (the deploy-time re-run is a
	// no-op); log-and-continue inside; kill switch DISABLE_ATTRIBUTION_STAMPING=1.
	stampSubject, stampHTML, stampFromName := "", "", ""
	if len(input.Variants) > 0 {
		stampSubject = input.Variants[0].Subject
		stampHTML = input.Variants[0].HTMLContent
		stampFromName = input.Variants[0].FromName
	}
	stampCtx, stampCancel := context.WithTimeout(context.Background(), 15*time.Second)
	stampCampaignAttribution(stampCtx, s.db, orgID, result.CampaignID, input, input.Name, stampSubject, stampHTML, stampFromName)
	stampCancel()

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id":  result.CampaignID,
		"name":         result.Name,
		"status":       result.Status,
		"scheduled_at": derivePMTADraftScheduledAt(result.CampaignInput),
	})
}

// deployGateOverride is the explicit operator escape hatch past red send-day
// gates: {"gate_override": {"reason": "<non-empty why>"}} on the deploy body.
// Every accepted override is audit-logged (log line + attestation-table row).
type deployGateOverride struct {
	Reason string `json:"reason"`
}

// HandleDeployCampaign creates a PMTA-routed campaign and queues it for sending.
//
// Server-side gate enforcement (REQ-007): before ANY campaign row is reserved
// — on both the id-less deploy path and the Draft Board's promote path
// (campaign_id set) — the six send-day gates are evaluated server-side
// (evaluateSendDayGates, send_day_handlers.go). A red or unknown gate blocks
// with 412 + {error, failed_gates, override_hint}; an explicit gate_override
// with a non-empty reason proceeds and is audit-logged. Staging drafts
// (POST /pmta-campaign/stage) is deliberately NOT gated — only promotion to a
// live send is.
func (s *PMTACampaignService) HandleDeployCampaign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		engine.PMTACampaignInput
		GateOverride *deployGateOverride `json:"gate_override,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	input := req.PMTACampaignInput
	orgID := getOrgID(r)

	// Kill switch (wave-1 manager review): restores pre-enforcement deploy
	// behavior without a redeploy — the one-line rollback for a live send-day
	// wedged on gate evaluation itself (same idiom as
	// DISABLE_SEND_OWNERSHIP_RECHECK / DISABLE_FAILED_ROW_RECOVERY).
	gateEnforcementDisabled := os.Getenv("DISABLE_SEND_DAY_GATE_ENFORCEMENT") == "true"

	// The six gates are a SEND-DAY BOARD discipline — PMTA host attestation,
	// wave-janitor cleanliness, volume-vs-yesterday — all of which describe the
	// operator's daily broadcast. Continuous internal automation (the partner
	// drip orchestrator deploys a wave group every few minutes, in-process via
	// WrapPMTACampaignDeploy) is NOT a send-day board: it has no volume ramp to
	// reconcile and no daily attestation ritual. Gating it wedges the drip.
	//
	// Verified in prod 2026-07-13: enforcement shipped, and within 20 minutes
	// 27 drip wave-group deploys failed with `412 send-day gates failed: A`
	// (nobody had attested host health that day) — partner touches simply
	// stopped. Internal callers therefore bypass the gates, audit-logged.
	if internal := strings.TrimSpace(r.Header.Get("X-Internal-Caller")); internal != "" {
		log.Printf("[SendDayGates] bypass: internal caller %q (campaign %q) — gates govern operator boards, not automation", internal, input.Name)
		gateEnforcementDisabled = true
	}

	report := s.evaluateDeployGates(r.Context(), orgID, input)
	if failed := report.failed(); len(failed) > 0 && gateEnforcementDisabled {
		names := make([]string, len(failed))
		for i, v := range failed {
			names[i] = v.Gate
		}
		log.Printf("[SendDayGates] enforcement DISABLED by kill switch — proceeding past red gates [%s] for campaign %q org=%s", strings.Join(names, ","), input.Name, orgID)
	}
	if failed := report.failed(); len(failed) > 0 && !gateEnforcementDisabled {
		names := make([]string, len(failed))
		for i, v := range failed {
			names[i] = v.Gate
		}
		reason := ""
		if req.GateOverride != nil {
			reason = strings.TrimSpace(req.GateOverride.Reason)
		}
		if reason == "" {
			respondJSON(w, http.StatusPreconditionFailed, map[string]interface{}{
				"error":         "send-day gates failed: " + strings.Join(names, ", "),
				"failed_gates":  failed,
				"override_hint": `re-POST the same payload with {"gate_override":{"reason":"<why this deploy must proceed>"}} — every override is audit-logged`,
			})
			return
		}
		s.auditGateOverride(r.Context(), orgID, input.Name, names, reason)
	}

	campaignID, status, alreadyExisted, err := s.deployFromInput(r.Context(), orgID, input)
	if err != nil {
		var inputErr *deployInputError
		if errors.As(err, &inputErr) {
			respondJSON(w, http.StatusBadRequest, map[string]string{"error": inputErr.Error()})
		} else {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}

	if alreadyExisted {
		// Idempotent convergence: a re-POST of a name that already has a
		// live campaign returns that campaign (200, never a 4xx) so
		// resilient re-deploy loops and dedupe scripts converge instead of
		// erroring or double-deploying (2026-07-13 over-deploy incident).
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"campaign_id":     campaignID,
			"name":            input.Name,
			"status":          status,
			"already_existed": true,
			"target_isps":     input.TargetISPs,
			"variant_count":   len(input.Variants),
		})
		return
	}

	// Respond immediately — campaign is accepted.
	// The AudienceFinalizationWorker will pick it up and process it.
	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"campaign_id":     campaignID,
		"name":            input.Name,
		"status":          status,
		"already_existed": false,
		"target_isps":     input.TargetISPs,
		"variant_count":   len(input.Variants),
	})
}

// evaluateDeployGates runs the server-side send-day gate evaluation for one
// deploy payload. gateEvalFn (tests) wins; otherwise the real
// evaluateSendDayGates reads the live gate sources. Gate D reuses runPreflight
// so a pinned sending_profile_id is validated exactly as the deploy itself
// will; Gate F's collapse floor is skipped for audience-bound (all-quotas-
// zero) payloads per the standing uncapped engaged-tier doctrine.
func (s *PMTACampaignService) evaluateDeployGates(ctx context.Context, orgID string, input engine.PMTACampaignInput) sendDayGateReport {
	in := sendDayGateEvalInput{
		OrgID:    orgID,
		Uncapped: deployInputIsUncapped(input),
		Preflight: func(pctx context.Context) preflightResult {
			return s.runPreflight(pctx, orgID, input.SendingDomain, input.SendingProfileID)
		},
	}
	if input.ScheduledAt != nil {
		// Anchor Gate F to the send-day being deployed (evening approvals of
		// tomorrow's board reconcile tomorrow's planned volume).
		in.TargetDay = *input.ScheduledAt
	}
	if s.gateEvalFn != nil {
		return s.gateEvalFn(ctx, in)
	}
	return evaluateSendDayGates(ctx, s.db, in)
}

// deployInputIsUncapped reports whether the payload carries NO finite volume
// cap anywhere (isp_quotas and per-ISP plan quotas all zero/absent). volume=0
// means UNLIMITED / audience-bound — the standing uncapped engaged-tier
// doctrine — so Gate F's planned-vs-yesterday collapse floor does not apply.
// A finite cap anywhere makes the deploy subject to it.
func deployInputIsUncapped(input engine.PMTACampaignInput) bool {
	for _, q := range input.ISPQuotas {
		if q.Volume > 0 {
			return false
		}
	}
	for _, p := range input.ISPPlans {
		if p.Quota > 0 {
			return false
		}
	}
	return true
}

// auditGateOverride records an operator gate override. The log line is the
// primary audit record; a row is also upserted into the existing
// mailing_send_day_gate_attestations table (gate='OVERRIDE', keyed by campaign
// name) best-effort — no new table, and a persist failure never blocks the
// already-authorized deploy.
func (s *PMTACampaignService) auditGateOverride(ctx context.Context, orgID, campaignName string, gates []string, reason string) {
	log.Printf("[GateOverride] org=%s campaign=%q gates=[%s] reason=%q",
		orgID, campaignName, strings.Join(gates, ","), reason)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_send_day_gate_attestations
		    (gate, server_key, state, message, last_checked_at, updated_by, updated_at)
		VALUES ('OVERRIDE', $1, 'override', $2, NOW(), $3, NOW())
		ON CONFLICT (gate, server_key) DO UPDATE
		   SET state = EXCLUDED.state,
		       message = EXCLUDED.message,
		       last_checked_at = EXCLUDED.last_checked_at,
		       updated_by = EXCLUDED.updated_by,
		       updated_at = NOW()
	`, campaignName, fmt.Sprintf("gates=[%s] reason=%s", strings.Join(gates, ","), reason), orgID); err != nil {
		log.Printf("[GateOverride] persist failed for campaign %q (log line above remains the audit record): %v", campaignName, err)
	}
}

// deployInputError marks payload validation / preflight / normalization
// failures that map to HTTP 400 in HandleDeployCampaign. Reservation failures
// remain plain errors (HTTP 500).
type deployInputError struct{ msg string }

func (e *deployInputError) Error() string { return e.msg }

// validateSegmentOwnership verifies that every segment referenced by a deploy
// payload — inclusion_segments, exclusion_segments, and send_priority items of
// type "segment" — belongs to the deploying org (REQ-046). It runs as part of
// the normalize step, before any campaign row is reserved: downstream the
// planner reads mailing_segment_members / mailing_segments by segment_id
// alone (pmta_campaign_planner.go), so without this gate a foreign-org UUID
// silently pulls (or suppresses against) another org's audience. Foreign and
// unknown IDs both fail as "segment not found" (HTTP 400 via
// deployInputError) — existence is not leaked across orgs.
func validateSegmentOwnership(ctx context.Context, db *sql.DB, orgID string, input engine.PMTACampaignInput) error {
	ids := make([]string, 0, len(input.InclusionSegments)+len(input.ExclusionSegments))
	seen := make(map[string]bool)
	collect := func(raw string) error {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			return nil
		}
		if _, err := uuid.Parse(id); err != nil {
			return &deployInputError{"invalid segment id: " + id}
		}
		seen[id] = true
		ids = append(ids, id)
		return nil
	}
	for _, raw := range input.InclusionSegments {
		if err := collect(raw); err != nil {
			return err
		}
	}
	for _, raw := range input.ExclusionSegments {
		if err := collect(raw); err != nil {
			return err
		}
	}
	for _, item := range input.SendPriority {
		if strings.EqualFold(strings.TrimSpace(item.Type), "segment") {
			if err := collect(item.ID); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id::text FROM mailing_segments WHERE id = ANY($1::uuid[]) AND organization_id = $2`,
		pq.Array(ids), orgID)
	if err != nil {
		return fmt.Errorf("segment ownership check: %w", err)
	}
	defer rows.Close()
	owned := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("segment ownership check: %w", err)
		}
		owned[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("segment ownership check: %w", err)
	}

	var missing []string
	for _, id := range ids {
		if !owned[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return &deployInputError{"segment not found: " + strings.Join(missing, ", ")}
	}
	return nil
}

// templateParseGateDisabled is the kill switch for the deploy/stage render
// gate: DISABLE_TEMPLATE_PARSE_GATE=true restores the previous
// accept-anything behavior without a redeploy. Same idiom as
// DISABLE_SEND_DAY_GATE_ENFORCEMENT above.
func templateParseGateDisabled() bool {
	return os.Getenv("DISABLE_TEMPLATE_PARSE_GATE") == "true"
}

// validateVariantTemplates rejects a deploy/stage whose creative cannot be
// parsed as a Liquid template (REQ: 2026-08-10 raw-source incident).
//
// This is the CHEAP, EARLY half of the fix. The send worker now fails closed
// on a render error (quarantineUnrenderable), but discovering a broken
// creative one message at a time — after the audience is finalized, the waves
// are enqueued and 25 workers are chewing through the queue — is strictly
// worse than refusing the campaign at the door. A parse failure is a property
// of the creative, so ONE parse per variant answers it for every recipient.
//
// Parse-only on purpose: mailing.TemplateService.Parse compiles the template
// and discards it (template_engine.go:364). No render context is built, no
// per-recipient work is done, no DB is touched — this is microseconds per
// variant, unlike validateSegmentOwnership which does a round trip.
//
// The incident's creative had an unterminated {% if %} inside an HTML comment;
// Liquid tokenizes tags inside comments, so the whole parse aborted and
// Render handed back the raw source. That template fails here.
func validateVariantTemplates(input engine.PMTACampaignInput) error {
	if templateParseGateDisabled() {
		log.Printf("[TemplateGate] enforcement DISABLED by kill switch — skipping creative parse check for campaign %q", input.Name)
		return nil
	}

	ts := mailing.NewTemplateService()
	for i, v := range input.Variants {
		name := strings.TrimSpace(v.VariantName)
		if name == "" {
			name = fmt.Sprintf("#%d", i+1)
		}
		for _, f := range []struct{ field, content string }{
			{"subject", v.Subject},
			{"preview_text", v.PreviewText},
			{"html_content", v.HTMLContent},
			{"plain_content", v.PlainContent},
		} {
			if strings.TrimSpace(f.content) == "" {
				continue
			}
			if err := ts.Parse(f.content); err != nil {
				return &deployInputError{fmt.Sprintf(
					"variant %s %s is not a valid template and would ship as raw source: %v",
					name, f.field, err)}
			}
		}
	}
	return nil
}

// DeployFromInput runs the synchronous deploy path — validation, preflight,
// normalization, campaign reservation — for an already-decoded payload. It is
// shared by HandleDeployCampaign and in-process callers (Domain Agent plan
// approval). Returns the reserved campaign UUID and its status
// ("finalizing_audience"); the AudienceFinalizationWorker completes the
// deploy asynchronously, exactly as with the HTTP endpoint's 202 semantics.
func (s *PMTACampaignService) DeployFromInput(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, error) {
	campaignID, status, _, err := s.deployFromInput(ctx, orgID, input)
	return campaignID, status, err
}

// deployFromInput is DeployFromInput plus the by-name idempotency signal:
// alreadyExisted=true means an id-less deploy matched a live (non-terminal)
// campaign with the same (org, name) and NO new campaign was reserved — the
// returned id/status are the existing row's (2026-07-13 over-deploy incident).
func (s *PMTACampaignService) deployFromInput(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error) {
	if input.Name == "" {
		return "", "", false, &deployInputError{"campaign name is required"}
	}

	if len(input.Variants) == 0 {
		return "", "", false, &deployInputError{"at least one content variant is required"}
	}
	for i, v := range input.Variants {
		if strings.TrimSpace(v.HTMLContent) == "" {
			return "", "", false, &deployInputError{fmt.Sprintf("variant %s has empty HTML content", input.Variants[i].VariantName)}
		}
	}
	// Creative render gate (2026-08-10 raw-source incident): a template that
	// cannot be parsed must never reach the queue. Runs before preflight and
	// before any campaign row is reserved — no DB, no network, parse only.
	if err := validateVariantTemplates(input); err != nil {
		return "", "", false, err
	}
	if len(input.TargetISPs) == 0 {
		if len(input.ISPPlans) == 0 {
			return "", "", false, &deployInputError{"at least one target ISP is required"}
		}
	}

	// ── Phase 1: synchronous pre-checks (fast, <2s) ─────────────────────

	preflight := s.runPreflight(ctx, orgID, input.SendingDomain, input.SendingProfileID)
	if !preflight.OK {
		msgs := make([]string, len(preflight.Errors))
		for i, e := range preflight.Errors {
			msgs[i] = e.Check + ": " + e.Message
		}
		return "", "", false, &deployInputError{"preflight failed: " + strings.Join(msgs, "; ")}
	}

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		return "", "", false, &deployInputError{err.Error()}
	}

	// Segment-ownership gate (REQ-046): part of normalization — every
	// referenced segment must belong to the deploying org before a campaign
	// row is reserved. Foreign IDs map to HTTP 400 via deployInputError.
	if err := validateSegmentOwnership(ctx, s.db, orgID, input); err != nil {
		return "", "", false, err
	}

	// Reserve campaign row as 'preparing' so the dashboard shows it immediately.
	campaignID, existingStatus, alreadyExisted, err := s.reserveCampaignForDeploy(ctx, orgID, input, normalized)
	if err != nil {
		return "", "", false, err
	}
	if alreadyExisted {
		return campaignID, existingStatus, true, nil
	}

	return campaignID, "finalizing_audience", false, nil
}

// reserveCampaignForDeploy resolves campaign identity and marks it as
// 'finalizing_audience' with the pmta_config blob persisted. Returns the
// campaign UUID, plus (existingStatus, true) when the by-name idempotency
// guard matched a live campaign instead of reserving a new one.
func (s *PMTACampaignService) reserveCampaignForDeploy(ctx context.Context, orgID string, input engine.PMTACampaignInput, normalized pmtaNormalizedCampaign) (string, string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, fmt.Errorf("begin reservation tx: %w", err)
	}
	defer tx.Rollback()

	// By-(org, name) idempotency guard (2026-07-13 over-deploy: resilient
	// re-deploy loops re-POSTed pairs already in flight, multiplying stuck
	// campaigns up to ×12). An id-less deploy whose name already has a live
	// campaign converges on that campaign instead of minting a duplicate.
	// Only terminal states (cancelled/failed/deleted) free a name for
	// re-deploy; a finalizing_audience row with sending_profile_id still
	// NULL is "pending", never "missing" — it matches by name like any
	// other live row (the guard never consults sending_profile_id). The
	// advisory xact lock (released at commit/rollback) serializes two
	// concurrent same-name deploys so both cannot pass the SELECT before
	// either INSERTs; orgID is one of the lock keys, so the same name under
	// a DIFFERENT org neither blocks nor matches. Explicit-id deploys skip
	// the guard: they are deliberate reuse/promotion (Draft Board approve,
	// wizard re-deploy) resolved by resolvePMTACampaignIdentity below.
	if strings.TrimSpace(input.CampaignID) == "" {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
			orgID, input.Name); err != nil {
			return "", "", false, fmt.Errorf("acquire deploy name lock: %w", err)
		}
		var existingID, existingStatus string
		err := tx.QueryRowContext(ctx, `
			SELECT id::text, status
			FROM mailing_campaigns
			WHERE organization_id = $1 AND name = $2
			  AND status NOT IN ('cancelled', 'failed', 'deleted')
			ORDER BY created_at ASC
			LIMIT 1`, orgID, input.Name).Scan(&existingID, &existingStatus)
		if err == nil {
			return existingID, existingStatus, true, nil
		}
		if err != sql.ErrNoRows {
			return "", "", false, fmt.Errorf("deploy name idempotency check: %w", err)
		}
	}

	campaignID, reusingDraft, err := resolvePMTACampaignIdentity(ctx, tx, orgID, input.CampaignID, s.colCache)
	if err != nil {
		return "", "", false, err
	}

	// Resolve content_locked. Explicit payload wins; else inherit from the
	// linked offer's flag (seeded TRUE for strict advertisers). If unresolvable
	// leave as nil so the column's DB default (FALSE) applies.
	resolvedLocked := resolveContentLocked(ctx, tx, input.ContentLocked, input.OfferID)

	// Persist content_locked into the pmta_config blob so it survives the
	// config roundtrip in AudienceFinalizationWorker. We write the resolved
	// value, not input.ContentLocked, so a nil-override with an offer default
	// still flows through to createPMTAWaveCampaign.
	inputForConfig := withCampaignID(input, campaignID.String())
	if resolvedLocked != nil {
		v := *resolvedLocked
		inputForConfig.ContentLocked = &v
	}
	configJSON, _ := json.Marshal(pmtaCampaignConfig{
		CampaignInput: inputForConfig,
	})

	if reusingDraft {
		setClauses := "name = $2, status = 'finalizing_audience', updated_at = NOW()"
		args := []any{campaignID, input.Name}
		nextP := 3
		if s.colCache.has("pmta_config") {
			setClauses += fmt.Sprintf(", pmta_config = $%d", nextP)
			args = append(args, configJSON)
			nextP++
		}
		if s.colCache.has("execution_mode") {
			setClauses += fmt.Sprintf(", execution_mode = $%d", nextP)
			args = append(args, pmtaExecutionModeWave)
			nextP++
		}
		// Honour explicit master-selection override from the payload. Without
		// this, drafts reused from older rows retain their stored flag and
		// segment-driven engager campaigns inadvertently run via the SDS
		// pure-pull path.
		if input.UseMasterSelection != nil && s.colCache.has("use_master_selection") {
			setClauses += fmt.Sprintf(", use_master_selection = $%d", nextP)
			args = append(args, *input.UseMasterSelection)
			nextP++
		}
		if resolvedLocked != nil && s.colCache.has("content_locked") {
			setClauses += fmt.Sprintf(", content_locked = $%d", nextP)
			args = append(args, *resolvedLocked)
			nextP++
		}
		args = append(args, orgID)
		query := fmt.Sprintf(`UPDATE mailing_campaigns SET %s WHERE id = $1 AND organization_id = $%d`, setClauses, nextP)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return "", "", false, fmt.Errorf("reserve draft campaign: %w", err)
		}
	} else {
		colList := []string{"id", "organization_id", "name", "status",
			"scheduled_at", "from_name", "from_email", "subject",
			"preview_text", "html_content", "plain_content",
			"created_at", "updated_at"}
		valList := []string{"$1", "$2", "$3", "'finalizing_audience'",
			"$4", "$5", "$6", "$7",
			"$8", "$9", "$10",
			"NOW()", "NOW()"}
		args := []any{
			campaignID, orgID, input.Name,
			normalized.EarliestStart,
			input.Variants[0].FromName, "", input.Variants[0].Subject,
			input.Variants[0].PreviewText, input.Variants[0].HTMLContent,
			input.Variants[0].PlainContent,
		}
		nextP := 11
		if s.colCache.has("pmta_config") {
			colList = append(colList, "pmta_config")
			valList = append(valList, fmt.Sprintf("$%d", nextP))
			args = append(args, configJSON)
			nextP++
		}
		if s.colCache.has("execution_mode") {
			colList = append(colList, "execution_mode")
			valList = append(valList, fmt.Sprintf("$%d", nextP))
			args = append(args, pmtaExecutionModeWave)
			nextP++
		}
		// Honour explicit master-selection override from the payload. nil
		// leaves the column to its DB-level default (true post-phase21),
		// matching the welcome/acquisition default behavior.
		if input.UseMasterSelection != nil && s.colCache.has("use_master_selection") {
			colList = append(colList, "use_master_selection")
			valList = append(valList, fmt.Sprintf("$%d", nextP))
			args = append(args, *input.UseMasterSelection)
			nextP++
		}
		if resolvedLocked != nil && s.colCache.has("content_locked") {
			colList = append(colList, "content_locked")
			valList = append(valList, fmt.Sprintf("$%d", nextP))
			args = append(args, *resolvedLocked)
			nextP++
		}
		query := fmt.Sprintf(`INSERT INTO mailing_campaigns (%s) VALUES (%s)`,
			strings.Join(colList, ", "), strings.Join(valList, ", "))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return "", "", false, fmt.Errorf("insert preparing campaign: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", "", false, fmt.Errorf("commit reservation: %w", err)
	}

	// Confirm the reservation row is durably committed before returning the id —
	// never return a 200 + campaign_id for a campaign that isn't actually
	// persisted (2026-06-22 staging/deploy burst false-success).
	if err := verifyCampaignPersisted(s.db, campaignID.String(), orgID); err != nil {
		return "", "", false, fmt.Errorf("reserve campaign for deploy: %w", err)
	}

	// Audience-identity links (audience unification W1): persist which
	// segments this campaign includes/excludes so the identity survives
	// outside the pmta_config blob. Non-fatal (log + continue) and
	// kill-switch guarded — must never fail a deploy.
	writeCampaignAudienceLinks(ctx, s.db, campaignID.String(), input.InclusionSegments, input.ExclusionSegments)

	return campaignID.String(), "", false, nil
}

// finalizeDeploy runs the slow audience planning and campaign persistence in
// the background. On success the campaign transitions to scheduled/sending;
// on failure it is marked as 'failed'.
func (s *PMTACampaignService) finalizeDeploy(campaignID, orgID string, input engine.PMTACampaignInput, normalized pmtaNormalizedCampaign) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Deploy/BG] PANIC finalizing campaign %s: %v", campaignID, r)
			s.markCampaignFailed(campaignID, fmt.Sprintf("internal panic: %v", r))
		}
	}()

	// Extended statement_timeout for audience queries
	var audienceDB dbQuerier = s.db
	conn, connErr := s.db.Conn(ctx)
	if connErr == nil {
		if _, err := conn.ExecContext(ctx, "SET statement_timeout = '600s'"); err == nil {
			audienceDB = conn
		}
		defer func() {
			conn.ExecContext(context.Background(), "RESET statement_timeout")
			conn.Close()
		}()
	}

	audience, err := planPMTAAudience(ctx, audienceDB, orgID, input, normalized, s.suppMatcher, s.globalHub, s.offerSuppMgr)
	if err != nil {
		log.Printf("[Deploy/BG] audience planning failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "audience planning failed: "+err.Error())
		return
	}

	// Force the campaign_id so createPMTAWaveCampaign reuses the reserved row.
	// The row was set to 'preparing' by reserveCampaignForDeploy; we need to
	// temporarily set it back to 'draft' so resolvePMTACampaignIdentity accepts it.
	if _, err := s.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'draft' WHERE id = $1`, campaignID); err != nil {
		log.Printf("[Deploy/BG] failed to reset status for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "internal error preparing campaign")
		return
	}
	input.CampaignID = campaignID

	// Attribution stamp — BEFORE the wave TX, same ordering as finalizeAudience
	// (stamp must be visible before any wave becomes dispatchable). Kept in
	// lockstep so any caller of this legacy synchronous finalizer stamps
	// identically. Log-and-continue; kill switch: DISABLE_ATTRIBUTION_STAMPING=1.
	stampSubject, stampHTML, stampFromName := "", "", ""
	if len(input.Variants) > 0 {
		stampSubject = input.Variants[0].Subject
		stampHTML = input.Variants[0].HTMLContent
		stampFromName = input.Variants[0].FromName
	}
	stampCtx, stampCancel := context.WithTimeout(context.Background(), 15*time.Second)
	stampCampaignAttribution(stampCtx, s.db, orgID, campaignID, input, input.Name, stampSubject, stampHTML, stampFromName)
	stampCancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[Deploy/BG] begin tx failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "internal error: "+err.Error())
		return
	}
	defer tx.Rollback()

	result, err := createPMTAWaveCampaign(ctx, tx, s.db, orgID, input, normalized, audience, s.colCache)
	if err != nil {
		log.Printf("[Deploy/BG] create wave campaign failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "campaign creation failed: "+err.Error())
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[Deploy/BG] commit failed for %s: %v", campaignID, err)
		s.markCampaignFailed(campaignID, "commit failed: "+err.Error())
		return
	}

	log.Printf("[Deploy/BG] campaign %s finalized: status=%s audience=%d variants=%d",
		campaignID, result.Status, result.TotalAudience, result.VariantCount)
}

// markCampaignFailed sets a campaign's status to 'failed' after a background
// deploy error.
func (s *PMTACampaignService) markCampaignFailed(campaignID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log.Printf("[Deploy/BG] marking campaign %s as failed: %s", campaignID, reason)
	if _, err := s.db.ExecContext(ctx, `UPDATE mailing_campaigns SET status = 'failed', updated_at = NOW() WHERE id = $1`, campaignID); err != nil {
		log.Printf("[Deploy/BG] CRITICAL: failed to mark campaign %s as failed: %v (campaign may be orphaned in preparing)", campaignID, err)
	}
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

	preflight := s.runPreflight(ctx, orgID, input.SendingDomain, input.SendingProfileID)

	normalized, err := normalizePMTACampaignInput(input)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var audienceDB dbQuerier = s.db
	conn, connErr := s.db.Conn(ctx)
	if connErr == nil {
		if _, err := conn.ExecContext(ctx, "SET statement_timeout = '600s'"); err == nil {
			audienceDB = conn
		}
		defer func() {
			conn.ExecContext(context.Background(), "RESET statement_timeout")
			conn.Close()
		}()
	}

	audience, err := planPMTAAudience(ctx, audienceDB, orgID, input, normalized, s.suppMatcher, s.globalHub, s.offerSuppMgr)
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
			ORDER BY is_default DESC, created_at DESC LIMIT 1
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
		inclusionIDs, _ := resolveListNamesToIDs(ctx, s.db, orgID, []string{"PMTA Test List"})
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
func resolveListNamesToIDs(ctx context.Context, db dbQuerier, orgID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return names, nil
	}
	var ids []string
	for _, name := range names {
		if _, err := uuid.Parse(name); err == nil {
			ids = append(ids, name)
			continue
		}
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
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("resolve list %q: %w", name, err)
		}
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_suppression_lists
			WHERE id = $1 OR LOWER(name) = LOWER($1)
			LIMIT 1
		`, name).Scan(&listID)
		if err == nil {
			ids = append(ids, listID)
		} else if err != sql.ErrNoRows {
			return nil, fmt.Errorf("resolve suppression list %q: %w", name, err)
		} else {
			log.Printf("[resolveListNamesToIDs] list %q not found for org %s", name, orgID)
		}
	}
	return ids, nil
}

// domainToISPLookup delegates to the canonical isp.GroupFromDomain classifier.
func domainToISPLookup(domain string) string {
	return isp.GroupFromDomain(domain)
}

// HandleRetryCampaign requeues a failed or draft campaign for audience
// finalization. The campaign must still have its pmta_config stored.
func (s *PMTACampaignService) HandleRetryCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")

	var name, status string
	var configJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(name,''), status, pmta_config::text
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&name, &status, &configJSON)
	if err != nil {
		respondJSON(w, 404, map[string]string{"error": "campaign not found"})
		return
	}
	if status != "draft" && status != "failed" {
		respondJSON(w, 409, map[string]string{"error": "can only retry draft or failed campaigns", "status": status})
		return
	}
	if !configJSON.Valid || configJSON.String == "" || configJSON.String == "{}" {
		respondJSON(w, 400, map[string]string{"error": "campaign has no pmta_config — cannot retry"})
		return
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'finalizing_audience', updated_at = NOW()
		WHERE id = $1
	`, campaignID)
	if err != nil {
		respondJSON(w, 500, map[string]string{"error": "failed to update status: " + err.Error()})
		return
	}

	log.Printf("[PMTA-Retry] campaign %s (%s) requeued for audience finalization", campaignID, name)
	respondJSON(w, 200, map[string]interface{}{
		"id":      campaignID,
		"name":    name,
		"status":  "finalizing_audience",
		"message": "Campaign requeued — AudienceWorker will pick it up within 15s",
	})
}

// HandleTriggerSend manually enqueues and processes a scheduled campaign,
// bypassing the scheduler goroutine (useful when the scheduler is not running).
func (s *PMTACampaignService) HandleTriggerSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID

	// SK-5 HARD SEND PATH: when Kafka send-routing is ON, the Kafka consumer
	// (QueueWriterConsumer) is the ONLY thing permitted to INSERT into
	// mailing_campaign_queue. This manual/admin trigger-send is a LEGACY bypass
	// that INSERTs directly; BLOCK it loudly so any unexpected use is a visible,
	// Kafka-attributable failure rather than a silent bypass of the hard path.
	// When routing is OFF (the dark default) this guard is a no-op and the path
	// behaves byte-identically to today.
	if sendqueue.SendRouteEnabled() {
		log.Printf("[kafka-route] BLOCKED direct enqueue at api.HandleTriggerSend — Kafka routing is ON; this path must be routed")
		respondJSON(w, 409, map[string]string{"error": "kafka send-routing is ON: direct trigger-send enqueue is disabled; sends route through the Kafka send path"})
		return
	}

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
	var fromName, fromEmail, replyTo, subject, htmlContent, plainContent, profileID string
	var listIDsJSON string
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(from_name,''), COALESCE(from_email,''), COALESCE(reply_to,''),
		       subject, COALESCE(html_content,''), COALESCE(plain_content,''),
		       COALESCE(sending_profile_id::text,''), COALESCE(list_ids::text,'[]')
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&fromName, &fromEmail, &replyTo, &subject, &htmlContent, &plainContent, &profileID, &listIDsJSON)

	var listIDs []string
	json.Unmarshal([]byte(listIDsJSON), &listIDs)
	if len(listIDs) == 0 {
		respondJSON(w, 200, map[string]string{"error": "no list_ids", "id": campaignID})
		return
	}

	// Enqueue subscribers.
	//
	// Pre-filter BOTH legacy global suppression AND brand-scoped suppression
	// at enqueue time so we don't waste queue slots and worker cycles on
	// subscribers who will be skipped at claim time. The brand-scoped check
	// uses the campaign's sending-domain suffix match against brand_root —
	// same shape as mailing_analytics.go so the two surfaces report
	// consistent active audiences. $3 is the sending domain we precompute
	// from the campaign's from_email.
	sendingDomain := ""
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at+1 < len(fromEmail) {
		sendingDomain = strings.ToLower(fromEmail[at+1:])
	}
	enqueued := 0
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.email FROM mailing_subscribers s
		WHERE s.list_id = ANY($1) AND s.status = 'confirmed' AND s.is_bot = false
		  AND NOT EXISTS (SELECT 1 FROM mailing_campaign_queue WHERE campaign_id = $2 AND subscriber_id = s.id)
		  AND NOT EXISTS (SELECT 1 FROM mailing_suppressions WHERE LOWER(email) = LOWER(s.email) AND active = true)
		  AND NOT EXISTS (
			SELECT 1 FROM mailing_domain_suppressions ds
			WHERE ds.email_hash = md5(LOWER(s.email))
			  AND ($3 = ds.brand_root OR $3 LIKE '%.' || ds.brand_root)
		  )
	`, pq.Array(listIDs), campaignID, sendingDomain)
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
				(id, campaign_id, subscriber_id, email, subject, html_content, plain_content, from_name, from_email, reply_to, sending_profile_id, esp_type, status, priority, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pmta', 'pending', 0, NOW())
		`, campaignID, subID, email, subject, htmlContent, plainContent, fromName, fromEmail, replyTo, profileID)
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

	// Resolve the recent-campaign ids first, then aggregate the queue with an
	// explicit = ANY(array) predicate. The previous IN-(subquery on
	// created_at > NOW()-6h) form let the planner misestimate and fall back
	// to a full heap scan of the multi-GB queue table — one of the uncached
	// "on-demand twins" called out in CAMPAIGN_QUEUE_STORAGE_REDESIGN.md
	// §4.2. With explicit ids the (campaign_id, ...) indexes always drive a
	// nested-loop, and we skip the queue entirely when there are no recent
	// campaigns.
	var recentCampaignIDs []string
	idRows, err := s.db.QueryContext(ctx, `
		SELECT id::text FROM mailing_campaigns
		WHERE organization_id = $1 AND created_at > NOW() - INTERVAL '6 hours'
	`, orgID)
	if err == nil {
		defer idRows.Close()
		for idRows.Next() {
			var id string
			if idRows.Scan(&id) == nil {
				recentCampaignIDs = append(recentCampaignIDs, id)
			}
		}
	}

	var queueStats []queueStat
	if len(recentCampaignIDs) > 0 {
		qrows, err := s.db.QueryContext(ctx, `
			SELECT campaign_id::text, status, COUNT(*), COALESCE(MAX(error_message),'')
			FROM mailing_campaign_queue
			WHERE campaign_id = ANY($1::uuid[])
			GROUP BY campaign_id, status
			ORDER BY campaign_id, status
		`, pq.Array(recentCampaignIDs))
		if err == nil {
			defer qrows.Close()
			for qrows.Next() {
				var qs queueStat
				qrows.Scan(&qs.CampaignID, &qs.Status, &qs.Count, &qs.Error)
				queueStats = append(queueStats, qs)
			}
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
		input.Subject = "SES-PMTA Relay Test"
	}
	if input.Domain == "" {
		input.Domain = "m.discountblog.com"
	}

	fromEmail := fmt.Sprintf("test@%s", input.Domain)
	now := time.Now().Format(time.RFC1123Z)

	// Build RFC822 message
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("From: SES Relay Test <%s>\r\n", fromEmail))
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
<p>If you received this, the relay chain is working: <code>Origin -&gt; PMTA Bridge -&gt; PMTA -&gt; SES SMTP -&gt; Gmail</code></p>
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
		// No conditions defined: if a list is bound, return all subscribers in
		// that list (inclusion use-case). Without a list, return an empty set
		// so exclusion segments with empty conditions never accidentally
		// exclude every subscriber in the system.
		if listIDVal != nil {
			return BuildSegmentSubscriberQuery(listIDVal, nil)
		}
		return "SELECT id::text, email FROM mailing_subscribers WHERE FALSE", nil
	}

	// Criteria-v2 segments ({"v2":{...}} — audience unification Phase 2)
	// compile to one indexed SELECT. This branch MUST run before the legacy
	// parsers: a v2 blob unmarshals into ConditionGroupBuilder with an empty
	// LogicOperator and would fall to the legacy array parser, whose
	// discarded error degrades to an unscoped full-base query (the same
	// hazard class as lake_spec). Invalid v2 fails CLOSED to the empty set.
	if v2, v2Err := segmentation.ParseV2Criteria(raw); v2Err != nil {
		log.Printf("[buildSegmentQuery] invalid criteria-v2 conditions — failing closed to empty set: %v", v2Err)
		return "SELECT id::text, email FROM mailing_subscribers WHERE FALSE", nil
	} else if v2 != nil {
		query, args, cErr := segmentation.CompileV2SQL(v2)
		if cErr != nil {
			log.Printf("[buildSegmentQuery] criteria-v2 compile error — failing closed to empty set: %v", cErr)
			return "SELECT id::text, email FROM mailing_subscribers WHERE FALSE", nil
		}
		return query, args
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
		  -- BROADCAST campaigns only (operator 2026-07-02): the clone picker must
		  -- not offer partner-drip / click-drip mini-campaigns (hundreds/day) —
		  -- you clone an operator broadcast, never a drip wave.
		  AND partner_drip_tag IS NULL
		  AND COALESCE(campaign_type, '') <> 'click_drip'
		ORDER BY COALESCE(completed_at, started_at, created_at) DESC
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
// formatted as a draft response the wizard can hydrate (with identity stripped).
func (s *PMTACampaignService) HandleCloneData(w http.ResponseWriter, r *http.Request) {
	s.loadCampaignData(w, r, true)
}

// HandleEditData returns the full PMTA config for a specific campaign,
// preserving campaign_id so the wizard can edit the existing campaign.
func (s *PMTACampaignService) HandleEditData(w http.ResponseWriter, r *http.Request) {
	s.loadCampaignData(w, r, false)
}

// loadCampaignData is the shared implementation for clone-data and edit-data.
// When forClone is true, campaign_id is stripped and "(Clone)" is appended.
// When forClone is false, identity is preserved for editing.
func (s *PMTACampaignService) loadCampaignData(w http.ResponseWriter, r *http.Request, forClone bool) {
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
			log.Printf("[CampaignData] query error for %s: %v", campaignID, err)
			http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
			return
		}
	} else {
		err := s.db.QueryRowContext(ctx, `
			SELECT name, status, completed_at
			FROM mailing_campaigns
			WHERE id = $1 AND organization_id = $2
		`, campaignID, orgID).Scan(&name, &status, &completedAt)
		if err != nil {
			log.Printf("[CampaignData] query error for %s: %v", campaignID, err)
			http.Error(w, `{"error":"campaign not found"}`, http.StatusNotFound)
			return
		}
	}

	scheduleMode := "quick"
	var inputMap map[string]interface{}

	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		var cfg struct {
			CampaignInput json.RawMessage `json:"campaign_input"`
			ScheduleMode  string          `json:"schedule_mode,omitempty"`
		}
		if err := json.Unmarshal([]byte(configJSON.String), &cfg); err == nil && len(cfg.CampaignInput) > 2 {
			if err := json.Unmarshal(cfg.CampaignInput, &inputMap); err != nil {
				inputMap = nil
			} else {
				delete(inputMap, "campaign_id")
				if forClone {
					inputMap["name"] = name + " (Clone)"
				} else {
					inputMap["name"] = name
				}
				if cfg.ScheduleMode != "" {
					scheduleMode = cfg.ScheduleMode
				}
			}
		}
	}

	if inputMap == nil {
		inputMap = buildCloneInputFromDB(ctx, s.db, campaignID, name)
		if forClone {
			inputMap["name"] = name + " (Clone)"
		}
	}

	enrichCloneInput(ctx, s.db, campaignID, inputMap)

	clonedJSON, _ := json.Marshal(inputMap)

	respCampaignID := ""
	respName := name + " (Clone)"
	respStatus := "draft"
	respSourceID := campaignID
	if !forClone {
		respCampaignID = campaignID
		respName = name
		respStatus = status
		respSourceID = ""
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id":    respCampaignID,
		"name":           respName,
		"status":         respStatus,
		"schedule_mode":  scheduleMode,
		"source_id":      respSourceID,
		"source_status":  status,
		"campaign_input": json.RawMessage(clonedJSON),
	})
}

// HandleCampaignISPVolume returns the REAL per-ISP recipient volume for a single
// campaign, read from the relational mailing_campaign_isp_plans table.
//
// Why this exists: the edit-data endpoint exposes per-ISP volume only as
// campaign_input.isp_quotas[].volume, which is the declared CAP. Segment-sourced
// engager campaigns intentionally set volume:0 per ISP (= uncapped / audience-bound),
// so the Send-Day board's ISP Composition drill-down had nothing to render and fell
// back to an "Uncapped" empty state. The planner-selected audience size per ISP lives
// in audience_selected_count (and actual sends in sent_count), which is NOT in the
// pmta_config blob — only here. Returned per ISP: planned (audience_selected_count),
// sent, the declared quota, and the estimate, so the UI can prefer real planned
// volume, fall back to a non-zero quota, and still list targeted ISPs otherwise.
func (s *PMTACampaignService) HandleCampaignISPVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	orgID := getOrgID(r)

	type ispVolume struct {
		ISP       string `json:"isp"`
		Planned   int    `json:"planned"`   // audience_selected_count — recipients the planner selected
		Sent      int    `json:"sent"`      // sent_count — actual sends so far
		Quota     int    `json:"quota"`     // declared per-ISP cap (0 = uncapped / audience-bound)
		Estimated int    `json:"estimated"` // audience_estimated_count — pre-finalisation estimate
	}

	out := []ispVolume{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT isp,
		       COALESCE(audience_selected_count, 0),
		       COALESCE(sent_count, 0),
		       COALESCE(quota, 0),
		       COALESCE(audience_estimated_count, 0)
		FROM mailing_campaign_isp_plans
		WHERE campaign_id = $1 AND organization_id = $2
		ORDER BY isp
	`, campaignID, orgID)
	if err != nil {
		log.Printf("[CampaignISPVolume] query error for %s: %v", campaignID, err)
		respondJSON(w, http.StatusOK, map[string]interface{}{"isps": out})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var v ispVolume
		if err := rows.Scan(&v.ISP, &v.Planned, &v.Sent, &v.Quota, &v.Estimated); err != nil {
			continue
		}
		out = append(out, v)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"isps": out})
}

// buildCloneInputFromDB reconstructs a full campaign_input map from relational
// DB columns when pmta_config is absent or unusable. This is the fallback path.
func buildCloneInputFromDB(ctx context.Context, db *sql.DB, campaignID, name string) map[string]interface{} {
	var subject, fromName, fromEmail, htmlContent, previewText sql.NullString
	var listIDsJSON, suppListIDsJSON, suppSegIDsJSON sql.NullString
	var scheduledAt sql.NullTime

	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(subject, ''), COALESCE(from_name, ''), COALESCE(from_email, ''),
		       COALESCE(html_content, ''), COALESCE(preview_text, ''),
		       COALESCE(list_ids::text, '[]'), COALESCE(suppression_list_ids::text, '[]'),
		       COALESCE(suppression_segment_ids::text, '[]'), scheduled_at
		FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&subject, &fromName, &fromEmail, &htmlContent, &previewText,
		&listIDsJSON, &suppListIDsJSON, &suppSegIDsJSON, &scheduledAt)
	if err != nil {
		log.Printf("[CloneData] fallback base query error for %s: %v", campaignID, err)
	}

	var inclusionLists, exclusionLists, exclusionSegments []string
	json.Unmarshal([]byte(listIDsJSON.String), &inclusionLists)
	json.Unmarshal([]byte(suppListIDsJSON.String), &exclusionLists)
	json.Unmarshal([]byte(suppSegIDsJSON.String), &exclusionSegments)
	if inclusionLists == nil {
		inclusionLists = []string{}
	}
	if exclusionLists == nil {
		exclusionLists = []string{}
	}
	if exclusionSegments == nil {
		exclusionSegments = []string{}
	}

	sendMode := "immediate"
	if scheduledAt.Valid {
		sendMode = "scheduled"
	}

	// Read ISP plans including sending_domain and randomize_audience
	planRows, planErr := db.QueryContext(ctx, `
		SELECT isp, quota, COALESCE(throttle_strategy, 'auto'), COALESCE(timezone, 'UTC'),
		       sending_domain, randomize_audience
		FROM mailing_campaign_isp_plans
		WHERE campaign_id = $1 ORDER BY isp
	`, campaignID)

	var ispPlans []map[string]interface{}
	var targetISPs []string
	var ispQuotas []map[string]interface{}
	sendingDomain := ""
	randomizeAudience := false

	if planErr != nil {
		log.Printf("[CloneData] ISP plans query error for %s: %v", campaignID, planErr)
	} else {
		defer planRows.Close()
		allRandomize := true
		anyRow := false
		for planRows.Next() {
			var ispName string
			var quota int
			var throttle, tz, planDomain string
			var planRandomize bool
			if err := planRows.Scan(&ispName, &quota, &throttle, &tz, &planDomain, &planRandomize); err != nil {
				log.Printf("[CloneData] ISP plan scan error: %v", err)
				continue
			}
			anyRow = true
			targetISPs = append(targetISPs, ispName)
			ispQuotas = append(ispQuotas, map[string]interface{}{"isp": ispName, "volume": quota})
			ispPlans = append(ispPlans, map[string]interface{}{
				"isp": ispName, "quota": quota, "throttle_strategy": throttle, "timezone": tz,
			})
			if sendingDomain == "" {
				sendingDomain = planDomain
			}
			if !planRandomize {
				allRandomize = false
			}
		}
		if anyRow {
			randomizeAudience = allRandomize
		}
	}

	// Ultimate fallback for sending_domain: split from from_email
	if sendingDomain == "" && fromEmail.Valid && strings.Contains(fromEmail.String, "@") {
		parts := strings.SplitN(fromEmail.String, "@", 2)
		if len(parts) == 2 {
			sendingDomain = parts[1]
		}
	}

	// Read A/B variants
	variants := loadCloneVariants(ctx, db, campaignID)
	if len(variants) == 0 {
		variants = []map[string]interface{}{{
			"variant_name":  "A",
			"from_name":     fromName.String,
			"subject":       subject.String,
			"preview_text":  previewText.String,
			"html_content":  htmlContent.String,
			"split_percent": float64(100),
		}}
	}

	return map[string]interface{}{
		"name":               name + " (Clone)",
		"target_isps":        targetISPs,
		"sending_domain":     sendingDomain,
		"isp_plans":          ispPlans,
		"isp_quotas":         ispQuotas,
		"variants":           variants,
		"inclusion_lists":    inclusionLists,
		"inclusion_segments": []string{},
		"exclusion_lists":    exclusionLists,
		"exclusion_segments": exclusionSegments,
		"send_priority":      []interface{}{},
		"randomize_audience": randomizeAudience,
		"send_mode":          sendMode,
	}
}

// enrichCloneInput fills empty/nil fields in an existing campaign_input map
// from authoritative DB sources. Used for both the primary pmta_config path
// (handles stale blobs missing newer fields) and the fallback reconstruction.
func enrichCloneInput(ctx context.Context, db *sql.DB, campaignID string, m map[string]interface{}) {
	// 1. Sending domain: prefer ISP plan table over from_email splitting
	if strVal, _ := m["sending_domain"].(string); strVal == "" {
		var domain sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT DISTINCT sending_domain
			FROM mailing_campaign_isp_plans
			WHERE campaign_id = $1
			LIMIT 1
		`, campaignID).Scan(&domain)
		if err == nil && domain.Valid && domain.String != "" {
			m["sending_domain"] = domain.String
		}
	}

	// 2. Lists and segments + scheduled_at for send_mode inference
	needLists := isEmptySlice(m["inclusion_lists"])
	needExclLists := isEmptySlice(m["exclusion_lists"])
	needExclSegs := isEmptySlice(m["exclusion_segments"])
	needSendMode := false
	if sm, _ := m["send_mode"].(string); sm == "" {
		needSendMode = true
	}

	if needLists || needExclLists || needExclSegs || needSendMode {
		var listIDsJSON, suppListIDsJSON, suppSegIDsJSON sql.NullString
		var scheduledAt sql.NullTime
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(list_ids::text, '[]'), COALESCE(suppression_list_ids::text, '[]'),
			       COALESCE(suppression_segment_ids::text, '[]'), scheduled_at
			FROM mailing_campaigns WHERE id = $1
		`, campaignID).Scan(&listIDsJSON, &suppListIDsJSON, &suppSegIDsJSON, &scheduledAt)
		if err != nil {
			log.Printf("[CloneData] enrich lists query error for %s: %v", campaignID, err)
		} else {
			if needLists {
				var ids []string
				if json.Unmarshal([]byte(listIDsJSON.String), &ids) == nil && len(ids) > 0 {
					m["inclusion_lists"] = ids
				}
			}
			if needExclLists {
				var ids []string
				if json.Unmarshal([]byte(suppListIDsJSON.String), &ids) == nil && len(ids) > 0 {
					m["exclusion_lists"] = ids
				}
			}
			if needExclSegs {
				var ids []string
				if json.Unmarshal([]byte(suppSegIDsJSON.String), &ids) == nil && len(ids) > 0 {
					m["exclusion_segments"] = ids
				}
			}
			if needSendMode {
				if scheduledAt.Valid {
					m["send_mode"] = "scheduled"
				} else {
					m["send_mode"] = "immediate"
				}
			}
		}
	}

	// 3. Randomize audience: only enrich when key is absent (not when false)
	if _, exists := m["randomize_audience"]; !exists {
		var ra sql.NullBool
		err := db.QueryRowContext(ctx, `
			SELECT bool_and(randomize_audience)
			FROM mailing_campaign_isp_plans
			WHERE campaign_id = $1
			HAVING count(*) > 0
		`, campaignID).Scan(&ra)
		if err == nil && ra.Valid {
			m["randomize_audience"] = ra.Bool
		}
	}

	// 4. A/B variants: enrich when missing, empty, or single variant with blank content
	needVariants := false
	switch v := m["variants"].(type) {
	case nil:
		needVariants = true
	case []interface{}:
		if len(v) == 0 {
			needVariants = true
		} else if len(v) == 1 {
			if vm, ok := v[0].(map[string]interface{}); ok {
				if html, _ := vm["html_content"].(string); html == "" {
					needVariants = true
				}
			}
		}
	case []map[string]interface{}:
		if len(v) == 0 {
			needVariants = true
		} else if len(v) == 1 {
			if html, _ := v[0]["html_content"].(string); html == "" {
				needVariants = true
			}
		}
	}
	if needVariants {
		variants := loadCloneVariants(ctx, db, campaignID)
		if len(variants) > 0 {
			m["variants"] = variants
		}
	}
}

// loadCloneVariants reads A/B variants from the DB for a given campaign.
// Returns nil if no variants are found.
func loadCloneVariants(ctx context.Context, db *sql.DB, campaignID string) []map[string]interface{} {
	rows, err := db.QueryContext(ctx, `
		SELECT v.variant_name, COALESCE(v.from_name, ''), COALESCE(v.subject, ''),
		       COALESCE(v.preheader, ''), COALESCE(v.html_content, ''),
		       COALESCE(v.split_percent, 50)
		FROM mailing_ab_variants v
		JOIN mailing_ab_tests t ON t.id = v.test_id
		WHERE t.campaign_id = $1
		ORDER BY v.variant_name ASC
	`, campaignID)
	if err != nil {
		log.Printf("[CloneData] AB variants query error for %s: %v", campaignID, err)
		return nil
	}
	defer rows.Close()

	var variants []map[string]interface{}
	for rows.Next() {
		var vName, vFromName, vSubject, vPreheader, vHTML string
		var vSplit int
		if err := rows.Scan(&vName, &vFromName, &vSubject, &vPreheader, &vHTML, &vSplit); err != nil {
			log.Printf("[CloneData] AB variant scan error: %v", err)
			continue
		}
		variants = append(variants, map[string]interface{}{
			"variant_name":  vName,
			"from_name":     vFromName,
			"subject":       vSubject,
			"preview_text":  vPreheader,
			"html_content":  vHTML,
			"split_percent": float64(vSplit),
		})
	}
	return variants
}

// isEmptySlice returns true if v is nil or an empty slice-like value.
func isEmptySlice(v interface{}) bool {
	if v == nil {
		return true
	}
	switch s := v.(type) {
	case []interface{}:
		return len(s) == 0
	case []string:
		return len(s) == 0
	case []map[string]interface{}:
		return len(s) == 0
	}
	return false
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

// HandleDeliverabilityRecommendations uses Claude to analyze 3-day ISP sending
// history for a given sending domain and return per-ISP quota recommendations.
func (s *PMTACampaignService) HandleDeliverabilityRecommendations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx := r.Context()

	var req struct {
		SendingDomain string `json:"sending_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SendingDomain == "" {
		respondError(w, http.StatusBadRequest, "sending_domain is required")
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		respondError(w, http.StatusInternalServerError, "ANTHROPIC_API_KEY not configured")
		return
	}

	// Query 1: Per-ISP daily volumes from mailing_campaign_isp_plans (3 days)
	type ispDayVolume struct {
		ISP      string `json:"isp"`
		SendDate string `json:"send_date"`
		Quota    int    `json:"quota"`
		Sent     int    `json:"sent"`
		Failed   int    `json:"failed"`
		Campaigns int   `json:"campaigns"`
	}
	var volumes []ispDayVolume

	volRows, err := s.db.QueryContext(ctx, `
		SELECT p.isp, DATE(c.created_at)::text AS send_date,
		       COALESCE(SUM(p.quota), 0), COALESCE(SUM(p.sent_count), 0),
		       COALESCE(SUM(p.failed_count), 0), COUNT(DISTINCT c.id)
		FROM mailing_campaign_isp_plans p
		JOIN mailing_campaigns c ON c.id = p.campaign_id
		WHERE c.organization_id = $1
		  AND p.sending_domain = $2
		  AND c.created_at >= NOW() - INTERVAL '3 days'
		  AND c.status IN ('completed', 'sent', 'sending', 'completed_with_errors')
		GROUP BY p.isp, DATE(c.created_at)
		ORDER BY DATE(c.created_at) DESC, p.isp
	`, orgID, req.SendingDomain)
	if err != nil {
		log.Printf("[deliverability-recs] volume query error: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query ISP volumes")
		return
	}
	defer volRows.Close()
	for volRows.Next() {
		var v ispDayVolume
		if err := volRows.Scan(&v.ISP, &v.SendDate, &v.Quota, &v.Sent, &v.Failed, &v.Campaigns); err != nil {
			log.Printf("[deliverability-recs] volume scan error: %v", err)
			continue
		}
		volumes = append(volumes, v)
	}

	// Query 2: Campaign-level engagement metrics (3 days, same domain)
	type campaignMetric struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		SendDate     string `json:"send_date"`
		Sent         int    `json:"sent"`
		Opens        int    `json:"opens"`
		Clicks       int    `json:"clicks"`
		HardBounces  int    `json:"hard_bounces"`
		SoftBounces  int    `json:"soft_bounces"`
		Complaints   int    `json:"complaints"`
	}
	var campaigns []campaignMetric

	campRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (c.id)
		       c.id::text, c.name, DATE(c.created_at)::text AS send_date,
		       COALESCE(c.sent_count, 0), COALESCE(c.open_count, 0),
		       COALESCE(c.click_count, 0), COALESCE(c.hard_bounce_count, 0),
		       COALESCE(c.soft_bounce_count, 0), COALESCE(c.complaint_count, 0)
		FROM mailing_campaigns c
		JOIN mailing_campaign_isp_plans p ON p.campaign_id = c.id
		WHERE c.organization_id = $1
		  AND p.sending_domain = $2
		  AND c.created_at >= NOW() - INTERVAL '3 days'
		  AND c.status IN ('completed', 'sent', 'sending', 'completed_with_errors')
		ORDER BY c.id, c.created_at DESC
	`, orgID, req.SendingDomain)
	if err != nil {
		log.Printf("[deliverability-recs] campaign query error: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to query campaign metrics")
		return
	}
	defer campRows.Close()
	for campRows.Next() {
		var cm campaignMetric
		if err := campRows.Scan(&cm.ID, &cm.Name, &cm.SendDate, &cm.Sent, &cm.Opens, &cm.Clicks, &cm.HardBounces, &cm.SoftBounces, &cm.Complaints); err != nil {
			log.Printf("[deliverability-recs] campaign scan error: %v", err)
			continue
		}
		campaigns = append(campaigns, cm)
	}

	if len(volumes) == 0 && len(campaigns) == 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"recommendations":  []interface{}{},
			"overall_summary":  "No sending data found for " + req.SendingDomain + " in the last 3 days. Set quotas manually based on your warmup plan.",
			"cautions":         []string{"No historical data available for AI analysis"},
			"data_context":     map[string]interface{}{"isp_volumes": []interface{}{}, "campaigns": []interface{}{}},
		})
		return
	}

	// Build the data summary for Claude
	volJSON, _ := json.MarshalIndent(volumes, "", "  ")
	campJSON, _ := json.MarshalIndent(campaigns, "", "  ")

	systemPrompt := `You are a senior email deliverability analyst. You analyze ISP sending data and recommend optimal sending quotas.

RULES:
- Analyze the 3-day sending history provided below.
- For each ISP that has data, recommend a quota for the next campaign.
- Base recommendations on: volume trends, failure rates, and engagement rates.
- If an ISP shows increasing failures or complaints, recommend reducing volume.
- If an ISP is performing well (low bounces, decent opens), recommend maintaining or slightly increasing.
- Be conservative with recommendations — reputation damage is hard to reverse.
- If an ISP had zero sends in the window, recommend starting small (100-500).
- Factor in warmup constraints: IPs in warmup should not exceed ~500/day per ISP initially.

RESPONSE FORMAT — return ONLY valid JSON, no markdown fences:
{
  "recommendations": [
    {
      "isp": "<isp name>",
      "suggested_quota": <integer>,
      "trend": "<increasing|stable|decreasing|new>",
      "risk_level": "<low|medium|high>",
      "rationale": "<1-2 sentence explanation>"
    }
  ],
  "overall_summary": "<2-3 sentence overall assessment>",
  "cautions": ["<any warnings or things to monitor>"]
}`

	userPrompt := fmt.Sprintf(`Analyze the following 3-day sending data for domain "%s" and provide ISP quota recommendations.

ISP DAILY VOLUMES (quota set vs actually sent vs failed, per day):
%s

CAMPAIGN-LEVEL ENGAGEMENT (opens, clicks, bounces, complaints per campaign):
%s

Provide your recommendations as JSON.`, req.SendingDomain, string(volJSON), string(campJSON))

	// Call Claude
	claudeReqBody := map[string]interface{}{
		"model":      "claude-opus-4-6",
		"max_tokens": 16000,
		"system":     systemPrompt,
		"thinking": map[string]interface{}{
			"type": "adaptive",
		},
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	jsonBody, err := json.Marshal(claudeReqBody)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to build AI request")
		return
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create AI request")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 90 * time.Second}
	aiResp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("[deliverability-recs] Claude request error: %v", err)
		respondError(w, http.StatusInternalServerError, "AI analysis request failed")
		return
	}
	defer aiResp.Body.Close()

	if aiResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(aiResp.Body)
		truncated := string(body)
		if len(truncated) > 500 {
			truncated = truncated[:500]
		}
		log.Printf("[deliverability-recs] Claude returned %d: %s", aiResp.StatusCode, truncated)
		var errDetail struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &errDetail) == nil && errDetail.Error.Message != "" {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("AI service error: %s", errDetail.Error.Message))
		} else {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("AI service returned %d", aiResp.StatusCode))
		}
		return
	}

	var claudeResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(aiResp.Body).Decode(&claudeResp); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to decode AI response")
		return
	}

	var raw string
	for _, block := range claudeResp.Content {
		if block.Type == "text" {
			raw = strings.TrimSpace(block.Text)
			break
		}
	}
	if raw == "" {
		respondError(w, http.StatusInternalServerError, "empty AI response — no text block")
		return
	}
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed struct {
		Recommendations []struct {
			ISP            string `json:"isp"`
			SuggestedQuota int    `json:"suggested_quota"`
			Trend          string `json:"trend"`
			RiskLevel      string `json:"risk_level"`
			Rationale      string `json:"rationale"`
		} `json:"recommendations"`
		OverallSummary string   `json:"overall_summary"`
		Cautions       []string `json:"cautions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("[deliverability-recs] failed to parse AI JSON: %v (raw: %.500s)", err, raw)
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"recommendations":  []interface{}{},
			"overall_summary":  "AI analysis returned an unparseable response. Raw output: " + raw[:min(len(raw), 300)],
			"cautions":         []string{"AI response could not be parsed"},
			"data_context":     map[string]interface{}{"isp_volumes": volumes, "campaigns": campaigns},
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"recommendations":  parsed.Recommendations,
		"overall_summary":  parsed.OverallSummary,
		"cautions":         parsed.Cautions,
		"data_context":     map[string]interface{}{"isp_volumes": volumes, "campaigns": campaigns},
	})
}

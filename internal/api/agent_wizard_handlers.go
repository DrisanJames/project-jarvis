package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// AgentWizardHandlers provides handlers for the AI-driven campaign configuration wizard.
// Users select an offer, set revenue targets, and the AI configures audience, timing, and ISP agents.
// It holds a reference to the parent Handlers so that collectors set after route
// registration are always accessible (avoids nil-at-copy-time bug).
type AgentWizardHandlers struct {
	db     *sql.DB
	parent *Handlers
}

func (awh *AgentWizardHandlers) getEverflowCollector() *everflow.Collector {
	if awh.parent != nil {
		return awh.parent.everflowCollector
	}
	return nil
}

func (awh *AgentWizardHandlers) getNetworkIntelCollector() *everflow.NetworkIntelligenceCollector {
	if awh.parent != nil {
		return awh.parent.networkIntelCollector
	}
	return nil
}

// ============================================================================
// Handler 1: HandleAnalyzeOffer
// GET /agent-wizard/analyze-offer?offer_id=X&offer_name=X
// ============================================================================

func (awh *AgentWizardHandlers) HandleAnalyzeOffer(w http.ResponseWriter, r *http.Request) {
	offerID := r.URL.Query().Get("offer_id")
	offerName := r.URL.Query().Get("offer_name")

	if offerID == "" && offerName == "" {
		respondError(w, http.StatusBadRequest, "offer_id or offer_name is required")
		return
	}

	ctx := r.Context()

	// Detect offer type from name
	offerType := everflow.GetOfferType(offerName)

	// --- Network performance from the network intel snapshot ---
	networkData := map[string]interface{}{
		"clicks":      int64(0),
		"conversions": int64(0),
		"revenue":     float64(0),
		"epc":         float64(0),
		"cvr":         float64(0),
	}

	if awh.getNetworkIntelCollector() != nil {
		snapshot := awh.getNetworkIntelCollector().GetSnapshot()
		if snapshot != nil {
			for _, offer := range snapshot.TopOffers {
				if offer.OfferID == offerID || strings.EqualFold(offer.OfferName, offerName) {
					networkData["clicks"] = offer.NetworkClicks
					networkData["conversions"] = offer.NetworkConversions
					networkData["revenue"] = offer.NetworkRevenue
					networkData["epc"] = offer.NetworkEPC
					networkData["cvr"] = offer.NetworkCVR
					if offerType == "OTHER" {
						offerType = offer.OfferType
					}
					break
				}
			}
		}
	}

	// --- Your team's performance for this offer (affiliates 9533, 9572) ---
	teamData := map[string]interface{}{
		"clicks":      int64(0),
		"conversions": int64(0),
		"revenue":     float64(0),
		"epc":         float64(0),
		"cvr":         float64(0),
	}

	if awh.getEverflowCollector() != nil {
		client := awh.getEverflowCollector().GetClient()
		if client != nil {
			now := time.Now()
			startDate := now.AddDate(0, 0, -30)
			affiliateIDs := []string{"9533", "9572"}
			report, err := client.GetEntityReportByOffer(ctx, startDate, now, affiliateIDs)
			if err == nil && report != nil {
				for _, row := range report.Table {
					var rowOfferID, rowOfferName string
					for _, col := range row.Columns {
						if col.ColumnType == "offer" {
							rowOfferID = col.ID
							rowOfferName = col.Label
							break
						}
					}
					if rowOfferID == offerID || strings.EqualFold(rowOfferName, offerName) {
						teamData["clicks"] = row.Reporting.TotalClick
						teamData["conversions"] = row.Reporting.Conversions
						teamData["revenue"] = row.Reporting.Revenue
						if row.Reporting.TotalClick > 0 {
							teamData["epc"] = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
							teamData["cvr"] = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick) * 100
						}
						break
					}
				}
			}
		}
	}

	// --- Local campaign history ---
	var campaignsCount int
	var totalSent sql.NullInt64
	var totalRevenue sql.NullFloat64

	if awh.db != nil && offerName != "" {
		err := awh.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(sent_count), 0), COALESCE(SUM(revenue), 0)
			 FROM mailing_campaigns WHERE name ILIKE '%' || $1 || '%'`,
			offerName,
		).Scan(&campaignsCount, &totalSent, &totalRevenue)
		if err != nil {
			campaignsCount = 0
		}
	}

	// --- Available payout types ---
	payoutTypes := []string{"CPS"}
	if strings.Contains(strings.ToUpper(offerName), "CPM") {
		payoutTypes = append(payoutTypes, "CPM")
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"offer": map[string]interface{}{
			"offer_id":   offerID,
			"offer_name": offerName,
			"offer_type": offerType,
		},
		"network":   networkData,
		"your_team": teamData,
		"local_history": map[string]interface{}{
			"campaigns_count": campaignsCount,
			"total_sent":      totalSent.Int64,
			"total_revenue":   totalRevenue.Float64,
		},
		"available_payout_types": payoutTypes,
	})
}

// ============================================================================
// Handler 2: HandleRevenueProjection
// POST /agent-wizard/revenue-projection
// ============================================================================

type revenueProjectionRequest struct {
	OfferID       string  `json:"offer_id"`
	OfferName     string  `json:"offer_name"`
	PayoutType    string  `json:"payout_type"`
	RevenueTarget float64 `json:"revenue_target"`
	ECPMLow       float64 `json:"ecpm_low"`
	ECPMHigh      float64 `json:"ecpm_high"`
}

func (awh *AgentWizardHandlers) HandleRevenueProjection(w http.ResponseWriter, r *http.Request) {
	var req revenueProjectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.RevenueTarget <= 0 {
		respondError(w, http.StatusBadRequest, "revenue_target must be positive")
		return
	}

	ecpmLow := req.ECPMLow
	ecpmHigh := req.ECPMHigh
	ecpmMid := (ecpmLow + ecpmHigh) / 2

	if ecpmMid <= 0 {
		respondError(w, http.StatusBadRequest, "ecpm range must be positive")
		return
	}

	// ── Mathematical Projections ──
	// Formula: Revenue = Sends × (ECPM / 1000)
	// Therefore: Sends = Revenue / (ECPM / 1000) = Revenue × 1000 / ECPM
	// Revenue per send = ECPM / 1000

	revenuePerSendLow := ecpmLow / 1000.0
	revenuePerSendMid := ecpmMid / 1000.0
	revenuePerSendHigh := ecpmHigh / 1000.0

	// Conservative (low ECPM = more sends needed)
	sendsNeededHigh := int64(math.Ceil(req.RevenueTarget / revenuePerSendLow))
	// Expected (mid ECPM)
	sendsNeededMid := int64(math.Ceil(req.RevenueTarget / revenuePerSendMid))
	// Optimistic (high ECPM = fewer sends needed)
	sendsNeededLow := int64(math.Ceil(req.RevenueTarget / revenuePerSendHigh))

	// Audience sizing: add 15% bounce/undeliverable buffer
	bounceBufferPct := 0.15
	audienceNeededHigh := int64(math.Ceil(float64(sendsNeededHigh) * (1 + bounceBufferPct)))
	audienceNeededMid := int64(math.Ceil(float64(sendsNeededMid) * (1 + bounceBufferPct)))
	audienceNeededLow := int64(math.Ceil(float64(sendsNeededLow) * (1 + bounceBufferPct)))

	// Projected revenue at each ECPM level given the mid sends volume
	projectedRevenueLow := float64(sendsNeededMid) * revenuePerSendLow
	projectedRevenueMid := float64(sendsNeededMid) * revenuePerSendMid
	projectedRevenueHigh := float64(sendsNeededMid) * revenuePerSendHigh

	// Confidence based on ECPM spread: tighter range = higher confidence
	spread := 0.0
	if ecpmMid > 0 {
		spread = (ecpmHigh - ecpmLow) / ecpmMid
	}
	confidence := "low"
	if spread < 0.2 {
		confidence = "high"
	} else if spread < 0.5 {
		confidence = "medium"
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"projection": map[string]interface{}{
			"target_revenue": req.RevenueTarget,
			"ecpm_range": map[string]interface{}{
				"low":  ecpmLow,
				"high": ecpmHigh,
				"mid":  math.Round(ecpmMid*100) / 100,
			},
			"revenue_per_send": map[string]interface{}{
				"low":  math.Round(revenuePerSendLow*10000) / 10000,
				"mid":  math.Round(revenuePerSendMid*10000) / 10000,
				"high": math.Round(revenuePerSendHigh*10000) / 10000,
			},
			"forecasted_volume": map[string]interface{}{
				"conservative": sendsNeededHigh,
				"expected":     sendsNeededMid,
				"optimistic":   sendsNeededLow,
			},
			"target_audience": map[string]interface{}{
				"conservative":   audienceNeededHigh,
				"expected":       audienceNeededMid,
				"optimistic":     audienceNeededLow,
				"bounce_buffer":  fmt.Sprintf("%.0f%%", bounceBufferPct*100),
			},
			"projected_revenue": map[string]interface{}{
				"at_low_ecpm":  math.Round(projectedRevenueLow*100) / 100,
				"at_mid_ecpm":  math.Round(projectedRevenueMid*100) / 100,
				"at_high_ecpm": math.Round(projectedRevenueHigh*100) / 100,
			},
			"formula":    "Revenue = Sends × (ECPM / 1000)",
			"confidence": confidence,
		},
	})
}

// ============================================================================
// Handler 3: HandleAIAudienceSelection
// POST /agent-wizard/audience-selection
// ============================================================================

type audienceSelectionRequest struct {
	OfferID             string   `json:"offer_id"`
	OfferName           string   `json:"offer_name"`
	RequiredSends       int64    `json:"required_sends"`
	PayoutType          string   `json:"payout_type"`
	SuppressionListIDs  []string `json:"suppression_list_ids"`
}

type segmentInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

func (awh *AgentWizardHandlers) HandleAIAudienceSelection(w http.ResponseWriter, r *http.Request) {
	var req audienceSelectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.RequiredSends <= 0 {
		respondError(w, http.StatusBadRequest, "required_sends must be positive")
		return
	}

	ctx := r.Context()

	if awh.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not available")
		return
	}

	// Query segments
	segRows, err := awh.db.QueryContext(ctx,
		`SELECT id, name, conditions FROM mailing_segments ORDER BY name`)
	if err != nil {
		log.Printf("ERROR: failed to query segments: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve segment data")
		return
	}
	defer segRows.Close()

	var segments []segmentInfo
	for segRows.Next() {
		var segID uuid.UUID
		var segName string
		var conditions sql.NullString
		if err := segRows.Scan(&segID, &segName, &conditions); err != nil {
			continue
		}

		// Get subscriber count for this segment
		// Extract list_id from conditions if available, otherwise count all confirmed subscribers
		var count int64
		if conditions.Valid && conditions.String != "" {
			// Try to extract list_id from JSONB conditions
			var condMap map[string]interface{}
			if json.Unmarshal([]byte(conditions.String), &condMap) == nil {
				if listIDVal, ok := condMap["list_id"]; ok {
					if listIDStr, ok := listIDVal.(string); ok {
						awh.db.QueryRowContext(ctx,
							`SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`,
							listIDStr,
						).Scan(&count)
					}
				}
			}
			// Fallback: count all confirmed subscribers if no specific list
			if count == 0 {
				awh.db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM mailing_subscribers WHERE status = 'confirmed'`,
				).Scan(&count)
			}
		} else {
			awh.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM mailing_subscribers WHERE status = 'confirmed'`,
			).Scan(&count)
		}

		segments = append(segments, segmentInfo{
			ID:    segID.String(),
			Name:  segName,
			Count: count,
		})
	}

	// Sort segments by count descending to select the top ones that meet required sends
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].Count > segments[j].Count
	})

	// Select segments until we meet the required sends
	var selectedSegments []segmentInfo
	var totalRecipients int64
	for _, seg := range segments {
		if totalRecipients >= req.RequiredSends {
			break
		}
		selectedSegments = append(selectedSegments, seg)
		totalRecipients += seg.Count
	}

	// If no segments were found, include all available
	if len(selectedSegments) == 0 {
		selectedSegments = segments
		for _, s := range segments {
			totalRecipients += s.Count
		}
	}

	// Query inbox profiles with high engagement
	var highEngagementCount int64
	var avgEngagement float64
	awh.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(AVG(engagement_score), 0) FROM mailing_inbox_profiles WHERE engagement_score >= 50`,
	).Scan(&highEngagementCount, &avgEngagement)

	// Engagement quality assessment
	engagementQuality := "low"
	if avgEngagement >= 70 {
		engagementQuality = "high"
	} else if avgEngagement >= 50 {
		engagementQuality = "medium"
	}

	// ── Query actual mailing lists with subscriber counts ──
	type listInfo struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		SubscriberCount int64  `json:"subscriber_count"`
		ActiveCount     int64  `json:"active_count"`
	}
	var mailingLists []listInfo
	listRows, listErr := awh.db.QueryContext(ctx,
		`SELECT l.id, l.name, COALESCE(l.subscriber_count, 0),
			(SELECT COUNT(*) FROM mailing_subscribers s WHERE s.list_id = l.id AND s.status = 'confirmed')
		 FROM mailing_lists l WHERE l.status = 'active' ORDER BY l.name`)
	if listErr == nil {
		defer listRows.Close()
		for listRows.Next() {
			var li listInfo
			if err := listRows.Scan(&li.ID, &li.Name, &li.SubscriberCount, &li.ActiveCount); err == nil {
				mailingLists = append(mailingLists, li)
			}
		}
	}

	// ── Collect list_ids from selected segments for cross-reference ──
	var segmentListIDs []string
	for _, seg := range selectedSegments {
		// Re-parse conditions to extract list_id
		var segConditions sql.NullString
		awh.db.QueryRowContext(ctx,
			`SELECT conditions FROM mailing_segments WHERE id = $1`, seg.ID,
		).Scan(&segConditions)
		if segConditions.Valid && segConditions.String != "" {
			var condMap map[string]interface{}
			if json.Unmarshal([]byte(segConditions.String), &condMap) == nil {
				if listIDVal, ok := condMap["list_id"]; ok {
					if listIDStr, ok := listIDVal.(string); ok {
						segmentListIDs = append(segmentListIDs, listIDStr)
					}
				}
			}
		}
	}

	// ── Query suppression lists with entry counts ──
	type suppressionInfo struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		EntryCount   int64  `json:"entry_count"`
		MatchedCount int64  `json:"matched_count"`
		IsGlobal     bool   `json:"is_global"`
	}
	var suppressionLists []suppressionInfo
	suppRows, suppErr := awh.db.QueryContext(ctx,
		`SELECT sl.id, sl.name, COALESCE(sl.entry_count, 0), COALESCE(sl.is_global, false)
		 FROM mailing_suppression_lists sl ORDER BY sl.is_global DESC, sl.name`)
	if suppErr == nil {
		defer suppRows.Close()
		for suppRows.Next() {
			var si suppressionInfo
			if err := suppRows.Scan(&si.ID, &si.Name, &si.EntryCount, &si.IsGlobal); err == nil {
				suppressionLists = append(suppressionLists, si)
			}
		}
	}

	// ── Cross-reference: sample-based suppression overlap ──
	// Strategy: take a statistically significant random sample of subscribers,
	// check those against suppression via the indexed md5_hash column,
	// then extrapolate per-list and total matches to the full audience.
	// With 5,000 samples we get ~1.4% margin of error at 95% confidence.
	log.Printf("Audience cross-reference: sampling subscribers against %d suppression lists...",
		len(suppressionLists))

	crossRefStart := time.Now()
	var totalMatched int64

	crossCtx, crossCancel := context.WithTimeout(ctx, 60*time.Second)
	defer crossCancel()

	const sampleSize = 5000

	// Step 1: Pull a random sample of subscriber emails from the target segments.
	// Use TABLESAMPLE BERNOULLI for speed (avoids full table sort from ORDER BY RANDOM()).
	// We request a small percentage and LIMIT to sampleSize.
	// Estimate sample pct: want ~sampleSize from totalRecipients. Ask for 2x to ensure enough rows.
	samplePct := float64(sampleSize*2) / float64(totalRecipients) * 100
	if samplePct > 100 {
		samplePct = 100
	}
	if samplePct < 0.01 {
		samplePct = 0.01
	}

	var sampleQuery string
	var sampleArgs []interface{}

	if len(segmentListIDs) > 0 {
		sampleQuery = fmt.Sprintf(`
			SELECT email FROM mailing_subscribers TABLESAMPLE BERNOULLI(%f)
			WHERE status = 'confirmed' AND list_id = ANY($1::uuid[])
			LIMIT $2
		`, samplePct)
		sampleArgs = []interface{}{
			fmt.Sprintf("{%s}", strings.Join(segmentListIDs, ",")),
			sampleSize,
		}
	} else {
		sampleQuery = fmt.Sprintf(`
			SELECT email FROM mailing_subscribers TABLESAMPLE BERNOULLI(%f)
			WHERE status = 'confirmed'
			LIMIT $1
		`, samplePct)
		sampleArgs = []interface{}{sampleSize}
	}

	sampleRows, sampleErr := awh.db.QueryContext(crossCtx, sampleQuery, sampleArgs...)

	var sampleEmails []string
	if sampleErr == nil {
		defer sampleRows.Close()
		for sampleRows.Next() {
			var email string
			if err := sampleRows.Scan(&email); err == nil {
				sampleEmails = append(sampleEmails, email)
			}
		}
	} else {
		log.Printf("WARNING: failed to sample subscribers: %v", sampleErr)
	}

	actualSampleSize := int64(len(sampleEmails))
	log.Printf("Audience cross-reference: sampled %d subscribers, computing MD5 hashes...", actualSampleSize)

	// Step 2: Compute MD5 hashes in Go (fast, avoids DB computation)
	sampleHashes := make([]string, len(sampleEmails))
	for i, email := range sampleEmails {
		h := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
		sampleHashes[i] = hex.EncodeToString(h[:])
	}

	// Step 3: Check sampled hashes against suppression entries (per-list)
	// Use ANY() with the sample hashes — this uses the md5_hash index efficiently
	matchByList := make(map[string]int64)
	if actualSampleSize > 0 {
		crossRows, crossErr := awh.db.QueryContext(crossCtx, `
			SELECT se.list_id, COUNT(DISTINCT se.md5_hash) AS matched
			FROM mailing_suppression_entries se
			WHERE se.md5_hash = ANY($1::text[])
			GROUP BY se.list_id
		`, fmt.Sprintf("{%s}", strings.Join(sampleHashes, ",")))

		if crossErr != nil {
			log.Printf("WARNING: suppression cross-ref query failed: %v", crossErr)
		} else {
			defer crossRows.Close()
			for crossRows.Next() {
				var listID string
				var matched int64
				if err := crossRows.Scan(&listID, &matched); err == nil {
					matchByList[listID] = matched
				}
			}
		}
	}

	// Step 4: Extrapolate sample matches to the full audience
	for i := range suppressionLists {
		sl := &suppressionLists[i]
		if sampleMatched, ok := matchByList[sl.ID]; ok && actualSampleSize > 0 {
			// Extrapolate: (sampleMatched / sampleSize) * totalRecipients
			matchRate := float64(sampleMatched) / float64(actualSampleSize)
			sl.MatchedCount = int64(math.Round(matchRate * float64(totalRecipients)))
			totalMatched += sl.MatchedCount
		}
	}

	crossRefDuration := time.Since(crossRefStart)
	log.Printf("Audience cross-reference complete in %s: ~%d of %d subscribers match suppression (from %d-sample extrapolation)",
		crossRefDuration.Round(time.Millisecond), totalMatched, totalRecipients, actualSampleSize)

	// Use the real cross-reference count for suppression
	suppressionEstimate := totalMatched
	if suppressionEstimate == 0 && totalRecipients > 0 {
		// If no matches found (e.g. suppression lists only have hashes we can't match),
		// fall back to a conservative 5% estimate
		suppressionEstimate = int64(math.Ceil(float64(totalRecipients) * 0.05))
	}

	netRecipients := totalRecipients - suppressionEstimate
	if netRecipients < 1 {
		netRecipients = 1
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"recommendation": map[string]interface{}{
			"selected_segments":      selectedSegments,
			"total_recipients":       totalRecipients,
			"net_recipients":         netRecipients,
			"engagement_quality":     engagementQuality,
			"avg_engagement_score":   math.Round(avgEngagement*100) / 100,
			"suppression_estimate":   suppressionEstimate,
			"suppression_checked":    true,
			"suppression_check_ms":   crossRefDuration.Milliseconds(),
			"mailing_lists":          mailingLists,
			"suppression_lists":      suppressionLists,
		},
	})
}

// ============================================================================
// Handler 4: HandleSendWindowOptimizer
// POST /agent-wizard/send-window
// ============================================================================

type sendWindowRequest struct {
	SendDays     []string `json:"send_days"`
	EndHour      int      `json:"end_hour"`
	Timezone     string   `json:"timezone"`
	AudienceSize int64    `json:"audience_size"`
}

// domainToISP maps common email domains to ISP display names.
var domainToISP = map[string]string{
	"gmail.com":    "Gmail",
	"yahoo.com":    "Yahoo",
	"outlook.com":  "Microsoft",
	"hotmail.com":  "Microsoft",
	"live.com":     "Microsoft",
	"msn.com":      "Microsoft",
	"aol.com":      "AOL",
	"att.net":      "AT&T",
	"comcast.net":  "Comcast",
	"icloud.com":   "Apple",
	"me.com":       "Apple",
	"mac.com":      "Apple",
	"ymail.com":    "Yahoo",
	"sbcglobal.net": "AT&T",
	"verizon.net":  "Verizon",
	"cox.net":      "Cox",
	"charter.net":  "Charter",
}

func classifyDomainToISP(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if isp, ok := domainToISP[domain]; ok {
		return isp
	}
	return "Other"
}

type ispScheduleEntry struct {
	ISP            string  `json:"isp"`
	Domain         string  `json:"domain"`
	RecipientCount int64   `json:"recipient_count"`
	OptimalHour    int     `json:"optimal_hour"`
	Percentage     float64 `json:"percentage"`
}

func (awh *AgentWizardHandlers) HandleSendWindowOptimizer(w http.ResponseWriter, r *http.Request) {
	var req sendWindowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.AudienceSize <= 0 {
		respondError(w, http.StatusBadRequest, "audience_size must be positive")
		return
	}

	if req.Timezone == "" {
		req.Timezone = "America/New_York"
	}
	if req.EndHour <= 0 {
		req.EndHour = 17
	}
	if len(req.SendDays) == 0 {
		req.SendDays = []string{"monday", "tuesday", "wednesday", "thursday", "friday"}
	}

	ctx := r.Context()

	type ispProfile struct {
		Domain  string
		Count   int64
		AvgHour float64
	}

	var ispProfiles []ispProfile

	if awh.db != nil {
		rows, err := awh.db.QueryContext(ctx,
			`SELECT domain, COUNT(*) as cnt, AVG(best_send_hour) as avg_hour
			 FROM mailing_inbox_profiles
			 GROUP BY domain
			 ORDER BY cnt DESC
			 LIMIT 10`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var p ispProfile
				var avgHour sql.NullFloat64
				if err := rows.Scan(&p.Domain, &p.Count, &avgHour); err != nil {
					continue
				}
				if avgHour.Valid {
					p.AvgHour = avgHour.Float64
				}
				ispProfiles = append(ispProfiles, p)
			}
		}
	}

	// If no data from DB, use default distribution
	if len(ispProfiles) == 0 {
		ispProfiles = []ispProfile{
			{Domain: "gmail.com", Count: 45, AvgHour: 10},
			{Domain: "yahoo.com", Count: 20, AvgHour: 11},
			{Domain: "hotmail.com", Count: 15, AvgHour: 9},
			{Domain: "aol.com", Count: 10, AvgHour: 14},
			{Domain: "icloud.com", Count: 10, AvgHour: 12},
		}
	}

	// Calculate total profile count for percentage distribution
	var totalProfileCount int64
	for _, p := range ispProfiles {
		totalProfileCount += p.Count
	}

	// Build ISP schedule
	var schedule []ispScheduleEntry
	for _, p := range ispProfiles {
		pct := float64(0)
		if totalProfileCount > 0 {
			pct = float64(p.Count) / float64(totalProfileCount) * 100
		}

		recipientCount := int64(math.Round(float64(req.AudienceSize) * pct / 100))

		// Calculate optimal hour: clamp to the send window [8, endHour]
		optimalHour := int(math.Round(p.AvgHour))
		if optimalHour < 8 {
			optimalHour = 8
		}
		if optimalHour > req.EndHour {
			optimalHour = req.EndHour
		}

		schedule = append(schedule, ispScheduleEntry{
			ISP:            classifyDomainToISP(p.Domain),
			Domain:         p.Domain,
			RecipientCount: recipientCount,
			OptimalHour:    optimalHour,
			Percentage:     math.Round(pct*100) / 100,
		})
	}

	// Estimate daily capacity and days to complete
	// Assume we can send to roughly audience_size / len(sendDays) per day
	totalDailyCapacity := req.AudienceSize
	if len(req.SendDays) > 0 {
		totalDailyCapacity = req.AudienceSize / int64(len(req.SendDays))
	}
	estimatedDaysToComplete := 1
	if totalDailyCapacity > 0 {
		estimatedDaysToComplete = int(math.Ceil(float64(req.AudienceSize) / float64(totalDailyCapacity)))
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"window": map[string]interface{}{
			"send_days": req.SendDays,
			"end_hour":  req.EndHour,
			"timezone":  req.Timezone,
		},
		"isp_schedule":             schedule,
		"total_daily_capacity":     totalDailyCapacity,
		"estimated_days_to_complete": estimatedDaysToComplete,
	})
}

// ============================================================================
// Handler 5: HandleDeployAgents
// POST /agent-wizard/deploy
// ============================================================================

type deployAgentsRequest struct {
	CampaignID  uuid.UUID `json:"campaign_id"`
	OfferID     string    `json:"offer_id"`
	OfferName   string    `json:"offer_name"`
	ISPSchedule []struct {
		ISP            string `json:"isp"`
		Domain         string `json:"domain"`
		RecipientCount int64  `json:"recipient_count"`
		OptimalHour    int    `json:"optimal_hour"`
	} `json:"isp_schedule"`
	SendWindow struct {
		Days     []string `json:"days"`
		EndHour  int      `json:"end_hour"`
		Timezone string   `json:"timezone"`
	} `json:"send_window"`
}

type deployedAgent struct {
	AgentID        uuid.UUID `json:"agent_id"`
	ISP            string    `json:"isp"`
	Domain         string    `json:"domain"`
	Status         string    `json:"status"`
	RecipientCount int64     `json:"recipient_count"`
	IsNew          bool      `json:"is_new"`
}

func (awh *AgentWizardHandlers) HandleDeployAgents(w http.ResponseWriter, r *http.Request) {
	var req deployAgentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.CampaignID == uuid.Nil {
		respondError(w, http.StatusBadRequest, "campaign_id is required")
		return
	}
	if len(req.ISPSchedule) == 0 {
		respondError(w, http.StatusBadRequest, "isp_schedule must not be empty")
		return
	}

	if awh.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not available")
		return
	}

	ctx := r.Context()
	orgID := uuid.Nil // Placeholder when org context not available

	// Use a transaction for all inserts
	tx, err := awh.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("ERROR: failed to start transaction: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to deploy agents")
		return
	}
	defer tx.Rollback()

	var deployed []deployedAgent

	for _, entry := range req.ISPSchedule {
		domain := strings.ToLower(strings.TrimSpace(entry.Domain))
		isp := entry.ISP
		if isp == "" {
			isp = classifyDomainToISP(domain)
		}

		var agentID uuid.UUID
		var isNew bool
		var status string

		// Check if agent already exists for this domain + organization
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM mailing_isp_agents WHERE domain = $1 AND organization_id = $2`,
			domain, orgID,
		).Scan(&agentID)

		if err == sql.ErrNoRows {
			// Create new agent
			isNew = true
			status = "spawned"
			agentID = uuid.New()

			_, insertErr := tx.ExecContext(ctx,
				`INSERT INTO mailing_isp_agents (id, organization_id, isp, domain, status, total_campaigns, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5, 1, NOW(), NOW())`,
				agentID, orgID, isp, domain, status,
			)
			if insertErr != nil {
				log.Printf("ERROR: failed to create agent for domain %s: %v", domain, insertErr)
			respondError(w, http.StatusInternalServerError, "Failed to create agent")
				return
			}
		} else if err != nil {
			log.Printf("ERROR: failed to check agent for domain %s: %v", domain, err)
			respondError(w, http.StatusInternalServerError, "Failed to verify agent")
			return
		} else {
			// Update existing agent
			isNew = false
			status = "learning"

			_, updateErr := tx.ExecContext(ctx,
				`UPDATE mailing_isp_agents SET status = $1, updated_at = NOW(), total_campaigns = total_campaigns + 1
				 WHERE id = $2`,
				status, agentID,
			)
			if updateErr != nil {
				respondError(w, http.StatusInternalServerError, "failed to update agent: "+updateErr.Error())
				return
			}
		}

		// Build send window JSON for the agent campaign link
		sendWindowJSON, _ := json.Marshal(map[string]interface{}{
			"days":         req.SendWindow.Days,
			"end_hour":     req.SendWindow.EndHour,
			"timezone":     req.SendWindow.Timezone,
			"optimal_hour": entry.OptimalHour,
		})

		// Insert agent-campaign link
		_, linkErr := tx.ExecContext(ctx,
			`INSERT INTO mailing_agent_campaigns (id, agent_id, campaign_id, recipient_count, status, send_window, created_at)
			 VALUES ($1, $2, $3, $4, 'pending', $5, NOW())`,
			uuid.New(), agentID, req.CampaignID, entry.RecipientCount, string(sendWindowJSON),
		)
		if linkErr != nil {
			log.Printf("ERROR: failed to link agent to campaign: %v", linkErr)
			respondError(w, http.StatusInternalServerError, "Failed to link agent to campaign")
			return
		}

		deployed = append(deployed, deployedAgent{
			AgentID:        agentID,
			ISP:            isp,
			Domain:         domain,
			Status:         status,
			RecipientCount: entry.RecipientCount,
			IsNew:          isNew,
		})
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ERROR: failed to commit transaction: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to finalize agent deployment")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deployed_agents": deployed,
		"total_agents":    len(deployed),
		"campaign_id":     req.CampaignID,
	})
}

// ============================================================================
// RegisterAgentWizardRoutes registers all agent wizard routes on the router.
// ============================================================================

func RegisterAgentWizardRoutes(r chi.Router, db *sql.DB, h *Handlers) {
	awh := &AgentWizardHandlers{
		db:     db,
		parent: h,
	}

	r.Get("/agent-wizard/analyze-offer", awh.HandleAnalyzeOffer)
	r.Post("/agent-wizard/revenue-projection", awh.HandleRevenueProjection)
	r.Post("/agent-wizard/audience-selection", awh.HandleAIAudienceSelection)
	r.Post("/agent-wizard/send-window", awh.HandleSendWindowOptimizer)
	r.Post("/agent-wizard/deploy", awh.HandleDeployAgents)
}

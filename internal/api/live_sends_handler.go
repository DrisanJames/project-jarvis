package api

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// ─── API Version Constants ────────────────────────────────────────────────────

const (
	VersionActiveSends   = "1.0"
	VersionBounceReasons = "1.0"
)

// ─── Active Sends ────────────────────────────────────────────────────────────

// HandleActiveSends returns a live view of currently sending / preparing
// campaigns, including wave counts by status, rolling 1m and 5m throughput,
// and per-ISP-plan breakdown. Intended to be polled by the Analytics Center
// live band every ~20 seconds.
//
// GET /api/mailing/analytics/active-sends
func (s *AdvancedMailingService) HandleActiveSends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type waveCounts struct {
		Total     int `json:"total"`
		Planned   int `json:"planned"`
		Enqueued  int `json:"enqueued"`
		Running   int `json:"running"`
		Completed int `json:"completed"`
		Cancelled int `json:"cancelled"`
		Failed    int `json:"failed"`
	}

	type campaignTotals struct {
		Recipients   int `json:"recipients"`
		Sent         int `json:"sent"`
		Delivered    int `json:"delivered"`
		Deferred     int `json:"deferred"`
		HardBounces  int `json:"hard_bounces"`
		SoftBounces  int `json:"soft_bounces"`
		Opens        int `json:"opens"`
		Clicks       int `json:"clicks"`
	}

	type ispPlanView struct {
		ISP          string         `json:"isp"`
		Label        string         `json:"label"`
		Status       string         `json:"status"`
		SendingDomain string        `json:"sending_domain"`
		Waves        waveCounts     `json:"waves"`
		Totals       campaignTotals `json:"totals"`
		SentLast1m   int            `json:"sent_last_1m"`
		SentLast5m   int            `json:"sent_last_5m"`
	}

	type campaignView struct {
		ID          string         `json:"id"`
		Name        string         `json:"name"`
		Status      string         `json:"status"`
		StartedAt   *time.Time     `json:"started_at,omitempty"`
		ScheduledAt *time.Time     `json:"scheduled_at,omitempty"`
		FromEmail   string         `json:"from_email"`
		SentLast1m  int            `json:"sent_last_1m"`
		SentLast5m  int            `json:"sent_last_5m"`
		Totals      campaignTotals `json:"totals"`
		Waves       waveCounts     `json:"waves"`
		NextWaveAt  *time.Time     `json:"next_wave_at,omitempty"`
		ISPPlans    []ispPlanView  `json:"isp_plans"`
	}

	// Step 1: find active campaigns.
	cRows, err := s.db.QueryContext(ctx, `
		SELECT id::text, name, status, started_at, scheduled_at,
		       COALESCE(from_email, ''),
		       COALESCE(sent_count, 0), COALESCE(delivered_count, 0),
		       COALESCE(hard_bounce_count, 0), COALESCE(soft_bounce_count, 0),
		       COALESCE(open_count, 0), COALESCE(click_count, 0)
		FROM mailing_campaigns
		WHERE status IN ('sending', 'preparing', 'finalizing_audience', 'scheduled')
		  AND COALESCE(execution_mode, '') = 'pmta_isp_wave'
		ORDER BY COALESCE(started_at, scheduled_at, created_at) DESC
		LIMIT 20
	`)
	if err != nil {
		log.Printf("[active-sends] campaigns query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer cRows.Close()

	type campaignRow struct {
		ID   string
		View campaignView
	}
	var campaigns []campaignRow
	campaignIDs := []string{}
	for cRows.Next() {
		var cr campaignRow
		var startedAt, scheduledAt sql.NullTime
		if err := cRows.Scan(
			&cr.ID, &cr.View.Name, &cr.View.Status,
			&startedAt, &scheduledAt, &cr.View.FromEmail,
			&cr.View.Totals.Sent, &cr.View.Totals.Delivered,
			&cr.View.Totals.HardBounces, &cr.View.Totals.SoftBounces,
			&cr.View.Totals.Opens, &cr.View.Totals.Clicks,
		); err != nil {
			continue
		}
		cr.View.ID = cr.ID
		if startedAt.Valid {
			t := startedAt.Time
			cr.View.StartedAt = &t
		}
		if scheduledAt.Valid {
			t := scheduledAt.Time
			cr.View.ScheduledAt = &t
		}
		campaigns = append(campaigns, cr)
		campaignIDs = append(campaignIDs, cr.ID)
	}
	cRows.Close()

	if len(campaigns) == 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"api_version": VersionActiveSends,
			"is_active":   false,
			"as_of":       time.Now().UTC().Format(time.RFC3339),
			"campaigns":   []interface{}{},
		})
		return
	}

	// Step 2: wave counts per campaign (aggregate across ISP plans).
	campaignWaves := make(map[string]*waveCounts)
	planWaves := make(map[string]*waveCounts)   // keyed by isp_plan_id
	campaignNextWave := make(map[string]time.Time)
	if wRows, err := s.db.QueryContext(ctx, `
		SELECT campaign_id::text, isp_plan_id::text, status, COUNT(*), MIN(CASE WHEN status = 'planned' THEN scheduled_at END)
		FROM mailing_campaign_waves
		WHERE campaign_id::text = ANY($1)
		GROUP BY campaign_id, isp_plan_id, status
	`, pqTextArray(campaignIDs)); err == nil {
		defer wRows.Close()
		for wRows.Next() {
			var cID, planID, status string
			var count int
			var nextPlanned sql.NullTime
			if err := wRows.Scan(&cID, &planID, &status, &count, &nextPlanned); err != nil {
				continue
			}
			cw := campaignWaves[cID]
			if cw == nil {
				cw = &waveCounts{}
				campaignWaves[cID] = cw
			}
			pw := planWaves[planID]
			if pw == nil {
				pw = &waveCounts{}
				planWaves[planID] = pw
			}
			cw.Total += count
			pw.Total += count
			switch status {
			case "planned":
				cw.Planned += count
				pw.Planned += count
		case "due", "dispatched", "enqueuing", "queued":
			cw.Enqueued += count
			pw.Enqueued += count
		case "sending", "running":
			cw.Running += count
			pw.Running += count
			case "completed":
				cw.Completed += count
				pw.Completed += count
			case "cancelled":
				cw.Cancelled += count
				pw.Cancelled += count
			case "failed", "dead_letter":
				cw.Failed += count
				pw.Failed += count
			}
			if nextPlanned.Valid {
				existing, has := campaignNextWave[cID]
				if !has || nextPlanned.Time.Before(existing) {
					campaignNextWave[cID] = nextPlanned.Time
				}
			}
		}
	}

	// Step 3: ISP plans per campaign (status, isp, sending domain, counts).
	type planMeta struct {
		ID             string
		CampaignID     string
		ISP            string
		Status         string
		SendingDomain  string
		SentCount      int
		AudienceCount  int
	}
	planMap := make(map[string]*planMeta) // isp_plan_id -> meta
	if pRows, err := s.db.QueryContext(ctx, `
		SELECT id::text, campaign_id::text, isp, status,
		       COALESCE(sending_domain, ''),
		       COALESCE(sent_count, 0), COALESCE(audience_selected_count, 0)
		FROM mailing_campaign_isp_plans
		WHERE campaign_id::text = ANY($1)
	`, pqTextArray(campaignIDs)); err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var p planMeta
			if err := pRows.Scan(
				&p.ID, &p.CampaignID, &p.ISP, &p.Status,
				&p.SendingDomain, &p.SentCount, &p.AudienceCount,
			); err != nil {
				continue
			}
			planMap[p.ID] = &p
		}
	}

	// Step 4: rolling throughput from mailing_tracking_events (last 5 minutes).
	// Scope by campaign_id — the table is large so an IN clause with a short
	// time window is the right shape.
	now := time.Now()
	fiveMinAgo := now.Add(-5 * time.Minute)
	oneMinAgo := now.Add(-1 * time.Minute)
	sentLast1m := make(map[string]int)        // campaign_id -> count
	sentLast5m := make(map[string]int)        // campaign_id -> count
	planSentLast1m := make(map[string]int)    // isp_plan_id -> count
	planSentLast5m := make(map[string]int)    // isp_plan_id -> count
	if tRows, err := s.db.QueryContext(ctx, `
		SELECT t.campaign_id::text,
		       LOWER(COALESCE(NULLIF(t.recipient_domain,''), SPLIT_PART(s.email,'@',2), '')) AS domain,
		       COUNT(*) FILTER (WHERE t.event_at >= $2) AS c1,
		       COUNT(*) FILTER (WHERE t.event_at >= $3) AS c5
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE t.campaign_id::text = ANY($1)
		  AND t.event_type = 'sent'
		  AND t.event_at >= $3
		GROUP BY t.campaign_id, domain
	`, pqTextArray(campaignIDs), oneMinAgo, fiveMinAgo); err == nil {
		defer tRows.Close()
		// Build campaign -> isp -> plan_id reverse map so we can attribute the
		// rolling throughput to a specific ISP plan.
		campaignISPToPlan := make(map[string]map[string]string)
		for pid, pm := range planMap {
			m := campaignISPToPlan[pm.CampaignID]
			if m == nil {
				m = make(map[string]string)
				campaignISPToPlan[pm.CampaignID] = m
			}
			m[pm.ISP] = pid
		}
		for tRows.Next() {
			var cID, domain string
			var c1, c5 int
			if err := tRows.Scan(&cID, &domain, &c1, &c5); err != nil {
				continue
			}
			sentLast1m[cID] += c1
			sentLast5m[cID] += c5
			group := isppkg.GroupFromDomain(domain)
			if m, ok := campaignISPToPlan[cID]; ok {
				if planID, ok := m[group]; ok {
					planSentLast1m[planID] += c1
					planSentLast5m[planID] += c5
				}
			}
		}
	}

	// Step 5: assemble response.
	out := make([]campaignView, 0, len(campaigns))
	for _, cr := range campaigns {
		cv := cr.View
		if cw, ok := campaignWaves[cr.ID]; ok {
			cv.Waves = *cw
		}
		if nw, ok := campaignNextWave[cr.ID]; ok {
			t := nw
			cv.NextWaveAt = &t
		}
		cv.SentLast1m = sentLast1m[cr.ID]
		cv.SentLast5m = sentLast5m[cr.ID]
		// Gather totals.Recipients from plan audience, if available.
		for _, p := range planMap {
			if p.CampaignID != cr.ID {
				continue
			}
			cv.Totals.Recipients += p.AudienceCount
		}

		// Build ISP plan views for this campaign.
		var plans []ispPlanView
		for _, p := range planMap {
			if p.CampaignID != cr.ID {
				continue
			}
			ipv := ispPlanView{
				ISP:           p.ISP,
				Label:         metricsISPDisplayName(p.ISP),
				Status:        p.Status,
				SendingDomain: p.SendingDomain,
				Totals: campaignTotals{
					Recipients: p.AudienceCount,
					Sent:       p.SentCount,
				},
				SentLast1m: planSentLast1m[p.ID],
				SentLast5m: planSentLast5m[p.ID],
			}
			if pw, ok := planWaves[p.ID]; ok {
				ipv.Waves = *pw
			}
			plans = append(plans, ipv)
		}
		sort.Slice(plans, func(i, j int) bool {
			return plans[i].ISP < plans[j].ISP
		})
		cv.ISPPlans = plans
		out = append(out, cv)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version": VersionActiveSends,
		"is_active":   len(out) > 0,
		"as_of":       time.Now().UTC().Format(time.RFC3339),
		"campaigns":   out,
	})
}

// ─── Bounce Reasons ──────────────────────────────────────────────────────────

// bounceCodeLabel maps an SMTP code prefix (first 5 characters like "5.7.1")
// to a short human-readable label and a category bucket. The list covers the
// most common codes observed across Gmail, Yahoo, Microsoft, Apple, and the
// cable ISPs. Unknown codes fall back to the raw SMTP code and "other".
type bounceCodeEntry struct {
	Label    string
	Category string
}

var bounceCodeDict = map[string]bounceCodeEntry{
	"5.7.1":  {"Authentication / delivery not authorized", "auth"},
	"5.7.0":  {"Authentication / security policy", "auth"},
	"5.7.9":  {"Authentication rejected by recipient", "auth"},
	"5.7.26": {"DMARC / SPF alignment failure", "auth"},
	"5.7.27": {"Missing sender authentication", "auth"},
	"5.2.2":  {"Mailbox full", "mailbox"},
	"5.1.1":  {"Bad destination address (user unknown)", "mailbox"},
	"5.1.10": {"Recipient address rejected (unknown)", "mailbox"},
	"5.1.0":  {"Address rejected", "mailbox"},
	"5.4.4":  {"Unable to route / DNS failure", "system"},
	"5.3.0":  {"System rejected message", "system"},
	"5.3.2":  {"System not accepting network messages", "system"},
	"5.5.2":  {"Syntax / protocol error", "system"},
	"5.7.606": {"Blocked by recipient policy", "policy"},
	"5.7.708": {"Blocked sender reputation", "reputation"},
	"4.4.2":  {"Timeout waiting for data from client", "transient"},
	"4.7.0":  {"Temporary throttling", "transient"},
	"4.2.1":  {"Recipient deferred / greylisted", "transient"},
	"4.3.0":  {"Temporary system failure", "transient"},
}

func classifyBounceCode(code string) bounceCodeEntry {
	// Normalise: take first 5 chars like "5.7.1" or "4.4.2".
	trimmed := strings.TrimSpace(code)
	if entry, ok := bounceCodeDict[trimmed]; ok {
		return entry
	}
	// Fall back to category based on the first digit.
	switch {
	case strings.HasPrefix(trimmed, "5.7"):
		return bounceCodeEntry{trimmed, "auth"}
	case strings.HasPrefix(trimmed, "5.1"), strings.HasPrefix(trimmed, "5.2"):
		return bounceCodeEntry{trimmed, "mailbox"}
	case strings.HasPrefix(trimmed, "5.3"), strings.HasPrefix(trimmed, "5.4"), strings.HasPrefix(trimmed, "5.5"):
		return bounceCodeEntry{trimmed, "system"}
	case strings.HasPrefix(trimmed, "4."):
		return bounceCodeEntry{trimmed, "transient"}
	}
	if trimmed == "" {
		return bounceCodeEntry{"Unclassified", "other"}
	}
	return bounceCodeEntry{trimmed, "other"}
}

// HandleBounceReasons returns top SMTP bounce reasons within the requested
// date range, grouped by the leading portion of the SMTP code (e.g. "5.7.1",
// "5.2.2") with optional per-ISP breakdown.
//
// GET /api/mailing/analytics/bounce-reasons
func (s *AdvancedMailingService) HandleBounceReasons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	type isprow struct {
		ISP   string `json:"isp"`
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	type reasonRow struct {
		Code       string   `json:"code"`
		Label      string   `json:"label"`
		Category   string   `json:"category"`
		Count      int      `json:"count"`
		PctOfTotal float64  `json:"pct_of_total"`
		ByISP      []isprow `json:"by_isp"`
	}

	// One pass: group by normalized SMTP code prefix + recipient domain, then
	// collapse domains into ISP groups in Go. Codes are extracted from the
	// free-text bounce_reason column on mailing_tracking_events, which is the
	// canonical location every ingester writes to.
	rows, err := s.db.QueryContext(ctx, buildBounceReasonsSQL(), start, end)
	if err != nil {
		log.Printf("[bounce-reasons] query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	defer rows.Close()

	type reasonKey struct{ Code string }
	reasonAgg := make(map[string]*reasonRow)      // code -> aggregate
	reasonISP := make(map[string]map[string]int)  // code -> isp -> count
	total := 0
	for rows.Next() {
		var code, domain string
		var count int
		if err := rows.Scan(&code, &domain, &count); err != nil {
			continue
		}
		entry := classifyBounceCode(code)
		rr, ok := reasonAgg[entry.Label+"::"+entry.Category]
		if !ok {
			rr = &reasonRow{
				Code:     code,
				Label:    entry.Label,
				Category: entry.Category,
			}
			reasonAgg[entry.Label+"::"+entry.Category] = rr
			reasonISP[entry.Label+"::"+entry.Category] = map[string]int{}
		}
		rr.Count += count
		group := isppkg.GroupFromDomain(domain)
		reasonISP[entry.Label+"::"+entry.Category][group] += count
		total += count
	}

	// Materialise final list sorted by count desc, top 20.
	reasons := make([]reasonRow, 0, len(reasonAgg))
	for key, rr := range reasonAgg {
		for ispKey, cnt := range reasonISP[key] {
			rr.ByISP = append(rr.ByISP, isprow{
				ISP:   ispKey,
				Label: metricsISPDisplayName(ispKey),
				Count: cnt,
			})
		}
		sort.Slice(rr.ByISP, func(i, j int) bool {
			return rr.ByISP[i].Count > rr.ByISP[j].Count
		})
		if total > 0 {
			rr.PctOfTotal = metricsRound2(float64(rr.Count) / float64(total) * 100)
		}
		reasons = append(reasons, *rr)
	}
	sort.Slice(reasons, func(i, j int) bool {
		return reasons[i].Count > reasons[j].Count
	})
	if len(reasons) > 20 {
		reasons = reasons[:20]
	}
	if reasons == nil {
		reasons = []reasonRow{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":    VersionBounceReasons,
		"range":          map[string]string{"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339)},
		"total_bounces":  total,
		"reasons":        reasons,
	})
}

// buildBounceReasonsSQL returns the SQL that extracts normalised SMTP codes
// (like "5.7.1" or "4.4.2") from the free-text bounce_reason column on
// mailing_tracking_events. Every ingester — PMTA accounting, SparkPost,
// SES, Mailgun webhooks — writes the SMTP response body to bounce_reason,
// so a single regex extract is sufficient. Domain prefers the recipient_domain
// column (populated at ingest time) and falls back to the subscriber join
// to match the behaviour of other ISP-aware analytics handlers.
func buildBounceReasonsSQL() string {
	return `
		SELECT
			COALESCE(NULLIF(SUBSTRING(COALESCE(t.bounce_reason, '') FROM '[45]\.\d+\.\d+'), ''), '') AS code_norm,
			LOWER(COALESCE(NULLIF(t.recipient_domain, ''), SPLIT_PART(s.email, '@', 2), '')) AS domain,
			COUNT(*) AS cnt
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE t.event_type = 'bounced'
		  AND t.event_at >= $1 AND t.event_at <= $2
		GROUP BY code_norm, domain
	`
}

// pqTextArray wraps a []string for use with ANY($1) against a text[] column
// using the lib/pq driver's Array() helper.
func pqTextArray(xs []string) interface{} {
	return pq.Array(xs)
}

// compile-time sanity guard: ensure context is still imported even if all
// uses are removed during refactors.
var _ = context.Background

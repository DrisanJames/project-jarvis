// jarvis_learning.go — Learning persistence, attribution, and post-campaign intelligence.
package api

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// ── Learning Persistence Layer ──────────────────────────────────────────────
// These methods write campaign outcomes back to the database so the ISP
// agents, inbox profiles, and AI decision log accumulate intelligence
// across campaigns. This is the feedback loop.

// persistAllLearnings orchestrates all post-campaign learning persistence.
// Runs in a goroutine after the final report is logged.
func (j *JarvisOrchestrator) persistAllLearnings(campaign *JarvisCampaign) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log.Printf("[Jarvis/Learning] Starting post-campaign learning persistence for campaign %s", campaign.ID[:8])

	// 1. Persist the full final campaign report (not just the launch snapshot)
	j.persistFinalCampaignReport(ctx, campaign)

	// 2. Write per-recipient outcomes to mailing_inbox_profiles
	j.persistRecipientOutcomes(ctx, campaign)

	// 3. Update ISP agent aggregate stats
	j.updateISPAgentStats(ctx, campaign)

	// 4. Persist Yahoo eDataSource intelligence to ISP agent knowledge
	j.persistYahooIntelligence(ctx, campaign)

	log.Printf("[Jarvis/Learning] Post-campaign learning persistence complete for campaign %s", campaign.ID[:8])
}

// persistFinalCampaignReport saves the completed campaign with all metrics
// to mailing_ai_decisions so it's queryable for future campaign planning.
func (j *JarvisOrchestrator) persistFinalCampaignReport(ctx context.Context, campaign *JarvisCampaign) {
	// Build a structured report (not just a JSON dump of the campaign)
	report := map[string]interface{}{
		"campaign_id":  campaign.ID,
		"offer_id":     campaign.OfferID,
		"offer_name":   campaign.OfferName,
		"status":       campaign.Status,
		"started_at":   campaign.StartedAt,
		"ended_at":     time.Now(),
		"rounds":       campaign.CurrentRound,
		"metrics":      campaign.Metrics,
		"recipient_outcomes": buildRecipientOutcomeSummary(campaign),
		"isp_performance":    campaign.Metrics.ISPMetrics,
		"creative_performance": buildCreativePerformanceSummary(campaign),
		"learnings": buildCampaignLearnings(campaign),
	}

	reportJSON, err := json.Marshal(report)
	if err != nil {
		log.Printf("[Jarvis/Learning] ERROR: failed to marshal final report: %v", err)
		return
	}

	_, err = j.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions 
		(id, campaign_id, organization_id, decision_type, decision_reason, metrics_snapshot, ai_model, confidence, applied, created_at)
		VALUES ($1, $2, $5, 'jarvis_final_report', $3, $4, 'jarvis-orchestrator', 1.0, true, NOW())
	`, uuid.New(), campaign.ID, "Campaign completed — final report persisted for learning", reportJSON, campaign.OrganizationID)

	if err != nil {
		log.Printf("[Jarvis/Learning] ERROR: failed to persist final report: %v", err)
		return
	}

	log.Printf("[Jarvis/Learning] Final campaign report persisted to mailing_ai_decisions")
}

// persistRecipientOutcomes writes per-recipient engagement results back to
// mailing_inbox_profiles using the update_inbox_profile database function.
// This feeds the engagement scoring, send time optimization, and inbox health system.
func (j *JarvisOrchestrator) persistRecipientOutcomes(ctx context.Context, campaign *JarvisCampaign) {
	updated := 0
	for _, r := range campaign.Recipients {
		if r.Suppressed {
			continue
		}

		emailHash := fmt.Sprintf("%x", md5.Sum([]byte(strings.ToLower(strings.TrimSpace(r.Email)))))
		domain := r.Domain
		eventHour := time.Now().Hour()
		eventDay := int(time.Now().Weekday())

		// Record the send event
		if r.SendCount > 0 {
			_, err := j.db.ExecContext(ctx,
				`SELECT update_inbox_profile($1, $2, 'sent', $3, $4)`,
				emailHash, domain, eventHour, eventDay,
			)
			if err != nil {
				log.Printf("[Jarvis/Learning] WARNING: failed to record send for %s: %v", r.Email, err)
			}
		}

		// Record the engagement outcome
		switch r.Status {
		case "opened":
			_, err := j.db.ExecContext(ctx,
				`SELECT update_inbox_profile($1, $2, 'open', $3, $4)`,
				emailHash, domain, eventHour, eventDay,
			)
			if err != nil {
				log.Printf("[Jarvis/Learning] WARNING: failed to record open for %s: %v", r.Email, err)
			}
			updated++

		case "clicked", "converted":
			// Record both open and click (click implies open)
			j.db.ExecContext(ctx, `SELECT update_inbox_profile($1, $2, 'open', $3, $4)`, emailHash, domain, eventHour, eventDay)
			_, err := j.db.ExecContext(ctx,
				`SELECT update_inbox_profile($1, $2, 'click', $3, $4)`,
				emailHash, domain, eventHour, eventDay,
			)
			if err != nil {
				log.Printf("[Jarvis/Learning] WARNING: failed to record click for %s: %v", r.Email, err)
			}
			updated++

		case "bounced":
			_, err := j.db.ExecContext(ctx,
				`SELECT update_inbox_profile($1, $2, 'bounce', $3, $4)`,
				emailHash, domain, eventHour, eventDay,
			)
			if err != nil {
				log.Printf("[Jarvis/Learning] WARNING: failed to record bounce for %s: %v", r.Email, err)
			}
			updated++

		case "spam_suspected":
			// Record as a bounce with inbox_health degraded — the system will
			// lower engagement score and potentially suspend future sends
			j.db.ExecContext(ctx, `SELECT update_inbox_profile($1, $2, 'bounce', $3, $4)`, emailHash, domain, eventHour, eventDay)
			// Also mark inbox health as degraded and set ISP
			j.db.ExecContext(ctx, `
				UPDATE mailing_inbox_profiles 
				SET inbox_health = 'degraded', 
				    isp = $2,
				    suspension_reason = 'spam_suspected_by_jarvis',
				    send_suspended_until = NOW() + INTERVAL '48 hours'
				WHERE email_hash = $1
			`, emailHash, strings.ToLower(r.ISP))
			updated++
		}

		// Ensure ISP is set on the profile
		if r.ISP != "" {
			j.db.ExecContext(ctx, `
				UPDATE mailing_inbox_profiles SET isp = $2 WHERE email_hash = $1 AND (isp IS NULL OR isp = '')
			`, emailHash, strings.ToLower(r.ISP))
		}
	}

	log.Printf("[Jarvis/Learning] Updated %d recipient outcomes in mailing_inbox_profiles", updated)
}

// updateISPAgentStats feeds aggregate send/open/click/bounce counts from this
// campaign into the corresponding ISP agents in mailing_isp_agents.
// This increments their lifetime counters and updates their knowledge.
func (j *JarvisOrchestrator) updateISPAgentStats(ctx context.Context, campaign *JarvisCampaign) {
	orgID := campaign.OrganizationID
	sendingDomain := campaign.SendingDomain
	updated := 0

	for ispName, im := range campaign.Metrics.ISPMetrics {
		if im.Sent == 0 {
			continue
		}

		// Upsert the ISP agent — create if not exists, update if exists
		_, err := j.db.ExecContext(ctx, `
			INSERT INTO mailing_isp_agents (organization_id, isp, domain, status, total_campaigns, total_sends, total_opens, total_clicks, total_bounces, last_active_at)
			VALUES ($1, $2, $3, 'sending', 1, $4, $5, $6, $7, NOW())
			ON CONFLICT (organization_id, domain) DO UPDATE SET
				total_campaigns = mailing_isp_agents.total_campaigns + 1,
				total_sends = mailing_isp_agents.total_sends + EXCLUDED.total_sends,
				total_opens = mailing_isp_agents.total_opens + EXCLUDED.total_opens,
				total_clicks = mailing_isp_agents.total_clicks + EXCLUDED.total_clicks,
				total_bounces = mailing_isp_agents.total_bounces + EXCLUDED.total_bounces,
				status = CASE WHEN mailing_isp_agents.status = 'dormant' THEN 'sending' ELSE mailing_isp_agents.status END,
				last_active_at = NOW(),
				updated_at = NOW()
		`, orgID, ispName, sendingDomain,
			im.Sent, im.Opens, im.Clicks, im.Bounced,
		)
		if err != nil {
			log.Printf("[Jarvis/Learning] WARNING: failed to update ISP agent for %s: %v", ispName, err)
			continue
		}

		// Update avg_engagement based on new cumulative stats
		j.db.ExecContext(ctx, `
			UPDATE mailing_isp_agents SET
				avg_engagement = CASE WHEN total_sends > 0 
					THEN (total_opens::float / total_sends::float) * 100.0 
					ELSE 0 END
			WHERE organization_id = $1 AND domain = $2
		`, orgID, sendingDomain)

		updated++
		log.Printf("[Jarvis/Learning] Updated ISP agent [%s/%s]: +%d sends, +%d opens, +%d clicks, +%d bounces",
			ispName, sendingDomain, im.Sent, im.Opens, im.Clicks, im.Bounced)
	}

	log.Printf("[Jarvis/Learning] Updated %d ISP agent(s) in mailing_isp_agents", updated)
}

// persistYahooIntelligence writes the eDataSource inbox placement data and
// Yahoo agent signals into the Yahoo ISP agent's `knowledge` JSONB column.
// This creates a persistent memory that survives server restarts and informs
// future campaigns.
func (j *JarvisOrchestrator) persistYahooIntelligence(ctx context.Context, campaign *JarvisCampaign) {
	yahooIM := campaign.Metrics.ISPMetrics["Yahoo"]
	if yahooIM == nil || yahooIM.LastInboxCheck == nil {
		log.Printf("[Jarvis/Learning] No Yahoo eDataSource data to persist — skipping")
		return
	}

	orgID := campaign.OrganizationID
	sendingDomain := campaign.SendingDomain

	// Build knowledge payload with eDataSource intelligence
	// Count spam-suspected Yahoo recipients
	spamSuspectedCount := 0
	totalYahooRecipients := 0
	for _, r := range campaign.Recipients {
		if r.ISP != "Yahoo" || r.Suppressed {
			continue
		}
		totalYahooRecipients++
		if r.SpamSuspected {
			spamSuspectedCount++
		}
	}

	// Get Yahoo agent state if available
	var agentState map[string]interface{}
	if j.yahooAgent != nil {
		agentState = map[string]interface{}{
			"phase":              j.yahooAgent.CurrentState.Phase,
			"is_paused":         j.yahooAgent.CurrentState.IsPaused,
			"pause_reason":      j.yahooAgent.CurrentState.PauseReason,
			"current_open_rate": j.yahooAgent.CurrentState.CurrentOpenRate,
			"complaint_rate":    j.yahooAgent.CurrentState.ComplaintRate,
			"inbox_rate":        j.yahooAgent.CurrentState.InboxRate,
			"throttle_rate":     j.yahooAgent.CurrentState.ThrottleRate,
		}
	}

	knowledge := map[string]interface{}{
		"last_campaign_id":   campaign.ID,
		"last_campaign_at":   time.Now(),
		"edatasource": map[string]interface{}{
			"inbox_rate":      yahooIM.InboxRate,
			"spam_rate":       yahooIM.SpamRate,
			"spam_detected":   yahooIM.SpamDetected,
			"last_checked_at": yahooIM.LastInboxCheck,
		},
		"campaign_outcomes": map[string]interface{}{
			"total_yahoo_recipients": totalYahooRecipients,
			"spam_suspected_count":   spamSuspectedCount,
			"sent":                   yahooIM.Sent,
			"delivered":              yahooIM.Delivered,
			"opens":                  yahooIM.Opens,
			"clicks":                 yahooIM.Clicks,
			"bounced":                yahooIM.Bounced,
		},
		"yahoo_agent_state": agentState,
		"risk_assessment": map[string]interface{}{
			"inbox_rate_below_70": yahooIM.InboxRate > 0 && yahooIM.InboxRate < 70,
			"inbox_rate_below_50": yahooIM.InboxRate > 0 && yahooIM.InboxRate < 50,
			"spam_suspected_pct": func() float64 {
				if totalYahooRecipients > 0 {
					return float64(spamSuspectedCount) / float64(totalYahooRecipients) * 100
				}
				return 0
			}(),
		},
		"recommendations": buildYahooRecommendations(yahooIM, spamSuspectedCount, totalYahooRecipients),
	}

	knowledgeJSON, err := json.Marshal(knowledge)
	if err != nil {
		log.Printf("[Jarvis/Learning] ERROR: failed to marshal Yahoo knowledge: %v", err)
		return
	}

	// Update the Yahoo ISP agent's knowledge (merge with existing)
	_, err = j.db.ExecContext(ctx, `
		UPDATE mailing_isp_agents SET
			knowledge = COALESCE(knowledge, '{}'::jsonb) || $3::jsonb,
			updated_at = NOW(),
			last_active_at = NOW()
		WHERE organization_id = $1 AND isp = 'Yahoo' AND domain = $2
	`, orgID, sendingDomain, string(knowledgeJSON))
	if err != nil {
		log.Printf("[Jarvis/Learning] WARNING: failed to persist Yahoo intelligence: %v", err)
		return
	}

	log.Printf("[Jarvis/Learning] Yahoo intelligence persisted — InboxRate: %.1f%%, SpamDetected: %v, SpamSuspected: %d/%d recipients",
		yahooIM.InboxRate, yahooIM.SpamDetected, spamSuspectedCount, totalYahooRecipients)
}

// ── Learning Helper Functions ────────────────────────────────────────────────

func buildRecipientOutcomeSummary(c *JarvisCampaign) map[string]interface{} {
	byStatus := make(map[string]int)
	byISP := make(map[string]map[string]int)

	for _, r := range c.Recipients {
		if r.Suppressed {
			byStatus["suppressed"]++
			continue
		}
		byStatus[r.Status]++

		if byISP[r.ISP] == nil {
			byISP[r.ISP] = make(map[string]int)
		}
		byISP[r.ISP][r.Status]++
	}

	return map[string]interface{}{
		"by_status": byStatus,
		"by_isp":    byISP,
	}
}

func buildCreativePerformanceSummary(c *JarvisCampaign) []map[string]interface{} {
	var result []map[string]interface{}
	for _, cr := range c.Creatives {
		openRate := 0.0
		if cr.Sends > 0 {
			openRate = float64(cr.Opens) / float64(cr.Sends) * 100
		}
		result = append(result, map[string]interface{}{
			"id":        cr.ID,
			"name":      cr.Name,
			"sends":     cr.Sends,
			"opens":     cr.Opens,
			"clicks":    cr.Clicks,
			"open_rate": openRate,
		})
	}
	return result
}

func buildCampaignLearnings(c *JarvisCampaign) map[string]interface{} {
	// Extract actionable learnings from the campaign
	learnings := map[string]interface{}{
		"total_rounds":  c.CurrentRound,
		"goal_reached":  c.Metrics.TotalConversions >= c.GoalConversions,
	}

	// Best creative (by open rate)
	bestCreative := ""
	bestOpenRate := 0.0
	for _, cr := range c.Creatives {
		if cr.Sends > 0 {
			rate := float64(cr.Opens) / float64(cr.Sends) * 100
			if rate > bestOpenRate {
				bestOpenRate = rate
				bestCreative = cr.Name
			}
		}
	}
	learnings["best_creative"] = bestCreative
	learnings["best_creative_open_rate"] = bestOpenRate

	// ISPs that performed well vs poorly
	goodISPs := []string{}
	badISPs := []string{}
	for ispName, im := range c.Metrics.ISPMetrics {
		if im.Sent == 0 {
			continue
		}
		if im.SpamDetected {
			badISPs = append(badISPs, ispName)
		} else if im.Opens > 0 {
			goodISPs = append(goodISPs, ispName)
		}
	}
	learnings["good_isps"] = goodISPs
	learnings["bad_isps"] = badISPs

	// Spam detection summary
	spamCount := 0
	for _, r := range c.Recipients {
		if r.SpamSuspected {
			spamCount++
		}
	}
	learnings["spam_suspected_recipients"] = spamCount

	return learnings
}

func buildYahooRecommendations(yahooIM *ISPMetrics, spamSuspectedCount, totalYahoo int) []string {
	var recs []string

	if yahooIM.InboxRate > 0 && yahooIM.InboxRate < 50 {
		recs = append(recs, "CRITICAL: Yahoo inbox rate below 50% — pause Yahoo sends for 2+ hours before next campaign")
		recs = append(recs, "Reduce Yahoo volume by 50% on resume")
		recs = append(recs, "Send only to most engaged Yahoo segment (hot openers)")
	} else if yahooIM.InboxRate > 0 && yahooIM.InboxRate < 70 {
		recs = append(recs, "WARNING: Yahoo inbox rate below 70% — consider reducing volume or improving content")
		recs = append(recs, "Review subject lines for spam trigger words")
		recs = append(recs, "Ensure SPF, DKIM, and DMARC alignment is correct")
	} else if yahooIM.InboxRate >= 70 {
		recs = append(recs, "Yahoo inbox health is acceptable — can maintain or gradually increase volume")
	}

	if spamSuspectedCount > 0 && totalYahoo > 0 {
		pct := float64(spamSuspectedCount) / float64(totalYahoo) * 100
		if pct > 50 {
			recs = append(recs, fmt.Sprintf("%.0f%% of Yahoo recipients are spam-suspected — domain reputation may be damaged", pct))
		}
	}

	if yahooIM.Bounced > 0 && yahooIM.Sent > 0 {
		bounceRate := float64(yahooIM.Bounced) / float64(yahooIM.Sent) * 100
		if bounceRate > 5 {
			recs = append(recs, fmt.Sprintf("Yahoo bounce rate %.1f%% is high — clean list before next send", bounceRate))
		}
	}

	return recs
}

// ── Everflow Attribution Helpers ─────────────────────────────────────────────

// emailHashShort returns the first 10 chars of the MD5 hash of a lowercased email.
// This is used as sub2 in Everflow tracking links for recipient-level attribution.
func emailHashShort(email string) string {
	hash := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", hash)[:10]
}

// buildRecipientTrackingURL constructs a per-recipient tracking URL with Everflow sub params.
// sub1 = JRV_{offerID}_{creativeID}_{date}_{campaignID8} — identifies the Jarvis campaign
// sub2 = {emailMD5_10} — identifies the specific recipient for conversion attribution
func (j *JarvisOrchestrator) buildRecipientTrackingURL(baseLink, recipientEmail string, creativeID int) string {
	j.mu.Lock()
	offerID := j.campaign.OfferID
	campaignID8 := j.campaign.ID[:8]
	j.mu.Unlock()

	date := time.Now().Format("01022006")
	sub1 := fmt.Sprintf("JRV_%s_%d_%s_%s", offerID, creativeID, date, campaignID8)
	sub2 := emailHashShort(recipientEmail)

	u, err := url.Parse(baseLink)
	if err != nil {
		// Fallback: append as raw query params
		sep := "?"
		if strings.Contains(baseLink, "?") {
			sep = "&"
		}
		return fmt.Sprintf("%s%ssub1=%s&sub2=%s", baseLink, sep, sub1, sub2)
	}
	q := u.Query()
	q.Set("sub1", sub1)
	q.Set("sub2", sub2)
	u.RawQuery = q.Encode()
	return u.String()
}

// persistConversion writes a matched Everflow conversion into mailing_revenue_attributions
// for permanent storage and downstream analytics. Runs as a goroutine (non-blocking).
func (j *JarvisOrchestrator) persistConversion(conv everflow.Conversion, recipientEmail, orgID, campaignID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Compute time-to-conversion interval
	var timeToConv *time.Duration
	if !conv.ClickTime.IsZero() && !conv.ConversionTime.IsZero() {
		d := conv.ConversionTime.Sub(conv.ClickTime)
		timeToConv = &d
	}

	// Check for duplicate (conversion_id is VARCHAR, no unique constraint — guard here)
	var exists bool
	_ = j.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM mailing_revenue_attributions WHERE conversion_id = $1)",
		conv.ConversionID,
	).Scan(&exists)
	if exists {
		log.Printf("[Jarvis/Attribution] Conversion %s already persisted — skipping duplicate", conv.ConversionID)
		return
	}

	// Build interval string for PostgreSQL
	var intervalStr *string
	if timeToConv != nil {
		s := fmt.Sprintf("%d seconds", int(timeToConv.Seconds()))
		intervalStr = &s
	}

	_, err := j.db.ExecContext(ctx, `
		INSERT INTO mailing_revenue_attributions (
			id, organization_id, conversion_id, revenue,
			attribution_model, attribution_weight, attributed_revenue,
			click_id, time_to_conversion, converted_at, created_at
		) VALUES ($1, $2, $3, $4, 'jarvis_attribution', 1.0, $4, $5, $6::interval, $7, NOW())
	`, uuid.New(), orgID, conv.ConversionID, conv.Revenue,
		conv.ClickID, intervalStr, conv.ConversionTime)

	if err != nil {
		log.Printf("[Jarvis/Attribution] Error persisting conversion %s: %v", conv.ConversionID, err)
	} else {
		log.Printf("[Jarvis/Attribution] Persisted conversion %s — $%.2f for %s (click: %s)",
			conv.ConversionID, conv.Revenue, recipientEmail, conv.ClickID)
	}
}

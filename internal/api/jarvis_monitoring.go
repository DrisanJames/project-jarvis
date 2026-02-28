// jarvis_monitoring.go — Autonomous loop, engagement monitoring, conversion checking, and logging.
package api

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/everflow"
)

// ── Autonomous Loop ──────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) runAutonomousLoop() {
	j.addLog("info", "Jarvis", "Autonomous orchestration loop started", nil)

	// ── Phase 1: Pre-flight checks ──
	j.preflightChecks()

	// ── Phase 2: Initial send wave ──
	j.sendWave("initial")

	// ── Phase 2b: Check Yahoo inbox placement immediately after first wave ──
	// Wait 3 minutes for delivery to settle, then check eDataSource
	time.Sleep(3 * time.Minute)
	j.checkYahooInboxPlacement()

	// ── Phase 3: Monitoring & optimization loop ──
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	retryTicker := time.NewTicker(20 * time.Minute)
	defer retryTicker.Stop()

	// Yahoo inbox check every 10 minutes (separate cadence)
	yahooTicker := time.NewTicker(10 * time.Minute)
	defer yahooTicker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check campaign status
			j.mu.Lock()
			status := j.campaign.Status
			endsAt := j.campaign.EndsAt
			j.mu.Unlock()

			if status != "running" {
				j.addLog("info", "Jarvis", fmt.Sprintf("Loop exiting: campaign status is %s", status), nil)
				return
			}
			if endsAt != nil && time.Now().After(*endsAt) {
				j.mu.Lock()
				j.campaign.Status = "completed"
				j.mu.Unlock()
				j.addLog("milestone", "Jarvis", "Campaign duration expired. Final status logged.", nil)
				j.logFinalReport()
				return
			}

			// Monitor engagement
			j.checkEngagement()

			// Check for conversions
			j.checkConversions()

		case <-retryTicker.C:
			j.mu.Lock()
			status := j.campaign.Status
			round := j.campaign.CurrentRound
			maxRounds := j.campaign.MaxRounds
			j.mu.Unlock()

			if status != "running" {
				return
			}
			if round < maxRounds {
				j.sendWave("retry")
			}

		case <-yahooTicker.C:
			// Periodic Yahoo inbox placement check via eDataSource
			j.mu.Lock()
			status := j.campaign.Status
			j.mu.Unlock()
			if status == "running" {
				j.checkYahooInboxPlacement()
			}
		}
	}
}

// ── Pre-flight Checks ────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) preflightChecks() {
	j.addLog("info", "PreFlight", "Running pre-flight checks...", nil)

	j.mu.Lock()
	suppressionID := j.campaign.SuppressionID
	recipientCount := len(j.campaign.Recipients)
	j.mu.Unlock()

	// 1. Check suppression list
	if suppressionID != "" {
		j.addLog("info", "Suppression", fmt.Sprintf(
			"Checking %d recipients against suppression list %s",
			recipientCount, suppressionID,
		), nil)

		j.mu.Lock()
		for i := range j.campaign.Recipients {
			email := j.campaign.Recipients[i].Email
			md5Hash := fmt.Sprintf("%x", md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email)))))

			var exists bool
			err := j.db.QueryRow(
				"SELECT EXISTS(SELECT 1 FROM mailing_suppression_entries WHERE list_id = $1 AND md5_hash = $2)",
				suppressionID, md5Hash,
			).Scan(&exists)

			if err != nil {
				j.addLogLocked("warning", "Suppression", fmt.Sprintf("Error checking suppression for %s: %v", email, err), nil)
				continue
			}

			if exists {
				j.campaign.Recipients[i].Suppressed = true
				j.campaign.Recipients[i].Status = "suppressed"
				j.addLogLocked("decision", "Suppression", fmt.Sprintf(
					"SUPPRESSED: %s found on suppression list %s — will not send", email, suppressionID,
				), nil)
			} else {
				j.addLogLocked("info", "Suppression", fmt.Sprintf(
					"CLEAR: %s is NOT on suppression list — eligible to receive", email,
				), nil)
			}
		}
		j.mu.Unlock()
	}

	// 2. Classify ISPs and select ESP for each
	j.mu.Lock()
	for i := range j.campaign.Recipients {
		if j.campaign.Recipients[i].Suppressed {
			continue
		}
		r := &j.campaign.Recipients[i]
		profileID := j.campaign.SendingProfiles[r.ISP]
		if profileID == "" {
			profileID = j.campaign.SendingProfiles["Other"]
		}
		j.addLogLocked("decision", "ESPRouter", fmt.Sprintf(
			"Recipient %s (ISP: %s, domain: %s) — will use sending profile %s",
			r.Email, r.ISP, r.Domain, profileID[:8]+"...",
		), nil)
	}
	j.mu.Unlock()

	// 3. Verify sending profile
	j.mu.Lock()
	pfID := j.campaign.PrimaryProfile
	j.mu.Unlock()

	var profileName, vendorType, fromEmail string
	err := j.db.QueryRow(
		"SELECT name, vendor_type, from_email FROM mailing_sending_profiles WHERE id = $1 AND status = 'active'",
		pfID,
	).Scan(&profileName, &vendorType, &fromEmail)
	if err != nil {
		j.addLog("error", "PreFlight", fmt.Sprintf("Default sending profile not found: %v", err), nil)
		return
	}
	j.addLog("info", "PreFlight", fmt.Sprintf(
		"Sending profile verified: %s (%s) from %s", profileName, vendorType, fromEmail,
	), nil)

	j.mu.Lock()
	eligible := 0
	for _, r := range j.campaign.Recipients {
		if !r.Suppressed {
			eligible++
		}
	}
	creativeCount := len(j.campaign.Creatives)
	j.mu.Unlock()

	j.addLog("milestone", "PreFlight", fmt.Sprintf(
		"Pre-flight complete: %d eligible recipients, %d suppressed, %d creatives loaded",
		eligible, recipientCount-eligible, creativeCount,
	), nil)
}

// ── Engagement Monitoring via SparkPost Events API ───────────────────────────

func (j *JarvisOrchestrator) checkEngagement() {
	// Collect active recipient emails
	j.mu.Lock()
	var emails []string
	emailToIdx := make(map[string]int) // email → recipient index
	startedAt := *j.campaign.StartedAt
	for i := range j.campaign.Recipients {
		r := &j.campaign.Recipients[i]
		if r.Suppressed || r.Status == "pending" || r.Status == "converted" {
			continue
		}
		emails = append(emails, r.Email)
		emailToIdx[r.Email] = i
	}
	j.mu.Unlock()

	if len(emails) == 0 {
		return
	}

	// Get the SparkPost API key from campaign's primary profile
	j.mu.Lock()
	profileID := j.campaign.PrimaryProfile
	j.mu.Unlock()

	var apiKey string
	j.db.QueryRow("SELECT api_key FROM mailing_sending_profiles WHERE id = $1",
		profileID).Scan(&apiKey)
	if apiKey == "" {
		j.addLog("warning", "Monitor", "Cannot check engagement — no SparkPost API key", nil)
		return
	}

	// Query SparkPost Events API by recipient emails (most reliable filter)
	events, err := j.fetchSparkPostEventsByRecipients(apiKey, emails, startedAt)
	if err != nil {
		j.addLog("warning", "Monitor", fmt.Sprintf("SparkPost events API error: %v", err), nil)
		return
	}

	j.addLog("info", "Monitor", fmt.Sprintf(
		"SparkPost returned %d events for %d recipients", len(events), len(emails),
	), nil)

	// Process events — track both global and per-ISP metrics
	j.mu.Lock()
	for _, evt := range events {
		recipIdx, ok := emailToIdx[strings.ToLower(evt.Recipient)]
		if !ok {
			continue
		}
		r := &j.campaign.Recipients[recipIdx]
		evtType := evt.Type
		isp := r.ISP
		im := j.campaign.Metrics.ISPMetrics[isp] // per-ISP metrics

		switch evtType {
		case "delivery":
			if r.Status == "sent" {
				r.Status = "delivered"
				j.campaign.Metrics.TotalDelivered++
				if im != nil {
					im.Delivered++
				}
				j.addLogLocked("info", "Delivery", fmt.Sprintf(
					"DELIVERED: %s [%s] confirmed by SparkPost (note: delivery ≠ inbox)", r.Email, isp,
				), nil)
			}
		case "open", "initial_open":
			if r.Status == "sent" || r.Status == "delivered" {
				now := time.Now()
				r.Status = "opened"
				r.LastOpenAt = &now
				j.campaign.Metrics.TotalOpens++
				if im != nil {
					im.Opens++
				}
				j.addLogLocked("milestone", "Engagement", fmt.Sprintf(
					"OPEN detected: %s [%s] opened the email!", r.Email, isp,
				), nil)
			}
		case "click":
			if r.Status == "sent" || r.Status == "delivered" || r.Status == "opened" {
				now := time.Now()
				r.Status = "clicked"
				r.LastClickAt = &now
				j.campaign.Metrics.TotalClicks++
				if im != nil {
					im.Clicks++
				}
				j.addLogLocked("milestone", "Engagement", fmt.Sprintf(
					"CLICK detected: %s [%s] clicked through to the offer!", r.Email, isp,
				), nil)
			}
		case "bounce", "out_of_band":
			if r.Status != "bounced" {
				r.Status = "bounced"
				j.campaign.Metrics.TotalBounces++
				if im != nil {
					im.Bounced++
				}
				j.addLogLocked("warning", "Delivery", fmt.Sprintf(
					"BOUNCE: %s [%s] (reason: %s)", r.Email, isp, evt.Reason,
				), nil)
			}
		}
	}

	// Update rates
	if j.campaign.Metrics.TotalSent > 0 {
		j.campaign.Metrics.OpenRate = float64(j.campaign.Metrics.TotalOpens) / float64(j.campaign.Metrics.TotalSent) * 100
		j.campaign.Metrics.ClickRate = float64(j.campaign.Metrics.TotalClicks) / float64(j.campaign.Metrics.TotalSent) * 100
	}

	// Log per-ISP engagement summary
	for ispName, im := range j.campaign.Metrics.ISPMetrics {
		if im.Sent > 0 {
			openRate := 0.0
			if im.Sent > 0 {
				openRate = float64(im.Opens) / float64(im.Sent) * 100
			}
			j.addLogLocked("info", "ISPMonitor", fmt.Sprintf(
				"[%s] Sent: %d | Delivered: %d | Opens: %d (%.1f%%) | Clicks: %d | Bounced: %d | InboxRate: %.1f%% | SpamDetected: %v",
				ispName, im.Sent, im.Delivered, im.Opens, openRate, im.Clicks, im.Bounced, im.InboxRate, im.SpamDetected,
			), nil)
		}
	}

	j.addLogLocked("info", "Monitor", fmt.Sprintf(
		"Engagement check — Sent: %d | Delivered: %d | Opens: %d (%.1f%%) | Clicks: %d (%.1f%%) | Bounced: %d | Conversions: %d",
		j.campaign.Metrics.TotalSent, j.campaign.Metrics.TotalDelivered,
		j.campaign.Metrics.TotalOpens, j.campaign.Metrics.OpenRate,
		j.campaign.Metrics.TotalClicks, j.campaign.Metrics.ClickRate,
		j.campaign.Metrics.TotalBounces, j.campaign.Metrics.TotalConversions,
	), nil)
	j.mu.Unlock()
}

// fetchSparkPostEventsByRecipients queries the SparkPost Events API filtering by recipient emails
func (j *JarvisOrchestrator) fetchSparkPostEventsByRecipients(apiKey string, emails []string, since time.Time) ([]sparkPostEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Build the URL using recipients filter — much more reliable than message_ids
	recipientList := strings.Join(emails, ",")
	fromTime := since.UTC().Format("2006-01-02T15:04")
	apiURL := fmt.Sprintf(
		"https://api.sparkpost.com/api/v1/events/message?recipients=%s&events=delivery,open,initial_open,click,bounce,out_of_band&from=%s&per_page=100",
		recipientList, fromTime,
	)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("SparkPost events API returned %d: %s", resp.StatusCode, string(body[:minInt(len(body), 200)]))
	}

	// SparkPost Events API returns events as top-level objects (NOT wrapped in msys — that's webhook format)
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResponse struct {
		Results []struct {
			Type           string `json:"type"`
			TransmissionID string `json:"transmission_id"`
			RcptTo         string `json:"rcpt_to"`
			RawReason      string `json:"raw_reason"`
			Timestamp      string `json:"timestamp"`
		} `json:"results"`
	}
	if err := json.Unmarshal(bodyBytes, &apiResponse); err != nil {
		return nil, fmt.Errorf("failed to parse events response: %w", err)
	}

	var events []sparkPostEvent
	for _, evt := range apiResponse.Results {
		if evt.Type != "" && evt.RcptTo != "" {
			events = append(events, sparkPostEvent{
				Type:      evt.Type,
				MessageID: evt.TransmissionID,
				Recipient: evt.RcptTo,
				Reason:    evt.RawReason,
				Timestamp: evt.Timestamp,
			})
		}
	}

	return events, nil
}

// Using minInt from everflow_creatives_handlers.go (same package)

// ── Conversion Checking ──────────────────────────────────────────────────────

func (j *JarvisOrchestrator) checkConversions() {
	if j.efClient == nil {
		j.addLog("warning", "Conversions", "Everflow client not configured — cannot check conversions", nil)
		return
	}

	// Gather campaign identity (no lock held during API call)
	j.mu.Lock()
	offerID := j.campaign.OfferID
	campaignID8 := j.campaign.ID[:8]
	startedAt := j.campaign.StartedAt
	orgID := j.campaign.OrganizationID
	campaignFullID := j.campaign.ID
	j.mu.Unlock()

	// Sub1 prefix for this Jarvis campaign: JRV_{offerID}_
	campaignPrefix := fmt.Sprintf("JRV_%s_", offerID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Poll Everflow API directly for today's conversions
	records, err := j.efClient.GetConversionsForDate(ctx, time.Now(), false)
	if err != nil {
		j.addLog("warning", "Conversions", fmt.Sprintf("Everflow API error: %v — will retry next cycle", err), nil)
		return
	}

	conversions := everflow.ProcessConversions(records)

	j.addLog("info", "Conversions", fmt.Sprintf(
		"Polled Everflow: %d total conversions today, filtering for Jarvis prefix '%s' with campaign '%s'...",
		len(conversions), campaignPrefix, campaignID8,
	), nil)

	// Build email-hash → recipient-index map for fast lookup
	j.mu.Lock()
	defer j.mu.Unlock()

	hashToIdx := make(map[string]int)
	for i, r := range j.campaign.Recipients {
		if r.Suppressed || r.Status == "converted" {
			continue
		}
		hashToIdx[emailHashShort(r.Email)] = i
	}

	matched := 0
	for _, conv := range conversions {
		// Filter 1: must match our Jarvis campaign sub1 prefix
		if !strings.HasPrefix(conv.Sub1, campaignPrefix) {
			continue
		}

		// Filter 2: verify the campaign ID from sub1 (MailingID field = campaignID first 8 chars)
		if conv.MailingID != campaignID8 {
			continue
		}

		// Filter 3: conversion must be after campaign start
		if startedAt != nil && !conv.ConversionTime.IsZero() && conv.ConversionTime.Before(*startedAt) {
			continue
		}

		// Match sub2 to a recipient via email hash
		recipIdx, found := hashToIdx[conv.Sub2]
		if !found {
			j.addLogLocked("info", "Conversions", fmt.Sprintf(
				"Conversion %s matched campaign but sub2='%s' doesn't match any active recipient — may be duplicate or already attributed",
				conv.ConversionID, conv.Sub2,
			), nil)
			continue
		}

		r := &j.campaign.Recipients[recipIdx]
		r.Status = "converted"
		j.campaign.Metrics.TotalConversions++
		j.campaign.Metrics.TotalRevenue += conv.Revenue
		matched++

		// Remove from hash map to prevent double-attribution
		delete(hashToIdx, conv.Sub2)

		timeToConv := "N/A"
		if !conv.ClickTime.IsZero() && !conv.ConversionTime.IsZero() {
			timeToConv = conv.ConversionTime.Sub(conv.ClickTime).Round(time.Second).String()
		}

		j.addLogLocked("milestone", "Conversion", fmt.Sprintf(
			"CONVERSION! %s converted — Revenue: $%.2f | Offer: %s | Click-to-Conv: %s | ConvID: %s",
			r.Email, conv.Revenue, j.campaign.OfferName, timeToConv, conv.ConversionID,
		), map[string]interface{}{
			"conversion_id": conv.ConversionID,
			"revenue":       conv.Revenue,
			"payout":        conv.Payout,
			"click_id":      conv.ClickID,
			"sub1":          conv.Sub1,
			"sub2":          conv.Sub2,
			"device":        conv.Device,
			"country":       conv.Country,
		})

		// Persist attribution to mailing_revenue_attributions (non-blocking)
		convCopy := conv
		go j.persistConversion(convCopy, r.Email, orgID, campaignFullID)

		// Check if goal is reached
		if j.campaign.Metrics.TotalConversions >= j.campaign.GoalConversions {
			j.campaign.Status = "completed"
			j.addLogLocked("milestone", "Jarvis", fmt.Sprintf(
				"GOAL REACHED! %d conversion(s), $%.2f total revenue. Campaign complete.",
				j.campaign.Metrics.TotalConversions, j.campaign.Metrics.TotalRevenue,
			), nil)
		}
	}

	// Update derived metrics
	if j.campaign.Metrics.TotalSent > 0 {
		j.campaign.Metrics.ConversionRate = float64(j.campaign.Metrics.TotalConversions) / float64(j.campaign.Metrics.TotalSent) * 100
		j.campaign.Metrics.RevenuePerSend = j.campaign.Metrics.TotalRevenue / float64(j.campaign.Metrics.TotalSent)
	}

	if matched > 0 {
		j.addLogLocked("info", "Conversions", fmt.Sprintf(
			"Attribution cycle complete: %d new conversion(s) matched, total: %d/$%.2f, CVR: %.2f%%, RPS: $%.4f",
			matched, j.campaign.Metrics.TotalConversions, j.campaign.Metrics.TotalRevenue,
			j.campaign.Metrics.ConversionRate, j.campaign.Metrics.RevenuePerSend,
		), nil)
	}
}

// ── Yahoo Inbox Placement Monitoring (eDataSource API) ───────────────────────

// checkYahooInboxPlacement queries eDataSource for Yahoo inbox placement data
// and feeds results into the Yahoo agent for automatic pause/throttle decisions.
// This is the critical missing piece — SparkPost "delivered" only means Yahoo's
// MTA accepted the message; it does NOT mean it went to inbox vs spam.
func (j *JarvisOrchestrator) checkYahooInboxPlacement() {
	if j.edsClient == nil {
		j.addLog("warning", "YahooMonitor", "eDataSource client not configured — cannot check Yahoo inbox placement", nil)
		return
	}

	// Get sending domain from campaign config
	j.mu.Lock()
	sendingDomain := j.campaign.SendingDomain
	j.mu.Unlock()
	if sendingDomain == "" {
		j.addLog("warning", "YahooMonitor", "No sending domain configured on campaign — cannot check Yahoo inbox placement", nil)
		return
	}

	j.addLog("info", "YahooMonitor", fmt.Sprintf(
		"Querying eDataSource for Yahoo inbox placement data (domain: %s)...", sendingDomain,
	), nil)

	// 1. Get Yahoo-specific inbox placement
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	yahooData, err := j.edsClient.GetYahooInboxData(ctx, sendingDomain)
	if err != nil {
		j.addLog("warning", "YahooMonitor", fmt.Sprintf(
			"eDataSource Yahoo API error: %v — will retry next cycle", err,
		), nil)
		return
	}

	now := time.Now()

	j.addLog("info", "YahooMonitor", fmt.Sprintf(
		"eDataSource Yahoo Report — InboxRate: %.1f%% | SpamRate: %.1f%% | BulkFolder: %.1f%% | MissingRate: %.1f%% | Trend: %s | Risk: %s | SampleSize: %d",
		yahooData.InboxRate, yahooData.SpamRate, yahooData.BulkFolderPct, yahooData.MissingRate,
		yahooData.Trend, yahooData.RiskLevel, yahooData.SampleSize,
	), map[string]interface{}{
		"inbox_rate":      yahooData.InboxRate,
		"spam_rate":       yahooData.SpamRate,
		"bulk_folder_pct": yahooData.BulkFolderPct,
		"trend":           yahooData.Trend,
		"risk_level":      yahooData.RiskLevel,
	})

	// 2. Update ISP metrics with eDataSource data
	j.mu.Lock()
	yahooIM := j.campaign.Metrics.ISPMetrics["Yahoo"]
	if yahooIM == nil {
		yahooIM = &ISPMetrics{ISP: "Yahoo"}
		j.campaign.Metrics.ISPMetrics["Yahoo"] = yahooIM
	}
	yahooIM.InboxRate = yahooData.InboxRate
	yahooIM.SpamRate = yahooData.SpamRate
	yahooIM.LastInboxCheck = &now

	// Gather Yahoo-specific metrics for the Yahoo agent
	yahooCounts := struct{ sent, opens, clicks, bounces, complaints int }{}
	for _, r := range j.campaign.Recipients {
		if r.ISP != "Yahoo" || r.Suppressed {
			continue
		}
		yahooCounts.sent += r.SendCount
		if r.Status == "opened" || r.Status == "clicked" || r.Status == "converted" {
			yahooCounts.opens++
		}
		if r.Status == "clicked" || r.Status == "converted" {
			yahooCounts.clicks++
		}
		if r.Status == "bounced" {
			yahooCounts.bounces++
		}
	}
	j.mu.Unlock()

	// 3. Feed signals into Yahoo Activation Agent for intelligent decision-making
	if j.yahooAgent != nil {
		alerts := j.yahooAgent.EvaluateSignals(
			yahooCounts.sent,
			yahooCounts.opens,
			yahooCounts.clicks,
			yahooCounts.bounces,
			yahooCounts.complaints,
			yahooData.InboxRate,
		)

		for _, alert := range alerts {
			j.addLog(alert.Level, "YahooAgent", fmt.Sprintf(
				"[YAHOO AGENT] %s (action: %s)", alert.Message, alert.Action,
			), map[string]interface{}{
				"alert_level":  alert.Level,
				"alert_action": alert.Action,
			})
		}
	}

	// 4. Per-recipient spam detection — flag SPECIFIC inboxes, not the whole ISP
	// Logic: if eDataSource says Yahoo inbox rate is poor AND a specific Yahoo
	// recipient was delivered but shows zero engagement after sufficient time,
	// that specific inbox is likely in spam. Block retries to THAT address only.
	j.mu.Lock()
	defer j.mu.Unlock()

	spamRisk := yahooData.InboxRate > 0 && yahooData.InboxRate < 70 // eDataSource says Yahoo delivery is risky
	if spamRisk {
		yahooIM.SpamDetected = true
	} else {
		yahooIM.SpamDetected = false
	}

	flaggedCount := 0
	for i := range j.campaign.Recipients {
		r := &j.campaign.Recipients[i]
		if r.ISP != "Yahoo" || r.Suppressed || r.SpamSuspected {
			continue
		}

		// Already engaged = clearly in inbox, no action needed
		if r.Status == "opened" || r.Status == "clicked" || r.Status == "converted" {
			continue
		}

		// Recipient was delivered but no engagement — check the evidence
		if (r.Status == "delivered" || r.Status == "sent") && r.SendCount > 0 && r.LastSentAt != nil {
			timeSinceSend := time.Since(*r.LastSentAt)

			// After 15+ minutes with delivery confirmed but zero opens,
			// AND eDataSource confirms spam risk → flag THIS specific inbox
			if timeSinceSend > 15*time.Minute && spamRisk {
				r.SpamSuspected = true
				r.Status = "spam_suspected"
				r.SpamReason = fmt.Sprintf(
					"delivered %s ago with no opens; eDataSource Yahoo inbox rate %.1f%% (spam rate %.1f%%)",
					timeSinceSend.Round(time.Minute), yahooData.InboxRate, yahooData.SpamRate,
				)
				flaggedCount++
				j.addLogLocked("warning", "YahooMonitor", fmt.Sprintf(
					"SPAM SUSPECTED: %s — %s. Retries to this inbox halted. Other Yahoo recipients unaffected.",
					r.Email, r.SpamReason,
				), nil)
			}

			// Even without eDataSource risk, if 30+ min and 2+ sends with zero opens → flag
			if timeSinceSend > 30*time.Minute && r.SendCount >= 2 && !r.SpamSuspected {
				r.SpamSuspected = true
				r.Status = "spam_suspected"
				r.SpamReason = fmt.Sprintf(
					"%d sends over %s with zero engagement — likely in spam folder",
					r.SendCount, timeSinceSend.Round(time.Minute),
				)
				flaggedCount++
				j.addLogLocked("warning", "YahooMonitor", fmt.Sprintf(
					"SPAM SUSPECTED: %s — %s. Retries to this inbox halted.",
					r.Email, r.SpamReason,
				), nil)
			}
		}
	}

	if flaggedCount > 0 {
		j.addLogLocked("info", "YahooMonitor", fmt.Sprintf(
			"Flagged %d Yahoo recipient(s) as spam-suspected. Remaining Yahoo recipients continue normally.",
			flaggedCount,
		), nil)
	}

	// Log overall Yahoo health
	if spamRisk {
		j.addLogLocked("warning", "YahooMonitor", fmt.Sprintf(
			"Yahoo inbox rate %.1f%% is below 70%% — individual non-engaging Yahoo recipients will be flagged. "+
				"Engaged Yahoo recipients are unaffected.",
			yahooData.InboxRate,
		), nil)
	} else if yahooData.InboxRate >= 70 {
		j.addLogLocked("info", "YahooMonitor", fmt.Sprintf(
			"Yahoo inbox health OK — InboxRate: %.1f%%, Trend: %s. All Yahoo sending continues normally.",
			yahooData.InboxRate, yahooData.Trend,
		), nil)
	}

	// Also fetch overall reputation for logging
	go func() {
		repCtx, repCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer repCancel()
		rep, repErr := j.edsClient.GetSenderReputation(repCtx, sendingDomain)
		if repErr == nil && rep != nil {
			j.addLog("info", "YahooMonitor", fmt.Sprintf(
				"Sender Reputation — Score: %.1f | 30d InboxRate: %.1f%% | 30d SpamRate: %.1f%% | ComplaintRate: %.3f%% | Risk: %s",
				rep.ReputationScore, rep.InboxRate30Day, rep.SpamRate30Day, rep.ComplaintRate, rep.Risk,
			), nil)
		}
	}()
}

// ── Final Report ─────────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) logFinalReport() {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.addLogLocked("milestone", "Jarvis", "═══════════════ FINAL CAMPAIGN REPORT ═══════════════", nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Campaign ID: %s", j.campaign.ID), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Duration: %s", time.Since(*j.campaign.StartedAt).Round(time.Minute)), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Rounds completed: %d", j.campaign.CurrentRound), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Total sent: %d", j.campaign.Metrics.TotalSent), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Opens: %d (%.1f%%)", j.campaign.Metrics.TotalOpens, j.campaign.Metrics.OpenRate), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Clicks: %d (%.1f%%)", j.campaign.Metrics.TotalClicks, j.campaign.Metrics.ClickRate), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Conversions: %d (%.1f%%)", j.campaign.Metrics.TotalConversions, j.campaign.Metrics.ConversionRate), nil)
	j.addLogLocked("info", "Report", fmt.Sprintf("Revenue: $%.2f | RPS: $%.4f", j.campaign.Metrics.TotalRevenue, j.campaign.Metrics.RevenuePerSend), nil)

	j.addLogLocked("info", "Report", "── Per-ISP Deliverability ──", nil)
	for ispName, im := range j.campaign.Metrics.ISPMetrics {
		inboxInfo := "not checked"
		if im.LastInboxCheck != nil {
			if im.SpamDetected {
				inboxInfo = fmt.Sprintf("RISKY — InboxRate: %.1f%%, SpamRate: %.1f%%", im.InboxRate, im.SpamRate)
			} else {
				inboxInfo = fmt.Sprintf("Healthy — InboxRate: %.1f%%", im.InboxRate)
			}
		}
		j.addLogLocked("info", "Report", fmt.Sprintf(
			"  [%s] Sent: %d | Delivered: %d | Opens: %d | Clicks: %d | Bounced: %d | Inbox: %s",
			ispName, im.Sent, im.Delivered, im.Opens, im.Clicks, im.Bounced, inboxInfo,
		), nil)
	}

	j.addLogLocked("info", "Report", "── Per-Recipient Summary ──", nil)
	for _, r := range j.campaign.Recipients {
		spamInfo := ""
		if r.SpamSuspected {
			spamInfo = fmt.Sprintf(" | SPAM: %s", r.SpamReason)
		}
		j.addLogLocked("info", "Report", fmt.Sprintf(
			"  %s | ISP: %s | Status: %s | Sends: %d | Messages: %d%s",
			r.Email, r.ISP, r.Status, r.SendCount, len(r.MessageIDs), spamInfo,
		), nil)
	}

	j.addLogLocked("info", "Report", "── Per-Creative Summary ──", nil)
	for _, c := range j.campaign.Creatives {
		j.addLogLocked("info", "Report", fmt.Sprintf(
			"  Creative %d (%s): Sends: %d | Opens: %d | Clicks: %d",
			c.ID, c.Name, c.Sends, c.Opens, c.Clicks,
		), nil)
	}

	j.addLogLocked("milestone", "Jarvis", "═══════════════════════════════════════════════════════", nil)

	// ── Persist learnings to database (runs outside the lock) ──
	// Copy data we need before releasing lock (lock is already held by defer)
	campaignCopy := *j.campaign
	// We'll call persist methods after unlock via a goroutine
	go j.persistAllLearnings(&campaignCopy)
}

// ── Logging ──────────────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) addLog(level, component, message string, data any) {
	entry := JarvisLogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Component: component,
		Message:   message,
		Data:      data,
	}
	j.mu.Lock()
	if j.campaign != nil {
		j.campaign.Log = append(j.campaign.Log, entry)
	}
	j.mu.Unlock()

	// Also log to stdout for terminal visibility
	log.Printf("[Jarvis/%s] [%s] %s", component, level, message)
}

func (j *JarvisOrchestrator) addLogLocked(level, component, message string, data any) {
	entry := JarvisLogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Component: component,
		Message:   message,
		Data:      data,
	}
	if j.campaign != nil {
		j.campaign.Log = append(j.campaign.Log, entry)
	}
	log.Printf("[Jarvis/%s] [%s] %s", component, level, message)
}

func (j *JarvisOrchestrator) persistCampaign() {
	j.mu.Lock()
	data, _ := json.Marshal(j.campaign)
	orgID := j.campaign.OrganizationID
	j.mu.Unlock()

	ctx := context.Background()
	j.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (id, organization_id, decision_type, decision_data, created_at)
		VALUES ($1, $2, 'jarvis_campaign', $3, NOW())
	`, uuid.New(), orgID, string(data))
}

var ownerTestAccounts = map[string]bool{
	"drisanjames@gmail.com":       true,
	"drisanjames@yahoo.com":       true,
	"drisan@myprolific.org":       true,
	"drisan@athletenarrative.com": true,
	"djames@ignitemediagroup.co":  true,
	"smurfturfgamin@gmail.com":    true,
}

func isOwnerTestAccount(email string) bool {
	return ownerTestAccounts[strings.ToLower(strings.TrimSpace(email))]
}

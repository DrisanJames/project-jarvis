// jarvis_send.go — Send wave, email delivery, creative fetching, and send time optimization.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Send Wave ────────────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) sendWave(waveType string) {
	j.mu.Lock()
	round := j.campaign.CurrentRound + 1
	j.campaign.CurrentRound = round
	recipients := j.campaign.Recipients
	creatives := j.campaign.Creatives
	trackingLink := j.campaign.TrackingLink
	j.mu.Unlock()

	j.addLog("action", "SendWave", fmt.Sprintf(
		"Starting %s wave (round %d)", waveType, round,
	), nil)

	for i := range recipients {
		j.mu.Lock()
		r := &j.campaign.Recipients[i]
		j.mu.Unlock()

		if r.Suppressed {
			continue
		}

		// Per-recipient spam block: skip THIS specific inbox if spam was detected
		if r.SpamSuspected {
			j.addLog("decision", "SendWave", fmt.Sprintf(
				"BLOCKED %s — spam suspected for this specific inbox (%s). Other %s recipients unaffected.",
				r.Email, r.SpamReason, r.ISP,
			), nil)
			continue
		}

		// STO: Send Time Optimization — defer sends to optimal hours per domain
		// Initial waves bypass STO (get the first send out), retries respect STO windows
		if waveType == "retry" && !j.isOptimalSendHour(r.Domain) {
			optHours := j.getDomainOptimalHours(r.Domain)
			j.addLog("decision", "STO", fmt.Sprintf(
				"DEFER %s — current hour %d is not optimal for %s (optimal: %v). Will retry in next optimal window.",
				r.Email, time.Now().UTC().Hour(), r.Domain, optHours,
			), nil)
			continue
		}

		// For retries: skip recipients who already clicked or converted
		if waveType == "retry" && (r.Status == "clicked" || r.Status == "converted") {
			j.addLog("decision", "SendWave", fmt.Sprintf(
				"SKIP %s — already %s, no retry needed", r.Email, r.Status,
			), nil)
			continue
		}

		// For retries: skip if sent too recently (< 30 min)
		if waveType == "retry" && r.LastSentAt != nil && time.Since(*r.LastSentAt) < 30*time.Minute {
			j.addLog("decision", "SendWave", fmt.Sprintf(
				"DEFER %s — last sent %s ago, waiting for engagement window",
				r.Email, time.Since(*r.LastSentAt).Round(time.Minute),
			), nil)
			continue
		}

		// Select creative: rotate through available creatives
		creativeIdx := (r.SendCount) % len(creatives)
		creative := creatives[creativeIdx]

		// Select subject: rotate through campaign's subject lines
		j.mu.Lock()
		campaignSubjects := j.campaign.SubjectLines
		j.mu.Unlock()

		subject := creative.Subject // default to creative's own subject
		if len(campaignSubjects) > 0 {
			subjectIdx := (r.SendCount) % len(campaignSubjects)
			subject = campaignSubjects[subjectIdx]
			// For retries, use a different subject
			if waveType == "retry" && r.SendCount > 0 {
				subjectIdx = (r.SendCount + 1 + rand.Intn(maxInt(len(campaignSubjects)-1, 1))) % len(campaignSubjects)
				subject = campaignSubjects[subjectIdx]
			}
		}

		// Build per-recipient tracking URL with sub1 (campaign identity) and sub2 (recipient identity)
		// This is the critical attribution link: Everflow click → sub1 matches campaign → sub2 matches recipient
		recipientTrackingLink := j.buildRecipientTrackingURL(trackingLink, r.Email, creative.ID)

		// Build HTML with per-recipient tracking link injected
		html := creative.HTML
		if html == "" {
			// Fallback HTML
			html = j.buildFallbackHTML(subject, recipientTrackingLink)
		} else {
			// Replace {tracking_link} placeholder with per-recipient attribution URL
			html = strings.ReplaceAll(html, "{tracking_link}", recipientTrackingLink)
		}

		j.addLog("action", "Sender", fmt.Sprintf(
			"SENDING to %s | Subject: \"%s\" | Creative: %s | ESP: SparkPost | Round: %d",
			r.Email, subject, creative.Name, round,
		), map[string]interface{}{
			"recipient":   r.Email,
			"subject":     subject,
			"creative_id": creative.ID,
			"round":       round,
			"wave_type":   waveType,
		})

		// Send via API
		result, err := j.sendEmail(r.Email, subject, html)
		if err != nil {
			j.addLog("error", "Sender", fmt.Sprintf(
				"FAILED to send to %s: %v", r.Email, err,
			), nil)
			j.mu.Lock()
			r.Status = "failed"
			j.mu.Unlock()
			continue
		}

		now := time.Now()
		j.mu.Lock()
		r.Status = "sent"
		r.LastSentAt = &now
		r.SendCount++
		r.CreativeID = creative.ID
		r.Subject = subject
		r.ESP = "sparkpost"
		if msgID, ok := result["message_id"].(string); ok {
			r.MessageIDs = append(r.MessageIDs, msgID)
		}
		j.campaign.Metrics.TotalSent++
		j.campaign.Creatives[creativeIdx].Sends++
		// Track per-ISP send count
		if im, ok := j.campaign.Metrics.ISPMetrics[r.ISP]; ok {
			im.Sent++
		}
		j.mu.Unlock()

		j.addLog("info", "Sender", fmt.Sprintf(
			"SENT to %s — message_id: %v", r.Email, result["message_id"],
		), result)

		// Small delay between sends to avoid rate limiting
		time.Sleep(2 * time.Second)
	}

	j.addLog("milestone", "SendWave", fmt.Sprintf(
		"Wave %d complete. Total sent: %d", round, j.campaign.Metrics.TotalSent,
	), nil)
}

// ── Send Email ───────────────────────────────────────────────────────────────

func (j *JarvisOrchestrator) sendEmail(to, subject, html string) (map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get sending profile from campaign config (not hardcoded)
	j.mu.Lock()
	profileID := j.campaign.PrimaryProfile
	orgID := j.campaign.OrganizationID
	// Check for ISP-specific profile override
	toParts := strings.Split(to, "@")
	if len(toParts) == 2 {
		recipISP := classifyISP(toParts[1])
		if ispProfile, ok := j.campaign.SendingProfiles[recipISP]; ok {
			profileID = ispProfile
		}
	}
	j.mu.Unlock()

	var apiKey string
	var fromEmail, fromName string
	err := j.db.QueryRowContext(ctx,
		"SELECT api_key, from_email, from_name FROM mailing_sending_profiles WHERE id = $1",
		profileID,
	).Scan(&apiKey, &fromEmail, &fromName)
	if err != nil {
		return nil, fmt.Errorf("sending profile %s error: %w", profileID, err)
	}

	textContent := "View this email in your browser for the best experience."

	result, err := j.mailingSvc.sendViaSparkPostWithKey(ctx, apiKey, to, fromEmail, fromName, "", subject, html, textContent)
	if err != nil {
		return nil, err
	}

	// Check for success
	if success, ok := result["success"].(bool); ok && !success {
		errMsg := "unknown error"
		if e, ok := result["error"].(string); ok {
			errMsg = e
		}
		return result, fmt.Errorf("ESP rejected: %s", errMsg)
	}

	// Log to database
	j.db.ExecContext(ctx, `
		INSERT INTO mailing_ai_decisions (id, organization_id, decision_type, decision_data, created_at)
		VALUES ($1, $2, 'jarvis_send', $3, NOW())
	`, uuid.New(), orgID, fmt.Sprintf(`{"to":"%s","subject":"%s","message_id":"%v","profile":"%s"}`, to, subject, result["message_id"], profileID))

	return result, nil
}

func (j *JarvisOrchestrator) buildFallbackHTML(subject, trackingLink string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>%s</title></head>
<body style="margin:0;padding:0;background:#f4f4f4;font-family:Arial,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="max-width:600px;margin:0 auto;background:#ffffff;">
<tr><td style="padding:20px;text-align:center;">
<h1 style="color:#333;">%s</h1>
<p style="font-size:16px;color:#333;">Click below to learn more about this exclusive offer.</p>
<a href="%s" style="display:inline-block;padding:15px 30px;background:#0060a9;color:#fff;text-decoration:none;border-radius:5px;font-size:18px;margin:20px 0;">
Learn More →
</a>
<p style="font-size:12px;color:#999;margin-top:20px;">This is a promotional email.</p>
</td></tr>
</table>
</body></html>`, subject, subject, trackingLink)
}

func (j *JarvisOrchestrator) fetchCreatives(offerID, orgID string) ([]JarvisCreative, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type efCreative struct {
		ID      int    `json:"network_offer_creative_id"`
		Name    string `json:"name"`
		Subject string `json:"email_subject"`
		HTML    string `json:"html_code"`
	}

	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("http://localhost:3001/api/mailing/everflow-creatives?offer_id=%s", offerID),
		nil,
	)
	req.Header.Set("X-Organization-ID", orgID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Creatives []efCreative `json:"creatives"`
	}
	json.NewDecoder(resp.Body).Decode(&data)

	var result []JarvisCreative
	for _, c := range data.Creatives {
		result = append(result, JarvisCreative{
			ID:      c.ID,
			Name:    c.Name,
			Subject: c.Subject,
			HTML:    c.HTML,
		})
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no creatives found for offer %s", offerID)
	}

	log.Printf("[Jarvis/CreativeLoader] [info] Loaded %d creatives from Everflow for offer %s", len(result), offerID)

	return result, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// SEND TIME OPTIMIZATION (STO) — per-inbox optimal hour selection
// ═══════════════════════════════════════════════════════════════════════════════

// getDomainOptimalHours queries mailing_domain_send_times for a domain's optimal hours.
// Returns the optimal hours for the current day type (weekday vs weekend).
func (j *JarvisOrchestrator) getDomainOptimalHours(domain string) []int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	isWeekend := time.Now().Weekday() == time.Saturday || time.Now().Weekday() == time.Sunday
	column := "weekday_optimal_hours"
	if isWeekend {
		column = "weekend_optimal_hours"
	}

	var hoursJSON []byte
	err := j.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s FROM mailing_domain_send_times WHERE domain = $1", column),
		strings.ToLower(domain),
	).Scan(&hoursJSON)

	if err != nil {
		// Try ISP-level fallback (e.g., "gmail" for any gmail.com variant)
		isp := strings.ToLower(classifyISP(domain))
		j.db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT %s FROM mailing_domain_send_times WHERE isp = $1 LIMIT 1", column),
			isp,
		).Scan(&hoursJSON)
	}

	var hours []int
	if len(hoursJSON) > 0 {
		json.Unmarshal(hoursJSON, &hours)
	}

	if len(hours) == 0 {
		// Industry defaults
		if isWeekend {
			return []int{10, 11, 19, 20}
		}
		return []int{9, 10, 11, 14, 15}
	}
	return hours
}

// isOptimalSendHour checks if the current UTC hour falls within optimal hours for the recipient's domain.
func (j *JarvisOrchestrator) isOptimalSendHour(domain string) bool {
	currentHour := time.Now().UTC().Hour()
	optimalHours := j.getDomainOptimalHours(domain)
	for _, h := range optimalHours {
		if h == currentHour {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

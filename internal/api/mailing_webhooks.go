package api

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

func (s *AdvancedMailingService) HandleSparkPostWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Limit webhook payload to 5MB to prevent OOM
	r.Body = http.MaxBytesReader(w, r.Body, 5*1024*1024)
	
	var events []struct {
		Msys struct {
			MessageEvent struct {
				Type        string `json:"type"`
				Recipient   string `json:"rcpt_to"`
				Reason      string `json:"reason"`
				BounceClass string `json:"bounce_class"`
				RawReason   string `json:"raw_reason"`
				ErrorCode   string `json:"error_code"`
				CampaignID  string `json:"campaign_id"`
			} `json:"message_event"`
			UnsubEvent struct {
				Type      string `json:"type"`
				Recipient string `json:"rcpt_to"`
			} `json:"unsubscribe_event"`
			ComplaintEvent struct {
				Type      string `json:"type"`
				Recipient string `json:"rcpt_to"`
			} `json:"spam_complaint"`
		} `json:"msys"`
	}
	
	json.NewDecoder(r.Body).Decode(&events)
	
	for _, event := range events {
		email := ""
		reason := ""
		eventType := ""
		bounceType := ""
		
		if event.Msys.MessageEvent.Type == "bounce" {
			email = event.Msys.MessageEvent.Recipient
			reason = event.Msys.MessageEvent.Reason
			eventType = "bounce"
			
			// Classify bounce type
			bounceClass := event.Msys.MessageEvent.BounceClass
			bounceType = "soft" // default
			if bounceClass == "10" || bounceClass == "25" || bounceClass == "30" {
				bounceType = "hard"
			}

			// Detect inbox full from error codes or reason text
			errorCode := event.Msys.MessageEvent.ErrorCode
			reasonLower := strings.ToLower(reason)
			if errorCode == "452" || errorCode == "552" ||
				strings.Contains(reasonLower, "mailbox full") ||
				strings.Contains(reasonLower, "over quota") ||
				strings.Contains(reasonLower, "insufficient storage") {
				bounceType = "inbox_full"
			}
			// Detect throttle bounces
			if strings.HasPrefix(errorCode, "421") || strings.HasPrefix(errorCode, "451") ||
				strings.Contains(reasonLower, "rate limit") ||
				strings.Contains(reasonLower, "try again later") {
				bounceType = "throttle"
			}

			// Hard bounce = auto-suppress (preserve existing behavior)
			if bounceType == "hard" {
				s.addToSuppression(ctx, email, "Hard bounce: "+reason, "sparkpost_webhook")
				s.updateSubscriberStatus(ctx, email, "bounced")
				s.updateCampaignStat(ctx, email, "bounce_count") // Link to campaign
			}
		} else if event.Msys.MessageEvent.Type == "open" {
			email = event.Msys.MessageEvent.Recipient
			eventType = "open"
		} else if event.Msys.MessageEvent.Type == "click" {
			email = event.Msys.MessageEvent.Recipient
			eventType = "click"
		} else if event.Msys.ComplaintEvent.Type == "spam_complaint" {
			email = event.Msys.ComplaintEvent.Recipient
			reason = "Spam complaint"
			eventType = "complaint"
			s.addToSuppression(ctx, email, reason, "sparkpost_webhook")
			s.updateSubscriberStatus(ctx, email, "complained")
			s.updateCampaignStat(ctx, email, "complaint_count") // Link to campaign
		} else if event.Msys.UnsubEvent.Type == "unsubscribe" {
			email = event.Msys.UnsubEvent.Recipient
			eventType = "unsubscribe"
			s.updateSubscriberStatus(ctx, email, "unsubscribed")
			s.updateCampaignStat(ctx, email, "unsubscribe_count") // Link to campaign
		}
		
		if email != "" && eventType != "" {
			recipientEmail := strings.ToLower(email)
			emailHash := sha256Hex(recipientEmail)

			// Record in bounces/complaints table (existing behavior)
			if eventType == "bounce" || eventType == "complaint" || eventType == "unsubscribe" {
				s.db.ExecContext(ctx, `
					INSERT INTO mailing_bounces (id, email, bounce_type, reason, source, created_at)
					VALUES ($1, $2, $3, $4, 'sparkpost', NOW())
					ON CONFLICT DO NOTHING
				`, uuid.New(), recipientEmail, eventType, reason)
			}

			// Update enriched inbox profile for bounces
			if eventType == "bounce" {
				_, _ = s.db.ExecContext(ctx,
					`SELECT record_inbox_event_v2($1, 'bounce', $2, NULL)`,
					emailHash, bounceType,
				)
			}

			// Update agent decision result (fire and forget)
			if eventType == "bounce" || eventType == "open" || eventType == "click" {
				evType := eventType // capture for goroutine
				eHash := emailHash  // capture for goroutine
				go func() {
					bgCtx := context.Background()
					result := "bounced"
					if evType == "open" {
						result = "opened"
					}
					if evType == "click" {
						result = "clicked"
					}
					s.db.ExecContext(bgCtx,
						`UPDATE mailing_agent_send_decisions 
						 SET result = $1, executed_at = NOW() 
						 WHERE email_hash = $2 AND executed = true AND result IS NULL`,
						result, eHash,
					)
				}()
			}
		}
	}
	
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "processed"})
}

// sha256Hex returns the lowercase hex-encoded SHA-256 hash of the input.
func sha256Hex(input string) string {
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h)
}

func (s *AdvancedMailingService) HandleSESWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Limit webhook payload to 5MB to prevent OOM
	r.Body = http.MaxBytesReader(w, r.Body, 5*1024*1024)
	
	var notification struct {
		Type    string `json:"Type"`
		Message string `json:"Message"`
	}
	json.NewDecoder(r.Body).Decode(&notification)
	
	if notification.Type == "Notification" {
		var message struct {
			NotificationType string `json:"notificationType"`
			Bounce struct {
				BounceType string `json:"bounceType"`
				BouncedRecipients []struct {
					EmailAddress string `json:"emailAddress"`
				} `json:"bouncedRecipients"`
			} `json:"bounce"`
			Complaint struct {
				ComplainedRecipients []struct {
					EmailAddress string `json:"emailAddress"`
				} `json:"complainedRecipients"`
			} `json:"complaint"`
		}
		json.Unmarshal([]byte(notification.Message), &message)
		
		if message.NotificationType == "Bounce" && message.Bounce.BounceType == "Permanent" {
			for _, recipient := range message.Bounce.BouncedRecipients {
				s.addToSuppression(ctx, recipient.EmailAddress, "SES hard bounce", "ses_webhook")
				s.updateSubscriberStatus(ctx, recipient.EmailAddress, "bounced")
				s.updateCampaignStat(ctx, recipient.EmailAddress, "bounce_count") // Link to campaign
			}
		} else if message.NotificationType == "Complaint" {
			for _, recipient := range message.Complaint.ComplainedRecipients {
				s.addToSuppression(ctx, recipient.EmailAddress, "SES complaint", "ses_webhook")
				s.updateSubscriberStatus(ctx, recipient.EmailAddress, "complained")
				s.updateCampaignStat(ctx, recipient.EmailAddress, "complaint_count") // Link to campaign
			}
		}
	}
	
	w.WriteHeader(http.StatusOK)
}

// HandleMailgunWebhook processes Mailgun bounce/complaint events
// and automatically adds them to the Global Suppression List
func (s *AdvancedMailingService) HandleMailgunWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Limit webhook payload to 5MB to prevent OOM
	r.Body = http.MaxBytesReader(w, r.Body, 5*1024*1024)
	
	// Mailgun sends webhooks as form data or JSON depending on version
	contentType := r.Header.Get("Content-Type")
	
	var eventType, recipient, reason string
	
	if strings.Contains(contentType, "application/json") {
		// Modern Mailgun webhook format (v2)
		var event struct {
			EventData struct {
				Event     string `json:"event"`
				Recipient string `json:"recipient"`
				Severity  string `json:"severity"` // "permanent" or "temporary" for bounces
				Reason    string `json:"reason"`
			} `json:"event-data"`
		}
		json.NewDecoder(r.Body).Decode(&event)
		
		eventType = event.EventData.Event
		recipient = event.EventData.Recipient
		reason = event.EventData.Reason
		
		// Handle different event types
		switch eventType {
		case "failed":
			// Check if it's a permanent (hard) bounce
			if event.EventData.Severity == "permanent" {
				s.addToSuppression(ctx, recipient, "Mailgun hard bounce: "+reason, "mailgun_webhook")
				s.updateSubscriberStatus(ctx, recipient, "bounced")
				s.updateCampaignStat(ctx, recipient, "bounce_count") // Link to campaign
				log.Printf("Mailgun hard bounce: %s - %s", recipient, reason)
			}
		case "complained":
			s.addToSuppression(ctx, recipient, "Mailgun spam complaint", "mailgun_webhook_fbl")
			s.updateSubscriberStatus(ctx, recipient, "complained")
			s.updateCampaignStat(ctx, recipient, "complaint_count") // Link to campaign
			log.Printf("Mailgun complaint: %s", recipient)
		case "unsubscribed":
			s.addToSuppression(ctx, recipient, "Mailgun unsubscribe", "mailgun_webhook_unsub")
			s.updateSubscriberStatus(ctx, recipient, "unsubscribed")
			s.updateCampaignStat(ctx, recipient, "unsubscribe_count") // Link to campaign
			log.Printf("Mailgun unsubscribe: %s", recipient)
		}
	} else {
		// Legacy Mailgun webhook format (form data)
		r.ParseForm()
		eventType = r.FormValue("event")
		recipient = r.FormValue("recipient")
		reason = r.FormValue("error")
		
		switch eventType {
		case "bounced":
			// Legacy format uses "bounced" for hard bounces
			s.addToSuppression(ctx, recipient, "Mailgun hard bounce: "+reason, "mailgun_webhook")
			s.updateSubscriberStatus(ctx, recipient, "bounced")
			s.updateCampaignStat(ctx, recipient, "bounce_count") // Link to campaign
		case "dropped":
			// Dropped can be due to previous bounces/complaints
			dropReason := r.FormValue("reason")
			if dropReason == "hardfail" {
				s.addToSuppression(ctx, recipient, "Mailgun dropped (hardfail)", "mailgun_webhook")
				s.updateSubscriberStatus(ctx, recipient, "bounced")
				s.updateCampaignStat(ctx, recipient, "bounce_count") // Link to campaign
			}
		case "complained":
			s.addToSuppression(ctx, recipient, "Mailgun spam complaint", "mailgun_webhook_fbl")
			s.updateSubscriberStatus(ctx, recipient, "complained")
			s.updateCampaignStat(ctx, recipient, "complaint_count") // Link to campaign
		case "unsubscribed":
			s.addToSuppression(ctx, recipient, "Mailgun unsubscribe", "mailgun_webhook_unsub")
			s.updateSubscriberStatus(ctx, recipient, "unsubscribed")
			s.updateCampaignStat(ctx, recipient, "unsubscribe_count") // Link to campaign
		}
	}
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// updateCampaignStat looks up the campaign from message log and updates stats
func (s *AdvancedMailingService) updateCampaignStat(ctx interface{}, email, statName string) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	
	// Find most recent campaign for this email
	var campaignID uuid.UUID
	err := s.db.QueryRow(`
		SELECT campaign_id FROM mailing_message_log 
		WHERE LOWER(email) = $1 AND campaign_id IS NOT NULL
		ORDER BY sent_at DESC LIMIT 1
	`, normalizedEmail).Scan(&campaignID)
	
	if err != nil {
		// Try tracking events as fallback
		s.db.QueryRow(`
			SELECT DISTINCT campaign_id FROM mailing_tracking_events 
			WHERE event_type = 'sent' 
			AND subscriber_id IN (SELECT id FROM mailing_subscribers WHERE LOWER(email) = $1)
			ORDER BY event_at DESC LIMIT 1
		`, normalizedEmail).Scan(&campaignID)
	}
	
	if campaignID != uuid.Nil {
		// Update campaign stat atomically
		query := fmt.Sprintf(`UPDATE mailing_campaigns SET %s = COALESCE(%s, 0) + 1, updated_at = NOW() WHERE id = $1`, statName, statName)
		_, err := s.db.Exec(query, campaignID)
		if err != nil {
			log.Printf("Failed to update campaign %s stat %s: %v", campaignID, statName, err)
		} else {
			log.Printf("Updated campaign %s: %s +1", campaignID, statName)
		}
	}
}

func (s *AdvancedMailingService) addToSuppression(ctx interface{}, email, reason, source string) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	
	// Add to legacy suppressions table
	s.db.Exec(`
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at)
		VALUES ($1, $2, $3, $4, true, NOW())
		ON CONFLICT (email) DO UPDATE SET reason = $3, source = $4, active = true, updated_at = NOW()
	`, uuid.New(), normalizedEmail, reason, source)
	
	// === AUTOMATIC GLOBAL SUPPRESSION LIST ===
	// Determine category based on reason/source
	category := "manual"
	categoryDescription := "Manual - Manually suppressed"
	
	reasonLower := strings.ToLower(reason)
	sourceLower := strings.ToLower(source)
	
	if strings.Contains(reasonLower, "hard bounce") || strings.Contains(reasonLower, "permanent") ||
	   strings.Contains(sourceLower, "bounce") {
		category = "hard_bounce"
		categoryDescription = "Hard Bounce - Permanent delivery failure"
	} else if strings.Contains(reasonLower, "complaint") || strings.Contains(reasonLower, "spam") ||
	          strings.Contains(sourceLower, "fbl") || strings.Contains(sourceLower, "complaint") {
		category = "spam_complaint"
		categoryDescription = "Spam Complaint - FBL/ISP complaint"
	} else if strings.Contains(reasonLower, "unsubscribe") {
		category = "unsubscribe"
		categoryDescription = "Unsubscribe - User opt-out request"
	}
	
	// Compute MD5 hash for the email
	hash := md5.Sum([]byte(normalizedEmail))
	md5Hash := hex.EncodeToString(hash[:])
	
	// Ensure Global Suppression List exists (using org_id from existing list)
	s.db.Exec(`
		INSERT INTO mailing_suppression_lists (id, organization_id, name, description, source, is_global)
		SELECT 'global-suppression-list', organization_id,
			'Global Suppression List', 
			'Industry-standard global suppression list containing hard bounces, complaints, unsubscribes, spam traps, and role-based addresses.',
			'system', TRUE
		FROM mailing_suppression_lists 
		WHERE id != 'global-suppression-list'
		AND NOT EXISTS (SELECT 1 FROM mailing_suppression_lists WHERE id = 'global-suppression-list')
		LIMIT 1
	`)
	
	// Add to Global Suppression List
	entryID := fmt.Sprintf("global-%d", time.Now().UnixNano())
	_, err := s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, category, is_global)
		VALUES ($1, 'global-suppression-list', $2, $3, $4, $5, $6, TRUE)
		ON CONFLICT (list_id, md5_hash) DO UPDATE SET 
			reason = EXCLUDED.reason,
			source = EXCLUDED.source,
			category = EXCLUDED.category
	`, entryID, normalizedEmail, md5Hash, categoryDescription, source, category)
	
	if err != nil {
		log.Printf("Warning: Failed to add %s to global suppression: %v", logger.RedactEmail(normalizedEmail), err)
	} else {
		log.Printf("Auto-suppressed %s to Global List [%s] via %s", logger.RedactEmail(normalizedEmail), category, source)
	}
	
	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = 'global-suppression-list'),
		    updated_at = NOW()
		WHERE id = 'global-suppression-list'
	`)
}

func (s *AdvancedMailingService) updateSubscriberStatus(ctx interface{}, email, status string) {
	s.db.Exec(`UPDATE mailing_subscribers SET status = $2, updated_at = NOW() WHERE LOWER(email) = $1`, 
		strings.ToLower(email), status)
}

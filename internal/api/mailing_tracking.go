package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Helper to sign tracking data
func signData(data, key string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// ========== REAL-TIME TRACKING HANDLERS ==========

// HandleTrackOpen records an email open event
func (svc *MailingService) HandleTrackOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	
	// Decode tracking data
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		// Return transparent pixel anyway
		svc.serveTrackingPixel(w)
		return
	}
	
	parts := strings.Split(string(decoded), "|")
	if len(parts) < 4 {
		svc.serveTrackingPixel(w)
		return
	}
	
	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])
	
	// Get subscriber email
	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)
	
	// Record open event (use emailID as the event ID for deduplication)
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, email, event_type, event_time, ip_address, user_agent, device_type)
		VALUES ($1, $2, $3, $4, $5, 'opened', NOW(), $6, $7, $8)
		ON CONFLICT DO NOTHING
	`, emailID, orgID, campaignID, subscriberID, email, r.RemoteAddr, r.UserAgent(), detectDeviceType(r.UserAgent()))
	
	// Update campaign stats (atomic increment)
	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET open_count = open_count + 1 WHERE id = $1`, campaignID)
	
	// Update subscriber engagement
	svc.db.ExecContext(ctx, `
		UPDATE mailing_subscribers SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, subscriberID)
	
	// Update inbox profile
	svc.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW()
		WHERE email = $1
	`, email)
	
	// Recalculate engagement score
	svc.updateEngagementScore(ctx, subscriberID)
	
	log.Printf("TRACK OPEN: campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	
	// Notify campaign event tracker
	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "open", email, "")
	}

	// Serve tracking pixel
	svc.serveTrackingPixel(w)
}

// HandleTrackClick records a click event and redirects
func (svc *MailingService) HandleTrackClick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	
	// Decode tracking data
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "Invalid tracking link", http.StatusBadRequest)
		return
	}
	
	parts := strings.Split(string(decoded), "|")
	if len(parts) < 5 {
		http.Error(w, "Invalid tracking data", http.StatusBadRequest)
		return
	}
	
	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])
	originalURL := parts[4]
	
	// Get subscriber email
	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)
	
	// Record click event
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, email, event_type, event_time, ip_address, user_agent, device_type, link_url)
		VALUES ($1, $2, $3, $4, $5, 'clicked', NOW(), $6, $7, $8, $9)
	`, uuid.New(), orgID, campaignID, subscriberID, email, r.RemoteAddr, r.UserAgent(), detectDeviceType(r.UserAgent()), originalURL)
	
	// Update campaign stats
	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET click_count = click_count + 1 WHERE id = $1`, campaignID)
	
	// Update subscriber engagement
	svc.db.ExecContext(ctx, `
		UPDATE mailing_subscribers SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, subscriberID)
	
	// Update inbox profile
	svc.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW()
		WHERE email = $1
	`, email)
	
	// Recalculate engagement score
	svc.updateEngagementScore(ctx, subscriberID)
	
	log.Printf("TRACK CLICK: campaign=%s subscriber=%s email_id=%s url=%s", campaignID, subscriberID, emailID, originalURL)
	
	// Notify campaign event tracker
	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "click", email, "")
	}

	// Redirect to original URL
	http.Redirect(w, r, originalURL, http.StatusTemporaryRedirect)
}

// HandleTrackUnsubscribe records an unsubscribe event
func (svc *MailingService) HandleTrackUnsubscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	encoded := chi.URLParam(r, "data")
	
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		http.Error(w, "Invalid unsubscribe link", http.StatusBadRequest)
		return
	}
	
	parts := strings.Split(string(decoded), "|")
	if len(parts) < 4 {
		http.Error(w, "Invalid data", http.StatusBadRequest)
		return
	}
	
	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	
	// Get subscriber email
	var email string
	svc.db.QueryRowContext(ctx, `SELECT email FROM mailing_subscribers WHERE id = $1`, subscriberID).Scan(&email)
	
	// Record unsubscribe event
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id, email, event_type, event_time, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, 'unsubscribed', NOW(), $6, $7)
	`, uuid.New(), orgID, campaignID, subscriberID, email, r.RemoteAddr, r.UserAgent())
	
	// Update subscriber status
	svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`, subscriberID)
	
	// Update campaign stats
	svc.db.ExecContext(ctx, `UPDATE mailing_campaigns SET unsubscribe_count = COALESCE(unsubscribe_count, 0) + 1 WHERE id = $1`, campaignID)
	
	// Add to suppression list
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at, updated_at)
		VALUES ($1, $2, 'User unsubscribed', 'unsubscribe', true, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE SET active = true, reason = 'User unsubscribed', updated_at = NOW()
	`, uuid.New(), email)
	
	log.Printf("TRACK UNSUBSCRIBE: campaign=%s subscriber=%s email=%s", campaignID, subscriberID, email)
	
	// Notify campaign event tracker
	if svc.onTrackingEvent != nil {
		svc.onTrackingEvent(campaignID.String(), "unsubscribe", email, "")
	}

	// Return confirmation page
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html><html><body style="font-family:Arial;text-align:center;padding:50px;">
		<h1>You have been unsubscribed</h1>
		<p>You will no longer receive emails from us.</p>
	</body></html>`))
}

// serveTrackingPixel returns a 1x1 transparent GIF
func (svc *MailingService) serveTrackingPixel(w http.ResponseWriter) {
	// 1x1 transparent GIF
	pixel := []byte{
		0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00,
		0x80, 0x00, 0x00, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x2c,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
		0x02, 0x44, 0x01, 0x00, 0x3b,
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Write(pixel)
}

// updateEngagementScore recalculates subscriber engagement score
func (svc *MailingService) updateEngagementScore(ctx context.Context, subscriberID uuid.UUID) {
	// Get engagement metrics
	var totalOpens, totalClicks, totalEmails int
	var lastOpenAt, lastClickAt *time.Time
	
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_opens, 0), COALESCE(total_clicks, 0), COALESCE(total_emails_received, 1),
			   last_open_at, last_click_at
		FROM mailing_subscribers WHERE id = $1
	`, subscriberID).Scan(&totalOpens, &totalClicks, &totalEmails, &lastOpenAt, &lastClickAt)
	
	// Calculate engagement score (0-100)
	openRate := float64(totalOpens) / float64(totalEmails) * 100
	clickRate := float64(totalClicks) / float64(totalEmails) * 100
	
	// Base score from rates
	score := (openRate * 0.4) + (clickRate * 0.6)
	
	// Recency bonus
	if lastOpenAt != nil && time.Since(*lastOpenAt) < 7*24*time.Hour {
		score += 20
	} else if lastOpenAt != nil && time.Since(*lastOpenAt) < 30*24*time.Hour {
		score += 10
	}
	
	// Cap at 100
	if score > 100 {
		score = 100
	}
	
	svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET engagement_score = $2, updated_at = NOW() WHERE id = $1`, subscriberID, score)
}

// detectDeviceType determines device type from User-Agent
func detectDeviceType(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return "mobile"
	}
	if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		return "tablet"
	}
	return "desktop"
}

// GetTrackingEvents returns real-time tracking events for a campaign
func (svc *MailingService) HandleGetTrackingEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	campaignID := chi.URLParam(r, "campaignId")
	eventType := r.URL.Query().Get("type") // opened, clicked, bounced, complained, unsubscribed
	limit := r.URL.Query().Get("limit")
	if limit == "" { limit = "100" }
	
	// Query uses correct schema - joins to get email from subscriber
	query := `
		SELECT e.id, COALESCE(s.email, ''), e.event_type, e.event_at, e.ip_address, e.user_agent, e.device_type, e.link_url
		FROM mailing_tracking_events e
		LEFT JOIN mailing_subscribers s ON e.subscriber_id = s.id
		WHERE e.campaign_id = $1
	`
	args := []interface{}{campaignID}
	
	if eventType != "" {
		query += " AND e.event_type = $2"
		args = append(args, eventType)
	}
	
	query += " ORDER BY e.event_at DESC LIMIT " + limit
	
	rows, err := svc.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("Error querying tracking events: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	
	var events []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var email, evtType string
		var deviceType *string
		var eventTime time.Time
		var ipAddress, userAgent, linkURL *string
		rows.Scan(&id, &email, &evtType, &eventTime, &ipAddress, &userAgent, &deviceType, &linkURL)
		events = append(events, map[string]interface{}{
			"id": id.String(), "email": email, "event_type": evtType,
			"event_time": eventTime, "ip_address": ipAddress,
			"device_type": deviceType, "link_url": linkURL,
		})
	}
	if events == nil { events = []map[string]interface{}{} }
	
	// Get summary counts
	var sentCount, openCount, clickCount, bounceCount, complaintCount, unsubCount int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'sent'", campaignID).Scan(&sentCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'opened'", campaignID).Scan(&openCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'clicked'", campaignID).Scan(&clickCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'bounced'", campaignID).Scan(&bounceCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'complained'", campaignID).Scan(&complaintCount)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_tracking_events WHERE campaign_id = $1 AND event_type = 'unsubscribed'", campaignID).Scan(&unsubCount)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"campaign_id": campaignID,
		"events": events,
		"summary": map[string]interface{}{
			"sent":         sentCount,
			"opened":       openCount,
			"clicked":      clickCount,
			"bounced":      bounceCount,
			"complained":   complaintCount,
			"unsubscribed": unsubCount,
			"open_rate":    calculateRate(openCount, sentCount),
			"click_rate":   calculateRate(clickCount, sentCount),
		},
	})
}

func calculateRate(count, total int) float64 {
	if total == 0 { return 0 }
	return float64(count) / float64(total) * 100
}

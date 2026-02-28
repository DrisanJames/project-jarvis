package mailing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TrackingService handles email tracking (opens, clicks, unsubscribes)
type TrackingService struct {
	store                 *Store
	signingKey            []byte
	trackingURL           string
	trackingDomainService *TrackingDomainService
}

// NewTrackingService creates a new tracking service
func NewTrackingService(store *Store, signingKey, trackingURL string) *TrackingService {
	return &TrackingService{
		store:       store,
		signingKey:  []byte(signingKey),
		trackingURL: trackingURL,
	}
}

// NewTrackingServiceWithCustomDomains creates a tracking service with custom domain support
func NewTrackingServiceWithCustomDomains(store *Store, signingKey, trackingURL string, db *sql.DB, platformDomain string) *TrackingService {
	return &TrackingService{
		store:                 store,
		signingKey:            []byte(signingKey),
		trackingURL:           trackingURL,
		trackingDomainService: NewTrackingDomainService(db, platformDomain, trackingURL),
	}
}

// SetTrackingDomainService sets the tracking domain service for custom domain support
func (ts *TrackingService) SetTrackingDomainService(tds *TrackingDomainService) {
	ts.trackingDomainService = tds
}

// getTrackingURLForOrg returns the tracking URL for a specific organization
// It will use a custom domain if one is verified, otherwise falls back to default
func (ts *TrackingService) getTrackingURLForOrg(ctx context.Context, orgID uuid.UUID) string {
	if ts.trackingDomainService == nil {
		return ts.trackingURL
	}

	customURL, err := ts.trackingDomainService.GetTrackingURL(ctx, orgID.String())
	if err != nil || customURL == "" {
		return ts.trackingURL
	}

	return customURL
}

// GenerateTrackingPixel generates the tracking pixel URL for opens
func (ts *TrackingService) GenerateTrackingPixel(orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/open/%s/%s", ts.trackingURL, encoded, signature)
}

// GenerateTrackingPixelWithContext generates the tracking pixel URL using custom domain if available
func (ts *TrackingService) GenerateTrackingPixelWithContext(ctx context.Context, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	trackingURL := ts.getTrackingURLForOrg(ctx, orgID)
	data := fmt.Sprintf("%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/open/%s/%s", trackingURL, encoded, signature)
}

// GenerateClickURL generates a tracked click URL
func (ts *TrackingService) GenerateClickURL(orgID, campaignID, subscriberID, emailID uuid.UUID, originalURL string) string {
	data := fmt.Sprintf("%s|%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID, originalURL)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/click/%s/%s", ts.trackingURL, encoded, signature)
}

// GenerateClickURLWithContext generates a tracked click URL using custom domain if available
func (ts *TrackingService) GenerateClickURLWithContext(ctx context.Context, orgID, campaignID, subscriberID, emailID uuid.UUID, originalURL string) string {
	trackingURL := ts.getTrackingURLForOrg(ctx, orgID)
	data := fmt.Sprintf("%s|%s|%s|%s|%s", orgID, campaignID, subscriberID, emailID, originalURL)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/click/%s/%s", trackingURL, encoded, signature)
}

// GenerateUnsubscribeURL generates an unsubscribe URL
func (ts *TrackingService) GenerateUnsubscribeURL(orgID, campaignID, subscriberID uuid.UUID) string {
	data := fmt.Sprintf("%s|%s|%s", orgID, campaignID, subscriberID)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/unsubscribe/%s/%s", ts.trackingURL, encoded, signature)
}

// GenerateUnsubscribeURLWithContext generates an unsubscribe URL using custom domain if available
func (ts *TrackingService) GenerateUnsubscribeURLWithContext(ctx context.Context, orgID, campaignID, subscriberID uuid.UUID) string {
	trackingURL := ts.getTrackingURLForOrg(ctx, orgID)
	data := fmt.Sprintf("%s|%s|%s", orgID, campaignID, subscriberID)
	signature := ts.sign(data)
	encoded := base64.URLEncoding.EncodeToString([]byte(data))
	return fmt.Sprintf("%s/track/unsubscribe/%s/%s", trackingURL, encoded, signature)
}

// sign creates an HMAC signature
func (ts *TrackingService) sign(data string) string {
	h := hmac.New(sha256.New, ts.signingKey)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// verify verifies an HMAC signature
func (ts *TrackingService) verify(data, signature string) bool {
	expected := ts.sign(data)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// HandleOpen processes an open tracking request
func (ts *TrackingService) HandleOpen(ctx context.Context, encoded, signature string, r *http.Request) error {
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("invalid encoding")
	}

	data := string(decoded)
	if !ts.verify(data, signature) {
		return fmt.Errorf("invalid signature")
	}

	parts := strings.Split(data, "|")
	if len(parts) != 4 {
		return fmt.Errorf("invalid data format")
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])

	event := &TrackingEvent{
		OrganizationID: orgID,
		CampaignID:     &campaignID,
		SubscriberID:   &subscriberID,
		EmailID:        &emailID,
		EventType:      EventOpened,
		IPAddress:      getIP(r),
		UserAgent:      r.UserAgent(),
		DeviceType:     detectDevice(r.UserAgent()),
		EventAt:        time.Now(),
	}

	if err := ts.store.RecordTrackingEvent(ctx, event); err != nil {
		return err
	}

	ts.store.UpdateSubscriberEngagement(ctx, subscriberID, EventOpened)
	ts.store.UpdateCampaignStats(ctx, campaignID, "open_count", 1)
	ts.store.UpdateSubscriberQuality(ctx, subscriberID, 1.00)

	return nil
}

// HandleClick processes a click tracking request
func (ts *TrackingService) HandleClick(ctx context.Context, encoded, signature string, r *http.Request) (string, error) {
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("invalid encoding")
	}

	data := string(decoded)
	if !ts.verify(data, signature) {
		return "", fmt.Errorf("invalid signature")
	}

	parts := strings.Split(data, "|")
	if len(parts) != 5 {
		return "", fmt.Errorf("invalid data format")
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])
	emailID, _ := uuid.Parse(parts[3])
	originalURL := parts[4]

	event := &TrackingEvent{
		OrganizationID: orgID,
		CampaignID:     &campaignID,
		SubscriberID:   &subscriberID,
		EmailID:        &emailID,
		EventType:      EventClicked,
		IPAddress:      getIP(r),
		UserAgent:      r.UserAgent(),
		DeviceType:     detectDevice(r.UserAgent()),
		LinkURL:        originalURL,
		EventAt:        time.Now(),
	}

	if err := ts.store.RecordTrackingEvent(ctx, event); err != nil {
		return originalURL, err
	}

	ts.store.UpdateSubscriberEngagement(ctx, subscriberID, EventClicked)
	ts.store.UpdateCampaignStats(ctx, campaignID, "click_count", 1)
	ts.store.UpdateSubscriberQuality(ctx, subscriberID, 1.00)

	// P3.1: Enrich redirect URL with subscriber identifiers for owned domains
	if isOwnedDomain(originalURL) {
		u, parseErr := url.Parse(originalURL)
		if parseErr == nil {
			q := u.Query()
			q.Set("eid", emailID.String())
			q.Set("cid", campaignID.String())
			q.Set("sid", subscriberID.String())
			u.RawQuery = q.Encode()
			originalURL = u.String()
		}
	}

	return originalURL, nil
}

// HandleUnsubscribe processes an unsubscribe request
func (ts *TrackingService) HandleUnsubscribe(ctx context.Context, encoded, signature string, r *http.Request) error {
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("invalid encoding")
	}

	data := string(decoded)
	if !ts.verify(data, signature) {
		return fmt.Errorf("invalid signature")
	}

	parts := strings.Split(data, "|")
	if len(parts) != 3 {
		return fmt.Errorf("invalid data format")
	}

	orgID, _ := uuid.Parse(parts[0])
	campaignID, _ := uuid.Parse(parts[1])
	subscriberID, _ := uuid.Parse(parts[2])

	event := &TrackingEvent{
		OrganizationID: orgID,
		CampaignID:     &campaignID,
		SubscriberID:   &subscriberID,
		EventType:      EventUnsubscribed,
		IPAddress:      getIP(r),
		UserAgent:      r.UserAgent(),
		EventAt:        time.Now(),
	}

	if err := ts.store.RecordTrackingEvent(ctx, event); err != nil {
		return err
	}

	ts.store.UpdateCampaignStats(ctx, campaignID, "unsubscribe_count", 1)

	// Update subscriber status - would need to add this method
	// ts.store.UpdateSubscriberStatus(ctx, subscriberID, SubscriberUnsubscribed)

	return nil
}

// InjectTracking injects tracking pixels and click tracking into HTML
func (ts *TrackingService) InjectTracking(html string, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	// Inject open tracking pixel before </body>
	pixel := fmt.Sprintf(`<img src="%s" width="1" height="1" style="display:none" />`,
		ts.GenerateTrackingPixel(orgID, campaignID, subscriberID, emailID))
	html = strings.Replace(html, "</body>", pixel+"</body>", 1)

	// Replace links with tracked versions
	html = ts.replaceLinks(html, orgID, campaignID, subscriberID, emailID)

	return html
}

// InjectTrackingWithContext injects tracking using custom domain if available
func (ts *TrackingService) InjectTrackingWithContext(ctx context.Context, html string, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	// Inject open tracking pixel before </body>
	pixel := fmt.Sprintf(`<img src="%s" width="1" height="1" style="display:none" />`,
		ts.GenerateTrackingPixelWithContext(ctx, orgID, campaignID, subscriberID, emailID))
	html = strings.Replace(html, "</body>", pixel+"</body>", 1)

	// Replace links with tracked versions using custom domain
	html = ts.replaceLinksWithContext(ctx, html, orgID, campaignID, subscriberID, emailID)

	return html
}

// replaceLinks replaces all href links with tracked versions
func (ts *TrackingService) replaceLinks(html string, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	// Simple link replacement - in production use proper HTML parsing
	result := html

	// Find and replace href attributes
	for {
		start := strings.Index(result, `href="http`)
		if start == -1 {
			break
		}
		start += 6 // Skip href="

		end := strings.Index(result[start:], `"`)
		if end == -1 {
			break
		}

		originalURL := result[start : start+end]
		
		// Skip tracking/unsubscribe URLs
		if strings.Contains(originalURL, "/track/") {
			result = result[:start] + "SKIP" + result[start:]
			continue
		}

		trackedURL := ts.GenerateClickURL(orgID, campaignID, subscriberID, emailID, originalURL)
		result = result[:start] + trackedURL + result[start+end:]
	}

	result = strings.ReplaceAll(result, "SKIP", "")
	return result
}

// replaceLinksWithContext replaces all href links with tracked versions using custom domain
func (ts *TrackingService) replaceLinksWithContext(ctx context.Context, html string, orgID, campaignID, subscriberID, emailID uuid.UUID) string {
	// Simple link replacement - in production use proper HTML parsing
	result := html

	// Find and replace href attributes
	for {
		start := strings.Index(result, `href="http`)
		if start == -1 {
			break
		}
		start += 6 // Skip href="

		end := strings.Index(result[start:], `"`)
		if end == -1 {
			break
		}

		originalURL := result[start : start+end]
		
		// Skip tracking/unsubscribe URLs
		if strings.Contains(originalURL, "/track/") {
			result = result[:start] + "SKIP" + result[start:]
			continue
		}

		trackedURL := ts.GenerateClickURLWithContext(ctx, orgID, campaignID, subscriberID, emailID, originalURL)
		result = result[:start] + trackedURL + result[start+end:]
	}

	result = strings.ReplaceAll(result, "SKIP", "")
	return result
}

// Helper functions
func getIP(r *http.Request) string {
	ip := r.Header.Get("X-Forwarded-For")
	if ip != "" {
		parts := strings.Split(ip, ",")
		return strings.TrimSpace(parts[0])
	}
	ip = r.Header.Get("X-Real-IP")
	if ip != "" {
		return ip
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

func detectDevice(userAgent string) string {
	ua := strings.ToLower(userAgent)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		return "mobile"
	}
	if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		return "tablet"
	}
	return "desktop"
}

// BotDetector detects bot traffic
type BotDetector struct {
	botPatterns []string
}

// NewBotDetector creates a new bot detector
func NewBotDetector() *BotDetector {
	return &BotDetector{
		botPatterns: []string{
			"bot", "crawler", "spider", "slurp", "googlebot", "bingbot",
			"yahoo", "baidu", "yandex", "preview", "proxy", "scanner",
		},
	}
}

// IsBot checks if the user agent is a bot
func (bd *BotDetector) IsBot(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, pattern := range bd.botPatterns {
		if strings.Contains(ua, pattern) {
			return true
		}
	}
	return false
}

// WebhookHandler handles ESP webhooks (bounces, complaints, etc.)
type WebhookHandler struct {
	store *Store
}

// NewWebhookHandler creates a new webhook handler
func NewWebhookHandler(store *Store) *WebhookHandler {
	return &WebhookHandler{store: store}
}

// HandleSparkPostWebhook processes SparkPost webhook events
func (wh *WebhookHandler) HandleSparkPostWebhook(ctx context.Context, events []map[string]interface{}) error {
	for _, event := range events {
		msys, ok := event["msys"].(map[string]interface{})
		if !ok {
			continue
		}

		for eventType, data := range msys {
			eventData, ok := data.(map[string]interface{})
			if !ok {
				continue
			}

			switch eventType {
			case "message_event":
				wh.processSparkPostMessageEvent(ctx, eventData)
			case "track_event":
				wh.processSparkPostTrackEvent(ctx, eventData)
			case "unsubscribe_event":
				wh.processSparkPostUnsubscribeEvent(ctx, eventData)
			case "relay_event":
				// Handle relay events if needed
			}
		}
	}
	return nil
}

func (wh *WebhookHandler) processSparkPostMessageEvent(ctx context.Context, data map[string]interface{}) {
	eventType, _ := data["type"].(string)
	rcptTo, _ := data["rcpt_to"].(string)
	messageID, _ := data["message_id"].(string)

	switch eventType {
	case "bounce":
		bounceClass, _ := data["bounce_class"].(string)
		reason, _ := data["reason"].(string)
		wh.processBounce(ctx, rcptTo, messageID, bounceClass, reason)
	case "spam_complaint":
		wh.processComplaint(ctx, rcptTo, messageID)
	case "delivery":
		// Update delivery status
	}
}

func (wh *WebhookHandler) processSparkPostTrackEvent(ctx context.Context, data map[string]interface{}) {
	// Already handled by our tracking service
}

func (wh *WebhookHandler) processSparkPostUnsubscribeEvent(ctx context.Context, data map[string]interface{}) {
	// Already handled by our tracking service
}

func (wh *WebhookHandler) processBounce(ctx context.Context, email, messageID, bounceClass, reason string) {
	bounceType := "soft"
	if bounceClass == "10" || bounceClass == "30" || bounceClass == "90" {
		bounceType = "hard"
	}

	// Record bounce event
	// Update subscriber status if hard bounce
	fmt.Printf("Bounce: %s, type: %s, reason: %s\n", email, bounceType, reason)
}

func (wh *WebhookHandler) processComplaint(ctx context.Context, email, messageID string) {
	// Record complaint
	// Unsubscribe user
	fmt.Printf("Complaint: %s\n", email)
}

// HandleSESWebhook processes AWS SES webhook events
func (wh *WebhookHandler) HandleSESWebhook(ctx context.Context, notification map[string]interface{}) error {
	notificationType, _ := notification["notificationType"].(string)

	switch notificationType {
	case "Bounce":
		bounce, _ := notification["bounce"].(map[string]interface{})
		wh.processSESBounce(ctx, bounce)
	case "Complaint":
		complaint, _ := notification["complaint"].(map[string]interface{})
		wh.processSESComplaint(ctx, complaint)
	case "Delivery":
		// Update delivery status
	}
	return nil
}

func (wh *WebhookHandler) processSESBounce(ctx context.Context, bounce map[string]interface{}) {
	bounceType, _ := bounce["bounceType"].(string)
	recipients, _ := bounce["bouncedRecipients"].([]interface{})

	for _, r := range recipients {
		recipient, _ := r.(map[string]interface{})
		email, _ := recipient["emailAddress"].(string)
		fmt.Printf("SES Bounce: %s, type: %s\n", email, bounceType)
	}
}

func (wh *WebhookHandler) processSESComplaint(ctx context.Context, complaint map[string]interface{}) {
	recipients, _ := complaint["complainedRecipients"].([]interface{})

	for _, r := range recipients {
		recipient, _ := r.(map[string]interface{})
		email, _ := recipient["emailAddress"].(string)
		fmt.Printf("SES Complaint: %s\n", email)
	}
}

// AddUnsubscribeHeaders adds List-Unsubscribe headers
func AddUnsubscribeHeaders(headers map[string]string, unsubscribeURL string) {
	headers["List-Unsubscribe"] = fmt.Sprintf("<%s>", unsubscribeURL)
	headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
}

var ownedDomains = []string{"getmecoupons.net", "discountblog.com", "quizfiesta.com"}

func isOwnedDomain(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range ownedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ValidateEmail performs basic email validation
func ValidateEmail(email string) bool {
	email = strings.TrimSpace(email)
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return false
	}
	
	local, domain := parts[0], parts[1]
	if len(local) == 0 || len(local) > 64 {
		return false
	}
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	
	// Check for valid URL encoding
	_, err := url.Parse("mailto:" + email)
	return err == nil
}

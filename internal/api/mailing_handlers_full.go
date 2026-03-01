package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// sha256Hash generates a SHA256 hash of the input
func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// MailingService provides complete mailing functionality
// TrackingEventCallback is called when an open/click/unsubscribe is recorded.
type TrackingEventCallback func(campaignID, eventType, recipient, isp string)

type MailingService struct {
	db              *sql.DB
	sparkpostKey    string
	trackingURL     string
	signingKey      string
	throttler       *MailingThrottler
	onTrackingEvent TrackingEventCallback
	globalHub       GlobalSuppressionChecker
}

// SetTrackingEventCallback registers a callback for open/click/unsubscribe events.
func (svc *MailingService) SetTrackingEventCallback(cb TrackingEventCallback) {
	svc.onTrackingEvent = cb
}

// SetGlobalSuppressionHub connects the mailing service to the global
// suppression single source of truth for pre-send checking.
func (svc *MailingService) SetGlobalSuppressionHub(hub GlobalSuppressionChecker) {
	svc.globalHub = hub
}

func NewMailingService(db *sql.DB, sparkpostKey string) *MailingService {
	trackingURL := os.Getenv("TRACKING_URL")
	if trackingURL == "" {
		trackingURL = "http://localhost:8080"
	}
	signingKey := os.Getenv("TRACKING_SECRET")
	if signingKey == "" {
		signingKey = "ignite-tracking-secret-dev"
	}
	svc := &MailingService{
		db:           db,
		sparkpostKey: sparkpostKey,
		trackingURL:  trackingURL,
		signingKey:   signingKey,
		throttler:    NewMailingThrottler(),
	}
	go svc.ensureTrackingSchema()
	return svc
}

// MailingThrottler controls send rates
type MailingThrottler struct {
	mu          sync.RWMutex
	minute      int
	hour        int
	lastMinute  time.Time
	lastHour    time.Time
	minuteLimit int
	hourLimit   int
}

func NewMailingThrottler() *MailingThrottler {
	return &MailingThrottler{
		lastMinute:  time.Now(),
		lastHour:    time.Now(),
		minuteLimit: 1000,
		hourLimit:   50000,
	}
}

func (t *MailingThrottler) CanSend() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if now.Sub(t.lastMinute) > time.Minute {
		t.minute = 0
		t.lastMinute = now
	}
	if now.Sub(t.lastHour) > time.Hour {
		t.hour = 0
		t.lastHour = now
	}

	return t.minute < t.minuteLimit && t.hour < t.hourLimit
}

func (t *MailingThrottler) RecordSend() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minute++
	t.hour++
}

func (t *MailingThrottler) SetLimits(minute, hour int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minuteLimit = minute
	t.hourLimit = hour
}

func (t *MailingThrottler) GetStatus() map[string]interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return map[string]interface{}{
		"minute_used":  t.minute,
		"minute_limit": t.minuteLimit,
		"hour_used":    t.hour,
		"hour_limit":   t.hourLimit,
	}
}

// RegisterFullMailingRoutes registers all mailing API routes
func RegisterFullMailingRoutes(r chi.Router, db *sql.DB, sparkpostKey string) {
	svc := NewMailingService(db, sparkpostKey)

	r.Route("/mailing", func(r chi.Router) {
		// Core CRUD
		r.Get("/dashboard", svc.HandleDashboard)
		r.Get("/lists", svc.HandleGetLists)
		r.Post("/lists", svc.HandleCreateList)
		r.Get("/lists/{listId}/subscribers", svc.HandleGetSubscribers)
		r.Post("/lists/{listId}/subscribers", svc.HandleAddSubscriber)

		// Suppressions
		r.Get("/suppressions", svc.HandleGetSuppressions)
		r.Post("/suppressions", svc.HandleAddSuppression)
		r.Delete("/suppressions/{email}", svc.HandleRemoveSuppression)

		// Campaigns
		r.Get("/campaigns", svc.HandleGetCampaigns)
		r.Post("/campaigns", svc.HandleCreateCampaign)
		r.Post("/campaigns/{campaignId}/send", svc.HandleSendCampaign)

		// Email Sending
		r.Post("/send", svc.HandleSendEmail)
		r.Post("/send-test", svc.HandleSendTestEmail)

		// Throttling
		r.Get("/throttle/status", svc.HandleThrottleStatus)
		r.Post("/throttle/config", svc.HandleThrottleConfig)

		// Inbox Profiles
		r.Get("/profiles", svc.HandleGetProfiles)
		r.Get("/profiles/{email}", svc.HandleGetProfile)

		// Analytics & Decisions
		r.Get("/analytics/campaign/{campaignId}", svc.HandleCampaignAnalytics)
		r.Get("/analytics/optimal-send", svc.HandleOptimalSendTime)
		r.Get("/analytics/decision/{email}", svc.HandleSendDecision)

		// Suggestions
		r.Get("/suggestions", svc.HandleGetSuggestions)
		r.Post("/suggestions", svc.HandleAddSuggestion)
		r.Patch("/suggestions/{id}", svc.HandleUpdateSuggestion)

		// Sending Plans
		r.Get("/sending-plans", svc.HandleGetSendingPlans)
		r.Get("/delivery-servers", svc.HandleGetDeliveryServers)
	})
}

// HandleDashboard returns mailing dashboard
func (svc *MailingService) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dashboard := map[string]interface{}{
		"overview":           map[string]interface{}{},
		"performance":        map[string]interface{}{},
		"recent_campaigns":   []interface{}{},
		"throttle_status":    svc.throttler.GetStatus(),
		"inbox_profiles":     0,
		"total_suppressions": 0,
		"active_automations": 0,
	}

	// Get counts
	var totalSubs, totalLists, totalCampaigns int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_subscribers WHERE status = 'confirmed'").Scan(&totalSubs)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_lists WHERE status = 'active'").Scan(&totalLists)
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_campaigns").Scan(&totalCampaigns)

	var totalSent, totalOpens, totalClicks int
	var totalRevenue float64
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count),0), COALESCE(SUM(open_count),0), 
			   COALESCE(SUM(click_count),0), COALESCE(SUM(revenue),0)
		FROM mailing_campaigns
	`).Scan(&totalSent, &totalOpens, &totalClicks, &totalRevenue)

	var suppressed int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_suppressions WHERE active = true").Scan(&suppressed)
	dashboard["total_suppressions"] = suppressed

	// Count inbox profiles
	var inboxProfiles int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles").Scan(&inboxProfiles)
	dashboard["inbox_profiles"] = inboxProfiles

	// Count active automations
	var activeAutomations int
	svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_automation_workflows WHERE status = 'active'").Scan(&activeAutomations)
	dashboard["active_automations"] = activeAutomations

	// Calculate actual daily sends (emails sent today)
	var dailySentToday int64
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count), 0) 
		FROM mailing_campaigns 
		WHERE (started_at >= CURRENT_DATE OR (started_at IS NULL AND created_at >= CURRENT_DATE))
		  AND status IN ('sending', 'completed', 'sent', 'paused')
	`).Scan(&dailySentToday)

	// Get daily capacity from sending profiles (sum of all active profile daily limits)
	var dailyCapacity int64
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(daily_limit), 0) 
		FROM mailing_sending_profiles 
		WHERE status = 'active' AND daily_limit > 0
	`).Scan(&dailyCapacity)
	if dailyCapacity == 0 {
		dailyCapacity = 500000 // fallback default
	}

	dailyUtilization := 0.0
	if dailyCapacity > 0 {
		dailyUtilization = float64(dailySentToday) / float64(dailyCapacity) * 100
	}

	dashboard["overview"] = map[string]interface{}{
		"total_subscribers":    totalSubs,
		"total_lists":          totalLists,
		"total_campaigns":      totalCampaigns,
		"suppressed_emails":    suppressed,
		"daily_capacity":       dailyCapacity,
		"daily_used":           dailySentToday,
		"daily_utilization":    dailyUtilization,
	}

	dashboard["total_subscribers"] = totalSubs
	dashboard["daily_capacity"] = dailyCapacity
	dashboard["daily_used"] = dailySentToday
	dashboard["daily_utilization"] = dailyUtilization
	dashboard["daily_remaining"] = dailyCapacity - dailySentToday

	openRate := 0.0
	clickRate := 0.0
	if totalSent > 0 {
		openRate = float64(totalOpens) / float64(totalSent)
		clickRate = float64(totalClicks) / float64(totalSent)
	}

	dashboard["performance"] = map[string]interface{}{
		"total_sent":    totalSent,
		"total_opens":   totalOpens,
		"total_clicks":  totalClicks,
		"total_revenue": totalRevenue,
		"open_rate":     openRate,
		"click_rate":    clickRate,
	}

	// Get recent campaigns
	rows, _ := svc.db.QueryContext(ctx, `
		SELECT id, name, status, sent_count, open_count, click_count, created_at
		FROM mailing_campaigns ORDER BY created_at DESC LIMIT 5
	`)
	defer rows.Close()

	var recentCampaigns []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, status string
		var sentCount, openCount, clickCount int
		var createdAt time.Time
		rows.Scan(&id, &name, &status, &sentCount, &openCount, &clickCount, &createdAt)
		recentCampaigns = append(recentCampaigns, map[string]interface{}{
			"id":          id.String(),
			"name":        name,
			"status":      status,
			"sent_count":  sentCount,
			"open_count":  openCount,
			"click_count": clickCount,
			"created_at":  createdAt,
		})
	}
	if recentCampaigns != nil {
		dashboard["recent_campaigns"] = recentCampaigns
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dashboard)
}

// HandleGetCampaigns returns campaigns filtered by organization
func (svc *MailingService) HandleGetCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Get organization ID dynamically
	orgID, err := GetOrgIDStringFromRequest(r)
	log.Printf("[HandleGetCampaigns] OrgID: %s, Error: %v", orgID, err)
	if err != nil || orgID == "" {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	rows, err := svc.db.QueryContext(ctx, `
		SELECT id, name, subject, from_name, from_email, status, sent_count, open_count, click_count, revenue, created_at
		FROM mailing_campaigns 
		WHERE organization_id = $1
		ORDER BY created_at DESC LIMIT 50
	`, orgID)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch campaigns"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var campaigns []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, subject, fromName, fromEmail, status string
		var sentCount, openCount, clickCount int
		var revenue float64
		var createdAt time.Time
		rows.Scan(&id, &name, &subject, &fromName, &fromEmail, &status, &sentCount, &openCount, &clickCount, &revenue, &createdAt)
		campaigns = append(campaigns, map[string]interface{}{
			"id": id.String(), "name": name, "subject": subject, "from_name": fromName,
			"from_email": fromEmail, "status": status, "sent_count": sentCount,
			"open_count": openCount, "click_count": clickCount, "revenue": revenue, "created_at": createdAt,
		})
	}
	if campaigns == nil {
		campaigns = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"campaigns": campaigns, "total": len(campaigns)})
}

// HandleCreateCampaign creates a campaign
func (svc *MailingService) HandleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name        string `json:"name"`
		Subject     string `json:"subject"`
		FromName    string `json:"from_name"`
		FromEmail   string `json:"from_email"`
		HTMLContent string `json:"html_content"`
		ListID      string `json:"list_id"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	id := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	var listID *uuid.UUID
	if input.ListID != "" {
		lid, _ := uuid.Parse(input.ListID)
		listID = &lid
	}

	_, err = svc.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (id, organization_id, list_id, name, subject, from_name, from_email, html_content, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'draft', NOW(), NOW())
	`, id, orgID, listID, input.Name, input.Subject, input.FromName, input.FromEmail, input.HTMLContent)

	if err != nil {
		log.Printf("Error creating campaign: %v", err)
		http.Error(w, `{"error":"failed to create campaign"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "name": input.Name, "subject": input.Subject, "status": "draft",
	})
}

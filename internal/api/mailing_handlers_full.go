package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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
	redisClient     *redis.Client
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

// SetRedisClient attaches a Redis client for engagement telemetry writes.
func (svc *MailingService) SetRedisClient(c *redis.Client) {
	svc.redisClient = c
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

// TryAcquire atomically checks capacity and reserves a send slot.
// Returns true if the send is allowed (slot consumed), false if throttled.
// This replaces the split CanSend()+RecordSend() pattern which had a
// TOCTOU race: concurrent callers could all observe CanSend()==true
// before any incremented the counter.
func (t *MailingThrottler) TryAcquire() bool {
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

	if t.minute >= t.minuteLimit || t.hour >= t.hourLimit {
		return false
	}
	t.minute++
	t.hour++
	return true
}

func (t *MailingThrottler) SetLimits(minute, hour int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minuteLimit = minute
	t.hourLimit = hour
}

func (t *MailingThrottler) GetStatus() ThrottleStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return ThrottleStatus{
		MinuteUsed:  t.minute,
		MinuteLimit: t.minuteLimit,
		HourUsed:    t.hour,
		HourLimit:   t.hourLimit,
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

// DefaultDailyCapacity is the absolute last-resort capacity.
// Exposed as a package-level constant so tests and config can override.
const DefaultDailyCapacity int64 = 500000

// HandleDashboard returns the mailing dashboard with:
//   - All tenant-owned data scoped to organization_id
//   - Global-only data (inbox_profiles, audience_metrics) segregated into
//     platform_intelligence with scope:"global"
//   - Section-level error handling — a failing section degrades that section,
//     not the whole response
//   - Typed response via DashboardResponse (no map[string]interface{})
func (svc *MailingService) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return
	}
	orgStr := orgID.String()

	// "Today" is the operator's local calendar day, not UTC. Without this,
	// after 6pm MDT the dashboard's "today" silently rolls forward to a fresh
	// UTC day and counters appear to reset. America/Denver matches the
	// operator's working hours; falls back to server-local if the tzdata is
	// unavailable in the runtime container.
	loc, locErr := time.LoadLocation("America/Denver")
	if locErr != nil || loc == nil {
		loc = time.Now().Location()
	}
	now := time.Now()
	nowLocal := now.In(loc)
	todayStart := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)

	sectionErrors := make(map[string]string)
	metricSources := map[string]string{
		"performance":   "mailing_campaigns SUM via ComputeMetrics (org+date-range; tracking_events fallback only for legacy filter combos)",
		"overview":      "mailing_lists.active_count + mailing_campaigns COUNT",
		"revenue":       "mailing_campaigns.revenue SUM",
		"daily_sending": "mailing_campaigns.sent_count SUM (today, org-scoped)",
		"capacity":      "mailing_sending_profiles.daily_limit SUM -> pmta_servers fallback -> DefaultDailyCapacity",
		"suppressions":  "mailing_global_suppressions (org-scoped)",
		"audience":      "mailing_subscribers (global, not org-scoped)",
		"churn":         "mailing_audience_metrics (global, not org-scoped)",
		"today_window":  "America/Denver calendar day (operator-local)",
	}

	// --- Section: Overview (org-scoped) ---
	var totalSubs, totalLists int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(active_count), 0)::int, COUNT(*)
		FROM mailing_lists
		WHERE organization_id = $1 AND status = 'active' AND COALESCE(is_visible, true) = true
	`, orgID).Scan(&totalSubs, &totalLists); err != nil {
		log.Printf("[dashboard] overview/lists error org=%s: %v", orgStr, err)
		sectionErrors["overview_lists"] = "failed to load list counts"
	}

	var totalCampaigns int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_campaigns WHERE organization_id = $1
	`, orgID).Scan(&totalCampaigns); err != nil {
		log.Printf("[dashboard] overview/campaigns error org=%s: %v", orgStr, err)
		sectionErrors["overview_campaigns"] = "failed to load campaign count"
	}

	// --- Section: Performance (org-scoped via ComputeMetrics) ---
	perfMetrics, perfErr := ComputeMetrics(ctx, svc.db, MetricsFilter{
		OrgID:     orgStr,
		StartDate: todayStart,
		EndDate:   now,
	})
	if perfErr != nil {
		log.Printf("[dashboard] performance error org=%s: %v", orgStr, perfErr)
		sectionErrors["performance"] = "failed to compute performance metrics"
	}

	// Revenue from org-scoped campaigns in today's window
	var totalRevenue float64
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(revenue), 0)
		FROM mailing_campaigns
		WHERE organization_id = $1 AND COALESCE(started_at, created_at) >= $2
	`, orgID, todayStart).Scan(&totalRevenue); err != nil {
		log.Printf("[dashboard] revenue error org=%s: %v", orgStr, err)
		sectionErrors["revenue"] = "failed to load revenue"
	}

	// --- Section: Suppressions (org-scoped) ---
	var suppressed int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1
	`, orgID).Scan(&suppressed); err != nil {
		log.Printf("[dashboard] suppressions error org=%s: %v", orgStr, err)
		sectionErrors["suppressions"] = "failed to load suppression count"
	}

	var globalSuppTotal, suppressionsToday, suppressionsYesterday int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1
	`, orgID).Scan(&globalSuppTotal); err != nil {
		log.Printf("[dashboard] global_supp_total error org=%s: %v", orgStr, err)
		sectionErrors["global_suppressions_total"] = "failed to load global suppression total"
	}
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_global_suppressions
		WHERE organization_id = $1 AND created_at >= $2
	`, orgID, todayStart).Scan(&suppressionsToday); err != nil {
		log.Printf("[dashboard] suppressions_today error org=%s: %v", orgStr, err)
		sectionErrors["suppressions_today"] = "failed to load today's suppressions"
	}
	yesterdayStart := todayStart.AddDate(0, 0, -1)
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_global_suppressions
		WHERE organization_id = $1 AND created_at >= $2 AND created_at < $3
	`, orgID, yesterdayStart, todayStart).Scan(&suppressionsYesterday); err != nil {
		log.Printf("[dashboard] suppressions_yesterday error org=%s: %v", orgStr, err)
		sectionErrors["suppressions_yesterday"] = "failed to load yesterday's suppressions"
	}

	// --- Section: Active automations (org-scoped) ---
	var activeAutomations int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM automation_flows WHERE organization_id = $1 AND status = 'active'
	`, orgID).Scan(&activeAutomations); err != nil {
		log.Printf("[dashboard] automations error org=%s: %v", orgStr, err)
		sectionErrors["automations"] = "failed to load automation count"
	}

	// --- Section: Daily sending (org-scoped) ---
	var dailySentToday int64
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(sent_count), 0)
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND (started_at >= $2 OR (started_at IS NULL AND created_at >= $2))
		  AND status IN ('sending', 'completed', 'sent', 'paused')
	`, orgID, todayStart).Scan(&dailySentToday); err != nil {
		log.Printf("[dashboard] daily_sending error org=%s: %v", orgStr, err)
		sectionErrors["daily_sending"] = "failed to load daily sent count"
	}

	// --- Section: Capacity (not org-scoped — infrastructure is shared) ---
	var dailyCapacity int64
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(daily_limit), 0)
		FROM mailing_sending_profiles
		WHERE status = 'active' AND daily_limit > 0
	`).Scan(&dailyCapacity); err != nil {
		log.Printf("[dashboard] capacity/profiles error: %v", err)
		sectionErrors["capacity"] = "failed to load sending profile capacity"
	}
	if dailyCapacity == 0 {
		if err := svc.db.QueryRowContext(ctx, `
			SELECT COUNT(*) * 100000
			FROM mailing_pmta_servers WHERE status IS NULL OR status != 'inactive'
		`).Scan(&dailyCapacity); err != nil {
			log.Printf("[dashboard] capacity/pmta error: %v", err)
		}
	}
	if dailyCapacity == 0 {
		dailyCapacity = DefaultDailyCapacity
		metricSources["capacity"] = "DefaultDailyCapacity (fallback — no active profiles or PMTA servers found)"
	}

	dailyUtilization := 0.0
	if dailyCapacity > 0 {
		dailyUtilization = float64(dailySentToday) / float64(dailyCapacity) * 100
	}

	// --- Section: Recent campaigns (org-scoped) ---
	var recentCampaigns []DashboardRecentCampaign
	if rows, qErr := svc.db.QueryContext(ctx, `
		SELECT id, name, status, sent_count, open_count, click_count, created_at
		FROM mailing_campaigns
		WHERE organization_id = $1
		ORDER BY created_at DESC LIMIT 5
	`, orgID); qErr != nil {
		log.Printf("[dashboard] recent_campaigns error org=%s: %v", orgStr, qErr)
		sectionErrors["recent_campaigns"] = "failed to load recent campaigns"
	} else {
		defer rows.Close()
		for rows.Next() {
			var c DashboardRecentCampaign
			var id uuid.UUID
			if err := rows.Scan(&id, &c.Name, &c.Status, &c.SentCount, &c.OpenCount, &c.ClickCount, &c.CreatedAt); err != nil {
				log.Printf("[dashboard] recent_campaigns scan error: %v", err)
				continue
			}
			c.ID = id.String()
			recentCampaigns = append(recentCampaigns, c)
		}
		if err := rows.Err(); err != nil {
			log.Printf("[dashboard] recent_campaigns iteration error: %v", err)
			sectionErrors["recent_campaigns"] = "partial failure reading recent campaigns"
		}
	}
	if recentCampaigns == nil {
		recentCampaigns = []DashboardRecentCampaign{}
	}

	// --- Section: PMTA infrastructure (shared) ---
	var pmtaServers int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_delivery_servers WHERE server_type = 'pmta' AND status = 'active'
	`).Scan(&pmtaServers); err != nil {
		log.Printf("[dashboard] pmta/delivery_servers error: %v", err)
	}
	var smtpProfiles int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_sending_profiles
		WHERE (vendor_type = 'pmta' OR vendor_type = 'smtp') AND status = 'active'
	`).Scan(&smtpProfiles); err != nil {
		log.Printf("[dashboard] pmta/sending_profiles error: %v", err)
	}
	var pmtaRegistry int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_pmta_servers WHERE status IS NULL OR status != 'inactive'
	`).Scan(&pmtaRegistry); err != nil {
		log.Printf("[dashboard] pmta/registry error: %v", err)
	}
	totalPMTA := pmtaServers + smtpProfiles + pmtaRegistry

	// --- Section: Platform Intelligence (global — tables lack org_id) ---
	var inboxProfiles, inboxProfilesToday int
	if err := svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles").Scan(&inboxProfiles); err != nil {
		log.Printf("[dashboard] inbox_profiles error: %v", err)
		sectionErrors["inbox_profiles"] = "failed to load inbox profiles"
	}
	if err := svc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mailing_inbox_profiles WHERE first_seen_at >= $1", todayStart).Scan(&inboxProfilesToday); err != nil {
		log.Printf("[dashboard] inbox_profiles_today error: %v", err)
		sectionErrors["inbox_profiles_today"] = "failed to load today's inbox profiles"
	}

	var activeAudience int
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM mailing_subscribers s
		WHERE s.status = 'confirmed'
		  AND (s.last_open_at >= NOW() - INTERVAL '60 days' OR s.last_click_at >= NOW() - INTERVAL '60 days')
		  AND s.list_id NOT IN (SELECT id FROM mailing_lists WHERE name ILIKE '%seed%')
	`).Scan(&activeAudience); err != nil {
		log.Printf("[dashboard] active_audience error: %v", err)
		sectionErrors["active_audience"] = "failed to compute active audience"
	}

	var churnPct, introPct float64
	var metricsComputedAt *time.Time
	if err := svc.db.QueryRowContext(ctx, `
		SELECT global_churn_pct, global_intro_pct, computed_at
		FROM mailing_audience_metrics WHERE id = 1
	`).Scan(&churnPct, &introPct, &metricsComputedAt); err != nil {
		log.Printf("[dashboard] audience_metrics error: %v", err)
	}

	platformIntel := &PlatformIntelligence{
		Scope:              "global",
		InboxProfiles:      inboxProfiles,
		InboxProfilesToday: inboxProfilesToday,
		ActiveAudience60d:  activeAudience,
		GlobalChurnPct:     churnPct,
		GlobalIntroPct:     introPct,
		MetricsComputedAt:  metricsComputedAt,
	}

	// Assemble typed response — backward-compatible top-level keys preserved
	openRate := 0.0
	clickRate := 0.0
	if perfMetrics.OpenRate > 0 {
		openRate = perfMetrics.OpenRate / 100
	}
	if perfMetrics.ClickRate > 0 {
		clickRate = perfMetrics.ClickRate / 100
	}

	resp := DashboardResponse{
		Overview: DashboardOverview{
			TotalSubscribers:         totalSubs,
			TotalLists:               totalLists,
			TotalCampaigns:           totalCampaigns,
			SuppressedEmails:         suppressed,
			PlatformDailyCapacity:    dailyCapacity,
			DailyUsed:                dailySentToday,
			PlatformDailyUtilization: dailyUtilization,
		},
		Performance: DashboardPerformance{
			TotalSent:      perfMetrics.Sent,
			TotalOpens:     perfMetrics.Opens,
			TotalClicks:    perfMetrics.Clicks,
			TotalRevenue:   totalRevenue,
			OpenRate:       openRate,
			ClickRate:      clickRate,
			HardBounces:    perfMetrics.HardBounces,
			SoftBounces:    perfMetrics.SoftBounces,
			Complaints:     perfMetrics.Complaints,
			Delivered:      perfMetrics.Delivered,
			HardBounceRate: perfMetrics.HardBounceRate,
			SoftBounceRate: perfMetrics.SoftBounceRate,
			ComplaintRate:  perfMetrics.ComplaintRate,
			DeliveryRate:   perfMetrics.DeliveryRate,
		},
		RecentCampaigns: recentCampaigns,
		ThrottleStatus:  svc.throttler.GetStatus(),

		TotalSuppressions:       suppressed,
		GlobalSuppressionsTotal: globalSuppTotal,
		SuppressionsToday:       suppressionsToday,
		SuppressionsYesterday:   suppressionsYesterday,
		ActiveAutomations:       activeAutomations,

		TotalSubscribers:         totalSubs,
		PlatformDailyCapacity:    dailyCapacity,
		DailyUsed:                dailySentToday,
		PlatformDailyUtilization: dailyUtilization,
		DailyRemaining:           dailyCapacity - dailySentToday,

		PMTAConnected:   totalPMTA > 0,
		PMTAServerCount: totalPMTA,

		// Global metrics live ONLY in platform_intelligence — no top-level dupes
		PlatformIntelligence: platformIntel,

		MetricSources: metricSources,
		GeneratedAt:   now,
	}

	if len(sectionErrors) > 0 {
		resp.SectionErrors = sectionErrors
	}

	respondJSON(w, http.StatusOK, resp)
}

// HandleGetCampaigns returns campaigns filtered by organization with real pagination.
// Total reflects actual campaign count, not just rows returned.
func (svc *MailingService) HandleGetCampaigns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return
	}

	pag := ParsePagination(r, 50, 200)

	// Real total count for this org
	var total int64
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_campaigns WHERE organization_id = $1
	`, orgID).Scan(&total); err != nil {
		log.Printf("[GetCampaigns] count error org=%s: %v", orgID, err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	rows, err := svc.db.QueryContext(ctx, `
		SELECT id, name, subject, from_name, from_email, status,
		       sent_count, open_count, click_count, bounce_count,
		       hard_bounce_count, soft_bounce_count,
		       COALESCE(delivered_count, GREATEST(sent_count - bounce_count, 0)),
		       complaint_count, unsubscribe_count, revenue, created_at
		FROM mailing_campaigns
		WHERE organization_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, orgID, pag.Limit, pag.Offset)
	if err != nil {
		log.Printf("[GetCampaigns] query error org=%s: %v", orgID, err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	defer rows.Close()

	campaigns := make([]CampaignListItem, 0)
	for rows.Next() {
		var c CampaignListItem
		var id uuid.UUID
		if err := rows.Scan(&id, &c.Name, &c.Subject, &c.FromName, &c.FromEmail, &c.Status,
			&c.SentCount, &c.OpenCount, &c.ClickCount, &c.BounceCount,
			&c.HardBounceCount, &c.SoftBounceCount, &c.DerivedDeliveredCount,
			&c.ComplaintCount, &c.UnsubscribeCount, &c.Revenue, &c.CreatedAt); err != nil {
			log.Printf("[GetCampaigns] scan error: %v", err)
			continue
		}
		c.ID = id.String()
		campaigns = append(campaigns, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[GetCampaigns] iteration error org=%s: %v", orgID, err)
	}

	// Overlay PMTA-authoritative delivery metrics from the summary table.
	overlayPMTASummary(ctx, svc.db, campaigns)

	pagMeta := PaginationMeta{
		Page:       pag.Page,
		Limit:      pag.Limit,
		Total:      total,
		TotalPages: int((total + int64(pag.Limit) - 1) / int64(pag.Limit)),
		HasMore:    int64(pag.Offset)+int64(len(campaigns)) < total,
	}

	respondJSON(w, http.StatusOK, CampaignListResponse{
		Campaigns:  campaigns,
		Total:      total,
		Pagination: &pagMeta,
	})
}

// HandleCreateCampaign creates a campaign with full input validation.
//   - JSON decode errors → 400
//   - Missing required fields (name, subject, from_name, from_email) → 400
//   - Invalid from_email format → 400
//   - Malformed list_id → 400
//   - list_id not found → 400 (sql.ErrNoRows); DB failure → 500
//   - list_id belongs to different org → 400 (prevents cross-tenant linkage)
func (svc *MailingService) HandleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return
	}

	var input struct {
		Name        string `json:"name"`
		Subject     string `json:"subject"`
		FromName    string `json:"from_name"`
		FromEmail   string `json:"from_email"`
		HTMLContent string `json:"html_content"`
		ListID      string `json:"list_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Trim all text inputs
	input.Name = strings.TrimSpace(input.Name)
	input.Subject = strings.TrimSpace(input.Subject)
	input.FromName = strings.TrimSpace(input.FromName)
	input.FromEmail = strings.TrimSpace(input.FromEmail)
	input.ListID = strings.TrimSpace(input.ListID)

	if input.Name == "" || input.Subject == "" {
		respondError(w, http.StatusBadRequest, "name and subject are required")
		return
	}
	if input.FromName == "" || input.FromEmail == "" {
		respondError(w, http.StatusBadRequest, "from_name and from_email are required")
		return
	}

	// Basic email format: must have exactly one @, non-empty local and domain parts
	if atIdx := strings.IndexByte(input.FromEmail, '@'); atIdx < 1 || atIdx >= len(input.FromEmail)-1 || strings.Count(input.FromEmail, "@") != 1 {
		respondError(w, http.StatusBadRequest, "from_email is not a valid email address")
		return
	}

	var listID *uuid.UUID
	if input.ListID != "" {
		lid, parseErr := uuid.Parse(input.ListID)
		if parseErr != nil {
			respondError(w, http.StatusBadRequest, "list_id is not a valid UUID")
			return
		}

		// Verify list exists AND belongs to this org.
		// Distinguish "not found" (user error) from DB failure (server error).
		var ownerOrg uuid.UUID
		err := svc.db.QueryRowContext(ctx, `
			SELECT organization_id FROM mailing_lists WHERE id = $1
		`, lid).Scan(&ownerOrg)
		if err == sql.ErrNoRows {
			respondError(w, http.StatusBadRequest, "list_id does not exist")
			return
		}
		if err != nil {
			log.Printf("[CreateCampaign] list ownership check error org=%s list=%s: %v", orgID, lid, err)
			respondError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if ownerOrg != orgID {
			respondError(w, http.StatusBadRequest, "list_id does not belong to your organization")
			return
		}
		listID = &lid
	}

	id := uuid.New()
	createdAt := time.Now().UTC()
	_, err = svc.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (id, organization_id, list_id, name, subject, from_name, from_email, html_content, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'draft', $9, $9)
	`, id, orgID, listID, input.Name, input.Subject, input.FromName, input.FromEmail, input.HTMLContent, createdAt)

	if err != nil {
		log.Printf("[CreateCampaign] insert error org=%s: %v", orgID, err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	respondJSON(w, http.StatusCreated, CampaignCreateResponse{
		ID:        id.String(),
		Name:      input.Name,
		Subject:   input.Subject,
		FromName:  input.FromName,
		FromEmail: input.FromEmail,
		Status:    "draft",
		CreatedAt: createdAt.Format(time.RFC3339),
	})
}

// overlayPMTASummary enriches a slice of CampaignListItem with delivery
// metrics from pmta_acct_daily_summary. Only campaigns with PMTA data are
// updated; others keep their mailing_campaigns counter values.
func overlayPMTASummary(ctx context.Context, db *sql.DB, campaigns []CampaignListItem) {
	if len(campaigns) == 0 || db == nil {
		return
	}
	idIdx := make(map[string]int, len(campaigns))
	for i, c := range campaigns {
		idIdx[c.ID] = i
	}

	for _, c := range campaigns {
		var del, hb, sb, comp int
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(delivered),0), COALESCE(SUM(hard_bounced),0),
			       COALESCE(SUM(soft_bounced),0), COALESCE(SUM(complained),0)
			FROM pmta_acct_daily_summary
			WHERE campaign_id = $1::uuid
		`, c.ID).Scan(&del, &hb, &sb, &comp)
		if err != nil || (del == 0 && hb == 0 && sb == 0 && comp == 0) {
			continue
		}
		idx := idIdx[c.ID]
		campaigns[idx].DerivedDeliveredCount = del
		campaigns[idx].HardBounceCount = hb
		campaigns[idx].SoftBounceCount = sb
		campaigns[idx].BounceCount = hb + sb
		campaigns[idx].ComplaintCount = comp
	}
}

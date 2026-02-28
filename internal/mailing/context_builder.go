// Package mailing provides context building for template personalization
package mailing

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RenderContext is the data structure exposed to Liquid templates
type RenderContext map[string]interface{}

// ContextBuilder builds render contexts for template personalization
type ContextBuilder struct {
	db         *sql.DB
	baseURL    string
	signingKey string
}

// NewContextBuilder creates a new context builder
func NewContextBuilder(db *sql.DB, baseURL, signingKey string) *ContextBuilder {
	return &ContextBuilder{
		db:         db,
		baseURL:    baseURL,
		signingKey: signingKey,
	}
}

// BuildContext creates a complete render context for a subscriber
func (cb *ContextBuilder) BuildContext(ctx context.Context, sub *Subscriber, campaign *Campaign) (RenderContext, error) {
	rc := make(RenderContext)

	// ============================================
	// 1. PROFILE DATA (Top-level variables)
	// ============================================
	rc["first_name"] = coalesceString(sub.FirstName, "")
	rc["last_name"] = coalesceString(sub.LastName, "")
	rc["email"] = sub.Email
	rc["full_name"] = strings.TrimSpace(sub.FirstName + " " + sub.LastName)

	// Email parts
	emailParts := strings.Split(sub.Email, "@")
	if len(emailParts) == 2 {
		rc["email_local"] = emailParts[0]
		rc["email_domain"] = emailParts[1]
	}

	// ============================================
	// 2. CUSTOM FIELDS (Nested under 'custom')
	// ============================================
	// CustomFields is already a map[string]interface{} (JSON type)
	if sub.CustomFields != nil {
		rc["custom"] = sub.CustomFields
	} else {
		rc["custom"] = make(map[string]interface{})
	}

	// ============================================
	// 3. ENGAGEMENT DATA (Nested under 'engagement')
	// ============================================
	engagement := map[string]interface{}{
		"score":             sub.EngagementScore,
		"total_emails":      sub.TotalEmailsReceived,
		"total_opens":       sub.TotalOpens,
		"total_clicks":      sub.TotalClicks,
		"subscribed_at":     sub.SubscribedAt,
		"optimal_send_hour": sub.OptimalSendHourUTC,
	}

	if sub.LastOpenAt != nil {
		engagement["last_open_at"] = *sub.LastOpenAt
	}
	if sub.LastClickAt != nil {
		engagement["last_click_at"] = *sub.LastClickAt
	}
	if sub.LastEmailAt != nil {
		engagement["last_email_at"] = *sub.LastEmailAt
	}

	rc["engagement"] = engagement

	// ============================================
	// 4. COMPUTED FIELDS (Nested under 'computed')
	// ============================================
	computed, err := cb.loadComputedFields(ctx, sub.ID)
	if err != nil {
		// Log but don't fail - computed fields are optional
		computed = make(map[string]interface{})
	}
	rc["computed"] = computed

	// ============================================
	// 5. SUBSCRIBER INTELLIGENCE (Nested under 'intel')
	// ============================================
	intel, err := cb.loadSubscriberIntelligence(ctx, sub.ID)
	if err != nil {
		intel = make(map[string]interface{})
	}
	rc["intel"] = intel

	// ============================================
	// 6. SYSTEM FIELDS (Nested under 'system')
	// ============================================
	now := time.Now()
	system := map[string]interface{}{
		"current_date":     now.Format("January 2, 2006"),
		"current_datetime": now,
		"current_year":     now.Year(),
		"current_month":    now.Month().String(),
		"current_day":      now.Day(),
		"current_weekday":  now.Weekday().String(),
		"current_hour":     now.Hour(),
		"timestamp":        now.Unix(),
	}

	// Generate tracking URLs
	if campaign != nil {
		system["unsubscribe_url"] = cb.generateUnsubscribeURL(sub.ID, campaign.ID)
		system["preferences_url"] = cb.generatePreferencesURL(sub.ID)
		system["view_in_browser_url"] = cb.generateViewInBrowserURL(campaign.ID, sub.ID)
	}

	rc["system"] = system
	// Also expose common system vars at top level for convenience
	rc["now"] = now
	rc["today"] = now.Format("January 2, 2006")
	rc["year"] = now.Year()

	// ============================================
	// 7. CAMPAIGN FIELDS (Nested under 'campaign')
	// ============================================
	if campaign != nil {
		campaignData := map[string]interface{}{
			"id":           campaign.ID.String(),
			"name":         campaign.Name,
			"subject":      campaign.Subject,
			"preview_text": campaign.PreviewText,
			"from_name":    campaign.FromName,
			"from_email":   campaign.FromEmail,
		}
		rc["campaign"] = campaignData
	}

	// ============================================
	// 8. SUBSCRIBER METADATA
	// ============================================
	subscriber := map[string]interface{}{
		"id":            sub.ID.String(),
		"status":        sub.Status,
		"source":        sub.Source,
		"timezone":      sub.Timezone,
		"subscribed_at": sub.SubscribedAt,
	}
	rc["subscriber"] = subscriber

	return rc, nil
}

// BuildSampleContext creates a sample context for preview/testing
func (cb *ContextBuilder) BuildSampleContext() RenderContext {
	now := time.Now()
	lastWeek := now.AddDate(0, 0, -7)
	lastMonth := now.AddDate(0, -1, 0)

	return RenderContext{
		// Profile
		"first_name":   "John",
		"last_name":    "Doe",
		"email":        "john.doe@example.com",
		"full_name":    "John Doe",
		"email_local":  "john.doe",
		"email_domain": "example.com",

		// Custom fields
		"custom": map[string]interface{}{
			"company":         "Acme Corporation",
			"job_title":       "Marketing Manager",
			"phone":           "+1 (555) 123-4567",
			"city":            "San Francisco",
			"country":         "United States",
			"industry":        "Technology",
			"company_size":    "50-200",
			"annual_revenue":  150000.00,
			"loyalty_points":  2500,
			"is_vip":          true,
			"preferred_time":  "morning",
			"interests":       []string{"marketing", "automation", "analytics"},
			"account_manager": "Sarah Smith",
		},

		// Engagement
		"engagement": map[string]interface{}{
			"score":             85.5,
			"total_emails":      47,
			"total_opens":       38,
			"total_clicks":      12,
			"last_open_at":      lastWeek,
			"last_click_at":     lastWeek,
			"subscribed_at":     lastMonth,
			"optimal_send_hour": 10,
			"churn_risk_score":  0.15,
			"predicted_ltv":     1250.00,
		},

		// Computed
		"computed": map[string]interface{}{
			"days_since_last_purchase": 14,
			"total_purchases":          8,
			"lifetime_value":           2450.00,
			"average_order_value":      306.25,
			"purchase_frequency":       "monthly",
			"segment":                  "high_value",
			"rfm_score":                "Champions",
			"next_best_action":         "upsell",
		},

		// Intel
		"intel": map[string]interface{}{
			"best_send_time":       "Tuesday 10:00 AM",
			"preferred_content":    "product_updates",
			"device_preference":    "mobile",
			"engagement_trend":     "increasing",
			"predicted_open_prob":  0.72,
			"predicted_click_prob": 0.28,
		},

		// System
		"system": map[string]interface{}{
			"current_date":        now.Format("January 2, 2006"),
			"current_datetime":    now,
			"current_year":        now.Year(),
			"current_month":       now.Month().String(),
			"current_day":         now.Day(),
			"current_weekday":     now.Weekday().String(),
			"unsubscribe_url":     "https://example.com/unsubscribe?token=abc123",
			"preferences_url":     "https://example.com/preferences?token=abc123",
			"view_in_browser_url": "https://example.com/view?id=campaign123",
		},
		"now":   now,
		"today": now.Format("January 2, 2006"),
		"year":  now.Year(),

		// Campaign
		"campaign": map[string]interface{}{
			"id":           "550e8400-e29b-41d4-a716-446655440000",
			"name":         "February Newsletter",
			"subject":      "Your Weekly Update",
			"preview_text": "Check out what's new this week...",
			"from_name":    "Acme Team",
			"from_email":   "hello@acme.com",
		},

		// Subscriber
		"subscriber": map[string]interface{}{
			"id":            "660e8400-e29b-41d4-a716-446655440001",
			"status":        "confirmed",
			"source":        "website",
			"timezone":      "America/Los_Angeles",
			"subscribed_at": lastMonth,
		},
	}
}

// BuildContextFromSubscriberID loads a subscriber and builds context
func (cb *ContextBuilder) BuildContextFromSubscriberID(ctx context.Context, subscriberID uuid.UUID, campaign *Campaign) (RenderContext, error) {
	sub, err := cb.loadSubscriber(ctx, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriber: %w", err)
	}

	return cb.BuildContext(ctx, sub, campaign)
}

// loadSubscriber fetches a subscriber from the database
func (cb *ContextBuilder) loadSubscriber(ctx context.Context, subscriberID uuid.UUID) (*Subscriber, error) {
	sub := &Subscriber{}

	err := cb.db.QueryRowContext(ctx, `
		SELECT id, organization_id, list_id, email, email_hash, first_name, last_name,
			   status, source, ip_address, custom_fields, engagement_score,
			   total_emails_received, total_opens, total_clicks,
			   last_open_at, last_click_at, last_email_at,
			   optimal_send_hour_utc, timezone,
			   subscribed_at, unsubscribed_at, created_at, updated_at
		FROM mailing_subscribers
		WHERE id = $1
	`, subscriberID).Scan(
		&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.EmailHash,
		&sub.FirstName, &sub.LastName, &sub.Status, &sub.Source, &sub.IPAddress,
		&sub.CustomFields, &sub.EngagementScore,
		&sub.TotalEmailsReceived, &sub.TotalOpens, &sub.TotalClicks,
		&sub.LastOpenAt, &sub.LastClickAt, &sub.LastEmailAt,
		&sub.OptimalSendHourUTC, &sub.Timezone,
		&sub.SubscribedAt, &sub.UnsubscribedAt, &sub.CreatedAt, &sub.UpdatedAt,
	)

	if err != nil {
		return nil, err
	}

	return sub, nil
}

// loadComputedFields fetches computed fields for a subscriber
func (cb *ContextBuilder) loadComputedFields(ctx context.Context, subscriberID uuid.UUID) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	rows, err := cb.db.QueryContext(ctx, `
		SELECT field_key, field_value
		FROM mailing_subscriber_computed
		WHERE subscriber_id = $1
	`, subscriberID)

	if err != nil {
		if err == sql.ErrNoRows {
			return result, nil
		}
		return result, err
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var value json.RawMessage

		if err := rows.Scan(&key, &value); err != nil {
			continue
		}

		var parsed interface{}
		if err := json.Unmarshal(value, &parsed); err != nil {
			// Store as string if not valid JSON
			result[key] = string(value)
		} else {
			result[key] = parsed
		}
	}

	return result, nil
}

// loadSubscriberIntelligence fetches AI-learned patterns for a subscriber
func (cb *ContextBuilder) loadSubscriberIntelligence(ctx context.Context, subscriberID uuid.UUID) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	var engagementProfile, temporalProfile, contentPrefs, riskProfile json.RawMessage
	var profileStage string

	err := cb.db.QueryRowContext(ctx, `
		SELECT engagement_profile, temporal_profile, content_preferences, 
			   risk_profile, profile_stage
		FROM mailing_subscriber_intelligence
		WHERE subscriber_id = $1
	`, subscriberID).Scan(&engagementProfile, &temporalProfile, &contentPrefs, &riskProfile, &profileStage)

	if err != nil {
		if err == sql.ErrNoRows {
			return result, nil
		}
		return result, err
	}

	// Parse and flatten intelligence data
	if len(engagementProfile) > 0 {
		var ep map[string]interface{}
		if json.Unmarshal(engagementProfile, &ep) == nil {
			for k, v := range ep {
				result["engagement_"+k] = v
			}
		}
	}

	if len(temporalProfile) > 0 {
		var tp map[string]interface{}
		if json.Unmarshal(temporalProfile, &tp) == nil {
			for k, v := range tp {
				result["temporal_"+k] = v
			}
		}
	}

	if len(contentPrefs) > 0 {
		var cp map[string]interface{}
		if json.Unmarshal(contentPrefs, &cp) == nil {
			for k, v := range cp {
				result["content_"+k] = v
			}
		}
	}

	if len(riskProfile) > 0 {
		var rp map[string]interface{}
		if json.Unmarshal(riskProfile, &rp) == nil {
			for k, v := range rp {
				result["risk_"+k] = v
			}
		}
	}

	result["profile_stage"] = profileStage

	return result, nil
}

// generateUnsubscribeURL creates a signed unsubscribe link
func (cb *ContextBuilder) generateUnsubscribeURL(subscriberID, campaignID uuid.UUID) string {
	token := generateToken(subscriberID.String(), campaignID.String(), cb.signingKey)
	return fmt.Sprintf("%s/unsubscribe?sid=%s&cid=%s&token=%s",
		cb.baseURL, subscriberID.String(), campaignID.String(), token)
}

// generatePreferencesURL creates a link to email preferences
func (cb *ContextBuilder) generatePreferencesURL(subscriberID uuid.UUID) string {
	token := generateToken(subscriberID.String(), "preferences", cb.signingKey)
	return fmt.Sprintf("%s/preferences?sid=%s&token=%s",
		cb.baseURL, subscriberID.String(), token)
}

// generateViewInBrowserURL creates a view-in-browser link
func (cb *ContextBuilder) generateViewInBrowserURL(campaignID, subscriberID uuid.UUID) string {
	return fmt.Sprintf("%s/view?cid=%s&sid=%s",
		cb.baseURL, campaignID.String(), subscriberID.String())
}

// generateToken creates a simple HMAC token for URL verification
func generateToken(parts ...string) string {
	// Simple implementation - in production use proper HMAC
	combined := strings.Join(parts, "|")
	hash := md5.Sum([]byte(combined))
	// Return first 16 chars of a hash for brevity
	return fmt.Sprintf("%x", hash)[:16]
}

// coalesceString returns the first non-empty string
func coalesceString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// GetAvailableMergeTags returns all available merge tags for the UI
func GetAvailableMergeTags() []MergeTagDefinition {
	return []MergeTagDefinition{
		// Profile Tags
		{Key: "first_name", Label: "First Name", Category: "profile", DataType: "string", Sample: "John", Syntax: "{{ first_name }}"},
		{Key: "last_name", Label: "Last Name", Category: "profile", DataType: "string", Sample: "Doe", Syntax: "{{ last_name }}"},
		{Key: "email", Label: "Email Address", Category: "profile", DataType: "string", Sample: "john@example.com", Syntax: "{{ email }}"},
		{Key: "full_name", Label: "Full Name", Category: "profile", DataType: "string", Sample: "John Doe", Syntax: "{{ full_name }}"},

		// Custom Field Tags (examples - actual fields come from mailing_contact_fields)
		{Key: "custom.company", Label: "Company Name", Category: "custom", DataType: "string", Sample: "Acme Inc", Syntax: "{{ custom.company }}"},
		{Key: "custom.job_title", Label: "Job Title", Category: "custom", DataType: "string", Sample: "CEO", Syntax: "{{ custom.job_title }}"},
		{Key: "custom.phone", Label: "Phone Number", Category: "custom", DataType: "string", Sample: "+1 555-1234", Syntax: "{{ custom.phone }}"},
		{Key: "custom.city", Label: "City", Category: "custom", DataType: "string", Sample: "San Francisco", Syntax: "{{ custom.city }}"},

		// Engagement Tags
		{Key: "engagement.score", Label: "Engagement Score", Category: "engagement", DataType: "number", Sample: "85", Syntax: "{{ engagement.score }}"},
		{Key: "engagement.total_opens", Label: "Total Opens", Category: "engagement", DataType: "number", Sample: "42", Syntax: "{{ engagement.total_opens }}"},
		{Key: "engagement.total_clicks", Label: "Total Clicks", Category: "engagement", DataType: "number", Sample: "15", Syntax: "{{ engagement.total_clicks }}"},
		{Key: "engagement.last_open_at", Label: "Last Open Date", Category: "engagement", DataType: "date", Sample: "2026-01-15", Syntax: "{{ engagement.last_open_at | date: '%B %d' }}"},

		// Computed Tags
		{Key: "computed.days_since_last_purchase", Label: "Days Since Last Purchase", Category: "computed", DataType: "number", Sample: "14", Syntax: "{{ computed.days_since_last_purchase }}"},
		{Key: "computed.lifetime_value", Label: "Lifetime Value", Category: "computed", DataType: "number", Sample: "1250.00", Syntax: "{{ computed.lifetime_value | currency }}"},
		{Key: "computed.total_purchases", Label: "Total Purchases", Category: "computed", DataType: "number", Sample: "8", Syntax: "{{ computed.total_purchases }}"},

		// System Tags
		{Key: "system.current_date", Label: "Current Date", Category: "system", DataType: "string", Sample: "February 1, 2026", Syntax: "{{ system.current_date }}"},
		{Key: "system.current_year", Label: "Current Year", Category: "system", DataType: "number", Sample: "2026", Syntax: "{{ system.current_year }}"},
		{Key: "system.unsubscribe_url", Label: "Unsubscribe Link", Category: "system", DataType: "string", Sample: "https://...", Syntax: "{{ system.unsubscribe_url }}"},
		{Key: "system.preferences_url", Label: "Preferences Link", Category: "system", DataType: "string", Sample: "https://...", Syntax: "{{ system.preferences_url }}"},

		// Logic Examples
		{Key: "if_vip", Label: "If VIP Customer", Category: "logic", DataType: "block", Sample: "{% if custom.is_vip %}VIP Content{% endif %}", Syntax: "{% if custom.is_vip %}...{% endif %}"},
		{Key: "if_has_name", Label: "If Has First Name", Category: "logic", DataType: "block", Sample: "{% if first_name %}Hi {{ first_name }}{% else %}Hi there{% endif %}", Syntax: "{% if first_name %}...{% else %}...{% endif %}"},
		{Key: "for_interests", Label: "Loop Through Interests", Category: "logic", DataType: "block", Sample: "{% for item in custom.interests %}{{ item }}{% endfor %}", Syntax: "{% for item in custom.interests %}...{% endfor %}"},
	}
}

// MergeTagDefinition describes a merge tag for the UI
type MergeTagDefinition struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Category string `json:"category"`
	DataType string `json:"data_type"`
	Sample   string `json:"sample"`
	Syntax   string `json:"syntax"`
}

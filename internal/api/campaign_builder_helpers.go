package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// subscriber holds subscriber data for campaign sending with personalization support
type subscriber struct {
	ID                  uuid.UUID
	Email               string
	FirstName           string
	LastName            string
	CustomFields        []byte // JSON raw message
	EngagementScore     float64
	TotalEmailsReceived int
	TotalOpens          int
	TotalClicks         int
	LastOpenAt          *time.Time
	LastClickAt         *time.Time
	LastEmailAt         *time.Time
	SubscribedAt        time.Time
	Status              string
	Source              string
	Timezone            string
}

func (cb *CampaignBuilder) getSubscribers(ctx context.Context, listID, segmentID *string, maxRecipients sql.NullInt64) []subscriber {
	var query string
	var args []interface{}
	
	// Base query with all personalization fields
	selectFields := `
		id, email, COALESCE(first_name, ''), COALESCE(last_name, ''),
		COALESCE(custom_fields, '{}'), engagement_score,
		total_emails_received, total_opens, total_clicks,
		last_open_at, last_click_at, last_email_at,
		subscribed_at, status, COALESCE(source, ''), COALESCE(timezone, '')`
	
	if segmentID != nil && *segmentID != "" {
		// Build segment query - need to replace the select fields
		baseQuery, baseArgs := cb.mailingSvc.buildSegmentQuery(ctx, *segmentID)
		if baseQuery == "" {
			return nil
		}
		// The segment query returns "SELECT id, email FROM ...", we need to expand it
		// Use a subquery approach to get all fields
		query = fmt.Sprintf(`
			SELECT %s FROM mailing_subscribers 
			WHERE id IN (SELECT id FROM (%s) AS segment_ids)
		`, selectFields, baseQuery)
		args = baseArgs
	} else if listID != nil && *listID != "" {
		query = fmt.Sprintf(`SELECT %s FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`, selectFields)
		args = []interface{}{*listID}
	} else {
		return nil
	}
	
	if maxRecipients.Valid && maxRecipients.Int64 > 0 {
		query += fmt.Sprintf(" LIMIT %d", maxRecipients.Int64)
	}
	
	rows, err := cb.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("Error fetching subscribers: %v", err)
		return nil
	}
	defer rows.Close()
	
	var subscribers []subscriber
	for rows.Next() {
		var s subscriber
		err := rows.Scan(
			&s.ID, &s.Email, &s.FirstName, &s.LastName,
			&s.CustomFields, &s.EngagementScore,
			&s.TotalEmailsReceived, &s.TotalOpens, &s.TotalClicks,
			&s.LastOpenAt, &s.LastClickAt, &s.LastEmailAt,
			&s.SubscribedAt, &s.Status, &s.Source, &s.Timezone,
		)
		if err != nil {
			log.Printf("Error scanning subscriber: %v", err)
			continue
		}
		subscribers = append(subscribers, s)
	}
	return subscribers
}

func (cb *CampaignBuilder) getAudienceCount(ctx context.Context, listID, segmentID *string) int {
	var count int
	
	if segmentID != nil && *segmentID != "" {
		query, args := cb.mailingSvc.buildSegmentQuery(ctx, *segmentID)
		if query != "" {
			countQuery := strings.Replace(query, "SELECT id, email", "SELECT COUNT(*)", 1)
			cb.db.QueryRowContext(ctx, countQuery, args...).Scan(&count)
		}
	} else if listID != nil && *listID != "" {
		cb.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'
		`, *listID).Scan(&count)
	}
	
	return count
}

func calcRate(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator) * 100
}

func stripHTML(html string) string {
	// Simple HTML stripping - in production use a proper library
	result := strings.ReplaceAll(html, "<br>", "\n")
	result = strings.ReplaceAll(result, "<br/>", "\n")
	result = strings.ReplaceAll(result, "</p>", "\n\n")
	result = strings.ReplaceAll(result, "</div>", "\n")
	// Remove remaining tags
	for strings.Contains(result, "<") {
		start := strings.Index(result, "<")
		end := strings.Index(result, ">")
		if start >= 0 && end > start {
			result = result[:start] + result[end+1:]
		} else {
			break
		}
	}
	return strings.TrimSpace(result)
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func nullIfZero(n int) interface{} {
	if n == 0 {
		return nil
	}
	return n
}

// ensureCampaignColumns adds any missing columns and drops restrictive constraints
func (cb *CampaignBuilder) ensureCampaignColumns(ctx context.Context) {
	// Drop restrictive CHECK constraints that block application-level values.
	// PostgreSQL may auto-name inline constraints as tablename_colname_check,
	// with numeric suffixes if recreated.
	constraints := []string{
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check1`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_campaign_type_check`,
	}
	for _, ddl := range constraints {
		cb.db.ExecContext(ctx, ddl)
	}

	migrations := []string{
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS list_ids JSONB DEFAULT '[]'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_list_ids JSONB DEFAULT '[]'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS suppression_segment_ids JSONB DEFAULT '[]'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS esp_quotas JSONB DEFAULT '[]'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS throttle_rate_per_minute INTEGER`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS throttle_duration_hours INTEGER`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS sending_profile_id UUID`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS send_type VARCHAR(20) DEFAULT 'blast'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS max_recipients INTEGER DEFAULT 0`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS throttle_speed VARCHAR(30) DEFAULT 'gentle'`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS preview_text VARCHAR(255)`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS plain_content TEXT`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS everflow_creative_id INTEGER`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS everflow_offer_id INTEGER`,
		`ALTER TABLE mailing_campaigns ADD COLUMN IF NOT EXISTS tracking_link_template TEXT`,
	}
	
	for _, migration := range migrations {
		if _, err := cb.db.ExecContext(ctx, migration); err != nil {
			log.Printf("[CampaignBuilder] Migration failed: %s: %v", migration[:60], err)
		}
	}

	// Re-add status constraint with the full set of valid values
	cb.db.ExecContext(ctx, `
		ALTER TABLE mailing_campaigns 
		ADD CONSTRAINT mailing_campaigns_status_check 
		CHECK (status IN ('draft','scheduled','preparing','sending','paused','completed','completed_with_errors','cancelled','failed','deleted','sent'))
	`)
}

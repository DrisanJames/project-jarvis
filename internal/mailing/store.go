package mailing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store provides database operations for mailing entities
type Store struct {
	db *sql.DB
}

// NewStore creates a new mailing store
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// HashEmail creates a SHA256 hash of an email address
func HashEmail(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}

// CreateList creates a new mailing list
func (s *Store) CreateList(ctx context.Context, list *List) error {
	list.ID = uuid.New()
	list.CreatedAt = time.Now()
	list.UpdatedAt = time.Now()
	if list.Status == "" {
		list.Status = StatusActive
	}

	query := `INSERT INTO mailing_lists (id, organization_id, name, description, default_from_name, 
		default_from_email, default_reply_to, opt_in_type, status, settings, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	_, err := s.db.ExecContext(ctx, query, list.ID, list.OrganizationID, list.Name, list.Description,
		list.DefaultFromName, list.DefaultFromEmail, list.DefaultReplyTo, list.OptInType, 
		list.Status, list.Settings, list.CreatedAt, list.UpdatedAt)
	return err
}

// GetList retrieves a list by ID
func (s *Store) GetList(ctx context.Context, orgID, listID uuid.UUID) (*List, error) {
	query := `SELECT id, organization_id, name, description, default_from_name, default_from_email,
		default_reply_to, subscriber_count, active_count, opt_in_type, status, settings, created_at, updated_at
		FROM mailing_lists WHERE id = $1 AND organization_id = $2`

	list := &List{}
	err := s.db.QueryRowContext(ctx, query, listID, orgID).Scan(
		&list.ID, &list.OrganizationID, &list.Name, &list.Description,
		&list.DefaultFromName, &list.DefaultFromEmail, &list.DefaultReplyTo,
		&list.SubscriberCount, &list.ActiveCount, &list.OptInType, &list.Status,
		&list.Settings, &list.CreatedAt, &list.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return list, err
}

// GetLists retrieves all lists for an organization
func (s *Store) GetLists(ctx context.Context, orgID uuid.UUID) ([]*List, error) {
	query := `SELECT id, organization_id, name, description, subscriber_count, active_count, status, created_at
		FROM mailing_lists WHERE organization_id = $1 AND status = 'active' ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lists []*List
	for rows.Next() {
		list := &List{}
		err := rows.Scan(&list.ID, &list.OrganizationID, &list.Name, &list.Description,
			&list.SubscriberCount, &list.ActiveCount, &list.Status, &list.CreatedAt)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}
	return lists, nil
}

// CreateSubscriber creates a new subscriber
func (s *Store) CreateSubscriber(ctx context.Context, sub *Subscriber) error {
	sub.ID = uuid.New()
	sub.EmailHash = HashEmail(sub.Email)
	sub.Email = strings.ToLower(strings.TrimSpace(sub.Email))
	sub.CreatedAt = time.Now()
	sub.UpdatedAt = time.Now()
	sub.SubscribedAt = time.Now()
	if sub.Status == "" {
		sub.Status = SubscriberConfirmed
	}
	if sub.EngagementScore == 0 {
		sub.EngagementScore = 50.0
	}

	query := `INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash,
		first_name, last_name, status, source, ip_address, custom_fields, engagement_score, 
		subscribed_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (list_id, email) DO UPDATE SET first_name = EXCLUDED.first_name,
		last_name = EXCLUDED.last_name, updated_at = NOW()`

	_, err := s.db.ExecContext(ctx, query, sub.ID, sub.OrganizationID, sub.ListID, sub.Email, 
		sub.EmailHash, sub.FirstName, sub.LastName, sub.Status, sub.Source, sub.IPAddress,
		sub.CustomFields, sub.EngagementScore, sub.SubscribedAt, sub.CreatedAt, sub.UpdatedAt)
	return err
}

// GetSubscriber retrieves a subscriber by ID
func (s *Store) GetSubscriber(ctx context.Context, subID uuid.UUID) (*Subscriber, error) {
	query := `SELECT id, organization_id, list_id, email, first_name, last_name, status,
		engagement_score, total_emails_received, total_opens, total_clicks, last_open_at,
		last_click_at, optimal_send_hour_utc, timezone, subscribed_at, created_at
		FROM mailing_subscribers WHERE id = $1`

	sub := &Subscriber{}
	err := s.db.QueryRowContext(ctx, query, subID).Scan(
		&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.FirstName, &sub.LastName,
		&sub.Status, &sub.EngagementScore, &sub.TotalEmailsReceived, &sub.TotalOpens,
		&sub.TotalClicks, &sub.LastOpenAt, &sub.LastClickAt, &sub.OptimalSendHourUTC,
		&sub.Timezone, &sub.SubscribedAt, &sub.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sub, err
}

// GetSubscribers retrieves subscribers for a list
func (s *Store) GetSubscribers(ctx context.Context, listID uuid.UUID, limit, offset int) ([]*Subscriber, int, error) {
	var total int
	countQuery := `SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`
	s.db.QueryRowContext(ctx, countQuery, listID).Scan(&total)

	query := `SELECT id, organization_id, list_id, email, first_name, last_name, status,
		engagement_score, total_opens, total_clicks, subscribed_at
		FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'
		ORDER BY subscribed_at DESC LIMIT $2 OFFSET $3`

	rows, err := s.db.QueryContext(ctx, query, listID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var subs []*Subscriber
	for rows.Next() {
		sub := &Subscriber{}
		err := rows.Scan(&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.FirstName,
			&sub.LastName, &sub.Status, &sub.EngagementScore, &sub.TotalOpens, &sub.TotalClicks, &sub.SubscribedAt)
		if err != nil {
			return nil, 0, err
		}
		subs = append(subs, sub)
	}
	return subs, total, nil
}

// GetActiveSubscribersForCampaign retrieves active subscribers for sending
func (s *Store) GetActiveSubscribersForCampaign(ctx context.Context, listID uuid.UUID) ([]*Subscriber, error) {
	query := `SELECT id, organization_id, list_id, email, first_name, last_name,
		engagement_score, optimal_send_hour_utc, timezone
		FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'
		ORDER BY engagement_score DESC`

	rows, err := s.db.QueryContext(ctx, query, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*Subscriber
	for rows.Next() {
		sub := &Subscriber{}
		err := rows.Scan(&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.FirstName,
			&sub.LastName, &sub.EngagementScore, &sub.OptimalSendHourUTC, &sub.Timezone)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// GetActiveSubscribersForSegment retrieves subscribers matching segment conditions
func (s *Store) GetActiveSubscribersForSegment(ctx context.Context, segmentID uuid.UUID) ([]*Subscriber, error) {
	// First get the segment's list_id and conditions
	var listID uuid.UUID
	err := s.db.QueryRowContext(ctx, `SELECT list_id FROM mailing_segments WHERE id = $1`, segmentID).Scan(&listID)
	if err != nil {
		return nil, fmt.Errorf("segment not found: %w", err)
	}
	
	// Get segment conditions
	rows, err := s.db.QueryContext(ctx, `
		SELECT field, operator, value FROM mailing_segment_conditions WHERE segment_id = $1 ORDER BY condition_group, id
	`, segmentID)
	if err != nil {
		return nil, err
	}
	
	type condition struct {
		Field, Operator, Value string
	}
	var conditions []condition
	for rows.Next() {
		var c condition
		rows.Scan(&c.Field, &c.Operator, &c.Value)
		conditions = append(conditions, c)
	}
	rows.Close()
	
	// Build dynamic WHERE clause
	query := `SELECT id, organization_id, list_id, email, first_name, last_name,
		engagement_score, optimal_send_hour_utc, timezone
		FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'`
	
	args := []interface{}{listID}
	argNum := 2
	
	for _, cond := range conditions {
		var sqlOp string
		switch cond.Operator {
		case "equals":
			sqlOp = fmt.Sprintf(" AND %s = $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		case "not_equals":
			sqlOp = fmt.Sprintf(" AND %s != $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		case "contains":
			sqlOp = fmt.Sprintf(" AND %s ILIKE $%d", cond.Field, argNum)
			args = append(args, "%"+cond.Value+"%")
		case "starts_with":
			sqlOp = fmt.Sprintf(" AND %s ILIKE $%d", cond.Field, argNum)
			args = append(args, cond.Value+"%")
		case "gt":
			sqlOp = fmt.Sprintf(" AND %s > $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		case "gte":
			sqlOp = fmt.Sprintf(" AND %s >= $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		case "lt":
			sqlOp = fmt.Sprintf(" AND %s < $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		case "lte":
			sqlOp = fmt.Sprintf(" AND %s <= $%d", cond.Field, argNum)
			args = append(args, cond.Value)
		default:
			continue
		}
		query += sqlOp
		argNum++
	}
	
	query += " ORDER BY engagement_score DESC"
	
	subRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer subRows.Close()

	var subs []*Subscriber
	for subRows.Next() {
		sub := &Subscriber{}
		err := subRows.Scan(&sub.ID, &sub.OrganizationID, &sub.ListID, &sub.Email, &sub.FirstName,
			&sub.LastName, &sub.EngagementScore, &sub.OptimalSendHourUTC, &sub.Timezone)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// UpdateSubscriberEngagement updates engagement metrics
func (s *Store) UpdateSubscriberEngagement(ctx context.Context, subID uuid.UUID, eventType string) error {
	var query string
	switch eventType {
	case EventOpened:
		query = `UPDATE mailing_subscribers SET total_opens = total_opens + 1, last_open_at = NOW(), updated_at = NOW() WHERE id = $1`
	case EventClicked:
		query = `UPDATE mailing_subscribers SET total_clicks = total_clicks + 1, last_click_at = NOW(), updated_at = NOW() WHERE id = $1`
	case EventSent:
		query = `UPDATE mailing_subscribers SET total_emails_received = total_emails_received + 1, last_email_at = NOW(), updated_at = NOW() WHERE id = $1`
	default:
		return nil
	}
	_, err := s.db.ExecContext(ctx, query, subID)
	return err
}

// CreateCampaign creates a new campaign
func (s *Store) CreateCampaign(ctx context.Context, c *Campaign) error {
	c.ID = uuid.New()
	c.CreatedAt = time.Now()
	c.UpdatedAt = time.Now()
	if c.Status == "" {
		c.Status = StatusDraft
	}

	// NOTE: We write to BOTH send_at AND scheduled_at for compatibility
	// - send_at is used by legacy code and the Campaign struct
	// - scheduled_at is used by campaign_builder.go and the campaign scheduler
	query := `INSERT INTO mailing_campaigns (id, organization_id, list_id, template_id, segment_id,
		name, campaign_type, subject, from_name, from_email, reply_to, html_content, plain_content,
		preview_text, delivery_server_id, send_at, scheduled_at, timezone, ai_send_time_optimization,
		ai_content_optimization, ai_audience_optimization, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`

	_, err := s.db.ExecContext(ctx, query, c.ID, c.OrganizationID, c.ListID, c.TemplateID, c.SegmentID,
		c.Name, c.CampaignType, c.Subject, c.FromName, c.FromEmail, c.ReplyTo, c.HTMLContent,
		c.PlainContent, c.PreviewText, c.DeliveryServerID, c.SendAt, c.SendAt, c.Timezone,
		c.AISendTimeOptimization, c.AIContentOptimization, c.AIAudienceOptimization,
		c.Status, c.CreatedAt, c.UpdatedAt)
	return err
}

// GetCampaign retrieves a campaign by ID
func (s *Store) GetCampaign(ctx context.Context, orgID, campaignID uuid.UUID) (*Campaign, error) {
	query := `SELECT id, organization_id, list_id, template_id, segment_id, name, campaign_type,
		subject, from_name, from_email, reply_to, html_content, plain_content, preview_text,
		delivery_server_id, send_at, timezone, ai_send_time_optimization, ai_content_optimization,
		ai_audience_optimization, status, total_recipients, sent_count, delivered_count, open_count,
		unique_open_count, click_count, unique_click_count, bounce_count, complaint_count,
		unsubscribe_count, revenue, started_at, completed_at, created_at, updated_at
		FROM mailing_campaigns WHERE id = $1 AND organization_id = $2`

	c := &Campaign{}
	err := s.db.QueryRowContext(ctx, query, campaignID, orgID).Scan(
		&c.ID, &c.OrganizationID, &c.ListID, &c.TemplateID, &c.SegmentID, &c.Name, &c.CampaignType,
		&c.Subject, &c.FromName, &c.FromEmail, &c.ReplyTo, &c.HTMLContent, &c.PlainContent,
		&c.PreviewText, &c.DeliveryServerID, &c.SendAt, &c.Timezone, &c.AISendTimeOptimization,
		&c.AIContentOptimization, &c.AIAudienceOptimization, &c.Status, &c.TotalRecipients,
		&c.SentCount, &c.DeliveredCount, &c.OpenCount, &c.UniqueOpenCount, &c.ClickCount,
		&c.UniqueClickCount, &c.BounceCount, &c.ComplaintCount, &c.UnsubscribeCount, &c.Revenue,
		&c.StartedAt, &c.CompletedAt, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetCampaigns retrieves campaigns for an organization
func (s *Store) GetCampaigns(ctx context.Context, orgID uuid.UUID, limit int) ([]*Campaign, error) {
	query := `SELECT id, organization_id, list_id, name, campaign_type, subject, from_name,
		from_email, status, total_recipients, sent_count, open_count, click_count, bounce_count,
		revenue, send_at, started_at, completed_at, created_at
		FROM mailing_campaigns WHERE organization_id = $1 ORDER BY created_at DESC`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var campaigns []*Campaign
	for rows.Next() {
		c := &Campaign{}
		err := rows.Scan(&c.ID, &c.OrganizationID, &c.ListID, &c.Name, &c.CampaignType, &c.Subject,
			&c.FromName, &c.FromEmail, &c.Status, &c.TotalRecipients, &c.SentCount, &c.OpenCount,
			&c.ClickCount, &c.BounceCount, &c.Revenue, &c.SendAt, &c.StartedAt, &c.CompletedAt, &c.CreatedAt)
		if err != nil {
			return nil, err
		}
		campaigns = append(campaigns, c)
	}
	return campaigns, nil
}

// UpdateCampaignStatus updates campaign status
func (s *Store) UpdateCampaignStatus(ctx context.Context, campaignID uuid.UUID, status string) error {
	query := `UPDATE mailing_campaigns SET status = $1, updated_at = NOW()`
	if status == StatusSending {
		query += ", started_at = NOW()"
	} else if status == StatusSent {
		query += ", completed_at = NOW()"
	}
	query += " WHERE id = $2"
	_, err := s.db.ExecContext(ctx, query, status, campaignID)
	return err
}

// UpdateCampaignStats updates campaign statistics
func (s *Store) UpdateCampaignStats(ctx context.Context, campaignID uuid.UUID, field string, increment int) error {
	query := fmt.Sprintf("UPDATE mailing_campaigns SET %s = %s + $1, updated_at = NOW() WHERE id = $2", field, field)
	_, err := s.db.ExecContext(ctx, query, increment, campaignID)
	return err
}

// GetDeliveryServers retrieves active delivery servers
func (s *Store) GetDeliveryServers(ctx context.Context, orgID uuid.UUID) ([]*DeliveryServer, error) {
	query := `SELECT id, organization_id, name, server_type, region, hourly_quota, daily_quota,
		monthly_quota, used_hourly, used_daily, used_monthly, probability, priority,
		warmup_enabled, warmup_stage, status, reputation_score, created_at
		FROM mailing_delivery_servers WHERE organization_id = $1 AND status IN ('active', 'warmup')
		ORDER BY priority, probability DESC`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []*DeliveryServer
	for rows.Next() {
		ds := &DeliveryServer{}
		err := rows.Scan(&ds.ID, &ds.OrganizationID, &ds.Name, &ds.ServerType, &ds.Region,
			&ds.HourlyQuota, &ds.DailyQuota, &ds.MonthlyQuota, &ds.UsedHourly, &ds.UsedDaily,
			&ds.UsedMonthly, &ds.Probability, &ds.Priority, &ds.WarmupEnabled, &ds.WarmupStage,
			&ds.Status, &ds.ReputationScore, &ds.CreatedAt)
		if err != nil {
			return nil, err
		}
		servers = append(servers, ds)
	}
	return servers, nil
}

// GetAvailableServer selects best server for sending
func (s *Store) GetAvailableServer(ctx context.Context, orgID uuid.UUID) (*DeliveryServer, error) {
	query := `SELECT id, organization_id, name, server_type, region, hourly_quota, daily_quota,
		used_hourly, used_daily, probability, priority, status, reputation_score
		FROM mailing_delivery_servers
		WHERE organization_id = $1 AND status IN ('active', 'warmup')
			AND used_hourly < hourly_quota AND used_daily < daily_quota
		ORDER BY priority, (RANDOM() * probability) DESC LIMIT 1`

	ds := &DeliveryServer{}
	err := s.db.QueryRowContext(ctx, query, orgID).Scan(&ds.ID, &ds.OrganizationID, &ds.Name,
		&ds.ServerType, &ds.Region, &ds.HourlyQuota, &ds.DailyQuota, &ds.UsedHourly,
		&ds.UsedDaily, &ds.Probability, &ds.Priority, &ds.Status, &ds.ReputationScore)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ds, err
}

// IncrementServerUsage increments usage counters
func (s *Store) IncrementServerUsage(ctx context.Context, serverID uuid.UUID) error {
	query := `UPDATE mailing_delivery_servers SET used_hourly = used_hourly + 1, 
		used_daily = used_daily + 1, used_monthly = used_monthly + 1 WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, serverID)
	return err
}

// EnqueueEmails adds emails to the send queue
func (s *Store) EnqueueEmails(ctx context.Context, items []*QueueItem) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO mailing_campaign_queue 
		(id, campaign_id, subscriber_id, subject, html_content, plain_content, scheduled_at, 
		priority, predicted_open_prob, predicted_revenue, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		item.ID = uuid.New()
		item.CreatedAt = time.Now()
		if item.Status == "" {
			item.Status = StatusQueued
		}
		_, err := stmt.ExecContext(ctx, item.ID, item.CampaignID, item.SubscriberID, item.Subject,
			item.HTMLContent, item.PlainContent, item.ScheduledAt, item.Priority,
			item.PredictedOpenProb, item.PredictedRevenue, item.Status, item.CreatedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetQueuedEmails retrieves emails ready to send
func (s *Store) GetQueuedEmails(ctx context.Context, limit int) ([]*QueueItem, error) {
	query := `SELECT q.id, q.campaign_id, q.subscriber_id, q.subject, q.html_content,
		q.plain_content, q.scheduled_at, q.priority, q.status
		FROM mailing_campaign_queue q
		JOIN mailing_campaigns c ON q.campaign_id = c.id
		WHERE q.status = 'queued' AND q.scheduled_at <= NOW() AND c.status = 'sending'
		ORDER BY q.priority DESC, q.scheduled_at LIMIT $1 FOR UPDATE SKIP LOCKED`

	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*QueueItem
	for rows.Next() {
		item := &QueueItem{}
		err := rows.Scan(&item.ID, &item.CampaignID, &item.SubscriberID, &item.Subject,
			&item.HTMLContent, &item.PlainContent, &item.ScheduledAt, &item.Priority, &item.Status)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// UpdateQueueItemStatus updates a queue item status
func (s *Store) UpdateQueueItemStatus(ctx context.Context, itemID uuid.UUID, status, messageID, errorMsg string, serverID *uuid.UUID) error {
	query := `UPDATE mailing_campaign_queue SET status = $1, message_id = $2, error_message = $3,
		delivery_server_id = $4, attempts = attempts + 1, last_attempt_at = NOW()`
	if status == StatusSent {
		query += ", sent_at = NOW()"
	}
	query += " WHERE id = $5"
	_, err := s.db.ExecContext(ctx, query, status, messageID, errorMsg, serverID, itemID)
	return err
}

// RecordTrackingEvent records a tracking event
func (s *Store) RecordTrackingEvent(ctx context.Context, event *TrackingEvent) error {
	event.ID = uuid.New()
	event.CreatedAt = time.Now()
	if event.EventAt.IsZero() {
		event.EventAt = time.Now()
	}

	query := `INSERT INTO mailing_tracking_events (id, organization_id, campaign_id, subscriber_id,
		email_id, event_type, ip_address, user_agent, device_type, link_url, bounce_type, 
		bounce_reason, event_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	_, err := s.db.ExecContext(ctx, query, event.ID, event.OrganizationID, event.CampaignID,
		event.SubscriberID, event.EmailID, event.EventType, event.IPAddress, event.UserAgent,
		event.DeviceType, event.LinkURL, event.BounceType, event.BounceReason,
		event.EventAt, event.CreatedAt)
	return err
}

// UpdateSubscriberQuality bumps a subscriber's data_quality_score to at least
// the given value. Uses GREATEST to never decrease quality.
func (s *Store) UpdateSubscriberQuality(ctx context.Context, subscriberID uuid.UUID, score float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mailing_subscribers SET data_quality_score = GREATEST(data_quality_score, $1), updated_at = NOW()
		WHERE id = $2`, score, subscriberID)
	return err
}

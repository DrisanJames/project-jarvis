// Package mailing provides RSS feed campaign functionality for automated
// email campaigns generated from RSS/Atom feeds.
package mailing

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mmcdole/gofeed"
)

// RSSCampaignService handles RSS feed campaign operations
type RSSCampaignService struct {
	db          *sql.DB
	campaignSvc *Store
	feedParser  *gofeed.Parser
	templateSvc *TemplateService
}

// RSSCampaign represents an RSS-to-email campaign configuration
type RSSCampaign struct {
	ID               string     `json:"id"`
	OrgID            string     `json:"org_id"`
	Name             string     `json:"name"`
	FeedURL          string     `json:"feed_url"`
	TemplateID       string     `json:"template_id"`
	ListID           string     `json:"list_id"`
	SegmentID        *string    `json:"segment_id,omitempty"`
	SendingProfileID string     `json:"sending_profile_id"`
	PollInterval     string     `json:"poll_interval"` // hourly, daily, weekly
	LastPolledAt     *time.Time `json:"last_polled_at,omitempty"`
	LastItemGUID     *string    `json:"last_item_guid,omitempty"`
	Active           bool       `json:"active"`
	AutoSend         bool       `json:"auto_send"`
	MaxItemsPerPoll  int        `json:"max_items_per_poll"`
	SubjectTemplate  string     `json:"subject_template"`
	FromName         string     `json:"from_name,omitempty"`
	FromEmail        string     `json:"from_email,omitempty"`
	ReplyTo          string     `json:"reply_to,omitempty"`
	ErrorCount       int        `json:"error_count"`
	LastError        *string    `json:"last_error,omitempty"`
	LastErrorAt      *time.Time `json:"last_error_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// FeedItem represents a single item from an RSS feed
type FeedItem struct {
	GUID        string    `json:"guid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Link        string    `json:"link"`
	PubDate     time.Time `json:"pub_date"`
	ImageURL    string    `json:"image_url,omitempty"`
	Author      string    `json:"author,omitempty"`
	Categories  []string  `json:"categories,omitempty"`
	Content     string    `json:"content,omitempty"`
}

// RSSSentItem tracks items that have been processed
type RSSSentItem struct {
	ID            string     `json:"id"`
	RSSCampaignID string     `json:"rss_campaign_id"`
	ItemGUID      string     `json:"item_guid"`
	ItemTitle     string     `json:"item_title"`
	ItemLink      string     `json:"item_link"`
	ItemPubDate   *time.Time `json:"item_pub_date,omitempty"`
	CampaignID    *string    `json:"campaign_id,omitempty"`
	Status        string     `json:"status"`
	ErrorMessage  *string    `json:"error_message,omitempty"`
	DiscoveredAt  time.Time  `json:"discovered_at"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

// RSSPollLog records polling activity
type RSSPollLog struct {
	ID                 string    `json:"id"`
	RSSCampaignID      string    `json:"rss_campaign_id"`
	ItemsFound         int       `json:"items_found"`
	NewItems           int       `json:"new_items"`
	CampaignsGenerated int       `json:"campaigns_generated"`
	Status             string    `json:"status"`
	ErrorMessage       *string   `json:"error_message,omitempty"`
	DurationMs         int       `json:"duration_ms"`
	PolledAt           time.Time `json:"polled_at"`
}

// NewRSSCampaignService creates a new RSS campaign service
func NewRSSCampaignService(db *sql.DB, campaignSvc *Store) *RSSCampaignService {
	return &RSSCampaignService{
		db:          db,
		campaignSvc: campaignSvc,
		feedParser:  gofeed.NewParser(),
		templateSvc: NewTemplateService(),
	}
}

// CreateRSSCampaign creates a new RSS campaign configuration
func (s *RSSCampaignService) CreateRSSCampaign(ctx context.Context, config RSSCampaign) (*RSSCampaign, error) {
	// Validate the feed URL by attempting to fetch it
	if _, err := s.fetchFeed(ctx, config.FeedURL); err != nil {
		return nil, fmt.Errorf("invalid feed URL: %w", err)
	}

	config.ID = uuid.New().String()
	config.CreatedAt = time.Now()
	config.UpdatedAt = time.Now()

	if config.PollInterval == "" {
		config.PollInterval = "daily"
	}
	if config.MaxItemsPerPoll == 0 {
		config.MaxItemsPerPoll = 5
	}
	if config.SubjectTemplate == "" {
		config.SubjectTemplate = "{{rss.title}}"
	}

	query := `
		INSERT INTO mailing_rss_campaigns (
			id, org_id, name, feed_url, template_id, list_id, segment_id,
			sending_profile_id, poll_interval, auto_send, max_items_per_poll,
			subject_template, from_name, from_email, reply_to, active,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18
		)`

	_, err := s.db.ExecContext(ctx, query,
		config.ID, config.OrgID, config.Name, config.FeedURL,
		nullString(config.TemplateID), nullString(config.ListID), config.SegmentID,
		nullString(config.SendingProfileID), config.PollInterval, config.AutoSend,
		config.MaxItemsPerPoll, config.SubjectTemplate, config.FromName, config.FromEmail,
		config.ReplyTo, config.Active, config.CreatedAt, config.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create RSS campaign: %w", err)
	}

	return &config, nil
}

// GetRSSCampaigns retrieves all RSS campaigns for an organization
func (s *RSSCampaignService) GetRSSCampaigns(ctx context.Context, orgID string) ([]RSSCampaign, error) {
	query := `
		SELECT id, org_id, name, feed_url, COALESCE(template_id::text, ''),
			   COALESCE(list_id::text, ''), segment_id, COALESCE(sending_profile_id::text, ''),
			   poll_interval, last_polled_at, last_item_guid, active, auto_send,
			   max_items_per_poll, subject_template, COALESCE(from_name, ''),
			   COALESCE(from_email, ''), COALESCE(reply_to, ''), error_count,
			   last_error, last_error_at, created_at, updated_at
		FROM mailing_rss_campaigns
		WHERE org_id = $1
		ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to query RSS campaigns: %w", err)
	}
	defer rows.Close()

	var campaigns []RSSCampaign
	for rows.Next() {
		var c RSSCampaign
		err := rows.Scan(
			&c.ID, &c.OrgID, &c.Name, &c.FeedURL, &c.TemplateID, &c.ListID,
			&c.SegmentID, &c.SendingProfileID, &c.PollInterval, &c.LastPolledAt,
			&c.LastItemGUID, &c.Active, &c.AutoSend, &c.MaxItemsPerPoll,
			&c.SubjectTemplate, &c.FromName, &c.FromEmail, &c.ReplyTo,
			&c.ErrorCount, &c.LastError, &c.LastErrorAt, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan RSS campaign: %w", err)
		}
		campaigns = append(campaigns, c)
	}

	if campaigns == nil {
		campaigns = []RSSCampaign{}
	}

	return campaigns, nil
}

// GetRSSCampaign retrieves a single RSS campaign by ID
func (s *RSSCampaignService) GetRSSCampaign(ctx context.Context, id string) (*RSSCampaign, error) {
	query := `
		SELECT id, org_id, name, feed_url, COALESCE(template_id::text, ''),
			   COALESCE(list_id::text, ''), segment_id, COALESCE(sending_profile_id::text, ''),
			   poll_interval, last_polled_at, last_item_guid, active, auto_send,
			   max_items_per_poll, subject_template, COALESCE(from_name, ''),
			   COALESCE(from_email, ''), COALESCE(reply_to, ''), error_count,
			   last_error, last_error_at, created_at, updated_at
		FROM mailing_rss_campaigns
		WHERE id = $1`

	var c RSSCampaign
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&c.ID, &c.OrgID, &c.Name, &c.FeedURL, &c.TemplateID, &c.ListID,
		&c.SegmentID, &c.SendingProfileID, &c.PollInterval, &c.LastPolledAt,
		&c.LastItemGUID, &c.Active, &c.AutoSend, &c.MaxItemsPerPoll,
		&c.SubjectTemplate, &c.FromName, &c.FromEmail, &c.ReplyTo,
		&c.ErrorCount, &c.LastError, &c.LastErrorAt, &c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get RSS campaign: %w", err)
	}

	return &c, nil
}

// UpdateRSSCampaign updates an existing RSS campaign
func (s *RSSCampaignService) UpdateRSSCampaign(ctx context.Context, campaign RSSCampaign) error {
	// Validate feed URL if changed
	existing, err := s.GetRSSCampaign(ctx, campaign.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("RSS campaign not found")
	}

	if existing.FeedURL != campaign.FeedURL {
		if _, err := s.fetchFeed(ctx, campaign.FeedURL); err != nil {
			return fmt.Errorf("invalid feed URL: %w", err)
		}
	}

	query := `
		UPDATE mailing_rss_campaigns SET
			name = $2, feed_url = $3, template_id = $4, list_id = $5,
			segment_id = $6, sending_profile_id = $7, poll_interval = $8,
			auto_send = $9, max_items_per_poll = $10, subject_template = $11,
			from_name = $12, from_email = $13, reply_to = $14, active = $15,
			updated_at = NOW()
		WHERE id = $1`

	_, err = s.db.ExecContext(ctx, query,
		campaign.ID, campaign.Name, campaign.FeedURL,
		nullString(campaign.TemplateID), nullString(campaign.ListID),
		campaign.SegmentID, nullString(campaign.SendingProfileID),
		campaign.PollInterval, campaign.AutoSend, campaign.MaxItemsPerPoll,
		campaign.SubjectTemplate, campaign.FromName, campaign.FromEmail,
		campaign.ReplyTo, campaign.Active,
	)
	if err != nil {
		return fmt.Errorf("failed to update RSS campaign: %w", err)
	}

	return nil
}

// DeleteRSSCampaign deletes an RSS campaign and its related data
func (s *RSSCampaignService) DeleteRSSCampaign(ctx context.Context, id string) error {
	// Delete cascades to sent_items and poll_log via foreign key
	_, err := s.db.ExecContext(ctx, `DELETE FROM mailing_rss_campaigns WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete RSS campaign: %w", err)
	}
	return nil
}

// PollFeed fetches the RSS feed and returns new items
func (s *RSSCampaignService) PollFeed(ctx context.Context, campaignID string) ([]FeedItem, error) {
	campaign, err := s.GetRSSCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, fmt.Errorf("RSS campaign not found")
	}

	startTime := time.Now()

	// Fetch and parse the feed
	feed, err := s.fetchFeed(ctx, campaign.FeedURL)
	if err != nil {
		s.recordPollError(ctx, campaignID, err)
		return nil, fmt.Errorf("failed to fetch feed: %w", err)
	}

	// Convert feed items to our FeedItem struct
	var items []FeedItem
	for _, item := range feed.Items {
		feedItem := s.parseFeedItem(item)

		// Check if this item has already been processed
		exists, err := s.itemExists(ctx, campaignID, feedItem.GUID)
		if err != nil {
			continue
		}
		if !exists {
			items = append(items, feedItem)
		}
	}

	// Limit to max items per poll
	if len(items) > campaign.MaxItemsPerPoll {
		items = items[:campaign.MaxItemsPerPoll]
	}

	// Update last polled time
	now := time.Now()
	var lastGUID *string
	if len(items) > 0 {
		lastGUID = &items[0].GUID
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_rss_campaigns 
		SET last_polled_at = $2, last_item_guid = COALESCE($3, last_item_guid), error_count = 0
		WHERE id = $1
	`, campaignID, now, lastGUID)
	if err != nil {
		// Log but don't fail
		fmt.Printf("Failed to update last_polled_at: %v\n", err)
	}

	// Record poll log
	duration := time.Since(startTime).Milliseconds()
	s.recordPollSuccess(ctx, campaignID, len(feed.Items), len(items), 0, int(duration))

	return items, nil
}

// GenerateCampaignFromFeed creates an email campaign from a feed item
func (s *RSSCampaignService) GenerateCampaignFromFeed(ctx context.Context, rssCampaignID string, item FeedItem) (*Campaign, error) {
	rssCampaign, err := s.GetRSSCampaign(ctx, rssCampaignID)
	if err != nil {
		return nil, err
	}
	if rssCampaign == nil {
		return nil, fmt.Errorf("RSS campaign not found")
	}

	// Get the template
	var htmlContent, plainContent, templateSubject string
	if rssCampaign.TemplateID != "" {
		err = s.db.QueryRowContext(ctx, `
			SELECT COALESCE(html_content, ''), COALESCE(plain_content, ''), COALESCE(subject, '')
			FROM mailing_templates WHERE id = $1
		`, rssCampaign.TemplateID).Scan(&htmlContent, &plainContent, &templateSubject)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("failed to get template: %w", err)
		}
	}

	// Build RSS merge tag context
	rssContext := map[string]interface{}{
		"rss": map[string]interface{}{
			"title":       item.Title,
			"description": item.Description,
			"link":        item.Link,
			"image":       item.ImageURL,
			"author":      item.Author,
			"date":        item.PubDate.Format("January 2, 2006"),
			"content":     item.Content,
			"categories":  strings.Join(item.Categories, ", "),
		},
	}

	// Render subject
	subject := rssCampaign.SubjectTemplate
	if subject == "" {
		subject = templateSubject
	}
	if subject == "" {
		subject = item.Title
	}
	renderedSubject, _ := s.templateSvc.Render("", subject, rssContext)

	// Render HTML content with RSS merge tags
	if htmlContent != "" {
		htmlContent, _ = s.templateSvc.Render("", htmlContent, rssContext)
	} else {
		// Default template if none specified
		htmlContent = s.generateDefaultRSSHTML(item)
	}

	// Render plain content
	if plainContent != "" {
		plainContent, _ = s.templateSvc.Render("", plainContent, rssContext)
	} else {
		plainContent = s.generateDefaultRSSPlain(item)
	}

	// Get from details
	fromName := rssCampaign.FromName
	fromEmail := rssCampaign.FromEmail
	replyTo := rssCampaign.ReplyTo

	// Fallback to list defaults
	if fromName == "" || fromEmail == "" {
		if rssCampaign.ListID != "" {
			var listFromName, listFromEmail, listReplyTo string
			s.db.QueryRowContext(ctx, `
				SELECT COALESCE(default_from_name, ''), COALESCE(default_from_email, ''), COALESCE(default_reply_to, '')
				FROM mailing_lists WHERE id = $1
			`, rssCampaign.ListID).Scan(&listFromName, &listFromEmail, &listReplyTo)
			if fromName == "" {
				fromName = listFromName
			}
			if fromEmail == "" {
				fromEmail = listFromEmail
			}
			if replyTo == "" {
				replyTo = listReplyTo
			}
		}
	}

	// Create the campaign
	campaign := &Campaign{
		OrganizationID: uuid.MustParse(rssCampaign.OrgID),
		Name:           fmt.Sprintf("RSS: %s", item.Title),
		CampaignType:   "rss",
		Subject:        renderedSubject,
		FromName:       fromName,
		FromEmail:      fromEmail,
		ReplyTo:        replyTo,
		HTMLContent:    htmlContent,
		PlainContent:   plainContent,
		Status:         StatusDraft,
	}

	if rssCampaign.ListID != "" {
		listID := uuid.MustParse(rssCampaign.ListID)
		campaign.ListID = &listID
	}
	if rssCampaign.TemplateID != "" {
		templateID := uuid.MustParse(rssCampaign.TemplateID)
		campaign.TemplateID = &templateID
	}
	if rssCampaign.SegmentID != nil && *rssCampaign.SegmentID != "" {
		segmentID := uuid.MustParse(*rssCampaign.SegmentID)
		campaign.SegmentID = &segmentID
	}

	// Save campaign
	err = s.campaignSvc.CreateCampaign(ctx, campaign)
	if err != nil {
		return nil, fmt.Errorf("failed to create campaign: %w", err)
	}

	// Record sent item
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_rss_sent_items (
			rss_campaign_id, item_guid, item_title, item_link, item_pub_date,
			campaign_id, status, discovered_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'generated', NOW())
		ON CONFLICT (rss_campaign_id, item_guid) DO UPDATE SET
			campaign_id = $6, status = 'generated'
	`, rssCampaignID, item.GUID, item.Title, item.Link, item.PubDate, campaign.ID)
	if err != nil {
		// Log but don't fail - campaign was created
		fmt.Printf("Failed to record sent item: %v\n", err)
	}

	return campaign, nil
}

// PreviewNextItems returns the next items that would be processed
func (s *RSSCampaignService) PreviewNextItems(ctx context.Context, campaignID string, limit int) ([]FeedItem, error) {
	campaign, err := s.GetRSSCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, fmt.Errorf("RSS campaign not found")
	}

	feed, err := s.fetchFeed(ctx, campaign.FeedURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch feed: %w", err)
	}

	var items []FeedItem
	for _, item := range feed.Items {
		feedItem := s.parseFeedItem(item)

		// Check if already processed
		exists, _ := s.itemExists(ctx, campaignID, feedItem.GUID)
		if !exists {
			items = append(items, feedItem)
			if len(items) >= limit {
				break
			}
		}
	}

	return items, nil
}

// GetSentItems retrieves items that have been sent for a campaign
func (s *RSSCampaignService) GetSentItems(ctx context.Context, campaignID string, limit int) ([]RSSSentItem, error) {
	query := `
		SELECT id, rss_campaign_id, item_guid, COALESCE(item_title, ''),
			   COALESCE(item_link, ''), item_pub_date, campaign_id, status,
			   error_message, discovered_at, sent_at
		FROM mailing_rss_sent_items
		WHERE rss_campaign_id = $1
		ORDER BY discovered_at DESC
		LIMIT $2`

	rows, err := s.db.QueryContext(ctx, query, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []RSSSentItem
	for rows.Next() {
		var item RSSSentItem
		err := rows.Scan(
			&item.ID, &item.RSSCampaignID, &item.ItemGUID, &item.ItemTitle,
			&item.ItemLink, &item.ItemPubDate, &item.CampaignID, &item.Status,
			&item.ErrorMessage, &item.DiscoveredAt, &item.SentAt,
		)
		if err != nil {
			continue
		}
		items = append(items, item)
	}

	if items == nil {
		items = []RSSSentItem{}
	}

	return items, nil
}

// GetPollHistory retrieves polling history for a campaign
func (s *RSSCampaignService) GetPollHistory(ctx context.Context, campaignID string, limit int) ([]RSSPollLog, error) {
	query := `
		SELECT id, rss_campaign_id, items_found, new_items, campaigns_generated,
			   status, error_message, duration_ms, polled_at
		FROM mailing_rss_poll_log
		WHERE rss_campaign_id = $1
		ORDER BY polled_at DESC
		LIMIT $2`

	rows, err := s.db.QueryContext(ctx, query, campaignID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []RSSPollLog
	for rows.Next() {
		var log RSSPollLog
		err := rows.Scan(
			&log.ID, &log.RSSCampaignID, &log.ItemsFound, &log.NewItems,
			&log.CampaignsGenerated, &log.Status, &log.ErrorMessage,
			&log.DurationMs, &log.PolledAt,
		)
		if err != nil {
			continue
		}
		logs = append(logs, log)
	}

	if logs == nil {
		logs = []RSSPollLog{}
	}

	return logs, nil
}

// GetActiveRSSCampaignsDueForPoll returns campaigns that need polling
func (s *RSSCampaignService) GetActiveRSSCampaignsDueForPoll(ctx context.Context) ([]RSSCampaign, error) {
	query := `
		SELECT id, org_id, name, feed_url, COALESCE(template_id::text, ''),
			   COALESCE(list_id::text, ''), segment_id, COALESCE(sending_profile_id::text, ''),
			   poll_interval, last_polled_at, last_item_guid, active, auto_send,
			   max_items_per_poll, subject_template, COALESCE(from_name, ''),
			   COALESCE(from_email, ''), COALESCE(reply_to, ''), error_count,
			   last_error, last_error_at, created_at, updated_at
		FROM mailing_rss_campaigns
		WHERE active = true
		  AND (
			last_polled_at IS NULL
			OR (poll_interval = 'hourly' AND last_polled_at < NOW() - INTERVAL '1 hour')
			OR (poll_interval = 'daily' AND last_polled_at < NOW() - INTERVAL '1 day')
			OR (poll_interval = 'weekly' AND last_polled_at < NOW() - INTERVAL '1 week')
		  )
		ORDER BY last_polled_at ASC NULLS FIRST`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query due campaigns: %w", err)
	}
	defer rows.Close()

	var campaigns []RSSCampaign
	for rows.Next() {
		var c RSSCampaign
		err := rows.Scan(
			&c.ID, &c.OrgID, &c.Name, &c.FeedURL, &c.TemplateID, &c.ListID,
			&c.SegmentID, &c.SendingProfileID, &c.PollInterval, &c.LastPolledAt,
			&c.LastItemGUID, &c.Active, &c.AutoSend, &c.MaxItemsPerPoll,
			&c.SubjectTemplate, &c.FromName, &c.FromEmail, &c.ReplyTo,
			&c.ErrorCount, &c.LastError, &c.LastErrorAt, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			continue
		}
		campaigns = append(campaigns, c)
	}

	return campaigns, nil
}

// Helper methods

func (s *RSSCampaignService) fetchFeed(ctx context.Context, url string) (*gofeed.Feed, error) {
	// gofeed doesn't support context directly, but we can set a timeout
	feed, err := s.feedParser.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return feed, nil
}

func (s *RSSCampaignService) parseFeedItem(item *gofeed.Item) FeedItem {
	feedItem := FeedItem{
		GUID:        item.GUID,
		Title:       item.Title,
		Description: stripHTML(item.Description),
		Link:        item.Link,
		Content:     item.Content,
	}

	// Use link as GUID if none provided
	if feedItem.GUID == "" {
		feedItem.GUID = item.Link
	}

	// Parse publication date
	if item.PublishedParsed != nil {
		feedItem.PubDate = *item.PublishedParsed
	} else if item.UpdatedParsed != nil {
		feedItem.PubDate = *item.UpdatedParsed
	} else {
		feedItem.PubDate = time.Now()
	}

	// Extract image
	if item.Image != nil {
		feedItem.ImageURL = item.Image.URL
	} else if len(item.Enclosures) > 0 {
		for _, enc := range item.Enclosures {
			if strings.HasPrefix(enc.Type, "image/") {
				feedItem.ImageURL = enc.URL
				break
			}
		}
	}

	// Extract author
	if len(item.Authors) > 0 {
		feedItem.Author = item.Authors[0].Name
	} else if item.Author != nil {
		feedItem.Author = item.Author.Name
	}

	// Extract categories
	for _, cat := range item.Categories {
		feedItem.Categories = append(feedItem.Categories, cat)
	}

	return feedItem
}

func (s *RSSCampaignService) itemExists(ctx context.Context, campaignID, guid string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mailing_rss_sent_items WHERE rss_campaign_id = $1 AND item_guid = $2)
	`, campaignID, guid).Scan(&exists)
	return exists, err
}

func (s *RSSCampaignService) recordPollSuccess(ctx context.Context, campaignID string, itemsFound, newItems, generated, durationMs int) {
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_rss_poll_log (
			rss_campaign_id, items_found, new_items, campaigns_generated,
			status, duration_ms, polled_at
		) VALUES ($1, $2, $3, $4, 'success', $5, NOW())
	`, campaignID, itemsFound, newItems, generated, durationMs)
}

func (s *RSSCampaignService) recordPollError(ctx context.Context, campaignID string, err error) {
	errMsg := err.Error()

	// Update campaign error info
	s.db.ExecContext(ctx, `
		UPDATE mailing_rss_campaigns 
		SET error_count = error_count + 1, last_error = $2, last_error_at = NOW()
		WHERE id = $1
	`, campaignID, errMsg)

	// Record in poll log
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_rss_poll_log (
			rss_campaign_id, items_found, new_items, campaigns_generated,
			status, error_message, polled_at
		) VALUES ($1, 0, 0, 0, 'failed', $2, NOW())
	`, campaignID, errMsg)
}

func (s *RSSCampaignService) generateDefaultRSSHTML(item FeedItem) string {
	html := fmt.Sprintf(`
<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; line-height: 1.6; color: #333; max-width: 600px; margin: 0 auto; padding: 20px; }
        h1 { color: #1a1a1a; font-size: 24px; margin-bottom: 10px; }
        .meta { color: #666; font-size: 14px; margin-bottom: 20px; }
        .image { width: 100%%; max-width: 100%%; height: auto; border-radius: 8px; margin-bottom: 20px; }
        .content { font-size: 16px; }
        .cta { display: inline-block; background: #0066cc; color: white; padding: 12px 24px; text-decoration: none; border-radius: 6px; margin-top: 20px; }
        .cta:hover { background: #0052a3; }
    </style>
</head>
<body>
    <h1>%s</h1>
    <p class="meta">%s</p>
`, item.Title, item.PubDate.Format("January 2, 2006"))

	if item.ImageURL != "" {
		html += fmt.Sprintf(`    <img src="%s" alt="%s" class="image">
`, item.ImageURL, item.Title)
	}

	html += fmt.Sprintf(`    <div class="content">%s</div>
    <p><a href="%s" class="cta">Read More</a></p>
</body>
</html>`, item.Description, item.Link)

	return html
}

func (s *RSSCampaignService) generateDefaultRSSPlain(item FeedItem) string {
	return fmt.Sprintf(`%s

Published: %s

%s

Read more: %s
`, item.Title, item.PubDate.Format("January 2, 2006"), item.Description, item.Link)
}

// Helper functions

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// stripHTML removes HTML tags from a string
func stripHTML(input string) string {
	// Remove HTML tags
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(input, "")
	// Decode HTML entities
	text = html.UnescapeString(text)
	// Normalize whitespace
	text = strings.Join(strings.Fields(text), " ")
	return text
}

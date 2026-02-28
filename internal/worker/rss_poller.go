// Package worker provides the RSS feed poller for automated campaign generation.
package worker

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// RSSPoller handles background polling of RSS feeds
type RSSPoller struct {
	db     *sql.DB
	rssSvc *mailing.RSSCampaignService

	// Configuration
	pollInterval    time.Duration
	maxConcurrent   int
	enableAutoSend  bool

	// Stats
	totalPolls       int64
	totalItemsFound  int64
	totalCampaigns   int64
	totalErrors      int64

	// Control
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	running  bool
}

// RSSPollerConfig holds configuration for the RSS poller
type RSSPollerConfig struct {
	PollInterval    time.Duration // How often to check for feeds due for polling
	MaxConcurrent   int           // Maximum concurrent feed fetches
	EnableAutoSend  bool          // Whether to auto-send generated campaigns
}

// DefaultRSSPollerConfig returns default configuration
func DefaultRSSPollerConfig() RSSPollerConfig {
	return RSSPollerConfig{
		PollInterval:   5 * time.Minute, // Check every 5 minutes for due feeds
		MaxConcurrent:  5,               // Process up to 5 feeds concurrently
		EnableAutoSend: true,            // Auto-send if campaign is configured for it
	}
}

// NewRSSPoller creates a new RSS poller
func NewRSSPoller(db *sql.DB, rssSvc *mailing.RSSCampaignService, config RSSPollerConfig) *RSSPoller {
	if config.PollInterval == 0 {
		config.PollInterval = 5 * time.Minute
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 5
	}

	return &RSSPoller{
		db:             db,
		rssSvc:         rssSvc,
		pollInterval:   config.PollInterval,
		maxConcurrent:  config.MaxConcurrent,
		enableAutoSend: config.EnableAutoSend,
	}
}

// Start begins the RSS poller background goroutine
func (p *RSSPoller) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	log.Printf("[RSSPoller] Starting with poll_interval=%s, max_concurrent=%d",
		p.pollInterval, p.maxConcurrent)

	p.wg.Add(1)
	go p.pollLoop()
}

// Stop gracefully stops the RSS poller
func (p *RSSPoller) Stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.cancel()
	p.mu.Unlock()

	log.Println("[RSSPoller] Stopping...")
	p.wg.Wait()

	log.Printf("[RSSPoller] Stopped. Stats: polls=%d, items_found=%d, campaigns=%d, errors=%d",
		atomic.LoadInt64(&p.totalPolls),
		atomic.LoadInt64(&p.totalItemsFound),
		atomic.LoadInt64(&p.totalCampaigns),
		atomic.LoadInt64(&p.totalErrors))
}

// Stats returns current polling statistics
func (p *RSSPoller) Stats() map[string]int64 {
	return map[string]int64{
		"total_polls":       atomic.LoadInt64(&p.totalPolls),
		"total_items_found": atomic.LoadInt64(&p.totalItemsFound),
		"total_campaigns":   atomic.LoadInt64(&p.totalCampaigns),
		"total_errors":      atomic.LoadInt64(&p.totalErrors),
	}
}

// IsRunning returns whether the poller is currently running
func (p *RSSPoller) IsRunning() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.running
}

// PollAllFeeds manually triggers polling of all due feeds
func (p *RSSPoller) PollAllFeeds(ctx context.Context) error {
	return p.pollDueFeeds(ctx)
}

// PollSingleFeed polls a specific RSS campaign immediately
func (p *RSSPoller) PollSingleFeed(ctx context.Context, campaignID string) (*PollResult, error) {
	campaign, err := p.rssSvc.GetRSSCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, nil
	}

	return p.processFeed(ctx, *campaign)
}

// PollResult contains the result of polling a single feed
type PollResult struct {
	CampaignID    string   `json:"campaign_id"`
	ItemsFound    int      `json:"items_found"`
	NewItems      int      `json:"new_items"`
	CampaignsCreated int   `json:"campaigns_created"`
	Errors        []string `json:"errors,omitempty"`
}

// pollLoop is the main polling loop
func (p *RSSPoller) pollLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	// Poll immediately on start
	if err := p.pollDueFeeds(p.ctx); err != nil {
		log.Printf("[RSSPoller] Initial poll error: %v", err)
	}

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.pollDueFeeds(p.ctx); err != nil {
				log.Printf("[RSSPoller] Poll cycle error: %v", err)
			}
		}
	}
}

// pollDueFeeds fetches and processes all RSS campaigns due for polling
func (p *RSSPoller) pollDueFeeds(ctx context.Context) error {
	// Get campaigns due for polling
	campaigns, err := p.rssSvc.GetActiveRSSCampaignsDueForPoll(ctx)
	if err != nil {
		return err
	}

	if len(campaigns) == 0 {
		return nil
	}

	log.Printf("[RSSPoller] Found %d campaigns due for polling", len(campaigns))
	atomic.AddInt64(&p.totalPolls, 1)

	// Process with concurrency limit
	sem := make(chan struct{}, p.maxConcurrent)
	var wg sync.WaitGroup

	for _, campaign := range campaigns {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sem <- struct{}{}:
			wg.Add(1)
			go func(c mailing.RSSCampaign) {
				defer wg.Done()
				defer func() { <-sem }()

				result, err := p.processFeed(ctx, c)
				if err != nil {
					log.Printf("[RSSPoller] Error processing feed %s (%s): %v",
						c.Name, c.ID, err)
					atomic.AddInt64(&p.totalErrors, 1)
					return
				}

				if result != nil && result.NewItems > 0 {
					log.Printf("[RSSPoller] Processed %s: found=%d, new=%d, campaigns=%d",
						c.Name, result.ItemsFound, result.NewItems, result.CampaignsCreated)
				}
			}(campaign)
		}
	}

	wg.Wait()
	return nil
}

// processFeed processes a single RSS feed
func (p *RSSPoller) processFeed(ctx context.Context, campaign mailing.RSSCampaign) (*PollResult, error) {
	result := &PollResult{
		CampaignID: campaign.ID,
		Errors:     []string{},
	}

	// Poll the feed for new items
	items, err := p.rssSvc.PollFeed(ctx, campaign.ID)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	result.ItemsFound = len(items) + p.countExistingItems(ctx, campaign.ID)
	result.NewItems = len(items)
	atomic.AddInt64(&p.totalItemsFound, int64(len(items)))

	if len(items) == 0 {
		return result, nil
	}

	// Generate campaigns for new items
	for _, item := range items {
		if !campaign.AutoSend && !p.enableAutoSend {
			// Just record the item without generating a campaign
			p.recordItemDiscovered(ctx, campaign.ID, item)
			continue
		}

		generatedCampaign, err := p.rssSvc.GenerateCampaignFromFeed(ctx, campaign.ID, item)
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			log.Printf("[RSSPoller] Failed to generate campaign for item %s: %v",
				item.GUID, err)
			continue
		}

		result.CampaignsCreated++
		atomic.AddInt64(&p.totalCampaigns, 1)

		// Auto-send if configured
		if campaign.AutoSend && p.enableAutoSend {
			if err := p.enqueueCampaign(ctx, generatedCampaign.ID.String()); err != nil {
				result.Errors = append(result.Errors,
					"failed to enqueue campaign: "+err.Error())
				log.Printf("[RSSPoller] Failed to enqueue campaign %s: %v",
					generatedCampaign.ID, err)
			} else {
				// Mark item as sent
				p.markItemSent(ctx, campaign.ID, item.GUID, generatedCampaign.ID.String())
			}
		}
	}

	return result, nil
}

// countExistingItems counts total items already processed for a campaign
func (p *RSSPoller) countExistingItems(ctx context.Context, campaignID string) int {
	var count int
	p.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_rss_sent_items WHERE rss_campaign_id = $1
	`, campaignID).Scan(&count)
	return count
}

// recordItemDiscovered records a discovered item without generating a campaign
func (p *RSSPoller) recordItemDiscovered(ctx context.Context, campaignID string, item mailing.FeedItem) {
	p.db.ExecContext(ctx, `
		INSERT INTO mailing_rss_sent_items (
			rss_campaign_id, item_guid, item_title, item_link, item_pub_date,
			status, discovered_at
		) VALUES ($1, $2, $3, $4, $5, 'pending', NOW())
		ON CONFLICT (rss_campaign_id, item_guid) DO NOTHING
	`, campaignID, item.GUID, item.Title, item.Link, item.PubDate)
}

// markItemSent marks an item as sent
func (p *RSSPoller) markItemSent(ctx context.Context, rssCampaignID, itemGUID, campaignID string) {
	p.db.ExecContext(ctx, `
		UPDATE mailing_rss_sent_items 
		SET status = 'sent', campaign_id = $3, sent_at = NOW()
		WHERE rss_campaign_id = $1 AND item_guid = $2
	`, rssCampaignID, itemGUID, campaignID)
}

// enqueueCampaign adds a campaign to the sending queue
func (p *RSSPoller) enqueueCampaign(ctx context.Context, campaignID string) error {
	// Update campaign status to scheduled/sending
	// NOTE: Set both send_at and scheduled_at for compatibility with scheduler
	_, err := p.db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET status = 'scheduled', send_at = NOW(), scheduled_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'draft'
	`, campaignID)
	if err != nil {
		return err
	}

	// Use existing EnqueueCampaign function
	_, err = EnqueueCampaign(ctx, p.db, campaignID)
	return err
}

// RSSPollerManager manages RSS polling for multiple organizations
type RSSPollerManager struct {
	poller *RSSPoller
}

// NewRSSPollerManager creates a new RSS poller manager
func NewRSSPollerManager(db *sql.DB, rssSvc *mailing.RSSCampaignService) *RSSPollerManager {
	return &RSSPollerManager{
		poller: NewRSSPoller(db, rssSvc, DefaultRSSPollerConfig()),
	}
}

// Start starts the RSS polling
func (m *RSSPollerManager) Start() {
	m.poller.Start()
}

// Stop stops the RSS polling
func (m *RSSPollerManager) Stop() {
	m.poller.Stop()
}

// GetPoller returns the underlying poller
func (m *RSSPollerManager) GetPoller() *RSSPoller {
	return m.poller
}

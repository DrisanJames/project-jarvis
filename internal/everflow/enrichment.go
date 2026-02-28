package everflow

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

// EnrichmentService handles cross-referencing Everflow and Ongage data
type EnrichmentService struct {
	everflowCollector *Collector
	ongageClient      *ongage.Client
	ongageCollector   *ongage.Collector // For accessing cached campaign stats
	cache             map[string]*EnrichedCampaignDetails
	cacheMu           sync.RWMutex
	cacheTTL          time.Duration
	cacheExpiry       map[string]time.Time
}

// NewEnrichmentService creates a new enrichment service
func NewEnrichmentService(everflowCollector *Collector, ongageClient *ongage.Client) *EnrichmentService {
	return &EnrichmentService{
		everflowCollector: everflowCollector,
		ongageClient:      ongageClient,
		cache:             make(map[string]*EnrichedCampaignDetails),
		cacheExpiry:       make(map[string]time.Time),
		cacheTTL:          5 * time.Minute, // Cache for 5 minutes
	}
}

// SetOngageCollector sets the Ongage collector for accessing cached stats
func (s *EnrichmentService) SetOngageCollector(collector *ongage.Collector) {
	s.ongageCollector = collector
}

// GetEnrichedCampaignDetails retrieves enriched campaign details by mailing ID
func (s *EnrichmentService) GetEnrichedCampaignDetails(ctx context.Context, mailingID string) (*EnrichedCampaignDetails, error) {
	// Check cache first
	s.cacheMu.RLock()
	if cached, ok := s.cache[mailingID]; ok {
		if expiry, exists := s.cacheExpiry[mailingID]; exists && time.Now().Before(expiry) {
			s.cacheMu.RUnlock()
			return cached, nil
		}
	}
	s.cacheMu.RUnlock()

	// Build enriched details
	enriched, err := s.buildEnrichedDetails(ctx, mailingID)
	if err != nil {
		return nil, err
	}

	// Cache the result
	s.cacheMu.Lock()
	s.cache[mailingID] = enriched
	s.cacheExpiry[mailingID] = time.Now().Add(s.cacheTTL)
	s.cacheMu.Unlock()

	return enriched, nil
}

// buildEnrichedDetails builds the enriched campaign details from both sources
func (s *EnrichmentService) buildEnrichedDetails(ctx context.Context, mailingID string) (*EnrichedCampaignDetails, error) {
	enriched := &EnrichedCampaignDetails{
		MailingID:    mailingID,
		OngageLinked: false,
	}

	// Get Everflow campaign data
	campaigns := s.everflowCollector.GetCampaignRevenue()
	var everflowCampaign *CampaignRevenue
	for i, c := range campaigns {
		if c.MailingID == mailingID {
			everflowCampaign = &campaigns[i]
			break
		}
	}

	if everflowCampaign == nil {
		return nil, fmt.Errorf("campaign with mailing ID %s not found in Everflow data", mailingID)
	}

	// Populate Everflow data
	enriched.PropertyCode = everflowCampaign.PropertyCode
	enriched.PropertyName = everflowCampaign.PropertyName
	enriched.OfferID = everflowCampaign.OfferID
	enriched.OfferName = everflowCampaign.OfferName
	enriched.Clicks = everflowCampaign.Clicks
	enriched.Conversions = everflowCampaign.Conversions
	enriched.Revenue = everflowCampaign.Revenue
	enriched.Payout = everflowCampaign.Payout

	// Calculate basic Everflow metrics
	if enriched.Clicks > 0 {
		enriched.ConversionRate = float64(enriched.Conversions) / float64(enriched.Clicks)
		enriched.RevenuePerClick = enriched.Revenue / float64(enriched.Clicks)
	}

	// Try to get Ongage campaign details
	if s.ongageClient != nil {
		err := s.enrichFromOngage(ctx, enriched, mailingID)
		if err != nil {
			enriched.LinkError = err.Error()
		} else {
			enriched.OngageLinked = true
		}
	} else {
		enriched.LinkError = "Ongage client not configured"
	}

	return enriched, nil
}

// enrichFromOngage fetches and adds Ongage campaign data
func (s *EnrichmentService) enrichFromOngage(ctx context.Context, enriched *EnrichedCampaignDetails, mailingID string) error {
	// Get campaign details from Ongage
	campaign, err := s.ongageClient.GetCampaign(ctx, mailingID)
	if err != nil {
		return fmt.Errorf("failed to fetch Ongage campaign: %w", err)
	}

	// Basic campaign info
	enriched.CampaignName = campaign.Name
	enriched.ScheduleDate = campaign.ScheduleDate
	enriched.SendingStartDate = campaign.SendingStartDate
	enriched.SendingEndDate = campaign.SendingEndDate
	enriched.Status = campaign.Status
	enriched.StatusDesc = campaign.StatusDesc

	// Parse audience size
	if campaign.Targeted != "" {
		if targeted, err := strconv.ParseInt(campaign.Targeted, 10, 64); err == nil {
			enriched.AudienceSize = targeted
		}
	}

	// Extract sending domain and ESP info from distribution
	if len(campaign.Distribution) > 0 {
		dist := campaign.Distribution[0]
		enriched.SendingDomain = dist.Domain
		enriched.ESPConnectionID = dist.ESPConnectionID.String()
		
		// Map ESP ID to name
		espID := dist.ESPID.String()
		if espName, ok := ongage.ESPNames[espID]; ok {
			enriched.ESPName = espName
		} else {
			enriched.ESPName = fmt.Sprintf("ESP %s", espID)
		}
	}

	// Extract subject from email messages
	if len(campaign.EmailMessages) > 0 {
		enriched.Subject = campaign.EmailMessages[0].Subject
	}

	// Process segments - separate sending from suppression
	enriched.SendingSegments = []SegmentInfo{}
	enriched.SuppressionSegments = []SegmentInfo{}
	
	for _, seg := range campaign.Segments {
		segInfo := SegmentInfo{
			SegmentID: seg.SegmentID,
			Name:      seg.Name,
		}
		
		// Parse last count if available
		if seg.LastCount != "" {
			if count, err := strconv.ParseInt(seg.LastCount, 10, 64); err == nil {
				segInfo.Count = count
			}
		}
		
		// Check if this is a suppression segment
		if seg.Exclude == "1" {
			segInfo.IsSuppression = true
			enriched.SuppressionSegments = append(enriched.SuppressionSegments, segInfo)
		} else {
			segInfo.IsSuppression = false
			enriched.SendingSegments = append(enriched.SendingSegments, segInfo)
		}
	}

	// Get campaign stats - first try cached data from Ongage collector, then fall back to Reports API
	statsFound := s.enrichWithCachedOngageStats(enriched, mailingID)
	if !statsFound {
		// Fall back to Ongage reports API
		err = s.enrichWithOngageStats(ctx, enriched, mailingID)
		if err != nil {
			log.Printf("EnrichmentService: Failed to get stats for campaign %s from Reports API: %v", mailingID, err)
			// Stats are optional, don't fail the whole request
		}
	}

	// Calculate eCPM: (Revenue / Audience Size) * 1000
	if enriched.AudienceSize > 0 {
		enriched.ECPM = (enriched.Revenue / float64(enriched.AudienceSize)) * 1000
	}

	// Calculate delivery rate
	if enriched.Sent > 0 {
		enriched.DeliveryRate = float64(enriched.Delivered) / float64(enriched.Sent)
	}

	// Calculate open rate
	if enriched.Delivered > 0 {
		enriched.OpenRate = float64(enriched.UniqueOpens) / float64(enriched.Delivered)
	}

	// Calculate click-to-open rate
	if enriched.UniqueOpens > 0 {
		enriched.ClickToOpenRate = float64(enriched.EmailClicks) / float64(enriched.UniqueOpens)
	}

	return nil
}

// enrichWithCachedOngageStats tries to get stats from the Ongage collector's cached campaign data
// Returns true if stats were found, false otherwise
func (s *EnrichmentService) enrichWithCachedOngageStats(enriched *EnrichedCampaignDetails, mailingID string) bool {
	if s.ongageCollector == nil {
		return false
	}

	// Get campaigns from Ongage collector's cache
	campaigns := s.ongageCollector.GetCampaigns()
	for _, c := range campaigns {
		if c.ID == mailingID {
			// Found matching campaign - use its stats
			if c.Targeted > 0 && enriched.AudienceSize == 0 {
				enriched.AudienceSize = c.Targeted
			}
			enriched.Sent = c.Sent
			enriched.Delivered = c.Delivered
			enriched.Opens = c.Opens
			enriched.UniqueOpens = c.UniqueOpens
			enriched.TotalEmailClicks = c.Clicks       // Total clicks (including repeats)
			enriched.EmailClicks = c.UniqueClicks      // Unique clickers (consistent with UniqueOpens)
			enriched.UniqueEmailClicks = c.UniqueClicks
			enriched.Bounces = c.Bounces               // Total bounces
			enriched.HardBounces = c.HardBounces       // Permanent failures
			enriched.SoftBounces = c.SoftBounces       // Temporary failures
			enriched.Failed = c.Failed                 // Non-bounce failures
			enriched.Unsubscribes = c.Unsubscribes
			enriched.Complaints = c.Complaints
			
			log.Printf("EnrichmentService: Got stats for campaign %s from Ongage collector cache (sent=%d, delivered=%d, opens=%d)", 
				mailingID, enriched.Sent, enriched.Delivered, enriched.UniqueOpens)
			return true
		}
	}

	log.Printf("EnrichmentService: Campaign %s not found in Ongage collector cache (%d campaigns available)", 
		mailingID, len(campaigns))
	return false
}

// enrichWithOngageStats fetches stats from Ongage reports API
func (s *EnrichmentService) enrichWithOngageStats(ctx context.Context, enriched *EnrichedCampaignDetails, mailingID string) error {
	// Build a query to get stats for this specific mailing
	query := ongage.ReportQuery{
		Select: []interface{}{
			map[string]interface{}{"column": "sent", "scalar": "sum"},
			map[string]interface{}{"column": "success", "scalar": "sum"},
			map[string]interface{}{"column": "opens", "scalar": "sum"},
			map[string]interface{}{"column": "unique_opens", "scalar": "sum"},
			map[string]interface{}{"column": "clicks", "scalar": "sum"},
			map[string]interface{}{"column": "unique_clicks", "scalar": "sum"},
			map[string]interface{}{"column": "hard_bounces", "scalar": "sum"},
			map[string]interface{}{"column": "soft_bounces", "scalar": "sum"},
			map[string]interface{}{"column": "unsubscribes", "scalar": "sum"},
			map[string]interface{}{"column": "complaints", "scalar": "sum"},
		},
		From: "mailing",
		Filter: [][]interface{}{
			{"mailing_id", "=", mailingID},
		},
		ListIDs: "all",
	}

	rows, err := s.ongageClient.QueryReports(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to query Ongage reports: %w", err)
	}

	if len(rows) > 0 {
		row := rows[0]
		
		// Parse stats from the report row
		if v, ok := row["sent"].(float64); ok {
			enriched.Sent = int64(v)
		}
		if v, ok := row["success"].(float64); ok {
			enriched.Delivered = int64(v)
		}
		if v, ok := row["opens"].(float64); ok {
			enriched.Opens = int64(v)
		}
		if v, ok := row["unique_opens"].(float64); ok {
			enriched.UniqueOpens = int64(v)
		}
		if v, ok := row["clicks"].(float64); ok {
			enriched.EmailClicks = int64(v)
		}
		if v, ok := row["unique_clicks"].(float64); ok {
			enriched.UniqueEmailClicks = int64(v)
		}
		if v, ok := row["hard_bounces"].(float64); ok {
			enriched.Bounces += int64(v)
		}
		if v, ok := row["soft_bounces"].(float64); ok {
			enriched.Bounces += int64(v)
		}
		if v, ok := row["unsubscribes"].(float64); ok {
			enriched.Unsubscribes = int64(v)
		}
		if v, ok := row["complaints"].(float64); ok {
			enriched.Complaints = int64(v)
		}
	}

	return nil
}

// ClearCache clears the enrichment cache
func (s *EnrichmentService) ClearCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache = make(map[string]*EnrichedCampaignDetails)
	s.cacheExpiry = make(map[string]time.Time)
}

// GetCachedDetails returns cached details without fetching
func (s *EnrichmentService) GetCachedDetails(mailingID string) (*EnrichedCampaignDetails, bool) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	
	if cached, ok := s.cache[mailingID]; ok {
		if expiry, exists := s.cacheExpiry[mailingID]; exists && time.Now().Before(expiry) {
			return cached, true
		}
	}
	return nil, false
}

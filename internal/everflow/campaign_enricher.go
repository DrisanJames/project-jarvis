package everflow

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

// CampaignEnricher handles batch enrichment of campaign data from Ongage
type CampaignEnricher struct {
	ongageClient    *ongage.Client
	ongageCollector *ongage.Collector
	// Cache for Ongage campaign data
	cache       map[string]*OngageCampaignData
	cacheMu     sync.RWMutex
	cacheTTL    time.Duration
	cacheExpiry time.Time
}

// OngageCampaignData contains the relevant Ongage data for a campaign
type OngageCampaignData struct {
	MailingID       string
	CampaignName    string
	AudienceSize    int64
	Sent            int64
	Delivered       int64
	Opens           int64
	UniqueOpens     int64
	EmailClicks     int64
	SendingDomain   string
	ESPName         string // SparkPost, Mailgun, SES, etc.
	ESPConnectionID string
	Found           bool
}

// NewCampaignEnricher creates a new campaign enricher
func NewCampaignEnricher(ongageClient *ongage.Client) *CampaignEnricher {
	return &CampaignEnricher{
		ongageClient: ongageClient,
		cache:        make(map[string]*OngageCampaignData),
		cacheTTL:     15 * time.Minute, // Cache Ongage data for 15 minutes
	}
}

// SetOngageCollector sets the Ongage collector for accessing pre-fetched stats
func (e *CampaignEnricher) SetOngageCollector(collector *ongage.Collector) {
	e.ongageCollector = collector
}

// GetOngageCampaigns returns the cached Ongage campaigns
func (e *CampaignEnricher) GetOngageCampaigns() []ongage.ProcessedCampaign {
	if e.ongageCollector == nil {
		return nil
	}
	return e.ongageCollector.GetCampaigns()
}

// EnrichCampaigns enriches a list of campaigns with Ongage data
func (e *CampaignEnricher) EnrichCampaigns(ctx context.Context, campaigns []CampaignRevenue) []CampaignRevenue {
	if e.ongageClient == nil {
		log.Println("CampaignEnricher: Ongage client not configured, skipping enrichment")
		return campaigns
	}

	// Check if cache is still valid
	e.cacheMu.RLock()
	cacheValid := time.Now().Before(e.cacheExpiry) && len(e.cache) > 0
	e.cacheMu.RUnlock()

	// Collect mailing IDs that need to be fetched
	var mailingIDsToFetch []string
	if !cacheValid {
		// Cache expired, need to fetch all
		for _, c := range campaigns {
			if c.MailingID != "" {
				mailingIDsToFetch = append(mailingIDsToFetch, c.MailingID)
			}
		}
	} else {
		// Check which ones are missing from cache
		e.cacheMu.RLock()
		for _, c := range campaigns {
			if c.MailingID != "" {
				if _, ok := e.cache[c.MailingID]; !ok {
					mailingIDsToFetch = append(mailingIDsToFetch, c.MailingID)
				}
			}
		}
		e.cacheMu.RUnlock()
	}

	// Fetch missing campaigns from Ongage
	if len(mailingIDsToFetch) > 0 {
		log.Printf("CampaignEnricher: Fetching %d campaigns from Ongage", len(mailingIDsToFetch))
		e.fetchAndCacheCampaigns(ctx, mailingIDsToFetch)
	}

	// Enrich campaigns with cached data
	enriched := make([]CampaignRevenue, len(campaigns))
	e.cacheMu.RLock()
	for i, c := range campaigns {
		enriched[i] = c
		if data, ok := e.cache[c.MailingID]; ok && data.Found {
			enriched[i].AudienceSize = data.AudienceSize
			enriched[i].Sent = data.Sent
			enriched[i].Delivered = data.Delivered
			enriched[i].Opens = data.Opens
			enriched[i].UniqueOpens = data.UniqueOpens
			enriched[i].EmailClicks = data.EmailClicks
			enriched[i].SendingDomain = data.SendingDomain
			enriched[i].ESPName = data.ESPName
			enriched[i].ESPConnectionID = data.ESPConnectionID
			enriched[i].OngageLinked = true
			
			// Use Ongage campaign name if available and current name is just sub1
			if data.CampaignName != "" && (enriched[i].CampaignName == "" || enriched[i].CampaignName == c.MailingID) {
				enriched[i].CampaignName = data.CampaignName
			}
			
			// Try to resolve unknown property code from Ongage campaign name
			// Campaign name format: DATE_PROPERTY_OFFERID_OFFERNAME_SEGMENT
			// Example: 01272026_FYF_419_MutualofOmaha_YAH_OPENERS
			if !ValidatePropertyCode(enriched[i].PropertyCode) && data.CampaignName != "" {
				_, extractedProperty, _, _, _, err := ParseCampaignName(data.CampaignName)
				if err == nil && extractedProperty != "" && ValidatePropertyCode(extractedProperty) {
					enriched[i].PropertyCode = extractedProperty
					enriched[i].PropertyName = GetPropertyName(extractedProperty)
				}
			}
			
			// Calculate metrics - ECPM based on Delivered (after suppression + bounces)
			if enriched[i].Delivered > 0 {
				enriched[i].ECPM = (enriched[i].Revenue / float64(enriched[i].Delivered)) * 1000
			}
			if enriched[i].Sent > 0 {
				enriched[i].RPM = (enriched[i].Revenue / float64(enriched[i].Sent)) * 1000
			}
			if enriched[i].UniqueOpens > 0 {
				enriched[i].RevenuePerOpen = enriched[i].Revenue / float64(enriched[i].UniqueOpens)
			}
		}
	}
	e.cacheMu.RUnlock()

	return enriched
}

// fetchAndCacheCampaigns fetches campaign data from Ongage and caches it
func (e *CampaignEnricher) fetchAndCacheCampaigns(ctx context.Context, mailingIDs []string) {
	// Fetch campaigns in batches to avoid overwhelming the API
	batchSize := 10
	var wg sync.WaitGroup
	results := make(chan *OngageCampaignData, len(mailingIDs))

	for i := 0; i < len(mailingIDs); i += batchSize {
		end := i + batchSize
		if end > len(mailingIDs) {
			end = len(mailingIDs)
		}
		batch := mailingIDs[i:end]

		wg.Add(1)
		go func(ids []string) {
			defer wg.Done()
			for _, id := range ids {
				data := e.fetchSingleCampaign(ctx, id)
				results <- data
				// Rate limiting
				time.Sleep(100 * time.Millisecond)
			}
		}(batch)

		// Small delay between batches
		if end < len(mailingIDs) {
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Wait for all fetches to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results and update cache
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()

	for data := range results {
		e.cache[data.MailingID] = data
	}

	// Update cache expiry
	e.cacheExpiry = time.Now().Add(e.cacheTTL)
	log.Printf("CampaignEnricher: Cached %d campaigns, expires at %s", len(e.cache), e.cacheExpiry.Format(time.RFC3339))
}

// fetchSingleCampaign fetches data for a single campaign from Ongage
func (e *CampaignEnricher) fetchSingleCampaign(ctx context.Context, mailingID string) *OngageCampaignData {
	data := &OngageCampaignData{
		MailingID: mailingID,
		Found:     false,
	}

	// Fetch campaign details
	campaign, err := e.ongageClient.GetCampaign(ctx, mailingID)
	if err != nil {
		// Don't log every error - campaigns might not exist in Ongage
		return data
	}

	data.Found = true
	data.CampaignName = campaign.Name

	// Parse targeted audience size from campaign details
	if campaign.Targeted != "" {
		if targeted, err := strconv.ParseInt(campaign.Targeted, 10, 64); err == nil {
			data.AudienceSize = targeted
		}
	}
	
	// Also calculate from segments if Targeted is not set
	if data.AudienceSize == 0 && len(campaign.Segments) > 0 {
		for _, seg := range campaign.Segments {
			if seg.Exclude != "1" && seg.LastCount != "" {
				if count, err := strconv.ParseInt(seg.LastCount, 10, 64); err == nil {
					data.AudienceSize += count
				}
			}
		}
	}

	// Get sending domain and ESP from distribution
	if len(campaign.Distribution) > 0 {
		dist := campaign.Distribution[0]
		data.SendingDomain = dist.Domain
		data.ESPConnectionID = dist.ESPConnectionID.String()
		
		// Map ESP ID to name
		espID := dist.ESPID.String()
		if espName, ok := ongage.ESPNames[espID]; ok {
			data.ESPName = espName
		} else {
			data.ESPName = "ESP " + espID
		}
	}

	// Fetch stats from reports API
	e.fetchCampaignStats(ctx, mailingID, data)

	return data
}

// fetchCampaignStats fetches delivery stats for a campaign including targeted audience
// First tries to get stats from Ongage collector's cached data, then falls back to API
func (e *CampaignEnricher) fetchCampaignStats(ctx context.Context, mailingID string, data *OngageCampaignData) {
	// Try to get stats from Ongage collector first (already fetched in background)
	if e.ongageCollector != nil {
		campaigns := e.ongageCollector.GetCampaigns()
		for _, c := range campaigns {
			if c.ID == mailingID {
				// Found matching campaign - use its stats
				if c.Targeted > 0 {
					data.AudienceSize = c.Targeted
				}
				data.Sent = c.Sent
				data.Delivered = c.Delivered
				data.Opens = c.Opens
				data.UniqueOpens = c.UniqueOpens
				data.EmailClicks = c.Clicks
				return
			}
		}
	}
	
	// Fallback: Use reports API if not found in collector cache
	// Use same query pattern as GetCampaignStats which works reliably
	dateFrom := time.Now().AddDate(0, 0, -90) // Look back 90 days for campaign stats
	
	query := ongage.ReportQuery{
		Select: []interface{}{
			"mailing_id",
			"sum(`targeted`)",
			"sum(`sent`)",
			"sum(`success`)",
			"sum(`opens`)",
			"sum(`unique_opens`)",
			"sum(`clicks`)",
		},
		From:  "mailing",
		Group: []interface{}{"mailing_id"},
		Filter: [][]interface{}{
			{"mailing_id", "=", mailingID},
			{"stats_date", ">=", dateFrom.Format("2006-01-02")},
		},
		ListIDs:        "all",
		CalculateRates: true,
	}

	rows, err := e.ongageClient.QueryReports(ctx, query)
	if err != nil {
		return
	}

	if len(rows) > 0 {
		row := rows[0]
		// Note: Ongage API returns column names without the sum() wrapper
		if v, ok := row["targeted"].(float64); ok && v > 0 {
			data.AudienceSize = int64(v)
		}
		if v, ok := row["sent"].(float64); ok {
			data.Sent = int64(v)
		}
		if v, ok := row["success"].(float64); ok {
			data.Delivered = int64(v)
		}
		if v, ok := row["opens"].(float64); ok {
			data.Opens = int64(v)
		}
		if v, ok := row["unique_opens"].(float64); ok {
			data.UniqueOpens = int64(v)
		}
		if v, ok := row["clicks"].(float64); ok {
			data.EmailClicks = int64(v)
		}
	}
}

// ClearCache clears the enrichment cache
func (e *CampaignEnricher) ClearCache() {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	e.cache = make(map[string]*OngageCampaignData)
	e.cacheExpiry = time.Time{}
}

// GetCacheStats returns cache statistics
func (e *CampaignEnricher) GetCacheStats() (size int, expiresAt time.Time, valid bool) {
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
	return len(e.cache), e.cacheExpiry, time.Now().Before(e.cacheExpiry)
}

// ResolvePropertyCodes looks up campaigns in Ongage and extracts property codes from campaign names
// This is used to attribute revenue when the sub1 field is missing the property code prefix
// Returns a map of mailingID -> resolved property code
func (e *CampaignEnricher) ResolvePropertyCodes(ctx context.Context, mailingIDs []string) map[string]string {
	resolved := make(map[string]string)
	
	if len(mailingIDs) == 0 {
		return resolved
	}
	
	log.Printf("CampaignEnricher: Resolving property codes for %d campaigns via Ongage", len(mailingIDs))
	
	// First check cache for already-fetched campaigns
	e.cacheMu.RLock()
	var uncachedIDs []string
	for _, id := range mailingIDs {
		if data, ok := e.cache[id]; ok && data.Found && data.CampaignName != "" {
			// Try to parse property from cached campaign name
			_, property, _, _, _, err := ParseCampaignName(data.CampaignName)
			if err == nil && property != "" && ValidatePropertyCode(property) {
				resolved[id] = property
			}
		} else {
			uncachedIDs = append(uncachedIDs, id)
		}
	}
	e.cacheMu.RUnlock()
	
	if len(uncachedIDs) == 0 {
		log.Printf("CampaignEnricher: Resolved %d property codes from cache", len(resolved))
		return resolved
	}
	
	// Fetch uncached campaigns from Ongage (with rate limiting)
	// Limit to 50 lookups to avoid overwhelming the API
	maxLookups := 50
	if len(uncachedIDs) > maxLookups {
		log.Printf("CampaignEnricher: Limiting Ongage lookups to %d (have %d uncached)", maxLookups, len(uncachedIDs))
		uncachedIDs = uncachedIDs[:maxLookups]
	}
	
	for _, mailingID := range uncachedIDs {
		// Rate limit: 100ms between requests
		time.Sleep(100 * time.Millisecond)
		
		data := e.fetchSingleCampaign(ctx, mailingID)
		if data.Found && data.CampaignName != "" {
			// Cache the result
			e.cacheMu.Lock()
			e.cache[mailingID] = data
			e.cacheMu.Unlock()
			
			// Parse property from campaign name
			// Format: DATE_PROPERTY_OFFERID_OFFERNAME_SEGMENT
			// Example: 01282026_FMO_342_SomeOffer_OPENERS
			_, property, _, _, _, err := ParseCampaignName(data.CampaignName)
			if err == nil && property != "" && ValidatePropertyCode(property) {
				resolved[mailingID] = property
				log.Printf("CampaignEnricher: Resolved %s -> property %s (from campaign: %s)", mailingID, property, data.CampaignName)
			}
		}
	}
	
	log.Printf("CampaignEnricher: Resolved %d property codes total (%d from cache, %d from API)", 
		len(resolved), len(resolved)-len(uncachedIDs), len(uncachedIDs))
	
	return resolved
}

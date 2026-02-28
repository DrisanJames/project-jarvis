package everflow

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// NetworkIntelligenceCollector is a background worker that continuously processes
// network-wide Everflow data to build intelligence about top-performing offers,
// audience profiles (from click/conversion metadata), and AI recommendations.
//
// Unlike the main Collector (which filters by affiliate IDs), this collector
// queries the ENTIRE network to understand what's converting across ALL affiliates.
type NetworkIntelligenceCollector struct {
	client        *Client
	mu            sync.RWMutex
	fetchInterval time.Duration
	stopChan      chan struct{}

	// Cached intelligence snapshot
	snapshot *NetworkIntelligenceSnapshot

	// Raw metadata cache for analysis (rotated daily)
	todayClicks      []ClickRecord
	todayConversions []ConversionRecord
	metadataDate     string // YYYY-MM-DD of cached metadata
}

// NewNetworkIntelligenceCollector creates a new network intelligence collector
func NewNetworkIntelligenceCollector(client *Client, fetchInterval time.Duration) *NetworkIntelligenceCollector {
	return &NetworkIntelligenceCollector{
		client:        client,
		fetchInterval: fetchInterval,
		stopChan:      make(chan struct{}),
	}
}

// Start begins the periodic network intelligence collection
func (nic *NetworkIntelligenceCollector) Start() {
	log.Println("[NetworkIntel] Starting network intelligence collector...")
	go nic.collectLoop()
}

// Stop halts the collection
func (nic *NetworkIntelligenceCollector) Stop() {
	close(nic.stopChan)
}

// GetSnapshot returns the current network intelligence snapshot
func (nic *NetworkIntelligenceCollector) GetSnapshot() *NetworkIntelligenceSnapshot {
	nic.mu.RLock()
	defer nic.mu.RUnlock()
	return nic.snapshot
}

// collectLoop runs the periodic collection cycle
func (nic *NetworkIntelligenceCollector) collectLoop() {
	// Initial full fetch
	nic.fullCollect()

	ticker := time.NewTicker(nic.fetchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			nic.incrementalCollect()
		case <-nic.stopChan:
			log.Println("[NetworkIntel] Collector stopped")
			return
		}
	}
}

// fullCollect performs a comprehensive collection of network-wide data
func (nic *NetworkIntelligenceCollector) fullCollect() {
	startTime := time.Now()
	log.Println("[NetworkIntel] Starting full network intelligence collection...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	now := time.Now()
	todayStr := now.Format("2006-01-02")
	sevenDaysAgo := now.AddDate(0, 0, -7)

	// Step 1: Fetch network-wide offer performance (7-day lookback)
	offerReport, err := nic.client.GetEntityReportByOfferNetworkWide(ctx, sevenDaysAgo, now)
	if err != nil {
		log.Printf("[NetworkIntel] Error fetching network offer report: %v", err)
		return
	}
	log.Printf("[NetworkIntel] Got %d offers from network-wide report", len(offerReport.Table))

	// Step 2: Fetch today's offer performance for trend comparison
	todayStart := now.Truncate(24 * time.Hour)
	todayOfferReport, _ := nic.client.GetEntityReportByOfferNetworkWide(ctx, todayStart, now)

	// Step 3: Fetch today's raw clicks for metadata analysis (user agent, geo, etc.)
	todayFrom := todayStart.Format("2006-01-02 00:00:00")
	todayTo := now.Format("2006-01-02 23:59:59")

	clicks, err := nic.client.GetClicksNetworkWide(ctx, todayFrom, todayTo)
	if err != nil {
		log.Printf("[NetworkIntel] Error fetching network clicks: %v", err)
		clicks = []ClickRecord{}
	}
	log.Printf("[NetworkIntel] Got %d network-wide clicks for metadata analysis", len(clicks))

	// Step 4: Fetch today's raw conversions for metadata analysis
	conversions, err := nic.client.GetConversionsNetworkWide(ctx, todayStr, todayStr)
	if err != nil {
		log.Printf("[NetworkIntel] Error fetching network conversions: %v", err)
		conversions = []ConversionRecord{}
	}
	log.Printf("[NetworkIntel] Got %d network-wide conversions for metadata analysis", len(conversions))

	// Step 5: Build the intelligence snapshot
	snapshot := nic.buildSnapshot(offerReport, todayOfferReport, clicks, conversions)
	snapshot.CollectionDuration = time.Since(startTime)

	// Store results
	nic.mu.Lock()
	nic.snapshot = snapshot
	nic.todayClicks = clicks
	nic.todayConversions = conversions
	nic.metadataDate = todayStr
	nic.mu.Unlock()

	log.Printf("[NetworkIntel] Full collection complete in %v. %d top offers, %d recommendations",
		time.Since(startTime), len(snapshot.TopOffers), len(snapshot.AudienceRecommendations))
}

// incrementalCollect fetches only today's latest data and updates the snapshot
func (nic *NetworkIntelligenceCollector) incrementalCollect() {
	log.Println("[NetworkIntel] Incremental update...")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	now := time.Now()
	todayStr := now.Format("2006-01-02")
	todayStart := now.Truncate(24 * time.Hour)
	sevenDaysAgo := now.AddDate(0, 0, -7)

	// Fetch current offer report
	offerReport, err := nic.client.GetEntityReportByOfferNetworkWide(ctx, sevenDaysAgo, now)
	if err != nil {
		log.Printf("[NetworkIntel] Incremental error fetching offers: %v", err)
		return
	}

	todayOfferReport, _ := nic.client.GetEntityReportByOfferNetworkWide(ctx, todayStart, now)

	// Fetch today's metadata
	todayFrom := todayStart.Format("2006-01-02 00:00:00")
	todayTo := now.Format("2006-01-02 23:59:59")

	clicks, _ := nic.client.GetClicksNetworkWide(ctx, todayFrom, todayTo)
	conversions, _ := nic.client.GetConversionsNetworkWide(ctx, todayStr, todayStr)

	snapshot := nic.buildSnapshot(offerReport, todayOfferReport, clicks, conversions)

	nic.mu.Lock()
	nic.snapshot = snapshot
	if todayStr != nic.metadataDate {
		// New day - reset metadata cache
		nic.todayClicks = clicks
		nic.todayConversions = conversions
		nic.metadataDate = todayStr
	} else {
		nic.todayClicks = clicks
		nic.todayConversions = conversions
	}
	nic.mu.Unlock()

	log.Printf("[NetworkIntel] Incremental update: %d offers, %d clicks, %d conversions",
		len(snapshot.TopOffers), len(clicks), len(conversions))
}

// buildSnapshot constructs a NetworkIntelligenceSnapshot from raw data
func (nic *NetworkIntelligenceCollector) buildSnapshot(
	offerReport *EntityReportResponse,
	todayOfferReport *EntityReportResponse,
	clicks []ClickRecord,
	conversions []ConversionRecord,
) *NetworkIntelligenceSnapshot {

	snapshot := &NetworkIntelligenceSnapshot{
		Timestamp:   time.Now(),
		LastUpdated: time.Now(),
	}

	if offerReport == nil {
		return snapshot
	}

	// Build today's map for quick lookup
	todayMap := make(map[string]*EntityReportRow)
	if todayOfferReport != nil {
		for i := range todayOfferReport.Table {
			row := &todayOfferReport.Table[i]
			for _, col := range row.Columns {
				if col.ColumnType == "offer" {
					todayMap[col.ID] = row
					break
				}
			}
		}
	}

	// Build click metadata index by offer
	clicksByOffer := make(map[string][]ClickRecord)
	for _, click := range clicks {
		clicksByOffer[click.OfferID] = append(clicksByOffer[click.OfferID], click)
	}

	// Build conversion metadata index by offer
	convsByOffer := make(map[string][]ConversionRecord)
	for _, conv := range conversions {
		offerID := conv.OfferID
		if conv.Relationship != nil && conv.Relationship.Offer != nil {
			offerID = fmt.Sprintf("%d", conv.Relationship.Offer.NetworkOfferID)
		}
		convsByOffer[offerID] = append(convsByOffer[offerID], conv)
	}

	// Process each offer
	var offers []NetworkOfferIntelligence
	var totalClicks, totalConversions int64
	var totalRevenue float64

	for _, row := range offerReport.Table {
		var offerID, offerName string
		for _, col := range row.Columns {
			if col.ColumnType == "offer" {
				offerID = col.ID
				offerName = col.Label
				break
			}
		}
		if offerID == "" {
			continue
		}

		offer := NetworkOfferIntelligence{
			OfferID:            offerID,
			OfferName:          offerName,
			OfferType:          GetOfferType(offerName),
			NetworkClicks:      row.Reporting.TotalClick,
			NetworkConversions: row.Reporting.Conversions,
			NetworkRevenue:     row.Reporting.Revenue,
		}

		if row.Reporting.TotalClick > 0 {
			offer.NetworkCVR = float64(row.Reporting.Conversions) / float64(row.Reporting.TotalClick) * 100
			offer.NetworkEPC = row.Reporting.Revenue / float64(row.Reporting.TotalClick)
		}

		// Today's metrics
		if todayRow, ok := todayMap[offerID]; ok {
			offer.TodayClicks = todayRow.Reporting.TotalClick
			offer.TodayConversions = todayRow.Reporting.Conversions
			offer.TodayRevenue = todayRow.Reporting.Revenue
			if todayRow.Reporting.TotalClick > 0 {
				offer.TodayEPC = todayRow.Reporting.Revenue / float64(todayRow.Reporting.TotalClick)
			}
		}

		// Trend calculation (today vs 7-day daily average)
		dailyAvgRevenue := row.Reporting.Revenue / 7.0
		if dailyAvgRevenue > 0 {
			trendPct := ((offer.TodayRevenue - dailyAvgRevenue) / dailyAvgRevenue) * 100
			offer.TrendPercentage = math.Round(trendPct*100) / 100
			if trendPct > 15 {
				offer.RevenueTrend = "accelerating"
			} else if trendPct < -15 {
				offer.RevenueTrend = "decelerating"
			} else {
				offer.RevenueTrend = "stable"
			}
		} else {
			offer.RevenueTrend = "stable"
		}

		// Hourly velocity
		hoursToday := time.Since(time.Now().Truncate(24 * time.Hour)).Hours()
		if hoursToday > 0 {
			offer.HourlyVelocity = math.Round(offer.TodayRevenue/hoursToday*100) / 100
		}

		// Build audience profile from click/conversion metadata
		offerClicks := clicksByOffer[offerID]
		offerConvs := convsByOffer[offerID]
		offer.AudienceProfile = buildAudienceProfile(offerClicks, offerConvs)

		// AI Score
		offer.AIScore, offer.AIRecommendation, offer.AIReason = calculateNetworkOfferScore(offer, offerReport.Summary)

		offers = append(offers, offer)
		totalClicks += row.Reporting.TotalClick
		totalConversions += row.Reporting.Conversions
		totalRevenue += row.Reporting.Revenue
	}

	// Sort by today's revenue (what matters now)
	sort.Slice(offers, func(i, j int) bool {
		return offers[i].TodayRevenue > offers[j].TodayRevenue
	})

	// Assign ranks
	for i := range offers {
		offers[i].NetworkRank = i + 1
	}

	// Take top 50
	if len(offers) > 50 {
		offers = offers[:50]
	}

	snapshot.TopOffers = offers
	snapshot.NetworkTotalClicks = totalClicks
	snapshot.NetworkTotalConversions = totalConversions
	snapshot.NetworkTotalRevenue = totalRevenue
	if totalClicks > 0 {
		snapshot.NetworkAvgCVR = float64(totalConversions) / float64(totalClicks) * 100
		snapshot.NetworkAvgEPC = totalRevenue / float64(totalClicks)
	}
	snapshot.TotalClicksAnalyzed = int64(len(clicks))
	snapshot.TotalConversionsAnalyzed = int64(len(conversions))

	// Generate audience match recommendations
	snapshot.AudienceRecommendations = generateAudienceRecommendations(offers)

	return snapshot
}

// buildAudienceProfile analyzes click/conversion metadata to build an audience profile for an offer
func buildAudienceProfile(clicks []ClickRecord, conversions []ConversionRecord) *AudienceProfile {
	totalSamples := int64(len(clicks) + len(conversions))
	if totalSamples == 0 {
		return nil
	}

	profile := &AudienceProfile{
		InboxDistribution:   make(map[InboxProvider]float64),
		BrowserDistribution: make(map[string]float64),
		DeviceDistribution:  make(map[string]float64),
		OSDistribution:      make(map[string]float64),
		ISPDistribution:     make(map[string]float64),
		TotalSamples:        totalSamples,
	}

	browserCounts := make(map[string]int64)
	deviceCounts := make(map[string]int64)
	osCounts := make(map[string]int64)
	countryCounts := make(map[string]int64)
	regionCounts := make(map[string]int64)
	inboxCounts := make(map[InboxProvider]int64)
	ispCounts := make(map[string]int64)
	hourCounts := make(map[int]int64)
	dayCounts := make(map[string]int64)
	var chromiumCount int64

	// Process clicks
	for _, click := range clicks {
		browser := normalizeBrowser(click.Browser)
		browserCounts[browser]++

		device := normalizeDevice(click.DeviceType, click.Device)
		deviceCounts[device]++

		os := normalizeOS(click.OS)
		osCounts[os]++

		if click.Country != "" {
			countryCounts[click.Country]++
		}
		if click.Region != "" {
			regionCounts[click.Region]++
		}

		// Infer inbox provider from user agent + browser
		inbox := inferInboxProvider(click.Browser, click.UserAgent, click.OS, "")
		inboxCounts[inbox]++

		if isChromiumBased(click.Browser, click.UserAgent) {
			chromiumCount++
		}
	}

	// Process conversions (richer metadata)
	for _, conv := range conversions {
		browser := normalizeBrowser(conv.Browser)
		browserCounts[browser]++

		device := normalizeDevice(conv.DeviceType, "")
		deviceCounts[device]++

		os := normalizeOS(conv.Platform)
		osCounts[os]++

		if conv.Country != "" {
			countryCounts[conv.Country]++
		}
		if conv.Region != "" {
			regionCounts[conv.Region]++
		}
		if conv.ISP != "" {
			ispCounts[conv.ISP]++
		}

		// Infer inbox provider from metadata
		inbox := inferInboxProvider(conv.Browser, conv.UserAgent, conv.Platform, conv.ISP)
		inboxCounts[inbox]++

		if isChromiumBased(conv.Browser, conv.UserAgent) {
			chromiumCount++
		}

		// Timing analysis from conversions
		if conv.ConversionUnixTimestamp > 0 {
			convTime := time.Unix(conv.ConversionUnixTimestamp, 0)
			hourCounts[convTime.Hour()]++
			dayCounts[convTime.Weekday().String()]++
		}
	}

	// Calculate distributions as percentages
	for browser, count := range browserCounts {
		profile.BrowserDistribution[browser] = math.Round(float64(count)/float64(totalSamples)*10000) / 100
	}
	for device, count := range deviceCounts {
		profile.DeviceDistribution[device] = math.Round(float64(count)/float64(totalSamples)*10000) / 100
	}
	for os, count := range osCounts {
		profile.OSDistribution[os] = math.Round(float64(count)/float64(totalSamples)*10000) / 100
	}
	for inbox, count := range inboxCounts {
		profile.InboxDistribution[inbox] = math.Round(float64(count)/float64(totalSamples)*10000) / 100
	}
	for isp, count := range ispCounts {
		if count > 1 { // Only include ISPs with meaningful counts
			profile.ISPDistribution[isp] = math.Round(float64(count)/float64(totalSamples)*10000) / 100
		}
	}

	profile.ChromiumPercentage = math.Round(float64(chromiumCount)/float64(totalSamples)*10000) / 100

	// Primary inbox
	var maxInboxCount int64
	for inbox, count := range inboxCounts {
		if count > maxInboxCount {
			maxInboxCount = count
			profile.PrimaryInbox = inbox
		}
	}
	if totalSamples > 0 {
		profile.PrimaryInboxPct = math.Round(float64(maxInboxCount)/float64(totalSamples)*10000) / 100
	}

	// Primary device
	var maxDeviceCount int64
	for device, count := range deviceCounts {
		if count > maxDeviceCount {
			maxDeviceCount = count
			profile.PrimaryDevice = device
		}
	}

	// Top countries
	profile.TopCountries = topGeoDistributions(countryCounts, totalSamples, 5)
	profile.TopRegions = topGeoDistributions(regionCounts, totalSamples, 5)

	// Peak conversion hours
	profile.PeakConversionHours = topHours(hourCounts, 3)
	if len(profile.PeakConversionHours) > 0 {
		profile.PeakHourUTC = profile.PeakConversionHours[0]
	}

	// Best day of week
	var maxDayCount int64
	for day, count := range dayCounts {
		if count > maxDayCount {
			maxDayCount = count
			profile.BestDayOfWeek = day
		}
	}

	return profile
}

// inferInboxProvider uses browser, user agent, OS, and ISP to infer the likely email inbox provider
func inferInboxProvider(browser, userAgent, os, isp string) InboxProvider {
	browserLower := strings.ToLower(browser)
	uaLower := strings.ToLower(userAgent)
	osLower := strings.ToLower(os)
	ispLower := strings.ToLower(isp)

	// ISP-based inference (most reliable when available)
	if ispLower != "" {
		if strings.Contains(ispLower, "google") || strings.Contains(ispLower, "gmail") {
			return InboxGmail
		}
		if strings.Contains(ispLower, "yahoo") || strings.Contains(ispLower, "oath") || strings.Contains(ispLower, "verizon media") {
			return InboxYahoo
		}
		if strings.Contains(ispLower, "microsoft") || strings.Contains(ispLower, "outlook") {
			return InboxOutlook
		}
		if strings.Contains(ispLower, "apple") || strings.Contains(ispLower, "icloud") {
			return InboxAppleMail
		}
		if strings.Contains(ispLower, "aol") {
			return InboxAOL
		}
	}

	// User Agent / Browser-based inference
	// Outlook desktop client
	if strings.Contains(uaLower, "outlook") || strings.Contains(uaLower, "microsoft outlook") {
		return InboxOutlook
	}

	// Apple Mail on iOS/macOS (Safari WebKit with Apple device)
	if (strings.Contains(osLower, "ios") || strings.Contains(osLower, "mac")) &&
		(strings.Contains(browserLower, "safari") || strings.Contains(uaLower, "applewebkit")) &&
		!strings.Contains(uaLower, "chrome") && !strings.Contains(uaLower, "crios") {
		return InboxAppleMail
	}

	// Chrome on desktop (high correlation with Gmail)
	if isChromiumBased(browser, userAgent) {
		// Chrome on desktop is very strongly correlated with Gmail users
		if strings.Contains(osLower, "windows") || strings.Contains(osLower, "mac") || strings.Contains(osLower, "linux") {
			return InboxGmail
		}
		// Chrome on Android - also often Gmail
		if strings.Contains(osLower, "android") {
			return InboxGmail
		}
	}

	// Firefox users - less clear, but often Gmail or Yahoo
	if strings.Contains(browserLower, "firefox") {
		return InboxOther // Can't reliably infer
	}

	// Edge users - could be Outlook
	if strings.Contains(browserLower, "edge") || strings.Contains(uaLower, "edg/") {
		return InboxOutlook
	}

	// Thunderbird
	if strings.Contains(uaLower, "thunderbird") {
		return InboxOther
	}

	return InboxOther
}

// isChromiumBased checks if the browser/user agent indicates a Chromium-based browser
func isChromiumBased(browser, userAgent string) bool {
	browserLower := strings.ToLower(browser)
	uaLower := strings.ToLower(userAgent)

	chromiumBrowsers := []string{"chrome", "chromium", "brave", "opera", "vivaldi", "edge"}
	for _, cb := range chromiumBrowsers {
		if strings.Contains(browserLower, cb) || strings.Contains(uaLower, cb) {
			return true
		}
	}
	// Chromium UA pattern
	return strings.Contains(uaLower, "crios") // Chrome on iOS
}

// normalizeBrowser normalizes browser names for consistent grouping
func normalizeBrowser(browser string) string {
	if browser == "" {
		return "Unknown"
	}
	lower := strings.ToLower(browser)
	switch {
	case strings.Contains(lower, "chrome") && !strings.Contains(lower, "edge"):
		return "Chrome"
	case strings.Contains(lower, "safari") && !strings.Contains(lower, "chrome"):
		return "Safari"
	case strings.Contains(lower, "firefox"):
		return "Firefox"
	case strings.Contains(lower, "edge"):
		return "Edge"
	case strings.Contains(lower, "opera"):
		return "Opera"
	case strings.Contains(lower, "brave"):
		return "Brave"
	case strings.Contains(lower, "outlook"):
		return "Outlook"
	default:
		return browser
	}
}

// normalizeDevice normalizes device type
func normalizeDevice(deviceType, device string) string {
	if deviceType == "" && device == "" {
		return "unknown"
	}
	lower := strings.ToLower(deviceType + " " + device)
	switch {
	case strings.Contains(lower, "mobile") || strings.Contains(lower, "phone"):
		return "mobile"
	case strings.Contains(lower, "tablet") || strings.Contains(lower, "ipad"):
		return "tablet"
	case strings.Contains(lower, "desktop") || strings.Contains(lower, "pc"):
		return "desktop"
	default:
		return "desktop" // Default to desktop
	}
}

// normalizeOS normalizes OS names
func normalizeOS(os string) string {
	if os == "" {
		return "Unknown"
	}
	lower := strings.ToLower(os)
	switch {
	case strings.Contains(lower, "windows"):
		return "Windows"
	case strings.Contains(lower, "mac") || strings.Contains(lower, "osx"):
		return "macOS"
	case strings.Contains(lower, "ios") || strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad"):
		return "iOS"
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "linux"):
		return "Linux"
	default:
		return os
	}
}

// topGeoDistributions returns the top N geographic distributions
func topGeoDistributions(counts map[string]int64, total int64, n int) []GeoDistribution {
	type geoEntry struct {
		name  string
		count int64
	}
	entries := make([]geoEntry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, geoEntry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].count > entries[j].count
	})
	if len(entries) > n {
		entries = entries[:n]
	}
	result := make([]GeoDistribution, len(entries))
	for i, e := range entries {
		result[i] = GeoDistribution{
			Name:       e.name,
			Count:      e.count,
			Percentage: math.Round(float64(e.count)/float64(total)*10000) / 100,
		}
	}
	return result
}

// topHours returns the top N hours by conversion count
func topHours(counts map[int]int64, n int) []int {
	type hourEntry struct {
		hour  int
		count int64
	}
	entries := make([]hourEntry, 0, len(counts))
	for hour, count := range counts {
		entries = append(entries, hourEntry{hour, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].count > entries[j].count
	})
	if len(entries) > n {
		entries = entries[:n]
	}
	result := make([]int, len(entries))
	for i, e := range entries {
		result[i] = e.hour
	}
	return result
}

// calculateNetworkOfferScore computes an AI score for a network-wide offer
func calculateNetworkOfferScore(offer NetworkOfferIntelligence, summary EntityReportSummary) (float64, string, string) {
	var score float64
	var reasons []string

	// Factor 1: Revenue contribution across network (0-25 points)
	if summary.Revenue > 0 {
		revenueShare := (offer.NetworkRevenue / summary.Revenue) * 100
		revenueScore := math.Min(revenueShare*2.5, 25)
		score += revenueScore
		if revenueShare > 3 {
			reasons = append(reasons, fmt.Sprintf("Top %.1f%% revenue contributor network-wide", revenueShare))
		}
	}

	// Factor 2: Today's momentum (0-25 points) - what's hot RIGHT NOW
	if offer.TodayRevenue > 0 {
		if offer.RevenueTrend == "accelerating" {
			score += 25
			reasons = append(reasons, fmt.Sprintf("Revenue accelerating today (+%.0f%% vs avg)", offer.TrendPercentage))
		} else if offer.RevenueTrend == "stable" {
			score += 15
		} else {
			score += 5
		}
	}

	// Factor 3: Conversion rate vs network average (0-20 points)
	avgCVR := 0.0
	if summary.TotalClick > 0 {
		avgCVR = float64(summary.Conversions) / float64(summary.TotalClick) * 100
	}
	if offer.NetworkCVR > avgCVR*1.5 && avgCVR > 0 {
		score += 20
		reasons = append(reasons, fmt.Sprintf("CVR %.2f%% (%.1fx network average)", offer.NetworkCVR, offer.NetworkCVR/avgCVR))
	} else if offer.NetworkCVR > avgCVR && avgCVR > 0 {
		score += 12
	} else if offer.NetworkCVR > avgCVR*0.5 {
		score += 5
	}

	// Factor 4: EPC performance (0-15 points)
	avgEPC := 0.0
	if summary.TotalClick > 0 {
		avgEPC = summary.Revenue / float64(summary.TotalClick)
	}
	if offer.NetworkEPC > avgEPC*1.5 && avgEPC > 0 {
		score += 15
		reasons = append(reasons, fmt.Sprintf("EPC $%.3f (%.1fx network average)", offer.NetworkEPC, offer.NetworkEPC/avgEPC))
	} else if offer.NetworkEPC > avgEPC {
		score += 8
	}

	// Factor 5: Volume / Activity level (0-10 points)
	if offer.TodayClicks > 500 {
		score += 10
		reasons = append(reasons, "High network activity today")
	} else if offer.TodayClicks > 100 {
		score += 7
	} else if offer.TodayClicks > 20 {
		score += 3
	}

	// Factor 6: Audience profile quality (0-5 points)
	if offer.AudienceProfile != nil && offer.AudienceProfile.TotalSamples > 50 {
		score += 5
		if offer.AudienceProfile.PrimaryInboxPct > 60 {
			reasons = append(reasons, fmt.Sprintf("Strong audience signal: %.0f%% %s users",
				offer.AudienceProfile.PrimaryInboxPct, offer.AudienceProfile.PrimaryInbox))
		}
	}

	// Normalize
	score = math.Min(score, 100)
	score = math.Round(score*10) / 10

	var recommendation, reason string
	if score >= 75 {
		recommendation = "highly_recommended"
		if len(reasons) >= 2 {
			reason = strings.Join(reasons[:2], ". ") + "."
		} else if len(reasons) == 1 {
			reason = reasons[0] + "."
		} else {
			reason = "Top performer across the network."
		}
	} else if score >= 50 {
		recommendation = "recommended"
		if len(reasons) > 0 {
			reason = reasons[0] + "."
		} else {
			reason = "Good network performance."
		}
	} else if score >= 25 {
		recommendation = "neutral"
		reason = "Average network performance. Consider testing."
	} else {
		recommendation = "caution"
		reason = "Below average. Low conversion activity."
	}

	return score, recommendation, reason
}

// ============================================================================
// ISP STRATEGY INTELLIGENCE (Jarvis-generated, Opus 4.6)
// ============================================================================

// ispStrategyDescriptions provides rich, actionable strategy guidance per ISP.
// These are used by the recommendation engine to provide detailed, ISP-specific
// advice that goes far beyond "send to Gmail openers."
var ispStrategyDescriptions = map[InboxProvider]string{
	InboxGmail: "Gmail prioritises recipient engagement above all other signals. " +
		"Focus on driving opens and clicks within the first 60 minutes of delivery. " +
		"Use concise subject lines (< 50 chars), avoid image-only layouts, and embed " +
		"at least one clear CTA in the first viewport. Segment aggressively — sending " +
		"to non-openers damages domain reputation. Consider AMP for Email to boost " +
		"interactive engagement. Warm new IPs by starting with your most engaged " +
		"7-day openers before expanding volume.",

	InboxYahoo: "Yahoo (including AOL backend) weights complaint rate heavily. " +
		"Keep complaint rate below 0.1%. Include a prominent, easy-to-find " +
		"unsubscribe link near the top of the email. Register with the Yahoo " +
		"Complaint Feedback Loop (CFL) and process removals in real-time. " +
		"Send during mid-morning windows (9-11 AM recipient local time) when " +
		"users actively check mail. Avoid link shorteners; use full branded URLs. " +
		"Yahoo users respond well to personalized content with clear value propositions.",

	InboxOutlook: "Outlook / Hotmail rely on Microsoft SmartScreen and domain " +
		"authentication (SPF, DKIM, DMARC). Ensure strict DMARC alignment " +
		"(p=reject). Apply to the SNDS (Smart Network Data Services) programme " +
		"and JMRP (Junk Mail Reporting Programme). Keep HTML clean — avoid " +
		"excessive CSS, JavaScript, or form elements. Personalise subject " +
		"lines with the recipient's first name; Outlook filters respond well " +
		"to one-to-one patterns. Limit sends to 30-day engaged users for best inbox placement.",

	InboxAOL: "AOL shares Yahoo's filtering infrastructure, so complaint-rate " +
		"management is paramount. AOL's user base skews older — use larger fonts " +
		"(16px+), high-contrast buttons, and simple single-column layouts. " +
		"Keep image file sizes under 100 KB total. Re-confirm inactive AOL " +
		"addresses every 90 days to avoid spam traps that Yahoo recycles " +
		"from abandoned AOL accounts. AOL subscribers tend to engage more " +
		"during evening hours (6-9 PM EST) — schedule accordingly.",

	InboxAppleMail: "Apple Mail Privacy Protection (MPP) pre-fetches images via " +
		"relay servers, inflating open rates and masking true engagement. Do NOT " +
		"rely on open-rate metrics for Apple Mail recipients. Instead, measure " +
		"click-through rate, conversion, and reply rate. Use click-based " +
		"re-engagement flows. For iCloud addresses, ensure SPF/DKIM/DMARC " +
		"are fully aligned — Apple enforces strict authentication. Consider " +
		"using link-decoration to identify Apple Mail clients and adjust " +
		"your analytics pipeline accordingly.",
}

// audienceSegmentDescriptions maps engagement tiers to human-readable targeting descriptions
var audienceSegmentDescriptions = map[string]struct {
	label     string
	sqlHint   string
	rationale string
}{
	"7d_openers": {
		label:     "7-Day Openers",
		sqlHint:   "last_open_date >= NOW() - INTERVAL 7 DAY",
		rationale: "Highest engagement tier. These subscribers actively interact with your emails and are most likely to convert. Best for offers requiring immediate action.",
	},
	"14d_clickers": {
		label:     "14-Day Clickers",
		sqlHint:   "last_click_date >= NOW() - INTERVAL 14 DAY",
		rationale: "Active clickers who demonstrate intent beyond opens. Ideal for CPA/CPL offers where click-through to conversion is the goal.",
	},
	"30d_engaged": {
		label:     "30-Day Engaged",
		sqlHint:   "(last_open_date >= NOW() - INTERVAL 30 DAY OR last_click_date >= NOW() - INTERVAL 30 DAY)",
		rationale: "Broader engaged audience. Good for CPM volume plays and brand-awareness offers. Provides scale while maintaining reasonable deliverability.",
	},
	"new_subscribers": {
		label:     "New Subscribers (< 30 Days)",
		sqlHint:   "created_date >= NOW() - INTERVAL 30 DAY",
		rationale: "Fresh data with highest inbox placement potential. New subscribers have not yet fatigued and ISPs give new sender-subscriber relationships a grace period.",
	},
	"60d_warm": {
		label:     "60-Day Warm Audience",
		sqlHint:   "last_open_date >= NOW() - INTERVAL 60 DAY",
		rationale: "Extended warm audience for higher volume sends. Use cautiously with ISPs that weight recent engagement heavily (Gmail, Yahoo).",
	},
}

// preferredSegmentByISP determines which audience segment works best per ISP,
// based on each ISP's filtering model and deliverability characteristics.
var preferredSegmentByISP = map[InboxProvider][]string{
	InboxGmail:     {"7d_openers", "14d_clickers", "new_subscribers"},
	InboxYahoo:     {"14d_clickers", "30d_engaged", "7d_openers"},
	InboxOutlook:   {"30d_engaged", "14d_clickers", "60d_warm"},
	InboxAOL:       {"14d_clickers", "30d_engaged", "7d_openers"},
	InboxAppleMail: {"14d_clickers", "30d_engaged", "new_subscribers"},
	InboxOther:     {"30d_engaged", "14d_clickers"},
}

// generateAudienceRecommendations creates AI-driven recommendations connecting
// top network offers to specific audience segments based on metadata analysis.
//
// KEY DESIGN: Instead of generating ONE recommendation per offer (biased to primary inbox),
// this generates recommendations for EVERY significant ISP segment found in the
// audience profile. This ensures diversity across Gmail, Yahoo, Outlook, AOL, and
// Apple Mail — reflecting the real-world ISP mix of the email ecosystem.
func generateAudienceRecommendations(offers []NetworkOfferIntelligence) []AudienceMatchRecommendation {
	var recommendations []AudienceMatchRecommendation

	// Track which ISP+segment combos we've already recommended to ensure variety
	usedCombos := make(map[string]int) // "isp:segment" -> count

	for _, offer := range offers {
		if offer.AIScore < 35 || offer.AudienceProfile == nil {
			continue
		}

		// Generate recommendations for EACH ISP with meaningful presence
		for inbox, pct := range offer.AudienceProfile.InboxDistribution {
			// Skip negligible ISP segments (< 3% share)
			if pct < 3.0 {
				continue
			}

			// Get the preferred segment order for this ISP
			segments := preferredSegmentByISP[inbox]
			if len(segments) == 0 {
				segments = preferredSegmentByISP[InboxOther]
			}

			// Pick the best segment that hasn't been overused
			chosenSegment := segments[0]
			for _, seg := range segments {
				comboKey := fmt.Sprintf("%s:%s", inbox, seg)
				if usedCombos[comboKey] < 2 { // Allow max 2 recommendations per ISP+segment combo
					chosenSegment = seg
					break
				}
			}
			comboKey := fmt.Sprintf("%s:%s", inbox, chosenSegment)
			usedCombos[comboKey]++

			segInfo := audienceSegmentDescriptions[chosenSegment]

			rec := AudienceMatchRecommendation{
				OfferID:   offer.OfferID,
				OfferName: offer.OfferName,
				OfferType: offer.OfferType,
			}

			// Build rich, ISP-specific targeting
			rec.TargetAudience = fmt.Sprintf("%s %s", inboxDisplayName(inbox), segInfo.label)
			rec.TargetISP = inboxDomainHint(inbox)
			rec.TargetSegmentHint = fmt.Sprintf("(%s) AND %s",
				inboxEmailFilter(inbox), segInfo.sqlHint)

			// Build diverse, detailed match reasons
			var matchReasons []string
			var matchScore float64 = offer.AIScore * 0.4

			// ISP presence reason
			matchReasons = append(matchReasons,
				fmt.Sprintf("%.1f%% of network conversions for this offer come from %s users — "+
					"your ecosystem has this audience available", pct, inboxDisplayName(inbox)))

			// Segment rationale
			matchReasons = append(matchReasons,
				fmt.Sprintf("Targeting \"%s\": %s", segInfo.label, segInfo.rationale))

			// Performance gap awareness
			if offer.AudienceProfile.TotalSamples > 20 {
				matchReasons = append(matchReasons,
					fmt.Sprintf("Network CVR: %.2f%% | Your ecosystem can close this gap by focusing on %s "+
						"engaged subscribers where ISP reputation is strongest",
						offer.NetworkCVR, inboxDisplayName(inbox)))
			}

			// ISP-specific scoring bonus
			switch inbox {
			case InboxGmail:
				matchScore += 20
			case InboxYahoo:
				matchScore += 16
			case InboxOutlook:
				matchScore += 15
			case InboxAppleMail:
				matchScore += 12
			case InboxAOL:
				matchScore += 10
			default:
				matchScore += 5
			}

			// Volume bonus from ISP share
			matchScore += pct * 0.3

			// Trend bonus
			if offer.RevenueTrend == "accelerating" {
				matchReasons = append(matchReasons,
					fmt.Sprintf("Offer trending up +%.0f%% today — time-sensitive opportunity, prioritize this send", offer.TrendPercentage))
				matchScore += 10
			}

			// Device-aware advice
			if offer.AudienceProfile.PrimaryDevice == "mobile" &&
				offer.AudienceProfile.DeviceDistribution["mobile"] > 50 {
				matchReasons = append(matchReasons,
					fmt.Sprintf("%.0f%% of conversions are mobile — ensure responsive design, short subject lines, and large CTA buttons",
						offer.AudienceProfile.DeviceDistribution["mobile"]))
				matchScore += 3
			}

			rec.MatchReasons = matchReasons
			rec.MatchScore = math.Min(math.Round(matchScore*10)/10, 100)

			// Predictions
			rec.PredictedCVR = math.Round(offer.NetworkCVR*100) / 100
			rec.PredictedEPC = math.Round(offer.NetworkEPC*1000) / 1000

			// Confidence
			confidence := 0.25
			if offer.AudienceProfile.TotalSamples > 500 {
				confidence += 0.35
			} else if offer.AudienceProfile.TotalSamples > 100 {
				confidence += 0.2
			} else if offer.AudienceProfile.TotalSamples > 20 {
				confidence += 0.1
			}
			if pct > 20 {
				confidence += 0.15
			}
			if offer.TodayConversions > 10 {
				confidence += 0.15
			}
			rec.ConfidenceLevel = math.Min(confidence, 0.95)

			// Strategy
			rec.RecommendedStrategy = generateEcosystemStrategy(inbox, chosenSegment, offer)

			// Send time hint
			if len(offer.AudienceProfile.PeakConversionHours) > 0 {
				peakHour := offer.AudienceProfile.PeakConversionHours[0]
				estHour := (peakHour - 5 + 24) % 24
				rec.SendTimeHint = fmt.Sprintf("Peak conversions around %d:00 EST — schedule 30min before for %s inbox placement",
					estHour, inboxDisplayName(inbox))
			} else {
				rec.SendTimeHint = getDefaultSendTimeByISP(inbox)
			}

			// Creative hints - ISP-specific
			rec.CreativeHints = generateCreativeHintsForISP(offer, inbox)

			recommendations = append(recommendations, rec)
		}

		// Cap total recommendations at 25 (covering multiple offers x ISPs)
		if len(recommendations) >= 25 {
			break
		}
	}

	// Sort by match score descending
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].MatchScore > recommendations[j].MatchScore
	})

	// Limit to top 15 to keep the UI digestible while showing ISP diversity
	if len(recommendations) > 15 {
		recommendations = recommendations[:15]
	}

	return recommendations
}

// generateEcosystemStrategy creates a comprehensive, ISP-specific strategy that
// addresses the performance gap between the user's ecosystem and the network.
func generateEcosystemStrategy(inbox InboxProvider, segment string, offer NetworkOfferIntelligence) string {
	segInfo := audienceSegmentDescriptions[segment]
	ispStrategy, hasISPStrategy := ispStrategyDescriptions[inbox]

	var strategy strings.Builder

	// ISP-specific deliverability strategy
	if hasISPStrategy {
		strategy.WriteString(ispStrategy)
	} else {
		strategy.WriteString("Follow standard deliverability practices: authenticate with SPF/DKIM/DMARC, monitor bounce rates, and keep complaint rate below 0.1%.")
	}

	strategy.WriteString(" | ECOSYSTEM GAP STRATEGY: ")

	// Performance gap closure advice based on offer type
	switch offer.OfferType {
	case "CPM":
		strategy.WriteString(fmt.Sprintf("For this CPM offer, maximise open rates by sending to your %s "+
			"segment. Your ecosystem underperforms the network — close the gap by tightening your "+
			"engagement window and increasing send frequency to proven openers. "+
			"Target: match the network's %.2f%% CVR by starting with highest-engaged subscribers.",
			segInfo.label, offer.NetworkCVR))
	case "CPL":
		strategy.WriteString(fmt.Sprintf("This CPL offer converts at %.2f%% across the network. "+
			"Your ecosystem needs click-through optimisation. Send to %s with clear, "+
			"single-CTA layouts. A/B test subject lines aggressively — even 0.5%% lift "+
			"in CTR compounds significantly at volume.",
			offer.NetworkCVR, segInfo.label))
	case "CPA":
		strategy.WriteString(fmt.Sprintf("Network CPA conversion rate is %.2f%% with $%.3f EPC. "+
			"Focus your %s segment on high-intent subscribers. "+
			"Pre-qualify through engagement scoring before sending. "+
			"Quality over quantity will close the performance gap.",
			offer.NetworkCVR, offer.NetworkEPC, segInfo.label))
	default:
		strategy.WriteString(fmt.Sprintf("Target your %s segment. The network achieves %.2f%% CVR — "+
			"your ecosystem can approach this by maintaining strict list hygiene, "+
			"segmenting by ISP, and monitoring real-time deliverability signals.",
			segInfo.label, offer.NetworkCVR))
	}

	return strategy.String()
}

// generateCreativeHintsForISP provides ISP-specific content suggestions
func generateCreativeHintsForISP(offer NetworkOfferIntelligence, inbox InboxProvider) []string {
	var hints []string

	// ISP-specific creative advice
	switch inbox {
	case InboxGmail:
		hints = append(hints, "Keep subject lines under 50 characters — Gmail truncates on mobile")
		hints = append(hints, "Avoid image-only emails; Gmail's image proxy can delay rendering")
		hints = append(hints, "Use preheader text strategically — it shows prominently in Gmail's preview")
	case InboxYahoo:
		hints = append(hints, "Place unsubscribe link prominently — Yahoo penalises high complaint rates")
		hints = append(hints, "Use full branded URLs, not link shorteners — Yahoo flags shortened links")
		hints = append(hints, "Mid-morning sends (9-11 AM local) perform best with Yahoo users")
	case InboxOutlook:
		hints = append(hints, "Personalise with first name in subject — Outlook rewards 1-to-1 patterns")
		hints = append(hints, "Keep HTML simple; avoid CSS floats and JavaScript that SmartScreen flags")
		hints = append(hints, "DMARC alignment is critical — Outlook heavily weights authentication")
	case InboxAOL:
		hints = append(hints, "Use 16px+ fonts and high-contrast CTAs — AOL audience skews older")
		hints = append(hints, "Single-column layouts perform best on AOL's webmail client")
		hints = append(hints, "Evening sends (6-9 PM EST) drive higher engagement with AOL users")
	case InboxAppleMail:
		hints = append(hints, "Do NOT rely on open rates — Apple MPP inflates them artificially")
		hints = append(hints, "Measure success by clicks, conversions, and reply rate instead")
		hints = append(hints, "Dark mode rendering is critical — test both light and dark themes")
	}

	// Offer type overlay
	switch offer.OfferType {
	case "CPM":
		hints = append(hints, "Maximise open rates: compelling subject lines and engaging preheaders")
	case "CPL":
		hints = append(hints, "Clear single CTA — remove distractions that dilute click-through")
	case "CPA":
		hints = append(hints, "Urgency + social proof: limited time offers with trust signals convert best")
	}

	// Device advice
	if offer.AudienceProfile != nil && offer.AudienceProfile.DeviceDistribution["mobile"] > 50 {
		hints = append(hints, "Mobile-first design: large tap targets (44px+), short copy, stacked layout")
	}

	// Trend advice
	if offer.RevenueTrend == "accelerating" {
		hints = append(hints, "Offer is trending — increase send volume and frequency while momentum lasts")
	}

	return hints
}

// getDefaultSendTimeByISP returns ISP-specific default send time recommendations
func getDefaultSendTimeByISP(inbox InboxProvider) string {
	switch inbox {
	case InboxGmail:
		return "Best for Gmail: 10:00 AM - 12:00 PM local time (high engagement window)"
	case InboxYahoo:
		return "Best for Yahoo: 9:00 AM - 11:00 AM local time (mid-morning check-in)"
	case InboxOutlook:
		return "Best for Outlook: 8:00 AM - 10:00 AM local time (workday start)"
	case InboxAOL:
		return "Best for AOL: 6:00 PM - 9:00 PM EST (evening engagement peak)"
	case InboxAppleMail:
		return "Best for Apple Mail: 12:00 PM - 2:00 PM local time (lunch break browsing)"
	default:
		return "General best: 10:00 AM - 12:00 PM local time"
	}
}

// inboxDisplayName returns a human-readable name for an inbox provider
func inboxDisplayName(inbox InboxProvider) string {
	switch inbox {
	case InboxGmail:
		return "Gmail"
	case InboxYahoo:
		return "Yahoo Mail"
	case InboxOutlook:
		return "Microsoft Outlook/Hotmail"
	case InboxAOL:
		return "AOL Mail"
	case InboxAppleMail:
		return "Apple Mail/iCloud"
	default:
		return "Other ISPs"
	}
}

// inboxDomainHint returns domain hints for filtering
func inboxDomainHint(inbox InboxProvider) string {
	switch inbox {
	case InboxGmail:
		return "gmail.com,googlemail.com"
	case InboxYahoo:
		return "yahoo.com,ymail.com,rocketmail.com"
	case InboxOutlook:
		return "outlook.com,hotmail.com,live.com,msn.com"
	case InboxAOL:
		return "aol.com,aim.com"
	case InboxAppleMail:
		return "icloud.com,me.com,mac.com"
	default:
		return ""
	}
}

// inboxEmailFilter returns a SQL-style filter for an inbox provider
func inboxEmailFilter(inbox InboxProvider) string {
	switch inbox {
	case InboxGmail:
		return "email LIKE '%@gmail.com' OR email LIKE '%@googlemail.com'"
	case InboxYahoo:
		return "email LIKE '%@yahoo.com' OR email LIKE '%@ymail.com'"
	case InboxOutlook:
		return "email LIKE '%@outlook.com' OR email LIKE '%@hotmail.com' OR email LIKE '%@live.com'"
	case InboxAOL:
		return "email LIKE '%@aol.com' OR email LIKE '%@aim.com'"
	case InboxAppleMail:
		return "email LIKE '%@icloud.com' OR email LIKE '%@me.com'"
	default:
		return "1=1"
	}
}

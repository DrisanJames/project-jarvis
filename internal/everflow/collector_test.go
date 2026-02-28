package everflow

import (
	"testing"
	"time"
)

func TestNewCollector(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	if collector.client != client {
		t.Error("Client not set correctly")
	}
	if collector.fetchInterval != 5*time.Minute {
		t.Errorf("fetchInterval = %v, want 5m", collector.fetchInterval)
	}
	if collector.lookbackDays != 30 {
		t.Errorf("lookbackDays = %d, want 30", collector.lookbackDays)
	}
}

func TestAggregateMetrics(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	today := time.Now()
	todayStr := today.Format("2006-01-02")

	clicks := []Click{
		{
			ClickID:      "1",
			Timestamp:    today,
			OfferID:      "407",
			OfferName:    "Empire Flooring",
			PropertyCode: "TDIH",
			PropertyName: "thisdayinhistory.co",
			MailingID:    "3219537162",
			Sub1:         "TDIH_407_3926_01262026_3219537162",
		},
		{
			ClickID:      "2",
			Timestamp:    today,
			OfferID:      "407",
			OfferName:    "Empire Flooring",
			PropertyCode: "TDIH",
			PropertyName: "thisdayinhistory.co",
			MailingID:    "3219537162",
			Sub1:         "TDIH_407_3926_01262026_3219537162",
		},
		{
			ClickID:      "3",
			Timestamp:    today,
			OfferID:      "556",
			OfferName:    "Another Offer",
			PropertyCode: "FYF",
			PropertyName: "findyourfit.net",
			MailingID:    "9999999",
			Sub1:         "FYF_556_1091_01262026_9999999",
		},
	}

	conversions := []Conversion{
		{
			ConversionID:   "1",
			ConversionTime: today,
			OfferID:        "407",
			OfferName:      "Empire Flooring",
			PropertyCode:   "TDIH",
			PropertyName:   "thisdayinhistory.co",
			MailingID:      "3219537162",
			Sub1:           "TDIH_407_3926_01262026_3219537162",
			Revenue:        225.00,
			Payout:         225.00,
		},
	}

	metrics := collector.aggregateMetrics(clicks, conversions)

	// Check today's metrics
	if metrics.TodayClicks != 3 {
		t.Errorf("TodayClicks = %d, want 3", metrics.TodayClicks)
	}
	if metrics.TodayConversions != 1 {
		t.Errorf("TodayConversions = %d, want 1", metrics.TodayConversions)
	}
	if metrics.TodayRevenue != 225.00 {
		t.Errorf("TodayRevenue = %f, want 225.00", metrics.TodayRevenue)
	}

	// Check daily performance
	if len(metrics.DailyPerformance) != 1 {
		t.Fatalf("DailyPerformance length = %d, want 1", len(metrics.DailyPerformance))
	}
	daily := metrics.DailyPerformance[0]
	if daily.Date != todayStr {
		t.Errorf("DailyPerformance[0].Date = %s, want %s", daily.Date, todayStr)
	}
	if daily.Clicks != 3 {
		t.Errorf("DailyPerformance[0].Clicks = %d, want 3", daily.Clicks)
	}
	if daily.Conversions != 1 {
		t.Errorf("DailyPerformance[0].Conversions = %d, want 1", daily.Conversions)
	}

	// Check offer performance
	if len(metrics.OfferPerformance) != 2 {
		t.Fatalf("OfferPerformance length = %d, want 2", len(metrics.OfferPerformance))
	}
	// Should be sorted by revenue, so 407 should be first
	if metrics.OfferPerformance[0].OfferID != "407" {
		t.Errorf("OfferPerformance[0].OfferID = %s, want 407", metrics.OfferPerformance[0].OfferID)
	}
	if metrics.OfferPerformance[0].Revenue != 225.00 {
		t.Errorf("OfferPerformance[0].Revenue = %f, want 225.00", metrics.OfferPerformance[0].Revenue)
	}

	// Check property performance
	if len(metrics.PropertyPerformance) != 2 {
		t.Fatalf("PropertyPerformance length = %d, want 2", len(metrics.PropertyPerformance))
	}
	// TDIH should be first (has revenue)
	if metrics.PropertyPerformance[0].PropertyCode != "TDIH" {
		t.Errorf("PropertyPerformance[0].PropertyCode = %s, want TDIH", metrics.PropertyPerformance[0].PropertyCode)
	}
	if metrics.PropertyPerformance[0].Clicks != 2 {
		t.Errorf("PropertyPerformance[0].Clicks = %d, want 2", metrics.PropertyPerformance[0].Clicks)
	}

	// Check campaign revenue
	if len(metrics.CampaignRevenue) != 2 {
		t.Fatalf("CampaignRevenue length = %d, want 2", len(metrics.CampaignRevenue))
	}
	
	// First campaign should be 3219537162 (has revenue)
	campaign := metrics.CampaignRevenue[0]
	if campaign.MailingID != "3219537162" {
		t.Errorf("CampaignRevenue[0].MailingID = %s, want 3219537162", campaign.MailingID)
	}
	if campaign.CampaignName != "TDIH_407_3926_01262026_3219537162" {
		t.Errorf("CampaignRevenue[0].CampaignName = %s, want TDIH_407_3926_01262026_3219537162", campaign.CampaignName)
	}
	if campaign.Clicks != 2 {
		t.Errorf("CampaignRevenue[0].Clicks = %d, want 2", campaign.Clicks)
	}
	if campaign.Conversions != 1 {
		t.Errorf("CampaignRevenue[0].Conversions = %d, want 1", campaign.Conversions)
	}
	if campaign.Revenue != 225.00 {
		t.Errorf("CampaignRevenue[0].Revenue = %f, want 225.00", campaign.Revenue)
	}
	// Conversion rate should be 1/2 = 0.5
	expectedConvRate := 0.5
	if campaign.ConversionRate != expectedConvRate {
		t.Errorf("CampaignRevenue[0].ConversionRate = %f, want %f", campaign.ConversionRate, expectedConvRate)
	}
	// EPC should be 225/2 = 112.5
	expectedEPC := 112.5
	if campaign.EPC != expectedEPC {
		t.Errorf("CampaignRevenue[0].EPC = %f, want %f", campaign.EPC, expectedEPC)
	}
}

func TestConversionRate(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	today := time.Now()

	clicks := []Click{
		{ClickID: "1", Timestamp: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111"},
		{ClickID: "2", Timestamp: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111"},
		{ClickID: "3", Timestamp: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111"},
		{ClickID: "4", Timestamp: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111"},
	}

	conversions := []Conversion{
		{ConversionID: "1", ConversionTime: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111", Revenue: 100},
	}

	metrics := collector.aggregateMetrics(clicks, conversions)

	// 1 conversion / 4 clicks = 0.25
	expectedRate := 0.25
	if metrics.DailyPerformance[0].ConversionRate != expectedRate {
		t.Errorf("ConversionRate = %f, want %f", metrics.DailyPerformance[0].ConversionRate, expectedRate)
	}

	// EPC = 100 / 4 = 25
	expectedEPC := 25.0
	if metrics.DailyPerformance[0].EPC != expectedEPC {
		t.Errorf("EPC = %f, want %f", metrics.DailyPerformance[0].EPC, expectedEPC)
	}
}

func TestCollectorGetMethods(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	today := time.Now()
	clicks := []Click{
		{ClickID: "1", Timestamp: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111"},
	}
	conversions := []Conversion{
		{ConversionID: "1", ConversionTime: today, OfferID: "100", PropertyCode: "HRO", MailingID: "1111", Sub1: "HRO_100_01262026_1111", Revenue: 100},
	}

	// Manually set metrics
	collector.metrics = collector.aggregateMetrics(clicks, conversions)
	collector.metrics.LastFetch = today

	// Test GetDailyPerformance
	daily := collector.GetDailyPerformance()
	if len(daily) != 1 {
		t.Errorf("GetDailyPerformance length = %d, want 1", len(daily))
	}

	// Test GetOfferPerformance
	offers := collector.GetOfferPerformance()
	if len(offers) != 1 {
		t.Errorf("GetOfferPerformance length = %d, want 1", len(offers))
	}

	// Test GetPropertyPerformance
	props := collector.GetPropertyPerformance()
	if len(props) != 1 {
		t.Errorf("GetPropertyPerformance length = %d, want 1", len(props))
	}

	// Test GetCampaignRevenue
	campaigns := collector.GetCampaignRevenue()
	if len(campaigns) != 1 {
		t.Errorf("GetCampaignRevenue length = %d, want 1", len(campaigns))
	}

	// Test LastFetch
	lastFetch := collector.LastFetch()
	if lastFetch.IsZero() {
		t.Error("LastFetch should not be zero")
	}

	// Test GetTotalRevenue
	total := collector.GetTotalRevenue()
	if total != 100.0 {
		t.Errorf("GetTotalRevenue = %f, want 100.0", total)
	}
}

func TestCollectorNilMetrics(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)
	collector.metrics = nil

	// All getters should return nil when metrics is nil
	if collector.GetDailyPerformance() != nil {
		t.Error("Expected nil daily performance")
	}
	if collector.GetOfferPerformance() != nil {
		t.Error("Expected nil offer performance")
	}
	if collector.GetPropertyPerformance() != nil {
		t.Error("Expected nil property performance")
	}
	if collector.GetCampaignRevenue() != nil {
		t.Error("Expected nil campaign revenue")
	}
	if collector.GetRecentClicks() != nil {
		t.Error("Expected nil recent clicks")
	}
	if collector.GetRecentConversions() != nil {
		t.Error("Expected nil recent conversions")
	}
	if !collector.LastFetch().IsZero() {
		t.Error("Expected zero time for LastFetch")
	}
}

func TestGetCampaignRevenueByID(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	collector.metrics = &CollectorMetrics{
		CampaignRevenue: []CampaignRevenue{
			{MailingID: "1111", Revenue: 100},
			{MailingID: "2222", Revenue: 200},
		},
	}

	// Test finding existing campaign
	cr := collector.GetCampaignRevenueByID("2222")
	if cr == nil {
		t.Fatal("Expected to find campaign 2222")
	}
	if cr.Revenue != 200 {
		t.Errorf("Revenue = %f, want 200", cr.Revenue)
	}

	// Test not finding campaign
	cr = collector.GetCampaignRevenueByID("9999")
	if cr != nil {
		t.Error("Expected nil for non-existent campaign")
	}
}

func TestGetPropertyRevenueByCode(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	collector.metrics = &CollectorMetrics{
		PropertyPerformance: []PropertyPerformance{
			{PropertyCode: "HRO", Revenue: 100},
			{PropertyCode: "TDIH", Revenue: 200},
		},
	}

	// Test finding existing property
	pp := collector.GetPropertyRevenueByCode("TDIH")
	if pp == nil {
		t.Fatal("Expected to find property TDIH")
	}
	if pp.Revenue != 200 {
		t.Errorf("Revenue = %f, want 200", pp.Revenue)
	}

	// Test not finding property
	pp = collector.GetPropertyRevenueByCode("INVALID")
	if pp != nil {
		t.Error("Expected nil for non-existent property")
	}
}

func TestRecentDataLimit(t *testing.T) {
	config := Config{
		APIKey:       "test-key",
		BaseURL:      "https://api.test.com",
		AffiliateIDs: []string{"9533"},
	}
	client := NewClient(config)
	collector := NewCollector(client, 5*time.Minute, 30)

	// Create more than 100 clicks
	clicks := make([]Click, 150)
	for i := 0; i < 150; i++ {
		clicks[i] = Click{ClickID: string(rune(i))}
	}

	// Create more than 100 conversions
	conversions := make([]Conversion, 150)
	for i := 0; i < 150; i++ {
		conversions[i] = Conversion{ConversionID: string(rune(i))}
	}

	metrics := collector.aggregateMetrics(clicks, conversions)

	// Should be limited to 100
	if len(metrics.RecentClicks) != 100 {
		t.Errorf("RecentClicks length = %d, want 100", len(metrics.RecentClicks))
	}
	if len(metrics.RecentConversions) != 100 {
		t.Errorf("RecentConversions length = %d, want 100", len(metrics.RecentConversions))
	}
}

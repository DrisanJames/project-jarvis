package everflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
)

func TestNewCampaignEnricher(t *testing.T) {
	enricher := NewCampaignEnricher(nil)
	
	if enricher == nil {
		t.Fatal("Expected non-nil enricher")
	}
	
	if enricher.cache == nil {
		t.Error("Expected cache to be initialized")
	}
	
	if enricher.cacheTTL != 15*time.Minute {
		t.Errorf("Expected cacheTTL of 15 minutes, got %v", enricher.cacheTTL)
	}
}

func TestCampaignEnricher_NoClient(t *testing.T) {
	enricher := NewCampaignEnricher(nil)
	
	campaigns := []CampaignRevenue{
		{MailingID: "123", Revenue: 100},
		{MailingID: "456", Revenue: 200},
	}
	
	ctx := context.Background()
	result := enricher.EnrichCampaigns(ctx, campaigns)
	
	// Should return campaigns unchanged when no client
	if len(result) != 2 {
		t.Errorf("Expected 2 campaigns, got %d", len(result))
	}
	
	// Campaigns should not be marked as Ongage linked
	for _, c := range result {
		if c.OngageLinked {
			t.Errorf("Campaign %s should not be marked as Ongage linked", c.MailingID)
		}
	}
}

func TestCampaignEnricher_EnrichCampaigns(t *testing.T) {
	// Create mock Ongage server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/mailings/123":
			response := ongage.CampaignDetailResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: ongage.Campaign{
					ID:       "123",
					Name:     "Test Campaign 1",
					Targeted: "50000",
					Distribution: []ongage.ESPDistribution{
						{Domain: "mail.example.com"},
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/api/mailings/456":
			response := ongage.CampaignDetailResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: ongage.Campaign{
					ID:       "456",
					Name:     "Test Campaign 2",
					Targeted: "25000",
					Distribution: []ongage.ESPDistribution{
						{Domain: "mail2.example.com"},
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/api/reports/query":
			// Both campaigns get the same report data (realistic - reports API returns for specified campaign)
			// Note: Ongage API returns columns with plain names, not sum() wrappers
			response := ongage.ReportResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: []ongage.ReportRow{
					{
						"targeted":     float64(50000),
						"sent":         float64(48000),
						"success":      float64(45600),
						"opens":        float64(12000),
						"unique_opens": float64(9120),
						"clicks":       float64(2500),
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := ongage.NewClient(ongage.Config{
		BaseURL: server.URL,
	})

	enricher := NewCampaignEnricher(client)

	campaigns := []CampaignRevenue{
		{MailingID: "123", Revenue: 1000, Clicks: 100, Conversions: 10},
		{MailingID: "456", Revenue: 500, Clicks: 50, Conversions: 5},
	}

	ctx := context.Background()
	result := enricher.EnrichCampaigns(ctx, campaigns)

	if len(result) != 2 {
		t.Fatalf("Expected 2 campaigns, got %d", len(result))
	}

	// Check first campaign enrichment
	c1 := result[0]
	if !c1.OngageLinked {
		t.Error("Campaign 1 should be marked as Ongage linked")
	}
	// Audience size comes from reports API
	if c1.AudienceSize != 50000 {
		t.Errorf("Expected AudienceSize 50000, got %d", c1.AudienceSize)
	}
	// Sending domain comes from campaign details
	if c1.SendingDomain != "mail.example.com" {
		t.Errorf("Expected SendingDomain 'mail.example.com', got '%s'", c1.SendingDomain)
	}
	// Campaign name comes from campaign details
	if c1.CampaignName != "Test Campaign 1" {
		t.Errorf("Expected CampaignName 'Test Campaign 1', got '%s'", c1.CampaignName)
	}
	// Stats from reports
	if c1.Sent != 48000 {
		t.Errorf("Expected Sent 48000, got %d", c1.Sent)
	}
	if c1.Delivered != 45600 {
		t.Errorf("Expected Delivered 45600, got %d", c1.Delivered)
	}
	
	// Check ECPM calculation: (1000 / 45600) * 1000 = 21.93 (based on Delivered, not Targeted)
	expectedECPM := (1000.0 / 45600.0) * 1000.0
	ecpmDiff := c1.ECPM - expectedECPM
	if ecpmDiff < 0 {
		ecpmDiff = -ecpmDiff
	}
	if ecpmDiff > 0.001 {
		t.Errorf("Expected ECPM ~%f, got %f (diff: %f)", expectedECPM, c1.ECPM, ecpmDiff)
	}

	// Check second campaign is also linked
	c2 := result[1]
	if !c2.OngageLinked {
		t.Error("Campaign 2 should be marked as Ongage linked")
	}
	if c2.CampaignName != "Test Campaign 2" {
		t.Errorf("Expected CampaignName 'Test Campaign 2', got '%s'", c2.CampaignName)
	}
	if c2.SendingDomain != "mail2.example.com" {
		t.Errorf("Expected SendingDomain 'mail2.example.com', got '%s'", c2.SendingDomain)
	}
}

func TestCampaignEnricher_CacheHit(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/api/mailings/123" {
			response := ongage.CampaignDetailResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: ongage.Campaign{
					ID:       "123",
					Name:     "Test Campaign",
					Targeted: "10000",
				},
			}
			json.NewEncoder(w).Encode(response)
		} else if r.URL.Path == "/api/reports/query" {
			response := ongage.ReportResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload:  []ongage.ReportRow{},
			}
			json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	client := ongage.NewClient(ongage.Config{
		BaseURL: server.URL,
	})

	enricher := NewCampaignEnricher(client)

	campaigns := []CampaignRevenue{
		{MailingID: "123", Revenue: 100},
	}

	ctx := context.Background()
	
	// First call
	enricher.EnrichCampaigns(ctx, campaigns)
	firstCallCount := callCount

	// Second call should use cache
	enricher.EnrichCampaigns(ctx, campaigns)

	if callCount != firstCallCount {
		t.Errorf("Expected cache hit, but API was called again. Calls: %d", callCount)
	}
}

func TestCampaignEnricher_ClearCache(t *testing.T) {
	enricher := NewCampaignEnricher(nil)
	
	// Add something to cache
	enricher.cache["test"] = &OngageCampaignData{
		MailingID: "test",
		Found:     true,
	}
	enricher.cacheExpiry = time.Now().Add(15 * time.Minute)
	
	// Verify cache has data
	size, _, valid := enricher.GetCacheStats()
	if size != 1 || !valid {
		t.Error("Cache should have 1 item and be valid")
	}
	
	// Clear cache
	enricher.ClearCache()
	
	// Verify cache is empty
	size, _, valid = enricher.GetCacheStats()
	if size != 0 {
		t.Errorf("Expected empty cache, got size %d", size)
	}
	if valid {
		t.Error("Cache should be invalid after clear")
	}
}

func TestCampaignEnricher_GetCacheStats(t *testing.T) {
	enricher := NewCampaignEnricher(nil)
	
	// Initially empty
	size, _, valid := enricher.GetCacheStats()
	if size != 0 {
		t.Errorf("Expected size 0, got %d", size)
	}
	if valid {
		t.Error("Cache should be invalid initially")
	}
	
	// Add some data
	enricher.cache["1"] = &OngageCampaignData{MailingID: "1"}
	enricher.cache["2"] = &OngageCampaignData{MailingID: "2"}
	enricher.cacheExpiry = time.Now().Add(time.Hour)
	
	size, expiry, valid := enricher.GetCacheStats()
	if size != 2 {
		t.Errorf("Expected size 2, got %d", size)
	}
	if !valid {
		t.Error("Cache should be valid")
	}
	if expiry.IsZero() {
		t.Error("Expected non-zero expiry time")
	}
}

func TestCampaignEnricher_PreservesExistingData(t *testing.T) {
	// Test that enrichment preserves existing Everflow data
	enricher := NewCampaignEnricher(nil)
	
	campaigns := []CampaignRevenue{
		{
			MailingID:      "123",
			CampaignName:   "Original Name",
			PropertyCode:   "TDIH",
			PropertyName:   "thisdayinhistory.co",
			OfferID:        "407",
			OfferName:      "Test Offer",
			Clicks:         1000,
			Conversions:    10,
			Revenue:        225.00,
			Payout:         225.00,
			ConversionRate: 0.01,
			EPC:            0.225,
		},
	}
	
	ctx := context.Background()
	result := enricher.EnrichCampaigns(ctx, campaigns)
	
	// All original data should be preserved
	c := result[0]
	if c.MailingID != "123" {
		t.Errorf("MailingID changed: %s", c.MailingID)
	}
	if c.CampaignName != "Original Name" {
		t.Errorf("CampaignName changed: %s", c.CampaignName)
	}
	if c.PropertyCode != "TDIH" {
		t.Errorf("PropertyCode changed: %s", c.PropertyCode)
	}
	if c.Revenue != 225.00 {
		t.Errorf("Revenue changed: %f", c.Revenue)
	}
	if c.Clicks != 1000 {
		t.Errorf("Clicks changed: %d", c.Clicks)
	}
}

func TestOngageCampaignData_Fields(t *testing.T) {
	data := &OngageCampaignData{
		MailingID:     "123",
		CampaignName:  "Test Campaign",
		AudienceSize:  50000,
		Sent:          48000,
		Delivered:     45600,
		Opens:         12000,
		UniqueOpens:   9120,
		EmailClicks:   2500,
		SendingDomain: "mail.example.com",
		Found:         true,
	}
	
	if data.MailingID != "123" {
		t.Errorf("Expected MailingID '123', got '%s'", data.MailingID)
	}
	if data.AudienceSize != 50000 {
		t.Errorf("Expected AudienceSize 50000, got %d", data.AudienceSize)
	}
	if !data.Found {
		t.Error("Expected Found to be true")
	}
}

func TestCampaignEnricher_SegmentBasedAudienceSize(t *testing.T) {
	// Test that audience size is calculated from segments when Targeted is empty
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/mailings/123":
			// Campaign without Targeted field, but with segments
			response := ongage.CampaignDetailResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: ongage.Campaign{
					ID:       "123",
					Name:     "Test Campaign",
					Targeted: "", // Empty targeted
					Distribution: []ongage.ESPDistribution{
						{Domain: "mail.example.com"},
					},
					Segments: []ongage.CampaignSegment{
						{SegmentID: "1", Name: "Active Users", LastCount: "10000", Exclude: "0"},
						{SegmentID: "2", Name: "Premium Users", LastCount: "5000", Exclude: "0"},
						{SegmentID: "3", Name: "Suppression List", LastCount: "2000", Exclude: "1"}, // Should be excluded
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/api/reports/query":
			response := ongage.ReportResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: []ongage.ReportRow{
					{
						"mailing_id": float64(123),
						"sent":       float64(14000),
						"success":    float64(13300), // Delivered
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := ongage.NewClient(ongage.Config{
		BaseURL: server.URL,
	})

	enricher := NewCampaignEnricher(client)

	campaigns := []CampaignRevenue{
		{MailingID: "123", Revenue: 300},
	}

	ctx := context.Background()
	result := enricher.EnrichCampaigns(ctx, campaigns)

	if len(result) != 1 {
		t.Fatalf("Expected 1 campaign, got %d", len(result))
	}

	c := result[0]
	if !c.OngageLinked {
		t.Error("Campaign should be marked as Ongage linked")
	}
	
	// Audience size should be sum of non-suppression segments: 10000 + 5000 = 15000
	expectedAudience := int64(15000)
	if c.AudienceSize != expectedAudience {
		t.Errorf("Expected AudienceSize %d, got %d", expectedAudience, c.AudienceSize)
	}
	
	// Delivered should come from report
	if c.Delivered != 13300 {
		t.Errorf("Expected Delivered 13300, got %d", c.Delivered)
	}
	
	// ECPM should be calculated based on Delivered: (300 / 13300) * 1000
	expectedECPM := (300.0 / 13300.0) * 1000.0
	ecpmDiff := c.ECPM - expectedECPM
	if ecpmDiff < 0 {
		ecpmDiff = -ecpmDiff
	}
	if ecpmDiff > 0.001 {
		t.Errorf("Expected ECPM ~%f, got %f (diff: %f)", expectedECPM, c.ECPM, ecpmDiff)
	}
}

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

func TestEnrichmentService_GetEnrichedCampaignDetails(t *testing.T) {
	// Create mock Ongage server
	ongageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/mailings/3219537162":
			// Return mock campaign details
			response := ongage.CampaignDetailResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: ongage.Campaign{
					ID:               "3219537162",
					Name:             "01272026_TDIH_407_Empire Flooring_Segment1",
					Targeted:         "50000",
					ScheduleDate:     "2026-01-27 08:00:00",
					SendingStartDate: "2026-01-27 08:01:00",
					SendingEndDate:   "2026-01-27 10:30:00",
					Status:           "60004",
					StatusDesc:       "Completed",
					Segments: []ongage.CampaignSegment{
						{SegmentID: "1001", Name: "TDIH Active 30 Days", LastCount: "45000", Exclude: "0"},
						{SegmentID: "1002", Name: "Global Suppression", LastCount: "5000", Exclude: "1"},
					},
					Distribution: []ongage.ESPDistribution{
						{ESPID: "4", ESPConnectionID: "100", Domain: "mail.thisdayinhistory.co", Percent: "100"},
					},
					EmailMessages: []ongage.EmailMessage{
						{EmailMessageID: "5001", Name: "Empire Flooring", Subject: "Transform Your Home with New Floors - Special Offer Inside!"},
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/api/reports/query":
			// Return mock stats
			response := ongage.ReportResponse{
				Metadata: ongage.ResponseMetadata{Error: false},
				Payload: []ongage.ReportRow{
					{
						"sent":          float64(48000),
						"success":       float64(45600),
						"opens":         float64(12000),
						"unique_opens":  float64(9120),
						"clicks":        float64(2500),
						"unique_clicks": float64(1824),
						"hard_bounces":  float64(200),
						"soft_bounces":  float64(100),
						"unsubscribes":  float64(45),
						"complaints":    float64(5),
					},
				},
			}
			json.NewEncoder(w).Encode(response)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ongageServer.Close()

	// Create Ongage client
	ongageClient := ongage.NewClient(ongage.Config{
		BaseURL:     ongageServer.URL,
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	})

	// Create mock Everflow collector with test data
	collector := &Collector{
		metrics: &CollectorMetrics{
			CampaignRevenue: []CampaignRevenue{
				{
					MailingID:    "3219537162",
					PropertyCode: "TDIH",
					PropertyName: "thisdayinhistory.co",
					OfferID:      "407",
					OfferName:    "Empire Flooring CPL",
					Clicks:       1500,
					Conversions:  15,
					Revenue:      3375.00,
					Payout:       3375.00,
				},
			},
		},
	}

	// Create enrichment service
	service := NewEnrichmentService(collector, ongageClient)

	// Test getting enriched details
	ctx := context.Background()
	details, err := service.GetEnrichedCampaignDetails(ctx, "3219537162")

	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify Everflow data
	if details.MailingID != "3219537162" {
		t.Errorf("Expected MailingID '3219537162', got '%s'", details.MailingID)
	}
	if details.PropertyCode != "TDIH" {
		t.Errorf("Expected PropertyCode 'TDIH', got '%s'", details.PropertyCode)
	}
	if details.Revenue != 3375.00 {
		t.Errorf("Expected Revenue 3375.00, got %f", details.Revenue)
	}

	// Verify Ongage data
	if !details.OngageLinked {
		t.Error("Expected OngageLinked to be true")
	}
	if details.CampaignName != "01272026_TDIH_407_Empire Flooring_Segment1" {
		t.Errorf("Expected CampaignName, got '%s'", details.CampaignName)
	}
	if details.AudienceSize != 50000 {
		t.Errorf("Expected AudienceSize 50000, got %d", details.AudienceSize)
	}
	if details.SendingDomain != "mail.thisdayinhistory.co" {
		t.Errorf("Expected SendingDomain 'mail.thisdayinhistory.co', got '%s'", details.SendingDomain)
	}
	if details.Subject != "Transform Your Home with New Floors - Special Offer Inside!" {
		t.Errorf("Expected Subject, got '%s'", details.Subject)
	}

	// Verify segments
	if len(details.SendingSegments) != 1 {
		t.Errorf("Expected 1 sending segment, got %d", len(details.SendingSegments))
	}
	if len(details.SuppressionSegments) != 1 {
		t.Errorf("Expected 1 suppression segment, got %d", len(details.SuppressionSegments))
	}

	// Verify stats
	if details.Sent != 48000 {
		t.Errorf("Expected Sent 48000, got %d", details.Sent)
	}
	if details.Delivered != 45600 {
		t.Errorf("Expected Delivered 45600, got %d", details.Delivered)
	}
	if details.UniqueOpens != 9120 {
		t.Errorf("Expected UniqueOpens 9120, got %d", details.UniqueOpens)
	}

	// Verify calculated metrics
	// eCPM = (3375.00 / 50000) * 1000 = 67.50
	expectedECPM := 67.50
	if details.ECPM < expectedECPM-0.01 || details.ECPM > expectedECPM+0.01 {
		t.Errorf("Expected ECPM ~%f, got %f", expectedECPM, details.ECPM)
	}

	// Conversion rate = 15 / 1500 = 0.01
	expectedConvRate := 0.01
	if details.ConversionRate < expectedConvRate-0.001 || details.ConversionRate > expectedConvRate+0.001 {
		t.Errorf("Expected ConversionRate ~%f, got %f", expectedConvRate, details.ConversionRate)
	}
}

func TestEnrichmentService_CacheHit(t *testing.T) {
	// Create mock servers
	callCount := 0
	ongageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer ongageServer.Close()

	ongageClient := ongage.NewClient(ongage.Config{
		BaseURL: ongageServer.URL,
	})

	collector := &Collector{
		metrics: &CollectorMetrics{
			CampaignRevenue: []CampaignRevenue{
				{MailingID: "123", Revenue: 100.00, Clicks: 10, Conversions: 1},
			},
		},
	}

	service := NewEnrichmentService(collector, ongageClient)
	ctx := context.Background()

	// First call should hit the API
	_, err := service.GetEnrichedCampaignDetails(ctx, "123")
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}
	firstCallCount := callCount

	// Second call should hit the cache
	_, err = service.GetEnrichedCampaignDetails(ctx, "123")
	if err != nil {
		t.Fatalf("Second call failed: %v", err)
	}

	if callCount != firstCallCount {
		t.Errorf("Expected cache hit, but API was called again. Call count: %d", callCount)
	}
}

func TestEnrichmentService_NotFoundInEverflow(t *testing.T) {
	collector := &Collector{
		metrics: &CollectorMetrics{
			CampaignRevenue: []CampaignRevenue{},
		},
	}

	service := NewEnrichmentService(collector, nil)
	ctx := context.Background()

	_, err := service.GetEnrichedCampaignDetails(ctx, "nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent campaign")
	}
}

func TestEnrichmentService_OngageNotConfigured(t *testing.T) {
	collector := &Collector{
		metrics: &CollectorMetrics{
			CampaignRevenue: []CampaignRevenue{
				{MailingID: "123", Revenue: 100.00, Clicks: 10, Conversions: 1},
			},
		},
	}

	// Create service without Ongage client
	service := NewEnrichmentService(collector, nil)
	ctx := context.Background()

	details, err := service.GetEnrichedCampaignDetails(ctx, "123")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if details.OngageLinked {
		t.Error("Expected OngageLinked to be false when client not configured")
	}
	if details.LinkError == "" {
		t.Error("Expected LinkError to be set")
	}
}

func TestEnrichmentService_ClearCache(t *testing.T) {
	collector := &Collector{
		metrics: &CollectorMetrics{
			CampaignRevenue: []CampaignRevenue{
				{MailingID: "123", Revenue: 100.00},
			},
		},
	}

	service := NewEnrichmentService(collector, nil)
	
	// Add to cache manually
	service.cache["123"] = &EnrichedCampaignDetails{MailingID: "123"}
	service.cacheExpiry["123"] = time.Now().Add(5 * time.Minute)

	// Verify cache has entry
	if _, ok := service.GetCachedDetails("123"); !ok {
		t.Error("Expected cache to have entry")
	}

	// Clear cache
	service.ClearCache()

	// Verify cache is empty
	if _, ok := service.GetCachedDetails("123"); ok {
		t.Error("Expected cache to be empty after clear")
	}
}

func TestSegmentInfo_IsSuppression(t *testing.T) {
	sending := SegmentInfo{
		SegmentID:     "1",
		Name:          "Active Users",
		IsSuppression: false,
	}

	suppression := SegmentInfo{
		SegmentID:     "2",
		Name:          "Global Suppression",
		IsSuppression: true,
	}

	if sending.IsSuppression {
		t.Error("Sending segment should not be suppression")
	}

	if !suppression.IsSuppression {
		t.Error("Suppression segment should be suppression")
	}
}

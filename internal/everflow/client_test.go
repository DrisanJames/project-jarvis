package everflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	config := Config{
		APIKey:       "test-api-key",
		BaseURL:      "https://api.test.com",
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: []string{"9533", "9572"},
		Enabled:      true,
	}

	client := NewClient(config)

	if client.apiKey != config.APIKey {
		t.Errorf("apiKey = %s, want %s", client.apiKey, config.APIKey)
	}
	if client.baseURL != config.BaseURL {
		t.Errorf("baseURL = %s, want %s", client.baseURL, config.BaseURL)
	}
	if client.timezoneID != config.TimezoneID {
		t.Errorf("timezoneID = %d, want %d", client.timezoneID, config.TimezoneID)
	}
	if client.currencyID != config.CurrencyID {
		t.Errorf("currencyID = %s, want %s", client.currencyID, config.CurrencyID)
	}
	if len(client.affiliateIDs) != len(config.AffiliateIDs) {
		t.Errorf("affiliateIDs length = %d, want %d", len(client.affiliateIDs), len(config.AffiliateIDs))
	}
}

func TestGetClicks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if r.Header.Get("X-Eflow-API-Key") == "" {
			t.Error("Missing X-Eflow-API-Key header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("Missing or incorrect Content-Type header")
		}

		// Return mock response
		clicks := []ClickRecord{
			{
				ClickID:       "click123",
				TransactionID: "trans456",
				AffiliateID:   "9533",
				AffiliateName: "Ignite Media",
				OfferID:       "407",
				OfferName:     "Empire Flooring CPL",
				Sub1:          "TDIH_407_3926_01262026_3219537162",
				Sub2:          "IGN_",
				Timestamp:     "01/26/2026 16:20:00 PST",
				IPAddress:     "174.233.175.209",
				Device:        "iOS 26.2",
				Browser:       "Safari",
				Country:       "United States",
				Region:        "Illinois",
				City:          "Bourbonnais",
			},
		}
		json.NewEncoder(w).Encode(clicks)
	}))
	defer server.Close()

	client := NewClient(Config{
		APIKey:       "test-key",
		BaseURL:      server.URL,
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: []string{"9533", "9572"},
	})

	clicks, err := client.GetClicks(context.Background(), "2026-01-26 00:00:00", "2026-01-26 23:59:59", nil)
	if err != nil {
		t.Fatalf("GetClicks failed: %v", err)
	}

	if len(clicks) != 1 {
		t.Errorf("Expected 1 click, got %d", len(clicks))
	}

	if clicks[0].ClickID != "click123" {
		t.Errorf("ClickID = %s, want click123", clicks[0].ClickID)
	}
	if clicks[0].Sub1 != "TDIH_407_3926_01262026_3219537162" {
		t.Errorf("Sub1 = %s, want TDIH_407_3926_01262026_3219537162", clicks[0].Sub1)
	}
}

func TestGetConversions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conversions := []ConversionRecord{
			{
				ConversionID:  "conv789",
				TransactionID: "trans456",
				ClickID:       "click123",
				AffiliateID:   "9572",
				AffiliateName: "Ignite Media Internal Email 4",
				OfferID:       "407",
			OfferName:               "Empire Flooring CPL (2950)",
			Status:                  "approved",
			EventName:               "Base",
			Revenue:                 225.00,
			Payout:                  225.00,
			RevenueType:             "rpa",
			PayoutType:              "cpa",
			Currency:                "USD",
			Sub1:                    "TDIH_407_3926_01262026_3219537162",
			ConversionUnixTimestamp: 1769504773, // 01/27/2026 00:06:13
			ClickUnixTimestamp:      1769476800, // 01/26/2026 16:20:00
			ConversionUserIP:        "34.73.83.118",
			DeviceType:              "Mobile",
			Browser:                 "Safari",
			Country:                 "United States",
			Region:                  "Illinois",
			City:                    "Bourbonnais",
		},
		}
		json.NewEncoder(w).Encode(conversions)
	}))
	defer server.Close()

	client := NewClient(Config{
		APIKey:       "test-key",
		BaseURL:      server.URL,
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: []string{"9533", "9572"},
	})

	conversions, err := client.GetConversions(context.Background(), "2026-01-27", "2026-01-27", nil, true)
	if err != nil {
		t.Fatalf("GetConversions failed: %v", err)
	}

	if len(conversions) != 1 {
		t.Errorf("Expected 1 conversion, got %d", len(conversions))
	}

	if conversions[0].Revenue != 225.00 {
		t.Errorf("Revenue = %f, want 225.00", conversions[0].Revenue)
	}
	if conversions[0].Status != "approved" {
		t.Errorf("Status = %s, want approved", conversions[0].Status)
	}
}

func TestProcessClickRecord(t *testing.T) {
	record := ClickRecord{
		ClickID:       "click123",
		TransactionID: "trans456",
		AffiliateID:   "9533",
		AffiliateName: "Ignite Media",
		OfferID:       "407",
		OfferName:     "Empire Flooring CPL",
		Sub1:          "TDIH_407_3926_01262026_3219537162",
		Sub2:          "IGN_",
		Timestamp:     "2026-01-26 16:20:00",
		IPAddress:     "174.233.175.209",
		Device:        "iOS",
		Browser:       "Safari",
		Country:       "United States",
	}

	click := ProcessClickRecord(record)

	if click.ClickID != record.ClickID {
		t.Errorf("ClickID = %s, want %s", click.ClickID, record.ClickID)
	}
	if click.PropertyCode != "TDIH" {
		t.Errorf("PropertyCode = %s, want TDIH", click.PropertyCode)
	}
	if click.PropertyName != "thisdayinhistory.co" {
		t.Errorf("PropertyName = %s, want thisdayinhistory.co", click.PropertyName)
	}
	if click.MailingID != "3219537162" {
		t.Errorf("MailingID = %s, want 3219537162", click.MailingID)
	}
	if click.ParsedOfferID != "407" {
		t.Errorf("ParsedOfferID = %s, want 407", click.ParsedOfferID)
	}
}

func TestProcessConversionRecord(t *testing.T) {
	record := ConversionRecord{
		ConversionID:            "conv789",
		TransactionID:           "trans456",
		ClickID:                 "click123",
		OfferID:                 "407",
		OfferName:               "Empire Flooring CPL",
		Status:                  "approved",
		Revenue:                 225.00,
		Payout:                  225.00,
		Sub1:                    "TDIH_407_3926_01262026_3219537162",
		ConversionUnixTimestamp: 1769504773, // 2026-01-27 00:06:13
		ClickUnixTimestamp:      1769476800, // 2026-01-26 16:20:00
	}

	conv := ProcessConversionRecord(record)

	if conv.ConversionID != record.ConversionID {
		t.Errorf("ConversionID = %s, want %s", conv.ConversionID, record.ConversionID)
	}
	if conv.Revenue != 225.00 {
		t.Errorf("Revenue = %f, want 225.00", conv.Revenue)
	}
	if conv.PropertyCode != "TDIH" {
		t.Errorf("PropertyCode = %s, want TDIH", conv.PropertyCode)
	}
	if conv.MailingID != "3219537162" {
		t.Errorf("MailingID = %s, want 3219537162", conv.MailingID)
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer server.Close()

	client := NewClient(Config{
		APIKey:  "bad-key",
		BaseURL: server.URL,
	})

	_, err := client.GetClicks(context.Background(), "2026-01-27 00:00:00", "2026-01-27 23:59:59", nil)
	if err == nil {
		t.Error("Expected error for unauthorized request, got nil")
	}
}

func TestGetClicksForDate(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		json.NewEncoder(w).Encode([]ClickRecord{})
	}))
	defer server.Close()

	client := NewClient(Config{
		APIKey:       "test-key",
		BaseURL:      server.URL,
		AffiliateIDs: []string{"9533"},
	})

	date := time.Date(2026, 1, 27, 0, 0, 0, 0, time.UTC)
	_, err := client.GetClicksForDate(context.Background(), date)
	if err != nil {
		t.Errorf("GetClicksForDate failed: %v", err)
	}
	if requestCount != 1 {
		t.Errorf("Expected 1 request, got %d", requestCount)
	}
}

func TestGetConversionsForDate(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		json.NewEncoder(w).Encode([]ConversionRecord{})
	}))
	defer server.Close()

	client := NewClient(Config{
		APIKey:       "test-key",
		BaseURL:      server.URL,
		TimezoneID:   90,
		CurrencyID:   "USD",
		AffiliateIDs: []string{"9533"},
	})

	date := time.Date(2026, 1, 27, 0, 0, 0, 0, time.UTC)
	_, err := client.GetConversionsForDate(context.Background(), date, true)
	if err != nil {
		t.Errorf("GetConversionsForDate failed: %v", err)
	}
	if requestCount != 1 {
		t.Errorf("Expected 1 request, got %d", requestCount)
	}
}

func TestProcessClicks(t *testing.T) {
	records := []ClickRecord{
		{ClickID: "1", Sub1: "FYF_100_200_01012026_1111111"},
		{ClickID: "2", Sub1: "HRO_101_201_01012026_2222222"},
	}

	clicks := ProcessClicks(records)

	if len(clicks) != 2 {
		t.Fatalf("Expected 2 clicks, got %d", len(clicks))
	}
	if clicks[0].PropertyCode != "FYF" {
		t.Errorf("clicks[0].PropertyCode = %s, want FYF", clicks[0].PropertyCode)
	}
	if clicks[1].PropertyCode != "HRO" {
		t.Errorf("clicks[1].PropertyCode = %s, want HRO", clicks[1].PropertyCode)
	}
}

func TestProcessConversions(t *testing.T) {
	records := []ConversionRecord{
		{ConversionID: "1", Revenue: 100.00, Sub1: "FYF_100_200_01012026_1111111"},
		{ConversionID: "2", Revenue: 200.00, Sub1: "HRO_101_201_01012026_2222222"},
	}

	conversions := ProcessConversions(records)

	if len(conversions) != 2 {
		t.Fatalf("Expected 2 conversions, got %d", len(conversions))
	}
	if conversions[0].Revenue != 100.00 {
		t.Errorf("conversions[0].Revenue = %f, want 100.00", conversions[0].Revenue)
	}
	if conversions[1].Revenue != 200.00 {
		t.Errorf("conversions[1].Revenue = %f, want 200.00", conversions[1].Revenue)
	}
}

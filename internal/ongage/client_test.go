package ongage

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
		BaseURL:     "https://api.ongage.net/12345",
		Username:    "testuser",
		Password:    "testpass",
		AccountCode: "test_account",
		ListID:      "1001",
	}

	client := NewClient(config)

	if client.baseURL != config.BaseURL {
		t.Errorf("Expected baseURL %s, got %s", config.BaseURL, client.baseURL)
	}
	if client.username != config.Username {
		t.Errorf("Expected username %s, got %s", config.Username, client.username)
	}
	if client.accountCode != config.AccountCode {
		t.Errorf("Expected accountCode %s, got %s", config.AccountCode, client.accountCode)
	}
}

func TestGetCampaigns(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if r.Header.Get("X_USERNAME") == "" {
			t.Error("Missing X_USERNAME header")
		}
		if r.Header.Get("X_PASSWORD") == "" {
			t.Error("Missing X_PASSWORD header")
		}
		if r.Header.Get("X_ACCOUNT_CODE") == "" {
			t.Error("Missing X_ACCOUNT_CODE header")
		}

		// Return mock response
		response := CampaignListResponse{
			Metadata: ResponseMetadata{Error: false, Total: "2"},
			Payload: []Campaign{
				{
					ID:           "123456",
					Name:         "Test Campaign 1",
					Status:       StatusCompleted,
					StatusDesc:   "Completed",
					ScheduleDate: "1704067200",
					ListID:       "1001",
					IsTest:       "0",
					ESPs:         "SparkPost",
				},
				{
					ID:           "123457",
					Name:         "Test Campaign 2",
					Status:       StatusScheduled,
					StatusDesc:   "Scheduled",
					ScheduleDate: "1704153600",
					ListID:       "1001",
					IsTest:       "0",
					ESPs:         "Mailgun",
				},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:     server.URL,
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	})

	campaigns, err := client.GetCampaigns(context.Background(), nil, nil, 100, 0)
	if err != nil {
		t.Fatalf("GetCampaigns failed: %v", err)
	}

	if len(campaigns) != 2 {
		t.Errorf("Expected 2 campaigns, got %d", len(campaigns))
	}

	if campaigns[0].Name != "Test Campaign 1" {
		t.Errorf("Expected campaign name 'Test Campaign 1', got '%s'", campaigns[0].Name)
	}

	if campaigns[1].Status != StatusScheduled {
		t.Errorf("Expected status %s, got %s", StatusScheduled, campaigns[1].Status)
	}
}

func TestGetCampaign(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := CampaignDetailResponse{
			Metadata: ResponseMetadata{Error: false},
			Payload: Campaign{
				ID:           "123456",
				Name:         "Detailed Campaign",
				Status:       StatusCompleted,
				ScheduleDate: "1704067200",
				EmailMessages: []EmailMessage{
					{
						EmailMessageID: "999",
						Name:           "Email Creative",
						Subject:        "Check out our latest deals!",
					},
				},
				Distribution: []ESPDistribution{
					{
						ESPID:           ESPIDSparkPost,
						ESPConnectionID: "5001",
						Percent:         "100",
						Name:            "SparkPost",
					},
				},
				Segments: []CampaignSegment{
					{
						SegmentID: "2001",
						Name:      "Active Users",
						Type:      "Active",
					},
				},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:     server.URL,
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	})

	campaign, err := client.GetCampaign(context.Background(), "123456")
	if err != nil {
		t.Fatalf("GetCampaign failed: %v", err)
	}

	if campaign.Name != "Detailed Campaign" {
		t.Errorf("Expected name 'Detailed Campaign', got '%s'", campaign.Name)
	}

	if len(campaign.EmailMessages) != 1 {
		t.Errorf("Expected 1 email message, got %d", len(campaign.EmailMessages))
	}

	if campaign.EmailMessages[0].Subject != "Check out our latest deals!" {
		t.Errorf("Unexpected subject: %s", campaign.EmailMessages[0].Subject)
	}
}

func TestQueryReports(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify POST method
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", r.Method)
		}

		response := ReportResponse{
			Metadata: ResponseMetadata{Error: false},
			Payload: []ReportRow{
				{
					"mailing_id":     "123",
					"mailing_name":   "Campaign A",
					"sum(`sent`)":    float64(10000),
					"sum(`success`)": float64(9800),
					"sum(`opens`)":   float64(2500),
				},
				{
					"mailing_id":     "124",
					"mailing_name":   "Campaign B",
					"sum(`sent`)":    float64(15000),
					"sum(`success`)": float64(14500),
					"sum(`opens`)":   float64(3800),
				},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:     server.URL,
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	})

	query := ReportQuery{
		Select: []interface{}{"mailing_id", "mailing_name", "sum(`sent`)", "sum(`success`)"},
		From:   "mailing",
		Filter: [][]interface{}{{"stats_date", ">=", "2024-01-01"}},
	}

	rows, err := client.QueryReports(context.Background(), query)
	if err != nil {
		t.Fatalf("QueryReports failed: %v", err)
	}

	if len(rows) != 2 {
		t.Errorf("Expected 2 rows, got %d", len(rows))
	}

	if getStringValue(rows[0], "mailing_name") != "Campaign A" {
		t.Errorf("Unexpected mailing_name: %s", getStringValue(rows[0], "mailing_name"))
	}

	sent := getInt64Value(rows[0], "sum(`sent`)")
	if sent != 10000 {
		t.Errorf("Expected sent=10000, got %d", sent)
	}
}

func TestGetESPConnections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := ESPConnectionResponse{
			Metadata: ResponseMetadata{Error: false},
			Payload: []ESPConnection{
				{ID: "5001", ESPID: ESPIDSparkPost, Name: "SparkPost", Active: "1"},
				{ID: "5002", ESPID: ESPIDMailgun, Name: "Mailgun", Active: "1"},
				{ID: "5003", ESPID: ESPIDAmazonSES, Name: "Amazon SES", Active: "1"},
			},
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:     server.URL,
		Username:    "test",
		Password:    "test",
		AccountCode: "test",
	})

	connections, err := client.GetESPConnections(context.Background(), true)
	if err != nil {
		t.Fatalf("GetESPConnections failed: %v", err)
	}

	if len(connections) != 3 {
		t.Errorf("Expected 3 connections, got %d", len(connections))
	}

	if connections[0].ESPID.String() != ESPIDSparkPost {
		t.Errorf("Expected ESP ID %s, got %s", ESPIDSparkPost, connections[0].ESPID.String())
	}
}

func TestParseUnixTimestamp(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Time
		hasError bool
	}{
		{"1704067200", time.Unix(1704067200, 0), false},
		{"0", time.Time{}, false},
		{"", time.Time{}, false},
		{"2024-01-01 12:00:00", time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), false},
	}

	for _, tc := range tests {
		result, err := ParseUnixTimestamp(tc.input)
		if tc.hasError && err == nil {
			t.Errorf("Expected error for input '%s'", tc.input)
		}
		if !tc.hasError && tc.input != "" && tc.input != "0" {
			if result.Unix() != tc.expected.Unix() {
				t.Errorf("For input '%s', expected %v, got %v", tc.input, tc.expected, result)
			}
		}
	}
}

func TestGetStatusDescription(t *testing.T) {
	tests := []struct {
		code     string
		expected string
	}{
		{StatusNew, "New"},
		{StatusScheduled, "Scheduled"},
		{StatusInProgress, "In Progress"},
		{StatusCompleted, "Completed"},
		{StatusError, "Error"},
		{"99999", "Unknown"},
	}

	for _, tc := range tests {
		result := GetStatusDescription(tc.code)
		if result != tc.expected {
			t.Errorf("For code %s, expected '%s', got '%s'", tc.code, tc.expected, result)
		}
	}
}

func TestGetESPName(t *testing.T) {
	tests := []struct {
		id       string
		expected string
	}{
		{ESPIDAmazonSES, "Amazon SES"},
		{ESPIDMailgun, "Mailgun"},
		{ESPIDSparkPost, "SparkPost"},
		{"999", "Unknown ESP"},
	}

	for _, tc := range tests {
		result := GetESPName(tc.id)
		if result != tc.expected {
			t.Errorf("For ID %s, expected '%s', got '%s'", tc.id, tc.expected, result)
		}
	}
}

func TestMapESPToProvider(t *testing.T) {
	tests := []struct {
		espID    string
		expected string
	}{
		{ESPIDAmazonSES, "ses"},
		{ESPIDMailgun, "mailgun"},
		{ESPIDSparkPost, "sparkpost"},
		{ESPIDSparkPostEnterprise, "sparkpost"},
		{"999", "unknown"},
	}

	for _, tc := range tests {
		result := MapESPToProvider(tc.espID)
		if result != tc.expected {
			t.Errorf("For ESP ID %s, expected '%s', got '%s'", tc.espID, tc.expected, result)
		}
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Unauthorized"}`))
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:     server.URL,
		Username:    "bad",
		Password:    "creds",
		AccountCode: "test",
	})

	_, err := client.GetCampaigns(context.Background(), nil, nil, 0, 0)
	if err == nil {
		t.Error("Expected error for unauthorized request")
	}
}

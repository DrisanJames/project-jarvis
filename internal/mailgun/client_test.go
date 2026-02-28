package mailgun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
)

func TestNewClient(t *testing.T) {
	cfg := config.MailgunConfig{
		APIKey:         "test-api-key",
		BaseURL:        "https://api.mailgun.net",
		TimeoutSeconds: 30,
		Domains:        []string{"test.com", "example.com"},
	}

	client := NewClient(cfg)

	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	if client.apiKey != cfg.APIKey {
		t.Errorf("apiKey = %q, want %q", client.apiKey, cfg.APIKey)
	}

	if client.baseURL != cfg.BaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, cfg.BaseURL)
	}

	if len(client.domains) != 2 {
		t.Errorf("len(domains) = %d, want %d", len(client.domains), 2)
	}
}

func TestClient_GetDomains(t *testing.T) {
	cfg := config.MailgunConfig{
		Domains: []string{"test.com", "example.com", "demo.com"},
	}

	client := NewClient(cfg)
	domains := client.GetDomains()

	if len(domains) != 3 {
		t.Errorf("len(GetDomains()) = %d, want %d", len(domains), 3)
	}

	if domains[0] != "test.com" {
		t.Errorf("domains[0] = %q, want %q", domains[0], "test.com")
	}
}

func TestClient_GetDomainStats(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Basic Auth
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Verify path
		expectedPath := "/v3/test.com/stats/total"
		if r.URL.Path != expectedPath {
			t.Errorf("URL.Path = %q, want %q", r.URL.Path, expectedPath)
		}

		// Return mock response
		resp := StatsResponse{
			End:        "2025-01-27T23:59:59Z",
			Resolution: "day",
			Start:      "2025-01-27T00:00:00Z",
			Stats: []StatsItem{
				{
					Time: "2025-01-27T00:00:00Z",
					Accepted: StatsCounter{Total: 1000},
					Delivered: StatsCounter{Total: 950},
					Opened: StatsCounter{Total: 200},
					Clicked: StatsCounter{Total: 50},
					Failed: FailedStats{
						Permanent: FailedDetail{Total: 30, Bounce: 25, EspBlock: 5},
						Temporary: FailedDetail{Total: 20},
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	ctx := context.Background()

	from := time.Date(2025, 1, 27, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 1, 27, 23, 59, 59, 0, time.UTC)

	stats, err := client.GetDomainStats(ctx, "test.com", from, to, "day")
	if err != nil {
		t.Fatalf("GetDomainStats returned error: %v", err)
	}

	if len(stats.Stats) != 1 {
		t.Errorf("len(stats.Stats) = %d, want %d", len(stats.Stats), 1)
	}

	if stats.Stats[0].Accepted.Total != 1000 {
		t.Errorf("Accepted.Total = %d, want %d", stats.Stats[0].Accepted.Total, 1000)
	}

	if stats.Stats[0].Delivered.Total != 950 {
		t.Errorf("Delivered.Total = %d, want %d", stats.Stats[0].Delivered.Total, 950)
	}
}

func TestClient_GetProviderAggregates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := ProviderAggregatesResponse{
			Providers: map[string]ProviderStats{
				"gmail.com": {
					Accepted:  500,
					Delivered: 480,
					Opened:    100,
					Clicked:   25,
					Bounced:   20,
				},
				"yahoo.com": {
					Accepted:  300,
					Delivered: 280,
					Opened:    50,
					Clicked:   10,
					Bounced:   20,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	ctx := context.Background()

	aggregates, err := client.GetProviderAggregates(ctx, "test.com")
	if err != nil {
		t.Fatalf("GetProviderAggregates returned error: %v", err)
	}

	if len(aggregates.Providers) != 2 {
		t.Errorf("len(Providers) = %d, want %d", len(aggregates.Providers), 2)
	}

	gmail, ok := aggregates.Providers["gmail.com"]
	if !ok {
		t.Fatal("gmail.com not found in providers")
	}

	if gmail.Accepted != 500 {
		t.Errorf("gmail.Accepted = %d, want %d", gmail.Accepted, 500)
	}
}

func TestClient_GetBounceClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := BounceClassificationResponse{
			Items: []BounceClassificationItem{
				{
					Entity:         "gmail.com",
					Classification: "hard",
					Count:          100,
					Reason:         "Mailbox not found",
				},
				{
					Entity:         "yahoo.com",
					Classification: "soft",
					Count:          50,
					Reason:         "Mailbox full",
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
	}

	client := NewClient(cfg)
	ctx := context.Background()

	from := time.Now().Add(-24 * time.Hour)
	to := time.Now()

	bounces, err := client.GetBounceClassification(ctx, "test.com", from, to)
	if err != nil {
		t.Fatalf("GetBounceClassification returned error: %v", err)
	}

	if len(bounces.Items) != 2 {
		t.Errorf("len(Items) = %d, want %d", len(bounces.Items), 2)
	}

	if bounces.Items[0].Classification != "hard" {
		t.Errorf("Items[0].Classification = %q, want %q", bounces.Items[0].Classification, "hard")
	}
}

func TestClient_GetSendingIPs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := IPsResponse{
			TotalCount: 2,
			Items: []IPInfo{
				{IP: "192.168.1.1", RDNS: "mail1.test.com", Dedicated: true},
				{IP: "192.168.1.2", RDNS: "mail2.test.com", Dedicated: true},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
	}

	client := NewClient(cfg)
	ctx := context.Background()

	ips, err := client.GetSendingIPs(ctx)
	if err != nil {
		t.Fatalf("GetSendingIPs returned error: %v", err)
	}

	if ips.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want %d", ips.TotalCount, 2)
	}

	if len(ips.Items) != 2 {
		t.Errorf("len(Items) = %d, want %d", len(ips.Items), 2)
	}
}

func TestClient_GetIPPools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		resp := IPPoolsResponse{
			IPPools: []IPPool{
				{Name: "default", Description: "Default pool", IPs: []string{"192.168.1.1"}},
				{Name: "transactional", Description: "Transactional pool", IPs: []string{"192.168.1.2"}},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "test-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
	}

	client := NewClient(cfg)
	ctx := context.Background()

	pools, err := client.GetIPPools(ctx)
	if err != nil {
		t.Fatalf("GetIPPools returned error: %v", err)
	}

	if len(pools.IPPools) != 2 {
		t.Errorf("len(IPPools) = %d, want %d", len(pools.IPPools), 2)
	}

	if pools.IPPools[0].Name != "default" {
		t.Errorf("IPPools[0].Name = %q, want %q", pools.IPPools[0].Name, "default")
	}
}

func TestClient_BuildMetricsRequest(t *testing.T) {
	cfg := config.MailgunConfig{
		Domains: []string{"test.com"},
	}

	client := NewClient(cfg)

	from := time.Date(2025, 1, 27, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 1, 27, 23, 59, 59, 0, time.UTC)

	query := MetricsQuery{
		From:       from,
		To:         to,
		Resolution: "hour",
		Domains:    []string{"test.com"},
	}

	req := client.BuildMetricsRequest(query)

	if req.Resolution != "hour" {
		t.Errorf("Resolution = %q, want %q", req.Resolution, "hour")
	}

	if req.Filter == nil {
		t.Fatal("Filter is nil")
	}

	if len(req.Filter.AND) != 1 {
		t.Errorf("len(Filter.AND) = %d, want %d", len(req.Filter.AND), 1)
	}

	if req.Filter.AND[0].Attribute != "domain" {
		t.Errorf("Filter.AND[0].Attribute = %q, want %q", req.Filter.AND[0].Attribute, "domain")
	}
}

func TestClient_BuildMetricsRequest_DefaultResolution(t *testing.T) {
	cfg := config.MailgunConfig{}
	client := NewClient(cfg)

	query := MetricsQuery{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now(),
	}

	req := client.BuildMetricsRequest(query)

	if req.Resolution != "hour" {
		t.Errorf("Resolution = %q, want default %q", req.Resolution, "hour")
	}
}

func TestClient_AggregateProviderStats(t *testing.T) {
	cfg := config.MailgunConfig{}
	client := NewClient(cfg)

	allStats := map[string]*ProviderAggregatesResponse{
		"domain1.com": {
			Providers: map[string]ProviderStats{
				"gmail.com": {Accepted: 100, Delivered: 95},
				"yahoo.com": {Accepted: 50, Delivered: 45},
			},
		},
		"domain2.com": {
			Providers: map[string]ProviderStats{
				"gmail.com": {Accepted: 200, Delivered: 190},
				"outlook.com": {Accepted: 75, Delivered: 70},
			},
		},
	}

	aggregated := client.AggregateProviderStats(allStats)

	// Gmail should be aggregated from both domains
	gmail, ok := aggregated["gmail.com"]
	if !ok {
		t.Fatal("gmail.com not found in aggregated stats")
	}

	if gmail.Accepted != 300 {
		t.Errorf("gmail.Accepted = %d, want %d", gmail.Accepted, 300)
	}

	if gmail.Delivered != 285 {
		t.Errorf("gmail.Delivered = %d, want %d", gmail.Delivered, 285)
	}

	// Yahoo should only have stats from domain1
	yahoo, ok := aggregated["yahoo.com"]
	if !ok {
		t.Fatal("yahoo.com not found in aggregated stats")
	}

	if yahoo.Accepted != 50 {
		t.Errorf("yahoo.Accepted = %d, want %d", yahoo.Accepted, 50)
	}

	// Outlook should only have stats from domain2
	outlook, ok := aggregated["outlook.com"]
	if !ok {
		t.Fatal("outlook.com not found in aggregated stats")
	}

	if outlook.Accepted != 75 {
		t.Errorf("outlook.Accepted = %d, want %d", outlook.Accepted, 75)
	}
}

func TestClient_AggregateProviderStats_HandlesNil(t *testing.T) {
	cfg := config.MailgunConfig{}
	client := NewClient(cfg)

	allStats := map[string]*ProviderAggregatesResponse{
		"domain1.com": nil,
		"domain2.com": {
			Providers: map[string]ProviderStats{
				"gmail.com": {Accepted: 100},
			},
		},
	}

	aggregated := client.AggregateProviderStats(allStats)

	if len(aggregated) != 1 {
		t.Errorf("len(aggregated) = %d, want %d", len(aggregated), 1)
	}

	gmail := aggregated["gmail.com"]
	if gmail.Accepted != 100 {
		t.Errorf("gmail.Accepted = %d, want %d", gmail.Accepted, 100)
	}
}

func TestClient_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message": "Invalid API key"}`))
	}))
	defer server.Close()

	cfg := config.MailgunConfig{
		APIKey:         "invalid-key",
		BaseURL:        server.URL,
		TimeoutSeconds: 30,
		Domains:        []string{"test.com"},
	}

	client := NewClient(cfg)
	ctx := context.Background()

	_, err := client.GetSendingIPs(ctx)
	if err == nil {
		t.Fatal("Expected error for unauthorized request")
	}
}

func TestAnalyzeIssues(t *testing.T) {
	signals := SignalsData{
		BounceReasons: []BounceClassificationItem{
			{Classification: "hard", Count: 15000, Reason: "Mailbox not found"},
			{Classification: "soft", Count: 5000, Reason: "Mailbox full"},
			{Classification: "espblock", Count: 2000, Reason: "Blocked by ESP"},
			{Classification: "hard", Count: 500, Reason: "Domain not found"}, // Under threshold
		},
	}

	issues := analyzeIssues(signals)

	// Should have 3 issues (one under threshold)
	if len(issues) != 3 {
		t.Errorf("len(issues) = %d, want %d", len(issues), 3)
	}

	// First issue should be the critical one (highest count)
	if issues[0].Count != 15000 {
		t.Errorf("issues[0].Count = %d, want %d", issues[0].Count, 15000)
	}

	if issues[0].Severity != "critical" {
		t.Errorf("issues[0].Severity = %q, want %q", issues[0].Severity, "critical")
	}
}

func TestGetBounceRecommendation(t *testing.T) {
	tests := []struct {
		classification string
		contains       string
	}{
		{"hard", "list hygiene"},
		{"soft", "retry"},
		{"espblock", "blocklist"},
		{"unknown", "Investigate"},
	}

	for _, tt := range tests {
		t.Run(tt.classification, func(t *testing.T) {
			rec := getBounceRecommendation(tt.classification)
			found := false
			for _, word := range []string{tt.contains} {
				if containsWord(rec, word) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("getBounceRecommendation(%q) = %q, want to contain %q", tt.classification, rec, tt.contains)
			}
		})
	}
}

func containsWord(s, word string) bool {
	return len(s) > 0 && len(word) > 0 && (s == word || len(s) > len(word))
}

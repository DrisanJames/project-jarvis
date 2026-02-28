package sparkpost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(server *httptest.Server) *Client {
	return &Client{
		baseURL: server.URL,
		apiKey:  "test-api-key",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func TestNewClient(t *testing.T) {
	cfg := config.SparkPostConfig{
		APIKey:         "test-key",
		BaseURL:        "https://api.sparkpost.com/api/v1",
		TimeoutSeconds: 30,
	}

	client := NewClient(cfg)

	assert.NotNil(t, client)
	assert.Equal(t, "test-key", client.apiKey)
	assert.Equal(t, "https://api.sparkpost.com/api/v1", client.baseURL)
}

func TestGetMetricsSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/deliverability", r.URL.Path)
		assert.Equal(t, "test-api-key", r.Header.Get("Authorization"))

		response := MetricsResponse{
			Results: []MetricResult{
				{
					CountTargeted:      1000000,
					CountDelivered:     980000,
					CountBounce:        20000,
					CountSpamComplaint: 100,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now(),
	}

	resp, err := client.GetMetricsSummary(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 1)

	assert.Equal(t, int64(1000000), resp.Results[0].CountTargeted)
	assert.Equal(t, int64(980000), resp.Results[0].CountDelivered)
	assert.Equal(t, int64(20000), resp.Results[0].CountBounce)
}

func TestGetMetricsByMailboxProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/deliverability/mailbox-provider", r.URL.Path)

		response := MetricsResponse{
			Results: []MetricResult{
				{
					MailboxProvider:   "Gmail",
					CountTargeted:     500000,
					CountDelivered:    495000,
					CountSpamComplaint: 25,
				},
				{
					MailboxProvider:   "Yahoo",
					CountTargeted:     300000,
					CountDelivered:    290000,
					CountSpamComplaint: 45,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From:  time.Now().Add(-24 * time.Hour),
		To:    time.Now(),
		Limit: 10,
	}

	resp, err := client.GetMetricsByMailboxProvider(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 2)

	assert.Equal(t, "Gmail", resp.Results[0].MailboxProvider)
	assert.Equal(t, int64(500000), resp.Results[0].CountTargeted)
	assert.Equal(t, "Yahoo", resp.Results[1].MailboxProvider)
}

func TestGetMetricsBySendingIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/deliverability/sending-ip", r.URL.Path)

		response := MetricsResponse{
			Results: []MetricResult{
				{
					SendingIP:      "18.236.253.72",
					CountTargeted:  200000,
					CountDelivered: 196000,
					CountBounce:    4000,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now(),
	}

	resp, err := client.GetMetricsBySendingIP(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 1)

	assert.Equal(t, "18.236.253.72", resp.Results[0].SendingIP)
}

func TestGetTimeSeriesMetrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/deliverability/time-series", r.URL.Path)

		response := MetricsResponse{
			Results: []MetricResult{
				{
					Timestamp:      "2026-01-27T10:00:00Z",
					CountTargeted:  50000,
					CountDelivered: 49000,
				},
				{
					Timestamp:      "2026-01-27T11:00:00Z",
					CountTargeted:  55000,
					CountDelivered: 54000,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From:      time.Now().Add(-24 * time.Hour),
		To:        time.Now(),
		Precision: "hour",
	}

	resp, err := client.GetTimeSeriesMetrics(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 2)
}

func TestGetBounceReasons(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/deliverability/bounce-reason", r.URL.Path)

		response := BounceReasonResponse{
			Results: []BounceReasonResult{
				{
					Reason:            "550 User unknown",
					BounceClassName:   "Invalid Recipient",
					BounceCategoryName: "Hard",
					CountBounce:       1500,
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From:  time.Now().Add(-24 * time.Hour),
		To:    time.Now(),
		Limit: 10,
	}

	resp, err := client.GetBounceReasons(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Results, 1)

	assert.Equal(t, "550 User unknown", resp.Results[0].Reason)
	assert.Equal(t, "Hard", resp.Results[0].BounceCategoryName)
}

func TestGetMailboxProviders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/metrics/mailbox-providers", r.URL.Path)

		response := ListResponse{
			Results: map[string][]string{
				"mailbox-providers": {"Gmail", "Yahoo", "Hotmail / Outlook", "AOL"},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	providers, err := client.GetMailboxProviders(ctx, time.Now().Add(-24*time.Hour), time.Now())
	require.NoError(t, err)
	require.Len(t, providers, 4)
	assert.Contains(t, providers, "Gmail")
	assert.Contains(t, providers, "Yahoo")
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors": [{"message": "Unauthorized"}]}`))
	}))
	defer server.Close()

	client := newTestClient(server)
	ctx := context.Background()

	query := MetricsQuery{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now(),
	}

	_, err := client.GetMetricsSummary(ctx, query)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API error (status 401)")
}

func TestBuildMetricsParams(t *testing.T) {
	client := &Client{}

	from := time.Date(2026, 1, 27, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 27, 11, 0, 0, 0, time.UTC)

	query := MetricsQuery{
		From:             from,
		To:               to,
		Precision:        "hour",
		Domains:          []string{"gmail.com", "yahoo.com"},
		MailboxProviders: []string{"Gmail"},
		Limit:            100,
		OrderBy:          "count_targeted",
		Metrics:          []string{"count_targeted", "count_delivered"},
	}

	params := client.buildMetricsParams(query)

	assert.Equal(t, "2026-01-27T10:00", params.Get("from"))
	assert.Equal(t, "2026-01-27T11:00", params.Get("to"))
	assert.Equal(t, "hour", params.Get("precision"))
	assert.Equal(t, "gmail.com,yahoo.com", params.Get("domains"))
	assert.Equal(t, "Gmail", params.Get("mailbox_providers"))
	assert.Equal(t, "100", params.Get("limit"))
	assert.Equal(t, "count_targeted", params.Get("order_by"))
	assert.Equal(t, "count_targeted,count_delivered", params.Get("metrics"))
	assert.Equal(t, "UTC", params.Get("timezone"))
}

func TestConvertToProcessedMetrics(t *testing.T) {
	result := MetricResult{
		MailboxProvider:    "Gmail",
		CountTargeted:      1000000,
		CountSent:          990000,
		CountDelivered:     980000,
		CountUniqueRendered: 196000,
		CountUniqueClicked: 28000,
		CountBounce:        10000,
		CountHardBounce:    4000,
		CountSoftBounce:    5000,
		CountBlockBounce:   1000,
		CountSpamComplaint: 100,
		CountUnsubscribe:   500,
	}

	pm := ConvertToProcessedMetrics(result, "mailbox_provider", "Gmail")

	assert.Equal(t, "sparkpost", pm.Source)
	assert.Equal(t, "mailbox_provider", pm.GroupBy)
	assert.Equal(t, "Gmail", pm.GroupValue)
	assert.Equal(t, int64(1000000), pm.Targeted)
	assert.Equal(t, int64(980000), pm.Delivered)
	assert.Equal(t, int64(196000), pm.UniqueOpened)

	// Check calculated rates
	assert.InDelta(t, 0.9899, pm.DeliveryRate, 0.001) // 980000/990000
	assert.InDelta(t, 0.2, pm.OpenRate, 0.001)        // 196000/980000
	assert.InDelta(t, 0.0101, pm.BounceRate, 0.001)   // 10000/990000
}

func TestContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newTestClient(server)
	
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	query := MetricsQuery{
		From: time.Now().Add(-24 * time.Hour),
		To:   time.Now(),
	}

	_, err := client.GetMetricsSummary(ctx, query)
	require.Error(t, err)
}

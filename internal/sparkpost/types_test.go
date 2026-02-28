package sparkpost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestProcessedMetrics_CalculateRates(t *testing.T) {
	tests := []struct {
		name     string
		metrics  ProcessedMetrics
		expected ProcessedMetrics
	}{
		{
			name: "normal metrics",
			metrics: ProcessedMetrics{
				Sent:         1000,
				Delivered:    950,
				UniqueOpened: 190,
				UniqueClicked: 28,
				Bounced:      50,
				HardBounced:  20,
				SoftBounced:  25,
				BlockBounced: 5,
				Complaints:   1,
				Unsubscribes: 5,
			},
			expected: ProcessedMetrics{
				DeliveryRate:    0.95,
				OpenRate:        0.2,
				ClickRate:       0.02947368421052632,
				BounceRate:      0.05,
				HardBounceRate:  0.02,
				SoftBounceRate:  0.025,
				BlockRate:       0.005,
				ComplaintRate:   0.0010526315789473684,
				UnsubscribeRate: 0.005263157894736842,
			},
		},
		{
			name: "zero sent",
			metrics: ProcessedMetrics{
				Sent:      0,
				Delivered: 0,
			},
			expected: ProcessedMetrics{
				DeliveryRate: 0,
				OpenRate:     0,
			},
		},
		{
			name: "zero delivered but some sent",
			metrics: ProcessedMetrics{
				Sent:      100,
				Delivered: 0,
				Bounced:   100,
			},
			expected: ProcessedMetrics{
				DeliveryRate: 0,
				BounceRate:   1.0,
				OpenRate:     0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.metrics.CalculateRates()
			
			assert.InDelta(t, tt.expected.DeliveryRate, tt.metrics.DeliveryRate, 0.0001)
			assert.InDelta(t, tt.expected.OpenRate, tt.metrics.OpenRate, 0.0001)
			assert.InDelta(t, tt.expected.BounceRate, tt.metrics.BounceRate, 0.0001)
			
			if tt.expected.ClickRate > 0 {
				assert.InDelta(t, tt.expected.ClickRate, tt.metrics.ClickRate, 0.0001)
			}
			if tt.expected.ComplaintRate > 0 {
				assert.InDelta(t, tt.expected.ComplaintRate, tt.metrics.ComplaintRate, 0.0001)
			}
		})
	}
}

func TestDefaultMetrics(t *testing.T) {
	metrics := DefaultMetrics()
	assert.NotEmpty(t, metrics)
	assert.Contains(t, metrics, "count_targeted")
	assert.Contains(t, metrics, "count_delivered")
	assert.Contains(t, metrics, "count_spam_complaint")
}

func TestAllMetrics(t *testing.T) {
	metrics := AllMetrics()
	assert.NotEmpty(t, metrics)
	assert.Greater(t, len(metrics), len(DefaultMetrics()))
	assert.Contains(t, metrics, "count_policy_rejection")
	assert.Contains(t, metrics, "total_msg_volume")
}

func TestMetricResult_Fields(t *testing.T) {
	result := MetricResult{
		Domain:          "gmail.com",
		MailboxProvider: "Gmail",
		CountTargeted:   1000000,
		CountDelivered:  980000,
		CountBounce:     20000,
		CountSpamComplaint: 100,
	}

	assert.Equal(t, "gmail.com", result.Domain)
	assert.Equal(t, "Gmail", result.MailboxProvider)
	assert.Equal(t, int64(1000000), result.CountTargeted)
	assert.Equal(t, int64(980000), result.CountDelivered)
	assert.Equal(t, int64(20000), result.CountBounce)
	assert.Equal(t, int64(100), result.CountSpamComplaint)
}

func TestISPMetrics(t *testing.T) {
	isp := ISPMetrics{
		Provider: "Gmail",
		Metrics: ProcessedMetrics{
			Timestamp:    time.Now(),
			Source:       "sparkpost",
			GroupBy:      "mailbox_provider",
			GroupValue:   "Gmail",
			Sent:         1000000,
			Delivered:    980000,
			Complaints:   50,
		},
		Status:       "healthy",
		StatusReason: "",
	}

	isp.Metrics.CalculateRates()

	assert.Equal(t, "Gmail", isp.Provider)
	assert.Equal(t, "healthy", isp.Status)
	assert.InDelta(t, 0.98, isp.Metrics.DeliveryRate, 0.001)
}

func TestIPMetrics(t *testing.T) {
	ip := IPMetrics{
		IP:   "18.236.253.72",
		Pool: "primary",
		Metrics: ProcessedMetrics{
			Sent:      500000,
			Delivered: 490000,
			Bounced:   10000,
		},
		Status: "healthy",
	}

	ip.Metrics.CalculateRates()

	assert.Equal(t, "18.236.253.72", ip.IP)
	assert.Equal(t, "primary", ip.Pool)
	assert.InDelta(t, 0.98, ip.Metrics.DeliveryRate, 0.001)
	assert.InDelta(t, 0.02, ip.Metrics.BounceRate, 0.001)
}

func TestIssue(t *testing.T) {
	issue := Issue{
		Severity:       "warning",
		Category:       "complaint",
		Description:    "Complaint rate elevated for Yahoo",
		AffectedISP:    "Yahoo",
		Count:          150,
		Recommendation: "Review content targeting Yahoo recipients",
	}

	assert.Equal(t, "warning", issue.Severity)
	assert.Equal(t, "complaint", issue.Category)
	assert.Equal(t, "Yahoo", issue.AffectedISP)
	assert.Equal(t, int64(150), issue.Count)
}

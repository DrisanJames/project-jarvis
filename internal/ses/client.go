package ses

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	appconfig "github.com/ignite/sparkpost-monitor/internal/config"
)

// Client is an AWS SES v2 API client for VDM metrics
type Client struct {
	client *sesv2.Client
	isps   []string
	region string
}

// NewClient creates a new SES API client
func NewClient(ctx context.Context, cfg appconfig.SESConfig) (*Client, error) {
	// Create AWS credentials
	creds := credentials.NewStaticCredentialsProvider(
		cfg.AccessKey,
		cfg.SecretKey,
		"", // session token (empty for static creds)
	)

	// Load AWS config with static credentials
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	// Create SES v2 client
	sesClient := sesv2.NewFromConfig(awsCfg)

	return &Client{
		client: sesClient,
		isps:   cfg.DefaultISPs(),
		region: cfg.Region,
	}, nil
}

// GetISPs returns the configured ISPs to query
func (c *Client) GetISPs() []string {
	return c.isps
}

// GetMetricsForISP fetches all VDM metrics for a specific ISP
func (c *Client) GetMetricsForISP(ctx context.Context, isp string, from, to time.Time) (*ISPMetricData, error) {
	// Build queries for all metric types for this ISP
	queries := make([]types.BatchGetMetricDataQuery, 0, len(AllMetrics()))
	
	for i, metric := range AllMetrics() {
		queries = append(queries, types.BatchGetMetricDataQuery{
			Id:        aws.String(fmt.Sprintf("q%d_%s", i, metric)),
			Namespace: types.MetricNamespaceVdm,
			Metric:    types.Metric(metric),
			Dimensions: map[string]string{
				"ISP": isp,
			},
			StartDate: aws.Time(from),
			EndDate:   aws.Time(to),
		})
	}

	input := &sesv2.BatchGetMetricDataInput{
		Queries: queries,
	}

	output, err := c.client.BatchGetMetricData(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics for ISP %s: %w", isp, err)
	}

	// Parse results into ISPMetricData
	data := &ISPMetricData{
		ISP: isp,
	}

	for _, result := range output.Results {
		if result.Id == nil {
			continue
		}
		
		// Sum all values for the metric
		var total int64
		for _, val := range result.Values {
			total += int64(val)
		}

		// Map result to ISPMetricData field based on metric type
		switch {
		case containsMetric(*result.Id, MetricSend):
			data.Send = total
		case containsMetric(*result.Id, MetricDelivery):
			data.Delivery = total
		case containsMetric(*result.Id, MetricPermanentBounce):
			data.PermanentBounce = total
		case containsMetric(*result.Id, MetricTransientBounce):
			data.TransientBounce = total
		case containsMetric(*result.Id, MetricComplaint):
			data.Complaint = total
		case containsMetric(*result.Id, MetricOpen):
			data.Open = total
		case containsMetric(*result.Id, MetricClick):
			data.Click = total
		}
	}

	return data, nil
}

// containsMetric checks if the result ID contains the metric name
func containsMetric(id, metric string) bool {
	return len(id) >= len(metric) && id[len(id)-len(metric):] == metric
}

// GetAllISPMetrics fetches metrics for all configured ISPs
func (c *Client) GetAllISPMetrics(ctx context.Context, from, to time.Time) ([]ISPMetrics, error) {
	var ispMetrics []ISPMetrics

	// First, try to get overall account metrics to see if VDM has data
	overallData, err := c.GetOverallMetrics(ctx, from, to)
	if err != nil {
		log.Printf("SES: Error getting overall metrics: %v", err)
	} else if overallData.Send == 0 {
		log.Printf("SES: No overall VDM data found for period %s to %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	} else {
		log.Printf("SES: Account has %d total sends in VDM for period", overallData.Send)
	}

	// Try to get ISP-specific metrics
	for _, isp := range c.isps {
		data, err := c.GetMetricsForISP(ctx, isp, from, to)
		if err != nil {
			log.Printf("SES: Failed to get metrics for ISP %s: %v", isp, err)
			continue
		}

		// Only include ISPs with data
		if data.Send > 0 {
			metrics := ConvertISPDataToISPMetrics(*data)
			ispMetrics = append(ispMetrics, metrics)
		}
	}

	// If no ISP-specific data but we have overall data, add an "All ISPs" entry
	if len(ispMetrics) == 0 && overallData != nil && overallData.Send > 0 {
		log.Printf("SES: No ISP-specific data available, showing overall VDM metrics")
		metrics := ConvertISPDataToISPMetrics(*overallData)
		ispMetrics = append(ispMetrics, metrics)
	}

	log.Printf("SES: Got metrics for %d ISPs", len(ispMetrics))
	return ispMetrics, nil
}

// checkOverallMetrics queries overall account SEND metrics without ISP filter
func (c *Client) checkOverallMetrics(ctx context.Context, from, to time.Time) int64 {
	query := types.BatchGetMetricDataQuery{
		Id:        aws.String("overall_send"),
		Namespace: types.MetricNamespaceVdm,
		Metric:    types.Metric(MetricSend),
		StartDate: aws.Time(from),
		EndDate:   aws.Time(to),
		// No dimensions = overall account metrics
	}

	output, err := c.client.BatchGetMetricData(ctx, &sesv2.BatchGetMetricDataInput{
		Queries: []types.BatchGetMetricDataQuery{query},
	})
	if err != nil {
		log.Printf("SES: Error checking overall metrics: %v", err)
		return 0
	}

	var total int64
	for _, result := range output.Results {
		for _, val := range result.Values {
			total += int64(val)
		}
	}
	return total
}

// GetOverallMetrics fetches all VDM metrics without ISP dimension (account-level)
func (c *Client) GetOverallMetrics(ctx context.Context, from, to time.Time) (*ISPMetricData, error) {
	queries := make([]types.BatchGetMetricDataQuery, 0, len(AllMetrics()))
	
	for i, metric := range AllMetrics() {
		queries = append(queries, types.BatchGetMetricDataQuery{
			Id:        aws.String(fmt.Sprintf("overall_%d_%s", i, metric)),
			Namespace: types.MetricNamespaceVdm,
			Metric:    types.Metric(metric),
			StartDate: aws.Time(from),
			EndDate:   aws.Time(to),
			// No dimensions = overall account metrics
		})
	}

	output, err := c.client.BatchGetMetricData(ctx, &sesv2.BatchGetMetricDataInput{
		Queries: queries,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching overall metrics: %w", err)
	}

	data := &ISPMetricData{
		ISP: "All ISPs",
	}

	for _, result := range output.Results {
		if result.Id == nil {
			continue
		}
		
		var total int64
		for _, val := range result.Values {
			total += int64(val)
		}

		switch {
		case containsMetric(*result.Id, MetricSend):
			data.Send = total
		case containsMetric(*result.Id, MetricDelivery):
			data.Delivery = total
		case containsMetric(*result.Id, MetricPermanentBounce):
			data.PermanentBounce = total
		case containsMetric(*result.Id, MetricTransientBounce):
			data.TransientBounce = total
		case containsMetric(*result.Id, MetricComplaint):
			data.Complaint = total
		case containsMetric(*result.Id, MetricOpen):
			data.Open = total
		case containsMetric(*result.Id, MetricClick):
			data.Click = total
		}
	}

	return data, nil
}

// GetSummary fetches overall metrics summary across all ISPs
func (c *Client) GetSummary(ctx context.Context, from, to time.Time) (*Summary, error) {
	ispMetrics, err := c.GetAllISPMetrics(ctx, from, to)
	if err != nil {
		return nil, err
	}

	summary := AggregateISPMetricsToSummary(ispMetrics, from, to)
	return summary, nil
}

// GetAccountStatistics gets general account sending statistics
func (c *Client) GetAccountStatistics(ctx context.Context) (*sesv2.GetAccountOutput, error) {
	input := &sesv2.GetAccountInput{}
	return c.client.GetAccount(ctx, input)
}

// VerifyVDMEnabled checks if VDM is enabled for the account
func (c *Client) VerifyVDMEnabled(ctx context.Context) (bool, error) {
	account, err := c.GetAccountStatistics(ctx)
	if err != nil {
		return false, fmt.Errorf("getting account info: %w", err)
	}

	// Check if VDM attributes exist and engagement tracking is enabled
	if account.VdmAttributes != nil {
		if account.VdmAttributes.VdmEnabled == types.FeatureStatusEnabled {
			return true, nil
		}
	}

	return false, nil
}

// GetSignals generates deliverability signals based on current metrics
func (c *Client) GetSignals(ctx context.Context, from, to time.Time) (*SignalsData, error) {
	ispMetrics, err := c.GetAllISPMetrics(ctx, from, to)
	if err != nil {
		return nil, err
	}

	signals := &SignalsData{
		Timestamp: time.Now(),
		TopIssues: make([]Issue, 0),
	}

	// Analyze each ISP for issues
	for _, isp := range ispMetrics {
		m := isp.Metrics

		// Check for high complaint rate
		if m.ComplaintRate >= 0.001 { // 0.1%
			signals.TopIssues = append(signals.TopIssues, Issue{
				Severity:       "critical",
				Category:       "complaint",
				Description:    fmt.Sprintf("High complaint rate at %s: %.4f%%", isp.Provider, m.ComplaintRate*100),
				AffectedISP:    isp.Provider,
				Count:          m.Complaints,
				Recommendation: "Review email content and list hygiene for this ISP",
			})
		} else if m.ComplaintRate >= 0.0005 { // 0.05%
			signals.TopIssues = append(signals.TopIssues, Issue{
				Severity:       "warning",
				Category:       "complaint",
				Description:    fmt.Sprintf("Elevated complaint rate at %s: %.4f%%", isp.Provider, m.ComplaintRate*100),
				AffectedISP:    isp.Provider,
				Count:          m.Complaints,
				Recommendation: "Monitor complaint trends and consider list cleaning",
			})
		}

		// Check for high bounce rate
		if m.BounceRate >= 0.10 { // 10%
			signals.TopIssues = append(signals.TopIssues, Issue{
				Severity:       "critical",
				Category:       "bounce",
				Description:    fmt.Sprintf("High bounce rate at %s: %.2f%%", isp.Provider, m.BounceRate*100),
				AffectedISP:    isp.Provider,
				Count:          m.Bounced,
				Recommendation: "Clean suppression list and verify email addresses",
			})
		} else if m.BounceRate >= 0.05 { // 5%
			signals.TopIssues = append(signals.TopIssues, Issue{
				Severity:       "warning",
				Category:       "bounce",
				Description:    fmt.Sprintf("Elevated bounce rate at %s: %.2f%%", isp.Provider, m.BounceRate*100),
				AffectedISP:    isp.Provider,
				Count:          m.Bounced,
				Recommendation: "Review bounce patterns and list quality",
			})
		}

		// Check for low delivery rate
		if m.DeliveryRate < 0.90 && m.Sent > 100 { // Below 90% with meaningful volume
			signals.TopIssues = append(signals.TopIssues, Issue{
				Severity:       "warning",
				Category:       "delivery",
				Description:    fmt.Sprintf("Low delivery rate at %s: %.2f%%", isp.Provider, m.DeliveryRate*100),
				AffectedISP:    isp.Provider,
				Count:          m.Sent - m.Delivered,
				Recommendation: "Check IP reputation and authentication settings",
			})
		}
	}

	// Add general recommendations based on issues
	if len(signals.TopIssues) > 0 {
		signals.Recommendations = generateRecommendations(signals.TopIssues)
	}

	return signals, nil
}

// generateRecommendations creates actionable recommendations based on issues
func generateRecommendations(issues []Issue) []string {
	recommendations := make(map[string]bool)

	for _, issue := range issues {
		switch issue.Category {
		case "complaint":
			recommendations["Review and segment your email lists to target engaged subscribers"] = true
			recommendations["Ensure easy unsubscribe options are visible in all emails"] = true
		case "bounce":
			recommendations["Implement email validation at collection points"] = true
			recommendations["Regularly clean your email lists of invalid addresses"] = true
		case "delivery":
			recommendations["Verify SPF, DKIM, and DMARC authentication records"] = true
			recommendations["Monitor your sending IP reputation"] = true
		}
	}

	result := make([]string, 0, len(recommendations))
	for rec := range recommendations {
		result = append(result, rec)
	}
	return result
}

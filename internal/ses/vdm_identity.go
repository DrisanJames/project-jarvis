package ses

// Identity × ISP VDM metrics (Vector A plan rev4, Step 15).
//
// One BatchGetMetricData call per (identity, raw AWS ISP name) fetches all 7
// VDM metrics with Dimensions {EMAIL_IDENTITY, ISP}. Per-query failures
// (output.Errors) are tolerated and surfaced as MissingMetrics — a partial
// batch never fails the whole cell. Values are summed EXPLICITLY across the
// returned data points (daily granularity: midnight-UTC bounds only — the API
// rejects non-midnight daily windows; the pinned SDK v1.37.0 has no hourly
// granularity).
//
// The return shape carries the RAW AWS name: canonicalization (raw →
// isp.GroupFromVDMISP group) and alias summing happen in the snapshot worker
// (internal/worker/ses_vdm_snapshot.go), never here.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// MetricsAPI is the test seam for the one SES call the identity path makes.
// *sesv2.Client satisfies it.
type MetricsAPI interface {
	BatchGetMetricData(ctx context.Context, params *sesv2.BatchGetMetricDataInput, optFns ...func(*sesv2.Options)) (*sesv2.BatchGetMetricDataOutput, error)
}

// IdentityISPMetrics is one (identity, raw ISP) cell's summed VDM counters.
type IdentityISPMetrics struct {
	Identity       string
	RawISP         string
	Values         map[string]int64 // keyed by MetricSend … MetricClick
	MissingMetrics []string         // metrics with a query error or no result
}

// NewClientForRegion builds a VDM metrics client on the DEFAULT AWS credential
// chain (task role in ECS — no static keys) with the standard retryer
// (exponential backoff + jitter). Region is explicit; prod passes SES_REGION
// (us-west-1).
func NewClientForRegion(ctx context.Context, region string) (*Client, error) {
	if region == "" {
		return nil, fmt.Errorf("ses: NewClientForRegion requires a region")
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithRetryer(func() aws.Retryer { return retry.NewStandard() }),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for region %s: %w", region, err)
	}
	sesClient := sesv2.NewFromConfig(awsCfg)
	return &Client{
		client:  sesClient,
		metrics: sesClient,
		region:  region,
	}, nil
}

// newClientWithMetricsAPI is the unit-test constructor: everything real
// except the wire.
func newClientWithMetricsAPI(m MetricsAPI, region string) *Client {
	return &Client{metrics: m, region: region}
}

// metricsAPI returns the injected seam, falling back to the real sesv2 client
// (NewClient-constructed instances predate the seam).
func (c *Client) metricsAPI() MetricsAPI {
	if c.metrics != nil {
		return c.metrics
	}
	return c.client
}

// GetMetricsForIdentityISP fetches all VDM metrics for one
// (identity, raw AWS ISP name) over [from, to) — midnight-UTC daily bounds.
func (c *Client) GetMetricsForIdentityISP(ctx context.Context, identity, rawISP string, from, to time.Time) (*IdentityISPMetrics, error) {
	metrics := AllMetrics()
	queries := make([]types.BatchGetMetricDataQuery, 0, len(metrics))
	idToMetric := make(map[string]string, len(metrics))
	for i, metric := range metrics {
		id := fmt.Sprintf("q%d_%s", i, metric)
		idToMetric[id] = metric
		queries = append(queries, types.BatchGetMetricDataQuery{
			Id:        aws.String(id),
			Namespace: types.MetricNamespaceVdm,
			Metric:    types.Metric(metric),
			Dimensions: map[string]string{
				string(types.MetricDimensionNameEmailIdentity): identity,
				string(types.MetricDimensionNameIsp):           rawISP,
			},
			StartDate: aws.Time(from),
			EndDate:   aws.Time(to),
		})
	}

	output, err := c.metricsAPI().BatchGetMetricData(ctx, &sesv2.BatchGetMetricDataInput{Queries: queries})
	if err != nil {
		return nil, fmt.Errorf("fetching VDM metrics for identity %s isp %s: %w", identity, rawISP, err)
	}

	out := &IdentityISPMetrics{
		Identity: identity,
		RawISP:   rawISP,
		Values:   make(map[string]int64, len(metrics)),
	}
	seen := map[string]bool{}

	// Per-query errors: the metric is MISSING, not zero.
	failed := map[string]bool{}
	for _, e := range output.Errors {
		if e.Id == nil {
			continue
		}
		if m, ok := idToMetric[*e.Id]; ok {
			failed[m] = true
			seen[m] = true
		}
	}

	for _, result := range output.Results {
		if result.Id == nil {
			continue
		}
		m, ok := idToMetric[*result.Id]
		if !ok || failed[m] {
			continue
		}
		var total int64
		for _, v := range result.Values {
			total += v
		}
		out.Values[m] = total
		seen[m] = true
	}

	for _, m := range metrics {
		if failed[m] || !seen[m] {
			out.MissingMetrics = append(out.MissingMetrics, m)
		}
	}
	sort.Strings(out.MissingMetrics)
	return out, nil
}

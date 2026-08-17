package ses

// Step-15 fixtures (Vector A plan rev4): partial-batch tolerance and explicit
// value summing on the identity × ISP VDM fetch. Permanent fixtures (I-11).

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

type fakeMetricsAPI struct {
	lastInput *sesv2.BatchGetMetricDataInput
	output    *sesv2.BatchGetMetricDataOutput
	err       error
}

func (f *fakeMetricsAPI) BatchGetMetricData(ctx context.Context, params *sesv2.BatchGetMetricDataInput, optFns ...func(*sesv2.Options)) (*sesv2.BatchGetMetricDataOutput, error) {
	f.lastInput = params
	return f.output, f.err
}

func utcDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// TestIdentityISPSumsValues: values are summed EXPLICITLY across data points,
// and the query carries both EMAIL_IDENTITY and ISP dimensions.
func TestIdentityISPSumsValues(t *testing.T) {
	fake := &fakeMetricsAPI{output: &sesv2.BatchGetMetricDataOutput{
		Results: []types.MetricDataResult{
			{Id: aws.String("q0_SEND"), Values: []int64{100, 250, 7}},
			{Id: aws.String("q1_DELIVERY"), Values: []int64{95, 240, 6}},
			{Id: aws.String("q2_PERMANENT_BOUNCE"), Values: []int64{1}},
			{Id: aws.String("q3_TRANSIENT_BOUNCE"), Values: []int64{2}},
			{Id: aws.String("q4_COMPLAINT"), Values: []int64{0}},
			{Id: aws.String("q5_OPEN"), Values: []int64{10, 20}},
			{Id: aws.String("q6_CLICK"), Values: []int64{3}},
		},
	}}
	c := newClientWithMetricsAPI(fake, "us-west-1")

	got, err := c.GetMetricsForIdentityISP(context.Background(),
		"em.discountblog.com", "Hotmail", utcDay(2026, 8, 15), utcDay(2026, 8, 16))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identity != "em.discountblog.com" || got.RawISP != "Hotmail" {
		t.Fatalf("identity/raw carried wrong: %+v", got)
	}
	if got.Values[MetricSend] != 357 || got.Values[MetricDelivery] != 341 || got.Values[MetricOpen] != 30 {
		t.Fatalf("summed values wrong: %+v", got.Values)
	}
	if len(got.MissingMetrics) != 0 {
		t.Fatalf("full batch must have no missing metrics, got %v", got.MissingMetrics)
	}
	// Dimension proof: every query carries the identity + raw ISP pair.
	if fake.lastInput == nil || len(fake.lastInput.Queries) != len(AllMetrics()) {
		t.Fatalf("expected %d queries", len(AllMetrics()))
	}
	for _, q := range fake.lastInput.Queries {
		if q.Dimensions["EMAIL_IDENTITY"] != "em.discountblog.com" || q.Dimensions["ISP"] != "Hotmail" {
			t.Fatalf("query %v missing dimensions: %v", aws.ToString(q.Id), q.Dimensions)
		}
	}
}

// TestIdentityISPPartialBatch: a per-query error (output.Errors) or an absent
// result marks that metric MISSING — never zero, never a whole-cell failure.
func TestIdentityISPPartialBatch(t *testing.T) {
	fake := &fakeMetricsAPI{output: &sesv2.BatchGetMetricDataOutput{
		Results: []types.MetricDataResult{
			{Id: aws.String("q0_SEND"), Values: []int64{50}},
			{Id: aws.String("q1_DELIVERY"), Values: []int64{48}},
			// q4_COMPLAINT absent from Results AND Errors → missing.
		},
		Errors: []types.MetricDataError{
			{Id: aws.String("q5_OPEN"), Code: types.QueryErrorCodeInternalFailure},
		},
	}}
	c := newClientWithMetricsAPI(fake, "us-west-1")

	got, err := c.GetMetricsForIdentityISP(context.Background(),
		"em.quizfiesta.com", "Yahoo", utcDay(2026, 8, 15), utcDay(2026, 8, 16))
	if err != nil {
		t.Fatalf("partial batch must not error the cell: %v", err)
	}
	if got.Values[MetricSend] != 50 {
		t.Fatalf("healthy metric lost: %+v", got.Values)
	}
	missing := map[string]bool{}
	for _, m := range got.MissingMetrics {
		missing[m] = true
	}
	if !missing[MetricOpen] {
		t.Fatalf("errored metric OPEN must be missing: %v", got.MissingMetrics)
	}
	if !missing[MetricComplaint] {
		t.Fatalf("absent metric COMPLAINT must be missing: %v", got.MissingMetrics)
	}
	if _, ok := got.Values[MetricOpen]; ok {
		t.Fatal("errored metric must not carry a value")
	}
}

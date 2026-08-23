package api

// VDM reader tests — response shaping against a stubbed sesv2 seam, the
// dimensioning contract (mirrored from agents/reporting/lane_tolerance.py),
// and the cache hit path.
// Run: go test ./internal/api/ -run VDM -v

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// stubVDMAPI answers BatchGetMetricData from a canned {queryID: value} map
// and records every query it saw.
type stubVDMAPI struct {
	values  map[string]int64
	failIDs map[string]bool
	err     error
	calls   int
	queries []types.BatchGetMetricDataQuery
}

func (s *stubVDMAPI) BatchGetMetricData(_ context.Context, in *sesv2.BatchGetMetricDataInput, _ ...func(*sesv2.Options)) (*sesv2.BatchGetMetricDataOutput, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if len(in.Queries) > vdmBatchQueryLimit {
		return nil, fmt.Errorf("batch of %d exceeds the API limit of %d", len(in.Queries), vdmBatchQueryLimit)
	}
	out := &sesv2.BatchGetMetricDataOutput{}
	for _, q := range in.Queries {
		s.queries = append(s.queries, q)
		id := aws.ToString(q.Id)
		if s.failIDs[id] {
			out.Errors = append(out.Errors, types.MetricDataError{Id: aws.String(id), Code: types.QueryErrorCodeInternalFailure})
			continue
		}
		// Two data points per day-bucket result proves values are SUMMED,
		// not first-value-read.
		v := s.values[id]
		out.Results = append(out.Results, types.MetricDataResult{
			Id:     aws.String(id),
			Values: []int64{v - v/2, v / 2},
		})
	}
	return out, nil
}

func vdmStubValues() map[string]int64 {
	return map[string]int64{
		"t|SEND": 1000, "t|DELIVERY": 990, "t|OPEN": 240, "t|CLICK": 31,
		"t|COMPLAINT": 1, "t|PERMANENT_BOUNCE": 4, "t|TRANSIENT_BOUNCE": 6,
		"i|Gmail|SEND": 600, "i|Gmail|DELIVERY": 595, "i|Gmail|OPEN": 150, "i|Gmail|CLICK": 20,
		"i|Yahoo|SEND": 400, "i|Yahoo|DELIVERY": 395, "i|Yahoo|OPEN": 90, "i|Yahoo|CLICK": 11,
	}
}

func TestVDMDomainDay_ShapeAndDimensioning(t *testing.T) {
	api := &stubVDMAPI{values: vdmStubValues()}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	r := newVDMReaderWithAPI(api, func() time.Time { return now })

	day := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	got, err := r.vdmDomainDay(context.Background(), "em.refinanceratesusa.com", day)
	if err != nil {
		t.Fatalf("vdmDomainDay: %v", err)
	}

	// Totals: summed across data points, all 7 lane_tolerance metrics.
	if got.Send != 1000 || got.Delivered != 990 || got.Open != 240 || got.Click != 31 ||
		got.Complaint != 1 || got.PermanentBounce != 4 || got.TransientBounce != 6 {
		t.Errorf("totals wrong: %+v", got)
	}
	if got.Domain != "em.refinanceratesusa.com" || got.DayUTC != "2026-08-21" {
		t.Errorf("identity/day wrong: %+v", got)
	}

	// by_isp: only lanes that sent, sorted by send desc.
	if len(got.ByISP) != 2 || got.ByISP[0].ISP != "Gmail" || got.ByISP[1].ISP != "Yahoo" {
		t.Fatalf("by_isp = %+v (want Gmail then Yahoo, silent lanes dropped)", got.ByISP)
	}
	if got.ByISP[0].Delivered != 595 || got.ByISP[0].Open != 150 || got.ByISP[0].Click != 20 {
		t.Errorf("Gmail row = %+v", got.ByISP[0])
	}

	// Dimensioning contract (mirrors lane_tolerance.py): 7 identity-only
	// totals queries + 12 raw ISPs × 4 metrics identity×ISP queries, all
	// Namespace VDM with midnight-UTC [day, day+24h) bounds.
	wantQueries := 7 + len(vdmRawISPs)*4
	if len(api.queries) != wantQueries {
		t.Fatalf("queries = %d, want %d", len(api.queries), wantQueries)
	}
	seenISPs := map[string]bool{}
	for _, q := range api.queries {
		if q.Namespace != types.MetricNamespaceVdm {
			t.Fatalf("namespace = %v, want VDM", q.Namespace)
		}
		if !aws.ToTime(q.StartDate).Equal(day) || !aws.ToTime(q.EndDate).Equal(day.Add(24*time.Hour)) {
			t.Fatalf("bounds = [%v, %v), want midnight-UTC day", aws.ToTime(q.StartDate), aws.ToTime(q.EndDate))
		}
		if q.Dimensions[string(types.MetricDimensionNameEmailIdentity)] != "em.refinanceratesusa.com" {
			t.Fatalf("EMAIL_IDENTITY dimension missing on %s", aws.ToString(q.Id))
		}
		id := aws.ToString(q.Id)
		if strings.HasPrefix(id, "t|") {
			if _, hasISP := q.Dimensions[string(types.MetricDimensionNameIsp)]; hasISP {
				t.Fatalf("totals query %s must be EMAIL_IDENTITY-only", id)
			}
		} else {
			seenISPs[q.Dimensions[string(types.MetricDimensionNameIsp)]] = true
		}
	}
	for _, isp := range vdmRawISPs {
		if !seenISPs[isp] {
			t.Errorf("raw ISP %q never queried (lane_tolerance list must be mirrored)", isp)
		}
	}
}

func TestVDMDomainDay_CacheHitAndTTL(t *testing.T) {
	api := &stubVDMAPI{values: vdmStubValues()}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	clock := &now
	r := newVDMReaderWithAPI(api, func() time.Time { return *clock })

	day := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC) // complete UTC day
	if _, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", day); err != nil {
		t.Fatalf("first: %v", err)
	}
	callsAfterFirst := api.calls
	if callsAfterFirst == 0 {
		t.Fatal("no API calls on cold cache")
	}

	// Within TTL: served from cache — zero new calls.
	got, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", day)
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if api.calls != callsAfterFirst {
		t.Errorf("cache miss: calls %d -> %d", callsAfterFirst, api.calls)
	}
	if got.Send != 1000 {
		t.Errorf("cached stats wrong: %+v", got)
	}

	// A complete day holds for 6h (not the 15m partial TTL)…
	*clock = now.Add(5 * time.Hour)
	if _, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", day); err != nil {
		t.Fatalf("5h: %v", err)
	}
	if api.calls != callsAfterFirst {
		t.Errorf("complete day expired before 6h: calls %d -> %d", callsAfterFirst, api.calls)
	}
	// …and refetches after it.
	*clock = now.Add(7 * time.Hour)
	if _, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", day); err != nil {
		t.Fatalf("7h: %v", err)
	}
	if api.calls == callsAfterFirst {
		t.Error("complete-day entry never expired after 6h")
	}
}

func TestVDMDomainDay_PartialDayShortTTL(t *testing.T) {
	api := &stubVDMAPI{values: vdmStubValues()}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	clock := &now
	r := newVDMReaderWithAPI(api, func() time.Time { return *clock })

	today := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC) // still accumulating
	if _, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", today); err != nil {
		t.Fatalf("first: %v", err)
	}
	first := api.calls
	*clock = now.Add(16 * time.Minute)
	if _, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", today); err != nil {
		t.Fatalf("16m: %v", err)
	}
	if api.calls == first {
		t.Error("partial day cached past the 15m TTL")
	}
}

func TestVDMDomainDay_APIErrorReturned(t *testing.T) {
	api := &stubVDMAPI{err: fmt.Errorf("AccessDenied")}
	r := newVDMReaderWithAPI(api, nil)
	_, err := r.vdmDomainDay(context.Background(), "em.discountblog.com",
		time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("err = %v, want the AWS error surfaced", err)
	}
}

func TestVDMDomainDay_SendQueryErrorIsFatal_ISPErrorTolerated(t *testing.T) {
	// A failed per-ISP cell must NOT sink the read; a failed SEND totals
	// query must (nothing to anchor the scorecard on).
	api := &stubVDMAPI{values: vdmStubValues(), failIDs: map[string]bool{"i|Yahoo|OPEN": true}}
	r := newVDMReaderWithAPI(api, nil)
	day := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	got, err := r.vdmDomainDay(context.Background(), "em.discountblog.com", day)
	if err != nil {
		t.Fatalf("per-ISP error must be tolerated: %v", err)
	}
	if got.ByISP[1].ISP != "Yahoo" || got.ByISP[1].Open != 0 {
		t.Errorf("failed cell should read 0: %+v", got.ByISP)
	}

	api2 := &stubVDMAPI{values: vdmStubValues(), failIDs: map[string]bool{"t|SEND": true}}
	r2 := newVDMReaderWithAPI(api2, nil)
	if _, err := r2.vdmDomainDay(context.Background(), "em.discountblog.com", day); err == nil {
		t.Fatal("failed SEND totals query must return an error")
	}
}

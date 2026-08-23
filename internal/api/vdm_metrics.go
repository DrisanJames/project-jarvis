package api

// SES VDM domain×day reader for the Day Cards scorecard.
//
// The doctrine scoreboard for per-(sending domain × ISP) delivery statements
// is SES VDM, never PG counters (memory pg-delivery-confirmation-undercount).
// This reader mirrors agents/reporting/lane_tolerance.py exactly:
//
//   - sesv2 BatchGetMetricData, Namespace "VDM", region us-west-1
//     (SES_REGION env override, same convention as the ses_vdm_snapshot boot
//     site in cmd/server/main.go — VDM metrics live in us-west-1, NOT the
//     prod us-west-2).
//   - Metrics: SEND DELIVERY OPEN CLICK COMPLAINT PERMANENT_BOUNCE
//     TRANSIENT_BOUNCE (ses.AllMetrics(), the same 7 lane_tolerance queries).
//   - Complete UTC-day bounds [midnight, midnight+24h) — the API rejects
//     non-midnight daily windows.
//   - Dimensioning: domain totals via one EMAIL_IDENTITY-only pass; per-ISP
//     rows via EMAIL_IDENTITY × ISP over the raw AWS ISP name list
//     lane_tolerance uses. Queries are chunked at 10 (the BatchGetMetricData
//     limit lane_tolerance chunks by).
//
// IDENTITY RESOLUTION: the Day Cards `domain` param IS
// mailing_sending_profiles.sending_domain, and the profile's sending_domain
// IS the SES identity — em.<apex> for the estate, m.<apex> where that is the
// live profile (m.wcl-heloc.com; verified against the SES identity list
// 2026-08-17 in internal/worker/ses_vdm_snapshot.go — there is no em.wcl
// identity). So the domain passes through VERBATIM as EMAIL_IDENTITY; no
// em.↔m. rewriting is ever attempted here.
//
// Errors are returned to the caller, never fatal — day_cards.go degrades to
// PG context with a vdm_error string.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	sespkg "github.com/ignite/sparkpost-monitor/internal/ses"
)

// vdmRawISPs is the raw AWS VDM ISP name list, mirrored VERBATIM from
// lane_tolerance.py ISPS (a superset of isp.VDMISPs(), which stops at the 7
// canonical groups — the scorecard wants the full lane map incl. Comcast,
// Sbcglobal, Verizon, Charter and the UNKNOWN_ISP residual).
var vdmRawISPs = []string{
	"Gmail", "Hotmail", "Yahoo", "Aol", "Att", "Comcast", "Cox",
	"Sbcglobal", "Icloud", "Verizon", "Charter", "UNKNOWN_ISP",
}

// vdmPerISPMetrics is the metric subset carried per ISP row (the Day Cards
// mini table); the 7-metric full set stays at the domain level.
var vdmPerISPMetrics = []string{
	sespkg.MetricSend, sespkg.MetricDelivery, sespkg.MetricOpen, sespkg.MetricClick,
}

// vdmBatchQueryLimit is the BatchGetMetricData per-call query cap
// (lane_tolerance chunks by 10; the API rejects larger batches).
const vdmBatchQueryLimit = 10

// Cache TTLs: a still-accumulating day refreshes every 15 min; a complete UTC
// day is immutable enough for 6h (VDM buckets move slowly — same posture as
// the 6h ses_vdm_snapshot worker).
const (
	vdmCachePartialTTL  = 15 * time.Minute
	vdmCacheCompleteTTL = 6 * time.Hour
)

// vdmISPStat is one (domain, raw AWS ISP) row of the scorecard.
type vdmISPStat struct {
	ISP       string `json:"isp"`
	Send      int64  `json:"send"`
	Delivered int64  `json:"delivered"`
	Open      int64  `json:"open"`
	Click     int64  `json:"click"`
}

// vdmDomainDayStats is one sending domain's VDM counters for one UTC day.
type vdmDomainDayStats struct {
	Domain          string       `json:"domain"`
	DayUTC          string       `json:"day_utc"`
	Send            int64        `json:"send"`
	Delivered       int64        `json:"delivered"`
	Open            int64        `json:"open"`
	Click           int64        `json:"click"`
	Complaint       int64        `json:"complaint"`
	PermanentBounce int64        `json:"permanent_bounce"`
	TransientBounce int64        `json:"transient_bounce"`
	ByISP           []vdmISPStat `json:"by_isp"`
}

// vdmBatchAPI is the test seam over the one sesv2 call this reader makes.
// *sesv2.Client satisfies it (same seam shape as ses.MetricsAPI).
type vdmBatchAPI interface {
	BatchGetMetricData(ctx context.Context, params *sesv2.BatchGetMetricDataInput, optFns ...func(*sesv2.Options)) (*sesv2.BatchGetMetricDataOutput, error)
}

type vdmCacheEntry struct {
	stats     vdmDomainDayStats
	fetchedAt time.Time
	ttl       time.Duration
}

// vdmReader fetches and caches (domain, UTC day) VDM stats. Zero-value-plus-
// constructor ready; the sesv2 client is built lazily on first use so boot
// never blocks on AWS config.
type vdmReader struct {
	mu    sync.Mutex
	api   vdmBatchAPI
	now   func() time.Time
	cache map[string]vdmCacheEntry
}

func newVDMReader() *vdmReader {
	return &vdmReader{now: time.Now, cache: map[string]vdmCacheEntry{}}
}

// newVDMReaderWithAPI is the unit-test constructor: everything real except
// the wire.
func newVDMReaderWithAPI(api vdmBatchAPI, now func() time.Time) *vdmReader {
	if now == nil {
		now = time.Now
	}
	return &vdmReader{api: api, now: now, cache: map[string]vdmCacheEntry{}}
}

// sharedVDMReader is the process-wide default (one cache for every request).
var sharedVDMReader = newVDMReader()

// vdmRegion resolves the VDM metrics region: SES_REGION env, default
// us-west-1 — the SAME convention as cmd/server/main.go's snapshot boot.
// VDM data does NOT live in us-west-2.
func vdmRegion() string {
	if r := strings.TrimSpace(os.Getenv("SES_REGION")); r != "" {
		return r
	}
	return "us-west-1"
}

// client lazily builds the sesv2 client on the DEFAULT AWS credential chain
// (task role in ECS — no static keys), standard retryer, region us-west-1.
// Mirrors ses.NewClientForRegion.
func (r *vdmReader) client(ctx context.Context) (vdmBatchAPI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.api != nil {
		return r.api, nil
	}
	region := vdmRegion()
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithRetryer(func() aws.Retryer { return retry.NewStandard() }),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for region %s: %w", region, err)
	}
	r.api = sesv2.NewFromConfig(awsCfg)
	return r.api, nil
}

func vdmCacheKey(domain string, day time.Time) string {
	return domain + "|" + day.Format("2006-01-02")
}

// vdmDomainDay returns one sending domain's VDM counters for one UTC day,
// serving from the in-process cache when fresh. dayUTC is normalized to
// midnight UTC (the API rejects non-midnight daily bounds).
func (r *vdmReader) vdmDomainDay(ctx context.Context, domain string, dayUTC time.Time) (*vdmDomainDayStats, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("vdm: domain is required")
	}
	day := time.Date(dayUTC.Year(), dayUTC.Month(), dayUTC.Day(), 0, 0, 0, 0, time.UTC)
	end := day.Add(24 * time.Hour)

	key := vdmCacheKey(domain, day)
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && r.now().Sub(e.fetchedAt) < e.ttl {
		out := e.stats // copy
		r.mu.Unlock()
		return &out, nil
	}
	r.mu.Unlock()

	api, err := r.client(ctx)
	if err != nil {
		return nil, err
	}

	stats, err := fetchVDMDomainDay(ctx, api, domain, day, end)
	if err != nil {
		return nil, err
	}

	ttl := vdmCachePartialTTL
	if !end.After(r.now()) { // the UTC day is complete
		ttl = vdmCacheCompleteTTL
	}
	r.mu.Lock()
	r.cache[key] = vdmCacheEntry{stats: *stats, fetchedAt: r.now(), ttl: ttl}
	// Opportunistic prune — the key space is small (domains × days) but a
	// long-lived process shouldn't accrete stale complete-day entries forever.
	if len(r.cache) > 512 {
		for k, e := range r.cache {
			if r.now().Sub(e.fetchedAt) >= e.ttl {
				delete(r.cache, k)
			}
		}
	}
	r.mu.Unlock()
	return stats, nil
}

// fetchVDMDomainDay runs the two lane_tolerance-shaped passes:
// pass 1 — all 7 metrics dimensioned by EMAIL_IDENTITY only (domain totals);
// pass 2 — SEND/DELIVERY/OPEN/CLICK dimensioned by EMAIL_IDENTITY × ISP for
// every raw AWS ISP name in vdmRawISPs.
func fetchVDMDomainDay(ctx context.Context, api vdmBatchAPI, domain string, start, end time.Time) (*vdmDomainDayStats, error) {
	// Pass 1: domain totals.
	totalsQ := make([]types.BatchGetMetricDataQuery, 0, len(sespkg.AllMetrics()))
	for _, m := range sespkg.AllMetrics() {
		totalsQ = append(totalsQ, types.BatchGetMetricDataQuery{
			Id:        aws.String("t|" + m),
			Namespace: types.MetricNamespaceVdm,
			Metric:    types.Metric(m),
			Dimensions: map[string]string{
				string(types.MetricDimensionNameEmailIdentity): domain,
			},
			StartDate: aws.Time(start),
			EndDate:   aws.Time(end),
		})
	}
	totals, failed, err := vdmRunQueries(ctx, api, totalsQ)
	if err != nil {
		return nil, err
	}
	if failed["t|"+sespkg.MetricSend] {
		return nil, fmt.Errorf("vdm: SEND totals query failed for %s %s", domain, start.Format("2006-01-02"))
	}

	// Pass 2: per-ISP rows.
	ispQ := make([]types.BatchGetMetricDataQuery, 0, len(vdmRawISPs)*len(vdmPerISPMetrics))
	for _, isp := range vdmRawISPs {
		for _, m := range vdmPerISPMetrics {
			ispQ = append(ispQ, types.BatchGetMetricDataQuery{
				Id:        aws.String("i|" + isp + "|" + m),
				Namespace: types.MetricNamespaceVdm,
				Metric:    types.Metric(m),
				Dimensions: map[string]string{
					string(types.MetricDimensionNameEmailIdentity): domain,
					string(types.MetricDimensionNameIsp):           isp,
				},
				StartDate: aws.Time(start),
				EndDate:   aws.Time(end),
			})
		}
	}
	ispVals, _, err := vdmRunQueries(ctx, api, ispQ)
	if err != nil {
		return nil, err
	}

	out := &vdmDomainDayStats{
		Domain:          domain,
		DayUTC:          start.Format("2006-01-02"),
		Send:            totals["t|"+sespkg.MetricSend],
		Delivered:       totals["t|"+sespkg.MetricDelivery],
		Open:            totals["t|"+sespkg.MetricOpen],
		Click:           totals["t|"+sespkg.MetricClick],
		Complaint:       totals["t|"+sespkg.MetricComplaint],
		PermanentBounce: totals["t|"+sespkg.MetricPermanentBounce],
		TransientBounce: totals["t|"+sespkg.MetricTransientBounce],
	}
	for _, isp := range vdmRawISPs {
		row := vdmISPStat{
			ISP:       isp,
			Send:      ispVals["i|"+isp+"|"+sespkg.MetricSend],
			Delivered: ispVals["i|"+isp+"|"+sespkg.MetricDelivery],
			Open:      ispVals["i|"+isp+"|"+sespkg.MetricOpen],
			Click:     ispVals["i|"+isp+"|"+sespkg.MetricClick],
		}
		if row.Send > 0 {
			out.ByISP = append(out.ByISP, row)
		}
	}
	sort.SliceStable(out.ByISP, func(i, j int) bool { return out.ByISP[i].Send > out.ByISP[j].Send })
	return out, nil
}

// vdmRunQueries executes queries in chunks of vdmBatchQueryLimit, summing
// each result's data points (daily granularity) into {Id: total}. Per-query
// errors (output.Errors) are tolerated — the id lands in failed, never in
// vals — so one bad cell can't sink the panel (same posture as
// ses.GetMetricsForIdentityISP).
func vdmRunQueries(ctx context.Context, api vdmBatchAPI, queries []types.BatchGetMetricDataQuery) (vals map[string]int64, failed map[string]bool, err error) {
	vals = map[string]int64{}
	failed = map[string]bool{}
	for i := 0; i < len(queries); i += vdmBatchQueryLimit {
		endIdx := i + vdmBatchQueryLimit
		if endIdx > len(queries) {
			endIdx = len(queries)
		}
		out, callErr := api.BatchGetMetricData(ctx, &sesv2.BatchGetMetricDataInput{Queries: queries[i:endIdx]})
		if callErr != nil {
			return nil, nil, fmt.Errorf("vdm BatchGetMetricData: %w", callErr)
		}
		for _, e := range out.Errors {
			if e.Id != nil {
				failed[*e.Id] = true
			}
		}
		for _, res := range out.Results {
			if res.Id == nil || failed[*res.Id] {
				continue
			}
			var total int64
			for _, v := range res.Values {
				total += v
			}
			vals[*res.Id] += total
		}
	}
	return vals, failed, nil
}

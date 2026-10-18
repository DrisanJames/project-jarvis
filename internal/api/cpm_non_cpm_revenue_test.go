package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var ehealthRates = []nonCpmRate{{EffectiveFrom: "2026-10-01", Rate: 1.80}, {EffectiveFrom: "2026-10-17", Rate: 2.00}}

// Prod series for the eHealth CPC group, measured 2026-10-17 with the exact
// nonCpmVolumeSQL / nonCpmBillableClicksSQL (sent by campaign start day,
// billable = DISTINCT (subscriber, campaign) per Denver day).
var ehealthSent = map[string]int64{
	"2026-10-01": 452714, "2026-10-02": 477556, "2026-10-05": 418781, "2026-10-06": 411803,
	"2026-10-07": 453045, "2026-10-14": 493469, "2026-10-15": 302385, "2026-10-16": 539965,
	"2026-10-17": 27351,
}
var ehealthBillable = map[string]int64{
	"2026-10-01": 1483, "2026-10-02": 1464, "2026-10-03": 252, "2026-10-04": 368, "2026-10-05": 697,
	"2026-10-06": 992, "2026-10-07": 1420, "2026-10-08": 611, "2026-10-09": 505, "2026-10-10": 193,
	"2026-10-11": 99, "2026-10-12": 90, "2026-10-13": 73, "2026-10-14": 1267, "2026-10-15": 681,
	"2026-10-16": 1213, "2026-10-17": 277,
}

var oct17 = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestNonCpmRateOn(t *testing.T) {
	if _, ok := nonCpmRateOn(ehealthRates, "2026-09-30"); ok {
		t.Fatal("a day before the first rate must report no rate")
	}
	if r, _ := nonCpmRateOn(ehealthRates, "2026-10-16"); r != 1.80 {
		t.Fatalf("10-16 rate = %v, want 1.80", r)
	}
	if r, _ := nonCpmRateOn(ehealthRates, "2026-10-17"); r != 2.00 {
		t.Fatalf("10-17 rate = %v, want 2.00 (effective that day)", r)
	}
}

// The dated split: each day's clicks are priced at THAT day's rate.
func TestNonCpmPlanCPCDatedRateSplit(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc", Budget: 1000}
	p := nonCpmPlanFor(cfg, ehealthRates,
		map[string]int64{"2026-10-16": 100_000, "2026-10-17": 50_000},
		map[string]int64{"2026-10-16": 100, "2026-10-17": 50}, nil,
		0, 0, true, oct17, false)
	if p.Units != 150 || !near(p.Revenue, 280) || p.RevenueBasis != "rate" {
		t.Fatalf("units/revenue/basis = %d/%v/%s, want 150/280 (100×1.80 + 50×2.00)/rate", p.Units, p.Revenue, p.RevenueBasis)
	}
	if len(p.Days) != 2 || p.Days[0].Rate != 1.80 || p.Days[1].Rate != 2.00 || !near(p.Days[1].Revenue, 100) {
		t.Fatalf("days = %+v", p.Days)
	}
	if !near(p.EarnedEcpm, 280.0/150_000*1000) || !near(p.BudgetPct, 0.28) || !near(p.BudgetGap, 720) {
		t.Fatalf("ecpm/pct/gap = %v/%v/%v", p.EarnedEcpm, p.BudgetPct, p.BudgetGap)
	}
	// Closed month: forecast = actuals, nothing required.
	if p.ProjSent != 150_000 || !near(p.ProjRevenue, 280) || p.RequiredPerDay != 0 {
		t.Fatalf("closed-month forecast = %d/%v/%v", p.ProjSent, p.ProjRevenue, p.RequiredPerDay)
	}
}

// eHealth October on the prod series: revenue at the split rate, eCPM,
// month-end forecast at the 3-day cadence, required revenue per day.
func TestNonCpmPlanEHealthOctober(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc", Budget: 40000}
	p := nonCpmPlanFor(cfg, ehealthRates, ehealthSent, ehealthBillable, nil, 0, 0, true, oct17, true)
	// 11,408 clicks through 10-16 × $1.80 + 277 on 10-17 × $2.00.
	if p.Sent != 3_577_069 || p.Units != 11_685 || !near(p.Revenue, 11408*1.80+277*2.00) {
		t.Fatalf("sent/units/revenue = %d/%d/%v", p.Sent, p.Units, p.Revenue)
	}
	if p.CurrentRate != 2.00 || int(p.EarnedEcpm*1000) != 5895 {
		t.Fatalf("rate/eCPM = %v/%v", p.CurrentRate, p.EarnedEcpm)
	}
	// 3 complete days 10-14..10-16: 445,273 sent/day and 1,053.67 clicks/day;
	// 14 days after today; forward clicks priced at today's $2.00.
	if p.ProjSent != 3_577_069+int64(math.Round(1335819.0/3*14)) {
		t.Fatalf("projected sent = %d", p.ProjSent)
	}
	wantProj := math.Round((p.Revenue+3161.0/3*14*2.00)*100) / 100
	if !near(p.ProjRevenue, wantProj) || !near(p.ProjEcpm, wantProj*1000/float64(p.ProjSent)) {
		t.Fatalf("projected revenue/eCPM = %v/%v, want %v", p.ProjRevenue, p.ProjEcpm, wantProj)
	}
	// (40,000 − revenue) over the 15 days incl. today.
	if !near(p.RequiredPerDay, (40000-p.Revenue)/15) || !near(p.ProjGap, 40000-wantProj) {
		t.Fatalf("required/day = %v, proj gap = %v", p.RequiredPerDay, p.ProjGap)
	}
	t.Logf("eHealth Oct MTD: sent=%d billable=%d revenue=$%.2f eCPM=$%.3f | proj sent=%d rev=$%.2f eCPM=$%.3f",
		p.Sent, p.Units, p.Revenue, p.EarnedEcpm, p.ProjSent, p.ProjRevenue, p.ProjEcpm)
}

func TestNonCpmPlanPaidOverridesModel(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc", Budget: 10000, PaidRevenue: 5000}
	p := nonCpmPlanFor(cfg, ehealthRates, map[string]int64{"2026-10-16": 1_000_000},
		map[string]int64{"2026-10-16": 1000}, nil, 0, 0, true, oct17, false)
	if p.RevenueBasis != "paid" || p.Revenue != 5000 || !near(p.ModeledRevenue, 1800) {
		t.Fatalf("paid must REPLACE the model, keeping it visible: %+v", p)
	}
}

// FAIL LOUD: a failed click pass must not price zero clicks as fact.
func TestNonCpmPlanCPCClicksUnavailable(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc", Budget: 10000}
	p := nonCpmPlanFor(cfg, ehealthRates, ehealthSent, nil, nil, 0, 0, false, oct17, true)
	if p.RevenueBasis != "unavailable" || p.Revenue != 0 || len(p.Warnings) == 0 {
		t.Fatalf("want basis=unavailable with a warning, got %+v", p)
	}
	if p.RequiredPerDay != 0 || p.ProjSent != p.Sent {
		t.Fatalf("no forecast may be built on missing clicks: %+v", p)
	}
}

func TestNonCpmPlanCPCWithoutRateWarns(t *testing.T) {
	p := nonCpmPlanFor(&nonCpmRevenueConfig{PayoutModel: "cpc"}, nil,
		map[string]int64{"2026-10-16": 10}, map[string]int64{"2026-10-16": 5}, nil, 0, 0, true, oct17, false)
	if p.RevenueBasis != "none" || len(p.Warnings) == 0 {
		t.Fatalf("CPC with no rate must warn, got %+v", p)
	}
}

func TestNonCpmPlanCPL(t *testing.T) {
	sent := map[string]int64{"2026-10-14": 100_000, "2026-10-15": 100_000, "2026-10-16": 100_000}
	// Configured rate: conversions × today's rate; forecast holds yield/send.
	p := nonCpmPlanFor(&nonCpmRevenueConfig{PayoutModel: "cpl", Budget: 3000},
		[]nonCpmRate{{EffectiveFrom: "2026-10-01", Rate: 25}}, sent, nil, nil, 20, 999, true, oct17, true)
	if p.RevenueBasis != "rate" || p.Revenue != 500 || p.ProjSent != 300_000+100_000*14 || !near(p.ProjRevenue, 500.0*1_700_000/300_000) {
		t.Fatalf("cpl = %+v", p)
	}
	// No config: the supplied Everflow payout.
	p = nonCpmPlanFor(nil, nil, sent, nil, nil, 20, 640, true, oct17, false)
	if p.Configured || p.RevenueBasis != "everflow" || p.Revenue != 640 {
		t.Fatalf("unconfigured = %+v", p)
	}
}

// ─── Handler ────────────────────────────────────────────────────────────────

const testOrg = "00000000-0000-0000-0000-000000000001"

func nonCpmHandlerMock(t *testing.T) (*CpmPlannerHandlers, sqlmock.Sqlmock, func()) {
	h, mock, done := pacingMock(t)
	h.dealIdentity, h.dealIdentityAt = map[string][2]string{}, time.Now() // deal map cached
	mock.ExpectQuery(`NOW\(\) AT TIME ZONE 'America/Denver'`).
		WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(oct17))
	mock.ExpectQuery(`c\.sent_count`).WithArgs(testOrg, "2026-10-01", "2026-10-17").
		WillReturnRows(sqlmock.NewRows([]string{"identity", "day", "sent", "delivered"}).
			AddRow("ehealth cpc - switchers", "2026-10-16", 300000, 290000).
			AddRow("ehealth cpc - advisor", "2026-10-16", 100000, 97000).
			AddRow("ehealth cpc - switchers", "2026-10-17", 20000, 0))
	mock.ExpectExec(`SET statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`COUNT\(DISTINCT te\.subscriber_id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"identity", "clicks", "clickers"}).
			AddRow("ehealth cpc - switchers", 900, 400))
	mock.ExpectQuery(`FROM mailing_offer_suppressions`).WillReturnRows(sqlmock.NewRows([]string{"id", "n"}))
	mock.ExpectQuery(`FROM mailing_everflow_conversions`).WillReturnRows(sqlmock.NewRows([]string{"id", "n", "rev", "total"}))
	mock.ExpectQuery(`FROM mailing_offer_groups`).WithArgs(testOrg).
		WillReturnRows(sqlmock.NewRows([]string{"id", "g", "co"}).
			AddRow("ehealth cpc - switchers", "eHealth CPC", false).
			AddRow("ehealth cpc - advisor", "eHealth CPC", false))
	return h, mock, done
}

func TestHandleNonCpmPerformanceCPCPlan(t *testing.T) {
	h, mock, done := nonCpmHandlerMock(t)
	defer done()
	mock.ExpectQuery(`FROM mailing_offer_revenue_config`).WithArgs(testOrg, "2026-10-01").
		WillReturnRows(sqlmock.NewRows([]string{"g", "b", "m", "p", "n"}).AddRow("eHealth CPC", 20000.0, "cpc", 0.0, ""))
	mock.ExpectQuery(`FROM mailing_offer_payout_rates`).WithArgs(testOrg).
		WillReturnRows(sqlmock.NewRows([]string{"g", "d", "r"}).
			AddRow("eHealth CPC", "2026-10-01", 1.80).AddRow("eHealth CPC", "2026-10-17", 2.00))
	mock.ExpectQuery(`= ANY\(\$2::text\[\]\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1").AddRow("c2"))
	mock.ExpectExec(`SET statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`te\.campaign_id = ANY\(\$4::uuid\[\]\)`).
		WillReturnRows(sqlmock.NewRows([]string{"identity", "day", "billable"}).
			AddRow("ehealth cpc - switchers", "2026-10-16", 300).
			AddRow("ehealth cpc - advisor", "2026-10-16", 100).
			AddRow("ehealth cpc - switchers", "2026-10-17", 50))
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE ec\.payout > 0\)`).WithArgs(testOrg, "2026-10-01", "2026-10-17").
		WillReturnRows(sqlmock.NewRows([]string{"id", "day", "n", "payout"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/mailing/cpm-planner/non-cpm", nil)
	req.Header.Set("X-Organization-ID", testOrg)
	h.HandleNonCpmPerformance(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Groups   []nonCpmGroupRow `json:"groups"`
		Degraded []string         `json:"degraded"`
		Totals   struct {
			NonCpm nonCpmTotals `json:"non_cpm"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Degraded) != 0 || len(resp.Groups) != 1 {
		t.Fatalf("degraded=%v groups=%d", resp.Degraded, len(resp.Groups))
	}
	g := resp.Groups[0]
	// 400 clicks on 10-16 × $1.80 + 50 on 10-17 × $2.00 = $820.
	if g.GroupName != "eHealth CPC" || g.Sent != 420000 || g.Billable != 450 || !near(g.Plan.Revenue, 820) || !near(g.Revenue, 820) {
		t.Fatalf("group = sent %d billable %d plan %v revenue %v", g.Sent, g.Billable, g.Plan.Revenue, g.Revenue)
	}
	if !near(g.Ecpm, 820.0/420000*1000) || !near(resp.Totals.NonCpm.Revenue, 820) || resp.Totals.NonCpm.Sent != 420000 {
		t.Fatalf("eCPM %v totals %+v", g.Ecpm, resp.Totals.NonCpm)
	}
	if !near(g.Plan.BudgetGap, 20000-820) || !near(g.Plan.RequiredPerDay, (20000-820)/15.0) {
		t.Fatalf("plan = %+v", g.Plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// FAIL LOUD: a config read failure is announced and never cached — it must
// not render as "no budget set".
func TestHandleNonCpmPerformanceConfigFailureIsDegraded(t *testing.T) {
	h, mock, done := nonCpmHandlerMock(t)
	defer done()
	mock.ExpectQuery(`FROM mailing_offer_revenue_config`).WillReturnError(errors.New("relation does not exist"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/mailing/cpm-planner/non-cpm", nil)
	req.Header.Set("X-Organization-ID", testOrg)
	h.HandleNonCpmPerformance(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"revenue-config"`) {
		t.Fatalf("want degraded revenue-config, got %d %s", rr.Code, rr.Body.String())
	}
	if len(h.nonCpmCache) != 0 {
		t.Fatal("a degraded build must not be cached")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHandleUpsertRevenueConfig(t *testing.T) {
	h, mock, done := pacingMock(t)
	defer done()
	h.nonCpmCache = map[string]nonCpmCached{testOrg + "|2026-10": {}, "other|2026-10": {}}

	// Validation rejects before any write.
	for _, body := range []string{
		`{"group_name":"eHealth CPC","month":"2026-10","payout_model":"cpm"}`,
		`{"group_name":"","month":"2026-10","payout_model":"cpc"}`,
		`{"group_name":"eHealth CPC","month":"2026-10","payout_model":"cpc","rates":[{"effective_from":"10/17","rate":2}]}`,
		`{"group_name":"eHealth CPC","month":"2026-10","payout_model":"cpc","rates":[{"effective_from":"2026-10-17","rate":0}]}`,
	} {
		rr := httptest.NewRecorder()
		h.HandleUpsertRevenueConfig(rr, httptest.NewRequest("PUT", "/x", strings.NewReader(body)))
		if rr.Code != 400 {
			t.Fatalf("%s: want 400, got %d", body, rr.Code)
		}
	}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO mailing_offer_revenue_config`).
		WithArgs(testOrg, "eHealth CPC", "2026-10-01", 20000.0, "cpc", 0.0, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM mailing_offer_payout_rates`).WithArgs(testOrg, "eHealth CPC").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO mailing_offer_payout_rates`).WithArgs(testOrg, "eHealth CPC", "2026-10-01", 1.80).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_offer_payout_rates`).WithArgs(testOrg, "eHealth CPC", "2026-10-17", 2.00).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/mailing/cpm-planner/revenue-config", strings.NewReader(
		`{"group_name":"eHealth CPC","month":"2026-10","budget":20000,"payout_model":"CPC",
		  "rates":[{"effective_from":"2026-10-01","rate":1.8},{"effective_from":"2026-10-17","rate":2}]}`))
	req.Header.Set("X-Organization-ID", testOrg)
	h.HandleUpsertRevenueConfig(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if _, ok := h.nonCpmCache[testOrg+"|2026-10"]; ok {
		t.Fatal("saving config must invalidate this org's cached builds")
	}
	if _, ok := h.nonCpmCache["other|2026-10"]; !ok {
		t.Fatal("another org's cache must be untouched")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Everflow postbacks take over from the first postback day: those days price
// the SUPPLIED payout on Everflow's count, earlier days keep tracked × rate,
// and both counts stay visible per day.
func TestNonCpmPlanCPCPostbackCutover(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc", Budget: 1000}
	p := nonCpmPlanFor(cfg, ehealthRates,
		map[string]int64{"2026-10-15": 100_000, "2026-10-16": 100_000, "2026-10-17": 50_000},
		map[string]int64{"2026-10-15": 100, "2026-10-16": 100, "2026-10-17": 60},
		map[string]nonCpmPostbackDay{"2026-10-16": {Count: 3, Payout: 5.4}, "2026-10-17": {Count: 40, Payout: 80}},
		0, 0, true, oct17, false)
	// 10-16 = the setup day (postbacks switched on mid-day) → stays tracked.
	if p.RevenueBasis != "postback" || p.PostbackSince != "2026-10-16" || p.PostbackFrom != "2026-10-17" {
		t.Fatalf("basis/since/from = %s/%s/%s, want postback/2026-10-16/2026-10-17", p.RevenueBasis, p.PostbackSince, p.PostbackFrom)
	}
	// 200 tracked × $1.80 before the cutover + Everflow's $80 on 10-17.
	if p.Units != 240 || !near(p.Revenue, 200*1.80+80) {
		t.Fatalf("units/revenue = %d/%v, want 240/440", p.Units, p.Revenue)
	}
	if p.TrackedUnits != 260 || p.PostbackUnits != 43 {
		t.Fatalf("tracked/postback = %d/%d, want 260/43 (both kept for the gap)", p.TrackedUnits, p.PostbackUnits)
	}
	last := p.Days[len(p.Days)-1]
	if last.Source != "postback" || last.Units != 40 || last.TrackedUnits != 60 || p.Days[1].Source != "tracked" || p.Days[1].PostbackUnits != 3 {
		t.Fatalf("days = %+v", p.Days)
	}
}

// A day of postbacks with no payout is priced at the dated rate; a failed
// tracked pass no longer blanks the group once postbacks exist.
func TestNonCpmPlanCPCPostbackZeroPayoutAndTrackedFailure(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc"}
	p := nonCpmPlanFor(cfg, ehealthRates, map[string]int64{"2026-10-17": 10_000}, nil,
		map[string]nonCpmPostbackDay{"2026-10-16": {Count: 1, Payout: 1.8}, "2026-10-17": {Count: 5}}, 0, 0, false, oct17, false)
	if p.RevenueBasis != "postback" || !near(p.Revenue, 10) || len(p.Warnings) == 0 { // 10-16 tracked (nil) + 5 × $2.00
		t.Fatalf("want postback basis, 5 × $2.00, tracked-failure warning; got %+v", p)
	}
}

// A reversal-only day (no paid click) never trips the cutover.
func TestNonCpmPlanCPCReversalIsNotCutover(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc"}
	p := nonCpmPlanFor(cfg, ehealthRates, map[string]int64{"2026-10-01": 10_000},
		map[string]int64{"2026-10-01": 50},
		map[string]nonCpmPostbackDay{"2026-10-01": {Count: 0, Payout: -1.75}}, 0, 0, true, oct17, false)
	if p.RevenueBasis != "rate" || p.PostbackSince != "" || !near(p.Revenue, 50*1.80) {
		t.Fatalf("reversal must not cut over: %+v", p)
	}
}

// The first postback day is the setup day: still priced on tracked clicks, so a
// late-evening test click cannot reprice a whole day on one postback.
func TestNonCpmPlanCPCSetupDayStaysTracked(t *testing.T) {
	cfg := &nonCpmRevenueConfig{PayoutModel: "cpc"}
	p := nonCpmPlanFor(cfg, ehealthRates, map[string]int64{"2026-10-17": 100_000},
		map[string]int64{"2026-10-17": 277},
		map[string]nonCpmPostbackDay{"2026-10-17": {Count: 1, Payout: 2}}, 0, 0, true, oct17, true)
	if p.RevenueBasis != "rate" || p.PostbackSince != "2026-10-17" || p.PostbackFrom != "2026-10-18" || p.Units != 277 || !near(p.Revenue, 554) {
		t.Fatalf("setup day must stay tracked: %+v", p)
	}
}

// The handler wires Everflow postbacks onto the CPC group: from the first
// postback day the group prices Everflow's paid clicks at the supplied payout.
func TestHandleNonCpmPerformanceCPCPostbacks(t *testing.T) {
	h, mock, done := nonCpmHandlerMock(t)
	defer done()
	mock.ExpectQuery(`FROM mailing_offer_revenue_config`).WithArgs(testOrg, "2026-10-01").
		WillReturnRows(sqlmock.NewRows([]string{"g", "b", "m", "p", "n"}).AddRow("eHealth CPC", 20000.0, "cpc", 0.0, ""))
	mock.ExpectQuery(`FROM mailing_offer_payout_rates`).WithArgs(testOrg).
		WillReturnRows(sqlmock.NewRows([]string{"g", "d", "r"}).
			AddRow("eHealth CPC", "2026-10-01", 1.80).AddRow("eHealth CPC", "2026-10-17", 2.00))
	mock.ExpectQuery(`= ANY\(\$2::text\[\]\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("c1").AddRow("c2"))
	mock.ExpectExec(`SET statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`te\.campaign_id = ANY\(\$4::uuid\[\]\)`).
		WillReturnRows(sqlmock.NewRows([]string{"identity", "day", "billable"}).
			AddRow("ehealth cpc - switchers", "2026-10-16", 300).
			AddRow("ehealth cpc - advisor", "2026-10-16", 100).
			AddRow("ehealth cpc - switchers", "2026-10-17", 50))
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE ec\.payout > 0\)`).WithArgs(testOrg, "2026-10-01", "2026-10-17").
		WillReturnRows(sqlmock.NewRows([]string{"id", "day", "n", "payout"}).
			AddRow("ehealth cpc - switchers", "2026-10-16", 2, 3.6). // setup day — stays tracked
			AddRow("ehealth cpc - switchers", "2026-10-17", 45, 90.0).
			AddRow("some cpl offer", "2026-10-17", 3, 150.0)) // not CPC — ignored by the plan

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/mailing/cpm-planner/non-cpm", nil)
	req.Header.Set("X-Organization-ID", testOrg)
	h.HandleNonCpmPerformance(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Groups   []nonCpmGroupRow `json:"groups"`
		Degraded []string         `json:"degraded"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var g *nonCpmGroupRow
	for i := range resp.Groups {
		if resp.Groups[i].GroupName == "eHealth CPC" {
			g = &resp.Groups[i]
		}
	}
	if g == nil || len(resp.Degraded) != 0 {
		t.Fatalf("degraded=%v groups=%+v", resp.Degraded, resp.Groups)
	}
	// 400 tracked on 10-16 × $1.80 + Everflow's 45 paid clicks = $90 on 10-17.
	p := g.Plan
	if p.RevenueBasis != "postback" || p.PostbackFrom != "2026-10-17" || p.Units != 445 || !near(p.Revenue, 720+90) || !near(g.Revenue, 810) {
		t.Fatalf("plan = basis %s since %s units %d revenue %v (row %v)", p.RevenueBasis, p.PostbackSince, p.Units, p.Revenue, g.Revenue)
	}
	if p.TrackedUnits != 450 || p.PostbackUnits != 47 {
		t.Fatalf("tracked/postback = %d/%d, want 450/47", p.TrackedUnits, p.PostbackUnits)
	}
}

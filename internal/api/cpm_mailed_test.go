package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// The mailed tracker must read campaign counters, never the tracking-events
// scan whose timeouts and once-per-day rollup made monthly volume unreliable.
func TestCpmMailedSQLShape(t *testing.T) {
	q := cpmMailedSQL()
	for _, want := range []string{"c.sent_count", "c.delivered_count", "WITH dm AS", "America/Denver", "$2::date", "$3::date",
		"FROM mailing_everflow_conversions", "JOIN dm ON dm.campaign_id = ec.campaign_id", "UNION ALL"} {
		if !strings.Contains(q, want) {
			t.Fatalf("cpmMailedSQL missing %q", want)
		}
	}
	if strings.Contains(q, "mailing_tracking_events") {
		t.Fatal("cpmMailedSQL must not scan mailing_tracking_events")
	}
	for _, st := range []string{"'draft'", "'cancelled'", "'deleted'", "'failed'"} {
		if !strings.Contains(q, st) {
			t.Fatalf("cpmMailedSQL must exclude status %s", st)
		}
	}
}

// Avg over the 3 COMPLETE days before today — today's partial day excluded.
func TestCpmMailedAvg3d(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	days := map[string]int64{
		"2026-10-17": 999_999, // today — must not count
		"2026-10-16": 300_000,
		"2026-10-15": 200_000,
		"2026-10-14": 100_000,
		"2026-10-13": 5_000_000, // 4 days back — must not count
	}
	if got := cpmMailedAvg3d(days, today); got != 200_000 {
		t.Fatalf("avg3d = %v, want 200000", got)
	}
	if got := cpmMailedAvg3d(map[string]int64{}, today); got != 0 {
		t.Fatalf("empty avg3d = %v, want 0", got)
	}
}

// End-to-end /pacing read: one 20M deal with a monthly target and $8k paid,
// campaign counters across 5 days, current month day 17 of 31.
func pacingMock(t *testing.T) (*CpmPlannerHandlers, sqlmock.Sqlmock, func()) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	return &CpmPlannerHandlers{db: db}, mock, func() { db.Close() }
}

var cpmDealCols = []string{"id", "name", "oid", "oname", "ef", "budget", "ecpm", "ecpa", "avg", "start", "end", "status", "notes", "pat", "conv", "c", "u", "payout"}

func TestHandleCurrentMonthPacingCounters(t *testing.T) {
	h, mock, done := pacingMock(t)
	defer done()
	org := "00000000-0000-0000-0000-000000000001"
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`NOW\(\) AT TIME ZONE 'America/Denver'`).
		WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(today))
	mock.ExpectQuery(`FROM mailing_cpm_deals d`).WithArgs(org).
		WillReturnRows(sqlmock.NewRows(cpmDealCols).
			AddRow("deal-lib", "Liberty Mutual", "", "", "338", 12000.0, 1.0, 100.0, 50000, start, nil, "active", "", "%liberty%", nil, start, start, 0.0).
			AddRow("deal-old", "Paused Old", "", "", "", 2000.0, 1.0, 0.0, 50000, start, nil, "paused", "", "", nil, start, start, 0.0))
	mock.ExpectQuery(`FROM mailing_cpm_deal_monthly_targets`).
		WillReturnRows(sqlmock.NewRows([]string{"deal_id", "b", "v", "e", "a", "paid", "notes"}).
			AddRow("deal-lib", 20000.0, 20000000, 1.0, 100.0, 8000.0, ""))
	mock.ExpectQuery(`c\.sent_count`).WithArgs(org, "2026-10-01", "2026-11-01").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "deal_id", "day", "sent", "delivered", "campaigns", "booked"}).
			AddRow("v", "deal-lib", "2026-10-13", 1000000, 950000, 20, 0). // outside the 3-day window
			AddRow("v", "deal-lib", "2026-10-14", 100000, 95000, 5, 0).
			AddRow("v", "deal-lib", "2026-10-15", 200000, 190000, 5, 0).
			AddRow("v", "deal-lib", "2026-10-16", 300000, 285000, 5, 0).
			AddRow("v", "deal-lib", "2026-10-17", 50000, 0, 3, 250000).
			AddRow("c", "deal-lib", "", 56, 0, 0, 0)) // conversions row must not count as volume
	mock.ExpectQuery(`FROM mailing_cpm_manual_conversions`).WithArgs(org, "2026-10-01", "2026-11-01").
		WillReturnRows(sqlmock.NewRows([]string{"deal_id", "c", "p"}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/mailing/cpm-planner/pacing", nil)
	req.Header.Set("X-Organization-ID", org)
	h.HandleCurrentMonthPacing(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Month string          `json:"month"`
		Deals []cpmPacingDeal `json:"deals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Month != "2026-10" || len(resp.Deals) != 1 {
		t.Fatalf("want 1 deal in 2026-10 (paused zero-volume deal dropped), got %s %+v", resp.Month, resp.Deals)
	}
	d := resp.Deals[0]
	if d.TargetVolume != 20_000_000 || d.TargetSource != "monthly" || d.MonthlyPaid == nil || *d.MonthlyPaid != 8000 {
		t.Fatalf("target = %d/%s paid=%v", d.TargetVolume, d.TargetSource, d.MonthlyPaid)
	}
	if d.MtdSent != 1_650_000 || d.MtdDelivered != 1_520_000 || d.Booked != 250_000 || d.SentToday != 50_000 {
		t.Fatalf("sent/delivered/booked/today = %d/%d/%d/%d", d.MtdSent, d.MtdDelivered, d.Booked, d.SentToday)
	}
	// Pacing runs on SENT: 18.35M over 15 days incl. today; projection =
	// 1.65M + 200k × 14.
	if d.Rate3d != 200_000 || int64(d.RequiredDaily) != 1_223_333 || d.Projected != 4_450_000 || d.OnPace {
		t.Fatalf("rate/req/proj/onPace = %v/%v/%d/%v", d.Rate3d, d.RequiredDaily, d.Projected, d.OnPace)
	}
	// Paid $8,000 replaces 56 × $100; 1.65M sent → $4.85 earned eCPM; the
	// $20k budget over 4.45M projected → $4.49.
	if d.RevenueBasis != "paid" || d.Revenue != 8000 || d.ConversionsNeeded != 200 || d.EfConversions != 56 || d.MtdConversions != 56 {
		t.Fatalf("model = %+v", d)
	}
	if int(d.BudgetEcpmAtProj*100) != 449 || int(d.ActualEcpm*100) != 484 || int(d.ProjRevenue) != 21575 {
		t.Fatalf("budget eCPM %v / earned %v / proj rev %v", d.BudgetEcpmAtProj, d.ActualEcpm, d.ProjRevenue)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A volume query failure must be an HTTP error, never a silent zero.
func TestHandleCurrentMonthPacingFailsLoud(t *testing.T) {
	h, mock, done := pacingMock(t)
	defer done()
	mock.ExpectQuery(`America/Denver`).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)))
	mock.ExpectQuery(`FROM mailing_cpm_deals d`).WillReturnRows(sqlmock.NewRows(cpmDealCols))
	mock.ExpectQuery(`FROM mailing_cpm_deal_monthly_targets`).WillReturnRows(sqlmock.NewRows([]string{"deal_id", "b", "v", "e", "a", "paid", "notes"}))
	mock.ExpectQuery(`c\.sent_count`).WillReturnError(errors.New("canceling statement due to statement timeout"))
	rr := httptest.NewRecorder()
	h.HandleCurrentMonthPacing(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != 500 {
		t.Fatalf("want 500 on volume query failure, got %d", rr.Code)
	}
}

func TestCpmMailedModelFor(t *testing.T) {
	// Modeled: 22 conv × $100 on 1.63M sent, projected 4.85M, $5k budget.
	m := cpmMailedModelFor(5000, 100, 1_631_613, 4_850_000, 22, 0, 0)
	if m.RevenueBasis != "model" || m.Revenue != 2200 || m.ConvNeeded != 50 || m.ProjConversions != 65 {
		t.Fatalf("model = %+v", m)
	}
	if int(m.EarnedEcpm*100) != 134 || int(m.BudgetEcpmAtProj*100) != 103 {
		t.Fatalf("earned %v budget %v", m.EarnedEcpm, m.BudgetEcpmAtProj)
	}
	// Paid wins over the model — never summed (double count).
	if p := cpmMailedModelFor(20000, 100, 4_000_000, 6_000_000, 56, 0, 8000); p.Revenue != 8000 || p.RevenueBasis != "paid" || p.ProjRevenue != 12000 {
		t.Fatalf("paid = %+v", p)
	}
	// Nothing sent, nothing earned: no division by zero, gap = budget.
	if z := cpmMailedModelFor(5000, 50, 0, 0, 0, 0, 0); z.RevenueBasis != "none" || z.EarnedEcpm != 0 || z.RevenueGap != 5000 {
		t.Fatalf("zero = %+v", z)
	}
}

// Paid-to-date is configurable through the monthly-target upsert: a value is
// written as $9, a negative one is refused before any write.
func TestUpsertMonthlyTargetPaidRevenue(t *testing.T) {
	put := func(body string) (*httptest.ResponseRecorder, sqlmock.Sqlmock, func()) {
		h, mock, done := pacingMock(t)
		mock.ExpectQuery(`SELECT EXISTS`).WillReturnRows(sqlmock.NewRows([]string{"e"}).AddRow(true))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "deal-lib")
		rctx.URLParams.Add("month", "2026-10")
		req := httptest.NewRequest("PUT", "/x", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rr := httptest.NewRecorder()
		if strings.Contains(body, "8000") {
			mock.ExpectExec(`paid_revenue`).
				WithArgs(sqlmock.AnyArg(), "deal-lib", sqlmock.AnyArg(), 20000.0, nil, 1.0, 100.0, sqlmock.AnyArg(), 8000.0).
				WillReturnResult(sqlmock.NewResult(0, 1))
		}
		h.HandleUpsertMonthlyTarget(rr, req)
		return rr, mock, done
	}
	rr, mock, done := put(`{"budget":20000,"ecpm":1,"ecpa":100,"paid_revenue":8000}`)
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	done()
	rr, mock, done = put(`{"paid_revenue":-1}`)
	defer done()
	if rr.Code != 400 {
		t.Fatalf("negative paid: want 400, got %d", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

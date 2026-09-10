package api

// Tests for the fulfillment verdict endpoint (GET /api/mailing/supply/verdict).
//
// What each block pins, and how it FAILS:
//
//   - The handler PROJECTS and never judges. There is exactly one implementation
//     of the ±5% rule (agents/reporting/supply_reconcile.py), so a status this
//     handler invented would be a second verdict. The test feeds a row whose
//     stored status contradicts a naive recomputation and asserts the STORED
//     status survives.
//   - `delivered` null is UNKNOWN, never 0 — and an all-null day leaves the
//     estate total null too, rather than silently summing to zero.
//   - live beats shadow for the same cell (mode is in the table's primary key,
//     so both can exist and only one is the answer).
//   - An empty day is DEGRADED, not healthy: zero rows must say the verdict is
//     unknown, or the screen reads as "nothing broken" on a day nobody graded.
//   - under/over always carry a reason in the composed verdict string.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var dripVerdictCols = []string{
	"sending_domain", "isp", "mode", "domain_contract_version",
	"promised", "minted", "sent", "delivered", "delivered_source",
	"tolerance_pct", "status", "reason", "window_closed_at", "computed_at",
}

func dripVerdictDecode(t *testing.T, body []byte) dripVerdictResponse {
	t.Helper()
	var out dripVerdictResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, string(body))
	}
	return out
}

func TestDripVerdict_ProjectsStoredStatusAndComposesReason(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	closed := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	computed := time.Date(2026, 9, 8, 20, 5, 0, 0, time.UTC)

	rows := sqlmock.NewRows(dripVerdictCols).
		// UNDER: delivered 5,347 vs promised 12,373.
		AddRow("em.learnpersonalloans.com", "att", "shadow", 6,
			12373, 32132, 5413, 5347, "lake:ignite_analytics.email_events",
			0.05, "under", "under_produced (would-be: refused_in_flight)", closed, computed).
		// OVER on a cap-0 ISP: promised 0 and 6,823 delivered. delta_pct has no
		// denominator and must stay null rather than divide by zero.
		AddRow("em.consumerpro.net", "microsoft", "shadow", 5,
			0, 0, 6865, 6823, "lake:ignite_analytics.email_events",
			0.05, "over", "unmetered", closed, computed).
		// FULFILLED, and deliberately 4% off the promise: a handler that
		// recomputed with a hardcoded tighter band would call this under.
		AddRow("em.discountblog.com", "aol", "live", 7,
			10000, 10000, 10000, 9600, "lake:ignite_analytics.email_events",
			0.05, "fulfilled", "", closed, computed).
		// PENDING: delivered not measured yet.
		AddRow("em.quizfiesta.com", "yahoo", "live", 4,
			5000, 5000, 4900, nil, "", 0.05, "pending", "delivered not yet measured", closed, computed)

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_fulfillment_verdict").WillReturnRows(rows)

	w := dripServe(svc, dripRequest(t, "GET", "/supply/verdict?day=2026-09-08", ""))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := dripVerdictDecode(t, w.Body.Bytes())

	if len(got.Verdicts) != 4 {
		t.Fatalf("want 4 verdict rows, got %d", len(got.Verdicts))
	}
	byCell := map[string]dripVerdictRow{}
	for _, v := range got.Verdicts {
		byCell[v.SendingDomain+"/"+v.ISP] = v
	}

	// The stored status survives. 9,600 vs 10,000 is -4%, inside ±5% — but the
	// point is that the handler did not do that arithmetic to decide.
	if s := byCell["em.discountblog.com/aol"].Status; s != "fulfilled" {
		t.Errorf("stored status was rewritten: got %q, want fulfilled", s)
	}
	if v := byCell["em.discountblog.com/aol"].Verdict; v != "fulfilled" {
		t.Errorf("fulfilled must stand alone, got %q", v)
	}

	// under/over always carry their reason in the composed string.
	if v := byCell["em.learnpersonalloans.com/att"].Verdict; v != "under:under_produced (would-be: refused_in_flight)" {
		t.Errorf("under verdict lost its reason: %q", v)
	}
	if v := byCell["em.consumerpro.net/microsoft"].Verdict; v != "over:unmetered" {
		t.Errorf("over verdict lost its producer: %q", v)
	}

	// promised = 0 has no percentage. A delta_pct here would be a divide-by-zero
	// dressed as a measurement.
	cap0 := byCell["em.consumerpro.net/microsoft"]
	if cap0.DeltaPct != nil {
		t.Errorf("delta_pct on a promised=0 cell must be null, got %v", *cap0.DeltaPct)
	}
	if cap0.Delta == nil || *cap0.Delta != 6823 {
		t.Errorf("delta on the cap-0 cell = %v, want 6823", cap0.Delta)
	}

	// null delivered is unknown, and it contributes to NEITHER anomaly volume.
	pending := byCell["em.quizfiesta.com/yahoo"]
	if pending.Delivered != nil {
		t.Errorf("delivered must stay null for a pending cell, got %d", *pending.Delivered)
	}
	if pending.Delta != nil {
		t.Errorf("delta must stay null when delivered is unknown, got %d", *pending.Delta)
	}

	s := got.Summary
	if s.Cells != 4 || s.Fulfilled != 1 || s.Under != 1 || s.Over != 1 || s.Pending != 1 {
		t.Errorf("summary counts wrong: %+v", s)
	}
	if s.Anomalies != 2 {
		t.Errorf("anomalies = %d, want 2 (under and over are equal-weight)", s.Anomalies)
	}
	if s.UnderVolume != 12373-5347 {
		t.Errorf("under_volume = %d, want %d", s.UnderVolume, 12373-5347)
	}
	if s.OverVolume != 6823 {
		t.Errorf("over_volume = %d, want 6823", s.OverVolume)
	}
	if s.Enforced != 2 {
		t.Errorf("enforced = %d, want 2 (two rows carry mode=live)", s.Enforced)
	}
	if s.Delivered == nil || *s.Delivered != 5347+6823+9600 {
		t.Errorf("estate delivered = %v, want %d", s.Delivered, 5347+6823+9600)
	}

	// Shadow cells must be named in degraded — a would-be verdict presented as a
	// verdict is the whole false-alarm class this work package removes.
	if !dripHasDegraded(got.Degraded, "SHADOW surface") {
		t.Errorf("degraded must name the shadow cells: %v", got.Degraded)
	}
	if !dripHasDegraded(got.Degraded, "delivery confirms up to ~3h late") {
		t.Errorf("degraded must explain the pending cell: %v", got.Degraded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestDripVerdict_EmptyDayIsUnknownNotHealthy(t *testing.T) {
	svc, mock, done := dripNewMock(t)
	defer done()

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM drip_fulfillment_verdict").
		WillReturnRows(sqlmock.NewRows(dripVerdictCols))

	w := dripServe(svc, dripRequest(t, "GET", "/supply/verdict?day=2026-09-08", ""))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got := dripVerdictDecode(t, w.Body.Bytes())

	if got.Summary.Cells != 0 || got.Summary.Anomalies != 0 {
		t.Errorf("empty day summary: %+v", got.Summary)
	}
	// The negative control. Zero anomalies on a day nobody graded must NOT read
	// as a clean estate.
	if got.Summary.Delivered != nil {
		t.Errorf("estate delivered on an empty day must be null (unknown), got %d", *got.Summary.Delivered)
	}
	if !dripHasDegraded(got.Degraded, "UNKNOWN, not fulfilled") {
		t.Errorf("an ungraded day must be degraded, got %v", got.Degraded)
	}
	if got.Labels["promised"] != dripLabelContracted || got.Labels["delivered"] != dripLabelActual {
		t.Errorf("labels missing or wrong: %v", got.Labels)
	}
	if got.AsOf.IsZero() {
		t.Error("as_of must be set on every supply response")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestDripComposeVerdict(t *testing.T) {
	cases := []struct{ status, reason, want string }{
		{"fulfilled", "", "fulfilled"},
		{"pending", "delivered not yet measured", "pending"},
		{"no_contract", "isp absent", "no_contract"},
		{"under", "no_lane_balance", "under:no_lane_balance"},
		{"over", "internal_auto_insurance_v10", "over:internal_auto_insurance_v10"},
		// A breach with no stored reason is still reportable, but it says so.
		{"under", "  ", "under:unexplained"},
	}
	for _, c := range cases {
		if got := dripComposeVerdict(c.status, c.reason); got != c.want {
			t.Errorf("dripComposeVerdict(%q,%q) = %q, want %q", c.status, c.reason, got, c.want)
		}
	}
}

func dripHasDegraded(list []string, sub string) bool {
	for _, d := range list {
		if strings.Contains(d, sub) {
			return true
		}
	}
	return false
}

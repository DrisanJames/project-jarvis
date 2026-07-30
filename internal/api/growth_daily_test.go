package api

// Growth endpoint tests — the rate/delta derivations the screen and the CSV
// both depend on, plus the handler's end-to-end shape over sqlmock. No DB.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const growthTestOrg = "11111111-2222-3333-4444-555555555555"

// ── Pure derivations ────────────────────────────────────────────────────────

func TestGrowthPct(t *testing.T) {
	cases := []struct {
		name string
		n, d int64
		want float64
	}{
		{"zero denominator never divides", 17, 0, 0},
		{"negative denominator never divides", 17, -3, 0},
		{"plain rate", 1, 4, 25},
		// The whole point of the EVENTS ÷ delivered convention: a scanner
		// opening one message many times legitimately exceeds 100%. Clamping
		// this would hide the exact signal the operator tracks.
		{"open rate may exceed 100%", 269062, 246295, 109.24},
		{"complaint-scale precision survives", 69, 246295, 0.03},
	}
	for _, c := range cases {
		if got := growthPct(c.n, c.d); got != c.want {
			t.Errorf("%s: growthPct(%d,%d) = %v, want %v", c.name, c.n, c.d, got, c.want)
		}
	}
}

func TestGrowthRelDelta(t *testing.T) {
	// A zero previous value makes the ratio undefined. It must come back nil
	// (rendered "n/a") — never 0%, which reads as "flat" and is a lie.
	if got := growthRelDelta(5, 0); got != nil {
		t.Errorf("zero previous must yield nil (undefined), got %v", *got)
	}
	if got := growthRelDelta(0, 0); got != nil {
		t.Errorf("0 from 0 is still undefined, got %v", *got)
	}
	got := growthRelDelta(1.89, 0.88)
	if got == nil {
		t.Fatal("expected a delta")
	}
	if *got != 114.77 { // the operator's own Jul 17 open-rate delta
		t.Errorf("growthRelDelta(1.89, 0.88) = %v, want 114.77", *got)
	}
	// Negative deltas must round AWAY from zero like positives do — the
	// int64(x+0.5) idiom silently gives -47.41 here and understates declines.
	neg := growthRelDelta(134451, 255724)
	if neg == nil {
		t.Fatal("expected a negative delta")
	}
	if *neg != -47.42 {
		t.Errorf("growthRelDelta(134451, 255724) = %v, want -47.42", *neg)
	}
}

func TestParseGrowthDay(t *testing.T) {
	if _, ok := parseGrowthDay(""); ok {
		t.Error("empty must not parse (caller applies its default)")
	}
	if _, ok := parseGrowthDay("2026/07/29"); ok {
		t.Error("non-ISO must not parse")
	}
	if _, ok := parseGrowthDay("garbage"); ok {
		t.Error("garbage must not parse")
	}
	d, ok := parseGrowthDay(" 2026-07-29 ")
	if !ok || d.Format("2006-01-02") != "2026-07-29" {
		t.Errorf("valid day failed: %v %v", d, ok)
	}
}

func TestGrowthNotes_AttributionCaveatFires(t *testing.T) {
	before, _ := time.Parse("2006-01-02", "2026-06-01")
	after, _ := time.Parse("2006-01-02", "2026-07-10")

	// Ranges reaching before the attribution date MUST say so — silently
	// under-counting a domain's history is the failure this guards.
	notes := growthNotes(before, "discountblog.com", 30)
	if !containsSub(notes, growthDomainAttributionFrom) {
		t.Errorf("per-domain pre-attribution range must carry the caveat: %v", notes)
	}
	notes = growthNotes(before, "", 30)
	if !containsSub(notes, growthUnattributedLabel) {
		t.Errorf("all-domain pre-attribution range must explain the unattributed bucket: %v", notes)
	}

	// A range entirely after the attribution date must NOT cry wolf.
	notes = growthNotes(after, "discountblog.com", 30)
	if containsSub(notes, growthDomainAttributionFrom) {
		t.Errorf("post-attribution range must not carry the caveat: %v", notes)
	}

	// An empty result explains itself rather than looking like zero volume.
	if notes = growthNotes(after, "", 0); !containsSub(notes, "back-fills") {
		t.Errorf("empty result must explain the backfill: %v", notes)
	}
}

func containsSub(notes []string, sub string) bool {
	for _, n := range notes {
		if len(sub) > 0 && len(n) >= len(sub) {
			for i := 0; i+len(sub) <= len(n); i++ {
				if n[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// ── Handler ─────────────────────────────────────────────────────────────────

func growthRowCols() []string {
	return []string{
		"day", "delivered", "hard_bounce", "soft_bounce", "reputation_block",
		"complaints", "unsubs", "open_events", "open_subs", "click_events",
		"click_subs", "action_click_subs", "computed_at",
	}
}

func TestHandleDaily_DerivesRatesAndDeltas(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now()
	rows := sqlmock.NewRows(growthRowCols()).
		// Two Denver days of microsoft-shaped numbers taken from the real Jul
		// 27/28 slice, so the derived rates are checkable by hand.
		AddRow(time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
			int64(259565), int64(80), int64(1777), int64(0), int64(117), int64(90),
			int64(278180), int64(70000), int64(25219), int64(5000), int64(900), now).
		AddRow(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
			int64(246295), int64(14), int64(1689), int64(0), int64(69), int64(80),
			int64(269062), int64(68957), int64(25200), int64(5247), int64(825), now)
	mock.ExpectQuery("FROM mailing_growth_daily").WillReturnRows(rows)

	svc := NewGrowthService(db)
	r := chi.NewRouter()
	svc.RegisterRoutes(r)

	req := httptest.NewRequest(http.MethodGet,
		"/growth/daily?from=2026-07-27&to=2026-07-28&isp=microsoft", nil)
	req.Header.Set("X-Organization-ID", growthTestOrg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body growthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — %s", err, w.Body.String())
	}
	if len(body.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(body.Rows))
	}

	first, second := body.Rows[0], body.Rows[1]

	// Attempted is DERIVED = delivered + hard + soft (never a stored column).
	if want := int64(246295 + 14 + 1689); second.Attempted != want {
		t.Errorf("attempted = %d, want %d", second.Attempted, want)
	}
	// Open rate is EVENTS ÷ delivered and exceeds 100% here — the operator's
	// convention, verified against prod for this exact day.
	if second.OpenPct != 109.24 {
		t.Errorf("open_pct = %v, want 109.24", second.OpenPct)
	}
	if second.ClickPct != 10.23 {
		t.Errorf("click_pct = %v, want 10.23", second.ClickPct)
	}
	// The governing cohort is unique mailboxes on navigational links —
	// 825/246295 = 0.33%, three orders of magnitude below the raw click rate
	// because ~93% of raw clicked events are asset fetches.
	if second.ActionClickPct != 0.33 {
		t.Errorf("action_click_pct = %v, want 0.33", second.ActionClickPct)
	}

	// First row has no predecessor — every delta must be null, not zero.
	if first.DeltaDelivered != nil || first.DeltaDeliveredPct != nil ||
		first.DeltaOpenPct != nil || first.DeltaClickPct != nil {
		t.Error("first row deltas must all be null (no prior day)")
	}
	// Second row's deltas are relative to the first.
	if second.DeltaDelivered == nil || *second.DeltaDelivered != 246295-259565 {
		t.Errorf("delta_delivered wrong: %v", second.DeltaDelivered)
	}
	if second.DeltaDeliveredPct == nil {
		t.Fatal("delta_delivered_pct missing")
	}
	if *second.DeltaDeliveredPct != -5.11 {
		t.Errorf("delta_delivered_pct = %v, want -5.11", *second.DeltaDeliveredPct)
	}

	// Totals aggregate the range and re-derive (not average) the rates.
	if body.Totals.Days != 2 {
		t.Errorf("totals.days = %d, want 2", body.Totals.Days)
	}
	if want := int64(259565 + 246295); body.Totals.Delivered != want {
		t.Errorf("totals.delivered = %d, want %d", body.Totals.Delivered, want)
	}
	if body.Totals.HardBounce+body.Totals.SoftBounce == 0 {
		t.Error("bounce totals must be carried")
	}
	// Hard and soft must remain separate fields — never a combined number.
	if body.Totals.HardBounce != 94 || body.Totals.SoftBounce != 3466 {
		t.Errorf("hard/soft must stay separate: hard=%d soft=%d", body.Totals.HardBounce, body.Totals.SoftBounce)
	}

	if body.Coverage.LakeEpoch != growthLakeEpoch {
		t.Errorf("coverage.lake_epoch = %q", body.Coverage.LakeEpoch)
	}
	if len(body.Notes) == 0 {
		t.Error("notes must always explain the rate conventions")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The range must be clamped to the lake epoch rather than returning leading
// empty days that read as "we sent nothing".
func TestHandleDaily_ClampsToLakeEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM mailing_growth_daily").
		WithArgs(growthLakeEpoch, "2026-07-29", "", "").
		WillReturnRows(sqlmock.NewRows(growthRowCols()))

	svc := NewGrowthService(db)
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	req := httptest.NewRequest(http.MethodGet, "/growth/daily?from=2020-01-01&to=2026-07-29", nil)
	req.Header.Set("X-Organization-ID", growthTestOrg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body growthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.From != growthLakeEpoch {
		t.Errorf("from = %q, want the clamped epoch %q", body.From, growthLakeEpoch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The sending_domain filter must bind as the brand APEX — the lake stores the
// apex, so an "em."-prefixed value has to normalise or the filter silently
// matches nothing.
func TestHandleDaily_NormalisesSendingDomainFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM mailing_growth_daily").
		WithArgs("2026-07-01", "2026-07-29", "discountblog.com", "microsoft").
		WillReturnRows(sqlmock.NewRows(growthRowCols()))

	svc := NewGrowthService(db)
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	req := httptest.NewRequest(http.MethodGet,
		"/growth/daily?from=2026-07-01&to=2026-07-29&sending_domain=em.discountblog.com&isp=MICROSOFT", nil)
	req.Header.Set("X-Organization-ID", growthTestOrg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("filter did not bind as expected: %v", err)
	}
}

// An empty filter must mean ALL — not a literal match on "".
func TestHandleDaily_EmptyFiltersMeanAll(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("FROM mailing_growth_daily").
		WithArgs("2026-07-01", "2026-07-29", "", "").
		WillReturnRows(sqlmock.NewRows(growthRowCols()))

	svc := NewGrowthService(db)
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	req := httptest.NewRequest(http.MethodGet, "/growth/daily?from=2026-07-01&to=2026-07-29", nil)
	req.Header.Set("X-Organization-ID", growthTestOrg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("empty filters must bind as '': %v", err)
	}
}

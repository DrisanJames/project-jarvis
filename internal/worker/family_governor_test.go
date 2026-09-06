package worker

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// Yahoo-family broadcast governor — Decide() contract (family_governor.go).
// These pin EXPECTED BEHAVIOUR: the math, fail-open, the negative controls
// (off / non-family / no contract issue ZERO queries), the 60s cache, and the
// idempotent ledger write. The dispatcher hook is covered in
// family_governor_dispatcher_test.go.

const (
	fgTestDomain = "m.discountblog.com"
	fgTestLane   = "broadcast-family.m.discountblog.com"
)

var fgTestDay = time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC) // 09:00 Denver

func fgNewMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func fgExpectContract(mock sqlmock.Sqlmock, lane string, ceiling any) {
	rows := sqlmock.NewRows([]string{"daily_ceiling"})
	if ceiling != nil {
		rows.AddRow(ceiling)
	}
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(lane).WillReturnRows(rows)
}

func fgExpectSpend(mock sqlmock.Sqlmock, domain string, spent int) {
	mock.ExpectQuery(`FROM mailing_campaign_queue q`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), domain).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(spent))
}

func fgExpectLedger(mock sqlmock.Sqlmock, waveID, domain, isp, mode string, requested, ceiling, spent, allowed int, reason string) {
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).
		WithArgs(waveID, sqlmock.AnyArg(), domain, isp, mode, requested, ceiling, spent, allowed, reason).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestParseFamilyGovernorMode(t *testing.T) {
	cases := []struct {
		in   string
		mode string
		ok   bool
	}{
		{"", FamilyGovernorOff, true},
		{"off", FamilyGovernorOff, true},
		{"OFF ", FamilyGovernorOff, true},
		{"shadow", FamilyGovernorShadow, true},
		{" Shadow", FamilyGovernorShadow, true},
		{"on", FamilyGovernorOn, true},
		{"ON", FamilyGovernorOn, true},
		{"garbage", FamilyGovernorOff, false},
		{"1", FamilyGovernorOff, false},
		{"enforce", FamilyGovernorOff, false},
	}
	for _, c := range cases {
		mode, ok := ParseFamilyGovernorMode(c.in)
		if mode != c.mode || ok != c.ok {
			t.Errorf("Parse(%q) = (%s,%v), want (%s,%v)", c.in, mode, ok, c.mode, c.ok)
		}
	}
}

func TestNewFamilyGovernor_ReadsEnvOnce(t *testing.T) {
	t.Setenv(FamilyGovernorModeEnv, "shadow")
	g := NewFamilyGovernor(nil)
	if g.Mode() != FamilyGovernorShadow || !g.Enabled() {
		t.Fatalf("want shadow/enabled, got %s/%v", g.Mode(), g.Enabled())
	}
	t.Setenv(FamilyGovernorModeEnv, "on")
	if g.Mode() != FamilyGovernorShadow {
		t.Fatal("mode must be read ONCE at construction, not per call")
	}
	t.Setenv(FamilyGovernorModeEnv, "bogus")
	if g2 := NewFamilyGovernor(nil); g2.Enabled() {
		t.Fatal("unknown mode must run OFF")
	}
	t.Setenv(FamilyGovernorModeEnv, "")
	if g3 := NewFamilyGovernor(nil); g3.Enabled() {
		t.Fatal("empty mode must run OFF")
	}
	var nilGov *FamilyGovernor
	if nilGov.Enabled() || nilGov.Mode() != FamilyGovernorOff {
		t.Fatal("nil governor must read as OFF")
	}
}

func TestIsFamilyGovernedISP(t *testing.T) {
	for _, in := range []string{"yahoo", "aol", "att", "sbcglobal", "cox", " Yahoo "} {
		if !IsFamilyGovernedISP(in) {
			t.Errorf("%q must be family", in)
		}
	}
	for _, in := range []string{"gmail", "microsoft", "apple", "comcast", "charter", "verizon", "other", ""} {
		if IsFamilyGovernedISP(in) {
			t.Errorf("%q must NOT be family (gmail is never governed here)", in)
		}
	}
}

func TestFamilyGovernorDecide_Math(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		ceiling   int
		spent     int
		allowed   int
		reason    string
	}{
		{"within", 500, 10000, 9000, 500, "within"},
		{"within_exact", 1000, 10000, 9000, 1000, "within"},
		{"trim", 1500, 10000, 9000, 1000, "trim"},
		{"deny_exact", 500, 10000, 10000, 0, "deny"},
		{"deny_over", 500, 10000, 12000, 0, "deny"},
		{"zero_ceiling", 500, 0, 0, 0, "deny"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock := fgNewMock(t)
			waveID := uuid.New().String()
			fgExpectContract(mock, fgTestLane, c.ceiling)
			fgExpectSpend(mock, fgTestDomain, c.spent)
			fgExpectLedger(mock, waveID, fgTestDomain, "yahoo", FamilyGovernorShadow, c.requested, c.ceiling, c.spent, c.allowed, c.reason)

			g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
			d, err := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, waveID, c.requested)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if !d.Governed || d.Allowed != c.allowed || d.Reason != c.reason || d.Ceiling != c.ceiling || d.Spent != c.spent {
				t.Fatalf("got %+v, want allowed=%d reason=%s", d, c.allowed, c.reason)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Negative control: OFF issues ZERO queries (sqlmock fails on any unexpected
// statement) and returns requested untouched.
func TestFamilyGovernorDecide_OffNoQueries(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOff)
	d, err := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 700)
	if err != nil || d.Governed || d.Allowed != 700 || d.Reason != "ungoverned" {
		t.Fatalf("off must be inert: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_NonFamilyNoQueries(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	for _, ispName := range []string{"gmail", "microsoft", "apple", "comcast", ""} {
		d, err := g.Decide(context.Background(), db, fgTestDomain, ispName, fgTestDay, uuid.New().String(), 700)
		if err != nil || d.Governed || d.Allowed != 700 || d.Reason != "ungoverned" {
			t.Fatalf("isp %q must be ungoverned: %+v err=%v", ispName, d, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_NoContract(t *testing.T) {
	db, mock := fgNewMock(t)
	fgExpectContract(mock, fgTestLane, nil) // zero rows
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.Decide(context.Background(), db, fgTestDomain, "aol", fgTestDay, uuid.New().String(), 700)
	if err != nil || d.Governed || d.Allowed != 700 || d.Reason != "no_contract" {
		t.Fatalf("no contract must be ungoverned: %+v err=%v", d, err)
	}
	// No spend query, no ledger row.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_NullCeilingIsNoContract(t *testing.T) {
	db, mock := fgNewMock(t)
	fgExpectContract(mock, fgTestLane, nil)
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(fgTestLane).
		WillReturnRows(sqlmock.NewRows([]string{"daily_ceiling"}).AddRow(nil))
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	g.now = func() time.Time { return fgTestDay }
	// First call: zero rows. Expire cache, second call: NULL daily_ceiling.
	if d, _ := g.Decide(context.Background(), db, fgTestDomain, "att", fgTestDay, uuid.New().String(), 5); d.Governed {
		t.Fatal("zero rows must be ungoverned")
	}
	g.now = func() time.Time { return fgTestDay.Add(2 * familyGovernorCacheTTL) }
	d, err := g.Decide(context.Background(), db, fgTestDomain, "att", fgTestDay, uuid.New().String(), 5)
	if err != nil || d.Governed || d.Allowed != 5 || d.Reason != "no_contract" {
		t.Fatalf("NULL ceiling must be ungoverned: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_ContractErrorFailsOpen(t *testing.T) {
	db, mock := fgNewMock(t)
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(fgTestLane).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 700)
	if err == nil {
		t.Fatal("error must be surfaced for logging")
	}
	if d.Governed || d.Allowed != 700 || d.Reason != "error:contract" {
		t.Fatalf("must fail OPEN with reason error:contract: %+v", d)
	}
	// An error is NOT cached: the next call re-reads the contract.
	fgExpectContract(mock, fgTestLane, nil)
	if d, _ := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 1); d.Reason != "no_contract" {
		t.Fatalf("error must not poison the cache: %+v", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_SpendErrorFailsOpen(t *testing.T) {
	db, mock := fgNewMock(t)
	waveID := uuid.New().String()
	fgExpectContract(mock, fgTestLane, 10000)
	mock.ExpectQuery(`FROM mailing_campaign_queue q`).WillReturnError(errors.New("boom"))
	// The ledger still records the fail-open decision (allowed == requested).
	fgExpectLedger(mock, waveID, fgTestDomain, "sbcglobal", FamilyGovernorOn, 700, 10000, 0, 700, "error:spend")
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.Decide(context.Background(), db, fgTestDomain, "sbcglobal", fgTestDay, waveID, 700)
	if err == nil {
		t.Fatal("error must be surfaced for logging")
	}
	if !d.Governed || d.Allowed != 700 || d.Reason != "error:spend" || d.Ceiling != 10000 {
		t.Fatalf("must fail OPEN with reason error:spend: %+v", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_LedgerFailureNeverBlocks(t *testing.T) {
	db, mock := fgNewMock(t)
	waveID := uuid.New().String()
	fgExpectContract(mock, fgTestLane, 100)
	fgExpectSpend(mock, fgTestDomain, 10)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnError(errors.New("relation does not exist"))
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	d, err := g.Decide(context.Background(), db, fgTestDomain, "cox", fgTestDay, waveID, 50)
	if err != nil || d.Allowed != 50 || d.Reason != "within" {
		t.Fatalf("ledger failure must not change the decision: %+v err=%v", d, err)
	}
	// Re-fire on the same wave: ON CONFLICT DO NOTHING → 0 rows affected is fine.
	fgExpectSpend(mock, fgTestDomain, 10)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).
		WithArgs(waveID, sqlmock.AnyArg(), fgTestDomain, "cox", FamilyGovernorShadow, 50, 100, 10, 50, "within").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if d, err := g.Decide(context.Background(), db, fgTestDomain, "cox", fgTestDay, waveID, 50); err != nil || d.Allowed != 50 {
		t.Fatalf("re-fire: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The ceiling is cached 60s per lane; spend is re-read on EVERY decision.
func TestFamilyGovernorDecide_CeilingCached60s(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	now := fgTestDay
	g.now = func() time.Time { return now }

	fgExpectContract(mock, fgTestLane, 1000) // once
	for i := 0; i < 3; i++ {
		fgExpectSpend(mock, fgTestDomain, 100*i)
		mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	for i := 0; i < 3; i++ {
		now = now.Add(20 * time.Second)
		d, err := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 50)
		if err != nil || d.Spent != 100*i || d.Ceiling != 1000 {
			t.Fatalf("call %d: %+v err=%v", i, d, err)
		}
	}
	// The contract was read at +20s; 61s after that → re-read (new ceiling visible).
	now = fgTestDay.Add(81 * time.Second)
	fgExpectContract(mock, fgTestLane, 2000)
	fgExpectSpend(mock, fgTestDomain, 1500)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	d, err := g.Decide(context.Background(), db, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 50)
	if err != nil || d.Ceiling != 2000 || d.Allowed != 50 {
		t.Fatalf("stale cache: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Lane key is the plan's sending_domain VERBATIM (lower-cased), one lane per domain.
func TestFamilyGovernorDecide_LaneIsPlanSendingDomain(t *testing.T) {
	db, mock := fgNewMock(t)
	fgExpectContract(mock, "broadcast-family.m.historythinking.com", 10)
	fgExpectSpend(mock, "m.historythinking.com", 0)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	if _, err := g.Decide(context.Background(), db, " M.HistoryThinking.com ", "aol", fgTestDay, uuid.New().String(), 5); err != nil {
		t.Fatal(err)
	}
	// Empty domain: no queries, ungoverned.
	d, err := g.Decide(context.Background(), db, "", "aol", fgTestDay, uuid.New().String(), 5)
	if err != nil || d.Governed || d.Allowed != 5 || d.Reason != "no_domain" {
		t.Fatalf("empty domain: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDayBounds(t *testing.T) {
	// 2026-09-06 05:30Z = 2026-09-05 23:30 MDT → day 09-05 [06:00Z, 06:00Z+1d)
	start := familyGovernorDayStart(time.Date(2026, 9, 6, 5, 30, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("dayStart = %s", start)
	}
	end := familyGovernorDayEnd(start)
	if !end.Equal(time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("dayEnd = %s", end)
	}
	// DST fall-back day (2026-11-01) is 25h long: end is the next local midnight.
	s2 := familyGovernorDayStart(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	if got := familyGovernorDayEnd(s2).Sub(s2); got != 25*time.Hour {
		t.Fatalf("DST day length = %s, want 25h", got)
	}
	if start.Format("2006-01-02") != "2026-09-05" {
		t.Fatalf("ledger day = %s", start.Format("2006-01-02"))
	}
}

func TestFamilyLane(t *testing.T) {
	cases := map[string]string{
		"m.discountblog.com":      "broadcast-family.m.discountblog.com",
		" M.DiscountBlog.COM ":    "broadcast-family.m.discountblog.com",
		"em.homeloansbyjaime.com": "broadcast-family.em.homeloansbyjaime.com",
		"":                        "",
		"   ":                     "",
	}
	for in, want := range cases {
		if got := familyLane(in); got != want {
			t.Errorf("familyLane(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(familyLane(in), ":") {
			t.Errorf("lane must never contain a colon: %q", familyLane(in))
		}
	}
}

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

// SendGovernor (family_governor.go) unit tests — Decide/DecideLane/Headroom
// arithmetic against sqlmock. The dispatcher hook is family_governor_dispatcher_test.go.

const (
	fgTestDomain   = "m.discountblog.com"
	fgTestLane     = "broadcast-family.m.discountblog.com"
	fgTestColdLane = "broadcast-cold.m.discountblog.com"
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

// fgExpectLane is the campaign → lane lookup Decide performs (cached 60s).
func fgExpectLane(mock sqlmock.Sqlmock, campaignID, lane string) {
	mock.ExpectQuery(`WHERE c.id = \$1::uuid`).WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"lane"}).AddRow(lane))
}

// fgExpectContract returns the lane × ISP contract row: (daily_ceiling,
// desired_daily_intros->>isp, ddi_empty). nil for either number = NULL; a
// missing row (found=false) returns zero rows.
func fgExpectContract(mock sqlmock.Sqlmock, lane, isp string, domainTotal, perISP any, ddiEmpty bool, found bool) {
	rows := sqlmock.NewRows([]string{"daily_ceiling", "per_isp", "ddi_empty"})
	if found {
		rows.AddRow(domainTotal, perISP, ddiEmpty)
	}
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(lane, isp, sqlmock.AnyArg()).WillReturnRows(rows)
}

// fgExpectSpend is THE one spend pass per decision: (per-ISP, domain total).
func fgExpectSpend2(mock sqlmock.Sqlmock, domain, lane, isp string, spentISP, spentDomain int) {
	mock.ExpectQuery(`FROM mailing_campaign_queue q`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), domain, lane, isp).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "total"}).AddRow(spentISP, spentDomain))
}

func fgExpectSpend(mock sqlmock.Sqlmock, domain, lane, isp string, spent int) {
	fgExpectSpend2(mock, domain, lane, isp, spent, spent)
}

// fgExpectPlanned is the deploy-time committed count (Headroom only).
func fgExpectPlanned(mock sqlmock.Sqlmock, domain, lane, isp string, planned int) {
	mock.ExpectQuery(`SUM\(p.audience_selected_count\)`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), domain, lane, isp, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "total"}).AddRow(planned, planned))
}

func fgExpectLedger(mock sqlmock.Sqlmock, waveID, domain, isp, mode string, requested, ceiling, spent, allowed int, reason, lane string) {
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).
		WithArgs(waveID, sqlmock.AnyArg(), domain, isp, mode, requested, ceiling, spent, allowed, reason, lane).
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

func TestParseGovernorLanes(t *testing.T) {
	if l := ParseGovernorLanes(""); !l["family"] || !l["cold"] || len(l) != 2 {
		t.Fatalf("default lanes = %v", l)
	}
	if l := ParseGovernorLanes(" Cold , engaged,kumo"); !l["cold"] || !l["kumo"] || l["engaged"] || len(l) != 2 {
		t.Fatalf("engaged must never be governed: %v", l)
	}
}

func TestNewFamilyGovernor_ReadsEnvOnce(t *testing.T) {
	t.Setenv(SendGovernorModeEnv, "")
	t.Setenv(FamilyGovernorModeEnv, "shadow")
	g := NewFamilyGovernor(nil)
	if g.Mode() != FamilyGovernorShadow || !g.Enabled() {
		t.Fatalf("FAMILY_GOVERNOR_MODE fallback: want shadow/enabled, got %s/%v", g.Mode(), g.Enabled())
	}
	t.Setenv(FamilyGovernorModeEnv, "on")
	if g.Mode() != FamilyGovernorShadow {
		t.Fatal("mode must be read ONCE at construction, not per call")
	}
	// SEND_GOVERNOR_MODE wins over the fallback.
	t.Setenv(SendGovernorModeEnv, "on")
	t.Setenv(FamilyGovernorModeEnv, "shadow")
	if g2 := NewFamilyGovernor(nil); g2.Mode() != FamilyGovernorOn {
		t.Fatalf("SEND_GOVERNOR_MODE must win: %s", g2.Mode())
	}
	t.Setenv(SendGovernorModeEnv, "bogus")
	if g3 := NewFamilyGovernor(nil); g3.Enabled() {
		t.Fatal("unknown mode must run OFF")
	}
	t.Setenv(SendGovernorModeEnv, "")
	t.Setenv(FamilyGovernorModeEnv, "")
	if g4 := NewFamilyGovernor(nil); g4.Enabled() {
		t.Fatal("empty mode must run OFF")
	}
	t.Setenv(SendGovernorLanesEnv, "cold")
	if g5 := NewFamilyGovernor(nil); !g5.Governs("cold") || g5.Governs("family") {
		t.Fatalf("lanes env: %v", g5.Lanes())
	}
	var nilGov *FamilyGovernor
	if nilGov.Enabled() || nilGov.Mode() != FamilyGovernorOff || nilGov.Governs("family") {
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
			t.Errorf("%q must NOT be family", in)
		}
	}
}

// LaneOf: the tag wins; untagged names classify by the same rule as LaneOfSQL.
func TestLaneOf(t *testing.T) {
	cases := []struct{ name, tag, want string }{
		{"09232026 - DB - NL-YF-NEWSLETTER-D14-COLD", "", LaneFamily}, // NL-YF before -COLD
		{"09232026 - DB - NL-YF-NEWSLETTER-D14-ENG", "", LaneFamily},
		{"09232026 - DB - NL-YF-NEWSLETTER", "", LaneFamily},
		{"09232026 - DB - NL-MS-NEWSLETTER-D10-COLD", "", LaneCold},
		{"09232026 - DB - NL-AP-NEWSLETTER-D10-COLD", "", LaneCold},
		{"09232026 - DB - NL-COLD-REMAIL-Liberty", "", LaneCold},
		{"09232026 - NX-DB - NL-NEWSLETTER-ms", "", LaneCold},
		{"09232026 - DB - ENG-NEWSLETTER", "", LaneEngaged},
		{"09232026 - DB - OFR-CLK-SamsCPL", "", LaneEngaged},
		{"09232026 - TRB - KUMO-WARM d21", "", LaneKumo},
		{"09232026 - DB - NL-FRESH-NEWSLETTER", "", LaneFresh},
		{"09232026 - DB - ENG-NEWSLETTER", "Cold ", LaneCold},
		{"anything", "family", LaneFamily},
	}
	for _, c := range cases {
		if got := LaneOf(c.name, c.tag); got != c.want {
			t.Errorf("LaneOf(%q,%q) = %q, want %q", c.name, c.tag, got, c.want)
		}
	}
	for _, needle := range []string{"NL-YF", "KUMO-WARM", "FRESH", "-COLD|^[0-9]{8} - NX-", "'engaged'", "campaign_input'->>'lane'"} {
		if !strings.Contains(LaneOfSQL, needle) {
			t.Errorf("LaneOfSQL must carry %q", needle)
		}
	}
	// The SQL CASE tests NL-YF before -COLD, as LaneOf does.
	if strings.Index(LaneOfSQL, "NL-YF") > strings.Index(LaneOfSQL, "-COLD") {
		t.Fatal("LaneOfSQL must test NL-YF before -COLD")
	}
}

func TestLaneKey(t *testing.T) {
	cases := map[[2]string]string{
		{"family", "m.discountblog.com"}:   "broadcast-family.m.discountblog.com",
		{"Family", " M.DiscountBlog.COM "}: "broadcast-family.m.discountblog.com",
		{"cold", "m.discountblog.com"}:     "broadcast-cold.m.discountblog.com",
		{"cold", ""}:                       "",
	}
	for in, want := range cases {
		if got := laneKey(in[0], in[1]); got != want || strings.Contains(got, ":") {
			t.Errorf("laneKey(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	if familyLane("m.x.com") != "broadcast-family.m.x.com" {
		t.Fatal("familyLane must keep the 09-07 derivation")
	}
}

// Per-ISP term only (daily_ceiling NULL): one spend query, the ISP intro binds.
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
			fgExpectContract(mock, fgTestLane, "yahoo", nil, c.ceiling, false, true)
			fgExpectSpend(mock, fgTestDomain, LaneFamily, "yahoo", c.spent)
			fgExpectLedger(mock, waveID, fgTestDomain, "yahoo", FamilyGovernorShadow, c.requested, c.ceiling, c.spent, c.allowed, c.reason, LaneFamily)

			g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
			d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "yahoo", fgTestDay, waveID, c.requested)
			if err != nil {
				t.Fatalf("DecideLane: %v", err)
			}
			if !d.Governed || d.Allowed != c.allowed || d.Reason != c.reason || d.Ceiling != c.ceiling || d.Spent != c.spent || d.Lane != LaneFamily {
				t.Fatalf("got %+v, want allowed=%d reason=%s", d, c.allowed, c.reason)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Both terms: the tighter of (per-ISP, domain total) binds and is what the
// ledger records.
func TestFamilyGovernorDecide_DomainTotalBinds(t *testing.T) {
	db, mock := fgNewMock(t)
	waveID := uuid.New().String()
	fgExpectContract(mock, fgTestColdLane, "microsoft", 1000, 800, false, true)
	fgExpectSpend2(mock, fgTestDomain, LaneCold, "microsoft", 100, 950) // isp balance 700; domain balance 50 ← binds
	fgExpectLedger(mock, waveID, fgTestDomain, "microsoft", FamilyGovernorOn, 300, 1000, 950, 50, "trim", LaneCold)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.DecideLane(context.Background(), db, LaneCold, fgTestDomain, "microsoft", fgTestDay, waveID, 300)
	if err != nil || d.Allowed != 50 || d.Reason != "trim" || d.Ceiling != 1000 || d.Spent != 950 {
		t.Fatalf("%+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An ISP ABSENT from a NON-empty desired_daily_intros is a contract GAP: the
// per-ISP term is skipped (never a silent zero), the domain total still
// binds, and the decision carries |no_isp_ceiling so the shadow report sees
// it. With no domain total either → no_isp_ceiling, ungoverned, ledgered.
func TestFamilyGovernorDecide_AbsentISPIsAGapNotZero(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	// gap + domain total 5000 spent 4990 → the total binds at 10, flagged
	wave := uuid.New().String()
	fgExpectContract(mock, fgTestLane, "comcast", 5000, nil, false, true)
	fgExpectSpend2(mock, fgTestDomain, LaneFamily, "comcast", 0, 4990)
	fgExpectLedger(mock, wave, fgTestDomain, "comcast", FamilyGovernorOn, 200, 5000, 4990, 10, "trim|no_isp_ceiling", LaneFamily)
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "comcast", fgTestDay, wave, 200)
	if err != nil || !d.Governed || d.Allowed != 10 || d.Reason != "trim|no_isp_ceiling" {
		t.Fatalf("gap with domain total: %+v err=%v", d, err)
	}
	// gap, no domain total → ungoverned, requested untouched, ledgered as no_isp_ceiling
	wave2 := uuid.New().String()
	fgExpectContract(mock, fgTestLane, "verizon", nil, nil, false, true)
	fgExpectSpend2(mock, fgTestDomain, LaneFamily, "verizon", 0, 0)
	fgExpectLedger(mock, wave2, fgTestDomain, "verizon", FamilyGovernorOn, 200, 0, 0, 200, "no_isp_ceiling", LaneFamily)
	d, err = g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "verizon", fgTestDay, wave2, 200)
	if err != nil || d.Governed || d.Allowed != 200 || d.Reason != "no_isp_ceiling" {
		t.Fatalf("gap without total must not deny: %+v err=%v", d, err)
	}
	// Empty map + domain total 5000, spent 4990 → domain-total term binds at 10, no flag.
	wave3 := uuid.New().String()
	fgExpectContract(mock, fgTestLane, "att", 5000, nil, true, true)
	fgExpectSpend2(mock, fgTestDomain, LaneFamily, "att", 0, 4990)
	fgExpectLedger(mock, wave3, fgTestDomain, "att", FamilyGovernorOn, 200, 5000, 4990, 10, "trim", LaneFamily)
	d, err = g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "att", fgTestDay, wave3, 200)
	if err != nil || d.Allowed != 10 || d.Reason != "trim" {
		t.Fatalf("empty map → domain total: %+v err=%v", d, err)
	}
	// Contract row with neither term → no_contract (nothing to enforce).
	fgExpectContract(mock, fgTestLane, "cox", nil, nil, true, true)
	fgExpectSpend2(mock, fgTestDomain, LaneFamily, "cox", 0, 0)
	d, err = g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "cox", fgTestDay, uuid.New().String(), 7)
	if err != nil || d.Governed || d.Allowed != 7 || d.Reason != "no_contract" {
		t.Fatalf("no terms → no_contract: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// countingQ counts every statement the governor issues — the negative controls
// assert on the COUNT, not on sqlmock silence (an unexpected query fails open
// and would let a "zero statements" test pass; QA 2026-09-23).
type countingQ struct {
	FamilyGovernorQueryer
	n int
}

func (c *countingQ) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.n++
	return c.FamilyGovernorQueryer.QueryRowContext(ctx, query, args...)
}
func (c *countingQ) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.n++
	return c.FamilyGovernorQueryer.ExecContext(ctx, query, args...)
}

func TestFamilyGovernor_StatementCounts(t *testing.T) {
	db, mock := fgNewMock(t)
	// OFF: zero statements on Decide, DecideLane and Headroom.
	off := newFamilyGovernorWithMode(db, FamilyGovernorOff)
	cq := &countingQ{FamilyGovernorQueryer: db}
	off.Decide(context.Background(), cq, uuid.New().String(), fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 5)
	off.DecideLane(context.Background(), cq, LaneFamily, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 5)
	off.Headroom(context.Background(), cq, LaneCold, fgTestDomain, "microsoft", "x", fgTestDay)
	if cq.n != 0 {
		t.Fatalf("OFF issued %d statements", cq.n)
	}
	// ON, engaged campaign: exactly ONE statement (the lane lookup).
	on := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	eng := uuid.New().String()
	fgExpectLane(mock, eng, LaneEngaged)
	cq = &countingQ{FamilyGovernorQueryer: db}
	if d, _ := on.Decide(context.Background(), cq, eng, fgTestDomain, "microsoft", fgTestDay, uuid.New().String(), 5); d.Governed || cq.n != 1 {
		t.Fatalf("engaged: governed=%v statements=%d", d.Governed, cq.n)
	}
	// ON, no contract: lane + contract = 2, nothing else.
	cold := uuid.New().String()
	fgExpectLane(mock, cold, LaneCold)
	fgExpectContract(mock, fgTestColdLane, "microsoft", nil, nil, false, false)
	cq = &countingQ{FamilyGovernorQueryer: db}
	if d, _ := on.Decide(context.Background(), cq, cold, fgTestDomain, "microsoft", fgTestDay, uuid.New().String(), 5); d.Governed || cq.n != 2 {
		t.Fatalf("no contract: governed=%v statements=%d", d.Governed, cq.n)
	}
	// ON, governed per-ISP only: lane + contract + spend + ledger = 4.
	cold2 := uuid.New().String()
	fgExpectLane(mock, cold2, LaneCold)
	fgExpectContract(mock, fgTestColdLane, "apple", nil, 50, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneCold, "apple", 0)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	cq = &countingQ{FamilyGovernorQueryer: db}
	if d, _ := on.Decide(context.Background(), cq, cold2, fgTestDomain, "apple", fgTestDay, uuid.New().String(), 5); !d.Governed || cq.n != 4 {
		t.Fatalf("governed: governed=%v statements=%d", d.Governed, cq.n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Decide resolves the lane from the campaign; an engaged campaign issues the
// lane lookup and nothing else; a cold campaign is governed on the cold key.
func TestFamilyGovernorDecide_LaneResolution(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	eng := uuid.New().String()
	fgExpectLane(mock, eng, LaneEngaged)
	d, err := g.Decide(context.Background(), db, eng, fgTestDomain, "microsoft", fgTestDay, uuid.New().String(), 900)
	if err != nil || d.Governed || d.Allowed != 900 || d.Reason != "ungoverned:engaged" {
		t.Fatalf("engaged must be untouched: %+v err=%v", d, err)
	}
	cold := uuid.New().String()
	wave := uuid.New().String()
	fgExpectLane(mock, cold, LaneCold)
	fgExpectContract(mock, fgTestColdLane, "microsoft", nil, 2100, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneCold, "microsoft", 2000)
	fgExpectLedger(mock, wave, fgTestDomain, "microsoft", FamilyGovernorOn, 900, 2100, 2000, 100, "trim", LaneCold)
	d, err = g.Decide(context.Background(), db, cold, fgTestDomain, "microsoft", fgTestDay, wave, 900)
	if err != nil || d.Allowed != 100 || d.Lane != LaneCold {
		t.Fatalf("cold: %+v err=%v", d, err)
	}
	// The lane is cached: a second decision on the same campaign issues no lane query.
	fgExpectContract(mock, fgTestColdLane, "apple", nil, 50, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneCold, "apple", 0)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	if d, err := g.Decide(context.Background(), db, cold, fgTestDomain, "apple", fgTestDay, uuid.New().String(), 10); err != nil || d.Allowed != 10 {
		t.Fatalf("cached lane: %+v err=%v", d, err)
	}
	// Lane lookup error: fail open, reason error:lane, no further queries.
	bad := uuid.New().String()
	mock.ExpectQuery(`WHERE c.id = \$1::uuid`).WithArgs(bad).WillReturnError(errors.New("boom"))
	d, err = g.Decide(context.Background(), db, bad, fgTestDomain, "microsoft", fgTestDay, uuid.New().String(), 5)
	if err == nil || d.Governed || d.Allowed != 5 || d.Reason != "error:lane" {
		t.Fatalf("lane error must fail open: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Headroom is the deploy-time question: allowed for an unbounded request, no
// ledger row. It COMPUTES in shadow too (the api hook decides whether to
// apply it — TestPlanPMTAAudience_GovernorShadowNeverClamps); the contract
// read is as-of the cell's send day and the planned term excludes the cell's
// own name twin (args pinned by fgExpectPlanned).
func TestFamilyGovernorHeadroom(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	fgExpectContract(mock, fgTestColdLane, "microsoft", nil, 2100, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneCold, "microsoft", 600)
	fgExpectPlanned(mock, fgTestDomain, LaneCold, "microsoft", 900) // an earlier deploy today, not yet enqueued
	n, governed, err := g.Headroom(context.Background(), db, LaneCold, fgTestDomain, "microsoft", "09252026 - DB - NL-MS-NEWSLETTER-D12-COLD", fgTestDay)
	if err != nil || !governed || n != 1200 {
		t.Fatalf("headroom = %d governed=%v err=%v (want 2100 - max(600 queued, 900 planned))", n, governed, err)
	}
	// Not a governed lane → governed=false, no queries.
	if _, governed, _ := g.Headroom(context.Background(), db, LaneEngaged, fgTestDomain, "microsoft", "x", fgTestDay); governed {
		t.Fatal("engaged must not be governed")
	}
	// Off → governed=false, no queries.
	off := newFamilyGovernorWithMode(db, FamilyGovernorOff)
	if _, governed, _ := off.Headroom(context.Background(), db, LaneCold, fgTestDomain, "microsoft", "x", fgTestDay); governed {
		t.Fatal("off must not govern")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Negative control: OFF issues ZERO queries and returns requested untouched.
func TestFamilyGovernorDecide_OffNoQueries(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOff)
	d, err := g.Decide(context.Background(), db, uuid.New().String(), fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 700)
	if err != nil || d.Governed || d.Allowed != 700 || d.Reason != "ungoverned" {
		t.Fatalf("off must be inert: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_NoContract(t *testing.T) {
	db, mock := fgNewMock(t)
	fgExpectContract(mock, fgTestLane, "aol", nil, nil, false, false) // zero rows
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "aol", fgTestDay, uuid.New().String(), 700)
	if err != nil || d.Governed || d.Allowed != 700 || d.Reason != "no_contract" {
		t.Fatalf("no contract must be ungoverned: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_ContractErrorFailsOpen(t *testing.T) {
	db, mock := fgNewMock(t)
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(fgTestLane, "yahoo", sqlmock.AnyArg()).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	// error decisions are ledgered (fail-open visible in the ledger)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 700)
	if err == nil {
		t.Fatal("error must be surfaced for logging")
	}
	if d.Governed || d.Allowed != 700 || d.Reason != "error:contract" {
		t.Fatalf("must fail OPEN with reason error:contract: %+v", d)
	}
	// An error is NOT cached: the next call re-reads the contract.
	fgExpectContract(mock, fgTestLane, "yahoo", nil, nil, false, false)
	if d, _ := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 1); d.Reason != "no_contract" {
		t.Fatalf("error must not poison the cache: %+v", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_SpendErrorFailsOpen(t *testing.T) {
	db, mock := fgNewMock(t)
	waveID := uuid.New().String()
	fgExpectContract(mock, fgTestLane, "sbcglobal", nil, 10000, false, true)
	mock.ExpectQuery(`FROM mailing_campaign_queue q`).WillReturnError(errors.New("boom"))
	fgExpectLedger(mock, waveID, fgTestDomain, "sbcglobal", FamilyGovernorOn, 700, 0, 0, 700, "error:spend", LaneFamily)
	g := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "sbcglobal", fgTestDay, waveID, 700)
	if err == nil {
		t.Fatal("error must be surfaced for logging")
	}
	if !d.Governed || d.Allowed != 700 || d.Reason != "error:spend" {
		t.Fatalf("must fail OPEN with reason error:spend: %+v", d)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDecide_LedgerFailureNeverBlocks(t *testing.T) {
	db, mock := fgNewMock(t)
	waveID := uuid.New().String()
	fgExpectContract(mock, fgTestLane, "cox", nil, 100, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneFamily, "cox", 10)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnError(errors.New("relation does not exist"))
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "cox", fgTestDay, waveID, 50)
	if err != nil || d.Allowed != 50 || d.Reason != "within" {
		t.Fatalf("ledger failure must not change the decision: %+v err=%v", d, err)
	}
	fgExpectSpend(mock, fgTestDomain, LaneFamily, "cox", 10)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).
		WithArgs(waveID, sqlmock.AnyArg(), fgTestDomain, "cox", FamilyGovernorShadow, 50, 100, 10, 50, "within", LaneFamily).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "cox", fgTestDay, waveID, 50); err != nil || d.Allowed != 50 {
		t.Fatalf("re-fire: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The ceiling is cached 60s per lane × ISP; spend is re-read on EVERY decision.
func TestFamilyGovernorDecide_CeilingCached60s(t *testing.T) {
	db, mock := fgNewMock(t)
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	now := fgTestDay
	g.now = func() time.Time { return now }

	fgExpectContract(mock, fgTestLane, "yahoo", nil, 1000, false, true) // once
	for i := 0; i < 3; i++ {
		fgExpectSpend(mock, fgTestDomain, LaneFamily, "yahoo", 100*i)
		mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	for i := 0; i < 3; i++ {
		now = now.Add(20 * time.Second)
		d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 50)
		if err != nil || d.Spent != 100*i || d.Ceiling != 1000 {
			t.Fatalf("call %d: %+v err=%v", i, d, err)
		}
	}
	now = fgTestDay.Add(81 * time.Second)
	fgExpectContract(mock, fgTestLane, "yahoo", nil, 2000, false, true)
	fgExpectSpend(mock, fgTestDomain, LaneFamily, "yahoo", 1500)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	d, err := g.DecideLane(context.Background(), db, LaneFamily, fgTestDomain, "yahoo", fgTestDay, uuid.New().String(), 50)
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
	fgExpectContract(mock, "broadcast-family.m.historythinking.com", "aol", nil, 10, false, true)
	fgExpectSpend(mock, "m.historythinking.com", LaneFamily, "aol", 0)
	mock.ExpectExec(`INSERT INTO family_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	g := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	if _, err := g.DecideLane(context.Background(), db, LaneFamily, " M.HistoryThinking.com ", "aol", fgTestDay, uuid.New().String(), 5); err != nil {
		t.Fatal(err)
	}
	d, err := g.DecideLane(context.Background(), db, LaneFamily, "", "aol", fgTestDay, uuid.New().String(), 5)
	if err != nil || d.Governed || d.Allowed != 5 || d.Reason != "no_domain" {
		t.Fatalf("empty domain: %+v err=%v", d, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyGovernorDayBounds(t *testing.T) {
	start := familyGovernorDayStart(time.Date(2026, 9, 6, 5, 30, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("dayStart = %s", start)
	}
	end := familyGovernorDayEnd(start)
	if !end.Equal(time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("dayEnd = %s", end)
	}
	s2 := familyGovernorDayStart(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	if got := familyGovernorDayEnd(s2).Sub(s2); got != 25*time.Hour {
		t.Fatalf("DST day length = %s, want 25h", got)
	}
	if start.Format("2006-01-02") != "2026-09-05" {
		t.Fatalf("ledger day = %s", start.Format("2006-01-02"))
	}
}

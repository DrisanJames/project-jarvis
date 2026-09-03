package dripsupply

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// bucketBalance is a balance parked at the window start with an empty bucket —
// the state EnsureDayBalances leaves behind, minus the opening credit, so each
// test states its own starting tokens.
func bucketBalance(t *testing.T, effective int, tokens float64) *Balance {
	t.Helper()
	day := testDay(t)
	start, _ := DefaultWindow().Bounds(day)
	return &Balance{
		Day:            day,
		SendingDomain:  "em.historythinking.com",
		ISP:            "aol",
		Contracted:     effective,
		Effective:      effective,
		Tokens:         tokens,
		LastRefillTick: start,
	}
}

func TestWindow_ActiveIntervalsMatchesTheDesignDoc(t *testing.T) {
	// §2.3: "01:00–20:00 at 15 min = 76".
	if got := DefaultWindow().ActiveIntervals(); got != 76 {
		t.Fatalf("ActiveIntervals = %d, want 76", got)
	}
	if got := DefaultWindow().Hours(); got != 19 {
		t.Fatalf("Hours = %v, want 19", got)
	}
	// A degenerate window must clamp, not divide by zero and mint +Inf tokens.
	for _, w := range []Window{
		{},
		{Start: 5 * time.Hour, End: time.Hour, Interval: 15 * time.Minute},
		{Start: 0, End: time.Minute, Interval: time.Hour},
	} {
		if got := w.ActiveIntervals(); got < 1 {
			t.Fatalf("ActiveIntervals(%+v) = %d, want >= 1", w, got)
		}
	}
}

func TestWindowOf_ReadsTheContractClockStrings(t *testing.T) {
	c := domainContract("em.historythinking.com", 1, map[string]int{"aol": 100})
	w, err := WindowOf(c)
	if err != nil {
		t.Fatalf("WindowOf: %v", err)
	}
	if w.Start != time.Hour || w.End != 20*time.Hour || w.Interval != 15*time.Minute || w.MaxBurstIntervals != 2 {
		t.Fatalf("WindowOf = %+v, want 01:00-20:00 @15m burst 2", w)
	}
	// A contract that cannot pace must be an error, never a silent default.
	bad := domainContract("em.historythinking.com", 1, map[string]int{"aol": 100})
	bad.ActiveWindowEnd = "01:00"
	if _, err := WindowOf(bad); err == nil {
		t.Fatal("WindowOf accepted a window whose end is not after its start")
	}
	bad2 := domainContract("em.historythinking.com", 1, map[string]int{"aol": 100})
	bad2.IntervalMinutes = 0
	if _, err := WindowOf(bad2); err == nil {
		t.Fatal("WindowOf accepted interval_minutes = 0")
	}
}

// -----------------------------------------------------------------------------
// §8.2 test 3 — scheduler downtime cannot produce a burst above max_burst_intervals
// -----------------------------------------------------------------------------

func TestRefill_DowntimeCannotBurstAboveTheCeiling(t *testing.T) {
	w := DefaultWindow() // 76 intervals, burst 2
	const effective = 7600
	refill := float64(effective) / 76 // 100 per interval
	ceiling := refill * 2             // 200

	b := bucketBalance(t, effective, 0)
	// Nine hours of downtime: the scheduler died at 01:00 and came back at 10:00.
	now := dayOf(b.Day).Add(10 * time.Hour)
	res := Refill(b, w, now)

	if res.IntervalsElapsed != 36 {
		t.Fatalf("intervals elapsed = %d, want 36 (9 h at 15 min)", res.IntervalsElapsed)
	}
	if b.Tokens != ceiling {
		t.Fatalf("tokens = %v after 9 h of downtime, want the burst ceiling %v", b.Tokens, ceiling)
	}
	if !res.Capped {
		t.Fatal("RefillResult.Capped is false — the operator cannot tell the ceiling bound this refill")
	}
	// The negative control: without the cap this refill mints 3,600 messages of
	// capacity in one tick. If the assertion above ever reads 3,600 the ceiling
	// is gone and the domain blasts 36 intervals of mail at once.
	if uncapped := refill * float64(res.IntervalsElapsed); b.Tokens >= uncapped {
		t.Fatalf("tokens %v reached the UNCAPPED value %v — max_burst_intervals is not being applied", b.Tokens, uncapped)
	}
}

func TestRefill_NegativeControl_ShortGapAccumulatesNormally(t *testing.T) {
	// Proves the ceiling test above is not passing because Refill simply always
	// returns 200: one interval must yield exactly one interval of tokens.
	w := DefaultWindow()
	b := bucketBalance(t, 7600, 0)
	res := Refill(b, w, dayOf(b.Day).Add(time.Hour+15*time.Minute))
	if res.IntervalsElapsed != 1 {
		t.Fatalf("intervals elapsed = %d, want 1", res.IntervalsElapsed)
	}
	if b.Tokens != 100 {
		t.Fatalf("tokens = %v after one interval, want 100", b.Tokens)
	}
	if res.Capped {
		t.Fatal("Capped is true after a single interval — the ceiling is being applied to normal pacing")
	}
}

// TestRefill_SubIntervalTicksStillAccumulate is the regression guard for the
// bug that makes a token bucket read as "paced" while granting zero forever:
// advancing last_refill_tick to `now` on every call discards the remainder, and
// a scheduler ticking every 15 s against a 15 min interval never earns a token.
func TestRefill_SubIntervalTicksStillAccumulate(t *testing.T) {
	w := DefaultWindow()
	b := bucketBalance(t, 7600, 0)
	start := dayOf(b.Day).Add(time.Hour)
	// 60 ticks at 15 s = exactly 15 minutes = one interval.
	for i := 1; i <= 60; i++ {
		Refill(b, w, start.Add(time.Duration(i)*15*time.Second))
	}
	if b.Tokens != 100 {
		t.Fatalf("tokens = %v after 60 sub-interval ticks spanning one full interval, want 100 — last_refill_tick is being advanced past the un-earned remainder", b.Tokens)
	}
}

func TestRefill_ClosedHoursMintNothing(t *testing.T) {
	w := DefaultWindow()

	// Before the window opens.
	b := bucketBalance(t, 7600, 0)
	b.LastRefillTick = dayOf(b.Day)
	res := Refill(b, w, dayOf(b.Day).Add(30*time.Minute))
	if b.Tokens != 0 || res.IntervalsElapsed != 0 {
		t.Fatalf("tokens = %v (elapsed %d) before the window opened, want 0", b.Tokens, res.IntervalsElapsed)
	}
	if res.InWindow {
		t.Fatal("InWindow is true at 00:30 for a 01:00-20:00 window")
	}

	// After it closes: accrual stops at 20:00, it does not run to midnight.
	b2 := bucketBalance(t, 7600, 0)
	b2.LastRefillTick = dayOf(b2.Day).Add(19*time.Hour + 45*time.Minute)
	res2 := Refill(b2, w, dayOf(b2.Day).Add(23*time.Hour))
	if res2.IntervalsElapsed != 1 {
		t.Fatalf("intervals elapsed = %d past the window close, want 1 (19:45 -> 20:00 only)", res2.IntervalsElapsed)
	}
	if res2.InWindow {
		t.Fatal("InWindow is true at 23:00 for a 01:00-20:00 window")
	}
}

// §8.2 test 7, the bucket half: tokens do not survive the day boundary.
func TestRefill_DayBoundaryResetsTokens(t *testing.T) {
	w := DefaultWindow()
	b := bucketBalance(t, 7600, 175)
	res := Refill(b, w, dayOf(b.Day).AddDate(0, 0, 1).Add(2*time.Hour))
	if !res.DayRolled {
		t.Fatal("DayRolled is false for a refill on the following day")
	}
	if b.Tokens != 0 {
		t.Fatalf("tokens = %v after the day rolled, want 0 — yesterday's bucket must not fund today", b.Tokens)
	}
	// Negative control: a refill on the SAME day, at the same clock time,
	// accumulates instead of resetting.
	b2 := bucketBalance(t, 7600, 175)
	Refill(b2, w, dayOf(b2.Day).Add(2*time.Hour))
	if b2.Tokens == 0 {
		t.Fatal("a same-day refill also zeroed the bucket — the reset is not keyed on the day boundary")
	}
	// And a clock that has gone backwards must not reset a live day either.
	b3 := bucketBalance(t, 7600, 175)
	Refill(b3, w, dayOf(b3.Day).AddDate(0, 0, -1).Add(2*time.Hour))
	if b3.Tokens != 175 {
		t.Fatalf("tokens = %v after a backwards clock, want the bucket untouched (175)", b3.Tokens)
	}
}

// TestRefill_ReturnedTokensAreClampedByTheNextRefill covers the overshoot
// Commit/Release/ExpireStale can create: they hand tokens back capped at
// `effective`, and the burst ceiling is re-applied here.
func TestRefill_ReturnedTokensAreClampedByTheNextRefill(t *testing.T) {
	w := DefaultWindow()
	b := bucketBalance(t, 7600, 5000) // as if a large release just landed
	res := Refill(b, w, dayOf(b.Day).Add(time.Hour))
	if res.IntervalsElapsed != 0 {
		t.Fatalf("intervals elapsed = %d, want 0", res.IntervalsElapsed)
	}
	if b.Tokens != 200 {
		t.Fatalf("tokens = %v, want the burst ceiling 200 — a release must not leave a bucket bigger than max_burst_intervals", b.Tokens)
	}
}

func TestRefill_ZeroEffectiveMintsNothing(t *testing.T) {
	b := bucketBalance(t, 0, 0)
	Refill(b, DefaultWindow(), dayOf(b.Day).Add(10*time.Hour))
	if b.Tokens != 0 {
		t.Fatalf("tokens = %v with effective 0, want 0", b.Tokens)
	}
}

// -----------------------------------------------------------------------------
// Governors
// -----------------------------------------------------------------------------

func TestApplyGovernors_ReduceNeverRaise(t *testing.T) {
	cases := []struct {
		name      string
		contract  int
		ceilings  []GovernorCeiling
		want      int
		wantBound string
	}{
		{"no governors", 5000, nil, 5000, ""},
		{"all unbound", 5000, []GovernorCeiling{{"ses_quota", NoLimit}, {"health_band", NoLimit}}, 5000, ""},
		{"above the contract is ignored", 5000, []GovernorCeiling{{"throttle", 9500}}, 5000, ""},
		{"below the contract binds", 5000, []GovernorCeiling{{"throttle", 1200}}, 1200, "throttle"},
		{"zero stops", 5000, []GovernorCeiling{{"gmail_hold", 0}}, 0, "gmail_hold"},
		{"lowest wins", 5000, []GovernorCeiling{{"throttle", 3000}, {"health_band", 900}}, 900, "health_band"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bound := ApplyGovernors(tc.contract, tc.ceilings)
			if got != tc.want || bound != tc.wantBound {
				t.Fatalf("ApplyGovernors = (%d, %q), want (%d, %q)", got, bound, tc.want, tc.wantBound)
			}
			if got > tc.contract {
				t.Fatalf("a governor RAISED capacity: %d > contract %d", got, tc.contract)
			}
		})
	}
}

func TestThrottleGovernor_ReadsTheRealTable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	w := DefaultWindow()
	g := ThrottleGovernor{DB: db}

	// No row: no opinion.
	cs, err := g.Ceilings(ctx, day, "em.historythinking.com", "aol", w)
	if err != nil {
		t.Fatalf("ceilings (no row): %v", err)
	}
	if len(cs) != 0 {
		t.Fatalf("ceilings with no throttle row = %+v, want none", cs)
	}

	if _, err := db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 0), ('yahoo', 120)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cs, err = g.Ceilings(ctx, day, "em.historythinking.com", "aol", w)
	if err != nil {
		t.Fatalf("ceilings (rate 0): %v", err)
	}
	if len(cs) != 1 || cs[0].Limit != 0 || cs[0].Name != "throttle" {
		t.Fatalf("rate 0 gave %+v, want a single throttle ceiling of 0", cs)
	}

	cs, err = g.Ceilings(ctx, day, "em.historythinking.com", "yahoo", w)
	if err != nil {
		t.Fatalf("ceilings (rate 120): %v", err)
	}
	want := int(math.Floor(120 * w.Hours())) // 2,280 over the 19 h window
	if len(cs) != 1 || cs[0].Limit != want {
		t.Fatalf("rate 120 gave %+v, want a ceiling of %d", cs, want)
	}
	// Negative control: if the reader ignored the rate and always blocked, the
	// yahoo case above would be 0 too.
	if cs[0].Limit == 0 {
		t.Fatal("a positive throttle rate produced a ceiling of 0")
	}
}

func TestStubGovernors_NeverInventACeiling(t *testing.T) {
	ctx := context.Background()
	day := testDay(t)
	for _, g := range []GovernorReader{NewSESQuotaGovernor(), NewHealthBandGovernor(), NewGmailHoldGovernor()} {
		cs, err := g.Ceilings(ctx, day, "em.historythinking.com", "gmail", DefaultWindow())
		if err != nil {
			t.Fatalf("%T: %v", g, err)
		}
		for _, c := range cs {
			if c.Limit != NoLimit {
				t.Fatalf("%T returned a real ceiling (%+v) — an unwired governor must not decide capacity", g, c)
			}
		}
		if eff, bound := ApplyGovernors(5000, cs); eff != 5000 || bound != "" {
			t.Fatalf("%T changed effective to %d (bound=%q) — an unwired governor must be inert", g, eff, bound)
		}
	}
}

// -----------------------------------------------------------------------------
// EnsureDayBalances / RefillDomain, against the real database
// -----------------------------------------------------------------------------

func TestEnsureDayBalances_SeedsOneIntervalOfOpeningCredit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "gmail": 0})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 5500, "gmail": 100, "yahoo": 0}, "gmail")
	res, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc}))
	if err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	if res.DomainRowsCreated != 2 {
		t.Fatalf("created %d domain rows, want 2", res.DomainRowsCreated)
	}
	// gmail is excluded and yahoo is desired 0: neither gets a lane row, so a
	// reservation against them fails closed instead of granting from thin air.
	if res.LaneRowsCreated != 1 {
		t.Fatalf("created %d lane rows, want 1 (gmail excluded, yahoo desired 0)", res.LaneRowsCreated)
	}

	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Contracted != 7600 || bal.Effective != 7600 {
		t.Fatalf("seeded balance = %+v, want contracted=effective=7600", bal)
	}
	if want := 7600.0 / 76.0; bal.Tokens != want {
		t.Fatalf("opening tokens = %v, want exactly one interval (%v)", bal.Tokens, want)
	}
	// A gmail row at 0 still exists (the contract names it) but funds nothing.
	if g := readBalance(t, db, day, "em.historythinking.com", "gmail"); g.Contracted != 0 || g.Tokens != 0 {
		t.Fatalf("gmail balance = %+v, want a zero row", g)
	}

	// Idempotent: a second pass creates nothing.
	res2, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc}))
	if err != nil {
		t.Fatalf("second EnsureDayBalances: %v", err)
	}
	if res2.DomainRowsCreated != 0 || res2.LaneRowsCreated != 0 {
		t.Fatalf("second pass created %+v, want nothing", res2)
	}
}

func TestRefillDomain_PersistsUnderTheRowLock(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 5000)

	// 9 h of downtime, then one refill: the persisted bucket must be capped.
	clock := dayOf(day).Add(10 * time.Hour)
	svc := NewService(db, WithClock(func() time.Time { return clock }))
	out, err := svc.RefillDomain(ctx, day, dc)
	if err != nil {
		t.Fatalf("RefillDomain: %v", err)
	}
	r, ok := out["aol"]
	if !ok {
		t.Fatalf("no refill result for aol: %+v", out)
	}
	if !r.Capped {
		t.Fatal("the persisted refill did not report the burst ceiling")
	}
	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Tokens != 200 {
		t.Fatalf("persisted tokens = %v, want the 200 burst ceiling", bal.Tokens)
	}
	if bal.Effective != 7600 {
		t.Fatalf("effective = %d with no governors, want the contract 7600", bal.Effective)
	}
}

// TestRefillDomain_NoBalanceRowInventsNothing pins the fail-closed rule: a
// refill never creates capacity for a domain×ISP the seeder did not open.
func TestRefillDomain_NoBalanceRowInventsNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc := domainContract("em.nowhere.com", 1, map[string]int{"aol": 5000})
	svc := NewService(db, WithClock(midWindow(day)))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("RefillDomain: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_capacity_balance`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("RefillDomain created %d balance row(s) for an unseeded domain", n)
	}
}

// TestRefillDomain_ToleratesNullLastRefillTick pins the cross-WP contract:
// WP1's drip_capacity_balance declares `last_refill_tick TIMESTAMPTZ` with no
// NOT NULL and no default, so a row seeded by anything other than
// EnsureDayBalances can carry NULL. Scanning NULL into a time.Time errors, and
// that error would wedge the domain's refill for the whole day.
func TestRefillDomain_ToleratesNullLastRefillTick(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 5000)

	if _, err := db.Exec(`UPDATE drip_capacity_balance SET last_refill_tick = NULL, tokens = 0`); err != nil {
		t.Fatalf("null the tick: %v", err)
	}
	svc := NewService(db, WithClock(func() time.Time { return dayOf(day).Add(3 * time.Hour) }))
	out, err := svc.RefillDomain(ctx, day, dc)
	if err != nil {
		t.Fatalf("RefillDomain over a NULL last_refill_tick: %v", err)
	}
	// A NULL tick defaults to the window start (01:00), so 03:00 is 8 intervals
	// later and the burst ceiling binds — never 0, never "since the zero time".
	if out["aol"].IntervalsElapsed != 8 {
		t.Fatalf("intervals elapsed = %d, want 8 (a NULL tick must default to the window start)", out["aol"].IntervalsElapsed)
	}
	if bal := readBalance(t, db, day, "em.historythinking.com", "aol"); bal.Tokens != 200 {
		t.Fatalf("tokens = %v, want the 200 burst ceiling", bal.Tokens)
	}
}

// TestPackageDDLMatchesWP1 is the drift guard for the DDL constants: they are
// the shape the integration tests build, and if WP1's production statements move
// the tests would silently keep passing against a schema that no longer exists.
func TestPackageDDLMatchesWP1(t *testing.T) {
	raw, err := os.ReadFile("../../../cmd/server/main.go")
	if err != nil {
		t.Skipf("cannot read cmd/server/main.go: %v", err)
	}
	main := string(raw)
	stmts := append([]string{CapacityLedgerDDL, CapacityBalanceDDL, LaneBalanceDDL}, CapacityLedgerIndexDDL...)
	for _, s := range stmts {
		if !strings.Contains(main, s) {
			head := s
			if i := strings.Index(head, "\n"); i > 0 {
				head = head[:i]
			}
			t.Fatalf("DDL drift: %q is no longer verbatim in cmd/server/main.go — WP1 changed the production schema and this package's tests are building a shape that does not exist", head)
		}
	}
}

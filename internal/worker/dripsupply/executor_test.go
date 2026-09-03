package dripsupply

// executor_test.go — REQ-118 WP5.
//
// The PG-backed tests reuse reservation_test.go's harness (newTestDB, seedDay,
// readBalance, readLedger): a scratch schema per test on the LOCAL
// apex-postgres, skipped — never failed, never faked — when it is unreachable.
// They need real Postgres because what is under test is row arithmetic
// (NUMERIC tokens, ON CONFLICT, the shared domain×ISP balance) that sqlmock
// cannot evaluate.

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Harness additions
// -----------------------------------------------------------------------------

// executorSchemaDDL is the WP1 shape of the two tables WP5 writes, VERBATIM from
// cmd/server/main.go (req118_create_drip_tick_outcomes and
// req118_create_drip_capacity_ledger_shadow). Keep them byte-identical: a WP1
// drift must surface here, not at 3am.
func executorSchemaDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS drip_tick_outcomes (
		tick        TIMESTAMPTZ NOT NULL,
		lane        TEXT NOT NULL,
		pass        TEXT NOT NULL,
		outcome     TEXT NOT NULL CHECK (outcome IN ('fired','skipped','zero','failed')),
		reason      TEXT NOT NULL DEFAULT '',
		caps_seen   JSONB NOT NULL DEFAULT '{}'::jsonb,
		claimed     INT NOT NULL DEFAULT 0,
		campaign_id UUID,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (tick, lane, pass)
	)`,
		`CREATE TABLE IF NOT EXISTS drip_capacity_ledger_shadow (LIKE drip_capacity_ledger INCLUDING ALL)`,
	}
}

func newExecutorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newTestDB(t)
	for _, stmt := range executorSchemaDDL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("executor ddl: %v", err)
		}
	}
	return db
}

// execFixture wires a Mediator over a seeded day. The contract set is injected
// through ContractSource so the test exercises the reservation + ledger path
// without also re-testing WP2's four contract tables and their HMAC tokens
// (contracts_test.go owns those).
type execFixture struct {
	db   *sql.DB
	med  *Mediator
	svc  *Service
	day  time.Time
	now  time.Time
	dc   *DomainContract
	lc   *DispatchContract
	set  *ActiveSet
	lane string
}

func newExecFixture(t *testing.T, mode Mode, domainMax, laneDesired int, canary ...CanaryCell) *execFixture {
	t.Helper()
	db := newExecutorTestDB(t)
	day := testDay(t)
	// 10:00 Denver: inside the 01:00-20:00 window, 36 whole intervals past the
	// window start, so Refill is deterministic and burst-capped.
	now := day.Add(10 * time.Hour)

	dc, lc := seedDay(t, db, day, domainMax, laneDesired)
	set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})

	svc := NewService(db, WithClock(func() time.Time { return now }))
	med := NewMediator(db, svc, MediatorConfig{
		Mode:           mode,
		Canary:         canary,
		Clock:          func() time.Time { return now },
		AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	med.TickStart(context.Background(), now)
	return &execFixture{db: db, med: med, svc: svc, day: day, now: now, dc: dc, lc: lc, set: set, lane: lc.Lane}
}

func (f *execFixture) grant(t *testing.T, waveKey, touchClass string, requested int, isps ...string) *Allocation {
	t.Helper()
	a, err := f.med.Grant(context.Background(), GrantReq{
		Day:        f.day,
		Lane:       f.lane,
		Brand:      f.dc.BrandCode,
		Domain:     f.dc.SendingDomain,
		TouchClass: touchClass,
		Pass:       PassWelcome,
		WaveKey:    waveKey,
		ISPs:       isps,
		Requested:  requested,
	})
	if err != nil {
		t.Fatalf("Grant(%s): %v", waveKey, err)
	}
	return a
}

// seedDay2 is seedDay for an arbitrary (domain, lane) pair, so a test can put a
// SECOND, unplanned cell next to the planned one.
func seedDay2(t *testing.T, db *sql.DB, day time.Time, domain, lane string, domainMax, laneDesired int) (*DomainContract, *DispatchContract) {
	t.Helper()
	dc := domainContract(domain, 1, map[string]int{"aol": domainMax})
	lc := dispatchContract(lane, 1, map[string]int{"aol": laneDesired})
	if _, err := EnsureDayBalances(context.Background(), db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances(%s/%s): %v", domain, lane, err)
	}
	return dc, lc
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// -----------------------------------------------------------------------------
// §8.2 test 10 — intros and follow-ups share ONE balance
// -----------------------------------------------------------------------------

// The whole point of §2.7's "intros and follow-ups share one balance" is that a
// lane cannot mail its ladder AND its introductions each up to the domain's
// full daily contract. The two touch classes reserve against the SAME
// drip_capacity_balance row, so the second grant sees what the first consumed.
//
// Fixture: aol 7,600/day over 76 intervals = 100 tokens per interval, burst 2
// => 200 tokens available at 10:00. The intro asks for 150 and gets it; the
// follow-up asks for 150 and can only get the 50 that are left.
func TestIntrosAndFollowupsShareOneBalance(t *testing.T) {
	f := newExecFixture(t, ModeOn, 7600, 100000)

	intro := f.grant(t, "wave-intro", TouchClassIntro, 150, "aol")
	if !intro.Enforced {
		t.Fatalf("intro not enforced: %+v", intro)
	}
	if got := intro.Caps["aol"]; got != 150 {
		t.Fatalf("intro granted %d, want 150 (tokens=200)", got)
	}

	followup := f.grant(t, "wave-followup", TouchClassFollowup, 150, "aol")
	if got := followup.Caps["aol"]; got != 50 {
		t.Fatalf("follow-up granted %d, want 50 — intros and follow-ups must share ONE domain×ISP balance", got)
	}
	if followup.Reason != ReasonDomainTokens {
		t.Fatalf("follow-up binding reason %q, want %q", followup.Reason, ReasonDomainTokens)
	}

	bal := readBalance(t, f.db, f.day, f.dc.SendingDomain, "aol")
	if bal.Reserved != 200 {
		t.Fatalf("balance.reserved = %d, want 200 (150 intro + 50 follow-up on one row)", bal.Reserved)
	}

	// NEGATIVE CONTROL for the same claim: two SEPARATE balances would have
	// let the follow-up take another 150. Assert the ledger carries two rows
	// against the same (day, domain, isp) whose reserved sums to the bucket,
	// not to 300.
	var sum int
	if err := f.db.QueryRow(`
		SELECT COALESCE(SUM(reserved), 0) FROM drip_capacity_ledger
		WHERE day = $1::date AND sending_domain = $2 AND isp = 'aol'
	`, dayKey(f.day), f.dc.SendingDomain).Scan(&sum); err != nil {
		t.Fatalf("sum ledger: %v", err)
	}
	if sum != 200 {
		t.Fatalf("ledger reserved sum = %d, want 200", sum)
	}
}

// Commit settles each touch class's OWN allocation, and the remainder of a
// partially submitted wave goes back immediately rather than waiting for
// ExpireStale.
func TestCommitReleasesTheUnsubmittedRemainder(t *testing.T) {
	f := newExecFixture(t, ModeOn, 7600, 100000)
	alloc := f.grant(t, "wave-1", TouchClassIntro, 150, "aol")
	if alloc.Caps["aol"] != 150 {
		t.Fatalf("granted %d, want 150", alloc.Caps["aol"])
	}
	campaign := uuid.New()
	if err := alloc.Commit(context.Background(), map[string]int{"aol": 90}, campaign); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	row := readLedger(t, f.db, alloc.ID)
	if row.Committed != 90 || row.Released != 60 {
		t.Fatalf("ledger committed=%d released=%d, want 90/60", row.Committed, row.Released)
	}
	bal := readBalance(t, f.db, f.day, f.dc.SendingDomain, "aol")
	if bal.Reserved != 0 || bal.Committed != 90 {
		t.Fatalf("balance reserved=%d committed=%d, want 0/90", bal.Reserved, bal.Committed)
	}
}

// -----------------------------------------------------------------------------
// Shadow mode
// -----------------------------------------------------------------------------

// MODE=shadow must compute and record, and change NOTHING: no live ledger row,
// no balance decrement, and caps=nil so the old chain still decides.
func TestShadowWritesOnlyToTheShadowLedger(t *testing.T) {
	f := newExecFixture(t, ModeShadow, 7600, 100000)

	alloc := f.grant(t, "wave-shadow", TouchClassIntro, 150, "aol")
	if alloc == nil {
		t.Fatal("shadow mode returned no allocation — the outcome row would have nothing to say")
	}
	if alloc.Enforced {
		t.Fatal("shadow mode must not enforce")
	}
	if alloc.EnforcedCaps() != nil {
		t.Fatalf("shadow mode returned caps %v — the old chain must decide", alloc.EnforcedCaps())
	}

	if n := countRows(t, f.db, "drip_capacity_ledger"); n != 0 {
		t.Fatalf("%d live ledger rows written in shadow mode, want 0", n)
	}
	if n := countRows(t, f.db, "drip_capacity_ledger_shadow"); n != 1 {
		t.Fatalf("%d shadow ledger rows, want 1", n)
	}
	b := readBalance(t, f.db, f.day, f.dc.SendingDomain, "aol")
	if b.Reserved != 0 {
		t.Fatalf("shadow mode reserved %d on the live balance, want 0 (no locks, no consumption)", b.Reserved)
	}

	var reserved int
	var reason string
	if err := f.db.QueryRow(`
		SELECT reserved, binding_reason FROM drip_capacity_ledger_shadow
	`).Scan(&reserved, &reason); err != nil {
		t.Fatalf("read shadow row: %v", err)
	}
	if reserved != 150 {
		t.Fatalf("shadow row reserved=%d, want the 150 the live path would have granted", reserved)
	}
	if reason != ReasonRequested {
		t.Fatalf("shadow binding_reason=%q, want %q", reason, ReasonRequested)
	}
}

// The shadow arithmetic must equal the live arithmetic — that is the entire
// premise of the §7 step-2 reconciliation gate. Two identical domains, one
// reserved live and one shadowed, must produce the same granted + reason.
func TestShadowReserveMatchesReserve(t *testing.T) {
	db := newExecutorTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)
	ctx := context.Background()

	live := domainContract("em.live.example", 3, map[string]int{"aol": 7600})
	shadow := domainContract("em.shadow.example", 3, map[string]int{"aol": 7600})
	// lane desired 150 < the 200-token burst ceiling, so lane_demand is the
	// binding term on BOTH sides and the parity assertion is about the reason
	// as well as the number.
	lane := dispatchContract("wcl_remail", 4, map[string]int{"aol": 150})
	set := activeSet(day, []*DomainContract{live, shadow}, []*DispatchContract{lane})
	if _, err := EnsureDayBalances(ctx, db, day, set); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}

	svc := NewService(db, WithClock(func() time.Time { return now }))
	med := NewMediator(db, svc, MediatorConfig{
		Mode: ModeShadow, Clock: func() time.Time { return now }, AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	med.TickStart(ctx, now)

	// Same bucket state on both sides.
	for _, d := range []string{live.SendingDomain, shadow.SendingDomain} {
		if _, err := svc.RefillDomain(ctx, day, domainContract(d, 3, map[string]int{"aol": 7600})); err != nil {
			t.Fatalf("refill %s: %v", d, err)
		}
	}

	base := ReserveReq{
		Day: day, ISP: "aol", Lane: lane.Lane, TouchClass: TouchClassIntro,
		Requested: 400, MailableSupply: -1, DomainVersion: 3, DispatchVersion: 4,
		Win: DefaultWindow(),
	}

	// SHADOW FIRST: the lane balance is shared across the two domains, and the
	// live Reserve decrements it. Running the shadow side first is what makes
	// the two calls see the same inputs — and is also the honest simulation of
	// what shadow mode does in production (it never consumes).
	shadowReq := base
	shadowReq.Domain, shadowReq.WaveKey = shadow.SendingDomain, "w1"
	granted, reason, err := med.shadowReserve(ctx, shadowReq)
	if err != nil {
		t.Fatalf("shadowReserve: %v", err)
	}

	liveReq := base
	liveReq.Domain, liveReq.WaveKey = live.SendingDomain, "w1"
	res, err := svc.Reserve(ctx, liveReq)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if granted != res.Granted || reason != res.BindingReason {
		t.Fatalf("shadow (%d, %q) != live (%d, %q) — the reconciliation gate compares these two numbers",
			granted, reason, res.Granted, res.BindingReason)
	}
	// The lane balance is shared, so the live reserve must NOT have moved the
	// shadow side's answer: both bind on lane_unfilled = 220 here.
	if reason != ReasonLaneDemand || granted != 150 {
		t.Fatalf("shadow granted=%d reason=%q, want 150/%q (lane desired 150 < the 200-token burst)", granted, reason, ReasonLaneDemand)
	}
}

// -----------------------------------------------------------------------------
// Canary
// -----------------------------------------------------------------------------

func TestParseCanaryAndMatching(t *testing.T) {
	cells, err := ParseCanary(" em.historythinking.com:aol:wcl_remail , em.discountblog.com:*:internal_auto_insurance ")
	if err != nil {
		t.Fatalf("ParseCanary: %v", err)
	}
	if len(cells) != 2 {
		t.Fatalf("parsed %d cells, want 2", len(cells))
	}
	cases := []struct {
		domain, isp, lane string
		want              bool
	}{
		{"em.historythinking.com", "aol", "wcl_remail", true},
		{"em.historythinking.com", "gmail", "wcl_remail", false},
		{"em.historythinking.com", "aol", "other_lane", false},
		{"em.discountblog.com", "gmail", "internal_auto_insurance", true},
		{"em.discountblog.com", "yahoo", "internal_auto_insurance", true},
		{"em.discountblog.com", "yahoo", "wcl_remail", false},
		{"em.myownhealth.net", "aol", "wcl_remail", false},
	}
	for _, c := range cases {
		if got := canaryMatch(cells, c.domain, c.isp, c.lane); got != c.want {
			t.Errorf("canaryMatch(%s,%s,%s) = %v, want %v", c.domain, c.isp, c.lane, got, c.want)
		}
	}
	for _, bad := range []string{"a:b", "a:b:c:d", "::", "a::c"} {
		if _, err := ParseCanary(bad); err == nil {
			t.Errorf("ParseCanary(%q) accepted a malformed cell", bad)
		}
	}
}

// MODE=canary enforces the listed cell and ONLY the listed cell; every other
// ISP of the same wave is shadowed and keeps its old-chain cap.
func TestCanaryEnforcesOnlyTheListedCell(t *testing.T) {
	db := newExecutorTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)
	ctx := context.Background()

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 7600, "gmail": 7600})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 100000, "gmail": 100000})
	set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})
	if _, err := EnsureDayBalances(ctx, db, day, set); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	svc := NewService(db, WithClock(func() time.Time { return now }))
	med := NewMediator(db, svc, MediatorConfig{
		Mode:           ModeCanary,
		Canary:         []CanaryCell{{Domain: "em.historythinking.com", ISP: "aol", Lane: "wcl_remail"}},
		Clock:          func() time.Time { return now },
		AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	med.TickStart(ctx, now)

	alloc, err := med.Grant(ctx, GrantReq{
		Day: day, Lane: "wcl_remail", Brand: "ht", Domain: "em.historythinking.com",
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "w1",
		ISPs: []string{"aol", "gmail"}, Requested: 120,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	caps := alloc.EnforcedCaps()
	if caps == nil {
		t.Fatal("canary cell was not enforced")
	}
	if _, ok := caps["gmail"]; ok {
		t.Fatalf("gmail is not a canary cell but came back enforced: %v", caps)
	}
	if caps["aol"] != 120 {
		t.Fatalf("aol granted %d, want 120", caps["aol"])
	}
	// aol consumed live capacity; gmail consumed none and left a shadow row.
	if b := readBalance(t, db, day, dc.SendingDomain, "aol"); b.Reserved != 120 {
		t.Fatalf("aol balance reserved=%d, want 120", b.Reserved)
	}
	if b := readBalance(t, db, day, dc.SendingDomain, "gmail"); b.Reserved != 0 {
		t.Fatalf("gmail balance reserved=%d, want 0 — a non-canary cell must not consume", b.Reserved)
	}
	if n := countRows(t, db, "drip_capacity_ledger_shadow"); n != 1 {
		t.Fatalf("%d shadow rows, want 1 (gmail)", n)
	}
}

// -----------------------------------------------------------------------------
// Fail-closed paths
// -----------------------------------------------------------------------------

// §1.5: with no CONTRACT_TOKEN_KEY no contract can be verified, so an enforcing
// mode must SKIP every wave with a reason that names the missing env var —
// not fall through to the old chain, and not enforce an unverified contract.
func TestNoContractKeyFailsClosedWhenEnforcing(t *testing.T) {
	t.Setenv("CONTRACT_TOKEN_KEY", "")
	db, err := sql.Open("postgres", "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	med := NewMediator(db, NewService(db), MediatorConfig{Mode: ModeOn, AlertsDisabled: true})
	alloc, gErr := med.Grant(context.Background(), GrantReq{
		Lane: "wcl_remail", Domain: "em.historythinking.com", Brand: "ht",
		TouchClass: TouchClassIntro, WaveKey: "w1", ISPs: []string{"aol"}, Requested: 10,
	})
	if gErr != nil {
		t.Fatalf("Grant returned an error instead of failing closed: %v", gErr)
	}
	if !alloc.ShouldSkip() || alloc.SkipReason() != SkipNoContractKey {
		t.Fatalf("alloc = %+v, want Skip with reason %q", alloc, SkipNoContractKey)
	}

	// NEGATIVE CONTROL: the same missing key with MODE=shadow must NOT skip —
	// shadow may not enforce, so it may not stop a wave either.
	shadowMed := NewMediator(db, NewService(db), MediatorConfig{Mode: ModeShadow, AlertsDisabled: true})
	sAlloc, _ := shadowMed.Grant(context.Background(), GrantReq{
		Lane: "wcl_remail", Domain: "em.historythinking.com", Brand: "ht",
		TouchClass: TouchClassIntro, WaveKey: "w1", ISPs: []string{"aol"}, Requested: 10,
	})
	if sAlloc.ShouldSkip() {
		t.Fatal("shadow mode must never stop a wave")
	}
}

// A lane with no active dispatch contract fails CLOSED under MODE=on and stays
// out of the way under MODE=shadow.
func TestMissingContractFailsClosedOnlyWhenEnforcing(t *testing.T) {
	for _, tc := range []struct {
		mode     Mode
		wantSkip bool
	}{
		{ModeOn, true},
		{ModeShadow, false},
		{ModeCanary, false}, // no cell listed => nothing enforced => nothing skipped
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			f := newExecFixture(t, tc.mode, 7600, 100000)
			alloc, err := f.med.Grant(context.Background(), GrantReq{
				Day: f.day, Lane: "a_lane_with_no_contract", Brand: "ht",
				Domain: f.dc.SendingDomain, TouchClass: TouchClassIntro,
				WaveKey: "w1", ISPs: []string{"aol"}, Requested: 10,
			})
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if alloc.ShouldSkip() != tc.wantSkip {
				t.Fatalf("mode=%s ShouldSkip=%v, want %v", tc.mode, alloc.ShouldSkip(), tc.wantSkip)
			}
			if tc.wantSkip && alloc.SkipReason() != SkipNoContract {
				t.Fatalf("skip reason %q, want %q", alloc.SkipReason(), SkipNoContract)
			}
		})
	}
}

// MODE=off is the rollback: Grant returns before it can touch the database.
// The DSN below points at a closed port, so ANY query would fail the test.
func TestModeOffTouchesNothing(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	med := NewMediator(db, NewService(db), MediatorConfig{Mode: ModeOff, AlertsDisabled: true})
	med.TickStart(context.Background(), time.Now())
	alloc, gErr := med.Grant(context.Background(), GrantReq{
		Lane: "wcl_remail", Domain: "em.historythinking.com", TouchClass: TouchClassIntro,
		WaveKey: "w1", ISPs: []string{"aol"}, Requested: 10,
	})
	if gErr != nil {
		t.Fatalf("MODE=off must not error: %v", gErr)
	}
	if alloc != nil {
		t.Fatalf("MODE=off returned %+v, want nil (the old chain decides)", alloc)
	}
	// Nil-receiver contract, exercised because the orchestrator calls these on
	// the result of Grant without a nil check.
	if alloc.EnforcedCaps() != nil || alloc.ShouldSkip() || alloc.AllocationID() != uuid.Nil {
		t.Fatal("nil *Allocation must read as 'not enforced, not skipped, no id'")
	}
	if err := alloc.Release(context.Background(), "x"); err != nil {
		t.Fatalf("Release on a nil allocation: %v", err)
	}
	if err := alloc.Commit(context.Background(), map[string]int{"aol": 1}, uuid.New()); err != nil {
		t.Fatalf("Commit on a nil allocation: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Tick outcomes
// -----------------------------------------------------------------------------

func readOutcome(t *testing.T, db *sql.DB, lane, pass string) (outcome, reason string, claimed int) {
	t.Helper()
	if err := db.QueryRow(`
		SELECT outcome, reason, claimed FROM drip_tick_outcomes WHERE lane = $1 AND pass = $2
	`, lane, pass).Scan(&outcome, &reason, &claimed); err != nil {
		t.Fatalf("read outcome %s/%s: %v", lane, pass, err)
	}
	return
}

// §8.2 test 11, mediator half: every outcome lands with a reason, several
// brands of one lane collapse onto ONE row per (tick, lane, pass), the highest
// priority outcome wins, and the claimed counts sum.
func TestOutcomeUpsertKeepsThePriorityOutcome(t *testing.T) {
	db := newExecutorTestDB(t)
	med := NewMediator(db, nil, MediatorConfig{Mode: ModeOff, AlertsDisabled: true,
		Clock: func() time.Time { return testDay(t).Add(10 * time.Hour) }})
	med.TickStart(context.Background(), testDay(t).Add(10*time.Hour))
	ctx := context.Background()

	med.Outcome(ctx, OutcomeRow{Lane: "l1", Pass: PassWelcome, Outcome: OutcomeZero,
		Reason: ZeroNoRecordsClaimed, Brand: "db", CapsSeen: map[string]int{"aol": 4}})
	med.Outcome(ctx, OutcomeRow{Lane: "l1", Pass: PassWelcome, Outcome: OutcomeFired,
		Brand: "ht", Claimed: 120, CampaignID: uuid.New().String()})
	med.Outcome(ctx, OutcomeRow{Lane: "l1", Pass: PassWelcome, Outcome: OutcomeSkipped,
		Reason: SkipBudgetExhausted, Brand: "mh"})

	outcome, reason, claimed := readOutcome(t, db, "l1", PassWelcome)
	if outcome != OutcomeFired {
		t.Fatalf("outcome=%q, want %q — a lane that shipped must not read as dead because a later brand skipped", outcome, OutcomeFired)
	}
	if claimed != 120 {
		t.Fatalf("claimed=%d, want 120 (summed across the lane's brands)", claimed)
	}
	if reason == "" {
		t.Fatal("reason is empty — every row must carry one")
	}
	if n := countRows(t, db, "drip_tick_outcomes"); n != 1 {
		t.Fatalf("%d outcome rows for one lane×pass×tick, want 1", n)
	}

	// A failure outranks the fire.
	med.Outcome(ctx, OutcomeRow{Lane: "l1", Pass: PassWelcome, Outcome: OutcomeFailed, Reason: "claim_records", Brand: "qf"})
	if outcome, _, _ = readOutcome(t, db, "l1", PassWelcome); outcome != OutcomeFailed {
		t.Fatalf("outcome=%q after a failure, want %q", outcome, OutcomeFailed)
	}

	// A different pass is a different row.
	med.Outcome(ctx, OutcomeRow{Lane: "l1", Pass: PassFollowup, Outcome: OutcomeZero, Reason: ZeroAllDeferred})
	if n := countRows(t, db, "drip_tick_outcomes"); n != 2 {
		t.Fatalf("%d rows, want 2 (welcome + followup)", n)
	}
}

// The kill switch has to actually kill.
func TestOutcomesDisabledWritesNothing(t *testing.T) {
	db := newExecutorTestDB(t)
	med := NewMediator(db, nil, MediatorConfig{Mode: ModeOff, OutcomesDisabled: true, AlertsDisabled: true})
	med.TickStart(context.Background(), time.Now())
	med.Outcome(context.Background(), OutcomeRow{Lane: "l1", Pass: PassWelcome, Outcome: OutcomeZero, Reason: "x"})
	if n := countRows(t, db, "drip_tick_outcomes"); n != 0 {
		t.Fatalf("%d rows written with DRIP_TICK_OUTCOMES_DISABLED=1, want 0", n)
	}
}

// -----------------------------------------------------------------------------
// Pure units
// -----------------------------------------------------------------------------

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, "SHADOW": ModeShadow, " canary ": ModeCanary, "on": ModeOn} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = (%v, %v), want (%v, nil)", in, got, err, want)
		}
	}
	if _, err := ParseMode("canery"); err == nil {
		t.Error("ParseMode accepted a typo — a mode that silently resolved to `on` would enforce estate-wide")
	}
}

func TestSplitSubmitted(t *testing.T) {
	tally := map[string]int{"aol": 60, "gmail": 30, "yahoo": 10}
	// Everything shipped: exact.
	if got := SplitSubmitted(tally, 100); !reflect.DeepEqual(got, tally) {
		t.Fatalf("full submission split = %v, want %v", got, tally)
	}
	// Partial: largest remainder, and the total is preserved exactly.
	got := SplitSubmitted(tally, 51)
	sum := 0
	for _, n := range got {
		sum += n
	}
	if sum != 51 {
		t.Fatalf("partial split sums to %d, want 51 (%v)", sum, got)
	}
	if got["aol"] < got["gmail"] || got["gmail"] < got["yahoo"] {
		t.Fatalf("partial split is not proportional: %v", got)
	}
	if n := len(SplitSubmitted(tally, 0)); n != 0 {
		t.Fatalf("zero submission produced %d entries, want 0", n)
	}
}

func TestOutcomePriorityOrder(t *testing.T) {
	if !(outcomePriority(OutcomeFailed) > outcomePriority(OutcomeFired) &&
		outcomePriority(OutcomeFired) > outcomePriority(OutcomeZero) &&
		outcomePriority(OutcomeZero) > outcomePriority(OutcomeSkipped)) {
		t.Fatal("outcome priority order is wrong: failed > fired > zero > skipped")
	}
}

// -----------------------------------------------------------------------------
// The daily plan — scheduling, idempotency, and the plan_share term
// -----------------------------------------------------------------------------

// planSchemaDDL adds the plan tables to a scratch schema. DailyPlanDDL is WP6's
// exported constant, so a schema drift breaks this test rather than production.
func execPlanSchemaDDL() []string {
	return []string{
		DailyPlanDDL,
		`CREATE TABLE IF NOT EXISTS drip_daily_plan_shadow (LIKE drip_daily_plan INCLUDING ALL)`,
	}
}

func newExecPlanTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newExecutorTestDB(t)
	for _, stmt := range execPlanSchemaDDL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("plan ddl: %v", err)
		}
	}
	return db
}

func insertPlanRow(t *testing.T, db *sql.DB, table string, day time.Time, lane, isp, domain string, firm, provisional, followups int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO `+table+`
		(day, lane, isp, sending_domain, award_firm, award_provisional, followups_reserved, plan_share, rank, rank_reason, frozen_at)
		VALUES ($1::date,$2,$3,$4,$5,$6,$7,1,1,'test',NOW())`,
		dayKey(day), lane, isp, domain, firm, provisional, followups); err != nil {
		t.Fatalf("insert plan row: %v", err)
	}
}

// The plan is computed ONCE per Denver day. Three independent guards have to
// hold, because each covers a failure the others do not: the in-process day
// cache (a 15-minute tick), the frozen-rows EXISTS (a second ECS instance), and
// the planner's own short circuit (a 00:05 double fire).
func TestDailyPlanRunsOncePerDay(t *testing.T) {
	db := newExecPlanTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)
	ctx := context.Background()

	runs := 0
	planFn := func(_ context.Context, d time.Time) (*Plan, error) {
		runs++
		insertPlanRow(t, db, "drip_daily_plan", d, "wcl_remail", "aol", "em.historythinking.com", 100, 0, 0)
		return &Plan{Day: d, FrozenAt: now}, nil
	}
	cfg := func() MediatorConfig {
		return MediatorConfig{
			Mode: ModeOn, Clock: func() time.Time { return now }, AlertsDisabled: true,
			PlanFunc:       planFn,
			ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return activeSet(day, nil, nil), nil },
		}
	}

	med := NewMediator(db, NewService(db), cfg())
	med.ensureDailyPlan(ctx, now)
	if runs != 1 {
		t.Fatalf("first pass ran the planner %d times, want 1", runs)
	}

	// Guard 1: the in-process day cache — a second tick in the same process.
	med.ensureDailyPlan(ctx, now.Add(15*time.Minute))
	if runs != 1 {
		t.Fatalf("a second tick re-planned (runs=%d) — the day cache is not holding", runs)
	}

	// Guard 2: a FRESH mediator (the other ECS instance, or a deploy) must see
	// the frozen rows and not re-plan.
	other := NewMediator(db, NewService(db), cfg())
	other.ensureDailyPlan(ctx, now.Add(2*time.Hour))
	if runs != 1 {
		t.Fatalf("a fresh process re-planned a frozen day (runs=%d) — the EXISTS guard is not holding", runs)
	}
	// Guard 2 also has to have recorded the day, or every later tick pays for
	// the EXISTS again.
	other.mu.Lock()
	cached := other.plannedDay
	other.mu.Unlock()
	if cached != dayKey(day) {
		t.Fatalf("fresh mediator cached %q, want %q", cached, dayKey(day))
	}

	// NEGATIVE CONTROL: the NEXT Denver day has no frozen rows, so it must plan
	// again. If this does not increment, the guards above are just "never plan".
	next := day.AddDate(0, 0, 1)
	nextNow := next.Add(10 * time.Hour)
	nextMed := NewMediator(db, NewService(db), MediatorConfig{
		Mode: ModeOn, Clock: func() time.Time { return nextNow }, AlertsDisabled: true,
		PlanFunc:       planFn,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return activeSet(next, nil, nil), nil },
	})
	nextMed.ensureDailyPlan(ctx, nextNow)
	if runs != 2 {
		t.Fatalf("a new Denver day did not plan (runs=%d, want 2)", runs)
	}
}

// The kill switch has to actually kill, and the worker must not poll when it
// can do nothing.
func TestPlannerDisabledAndWorkerInertness(t *testing.T) {
	db := newExecPlanTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	runs := 0
	base := MediatorConfig{
		Mode: ModeOn, Clock: func() time.Time { return now }, AlertsDisabled: true,
		PlanFunc:       func(context.Context, time.Time) (*Plan, error) { runs++; return &Plan{Day: day}, nil },
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return activeSet(day, nil, nil), nil },
	}

	disabled := base
	disabled.PlannerDisabled = true
	med := NewMediator(db, NewService(db), disabled)
	med.ensureDailyPlan(context.Background(), now)
	if runs != 0 {
		t.Fatalf("DRIP_PLANNER_DISABLED planned anyway (runs=%d)", runs)
	}
	if w := NewPlannerWorker(db, nil, PlannerWorkerConfig{Mediator: med}); !w.Disabled() {
		t.Fatal("the worker must be inert when the mediator's planner is disabled")
	}

	// MODE=off is the rollback: nothing reads the plan, so nothing plans.
	off := base
	off.Mode = ModeOff
	offMed := NewMediator(db, NewService(db), off)
	if w := NewPlannerWorker(db, nil, PlannerWorkerConfig{Mediator: offMed}); !w.Disabled() {
		t.Fatal("the worker must be inert on MODE=off")
	}

	// NEGATIVE CONTROL: enabled + a plan source => the worker is live and its
	// RunOnce plans the day.
	live := NewMediator(db, NewService(db), base)
	w := NewPlannerWorker(db, nil, PlannerWorkerConfig{Mediator: live, NowFn: func() time.Time { return now }})
	if w.Disabled() {
		t.Fatal("the worker must be live when the planner is wired and the mode enforces")
	}
	if err := w.RunOnce(context.Background(), day); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if runs != 1 {
		t.Fatalf("RunOnce planned %d times, want 1", runs)
	}
}

// §7 step 2: a shadow plan lands in the shadow table and NOWHERE else — in
// particular it must not touch drip_lane_balance, which is what the live
// executor reserves against.
func TestShadowPlanWritesOnlyToTheShadowTable(t *testing.T) {
	db := newExecPlanTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	// Seed a live lane balance so "untouched" is a real assertion.
	dc, lc := seedDay(t, db, day, 7600, 4321)

	med := NewMediator(db, NewService(db), MediatorConfig{
		Mode: ModeShadow, Clock: func() time.Time { return now }, AlertsDisabled: true,
	})
	plan := &Plan{Day: day, FrozenAt: now, Rows: []PlanRow{{
		Day: day, Lane: lc.Lane, ISP: "aol", SendingDomain: dc.SendingDomain,
		AwardFirm: 900, AwardProvisional: 100, FollowupsReserved: 50, RankReason: "test",
	}}}
	if err := med.storeShadowPlan(context.Background(), plan); err != nil {
		t.Fatalf("storeShadowPlan: %v", err)
	}

	if n := countRows(t, db, "drip_daily_plan_shadow"); n != 1 {
		t.Fatalf("%d shadow plan rows, want 1", n)
	}
	if n := countRows(t, db, "drip_daily_plan"); n != 0 {
		t.Fatalf("%d LIVE plan rows written in shadow mode, want 0", n)
	}
	if l := readLane(t, db, day, lc.Lane, "aol"); l.Desired != 4321 || l.Unfilled != 4321 {
		t.Fatalf("shadow plan moved the live lane balance: %+v", l)
	}
	// The mediator must also READ its plan from the shadow table in this mode,
	// or the frozen-day guard would consult a table it never writes.
	if got := med.planTable(); got != "drip_daily_plan_shadow" {
		t.Fatalf("planTable() = %q in shadow mode", got)
	}
}

// The plan is only worth writing if it BINDS. A plan row awarding 100 must cap
// a 500-record request at 100, with binding_reason='plan_share'.
func TestPlanReaderBoundsAReservation(t *testing.T) {
	db := newExecPlanTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)
	ctx := context.Background()

	dc, lc := seedDay(t, db, day, 7600, 100000)
	svc := NewService(db,
		WithClock(func() time.Time { return now }),
		WithPlanReader(PlanStore{}),
	)
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill: %v", err)
	}
	insertPlanRow(t, db, "drip_daily_plan", day, lc.Lane, "aol", dc.SendingDomain, 100, 0, 0)

	req := ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "aol", Lane: lc.Lane,
		TouchClass: TouchClassIntro, WaveKey: "w1", Requested: 500,
		MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: DefaultWindow(),
	}
	res, err := svc.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if res.Granted != 100 || res.BindingReason != ReasonPlanShare {
		t.Fatalf("granted=%d reason=%q, want 100/%q — the daily plan does not bind",
			res.Granted, res.BindingReason, ReasonPlanShare)
	}

	// NEGATIVE CONTROL: a cell with NO plan row must fail OPEN (bounded=false),
	// so the same request is capped by the token bucket (200), not by the plan.
	// Without this, "granted=100" could just as well mean "the plan reader
	// returns 100 for everything" or "reserve is broken".
	dc2, lc2 := seedDay2(t, db, day, "em.unplanned.example", "unplanned_lane", 7600, 100000)
	if _, err := svc.RefillDomain(ctx, day, dc2); err != nil {
		t.Fatalf("refill 2: %v", err)
	}
	req2 := req
	req2.Domain, req2.Lane, req2.WaveKey = dc2.SendingDomain, lc2.Lane, "w2"
	res2, err := svc.Reserve(ctx, req2)
	if err != nil {
		t.Fatalf("Reserve (unplanned): %v", err)
	}
	if res2.BindingReason == ReasonPlanShare {
		t.Fatal("a cell with no plan row was bound by plan_share — PlanRemaining must fail OPEN")
	}
	if res2.Granted != 200 {
		t.Fatalf("unplanned cell granted %d, want the 200-token burst ceiling", res2.Granted)
	}
}

// Follow-ups take their ceiling from followups_reserved, not from the intro
// award (§2.7) — the obligation §5.4 exists to protect.
func TestPlanReaderIsTouchClassAware(t *testing.T) {
	db := newExecPlanTestDB(t)
	day := testDay(t)
	now := day.Add(10 * time.Hour)
	ctx := context.Background()

	dc, lc := seedDay(t, db, day, 7600, 100000)
	svc := NewService(db, WithClock(func() time.Time { return now }), WithPlanReader(PlanStore{}))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill: %v", err)
	}
	// Intro award 10, follow-up reserve 150.
	insertPlanRow(t, db, "drip_daily_plan", day, lc.Lane, "aol", dc.SendingDomain, 10, 0, 150)

	base := ReserveReq{
		Day: day, Domain: dc.SendingDomain, ISP: "aol", Lane: lc.Lane,
		Requested: 500, MailableSupply: -1, DomainVersion: 1, DispatchVersion: 1, Win: DefaultWindow(),
	}
	intro := base
	intro.TouchClass, intro.WaveKey = TouchClassIntro, "wi"
	ri, err := svc.Reserve(ctx, intro)
	if err != nil {
		t.Fatalf("Reserve intro: %v", err)
	}
	if ri.Granted != 10 {
		t.Fatalf("intro granted %d, want the 10-row intro award", ri.Granted)
	}

	fu := base
	fu.TouchClass, fu.WaveKey = TouchClassFollowup, "wf"
	rf, err := svc.Reserve(ctx, fu)
	if err != nil {
		t.Fatalf("Reserve followup: %v", err)
	}
	// 150 follow-up ceiling, but only 190 tokens are left after the intro's 10.
	if rf.Granted != 150 || rf.BindingReason != ReasonPlanShare {
		t.Fatalf("follow-up granted=%d reason=%q, want 150/%q — the intro award must not be the follow-up ceiling",
			rf.Granted, rf.BindingReason, ReasonPlanShare)
	}
}

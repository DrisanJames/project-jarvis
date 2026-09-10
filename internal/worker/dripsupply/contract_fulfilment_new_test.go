package dripsupply

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// contract_fulfilment_new_test.go — the two rules whose surface did not exist
// before 2026-09-09, so these tests cannot compile on pre-change HEAD (the
// symbols they name are new). They FAIL there as a build failure; the
// behavioural proofs live in contract_fulfilment_test.go.
//
//	rule 2 (verdict half) — a mint that cannot reach the promise writes a
//	                        `short_mint` outcome carrying the shortfall and its
//	                        cause. Nothing wrote one; the shortfall sat in
//	                        drip_daily_plan.unserved, a column nobody queries
//	                        until they already suspect a problem.
//	rule 4               — `failed_retryable` rows are expired at window close
//	                        and never carried into the next day. Nothing expired
//	                        them; the send worker's claim predicate has no day
//	                        bound, so a row that failed at 19:50 shipped against
//	                        TOMORROW's contract.

// -----------------------------------------------------------------------------
// RULE 2 (verdict half) — short_mint
// -----------------------------------------------------------------------------

// The arithmetic, as a pure function: promised, planned, the shortfall, and the
// cause that carried the most of it.
func TestShortMint_VerdictFoldsToTheLaneAndNamesTheDominantCause(t *testing.T) {
	plan := &Plan{Lanes: []LaneAward{
		// aol is fine; yahoo is 60% short on supply; gmail is 90% short on
		// domain capacity and is the LARGER shortfall, so it must be the cause.
		{Lane: "wcl_remail", ISP: "aol", Desired: 1000, AwardedFirm: 1000},
		{Lane: "wcl_remail", ISP: "yahoo", Desired: 5000, AwardedFirm: 1000, AwardedProvisional: 1000, UnservedReason: UnservedSupply},
		{Lane: "wcl_remail", ISP: "gmail", Desired: 10000, AwardedFirm: 1000, UnservedReason: UnservedDomainCapacity},
		{Lane: "quiet_lane", ISP: "aol", Desired: 2000, AwardedFirm: 2000},
	}}
	got := mintVerdicts(plan)
	if len(got) != 2 {
		t.Fatalf("got %d verdicts, want one per lane", len(got))
	}
	// Lanes come back sorted, so two instances replaying the same plan write the
	// same rows in the same order.
	if got[0].Lane != "quiet_lane" || got[1].Lane != "wcl_remail" {
		t.Fatalf("verdict order = %s,%s, want sorted", got[0].Lane, got[1].Lane)
	}
	q := got[0]
	if q.Short != 0 || q.Promised != 2000 || q.Planned != 2000 {
		t.Errorf("quiet_lane = %+v, want a kept promise", q)
	}
	w := got[1]
	if w.Promised != 16000 || w.Planned != 4000 || w.Short != 12000 {
		t.Errorf("wcl_remail promised/planned/short = %d/%d/%d, want 16000/4000/12000", w.Promised, w.Planned, w.Short)
	}
	if w.Cause != UnservedDomainCapacity {
		t.Errorf("cause = %q, want the reason carrying the LARGEST shortfall (%q); first-seen would have said %q",
			w.Cause, UnservedDomainCapacity, UnservedSupply)
	}
	// Worst ISP first — an operator reads the first name and acts on it.
	if len(w.ISPs) < 2 || w.ISPs[0] != "gmail" || w.ISPs[1] != "yahoo" {
		t.Errorf("isps = %v, want gmail (9000 short) before yahoo (3000 short)", w.ISPs)
	}
}

// The ±5% band, both edges, and the one case that is NOT an anomaly: a promise
// of zero is KEPT by minting zero. Treating a contracted zero as a 100%
// shortfall would fill the outcomes table with anomalies for every ISP the
// estate deliberately does not mail — the eight-brand gmail ban alone.
func TestShortMint_ToleranceIsFivePercentAndZeroIsAlwaysKept(t *testing.T) {
	cases := []struct {
		promised, got int
		wantShort     int
		wantBreach    bool
		why           string
	}{
		{0, 0, 0, false, "a promise of zero is kept by minting zero"},
		{0, 500, 0, false, "over-minting a zero promise is not SHORT (it is the verdict's over: case)"},
		{1000, 1000, 0, false, "exact"},
		{1000, 960, 40, false, "4% short is inside the band"},
		{1000, 950, 50, false, "exactly 5% is inside the band"},
		{1000, 949, 51, true, "5.1% short is a broken promise"},
		{1000, 0, 1000, true, "a dark lane"},
		{100000, 94999, 5001, true, "the band scales with the promise"},
	}
	for _, c := range cases {
		short, breach := shortOfPromise(c.promised, c.got)
		if short != c.wantShort || breach != c.wantBreach {
			t.Errorf("shortOfPromise(%d,%d) = %d,%t want %d,%t — %s",
				c.promised, c.got, short, breach, c.wantShort, c.wantBreach, c.why)
		}
	}
}

// The verdict reaches drip_tick_outcomes, on its OWN pass, with the numbers in
// the reason — because a shortfall that lives only in drip_daily_plan.unserved
// is a shortfall nobody finds until they already suspect one.
func TestShortMint_ReachesTheOutcomesTable(t *testing.T) {
	db := newExecutorTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(5 * time.Minute)

	plan := &Plan{Day: day, Lanes: []LaneAward{
		{Lane: "short_lane", ISP: "aol", Desired: 10000, AwardedFirm: 400, UnservedReason: UnservedSupply},
		{Lane: "kept_lane", ISP: "aol", Desired: 5000, AwardedFirm: 5000},
	}}
	med := NewMediator(db, nil, MediatorConfig{
		Mode: ModeOff, AlertsDisabled: true, Clock: func() time.Time { return now },
	})
	med.TickStart(ctx, now) // sets the tick the outcome rows key on
	med.reportMintVerdict(ctx, day, plan)

	outcome, reason, claimed := readOutcome(t, db, "short_lane", PassMint)
	if outcome != OutcomeFailed {
		t.Errorf("short lane outcome = %q, want %q — an unkeepable promise is an anomaly", outcome, OutcomeFailed)
	}
	for _, want := range []string{ReasonShortMint, "promised=10000", "planned=400", "short=9600", "cause=" + UnservedSupply, "isps=aol"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q is missing %q — the row must carry the shortfall AND its cause", reason, want)
		}
	}
	if claimed != 400 {
		t.Errorf("claimed = %d, want the 400 the plan can actually mint", claimed)
	}

	// The kept lane is recorded too. Absence of a short_mint row must mean
	// "judged and fine", never "the verdict never ran" — the __meta__-sentinel
	// lesson.
	keptOutcome, keptReason, _ := readOutcome(t, db, "kept_lane", PassMint)
	if keptOutcome != OutcomeFired || !strings.Contains(keptReason, ReasonMintOnPromise) {
		t.Errorf("kept lane = %q/%q, want %q/%s", keptOutcome, keptReason, OutcomeFired, ReasonMintOnPromise)
	}
}

// The verdict runs on BOTH plan paths — the 00:05 PlannerWorker (the scheduled
// owner) and the tick's ensureDailyPlan safety net. Wiring only the safety net
// would mean the verdict never runs on a normal day, which is the "worker not
// started at boot is dead code" failure in miniature.
func TestShortMint_RunsOnTheScheduledPlannerPassToo(t *testing.T) {
	db := newExecutorTestDB(t)
	if _, err := db.Exec(DailyPlanDDL); err != nil {
		t.Fatalf("drip_daily_plan ddl: %v", err)
	}
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Minute)

	plan := &Plan{Day: day, FrozenAt: now, Lanes: []LaneAward{
		{Lane: "short_lane", ISP: "aol", Desired: 10000, AwardedFirm: 100, UnservedReason: UnservedSupply},
	}}
	med := NewMediator(db, nil, MediatorConfig{
		Mode: ModeOn, AlertsDisabled: true, Clock: func() time.Time { return now },
		PlanFunc: func(context.Context, time.Time) (*Plan, error) { return plan, nil },
	})
	med.TickStart(ctx, now)
	w := &PlannerWorker{med: med, db: db, loc: day.Location()}
	if err := w.RunOnce(ctx, day); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	outcome, reason, _ := readOutcome(t, db, "short_lane", PassMint)
	if outcome != OutcomeFailed || !strings.Contains(reason, ReasonShortMint) {
		t.Errorf("the 00:05 pass produced %q/%q, want a short_mint failure", outcome, reason)
	}
}

// -----------------------------------------------------------------------------
// RULE 4 — a day's volume dies with its window
// -----------------------------------------------------------------------------

// mcqDDL is the slice of mailing_campaign_queue this sweep touches
// (cmd/server/main.go create_campaign_queue + the durable-outbox columns),
// without the FK to mailing_campaigns/mailing_subscribers, which are irrelevant
// here and would drag half the schema into a scratch database.
func mcqDDL() string {
	return `CREATE TABLE IF NOT EXISTS mailing_campaign_queue (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		campaign_id UUID NOT NULL,
		subscriber_id UUID NOT NULL,
		status VARCHAR(20) DEFAULT 'queued',
		attempts INTEGER DEFAULT 0,
		last_attempt_at TIMESTAMPTZ,
		error_message TEXT,
		locked_at TIMESTAMPTZ,
		next_attempt_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ DEFAULT NOW()
	)`
}

// FAILS ON PRE-CHANGE HEAD (build): ExpireRetryablesAtWindowClose did not
// exist, and nothing else expired these rows.
//
// The leak: send_worker.go's claim predicate re-picks
// `status='failed_retryable' AND (next_attempt_at IS NULL OR next_attempt_at <=
// NOW())` with NO day bound. One row that failed at 19:50 is therefore TWO
// broken promises — today under-delivers by it, tomorrow over-delivers by it —
// and the second is invisible because nothing counts a yesterday row against
// today's balance.
func TestRule4_WindowCloseExpiresRetryablesForTheDaysCampaigns(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	if _, err := db.Exec(mcqDDL()); err != nil {
		t.Fatalf("mcq ddl: %v", err)
	}

	mine := uuid.New()      // funded by this domain's contract today
	other := uuid.New()     // another domain
	yesterday := uuid.New() // this domain, but a different day

	seedLedgerCampaign(t, db, day, "em.historythinking.com", mine)
	seedLedgerCampaign(t, db, day, "em.discountblog.com", other)
	seedLedgerCampaign(t, db, day.AddDate(0, 0, -1), "em.historythinking.com", yesterday)

	retryable := seedQueueRow(t, db, mine, "failed_retryable")
	queued := seedQueueRow(t, db, mine, "queued")
	accepted := seedQueueRow(t, db, mine, "accepted")
	otherLane := seedQueueRow(t, db, other, "failed_retryable")
	yday := seedQueueRow(t, db, yesterday, "failed_retryable")

	tr := NewTransitions()
	res, err := tr.ExpireRetryablesAtWindowClose(ctx, db, day, "em.historythinking.com", 0)
	if err != nil {
		t.Fatalf("ExpireRetryablesAtWindowClose: %v", err)
	}
	if res.Campaigns != 1 || res.Expired != 1 {
		t.Fatalf("swept %+v, want 1 campaign / 1 row", res)
	}

	got := readQueueRow(t, db, retryable)
	if got.status != ExpiredAtWindowStatus {
		t.Errorf("status = %q, want %q", got.status, ExpiredAtWindowStatus)
	}
	if got.errMessage != ExpiredAtWindowReason {
		t.Errorf("error_message = %q, want %q — this is the operator's search key and the reconciliation's discriminator",
			got.errMessage, ExpiredAtWindowReason)
	}
	if got.nextAttempt.Valid {
		t.Error("next_attempt_at survived: a live backoff on a terminal row is how a later reader resurrects it into tomorrow")
	}

	// NEGATIVE CONTROLS. The sweep is surgical or it is a second incident.
	for _, c := range []struct {
		id   uuid.UUID
		want string
		why  string
	}{
		{queued, "queued", "a queued row has not failed and is still today's mail"},
		{accepted, "accepted", "an accepted row already shipped"},
		{otherLane, "failed_retryable", "another domain's window is not this domain's"},
		{yday, "failed_retryable", "yesterday's campaign is not in today's ledger day"},
	} {
		if got := readQueueRow(t, db, c.id); got.status != c.want {
			t.Errorf("row %s status = %q, want %q — %s", c.id, got.status, c.want, c.why)
		}
	}
}

// The status is `cancelled` and NOT a new value, and that is a RETENTION
// decision (WP-C, 2026-09-09): mailing_campaign_queue has no status CHECK in
// prod, so any string would be accepted — and the janitor reaps only
// ('accepted','cancelled','failed','dead_letter','dead_letter_strict'). A row
// parked under 'expired' or 'failed_permanent' would be terminal, unreapable
// and immortal on an 86 GB table.
func TestRule4_ExpiredStatusIsOneTheJanitorReaps(t *testing.T) {
	reaped := map[string]bool{
		"accepted": true, "cancelled": true, "failed": true,
		"dead_letter": true, "dead_letter_strict": true,
	}
	if !reaped[ExpiredAtWindowStatus] {
		t.Fatalf("ExpiredAtWindowStatus = %q is not in the janitor's reap set (internal/api/data_cleanup.go, idx_mcq_terminal_cleanup) — these rows would be immortal on an 86 GB table",
			ExpiredAtWindowStatus)
	}
	// And it must not be a status the send worker will claim again.
	for _, claimable := range []string{"queued", "failed_retryable"} {
		if ExpiredAtWindowStatus == claimable {
			t.Fatalf("ExpiredAtWindowStatus = %q is re-claimable by send_worker.go — the row would ship tomorrow anyway", ExpiredAtWindowStatus)
		}
	}
}

// Re-run safety: the sweep fires on every tick after window close and on every
// ECS bounce. A second pass must find nothing and change nothing.
func TestRule4_SweepIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	if _, err := db.Exec(mcqDDL()); err != nil {
		t.Fatalf("mcq ddl: %v", err)
	}
	camp := uuid.New()
	seedLedgerCampaign(t, db, day, "em.historythinking.com", camp)
	id := seedQueueRow(t, db, camp, "failed_retryable")

	tr := NewTransitions()
	for i := 0; i < 3; i++ {
		res, err := tr.ExpireRetryablesAtWindowClose(ctx, db, day, "em.historythinking.com", 0)
		if err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		want := 1
		if i > 0 {
			want = 0
		}
		if res.Expired != want {
			t.Errorf("sweep %d expired %d rows, want %d", i, res.Expired, want)
		}
	}
	if got := readQueueRow(t, db, id); got.status != ExpiredAtWindowStatus {
		t.Errorf("status = %q after three sweeps, want %q", got.status, ExpiredAtWindowStatus)
	}
}

// A domain with no funded campaigns runs no UPDATE at all: the sweep must never
// become an estate-wide scan keyed on status alone (WP-C: pcq and mcq are large
// and `status='failed_retryable'` across the estate dies at the 30s budget).
func TestRule4_NoCampaignsMeansNoUpdate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	if _, err := db.Exec(mcqDDL()); err != nil {
		t.Fatalf("mcq ddl: %v", err)
	}
	// A retryable row exists, but it belongs to no funded campaign of this
	// domain-day. It must be untouched.
	orphan := seedQueueRow(t, db, uuid.New(), "failed_retryable")

	res, err := NewTransitions().ExpireRetryablesAtWindowClose(ctx, db, day, "em.historythinking.com", 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Campaigns != 0 || res.Expired != 0 {
		t.Fatalf("swept %+v on a domain with no funded campaigns, want nothing", res)
	}
	if got := readQueueRow(t, db, orphan); got.status != "failed_retryable" {
		t.Errorf("orphan row status = %q — the sweep reached a campaign it did not fund", got.status)
	}
}

// The batch bound is real, and a truncated sweep says so. "Expired 2,000" must
// never be mistaken for "there were 2,000".
func TestRule4_SweepIsBoundedAndReportsTruncation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	if _, err := db.Exec(mcqDDL()); err != nil {
		t.Fatalf("mcq ddl: %v", err)
	}
	camp := uuid.New()
	seedLedgerCampaign(t, db, day, "em.historythinking.com", camp)
	for i := 0; i < 5; i++ {
		seedQueueRow(t, db, camp, "failed_retryable")
	}
	tr := NewTransitions()
	res, err := tr.ExpireRetryablesAtWindowClose(ctx, db, day, "em.historythinking.com", 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Expired != 2 || !res.Truncated {
		t.Fatalf("bounded sweep = %+v, want 2 expired and Truncated=true", res)
	}
	res2, _ := tr.ExpireRetryablesAtWindowClose(ctx, db, day, "em.historythinking.com", 10)
	if res2.Expired != 3 || res2.Truncated {
		t.Fatalf("second sweep = %+v, want the remaining 3 and Truncated=false", res2)
	}
}

// The mediator's gates. A LIVE mailing_campaign_queue write must never happen in
// `off` or `shadow` — shadow's whole contract is that it observes — and the
// operator must be able to stop it with a task restart.
func TestRule4_ExpiryGatesOnModeAndTheKillSwitch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       Mode
		killSwitch string
		wantSwept  bool
	}{
		{"off never mutates", ModeOff, "", false},
		{"shadow never mutates", ModeShadow, "", false},
		{"on sweeps", ModeOn, "", true},
		{"canary sweeps", ModeCanary, "", true},
		{"kill switch stops on", ModeOn, "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.killSwitch != "" {
				t.Setenv(WindowExpiryDisabledEnv, tc.killSwitch)
			}
			db := newTestDB(t)
			ctx := context.Background()
			day := testDay(t)
			if _, err := db.Exec(mcqDDL()); err != nil {
				t.Fatalf("mcq ddl: %v", err)
			}
			dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 1000})
			lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 500})
			set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})

			camp := uuid.New()
			seedLedgerCampaign(t, db, day, dc.SendingDomain, camp)
			id := seedQueueRow(t, db, camp, "failed_retryable")

			// 21:00 Denver: the 01:00-20:00 window has CLOSED.
			now := day.Add(21 * time.Hour)
			med := NewMediator(db, NewService(db), MediatorConfig{
				Mode: tc.mode, AlertsDisabled: true, Clock: func() time.Time { return now },
			})
			med.tr = NewTransitions()
			med.expireClosedWindows(ctx, day, now, set)

			got := readQueueRow(t, db, id)
			swept := got.status == ExpiredAtWindowStatus
			if swept != tc.wantSwept {
				t.Fatalf("swept=%t (status %q), want swept=%t", swept, got.status, tc.wantSwept)
			}
		})
	}
}

// The window has to actually be CLOSED. Sweeping inside the window would
// terminate mail the day is still legitimately retrying — a self-inflicted
// under-delivery, which is the same broken promise from the other side.
func TestRule4_OpenWindowIsNeverSwept(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	if _, err := db.Exec(mcqDDL()); err != nil {
		t.Fatalf("mcq ddl: %v", err)
	}
	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 1000})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 500})
	set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})
	camp := uuid.New()
	seedLedgerCampaign(t, db, day, dc.SendingDomain, camp)
	id := seedQueueRow(t, db, camp, "failed_retryable")

	for _, hour := range []int{0, 1, 10, 19} { // 00:00 is BEFORE the window opens
		now := day.Add(time.Duration(hour) * time.Hour)
		med := NewMediator(db, NewService(db), MediatorConfig{
			Mode: ModeOn, AlertsDisabled: true, Clock: func() time.Time { return now },
		})
		med.tr = NewTransitions()
		med.expireClosedWindows(ctx, day, now, set)
		if got := readQueueRow(t, db, id); got.status != "failed_retryable" {
			t.Fatalf("hour %d: row was expired at %q while the window was not closed", hour, got.status)
		}
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func seedLedgerCampaign(t *testing.T, db *sql.DB, day time.Time, domain string, campaign uuid.UUID) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO drip_capacity_ledger
			(allocation_id, idempotency_key, day, tick, sending_domain, isp, lane, touch_class,
			 domain_contract_version, dispatch_contract_version, requested, reserved, committed,
			 released, status, campaign_id, binding_reason, domain_balance_after, lane_unfilled_after)
		VALUES ($1, $2, $3::date, NOW(), $4, 'aol', 'wcl_remail', 'intro', 1, 1, 100, 100, 100, 0,
		        'committed', $5, 'requested', 0, 0)
	`, uuid.New(), uuid.New().String(), dayKey(day), domain, campaign); err != nil {
		t.Fatalf("seed ledger campaign: %v", err)
	}
}

func seedQueueRow(t *testing.T, db *sql.DB, campaign uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO mailing_campaign_queue (id, campaign_id, subscriber_id, status, next_attempt_at)
		VALUES ($1, $2, $3, $4, NOW() + interval '6 hours')
	`, id, campaign, uuid.New(), status); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	return id
}

type queueRow struct {
	status      string
	errMessage  string
	nextAttempt sql.NullTime
}

func readQueueRow(t *testing.T, db *sql.DB, id uuid.UUID) queueRow {
	t.Helper()
	var r queueRow
	if err := db.QueryRow(`
		SELECT status, COALESCE(error_message, ''), next_attempt_at
		FROM mailing_campaign_queue WHERE id = $1
	`, id).Scan(&r.status, &r.errMessage, &r.nextAttempt); err != nil {
		t.Fatalf("read queue row %s: %v", id, err)
	}
	return r
}

// -----------------------------------------------------------------------------
// RULE 5 (leak 1) — a contracted ZERO is a prohibition and binds estate-wide
// -----------------------------------------------------------------------------

// WP-D measured the leak on 2026-09-08: em.consumerpro.net×microsoft promised 0
// and DELIVERED 6,823; yourinsurancehub 0 → 4,501; ratesbazar 0 → 2,656. The
// cause is scope, not arithmetic — under MODE=canary only the canary's cells
// reserve, and every other lane runs the old cap chain, which has never read
// drip_domain_contracts.
//
// A contracted zero is the one term that can be honoured outside that scope
// safely, because it moves volume in exactly one direction.
func TestRule5_ContractedZeroBindsOutsideTheCanaryScope(t *testing.T) {
	db := newExecutorTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	now := day.Add(10 * time.Hour)

	// microsoft is contracted at ZERO; aol is funded.
	dc := domainContract("em.consumerpro.net", 1, map[string]int{"aol": 5000, "microsoft": 0})
	lc := dispatchContract("consumer", 1, map[string]int{"aol": 4000, "microsoft": 4000})
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})

	// MODE=canary pinned to a DIFFERENT lane — exactly rev 1108's
	// DRIP_SUPPLY_CANARY=*:*:yahoo_family. This wave is out of scope and its
	// caps come from the old chain.
	med := NewMediator(db, NewService(db, WithClock(func() time.Time { return now })), MediatorConfig{
		Mode:           ModeCanary,
		Canary:         []CanaryCell{{Domain: "*", ISP: "*", Lane: "yahoo_family"}},
		Clock:          func() time.Time { return now },
		AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	med.TickStart(ctx, now)

	alloc, err := med.Grant(ctx, GrantReq{
		Day: day, Lane: "consumer", Brand: "cp", Domain: dc.SendingDomain,
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "w1",
		ISPs: []string{"aol", "microsoft"}, Requested: 1000,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if alloc.EnforcedCaps() != nil {
		t.Fatal("fixture: this cell must NOT be in the canary's enforcement scope, or the test proves nothing")
	}
	zeros := alloc.ZeroCapISPs()
	if len(zeros) != 1 || zeros[0] != "microsoft" {
		t.Fatalf("ZeroCapISPs = %v, want [microsoft] — a contracted zero must be reported even on an unenforced cell", zeros)
	}
}

// The gate is inert when nothing is enforced: `off` and `shadow` stay
// byte-identical on the send path, which is shadow mode's whole contract.
func TestRule5_ContractedZeroGateIsInertWhenNothingIsEnforced(t *testing.T) {
	for _, tc := range []struct {
		mode       Mode
		killSwitch string
		wantZeros  int
	}{
		{ModeOff, "", 0},
		{ModeShadow, "", 0},
		{ModeCanary, "", 1},
		{ModeOn, "", 1},
		{ModeOn, "1", 0},
	} {
		t.Run(string(tc.mode)+"/"+tc.killSwitch, func(t *testing.T) {
			if tc.killSwitch != "" {
				t.Setenv(ZeroCapGateDisabledEnv, tc.killSwitch)
			}
			db := newExecutorTestDB(t)
			ctx := context.Background()
			day := testDay(t)
			now := day.Add(10 * time.Hour)
			dc := domainContract("em.consumerpro.net", 1, map[string]int{"aol": 5000, "microsoft": 0})
			lc := dispatchContract("consumer", 1, map[string]int{"aol": 4000, "microsoft": 4000})
			set := activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})
			if _, err := EnsureDayBalances(ctx, db, day, set); err != nil {
				t.Fatalf("EnsureDayBalances: %v", err)
			}
			med := NewMediator(db, NewService(db, WithClock(func() time.Time { return now })), MediatorConfig{
				Mode: tc.mode, Clock: func() time.Time { return now }, AlertsDisabled: true,
				ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
			})
			med.TickStart(ctx, now)
			alloc, err := med.Grant(ctx, GrantReq{
				Day: day, Lane: "consumer", Brand: "cp", Domain: dc.SendingDomain,
				TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "w1",
				ISPs: []string{"aol", "microsoft"}, Requested: 1000,
			})
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if got := len(alloc.ZeroCapISPs()); got != tc.wantZeros {
				t.Errorf("mode=%s kill=%q: %d zero-cap ISPs, want %d", tc.mode, tc.killSwitch, got, tc.wantZeros)
			}
		})
	}
}

// An ISP the domain contract does NOT mention is not a prohibition. Treating
// absence as zero would silently dark any ISP a contract has not been updated
// for yet — the same over-reach the rule-1 negative control guards.
func TestRule5_UnmentionedISPIsNotAContractedZero(t *testing.T) {
	dc := domainContract("em.consumerpro.net", 1, map[string]int{"aol": 5000, "microsoft": 0})
	cases := map[string]bool{"microsoft": true, "Microsoft": true, "aol": false, "yahoo": false, "": false}
	for isp, want := range cases {
		if got := domainContractZero(dc, isp); got != want {
			t.Errorf("domainContractZero(%q) = %t, want %t", isp, got, want)
		}
	}
	if domainContractZero(nil, "gmail") {
		t.Error("a nil contract must not report a prohibition")
	}
}

// The binding reason for a domain contracted at zero is `zero_contracted`, not
// `domain_tokens`. With contracted = effective = tokens = 0 the raw arithmetic
// reports pacing for what is a prohibition, and WP-D's verdict keys on
// binding_reason.
func TestRule5_ContractedZeroIsNamedNotReportedAsPacing(t *testing.T) {
	d := nameContractedZero(
		Decision{Granted: 0, BindingReason: ReasonDomainTokens},
		Balance{Contracted: 0, Effective: 0},
		LaneBalance{Desired: 4000, Unfilled: 4000})
	if d.BindingReason != ReasonZeroContracted {
		t.Errorf("reason = %q, want %q", d.BindingReason, ReasonZeroContracted)
	}
	// A governor at 0 on a cell the CONTRACT already holds at 0: the contract is
	// the story, not the governor.
	d = nameContractedZero(
		Decision{Granted: 0, BindingReason: ReasonGovernor + ":gmail_hold"},
		Balance{Contracted: 0, Effective: 0},
		LaneBalance{Desired: 4000, Unfilled: 4000})
	if d.BindingReason != ReasonZeroContracted {
		t.Errorf("reason = %q, want %q", d.BindingReason, ReasonZeroContracted)
	}
	// NEGATIVE CONTROL: a FUNDED domain whose bucket is merely spent still
	// reports pacing. Renaming that would delete the pacing signal.
	d = nameContractedZero(
		Decision{Granted: 0, BindingReason: ReasonDomainTokens},
		Balance{Contracted: 5000, Effective: 5000, Tokens: 0},
		LaneBalance{Desired: 4000, Unfilled: 4000})
	if d.BindingReason != ReasonDomainTokens {
		t.Errorf("reason = %q, want %q — a funded domain out of tokens is pacing", d.BindingReason, ReasonDomainTokens)
	}
	// And a positive grant is never renamed.
	d = nameContractedZero(
		Decision{Granted: 500, BindingReason: ReasonRequested},
		Balance{Contracted: 0}, LaneBalance{Desired: 0})
	if d.BindingReason != ReasonRequested {
		t.Errorf("a positive grant was renamed to %q", d.BindingReason)
	}
}

// -----------------------------------------------------------------------------
// The UTC/Denver day seam (WP-C, measured 2026-09-09)
// -----------------------------------------------------------------------------

// Two writers, two clocks, one column. The planner keys on the DENVER day; the
// mediator keyed on dayOf(time.Now()), and the task definition sets no TZ, so
// the server's clock is UTC. In the 00:00-07:00Z band those are different dates:
// drip_capacity_ledger_shadow held 177,951 UTC-keyed rows and 0 Denver-keyed
// ones, while the live ledger held 21,895 UTC-keyed and 301 Denver-keyed.
//
// reserved + committed + unfilled == desired cannot hold across that seam, so it
// is an enforcement bug and not a reporting one.
func TestDaySeam_ATickAt0005ZKeysToThePreviousDenverDay(t *testing.T) {
	db := newExecutorTestDB(t)
	ctx := context.Background()
	den := testLoc(t)
	if den.String() != "America/Denver" {
		t.Skip("America/Denver tzdata unavailable")
	}

	// 2026-09-11 00:05 UTC == 2026-09-10 18:05 Denver. The send-day is the 10th.
	nowUTC := time.Date(2026, 9, 11, 0, 5, 0, 0, time.UTC)
	wantDay := time.Date(2026, 9, 10, 0, 0, 0, 0, den)

	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": 5000})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": 4000})
	set := activeSet(wantDay, []*DomainContract{dc}, []*DispatchContract{lc})

	med := NewMediator(db, NewService(db, WithClock(func() time.Time { return nowUTC })), MediatorConfig{
		Mode: ModeOn, Clock: func() time.Time { return nowUTC }, AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	med.TickStart(ctx, nowUTC)

	med.mu.Lock()
	gotDay := med.day
	med.mu.Unlock()
	if dayKey(gotDay) != dayKey(wantDay) {
		t.Fatalf("tick at %s keyed day %s, want the Denver send-day %s",
			nowUTC.Format(time.RFC3339), dayKey(gotDay), dayKey(wantDay))
	}

	// And the rows actually land there: the balance the tick seeded, and the
	// ledger row a grant writes, must both carry the Denver date.
	if l := readLane(t, db, wantDay, "wcl_remail", "aol"); l.Desired != 4000 {
		t.Fatalf("lane balance on the Denver day = %+v, want desired=4000", l)
	}
	alloc, err := med.Grant(ctx, GrantReq{
		Lane: "wcl_remail", Brand: "ht", Domain: dc.SendingDomain,
		TouchClass: TouchClassIntro, Pass: PassWelcome, WaveKey: "w1",
		ISPs: []string{"aol"}, Requested: 100,
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if alloc.AllocationID() == uuid.Nil {
		t.Fatalf("nothing was granted (reason %q) — the 18:05 Denver tick is inside the 01:00-20:00 window", alloc.Reason)
	}
	var ledgerDay string
	if err := db.QueryRow(`SELECT day::text FROM drip_capacity_ledger WHERE allocation_id = $1`,
		alloc.AllocationID()).Scan(&ledgerDay); err != nil {
		t.Fatal(err)
	}
	if ledgerDay != dayKey(wantDay) {
		t.Errorf("ledger row day = %s, want %s — this is the column the planner and the mediator must agree on", ledgerDay, dayKey(wantDay))
	}
}

// The kill switch restores the UTC anchoring exactly, because the fix also moves
// the contract's active window from 01:00-20:00Z to 01:00-20:00 Denver — a
// seven-hour shift in when the estate mails, and the operator owns that cutover.
func TestDaySeam_KillSwitchRestoresTheUTCAnchor(t *testing.T) {
	t.Setenv(UTCDayKeyEnv, "1")
	db := newExecutorTestDB(t)
	nowUTC := time.Date(2026, 9, 11, 0, 5, 0, 0, time.UTC)
	med := NewMediator(db, NewService(db), MediatorConfig{
		Mode: ModeOff, Clock: func() time.Time { return nowUTC }, AlertsDisabled: true,
	})
	if got := dayKey(med.denverDay(nowUTC)); got != "2026-09-11" {
		t.Errorf("with the switch armed the day key is %s, want the UTC 2026-09-11", got)
	}
}

// A tick INSIDE the Denver day (mid-afternoon, where UTC and Denver agree on the
// date) is unaffected. The fix must move the seam, not the other 17 hours.
func TestDaySeam_MidDayTicksAreUnchanged(t *testing.T) {
	den := testLoc(t)
	if den.String() != "America/Denver" {
		t.Skip("America/Denver tzdata unavailable")
	}
	db := newExecutorTestDB(t)
	nowUTC := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC) // 12:00 Denver
	med := NewMediator(db, NewService(db), MediatorConfig{
		Mode: ModeOff, Clock: func() time.Time { return nowUTC }, AlertsDisabled: true,
	})
	if got := dayKey(med.denverDay(nowUTC)); got != "2026-09-10" {
		t.Errorf("mid-day tick keyed %s, want 2026-09-10", got)
	}
}

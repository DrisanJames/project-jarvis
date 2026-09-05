package dripsupply

// dark_alert_test.go — regression guard for the 2026-09-05 outage:
// DRIP_SUPPLY_CHAIN_MODE=on, no active dispatch contract, every wave skipped
// `no_contract`, 11h42m of zero drip mail, and NOTHING said so. One lane
// (internal_auto_insurance_v12) was dark for an hour before a human noticed.
//
// The sqlmock tests own the control flow: which mode alerts, which reasons
// count, whether the supply probe gates it, and whether it dedupes. That is
// what the defect was — sqlmock, not row arithmetic, is the right instrument
// (same reasoning as activation_gate_test.go).
//
// TestDarkAlertSupplyProbeSQLAgainstRealPostgres is the one that needs real PG:
// sqlmock cannot tell whether the probe SQL parses, whether `vertical` is the
// lane column, or whether the emergency-paused exclusion actually excludes.

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	_ "github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/notify"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------

// captureNotifier records every delivered alert. The package had no test
// notifier before this file — every other test sets AlertsDisabled:true, which
// is exactly why an alert that never fires could not have been caught.
type captureNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureNotifier) Notify(title, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, title+"\n"+body)
	return nil
}
func (c *captureNotifier) Name() string { return "capture" }

func (c *captureNotifier) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func (c *captureNotifier) matching(sub string) []string {
	var out []string
	for _, m := range c.all() {
		if strings.Contains(m, sub) {
			out = append(out, m)
		}
	}
	return out
}

// darkFixture is a Mediator over sqlmock with alerts ARMED.
type darkFixture struct {
	med  *Mediator
	mock sqlmock.Sqlmock
	note *captureNotifier
	now  *time.Time
}

func newDarkFixture(t *testing.T, mode Mode) (*darkFixture, func()) {
	t.Helper()
	// The tick-outcome upsert and the supply probe are matched by fragment, in
	// any order: this file asserts on ALERTS, not on statement sequencing.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	mock.MatchExpectationsInOrder(false)
	now := time.Date(2026, 9, 5, 4, 0, 0, 0, time.UTC)
	note := &captureNotifier{}
	med := NewMediator(db, nil, MediatorConfig{
		Mode:       mode,
		Notifier:   note,
		AlertEvery: time.Hour,
		Clock:      func() time.Time { return now },
		// Keep every other alert path out of the way: with no ContractSource
		// and no Service, Grant is never called here — Outcome is.
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return nil, nil },
	})
	med.TickStart(context.Background(), now)
	return &darkFixture{med: med, mock: mock, note: note, now: &now}, func() { db.Close() }
}

const outcomeUpsertFragment = `INSERT INTO drip_tick_outcomes`

// expectOutcome queues one tick-outcome upsert.
func (f *darkFixture) expectOutcome() {
	f.mock.ExpectExec(outcomeUpsertFragment).WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectSupplyProbe queues one supply probe returning `n` (capped at the
// probe's LIMIT, so callers only ever mean 0 or 1).
func (f *darkFixture) expectSupplyProbe(lane string, n int) {
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM partner_clean_queue`)).
		WithArgs(lane, darkSupplyProbeLimit).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
}

// tick writes one outcome row for (lane, pass).
func (f *darkFixture) tick(lane, pass, outcome, reason, brand string) {
	f.med.Outcome(context.Background(), OutcomeRow{
		Lane: lane, Pass: pass, Outcome: outcome, Reason: reason, Brand: brand,
	})
}

// advance moves the fixture clock, which is what re-arms alertOnce.
func (f *darkFixture) advance(d time.Duration) { *f.now = f.now.Add(d) }

const darkHeadline = "DARK under contract enforcement"

// -----------------------------------------------------------------------------
// 1. It FIRES: enforcing + supply exists + the contract denied the wave
// -----------------------------------------------------------------------------

// THE incident, replayed. mode=on, no dispatch contract, `skipped/no_contract`
// on consecutive ticks, and partner_clean_queue holds claimable records.
func TestDarkAlertFiresWhenEnforcingAndSupplyExists(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "internal_auto_insurance_v12"

	// Tick 1: streak 1 — below the threshold, so NO probe and NO alert.
	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")
	if got := f.note.matching(darkHeadline); len(got) != 0 {
		t.Fatalf("alerted on the FIRST denied tick: %v", got)
	}

	// Tick 2: streak 2 — probe finds supply, alert fires.
	f.expectOutcome()
	f.expectSupplyProbe(lane, 1)
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")

	got := f.note.matching(darkHeadline)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 dark alert at streak %d, got %d: %v", ContractDarkStreak, len(got), got)
	}
	for _, want := range []string{lane, SkipNoContract, "Mode: on", "record(s) the claim could have taken"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("alert body missing %q:\n%s", want, got[0])
		}
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// The pre-fix behaviour, pinned so it cannot come back: `skipped` hits
// trackStreak's `default:` branch and DELETES zeroStreak, so the old alert
// could never reach its threshold no matter how long the lane stayed dark.
// darkStreak must be counting where zeroStreak is being reset.
func TestSkippedOutcomeStillResetsZeroStreakButNotDarkStreak(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "regression_lane"

	for i := 0; i < 4; i++ {
		f.expectOutcome()
	}
	f.expectSupplyProbe(lane, 1)
	for i := 0; i < 4; i++ {
		f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")
	}

	f.med.mu.Lock()
	zero := f.med.zeroStreak[lane+"|"+PassWelcome]
	dark := f.med.darkStreak[lane+"|"+PassWelcome]
	f.med.mu.Unlock()

	if zero != 0 {
		t.Errorf("zeroStreak should still be reset by `skipped` (unchanged behaviour), got %d", zero)
	}
	if dark != 4 {
		t.Errorf("darkStreak want 4 consecutive denials, got %d", dark)
	}
	if len(f.note.matching(darkHeadline)) != 1 {
		t.Errorf("want 1 dark alert across 4 denied ticks, got %d", len(f.note.matching(darkHeadline)))
	}
}

// canary enforces too, so it must alert too. The reason the outcome carries is
// itself the proof the cell was enforced: failClosed only sets Skip (and the
// orchestrator only writes `skipped/no_contract`) for a matched canary cell.
func TestDarkAlertFiresInCanaryMode(t *testing.T) {
	f, done := newDarkFixture(t, ModeCanary)
	defer done()
	const lane = "canary_lane"

	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
	f.expectOutcome()
	f.expectSupplyProbe(lane, 1)
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")

	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("canary must alert; got %d alerts", n)
	}
}

// no_contract_key / no_balance / no_lane_balance are the same class of failure:
// the contract or balance state denied the wave before any record was seen.
func TestDarkAlertFiresForEveryContractDenialReason(t *testing.T) {
	for _, reason := range []string{SkipNoContract, SkipNoContractKey, ReasonNoBalance, ReasonNoLaneBalance} {
		t.Run(reason, func(t *testing.T) {
			f, done := newDarkFixture(t, ModeOn)
			defer done()
			lane := "lane_" + reason

			f.expectOutcome()
			f.tick(lane, PassWelcome, OutcomeSkipped, reason, "ht")
			f.expectOutcome()
			f.expectSupplyProbe(lane, 1)
			f.tick(lane, PassWelcome, OutcomeSkipped, reason, "ht")

			if n := len(f.note.matching(darkHeadline)); n != 1 {
				t.Fatalf("reason %q must alert; got %d", reason, n)
			}
		})
	}
}

// A zero outcome carrying a contract-denial reason is just as dark as a skip.
func TestDarkAlertCountsZeroOutcomesToo(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "zero_reason_lane"

	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeZero, ReasonNoLaneBalance, "")
	f.expectOutcome()
	f.expectSupplyProbe(lane, 1)
	f.tick(lane, PassWelcome, OutcomeZero, ReasonNoLaneBalance, "")

	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("want 1 dark alert, got %d", n)
	}
}

// The follow-up pass has a different supply shape (due next_touch_at, not
// status='ready'), so it must probe the follow-up SQL.
func TestDarkAlertUsesFollowupProbeForFollowupPass(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "followup_lane"

	f.expectOutcome()
	f.tick(lane, PassFollowup, OutcomeSkipped, SkipNoContract, "")
	f.expectOutcome()
	f.mock.ExpectQuery(regexp.QuoteMeta(`next_touch_at <= NOW()`)).
		WithArgs(lane, darkSupplyProbeLimit).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	f.tick(lane, PassFollowup, OutcomeSkipped, SkipNoContract, "")

	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("want 1 dark alert on the follow-up pass, got %d", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the follow-up probe SQL was not the one issued: %v", err)
	}
}

// A probe that fails is NOT evidence of empty supply. Fail OPEN: the
// conjunction that got here is already strong enough to page on.
func TestDarkAlertFailsOpenWhenSupplyProbeErrors(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "probe_error_lane"

	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
	f.expectOutcome()
	f.mock.ExpectQuery(regexp.QuoteMeta(`FROM partner_clean_queue`)).
		WithArgs(lane, darkSupplyProbeLimit).
		WillReturnError(context.DeadlineExceeded)
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")

	got := f.note.matching(darkHeadline)
	if len(got) != 1 {
		t.Fatalf("must fail OPEN on a probe error; got %d alerts", len(got))
	}
	if !strings.Contains(got[0], "supply probe failed") {
		t.Errorf("body must say the probe failed rather than claim a supply number:\n%s", got[0])
	}
}

// -----------------------------------------------------------------------------
// 2. It does NOT fire in shadow / off
// -----------------------------------------------------------------------------

// Production runs `shadow`. An alert storm there is a regression worse than the
// silence this file removes: in shadow failClosed never sets Skip, the legacy
// chain still ships the wave, and the lane is not dark at all.
func TestDarkAlertInertInShadowAndOff(t *testing.T) {
	for _, mode := range []Mode{ModeShadow, ModeOff} {
		t.Run(string(mode), func(t *testing.T) {
			f, done := newDarkFixture(t, mode)
			defer done()
			const lane = "shadow_lane"

			// Ten consecutive denied ticks. No supply probe is queued: issuing
			// one would be an unmet-expectation failure, which is the point —
			// shadow must not even READ the database for this.
			for i := 0; i < 10; i++ {
				f.expectOutcome()
				f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")
			}

			if n := len(f.note.all()); n != 0 {
				t.Fatalf("mode %s must be inert, got %d alerts: %v", mode, n, f.note.all())
			}
			f.med.mu.Lock()
			dark := len(f.med.darkStreak)
			f.med.mu.Unlock()
			if dark != 0 {
				t.Errorf("mode %s must not even carry dark streak state, got %d keys", mode, dark)
			}
			if err := f.mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mode %s issued an unexpected statement: %v", mode, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 3. It does NOT fire when the lane genuinely has no supply
// -----------------------------------------------------------------------------

// The benign, already-common case: the contract denied a lane that had nothing
// to send anyway. Alerting here would train the operator to ignore the alert.
func TestDarkAlertSilentWhenLaneHasNoSupply(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "empty_lane"

	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
	f.expectOutcome()
	f.expectSupplyProbe(lane, 0) // nothing claimable
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")

	if n := len(f.note.all()); n != 0 {
		t.Fatalf("empty supply must stay silent, got %d alerts: %v", n, f.note.all())
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// The reasons that are NOT contract state: normal pacing, operator states, and
// genuinely empty supply. None may alert, and none may even reach the probe.
func TestDarkAlertIgnoresNonContractReasons(t *testing.T) {
	benign := []string{
		SkipPaused, SkipBudgetExhausted, SkipOutsideWindow, SkipNoWaveSize,
		SkipNoPositiveGrant, SkipReserveTimeout, ZeroNoRecordsClaimed, ZeroAllDeferred, "",
	}
	for _, reason := range benign {
		name := reason
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			f, done := newDarkFixture(t, ModeOn)
			defer done()
			lane := "benign_" + name

			for i := 0; i < 5; i++ {
				f.expectOutcome()
				f.tick(lane, PassWelcome, OutcomeSkipped, reason, "db")
			}
			if n := len(f.note.matching(darkHeadline)); n != 0 {
				t.Fatalf("reason %q must not raise a dark alert, got %d", reason, n)
			}
			if err := f.mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("reason %q issued a supply probe it should not have: %v", reason, err)
			}
		})
	}
}

// "Consecutive" must mean consecutive: a benign skip or a fired wave in the
// middle resets the streak.
func TestDarkStreakResetsOnAnyNonDenialOutcome(t *testing.T) {
	for _, mid := range []struct {
		name, outcome, reason string
	}{
		{"fired", OutcomeFired, ""},
		{"benign_skip", OutcomeSkipped, SkipPaused},
		{"zero_no_records", OutcomeZero, ZeroNoRecordsClaimed},
	} {
		t.Run(mid.name, func(t *testing.T) {
			f, done := newDarkFixture(t, ModeOn)
			defer done()
			lane := "reset_" + mid.name

			for i := 0; i < 3; i++ {
				f.expectOutcome()
			}
			f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
			f.tick(lane, PassWelcome, mid.outcome, mid.reason, "")
			f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")

			if n := len(f.note.matching(darkHeadline)); n != 0 {
				t.Fatalf("%s in the middle must reset the streak, got %d alerts", mid.name, n)
			}
			f.med.mu.Lock()
			dark := f.med.darkStreak[lane+"|"+PassWelcome]
			f.med.mu.Unlock()
			if dark != 1 {
				t.Errorf("want darkStreak 1 after the reset, got %d", dark)
			}
		})
	}
}

// The streak is per lane×pass: two passes of the same lane each denied ONCE is
// not a two-tick streak on either.
func TestDarkStreakIsPerLanePass(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "two_pass_lane"

	f.expectOutcome()
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
	f.expectOutcome()
	f.tick(lane, PassFollowup, OutcomeSkipped, SkipNoContract, "")

	if n := len(f.note.all()); n != 0 {
		t.Fatalf("one denial on each of two passes is not a streak, got %d alerts", n)
	}
}

// -----------------------------------------------------------------------------
// 4. Dedupe across ticks — one alert per lane per window
// -----------------------------------------------------------------------------

// The orchestrator ticks every few minutes. A dark lane must page ONCE per
// AlertEvery window, and must not pay for a supply probe on every tick in
// between (the ExpectationsWereMet check is what proves the second half).
func TestDarkAlertDedupesAcrossTicksAndReArmsAfterTheWindow(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "dedupe_lane"

	// 20 denied ticks inside one hour: exactly ONE probe, exactly ONE alert.
	for i := 0; i < 20; i++ {
		f.expectOutcome()
	}
	f.expectSupplyProbe(lane, 1)
	for i := 0; i < 20; i++ {
		f.advance(time.Minute)
		f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")
	}
	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("want 1 alert across 20 ticks in the window, got %d", n)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("more than one supply probe was issued inside the window: %v", err)
	}

	// Past the window it re-arms: the lane is STILL dark and still deserves a page.
	f.advance(time.Hour + time.Minute)
	f.expectOutcome()
	f.expectSupplyProbe(lane, 1)
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "db")
	if n := len(f.note.matching(darkHeadline)); n != 2 {
		t.Fatalf("want a 2nd alert after the window re-armed, got %d", n)
	}
}

// The dark alert must not share alertOnce's key with trackStreak's zero-claim
// WARN: a benign zero streak firing first would otherwise swallow the dark
// ALERT for a whole hour.
func TestDarkAlertKeyIsDistinctFromZeroStreakKey(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	const lane = "key_collision_lane"

	// Two zero-claim ticks first: that arms trackStreak's "lane:<lane>" key.
	for i := 0; i < 2; i++ {
		f.expectOutcome()
		f.tick(lane, PassWelcome, OutcomeZero, ZeroNoRecordsClaimed, "")
	}
	if n := len(f.note.matching("produced nothing")); n != 1 {
		t.Fatalf("setup: want the existing zero-streak WARN, got %d", n)
	}

	// Now the lane goes contract-dark inside the SAME window.
	for i := 0; i < 2; i++ {
		f.expectOutcome()
	}
	f.expectSupplyProbe(lane, 1)
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")
	f.tick(lane, PassWelcome, OutcomeSkipped, SkipNoContract, "")

	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("the zero-streak WARN swallowed the dark ALERT: got %d dark alerts", n)
	}
}

// -----------------------------------------------------------------------------
// Kill switches
// -----------------------------------------------------------------------------

func TestDarkAlertKillSwitches(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv(DarkAlertDisabledEnv, "1")
		f, done := newDarkFixture(t, ModeOn)
		defer done()
		for i := 0; i < 5; i++ {
			f.expectOutcome()
			f.tick("killed_lane", PassWelcome, OutcomeSkipped, SkipNoContract, "")
		}
		if n := len(f.note.all()); n != 0 {
			t.Fatalf("%s=1 must silence it, got %d alerts", DarkAlertDisabledEnv, n)
		}
		if err := f.mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("a silenced alert must cost zero database reads: %v", err)
		}
	})

	t.Run("AlertsDisabled", func(t *testing.T) {
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.MatchExpectationsInOrder(false)
		note := &captureNotifier{}
		med := NewMediator(db, nil, MediatorConfig{
			Mode: ModeOn, Notifier: note, AlertsDisabled: true,
		})
		for i := 0; i < 5; i++ {
			mock.ExpectExec(outcomeUpsertFragment).WillReturnResult(sqlmock.NewResult(0, 1))
			med.Outcome(context.Background(), OutcomeRow{
				Lane: "l", Pass: PassWelcome, Outcome: OutcomeSkipped, Reason: SkipNoContract,
			})
		}
		if n := len(note.all()); n != 0 {
			t.Fatalf("AlertsDisabled must silence it, got %d", n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("a silenced alert must cost zero database reads: %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Classifier
// -----------------------------------------------------------------------------

// Outcome folds the brand into the reason ("no_contract brand=db"), so the
// classifier has to tokenise. A whole-string compare would have missed every
// real outcome row the orchestrator writes.
func TestIsContractDenialReasonTokenisesBrandSuffix(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{SkipNoContract, true},
		{SkipNoContract + " brand=db", true},
		{"brand=internal_auto " + ReasonNoLaneBalance, true},
		{SkipNoContractKey + " brand=ht", true},
		{ReasonNoBalance, true},
		{SkipPaused + " brand=db", false},
		{SkipBudgetExhausted, false},
		{SkipNoPositiveGrant, false},
		{ZeroNoRecordsClaimed, false},
		{"", false},
		{"brand=db", false},
		// Substring, not a token: must NOT match.
		{"no_contract_pending", false},
		{"xno_contract", false},
	}
	for _, c := range cases {
		if got := isContractDenialReason(c.reason); got != c.want {
			t.Errorf("isContractDenialReason(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}

func TestDarkAlertNilMediatorIsSafe(t *testing.T) {
	var m *Mediator
	m.trackContractDark(context.Background(), "l", PassWelcome, OutcomeSkipped, SkipNoContract)
	if m.alertDue("k") {
		t.Error("a nil mediator must never report an alert as due")
	}
	if _, err := m.laneClaimableSupply(context.Background(), "l", PassWelcome); err == nil {
		t.Error("a nil mediator must error rather than pretend supply is zero")
	}
}

// -----------------------------------------------------------------------------
// Real Postgres: the probe SQL itself
// -----------------------------------------------------------------------------

// pcqProbeSchemaDDL is the slice of partner_clean_queue / partner_datasets the
// probes touch. Column names and types match the production tables the claim
// paths use (transition.go:308, partner_drip_orchestrator.go:5390); a rename
// there must break this test rather than silently return 0 forever.
func pcqProbeSchemaDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS partner_datasets (
			id              UUID PRIMARY KEY,
			paused_emergency BOOLEAN NOT NULL DEFAULT FALSE
		)`,
		`CREATE TABLE IF NOT EXISTS partner_clean_queue (
			id              BIGSERIAL PRIMARY KEY,
			dataset_id      UUID,
			vertical        TEXT NOT NULL,
			status          TEXT NOT NULL,
			isp_family      TEXT,
			ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			mailed_at       TIMESTAMPTZ,
			next_touch_at   TIMESTAMPTZ,
			touch_count     INT NOT NULL DEFAULT 0,
			terminal_reason TEXT
		)`,
	}
}

// The one thing sqlmock cannot check: that the probe SQL parses, that
// `vertical` is really the lane column, and that the emergency-paused
// exclusion excludes.
func TestDarkAlertSupplyProbeSQLAgainstRealPostgres(t *testing.T) {
	db := newTestDB(t)
	for _, stmt := range pcqProbeSchemaDDL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("pcq ddl: %v", err)
		}
	}
	med := NewMediator(db, nil, MediatorConfig{Mode: ModeOn, AlertsDisabled: true})
	ctx := context.Background()

	live := "11111111-1111-1111-1111-111111111111"
	paused := "22222222-2222-2222-2222-222222222222"
	if _, err := db.Exec(`INSERT INTO partner_datasets (id, paused_emergency) VALUES ($1,false),($2,true)`,
		live, paused); err != nil {
		t.Fatalf("seed datasets: %v", err)
	}

	// Empty lane -> 0.
	if n, err := med.laneClaimableSupply(ctx, "lane_a", PassWelcome); err != nil || n != 0 {
		t.Fatalf("empty lane: n=%d err=%v", n, err)
	}

	// A ready row on the lane -> the probe sees it.
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (dataset_id, vertical, status) VALUES ($1,'lane_a','ready')`, live); err != nil {
		t.Fatalf("seed ready: %v", err)
	}
	if n, err := med.laneClaimableSupply(ctx, "lane_a", PassWelcome); err != nil || n != 1 {
		t.Fatalf("ready row: n=%d err=%v (want 1)", n, err)
	}
	// ...and only on ITS lane.
	if n, err := med.laneClaimableSupply(ctx, "lane_b", PassWelcome); err != nil || n != 0 {
		t.Fatalf("lane scoping broken: lane_b n=%d err=%v", n, err)
	}

	// An emergency-paused dataset is NOT claimable supply — this is the false
	// alert the exclusion prevents.
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (dataset_id, vertical, status) VALUES ($1,'lane_paused','ready')`, paused); err != nil {
		t.Fatalf("seed paused: %v", err)
	}
	if n, err := med.laneClaimableSupply(ctx, "lane_paused", PassWelcome); err != nil || n != 0 {
		t.Fatalf("emergency-paused dataset must not count as supply: n=%d err=%v", n, err)
	}

	// Follow-up shape: a due, non-terminal mailed row counts; a future one does not.
	if _, err := db.Exec(`
		INSERT INTO partner_clean_queue (dataset_id, vertical, status, next_touch_at)
		VALUES ($1,'lane_fu','mailed', NOW() - INTERVAL '1 hour')`, live); err != nil {
		t.Fatalf("seed followup due: %v", err)
	}
	if n, err := med.laneClaimableSupply(ctx, "lane_fu", PassFollowup); err != nil || n != 1 {
		t.Fatalf("due follow-up: n=%d err=%v (want 1)", n, err)
	}
	if _, err := db.Exec(`
		INSERT INTO partner_clean_queue (dataset_id, vertical, status, next_touch_at)
		VALUES ($1,'lane_future','mailed', NOW() + INTERVAL '6 hours')`, live); err != nil {
		t.Fatalf("seed followup future: %v", err)
	}
	if n, err := med.laneClaimableSupply(ctx, "lane_future", PassFollowup); err != nil || n != 0 {
		t.Fatalf("a follow-up not yet due is not supply: n=%d err=%v", n, err)
	}
	// A terminal row is finished, not supply.
	if _, err := db.Exec(`
		INSERT INTO partner_clean_queue (dataset_id, vertical, status, next_touch_at, terminal_reason)
		VALUES ($1,'lane_term','mailed', NOW() - INTERVAL '1 hour', 'clicked_exit')`, live); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	if n, err := med.laneClaimableSupply(ctx, "lane_term", PassFollowup); err != nil || n != 0 {
		t.Fatalf("a terminal row is not supply: n=%d err=%v", n, err)
	}

	// The probe is LIMIT-bounded: many rows still return the cap, never a full count.
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(`INSERT INTO partner_clean_queue (dataset_id, vertical, status) VALUES ($1,'lane_many','ready')`, live); err != nil {
			t.Fatalf("seed many: %v", err)
		}
	}
	n, err := med.laneClaimableSupply(ctx, "lane_many", PassWelcome)
	if err != nil {
		t.Fatalf("many: %v", err)
	}
	if n != darkSupplyProbeLimit {
		t.Fatalf("probe must stop at LIMIT %d, got %d — it is counting the whole table", darkSupplyProbeLimit, n)
	}
}

// A tick-outcome write that never reaches the alert path is still the incident.
// This pins the wiring: the mediator's own Outcome() is what calls it, so any
// caller that writes outcome rows inherits the alert for free.
func TestOutcomeIsWiredToTheDarkAlert(t *testing.T) {
	f, done := newDarkFixture(t, ModeOn)
	defer done()
	if f.med.darkStreak == nil {
		t.Fatal("NewMediator must initialise darkStreak")
	}
	for i := 0; i < ContractDarkStreak; i++ {
		f.expectOutcome()
	}
	f.expectSupplyProbe("wired_lane", 1)
	for i := 0; i < ContractDarkStreak; i++ {
		f.tick("wired_lane", PassGoverned, OutcomeSkipped, SkipNoContract, "db")
	}
	if n := len(f.note.matching(darkHeadline)); n != 1 {
		t.Fatalf("Outcome() must drive the dark alert, got %d", n)
	}
	if got := f.note.matching(string(notify.ScopeDrip)); len(got) == 0 {
		t.Error("the alert must carry the Drip scope the other §6 alerts use")
	}
}

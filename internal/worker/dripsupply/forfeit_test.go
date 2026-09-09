package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// R1 / R4 — the refill path can no longer drop allowance silently
// -----------------------------------------------------------------------------
//
// These run against real Postgres (same harness as reservation_test.go) because
// the claim under test is that a row EXISTS after the tick, and because the
// forfeit row and the balance row it describes must land in ONE transaction.

// errGovernor is a GovernorReader that always fails, which is the only way to
// reach RefillDomain's `continue`.
type errGovernor struct{ err error }

func (g errGovernor) Ceilings(context.Context, time.Time, string, string, Window) ([]GovernorCeiling, error) {
	return nil, g.err
}

func readForfeit(t *testing.T, db *sql.DB, day time.Time, domain, isp string) (forfeited, perInterval float64, burst, active int, found bool) {
	t.Helper()
	err := db.QueryRow(`
		SELECT forfeited, refill_per_interval, burst_intervals, active_intervals
		FROM drip_refill_forfeit
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&forfeited, &perInterval, &burst, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, 0, false
	}
	if err != nil {
		t.Fatalf("read drip_refill_forfeit: %v", err)
	}
	return forfeited, perInterval, burst, active, true
}

// TestRefillDomain_ForfeitIsDurableAndAdditive is R1. RefillResult.Capped was
// set and then discarded — RefillDomain's only caller throws the whole map away
// (executor.go:799) — so a domain could forfeit an interval of allowance on
// every tick of a day with no evidence anywhere.
func TestRefillDomain_ForfeitIsDurableAndAdditive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 5000)
	const domain, isp = "em.historythinking.com", "aol"

	// 7,600 over a 76-interval window = 100/interval, burst 2 = a 200 ceiling.
	// Nine hours of scheduler downtime accrues 36 intervals = 3,600 and keeps 200.
	if _, err := db.Exec(`UPDATE drip_capacity_balance SET tokens = 0`); err != nil {
		t.Fatalf("zero the bucket: %v", err)
	}
	svc := NewService(db, WithClock(func() time.Time { return dayOf(day).Add(10 * time.Hour) }))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("RefillDomain: %v", err)
	}

	forfeited, perInterval, burst, active, found := readForfeit(t, db, day, domain, isp)
	if !found {
		t.Fatal("no drip_refill_forfeit row after a clamped refill — the discarded allowance is still invisible")
	}
	if forfeited != 3_400 {
		t.Fatalf("forfeited = %v, want 3400 (3600 accrued - the 200 burst ceiling)", forfeited)
	}
	if perInterval != 100 || burst != 2 || active != 76 {
		t.Fatalf("shape columns = per_interval %v burst %d active %d, want 100 / 2 / 76 — without them an operator cannot tell an under-paced contract from a dead scheduler", perInterval, burst, active)
	}
	// The bucket really was clamped: the row records a loss that happened.
	if bal := readBalance(t, db, day, domain, isp); bal.Tokens != 200 {
		t.Fatalf("tokens = %v, want the 200 ceiling", bal.Tokens)
	}

	// ADDITIVE: a second clamped tick adds to the day's total rather than
	// overwriting it. A last-writer column would report one interval of loss
	// for a day that lost 76 of them.
	if _, err := db.Exec(`UPDATE drip_capacity_balance SET tokens = 0, last_refill_tick = $1`, dayOf(day).Add(time.Hour)); err != nil {
		t.Fatalf("rewind the bucket: %v", err)
	}
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("second RefillDomain: %v", err)
	}
	again, _, _, _, _ := readForfeit(t, db, day, domain, isp)
	if again != 6_800 {
		t.Fatalf("forfeited after two clamped ticks = %v, want 6800 — the upsert is not additive", again)
	}
}

// TestRefillDomain_NormalPacingWritesNoForfeitRow is the negative control for
// the test above: if a healthy tick also wrote a row, the table would be noise
// and nobody would read it.
func TestRefillDomain_NormalPacingWritesNoForfeitRow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 5000)

	// One interval past the window open, starting from an empty bucket.
	if _, err := db.Exec(`UPDATE drip_capacity_balance SET tokens = 0, last_refill_tick = $1`, dayOf(day).Add(time.Hour)); err != nil {
		t.Fatalf("reset the bucket: %v", err)
	}
	svc := NewService(db, WithClock(func() time.Time { return dayOf(day).Add(time.Hour + 15*time.Minute) }))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("RefillDomain: %v", err)
	}
	if _, _, _, _, found := readForfeit(t, db, day, "em.historythinking.com", "aol"); found {
		t.Fatal("a normally paced refill wrote a forfeit row — the table is recording non-events")
	}
}

// TestRefillDomain_GovernorReadErrorIsRecorded is R4. The `continue` at
// bucket.go:799 skips the ISP's accrual entirely; before this it left one log
// line and nothing an audit could query.
func TestRefillDomain_GovernorReadErrorIsRecorded(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 5000)
	const domain, isp = "em.historythinking.com", "aol"

	if _, err := db.Exec(`UPDATE drip_capacity_balance SET tokens = 0, last_refill_tick = $1`, dayOf(day).Add(time.Hour)); err != nil {
		t.Fatalf("reset the bucket: %v", err)
	}
	svc := NewService(db,
		WithClock(func() time.Time { return dayOf(day).Add(10 * time.Hour) }),
		WithGovernors(errGovernor{err: errors.New("throttle table unreachable")}),
	)
	out, err := svc.RefillDomain(ctx, day, dc)
	if err != nil {
		t.Fatalf("RefillDomain must not fail the domain on a governor read error: %v", err)
	}
	if _, ok := out["aol"]; ok {
		t.Fatal("the skipped ISP produced a RefillResult — it was supposed to be skipped")
	}
	// Fail closed: no accrual happened.
	if bal := readBalance(t, db, day, domain, isp); bal.Tokens != 0 {
		t.Fatalf("tokens = %v after a skipped refill, want 0 (fail closed)", bal.Tokens)
	}

	var (
		skips  int
		reason string
	)
	if err := db.QueryRow(`
		SELECT skips, reason FROM drip_refill_skip
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&skips, &reason); err != nil {
		t.Fatalf("no drip_refill_skip row after a governor read error: %v", err)
	}
	if skips != 1 {
		t.Fatalf("skips = %d, want 1", skips)
	}
	if !strings.HasPrefix(reason, "governor_read: ") || !strings.Contains(reason, "throttle table unreachable") {
		t.Fatalf("reason = %q, want it to NAME the cause", reason)
	}

	// Additive too: a governor that is down all day must read as down all day.
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("second RefillDomain: %v", err)
	}
	if err := db.QueryRow(`
		SELECT skips FROM drip_refill_skip
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&skips); err != nil {
		t.Fatalf("re-read drip_refill_skip: %v", err)
	}
	if skips != 2 {
		t.Fatalf("skips = %d after two failed ticks, want 2", skips)
	}
}

// TestRefillForfeitSchema_IsCreatedOnFirstUse pins that the DDL is applied by
// this package and not by cmd/server/main.go: the scratch schema these tests
// build (schemaDDL()) does not contain either table, so if RefillDomain did not
// create them every assertion above would be an undefined_table error.
func TestRefillForfeitSchema_IsCreatedOnFirstUse(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 7600, 0)

	for _, tbl := range []string{"drip_refill_forfeit", "drip_refill_skip"} {
		if _, err := db.Exec(`SELECT 1 FROM ` + tbl + ` LIMIT 1`); err == nil {
			t.Fatalf("%s already exists before the first refill — this test proves nothing", tbl)
		}
	}
	svc := NewService(db, WithClock(midWindow(day)))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("RefillDomain: %v", err)
	}
	for _, tbl := range []string{"drip_refill_forfeit", "drip_refill_skip"} {
		if _, err := db.Exec(`SELECT 1 FROM ` + tbl + ` LIMIT 1`); err != nil {
			t.Fatalf("%s was not created on first use: %v", tbl, err)
		}
	}
	// Idempotent: a second tick must not fail on the CREATE.
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("second RefillDomain after the schema existed: %v", err)
	}
}

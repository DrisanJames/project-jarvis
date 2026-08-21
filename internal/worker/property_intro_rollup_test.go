package worker

// Step-14 fixtures (Vector A plan rev4): the counter worker's gates and pass
// shape. Permanent fixtures (I-11):
//   - disabled tick = ONE heartbeat with status 'disabled', nothing else.
//   - missing/invalid idx_pcq_intro_rollup = heartbeat 'blocked', NO heavy
//     query, NO promotion.
//   - a passing gate runs: promotion (with today's Denver date) → 3 day runs
//     (today + prior two — the catch-up window) → finalize → heartbeat ok.
//   - Denver day windows are Go-computed UTC bounds; DST days are 23h/25h.
//
// Zero-cell materialization, the promotion boundary, and lease-expiry overlap
// are SQL/concurrency semantics — pinned in the real-PG integration suite
// (property_ledger_p4_integration_test.go).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newRollupWorkerWithMock(t *testing.T) (*PropertyIntroRollupWorker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	w := NewPropertyIntroRollupWorker(db, nil)
	return w, mock
}

func TestPropertyIntroRollupDisabledHeartbeatOnly(t *testing.T) {
	t.Setenv("PROPERTY_INTRO_ROLLUP_DISABLED", "1")
	w, mock := newRollupWorkerWithMock(t)

	// The ONLY statement allowed is the heartbeat upsert with status 'disabled'.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(propertyIntroRollupWorkerName, "disabled", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("disabled tick must be heartbeat-only: %v", err)
	}
}

func TestPropertyIntroRollupBlockedWithoutIndex(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	// Index gate reads indisvalid = false → blocked; nothing else runs.
	mock.ExpectQuery(`SELECT i\.indisvalid FROM pg_index`).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(false))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(propertyIntroRollupWorkerName, "blocked", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The cap-breach detector rides these counters: a blocked counter pass MUST
	// surface as a blocked detector, never as "no breaches found".
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "blocked", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("blocked pass must stop at the gate (no promotion, no heavy query): %v", err)
	}
}

func TestPropertyIntroRollupBlockedWhenIndexAbsent(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)

	// No row at all (index never built) → blocked, same as invalid.
	mock.ExpectQuery(`SELECT i\.indisvalid FROM pg_index`).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(propertyIntroRollupWorkerName, "blocked", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "blocked", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("absent index must block: %v", err)
	}
}

func TestPropertyIntroRollupFullPassShape(t *testing.T) {
	w, mock := newRollupWorkerWithMock(t)
	today := denverDate(time.Now(), w.loc)

	mock.ExpectQuery(`SELECT i\.indisvalid FROM pg_index`).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))

	// Promotion carries TODAY's Denver date (I-2 boundary predicate is <=).
	mock.ExpectExec(`UPDATE partner_drip_brand_budgets\s+SET daily_budget = pending_budget`).
		WithArgs(today.Format("2006-01-02")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Three day runs: today + the prior two Denver days (the catch-up window).
	for i := 0; i <= propertyIntroRollupRecomputeDays; i++ {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		mock.ExpectQuery(`INSERT INTO property_counter_runs`).
			WithArgs(day, 16*14).
			WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("00000000-0000-0000-0000-00000000000" + string(rune('1'+i))))
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`INSERT INTO property_intro_counters`).
			WillReturnResult(sqlmock.NewResult(0, 16*14))
		mock.ExpectCommit()
		mock.ExpectExec(`UPDATE property_counter_runs SET status='completed'`).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// Finalize cutoff = today - 2 (days OLDER than the recompute window).
	mock.ExpectExec(`UPDATE property_intro_counters SET finalized_at = NOW\(\)`).
		WithArgs(today.AddDate(0, 0, -propertyIntroRollupRecomputeDays).Format("2006-01-02")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(propertyIntroRollupWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// (5) The cap-breach detector runs LAST, in the same lease, on the counters
	// this pass just wrote — wired here, not at boot, so it can never be dead
	// code. Empty ledger => it governs nothing and says so.
	mock.ExpectQuery(`FROM partner_drip_brand_budgets b`).
		WillReturnRows(sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold", "lock_version", "introduced", "auto_tripped_today"}))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(capBreachWorkerName, "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("full pass shape: %v", err)
	}
}

func TestDenverDayWindowUTCHandlesDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skipf("America/Denver tz unavailable: %v", err)
	}
	// Ordinary day = 24h.
	start, end := denverDayWindowUTC(time.Date(2026, 8, 15, 0, 0, 0, 0, loc), loc)
	if end.Sub(start) != 24*time.Hour {
		t.Fatalf("ordinary day = %s, want 24h", end.Sub(start))
	}
	// Spring-forward day (2026-03-08) = 23h.
	start, end = denverDayWindowUTC(time.Date(2026, 3, 8, 0, 0, 0, 0, loc), loc)
	if end.Sub(start) != 23*time.Hour {
		t.Fatalf("spring-forward day = %s, want 23h", end.Sub(start))
	}
	// Fall-back day (2026-11-01) = 25h.
	start, end = denverDayWindowUTC(time.Date(2026, 11, 1, 0, 0, 0, 0, loc), loc)
	if end.Sub(start) != 25*time.Hour {
		t.Fatalf("fall-back day = %s, want 25h", end.Sub(start))
	}
}

// TestPropertyIntroRollupGridAuthority pins the grid dimensions: 16 roster
// brands × the 14-value ledger authority — and that the heavy SQL reads
// partner_clean_queue via the normalized keys.
func TestPropertyIntroRollupGridAuthority(t *testing.T) {
	if got := len(DripIntroBrands()); got != 16 {
		t.Fatalf("DripIntroBrands() = %d codes, want 16", got)
	}
	for _, frag := range []string{
		"LOWER(BTRIM(mailed_brand))",
		"LOWER(COALESCE(NULLIF(BTRIM(isp_family), ''), 'other'))",
		"mailed_at IS NOT NULL",
		"ON CONFLICT (day, brand, isp) DO UPDATE",
	} {
		if !strings.Contains(propertyIntroRollupUpsertSQL, frag) {
			t.Errorf("rollup SQL missing %q", frag)
		}
	}
}

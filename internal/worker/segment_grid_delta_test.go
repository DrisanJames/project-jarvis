package worker

// Phase-2 (delta write path) tests. The negative paths the operator ruling
// requires pinned:
//   - bootstrap (no prior snapshot) falls back to the FULL path
//   - a missing base snapshot is never interpreted as "remove everything"
//   - a diff removing >50% of a big segment is refused with skipped_delta
//   - a diff that would wipe a segment to zero is refused even below the
//     guard size
//   - a failed COPY leaves members untouched (single-TX proof)
//   - the 7-day reconcile forces a full rebuild
//   - a failed Athena diff applies NOTHING (error ledger row)
//   - ledger statuses stay ok|skipped_delta|error (API contract unchanged)

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// mkGridSeg builds one parsed grid segment row.
func mkGridSeg(id, name, event string, window int, apex string) gridSegmentRow {
	return gridSegmentRow{
		spec: gridSpec{
			ID: id, Name: name, Event: event, WindowDays: window, BrandApex: apex,
		},
		orgID: uuid.New().String(),
	}
}

// expectLedgerTriple registers unordered expectations for mark-running, the
// terminal upsert with the given (source, status), and — when stampDt is
// non-nil — the delta-state stamp.
func expectLedgerTriple(mock sqlmock.Sqlmock, source, status string, stamp bool) {
	mock.ExpectExec(`VALUES \(\$1::uuid, \$2, 'running'`).
		WithArgs(sqlmock.AnyArg(), source).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`CASE WHEN \$4::text = 'ok'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), source, status,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if stamp {
		mock.ExpectExec(`last_snapshot_dt\s+= NULLIF`).
			WithArgs(sqlmock.AnyArg(), source, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

// stubDeltaSeams wires every delta seam to an "eligible, snapshots all
// present, bookkeeping no-op" default on a fresh worker.
func stubDeltaSeams(w *SegmentGridWorker, state map[string]gridDeltaState) {
	w.readerEnabledFn = func() bool { return true }
	w.deltaSupportedFn = func() bool { return true }
	w.ensureSnapFn = func(ctx context.Context) error { return nil }
	w.snapExistsFn = func(ctx context.Context, event string, windowDays int, dt string) bool { return true }
	w.unloadFn = func(ctx context.Context, event string, windowDays int, dt string) (int64, error) { return 0, nil }
	w.recordSnapFn = func(ctx context.Context, event string, windowDays int, dt string, pairs int64) {}
	w.pruneFn = func(ctx context.Context, event string, windowDays int, todayDt string) {}
	w.loadDeltaStateFn = func(ctx context.Context, ids []string) (map[string]gridDeltaState, bool) { return state, true }
	w.resolveFn = func(ctx context.Context, ids []string, seedListIDs []string) (map[string]gridSubscriber, error) {
		out := make(map[string]gridSubscriber, len(ids))
		for _, id := range ids {
			out[id] = gridSubscriber{Email: id + "@example.com"}
		}
		return out, nil
	}
}

// nullTimeSQL wraps a time in a valid sql.NullTime.
func nullTimeSQL(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }

// ---------------------------------------------------------------------------
// Bootstrap: no prior snapshot → FULL swap, never a diff/merge
// ---------------------------------------------------------------------------

func TestBuildSegments_BootstrapFallsBackToFull(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {snapshotDt: ""}}) // bootstrap
	sub := uuid.New().String()
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		return []analytics.GridPair{{SubscriberID: sub, BrandApex: "discountblog.com"}}, nil
	}
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		t.Fatal("bootstrap must never diff")
		return nil, nil
	}
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		t.Fatal("bootstrap must never delta-merge")
		return 0, nil
	}
	unloaded := false
	w.unloadFn = func(ctx context.Context, event string, windowDays int, dt string) (int64, error) {
		unloaded = true
		return 1, nil
	}
	w.snapExistsFn = func(ctx context.Context, event string, windowDays int, dt string) bool { return false }
	swapCalled := false
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		swapCalled = true
		if len(members) != 1 {
			t.Fatalf("full swap expected the whole membership, got %d", len(members))
		}
		return nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceFull, "ok", true)

	trip, built, _, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, map[string]gridLedgerRow{})
	if trip || built != 1 || failed != 0 {
		t.Fatalf("bootstrap full build expected (trip=%v built=%d failed=%d)", trip, built, failed)
	}
	if !swapCalled {
		t.Fatal("bootstrap must run the FULL swap path")
	}
	if !unloaded {
		t.Fatal("bootstrap must still UNLOAD today's snapshot (tomorrow's base)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delta path: only the diff ships; the full swap never runs
// ---------------------------------------------------------------------------

func TestBuildSegments_DeltaPathMergesOnlyDiff(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt:  yesterday,
		fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-24 * time.Hour)),
	}})
	addID, remID := uuid.New().String(), uuid.New().String()
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		if baseDt != yesterday {
			t.Fatalf("diff base must be the ledger's snapshot dt, got %s", baseDt)
		}
		return []analytics.GridDelta{
			{Op: "add", SubscriberID: addID, BrandApex: "discountblog.com"},
			{Op: "del", SubscriberID: remID, BrandApex: "discountblog.com"},
			{Op: "add", SubscriberID: uuid.New().String(), BrandApex: "otherbrand.com"}, // not ours
		}, nil
	}
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		t.Fatal("delta path must never run the full bucket query")
		return nil, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("delta path must never full-swap")
		return nil
	}
	var gotAdds []gridMember
	var gotRemoves []string
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		gotAdds, gotRemoves = adds, removes
		return 1000, nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceDelta, "ok", true)

	ledger := map[string]gridLedgerRow{segID: {count: 1000, builtAt: nullTimeSQL(time.Now().Add(-30 * time.Hour)), status: "ok"}}
	trip, built, _, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, ledger)
	if trip || built != 1 || failed != 0 {
		t.Fatalf("delta merge expected (trip=%v built=%d failed=%d)", trip, built, failed)
	}
	if len(gotAdds) != 1 || gotAdds[0].SubscriberID != addID {
		t.Fatalf("merge must receive exactly the brand's filtered adds, got %+v", gotAdds)
	}
	if len(gotRemoves) != 1 || gotRemoves[0] != remID {
		t.Fatalf("merge must receive exactly the brand's removes, got %v", gotRemoves)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Diff guard: a diff removing >50% of a big segment is refused
// ---------------------------------------------------------------------------

func TestBuildSegments_MajorityRemovalDiffRefused(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt: yesterday, fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-24 * time.Hour)),
	}})
	// 600 removes but 550 compensating adds: the PROJECTED count (950,
	// -5%) sails past the drop guard — only the diff-size rail can refuse
	// this churn storm (a half-lost base snapshot looks exactly like this).
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		out := make([]analytics.GridDelta, 0, 1150)
		for i := 0; i < 600; i++ {
			out = append(out, analytics.GridDelta{Op: "del", SubscriberID: uuid.New().String(), BrandApex: "discountblog.com"})
		}
		for i := 0; i < 550; i++ {
			out = append(out, analytics.GridDelta{Op: "add", SubscriberID: uuid.New().String(), BrandApex: "discountblog.com"})
		}
		return out, nil
	}
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		t.Fatal("a majority-removal diff must never merge")
		return 0, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("a refused diff must not fall through to a full swap")
		return nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceDelta, "skipped_delta", false)

	ledger := map[string]gridLedgerRow{segID: {count: 1000, builtAt: nullTimeSQL(time.Now().Add(-30 * time.Hour)), status: "ok"}}
	trip, built, skipped, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, ledger)
	if trip || built != 0 || skipped != 1 || failed != 0 {
		t.Fatalf("skipped_delta expected (built=%d skipped=%d failed=%d)", built, skipped, failed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the refusal must write its skipped_delta ledger row: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Empty rail: a diff wiping a segment to zero is refused even below the
// guard size (an empty/lost today-snapshot must never mass-remove)
// ---------------------------------------------------------------------------

func TestBuildSegments_DeltaNeverWipesToZero(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "AAD 7D Openers", "open", 7, "aadwd.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt: yesterday, fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-24 * time.Hour)),
	}})
	// 100 current members (below the 200 guard exemption) — remove them ALL.
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		out := make([]analytics.GridDelta, 0, 100)
		for i := 0; i < 100; i++ {
			out = append(out, analytics.GridDelta{Op: "del", SubscriberID: uuid.New().String(), BrandApex: "aadwd.com"})
		}
		return out, nil
	}
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		t.Fatal("a wipe-to-zero diff must never merge")
		return 0, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("no full swap either")
		return nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceDelta, "skipped_delta", false)

	ledger := map[string]gridLedgerRow{segID: {count: 100, builtAt: nullTimeSQL(time.Now().Add(-30 * time.Hour)), status: "ok"}}
	_, built, skipped, _ := w.buildSegments(context.Background(), []gridSegmentRow{seg}, ledger)
	if built != 0 || skipped != 1 {
		t.Fatalf("empty-delta rail expected (built=%d skipped=%d)", built, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reconcile: last full build past 7 days → full rebuild forced
// ---------------------------------------------------------------------------

func TestBuildSegments_SevenDayReconcileForcesFull(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt:  yesterday,
		fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-8 * 24 * time.Hour)), // 8 days: reconcile due
	}})
	sub := uuid.New().String()
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		return []analytics.GridPair{{SubscriberID: sub, BrandApex: "discountblog.com"}}, nil
	}
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		t.Fatal("reconcile must not diff — it exists to correct diff drift")
		return nil, nil
	}
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		t.Fatal("reconcile must not delta-merge")
		return 0, nil
	}
	swapCalled := false
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		swapCalled = true
		return nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceFull, "ok", true)

	ledger := map[string]gridLedgerRow{segID: {count: 1, builtAt: nullTimeSQL(time.Now().Add(-30 * time.Hour)), status: "ok"}}
	_, built, _, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, ledger)
	if built != 1 || failed != 0 || !swapCalled {
		t.Fatalf("reconcile full swap expected (built=%d failed=%d swap=%v)", built, failed, swapCalled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A failed Athena diff applies NOTHING: error ledger row, no merge, no
// surprise full swap
// ---------------------------------------------------------------------------

func TestBuildSegments_DiffFailureAppliesNothing(t *testing.T) {
	resetGridState()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt: yesterday, fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-24 * time.Hour)),
	}})
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		return nil, errors.New("athena: GENERIC_INTERNAL_ERROR")
	}
	w.mergeFn = func(ctx context.Context, segmentID string, adds []gridMember, removes []string) (int64, error) {
		t.Fatal("a failed diff must never merge")
		return 0, nil
	}
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		t.Fatal("a failed diff must not fall back to a full swap mid-pass")
		return nil
	}
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		t.Fatal("no full bucket query for a delta segment")
		return nil, nil
	}

	// Error terminal row only (the diff fails before mark-running).
	mock.ExpectExec(`CASE WHEN \$4::text = 'ok'`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), segmentGridLedgerSourceDelta, "error",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ledger := map[string]gridLedgerRow{segID: {count: 1000, builtAt: nullTimeSQL(time.Now().Add(-30 * time.Hour)), status: "ok"}}
	_, built, skipped, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, ledger)
	if built != 0 || skipped != 0 || failed != 1 {
		t.Fatalf("error row expected (built=%d skipped=%d failed=%d)", built, skipped, failed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Kill switch: DISABLE_SEGMENT_GRID_DELTA forces the phase-1 path
// ---------------------------------------------------------------------------

func TestBuildSegments_DeltaKillSwitchForcesFull(t *testing.T) {
	resetGridState()
	t.Setenv("DISABLE_SEGMENT_GRID_DELTA", "true")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	segID := uuid.New().String()
	seg := mkGridSeg(segID, "DB 30D Openers", "open", 30, "discountblog.com")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	w := NewSegmentGridWorker(db, nil)
	stubDeltaSeams(w, map[string]gridDeltaState{segID: {
		snapshotDt: yesterday, fullBuiltAt: nullTimeSQL(time.Now().UTC().Add(-24 * time.Hour)),
	}})
	w.loadDeltaStateFn = func(ctx context.Context, ids []string) (map[string]gridDeltaState, bool) {
		t.Fatal("killed delta must not even read delta state")
		return nil, false
	}
	w.unloadFn = func(ctx context.Context, event string, windowDays int, dt string) (int64, error) {
		t.Fatal("killed delta must not snapshot")
		return 0, nil
	}
	w.diffFn = func(ctx context.Context, event string, windowDays int, baseDt, todayDt string) ([]analytics.GridDelta, error) {
		t.Fatal("killed delta must not diff")
		return nil, nil
	}
	sub := uuid.New().String()
	w.bucketFn = func(ctx context.Context, event string, windowDays int) ([]analytics.GridPair, error) {
		return []analytics.GridPair{{SubscriberID: sub, BrandApex: "discountblog.com"}}, nil
	}
	swapCalled := false
	w.swapFn = func(ctx context.Context, segmentID string, members []gridMember) error {
		swapCalled = true
		return nil
	}

	expectLedgerTriple(mock, segmentGridLedgerSourceFull, "ok", true)

	_, built, _, failed := w.buildSegments(context.Background(), []gridSegmentRow{seg}, map[string]gridLedgerRow{})
	if built != 1 || failed != 0 || !swapCalled {
		t.Fatalf("kill switch must run phase-1 full path (built=%d failed=%d swap=%v)", built, failed, swapCalled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger flow: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mergeSegmentDelta — the one-TX proof
// ---------------------------------------------------------------------------

func TestMergeSegmentDelta_FailedCopyRollsBackEverything(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segID := uuid.New().String()
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`TRUNCATE mailing_segment_grid_stage`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectPrepare(`COPY "mailing_segment_grid_stage"`).
		ExpectExec().WillReturnError(errors.New("pq: out of memory"))
	mock.ExpectRollback()
	// NOTE: no DELETE, no INSERT, no UPDATE expectations — a failed COPY
	// must never reach them, and members stay untouched (the rollback is the
	// whole write).

	w := NewSegmentGridWorker(db, nil)
	_, err = w.mergeSegmentDelta(context.Background(), segID,
		[]gridMember{{SubscriberID: uuid.New().String(), Email: "a@example.com"}},
		[]string{uuid.New().String()})
	if err == nil {
		t.Fatal("merge must surface the COPY failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("TX flow: %v", err)
	}
}

func TestMergeSegmentDelta_HappyPathSingleTx(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segID := uuid.New().String()
	addID := uuid.New().String()
	remID := uuid.New().String()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`TRUNCATE mailing_segment_grid_stage`).WillReturnResult(sqlmock.NewResult(0, 0))
	prep := mock.ExpectPrepare(`COPY "mailing_segment_grid_stage"`)
	prep.ExpectExec().WithArgs("del", remID, "").WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().WithArgs("add", addID, "a@example.com").WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 0)) // flush
	mock.ExpectExec(`DELETE FROM mailing_segment_members m`).
		WithArgs(segID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`ON CONFLICT \(segment_id, subscriber_id\) DO NOTHING`).
		WithArgs(segID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM mailing_segment_members`).
		WithArgs(segID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1234))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(int64(1234), segID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`TRUNCATE mailing_segment_grid_stage`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	w := NewSegmentGridWorker(db, nil)
	count, err := w.mergeSegmentDelta(context.Background(), segID,
		[]gridMember{{SubscriberID: addID, Email: "a@example.com"}},
		[]string{remID})
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if count != 1234 {
		t.Fatalf("merge must return the post-merge count, got %d", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("TX flow: %v", err)
	}
}

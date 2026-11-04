package worker

// WP-F 2026-11-04: the refresh scope must equal SegmentCleanupWorker's
// keep-set (registry keep_policy='protect' families included), and the
// member count must run in bounded chunks so one statement_timeout costs only
// its own chunk — never the whole pass, and never a zero write.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestSegmentRefresh_ScopeIncludesRegistryProtected(t *testing.T) {
	q := segmentRefreshSelectSQL(false)
	for _, want := range []string{
		"mailing_segment_registry r",
		"r.keep_policy = 'protect'",
		"s.name LIKE r.family_pattern",
		"COALESCE(s.last_used_at, s.created_at) > NOW() - INTERVAL '30 days'",
		"ORDER BY s.updated_at ASC",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("scope SQL missing %q:\n%s", want, q)
		}
	}
}

// Negative path: the kill switch restores the pre-fix guard exactly.
func TestSegmentRefresh_LegacyScopeKillSwitch(t *testing.T) {
	t.Setenv("SEGMENT_REFRESH_LEGACY_SCOPE", "1")
	if !segmentRefreshLegacyScope() {
		t.Fatal("SEGMENT_REFRESH_LEGACY_SCOPE=1 must enable legacy scope")
	}
	q := segmentRefreshSelectSQL(segmentRefreshLegacyScope())
	if strings.Contains(q, "mailing_segment_registry") {
		t.Fatalf("legacy scope must not consult the registry:\n%s", q)
	}
	if !strings.Contains(q, "OR COALESCE(s.last_used_at, s.created_at) > NOW() - INTERVAL '30 days')") {
		t.Fatalf("legacy scope must keep the 30d guard:\n%s", q)
	}
	t.Setenv("SEGMENT_REFRESH_LEGACY_SCOPE", " TRUE ")
	if !segmentRefreshLegacyScope() {
		t.Fatal("SEGMENT_REFRESH_LEGACY_SCOPE=' TRUE ' must enable legacy scope (TrimSpace + EqualFold)")
	}
	t.Setenv("SEGMENT_REFRESH_LEGACY_SCOPE", "")
	if segmentRefreshLegacyScope() {
		t.Fatal("unset kill switch must use the protected scope")
	}
}

func TestSegmentRefresh_CountChunksRespectBudgetAndOrder(t *testing.T) {
	mk := func(n int) segmentRow { return segmentRow{ID: uuid.New(), OldCount: n} }
	segs := []segmentRow{mk(100), mk(150), mk(10), mk(500), mk(0), mk(-5), mk(240)}
	chunks := segmentRefreshCountChunks(segs, 250, 25)
	// [100,150] [10] [500] [0,-5,240]
	wantLens := []int{2, 1, 1, 3}
	if len(chunks) != len(wantLens) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(wantLens))
	}
	i := 0
	for c, ch := range chunks {
		if len(ch) != wantLens[c] {
			t.Fatalf("chunk %d len %d, want %d", c, len(ch), wantLens[c])
		}
		for _, s := range ch {
			if s.ID != segs[i].ID {
				t.Fatalf("order not preserved at %d", i)
			}
			i++
		}
	}
	if got := segmentRefreshCountChunks(nil, 250, 25); len(got) != 0 {
		t.Fatalf("empty input must give no chunks, got %d", len(got))
	}
}

// A run of 0/stale-count segments (the newly in-scope protected ones) must
// still be bounded by the per-chunk segment cap.
func TestSegmentRefresh_CountChunksCapSegmentsWhenCountsZero(t *testing.T) {
	segs := make([]segmentRow, 60)
	for i := range segs {
		segs[i] = segmentRow{ID: uuid.New(), OldCount: 0}
	}
	chunks := segmentRefreshCountChunks(segs, segmentRefreshChunkMembers, segmentRefreshChunkMaxSegments)
	wantLens := []int{25, 25, 10}
	if len(chunks) != len(wantLens) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(wantLens))
	}
	for c, ch := range chunks {
		if len(ch) != wantLens[c] {
			t.Fatalf("chunk %d len %d, want %d", c, len(ch), wantLens[c])
		}
	}
}

// A chunk whose first attempt times out is split once: both halves are
// queried; the clean half's counts land, the failing half writes nothing and
// stamps no zero, and the cycle reports error.
func TestSegmentRefresh_FailedChunkSplitsOnceAndRetriesHalves(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	orgID := uuid.New().String()
	segRows := sqlmock.NewRows([]string{"id", "list_id", "name", "conditions", "organization_id", "subscriber_count"}).
		AddRow(a.String(), nil, "HFC 30D Openers", "[]", orgID, 0).
		AddRow(b.String(), nil, "AAD 30D Openers", "[]", orgID, 0).
		AddRow(c.String(), nil, "BCC 7D Openers", "[]", orgID, 0).
		AddRow(d.String(), nil, "PMD 7D Clickers", "[]", orgID, 0)
	mock.ExpectQuery(`keep_policy = 'protect'`).WillReturnRows(segRows)

	timeout := errors.New("pq: canceling statement due to statement timeout")
	// first attempt: all four in one chunk — times out
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(timeout)
	// half 1 = [a, b] — times out again
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count"}).
			AddRow(a.String(), 29247).
			RowError(0, timeout))
	// half 2 = [c, d] — clean; d absent = verified empty
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count"}).
			AddRow(c.String(), 1148))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(c.String(), 1148).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(d.String(), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_refresh", "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"failed", 2, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentRefreshWorkerWithConcurrency(db, db, time.Hour, 1)
	w.refreshAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("split retry: clean half must land, failing half must write nothing: %v", err)
	}
}

// One chunk times out (the prod "canceling statement due to statement
// timeout" shape); the other is clean. Only the clean chunk's segments are
// written; the failed chunk's segment gets NO write (not a zero); the cycle
// still reports error so the failure stays visible.
func TestSegmentRefresh_FailedChunkWritesNothingCleanChunkLands(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	big, b1, b2 := uuid.New(), uuid.New(), uuid.New()
	orgID := uuid.New().String()
	segRows := sqlmock.NewRows([]string{"id", "list_id", "name", "conditions", "organization_id", "subscriber_count"}).
		AddRow(big.String(), nil, "DB 30D Openers", "[]", orgID, segmentRefreshChunkMembers).
		AddRow(b1.String(), nil, "HFC 30D Openers", "[]", orgID, 29247).
		AddRow(b2.String(), nil, "AAD 7D Clickers", "[]", orgID, 0)
	mock.ExpectQuery(`keep_policy = 'protect'`).WillReturnRows(segRows)

	// chunk 1 = [big] — truncated mid-stream by the server timeout
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count"}).
			AddRow(big.String(), 82396).
			RowError(0, errors.New("pq: canceling statement due to statement timeout")))
	// chunk 2 = [b1, b2] — clean; b2 absent = verified empty
	mock.ExpectQuery(`FROM mailing_segment_members`).
		WillReturnRows(sqlmock.NewRows([]string{"segment_id", "count"}).
			AddRow(b1.String(), 29250))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(b1.String(), 29250).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(b2.String(), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_refresh", "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"failed", 2, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentRefreshWorkerWithConcurrency(db, db, time.Hour, 1)
	w.refreshAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("failed chunk must write nothing, clean chunk must land: %v", err)
	}
}

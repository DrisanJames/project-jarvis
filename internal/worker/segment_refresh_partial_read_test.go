package worker

// Regression guard for the 2026-08-21 mass-zeroing incident: a member-count
// result set that fails MID-STREAM (rows.Err() != nil after the Next() loop)
// must abort the refresh pass with ZERO writes to mailing_segments. The old
// code never checked rows.Err(), so a truncated read left absent segments in
// the counts map defaulting to 0 and the write loop stamped
// subscriber_count = 0 onto 118 active segments whose member tables were
// fresh. Absence of measurement is not zero.
//
// Assertion technique: sqlmock does NOT fail ExpectationsWereMet on
// unexpected calls (the worker swallows per-statement errors), so the
// negative path is pinned through the pass's own terminal bookkeeping — the
// heartbeat upsert must carry status='error' and the run record 'failed'. If
// a regression reintroduces the silent-zero write, the pass ends 'ok', those
// argument matches fail, and the test fails.

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// segRefreshSegmentsRows mirrors the segment-list SELECT in refreshAll.
func segRefreshSegmentsRows(ids ...uuid.UUID) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "list_id", "name", "conditions", "organization_id", "subscriber_count"})
	for i, id := range ids {
		rows.AddRow(id.String(), nil, "Seg "+id.String()[:8], "[]", uuid.New().String(), 1000+i)
	}
	return rows
}

// TestRefreshAll_TruncatedCountResultZeroesNothing: the count query returns
// one good row and then a row-level error (the mid-stream shape a 120s ctx
// expiry or replica recovery-conflict cancellation produces). The pass must
// abort as an ERROR cycle without executing any UPDATE mailing_segments —
// including for the segment whose count DID arrive (a partial map must not
// be trusted at all).
func TestRefreshAll_TruncatedCountResultZeroesNothing(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segA, segB := uuid.New(), uuid.New()

	mock.ExpectQuery(`FROM mailing_segments s`).
		WillReturnRows(segRefreshSegmentsRows(segA, segB))

	countRows := sqlmock.NewRows([]string{"segment_id", "count"}).
		AddRow(segA.String(), 46484).
		AddRow(segB.String(), 18597).
		RowError(1, errors.New("pq: canceling statement due to conflict with recovery"))
	mock.ExpectQuery(`FROM mailing_segment_members`).WillReturnRows(countRows)

	// Terminal bookkeeping proves the abort: heartbeat status MUST be
	// 'error' (a zero-writing regression would beat 'ok' and fail this
	// match), and the run row MUST be 'failed' with 0 items processed.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_refresh", "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"failed", 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentRefreshWorkerWithConcurrency(db, db, time.Hour, 1)
	w.refreshAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("truncated count read must abort the pass with no segment writes: %v", err)
	}
}

// TestRefreshAll_ScanErrorZeroesNothing: a row that fails Scan (malformed id)
// is also a partial read — the pass must abort with no writes rather than
// treat the unscanned segment as empty.
func TestRefreshAll_ScanErrorZeroesNothing(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segA, segB := uuid.New(), uuid.New()

	mock.ExpectQuery(`FROM mailing_segments s`).
		WillReturnRows(segRefreshSegmentsRows(segA, segB))

	countRows := sqlmock.NewRows([]string{"segment_id", "count"}).
		AddRow("not-a-uuid", 46484). // Scan into uuid.UUID fails
		AddRow(segB.String(), 18597)
	mock.ExpectQuery(`FROM mailing_segment_members`).WillReturnRows(countRows)

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_refresh", "error", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"failed", 0, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentRefreshWorkerWithConcurrency(db, db, time.Hour, 1)
	w.refreshAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scan failure must abort the pass with no segment writes: %v", err)
	}
}

// TestRefreshAll_CompleteResultStillWritesAbsentAsZero pins the POSITIVE
// contract: when the count result is verified complete (rows.Err() == nil,
// every row scanned), a segment absent from the result genuinely has zero
// materialized members and IS written 0 — the guard must not make honest
// zeros unreachable.
func TestRefreshAll_CompleteResultStillWritesAbsentAsZero(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	segA, segB := uuid.New(), uuid.New()

	mock.ExpectQuery(`FROM mailing_segments s`).
		WillReturnRows(segRefreshSegmentsRows(segA, segB))

	countRows := sqlmock.NewRows([]string{"segment_id", "count"}).
		AddRow(segA.String(), 46484) // segB absent = verified empty
	mock.ExpectQuery(`FROM mailing_segment_members`).WillReturnRows(countRows)

	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(segA.String(), 46484).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_segments`).
		WithArgs(segB.String(), 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs("segment_refresh", "ok", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_worker_runs`).
		WithArgs("segment_refresh", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"ok", 2, 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE FROM mailing_worker_runs`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := NewSegmentRefreshWorkerWithConcurrency(db, db, time.Hour, 1)
	w.refreshAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("complete result should update both segments: %v", err)
	}
}

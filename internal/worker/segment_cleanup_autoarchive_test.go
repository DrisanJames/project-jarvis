package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAutoArchive_KillSwitchZeroArchives is the NEGATIVE PATH: with
// DISABLE_SEGMENT_AUTO_ARCHIVE set, the pass must issue ZERO SQL and archive
// nothing. The sqlmock carries no expectations, so ANY query fails the test
// — armed expectations, not a vacuous assertion.
func TestAutoArchive_KillSwitchZeroArchives(t *testing.T) {
	for _, v := range []string{"1", "true"} {
		t.Setenv("DISABLE_SEGMENT_AUTO_ARCHIVE", v)

		db, mock, err := sqlmock.New() // NO expectations: any query = failure
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()

		w := &SegmentCleanupWorker{db: db}
		if n := w.autoArchiveUnreferencedSegments(context.Background()); n != 0 {
			t.Errorf("kill switch %q: expected 0 archives, got %d", v, n)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("kill switch %q: SQL was issued despite kill switch: %v", v, err)
		}
	}
}

// TestAutoArchive_ArmedRunsUpdate is the positive control proving the
// negative test's expectations are ARMED: with the switch unset, the pass
// runs exactly the archive UPDATE (status='archived', archived_at=NOW(),
// gated on registry-protect + 30d campaign-reference NOT EXISTS) and
// reports the affected row count.
func TestAutoArchive_ArmedRunsUpdate(t *testing.T) {
	t.Setenv("DISABLE_SEGMENT_AUTO_ARCHIVE", "")

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE mailing_segments\s+SET status = 'archived', archived_at = NOW\(\)`).
		WithArgs(autoArchiveIdleDays, autoArchiveMaxPerCycle).
		WillReturnResult(sqlmock.NewResult(0, 3))

	w := &SegmentCleanupWorker{db: db}
	if n := w.autoArchiveUnreferencedSegments(context.Background()); n != 3 {
		t.Errorf("expected 3 archived, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected archive UPDATE was not issued: %v", err)
	}
}

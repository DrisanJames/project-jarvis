package worker

// REQ-C16 — SegmentCleanupWorker registry consent (closed-by-default).
// The whole point of these tests is the NEGATIVE path: a segment that is
// registered-protected, or not registered at all, must SURVIVE a cleanup pass.
// sqlmock only; any unexpected DELETE fails the test via ExpectationsWereMet.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func consentCols() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "purgeable", "registered"})
}

// newCleanupWorkerForTest builds the worker without starting its loop.
func newCleanupWorkerForTest(t *testing.T) (*SegmentCleanupWorker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewSegmentCleanupWorker(db, nil), mock
}

// Negative path 1 (the F3 fix): a keep_active=FALSE, aged, REGISTERED but
// PROTECTED segment reaches the hard-delete choke point and SURVIVES — no
// DELETE statement is ever issued, and the protected-skip counter records it.
func TestRegistryConsent_ProtectedSegmentSurvivesDelete(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	id := uuid.New()

	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnRows(consentCols().AddRow(id, "SLOT-APPLE-DB-CLK", false, true))
	// NOTE: no ExpectExec for any DELETE — issuing one would fail the test.

	segs, members := w.deleteSegmentsWithMembers(context.Background(), []uuid.UUID{id})
	if segs != 0 || members != 0 {
		t.Fatalf("protected segment was deleted: segs=%d members=%d", segs, members)
	}
	if w.consentSkippedProtected != 1 || w.consentSkippedUnregistered != 0 {
		t.Fatalf("skip counters = protected %d / unregistered %d, want 1/0",
			w.consentSkippedProtected, w.consentSkippedUnregistered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Negative path 2 (closed-by-default): a segment matching NO registry row is
// skipped, never deleted, and the skip is recorded in the counters that feed
// the mailing_worker_runs detail row (the assertable "alert" surface).
func TestRegistryConsent_UnregisteredFamilySurvivesCleanup(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	id := uuid.New()

	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnRows(consentCols().AddRow(id, "Some Future Family Nobody Registered", false, false))

	segs, members := w.deleteSegmentsWithMembers(context.Background(), []uuid.UUID{id})
	if segs != 0 || members != 0 {
		t.Fatalf("unregistered segment was deleted: segs=%d members=%d", segs, members)
	}
	if w.consentSkippedUnregistered != 1 {
		t.Fatalf("consentSkippedUnregistered = %d, want 1", w.consentSkippedUnregistered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
	// The run-row detail must carry the skip so the refusal is visible.
	if got := w.consentDetail(); got != ", registry-consent skipped 0 protected + 1 unregistered" {
		t.Fatalf("consentDetail = %q", got)
	}
}

// Positive path: a family explicitly registered keep_policy='purgeable' still
// cleans up — no accidental never-delete-anything regression.
func TestRegistryConsent_PurgeableFamilyStillDeletes(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	id := uuid.New()

	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnRows(consentCols().AddRow(id, "data-partner-wave-solar-db-20260701T090000", true, true))
	mock.ExpectExec(`DELETE FROM mailing_segment_members`).
		WillReturnResult(sqlmock.NewResult(0, 1200))
	mock.ExpectExec(`DELETE FROM mailing_segments`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	segs, members := w.deleteSegmentsWithMembers(context.Background(), []uuid.UUID{id})
	if segs != 1 || members != 1200 {
		t.Fatalf("purgeable delete: segs=%d members=%d, want 1/1200", segs, members)
	}
	if w.consentSkippedProtected != 0 || w.consentSkippedUnregistered != 0 {
		t.Fatalf("unexpected skips: %d/%d", w.consentSkippedProtected, w.consentSkippedUnregistered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Mixed batch: only the purgeable id reaches DELETE; protected + unregistered
// are partitioned out of the SAME batch.
func TestRegistryConsent_MixedBatchPartitions(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	purgeable, protected, unregistered := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnRows(consentCols().
			AddRow(purgeable, "data-partner-wave-x", true, true).
			AddRow(protected, "FRESH-J24-PARTNER-APL-DB", false, true).
			AddRow(unregistered, "mystery-cohort", false, false))
	mock.ExpectExec(`DELETE FROM mailing_segment_members`).
		WillReturnResult(sqlmock.NewResult(0, 10))
	mock.ExpectExec(`DELETE FROM mailing_segments`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	segs, _ := w.deleteSegmentsWithMembers(context.Background(),
		[]uuid.UUID{purgeable, protected, unregistered})
	if segs != 1 {
		t.Fatalf("segs = %d, want 1 (only the purgeable id)", segs)
	}
	if w.consentSkippedProtected != 1 || w.consentSkippedUnregistered != 1 {
		t.Fatalf("skip counters = %d/%d, want 1/1", w.consentSkippedProtected, w.consentSkippedUnregistered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// FAIL CLOSED: registry unreadable (e.g. table not migrated yet on a skewed
// boot) → the cycle deletes NOTHING rather than reverting to unguarded
// deletes.
func TestRegistryConsent_FailClosedOnRegistryError(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	id := uuid.New()

	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnError(errors.New(`pq: relation "mailing_segment_registry" does not exist`))

	segs, members := w.deleteSegmentsWithMembers(context.Background(), []uuid.UUID{id})
	if segs != 0 || members != 0 {
		t.Fatalf("fail-closed violated: segs=%d members=%d", segs, members)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Kill switch: DISABLE_SEGMENT_REGISTRY_CONSENT=true restores the exact
// legacy behavior (no registry read at all, deletes proceed) — the one-move
// rollback for the deploy gate.
func TestRegistryConsent_KillSwitchRestoresLegacy(t *testing.T) {
	t.Setenv("DISABLE_SEGMENT_REGISTRY_CONSENT", "true")
	w, mock := newCleanupWorkerForTest(t)
	id := uuid.New()

	// No consent SELECT expected — straight to the deletes.
	mock.ExpectExec(`DELETE FROM mailing_segment_members`).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`DELETE FROM mailing_segments`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	segs, _ := w.deleteSegmentsWithMembers(context.Background(), []uuid.UUID{id})
	if segs != 1 {
		t.Fatalf("kill switch: segs = %d, want 1", segs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Full purge-pass path: an aged keep_active=FALSE static snapshot that is
// UNREGISTERED enters purgeStaticSnapshots and survives the entire pass (the
// batch loop must also terminate rather than hot-loop on the skipped id).
func TestPurgeStaticSnapshots_UnregisteredSurvivesFullPass(t *testing.T) {
	w, mock := newCleanupWorkerForTest(t)
	orgID := uuid.New()
	segID := uuid.New()

	// Candidate list query (SELECT id FROM mailing_segments WHERE ... static ...)
	mock.ExpectQuery(`SELECT id FROM mailing_segments`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(segID))
	// Consent gate: unregistered → skipped.
	mock.ExpectQuery(`SELECT s\.id, s\.name`).
		WillReturnRows(consentCols().AddRow(segID, "ad-hoc snapshot 2026-06-01", false, false))
	// No DELETEs; loop must break on segs==0 (no second list query expected).

	deleted := w.purgeStaticSnapshots(context.Background(), CleanupSettings{OrganizationID: orgID})
	if deleted != 0 {
		t.Fatalf("purgeStaticSnapshots deleted %d, want 0", deleted)
	}
	if w.consentSkippedUnregistered != 1 {
		t.Fatalf("consentSkippedUnregistered = %d, want 1", w.consentSkippedUnregistered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

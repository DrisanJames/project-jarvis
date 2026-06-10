package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// newRedislessCapChecker builds a CapChecker with no Redis (Peek reports
// under-cap) and its own mock DB serving the one org-cap lookup the first
// Peek performs (subsequent Peeks hit the 60s cache).
func newRedislessCapChecker(t *testing.T) *mailing.CapChecker {
	t.Helper()
	capDB, capMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	capMock.ExpectQuery(`cross_brand_daily_cap`).WillReturnError(sql.ErrNoRows)
	t.Cleanup(func() { capDB.Close() })
	return mailing.NewCapChecker(capDB, nil, 2)
}

func setBasedTestParams() waveEnqueueParams {
	return waveEnqueueParams{
		waveID:       "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		waveUUID:     uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		campaignID:   uuid.New(),
		ispPlanID:    uuid.New(),
		orgID:        uuid.New(),
		scheduledAt:  time.Now(),
		baseHTML:     snapTestHTML,
		baseSubject:  "Best deals today",
		plainContent: "plain body",
		brandKey:     "discountblog",
	}
}

// claimColumns matches the set-based claim SELECT: plan_recipient fields plus
// the in-statement compliance verdicts.
var claimColumns = []string{
	"id", "subscriber_id", "email", "recipient_isp", "selection_rank",
	"audience_source_type", "audience_source_id", "status",
	"subscriber_status", "suppressed",
}

func expectSnapshotEnsured(mock sqlmock.Sqlmock, snapshotID uuid.UUID) {
	mock.ExpectQuery(`SELECT id FROM mailing_content_snapshots WHERE content_hash`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO mailing_content_snapshots`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(snapshotID.String()))
}

// TestEnqueueWaveSetBased_PartitionsAndBatches drives the full set-based path:
// compliance verdicts from the claim statement partition candidates into
// queued/skipped, and exactly three batched writes follow (skip transition,
// queue insert, queued transition) — never a per-recipient statement.
func TestEnqueueWaveSetBased_PartitionsAndBatches(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 2

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "good1@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false).
			AddRow(uuid.New().String(), uuid.New().String(), "unsubbed@gmail.com", "gmail", 2, "segment", nil, "selected", "unsubscribed", false).
			AddRow(uuid.New().String(), uuid.New().String(), "suppressed@yahoo.com", "yahoo", 3, "segment", nil, "selected", "active", true).
			AddRow(uuid.New().String(), uuid.New().String(), "good2@yahoo.com", "yahoo", 4, "segment", uuid.New().String(), "selected", "confirmed", false))

	mock.ExpectExec(`SET status = 'skipped' WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	queued, skipped, capSkipped, reserveUsed, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p)
	if err != nil {
		t.Fatalf("enqueueWaveSetBased: %v", err)
	}
	if queued != 2 || skipped != 2 || capSkipped != 0 || reserveUsed != 0 {
		t.Fatalf("unexpected counts queued=%d skipped=%d capSkipped=%d reserveUsed=%d", queued, skipped, capSkipped, reserveUsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestEnqueueWaveSetBased_EarlyExitAtRemaining preserves the legacy
// reserve-substitution gate: once `remaining` rows are queued, later
// candidates are untouched (still 'selected' — no skip statement at all).
func TestEnqueueWaveSetBased_EarlyExitAtRemaining(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 1

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false).
			AddRow(uuid.New().String(), uuid.New().String(), "b@gmail.com", "gmail", 2, "segment", nil, "selected", "active", false).
			AddRow(uuid.New().String(), uuid.New().String(), "c@gmail.com", "gmail", 3, "segment", nil, "selected", "active", false))

	// No skip statement expected: untouched candidates stay 'selected'.
	mock.ExpectExec(`INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	queued, skipped, _, _, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p)
	if err != nil {
		t.Fatalf("enqueueWaveSetBased: %v", err)
	}
	if queued != 1 || skipped != 0 {
		t.Fatalf("unexpected counts queued=%d skipped=%d (want 1, 0)", queued, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestEnqueueWaveSetBased_CountsReserveUse exercises the cap-aware claim shape
// (selected + reserve, over-pull, SKIP LOCKED) with a Redis-less CapChecker —
// Peek reports under-cap without Redis, so reserves substitute in rank order
// and reserve_used is reported, exactly like the legacy loop.
func TestEnqueueWaveSetBased_CountsReserveUse(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 2
	p.useCapAware = true
	capChecker := newRedislessCapChecker(t)

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FOR UPDATE OF pr SKIP LOCKED`).
		WithArgs(p.ispPlanID, p.remaining*2).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "sel@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false).
			AddRow(uuid.New().String(), uuid.New().String(), "gone@gmail.com", "gmail", 2, "segment", nil, "selected", "bounced", false).
			AddRow(uuid.New().String(), uuid.New().String(), "res@gmail.com", "gmail", 3, "segment", nil, "reserve", "active", false))

	mock.ExpectExec(`SET status = 'skipped' WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	queued, skipped, capSkipped, reserveUsed, err := enqueueWaveSetBased(context.Background(), tx, db, capChecker, p)
	if err != nil {
		t.Fatalf("enqueueWaveSetBased: %v", err)
	}
	if queued != 2 || skipped != 1 || capSkipped != 0 || reserveUsed != 1 {
		t.Fatalf("unexpected counts queued=%d skipped=%d capSkipped=%d reserveUsed=%d", queued, skipped, capSkipped, reserveUsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSourceID(t *testing.T) {
	valid := uuid.New()
	got := normalizeSourceID(sql.NullString{String: valid.String(), Valid: true})
	if !got.Valid || got.String != valid.String() {
		t.Fatalf("valid uuid should survive: %+v", got)
	}
	if normalizeSourceID(sql.NullString{}).Valid {
		t.Fatal("NULL in should be NULL out")
	}
	if normalizeSourceID(sql.NullString{String: "not-a-uuid", Valid: true}).Valid {
		t.Fatal("unparseable source id must become NULL (legacy parity)")
	}
}

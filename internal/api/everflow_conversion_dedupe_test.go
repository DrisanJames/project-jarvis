package api

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// TestRecordEverflowConversion_NewRowReportsInserted: a first-delivery postback
// inserts a row and must report true so the Slack alert fires exactly once.
func TestRecordEverflowConversion_NewRowReportsInserted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO mailing_everflow_conversions").
		WillReturnResult(sqlmock.NewResult(1, 1))

	got := recordEverflowConversion(context.Background(), db, "org",
		"42ba09106dc24cf3a18f88ed377b987a", "162", uuid.New(), uuid.New(),
		"sub1", "sub2", "sub3", 50.0)
	if !got {
		t.Fatal("first delivery returned false — the conversion alert would never fire")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestRecordEverflowConversion_RetrySuppressed is the regression guard for the
// 2026-07-31 duplicate-alert incident: Everflow retried one $50 conversion and
// the operator got FIVE Slack alerts, making $200 of revenue read as $400.
// ON CONFLICT DO NOTHING absorbs the retry (0 rows affected) and the function
// must report false so no second alert is posted.
func TestRecordEverflowConversion_RetrySuppressed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// Same transaction id arriving again -> ON CONFLICT DO NOTHING -> 0 rows.
	mock.ExpectExec("INSERT INTO mailing_everflow_conversions").
		WillReturnResult(sqlmock.NewResult(0, 0))

	got := recordEverflowConversion(context.Background(), db, "org",
		"42ba09106dc24cf3a18f88ed377b987a", "162", uuid.New(), uuid.New(),
		"sub1", "sub2", "sub3", 50.0)
	if got {
		t.Fatal("duplicate postback reported as new — this is exactly the 5-alerts-for-1-conversion bug")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestRecordEverflowConversion_ErrorDoesNotAlert: on an insert error we cannot
// know whether this conversion was already announced, so the safe direction is
// to stay silent. A missed alert is recoverable from the durable row (or the
// Everflow export); a phantom alert corrupts the operator's revenue read.
func TestRecordEverflowConversion_ErrorDoesNotAlert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO mailing_everflow_conversions").
		WillReturnError(context.DeadlineExceeded)

	if recordEverflowConversion(context.Background(), db, "org",
		"txn-err", "162", uuid.Nil, uuid.Nil, "", "", "", 50.0) {
		t.Fatal("insert error reported as a new conversion — would alert on an unrecorded event")
	}
}

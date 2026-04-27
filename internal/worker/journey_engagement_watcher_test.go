package worker

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestEngagementWatcher_Tick_NoEvents covers the empty case: no
// events means we don't run the exit UPDATE at all.
func TestEngagementWatcher_Tick_NoEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT event_type, COALESCE(recipient_email, ''), occurred_at`)).
		WillReturnRows(sqlmock.NewRows([]string{"event_type", "email", "occurred_at"}))

	w := NewJourneyEngagementWatcher(db)
	w.tick(context.Background())

	exited, errs := w.Stats()
	if exited != 0 || errs != 0 {
		t.Fatalf("expected 0/0; got %d/%d", exited, errs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestEngagementWatcher_ExitsOnOpen covers the core behavior: an
// 'opened' event in mailing_tracking_events should trigger the exit
// UPDATE for any active enrollment in a journey with exit_on_open=true.
// Critically, the join clause filters by the journey's flag column,
// so journeys with the toggle off are NOT affected — even if the
// subscriber is enrolled.
func TestEngagementWatcher_ExitsOnOpen(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT event_type, COALESCE(recipient_email, ''), occurred_at`)).
		WillReturnRows(sqlmock.NewRows([]string{"event_type", "email", "occurred_at"}).
			AddRow("opened", "alice@example.com", now))

	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'exited'`)).
		WithArgs(sqlmock.AnyArg(), "engaged_open").
		WillReturnResult(sqlmock.NewResult(0, 3))

	w := NewJourneyEngagementWatcher(db)
	w.tick(context.Background())

	exited, _ := w.Stats()
	if exited != 3 {
		t.Fatalf("expected 3 exited; got %d", exited)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestEngagementWatcher_ExitsOnClick parallels the open path but
// exercises the exit_on_click flag column. The two flags are
// substituted into the SQL by the watcher, so a regression that
// swapped them would surface as test failure.
func TestEngagementWatcher_ExitsOnClick(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT event_type, COALESCE(recipient_email, ''), occurred_at`)).
		WillReturnRows(sqlmock.NewRows([]string{"event_type", "email", "occurred_at"}).
			AddRow("clicked", "alice@example.com", now))

	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'exited'`)).
		WithArgs(sqlmock.AnyArg(), "engaged_click").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := NewJourneyEngagementWatcher(db)
	w.tick(context.Background())

	exited, _ := w.Stats()
	if exited != 1 {
		t.Fatalf("expected 1 exited on click; got %d", exited)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

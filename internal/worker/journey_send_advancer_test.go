package worker

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestSendAdvancer_Tick_NoEvents covers the empty case: no rows
// returned from the join means we don't touch the enrollments table.
func TestSendAdvancer_Tick_NoEvents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			e.id::text`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_type", "occurred_at", "campaign_id",
			"journey_id", "journey_node_id", "recipient_email",
		}))

	a := NewJourneySendAdvancer(db)
	a.tick(context.Background())

	advanced, exited, errs := a.Stats()
	if advanced != 0 || exited != 0 || errs != 0 {
		t.Fatalf("expected 0/0/0; got %d/%d/%d", advanced, exited, errs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestSendAdvancer_AdvancesOnSent proves that a 'sent' event causes
// the matching enrollment to advance to the next node id (resolved
// from the journey's connections JSON). This is the happy path.
func TestSendAdvancer_AdvancesOnSent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			e.id::text`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_type", "occurred_at", "campaign_id",
			"journey_id", "journey_node_id", "recipient_email",
		}).AddRow("ev-1", "sent", now, "camp-1", "j1", "n1", "alice@example.com"))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT connections::text FROM mailing_journeys`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"connections"}).
			AddRow(`[{"from":"n1","to":"n2"},{"from":"n2","to":"n3"}]`))

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs("j1", "alice@example.com", "camp-1", "n2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	a := NewJourneySendAdvancer(db)
	a.tick(context.Background())

	advanced, _, errs := a.Stats()
	if advanced != 1 {
		t.Fatalf("expected advanced=1; got %d", advanced)
	}
	if errs != 0 {
		t.Fatalf("expected no errors; got %d", errs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestSendAdvancer_CompletesWhenNoNextNode validates the journey-end
// case: a sent event for the last node in the graph completes the
// enrollment. exit_reason is not set; status is 'completed'.
func TestSendAdvancer_CompletesWhenNoNextNode(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			e.id::text`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_type", "occurred_at", "campaign_id",
			"journey_id", "journey_node_id", "recipient_email",
		}).AddRow("ev-1", "sent", now, "camp-1", "j1", "final", "alice@example.com"))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT connections::text FROM mailing_journeys`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"connections"}).
			AddRow(`[{"from":"start","to":"final"}]`))

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments
			SET status = 'completed'`)).
		WithArgs("j1", "alice@example.com", "camp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	a := NewJourneySendAdvancer(db)
	a.tick(context.Background())

	advanced, _, _ := a.Stats()
	if advanced != 1 {
		t.Fatalf("expected advanced=1 (treated as completed); got %d", advanced)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestSendAdvancer_FailsOnBounce verifies bounce/failed events flip
// the enrollment to 'failed' with exit_reason='send_bounced'. We do
// NOT advance because the user shouldn't get the next email if the
// previous one didn't deliver.
func TestSendAdvancer_FailsOnBounce(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
			e.id::text`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_type", "occurred_at", "campaign_id",
			"journey_id", "journey_node_id", "recipient_email",
		}).AddRow("ev-1", "bounced", now, "camp-1", "j1", "n1", "alice@example.com"))

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments
		SET status = 'failed'`)).
		WithArgs("j1", "alice@example.com", "camp-1", "send_bounced").
		WillReturnResult(sqlmock.NewResult(0, 1))

	a := NewJourneySendAdvancer(db)
	a.tick(context.Background())

	_, exited, _ := a.Stats()
	if exited != 1 {
		t.Fatalf("expected exited=1; got %d", exited)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

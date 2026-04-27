package worker

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestActivator_BuffersUntilDrain proves that ActivateNode does NOT
// touch the database, only the in-memory bucket. This is the core
// invariant that lets us batch many enrollments into one shadow
// campaign per drain tick.
func TestActivator_BuffersUntilDrain(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	a := NewJourneyEmailNodeActivator(db)
	if err := a.ActivateNode("e1", "j1", "n1", "alice@example.com",
		"Hi", "<p>hi</p>", "Brand", "from@brand.com"); err != nil {
		t.Fatalf("ActivateNode: %v", err)
	}
	if err := a.ActivateNode("e2", "j1", "n1", "bob@example.com",
		"Hi", "<p>hi</p>", "Brand", "from@brand.com"); err != nil {
		t.Fatalf("ActivateNode: %v", err)
	}

	if got := a.PendingCount(); got != 2 {
		t.Fatalf("expected 2 pending; got %d", got)
	}
	if got := a.PendingForNode("j1", "n1"); got != 2 {
		t.Fatalf("expected 2 pending for j1/n1; got %d", got)
	}
}

// TestActivator_DrainCreatesShadowCampaign exercises the full drain
// path: it mocks the wave-index lookup (none yet -> 1), the
// organization_id lookup, the INSERT into mailing_campaigns, and one
// metadata UPDATE per buffered enrollment. After drain the bucket is
// empty.
func TestActivator_DrainCreatesShadowCampaign(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	a := NewJourneyEmailNodeActivator(db)
	if err := a.ActivateNode("e1", "j1", "n1", "alice@example.com",
		"Subject A", "<p>html</p>", "Brand", "from@brand.com"); err != nil {
		t.Fatalf("ActivateNode: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT MAX(journey_wave_index)`)).
		WithArgs("j1", "n1").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT organization_id::text FROM mailing_journeys`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("00000000-0000-0000-0000-000000000001"))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs("e1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	a.drainAll(context.Background())

	if got := a.PendingCount(); got != 0 {
		t.Fatalf("expected 0 pending after drain; got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestActivator_NextWaveIndex_Increments verifies that successive
// drains for the same (journey, node) get incrementing wave indices.
// This is the property that makes shadow campaigns sortable in the
// Campaigns list and that gives /node-stats a deterministic
// shadow_campaigns count.
func TestActivator_NextWaveIndex_Increments(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	a := NewJourneyEmailNodeActivator(db)
	if err := a.ActivateNode("e1", "j1", "n1", "alice@example.com",
		"S", "<p/>", "B", "f@b.com"); err != nil {
		t.Fatalf("ActivateNode: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT MAX(journey_wave_index)`)).
		WithArgs("j1", "n1").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(7)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT organization_id::text FROM mailing_journeys`)).
		WithArgs("j1").
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow("00000000-0000-0000-0000-000000000001"))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_enrollments`)).
		WithArgs("e1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	a.drainAll(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestActivator_RejectsInvalidArgs prevents foot-guns where a buggy
// caller passes "" for journey or node id, which would let unrelated
// rows collapse into one bucket.
func TestActivator_RejectsInvalidArgs(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	a := NewJourneyEmailNodeActivator(db)
	if err := a.ActivateNode("e", "", "n", "x@y.com", "s", "h", "fn", "fe@b.com"); err == nil {
		t.Fatalf("expected error on missing journeyID")
	}
	if err := a.ActivateNode("e", "j", "", "x@y.com", "s", "h", "fn", "fe@b.com"); err == nil {
		t.Fatalf("expected error on missing nodeID")
	}
	if err := a.ActivateNode("e", "j", "n", "", "s", "h", "fn", "fe@b.com"); err == nil {
		t.Fatalf("expected error on missing subscriberEmail")
	}
	if got := a.PendingCount(); got != 0 {
		t.Fatalf("rejected calls should not buffer; pending=%d", got)
	}
}

package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// A transient error looking up the ORIGINAL of a "~fresh" orphan is "unknown",
// not "absent": before the fix it fell through to "discarded" and cancelled the
// sibling (with the original possibly already cancelled — both cells dead).
// Now the tick leaves the sibling alone and retries next tick.
func TestPredispatchRecoverOrphans_LookupErrorDoesNotCancelSibling(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM mailing_campaigns c\s+WHERE name LIKE '%' \|\| \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "org", "name", "status", "recips", "waves", "created", "anchor"}).
			AddRow("bbbbbbbb-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000001",
				"11182026 - DB - Offer"+predispatchSiblingSfx, "scheduled", 500, 3, now.Add(-2*time.Hour), now.Add(10*time.Minute)))
	mock.ExpectQuery(`SELECT id::text, status, COALESCE\(total_recipients,0\)`).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	// sqlmock rejects (does not record) an unexpected Exec, so watch the log:
	// the cancel path logs "discarded" before it issues the UPDATE.
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	s := &PMTACampaignService{db: db}
	s.predispatchRecoverOrphans(context.Background(), predispatchDeps{now: func() time.Time { return now }})
	log.SetOutput(prev)
	if strings.Contains(buf.String(), "discarded") {
		t.Fatalf("sibling was discarded/cancelled on a transient lookup error:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "retry next tick") {
		t.Fatalf("expected a loud retry log, got:\n%s", buf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

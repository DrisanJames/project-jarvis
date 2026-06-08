package worker

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// busyProbe matches primaryBusy's pg_stat_activity probe.
var busyProbe = regexp.MustCompile(`FROM pg_stat_activity`)

// slimUpdate matches the slimmer's batched UPDATE.
var slimUpdate = regexp.MustCompile(`UPDATE mailing_campaign_queue q\s+SET html_content = NULL`)

func newCleanupMock(t *testing.T) (*DataCleanupWorker, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return &DataCleanupWorker{db: db, interval: DefaultCleanupInterval}, mock, func() { db.Close() }
}

// When the primary is under heavy IO load, the slimmer must defer without
// issuing any UPDATE.
func TestSlimAcceptedQueueHTML_DefersWhenPrimaryBusy(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(slimMaxIOWaitBackends))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A failing/slow load probe must fail closed (defer), never proceed to slim.
func TestSlimAcceptedQueueHTML_FailsClosedOnProbeError(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).WillReturnError(errors.New("probe timeout"))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Happy path: idle primary -> slim drains batches until an UPDATE affects zero
// rows, then stops.
func TestSlimAcceptedQueueHTML_DrainsUntilEmpty(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// First batch slims a full batch, second batch finds nothing -> loop ends.
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnResult(sqlmock.NewResult(0, slimBatchSize))
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 0))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A missing table on the first batch is swallowed (worker safe before
// migrations) and does not panic or loop.
func TestSlimAcceptedQueueHTML_MissingTableIsSafe(t *testing.T) {
	dc, mock, done := newCleanupMock(t)
	defer done()

	mock.ExpectQuery(busyProbe.String()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(slimUpdate.String()).
		WithArgs(slimBatchSize).
		WillReturnError(errors.New(`relation "mailing_campaign_queue" does not exist`))

	dc.slimAcceptedQueueHTML(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPrimaryBusy_Thresholds(t *testing.T) {
	tests := []struct {
		name    string
		ioWait  int
		wantTrue bool
	}{
		{"below threshold proceeds", slimMaxIOWaitBackends - 1, false},
		{"at threshold defers", slimMaxIOWaitBackends, true},
		{"above threshold defers", slimMaxIOWaitBackends + 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dc, mock, done := newCleanupMock(t)
			defer done()
			mock.ExpectQuery(busyProbe.String()).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(tt.ioWait))
			if got := dc.primaryBusy(context.Background()); got != tt.wantTrue {
				t.Fatalf("primaryBusy(ioWait=%d) = %v, want %v", tt.ioWait, got, tt.wantTrue)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

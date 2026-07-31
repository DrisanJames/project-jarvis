package worker

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectedMonths mirrors EnsurePartitions' own month walk so the test pins the
// naming/bound CONTRACT rather than re-deriving it a second, drifting way.
func expectedMonths() []time.Time {
	base := time.Now().UTC()
	out := make([]time.Time, 0, monthsAhead+1)
	for i := 0; i <= monthsAhead; i++ {
		out = append(out, time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0))
	}
	return out
}

// TestEnsurePartitions_CreatesMissing is the regression guard for the
// 2026-07-31 near-miss: a month with no partition must produce a CREATE TABLE
// ... PARTITION OF with the correct half-open monthly bounds.
func TestEnsurePartitions_CreatesMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	months := expectedMonths()
	for _, table := range partitionedEventTables {
		for _, start := range months {
			end := start.AddDate(0, 1, 0)
			child := fmt.Sprintf("%s_%04d_%02d", table, start.Year(), int(start.Month()))

			// Nothing exists yet -> every month must be created.
			mock.ExpectQuery(`FROM pg_inherits`).
				WithArgs(table, child).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

			want := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				child, table,
				start.Format("2006-01-02 15:04:05-07"),
				end.Format("2006-01-02 15:04:05-07"),
			)
			mock.ExpectExec(regexp.QuoteMeta(want)).
				WillReturnResult(sqlmock.NewResult(0, 0))
		}
	}

	created, err := EnsurePartitions(context.Background(), db)
	if err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	if want := len(partitionedEventTables) * len(months); created != want {
		t.Errorf("created = %d, want %d", created, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestEnsurePartitions_SkipsExisting proves the steady-state run is read-only:
// this function executes on every boot, so an existing month must issue NO
// DDL at all. sqlmock fails the test if any unexpected Exec arrives.
func TestEnsurePartitions_SkipsExisting(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	for _, table := range partitionedEventTables {
		for _, start := range expectedMonths() {
			child := fmt.Sprintf("%s_%04d_%02d", table, start.Year(), int(start.Month()))
			mock.ExpectQuery(`FROM pg_inherits`).
				WithArgs(table, child).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		}
	}

	created, err := EnsurePartitions(context.Background(), db)
	if err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	if created != 0 {
		t.Errorf("created = %d on an already-provisioned schema, want 0", created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestEnsurePartitions_CoversMonthBoundary is the check that actually matters:
// whatever "now" is, the provisioned range must extend strictly PAST the end
// of the current month, so an insert at 00:00 on the 1st always lands.
func TestEnsurePartitions_CoversMonthBoundary(t *testing.T) {
	months := expectedMonths()
	last := months[len(months)-1].AddDate(0, 1, 0) // exclusive end of coverage

	now := time.Now().UTC()
	nextMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)

	if !last.After(nextMonthStart) {
		t.Fatalf("coverage ends %s, which does not extend past the next month boundary %s — "+
			"the exact failure mode of the 2026-07-31 near-miss", last, nextMonthStart)
	}
	if got := last.Sub(now); got < 60*24*time.Hour {
		t.Errorf("only %v of partition runway; want >= 60d", got)
	}
}

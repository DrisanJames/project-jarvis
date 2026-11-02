package worker

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const testAudienceOrg = "00000000-0000-0000-0000-000000000001"

func newAudienceTestWorker(t *testing.T) (*GrowthRollupWorker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	return &GrowthRollupWorker{db: db, loc: loc, trailingDays: 3}, mock
}

func TestAudienceDaysToCompute_TrailingPlusMissing(t *testing.T) {
	w, mock := newAudienceTestWorker(t)
	now := time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC) // Nov 2 in Denver

	rows := sqlmock.NewRows([]string{"day"})
	for _, d := range []string{"2026-11-02", "2026-11-01", "2026-10-31", "2026-10-30", "2026-10-28", "2026-10-27"} {
		v, _ := time.Parse("2006-01-02", d)
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT day FROM mailing_audience_growth_daily").
		WithArgs("2026-10-27").WillReturnRows(rows)

	days, err := w.audienceDaysToCompute(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, d := range days {
		got = append(got, d.Format("2006-01-02"))
	}
	// Trailing 3 always; 10-29 because it has no rows; covered older days skipped.
	want := "2026-11-02 2026-11-01 2026-10-31 2026-10-29"
	if strings.Join(got, " ") != want {
		t.Fatalf("days = %v, want %s", got, want)
	}
}

func TestComputeAudienceDay_ReplacesDayInOneTx(t *testing.T) {
	w, mock := newAudienceTestWorker(t)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, w.loc)

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_audience_growth_daily").WithArgs("2026-11-02", testAudienceOrg).
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_audience_growth_daily")).
		WithArgs(day, day.AddDate(0, 0, 1), "2026-11-02", testAudienceOrg).WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectCommit()

	if err := w.computeAudienceDay(context.Background(), testAudienceOrg, day); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A failed insert must roll back, or the delete would blank a good day.
func TestComputeAudienceDay_InsertFailureRollsBack(t *testing.T) {
	w, mock := newAudienceTestWorker(t)
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, w.loc)

	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_audience_growth_daily").WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec("INSERT INTO mailing_audience_growth_daily").WillReturnError(errors.New("timeout"))
	mock.ExpectRollback()

	if err := w.computeAudienceDay(context.Background(), testAudienceOrg, day); err == nil {
		t.Fatal("want error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAudienceGrowthInsertSQL_Invariants(t *testing.T) {
	for _, want := range []string{
		"split_part(email, '@', 2)", // ISP from the address, never the polluted isp column
		"organization_id = $4", // a literal org is what lets the (org, created_at) index serve it
		"FULL JOIN", // an ISP with churn and no acquisition must still appear
		"'bounced','complained','blacklisted','unsubscribed'",
	} {
		if !strings.Contains(audienceGrowthInsertSQL, want) {
			t.Errorf("missing %q", want)
		}
	}
}

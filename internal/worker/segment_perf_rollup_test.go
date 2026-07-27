package worker

// SegmentPerfRollupWorker tests — Denver day bucketing, schedule math, the
// gap-fill day selection, and the per-day compute SQL (sqlmock: own tx,
// raised statement_timeout, idempotent upsert args). No DB needed.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDenverDayBounds(t *testing.T) {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("America/Denver tzdata unavailable")
	}
	// 2026-07-26 03:30 UTC = 2026-07-25 21:30 Denver (MDT, UTC-6) — the
	// Denver day is still the 25th.
	utc := time.Date(2026, 7, 26, 3, 30, 0, 0, time.UTC)
	day, start, end := denverDayBounds(utc, loc)
	if day.Format("2006-01-02") != "2026-07-25" {
		t.Fatalf("day = %s, want 2026-07-25", day.Format("2006-01-02"))
	}
	if end.Sub(start) != 24*time.Hour {
		t.Errorf("day span = %s, want 24h", end.Sub(start))
	}
	if got := start.UTC().Hour(); got != 6 { // 00:00 MDT = 06:00 UTC
		t.Errorf("day start UTC hour = %d, want 6 (MDT)", got)
	}
}

func TestSegmentPerfTimeUntilNextTick(t *testing.T) {
	w := &SegmentPerfRollupWorker{}
	// Before 04:10 UTC → same day.
	d := w.timeUntilNextTick(time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC))
	if d != 2*time.Hour+10*time.Minute {
		t.Errorf("sleep = %s, want 2h10m", d)
	}
	// After 04:10 UTC → tomorrow.
	d = w.timeUntilNextTick(time.Date(2026, 7, 26, 5, 0, 0, 0, time.UTC))
	if d != 23*time.Hour+10*time.Minute {
		t.Errorf("sleep = %s, want 23h10m", d)
	}
}

func TestSegmentPerfDaysNeedingCompute_GapFillPlusTrailing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	w := NewSegmentPerfRollupWorker(db, nil)
	w.backfillDays = 5
	w.trailingDays = 2

	today, _, _ := denverDayBounds(time.Now(), w.loc)
	// Coverage: days -5..-3 already have rows; -2 and -1 do not.
	rows := sqlmock.NewRows([]string{"day"})
	for i := 5; i >= 3; i-- {
		rows.AddRow(today.AddDate(0, 0, -i))
	}
	mock.ExpectQuery(`SELECT DISTINCT day FROM mailing_segment_perf_daily`).
		WithArgs(today.AddDate(0, 0, -5).Format("2006-01-02")).
		WillReturnRows(rows)

	days, err := w.daysNeedingCompute(context.Background())
	if err != nil {
		t.Fatalf("daysNeedingCompute: %v", err)
	}
	// Expect: the uncovered -2 and -1 (gap fill) — which are ALSO the trailing
	// window — and nothing older (covered, outside trailing). Today excluded.
	if len(days) != 2 {
		t.Fatalf("days = %v, want exactly the trailing/uncovered 2", days)
	}
	if days[0].Format("2006-01-02") != today.AddDate(0, 0, -2).Format("2006-01-02") ||
		days[1].Format("2006-01-02") != today.AddDate(0, 0, -1).Format("2006-01-02") {
		t.Errorf("days = %v, want [-2, -1] oldest first", days)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSegmentPerfComputeDay_BoundedTxUpsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	w := NewSegmentPerfRollupWorker(db, nil)

	day := time.Date(2026, 7, 24, 0, 0, 0, 0, w.loc)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_segment_perf_daily`).
		WithArgs("2026-07-24", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 42))
	mock.ExpectCommit()

	if err := w.computeDay(context.Background(), day); err != nil {
		t.Fatalf("computeDay: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSegmentPerfComputeDay_ErrorRollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	w := NewSegmentPerfRollupWorker(db, nil)

	day := time.Date(2026, 7, 24, 0, 0, 0, 0, w.loc)
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO mailing_segment_perf_daily`).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()

	if err := w.computeDay(context.Background(), day); err == nil {
		t.Fatal("computeDay must surface the statement error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

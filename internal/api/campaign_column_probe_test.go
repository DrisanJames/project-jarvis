package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const colProbeSQL = `SELECT column_name\s+FROM information_schema.columns`

// A boot probe that fails (or is cut short) must not be cached as "no columns":
// the next use re-probes and sees pmta_config. Before the fix the empty set was
// kept for the process lifetime and every deploy inserted without pmta_config.
func TestCampaignColumnCache_FailedProbeIsNotCached(t *testing.T) {
	orig := campaignColumnReprobeBackoff
	campaignColumnReprobeBackoff = 0
	t.Cleanup(func() { campaignColumnReprobeBackoff = orig })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Boot probe: cursor cut short after one row (the statement-timeout shape).
	mock.ExpectQuery(colProbeSQL).WillReturnRows(sqlmock.NewRows([]string{"column_name"}).
		AddRow("id").AddRow("pmta_config").
		RowError(1, errors.New("canceling statement due to statement timeout")))
	// Second probe: outright query error.
	mock.ExpectQuery(colProbeSQL).WillReturnError(errors.New("driver: bad connection"))
	// Third probe: complete.
	mock.ExpectQuery(colProbeSQL).WillReturnRows(sqlmock.NewRows([]string{"column_name"}).
		AddRow("id").AddRow("pmta_config").AddRow("execution_mode"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cc := probeCampaignColumns(ctx, db)

	if cc.has("pmta_config") {
		t.Fatal("second probe errored, yet pmta_config reported present")
	}
	if !cc.has("pmta_config") || !cc.has("execution_mode") {
		t.Fatal("complete re-probe did not populate the cache")
	}
	// Loaded: no further probes.
	_ = cc.has("pmta_config")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Concurrency: many first-use callers after a failed boot probe trigger
// exactly one re-probe (run with -race).
func TestCampaignColumnCache_ConcurrentReprobeOnce(t *testing.T) {
	orig := campaignColumnReprobeBackoff
	campaignColumnReprobeBackoff = 0
	t.Cleanup(func() { campaignColumnReprobeBackoff = orig })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(colProbeSQL).WillReturnError(errors.New("timeout"))
	mock.ExpectQuery(colProbeSQL).WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("pmta_config"))

	cc := probeCampaignColumns(context.Background(), db)
	done := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		go func() { done <- cc.has("pmta_config") }()
	}
	for i := 0; i < 16; i++ {
		if !<-done {
			t.Error("caller saw pmta_config absent after re-probe")
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

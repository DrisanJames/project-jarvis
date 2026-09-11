package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// 2026-09-11 barricade: the tags DDL must be probe-skippable and must run
// with a LOCAL lock_timeout, never as a raw db.Exec.
func TestSubscribersTagsDDL_IsProbeRecognized(t *testing.T) {
	want := map[string]migrationStatementKind{"add_subscribers_tags": migStmtAddColumn, "idx_subscribers_tags_gin": migStmtCreateIndex}
	for _, st := range subscribersTagsDDL {
		kind, _, _ := classifyMigrationStatement(st.sql)
		if kind != want[st.name] {
			t.Errorf("%s: classified %v, want %v — migrationSkipProbe would not skip it", st.name, kind, want[st.name])
		}
	}
}

func TestExecHotTableDDL_SetsLocalLockTimeout(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET LOCAL lock_timeout = '3s'`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`SET LOCAL statement_timeout = '20s'`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(subscribersTagsDDL[0].sql)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if err := execHotTableDDL(db, subscribersTagsDDL[0].sql); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNoRawSubscribersDDLAtBoot(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile("db\\.Exec\\(`(ALTER TABLE|CREATE INDEX IF NOT EXISTS \\w+ ON) mailing_subscribers").Match(src) {
		t.Fatal("raw db.Exec DDL on mailing_subscribers at boot: route it through migrationSkipProbe + execHotTableDDL")
	}
}

package main

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Classification samples are drawn verbatim from the live migration slice —
// the probe must recognize the real shapes, not idealized ones.
func TestClassifyMigrationStatement(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   migrationStatementKind
		ident1 string
		ident2 string
	}{
		{
			name: "create table multiline",
			sql: `CREATE TABLE IF NOT EXISTS mailing_content_snapshots (
				id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				content_hash TEXT NOT NULL UNIQUE
			)`,
			kind: migStmtCreateTable, ident1: "mailing_content_snapshots",
		},
		{
			name: "create index",
			sql:  `CREATE INDEX IF NOT EXISTS idx_queue_wave_id ON mailing_campaign_queue (wave_id)`,
			kind: migStmtCreateIndex, ident1: "idx_queue_wave_id",
		},
		{
			name: "create unique partial index",
			sql:  `CREATE UNIQUE INDEX IF NOT EXISTS uq_mcq_idempotency_key ON mailing_campaign_queue (idempotency_key) WHERE idempotency_key IS NOT NULL`,
			kind: migStmtCreateIndex, ident1: "uq_mcq_idempotency_key",
		},
		{
			name: "add column simple",
			sql:  `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS content_snapshot_id UUID`,
			kind: migStmtAddColumn, ident1: "mailing_campaign_queue", ident2: "content_snapshot_id",
		},
		{
			name: "add column with comma inside type parens",
			sql:  `ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS engagement_score DECIMAL(5,2) DEFAULT 0`,
			kind: migStmtAddColumn, ident1: "mailing_subscribers", ident2: "engagement_score",
		},
		{
			name: "add column with default expr",
			sql:  `ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}'`,
			kind: migStmtAddColumn, ident1: "mailing_tracking_events", ident2: "metadata",
		},
		{
			name: "drop constraint",
			sql:  `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`,
			kind: migStmtDropConstraint, ident1: "mailing_campaigns", ident2: "mailing_campaigns_status_check",
		},
		// ── shapes that must NEVER be skipped ──
		{
			name: "multi-action alter is unknown",
			sql:  `ALTER TABLE t ADD COLUMN IF NOT EXISTS a INT, ADD COLUMN IF NOT EXISTS b INT`,
			kind: migStmtUnknown,
		},
		{
			name: "add column without IF NOT EXISTS is unknown",
			sql:  `ALTER TABLE t ADD COLUMN a INT`,
			kind: migStmtUnknown,
		},
		{
			name: "do block is unknown",
			sql:  `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT; EXCEPTION WHEN OTHERS THEN NULL; END $$`,
			kind: migStmtUnknown,
		},
		{
			name: "seed insert is unknown",
			sql:  `INSERT INTO mailing_consumer_signal_slugs (slug) VALUES ('X') ON CONFLICT (slug) DO NOTHING`,
			kind: migStmtUnknown,
		},
		{
			name: "backfill update is unknown",
			sql:  `UPDATE mailing_campaigns SET status = 'sent' WHERE status = 'sending'`,
			kind: migStmtUnknown,
		},
		{
			name: "create table without IF NOT EXISTS is unknown",
			sql:  `CREATE TABLE t (id INT)`,
			kind: migStmtUnknown,
		},
		{
			name: "alter column type is unknown",
			sql:  `ALTER TABLE mailing_campaigns ALTER COLUMN status TYPE TEXT`,
			kind: migStmtUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, i1, i2 := classifyMigrationStatement(tc.sql)
			if kind != tc.kind {
				t.Fatalf("kind = %v, want %v", kind, tc.kind)
			}
			if tc.kind == migStmtUnknown {
				return
			}
			if i1 != tc.ident1 || i2 != tc.ident2 {
				t.Fatalf("idents = (%q, %q), want (%q, %q)", i1, i2, tc.ident1, tc.ident2)
			}
		})
	}
}

func TestMigrationSkipProbe_SkipsExistingColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`information_schema\.columns`).
		WithArgs("mailing_campaign_queue", "content_snapshot_id").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	if !migrationSkipProbe(db, `ALTER TABLE mailing_campaign_queue ADD COLUMN IF NOT EXISTS content_snapshot_id UUID`) {
		t.Fatal("existing column must be skipped")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSkipProbe_ExecutesMissingColumnAndFailsOpen(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Missing column → don't skip.
	mock.ExpectQuery(`information_schema\.columns`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if migrationSkipProbe(db, `ALTER TABLE t ADD COLUMN IF NOT EXISTS missing_col INT`) {
		t.Fatal("missing column must execute")
	}

	// Probe error → fail open (don't skip).
	mock.ExpectQuery(`information_schema\.columns`).
		WillReturnError(errTestProbe)
	if migrationSkipProbe(db, `ALTER TABLE t ADD COLUMN IF NOT EXISTS any_col INT`) {
		t.Fatal("probe error must fail open into execution")
	}

	// Unknown statements never touch the DB (no expectation registered).
	if migrationSkipProbe(db, `UPDATE t SET x = 1`) {
		t.Fatal("unknown statement must execute")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSkipProbe_InvalidIndexIsNotSkipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// indisvalid=false (interrupted CONCURRENTLY leftover) → execute.
	mock.ExpectQuery(`pg_index`).
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(false))
	if migrationSkipProbe(db, `CREATE INDEX IF NOT EXISTS idx_x ON t (col)`) {
		t.Fatal("invalid index must not suppress execution")
	}

	// Valid index → skip.
	mock.ExpectQuery(`pg_index`).
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
	if !migrationSkipProbe(db, `CREATE INDEX IF NOT EXISTS idx_x ON t (col)`) {
		t.Fatal("valid index must be skipped")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type probeErr string

func (e probeErr) Error() string { return string(e) }

const errTestProbe probeErr = "probe boom"

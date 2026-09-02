package main

import (
	"os"
	"strings"
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
		{
			name: "drop trigger",
			sql:  `DROP TRIGGER IF EXISTS trigger_update_optimal_time_on_open ON mailing_tracking_events`,
			kind: migStmtDropTrigger, ident1: "mailing_tracking_events", ident2: "trigger_update_optimal_time_on_open",
		},
		{
			name: "drop trigger without IF EXISTS is unknown",
			sql:  `DROP TRIGGER trigger_update_optimal_time_on_open ON mailing_tracking_events`,
			kind: migStmtUnknown,
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
			// SUPERSEDED by REQ-094 (2026-09-01): the single-column DO wrapper
			// IS now recognized. Leaving it unknown is what made the tracking
			// ADD COLUMNs re-take an ACCESS EXCLUSIVE lock on a partitioned
			// 12-relation table on every boot. Multi-statement DO blocks are
			// still unknown — see TestClassifyMigrationStatement_DoBlockIdioms.
			name: "single-column do block is an add-column",
			sql:  `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT; EXCEPTION WHEN OTHERS THEN NULL; END $$`,
			kind: migStmtDoAddColumn, ident1: "mailing_tracking_events", ident2: "email",
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

// =============================================================================
// REQ-094 — DO-block probes, the partitioned CONCURRENTLY plan, and the
// regression guard that keeps the pruned entries out of the 5s slice.
// =============================================================================

// Samples are verbatim from the migration slice as it stood on 2026-09-01.
func TestClassifyMigrationStatement_DoBlockIdioms(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   migrationStatementKind
		ident1 string
		ident2 string
	}{
		{
			name:   "do-block add column (add_tracking_metadata_col)",
			sql:    `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS metadata JSONB DEFAULT '{}'; EXCEPTION WHEN OTHERS THEN NULL; END $$`,
			kind:   migStmtDoAddColumn,
			ident1: "mailing_tracking_events", ident2: "metadata",
		},
		{
			name:   "do-block add column (add_tracking_sending_ip)",
			sql:    `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS sending_ip VARCHAR(45); EXCEPTION WHEN OTHERS THEN NULL; END $$`,
			kind:   migStmtDoAddColumn,
			ident1: "mailing_tracking_events", ident2: "sending_ip",
		},
		{
			// add_escalation_cols_throttle_state: THREE columns in one block.
			// Recognizing it would skip the block once the FIRST column exists
			// and silently drop the other two.
			name: "multi-column do-block is never classified",
			sql: `DO $$ BEGIN
				ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS escalation_adj DOUBLE PRECISION NOT NULL DEFAULT 1.0;
				ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS escalation_cooldown_until TIMESTAMPTZ;
				ALTER TABLE mailing_engine_throttle_agent_state ADD COLUMN IF NOT EXISTS last_escalation_at TIMESTAMPTZ;
			END $$`,
			kind: migStmtUnknown,
		},
		{
			// A DO block that does more than ALTER (ensure_tracking_email_col's
			// old per-partition loop shape) must stay unrecognized.
			name: "do-block with a loop is never classified",
			sql: `DO $$ DECLARE part regclass; BEGIN
				ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS email TEXT;
				FOR part IN SELECT inhrelid::regclass FROM pg_inherits LOOP
					EXECUTE format('ALTER TABLE %s ADD COLUMN IF NOT EXISTS email TEXT', part);
				END LOOP;
			END $$`,
			kind: migStmtUnknown,
		},
		{
			name:   "do-block named index (idx_queue_active_by_campaign)",
			sql:    `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_queue_active_by_campaign') THEN CREATE INDEX idx_queue_active_by_campaign ON mailing_campaign_queue (campaign_id) WHERE status IN ('queued','claimed'); END IF; END $$`,
			kind:   migStmtDoNamedIndex,
			ident1: "idx_queue_active_by_campaign",
		},
		{
			// criticalSendPathDDL's idx_waves_completed_at carries a SECOND
			// guard (a reltuples size check) that deliberately suppresses the
			// in-line build on a large DB. The probe must not match it, or the
			// entry's behaviour would change on a path it does not own.
			name: "do-block index guard with an extra condition is not classified",
			sql: `DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_waves_completed_at')
				   AND COALESCE((SELECT reltuples FROM pg_class WHERE relname = 'mailing_campaign_waves'), 0) < 1000000 THEN
					CREATE INDEX idx_waves_completed_at ON mailing_campaign_waves(status, completed_at)
						WHERE status = 'completed' AND enqueued_recipients > 0;
				END IF;
			END $$`,
			kind: migStmtUnknown,
		},
		{
			// A pg_constraint-guarded block (pdbb_*) is a different idiom and
			// must not be mistaken for the index one.
			name: "do-block guarded on pg_constraint is not an index",
			sql:  `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='pdbb_minmax') THEN ALTER TABLE partner_drip_brand_budgets ADD CONSTRAINT pdbb_minmax CHECK (min_budget <= max_budget); END IF; END $$`,
			kind: migStmtUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, i1, i2 := classifyMigrationStatement(tc.sql)
			if kind != tc.kind {
				t.Fatalf("kind = %v, want %v", kind, tc.kind)
			}
			if tc.kind != migStmtUnknown && (i1 != tc.ident1 || i2 != tc.ident2) {
				t.Fatalf("idents = (%q,%q), want (%q,%q)", i1, i2, tc.ident1, tc.ident2)
			}
		})
	}
}

func TestMigrationSkipProbe_DoBlockAddColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const stmt = `DO $$ BEGIN ALTER TABLE mailing_tracking_events ADD COLUMN IF NOT EXISTS event_time TIMESTAMPTZ DEFAULT NOW(); EXCEPTION WHEN OTHERS THEN NULL; END $$`

	// POSITIVE: column present (prod: event_time exists on 12/12 relations) →
	// one catalog read, no DDL, no lock.
	mock.ExpectQuery(`information_schema\.columns`).
		WithArgs("mailing_tracking_events", "event_time").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	if !migrationSkipProbe(db, stmt) {
		t.Fatal("DO-block ADD COLUMN with an existing column must be skipped")
	}

	// NEGATIVE: column absent → execute, exactly as before the probe existed.
	mock.ExpectQuery(`information_schema\.columns`).
		WithArgs("mailing_tracking_events", "event_time").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	if migrationSkipProbe(db, stmt) {
		t.Fatal("DO-block ADD COLUMN with a missing column must execute")
	}

	// NEGATIVE: probe error → fail open.
	mock.ExpectQuery(`information_schema\.columns`).WillReturnError(errTestProbe)
	if migrationSkipProbe(db, stmt) {
		t.Fatal("probe error must fail open into execution")
	}

	// NEGATIVE: a multi-column DO block never reaches the DB at all.
	if migrationSkipProbe(db, `DO $$ BEGIN
		ALTER TABLE t ADD COLUMN IF NOT EXISTS a INT;
		ALTER TABLE t ADD COLUMN IF NOT EXISTS b INT;
	END $$`) {
		t.Fatal("multi-column DO block must never be skipped")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSkipProbe_DoBlockNamedIndex(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const stmt = `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_waves_active_by_campaign') THEN CREATE INDEX idx_waves_active_by_campaign ON mailing_campaign_waves (campaign_id); END IF; END $$`

	// POSITIVE: valid index → skip.
	mock.ExpectQuery(`pg_index`).
		WithArgs("idx_waves_active_by_campaign").
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(true))
	if !migrationSkipProbe(db, stmt) {
		t.Fatal("DO-block named index that already exists must be skipped")
	}

	// NEGATIVE: an INVALID leftover must not suppress the rebuild.
	mock.ExpectQuery(`pg_index`).
		WithArgs("idx_waves_active_by_campaign").
		WillReturnRows(sqlmock.NewRows([]string{"valid"}).AddRow(false))
	if migrationSkipProbe(db, stmt) {
		t.Fatal("invalid index must not be skipped")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentIndexPlan_OrdinaryTableIsIdentity — the fast path must not
// change behaviour for the 20+ existing specs.
func TestConcurrentIndexPlan_OrdinaryTableIsIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const stmt = `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pcq_mailed_at ON partner_clean_queue (mailed_at) WHERE mailed_at IS NOT NULL`
	mock.ExpectQuery(`relkind`).WithArgs("partner_clean_queue").
		WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("r"))

	plan := concurrentIndexPlan(db, stmt)
	if len(plan) != 1 || plan[0] != stmt {
		t.Fatalf("ordinary table must yield the statement unchanged, got %#v", plan)
	}

	// An unparseable statement is also the identity — never withhold a build.
	if plan := concurrentIndexPlan(db, `CREATE INDEX CONCURRENTLY idx_x ON t (a)`); len(plan) != 1 {
		t.Fatalf("unparseable statement must yield the identity, got %#v", plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentIndexPlan_PartitionedParentExpands — PostgreSQL 16 rejects
// CREATE INDEX CONCURRENTLY on a partitioned table, so idx_mte_click_verdict
// must be expanded, not passed through.
func TestConcurrentIndexPlan_PartitionedParentExpands(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmt := strings.Replace(clickVerdictIndexDDL,
		"CREATE INDEX IF NOT EXISTS", "CREATE INDEX CONCURRENTLY IF NOT EXISTS", 1)

	mock.ExpectQuery(`relkind`).WithArgs("mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"relkind"}).AddRow("p"))
	mock.ExpectQuery(`pg_inherits`).WithArgs("mailing_tracking_events").
		WillReturnRows(sqlmock.NewRows([]string{"relname"}).
			AddRow("mailing_tracking_events_2026_08").
			AddRow("mailing_tracking_events_2026_09"))

	plan := concurrentIndexPlan(db, stmt)
	// 2 child builds + 1 ON ONLY parent + 2 guarded ATTACHes.
	if len(plan) != 5 {
		t.Fatalf("plan length = %d, want 5:\n%s", len(plan), strings.Join(plan, "\n---\n"))
	}
	for i, want := range []string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mte_click_verdict_2026_08 ON mailing_tracking_events_2026_08",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mte_click_verdict_2026_09 ON mailing_tracking_events_2026_09",
		"CREATE INDEX IF NOT EXISTS idx_mte_click_verdict ON ONLY mailing_tracking_events",
		"ATTACH PARTITION idx_mte_click_verdict_2026_08",
		"ATTACH PARTITION idx_mte_click_verdict_2026_09",
	} {
		if !strings.Contains(plan[i], want) {
			t.Fatalf("plan[%d] = %q, want it to contain %q", i, plan[i], want)
		}
	}
	// Every child build and the parent must carry the partial predicate, or
	// the ATTACH is rejected for a mismatched definition.
	for _, stmt := range plan[:3] {
		if !strings.Contains(stmt, "event_type = 'clicked'") {
			t.Fatalf("index definition lost its WHERE clause: %q", stmt)
		}
	}
	// No plain (non-CONCURRENT) build may target the partitioned PARENT with
	// data — only the catalog-only ON ONLY form.
	if strings.Contains(plan[2], "CONCURRENTLY") || !strings.Contains(plan[2], "ON ONLY") {
		t.Fatalf("parent statement must be the ON ONLY form, got %q", plan[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestEnsurePartitionedColumns_PresentColumnIssuesNoDDL — the whole point of
// the retry path: once the column exists it costs one catalog read forever.
func TestEnsurePartitionedColumns_PresentColumnIssuesNoDDL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for range partitionedColumnSpecs {
		mock.ExpectQuery(`information_schema\.columns`).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	}
	ensurePartitionedColumns(db)
	// sqlmock fails ExpectationsWereMet if any unexpected Exec was issued.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationsPrunedEntriesStayPruned — REQ-094's regression guard. Each name
// below re-failed on EVERY boot (verified in the 2026-09-01 CloudWatch boot log)
// and was pruned or re-vehicled with prod catalog evidence. Re-registering one
// in the 5s slice reinstates a guaranteed failure, and for the send-path tables
// a per-boot ACCESS EXCLUSIVE attempt.
func TestMigrationsPrunedEntriesStayPruned(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Skipf("cannot read main.go: %v", err)
	}
	// Scope to the 5s slice only: concurrentIndexSpecs and criticalSendPathDDL
	// use the same {"name", `sql`} literal shape, and two of these names now
	// legitimately live there.
	body := string(src)
	start := strings.Index(body, "migrations := []struct {")
	end := strings.Index(body, "// Use a dedicated connection with a short statement timeout")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("cannot locate the runStartupMigrations slice in main.go (start=%d end=%d)", start, end)
	}
	body = body[start:end]
	for _, name := range []string{
		// impossible: permissions / ownership / CHECK violation / SQL defect
		"ensure_pgcrypto", "drop_old_list_counts_fn", "fix_list_ids_names_to_uuids",
		"seed_ghost_visitor_segment_row", "seed_pipeline_domain_lists",
		// validating ADD CONSTRAINT on hot send-path tables
		"readd_status_chk", "readd_status_chk_v2", "readd_execution_mode_chk",
		"outbox_add_expanded_status_chk", "widen_status_col",
		"phase21_default_use_master_selection_true",
		// backfills — never belong in the 5s slice
		"backfill_mpp_opens_sent_v3", "backfill_inbox_profile_counts_v1",
		"backfill_inbox_profile_engagement_v1", "consolidate_suppression_legacy",
		"sync_bot_clickers_to_global_supp", "bot_backstop_purge_segment_members",
		"bot_backstop_reset_score_local", "sds_state_backfill_from_history",
		"phase21_backfill_source_system", "phase21_backfill_source_detail",
		"phase21_backfill_imported_at", "phase21_backfill_imported_from_list_id",
		"set_test_subscriber_names", "hide_duplicate_lists",
		"purge_besmed_tracking_events", "may29_seg_archive_stale_partner_waves",
		// re-vehicled out of the slice
		"ensure_tracking_email_col", "add_tracking_event_time_col",
		"add_idx_mte_click_verdict", "idx_subscribers_audience_scan",
		"phase21_idx_source_metadata_gin",
	} {
		if strings.Contains(body, `{"`+name+`",`) {
			t.Errorf("%s is registered as a startup-migration entry again — see REQ-094; it re-fails on every boot", name)
		}
	}
}

// TestRevehicledTargetsHaveAHome — the other half of the guard: pruning is only
// safe if the three re-vehicled indexes and the two partitioned columns still
// have a vehicle that can actually apply them.
func TestRevehicledTargetsHaveAHome(t *testing.T) {
	names := map[string]bool{}
	for _, s := range concurrentIndexSpecs {
		names[s.name] = true
	}
	for _, want := range []string{
		"idx_mte_click_verdict", "idx_subscribers_audience_scan",
		"idx_subscribers_source_metadata_gin",
	} {
		if !names[want] {
			t.Errorf("%s left the migration slice but is not in concurrentIndexSpecs", want)
		}
	}
	cols := map[string]bool{}
	for _, s := range partitionedColumnSpecs {
		cols[s.table+"."+s.column] = true
	}
	for _, want := range []string{
		"mailing_tracking_events.email", "mailing_tracking_events.event_time",
	} {
		if !cols[want] {
			t.Errorf("%s left the migration slice but is not in partitionedColumnSpecs", want)
		}
	}
}

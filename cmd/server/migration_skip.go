package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// =============================================================================
// Startup-migration skip probe (AAR action item 1, 2026-06-10)
// =============================================================================
// runStartupMigrations re-executes ~530 statements on every boot. Most are
// idempotent DDL whose effect already exists — but "idempotent" is not
// "free": even a no-op ALTER TABLE ... ADD COLUMN IF NOT EXISTS demands a
// brief ACCESS EXCLUSIVE lock on its table. Replayed hundreds of times
// against the hot mailing_campaign_queue table on a busy primary, those
// no-ops queue behind live traffic, time out at the runner's 5s budget, and
// stall everything queued behind their lock request — the rolling brownout
// that turned the 2026-06-10 deploy into a send outage.
//
// The probe classifies the dominant DDL shapes and checks the catalog
// (cheap reads, no table locks) to decide whether the statement can be
// skipped outright. Everything it does not positively recognize — DO blocks,
// seeds, backfills, multi-action ALTERs — executes exactly as before, and
// any probe error fails OPEN (execute). Skipping is a pure fast-path.

type migrationStatementKind int

const (
	migStmtUnknown migrationStatementKind = iota
	migStmtCreateTable
	migStmtCreateIndex
	migStmtAddColumn
	migStmtDropConstraint
	migStmtDropTrigger
	// The two DO-block idioms below are the reason 41 entries re-executed on
	// every boot (deploy-config SEV-2 #4, 2026-09-01): the classifier only
	// looked at leading keywords, so `DO $$ … ADD COLUMN IF NOT EXISTS … $$`
	// and the pg_indexes-guarded CREATE INDEX idiom were both migStmtUnknown
	// and paid a full lock-taking round trip against the hot partitioned
	// mailing_tracking_events every single boot. They probe the same catalogs
	// as their plain-SQL equivalents.
	migStmtDoAddColumn
	migStmtDoNamedIndex
)

var (
	reMigCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_.]+)`)
	reMigCreateIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)\s`)
	// DO $$ BEGIN ALTER TABLE t ADD COLUMN IF NOT EXISTS c <type>;
	//   EXCEPTION WHEN OTHERS THEN NULL; END $$
	// Deliberately anchored on the whole block: the EXCEPTION clause must
	// follow the FIRST statement terminator, so a multi-ALTER block (e.g.
	// add_escalation_cols_throttle_state, three ADD COLUMNs) cannot match and
	// be skipped on the strength of its first column alone — the exact
	// silent-drop failure `startup-migration-footguns` warns about.
	reMigDoAddColumn = regexp.MustCompile(`(?is)^\s*DO\s+\$\$\s*BEGIN\s+ALTER\s+TABLE\s+(?:ONLY\s+)?([a-z0-9_.]+)\s+ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)[^;]*;\s*EXCEPTION\s+WHEN\s+OTHERS\s+THEN\s+NULL\s*;\s*END\s*\$\$\s*;?\s*$`)
	// DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname =
	//   'idx_x') THEN CREATE [UNIQUE] INDEX idx_x … ; END IF; END $$
	// The block's own guard and this probe ask the same question, so skipping
	// is behaviour-preserving; what it saves is the round trip and the SHARE
	// lock CREATE INDEX takes on a constantly-written table.
	reMigDoNamedIndex = regexp.MustCompile(`(?is)^\s*DO\s+\$\$\s*BEGIN\s+IF\s+NOT\s+EXISTS\s*\(\s*SELECT\s+1\s+FROM\s+pg_indexes\s+WHERE\s+indexname\s*=\s*'([a-z0-9_]+)'\s*\)\s+THEN\s+CREATE\s+(?:UNIQUE\s+)?INDEX\s`)
	// Single-action ADD COLUMN only: the tail may contain commas inside
	// parentheses (e.g. DECIMAL(5,2)) but a top-level comma means a
	// multi-action ALTER, which we never skip.
	reMigAddColumn      = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?([a-z0-9_.]+)\s+ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)(?:[^,()]|\([^)]*\))*$`)
	reMigDropConstraint = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?([a-z0-9_.]+)\s+DROP\s+CONSTRAINT\s+IF\s+EXISTS\s+([a-z0-9_]+)\s*$`)
	reMigDropTrigger    = regexp.MustCompile(`(?is)^\s*DROP\s+TRIGGER\s+IF\s+EXISTS\s+([a-z0-9_]+)\s+ON\s+([a-z0-9_.]+)\s*$`)
)

// classifyMigrationStatement recognizes the skippable statement shapes.
// ident1/ident2 are the relation/column (or index/constraint) names, with
// any schema qualifier stripped for catalog lookups.
func classifyMigrationStatement(sqlText string) (kind migrationStatementKind, ident1, ident2 string) {
	if m := reMigCreateTable.FindStringSubmatch(sqlText); m != nil {
		return migStmtCreateTable, m[1], ""
	}
	if m := reMigCreateIndex.FindStringSubmatch(sqlText); m != nil {
		return migStmtCreateIndex, m[1], ""
	}
	if m := reMigAddColumn.FindStringSubmatch(sqlText); m != nil {
		return migStmtAddColumn, stripSchemaQualifier(m[1]), m[2]
	}
	if m := reMigDoAddColumn.FindStringSubmatch(sqlText); m != nil {
		// Belt-and-braces on top of the anchored regex: never skip a block
		// that carries more than one column.
		if strings.Count(strings.ToUpper(sqlText), "ADD COLUMN") == 1 {
			return migStmtDoAddColumn, stripSchemaQualifier(m[1]), m[2]
		}
	}
	if m := reMigDoNamedIndex.FindStringSubmatch(sqlText); m != nil {
		return migStmtDoNamedIndex, m[1], ""
	}
	if m := reMigDropConstraint.FindStringSubmatch(sqlText); m != nil {
		return migStmtDropConstraint, stripSchemaQualifier(m[1]), m[2]
	}
	if m := reMigDropTrigger.FindStringSubmatch(sqlText); m != nil {
		// ident1 = table, ident2 = trigger (mirrors the constraint case).
		return migStmtDropTrigger, stripSchemaQualifier(m[2]), m[1]
	}
	return migStmtUnknown, "", ""
}

func stripSchemaQualifier(rel string) string {
	if i := strings.LastIndex(rel, "."); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// migrationSkipProbe reports whether the statement's effect already exists.
// Probes are bounded catalog reads; on any error the answer is "don't skip".
func migrationSkipProbe(db *sql.DB, sqlText string) bool {
	kind, ident1, ident2 := classifyMigrationStatement(sqlText)
	if kind == migStmtUnknown {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var skip bool
	var err error
	switch kind {
	case migStmtCreateTable:
		err = db.QueryRowContext(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, ident1).Scan(&skip)
	case migStmtCreateIndex, migStmtDoNamedIndex:
		// Only a VALID index counts: an invalid leftover from an
		// interrupted CONCURRENTLY build must not suppress a rebuild path.
		err = db.QueryRowContext(ctx, `
			SELECT COALESCE((
				SELECT i.indisvalid
				FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
				WHERE c.relname = $1
				LIMIT 1
			), FALSE)`, ident1).Scan(&skip)
	case migStmtAddColumn, migStmtDoAddColumn:
		err = db.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM information_schema.columns
				WHERE table_name = $1 AND column_name = $2
			)`, ident1, ident2).Scan(&skip)
	case migStmtDropConstraint:
		// Skippable when the constraint is already gone.
		err = db.QueryRowContext(ctx, `
			SELECT NOT EXISTS(
				SELECT 1
				FROM pg_constraint con
				JOIN pg_class rel ON rel.oid = con.conrelid
				WHERE rel.relname = $1 AND con.conname = $2
			)`, ident1, ident2).Scan(&skip)
	case migStmtDropTrigger:
		// Skippable when the trigger is already gone — a DROP TRIGGER still
		// needs the table lock even when the trigger doesn't exist, so the
		// probe matters on every boot after the first success.
		err = db.QueryRowContext(ctx, `
			SELECT NOT EXISTS(
				SELECT 1
				FROM pg_trigger tg
				JOIN pg_class rel ON rel.oid = tg.tgrelid
				WHERE rel.relname = $1 AND tg.tgname = $2 AND NOT tg.tgisinternal
			)`, ident1, ident2).Scan(&skip)
	}
	if err != nil {
		return false
	}
	return skip
}

// =============================================================================
// Partitioned-parent ADD COLUMN — the lock_timeout retry path (REQ-094 DoD 2)
// =============================================================================
// `mailing_tracking_events` is PARTITION BY RANGE (event_at) with 11 live
// partitions (verified on prod 2026-09-01). PostgreSQL refuses ADD COLUMN on a
// partition directly, so the only route is the parent, and the parent ALTER
// takes ACCESS EXCLUSIVE on the parent AND every partition at once. Inside
// runStartupMigrations' 5s statement budget that lock is never granted against
// live tracking ingest: `ensure_tracking_email_col` logged
// "TIMEOUT (skipped — will retry next boot)" on every boot for weeks and the
// column is still absent on 0/12 relations, while a queued ACCESS EXCLUSIVE
// barricades every later lock request on the table — the 2026-08-20 mechanism
// that destroyed ~10 min of SES events (memory `startup-migration-lock-barricade`).
//
// This path fixes both halves:
//   - one bounded catalog read decides whether any DDL is needed at all, so a
//     landed column costs nothing (no lock, no ALTER, ever again);
//   - when the column IS missing, the attempt runs with an explicit
//     lock_timeout, so it fails fast instead of queueing. Losing the lock is
//     the DESIGNED outcome, not an error: it is logged as DEFERRED (no "ERROR"
//     token, so it does not pollute the boot-error filter) and retried.
//
// NOT criticalSendPathDDL: `send_worker.go`'s sent/bounced INSERTs
// (:2429/:2435/:2750) key on subscriber_id and never reference `email`, so no
// worker fails without it — it does not meet that vehicle's "schema the send
// path cannot run without" bar, and holding boot synchronously on a 12-relation
// ACCESS EXCLUSIVE is exactly the barricade shape to avoid.
var partitionedColumnSpecs = []struct {
	name    string
	table   string
	column  string
	ddlType string
}{
	// Written by HandleSendTransactional (internal/api/mailing_sending.go:647)
	// and the legacy SparkPost loop (:1235/:1246/:1270); absent on prod parent
	// + all 11 partitions, so every one of those tracking rows is dropped.
	{"ensure_tracking_email_col", "mailing_tracking_events", "email", "TEXT"},
	// Already present on 12/12 relations in prod — listed here so the entry has
	// ONE home; the catalog probe makes it a no-op read.
	{"add_tracking_event_time_col", "mailing_tracking_events", "event_time", "TIMESTAMPTZ DEFAULT NOW()"},
}

const (
	// Short enough that a queued ACCESS EXCLUSIVE can never barricade the
	// table for a human-visible interval; long enough to win the gap between
	// send-worker claim batches.
	partitionedColumnLockTimeout = "500ms"
	partitionedColumnAttempts    = 4
)

// ensurePartitionedColumns applies partitionedColumnSpecs outside the 5s slice.
// Called from runStartupMigrations while the migration advisory lock is held,
// so the two ECS tasks cannot race the same ALTER (the other half of the
// 2026-08-20 incident). Never fatal: a column that cannot be won this boot is
// retried on the next one, exactly as before — but without the 5s barricade.
func ensurePartitionedColumns(db *sql.DB) {
	for _, spec := range partitionedColumnSpecs {
		probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var present bool
		err := db.QueryRowContext(probeCtx, `
			SELECT EXISTS(
				SELECT 1 FROM information_schema.columns
				WHERE table_name = $1 AND column_name = $2
			)`, spec.table, spec.column).Scan(&present)
		cancel()
		if err != nil {
			log.Printf("[StartupMigration] %s: catalog probe failed: %v — deferring to next boot", spec.name, err)
			continue
		}
		if present {
			continue // one catalog read; no DDL, no lock
		}

		ddl := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
			spec.table, spec.column, spec.ddlType)
		var lastErr error
		for attempt := 1; attempt <= partitionedColumnAttempts; attempt++ {
			conn, connErr := db.Conn(context.Background())
			if connErr != nil {
				lastErr = connErr
				break
			}
			execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, sErr := conn.ExecContext(execCtx, `SET lock_timeout = '`+partitionedColumnLockTimeout+`'; SET statement_timeout = '10s'`); sErr != nil {
				log.Printf("[StartupMigration] %s: session setup failed: %v", spec.name, sErr)
			}
			_, lastErr = conn.ExecContext(execCtx, ddl)
			execCancel()
			conn.Close()
			if lastErr == nil {
				log.Printf("[StartupMigration] %s: applied on attempt %d (%s.%s)", spec.name, attempt, spec.table, spec.column)
				break
			}
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if lastErr != nil {
			// "DEFERRED", not "ERROR": losing a bounded lock race is the
			// designed behaviour of this path.
			log.Printf("[StartupMigration] %s: DEFERRED after %d attempts at lock_timeout=%s (%v) — retried next boot",
				spec.name, partitionedColumnAttempts, partitionedColumnLockTimeout, lastErr)
		}
	}
}

// =============================================================================
// CONCURRENTLY builds against a partitioned parent (REQ-094 DoD 2)
// =============================================================================
// PostgreSQL 16 still refuses CREATE INDEX CONCURRENTLY on a partitioned table
// ("cannot create index on partitioned table … concurrently"), so moving
// idx_mte_click_verdict into concurrentIndexSpecs as a plain statement would
// swap one guaranteed-failing boot entry for another. The supported route is
// per-partition CONCURRENTLY builds, then an ON ONLY parent index, then ATTACH
// — after which every partition created later by worker.EnsurePartitions
// inherits the index automatically.
var reConcurrentIndexStmt = regexp.MustCompile(`(?is)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)\s+ON\s+([a-z0-9_.]+)\s+(.*?)\s*;?\s*$`)

// concurrentIndexPlan returns the statements that build one concurrentIndexSpecs
// entry. For an ordinary table that is the spec's own statement, unchanged. For
// a partitioned parent it is the expanded, idempotent, re-runnable sequence.
// Any parse or catalog problem falls back to the original statement, so this is
// a pure fast path and can never withhold a build.
func concurrentIndexPlan(db *sql.DB, stmt string) []string {
	single := []string{stmt}
	m := reConcurrentIndexStmt.FindStringSubmatch(stmt)
	if m == nil {
		return single
	}
	unique, indexName, table, tail := m[1], m[2], m[3], m[4]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var relkind string
	if err := db.QueryRowContext(ctx,
		`SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&relkind); err != nil {
		return single
	}
	if relkind != "p" {
		return single // ordinary table: CONCURRENTLY works as written
	}

	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_class p
		JOIN pg_inherits i ON i.inhparent = p.oid
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE p.relname = $1
		ORDER BY c.relname`, stripSchemaQualifier(table))
	if err != nil {
		return single
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return single
		}
		parts = append(parts, p)
	}
	if rows.Err() != nil || len(parts) == 0 {
		return single
	}

	plan := make([]string, 0, 2*len(parts)+1)
	childNames := make([]string, 0, len(parts))
	for _, part := range parts {
		child := partitionIndexName(indexName, stripSchemaQualifier(table), part)
		childNames = append(childNames, child)
		plan = append(plan, fmt.Sprintf("CREATE %sINDEX CONCURRENTLY IF NOT EXISTS %s ON %s %s",
			unique, child, part, tail))
	}
	// ON ONLY: catalog-only, instant, and leaves the parent index INVALID
	// until every partition is attached — which is why it comes AFTER the
	// (slow) child builds, keeping the invalid window to seconds. The
	// ensureConcurrentIndexes validity probe drops-and-rebuilds a parent left
	// invalid by a killed process, so a partial run is recoverable.
	plan = append(plan, fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON ONLY %s %s",
		unique, indexName, table, tail))
	for _, child := range childNames {
		// ATTACH errors if the index is already a member, so guard it —
		// this whole sequence re-runs on every boot until the parent is valid.
		plan = append(plan, fmt.Sprintf(`DO $plan$ BEGIN
	IF to_regclass('%[1]s') IS NOT NULL AND NOT EXISTS (
		SELECT 1 FROM pg_inherits
		WHERE inhrelid = to_regclass('%[1]s') AND inhparent = to_regclass('%[2]s')
	) THEN
		EXECUTE 'ALTER INDEX %[2]s ATTACH PARTITION %[1]s';
	END IF;
END $plan$`, child, indexName))
	}
	return plan
}

// partitionIndexName derives a stable per-partition index name, bounded by
// PostgreSQL's 63-byte identifier limit.
func partitionIndexName(indexName, table, partition string) string {
	suffix := strings.TrimPrefix(partition, table+"_")
	if suffix == partition || suffix == "" {
		suffix = partition
	}
	name := indexName + "_" + suffix
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

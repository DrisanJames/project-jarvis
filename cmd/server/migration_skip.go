package main

import (
	"context"
	"database/sql"
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
)

var (
	reMigCreateTable = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_.]+)`)
	reMigCreateIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)\s`)
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
	case migStmtCreateIndex:
		// Only a VALID index counts: an invalid leftover from an
		// interrupted CONCURRENTLY build must not suppress a rebuild path.
		err = db.QueryRowContext(ctx, `
			SELECT COALESCE((
				SELECT i.indisvalid
				FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
				WHERE c.relname = $1
				LIMIT 1
			), FALSE)`, ident1).Scan(&skip)
	case migStmtAddColumn:
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

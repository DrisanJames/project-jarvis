package main

// WP-E (QA): the 2026-09-09 contract-fulfilment migration slice, held to the
// same gates every other REQ-118 migration is held to — and wired.
//
// WHY THIS FILE EXISTS. `contractFulfillmentMigrations` (main.go:3536) is a NEW
// top-level slice. Every migration gate this subsystem built —
// TestDripSupplyMigrationsAreIdempotentInForm,
// TestDripSupplyMigrationsCreateEverySchemaObject, and the double-apply
// integration gate via req118Statements() — iterates `dripSupplyMigrations` or
// `criticalSendPathDDL` by name. None of them can see the new slice, so its one
// entry shipped outside every check the slice next to it must pass. That is the
// "new code unreachable from the gate that would have caught it" shape, applied
// to schema.
//
// These run in the default suite: no build tag, no database.

import (
	"os"
	"strings"
	"testing"
)

// contractFulfillmentTables is every table the slice must create. Named here
// rather than derived from the SQL, so deleting an entry fails the test instead
// of shrinking the expectation with it.
var contractFulfillmentTables = []string{"drip_fulfillment_verdict"}

// verdictColumns is the column contract between the three parties that touch
// this table: WP-C's DDL writes it, agents/reporting/supply_reconcile.py
// inserts these fifteen names verbatim (verdict_upsert_sql), and
// internal/api/drip_supply_handlers.go's dripVerdictSQL selects fourteen of
// them. A column dropped or renamed on any one side is a runtime 500 on the
// Supply tab and a failed upsert in the nightly runner — neither of which any
// existing test would notice.
var verdictColumns = []string{
	"day", "sending_domain", "isp", "mode", "domain_contract_version",
	"promised", "minted", "sent", "delivered", "delivered_source",
	"tolerance_pct", "status", "reason", "window_closed_at", "computed_at",
}

func TestQA_ContractFulfillmentMigrationsPassTheREQ118Gates(t *testing.T) {
	if len(contractFulfillmentMigrations) == 0 {
		t.Fatal("contractFulfillmentMigrations is empty — the verdict table has no migration")
	}

	// Names must not collide with the slice they are appended after: both land
	// in the same `migrations` list, and migrationSkipProbe keys on the name.
	existing := map[string]bool{}
	for _, m := range dripSupplyMigrations {
		existing[m.name] = true
	}
	for _, m := range criticalSendPathDDL {
		existing[m.name] = true
	}

	seen := map[string]bool{}
	for _, m := range contractFulfillmentMigrations {
		if seen[m.name] {
			t.Errorf("duplicate migration name %q", m.name)
		}
		if existing[m.name] {
			t.Errorf("%q collides with a name already in dripSupplyMigrations/criticalSendPathDDL — the later entry silently shadows the earlier one in the merged list", m.name)
		}
		seen[m.name] = true

		// ONE STATEMENT PER ENTRY. migrationSkipProbe classifies an entry by its
		// LEADING keywords (migration_skip.go:41), so a CREATE TABLE carrying a
		// trailing CREATE INDEX is probed as a CREATE TABLE and the index
		// silently never lands once the table exists.
		if strings.Contains(stripSQLLineComments(m.sql), ";") {
			t.Errorf("%s: more than one statement in a single entry — everything after the first would silently never land", m.name)
		}

		// IDEMPOTENT IN FORM. runStartupMigrations re-executes its whole slice on
		// every boot.
		up := strings.ToUpper(strings.TrimSpace(m.sql))
		switch {
		case strings.HasPrefix(up, "CREATE TABLE"):
			if !strings.Contains(up, "CREATE TABLE IF NOT EXISTS") {
				t.Errorf("%s: CREATE TABLE without IF NOT EXISTS", m.name)
			}
		case strings.HasPrefix(up, "CREATE UNIQUE INDEX"), strings.HasPrefix(up, "CREATE INDEX"):
			if !strings.Contains(up, "IF NOT EXISTS") {
				t.Errorf("%s: CREATE INDEX without IF NOT EXISTS", m.name)
			}
			// A plain CREATE INDEX in the 5s slice builds with an ACCESS
			// EXCLUSIVE lock; heavy indexes belong in concurrentIndexSpecs.
			if strings.Contains(up, "CONCURRENTLY") {
				t.Errorf("%s: CREATE INDEX CONCURRENTLY cannot run inside runStartupMigrations — it belongs in concurrentIndexSpecs", m.name)
			}
		case strings.HasPrefix(up, "ALTER TABLE"):
			if !strings.Contains(up, "IF NOT EXISTS") {
				t.Errorf("%s: ALTER TABLE without IF NOT EXISTS", m.name)
			}
		default:
			t.Errorf("%s: unrecognized statement shape; add an idempotency rule for it before shipping:\n%s", m.name, m.sql)
		}

		// VEHICLE. The 5s slice must not touch the two tables whose size makes a
		// 5-second statement budget unsurvivable — a timeout there logs
		// "skipped, will retry next boot" and the DDL is silently absent forever.
		for _, big := range []string{"partner_clean_queue", "mailing_campaign_queue", "mailing_subscribers", "tracking_events"} {
			if strings.Contains(m.sql, big) {
				t.Errorf("%s: references %s from the 5s slice — wrong vehicle (criticalSendPathDDL or concurrentIndexSpecs)", m.name, big)
			}
		}
	}

	// Every declared table has exactly one creating entry.
	created := map[string]int{}
	for _, m := range contractFulfillmentMigrations {
		for _, tbl := range contractFulfillmentTables {
			if strings.Contains(m.sql, "CREATE TABLE IF NOT EXISTS "+tbl+" (") {
				created[tbl]++
			}
		}
	}
	for _, tbl := range contractFulfillmentTables {
		if created[tbl] != 1 {
			t.Errorf("table %s is created by %d entries, want exactly 1", tbl, created[tbl])
		}
	}
}

// WIRING. A migration slice that is declared and never appended is dead schema:
// the table never exists, and every reader of it 500s with `relation does not
// exist` on a screen nobody opens until the day it matters. `migrations` is a
// local inside runStartupMigrations, so the append is only assertable at the
// source level — the same idiom partner_drip_aol_rotate_mediation_test.go uses
// for the phase argument.
func TestQA_ContractFulfillmentMigrationsAreAppendedToTheStartupSlice(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	const appendLine = "migrations = append(migrations, contractFulfillmentMigrations...)"
	if !strings.Contains(body, appendLine) {
		t.Fatalf("main.go never appends contractFulfillmentMigrations to the startup slice — the verdict table would never be created and every reader of it would 500 on `relation does not exist`")
	}

	// Order matters only in one direction: the drip-supply slice must be
	// appended first, so a fresh-database boot creates the base tables before
	// anything that could depend on them.
	iDrip := strings.Index(body, "migrations = append(migrations, dripSupplyMigrations...)")
	iCF := strings.Index(body, appendLine)
	if iDrip < 0 {
		t.Fatal("dripSupplyMigrations is no longer appended — this guard is pinned to the wrong block")
	}
	if iCF < iDrip {
		t.Error("contractFulfillmentMigrations is appended BEFORE dripSupplyMigrations — the drip-supply schema must stay contiguous and in dependency order")
	}
}

// COLUMN CONTRACT. The DDL and the handler that projects it must agree, or the
// Supply tab's verdict returns a 500 that no Go test exercises (the handler's
// own tests feed sqlmock canned rows and therefore cannot notice a column that
// does not exist in the real table).
func TestQA_VerdictTableAndItsReaderAgreeOnEveryColumn(t *testing.T) {
	var ddl string
	for _, m := range contractFulfillmentMigrations {
		if strings.Contains(m.sql, "CREATE TABLE IF NOT EXISTS drip_fulfillment_verdict (") {
			ddl = m.sql
		}
	}
	if ddl == "" {
		t.Fatal("no entry creates drip_fulfillment_verdict")
	}
	stripped := stripSQLLineComments(ddl)
	for _, col := range verdictColumns {
		if !strings.Contains(stripped, col) {
			t.Errorf("column %s is missing from the DDL — supply_reconcile.py's verdict_upsert_sql inserts it by name and the insert would fail", col)
		}
	}

	// The Go reader. Read as source rather than imported because
	// dripVerdictSQL is unexported in package api.
	handler, err := os.ReadFile("../../internal/api/drip_supply_handlers.go")
	if err != nil {
		t.Skipf("cannot read the verdict handler (%v) — the DDL half still ran", err)
	}
	h := string(handler)
	if !strings.Contains(h, "FROM drip_fulfillment_verdict") {
		t.Fatal("nothing in internal/api reads drip_fulfillment_verdict — the table would be write-only schema")
	}
	// Everything the handler scans must exist in the DDL. `day` is the WHERE
	// key rather than a projected column, so it is checked above and not here.
	i := strings.Index(h, "const dripVerdictSQL = `")
	if i < 0 {
		t.Skip("dripVerdictSQL was renamed — the DDL half still ran")
	}
	rest := h[i+len("const dripVerdictSQL = `"):]
	j := strings.Index(rest, "`")
	if j < 0 {
		t.Fatal("dripVerdictSQL is unterminated")
	}
	projection := rest[:j]
	for _, col := range verdictColumns {
		if col == "day" {
			continue
		}
		if !strings.Contains(projection, col) {
			t.Errorf("dripVerdictSQL does not project %s — a column the DDL carries and the Python writer fills, read by nobody", col)
		}
	}
}

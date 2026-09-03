package main

// REQ-118 Drip Supply Chain schema gates (docs/DRIP_SUPPLY_CHAIN_DESIGN.md
// §1, WP1). No database: these parse the migration entries themselves and
// pin the three properties a boot cannot recover from —
//
//  1. VEHICLE. Every statement is in the slice whose budget it fits: new
//     tables in the 5s slice, the two partner_clean_queue ADD COLUMNs and
//     their NOT VALID constraint in criticalSendPathDDL (schema before the
//     binary that references them), idx_pcq_alloc in concurrentIndexSpecs.
//  2. IDEMPOTENT IN FORM. Every entry re-executes cleanly on the next boot:
//     IF NOT EXISTS, a pg_constraint DO guard, or INSERT … WHERE NOT EXISTS.
//     Never ON CONFLICT DO UPDATE on the rate seed — a boot must not revert
//     an operator-corrected rate.
//  3. ONE STATEMENT PER ENTRY. migrationSkipProbe classifies an entry by its
//     LEADING keywords (migration_skip.go:41), so a CREATE TABLE carrying a
//     trailing CREATE INDEX is probed as a CREATE TABLE and the index
//     silently never lands once the table exists — the exact silent-drop
//     `startup-migration-footguns` warns about.

import (
	"strings"
	"testing"
)

// req118StartupTables — every table §1 puts in the 5s slice, in the order
// the entries must create them (the three shadow twins are LIKE-derived, so
// they cannot precede their base table).
var req118StartupTables = []string{
	"drip_domain_contracts",
	"drip_dispatch_contracts",
	"drip_inventory_contracts",
	"drip_source_contracts",
	"drip_capacity_ledger",
	"drip_capacity_balance",
	"drip_lane_balance",
	"drip_supply_ledger",
	"drip_daily_plan",
	"drip_tick_outcomes",
	"drip_manual_revenue",
	"drip_cost_rates",
	"drip_capacity_ledger_shadow",
	"drip_daily_plan_shadow",
	"drip_supply_ledger_shadow",
}

// req118StartupIndexes — the four contract-uniqueness indexes and the four
// capacity-ledger indexes named in §1.1/§1.2.
var req118StartupIndexes = []string{
	"uq_drip_domain_contracts_active",
	"uq_drip_dispatch_contracts_active",
	"uq_drip_inventory_contracts_active",
	"uq_drip_source_contracts_active",
	"idx_drip_capacity_ledger_day_domain_isp",
	"idx_drip_capacity_ledger_day_lane_isp",
	"idx_drip_capacity_ledger_campaign",
	"idx_drip_capacity_ledger_reserved",
}

// stripSQLLineComments removes `-- …` tails so the one-statement check counts
// only real statement terminators (the column annotations carry semicolons).
func stripSQLLineComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDripSupplyMigrationsAreIdempotentInForm pins property 2 and 3 for the
// 5s-slice entries, and that names are unique (a duplicate name makes the
// boot log ambiguous and hides which statement failed).
func TestDripSupplyMigrationsAreIdempotentInForm(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range dripSupplyMigrations {
		if seen[m.name] {
			t.Errorf("duplicate migration name %q", m.name)
		}
		seen[m.name] = true

		// Property 3: exactly one statement. None of these entries is a DO
		// block, so any semicolon outside a -- comment means a second
		// statement rode in.
		if strings.Contains(stripSQLLineComments(m.sql), ";") {
			t.Errorf("%s: more than one statement in a single entry — migrationSkipProbe classifies by leading keyword, so everything after the first statement would silently never land", m.name)
		}

		// Property 2: idempotent in form.
		up := strings.ToUpper(m.sql)
		switch {
		case strings.HasPrefix(strings.TrimSpace(up), "CREATE TABLE"):
			if !strings.Contains(up, "CREATE TABLE IF NOT EXISTS") {
				t.Errorf("%s: CREATE TABLE without IF NOT EXISTS", m.name)
			}
		case strings.HasPrefix(strings.TrimSpace(up), "CREATE UNIQUE INDEX"),
			strings.HasPrefix(strings.TrimSpace(up), "CREATE INDEX"):
			if !strings.Contains(up, "IF NOT EXISTS") {
				t.Errorf("%s: CREATE INDEX without IF NOT EXISTS", m.name)
			}
		case strings.HasPrefix(strings.TrimSpace(up), "INSERT"):
			if !strings.Contains(up, "WHERE NOT EXISTS") {
				t.Errorf("%s: seed INSERT without a WHERE NOT EXISTS guard", m.name)
			}
			if strings.Contains(up, "DO UPDATE") {
				t.Errorf("%s: seed uses ON CONFLICT DO UPDATE — a boot would revert an operator-corrected value", m.name)
			}
		default:
			t.Errorf("%s: unrecognized statement shape; add an idempotency rule for it before shipping:\n%s", m.name, m.sql)
		}

		// Property 1 (negative half): the 5s slice never touches the
		// 13.7M-row claim table. Those statements belong to
		// criticalSendPathDDL / concurrentIndexSpecs.
		if strings.Contains(m.sql, "partner_clean_queue") {
			t.Errorf("%s: references partner_clean_queue from the 5s slice — wrong vehicle (§1.3)", m.name)
		}
	}
}

// TestDripSupplyMigrationsCreateEverySchemaObject pins that each §1 table and
// index has an entry, and that a shadow twin never precedes the base table
// its LIKE clause copies.
func TestDripSupplyMigrationsCreateEverySchemaObject(t *testing.T) {
	createdAt := map[string]int{}
	for i, m := range dripSupplyMigrations {
		for _, tbl := range req118StartupTables {
			if strings.Contains(m.sql, "CREATE TABLE IF NOT EXISTS "+tbl+" ") ||
				strings.Contains(m.sql, "CREATE TABLE IF NOT EXISTS "+tbl+" (") {
				if _, dup := createdAt[tbl]; dup {
					t.Errorf("table %s created by more than one entry", tbl)
				}
				createdAt[tbl] = i
			}
		}
	}
	for _, tbl := range req118StartupTables {
		if _, ok := createdAt[tbl]; !ok {
			t.Errorf("no runStartupMigrations entry creates %s (§1)", tbl)
		}
	}
	// LIKE-derived twins must come after their source.
	for shadow, base := range map[string]string{
		"drip_capacity_ledger_shadow": "drip_capacity_ledger",
		"drip_daily_plan_shadow":      "drip_daily_plan",
		"drip_supply_ledger_shadow":   "drip_supply_ledger",
	} {
		if createdAt[shadow] < createdAt[base] {
			t.Errorf("%s is created before %s — its LIKE clause would fail on a fresh DB", shadow, base)
		}
	}

	for _, idx := range req118StartupIndexes {
		found := false
		for _, m := range dripSupplyMigrations {
			if strings.Contains(m.sql, "IF NOT EXISTS "+idx+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no runStartupMigrations entry creates index %s (§1.1/§1.2)", idx)
		}
	}
}

// TestDripSupplyCostRateSeeds pins the five §1.2 rates and their values —
// the economics layer (WP8) reads them by key, so a typo here is a silently
// wrong dollar figure rather than an error.
func TestDripSupplyCostRateSeeds(t *testing.T) {
	var seed string
	for _, m := range dripSupplyMigrations {
		if m.name == "req118_seed_drip_cost_rates" {
			seed = m.sql
		}
	}
	if seed == "" {
		t.Fatal("req118_seed_drip_cost_rates entry missing")
	}
	for _, want := range []string{
		"('eo_per_verdict',      0.000244::numeric",
		"('eo_list_per_verdict', 0.0006::numeric",
		"('ses_per_message',     0.0001::numeric",
		"('pmta_per_message',    0::numeric",
		"('infra_monthly_usd',   NULL::numeric",
	} {
		if !strings.Contains(seed, want) {
			t.Errorf("cost-rate seed missing %q", want)
		}
	}
}

// TestDripSupplyPCQVehicles pins property 1 for §1.3: the two columns and the
// constraint are send-path-critical, the index is concurrent, and neither
// appears in the other's slice.
func TestDripSupplyPCQVehicles(t *testing.T) {
	crit := map[string]string{}
	for _, m := range criticalSendPathDDL {
		crit[m.name] = m.sql
	}

	for name, col := range map[string]string{
		"req118_pcq_capacity_allocation_id": "capacity_allocation_id",
		"req118_pcq_supply_reservation_id":  "supply_reservation_id",
	} {
		sql, ok := crit[name]
		if !ok {
			t.Errorf("criticalSendPathDDL missing %s (§1.3)", name)
			continue
		}
		want := "ALTER TABLE partner_clean_queue ADD COLUMN IF NOT EXISTS " + col + " UUID"
		if sql != want {
			t.Errorf("%s: want %q, got %q", name, want, sql)
		}
		// Nullable, no DEFAULT — the property that makes it catalog-only on
		// a 13.7M-row table. A DEFAULT or NOT NULL would rewrite the heap
		// and blow the 20s statement budget.
		if strings.Contains(strings.ToUpper(sql), "DEFAULT") || strings.Contains(strings.ToUpper(sql), "NOT NULL") {
			t.Errorf("%s: must stay nullable with no default (instant ADD COLUMN); got %q", name, sql)
		}
		// The skip probe must recognize it, or every boot re-takes an
		// ACCESS EXCLUSIVE lock on the claim table for a no-op.
		if kind, tbl, gotCol := classifyMigrationStatement(sql); kind != migStmtAddColumn || tbl != "partner_clean_queue" || gotCol != col {
			t.Errorf("%s: not recognized by the skip probe (kind=%v tbl=%q col=%q)", name, kind, tbl, gotCol)
		}
	}

	constraint, ok := crit["req118_pcq_claim_requires_allocation"]
	if !ok {
		t.Fatal("criticalSendPathDDL missing req118_pcq_claim_requires_allocation (§1.3)")
	}
	for _, want := range []string{
		"pg_constraint WHERE conname = 'pcq_claim_requires_allocation'",
		"ADD CONSTRAINT pcq_claim_requires_allocation",
		"CHECK (status <> 'claimed' OR capacity_allocation_id IS NOT NULL OR claimed_at < '" + pcqAllocationFence + "')",
		"NOT VALID",
	} {
		if !strings.Contains(constraint, want) {
			t.Errorf("constraint entry missing %q:\n%s", want, constraint)
		}
	}
	// Without NOT VALID the ALTER scans and verifies 13.7M rows at boot.
	if !strings.Contains(constraint, "NOT VALID;") {
		t.Error("constraint must be added NOT VALID (§1.3 / §10): a validating ADD CONSTRAINT scans the whole 14 GB queue inside the 20s budget")
	}
	// Idempotency is the DO guard's job — ADD CONSTRAINT has no IF NOT EXISTS.
	if !strings.HasPrefix(strings.TrimSpace(constraint), "DO $$") {
		t.Error("constraint entry must be wrapped in the pg_constraint DO guard — a bare ADD CONSTRAINT errors on every boot after the first")
	}

	var allocIdx string
	for _, s := range concurrentIndexSpecs {
		if s.name == "idx_pcq_alloc" {
			allocIdx = s.sql
		}
	}
	if allocIdx == "" {
		t.Fatal("concurrentIndexSpecs missing idx_pcq_alloc (§1.3)")
	}
	want := "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pcq_alloc ON partner_clean_queue (capacity_allocation_id) WHERE capacity_allocation_id IS NOT NULL"
	if allocIdx != want {
		t.Errorf("idx_pcq_alloc: want %q, got %q", want, allocIdx)
	}

	// Vehicle separation, both directions.
	for _, m := range criticalSendPathDDL {
		if strings.Contains(m.sql, "idx_pcq_alloc") {
			t.Errorf("%s: idx_pcq_alloc belongs in concurrentIndexSpecs — a lock-taking build on partner_clean_queue is the 2026-08-20 barricade", m.name)
		}
	}
	for _, s := range concurrentIndexSpecs {
		if strings.Contains(s.sql, "pcq_claim_requires_allocation") {
			t.Errorf("%s: the constraint belongs in criticalSendPathDDL", s.name)
		}
	}
}

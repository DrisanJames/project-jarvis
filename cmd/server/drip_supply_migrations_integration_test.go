//go:build integration

// REQ-118 WP1 double-apply gate (docs/DRIP_SUPPLY_CHAIN_DESIGN.md §8, WP1
// Definition of Done: "boots clean on a fresh local PG … idempotent on
// double boot").
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run DripSupplyMigrations ./cmd/server/ -v
//
// Creates a scratch database req118_test, stubs the ONE pre-existing table
// §1.3 alters (partner_clean_queue), then applies every REQ-118 statement
// from all three vehicles TWICE, asserting no error on either pass — the
// property a boot depends on, since runStartupMigrations and
// ensureSendPathSchema re-execute their whole slice on every start.
package main

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

const (
	req118AdminDSN   = "postgres://apex_user:apex_password@localhost:5432/postgres?sslmode=disable"
	req118ScratchDSN = "postgres://apex_user:apex_password@localhost:5432/req118_test?sslmode=disable"
)

func req118DSN(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// req118Statements returns every REQ-118 statement in boot order: the
// send-path-critical partner_clean_queue changes first (they land before any
// worker starts), then the 5s slice, then the concurrent index.
func req118Statements() []struct{ name, sql string } {
	out := []struct{ name, sql string }{}
	for _, m := range criticalSendPathDDL {
		if strings.HasPrefix(m.name, "req118_") {
			out = append(out, struct{ name, sql string }{m.name, m.sql})
		}
	}
	for _, m := range dripSupplyMigrations {
		out = append(out, struct{ name, sql string }{m.name, m.sql})
	}
	for _, s := range concurrentIndexSpecs {
		if s.name == "idx_pcq_alloc" {
			out = append(out, struct{ name, sql string }{s.name, s.sql})
		}
	}
	return out
}

func TestDripSupplyMigrationsDoubleApply(t *testing.T) {
	admin, err := sql.Open("postgres", req118DSN("REQ118_ADMIN_DSN", req118AdminDSN))
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB (%v). Start apex-postgres.", err)
	}

	if _, err := admin.Exec(`DROP DATABASE IF EXISTS req118_test`); err != nil {
		t.Fatalf("drop scratch db: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE req118_test`); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}

	db, err := sql.Open("postgres", req118DSN("REQ118_SCRATCH_DSN", req118ScratchDSN))
	if err != nil {
		t.Fatalf("open scratch db: %v", err)
	}
	defer db.Close()

	// The only pre-existing table §1.3 touches. Prod column types, so the
	// CHECK constraint resolves exactly as it will on the real table.
	if _, err := db.Exec(`CREATE TABLE partner_clean_queue (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		status TEXT NOT NULL,
		claimed_at TIMESTAMPTZ
	)`); err != nil {
		t.Fatalf("stub partner_clean_queue: %v", err)
	}

	stmts := req118Statements()
	// 24 in the 5s slice + 3 send-path-critical + 1 concurrent index.
	if len(stmts) != 28 {
		t.Fatalf("expected the full 28-statement REQ-118 set, got %d", len(stmts))
	}
	for pass := 1; pass <= 2; pass++ {
		for _, s := range stmts {
			if _, err := db.Exec(s.sql); err != nil {
				t.Fatalf("pass %d: %s failed: %v", pass, s.name, err)
			}
		}
		t.Logf("pass %d: %d statements applied with no error", pass, len(stmts))
	}

	// Every §1 table exists.
	for _, tbl := range req118StartupTables {
		var got string
		if err := db.QueryRow(`SELECT table_name FROM information_schema.tables
			WHERE table_schema='public' AND table_name=$1`, tbl).Scan(&got); err != nil {
			t.Errorf("table %s missing after double apply: %v", tbl, err)
		}
	}
	// Every §1 index exists, plus §1.3's.
	for _, idx := range append(append([]string{}, req118StartupIndexes...), "idx_pcq_alloc") {
		var got string
		if err := db.QueryRow(`SELECT indexname FROM pg_indexes WHERE indexname=$1`, idx).Scan(&got); err != nil {
			t.Errorf("index %s missing after double apply: %v", idx, err)
		}
	}
	// §1.3 columns.
	for _, col := range []string{"capacity_allocation_id", "supply_reservation_id"} {
		var typ string
		if err := db.QueryRow(`SELECT data_type FROM information_schema.columns
			WHERE table_schema='public' AND table_name='partner_clean_queue' AND column_name=$1`, col).Scan(&typ); err != nil {
			t.Errorf("partner_clean_queue.%s missing: %v", col, err)
		} else if typ != "uuid" {
			t.Errorf("partner_clean_queue.%s is %s, want uuid", col, typ)
		}
	}
	// §1.3 constraint, still NOT VALID (validation is a §7 step-5 operator
	// action; a boot must never scan the 13.7M-row queue).
	var condef string
	var convalidated bool
	if err := db.QueryRow(`SELECT pg_get_constraintdef(oid), convalidated FROM pg_constraint
		WHERE conname='pcq_claim_requires_allocation'`).Scan(&condef, &convalidated); err != nil {
		t.Fatalf("constraint pcq_claim_requires_allocation missing: %v", err)
	}
	if convalidated {
		t.Error("constraint is VALIDATED — it must ship NOT VALID (§1.3)")
	}
	t.Logf("constraint: %s", condef)

	// The seed is applied once, not once per boot.
	var rates int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_cost_rates`).Scan(&rates); err != nil {
		t.Fatal(err)
	}
	if rates != 5 {
		t.Errorf("drip_cost_rates has %d rows after two applies, want 5", rates)
	}
	var eo sql.NullFloat64
	if err := db.QueryRow(`SELECT value FROM drip_cost_rates WHERE key='eo_per_verdict'`).Scan(&eo); err != nil {
		t.Fatal(err)
	}
	if !eo.Valid || eo.Float64 != 0.000244 {
		t.Errorf("eo_per_verdict = %v, want 0.000244", eo)
	}
	var infra sql.NullFloat64
	if err := db.QueryRow(`SELECT value FROM drip_cost_rates WHERE key='infra_monthly_usd'`).Scan(&infra); err != nil {
		t.Fatal(err)
	}
	if infra.Valid {
		t.Errorf("infra_monthly_usd = %v, want NULL (not yet allocated)", infra.Float64)
	}

	// Behaviour of the constraint, both directions (non-negotiable 1).
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (status, claimed_at) VALUES ('claimed', '2026-09-03')`); err != nil {
		t.Errorf("a pre-cutover claimed row must still be legal: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (status, claimed_at) VALUES ('claimed', '2099-06-01')`); err == nil {
		t.Error("NEGATIVE CONTROL FAILED: a post-cutover claimed row with no capacity_allocation_id was accepted")
	}
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (status, claimed_at, capacity_allocation_id)
		VALUES ('claimed', '2099-06-01', gen_random_uuid())`); err != nil {
		t.Errorf("a post-cutover claimed row WITH an allocation must be legal: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO partner_clean_queue (status, claimed_at) VALUES ('ready', '2099-06-01')`); err != nil {
		t.Errorf("a non-claimed row must be unaffected: %v", err)
	}

	// Shadow twins carry the same columns as their base tables.
	for shadow, base := range map[string]string{
		"drip_capacity_ledger_shadow": "drip_capacity_ledger",
		"drip_daily_plan_shadow":      "drip_daily_plan",
		"drip_supply_ledger_shadow":   "drip_supply_ledger",
	} {
		var diff int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM (
				(SELECT column_name, data_type FROM information_schema.columns
				   WHERE table_schema='public' AND table_name=$1
				 EXCEPT
				 SELECT column_name, data_type FROM information_schema.columns
				   WHERE table_schema='public' AND table_name=$2)
				UNION ALL
				(SELECT column_name, data_type FROM information_schema.columns
				   WHERE table_schema='public' AND table_name=$2
				 EXCEPT
				 SELECT column_name, data_type FROM information_schema.columns
				   WHERE table_schema='public' AND table_name=$1)
			) d`, shadow, base).Scan(&diff); err != nil {
			t.Fatal(err)
		}
		if diff != 0 {
			t.Errorf("%s and %s differ by %d columns", shadow, base, diff)
		}
	}
}

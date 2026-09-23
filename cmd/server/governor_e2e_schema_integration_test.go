//go:build integration

// SendGovernor end-to-end schema prep (local apex-postgres ONLY — never prod):
//
//	docker exec apex-postgres psql -U apex_user -d postgres -c "CREATE DATABASE governor_e2e"
//	DATABASE_URL=postgres://apex_user:apex_password@localhost:5432/governor_e2e?sslmode=disable go run ./cmd/migrate/ migrations/
//	go test -tags integration -run GovernorE2ESchema ./cmd/server/ -v
//
// Applies the SAME boot vehicles prod applies (ensureSendPathSchema, then
// runStartupMigrations) to the scratch database so internal/api's
// governor_e2e integration test runs the deploy → finalize → waves → enqueue
// chain over the real schema.
package main

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

const governorE2EDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/governor_e2e?sslmode=disable"

func TestGovernorE2ESchema(t *testing.T) {
	dsn := os.Getenv("GOVERNOR_E2E_DSN")
	if dsn == "" {
		dsn = governorE2EDefaultDSN
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("scratch db unreachable (%v) — create governor_e2e and run cmd/migrate first", err)
	}
	// Order differs from a prod boot on purpose: the SQL migration files lack
	// the drip/journey tables that some send-path ALTERs reference, and the 5s
	// slice creates them. Run the slice first, then the send-path DDL (its
	// bool is a rollup over statements that do not matter here).
	runStartupMigrations(db)
	ensureSendPathSchema(db)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('mailing_segment_members','drip_dispatch_contracts','family_governor_decisions')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("expected the three governor tables, got %d err=%v", n, err)
	}
	var hasLane bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='family_governor_decisions' AND column_name='lane')`).Scan(&hasLane); err != nil || !hasLane {
		t.Fatalf("family_governor_decisions.lane missing: %v", err)
	}
}

//go:build integration

// SendGovernor end-to-end schema prep (local apex-postgres ONLY — never prod):
//
//	docker exec apex-postgres psql -U apex_user -d postgres -c "DROP DATABASE IF EXISTS governor_e2e" -c "CREATE DATABASE governor_e2e"
//	go test -tags integration -run GovernorE2ESchema ./cmd/server/ -v
//	go test -tags integration -run GovernorE2E ./internal/api/ -v
//
// Deterministic from an EMPTY database: applies migrations/*.sql to a fixed
// point (each file in its own transaction, failures ignored, passes repeated
// until a pass changes nothing — the files depend on each other out of
// filename order, so one cmd/migrate pass leaves 029's scheduled_at behind),
// then the SAME boot vehicles prod applies (runStartupMigrations, then
// ensureSendPathSchema), then mirrors prod's send-path CHECK set (none) and
// asserts the columns the governor chain needs. Idempotent: re-run at will.
package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		t.Fatalf("scratch db unreachable (%v) — create governor_e2e first (see the file header)", err)
	}
	// The SQL files and the boot slices depend on EACH OTHER (029's later
	// statements need tables the 5s slice creates; some send-path ALTERs need
	// tables the files create), so interleave to a fixed point: files → slices
	// → files → slices. Two rounds are enough on 2026-09-23; the assertions
	// below fail loudly if that ever changes.
	for round := 1; round <= 2; round++ {
		applySQLMigrationsToFixpoint(t, db)
		runStartupMigrations(db)
		ensureSendPathSchema(db)
	}
	// Prod carries NO CHECK constraints on the send-path tables (verified
	// 2026-09-23: information_schema.table_constraints, type CHECK, on
	// mailing_campaigns / _isp_plans / _waves / _plan_recipients / _queue /
	// _isp_time_spans = 0 rows) — the boot slices dropped the 001-era ones over
	// time and cmd/migrate re-creates them here. Mirror prod.
	for _, stmt := range []string{
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_status_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_campaign_type_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_send_type_check`,
		`ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS mailing_campaigns_execution_mode_check`,
		`ALTER TABLE mailing_campaign_isp_plans DROP CONSTRAINT IF EXISTS mailing_campaign_isp_plans_status_check`,
		`ALTER TABLE mailing_campaign_isp_time_spans DROP CONSTRAINT IF EXISTS mailing_campaign_isp_time_spans_span_type_check`,
		`ALTER TABLE mailing_campaign_waves DROP CONSTRAINT IF EXISTS mailing_campaign_waves_status_check`,
		`ALTER TABLE mailing_campaign_plan_recipients DROP CONSTRAINT IF EXISTS mailing_campaign_plan_recipients_status_check`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var checks int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints WHERE constraint_type='CHECK' AND constraint_name NOT LIKE '%not_null%' AND table_name IN ('mailing_campaigns','mailing_campaign_isp_plans','mailing_campaign_waves','mailing_campaign_plan_recipients','mailing_campaign_queue','mailing_campaign_isp_time_spans')`).Scan(&checks); err != nil || checks != 0 {
		t.Fatalf("send-path CHECK constraints remain: %d err=%v", checks, err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('mailing_segment_members','drip_dispatch_contracts','family_governor_decisions')`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("expected the three governor tables, got %d err=%v", n, err)
	}
	// The chain needs these; a missing column fails OPEN in the governor and
	// would read as "no clamp" — assert loudly here instead.
	for _, col := range []string{"scheduled_at", "pmta_config", "partner_drip_tag", "journey_id", "isp_quotas", "esp_quotas"} {
		var ok bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='mailing_campaigns' AND column_name=$1)`, col).Scan(&ok); err != nil || !ok {
			t.Fatalf("mailing_campaigns.%s missing (run cmd/migrate twice — the first pass errors on 001's own indexes and skips later files): %v", col, err)
		}
	}
	var hasLane bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='family_governor_decisions' AND column_name='lane')`).Scan(&hasLane); err != nil || !hasLane {
		t.Fatalf("family_governor_decisions.lane missing: %v", err)
	}
}

// applySQLMigrationsToFixpoint applies ../../migrations/*.sql like cmd/migrate
// does (one transaction per file), ignoring failures, until a full pass
// applies nothing new. Files whose dependencies land later succeed on a later
// pass; files that can never apply locally (CONCURRENTLY, psql meta-commands)
// stay failed and are reported once.
func applySQLMigrationsToFixpoint(t *testing.T, db *sql.DB) {
	t.Helper()
	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	applied := map[string]bool{}
	for pass := 1; pass <= 6; pass++ {
		newly := 0
		for _, f := range files {
			if applied[f] {
				continue
			}
			content, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(string(content)); err != nil {
				tx.Rollback()
				continue
			}
			if err := tx.Commit(); err != nil {
				continue
			}
			applied[f] = true
			newly++
		}
		t.Logf("sql migrations pass %d: %d newly applied, %d total", pass, newly, len(applied))
		if newly == 0 {
			break
		}
	}
	var never []string
	for _, f := range files {
		if !applied[f] {
			never = append(never, f)
		}
	}
	t.Logf("sql migrations never applied locally (%d): %s", len(never), strings.Join(never, ", "))
}

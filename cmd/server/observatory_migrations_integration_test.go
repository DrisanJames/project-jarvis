//go:build integration

// Drip Observatory P1 migration applier (Vector B plan rev4 §5 DoD).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run DripObservatoryMigrations ./cmd/server/ -v
//
// Applies runStartupMigrations against the LOCAL dev DB (the same slice a
// boot runs — this deliberately avoids booting the whole server binary,
// whose config.yaml DSN fallback points at a non-local DB), then counts the
// P1 observatory tables by name (plan §5 DoD proof). Idempotent; safe to
// re-run. The internal/api schema fixtures
// (drip_observatory_schema_integration_test.go) depend on this having run.
package main

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

const dripObsDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"

func dripObsDSN() string {
	if v := os.Getenv("DRIP_OBSERVATORY_TEST_DSN"); v != "" {
		return v
	}
	return dripObsDefaultDSN
}

// dripObservatoryP1Tables — the P1 schema shipped in this change. The plan's
// full §5 DoD names 13 tables; four (partner_drip_link_audit,
// partner_drip_hygiene_daily, partner_drip_cap_decisions,
// partner_drip_cap_xray_daily) are DEFERRED because their DDL is defined by
// reference to plan rev 3, which is not on disk — see the engagement report.
var dripObservatoryP1Tables = []string{
	"partner_drip_observatory_runs",
	"partner_drip_observatory_run_scope",
	"partner_drip_observatory_cursor",
	"partner_drip_send_cohort_daily",
	"partner_drip_event_daily",
	"partner_drip_observatory_quarantine",
	"mailing_brand_codes",
	"partner_drip_alert_state",
	"partner_drip_alert_deliveries",
}

func TestDripObservatoryMigrationsApplyLocal(t *testing.T) {
	db, err := sql.Open("postgres", dripObsDSN())
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB at %s (%v). Start apex-postgres.", dripObsDSN(), err)
	}

	// The boot path for the 5s-budget slice — includes the dob_* entries.
	runStartupMigrations(db)

	// DoD: boot creates ALL P1 tables — count them by name.
	for _, name := range dripObservatoryP1Tables {
		var got string
		err := db.QueryRow(`
			SELECT table_name FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		`, name).Scan(&got)
		if err != nil {
			t.Errorf("table %s missing after runStartupMigrations: %v", name, err)
		}
	}

	// Seed proof: mailing_brand_codes carries the full 27-brand registry.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mailing_brand_codes WHERE source = 'seed'`).Scan(&n); err != nil {
		t.Fatalf("brand codes count: %v", err)
	}
	if n != 27 {
		t.Errorf("mailing_brand_codes must seed exactly 27 registry rows, got %d", n)
	}
}

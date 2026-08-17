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
	"strings"
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

// dripObservatoryP1Tables — the FOURTEEN logical P1 tables (plan rev 4.1 §5
// DoD, reconciled count: the rev-4.1 completion increment restored the four
// STOP-1 tables and moved partner_drip_campaign_meta's DDL into the D2
// schema wave — its WRITER remains HOLD-CRITICAL at D3b).
var dripObservatoryP1Tables = []string{
	"partner_drip_observatory_runs",
	"partner_drip_observatory_run_scope",
	"partner_drip_observatory_cursor",
	"partner_drip_send_cohort_daily",
	"partner_drip_event_daily",
	"partner_drip_link_audit",
	"partner_drip_hygiene_daily",
	"partner_drip_observatory_quarantine",
	"mailing_brand_codes",
	"partner_drip_alert_state",
	"partner_drip_alert_deliveries",
	"partner_drip_cap_decisions",
	"partner_drip_cap_xray_daily",
	"partner_drip_campaign_meta",
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

	// §5.8 DoD: the DEFAULT partition exists and is a partition (month
	// partitions must NOT be created by the migration — §10.6 owns those).
	var partName string
	if err := db.QueryRow(`SELECT relname FROM pg_class
		WHERE relname = 'partner_drip_cap_decisions_default' AND relispartition`).Scan(&partName); err != nil {
		t.Errorf("partner_drip_cap_decisions_default partition missing: %v", err)
	}
	var monthParts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_class
		WHERE relname LIKE 'partner_drip_cap_decisions_2%' AND relispartition`).Scan(&monthParts); err != nil {
		t.Fatal(err)
	}
	if monthParts != 0 {
		t.Errorf("migration must not create month partitions (found %d) — §10.6 owns those", monthParts)
	}

	// §5.0b DoD: both fact-table vocab constraints contain 'other' after the
	// widening entry runs (regardless of whether this DB was created narrow
	// by 580f313 or wide by the updated generator).
	for _, conname := range []string{"dob_cohort_isp_vocab", "dob_event_isp_vocab"} {
		var def string
		if err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1`, conname).Scan(&def); err != nil {
			t.Errorf("constraint %s missing: %v", conname, err)
			continue
		}
		if !strings.Contains(def, "'other'") {
			t.Errorf("constraint %s not widened — def lacks 'other': %s", conname, def)
		}
	}
}

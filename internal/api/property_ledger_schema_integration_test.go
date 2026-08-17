//go:build integration

// Property Ledger P2 schema fixtures (Vector A plan rev4, Step 10).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run PropertyLedgerSchema ./internal/api/ -v
//
// PREREQUISITES (asserted up front; SKIPs with a clear message if missing):
//   - the local apex-postgres dev container (postgres:16) on localhost:5432.
//     DSN below; override with PROPERTY_LEDGER_TEST_DSN.
//   - the P2 startup migrations applied — boot `go run ./cmd/server/` once
//     against the local DB so runStartupMigrations lands the ledger columns,
//     CHECKs, and the property_* tables.
//
// These are PERMANENT negative fixtures (invariant I-11,
// internal/worker/property_ledger_doc.go): each fixture must PRODUCE the gated
// outcome (CHECK rejection / partial-unique violation / CAS zero-row match) —
// never "invert production logic, watch RED, restore".
package api

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	_ "github.com/lib/pq"
)

const propertyLedgerDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"

func propertyLedgerDSN() string {
	if v := os.Getenv("PROPERTY_LEDGER_TEST_DSN"); v != "" {
		return v
	}
	return propertyLedgerDefaultDSN
}

// openPropertyLedgerDB connects to the LOCAL dev DB or SKIPs the suite.
func openPropertyLedgerDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", propertyLedgerDSN())
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB at %s (%v). Start apex-postgres.", propertyLedgerDSN(), err)
	}
	// Confirm the P2 schema landed (lock_version is the last Step-3 column).
	var col sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_name='partner_drip_brand_budgets' AND column_name='lock_version'
	`).Scan(&col)
	if err != nil || !col.Valid {
		t.Skipf("SKIP: P2 ledger schema missing (%v). Boot `go run ./cmd/server/` against the local DB once to apply runStartupMigrations.", err)
	}
	return db
}

// wantPGError asserts err is a *pq.Error with the given SQLSTATE code and,
// when non-empty, the given constraint name.
func wantPGError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected pq error code=%s constraint=%q, got nil (the gate did NOT fire)", code, constraint)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("expected *pq.Error, got %T: %v", err, err)
	}
	if string(pqErr.Code) != code {
		t.Fatalf("expected SQLSTATE %s, got %s (%v)", code, pqErr.Code, err)
	}
	if constraint != "" && pqErr.Constraint != constraint {
		t.Fatalf("expected constraint %q, got %q (%v)", constraint, pqErr.Constraint, err)
	}
}

const (
	pgCheckViolation  = "23514"
	pgUniqueViolation = "23505"
)

// cleanupLedgerCell removes every fixture row for a (brand, isp) cell.
func cleanupLedgerCell(t *testing.T, db *sql.DB, brand, isp string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_brand_budgets WHERE brand=$1 AND isp=$2`, brand, isp)
		_, _ = db.Exec(`DELETE FROM property_hold_intervals WHERE brand=$1 AND isp=$2`, brand, isp)
		_, _ = db.Exec(`DELETE FROM property_budget_proposals WHERE brand=$1 AND isp=$2`, brand, isp)
	})
}

// TestPropertyLedgerSchema_ChecksReject pins the six Step-3 CHECK constraints:
// each malformed row must be REJECTED with the named constraint.
func TestPropertyLedgerSchema_ChecksReject(t *testing.T) {
	db := openPropertyLedgerDB(t)
	cleanupLedgerCell(t, db, "zzpltest", "gmail")

	cases := []struct {
		name       string
		sql        string
		constraint string
	}{
		{"brand not canonical (upper)", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('DB','gmail',5)`, "pdbb_brand_canonical"},
		{"brand empty", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('','gmail',5)`, "pdbb_brand_canonical"},
		{"isp not canonical (upper)", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('zzpltest','GMAIL',5)`, "pdbb_isp_canonical"},
		{"negative budget", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('zzpltest','gmail',-1)`, "pdbb_budget_nonneg"},
		{"min > max", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, min_budget, max_budget) VALUES ('zzpltest','gmail',5,100,50)`, "pdbb_minmax"},
		{"pending budget without day", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, pending_budget) VALUES ('zzpltest','gmail',5,10)`, "pdbb_pending_pair"},
		{"pending day without budget", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, pending_effective_day) VALUES ('zzpltest','gmail',5,CURRENT_DATE+1)`, "pdbb_pending_pair"},
		{"negative pending budget", `INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, pending_budget, pending_effective_day) VALUES ('zzpltest','gmail',5,-3,CURRENT_DATE+1)`, "pdbb_pending_nonneg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.sql)
			wantPGError(t, err, pgCheckViolation, tc.constraint)
		})
	}

	// Control: the canonical, well-formed row is ACCEPTED.
	if _, err := db.Exec(`INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('zzpltest','gmail',5)`); err != nil {
		t.Fatalf("well-formed control row rejected: %v", err)
	}
}

// TestPropertyLedgerSchema_VersionsChecksReject pins property_budget_versions'
// CHECKs (budget >= 0, source enum).
func TestPropertyLedgerSchema_VersionsChecksReject(t *testing.T) {
	db := openPropertyLedgerDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM property_budget_versions WHERE brand='zzpltest'`)
	})

	_, err := db.Exec(`INSERT INTO property_budget_versions (brand, isp, version, budget, effective_day, approved_by, source)
		VALUES ('zzpltest','gmail',1,-1,CURRENT_DATE,'test','seed')`)
	wantPGError(t, err, pgCheckViolation, "")

	_, err = db.Exec(`INSERT INTO property_budget_versions (brand, isp, version, budget, effective_day, approved_by, source)
		VALUES ('zzpltest','gmail',1,5,CURRENT_DATE,'test','not-a-source')`)
	wantPGError(t, err, pgCheckViolation, "")

	// Control: valid row accepted.
	if _, err := db.Exec(`INSERT INTO property_budget_versions (brand, isp, version, budget, effective_day, approved_by, source)
		VALUES ('zzpltest','gmail',1,5,CURRENT_DATE,'test','seed')`); err != nil {
		t.Fatalf("well-formed version row rejected: %v", err)
	}
}

// TestPropertyLedgerSchema_OneUnresolvedProposal pins uq_pbp_one_unresolved
// under TWO connections: a second unresolved proposal for the same cell must
// hit a unique violation.
func TestPropertyLedgerSchema_OneUnresolvedProposal(t *testing.T) {
	db := openPropertyLedgerDB(t)
	cleanupLedgerCell(t, db, "zzpltest", "yahoo")

	// Two independent connections (the plan's concurrency shape).
	c1, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	const ins = `INSERT INTO property_budget_proposals
		(brand, isp, proposed_budget, base_budget, base_lock_version, basis, expires_at)
		VALUES ('zzpltest','yahoo',100,120,0,'fixture',NOW() + INTERVAL '48 hours')`
	if _, err := c1.ExecContext(context.Background(), ins); err != nil {
		t.Fatalf("first unresolved proposal rejected: %v", err)
	}
	_, err = c2.ExecContext(context.Background(), ins)
	wantPGError(t, err, pgUniqueViolation, "uq_pbp_one_unresolved")

	// Resolving the first frees the slot: a new unresolved insert succeeds.
	if _, err := c1.ExecContext(context.Background(), `UPDATE property_budget_proposals
		SET resolved_at=NOW(), resolution='superseded' WHERE brand='zzpltest' AND isp='yahoo' AND resolved_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.ExecContext(context.Background(), ins); err != nil {
		t.Fatalf("unresolved insert after resolution should succeed: %v", err)
	}

	// resolved/resolution pairing CHECK: one without the other is rejected.
	_, err = c1.ExecContext(context.Background(), `UPDATE property_budget_proposals
		SET resolved_at=NOW() WHERE brand='zzpltest' AND isp='yahoo' AND resolved_at IS NULL`)
	wantPGError(t, err, pgCheckViolation, "")
}

// TestPropertyLedgerSchema_OneOpenHoldInterval pins uq_phi_one_open and the
// half-open interval CHECK (held_to > held_from) — I-3/I-4.
func TestPropertyLedgerSchema_OneOpenHoldInterval(t *testing.T) {
	db := openPropertyLedgerDB(t)
	cleanupLedgerCell(t, db, "zzpltest", "aol")

	c1, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	const open = `INSERT INTO property_hold_intervals (brand, isp, held_from, reason, changed_by)
		VALUES ('zzpltest','aol',NOW(),'fixture','test')`
	if _, err := c1.ExecContext(context.Background(), open); err != nil {
		t.Fatalf("first open interval rejected: %v", err)
	}
	_, err = c2.ExecContext(context.Background(), open)
	wantPGError(t, err, pgUniqueViolation, "uq_phi_one_open")

	// Degenerate interval (to <= from) rejected by the table CHECK.
	_, err = c1.ExecContext(context.Background(), `INSERT INTO property_hold_intervals
		(brand, isp, held_from, held_to, reason, changed_by)
		VALUES ('zzpltest','aol',NOW(),NOW() - INTERVAL '1 second','fixture','test')`)
	wantPGError(t, err, pgCheckViolation, "")

	// Closing the open interval frees the slot.
	if _, err := c1.ExecContext(context.Background(), `UPDATE property_hold_intervals
		SET held_to=NOW() WHERE brand='zzpltest' AND isp='aol' AND held_to IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.ExecContext(context.Background(), open); err != nil {
		t.Fatalf("open after close should succeed: %v", err)
	}
}

// TestPropertyLedgerSchema_OneOpenFlagVersion pins uq_plfv_one_open on
// property_ledger_flag_versions and confirms the global_hold flag row is
// seeded FALSE (Step 5 seed_flag_global_hold).
func TestPropertyLedgerSchema_OneOpenFlagVersion(t *testing.T) {
	db := openPropertyLedgerDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM property_ledger_flag_versions WHERE flag='zz_test_flag'`)
	})

	var val bool
	if err := db.QueryRow(`SELECT value FROM property_ledger_flags WHERE flag='global_hold'`).Scan(&val); err != nil {
		t.Fatalf("global_hold flag row not seeded: %v", err)
	}
	if val {
		t.Fatalf("global_hold must seed FALSE, got TRUE")
	}

	const open = `INSERT INTO property_ledger_flag_versions (flag, version, value, changed_by, effective_from)
		VALUES ('zz_test_flag', $1, TRUE, 'test', NOW())`
	if _, err := db.Exec(open, 1); err != nil {
		t.Fatalf("first open flag version rejected: %v", err)
	}
	_, err := db.Exec(open, 2)
	wantPGError(t, err, pgUniqueViolation, "uq_plfv_one_open")

	// Close it; a new open version is then accepted.
	if _, err := db.Exec(`UPDATE property_ledger_flag_versions SET effective_to=NOW()
		WHERE flag='zz_test_flag' AND effective_to IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(open, 2); err != nil {
		t.Fatalf("open after close should succeed: %v", err)
	}
}

// TestPropertyLedgerSchema_LockVersionCAS pins the I-10 write contract at the
// schema level: every write sets lock_version = lock_version + 1, and a stale
// compare-and-swap matches ZERO rows (the 409 signal for the Step-18 API).
func TestPropertyLedgerSchema_LockVersionCAS(t *testing.T) {
	db := openPropertyLedgerDB(t)
	cleanupLedgerCell(t, db, "zzpltest", "microsoft")

	if _, err := db.Exec(`INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget) VALUES ('zzpltest','microsoft',10)`); err != nil {
		t.Fatal(err)
	}
	var lv int64
	if err := db.QueryRow(`SELECT lock_version FROM partner_drip_brand_budgets WHERE brand='zzpltest' AND isp='microsoft'`).Scan(&lv); err != nil {
		t.Fatal(err)
	}
	if lv != 0 {
		t.Fatalf("new row must start at lock_version=0, got %d", lv)
	}

	// Write path idiom: CAS on the given version, increment on success.
	res, err := db.Exec(`UPDATE partner_drip_brand_budgets
		SET daily_budget=20, lock_version=lock_version+1
		WHERE brand='zzpltest' AND isp='microsoft' AND lock_version=$1`, lv)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("fresh CAS must match exactly 1 row, matched %d", n)
	}

	// Stale CAS (the version we already consumed) must match ZERO rows.
	res, err = db.Exec(`UPDATE partner_drip_brand_budgets
		SET daily_budget=30, lock_version=lock_version+1
		WHERE brand='zzpltest' AND isp='microsoft' AND lock_version=$1`, lv)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("stale CAS must match 0 rows (409 signal), matched %d", n)
	}

	if err := db.QueryRow(`SELECT lock_version FROM partner_drip_brand_budgets WHERE brand='zzpltest' AND isp='microsoft'`).Scan(&lv); err != nil {
		t.Fatal(err)
	}
	if lv != 1 {
		t.Fatalf("lock_version must increment exactly once, got %d", lv)
	}
}

//go:build integration

// Real-Postgres proof for ignite_ip_classification / ignite_ip_class().
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run IPClassificationPG ./cmd/server/ -v
//
// WHY THIS EXISTS ALONGSIDE ip_classification_test.go. That file parses the
// committed SQL and evaluates a transcription of it, which is fast, always
// runs, and cannot be fooled by a hand-written expectation — but it is still a
// transcription. This file executes the PRODUCT CONSTANTS THEMSELVES
// (igniteIPClassificationDDL, ...GistDDL, ...SeedDDL, igniteIPClassFnDDL,
// byte-for-byte what runStartupMigrations installs) against a live PostgreSQL
// and asks Postgres for the four values. It is the authority on:
//   - whether `ORDER BY masklen(cidr) DESC LIMIT 1` really resolves
//     narrowest-first over a GiST <<= scan
//   - whether the CHECK constraint really rejects a bogus class
//   - whether ON CONFLICT DO NOTHING really leaves a curated row alone on the
//     second boot
//   - whether the three-statement seed entry really lands as one Exec
//
// HERMETIC AND NON-DESTRUCTIVE. Everything happens inside ONE transaction that
// always ROLLBACKs, in a throwaway schema placed first on search_path, against
// a private stub of ignite_datacenter_ranges. It never reads or writes the
// dev DB's own tables and cannot touch prod.
//
// NOT SKIPPED, BY DESIGN. There is no t.Skip in this file. It is gated by a
// build tag — an explicit opt-in — so it either runs and reports, or it is not
// compiled. A test that silently green-skips when the DB is absent is the
// failure mode this codebase already has with inert columns.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

const ipClassDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"

func ipClassDSN() string {
	if v := os.Getenv("IP_CLASSIFICATION_TEST_DSN"); v != "" {
		return v
	}
	return ipClassDefaultDSN
}

// ipClassStubDatacenterRanges mirrors the columns igniteIPClassificationSeedDDL
// reads (cidr, provider, service_tag, source). It carries the two blanket
// ownership /16s the prod-proven addresses live inside — the exact situation
// the behaviour table exists to correct.
const ipClassStubDatacenterRanges = `
	CREATE TABLE ignite_datacenter_ranges (
		cidr        cidr PRIMARY KEY,
		provider    text NOT NULL DEFAULT 'microsoft',
		service_tag text,
		source      text NOT NULL DEFAULT 'seed'
	);
	INSERT INTO ignite_datacenter_ranges (cidr, provider, service_tag, source) VALUES
		('135.232.0.0/16','microsoft','AzureCloud','observed'),
		('74.179.0.0/16','microsoft','AzureCloud','observed'),
		('13.64.0.0/11','microsoft','AzureCloud','seed')`

// ipClassPGFixture opens a transaction, builds the whole unit inside a
// throwaway schema, and returns the tx plus a cleanup that always rolls back.
func ipClassPGFixture(t *testing.T) (*sql.Tx, context.Context) {
	t.Helper()
	db, err := sql.Open("postgres", ipClassDSN())
	if err != nil {
		t.Fatalf("open %s: %v", ipClassDSN(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := db.PingContext(ctx); err != nil {
		cancel()
		t.Fatalf("cannot reach the LOCAL dev Postgres at %s: %v\n"+
			"This test needs apex-postgres up (docker ps | grep apex-postgres). It does NOT skip: "+
			"an unreachable DB means the SQL is UNVERIFIED, not fine.", ipClassDSN(), err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback() // never commit — this is a read-only-by-construction harness
		cancel()
		_ = db.Close()
	})

	schema := fmt.Sprintf("ipclass_test_%d", time.Now().UnixNano())
	mustExec(t, ctx, tx, "CREATE SCHEMA "+schema)
	mustExec(t, ctx, tx, "SET LOCAL search_path TO "+schema)
	mustExec(t, ctx, tx, ipClassStubDatacenterRanges)
	return tx, ctx
}

// installIPClassUnit executes the four product constants in registration order.
func installIPClassUnit(t *testing.T, ctx context.Context, tx *sql.Tx, fnDDL string) {
	t.Helper()
	mustExec(t, ctx, tx, igniteIPClassificationDDL)
	mustExec(t, ctx, tx, igniteIPClassificationGistDDL)
	mustExec(t, ctx, tx, igniteIPClassificationSeedDDL)
	mustExec(t, ctx, tx, fnDDL)
}

func mustExec(t *testing.T, ctx context.Context, tx *sql.Tx, q string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx, q); err != nil {
		t.Fatalf("exec failed: %v\nSQL:\n%s", err, q)
	}
}

// ipClass calls the real accessor. ok=false is SQL NULL.
func ipClass(t *testing.T, ctx context.Context, tx *sql.Tx, ip string) (string, bool) {
	t.Helper()
	var out sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT ignite_ip_class($1::inet)`, ip).Scan(&out); err != nil {
		t.Fatalf("ignite_ip_class(%s): %v", ip, err)
	}
	return out.String, out.Valid
}

func ipClassAged(t *testing.T, ctx context.Context, tx *sql.Tx, ip, maxAge string) (string, bool) {
	t.Helper()
	var out sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT ignite_ip_class($1::inet, $2::interval)`, ip, maxAge).Scan(&out); err != nil {
		t.Fatalf("ignite_ip_class(%s, %s): %v", ip, maxAge, err)
	}
	return out.String, out.Valid
}

// =============================================================================
// THE FOUR PROD-PROVEN VALUES
// =============================================================================

func TestIPClassificationPGNarrowestMatchWins(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	cases := []struct {
		ip   string
		want string // "" = NULL
		why  string
	}{
		{"135.232.20.148", "scanner", "a /32 that sits INSIDE a hosting /16 — the narrow row must win"},
		{"135.232.20.64", "unresolved", "mixed-traffic /32 inside the same hosting /16"},
		{"135.232.99.99", "hosting", "no /32 match, falls back to the /16"},
		{"8.8.8.8", "", "no match at all"},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			got, ok := ipClass(t, ctx, tx, c.ip)
			gotStr, wantStr := "NULL", "NULL"
			if ok {
				gotStr = got
			}
			if c.want != "" {
				wantStr = c.want
			}
			if gotStr != wantStr {
				t.Fatalf("ignite_ip_class('%s') = %s, want %s — %s", c.ip, gotStr, wantStr, c.why)
			}
		})
	}
}

// TestIPClassificationPGNegativeControl proves the test above is sensitive to
// the bug it guards: with the accessor's DESC flipped to ASC, real Postgres
// returns the blanket hosting /16 for the scanner /32, and the pin must break.
func TestIPClassificationPGNegativeControl(t *testing.T) {
	broken := strings.Replace(igniteIPClassFnDDL, "masklen(c.cidr) DESC", "masklen(c.cidr) ASC", 1)
	if broken == igniteIPClassFnDDL {
		t.Fatal("could not mutate the accessor's ORDER BY — the negative control no longer bites; rewrite it")
	}
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, broken)

	got, ok := ipClass(t, ctx, tx, "135.232.20.148")
	if ok && got == "scanner" {
		t.Fatal("NEGATIVE CONTROL FAILED: broadest-first resolution still answers 'scanner'. " +
			"The narrowest-match assertions are not actually testing narrowest-match.")
	}
	if !ok || got != "hosting" {
		t.Fatalf("negative control produced %q (matched=%v); expected the blanket 'hosting' /16 to swallow the /32", got, ok)
	}
	t.Logf("negative control OK — with ORDER BY masklen ASC, Postgres answers ignite_ip_class('135.232.20.148') = %q (the pre-fix misclassification)", got)
}

// =============================================================================
// GATES: is_active, max_age, the CHECK constraint
// =============================================================================

func TestIPClassificationPGIsActiveGate(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	if got, ok := ipClass(t, ctx, tx, "135.232.20.148"); !ok || got != "scanner" {
		t.Fatalf("precondition: want scanner, got %q (%v)", got, ok)
	}
	mustExec(t, ctx, tx, `UPDATE ignite_ip_classification SET is_active = false WHERE cidr = '135.232.20.148/32'`)

	got, ok := ipClass(t, ctx, tx, "135.232.20.148")
	if !ok || got != "hosting" {
		t.Fatalf("with the /32 retired (is_active=false), ignite_ip_class = %q (matched=%v); want \"hosting\" — "+
			"an inactive row must be excluded from resolution AND the /16 must still answer", got, ok)
	}
	// The row is retired, not deleted — retirement stays reversible/auditable.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ignite_ip_classification WHERE cidr = '135.232.20.148/32'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("retired row count = %d, want 1 — is_active must not imply deletion", n)
	}
}

func TestIPClassificationPGMaxAgeGate(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	mustExec(t, ctx, tx, `UPDATE ignite_ip_classification
		SET last_confirmed_at = now() - interval '40 days'
		WHERE cidr = '135.232.20.148/32'`)

	if got, ok := ipClassAged(t, ctx, tx, "135.232.20.148", "30 days"); !ok || got != "hosting" {
		t.Fatalf("max_age=30d vs a 40d-old row: got %q (matched=%v), want \"hosting\" — the stale narrow row must drop out", got, ok)
	}
	if got, ok := ipClassAged(t, ctx, tx, "135.232.20.148", "60 days"); !ok || got != "scanner" {
		t.Fatalf("max_age=60d vs a 40d-old row: got %q (matched=%v), want \"scanner\" — a row inside the window must still win", got, ok)
	}
	// NULL max_age (the DEFAULT, and the explicit NULL) matches regardless of age.
	if got, ok := ipClass(t, ctx, tx, "135.232.20.148"); !ok || got != "scanner" {
		t.Fatalf("default (NULL) max_age vs a 40d-old row: got %q (matched=%v), want \"scanner\"", got, ok)
	}
	var out sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT ignite_ip_class('135.232.20.148'::inet, NULL::interval)`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Valid || out.String != "scanner" {
		t.Fatalf("explicit NULL max_age: got %v/%q, want scanner", out.Valid, out.String)
	}
}

func TestIPClassificationPGClassCheckRejectsBogusValues(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	bad := []struct{ col, val string }{
		{"class", "datacenter"}, // the verdict vocabulary must not leak in
		{"class", "human"},
		{"class", "SCANNER"}, // case matters: the CHECK is exact
		{"class", ""},
		{"confidence", "certain"},
		{"confidence", "guess"},
	}
	for _, b := range bad {
		t.Run(b.col+"="+b.val, func(t *testing.T) {
			mustExec(t, ctx, tx, "SAVEPOINT chk")
			var q string
			if b.col == "class" {
				q = `INSERT INTO ignite_ip_classification (cidr, class) VALUES ('203.0.113.7/32', $1)`
			} else {
				q = `INSERT INTO ignite_ip_classification (cidr, class, confidence) VALUES ('203.0.113.7/32', 'scanner', $1)`
			}
			_, err := tx.ExecContext(ctx, q, b.val)
			if err == nil {
				mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT chk")
				t.Fatalf("%s=%q was ACCEPTED; the CHECK constraint must reject it, or an unhandled value reaches every consumer", b.col, b.val)
			}
			if !strings.Contains(err.Error(), "violates check constraint") {
				mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT chk")
				t.Fatalf("%s=%q was rejected, but not by a CHECK constraint: %v", b.col, b.val, err)
			}
			mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT chk")
		})
	}

	// Every legitimate class must be accepted — an over-tight CHECK silently
	// blocks curation.
	for _, good := range []string{"scanner", "hosting", "vpn-or-proxy", "residential-or-mobile", "unresolved", "unknown"} {
		mustExec(t, ctx, tx, "SAVEPOINT ok")
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ignite_ip_classification (cidr, class) VALUES ('203.0.113.8/32', $1)`, good); err != nil {
			t.Fatalf("legitimate class %q was rejected: %v", good, err)
		}
		mustExec(t, ctx, tx, "ROLLBACK TO SAVEPOINT ok")
	}
}

// =============================================================================
// THE CURATION GUARANTEE — the seed re-runs on EVERY boot
// =============================================================================

// TestIPClassificationPGSeedIsIdempotentAndPreservesCuration is the one that
// protects operator curation. The seed entry's leading keyword is INSERT, which
// migrationSkipProbe does not recognize, so it re-executes on every single
// boot. ON CONFLICT DO NOTHING is what stops each restart from reverting a
// hand-corrected row back to the seed value.
func TestIPClassificationPGSeedIsIdempotentAndPreservesCuration(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	var before int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ignite_ip_classification`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("seed inserted nothing — the three-statement entry did not land")
	}

	// An operator curates two rows: one the seed inserts literally, and one the
	// ignite_datacenter_ranges migration produces.
	mustExec(t, ctx, tx, `UPDATE ignite_ip_classification
		SET class = 'residential-or-mobile', confidence = 'confirmed', note = 'operator curated: real people behind this address'
		WHERE cidr = '135.232.20.148/32'`)
	mustExec(t, ctx, tx, `UPDATE ignite_ip_classification
		SET class = 'vpn-or-proxy', confidence = 'confirmed', note = 'operator curated: ownership row reclassified'
		WHERE cidr = '135.232.0.0/16'`)
	// And adds a wholly new curated row the seed knows nothing about.
	mustExec(t, ctx, tx, `INSERT INTO ignite_ip_classification (cidr, class, confidence, evidence_source, note)
		VALUES ('198.51.100.0/24','residential-or-mobile','confirmed','operator','operator curated: net-new')`)

	// SECOND BOOT: the exact same seed statement runs again.
	mustExec(t, ctx, tx, igniteIPClassificationSeedDDL)

	for _, want := range []struct{ cidr, class, confidence, note string }{
		{"135.232.20.148/32", "residential-or-mobile", "confirmed", "operator curated: real people behind this address"},
		{"135.232.0.0/16", "vpn-or-proxy", "confirmed", "operator curated: ownership row reclassified"},
		{"198.51.100.0/24", "residential-or-mobile", "confirmed", "operator curated: net-new"},
	} {
		var class, confidence, note string
		if err := tx.QueryRowContext(ctx,
			`SELECT class, confidence, coalesce(note,'') FROM ignite_ip_classification WHERE cidr = $1`,
			want.cidr).Scan(&class, &confidence, &note); err != nil {
			t.Fatalf("curated row %s disappeared after the seed re-ran: %v", want.cidr, err)
		}
		if class != want.class || confidence != want.confidence || note != want.note {
			t.Fatalf("re-running the seed REVERTED operator curation on %s:\n got  class=%q confidence=%q note=%q\n want class=%q confidence=%q note=%q\n"+
				"The seed runs on every boot — this means every restart destroys curation.",
				want.cidr, class, confidence, note, want.class, want.confidence, want.note)
		}
	}

	// And a re-run adds no duplicates (it added only the one net-new row).
	var after int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ignite_ip_classification`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("row count after curation + second seed = %d, want %d (%d seeded + 1 curated net-new)", after, before+1, before)
	}
}

// TestIPClassificationPGOwnershipNeverBecomesScanner: every row the seed
// derives from ignite_datacenter_ranges is class 'hosting'. That table proves
// who OWNS a range, never what it DOES.
func TestIPClassificationPGOwnershipNeverBecomesScanner(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	rows, err := tx.QueryContext(ctx,
		`SELECT cidr::text, class FROM ignite_ip_classification
		 WHERE evidence_source = 'ignite_datacenter_ranges' ORDER BY cidr`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var cidr, class string
		if err := rows.Scan(&cidr, &class); err != nil {
			t.Fatal(err)
		}
		n++
		if class != "hosting" {
			t.Errorf("row %s migrated from ignite_datacenter_ranges has class %q, want \"hosting\" — ownership is never behaviour", cidr, class)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("migrated %d rows from the 3-row ignite_datacenter_ranges stub, want 3", n)
	}

	// Symmetric check: nothing classified 'scanner' cites the ownership table.
	var leaked int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM ignite_ip_classification
		 WHERE class = 'scanner' AND evidence_source = 'ignite_datacenter_ranges'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d 'scanner' rows cite ignite_datacenter_ranges as evidence — that is ownership being read as behaviour", leaked)
	}
}

// TestIPClassificationPGGistIndexExists: the GiST index is what makes the <<=
// containment scan viable. A missing index is a silent full scan, not an error.
func TestIPClassificationPGGistIndexExists(t *testing.T) {
	tx, ctx := ipClassPGFixture(t)
	installIPClassUnit(t, ctx, tx, igniteIPClassFnDDL)

	var def string
	if err := tx.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE tablename = 'ignite_ip_classification'
		    AND indexname = 'idx_ignite_ip_classification_gist'`).Scan(&def); err != nil {
		t.Fatalf("GiST index idx_ignite_ip_classification_gist did not land: %v", err)
	}
	if !strings.Contains(def, "USING gist") || !strings.Contains(def, "inet_ops") {
		t.Fatalf("index is not gist(cidr inet_ops): %s", def)
	}
}

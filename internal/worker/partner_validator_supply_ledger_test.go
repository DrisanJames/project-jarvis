package worker

// REQ-118 §2.6 — the PartnerValidator's Supply Ledger mirror.
//
// These live here rather than in dripsupply/supply_test.go for one reason:
// PartnerValidator, pendingRecord and eoOutcome are unexported members of
// package `worker`, so a test in package `dripsupply` cannot reach them. Same
// scratch database and skip-when-unreachable discipline as the WP7 suite.
//
// What is pinned:
//
//  1. The mirror GROUPS a batch into one row per (lane, source, ISP, verdict
//     class) with the right quantity — MeasuredYield divides VALIDATION_VALID by
//     VALIDATION_ORDERED, so a mirror that double-counts or drops a class moves
//     every EO order the controller places.
//  2. Only a record that reached `dead_letter` is VALIDATION_NO_VERDICT. A
//     record going back to `pending_eo` has not been decided and must produce
//     no row, or the denominator inflates every retry cycle.
//  3. A LEDGER FAILURE NEVER FAILS THE BATCH. The records are already
//     transitioned; the failure is logged and counted.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
)

const (
	pvLedgerAdminDSNEnv = "DRIPSUPPLY_TEST_ADMIN_DSN"
	pvLedgerDefaultDSN  = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	pvLedgerScratchDB   = "req118_res"
)

func pvLedgerAdminDSN() string {
	if v := strings.TrimSpace(os.Getenv(pvLedgerAdminDSNEnv)); v != "" {
		return v
	}
	return pvLedgerDefaultDSN
}

func pvLedgerScratchDSN(t *testing.T) string {
	t.Helper()
	dsn := pvLedgerAdminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + pvLedgerScratchDB + q
}

func pvLedgerDB(t *testing.T) *sql.DB {
	t.Helper()
	admin, err := sql.Open("postgres", pvLedgerAdminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("local postgres unreachable (%v) — set %s to run this test", err, pvLedgerAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, pvLedgerScratchDB).Scan(&exists); err != nil {
		admin.Close()
		t.Skipf("cannot list databases: %v", err)
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+pvLedgerScratchDB); err != nil &&
			!strings.Contains(err.Error(), "already exists") {
			admin.Close()
			t.Skipf("cannot create scratch database: %v", err)
		}
	}
	admin.Close()

	schema := "pv" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	boot, err := sql.Open("postgres", pvLedgerScratchDSN(t))
	if err != nil {
		t.Skipf("cannot open scratch DSN: %v", err)
	}
	if _, err := boot.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		boot.Close()
		t.Fatalf("create schema: %v", err)
	}
	boot.Close()

	dsn := pvLedgerScratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if clean, err := sql.Open("postgres", pvLedgerScratchDSN(t)); err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})
	for _, stmt := range []string{
		`CREATE TABLE partner_clean_queue (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			dataset_id     UUID NOT NULL,
			partner_id     UUID NOT NULL DEFAULT gen_random_uuid(),
			vertical       TEXT NOT NULL,
			email          TEXT NOT NULL,
			email_md5      VARCHAR NOT NULL,
			isp_family     TEXT NOT NULL,
			status         TEXT NOT NULL,
			eo_result_code INT,
			eo_result      TEXT,
			eo_attempts    INT NOT NULL DEFAULT 0,
			ingested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			validated_at   TIMESTAMPTZ,
			claimed_at     TIMESTAMPTZ
		)`,
		`CREATE TABLE partner_datasets (
			id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			slug     TEXT NOT NULL,
			vertical TEXT NOT NULL
		)`,
		`CREATE TABLE drip_supply_ledger (
			entry_id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			occurred_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			lane                       TEXT NOT NULL,
			source_slug                TEXT NOT NULL,
			isp                        TEXT NOT NULL,
			event                      TEXT NOT NULL CHECK (event IN (
				'RECEIVED','PRECHECK_PASSED','SUPPRESSED','INTERNAL_INVALID',
				'VALIDATION_ORDERED','VALIDATION_VALID','VALIDATION_INVALID','VALIDATION_NO_VERDICT',
				'MAILABLE','REMAIL_ELIGIBLE','RESERVED_FOR_INTRO','CONSUMED','EXPIRED','RELEASED')),
			quantity                   INT NOT NULL,
			unit_cost                  NUMERIC NOT NULL DEFAULT 0,
			total_cost                 NUMERIC NOT NULL DEFAULT 0,
			batch_id                   UUID,
			reservation_id             UUID,
			reason                     TEXT,
			source_contract_version    INT,
			inventory_contract_version INT
		)`,
		`CREATE TABLE mailing_global_suppressions (
			organization_id TEXT NOT NULL,
			email           TEXT NOT NULL,
			md5_hash        TEXT NOT NULL,
			reason          TEXT,
			source          TEXT,
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			updated_at      TIMESTAMPTZ,
			PRIMARY KEY (organization_id, md5_hash)
		)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %.60s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

// pvRoutingEO classifies by an email prefix so ONE batch can carry every
// verdict class: v- verified, s- suppressed, r- retry.
type pvRoutingEO struct{}

func (pvRoutingEO) Validate(_ context.Context, email string) (*emailoversight.ValidationResponse, error) {
	switch {
	case strings.HasPrefix(email, "v-"):
		return &emailoversight.ValidationResponse{Email: email, Result: "Verified", ResultID: 1}, nil
	case strings.HasPrefix(email, "s-"):
		return &emailoversight.ValidationResponse{Email: email, Result: "Undeliverable", ResultID: 4}, nil
	default:
		return &emailoversight.ValidationResponse{Email: email, Result: "Retry", ResultID: 0}, nil
	}
}

func pvSeed(t *testing.T, db *sql.DB, dsID, lane, isp, email string, attempts int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO partner_clean_queue
		(dataset_id, vertical, email, email_md5, isp_family, status, eo_attempts)
		VALUES ($1::uuid, $2, $3, md5($3), $4, 'pending_eo', $5)`, dsID, lane, email, isp, attempts); err != nil {
		t.Fatalf("seed %s: %v", email, err)
	}
}

// TestValidatorMirrorsVerdictsToSupplyLedger is the grouping test.
func TestValidatorMirrorsVerdictsToSupplyLedger(t *testing.T) {
	db := pvLedgerDB(t)
	dsID := uuid.New().String()
	if _, err := db.Exec(`INSERT INTO partner_datasets (id, slug, vertical) VALUES ($1::uuid, 'src_a', 'wcl_test')`, dsID); err != nil {
		t.Fatalf("seed dataset: %v", err)
	}
	// 5 aol verified, 3 aol undeliverable, 2 gmail verified,
	// 1 gmail retry at its LAST attempt (-> dead_letter -> NO_VERDICT),
	// 2 gmail retries with attempts left (-> pending_eo -> NO ledger row).
	for i := 0; i < 5; i++ {
		pvSeed(t, db, dsID, "wcl_test", "aol", fmt.Sprintf("v-aol-%d@x.test", i), 0)
	}
	for i := 0; i < 3; i++ {
		pvSeed(t, db, dsID, "wcl_test", "aol", fmt.Sprintf("s-aol-%d@x.test", i), 0)
	}
	for i := 0; i < 2; i++ {
		pvSeed(t, db, dsID, "wcl_test", "gmail", fmt.Sprintf("v-gmail-%d@x.test", i), 0)
	}
	pvSeed(t, db, dsID, "wcl_test", "gmail", "r-gmail-last@x.test", 2) // attempts+1 == MaxRetries
	for i := 0; i < 2; i++ {
		pvSeed(t, db, dsID, "wcl_test", "gmail", fmt.Sprintf("r-gmail-%d@x.test", i), 0)
	}

	pv := NewPartnerValidator(db, pvRoutingEO{}, PartnerValidatorConfig{BatchSize: 100, MaxRetries: 3, Concurrency: 8})
	// ONE claim + ONE apply, deliberately, rather than processOnce: processOnce
	// drains in a loop, so the two retryable records would be re-claimed until
	// they exhausted MaxRetries and the batch under test would not be one batch.
	batch, err := pv.claimPendingEO(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(batch) != 13 {
		t.Fatalf("claimed %d records, want 13", len(batch))
	}
	// The claim's RETURNING must carry the two fields the mirror is keyed on.
	for _, rec := range batch {
		if rec.isp == "" || rec.sourceSlug == "" {
			t.Fatalf("claim did not return isp/source for %s: isp=%q source=%q", rec.email, rec.isp, rec.sourceSlug)
		}
	}
	pv.validateAndApply(context.Background(), batch)

	type row struct{ lane, source, isp, event string }
	got := map[row]int{}
	rows, qerr := db.Query(`SELECT lane, source_slug, isp, event, quantity FROM drip_supply_ledger`)
	if qerr != nil {
		t.Fatalf("read ledger: %v", qerr)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var r row
		var q int
		if err := rows.Scan(&r.lane, &r.source, &r.isp, &r.event, &q); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[r] += q
		n++
	}
	want := map[row]int{
		{"wcl_test", "src_a", "aol", "VALIDATION_VALID"}:        5,
		{"wcl_test", "src_a", "aol", "VALIDATION_INVALID"}:      3,
		{"wcl_test", "src_a", "gmail", "VALIDATION_VALID"}:      2,
		{"wcl_test", "src_a", "gmail", "VALIDATION_NO_VERDICT"}: 1,
	}
	if n != len(want) {
		t.Errorf("ledger has %d rows, want %d — one per (lane, source, isp, class)", n, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%v = %d, want %d", k, got[k], v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected ledger group %v = %d (a re-queued retry is NOT a verdict)", k, v)
		}
	}
	// The two retries with attempts left are back in pending_eo and produced
	// no ledger row — the negative control on rule 2.
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM partner_clean_queue WHERE status='pending_eo'`).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("pending_eo rows = %d, want 2", pending)
	}
	if f := pv.LedgerMirrorFailures(); f != 0 {
		t.Errorf("LedgerMirrorFailures = %d, want 0", f)
	}
}

// TestValidatorMirrorFailureDoesNotFailTheBatch: the records must still be
// transitioned when the ledger write blows up.
func TestValidatorMirrorFailureDoesNotFailTheBatch(t *testing.T) {
	db := pvLedgerDB(t)
	dsID := uuid.New().String()
	if _, err := db.Exec(`INSERT INTO partner_datasets (id, slug, vertical) VALUES ($1::uuid, 'src_a', 'wcl_test')`, dsID); err != nil {
		t.Fatalf("seed dataset: %v", err)
	}
	for i := 0; i < 4; i++ {
		pvSeed(t, db, dsID, "wcl_test", "aol", fmt.Sprintf("v-aol-%d@x.test", i), 0)
	}
	// The ledger is gone — exactly the shape of a boot where WP1's migration
	// timed out of the 5s slice.
	if _, err := db.Exec(`DROP TABLE drip_supply_ledger`); err != nil {
		t.Fatalf("drop ledger: %v", err)
	}

	pv := NewPartnerValidator(db, pvRoutingEO{}, PartnerValidatorConfig{BatchSize: 100, MaxRetries: 3, Concurrency: 4})
	pv.processOnce()

	var ready int
	if err := db.QueryRow(`SELECT COUNT(*) FROM partner_clean_queue WHERE status='ready' AND validated_at IS NOT NULL`).Scan(&ready); err != nil {
		t.Fatalf("count ready: %v", err)
	}
	if ready != 4 {
		t.Fatalf("ready rows = %d, want 4 — a ledger failure must not unwind the validation batch", ready)
	}
	if f := pv.LedgerMirrorFailures(); f != 1 {
		t.Errorf("LedgerMirrorFailures = %d, want 1 (the failure must be COUNTED, not swallowed silently)", f)
	}
}

// TestValidatorMirrorKillSwitch: DRIP_SUPPLY_LEDGER_MIRROR_DISABLED=1 stops the
// mirror without touching the validation path.
func TestValidatorMirrorKillSwitch(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_LEDGER_MIRROR_DISABLED", "1")
	db := pvLedgerDB(t)
	dsID := uuid.New().String()
	if _, err := db.Exec(`INSERT INTO partner_datasets (id, slug, vertical) VALUES ($1::uuid, 'src_a', 'wcl_test')`, dsID); err != nil {
		t.Fatalf("seed dataset: %v", err)
	}
	for i := 0; i < 3; i++ {
		pvSeed(t, db, dsID, "wcl_test", "aol", fmt.Sprintf("v-aol-%d@x.test", i), 0)
	}
	pv := NewPartnerValidator(db, pvRoutingEO{}, PartnerValidatorConfig{BatchSize: 100, MaxRetries: 3, Concurrency: 4})
	pv.processOnce()

	var ready, ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM partner_clean_queue WHERE status='ready'`).Scan(&ready); err != nil {
		t.Fatalf("count ready: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_supply_ledger`).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ready != 3 {
		t.Errorf("ready = %d, want 3 — the kill switch must not change validation", ready)
	}
	if ledger != 0 {
		t.Errorf("ledger rows = %d, want 0 with the mirror disabled", ledger)
	}
}

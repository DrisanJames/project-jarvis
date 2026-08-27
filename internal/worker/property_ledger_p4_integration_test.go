//go:build integration

// Property Ledger P4 worker fixtures (Vector A plan rev4, Steps 14/16/20).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run PropertyLedgerP4 ./internal/worker/ -v
//
// PREREQUISITES: local apex-postgres with the P2 schema, the P4
// create_property_intro_counters migration, and idx_pcq_intro_rollup built
// (boot `go run ./cmd/server/` once against the local DB).
//
// Permanent fixtures (I-11):
//   - zero-cell materialization: the full 16×14 grid exists per day, absent
//     sends recorded as 0; observed sends land on the normalized cell.
//   - concurrent double-run: no double-count (idempotent upserts + PK).
//   - pending promotion fires ONLY on/after the Denver boundary day.
//   - VDM catch-up after fully-absent days (run-table gap detection).
//   - VDM 48h finalization boundary (window_end 49h → finalized; 47h → not;
//     incomplete → never).
//   - finalized ses_vdm_daily rows are IMMUTABLE (upsert is a no-op).

package worker

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	sespkg "github.com/ignite/sparkpost-monitor/internal/ses"
	_ "github.com/lib/pq"
)

const propertyLedgerWorkerTestDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"

func openWorkerIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", propertyLedgerWorkerTestDSN)
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB (%v). Start apex-postgres.", err)
	}
	var reg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('property_intro_counters')::text`).Scan(&reg); err != nil || !reg.Valid {
		t.Skipf("SKIP: property_intro_counters missing — boot `go run ./cmd/server/` once against the local DB (err=%v)", err)
	}
	return db
}

// seedPCQFixture builds the partner→dataset→batch chain and returns an
// inserter for pcq rows. Everything is cleaned up (CASCADE) on test end.
func seedPCQFixture(t *testing.T, db *sql.DB) func(brand, ispFamily string, mailedAt time.Time) {
	t.Helper()
	suffix := fmt.Sprintf("plp4%06d", rand.Intn(1000000))
	var partnerID, datasetID, batchID string
	if err := db.QueryRow(`
		INSERT INTO data_partners (name, slug) VALUES ($1, $1) RETURNING id::text`, "itest-"+suffix).Scan(&partnerID); err != nil {
		t.Fatalf("seed partner: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM data_partners WHERE id=$1::uuid`, partnerID) })
	if err := db.QueryRow(`
		INSERT INTO partner_datasets (partner_id, name, slug, vertical)
		VALUES ($1::uuid, $2, $2, 'direct_offer') RETURNING id::text`, partnerID, "itest-ds-"+suffix).Scan(&datasetID); err != nil {
		t.Fatalf("seed dataset: %v", err)
	}
	if err := db.QueryRow(`
		INSERT INTO partner_inbound_batches (dataset_id, partner_id, s3_bucket, s3_key)
		VALUES ($1::uuid, $2::uuid, 'itest', $3) RETURNING id::text`, datasetID, partnerID, "itest/"+suffix).Scan(&batchID); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	n := 0
	return func(brand, ispFamily string, mailedAt time.Time) {
		n++
		email := fmt.Sprintf("p4-%s-%d@example.test", suffix, n)
		if _, err := db.Exec(`
			INSERT INTO partner_clean_queue
				(batch_id, dataset_id, partner_id, vertical, email, email_md5,
				 isp_family, status, mailed_brand, mailed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'direct_offer', $4, md5($4),
			        $5, 'mailed', $6, $7)`,
			batchID, datasetID, partnerID, email, ispFamily, brand, mailedAt); err != nil {
			t.Fatalf("seed pcq row: %v", err)
		}
	}
}

func TestPropertyLedgerP4CounterZeroCellMaterialization(t *testing.T) {
	db := openWorkerIntegrationDB(t)
	w := NewPropertyIntroRollupWorker(db, nil)
	insert := seedPCQFixture(t, db)

	now := time.Now()
	insert("db", "gmail", now)
	insert("DB", "gmail", now) // normalization: LOWER(BTRIM(...)) folds onto db/gmail
	insert("ht", "", now)      // '' isp_family → COALESCE 'other'

	w.RunOnce(context.Background())

	today := denverDate(now, w.loc).Format("2006-01-02")
	var gridCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM property_intro_counters WHERE day=$1::date`, today).Scan(&gridCount); err != nil {
		t.Fatal(err)
	}
	if gridCount != 16*14 {
		t.Fatalf("today's grid = %d cells, want %d (16 brands × 14 ledger groups)", gridCount, 16*14)
	}
	check := func(brand, isp string, want int) {
		var got int
		if err := db.QueryRow(`
			SELECT introduced FROM property_intro_counters
			WHERE day=$1::date AND brand=$2 AND isp=$3`, today, brand, isp).Scan(&got); err != nil {
			t.Fatalf("cell %s/%s missing: %v", brand, isp, err)
		}
		if got != want {
			t.Fatalf("cell %s/%s introduced = %d, want %d", brand, isp, got, want)
		}
	}
	check("db", "gmail", 2)
	check("ht", "other", 1)
	check("qf", "yahoo", 0) // ZERO-CELL: absence of sends is a recorded 0

	var runStatus string
	var expected, completed int
	if err := db.QueryRow(`
		SELECT status, expected_cells, completed_cells FROM property_counter_runs
		WHERE day=$1::date ORDER BY started_at DESC LIMIT 1`, today).Scan(&runStatus, &expected, &completed); err != nil {
		t.Fatal(err)
	}
	if runStatus != "completed" || expected != 16*14 || completed != 16*14 {
		t.Fatalf("run row: %s %d/%d, want completed %d/%d", runStatus, completed, expected, 16*14, 16*14)
	}

	// Concurrent double-run (lease-expiry overlap shape): idempotent upserts —
	// no double-count, grid size unchanged.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.RunOnce(context.Background()) }()
	}
	wg.Wait()
	check("db", "gmail", 2)
	if err := db.QueryRow(`SELECT COUNT(*) FROM property_intro_counters WHERE day=$1::date`, today).Scan(&gridCount); err != nil {
		t.Fatal(err)
	}
	if gridCount != 16*14 {
		t.Fatalf("grid after concurrent double-run = %d, want %d", gridCount, 16*14)
	}
}

func TestPropertyLedgerP4PromotionBoundary(t *testing.T) {
	db := openWorkerIntegrationDB(t)
	w := NewPropertyIntroRollupWorker(db, nil)
	ctx := context.Background()
	today := denverDate(time.Now(), w.loc)
	tomorrow := today.AddDate(0, 0, 1)

	reset := func(brand, isp string) {
		_, _ = db.Exec(`DELETE FROM partner_drip_brand_budgets WHERE brand=$1 AND isp=$2`, brand, isp)
	}
	seed := func(brand, isp string, pendDay time.Time) {
		reset(brand, isp)
		if _, err := db.Exec(`
			INSERT INTO partner_drip_brand_budgets
				(brand, isp, daily_budget, hold, lock_version, pending_budget, pending_effective_day)
			VALUES ($1, $2, 100, FALSE, 1, 500, $3::date)`,
			brand, isp, pendDay.Format("2006-01-02")); err != nil {
			t.Fatalf("seed %s/%s: %v", brand, isp, err)
		}
		t.Cleanup(func() { reset(brand, isp) })
	}
	seed("db", "cox", today)     // due → promotes
	seed("db", "zoho", tomorrow) // future → untouched

	if _, err := w.promotePendingBudgets(ctx, today); err != nil {
		t.Fatalf("promotion: %v", err)
	}

	var daily int
	var pendB sql.NullInt64
	var lockV int64
	if err := db.QueryRow(`
		SELECT daily_budget, pending_budget, lock_version FROM partner_drip_brand_budgets
		WHERE brand='db' AND isp='cox'`).Scan(&daily, &pendB, &lockV); err != nil {
		t.Fatal(err)
	}
	if daily != 500 || pendB.Valid || lockV != 2 {
		t.Fatalf("due pending must promote: daily=%d pend=%v lock=%d (want 500, NULL, 2)", daily, pendB, lockV)
	}
	if err := db.QueryRow(`
		SELECT daily_budget, pending_budget, lock_version FROM partner_drip_brand_budgets
		WHERE brand='db' AND isp='zoho'`).Scan(&daily, &pendB, &lockV); err != nil {
		t.Fatal(err)
	}
	if daily != 100 || !pendB.Valid || pendB.Int64 != 500 || lockV != 1 {
		t.Fatalf("future pending must NOT promote: daily=%d pend=%v lock=%d", daily, pendB, lockV)
	}
}

// stubVDMFetcher returns complete metrics for every cell.
type stubVDMFetcher struct{ calls int }

func (f *stubVDMFetcher) GetMetricsForIdentityISP(ctx context.Context, identity, rawISP string, from, to time.Time) (*sespkg.IdentityISPMetrics, error) {
	f.calls++
	values := map[string]int64{}
	for _, m := range sespkg.AllMetrics() {
		values[m] = 1
	}
	return &sespkg.IdentityISPMetrics{Identity: identity, RawISP: rawISP, Values: values}, nil
}

func TestPropertyLedgerP4VDMCatchupAndFinalization(t *testing.T) {
	db := openWorkerIntegrationDB(t)
	region := fmt.Sprintf("itest-%06d", rand.Intn(1000000))
	w := NewSESVDMSnapshotWorker(db, nil, region).SetFetcher(&stubVDMFetcher{})
	w.pause = 0
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ses_vdm_daily WHERE region=$1`, region)
		_, _ = db.Exec(`DELETE FROM ses_vdm_snapshot_runs WHERE region=$1`, region)
	})

	today := utcMidnight(time.Now())

	// Last completed run 4 days ago → 3 fully-absent days to catch up.
	if _, err := db.Exec(`
		INSERT INTO ses_vdm_snapshot_runs (day, region, expected_cells, completed_cells, status, finished_at)
		VALUES ($1::date, $2, 1, 1, 'completed', NOW())`,
		today.AddDate(0, 0, -4).Format("2006-01-02"), region); err != nil {
		t.Fatalf("seed completed run: %v", err)
	}

	// 48h-boundary fixtures (finalize is worker-run, cross-region):
	// window_end 49h ago + complete → finalizes; 47h → not; incomplete → never.
	insertVDMRow := func(identity string, windowEnd time.Time, complete bool) {
		day := utcMidnight(windowEnd.AddDate(0, 0, -1))
		if _, err := db.Exec(`
			INSERT INTO ses_vdm_daily
				(day, identity, isp, region, send, complete, source_window_start, source_window_end)
			VALUES ($1::date, $2, 'gmail', $3, 100, $4, $5, $6)
			ON CONFLICT (day, identity, isp, region) DO NOTHING`,
			day.Format("2006-01-02"), identity, region, complete, windowEnd.Add(-24*time.Hour), windowEnd); err != nil {
			t.Fatalf("seed vdm row: %v", err)
		}
	}
	now := time.Now()
	insertVDMRow("fin-49h.test", now.Add(-49*time.Hour), true)
	insertVDMRow("fin-47h.test", now.Add(-47*time.Hour), true)
	insertVDMRow("fin-incomplete.test", now.Add(-60*time.Hour), false)

	w.RunOnce(ctx)

	// Catch-up: the 3 absent days + today all have completed runs.
	for _, back := range []int{3, 2, 1, 0} {
		day := today.AddDate(0, 0, -back).Format("2006-01-02")
		var status string
		if err := db.QueryRow(`
			SELECT status FROM ses_vdm_snapshot_runs
			WHERE region=$1 AND day=$2::date ORDER BY started_at DESC LIMIT 1`, region, day).Scan(&status); err != nil {
			t.Fatalf("catch-up day %s has no run: %v", day, err)
		}
		if status != "completed" {
			t.Fatalf("catch-up day %s run = %s, want completed", day, status)
		}
	}

	// Alias summing landed canonical rows: Hotmail → microsoft with raw name.
	var rawISPs []byte
	if err := db.QueryRow(`
		SELECT raw_isps FROM ses_vdm_daily
		WHERE region=$1 AND isp='microsoft' LIMIT 1`, region).Scan(&rawISPs); err != nil {
		t.Fatalf("canonical microsoft row missing (Hotmail alias): %v", err)
	}
	if string(rawISPs) != "{Hotmail}" {
		t.Fatalf("raw_isps = %s, want {Hotmail}", rawISPs)
	}

	// 48h boundary.
	checkFinalized := func(identity string, wantFinalized bool) {
		var finalized sql.NullTime
		if err := db.QueryRow(`
			SELECT finalized_at FROM ses_vdm_daily WHERE region=$1 AND identity=$2`, region, identity).Scan(&finalized); err != nil {
			t.Fatalf("row %s: %v", identity, err)
		}
		if finalized.Valid != wantFinalized {
			t.Fatalf("%s finalized=%v, want %v (48h boundary)", identity, finalized.Valid, wantFinalized)
		}
	}
	checkFinalized("fin-49h.test", true)
	checkFinalized("fin-47h.test", false)
	checkFinalized("fin-incomplete.test", false)

	// Immutability: the upsert is a NO-OP on the finalized row.
	row := &vdmCanonicalRow{isp: "gmail", rawISPs: []string{"Gmail"},
		values: map[string]int64{sespkg.MetricSend: 999999}, complete: true}
	day := utcMidnight(now.Add(-49*time.Hour).AddDate(0, 0, -1))
	n, err := w.upsertCell(ctx, day.Format("2006-01-02"), "fin-49h.test", row,
		day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("upsert on finalized: %v", err)
	}
	if n != 0 {
		t.Fatalf("finalized row upsert affected %d rows, want 0 (I-7 immutability)", n)
	}
	var send int64
	if err := db.QueryRow(`
		SELECT send FROM ses_vdm_daily WHERE region=$1 AND identity='fin-49h.test'`, region).Scan(&send); err != nil {
		t.Fatal(err)
	}
	if send != 100 {
		t.Fatalf("finalized row mutated: send=%d, want 100", send)
	}
}

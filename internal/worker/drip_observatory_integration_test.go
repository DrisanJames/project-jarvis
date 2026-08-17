//go:build integration

// Drip Observatory integration suite — worker side (Vector B plan rev 4.1
// §11). This file grows the enumerated TestObsIntegration_* scenarios as the
// rollup worker lands (P2+); shipped now: #23, the rev-4.1 STOP-2 regression
// guard at schema level.
//
// RUN (local apex-postgres ONLY — never prod), after the migrations applier:
//
//	go test -tags integration -run DripObservatoryMigrations ./cmd/server/ -v
//	go test -tags integration -count=1 ./internal/worker/ -run 'TestObsIntegration' -v
package worker

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

const dripObsWorkerDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"

func openDripObsWorkerDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DRIP_OBSERVATORY_TEST_DSN")
	if dsn == "" {
		dsn = dripObsWorkerDefaultDSN
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("SKIP: cannot open local dev DB (%v). Start apex-postgres.", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("SKIP: cannot ping local dev DB (%v). Start apex-postgres.", err)
	}
	var name sql.NullString
	err = db.QueryRow(`SELECT table_name FROM information_schema.tables
		WHERE table_schema='public' AND table_name='partner_drip_send_cohort_daily'`).Scan(&name)
	if err != nil || !name.Valid {
		t.Skipf("SKIP: observatory schema missing (%v). Run the cmd/server DripObservatoryMigrations applier first.", err)
	}
	return db
}

// TestObsIntegration_LongTailOtherISPRow (§11 #23): a long-tail recipient's
// fact rows carry isp='other' and must PASS the widened vocabulary CHECKs on
// every isp-bearing observatory table (rev-4.1 STOP-2 regression guard —
// GroupFromDomain returns "other" for the long tail, and the pre-ruling
// narrow constraint rejected it). Negative control: a token outside
// append(AllGroups(), Other) still rejects. The rollup-path half of this
// scenario (the worker GROUPing a long-tail domain into 'other') lands with
// the P2 worker and extends this test.
func TestObsIntegration_LongTailOtherISPRow(t *testing.T) {
	db := openDripObsWorkerDB(t)

	const org = "0b5e17a1-0000-4000-8000-0000000000e1"
	const dataset = "0b5e17a1-0000-4000-8000-0000000000e2"
	const attempt = "0b5e17a1-0000-4000-8000-0000000000e3"

	var runID string
	if err := db.QueryRow(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass)
		VALUES (CURRENT_DATE, 'rollup') RETURNING run_id`).Scan(&runID); err != nil {
		t.Fatalf("fixture run insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_send_cohort_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_event_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_cap_xray_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_cap_decisions WHERE wave_attempt_id=$1`, attempt)
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_runs WHERE run_id=$1`, runID)
	})

	insCohort := func(isp string) error {
		_, err := db.Exec(`INSERT INTO partner_drip_send_cohort_daily
			(run_id, organization_id, cohort_day, dataset_id, vertical, touch_number,
			 brand_code, sending_apex, dimension_scope, isp, dispatch_basis,
			 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
			 human_clicks, click_basis,
			 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,CURRENT_DATE,$3,'fixture',1,'db','discountblog.com','lane_isp',$4,'pcq',
			 0,0,0,0,'none','available','available','partial','available','available')`,
			runID, org, dataset, isp)
		return err
	}
	insEvent := func(isp string) error {
		_, err := db.Exec(`INSERT INTO partner_drip_event_daily
			(run_id, organization_id, event_day, dataset_id, vertical, touch_number,
			 brand_code, sending_apex, dimension_scope, isp, dispatch_basis,
			 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
			 human_clicks, click_basis,
			 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,CURRENT_DATE,$3,'fixture',1,'db','discountblog.com','lane_isp',$4,'message_log',
			 0,0,0,0,'none','available','available','partial','available','available')`,
			runID, org, dataset, isp)
		return err
	}

	// The widened vocabulary accepts the long-tail bucket everywhere.
	if err := insCohort("other"); err != nil {
		t.Errorf("cohort row isp='other' rejected (vocab not widened?): %v", err)
	}
	if err := insEvent("other"); err != nil {
		t.Errorf("event row isp='other' rejected (vocab not widened?): %v", err)
	}
	if _, err := db.Exec(`INSERT INTO partner_drip_cap_decisions
		(wave_attempt_id, decided_at, pass, vertical, dataset_id, brand_code, sending_apex,
		 touch_number, isp, base_cap, preclaim_cap, post_throughput_cap, final_cap,
		 claimed, deferred, stage_vector, binding_stage, binding_value, organization_id)
		VALUES ($1,NOW(),'welcome','fixture',$2,'db','discountblog.com',
		 0,'other',10,10,10,10,0,0,'[{"stage":"base","cap":10}]','none',10,$3)`,
		attempt, dataset, org); err != nil {
		t.Errorf("cap-decision row isp='other' rejected: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO partner_drip_cap_xray_daily
		(run_id, organization_id, day, dataset_id, pass, brand_code, sending_apex, isp,
		 attempts, claimed, deferred, min_final_cap, max_final_cap)
		VALUES ($1,$2,CURRENT_DATE,$3,'welcome','db','discountblog.com','other',1,0,0,10,10)`,
		runID, org, dataset); err != nil {
		t.Errorf("xray row isp='other' rejected: %v", err)
	}

	// Negative control: the vocabulary is wider by exactly {'other'} — an
	// arbitrary token still rejects on both fact tables.
	if err := insCohort("notanisp"); err == nil {
		t.Error("cohort row with isp outside append(AllGroups(), Other) must reject")
	}
	if err := insEvent("notanisp"); err == nil {
		t.Error("event row with isp outside append(AllGroups(), Other) must reject")
	}
}

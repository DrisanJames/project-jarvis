//go:build integration

// Drip Observatory P1 schema fixtures (Vector B plan rev4 §5, P0 contracts §3).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run DripObservatorySchema ./internal/api/ -v
//
// PREREQUISITES: the local apex-postgres dev container, with the P1
// migrations applied — either boot the server once against the local DB, or
// run the applier test first:
//
//	go test -tags integration -run DripObservatoryMigrations ./cmd/server/ -v
//
// These are PERMANENT negative fixtures (the property-ledger house pattern,
// property_ledger_schema_integration_test.go): each fixture must PRODUCE the
// gated outcome — CHECK rejection, unique/PK violation, FK violation —
// never "invert production logic, watch RED, restore". Helpers
// (openPropertyLedgerDB-style DSN handling, wantPGError, pgCheckViolation,
// pgUniqueViolation) are shared with the property-ledger suite in this
// package.
package api

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

const (
	pgNotNullViolation = "23502"
	pgFKViolation      = "23503"

	dobTestOrg     = "0b5e17a1-0000-4000-8000-000000000001"
	dobTestOrgB    = "0b5e17a1-0000-4000-8000-000000000002"
	dobTestDataset = "0b5e17a1-0000-4000-8000-0000000000d5"
)

// openDripObservatoryDB reuses the local-dev-only connection guard and skips
// unless the P1 observatory schema is present.
func openDripObservatoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openPropertyLedgerDB(t)
	var name sql.NullString
	err := db.QueryRow(`
		SELECT table_name FROM information_schema.tables
		WHERE table_schema='public' AND table_name='partner_drip_observatory_runs'
	`).Scan(&name)
	if err != nil || !name.Valid {
		t.Skipf("SKIP: Drip Observatory P1 schema missing (%v). Run `go test -tags integration -run DripObservatoryMigrations ./cmd/server/` first.", err)
	}
	return db
}

// newObservatoryRun inserts a fixture run row and registers cleanup of every
// fact/scope/quarantine row hanging off it (FK order: facts first, run last).
func newObservatoryRun(t *testing.T, db *sql.DB) string {
	t.Helper()
	var runID string
	if err := db.QueryRow(`
		INSERT INTO partner_drip_observatory_runs (operational_day, source_pass)
		VALUES (CURRENT_DATE, 'rollup') RETURNING run_id
	`).Scan(&runID); err != nil {
		t.Fatalf("fixture run insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_send_cohort_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_event_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_quarantine WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_run_scope WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_runs WHERE run_id=$1`, runID)
	})
	return runID
}

// insertCohort inserts a cohort fact row with the varying fields under test.
const insertCohortSQL = `INSERT INTO partner_drip_send_cohort_daily
	(run_id, organization_id, cohort_day, dataset_id, vertical, touch_number,
	 brand_code, sending_apex, dimension_scope, isp, recipients_planned, dispatch_basis,
	 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
	 human_clicks, click_basis,
	 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
	VALUES ($1,$2,CURRENT_DATE,$3,'fixture',1,'db','discountblog.com',$4,$5,$6,'pcq',
	 $7,$8,$9,$10,$11,'available','available','partial','available','available')`

// TestDripObservatorySchema_TableInventory: all FOURTEEN logical P1 tables
// exist (plan rev 4.1 §5 DoD reconciled count — the rev-4.1 completion
// increment restored the four STOP-1 tables and added the campaign-meta
// table), plus the cap-decisions DEFAULT partition and no month partitions.
func TestDripObservatorySchema_TableInventory(t *testing.T) {
	db := openDripObservatoryDB(t)
	present := []string{
		"partner_drip_observatory_runs", "partner_drip_observatory_run_scope",
		"partner_drip_observatory_cursor", "partner_drip_send_cohort_daily",
		"partner_drip_event_daily", "partner_drip_link_audit",
		"partner_drip_hygiene_daily", "partner_drip_observatory_quarantine",
		"mailing_brand_codes", "partner_drip_alert_state", "partner_drip_alert_deliveries",
		"partner_drip_cap_decisions", "partner_drip_cap_xray_daily",
		"partner_drip_campaign_meta",
	}
	count := func(name string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema='public' AND table_name=$1`, name).Scan(&n); err != nil {
			t.Fatalf("inventory probe %s: %v", name, err)
		}
		return n
	}
	for _, name := range present {
		if count(name) != 1 {
			t.Errorf("P1 table %s missing", name)
		}
	}
	// §5.8: DEFAULT partition present; NO month partitions (§10.6 owns those).
	var defaultPart string
	if err := db.QueryRow(`SELECT relname FROM pg_class
		WHERE relname='partner_drip_cap_decisions_default' AND relispartition`).Scan(&defaultPart); err != nil {
		t.Errorf("cap-decisions DEFAULT partition missing: %v", err)
	}
	var monthParts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_class
		WHERE relname LIKE 'partner_drip_cap_decisions_2%' AND relispartition`).Scan(&monthParts); err != nil {
		t.Fatal(err)
	}
	if monthParts != 0 {
		t.Errorf("migration must not create month partitions (found %d)", monthParts)
	}
}

// TestDripObservatorySchema_ISPVocabParity: every LIVE isp vocab CHECK —
// the fact tables (post-§5.0b widening) and the cap tables (born wide) —
// carries exactly append(isp.AllGroups(), isp.Other): DDL↔code parity
// measured against pg_get_constraintdef, not the generator's output
// (plan §5.0, rev-4.1 STOP-2 ruling).
func TestDripObservatorySchema_ISPVocabParity(t *testing.T) {
	db := openDripObservatoryDB(t)
	// PG renders the IN list as `isp = ANY (ARRAY['gmail'::text, ...])`.
	arrayRE := regexp.MustCompile(`ARRAY\[([^\]]*)\]`)
	tokenRE := regexp.MustCompile(`'([a-z]*)'`)
	want := map[string]bool{}
	for _, g := range append(isp.AllGroups(), isp.Other) {
		want[g] = true
	}
	cases := []struct {
		constraint string
		laneBranch bool // fact tables also carry the dimension_scope + empty-isp branch
	}{
		{"dob_cohort_isp_vocab", true},
		{"dob_event_isp_vocab", true},
		{"capdec_isp_vocab", false},
		{"xrayd_isp_vocab", false},
	}
	for _, tc := range cases {
		var def string
		if err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1`, tc.constraint).Scan(&def); err != nil {
			t.Fatalf("constraint %s not found: %v", tc.constraint, err)
		}
		arr := arrayRE.FindStringSubmatch(def)
		if arr == nil {
			t.Fatalf("%s: no ARRAY vocabulary in live def: %s", tc.constraint, def)
		}
		got := map[string]bool{}
		for _, m := range tokenRE.FindAllStringSubmatch(arr[1], -1) {
			got[m[1]] = true
		}
		for g := range want {
			if !got[g] {
				t.Errorf("%s: live CHECK missing %q (def: %s)", tc.constraint, g, def)
			}
		}
		for g := range got {
			if !want[g] {
				t.Errorf("%s: live CHECK carries %q not in append(isp.AllGroups(), isp.Other)", tc.constraint, g)
			}
		}
		if tc.laneBranch && !strings.Contains(def, `= ''`) {
			t.Errorf("%s: lane-scope empty-string branch missing (def: %s)", tc.constraint, def)
		}
	}
}

// TestDripObservatorySchema_BrandCodesSeedParity: mailing_brand_codes seed
// rows equal the brandident literal EXACTLY, both directions, and the apex
// UNIQUE + code PK reject duplicates.
func TestDripObservatorySchema_BrandCodesSeedParity(t *testing.T) {
	db := openDripObservatoryDB(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM mailing_brand_codes WHERE source='fixture'`) })

	rows, err := db.Query(`SELECT brand_code, apex FROM mailing_brand_codes WHERE source='seed'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	table := map[string]string{}
	for rows.Next() {
		var code, apex string
		if err := rows.Scan(&code, &apex); err != nil {
			t.Fatal(err)
		}
		table[code] = apex
	}
	lit := brandident.Canonical()
	if len(table) != len(lit) {
		t.Errorf("seed rows = %d, literal = %d", len(table), len(lit))
	}
	for _, p := range lit {
		if table[p.Code] != p.Apex {
			t.Errorf("table[%s] = %q, literal wants %q", p.Code, table[p.Code], p.Apex)
		}
	}

	// PK: duplicate code rejected.
	_, err = db.Exec(`INSERT INTO mailing_brand_codes (brand_code, apex, source) VALUES ('db','zz-fixture.example','fixture')`)
	wantPGError(t, err, pgUniqueViolation, "")
	// UNIQUE: duplicate apex rejected.
	_, err = db.Exec(`INSERT INTO mailing_brand_codes (brand_code, apex, source) VALUES ('zzfix','discountblog.com','fixture')`)
	wantPGError(t, err, pgUniqueViolation, "")
}

// TestDripObservatorySchema_ClickBasisChecks pins the §5.0 click CHECK set on
// the cohort table: every malformed combination rejected, both canonical
// branches and 'mixed'/'none' accepted.
func TestDripObservatorySchema_ClickBasisChecks(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)

	reject := []struct {
		name                            string
		scope, isp                      string
		wrapper, resolved, total, human int
		basis                           string
	}{
		{"total != wrapper+resolved", "lane_isp", "gmail", 10, 5, 16, 0, "mixed"},
		{"basis none with total>0", "lane_isp", "gmail", 10, 0, 10, 0, "none"},
		{"basis wrapper with resolved>0", "lane_isp", "gmail", 10, 5, 15, 0, "wrapper"},
		{"basis resolved-only with wrapper>0", "lane_isp", "gmail", 10, 5, 15, 0, "resolved-only"},
		{"human_clicks > total", "lane_isp", "gmail", 10, 0, 10, 11, "wrapper"},
		{"basis not-in-vocab", "lane_isp", "gmail", 0, 0, 0, 0, "bogus"},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(insertCohortSQL, runID, dobTestOrg, dobTestDataset,
				tc.scope, tc.isp, nil, tc.wrapper, tc.resolved, tc.total, tc.human, tc.basis)
			wantPGError(t, err, pgCheckViolation, "")
		})
	}

	// Controls: the §3.3 legitimate shapes are ACCEPTED.
	accept := []struct {
		name                            string
		isp                             string
		wrapper, resolved, total, human int
		basis                           string
	}{
		{"mixed (10 wrapper + 5 resolved = 15)", "gmail", 10, 5, 15, 3, "mixed"},
		{"wrapper basis", "yahoo", 10, 0, 10, 2, "wrapper"},
		{"resolved-only basis", "aol", 0, 5, 5, 1, "resolved-only"},
		{"none iff zero", "microsoft", 0, 0, 0, 0, "none"},
	}
	for _, tc := range accept {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(insertCohortSQL, runID, dobTestOrg, dobTestDataset,
				"lane_isp", tc.isp, nil, tc.wrapper, tc.resolved, tc.total, tc.human, tc.basis); err != nil {
				t.Fatalf("well-formed %s row rejected: %v", tc.basis, err)
			}
		})
	}
}

// TestDripObservatorySchema_ScopePlanningAndVocab pins the lane/lane_isp
// pairing CHECK, the planning-grain CHECK (§3.2), the generated isp
// vocabulary rejection, and the cohort unique logical key (org in key, §3.6).
func TestDripObservatorySchema_ScopePlanningAndVocab(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)

	insert := func(scope, ispVal string, planned interface{}) error {
		_, err := db.Exec(insertCohortSQL, runID, dobTestOrg, dobTestDataset,
			scope, ispVal, planned, 0, 0, 0, 0, "none")
		return err
	}
	// lane rows carry isp=''; lane_isp rows carry isp<>''.
	wantPGError(t, insert("lane", "gmail", nil), pgCheckViolation, "")
	wantPGError(t, insert("lane_isp", "", nil), pgCheckViolation, "")
	// isp outside the generated vocabulary.
	wantPGError(t, insert("lane_isp", "notanisp", nil), pgCheckViolation, "dob_cohort_isp_vocab")
	// recipients_planned only on lane rows, and never negative.
	wantPGError(t, insert("lane_isp", "gmail", 100), pgCheckViolation, "")
	wantPGError(t, insert("lane", "", -1), pgCheckViolation, "")
	if err := insert("lane", "", 100); err != nil {
		t.Fatalf("lane row with recipients_planned rejected: %v", err)
	}

	// Unique logical key (organization_id, cohort_day, dataset_id,
	// touch_number, brand_code, isp, run_id): same key again → 23505;
	// a different ORG with the same remaining key → accepted (§3.6).
	dup := func(org string) error {
		_, err := db.Exec(insertCohortSQL, runID, org, dobTestDataset,
			"lane_isp", "cox", nil, 0, 0, 0, 0, "none")
		return err
	}
	if err := dup(dobTestOrg); err != nil {
		t.Fatalf("first keyed row rejected: %v", err)
	}
	wantPGError(t, dup(dobTestOrg), pgUniqueViolation, "uq_drip_cohort_daily")
	if err := dup(dobTestOrgB); err != nil {
		t.Fatalf("same key under a different org must be a distinct row: %v", err)
	}
}

// TestDripObservatorySchema_StatusColumnsExplicit pins §3.4: the five
// per-metric statuses have NO defaults (NULL insert → not-null violation)
// and a value outside the vocabulary is rejected.
func TestDripObservatorySchema_StatusColumnsExplicit(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)

	// Omit every status column: the writer failed to establish availability.
	_, err := db.Exec(`INSERT INTO partner_drip_send_cohort_daily
		(run_id, organization_id, cohort_day, dataset_id, vertical, touch_number,
		 brand_code, sending_apex, dimension_scope, isp, dispatch_basis, click_basis)
		VALUES ($1,$2,CURRENT_DATE,$3,'fixture',1,'db','discountblog.com','lane','','pcq','none')`,
		runID, dobTestOrg, dobTestDataset)
	wantPGError(t, err, pgNotNullViolation, "")

	// Status outside the vocabulary.
	_, err = db.Exec(insertCohortSQL+``, runID, dobTestOrg, dobTestDataset,
		"lane_isp", "att", nil, 0, 0, 0, 0, "none")
	if err != nil {
		t.Fatalf("control row rejected: %v", err)
	}
	_, err = db.Exec(`UPDATE partner_drip_send_cohort_daily SET dispatch_status='bogus' WHERE run_id=$1 AND isp='att'`, runID)
	wantPGError(t, err, pgCheckViolation, "")
	// click_basis and dispatch_basis likewise have no defaults (§3.4).
	var hasDefault bool
	for _, col := range []string{"click_basis", "dispatch_basis", "dispatch_status", "delivery_status", "bounce_status", "engagement_status", "conversion_status"} {
		if err := db.QueryRow(`SELECT column_default IS NOT NULL FROM information_schema.columns
			WHERE table_name='partner_drip_send_cohort_daily' AND column_name=$1`, col).Scan(&hasDefault); err != nil {
			t.Fatalf("column probe %s: %v", col, err)
		}
		if hasDefault {
			t.Errorf("column %s must have NO default (§3.4)", col)
		}
	}
}

// TestDripObservatorySchema_EventDaily pins the event table: same CHECK set,
// its own unique key on event_day, and NO recipients_planned column (§3.2).
func TestDripObservatorySchema_EventDaily(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='partner_drip_event_daily' AND column_name='recipients_planned'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("partner_drip_event_daily must NOT carry recipients_planned — planning is cohort-table-only (§3.2)")
	}

	insert := func(ispVal string) error {
		_, err := db.Exec(`INSERT INTO partner_drip_event_daily
			(run_id, organization_id, event_day, dataset_id, vertical, touch_number,
			 brand_code, sending_apex, dimension_scope, isp, dispatch_basis,
			 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total,
			 human_clicks, click_basis,
			 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,CURRENT_DATE,$3,'fixture',1,'db','discountblog.com','lane_isp',$4,'message_log',
			 0,0,0,0,'none','available','unavailable','unavailable','available','available')`,
			runID, dobTestOrg, dobTestDataset, ispVal)
		return err
	}
	if err := insert("gmail"); err != nil {
		t.Fatalf("well-formed event row rejected: %v", err)
	}
	wantPGError(t, insert("gmail"), pgUniqueViolation, "uq_drip_event_daily")
	wantPGError(t, insert("notanisp"), pgCheckViolation, "dob_event_isp_vocab")
}

// TestDripObservatorySchema_RunsScopeCursor pins the run/scope/cursor model:
// vocab CHECKs, exact-key PK (dup → 23505), scope ON DELETE CASCADE, and the
// per-(source_pass × dataset) cursor PK.
func TestDripObservatorySchema_RunsScopeCursor(t *testing.T) {
	db := openDripObservatoryDB(t)

	// runs vocabularies.
	_, err := db.Exec(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass) VALUES (CURRENT_DATE,'bogus')`)
	wantPGError(t, err, pgCheckViolation, "")
	_, err = db.Exec(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass, status) VALUES (CURRENT_DATE,'rollup','bogus')`)
	wantPGError(t, err, pgCheckViolation, "")

	// scope: exact-key PK + fact_kind vocabulary + cascade.
	runID := newObservatoryRun(t, db)
	scopeIns := `INSERT INTO partner_drip_observatory_run_scope (run_id, fact_kind, organization_id, day, dataset_id)
		VALUES ($1,$2,$3,CURRENT_DATE,$4)`
	if _, err := db.Exec(scopeIns, runID, "cohort", dobTestOrg, dobTestDataset); err != nil {
		t.Fatalf("scope insert: %v", err)
	}
	_, err = db.Exec(scopeIns, runID, "cohort", dobTestOrg, dobTestDataset)
	wantPGError(t, err, pgUniqueViolation, "")
	// Same key under a DIFFERENT fact kind is a distinct scope row (§5.2).
	if _, err := db.Exec(scopeIns, runID, "event", dobTestOrg, dobTestDataset); err != nil {
		t.Fatalf("per-kind scope row rejected: %v", err)
	}
	_, err = db.Exec(scopeIns, runID, "bogus", dobTestOrg, dobTestDataset)
	wantPGError(t, err, pgCheckViolation, "")

	// ON DELETE CASCADE: deleting the run removes its scope rows.
	var scopeRun string
	if err := db.QueryRow(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass)
		VALUES (CURRENT_DATE,'link_audit') RETURNING run_id`).Scan(&scopeRun); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(scopeIns, scopeRun, "link_audit", dobTestOrg, dobTestDataset); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM partner_drip_observatory_runs WHERE run_id=$1`, scopeRun); err != nil {
		t.Fatalf("run delete with scope rows must cascade: %v", err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM partner_drip_observatory_run_scope WHERE run_id=$1`, scopeRun).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("scope rows survived run delete: %d", left)
	}

	// cursor: one row per (source_pass, dataset).
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_cursor WHERE dataset_id=$1`, dobTestDataset)
	})
	curIns := `INSERT INTO partner_drip_observatory_cursor (source_pass, dataset_id, organization_id, watermark_to)
		VALUES ('rollup',$1,$2,NOW())`
	if _, err := db.Exec(curIns, dobTestDataset, dobTestOrg); err != nil {
		t.Fatalf("cursor insert: %v", err)
	}
	_, err = db.Exec(curIns, dobTestDataset, dobTestOrg)
	wantPGError(t, err, pgUniqueViolation, "")
}

// TestDripObservatorySchema_QuarantineAndAlerts pins the quarantine FK, the
// alert state/delivery vocabularies, the delivery→state FK, and the pending
// partial index.
func TestDripObservatorySchema_QuarantineAndAlerts(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)

	// Quarantine rows must belong to a real run.
	_, err := db.Exec(`INSERT INTO partner_drip_observatory_quarantine (run_id, source_pass, reason, quarantine_key)
		VALUES ('0b5e17a1-dead-4000-8000-000000000bad','rollup','brand_unknown','x')`)
	wantPGError(t, err, pgFKViolation, "")
	if _, err := db.Exec(`INSERT INTO partner_drip_observatory_quarantine (run_id, source_pass, reason, quarantine_key)
		VALUES ($1,'rollup','brand_unknown','em.zz-unknown.example')`, runID); err != nil {
		t.Fatalf("well-formed quarantine row rejected: %v", err)
	}

	// Alerts.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_alert_deliveries WHERE alert_key LIKE 'zzdob:%'`)
		_, _ = db.Exec(`DELETE FROM partner_drip_alert_state WHERE alert_key LIKE 'zzdob:%'`)
	})
	_, err = db.Exec(`INSERT INTO partner_drip_alert_state (alert_key, state, first_observed_at, last_observed_at, last_value)
		VALUES ('zzdob:x:gmail','bogus',NOW(),NOW(),'{}')`)
	wantPGError(t, err, pgCheckViolation, "")
	if _, err := db.Exec(`INSERT INTO partner_drip_alert_state (alert_key, state, first_observed_at, last_observed_at, last_value)
		VALUES ('zzdob:x:gmail','pending',NOW(),NOW(),'{}')`); err != nil {
		t.Fatalf("well-formed alert state rejected: %v", err)
	}
	_, err = db.Exec(`INSERT INTO partner_drip_alert_deliveries (alert_key, notification_kind, payload)
		VALUES ('zzdob:missing:key','firing','{}')`)
	wantPGError(t, err, pgFKViolation, "")
	_, err = db.Exec(`INSERT INTO partner_drip_alert_deliveries (alert_key, notification_kind, payload)
		VALUES ('zzdob:x:gmail','bogus','{}')`)
	wantPGError(t, err, pgCheckViolation, "")
	if _, err := db.Exec(`INSERT INTO partner_drip_alert_deliveries (alert_key, notification_kind, payload)
		VALUES ('zzdob:x:gmail','firing','{}')`); err != nil {
		t.Fatalf("well-formed delivery rejected: %v", err)
	}
	_, err = db.Exec(`UPDATE partner_drip_alert_deliveries SET delivery_state='bogus' WHERE alert_key='zzdob:x:gmail'`)
	wantPGError(t, err, pgCheckViolation, "")

	// The pending-claim partial index exists by name (§5.10).
	var idx string
	if err := db.QueryRow(`SELECT indexname FROM pg_indexes
		WHERE tablename='partner_drip_alert_deliveries' AND indexname='idx_dob_alert_deliveries_pending'`).Scan(&idx); err != nil {
		t.Errorf("idx_dob_alert_deliveries_pending missing: %v", err)
	}
}

// TestDripObservatorySchema_LinkAudit pins §5.6: row_kind/status vocab
// rejects by NAMED constraint, the red<=clicks and sub2 identity CHECKs,
// NO defaults on status/row_kind (GREEN must be earned — rev-4.1 letter
// correction), and the unique logical key.
func TestDripObservatorySchema_LinkAudit(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM partner_drip_link_audit WHERE run_id=$1`, runID) })

	const ins = `INSERT INTO partner_drip_link_audit
		(run_id, organization_id, event_day, campaign_id, url_host, row_kind,
		 clicks, red_clicks, status, sub2_present, sub2_invalid)
		VALUES ($1,$2,CURRENT_DATE,$3,$4,$5,$6,$7,$8,$9,$10)`
	campaign := "0b5e17a1-0000-4000-8000-00000000ca01"

	// Vocabulary rejects with the plan's NAMED constraints.
	_, err := db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "bogus", 0, 0, "GREEN", 0, 0)
	wantPGError(t, err, pgCheckViolation, "dob_linkaudit_rowkind_vocab")
	_, err = db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "host", 0, 0, "bogus", 0, 0)
	wantPGError(t, err, pgCheckViolation, "dob_linkaudit_status_vocab")
	// red_clicks <= clicks; sub2_invalid <= sub2_present.
	_, err = db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "host", 5, 6, "RED", 0, 0)
	wantPGError(t, err, pgCheckViolation, "")
	_, err = db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "host", 5, 0, "GREEN", 2, 3)
	wantPGError(t, err, pgCheckViolation, "")

	// NO defaults: omitting row_kind/status is a not-null violation, and
	// information_schema confirms no column_default (§5.0 no-unsafe-defaults).
	_, err = db.Exec(`INSERT INTO partner_drip_link_audit (run_id, organization_id, event_day, campaign_id)
		VALUES ($1,$2,CURRENT_DATE,$3)`, runID, dobTestOrg, campaign)
	wantPGError(t, err, pgNotNullViolation, "")
	for _, col := range []string{"row_kind", "status"} {
		var hasDefault bool
		if err := db.QueryRow(`SELECT column_default IS NOT NULL FROM information_schema.columns
			WHERE table_name='partner_drip_link_audit' AND column_name=$1`, col).Scan(&hasDefault); err != nil {
			t.Fatal(err)
		}
		if hasDefault {
			t.Errorf("partner_drip_link_audit.%s must have NO default", col)
		}
	}

	// Controls + unique key (org, event_day, campaign, url_host, row_kind, run).
	if _, err := db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "host", 10, 2, "RED", 8, 1); err != nil {
		t.Fatalf("well-formed host row rejected: %v", err)
	}
	_, err = db.Exec(ins, runID, dobTestOrg, campaign, "cratoolpro.com", "host", 3, 0, "GREEN", 3, 0)
	wantPGError(t, err, pgUniqueViolation, "uq_drip_link_audit")
	// The coverage row (url_host='') is a distinct row_kind under the same key.
	if _, err := db.Exec(ins, runID, dobTestOrg, campaign, "", "coverage", 25, 0, "GREEN", 0, 0); err != nil {
		t.Fatalf("coverage row rejected: %v", err)
	}
}

// TestDripObservatorySchema_Hygiene pins §5.7: dataset×day×population grain
// (no brand columns), the population vocabulary, and the HARD match-identity
// CHECK on first_touch_dispatch rows only.
func TestDripObservatorySchema_Hygiene(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM partner_drip_hygiene_daily WHERE run_id=$1`, runID) })

	// No brand columns (rev-4.1 letter correction).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='partner_drip_hygiene_daily' AND column_name IN ('brand_code','sending_apex')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("partner_drip_hygiene_daily must carry NO brand columns (dataset×day×population grain)")
	}

	const ins = `INSERT INTO partner_drip_hygiene_daily
		(run_id, organization_id, cohort_day, dataset_id, population,
		 dispatched_total, subs_matched, subs_unmatched)
		VALUES ($1,$2,CURRENT_DATE,$3,$4,$5,$6,$7)`
	_, err := db.Exec(ins, runID, dobTestOrg, dobTestDataset, "bogus", 0, 0, 0)
	wantPGError(t, err, pgCheckViolation, "dob_hygiene_population_vocab")
	// Match identity: matched + unmatched MUST equal dispatched_total on
	// first_touch_dispatch rows...
	_, err = db.Exec(ins, runID, dobTestOrg, dobTestDataset, "first_touch_dispatch", 100, 60, 39)
	wantPGError(t, err, pgCheckViolation, "dob_hygiene_match_identity")
	if _, err := db.Exec(ins, runID, dobTestOrg, dobTestDataset, "first_touch_dispatch", 100, 60, 40); err != nil {
		t.Fatalf("identity-satisfying row rejected: %v", err)
	}
	// ...and is EXEMPT on all_touch_events rows (their content is bounce
	// anatomy + complaints; subs_*/pcq_* stay 0 — §5.7).
	if _, err := db.Exec(ins, runID, dobTestOrg, dobTestDataset, "all_touch_events", 0, 0, 0); err != nil {
		t.Fatalf("all_touch_events row rejected: %v", err)
	}
	// population is part of the unique key; same key + population duplicates reject.
	_, err = db.Exec(ins, runID, dobTestOrg, dobTestDataset, "all_touch_events", 0, 0, 0)
	wantPGError(t, err, pgUniqueViolation, "uq_drip_hygiene_daily")
}

// TestDripObservatorySchema_CapDecisions pins §5.8: pass/isp/binding_stage
// vocabularies (isp born WIDE — 'other' accepted), the partitioned unique
// (wave_attempt_id, isp, decided_at), and that rows land in the DEFAULT
// partition (no month partitions exist yet).
func TestDripObservatorySchema_CapDecisions(t *testing.T) {
	db := openDripObservatoryDB(t)
	attempt := "0b5e17a1-0000-4000-8000-00000000aa01"
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM partner_drip_cap_decisions WHERE wave_attempt_id=$1`, attempt) })

	const ins = `INSERT INTO partner_drip_cap_decisions
		(wave_attempt_id, decided_at, pass, vertical, dataset_id, brand_code, sending_apex,
		 touch_number, isp, base_cap, preclaim_cap, post_throughput_cap, final_cap,
		 claimed, deferred, stage_vector, binding_stage, binding_value, organization_id)
		VALUES ($1,'2026-08-17T12:00:00Z',$2,'fixture',$3,'db','discountblog.com',
		 0,$4,100,80,80,80,50,0,'[{"stage":"base","cap":100}]',$5,80,$6)`

	_, err := db.Exec(ins, attempt, "bogus", dobTestDataset, "gmail", "base", dobTestOrg)
	wantPGError(t, err, pgCheckViolation, "capdec_pass_vocab")
	_, err = db.Exec(ins, attempt, "welcome", dobTestDataset, "notanisp", "base", dobTestOrg)
	wantPGError(t, err, pgCheckViolation, "capdec_isp_vocab")
	_, err = db.Exec(ins, attempt, "welcome", dobTestDataset, "gmail", "bogus_stage", dobTestOrg)
	wantPGError(t, err, pgCheckViolation, "capdec_stage_vocab")

	// Born wide: 'other' is a legal isp from birth (rev-4.1 STOP-2).
	if _, err := db.Exec(ins, attempt, "welcome", dobTestDataset, "other", "intro_budget", dobTestOrg); err != nil {
		t.Fatalf("isp='other' cap-decision row rejected: %v", err)
	}
	// Partitioned unique: same (wave_attempt_id, isp, decided_at) rejects —
	// duplicate batches in either order are idempotent (§10.1).
	_, err = db.Exec(ins, attempt, "welcome", dobTestDataset, "other", "intro_budget", dobTestOrg)
	wantPGError(t, err, pgUniqueViolation, "")
	// The row landed in the DEFAULT partition.
	var inDefault int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ONLY partner_drip_cap_decisions_default
		WHERE wave_attempt_id=$1`, attempt).Scan(&inDefault); err != nil {
		t.Fatal(err)
	}
	if inDefault != 1 {
		t.Errorf("cap-decision row must land in the DEFAULT partition, found %d", inDefault)
	}
}

// TestDripObservatorySchema_CapXrayAndCampaignMeta pins §5.8b + §5.11:
// the aggregate's composite PK (org in key), its wide isp vocabulary, and
// the campaign-meta table shape (PK + touch CHECK; writer ships at D3b).
func TestDripObservatorySchema_CapXrayAndCampaignMeta(t *testing.T) {
	db := openDripObservatoryDB(t)
	runID := newObservatoryRun(t, db)
	campaign := "0b5e17a1-0000-4000-8000-00000000cb01"
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_cap_xray_daily WHERE run_id=$1`, runID)
		_, _ = db.Exec(`DELETE FROM partner_drip_campaign_meta WHERE campaign_id=$1`, campaign)
	})

	const insX = `INSERT INTO partner_drip_cap_xray_daily
		(run_id, organization_id, day, dataset_id, pass, brand_code, sending_apex, isp,
		 attempts, claimed, deferred, min_final_cap, max_final_cap)
		VALUES ($1,$2,CURRENT_DATE,$3,'welcome','db','discountblog.com',$4,3,120,4,40,80)`
	_, err := db.Exec(insX, runID, dobTestOrg, dobTestDataset, "notanisp")
	wantPGError(t, err, pgCheckViolation, "xrayd_isp_vocab")
	if _, err := db.Exec(insX, runID, dobTestOrg, dobTestDataset, "other"); err != nil {
		t.Fatalf("isp='other' xray row rejected: %v", err)
	}
	// Composite PK dup rejects; different org under same remaining key is distinct (§3.6).
	_, err = db.Exec(insX, runID, dobTestOrg, dobTestDataset, "other")
	wantPGError(t, err, pgUniqueViolation, "")
	if _, err := db.Exec(insX, runID, dobTestOrgB, dobTestDataset, "other"); err != nil {
		t.Fatalf("same key under different org must be distinct: %v", err)
	}

	// Campaign meta (§5.11): table only — negative touch rejects, PK dedupes.
	const insM = `INSERT INTO partner_drip_campaign_meta
		(campaign_id, dataset_id, brand_code, sending_apex, touch_number, organization_id)
		VALUES ($1,$2,'db','discountblog.com',$3,$4)`
	_, err = db.Exec(insM, campaign, dobTestDataset, -1, dobTestOrg)
	wantPGError(t, err, pgCheckViolation, "")
	if _, err := db.Exec(insM, campaign, dobTestDataset, 1, dobTestOrg); err != nil {
		t.Fatalf("well-formed meta row rejected: %v", err)
	}
	_, err = db.Exec(insM, campaign, dobTestDataset, 2, dobTestOrg)
	wantPGError(t, err, pgUniqueViolation, "")
}

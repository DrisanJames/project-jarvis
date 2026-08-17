//go:build integration

// Property Ledger P4 API fixtures (Vector A plan rev4, Steps 18/20).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run PropertyLedgerP4 ./internal/api/ -v
//
// PREREQUISITES: the P2 schema AND the P4 create_property_intro_counters
// migration applied (boot `go run ./cmd/server/` once against the local DB).
// Reuses openPropertyLedgerDB from the Step-10 suite.
//
// Permanent fixtures (I-11) — the plan's P4 negative-path acceptance proofs:
//   - budget edit stages NEXT-DAY (live daily_budget unchanged, pending pair
//     set, version row effective tomorrow) while hold applies IMMEDIATELY
//     with an open interval row (I-2 vs I-3).
//   - stale lock_version → 409 carrying the current lock_version (CAS).
//   - any budget/hold edit supersedes the cell's unresolved proposal SAME-TX.
//   - approve with stale base_lock_version → 409 + proposal superseded.
//   - PROPERTY_LEDGER_TOTAL_MAX unset → budget INCREASE refused (422).
//   - global hold: CAS 409 on stale flag lock_version; engage/release write
//     interval-tracked flag versions.

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// resetLedgerCell wipes and re-seeds one ledger cell (+ its versions,
// intervals, proposals) for a deterministic fixture.
func resetLedgerCell(t *testing.T, db *sql.DB, brand, isp string, daily int) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM property_budget_proposals WHERE brand=$1 AND isp=$2`,
		`DELETE FROM property_hold_intervals WHERE brand=$1 AND isp=$2`,
		`DELETE FROM property_budget_versions WHERE brand=$1 AND isp=$2`,
		`DELETE FROM partner_drip_brand_budgets WHERE brand=$1 AND isp=$2`,
	} {
		if _, err := db.Exec(q, brand, isp); err != nil {
			t.Fatalf("reset %s: %v", q, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, hold, lock_version)
		VALUES ($1, $2, $3, FALSE, 0)`, brand, isp, daily); err != nil {
		t.Fatalf("seed cell: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM property_budget_proposals WHERE brand=$1 AND isp=$2`,
			`DELETE FROM property_hold_intervals WHERE brand=$1 AND isp=$2`,
			`DELETE FROM property_budget_versions WHERE brand=$1 AND isp=$2`,
			`DELETE FROM partner_drip_brand_budgets WHERE brand=$1 AND isp=$2`,
		} {
			_, _ = db.Exec(q, brand, isp)
		}
	})
}

func ledgerPost(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Email", "integration@test")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestPropertyLedgerP4EditStagesNextDayHoldImmediate(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "db", "verizon", 100)
	tomorrow := propertyLedgerTomorrow()

	// (a) Budget edit: NEXT-DAY. Live cap must NOT move.
	rec := ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"db","isp":"verizon","daily_budget":80,"lock_version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("budget edit: %d %s", rec.Code, rec.Body.String())
	}
	var daily int
	var pendB sql.NullInt64
	var pendDay sql.NullTime
	var lockV int64
	var approvedBy string
	if err := db.QueryRow(`
		SELECT daily_budget, pending_budget, pending_effective_day, lock_version, approved_by
		FROM partner_drip_brand_budgets WHERE brand='db' AND isp='verizon'`).
		Scan(&daily, &pendB, &pendDay, &lockV, &approvedBy); err != nil {
		t.Fatalf("read cell: %v", err)
	}
	if daily != 100 {
		t.Fatalf("live daily_budget moved to %d — edits must be NEXT-DAY (I-2)", daily)
	}
	if !pendB.Valid || pendB.Int64 != 80 || !pendDay.Valid || pendDay.Time.Format("2006-01-02") != tomorrow {
		t.Fatalf("pending pair wrong: %v %v (want 80 @ %s)", pendB, pendDay, tomorrow)
	}
	if lockV != 1 {
		t.Fatalf("lock_version = %d, want 1", lockV)
	}
	if approvedBy != "integration@test" {
		t.Fatalf("edits ARE approvals: approved_by = %q", approvedBy)
	}
	var verDay time.Time
	var verBudget int
	var verSource string
	if err := db.QueryRow(`
		SELECT effective_day, budget, source FROM property_budget_versions
		WHERE brand='db' AND isp='verizon' ORDER BY version DESC LIMIT 1`).
		Scan(&verDay, &verBudget, &verSource); err != nil {
		t.Fatalf("version row missing: %v", err)
	}
	if verDay.Format("2006-01-02") != tomorrow || verBudget != 80 || verSource != "portal-edit" {
		t.Fatalf("version row wrong: %s %d %s", verDay.Format("2006-01-02"), verBudget, verSource)
	}

	// (b) Hold: IMMEDIATE + open interval.
	rec = ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"db","isp":"verizon","hold":true,"reason":"integration hold","lock_version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body.String())
	}
	var hold bool
	var openIntervals int
	if err := db.QueryRow(`SELECT hold FROM partner_drip_brand_budgets WHERE brand='db' AND isp='verizon'`).Scan(&hold); err != nil || !hold {
		t.Fatalf("hold must apply to the LIVE row immediately (I-3): hold=%v err=%v", hold, err)
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM property_hold_intervals
		WHERE brand='db' AND isp='verizon' AND held_to IS NULL AND reason='integration hold'`).
		Scan(&openIntervals); err != nil || openIntervals != 1 {
		t.Fatalf("open hold interval: got %d, want 1 (err=%v)", openIntervals, err)
	}

	// (c) Unhold closes the interval half-open.
	rec = ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"db","isp":"verizon","hold":false,"reason":"release","lock_version":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unhold: %d %s", rec.Code, rec.Body.String())
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM property_hold_intervals
		WHERE brand='db' AND isp='verizon' AND held_to IS NULL`).Scan(&openIntervals); err != nil || openIntervals != 0 {
		t.Fatalf("interval must close on unhold: open=%d err=%v", openIntervals, err)
	}
}

func TestPropertyLedgerP4CAS409(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "db", "zoho", 100)

	// Bump the row so lock_version=1, then post with the stale 0.
	if _, err := db.Exec(`UPDATE partner_drip_brand_budgets SET lock_version=1 WHERE brand='db' AND isp='zoho'`); err != nil {
		t.Fatal(err)
	}
	rec := ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"db","isp":"zoho","daily_budget":50,"lock_version":0}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale lock_version: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if lv, _ := body["lock_version"].(float64); int64(lv) != 1 {
		t.Fatalf("409 must carry the current lock_version: %s", rec.Body.String())
	}
	var pendB sql.NullInt64
	if err := db.QueryRow(`SELECT pending_budget FROM partner_drip_brand_budgets WHERE brand='db' AND isp='zoho'`).Scan(&pendB); err != nil || pendB.Valid {
		t.Fatalf("rejected edit must write NOTHING: pending=%v err=%v", pendB, err)
	}
}

func TestPropertyLedgerP4EditSupersedesProposal(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "ht", "verizon", 200)

	if _, err := db.Exec(`
		INSERT INTO property_budget_proposals
			(brand, isp, proposed_budget, base_budget, base_lock_version, basis, expires_at)
		VALUES ('ht', 'verizon', 150, 200, 0, 'grader-test', NOW() + INTERVAL '48 hours')`); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	rec := ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"ht","isp":"verizon","daily_budget":120,"lock_version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	var resolution sql.NullString
	if err := db.QueryRow(`
		SELECT resolution FROM property_budget_proposals WHERE brand='ht' AND isp='verizon'`).
		Scan(&resolution); err != nil {
		t.Fatal(err)
	}
	if !resolution.Valid || resolution.String != "superseded" {
		t.Fatalf("manual edit must supersede the open proposal same-tx: %v", resolution)
	}
}

func TestPropertyLedgerP4StaleProposal409(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "mh", "verizon", 300)

	// Proposal minted at base_lock_version 0; the ledger then moved to 2.
	var proposalID string
	if err := db.QueryRow(`
		INSERT INTO property_budget_proposals
			(brand, isp, proposed_budget, base_budget, base_lock_version, basis, expires_at)
		VALUES ('mh', 'verizon', 250, 300, 0, 'grader-test', NOW() + INTERVAL '48 hours')
		RETURNING id::text`).Scan(&proposalID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	if _, err := db.Exec(`UPDATE partner_drip_brand_budgets SET lock_version=2 WHERE brand='mh' AND isp='verizon'`); err != nil {
		t.Fatal(err)
	}

	rec := ledgerPost(t, s.HandleApproveProposal, `{"proposal_id":"`+proposalID+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale base_lock_version: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	var resolution sql.NullString
	if err := db.QueryRow(`SELECT resolution FROM property_budget_proposals WHERE id=$1::uuid`, proposalID).
		Scan(&resolution); err != nil {
		t.Fatal(err)
	}
	if !resolution.Valid || resolution.String != "superseded" {
		t.Fatalf("stale approve must stamp superseded: %v", resolution)
	}
	var pendB sql.NullInt64
	if err := db.QueryRow(`SELECT pending_budget FROM partner_drip_brand_budgets WHERE brand='mh' AND isp='verizon'`).Scan(&pendB); err != nil || pendB.Valid {
		t.Fatalf("stale approve must not stage a budget: %v %v", pendB, err)
	}
}

func TestPropertyLedgerP4ApproveDecreaseAppliesNextDay(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "qf", "verizon", 400)
	tomorrow := propertyLedgerTomorrow()

	// A DECREASE passes the unset ceiling (increases are what it refuses).
	var proposalID string
	if err := db.QueryRow(`
		INSERT INTO property_budget_proposals
			(brand, isp, proposed_budget, base_budget, base_lock_version, basis, expires_at)
		VALUES ('qf', 'verizon', 360, 400, 0, 'grader-down', NOW() + INTERVAL '48 hours')
		RETURNING id::text`).Scan(&proposalID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	rec := ledgerPost(t, s.HandleApproveProposal, `{"proposal_id":"`+proposalID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve decrease: %d %s", rec.Code, rec.Body.String())
	}
	var daily int
	var pendB sql.NullInt64
	var pendDay sql.NullTime
	var resolution sql.NullString
	if err := db.QueryRow(`
		SELECT b.daily_budget, b.pending_budget, b.pending_effective_day, p.resolution
		FROM partner_drip_brand_budgets b
		JOIN property_budget_proposals p ON p.id = $1::uuid
		WHERE b.brand='qf' AND b.isp='verizon'`, proposalID).
		Scan(&daily, &pendB, &pendDay, &resolution); err != nil {
		t.Fatal(err)
	}
	if daily != 400 || !pendB.Valid || pendB.Int64 != 360 || pendDay.Time.Format("2006-01-02") != tomorrow {
		t.Fatalf("approve must stage next-day: daily=%d pend=%v day=%v", daily, pendB, pendDay)
	}
	if resolution.String != "approved" {
		t.Fatalf("proposal resolution = %v, want approved", resolution)
	}
	var verSource string
	if err := db.QueryRow(`
		SELECT source FROM property_budget_versions
		WHERE brand='qf' AND isp='verizon' ORDER BY version DESC LIMIT 1`).Scan(&verSource); err != nil || verSource != "proposal-approve" {
		t.Fatalf("version source = %q err=%v, want proposal-approve", verSource, err)
	}
}

func TestPropertyLedgerP4CeilingUnsetRefusesIncreaseIntegration(t *testing.T) {
	t.Setenv("PROPERTY_LEDGER_TOTAL_MAX", "")
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}
	resetLedgerCell(t, db, "bwp", "verizon", 100)

	rec := ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"bwp","isp":"verizon","daily_budget":150,"lock_version":0}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ceiling-unset increase: got %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	// A decrease still passes.
	rec = ledgerPost(t, s.HandleUpdatePropertyLedger,
		`{"brand":"bwp","isp":"verizon","daily_budget":50,"lock_version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ceiling-unset decrease must pass: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPropertyLedgerP4GlobalHoldCASAndVersions(t *testing.T) {
	db := openPropertyLedgerDB(t)
	s := &PMTACampaignService{db: db}

	// Snapshot + restore the flag around the test.
	var origValue bool
	var origLock int64
	if err := db.QueryRow(`SELECT value, lock_version FROM property_ledger_flags WHERE flag='global_hold'`).
		Scan(&origValue, &origLock); err != nil {
		t.Skipf("SKIP: global_hold flag row missing (boot the server once): %v", err)
	}
	if origValue {
		t.Skip("SKIP: global hold currently engaged locally — not touching it")
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`UPDATE property_ledger_flags SET value=FALSE WHERE flag='global_hold'`)
		_, _ = db.Exec(`UPDATE property_ledger_flag_versions SET effective_to=NOW() WHERE flag='global_hold' AND effective_to IS NULL`)
	})

	// Stale lock_version → 409, flag untouched (the confirmation negative path).
	rec := ledgerPost(t, s.HandleGlobalHold,
		`{"value":true,"reason":"stale-test","lock_version":`+jsonInt(origLock+999)+`}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale global-hold: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	var v bool
	if err := db.QueryRow(`SELECT value FROM property_ledger_flags WHERE flag='global_hold'`).Scan(&v); err != nil || v {
		t.Fatalf("stale request must not flip the flag: %v %v", v, err)
	}

	// Fresh engage → flag true + open version interval.
	rec = ledgerPost(t, s.HandleGlobalHold,
		`{"value":true,"reason":"integration engage","lock_version":`+jsonInt(origLock)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("engage: %d %s", rec.Code, rec.Body.String())
	}
	var open int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM property_ledger_flag_versions
		WHERE flag='global_hold' AND effective_to IS NULL AND value = TRUE`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("open TRUE flag version: got %d err=%v, want 1", open, err)
	}

	// Release with the bumped lock_version → interval closes, new one opens.
	rec = ledgerPost(t, s.HandleGlobalHold,
		`{"value":false,"reason":"integration release","lock_version":`+jsonInt(origLock+1)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rec.Code, rec.Body.String())
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM property_ledger_flag_versions
		WHERE flag='global_hold' AND effective_to IS NULL`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("exactly one open flag version after release: got %d err=%v", open, err)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

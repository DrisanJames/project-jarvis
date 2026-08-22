package api

// Lane-action fixtures. What each test PINS (behavior, not implementation):
//   - journey/advance touches ONLY waiting rows (next_touch_at > NOW() is in
//     the statement) and never claims rows the orchestrator holds (FOR UPDATE
//     SKIP LOCKED);
//   - the limit is a real budget: it is consumed across the vertical's
//     datasets in order, and once consumed NO further dataset is touched
//     (ordered sqlmock + ExpectationsWereMet prove the third dataset saw zero
//     statements);
//   - pcq is never filtered by its UNINDEXED vertical column — the vertical is
//     resolved to dataset ids first and every pcq statement keys on dataset_id
//     (docs/JAOS/drip-lanes.md §7);
//   - the write gate is the SAME roster flag and is a REAL gate: env unset ->
//     403 with ZERO SQL executed;
//   - lane-pause applies the per-dataset emergency-stop UPDATE to EVERY active
//     dataset of the vertical, both directions (pause + resume);
//   - the express toggle 404s an unknown dataset and audit-logs old -> new;
//   - the supply feed carries express_dispatch (asserted in
//     property_lane_supply_test.go TestLaneSupplyCountsMapping).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const (
	laneActionsDS1 = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	laneActionsDS2 = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"
	laneActionsDS3 = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa3"
)

// The advance statements must (a) only touch WAITING (future next_touch_at)
// rows, (b) skip rows a concurrent orchestrator claim holds, (c) mirror the
// follow-up claim's not-engaged/not-terminal predicates, and (d) NEVER filter
// partner_clean_queue by its unindexed vertical column.
func TestJourneyAdvanceSQLShape(t *testing.T) {
	for _, sqlText := range []string{laneJourneyAdvanceSQL, laneJourneyAdvanceBrandSQL} {
		for _, want := range []string{
			"next_touch_at > NOW()",
			"FOR UPDATE SKIP LOCKED",
			"status = 'mailed'",
			"engaged_at IS NULL",
			"terminal_reason IS NULL",
			"dataset_id = $1::uuid",
		} {
			if !strings.Contains(sqlText, want) {
				t.Fatalf("advance SQL must contain %q:\n%s", want, sqlText)
			}
		}
		if strings.Contains(sqlText, "vertical") {
			t.Fatalf("advance SQL must not filter pcq by the unindexed vertical column:\n%s", sqlText)
		}
	}
	// The brand variant matches the orchestrator's follow-up brand identity.
	if !strings.Contains(laneJourneyAdvanceBrandSQL,
		"COALESCE(NULLIF(last_touch_brand, ''), mailed_brand) = $4") {
		t.Fatalf("brand variant must match COALESCE(NULLIF(last_touch_brand,''), mailed_brand):\n%s",
			laneJourneyAdvanceBrandSQL)
	}
}

// 1. Advance respects the limit across datasets: ds1 yields 600 of 1000, ds2
// is asked only for the remaining 400 — and ds3 is NEVER touched (ordered
// sqlmock: an exec for ds3 would be unexpected and ExpectationsWereMet pins
// that exactly two advance statements ran).
func TestJourneyAdvanceRespectsLimitAcrossDatasets(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`SELECT id::text\s+FROM partner_datasets`).
		WithArgs("internal_auto_insurance").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(laneActionsDS1).AddRow(laneActionsDS2).AddRow(laneActionsDS3))
	mock.ExpectExec(`UPDATE partner_clean_queue`).
		WithArgs(laneActionsDS1, 2, 1000).
		WillReturnResult(sqlmock.NewResult(0, 600))
	mock.ExpectExec(`UPDATE partner_clean_queue`).
		WithArgs(laneActionsDS2, 2, 400). // only the REMAINING budget
		WillReturnResult(sqlmock.NewResult(0, 400))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleJourneyAdvance,
		"/api/mailing/pmta-campaign/property-ledger/journey/advance",
		`{"vertical":"internal_auto_insurance","touch":2,"limit":1000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Advanced  int64                         `json:"advanced"`
		ByDataset []laneJourneyAdvanceByDataset `json:"by_dataset"`
		Note      string                        `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Advanced != 1000 {
		t.Fatalf("advanced = %d, want 1000", resp.Advanced)
	}
	if len(resp.ByDataset) != 2 ||
		resp.ByDataset[0].DatasetID != laneActionsDS1 || resp.ByDataset[0].Count != 600 ||
		resp.ByDataset[1].DatasetID != laneActionsDS2 || resp.ByDataset[1].Count != 400 {
		t.Fatalf("by_dataset wrong: %+v", resp.ByDataset)
	}
	if !strings.Contains(resp.Note, "next tick") {
		t.Fatalf("response must state the orchestrator picks rows up on its next tick, got %q", resp.Note)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("limit consumed but a further dataset was touched: %v", err)
	}
}

// 2. Optional brand filter uses the branded statement with the brand bound.
func TestJourneyAdvanceBrandFilter(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`SELECT id::text\s+FROM partner_datasets`).
		WithArgs("consumer").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(laneActionsDS1))
	mock.ExpectExec(`UPDATE partner_clean_queue`).
		WithArgs(laneActionsDS1, 1, 500, "db").
		WillReturnResult(sqlmock.NewResult(0, 42))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleJourneyAdvance, "/x",
		`{"vertical":"Consumer ","touch":1,"limit":500,"brand":"DB"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Advanced int64  `json:"advanced"`
		Brand    string `json:"brand"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Advanced != 42 || resp.Brand != "db" {
		t.Fatalf("advanced/brand wrong: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 3. NEGATIVE: write flag unset -> 403 with ZERO SQL (same gate as roster
// writes; no expectations queued, so ExpectationsWereMet proves the gate fired
// before any statement).
func TestJourneyAdvanceWriteGateBlocksWithNoSQL(t *testing.T) {
	for _, env := range []string{"", "0", "true"} { // only "1" enables
		t.Run("env="+env, func(t *testing.T) {
			t.Setenv(laneRosterWriteFlagEnv, env)
			s, mock := newLedgerServiceWithMock(t)
			rec := rosterPost(t, s.HandleJourneyAdvance, "/x",
				`{"vertical":"consumer","touch":1,"limit":100}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), laneRosterWriteFlagEnv) {
				t.Fatalf("403 must name the env var, got %s", rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("gated write executed SQL: %v", err)
			}
		})
	}
}

// 4. NEGATIVE: validation — bad vertical / touch out of 1..4 / limit missing
// or out of 1..50000 are all 400 with zero SQL.
func TestJourneyAdvanceValidation(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	for _, c := range []struct{ name, body string }{
		{"unknown vertical", `{"vertical":"not_a_lane","touch":1,"limit":100}`},
		{"touch 0", `{"vertical":"consumer","touch":0,"limit":100}`},
		{"touch 5", `{"vertical":"consumer","touch":5,"limit":100}`}, // MaxTouchCount rows have no next touch
		{"limit missing", `{"vertical":"consumer","touch":1}`},
		{"limit too big", `{"vertical":"consumer","touch":1,"limit":50001}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, mock := newLedgerServiceWithMock(t)
			rec := rosterPost(t, s.HandleJourneyAdvance, "/x", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("invalid request executed SQL: %v", err)
			}
		})
	}
}

// 5. Lane-pause PAUSES every active dataset of the vertical with the
// per-dataset emergency-stop UPDATE (paused_emergency/paused_reason/paused_at)
// and reports datasets_affected.
func TestLanePausePauseAffectsAllDatasets(t *testing.T) {
	for _, want := range []string{"paused_emergency = true", "paused_reason = $2", "paused_at = NOW()"} {
		if !strings.Contains(lanePauseSQL, want) {
			t.Fatalf("lanePauseSQL must mirror HandleEmergencyStopDataset (%q missing):\n%s", want, lanePauseSQL)
		}
	}
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`SELECT id::text\s+FROM partner_datasets`).
		WithArgs("term_life").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(laneActionsDS1).AddRow(laneActionsDS2))
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs(laneActionsDS1, "complaint spike").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs(laneActionsDS2, "complaint spike").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleLanePause,
		"/api/mailing/pmta-campaign/property-ledger/lane-pause",
		`{"vertical":"term_life","pause":true,"reason":"complaint spike"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		DatasetsAffected int64  `json:"datasets_affected"`
		Paused           bool   `json:"paused_emergency"`
		ScopeNote        string `json:"scope_note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DatasetsAffected != 2 || !resp.Paused {
		t.Fatalf("pause must affect both datasets: %+v", resp)
	}
	// The pause semantics must be stated honestly: follow-ups stop too.
	if !strings.Contains(resp.ScopeNote, "follow-up") {
		t.Fatalf("scope_note must state follow-ups stop too, got %q", resp.ScopeNote)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 6. Lane-pause RESUMES every active dataset (paused_emergency=false, reason
// and paused_at cleared) — the exact inverse, no reason required.
func TestLanePauseResumeAffectsAllDatasets(t *testing.T) {
	for _, want := range []string{"paused_emergency = false", "paused_reason = NULL", "paused_at = NULL"} {
		if !strings.Contains(laneResumeSQL, want) {
			t.Fatalf("laneResumeSQL must mirror HandleResumeDataset (%q missing):\n%s", want, laneResumeSQL)
		}
	}
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`SELECT id::text\s+FROM partner_datasets`).
		WithArgs("term_life").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(laneActionsDS1).AddRow(laneActionsDS2))
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs(laneActionsDS1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs(laneActionsDS2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleLanePause, "/x",
		`{"vertical":"term_life","pause":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		DatasetsAffected int64 `json:"datasets_affected"`
		Paused           bool  `json:"paused_emergency"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DatasetsAffected != 2 || resp.Paused {
		t.Fatalf("resume must affect both datasets and report paused=false: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 7. NEGATIVE: pausing without a reason is refused with zero SQL; the write
// gate applies to lane-pause exactly as to advance.
func TestLanePauseValidationAndGate(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	for _, c := range []struct{ name, body string }{
		{"reason required when pausing", `{"vertical":"term_life","pause":true}`},
		{"pause field required", `{"vertical":"term_life","reason":"x"}`},
		{"unknown vertical", `{"vertical":"nope","pause":true,"reason":"x"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, mock := newLedgerServiceWithMock(t)
			rec := rosterPost(t, s.HandleLanePause, "/x", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("invalid request executed SQL: %v", err)
			}
		})
	}

	t.Setenv(laneRosterWriteFlagEnv, "")
	s, mock := newLedgerServiceWithMock(t)
	rec := rosterPost(t, s.HandleLanePause, "/x",
		`{"vertical":"term_life","pause":true,"reason":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 with gate off, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("gated write executed SQL: %v", err)
	}
}

// ── Express toggle (POST /api/mailing/data-partners/datasets/{id}/express) ──

func expressReq(t *testing.T, id, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/"+id+"/express", strings.NewReader(body))
	req.Header.Set("X-User-Email", rosterTestActor)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// 8. Toggle ON: reads the old value, updates, audit-logs old -> new, and
// returns the row identity with the new flag plus the next-tick note.
func TestSetDatasetExpressTogglesAndAudits(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewPartnerAdminHandler(db)

	mock.ExpectQuery(`SELECT name, COALESCE\(express_dispatch, false\)`).
		WithArgs(laneActionsDS1).
		WillReturnRows(sqlmock.NewRows([]string{"name", "express_dispatch"}).
			AddRow("Attribits-Auto", false))
	mock.ExpectExec(`UPDATE partner_datasets\s+SET express_dispatch = \$2`).
		WithArgs(laneActionsDS1, true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// old -> new must be in the audit row (action set_express_dispatch).
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WithArgs(rosterTestActor, "set_express_dispatch", "partner_dataset", laneActionsDS1,
			`{"express_dispatch":false}`, `{"express_dispatch":true}`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := httptest.NewRecorder()
	h.HandleSetDatasetExpress(rec, expressReq(t, laneActionsDS1, `{"enabled":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		ExpressDispatch bool   `json:"express_dispatch"`
		Note            string `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != laneActionsDS1 || resp.Name != "Attribits-Auto" || !resp.ExpressDispatch {
		t.Fatalf("response wrong: %+v", resp)
	}
	if !strings.Contains(resp.Note, "next tick") {
		t.Fatalf("note must state next-tick effect, got %q", resp.Note)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 9. NEGATIVE: unknown dataset -> 404 with no UPDATE and no audit row; a body
// without `enabled` -> 400 with zero SQL; a non-UUID id -> 400.
func TestSetDatasetExpressNegatives(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewPartnerAdminHandler(db)

	// Unknown id: the lookup finds no row -> 404, nothing else executes.
	mock.ExpectQuery(`SELECT name, COALESCE\(express_dispatch, false\)`).
		WithArgs(laneActionsDS2).
		WillReturnRows(sqlmock.NewRows([]string{"name", "express_dispatch"}))
	rec := httptest.NewRecorder()
	h.HandleSetDatasetExpress(rec, expressReq(t, laneActionsDS2, `{"enabled":true}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown dataset: want 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing `enabled`: 400 before any SQL.
	rec = httptest.NewRecorder()
	h.HandleSetDatasetExpress(rec, expressReq(t, laneActionsDS1, `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing enabled: want 400, got %d", rec.Code)
	}

	// Invalid UUID: 400 before any SQL.
	rec = httptest.NewRecorder()
	h.HandleSetDatasetExpress(rec, expressReq(t, "not-a-uuid", `{"enabled":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: want 400, got %d", rec.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("negative paths executed unexpected SQL: %v", err)
	}
}

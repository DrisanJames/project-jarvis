package api

// Lane-roster fixtures. What each test PINS (behavior, not implementation):
//   - the list shows INACTIVE rows (soft-disabled bindings must remain visible
//     or the operator cannot re-assign them);
//   - the weight clamp is SERVER-SIDE and the CLAMPED value is what reaches the
//     database — the assertion is on the argument actually sent, because a
//     stored 999 that the orchestrator silently runs at 20 is the defect;
//   - unassign is a SOFT DISABLE: the SQL sets active and contains no DELETE
//     (operator standing constraint — additive only, no deletes);
//   - the write gate is a REAL gate: with the env unset the handler answers 403
//     having executed ZERO statements (ExpectationsWereMet with no expectations
//     queued is the negative control);
//   - the LIST keeps working with the gate off and reports write_enabled=false,
//     which is what the UI reads to hide the edit surface.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const rosterTestActor = "operator@jv"

func rosterPost(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Email", rosterTestActor)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

type laneRosterListResp struct {
	Rows         []laneRosterRow `json:"rows"`
	WriteEnabled bool            `json:"write_enabled"`
	WriteFlagEnv string          `json:"write_flag_env"`
}

// 1. List by brand returns every row for that brand INCLUDING active=false.
func TestLaneRosterListByBrandIncludesInactive(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	updated := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`WHERE \(\$1 = ''`).
		WithArgs("mrd", "").
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "brand", "weight", "active", "sort_order", "updated_at", "updated_by"}).
			AddRow("consumer", "mrd", 3, true, 0, updated, rosterTestActor).
			AddRow("remodel", "mrd", 1, true, 1, updated, "").
			AddRow("term_life", "mrd", 1, false, 2, updated, rosterTestActor))

	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/lane-roster?brand=mrd", nil)
	rec := httptest.NewRecorder()
	s.HandleLaneRosterList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneRosterListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Rows) != 3 {
		t.Fatalf("want 3 rows, got %d (%s)", len(resp.Rows), rec.Body.String())
	}
	sawInactive := false
	for _, r := range resp.Rows {
		if !r.Active {
			sawInactive = true
			if r.Vertical != "term_life" {
				t.Fatalf("unexpected inactive row: %+v", r)
			}
		}
	}
	if !sawInactive {
		t.Fatal("inactive roster rows MUST be returned — the UI needs to show what is unassigned")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 2. Assign upserts and clamps weight 999 -> 20. The assertion is on the
// ARGUMENT ACTUALLY SENT, mirroring the orchestrator's
// GREATEST(1, LEAST(COALESCE(weight,1), 20)).
func TestLaneRosterAssignUpsertsAndClampsWeight(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)
	updated := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`vertical_known`).
		WithArgs("consumer", "mrd").
		WillReturnRows(sqlmock.NewRows([]string{"vertical_known", "weight", "active", "sort_order"}).
			AddRow(true, 3, true, 0))
	// The clamped 20 — not 999 — must be the value bound into the INSERT.
	mock.ExpectQuery(`INSERT INTO partner_drip_vertical_roster`).
		WithArgs("consumer", "mrd", 20, 2, rosterTestActor).
		WillReturnRows(sqlmock.NewRows([]string{"weight", "active", "sort_order", "updated_at", "updated_by"}).
			AddRow(20, true, 2, updated, rosterTestActor))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleLaneRosterAssign,
		"/api/mailing/pmta-campaign/property-ledger/lane-roster/assign",
		`{"vertical":"Consumer ","brand":"MRD","weight":999,"sort_order":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Row             laneRosterRow `json:"row"`
		WeightRequested int           `json:"weight_requested"`
		WeightClamped   bool          `json:"weight_clamped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Row.Weight != laneRosterWeightMax {
		t.Fatalf("weight must clamp to %d, got %d", laneRosterWeightMax, resp.Row.Weight)
	}
	if resp.WeightRequested != 999 || !resp.WeightClamped {
		t.Fatalf("response must disclose the clamp, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Unit-level clamp table — both ends plus the "omitted weight" case (0 -> 1).
func TestLaneRosterWeightClamp(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{-5, 1}, {0, 1}, {1, 1}, {7, 7}, {20, 20}, {21, 20}, {999, 20},
	} {
		if got := clampLaneRosterWeight(c.in); got != c.want {
			t.Fatalf("clamp(%d)=%d, want %d", c.in, got, c.want)
		}
	}
}

// 3. Unassign is a SOFT DISABLE: the statement sets active and contains NO
// DELETE. This is the operator's standing constraint encoded as a test — if
// someone "optimizes" the tombstone away, this fails.
func TestLaneRosterUnassignIsSoftDisableNeverDelete(t *testing.T) {
	upper := strings.ToUpper(laneRosterUnassignSQL)
	if strings.Contains(upper, "DELETE") {
		t.Fatalf("unassign MUST NOT delete the roster row:\n%s", laneRosterUnassignSQL)
	}
	if !strings.Contains(laneRosterUnassignSQL, "active = false") {
		t.Fatalf("unassign must set active = false:\n%s", laneRosterUnassignSQL)
	}
	if !strings.Contains(upper, "UPDATE PARTNER_DRIP_VERTICAL_ROSTER") {
		t.Fatalf("unassign must be an UPDATE on the roster table:\n%s", laneRosterUnassignSQL)
	}

	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)
	updated := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SET active = false`).
		WithArgs("term_life", "mrd", rosterTestActor).
		WillReturnRows(sqlmock.NewRows([]string{"weight", "sort_order", "updated_at"}).
			AddRow(1, 2, updated))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := rosterPost(t, s.HandleLaneRosterUnassign,
		"/api/mailing/pmta-campaign/property-ledger/lane-roster/unassign",
		`{"vertical":"term_life","brand":"mrd"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Row          laneRosterRow `json:"row"`
		SoftDisabled bool          `json:"soft_disabled"`
		Deleted      bool          `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Row.Active || !resp.SoftDisabled || resp.Deleted {
		t.Fatalf("unassign must report a surviving, inactive row: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 4. NEGATIVE: unknown brand -> 400, and no statement is executed (a bad brand
// must never reach the roster table).
func TestLaneRosterUnknownBrandRejected(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	for _, c := range []struct {
		name string
		body string
	}{
		{"assign", `{"vertical":"consumer","brand":"zz","weight":1}`},
		{"unassign", `{"vertical":"consumer","brand":"zz"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, mock := newLedgerServiceWithMock(t)
			h := s.HandleLaneRosterAssign
			if c.name == "unassign" {
				h = s.HandleLaneRosterUnassign
			}
			rec := rosterPost(t, h, "/x", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unknown brand must execute no SQL: %v", err)
			}
		})
	}

	// The GET side rejects it too.
	s, mock := newLedgerServiceWithMock(t)
	req := httptest.NewRequest("GET", "/x?brand=zz", nil)
	rec := httptest.NewRecorder()
	s.HandleLaneRosterList(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("list: want 400 for unknown brand, got %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// NEGATIVE: an unknown vertical is refused so a typo cannot mint a phantom lane
// the orchestrator would load every tick.
func TestLaneRosterUnknownVerticalRejected(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`vertical_known`).
		WithArgs("consumr", "mrd").
		WillReturnRows(sqlmock.NewRows([]string{"vertical_known", "weight", "active", "sort_order"}).
			AddRow(false, nil, nil, nil))

	rec := rosterPost(t, s.HandleLaneRosterAssign, "/x",
		`{"vertical":"consumr","brand":"mrd","weight":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "phantom lane") {
		t.Fatalf("error must explain the phantom-lane risk, got %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 5. NEGATIVE: write flag unset -> 403 AND zero statements executed. No
// expectations are queued, so ExpectationsWereMet proves the gate fired BEFORE
// any SQL — a late refusal would have already moved the row.
func TestLaneRosterWriteGateBlocksWithNoSQL(t *testing.T) {
	for _, env := range []string{"", "0", "true", "yes"} { // only "1" enables
		t.Run("env="+env, func(t *testing.T) {
			t.Setenv(laneRosterWriteFlagEnv, env)
			for _, c := range []struct {
				name string
				body string
			}{
				{"assign", `{"vertical":"consumer","brand":"mrd","weight":1}`},
				{"unassign", `{"vertical":"consumer","brand":"mrd"}`},
			} {
				s, mock := newLedgerServiceWithMock(t)
				h := s.HandleLaneRosterAssign
				if c.name == "unassign" {
					h = s.HandleLaneRosterUnassign
				}
				rec := rosterPost(t, h, "/x", c.body)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s: want 403, got %d: %s", c.name, rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), laneRosterWriteFlagEnv) {
					t.Fatalf("%s: 403 must name the env var, got %s", c.name, rec.Body.String())
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatalf("%s: gated write executed SQL: %v", c.name, err)
				}
			}
		})
	}
}

// 6. The LIST still 200s with the flag unset and reports write_enabled=false —
// read-only panel with an honest banner naming the env var.
func TestLaneRosterListWorksWithWriteFlagUnset(t *testing.T) {
	for _, c := range []struct {
		env  string
		want bool
	}{{"", false}, {"0", false}, {"1", true}} {
		t.Run("env="+c.env, func(t *testing.T) {
			t.Setenv(laneRosterWriteFlagEnv, c.env)
			s, mock := newLedgerServiceWithMock(t)
			mock.ExpectQuery(`WHERE \(\$1 = ''`).
				WithArgs("", "consumer").
				WillReturnRows(sqlmock.NewRows([]string{"vertical", "brand", "weight", "active", "sort_order", "updated_at", "updated_by"}).
					AddRow("consumer", "mrd", 3, true, 0, time.Now(), ""))

			req := httptest.NewRequest("GET", "/x?vertical=consumer", nil)
			rec := httptest.NewRecorder()
			s.HandleLaneRosterList(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("list must stay live with the write flag off, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp laneRosterListResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.WriteEnabled != c.want {
				t.Fatalf("env=%q: write_enabled=%v, want %v", c.env, resp.WriteEnabled, c.want)
			}
			if resp.WriteFlagEnv != "PROPERTY_LEDGER_ROSTER_WRITE_ENABLED" {
				t.Fatalf("payload must name the gate env, got %q", resp.WriteFlagEnv)
			}
			if len(resp.Rows) != 1 {
				t.Fatalf("want 1 row, got %d", len(resp.Rows))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Guard: brand or vertical is required — no unfiltered dump of every binding.
func TestLaneRosterListRequiresAFilter(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	s.HandleLaneRosterList(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

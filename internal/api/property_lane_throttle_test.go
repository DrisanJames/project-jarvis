package api

// Throttle read fixtures (Pipeline Cockpit plan P2 DoD):
//   - CONFIG-READ GATE (the C2 negative control): the cockpit's throttle EDIT
//     surface is gated by the SERVER env PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED
//     surfaced as write_enabled in this read — flag unset ⇒ write_enabled=false
//     (UI renders read-only with a banner naming the env var); "1" ⇒ true.
//   - Read mapping mirrors the admin overrides read (isp / pct_override /
//     max_per_wave / nullable daily_cap) + updated_at/updated_by, and renders
//     the effective posture: default_isps = LedgerGroups minus overridden.
//   - The TWO cap systems stay distinct: supply_release_daily_cap
//     (partner_datasets.daily_cap, SUPPLY side) is returned per feed beside
//     the claim-side overrides, never merged.
//   - Negative fixtures: unknown domain → 400; a query error → 500 (never a
//     silent partial-200).
//
// The WRITE path itself is the pre-existing HandleUpdateISPDistribution
// (partner_test.go TestHandleUpdateISPDistribution*) — validation + the
// partner_admin_audit_log row are asserted THERE; this file deliberately does
// not duplicate that coverage (no second writer, no second writer-test).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func getThrottle(t *testing.T, s *PMTACampaignService, domain string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/throttle?domain="+domain, nil)
	rec := httptest.NewRecorder()
	s.HandleLaneThrottle(rec, req)
	return rec
}

type laneThrottleResp struct {
	Domain          string             `json:"domain"`
	Brand           string             `json:"brand"`
	NonLedger       bool               `json:"non_ledger"`
	WriteEnabled    bool               `json:"write_enabled"`
	WriteFlagEnv    string             `json:"write_flag_env"`
	WriteEndpoint   string             `json:"write_endpoint"`
	ReplacementNote string             `json:"replacement_note"`
	Feeds           []laneThrottleFeed `json:"feeds"`
}

func expectThrottleEmptyRoster(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}))
}

// TestLaneThrottleWriteFlagConfigRead is the C2 "edit surface hidden when the
// flag is off" proof at the API layer: the UI renders the edit surface ONLY
// when write_enabled=true, and write_enabled reflects the server env — unset
// (the shipped HOLD-CRITICAL default) ⇒ false; "1" ⇒ true. Unsetting the env
// is the one-move rollback.
func TestLaneThrottleWriteFlagConfigRead(t *testing.T) {
	for _, c := range []struct {
		envValue string
		want     bool
	}{
		{"", false},  // shipped default: read-only panel
		{"0", false}, // explicit off is off — only "1" enables
		{"1", true},
	} {
		t.Run(fmt.Sprintf("env=%q", c.envValue), func(t *testing.T) {
			t.Setenv(laneThrottleWriteFlagEnv, c.envValue)
			s, mock := newLedgerServiceWithMock(t)
			expectThrottleEmptyRoster(mock)
			rec := getThrottle(t, s, "em.discountblog.com")
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			var resp laneThrottleResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.WriteEnabled != c.want {
				t.Fatalf("env=%q: write_enabled=%v, want %v", c.envValue, resp.WriteEnabled, c.want)
			}
			// The read-only banner names the env var — the payload must carry it.
			if resp.WriteFlagEnv != "PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED" {
				t.Fatalf("write_flag_env must name the gate env, got %q", resp.WriteFlagEnv)
			}
			// The one writer, reused — never a second writer endpoint.
			if !strings.Contains(resp.WriteEndpoint, "/data-partners/datasets/{id}/isp-distribution") {
				t.Fatalf("write_endpoint must point at the existing PUT, got %q", resp.WriteEndpoint)
			}
		})
	}
}

func TestLaneThrottleReadMapping(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	dsID := "44444444-4444-4444-4444-444444444444"
	updated := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("homeimprovement"))
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "daily_cap", "paused_emergency"}).
			AddRow(dsID, "feed-one", "active", 5000, false))
	mock.ExpectQuery(`SELECT brand FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db").AddRow("ht"))
	// gmail carries a lane daily budget; yahoo rides NULL (global default).
	mock.ExpectQuery(`FROM partner_isp_distribution_overrides`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "pct_override", "max_per_wave", "daily_cap", "updated_at", "updated_by"}).
			AddRow("gmail", 0.4, 1000, 500, updated, "operator@jv").
			AddRow("yahoo", 0.3, 0, nil, updated, ""))

	rec := getThrottle(t, s, "em.discountblog.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneThrottleResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Brand != "db" || resp.NonLedger {
		t.Fatalf("em.discountblog.com must resolve to ledger brand db, got %q/%v", resp.Brand, resp.NonLedger)
	}
	if len(resp.Feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(resp.Feeds))
	}
	f := resp.Feeds[0]
	if f.DatasetID != dsID || f.Name != "feed-one" || f.Vertical != "homeimprovement" {
		t.Fatalf("feed identity wrong: %+v", f)
	}
	// Cap system (1): the SUPPLY-side release budget, distinct field.
	if f.SupplyReleaseDailyCap != 5000 {
		t.Fatalf("supply_release_daily_cap must carry partner_datasets.daily_cap, got %d", f.SupplyReleaseDailyCap)
	}
	if len(f.SharedBrands) != 2 || f.SharedBrands[0] != "db" {
		t.Fatalf("shared_brands must carry the rotation, got %v", f.SharedBrands)
	}
	// Cap system (2): the claim-side override rows.
	if len(f.Overrides) != 2 {
		t.Fatalf("want 2 override rows, got %+v", f.Overrides)
	}
	g := f.Overrides[0]
	if g.ISP != "gmail" || g.PctOverride != 0.4 || g.MaxPerWave != 1000 ||
		g.DailyCap == nil || *g.DailyCap != 500 || g.UpdatedBy != "operator@jv" {
		t.Fatalf("gmail override mapping wrong: %+v", g)
	}
	y := f.Overrides[1]
	if y.ISP != "yahoo" || y.DailyCap != nil {
		t.Fatalf("yahoo NULL daily_cap must stay omitted (global default), got %+v", y)
	}
	// Effective posture: overridden ISPs are OUT of default_isps; the rest of
	// the ledger vocabulary rides the defaults.
	def := map[string]bool{}
	for _, d := range f.DefaultISPs {
		def[d] = true
	}
	if def["gmail"] || def["yahoo"] {
		t.Fatalf("overridden ISPs must not appear in default_isps: %v", f.DefaultISPs)
	}
	for _, want := range []string{"microsoft", "aol", "apple", "comcast", "other"} {
		if !def[want] {
			t.Fatalf("default_isps must include %q (rides global default): %v", want, f.DefaultISPs)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaneThrottleUnknownDomain400(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	if rec := getThrottle(t, s, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty domain: got %d, want 400", rec.Code)
	}
	mock.ExpectQuery(`FROM mailing_brand_metadata`).
		WillReturnError(fmt.Errorf("sql: no rows in result set"))
	if rec := getThrottle(t, s, "nope.example.com"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown domain: got %d, want 400", rec.Code)
	}
}

func TestLaneThrottleOverrideQueryErrorFails(t *testing.T) {
	// Negative control: a failed query must 500 — never a silent partial-200.
	s, mock := newLedgerServiceWithMock(t)
	dsID := "55555555-5555-5555-5555-555555555555"
	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("insurance"))
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "daily_cap", "paused_emergency"}).
			AddRow(dsID, "feed-one", "active", 0, false))
	mock.ExpectQuery(`SELECT brand FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db"))
	mock.ExpectQuery(`FROM partner_isp_distribution_overrides`).
		WillReturnError(fmt.Errorf("boom"))
	rec := getThrottle(t, s, "em.discountblog.com")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
}

func TestLaneThrottleSQLShape(t *testing.T) {
	if strings.Contains(laneThrottleOverridesSQL, "AT TIME ZONE") {
		t.Fatal("throttle SQL must not tz-cast")
	}
	if !strings.Contains(laneThrottleOverridesSQL, "WHERE dataset_id = $1") {
		t.Fatal("overrides SQL must be keyed by dataset_id (PK prefix)")
	}
	for _, col := range []string{"pct_override", "max_per_wave", "daily_cap", "updated_at", "updated_by"} {
		if !strings.Contains(laneThrottleOverridesSQL, col) {
			t.Fatalf("overrides SQL missing %q", col)
		}
	}
}

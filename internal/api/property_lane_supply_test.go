package api

// Supply strip fixtures (Pipeline Cockpit plan P1 DoD):
//   - SQL shape: the anatomy + ready-by-ISP statements carry NO tz-cast
//     (`AT TIME ZONE` is the drip_lanes.go seq-scan footgun) — Denver bounds
//     are Go-computed and param-injected ($2/$3); ready-by-ISP keeps the
//     `dataset_id = $1 AND status = 'ready' … GROUP BY isp_family` shape that
//     rides idx_pcq_isp_family.
//   - Denver bounds helper is DST-correct (23h/24h/25h days), UTC out.
//   - Feeds resolution mirrors HandleLaneContent (roster → datasets), counts
//     map through to the payload, cleaning = pending_eo + eo_in_flight.
//   - Negative fixtures: unknown domain → 400 (no partial data); a query
//     error → 500 (never a silent partial-200); a dataset with zero pcq rows
//     → all-zero counts, not an error.
//   - wcl-heloc resolves as a first-class NON-ledger domain through
//     mailing_brand_metadata (non_ledger=true).

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

func TestLaneSupplySQLShape(t *testing.T) {
	for name, q := range map[string]string{
		"anatomy":      laneSupplyAnatomySQL,
		"ready-by-isp": laneSupplyReadyByISPSQL,
	} {
		if strings.Contains(q, "AT TIME ZONE") {
			t.Fatalf("%s SQL must not tz-cast (Denver bounds are Go-computed and param-injected)", name)
		}
	}
	if !strings.Contains(laneSupplyAnatomySQL, "mailed_at >= $2 AND mailed_at < $3") {
		t.Fatal("anatomy SQL must take Go-computed UTC day bounds as $2/$3")
	}
	for _, frag := range []string{
		"FILTER (WHERE status IN ('pending_eo','eo_in_flight'))",
		"FILTER (WHERE status = 'ready')",
		"FILTER (WHERE status = 'held')",
		"FILTER (WHERE status = 'suppressed_eo')",
		"FILTER (WHERE status = 'dead_letter')",
		"WHERE dataset_id = $1",
	} {
		if !strings.Contains(laneSupplyAnatomySQL, frag) {
			t.Fatalf("anatomy SQL missing %q", frag)
		}
	}
	// The ready-by-ISP query must keep the (dataset_id, isp_family, status)
	// shape that rides idx_pcq_isp_family.
	if !strings.Contains(laneSupplyReadyByISPSQL, "WHERE dataset_id = $1 AND status = 'ready'") ||
		!strings.Contains(laneSupplyReadyByISPSQL, "GROUP BY isp_family") {
		t.Fatal("ready-by-isp SQL must filter (dataset_id, status='ready') and group by isp_family (idx_pcq_isp_family)")
	}
}

func TestLaneSupplyDenverBoundsUTC(t *testing.T) {
	cases := []struct {
		name  string
		at    string // RFC3339 instant inside the Denver day
		start string
		end   string
		hours float64
	}{
		// Ordinary MDT day (UTC-6): 24h.
		{"ordinary", "2026-08-17T18:00:00Z", "2026-08-17T06:00:00Z", "2026-08-18T06:00:00Z", 24},
		// Spring-forward day 2026-03-08: 23h (MST 07:00Z start → MDT 06:00Z end).
		{"spring-forward", "2026-03-08T20:00:00Z", "2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z", 23},
		// Fall-back day 2026-11-01: 25h.
		{"fall-back", "2026-11-01T20:00:00Z", "2026-11-01T06:00:00Z", "2026-11-02T07:00:00Z", 25},
	}
	for _, c := range cases {
		at, _ := time.Parse(time.RFC3339, c.at)
		start, end := laneSupplyDenverDayBoundsUTC(at)
		if start.Format(time.RFC3339) != c.start || end.Format(time.RFC3339) != c.end {
			t.Fatalf("%s: got [%s, %s), want [%s, %s)", c.name,
				start.Format(time.RFC3339), end.Format(time.RFC3339), c.start, c.end)
		}
		if got := end.Sub(start).Hours(); got != c.hours {
			t.Fatalf("%s: day length %v h, want %v h", c.name, got, c.hours)
		}
		if start.Location() != time.UTC || end.Location() != time.UTC {
			t.Fatalf("%s: bounds must be UTC instants", c.name)
		}
	}
}

func getSupply(t *testing.T, s *PMTACampaignService, domain string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/supply?domain="+domain, nil)
	rec := httptest.NewRecorder()
	s.HandleLaneSupply(rec, req)
	return rec
}

func TestLaneSupplyUnknownDomain400(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	// Empty domain: rejected before any DB access.
	if rec := getSupply(t, s, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty domain: got %d, want 400", rec.Code)
	}

	// Unknown domain: static roster misses, metadata lookup finds nothing.
	mock.ExpectQuery(`FROM mailing_brand_metadata`).
		WillReturnError(fmt.Errorf("sql: no rows in result set"))
	if rec := getSupply(t, s, "nope.example.com"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown domain: got %d, want 400", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSupplyRosterErrorFails(t *testing.T) {
	// Negative control: a failed query must 500 — never a silent partial-200.
	s, mock := newLedgerServiceWithMock(t)
	expectSupplyIndexValid(mock, true)
	mock.ExpectQuery(`SELECT vertical`).WillReturnError(fmt.Errorf("boom"))
	rec := getSupply(t, s, "em.discountblog.com") // resolves statically to db
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}
}

type laneSupplyResp struct {
	Domain         string           `json:"domain"`
	Brand          string           `json:"brand"`
	SendingDomain  string           `json:"sending_domain"`
	NonLedger      bool             `json:"non_ledger"`
	ReadySemantics string           `json:"ready_semantics"`
	Feeds          []laneSupplyFeed `json:"feeds"`
}

func expectSupplyFeedQueries(mock sqlmock.Sqlmock, vertical, dsID string, withAnatomyRow bool) {
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "daily_cap", "paused_emergency"}).
			AddRow(dsID, "feed-one", "active", 5000, false))
	mock.ExpectQuery(`SELECT brand FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db").AddRow("ht").AddRow("mh"))
	anatomy := sqlmock.NewRows([]string{"dataset_id", "tranche_total", "cleaning", "pending_eo",
		"eo_in_flight", "ready_total", "held", "suppressed", "dead_letter", "mailed_lifetime", "mailed_today"})
	if withAnatomyRow {
		anatomy.AddRow(dsID, 1000, 150, 100, 50, 300, 200, 40, 10, 300, 25)
	}
	mock.ExpectQuery(`AS tranche_total`).WillReturnRows(anatomy)
	mock.ExpectQuery(`GROUP BY isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "count"}).
			AddRow("yahoo", 120).AddRow("gmail", 150).AddRow("weirdisp", 5).AddRow("other", 25))
}

func expectSupplyIndexValid(mock sqlmock.Sqlmock, valid bool) {
	mock.ExpectQuery(`indisvalid`).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(valid))
}

func TestLaneSupplyCountsMapping(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	dsID := "11111111-1111-1111-1111-111111111111"
	expectSupplyIndexValid(mock, true)

	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("homeimprovement"))
	expectSupplyFeedQueries(mock, "homeimprovement", dsID, true)

	rec := getSupply(t, s, "em.discountblog.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneSupplyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Brand != "db" || resp.NonLedger {
		t.Fatalf("em.discountblog.com must resolve to ledger brand db (non_ledger=false), got %q/%v", resp.Brand, resp.NonLedger)
	}
	if resp.ReadySemantics != laneSupplyReadySemantics ||
		!strings.Contains(resp.ReadySemantics, "Verified (1)") ||
		!strings.Contains(resp.ReadySemantics, "Complainer (7)") {
		t.Fatalf("ready_semantics must state the markReady construction, got %q", resp.ReadySemantics)
	}
	if len(resp.Feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(resp.Feeds))
	}
	f := resp.Feeds[0]
	if f.DatasetID != dsID || f.Name != "feed-one" || f.Vertical != "homeimprovement" {
		t.Fatalf("feed identity wrong: %+v", f)
	}
	if f.DailyCap != 5000 || f.Paused {
		t.Fatalf("daily_cap/paused wrong: %+v", f)
	}
	if f.TrancheTotal != 1000 || f.Cleaning != 150 || f.PendingEO != 100 || f.EOInFlight != 50 ||
		f.ReadyTotal != 300 || f.Held != 200 || f.Suppressed != 40 || f.DeadLetter != 10 ||
		f.MailedLifetime != 300 || f.MailedToday != 25 {
		t.Fatalf("counts mapping wrong: %+v", f)
	}
	if f.Cleaning != f.PendingEO+f.EOInFlight {
		t.Fatalf("cleaning headline must equal pending_eo + eo_in_flight split")
	}
	// Shared-pool framing: dataset supply shared across the rotation brands.
	if len(f.SharedBrands) != 3 || f.SharedBrands[0] != "db" {
		t.Fatalf("shared_brands must carry the vertical's active rotation, got %v", f.SharedBrands)
	}
	// Canonical ISP order: gmail before yahoo before other; strangers last.
	wantOrder := []string{"gmail", "yahoo", "other", "weirdisp"}
	if len(f.ReadyByISP) != len(wantOrder) {
		t.Fatalf("ready_by_isp: got %v", f.ReadyByISP)
	}
	var sum int64
	for i, e := range f.ReadyByISP {
		if e.ISP != wantOrder[i] {
			t.Fatalf("ready_by_isp order: got %v, want %v", f.ReadyByISP, wantOrder)
		}
		sum += e.Ready
	}
	if sum != 300 {
		t.Fatalf("ready_by_isp must sum to ready_total, got %d", sum)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSupplyEmptyDatasetIsZerosNotError(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	expectSupplyIndexValid(mock, true)
	dsID := "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("insurance"))
	// Anatomy GROUP BY returns no row for an empty dataset — the handler must
	// render zeros (the queue's true state), not fail.
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "daily_cap", "paused_emergency"}).
			AddRow(dsID, "feed-empty", "active", 0, true))
	mock.ExpectQuery(`SELECT brand FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db"))
	mock.ExpectQuery(`AS tranche_total`).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "tranche_total", "cleaning", "pending_eo",
			"eo_in_flight", "ready_total", "held", "suppressed", "dead_letter", "mailed_lifetime", "mailed_today"}))
	mock.ExpectQuery(`GROUP BY isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "count"}))

	rec := getSupply(t, s, "em.discountblog.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneSupplyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Feeds) != 1 {
		t.Fatalf("want 1 feed, got %d", len(resp.Feeds))
	}
	f := resp.Feeds[0]
	if f.TrancheTotal != 0 || f.ReadyTotal != 0 || f.Held != 0 || len(f.ReadyByISP) != 0 {
		t.Fatalf("empty dataset must render zeros: %+v", f)
	}
	if !f.Paused {
		t.Fatal("paused_emergency must map through")
	}
}

func TestLaneSupplyWclNonLedgerDomain(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	dsID := "33333333-3333-3333-3333-333333333333"

	// wcl-heloc.com is NOT a drip roster brand — it resolves through the
	// mailing_brand_metadata overlay and is flagged non_ledger (plan §1).
	mock.ExpectQuery(`FROM mailing_brand_metadata`).
		WillReturnRows(sqlmock.NewRows([]string{"brand_code", "sending_domain"}).
			AddRow("wcl", "m.wcl-heloc.com"))
	expectSupplyIndexValid(mock, true)
	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("heloc"))
	expectSupplyFeedQueries(mock, "heloc", dsID, true)

	rec := getSupply(t, s, "wcl-heloc.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneSupplyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Brand != "wcl" || !resp.NonLedger || resp.SendingDomain != "m.wcl-heloc.com" {
		t.Fatalf("wcl resolution wrong: brand=%q non_ledger=%v sending_domain=%q",
			resp.Brand, resp.NonLedger, resp.SendingDomain)
	}
	if len(resp.Feeds) != 1 || resp.Feeds[0].Vertical != "heloc" {
		t.Fatalf("wcl feeds wrong: %+v", resp.Feeds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLaneSupplyIndexNotValid503(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	expectSupplyIndexValid(mock, false)
	rec := getSupply(t, s, "em.discountblog.com")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while the index is building, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "idx_pcq_dataset_status_mailed") {
		t.Fatalf("503 should name the index: %s", rec.Body.String())
	}
}

// TestLaneSupplyCacheCollapse — load-collapse addendum: the anatomy
// aggregate (~1.8s / ~550k shared buffers on the largest live dataset) must
// run at most ONCE per dataset per TTL. Two immediate consecutive handler
// calls: sqlmock (ordered) expects exactly one index probe and exactly one
// anatomy + ready-by-isp pair — the second call re-resolves the cheap feed
// identities but serves counts from the cache, with the SAME computed_at.
func TestLaneSupplyCacheCollapse(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	dsID := "66666666-6666-6666-6666-666666666666"

	// Call 1: index probe + resolution + anatomy + ready.
	expectSupplyIndexValid(mock, true)
	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("homeimprovement"))
	expectSupplyFeedQueries(mock, "homeimprovement", dsID, true)

	// Call 2: resolution ONLY — no index probe (once-valid-stays-valid), no
	// anatomy, no ready-by-isp. Ordered sqlmock fails the test if the
	// handler runs the scan again.
	mock.ExpectQuery(`SELECT vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical"}).AddRow("homeimprovement"))
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "daily_cap", "paused_emergency"}).
			AddRow(dsID, "feed-one", "active", 5000, false))
	mock.ExpectQuery(`SELECT brand FROM partner_drip_vertical_roster`).
		WillReturnRows(sqlmock.NewRows([]string{"brand"}).AddRow("db").AddRow("ht").AddRow("mh"))

	rec1 := getSupply(t, s, "em.discountblog.com")
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 got %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := getSupply(t, s, "em.discountblog.com")
	if rec2.Code != http.StatusOK {
		t.Fatalf("call 2 got %d: %s", rec2.Code, rec2.Body.String())
	}

	var r1, r2 struct {
		Feeds []laneSupplyFeed `json:"feeds"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &r1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &r2); err != nil {
		t.Fatal(err)
	}
	if len(r1.Feeds) != 1 || len(r2.Feeds) != 1 {
		t.Fatalf("want 1 feed per call, got %d/%d", len(r1.Feeds), len(r2.Feeds))
	}
	if r2.Feeds[0].ReadyTotal != r1.Feeds[0].ReadyTotal || r2.Feeds[0].TrancheTotal != r1.Feeds[0].TrancheTotal {
		t.Fatalf("cached counts must match the scan: %+v vs %+v", r2.Feeds[0], r1.Feeds[0])
	}
	if r1.Feeds[0].ComputedAt.IsZero() || !r2.Feeds[0].ComputedAt.Equal(r1.Feeds[0].ComputedAt) {
		t.Fatalf("computed_at must be the scan instant on both calls (cache hit): %v vs %v",
			r1.Feeds[0].ComputedAt, r2.Feeds[0].ComputedAt)
	}
	// The decisive assertion: every expectation consumed, none left — the
	// anatomy query ran exactly once across both calls.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

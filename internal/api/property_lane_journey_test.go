package api

// Lane-journey fixtures:
//   - REGRESSION GUARD: the waiting census keeps all four clauses of the
//     idx_pcq_followup_isp partial predicate (status='mailed', engaged_at IS
//     NULL, terminal_reason IS NULL, plus the vertical leading key). Dropping
//     the two NULL clauses measured 16.9s vs 1.26s; keying on dataset_id
//     measured 56s — both over the prod 30s statement_timeout.
//   - Edges are assembled from the grouped rows with by_isp nested under the
//     edge, and the structural ladder is always drawn (zero-waiting edges are
//     emitted, not omitted).
//   - Negative fixtures: unknown brand -> 400 and missing/junk vertical -> 400,
//     each proven to fire BEFORE any DB access; a query error -> 500, never a
//     silent partial-200.
//   - A touch with no content row renders configured:false rather than being
//     dropped — that is how the UI shows a ladder that retires early.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type laneJourneyResp struct {
	OrganizationID string             `json:"organization_id"`
	Brand          string             `json:"brand"`
	SendingDomain  string             `json:"sending_domain"`
	Vertical       string             `json:"vertical"`
	DelayHours     int                `json:"delay_hours"`
	MaxTouches     int                `json:"max_touches"`
	Touches        []laneJourneyTouch `json:"touches"`
	Edges          []laneJourneyEdge  `json:"edges"`
	Totals         laneJourneyTotals  `json:"totals"`
	ScopeNote      string             `json:"scope_note"`
}

func getJourney(t *testing.T, s *PMTACampaignService, brand, vertical string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/lane-journey?brand="+brand+"&vertical="+vertical, nil)
	rec := httptest.NewRecorder()
	s.HandleLaneJourney(rec, req)
	return rec
}

// TestLaneJourneyWaitingSQLShape is the index-predicate regression guard.
func TestLaneJourneyWaitingSQLShape(t *testing.T) {
	// All four clauses of the idx_pcq_followup_isp partial predicate.
	for _, frag := range []string{
		"vertical = $1",
		"status = 'mailed'",
		"engaged_at IS NULL",
		"terminal_reason IS NULL",
		"next_touch_at IS NOT NULL",
		"GROUP BY touch_count, isp_family",
	} {
		if !strings.Contains(laneJourneyWaitingSQL, frag) {
			t.Fatalf("waiting SQL missing %q — it must match the idx_pcq_followup_isp "+
				"partial index (measured 1.26s with all clauses, 16.9s without the "+
				"NULL clauses, 56s keyed on dataset_id)", frag)
		}
	}
	// The 56s variant: never key this census on dataset_id.
	if strings.Contains(laneJourneyWaitingSQL, "dataset_id") {
		t.Fatal("waiting SQL must key on vertical, never dataset_id (measured 56s — over the prod 30s statement_timeout)")
	}
	// Denver tz-casting in a WHERE clause is the documented seq-scan footgun.
	if strings.Contains(laneJourneyWaitingSQL, "AT TIME ZONE") {
		t.Fatal("waiting SQL must not tz-cast")
	}
}

// TestLaneJourneyRejectsBadInput proves both gates fire before any DB access:
// the mock is created with ZERO expectations, so any query would fail.
func TestLaneJourneyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		brand    string
		vertical string
	}{
		{"unknown brand", "zz", "internal_auto_insurance"},
		{"empty brand", "", "internal_auto_insurance"},
		{"kumo brand not on drip roster", "aad", "internal_auto_insurance"},
		{"missing vertical", "db", ""},
		{"blank vertical", "db", "%20%20"},
		{"uppercase vertical", "db", "Internal_Auto"},
		{"vertical with a space", "db", "internal%20auto"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, mock := newLedgerServiceWithMock(t)
			rec := getJourney(t, s, c.brand, c.vertical)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d (%s), want 400", rec.Code, rec.Body.String())
			}
			// No query may have been issued.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func expectJourneyTouch1(mock sqlmock.Sqlmock, active bool) {
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "subject_line", "preheader",
			"from_name", "offer_id", "active"}).
			AddRow("welcome-auto.html", "Your auto quote", "60 seconds", "Jamie", "", active))
}

func TestLaneJourneyHappyPath(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	soonest := time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 8, 20, 2, 30, 0, 0, time.UTC)

	expectJourneyTouch1(mock, true)

	// Touch 2 has BOTH a vertical-specific row and a global fallback: the
	// vertical row must win (resolveFollowupCreative's ORDER BY). Touch 3 has
	// only the global fallback. Touches 4-5 have nothing.
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}).
			AddRow(2, "internal_auto_insurance", "db", "t2-auto.html", "Still shopping?", "pre2", "Jamie", "", true).
			AddRow(2, "", "db", "t2-global.html", "GLOBAL should lose", "preG", "Jamie", "", true).
			AddRow(3, "", "db", "t3-global.html", "Last call", "pre3", "Jamie", "offer-3", true))

	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}).
			AddRow(1, "gmail", 257, 0, soonest, latest).
			AddRow(1, "microsoft", 164, 0, soonest, latest).
			AddRow(2, "gmail", 40, 40, soonest, soonest))

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Brand != "db" || resp.SendingDomain != "em.discountblog.com" {
		t.Fatalf("identity wrong: %+v", resp)
	}
	if resp.Vertical != "internal_auto_insurance" {
		t.Fatalf("vertical wrong: %q", resp.Vertical)
	}
	// Ladder shape constants come from the orchestrator, not from thin air.
	if resp.DelayHours != 24 {
		t.Fatalf("delay_hours must mirror followupTouchGapHours (24), got %d", resp.DelayHours)
	}
	if resp.MaxTouches != worker.MaxTouchCount {
		t.Fatalf("max_touches must be worker.MaxTouchCount (%d), got %d", worker.MaxTouchCount, resp.MaxTouches)
	}
	if resp.ScopeNote == "" || !strings.Contains(resp.ScopeNote, "VERTICAL") {
		t.Fatalf("scope_note must state the vertical-shared framing, got %q", resp.ScopeNote)
	}

	// Nodes: one per touch 1..max, always present.
	if len(resp.Touches) != worker.MaxTouchCount {
		t.Fatalf("want %d touch nodes, got %d", worker.MaxTouchCount, len(resp.Touches))
	}
	if got := resp.Touches[0]; got.Touch != 1 || got.SubjectLine != "Your auto quote" ||
		got.CreativeFilename != "welcome-auto.html" || got.FromName != "Jamie" ||
		!got.Configured || got.Source != "touch1" {
		t.Fatalf("touch 1 wrong: %+v", got)
	}
	// Vertical-specific row beats the global fallback at touch 2.
	if got := resp.Touches[1]; got.SubjectLine != "Still shopping?" || got.Source != "vertical" || !got.Configured {
		t.Fatalf("touch 2 must resolve to the vertical-specific row: %+v", got)
	}
	if got := resp.Touches[2]; got.SubjectLine != "Last call" || got.Source != "global-fallback" ||
		!got.Configured || got.OfferID != "offer-3" {
		t.Fatalf("touch 3 must resolve to the global fallback row: %+v", got)
	}

	// Edges: structural ladder 1->2 .. 4->5, counts from the grouped rows.
	if len(resp.Edges) != worker.MaxTouchCount-1 {
		t.Fatalf("want %d edges, got %d", worker.MaxTouchCount-1, len(resp.Edges))
	}
	e := resp.Edges[0]
	if e.FromTouch != 1 || e.ToTouch != 2 {
		t.Fatalf("first edge must be 1->2: %+v", e)
	}
	if e.Waiting != 421 {
		t.Fatalf("edge 1->2 waiting must sum the ISP rows (257+164=421), got %d", e.Waiting)
	}
	// by_isp nests UNDER the edge, in canonical ledger order (gmail < microsoft).
	if len(e.ByISP) != 2 || e.ByISP[0].ISP != "gmail" || e.ByISP[0].Waiting != 257 ||
		e.ByISP[1].ISP != "microsoft" || e.ByISP[1].Waiting != 164 {
		t.Fatalf("by_isp nesting/order wrong: %+v", e.ByISP)
	}
	if e.Soonest == nil || !e.Soonest.Equal(soonest) || e.Latest == nil || !e.Latest.Equal(latest) {
		t.Fatalf("edge window wrong: %+v / %+v", e.Soonest, e.Latest)
	}
	if resp.Edges[1].Waiting != 40 || resp.Edges[1].DueNow != 40 {
		t.Fatalf("edge 2->3 wrong: %+v", resp.Edges[1])
	}
	// Structural edges with nobody on them are drawn, not omitted.
	if resp.Edges[3].FromTouch != 4 || resp.Edges[3].Waiting != 0 || len(resp.Edges[3].ByISP) != 0 {
		t.Fatalf("empty structural edge must still render: %+v", resp.Edges[3])
	}
	if resp.Totals.InFlight != 461 || resp.Totals.DueNow != 40 {
		t.Fatalf("totals wrong: %+v", resp.Totals)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLaneJourneyUnconfiguredTouchRendered: a ladder that retires early must
// SHOW the retirement (configured:false), never silently drop the node.
func TestLaneJourneyUnconfiguredTouchRendered(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	expectJourneyTouch1(mock, true)
	// Only touch 2 is configured for this brand. Touch 3 exists for ANOTHER
	// brand (the stall state: vertical_configured true, configured false).
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}).
			AddRow(2, "internal_auto_insurance", "db", "t2.html", "Touch two", "p", "Jamie", "", true).
			AddRow(3, "internal_auto_insurance", "ht", "t3.html", "Other brand", "p", "Hank", "", true))
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Touches) != worker.MaxTouchCount {
		t.Fatalf("every touch <= max must be present, got %d nodes", len(resp.Touches))
	}
	for i, want := range []bool{true, true, false, false, false} {
		got := resp.Touches[i]
		if got.Touch != i+1 {
			t.Fatalf("node %d has touch %d", i, got.Touch)
		}
		if got.Configured != want {
			t.Fatalf("touch %d configured=%v, want %v (%+v)", i+1, got.Configured, want, got)
		}
	}
	// Touch 3: configured false for THIS brand, but the vertical has an active
	// row — followupTouchConfigured (no brand filter) would NOT retire it.
	if t3 := resp.Touches[2]; t3.SubjectLine != "" || t3.Source != "" || !t3.VerticalConfigured {
		t.Fatalf("touch 3 must render empty+unconfigured but vertical_configured: %+v", t3)
	}
	// Touch 4: nothing anywhere.
	if t4 := resp.Touches[3]; t4.VerticalConfigured {
		t.Fatalf("touch 4 must be unconfigured at the vertical level too: %+v", t4)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLaneJourneyQueryErrorsFail500: negative control — no silent partial-200.
func TestLaneJourneyQueryErrorsFail500(t *testing.T) {
	t.Run("census query error", func(t *testing.T) {
		s, mock := newLedgerServiceWithMock(t)
		expectJourneyTouch1(mock, true)
		mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
			WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
				"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
		mock.ExpectQuery(`GROUP BY touch_count, isp_family`).WillReturnError(fmt.Errorf("statement timeout"))
		if rec := getJourney(t, s, "db", "internal_auto_insurance"); rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
	})
	t.Run("followup query error", func(t *testing.T) {
		s, mock := newLedgerServiceWithMock(t)
		expectJourneyTouch1(mock, true)
		mock.ExpectQuery(`FROM partner_drip_followup_creatives`).WillReturnError(fmt.Errorf("boom"))
		if rec := getJourney(t, s, "db", "internal_auto_insurance"); rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
	})
}

// TestLaneJourneyMissingTouch1IsNotAnError: a lane with no welcome row still
// renders (the operator needs to SEE the hole), it does not 500.
func TestLaneJourneyMissingTouch1IsNotAnError(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM partner_drip_creatives`).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Touches[0].Configured || resp.Touches[0].Touch != 1 {
		t.Fatalf("missing touch-1 row must render as an unconfigured node: %+v", resp.Touches[0])
	}
}

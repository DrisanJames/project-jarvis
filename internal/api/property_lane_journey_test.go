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
	OrganizationID string                  `json:"organization_id"`
	Brand          string                  `json:"brand"`
	SendingDomain  string                  `json:"sending_domain"`
	Vertical       string                  `json:"vertical"`
	DelayHours     int                     `json:"delay_hours"`
	MaxTouches     int                     `json:"max_touches"`
	Touches        []laneJourneyTouch      `json:"touches"`
	Edges          []laneJourneyEdge       `json:"edges"`
	Totals         laneJourneyTotals       `json:"totals"`
	ScopeNote      string                  `json:"scope_note"`
	SendActivity   laneJourneySendActivity `json:"send_activity"`
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

// expectJourneyDominant stands in for the dominant-dataset lookup that decides
// whether touch 1 resolves through the offer centre (refreshVerticalState
// phase 2). It runs FIRST, before any content query.
func expectJourneyDominant(mock sqlmock.Sqlmock, datasetID, slug, offerID string) {
	mock.ExpectQuery(`LEFT JOIN partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "offer_id"}).
			AddRow(datasetID, slug, offerID))
}

// expectJourneyOffers stands in for the single offer-pool census. rows are
// (offer_id, name, status, creatives, subjects, from_names, first_subject).
func expectJourneyOffers(mock sqlmock.Sqlmock, rows ...[7]interface{}) {
	r := sqlmock.NewRows([]string{"id", "name", "status", "creatives", "subjects", "from_names", "first_subject"})
	for _, v := range rows {
		r = r.AddRow(v[0], v[1], v[2], v[3], v[4], v[5], v[6])
	}
	mock.ExpectQuery(`FROM mailing_offers`).WillReturnRows(r)
}

// expectJourneyActivity stands in for the send-activity reads (rotation state
// row, then the per-family wave/message counts).
func expectJourneyActivity(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM partner_drip_state`).
		WillReturnRows(sqlmock.NewRows([]string{"last_wave_at", "last_wave_brand", "last_wave_size"}))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"is_followup", "last_wave_at",
			"waves_1h", "sent_1h", "waves_today", "sent_today"}))
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

	// No dataset-bound offer on this vertical: touch 1 resolves through
	// partner_drip_creatives, as it did before this change.
	expectJourneyDominant(mock, "ds-1", "feed-one", "")
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

	// Touch 3 carries an offer_id but ALSO a filename — the filename wins, so
	// no offer needs resolving and no census query is issued.
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}).
			AddRow(1, "gmail", 257, 0, soonest, latest).
			AddRow(1, "microsoft", 164, 0, soonest, latest).
			AddRow(2, "gmail", 40, 40, soonest, soonest))
	expectJourneyActivity(mock)

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
	expectJourneyDominant(mock, "ds-1", "feed-one", "")
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
	expectJourneyActivity(mock)

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
		expectJourneyDominant(mock, "ds-1", "feed-one", "")
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
		expectJourneyDominant(mock, "ds-1", "feed-one", "")
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
	expectJourneyDominant(mock, "ds-1", "feed-one", "")
	mock.ExpectQuery(`FROM partner_drip_creatives`).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
	expectJourneyActivity(mock)

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

// ── COMPLAINT 2 (operator 2026-08-21): "(no creative bound)" on all touches ──
//
// An empty creative_filename is the OFFER-CENTER path, not an empty binding.
// Verified on prod for (db, internal_auto_insurance): all four configured
// touches carry creative_filename=” + offer_id, and every one of them was
// rendering as "no creative bound".
func TestLaneJourneyOfferPathIsNotUnbound(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	const offerID = "48ddcdf4-5bd0-4cca-a8e8-878a72d6702a"

	expectJourneyDominant(mock, "ds-1", "feed-one", "")
	// Touch 1: empty filename + offer_id -> resolveCreative's offer branch.
	mock.ExpectQuery(`FROM partner_drip_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "subject_line", "preheader",
			"from_name", "offer_id", "active"}).
			AddRow("", "your quotes are ready", "", "Discount Auto Insurance", offerID, true))
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}).
			AddRow(2, "internal_auto_insurance", "db", "", "still saved", "", "Discount Auto Insurance", offerID, true).
			// Touch 3: a filename WINS even though the row carries an offer.
			AddRow(3, "internal_auto_insurance", "db", "t3.html", "last look", "", "Jamie", offerID, true).
			// Touch 4: neither a file nor an offer -> the resolver reads an
			// empty path and the wave errors.
			AddRow(4, "internal_auto_insurance", "db", "", "orphan", "", "Jamie", "", true))
	expectJourneyOffers(mock, [7]interface{}{offerID, "AutoCoveragePoint - Quote Ready", "active", 1, 3, 1, "Your 3 quotes are waiting"})
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
	expectJourneyActivity(mock)

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	t1 := resp.Touches[0]
	if t1.ContentSource != "offer" || !t1.Resolves {
		t.Fatalf("touch 1 (empty filename + offer_id) must resolve via the OFFER path: %+v", t1)
	}
	if t1.ResolutionLabel != "offer: AutoCoveragePoint - Quote Ready" {
		t.Fatalf("the node must name the offer, not report an unbound creative: %q", t1.ResolutionLabel)
	}
	if t1.OfferSource != "touch-row" {
		t.Fatalf("touch 1 offer came from the creative ROW here (no dataset offer): %+v", t1)
	}
	if t1.Offer == nil || t1.Offer.Subjects != 3 || t1.Offer.FromNames != 1 {
		t.Fatalf("the pool census must ride along so the operator sees WHY it resolves: %+v", t1.Offer)
	}

	t2 := resp.Touches[1]
	if t2.ContentSource != "offer" || !t2.Resolves {
		t.Fatalf("touch 2 must resolve via the offer path: %+v", t2)
	}

	// A configured filename WINS on touch 2+ (resolveFollowupCreative:4924).
	t3 := resp.Touches[2]
	if t3.ContentSource != "file" || t3.ResolutionLabel != "t3.html" {
		t.Fatalf("a configured filename must win over the row's offer_id: %+v", t3)
	}
	if !strings.Contains(t3.ResolutionDetail, "suppression/attribution only") {
		t.Fatalf("the offer_id on a file-path touch must be labeled as scrub-only: %q", t3.ResolutionDetail)
	}

	// Neither file nor offer: the orchestrator reads "" and errors.
	t4 := resp.Touches[3]
	if t4.Resolves || t4.ContentSource != "none" {
		t.Fatalf("a row with no file and no offer must be UNRESOLVABLE: %+v", t4)
	}
	if !strings.Contains(t4.ResolutionLabel, "UNRESOLVABLE") {
		t.Fatalf("the label must be alarming, got %q", t4.ResolutionLabel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLaneJourneyArchivedPoolIsUnresolvable: an offer whose subject or
// from-name pool is entirely archived HARD-ERRORS at send ("offer has no
// active from-names", partner_drip_orchestrator.go:1913). The node must say
// so, and say WHICH pool.
func TestLaneJourneyArchivedPoolIsUnresolvable(t *testing.T) {
	for _, c := range []struct {
		name                           string
		creatives, subjects, fromNames int
		wantFragment                   string
	}{
		{"from-names all archived", 1, 3, 0, "FROM-NAME pool is empty"},
		{"subjects all archived", 1, 0, 1, "SUBJECT pool is empty"},
		{"creatives all archived", 0, 3, 1, "CREATIVE pool is empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, mock := newLedgerServiceWithMock(t)
			const offerID = "0f0f0f0f-0000-0000-0000-000000000001"
			// This time the DATASET binds the offer: partner_drip_creatives is
			// still read for the row, but the dataset's offer wins for touch 1.
			expectJourneyDominant(mock, "ds-1", "attribits-internal-car-insurance", offerID)
			mock.ExpectQuery(`FROM partner_drip_creatives`).
				WillReturnRows(sqlmock.NewRows([]string{"creative_filename", "subject_line", "preheader",
					"from_name", "offer_id", "active"}).
					AddRow("legacy.html", "s", "", "Jamie", "", true))
			mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
				WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
					"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
			expectJourneyOffers(mock, [7]interface{}{offerID, "Dead Offer", "active",
				c.creatives, c.subjects, c.fromNames, ""})
			mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
				WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
			expectJourneyActivity(mock)

			rec := getJourney(t, s, "db", "internal_auto_insurance")
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			var resp laneJourneyResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			t1 := resp.Touches[0]
			// The DATASET's offer wins: the row's legacy.html is NOT consulted.
			if t1.ContentSource != "offer" || t1.OfferSource != "dataset" {
				t.Fatalf("a dataset-bound offer must bypass partner_drip_creatives: %+v", t1)
			}
			if t1.Resolves {
				t.Fatalf("an offer with an empty pool must not read as resolvable: %+v", t1)
			}
			if !strings.Contains(t1.ResolutionLabel, "UNRESOLVABLE") {
				t.Fatalf("label must be alarming: %q", t1.ResolutionLabel)
			}
			if !strings.Contains(t1.ResolutionProblem, c.wantFragment) {
				t.Fatalf("the problem must name WHICH pool is empty (want %q): %q", c.wantFragment, t1.ResolutionProblem)
			}
		})
	}
}

// ── COMPLAINT 1 (operator 2026-08-21): "very challenging to infer if mail is
// being sent" ──
//
// THE COUNTING RULE. A lane mailing only FOLLOW-UPS has a stale/NULL
// per-vertical partner_drip_state.last_wave_at (updateDripState is called only
// by the welcome pass) while it is actively sending. A single merged verdict
// would call that lane STALLED. The two families are graded independently.
func TestLaneJourneySendActivitySplitsWelcomeAndFollowup(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	now := time.Now().UTC()
	staleWelcome := now.Add(-9 * time.Hour)
	freshFollowup := now.Add(-6 * time.Minute)

	expectJourneyDominant(mock, "ds-1", "feed-one", "")
	expectJourneyTouch1(mock, true)
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
	// The rotation pointer is 9h stale — reading it alone says STALLED.
	mock.ExpectQuery(`FROM partner_drip_state`).
		WillReturnRows(sqlmock.NewRows([]string{"last_wave_at", "last_wave_brand", "last_wave_size"}).
			AddRow(staleWelcome, "db", 1))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"is_followup", "last_wave_at",
			"waves_1h", "sent_1h", "waves_today", "sent_today"}).
			AddRow(false, staleWelcome, 0, 0, 4, 12).
			AddRow(true, freshFollowup, 3, 271, 37, 2655))

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	a := resp.SendActivity
	if a.Error != "" {
		t.Fatalf("activity read failed: %q", a.Error)
	}
	if len(a.Families) != 2 || a.Families[0].Family != "welcome" || a.Families[1].Family != "followup" {
		t.Fatalf("welcome and follow-up must be two separate lines, got %+v", a.Families)
	}
	w, f := a.Families[0], a.Families[1]
	if w.Verdict != "stalled" {
		t.Fatalf("a 9h-old welcome wave is STALLED at the 2h threshold: %+v", w)
	}
	if f.Verdict != "flowing" {
		t.Fatalf("a 6m-old FOLLOW-UP wave is FLOWING — the lane is not static: %+v", f)
	}
	if f.Sent1h != 271 || f.SentToday != 2655 || f.Waves1h != 3 {
		t.Fatalf("follow-up counts must come from campaign rows + message_log: %+v", f)
	}
	if w.SentToday != 12 {
		t.Fatalf("welcome counts must stay separate: %+v", w)
	}
	// The rotation pointer is reported, but labeled for what it is — it must
	// never be presented as this brand's follow-up evidence.
	if !a.RotationState.Present || a.RotationState.LastWaveAt == nil {
		t.Fatalf("the partner_drip_state row must be surfaced: %+v", a.RotationState)
	}
	if !strings.Contains(a.RotationState.Note, "welcome pass") {
		t.Fatalf("the rotation note must say only the welcome pass writes it: %q", a.RotationState.Note)
	}
	if !strings.Contains(a.CountingNote, "must not be merged") {
		t.Fatalf("the counting rule must ship in the payload: %q", a.CountingNote)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLaneJourneyActivityFailureIsANamedGap: the activity read is the heaviest
// query on this endpoint. If it fails, the strip reports a NAMED gap and the
// canvas still draws — a failure is never rendered as "0 sent".
func TestLaneJourneyActivityFailureIsANamedGap(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	expectJourneyDominant(mock, "ds-1", "feed-one", "")
	expectJourneyTouch1(mock, true)
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}))
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
	mock.ExpectQuery(`FROM partner_drip_state`).
		WillReturnRows(sqlmock.NewRows([]string{"last_wave_at", "last_wave_brand", "last_wave_size"}))
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnError(fmt.Errorf("statement timeout"))

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("the ladder must still render: got %d", rec.Code)
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SendActivity.Error == "" {
		t.Fatal("a failed activity read must be a NAMED gap, not silent")
	}
	if len(resp.SendActivity.Families) != 0 {
		t.Fatalf("a failed read must not fabricate zero rows: %+v", resp.SendActivity.Families)
	}
}

// TestLaneJourneyActivitySQLShape pins the access path. mailing_campaigns has
// NO index on `name`, so the driver must be
// idx_campaigns_org_status_created (organization_id, status, created_at DESC)
// with the name regex as a filter on the already-narrow slice.
func TestLaneJourneyActivitySQLShape(t *testing.T) {
	for _, frag := range []string{
		"c.organization_id = $1::uuid",
		"c.status IN ('sending','sent','completed','completed_with_errors')",
		"c.created_at >= $2",
		"c.partner_drip_tag IS NOT NULL",
		"mailing_message_log",
	} {
		if !strings.Contains(laneJourneyActivitySQL, frag) {
			t.Fatalf("activity SQL missing %q — it must ride idx_campaigns_org_status_created", frag)
		}
	}
	// Denver windows are param-injected, never tz-cast in the WHERE clause.
	if strings.Contains(laneJourneyActivitySQL, "AT TIME ZONE") {
		t.Fatal("activity SQL must not tz-cast (the documented seq-scan footgun)")
	}
	// sent_count is stamped ahead of the physical send; message_log is the
	// authority the ladder-stamp recovery uses.
	if strings.Contains(laneJourneyActivitySQL, "sent_count") {
		t.Fatal("sent must come from mailing_message_log, never mailing_campaigns.sent_count")
	}
}

// ── SERVING (the shelf's lead object) ───────────────────────────────────────
//
// Each touch carries serving{source, creative_label, subject_mode, subject,
// pool_size} derived from the resolution fields. The offer-center path
// ROTATES subjects evenly across the pool — there is no single 'default' —
// so its subject is a SAMPLE (the pool's first active row) and pool_size
// says how many rotate; the file path serves the row's fixed subject_line.
func TestLaneJourneyServingObject(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	const offerID = "48ddcdf4-5bd0-4cca-a8e8-878a72d6702a"

	expectJourneyDominant(mock, "ds-1", "feed-one", "")
	// Touch 1: FILE path — a configured filename with a fixed subject.
	expectJourneyTouch1(mock, true)
	// Touch 2: OFFER path — empty filename + offer_id. Touch 3+: unconfigured.
	mock.ExpectQuery(`FROM partner_drip_followup_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_number", "vertical", "brand",
			"creative_filename", "subject_line", "preheader", "from_name", "offer_id", "active"}).
			AddRow(2, "internal_auto_insurance", "db", "", "row subject (not what ships)", "", "Jamie", offerID, true))
	expectJourneyOffers(mock, [7]interface{}{offerID, "AutoCoveragePoint - Quote Ready", "active", 2, 5, 1, "Your 3 quotes are waiting"})
	mock.ExpectQuery(`GROUP BY touch_count, isp_family`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "isp_family", "waiting", "due_now", "soonest", "latest"}))
	expectJourneyActivity(mock)

	rec := getJourney(t, s, "db", "internal_auto_insurance")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp laneJourneyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// FILE shape: the row's own subject is what ships — fixed, no pool.
	sv1 := resp.Touches[0].Serving
	if sv1.Source != "file" || sv1.CreativeLabel != "welcome-auto.html" {
		t.Fatalf("file touch serving wrong: %+v", sv1)
	}
	if sv1.SubjectMode != "fixed" || sv1.Subject != "Your auto quote" || sv1.PoolSize != 0 {
		t.Fatalf("file touch must serve the row's FIXED subject: %+v", sv1)
	}

	// OFFER_POOL shape: rotating pool, sample subject + pool size, and the
	// creative label names the offer.
	sv2 := resp.Touches[1].Serving
	if sv2.Source != "offer_pool" || sv2.CreativeLabel != "offer: AutoCoveragePoint - Quote Ready" {
		t.Fatalf("offer touch serving wrong: %+v", sv2)
	}
	if sv2.SubjectMode != "rotating_pool" || sv2.PoolSize != 5 {
		t.Fatalf("offer touch must report the rotating pool: %+v", sv2)
	}
	if sv2.Subject != "Your 3 quotes are waiting" {
		t.Fatalf("offer touch subject must be the pool's first active SAMPLE, got %q", sv2.Subject)
	}
	if sv2.Subject == resp.Touches[1].SubjectLine {
		t.Fatal("the offer path must NOT lead with the creative row's subject_line — the pool ships, the row does not")
	}

	// NONE shape: an unconfigured touch says so plainly.
	sv3 := resp.Touches[2].Serving
	if sv3.Source != "none" || sv3.SubjectMode != "fixed" || sv3.Subject != "" || sv3.PoolSize != 0 {
		t.Fatalf("unconfigured touch serving wrong: %+v", sv3)
	}
	if !strings.Contains(sv3.CreativeLabel, "retires") {
		t.Fatalf("unconfigured serving must carry the retirement label: %q", sv3.CreativeLabel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestLaneJourneyActivityBudgetPinned: the send-activity read runs under its
// OWN short deadline so it can never consume the whole journey request; a
// timeout lands in the fail-soft named-gap path
// (TestLaneJourneyActivityFailureIsANamedGap proves the journey stays 200).
func TestLaneJourneyActivityBudgetPinned(t *testing.T) {
	if laneJourneyActivityBudget != 8*time.Second {
		t.Fatalf("the activity read's own budget is 8s (measured 0.56–4.75s; unbounded under IO starvation), got %v", laneJourneyActivityBudget)
	}
}

// TestLaneJourneyStaleHoursClamp: the FLOWING/STALLED threshold defaults to the
// operator's 2h and only accepts 1..48.
func TestLaneJourneyStaleHoursClamp(t *testing.T) {
	for raw, want := range map[string]int{
		"":     laneJourneyStaleDefaultHours,
		"junk": laneJourneyStaleDefaultHours,
		"0":    laneJourneyStaleDefaultHours,
		"-3":   laneJourneyStaleDefaultHours,
		"49":   laneJourneyStaleDefaultHours,
		"1":    1,
		"6":    6,
		"48":   48,
	} {
		if got := laneJourneyStaleHours(raw); got != want {
			t.Fatalf("stale_hours=%q -> %d, want %d", raw, got, want)
		}
	}
	if laneJourneyStaleDefaultHours != 2 {
		t.Fatalf("the operator's stated default is 2h, got %d", laneJourneyStaleDefaultHours)
	}
}

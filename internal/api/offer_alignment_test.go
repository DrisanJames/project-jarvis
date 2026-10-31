package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ─── classifyAlignmentCell — the single threshold source (design doc B5) ────

func TestClassifyAlignmentCell(t *testing.T) {
	cases := []struct {
		name       string
		in         alignmentCellStats
		wantBadge  string
		wantSample bool
		wantAction string // "" = don't care unless set; matched as substring
	}{
		{
			name:       "under sample floor is LOW_VOLUME",
			in:         alignmentCellStats{Delivered: 400, Hard: 50, Soft: 30, ReputationBlock: 19},
			wantBadge:  "LOW_VOLUME",
			wantSample: false,
		},
		{
			name:       "exactly at sample floor gets a verdict",
			in:         alignmentCellStats{Delivered: 500},
			wantBadge:  "HEALTHY",
			wantSample: true,
		},
		{
			name:      "block rate at 10 percent is BLOCKING",
			in:        alignmentCellStats{Delivered: 900, ReputationBlock: 100},
			wantBadge: "BLOCKING",
		},
		{
			name:      "block rate just under 10 percent is not BLOCKING",
			in:        alignmentCellStats{Delivered: 901, ReputationBlock: 99},
			wantBadge: "HEALTHY",
		},
		{
			name:      "blocking DSN count trips BLOCKING even with low block rate",
			in:        alignmentCellStats{Delivered: 10000, ReputationBlock: 120, BlockingDSN: 100},
			wantBadge: "BLOCKING",
		},
		{
			name:       "HM08 blocking gets the apple-pull action",
			in:         alignmentCellStats{Delivered: 5000, ReputationBlock: 800, BlockingDSN: 700, HM08: 700},
			wantBadge:  "BLOCKING",
			wantAction: "Pull this offer from Apple",
		},
		{
			name: "blocking with real demand gets demand-but-blocked",
			in: alignmentCellStats{Delivered: 5000, ReputationBlock: 800,
				Clickers: 60}, // clicker rate 0.6% ≥ 0.5%
			wantBadge:  "BLOCKING",
			wantAction: "Demand exists but the ISP is blocking",
		},
		{
			name:       "deferred at 20 percent with low block rate is THROTTLED",
			in:         alignmentCellStats{Delivered: 950, ReputationBlock: 50, Deferred: 200},
			wantBadge:  "THROTTLED",
			wantAction: "Capacity throttle",
		},
		{
			name:      "deferred heavy but block rate over the bar stays BLOCKING",
			in:        alignmentCellStats{Delivered: 800, ReputationBlock: 200, Deferred: 500},
			wantBadge: "BLOCKING",
		},
		{
			name: "hard 3 percent dominated by 5.1.1 is LIST_QUALITY",
			in: alignmentCellStats{Delivered: 9700, Hard: 300, // 3.0%
				Hard511: 200}, // 200*2 >= 300 → dominated
			wantBadge:  "LIST_QUALITY",
			wantAction: "Quarantine",
		},
		{
			name: "hard 3 percent NOT dominated by 5.1.1 stays HEALTHY",
			in: alignmentCellStats{Delivered: 9700, Hard: 300,
				Hard511: 100}, // 100*2 < 300
			wantBadge: "HEALTHY",
		},
		{
			name: "healthy with dead clicker rate gets audience-mismatch",
			in: alignmentCellStats{Delivered: 10000,
				Clickers: 10}, // 0.1% < 0.2%
			wantBadge:  "HEALTHY",
			wantAction: "Audience mismatch",
		},
		{
			name: "healthy with strong RPM gets scale",
			in: alignmentCellStats{Delivered: 10000, Revenue: 300, // rpm $30
				Clickers: 100},
			wantBadge:  "HEALTHY",
			wantAction: "Scale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badge, reason, action, sampleOK := classifyAlignmentCell(tc.in)
			if badge != tc.wantBadge {
				t.Fatalf("badge = %q, want %q (reason %q)", badge, tc.wantBadge, reason)
			}
			if badge != "LOW_VOLUME" && !sampleOK {
				t.Fatalf("sample_ok should be true for %s", badge)
			}
			if tc.wantBadge == "LOW_VOLUME" && sampleOK != tc.wantSample {
				t.Fatalf("sample_ok = %v, want %v", sampleOK, tc.wantSample)
			}
			if tc.wantAction != "" && !strings.Contains(action, tc.wantAction) {
				t.Fatalf("action = %q, want it to contain %q", action, tc.wantAction)
			}
			if reason == "" {
				t.Fatalf("badge_reason must always carry the numbers")
			}
		})
	}
}

// ─── dsn decode map ──────────────────────────────────────────────────────────

func TestAlignmentDSNMeaning(t *testing.T) {
	if m := alignmentDSNMeaning("HM08"); !strings.Contains(m, "Apple local-policy") {
		t.Fatalf("HM08 meaning wrong: %q", m)
	}
	if m := alignmentDSNMeaning("4.7.650"); !strings.Contains(m, "velocity throttle") {
		t.Fatalf("4.7.650 meaning wrong: %q", m)
	}
	if m := alignmentDSNMeaning("totally-unknown"); !strings.Contains(m, "unclassified") {
		t.Fatalf("default meaning wrong: %q", m)
	}
}

// ─── offer campaign-set resolver: SQL shape + union/dedupe semantics ─────────

func TestResolveOfferCampaignSet(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	org := "00000000-0000-0000-0000-000000000001"
	idStamped := "aaaaaaaa-0000-0000-0000-000000000001"
	idName := "bbbbbbbb-0000-0000-0000-000000000002"
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 7, 23, 59, 59, 0, time.UTC)

	// (0) stamped — keyed on lower(offer_key).
	mock.ExpectQuery(`lower\(COALESCE\(offer_key,''\)\) = lower\(\$2\)`).
		WithArgs(org, "fidelity", from, to, "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(idStamped))
	// (1) name-suffix inferred — only campaigns WITHOUT offer_key; the
	// " - <offer>" suffix ILIKE against both the key and the slug-map name.
	mock.ExpectQuery(`COALESCE\(offer_key,''\) = ''[\s\S]*name ILIKE \$4`).
		WithArgs(org, from, to, "% - fidelity", "% - Fidelity Term Life", "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(idName).AddRow(idStamped)) // idStamped dupes → deduped
	// NO click query: stamped campaigns already define the offer, and that
	// scan times out on prod.

	set, err := resolveOfferCampaignSet(context.Background(), db, org,
		"fidelity", "Fidelity Term Life", []string{"%/FIDELITY/%"}, from, to, "")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet: %v", err)
	}
	if set.Stamped != 1 || set.Inferred != 1 {
		t.Fatalf("stamped=%d inferred=%d, want 1/1", set.Stamped, set.Inferred)
	}
	if len(set.IDs) != 2 {
		t.Fatalf("ids = %v, want 2 deduped", set.IDs)
	}
	if set.IDs[0] != idStamped {
		t.Fatalf("stamped ids must sort first, got %v", set.IDs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestResolveOfferCampaignSetSkipsClickQueryWithoutPatterns(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	org := "00000000-0000-0000-0000-000000000001"
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`lower\(COALESCE\(offer_key,''\)\)`).
		WithArgs(org, "mystery", from, to, "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// offerName '' ⇒ the name pattern arg is '' and the predicate skips it.
	mock.ExpectQuery(`COALESCE\(offer_key,''\) = ''`).
		WithArgs(org, from, to, "% - mystery", "", "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// NO third query: patterns empty ⇒ click-inferred pass is skipped.

	set, err := resolveOfferCampaignSet(context.Background(), db, org, "mystery", "", nil, from, to, "")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet: %v", err)
	}
	if len(set.IDs) != 0 || set.Stamped != 0 || set.Inferred != 0 {
		t.Fatalf("expected empty set, got %+v", set)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ─── ISP CASE mirrors the canonical buckets ──────────────────────────────────

func TestAlignmentISPCaseBuckets(t *testing.T) {
	sql := alignmentISPCase("d")
	for _, frag := range []string{
		"'microsoft'", "'gmail'", "'yahoo'", "'apple'", "'comcast'",
		"'aol'", "'att'", "'sbcglobal'", "'cox'", "'charter'", "'verizon'", "'other'",
	} {
		if !strings.Contains(sql, frag) {
			t.Fatalf("ISP case missing bucket %s", frag)
		}
	}
	// Spot-check the canonical domain lists (mirrors _db.py ISP_CASE_PG).
	for _, frag := range []string{"'hotmail.com'", "'googlemail.com'", "'icloud.com'", "'spectrum.net'"} {
		if !strings.Contains(sql, frag) {
			t.Fatalf("ISP case missing domain %s", frag)
		}
	}
}

// ─── handler smoke tests ─────────────────────────────────────────────────────

func TestHandleOfferAlignmentMatrixValidation(t *testing.T) {
	s := &Server{}
	// Bad window → 400 with the {"error": ...} shape.
	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/matrix?window=30", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentMatrix(rec, req)
	if rec.Code != 400 {
		t.Fatalf("window=30 status = %d, want 400", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == nil {
		t.Fatalf("expected {\"error\":...}, got %s", rec.Body.String())
	}
}

func TestHandleOfferAlignmentMatrixDisabledWhenLakeDark(t *testing.T) {
	// The analytics reader is never initialised in tests, so ReaderEnabled()
	// is false — the matrix must serve the designed {"disabled":true} shape
	// on 200, tolerating the trailing-'d' window form.
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/matrix?window=7d", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentMatrix(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["disabled"] != true {
		t.Fatalf("expected disabled=true, got %s", rec.Body.String())
	}
}

func TestHandleOfferAlignmentOfferRequiresOffer(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/offer", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentOffer(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "offer is required") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleOfferAlignmentOfferRejectsBadRange(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/offer?offer=fidelity&from=07/01/2026&to=2026-07-07", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentOffer(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "YYYY-MM-DD") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleOfferAlignmentEvidenceValidation(t *testing.T) {
	s := &Server{}
	// Missing isp → 400.
	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/evidence?offer=fidelity", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentEvidence(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "isp is required") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Missing offer → 400.
	req = httptest.NewRequest("GET", "/api/mailing/offer-alignment/evidence?isp=apple", nil)
	rec = httptest.NewRecorder()
	s.HandleOfferAlignmentEvidence(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "offer is required") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Injection-shaped isp → 400 before any query.
	req = httptest.NewRequest("GET", "/api/mailing/offer-alignment/evidence?offer=fidelity&isp=gmail%27+OR+1%3D1", nil)
	rec = httptest.NewRecorder()
	s.HandleOfferAlignmentEvidence(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad isp status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Lake dark → designed {"disabled":true} (params valid).
	req = httptest.NewRequest("GET", "/api/mailing/offer-alignment/evidence?offer=fidelity&isp=apple&from=2026-07-01&to=2026-07-07", nil)
	rec = httptest.NewRecorder()
	s.HandleOfferAlignmentEvidence(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleOfferAlignmentRefreshAccepted(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &Server{mailingDB: db}
	req := httptest.NewRequest("POST", "/api/mailing/offer-alignment/refresh", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentRefresh(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"accepted"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestHandleOfferAlignmentOfferEmptySet drives the full handler path with the
// lake dark and an empty campaign set: slug resolution + the two resolver
// queries run, then the response is the well-formed empty profile.
func TestHandleOfferAlignmentOfferEmptySet(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := &Server{mailingDB: db}

	// resolveOfferSlugsForKey: slug map miss → bare-slug self-resolution.
	mock.ExpectQuery(`FROM mailing_offer_slug_map`).
		WithArgs("fidelity").
		WillReturnRows(sqlmock.NewRows([]string{"cratoolpro_slug", "offer_name", "everflow_offer_id"}))
	// Campaign set: stamped, name-inferred, click-inferred (patterns exist
	// from the bare-slug fallback) — all empty.
	mock.ExpectQuery(`lower\(COALESCE\(offer_key,''\)\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`COALESCE\(offer_key,''\) = ''`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`ILIKE ANY`).
		WillReturnRows(sqlmock.NewRows([]string{"campaign_id"}))

	req := httptest.NewRequest("GET", "/api/mailing/offer-alignment/offer?offer=Fidelity&from=2026-07-01&to=2026-07-07", nil)
	rec := httptest.NewRecorder()
	s.HandleOfferAlignmentOffer(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Header struct {
			StatusLine  string `json:"status_line"`
			Attribution struct {
				Stamped  int `json:"stamped_campaigns"`
				Inferred int `json:"inferred_campaigns"`
			} `json:"attribution"`
		} `json:"header"`
		ISPRows     []any `json:"isp_rows"`
		Creatives   []any `json:"creatives"`
		DataSources []any `json:"data_sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(body.Header.StatusLine, "no campaigns found") {
		t.Fatalf("status_line = %q", body.Header.StatusLine)
	}
	if body.ISPRows == nil || body.Creatives == nil || body.DataSources == nil {
		t.Fatalf("arrays must be [] not null: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestResolveOfferCampaignSetByOfferID pins the identity most campaigns carry:
// an offer-id key selects on offer_id alone, with no inference and no
// click scan. Stamped-key matching missed 18,677 of 24,600 campaigns.
func TestResolveOfferCampaignSetByOfferID(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	org := "00000000-0000-0000-0000-000000000001"
	key := "b0559641-3dc5-4623-9fe5-a1f199dacd9b"
	from := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`offer_id = \$2::uuid`).
		WithArgs(org, key, from, to, "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("aaaaaaaa-0000-0000-0000-000000000001"))

	set, err := resolveOfferCampaignSet(context.Background(), db, org, key, "eHealth CPC - Switchers",
		[]string{"%/X/%"}, from, to, "")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet: %v", err)
	}
	if set.Stamped != 1 || set.Inferred != 0 || len(set.IDs) != 1 {
		t.Fatalf("set = %+v, want 1 exact campaign", set)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAlignmentConversionsFromLedger is the regression guard for $0.00 RPM:
// the ledger's posted dollars reach the cell, a $0 post prices at the on-file
// rate, and a window slices without a second read.
func TestAlignmentConversionsFromLedger(t *testing.T) {
	rows := []alignmentConvRow{
		{LocalDt: "2026-10-30", ISP: "microsoft", EverflowID: "379", Conversions: 10, Payout: 25},
		{LocalDt: "2026-10-20", ISP: "microsoft", EverflowID: "379", Conversions: 4, Payout: 10},
		{LocalDt: "2026-10-30", ISP: "apple", EverflowID: "338", Conversions: 3, Unpriced: 3},
	}
	wide := aggregateAlignmentConversions(rows, "2026-10-18", "2026-10-31")
	if c := wide["microsoft"]; c.Conversions != 14 || c.Revenue != 35 {
		t.Fatalf("14d microsoft = %+v, want 14 conv $35", c)
	}
	if c := wide["apple"]; c.Conversions != 3 || c.Revenue != 198 {
		t.Fatalf("Liberty posts $0 and must price at $66 on file: %+v", c)
	}
	narrow := aggregateAlignmentConversions(rows, "2026-10-29", "2026-10-31")
	if c := narrow["microsoft"]; c.Conversions != 10 || c.Revenue != 25 {
		t.Fatalf("3d microsoft = %+v, want 10 conv $25", c)
	}
	if !alignmentOfferPriced(rows) {
		t.Fatalf("offer with posted payout must read priced")
	}
}

// TestAlignmentRevenueUnknownIsNotZero pins the rule that an unknown payout
// shows — and never $0.00, and that no deal goal is ever used as revenue.
func TestAlignmentRevenueUnknownIsNotZero(t *testing.T) {
	empire := []alignmentConvRow{{LocalDt: "2026-10-30", ISP: "yahoo", EverflowID: "cake:48865", Conversions: 14, Unpriced: 14}}
	if alignmentOfferPriced(empire) {
		t.Fatalf("$0 post with no rate on file must read unpriced")
	}
	cell := aggregateAlignmentConversions(empire, "", "")["yahoo"]
	if cell.Revenue != 0 {
		t.Fatalf("no price must not invent revenue: %+v", cell)
	}
	if alignmentRevenueKnown(false, cell) {
		t.Fatalf("unpriced offer must be unknown")
	}
	if alignmentRevenueKnown(false, &alignmentConversions{}) {
		t.Fatalf("offer with no price and no conversions has unknown revenue (CPM)")
	}
	if !alignmentRevenueKnown(true, &alignmentConversions{}) {
		t.Fatalf("priced offer with no conversions in the window is a true $0")
	}
	if alignmentRevenueKnown(true, cell) {
		t.Fatalf("conversions with no dollars must be unknown even on a priced offer")
	}
}

// TestFetchAlignmentConversionRowsReadsLedger pins the source: the live
// ledger by campaign_id, the excluded allocation row left out.
func TestFetchAlignmentConversionRowsReadsLedger(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_everflow_conversions ec[\s\S]*ec\.campaign_id = ANY\(\$2::uuid\[\]\)[\s\S]*<> ALL\(\$5\)`).
		WillReturnRows(sqlmock.NewRows([]string{"c", "d", "i", "e", "s", "n", "p", "u"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "2026-10-30", "microsoft", "379", "src", 2, 5.0, 0))
	rows, err := fetchAlignmentConversionRows(context.Background(), db, "org",
		[]string{"aaaaaaaa-0000-0000-0000-000000000001"}, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("fetchAlignmentConversionRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Conversions != 2 || rows[0].revenue() != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAlignmentEngagementReadsRollup pins the fix for 0% click rates: the live
// path must not touch mailing_tracking_events, whose per-offer scan never
// finishes. Empty rollup rows are a value, not an error.
func TestAlignmentEngagementReadsRollup(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_offer_alignment_clicks`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "c", "n"}).AddRow("microsoft", 192, 59))
	eng, err := fetchAlignmentEngagement(context.Background(), db, "org",
		[]string{"aaaaaaaa-0000-0000-0000-000000000001"}, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("fetchAlignmentEngagement: %v", err)
	}
	if e := eng["microsoft"]; e == nil || e.HumanClicks != 192 || e.Clickers != 59 {
		t.Fatalf("engagement = %+v", eng["microsoft"])
	}
}

func TestAlignmentMoneyClickPredicateCoversGatewayLinks(t *testing.T) {
	if !strings.Contains(alignmentMoneyClickPredicate, "/o/") {
		t.Fatalf("predicate must match /o/ gateway links: %s", alignmentMoneyClickPredicate)
	}
}

func TestOfferAlignmentWindows(t *testing.T) {
	for _, d := range []int{3, 7, 14} {
		if !isOfferAlignmentWindow(d) {
			t.Fatalf("window %d must be valid", d)
		}
	}
	for _, d := range []int{0, 1, 30} {
		if isOfferAlignmentWindow(d) {
			t.Fatalf("window %d must be invalid", d)
		}
	}
	old := time.Now().AddDate(0, 0, -60)
	if got := alignmentScopeFrom(old); !got.Equal(old) {
		t.Fatalf("a range older than 14d keeps its start: %v", got)
	}
	if got := alignmentScopeFrom(time.Now()); time.Since(got) < 12*24*time.Hour {
		t.Fatalf("a short range widens to the 14d campaign set: %v", got)
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// Every offer name, list name and count below was read out of the prod
// mailing_offers / mailing_suppression_lists / mailing_offer_suppressions
// catalogs on 2026-08-18. These are not invented fixtures — the matcher has to
// hold against the real catalog, including its near-duplicate rows.

func TestSharesOfferPrefix_RealCatalogSiblings(t *testing.T) {
	// MUST match: the duplicate-offer trap. The left row is the one the
	// operator picks; the right row is the one actually holding suppressions.
	shouldMatch := [][2]string{
		{"Sam's Club Membership", "Sam's Club Membership - Partner Drip (4989)"},
		{"Get Metal Roofing", "Get Metal Roofing (EF 9539)"},
		{"CarShield Auto Warranty", "CarShield Auto Warranty - IceT2000 (545801)"},
		{"Fidelity Life", "Fidelity Life Insurance"},
		{"3 Day Blinds - v2", "3 Day Blinds"},
		{"West Capital HELOC", "West Capital HELOC (iwchelocv1)"},
	}
	for _, c := range shouldMatch {
		if !sharesOfferPrefix(normalizeOfferWords(c[0]), normalizeOfferWords(c[1])) {
			t.Errorf("expected sibling match: %q <-> %q", c[0], c[1])
		}
	}

	// MUST NOT match: distinct advertisers that share a leading token, and the
	// one-token case the >=2 floor exists to reject.
	shouldNotMatch := [][2]string{
		{"West Capital HELOC", "West Shore Home Bath Remodel"},
		{"West Capital HELOC", "West Shore Windows"},
		{"Quicken Loans HELOC", "AmeriSave HELOC"},
		{"Optima Tax Relief", "National Debt Relief"},
		{"Babbel", "Budget Blinds"},
		// Single leading token in common is not enough.
		{"Auto Coverage Map - Quote Ready", "AutoCoveragePoint - Quote Ready"},
	}
	for _, c := range shouldNotMatch {
		if sharesOfferPrefix(normalizeOfferWords(c[0]), normalizeOfferWords(c[1])) {
			t.Errorf("unexpected sibling match: %q <-> %q", c[0], c[1])
		}
	}
}

func TestNormalizeSuppressionListWords_StripsListNoise(t *testing.T) {
	cases := []struct {
		list string
		want string
	}{
		{"SBLI Quick Quote Offer Suppression", "sbli quick quote"},
		{"AmeriSave HELOC — 2026-05-04", "amerisave heloc"},
		{"Quicken Loans — 2026-05-04", "quicken loans"},
		{"Jacuzzi Bath Remodel (Zip-Targeted CPL) — 2026-05-04", "jacuzzi bath remodel zip targeted"},
		{"Fidelity Life Insurance - suppression", "fidelity life insurance"},
		{"BetterHelp Email CPA Offer Suppression", "betterhelp"},
	}
	for _, c := range cases {
		got := strings.Join(normalizeSuppressionListWords(c.list), " ")
		if got != c.want {
			t.Errorf("normalizeSuppressionListWords(%q) = %q, want %q", c.list, got, c.want)
		}
	}
}

func TestSharesOfferPrefix_RealCatalogLists(t *testing.T) {
	// Offer name -> the curated advertiser list that belongs to it.
	pairs := [][2]string{
		{"AmeriSave HELOC", "AmeriSave HELOC — 2026-05-04"},
		{"Quicken Loans HELOC", "Quicken Loans — 2026-05-04"},
		{"SBLI Quick Quote", "SBLI Quick Quote Offer Suppression"},
		{"Jacuzzi Bath Remodel", "Jacuzzi Bath Remodel (Zip-Targeted CPL) — 2026-05-04"},
	}
	for _, p := range pairs {
		if !sharesOfferPrefix(normalizeOfferWords(p[0]), normalizeSuppressionListWords(p[1])) {
			t.Errorf("expected list match: offer %q <-> list %q", p[0], p[1])
		}
	}
	// Globe Life has no list in prod — nothing must match it.
	globe := normalizeOfferWords("Globe Life - New Form")
	for _, l := range []string{
		"BetterHelp Email CPA Offer Suppression",
		"Home Services Shared Suppression (Bath & Shower)",
		"TruGreen — Sensitive IO Req (Mon–Sat) — 2026-05-04",
		"Fidelity Life Insurance - suppression",
	} {
		if sharesOfferPrefix(globe, normalizeSuppressionListWords(l)) {
			t.Errorf("Globe Life must not match list %q", l)
		}
	}
}

func TestOfferSuppressionWarning(t *testing.T) {
	// The Sam's Club shape: zero on the picked row, 949,785 on the sibling.
	w := offerSuppressionWarning(offerSuppressionSummary{
		SuppressionCount: 0,
		Siblings: []offerSuppressionSibling{
			{Name: "Sam's Club Membership - Partner Drip (4989)", Status: "sunset", SuppressionCount: 949785},
		},
	})
	if !strings.Contains(w, "949,785") || !strings.Contains(w, "NO offer-level suppression") {
		t.Errorf("sibling warning missing count or headline: %q", w)
	}

	// The Globe Life shape: nothing anywhere.
	w = offerSuppressionWarning(offerSuppressionSummary{SuppressionCount: 0})
	if !strings.Contains(w, "NO suppression of any kind") {
		t.Errorf("bare-zero warning wrong: %q", w)
	}

	// Healthy offer with no list: no warning at all.
	if w = offerSuppressionWarning(offerSuppressionSummary{SuppressionCount: 44835}); w != "" {
		t.Errorf("healthy offer should carry no warning, got %q", w)
	}

	// Healthy offer WITH an unapplied advertiser list: still worth saying.
	w = offerSuppressionWarning(offerSuppressionSummary{
		SuppressionCount: 3108,
		SuggestedLists:   []offerSuppressionListRef{{Name: "SBLI Quick Quote Offer Suppression"}},
	})
	if !strings.Contains(w, "NOT applied automatically") {
		t.Errorf("unapplied-list warning wrong: %q", w)
	}
}

func TestFormatThousands(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 135: "135", 3108: "3,108", 949785: "949,785", 2994793: "2,994,793"}
	for in, want := range cases {
		if got := formatThousands(in); got != want {
			t.Errorf("formatThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// End-to-end through the registered chi route: the handler must report the
// sibling that holds the suppressions, which is the whole point of the surface.
func TestHandleOfferSuppression_SurfacesSiblingHoldingTheLedger(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const picked = "5d99a5f7-1742-43c0-9c0e-11bca561dd7a"  // Sam's Club Membership (active)
	const sibling = "cc108c5b-14ba-56c8-ad03-64d97a440f14" // …- Partner Drip (4989) (sunset)

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers WHERE id = $1`)).
		WithArgs(picked, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).
			AddRow("Sam's Club Membership", "active"))

	// Zero rows: the picked offer's ledger is empty.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_suppressions`)).
		WithArgs(picked).
		WillReturnRows(sqlmock.NewRows([]string{"reason", "n"}))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_suppression_lists`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "entry_count"}).
			AddRow("offer-betterhelp-20260714", "BetterHelp Email CPA Offer Suppression", 19512))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs(sqlmock.AnyArg(), picked).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}).
			AddRow(sibling, "Sam's Club Membership - Partner Drip (4989)", "sunset").
			AddRow("11111111-1111-1111-1111-111111111111", "Warby Parker", "active"))

	// Only the name-matched candidate is counted — Warby Parker must not be.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM mailing_offer_suppressions WHERE offer_id = $1`)).
		WithArgs(sibling).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(949785))

	svc := &PMTACampaignService{db: db}
	r := chi.NewRouter()
	r.Get("/offer-suppression", svc.HandleOfferSuppression)

	req := httptest.NewRequest(http.MethodGet, "/offer-suppression?offer_id="+picked, nil)
	req.Header.Set("X-Organization-ID", "00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got offerSuppressionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.SuppressionCount != 0 {
		t.Errorf("suppression_count = %d, want 0", got.SuppressionCount)
	}
	if len(got.Siblings) != 1 || got.Siblings[0].SuppressionCount != 949785 {
		t.Fatalf("siblings = %+v, want the 949,785-row Partner Drip row", got.Siblings)
	}
	if len(got.SuggestedLists) != 0 {
		t.Errorf("BetterHelp must not match Sam's Club, got %+v", got.SuggestedLists)
	}
	if !strings.Contains(got.Warning, "949,785") {
		t.Errorf("warning must name the sibling's count, got %q", got.Warning)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The negative path: an offer with a healthy ledger and no sibling reports no
// warning, so the panel does not cry wolf on every offer.
func TestHandleOfferSuppression_HealthyOfferHasNoWarning(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	const id = "44444444-4444-4444-4444-444444444444"
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers WHERE id = $1`)).
		WithArgs(id, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status"}).AddRow("Liberty Mutual Insurance", "active"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_suppressions`)).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"reason", "n"}).
			AddRow("converted", 763190).AddRow("manual", 100))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_suppression_lists`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "entry_count"}))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs(sqlmock.AnyArg(), id).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}))

	svc := &PMTACampaignService{db: db}
	r := chi.NewRouter()
	r.Get("/offer-suppression", svc.HandleOfferSuppression)
	req := httptest.NewRequest(http.MethodGet, "/offer-suppression?offer_id="+id, nil)
	req.Header.Set("X-Organization-ID", "00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var got offerSuppressionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.SuppressionCount != 763290 {
		t.Errorf("suppression_count = %d, want 763290 (sum of reason buckets)", got.SuppressionCount)
	}
	if got.Warning != "" {
		t.Errorf("healthy offer must carry no warning, got %q", got.Warning)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// offer_id is mandatory — without it the handler must not scan the catalog.
func TestHandleOfferSuppression_RequiresOfferID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := &PMTACampaignService{db: db}
	r := chi.NewRouter()
	r.Get("/offer-suppression", svc.HandleOfferSuppression)
	req := httptest.NewRequest(http.MethodGet, "/offer-suppression", nil)
	req.Header.Set("X-Organization-ID", "00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

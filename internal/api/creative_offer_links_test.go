package api

// Tests for the Creative Studio offer-link mapping report
// (creative_offer_links.go). sqlmock + httptest, matching the pattern in
// creative_proof_gateway_test.go (shares gwTestOrgID). The load-bearing
// assertion is that "mapped" is decided by moneylink.Normalize, so a rendered
// href with concrete params resolves to a stored template's hash.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

func newOfferLinksHandler(t *testing.T) (*ProofSendHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &ProofSendHandler{
		db:         db,
		orgID:      gwTestOrgID,
		smartLinks: NewSmartLinkService(db),
	}
	return h, mock
}

func offerLinksGET(t *testing.T, h *ProofSendHandler, creativeID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/creatives/"+creativeID+"/offer-links", nil)
	req.Header.Set("X-Organization-ID", gwTestOrgID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", creativeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.HandleCreativeOfferLinks(rec, req)
	return rec
}

func expectOfferLinksCreativeLoad(mock sqlmock.Sqlmock, creativeID, html string) {
	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_creatives`).
		WithArgs(creativeID, gwTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow(html))
}

var offerLinkSmartLinkCols = []string{"hash", "offer_url_template", "slug", "risk_profile"}

func expectSmartLinkLoad(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT hash, offer_url_template, slug, COALESCE\(risk_profile,'low'\)`).
		WillReturnRows(rows)
}

type offerLinksResp struct {
	OfferLinks []offerLinkEntry `json:"offer_links"`
}

func decodeOfferLinks(t *testing.T, rec *httptest.ResponseRecorder) offerLinksResp {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out offerLinksResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v body=%s", err, rec.Body.String())
	}
	return out
}

// A mapped cratoolpro link (rendered with concrete params) + an unmapped
// eos57ytf link → both returned; the cratoolpro one carries the seeded hash and
// mapped=true; the eos one is mapped=false with a suggested slug.
func TestOfferLinks_MappedAndUnmapped(t *testing.T) {
	h, mock := newOfferLinksHandler(t)

	html := `<html><body>` +
		`<a href="https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}">Shop</a>` +
		`<a href="https://www.eos57ytf.com/XY/DEAL/?source_id=email">Other</a>` +
		`</body></html>`
	expectOfferLinksCreativeLoad(mock, "creative-1", html)

	// The stored template normalizes to the SAME key as the rendered cratoolpro
	// href above, proving moneylink.Normalize drives the match.
	smRows := sqlmock.NewRows(offerLinkSmartLinkCols).AddRow(
		"hsh1234567",
		"https://www.cratoolpro.com/BJB4Q5BF/ABC123/?sub1={{subscriber.id}}&sub2={{brand.domain}}",
		"auto-refi", "low",
	)
	expectSmartLinkLoad(mock, smRows)

	resp := decodeOfferLinks(t, offerLinksGET(t, h, "creative-1"))
	if len(resp.OfferLinks) != 2 {
		t.Fatalf("expected 2 offer links, got %d: %+v", len(resp.OfferLinks), resp.OfferLinks)
	}

	byHost := map[string]offerLinkEntry{}
	for _, e := range resp.OfferLinks {
		byHost[e.Host] = e
	}

	cra, ok := byHost["cratoolpro.com"]
	if !ok {
		t.Fatalf("cratoolpro entry missing: %+v", resp.OfferLinks)
	}
	if !cra.Mapped {
		t.Errorf("cratoolpro link must be mapped: %+v", cra)
	}
	if cra.Hash != "hsh1234567" {
		t.Errorf("cratoolpro hash = %q, want hsh1234567", cra.Hash)
	}
	if cra.Slug != "auto-refi" {
		t.Errorf("cratoolpro slug = %q, want auto-refi", cra.Slug)
	}
	if cra.RiskProfile != "low" {
		t.Errorf("cratoolpro risk_profile = %q, want low", cra.RiskProfile)
	}
	if cra.Normalized != "https://www.cratoolpro.com/BJB4Q5BF/ABC123" {
		t.Errorf("cratoolpro normalized = %q", cra.Normalized)
	}
	if cra.SuggestedSlug != "" {
		t.Errorf("mapped link must not carry a suggested_slug: %q", cra.SuggestedSlug)
	}

	eos, ok := byHost["eos57ytf.com"]
	if !ok {
		t.Fatalf("eos57ytf entry missing: %+v", resp.OfferLinks)
	}
	if eos.Mapped {
		t.Errorf("eos57ytf link must be unmapped: %+v", eos)
	}
	if eos.Hash != "" {
		t.Errorf("unmapped link must not carry a hash: %q", eos.Hash)
	}
	if eos.SuggestedSlug == "" {
		t.Errorf("unmapped link must carry a suggested_slug: %+v", eos)
	}
	if !slugPattern.MatchString(eos.SuggestedSlug) {
		t.Errorf("suggested_slug %q is not slugPattern-valid", eos.SuggestedSlug)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A creative with no money links → empty offer_links (rendered as [] not null).
func TestOfferLinks_NoMoneyLinks(t *testing.T) {
	h, mock := newOfferLinksHandler(t)

	html := `<html><body><a href="https://www.discountblog.com/reviews/auto">Read</a></body></html>`
	expectOfferLinksCreativeLoad(mock, "creative-2", html)
	// The smart-link load still runs (handler always queries); zero rows.
	expectSmartLinkLoad(mock, sqlmock.NewRows(offerLinkSmartLinkCols))

	rec := offerLinksGET(t, h, "creative-2")
	resp := decodeOfferLinks(t, rec)
	if len(resp.OfferLinks) != 0 {
		t.Fatalf("expected 0 offer links, got %d: %+v", len(resp.OfferLinks), resp.OfferLinks)
	}
	// The JSON must be an empty array, never null (non-nil slice contract).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if string(raw["offer_links"]) != "[]" {
		t.Errorf("offer_links must be [] not %s", string(raw["offer_links"]))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Two hrefs with the SAME destination (different query params) dedup to one
// entry, keyed by the normalized destination.
func TestOfferLinks_DedupIdenticalDestinations(t *testing.T) {
	h, mock := newOfferLinksHandler(t)

	html := `<html><body>` +
		`<a href="https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=1">A</a>` +
		`<a href="https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=2">B</a>` +
		`</body></html>`
	expectOfferLinksCreativeLoad(mock, "creative-3", html)
	expectSmartLinkLoad(mock, sqlmock.NewRows(offerLinkSmartLinkCols))

	resp := decodeOfferLinks(t, offerLinksGET(t, h, "creative-3"))
	if len(resp.OfferLinks) != 1 {
		t.Fatalf("expected 1 deduped offer link, got %d: %+v", len(resp.OfferLinks), resp.OfferLinks)
	}
	if resp.OfferLinks[0].Normalized != "https://www.cratoolpro.com/BJB4Q5BF/ABC123" {
		t.Errorf("normalized = %q", resp.OfferLinks[0].Normalized)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The /integration/ postback path is NOT reported as an offer link.
func TestOfferLinks_IntegrationExcluded(t *testing.T) {
	h, mock := newOfferLinksHandler(t)

	html := `<html><body>` +
		`<a href="https://www.cratoolpro.com/integration/postback?x=1">pixel</a>` +
		`<a href="https://www.cratoolpro.com/REAL/OFFER/?source_id=email">Shop</a>` +
		`</body></html>`
	expectOfferLinksCreativeLoad(mock, "creative-4", html)
	expectSmartLinkLoad(mock, sqlmock.NewRows(offerLinkSmartLinkCols))

	resp := decodeOfferLinks(t, offerLinksGET(t, h, "creative-4"))
	if len(resp.OfferLinks) != 1 {
		t.Fatalf("expected 1 offer link (integration excluded), got %d: %+v", len(resp.OfferLinks), resp.OfferLinks)
	}
	if resp.OfferLinks[0].Normalized != "https://www.cratoolpro.com/REAL/OFFER" {
		t.Errorf("expected the REAL offer, got %q", resp.OfferLinks[0].Normalized)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Missing creative → 404.
func TestOfferLinks_CreativeNotFound(t *testing.T) {
	h, mock := newOfferLinksHandler(t)
	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_creatives`).
		WithArgs("nope", gwTestOrgID).
		WillReturnError(sql.ErrNoRows)

	rec := offerLinksGET(t, h, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

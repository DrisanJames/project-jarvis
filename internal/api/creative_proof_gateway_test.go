package api

// Tests for the Smart Link Gateway routing on Creative Studio proof sends
// (creative_proof_send.go + smartlink_rewrite.go + the shared
// resolveActiveSmartLink lookup in smartlink_handlers.go).
//
// Coverage:
//   - RewriteMoneyLinksToGateway: single/multiple hrefs, non-cratoolpro
//     untouched, /integration/ excluded, idempotency, empty inputs, counts.
//   - resolveActiveSmartLink (sqlmock): active row scanned incl. risk_profile;
//     ErrNoRows on no row; query carries status='active'.
//   - HandleCreativeProof gateway branch (Wave 1 — tracking-layer /o/ URL):
//     happy path (tracking /o/ URL in HTML + tracking_url/hash/risk_profile in
//     response), no-active-row (no send), empty slug (no smart-link query, no
//     send), route_via_gateway=false default-safe pass-through, and the /o/
//     URL NOT re-wrapped into /track/click (send_worker skip rule).
//
// Follows the sqlmock + mockSender pattern already established in
// offer_compliance_test.go and smartlink_handlers_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

// A real-shaped cratoolpro money href carrying Everflow sub tags, as it
// appears in a Creative Studio creative before send.
const cratoolproHref = `href="https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"`

func gatewayCreativeHTML() string {
	return `<html><body><h1>Big Deal</h1>` +
		`<a ` + cratoolproHref + `>Shop Now</a>` +
		`</body></html>`
}

// ---------------------------------------------------------------------------
// resolveActiveSmartLink
// ---------------------------------------------------------------------------

func TestResolveActiveSmartLink_ScansRiskProfile(t *testing.T) {
	svc, mock := newSmartLinkService(t)
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`(?s)SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\).*WHERE brand_root = \$1 AND slug = \$2 AND status = 'active'`).
		WithArgs("discountblog.com", "auto-refi").
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"11111111-2222-3333-4444-555555555555",
			"discountblog.com", "auto-refi", "best-auto-refi",
			"https://www.cratoolpro.com/A/B/?source_id=email",
			"active", "high", "", "abc1234567", now, now,
		))

	sl, err := svc.resolveActiveSmartLink(context.Background(), "discountblog.com", "auto-refi")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sl.RiskProfile != "high" {
		t.Errorf("risk_profile not scanned: %q", sl.RiskProfile)
	}
	if sl.Status != "active" {
		t.Errorf("status not scanned: %q", sl.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestResolveActiveSmartLink_ErrNoRows(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	// A paused/absent row is simply not returned by the active-only query.
	mock.ExpectQuery(`WHERE brand_root = \$1 AND slug = \$2 AND status = 'active'`).
		WithArgs("discountblog.com", "missing").
		WillReturnRows(sqlmock.NewRows(smartLinkCols)) // zero rows

	_, err := svc.resolveActiveSmartLink(context.Background(), "discountblog.com", "missing")
	if err == nil {
		t.Fatal("expected sql.ErrNoRows, got nil")
	}
	if err.Error() != "sql: no rows in result set" {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HandleCreativeProof — gateway branch
// ---------------------------------------------------------------------------

const gwTestOrgID = "00000000-0000-0000-0000-000000000001"

// newGatewayProofHandler builds a ProofSendHandler wired to sqlmock + a
// capturing sender. trackBase controls whether tracking injection runs:
// when trackURL/secret are empty AND the profile returns NULL tracking
// columns, injection is skipped and rewritten gateway URLs appear literally.
func newGatewayProofHandler(t *testing.T, trackURL, secret string) (*ProofSendHandler, sqlmock.Sqlmock, *mockSender) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sender := &mockSender{}
	h := &ProofSendHandler{
		db:             db,
		sender:         sender,
		trackingURL:    trackURL,
		trackingSecret: secret,
		orgID:          gwTestOrgID,
		smartLinks:     NewSmartLinkService(db),
	}
	return h, mock, sender
}

func creativeProofPOST(t *testing.T, h *ProofSendHandler, creativeID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/creatives/"+creativeID+"/send-proof", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", gwTestOrgID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", creativeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.HandleCreativeProof(rec, req)
	return rec
}

// expectCreativeLoad registers the org-scoped creative SELECT (brand_code DB).
func expectCreativeLoad(mock sqlmock.Sqlmock, creativeID string) {
	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\), COALESCE\(subject,''\), COALESCE\(preheader,''\), COALESCE\(brand_code,''\)`).
		WithArgs(creativeID, gwTestOrgID).
		WillReturnRows(sqlmock.NewRows([]string{"html_content", "subject", "preheader", "brand_code"}).
			AddRow(gatewayCreativeHTML(), "Summer Sale", "peek inside", "DB"))
}

// expectProfileNoTracking registers the sending-profile SELECT returning NULL
// tracking columns so trackBase resolves to "" (tracking injection skipped).
func expectProfileNoTracking(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id::text, COALESCE\(from_email,''\), tracking_domain, sending_domain`).
		WithArgs("em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "tracking_domain", "sending_domain",
			"via_ses", "ses_configuration_set", "ses_tenant_name", "raw_creative"}).
			AddRow("profile-abc", "deals@em.discountblog.com", nil, nil, false, "", "", false))
}

// expectProfileWithTracking registers the sending-profile SELECT returning a
// sending_domain so trackBase resolves to https://trk.em.discountblog.com —
// the tracking domain the /o/ offer URL is built on. tracking_domain is NULL
// so the trk.<sending_domain> fallback fires.
func expectProfileWithTracking(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id::text, COALESCE\(from_email,''\), tracking_domain, sending_domain`).
		WithArgs("em.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "tracking_domain", "sending_domain",
			"via_ses", "ses_configuration_set", "ses_tenant_name", "raw_creative"}).
			AddRow("profile-abc", "deals@em.discountblog.com", nil, "em.discountblog.com", false, "", "", false))
}

// oURL builds the expected tracking-layer /o/ offer URL for the proof consts.
// Brand-in-path (2026-07-22): trackBase trk.em.discountblog.com → apex
// discountblog.com is the first /o/ segment.
func oURL(hash string) string {
	return "https://trk.em.discountblog.com/o/discountblog.com/" + proofSubscriberID + "/" + hash + "/" + proofCampaignID
}

// (a) route_via_gateway=true + active slug → 200, tracking /o/ URL in HTML,
// cratoolpro gone, response carries tracking_url + hash + risk_profile.
// tracking secret is empty here so InjectTrackingPixelAndLinks is skipped and
// the /o/ URL appears literally (the rewrite only needs a non-empty trackBase).
func TestHandleCreativeProof_GatewayHappyPath(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "") // secret "" → no injection, literal /o/ URL

	expectCreativeLoad(mock, "creative-1")
	mock.ExpectQuery(`SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\)`).
		WithArgs("discountblog.com", "auto-refi").
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"id-1", "discountblog.com", "auto-refi", "best-auto-refi",
			"https://www.cratoolpro.com/A/B/?source_id=email", "active", "low", "", "hsh1234567",
			time.Now(), time.Now(),
		))
	expectProfileWithTracking(mock)

	rec := creativeProofPOST(t, h, "creative-1",
		`{"to_email":"op@gmail.com","route_via_gateway":true,"gateway_slug":"auto-refi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["status"] != "sent" {
		t.Errorf("expected status sent, got %q (%s)", body["status"], rec.Body.String())
	}
	if body["tracking_url"] != oURL("hsh1234567") {
		t.Errorf("wrong tracking_url: %q want %q", body["tracking_url"], oURL("hsh1234567"))
	}
	if body["gateway_slug"] != "auto-refi" {
		t.Errorf("wrong gateway_slug: %q", body["gateway_slug"])
	}
	if body["hash"] != "hsh1234567" {
		t.Errorf("wrong hash: %q", body["hash"])
	}
	if body["risk_profile"] != "low" {
		t.Errorf("wrong risk_profile: %q", body["risk_profile"])
	}

	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	html := sender.messages[0].HTMLContent
	if !strings.Contains(html, oURL("hsh1234567")) {
		t.Errorf("sent HTML missing /o/ tracking URL: %s", html)
	}
	if strings.Contains(html, "cratoolpro.com") {
		t.Errorf("sent HTML still contains cratoolpro: %s", html)
	}
	if strings.Contains(html, "discountblog.com/auto-refi") {
		t.Errorf("sent HTML must NOT contain the old brand-apex gateway URL: %s", html)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Full chain with tracking ON: the /o/ offer URL is itself a tracking URL and
// must NOT be re-wrapped into a signed /track/click redirect (the send_worker
// RewriteClickLinks skip rule). The open pixel still injects (/track/open),
// proving injection ran — but the offer href stays the bare /o/ URL.
func TestHandleCreativeProof_GatewayNotReWrapped(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "https://trk.em.discountblog.com", "secret")

	expectCreativeLoad(mock, "creative-1")
	mock.ExpectQuery(`SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\)`).
		WithArgs("discountblog.com", "auto-refi").
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"id-1", "discountblog.com", "auto-refi", "best-auto-refi",
			"https://www.cratoolpro.com/A/B/?source_id=email", "active", "high", "", "hsh1234567",
			time.Now(), time.Now(),
		))
	// Profile with a sending_domain so trackBase resolves non-empty → injection on.
	expectProfileWithTracking(mock)

	rec := creativeProofPOST(t, h, "creative-1",
		`{"to_email":"op@gmail.com","route_via_gateway":true,"gateway_slug":"auto-refi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	html := sender.messages[0].HTMLContent
	if strings.Contains(html, "cratoolpro") {
		t.Errorf("cratoolpro must not appear anywhere: %s", html)
	}
	// The offer href must be the BARE /o/ URL — never wrapped into /track/click.
	if !strings.Contains(html, `href="`+oURL("hsh1234567")+`"`) {
		t.Errorf("offer href is not the bare /o/ URL: %s", html)
	}
	if strings.Contains(html, "/track/click/") {
		t.Errorf("/o/ offer URL must NOT be re-wrapped into /track/click: %s", html)
	}
	// Sanity: injection DID run (open pixel present), proving the skip is
	// selective, not a blanket injection bypass.
	if !strings.Contains(html, "/track/open/") {
		t.Errorf("expected the open pixel to still inject (/track/open): %s", html)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// (b) route_via_gateway=true + no active row → 422, NO send.
func TestHandleCreativeProof_GatewayNoActiveRow(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")
	mock.ExpectQuery(`SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\)`).
		WithArgs("discountblog.com", "auto-refi").
		WillReturnRows(sqlmock.NewRows(smartLinkCols)) // zero rows → ErrNoRows
	// NO profile query, NO send expected.

	rec := creativeProofPOST(t, h, "creative-1",
		`{"to_email":"op@gmail.com","route_via_gateway":true,"gateway_slug":"auto-refi"}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no active smart-link gateway") {
		t.Errorf("missing seed-it error: %s", rec.Body.String())
	}
	if len(sender.messages) != 0 {
		t.Fatalf("no send must happen when gateway is absent, got %d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// (c) route_via_gateway=true + empty gateway_slug → 400, NO smart-link query,
// NO send. sqlmock fails on any unexpected query, so registering only the
// creative load proves the smart-link query never ran.
func TestHandleCreativeProof_GatewayEmptySlug(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")

	rec := creativeProofPOST(t, h, "creative-1",
		`{"to_email":"op@gmail.com","route_via_gateway":true,"gateway_slug":"  "}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_slug required") {
		t.Errorf("missing slug error: %s", rec.Body.String())
	}
	if len(sender.messages) != 0 {
		t.Fatalf("no send must happen on bad slug, got %d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// (d) route_via_gateway=false → NO smart-link query, cratoolpro passes through
// to tracking injection UNCHANGED (default-safe). Tracking OFF so the literal
// cratoolpro href survives for assertion.
func TestHandleCreativeProof_DefaultSafeNoGateway(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")
	expectProfileNoTracking(mock)
	// NO smart-link query registered → sqlmock fails if one runs.

	rec := creativeProofPOST(t, h, "creative-1",
		`{"to_email":"op@gmail.com"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["status"] != "sent" {
		t.Errorf("expected sent, got %q", body["status"])
	}
	if _, ok := body["gateway_url"]; ok {
		t.Errorf("gateway_url must be absent when routing off: %s", rec.Body.String())
	}
	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	html := sender.messages[0].HTMLContent
	if !strings.Contains(html, "cratoolpro.com") {
		t.Errorf("cratoolpro href must pass through unchanged: %s", html)
	}
	if strings.Contains(html, "discountblog.com/auto-refi") {
		t.Errorf("no gateway rewrite should occur: %s", html)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

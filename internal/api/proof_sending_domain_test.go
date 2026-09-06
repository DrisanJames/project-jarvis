package api

// Optional `sending_domain` on the two proof-send endpoints (operator
// 2026-09-06: the brand resolves from the SENDING domain, never hardcoded).
//
//   - explicit sending_domain wins over the offer's web_property brand kit /
//     the creative's brand_code;
//   - absent field → byte-identical historical resolution (negative control;
//     the creative path's control is TestHandleCreativeProof_DefaultTransportUnchanged);
//   - a domain with no active profile → the existing failure shape, no send
//     (offer path: 422; creative path: 200 {"status":"error"} — see below).
//
// sqlmock + mockSender pattern from creative_proof_gateway_test.go.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

func offerProofPOST(t *testing.T, h *ProofSendHandler, offerID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/offer-center/offers/"+offerID+"/proof-send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", gwTestOrgID)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", offerID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.HandleProofSend(rec, req)
	return rec
}

func expectOfferWebProperty(mock sqlmock.Sqlmock, offerID, webProperty string) {
	mock.ExpectQuery(`SELECT COALESCE\(web_property,''\) FROM mailing_offers WHERE id = \$1`).
		WithArgs(offerID).
		WillReturnRows(sqlmock.NewRows([]string{"web_property"}).AddRow(webProperty))
}

// expectProfileFor registers the PMTA profile SELECT for one sending domain.
func expectProfileFor(mock sqlmock.Sqlmock, domain, profileID string) {
	mock.ExpectQuery(`SELECT id::text, COALESCE\(from_email,''\), tracking_domain, sending_domain`).
		WithArgs(domain).
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "tracking_domain", "sending_domain",
			"via_ses", "ses_configuration_set", "ses_tenant_name", "raw_creative"}).
			AddRow(profileID, "news@"+domain, nil, domain, false, "", "", false))
}

func expectProfileMissing(mock sqlmock.Sqlmock, domain string) {
	mock.ExpectQuery(`SELECT id::text, COALESCE\(from_email,''\), tracking_domain, sending_domain`).
		WithArgs(domain).
		WillReturnError(sql.ErrNoRows)
}

// expectOfferProofPieces registers the creative / subject / from-name loads
// sendOneProof performs for one proof item.
func expectOfferProofPieces(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_offer_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow(`<html><body><a href="https://example.com/x">x</a></body></html>`))
	mock.ExpectQuery(`SELECT subject_line FROM mailing_offer_subject_lines`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).AddRow("Hello"))
	mock.ExpectQuery(`SELECT from_name FROM mailing_offer_from_names`).
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).AddRow("Team"))
}

const offerProofBody = `{"recipient_email":"op@gmail.com","proofs":[{"creative_id":"c1","subject_line_id":"s1","from_name_id":"f1"}]`

// Explicit sending_domain wins: the offer's web_property is discountblog, the
// request names em.quizfiesta.com, and the profile lookup + send use quizfiesta.
func TestHandleProofSend_ExplicitSendingDomainWins(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectOfferWebProperty(mock, "offer-1", "discountblog")
	expectProfileFor(mock, "em.quizfiesta.com", "profile-qf")
	expectOfferProofPieces(mock)

	rec := offerProofPOST(t, h, "offer-1", offerProofBody+`,"sending_domain":" EM.quizfiesta.com "}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	if got := sender.messages[0].ProfileID; got != "profile-qf" {
		t.Errorf("sent through profile %q, want the explicit domain's profile-qf", got)
	}
	if got := sender.messages[0].FromEmail; got != "news@em.quizfiesta.com" {
		t.Errorf("from_email = %q, want the quizfiesta profile's", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// NEGATIVE CONTROL — no sending_domain: resolution is the historical
// GetBrandKit(web_property) path (discountblog → em.discountblog.com).
func TestHandleProofSend_AbsentSendingDomainUnchanged(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectOfferWebProperty(mock, "offer-1", "discountblog")
	expectProfileFor(mock, "em.discountblog.com", "profile-db")
	expectOfferProofPieces(mock)

	rec := offerProofPOST(t, h, "offer-1", offerProofBody+`}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 1 || sender.messages[0].ProfileID != "profile-db" {
		t.Fatalf("absent sending_domain must resolve via web_property brand kit; sends=%d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// A sending_domain with no active profile for the transport → the existing
// 422, no proof pieces loaded, no send.
func TestHandleProofSend_UnknownSendingDomain422(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectOfferWebProperty(mock, "offer-1", "discountblog")
	expectProfileMissing(mock, "em.nope.com")

	rec := offerProofPOST(t, h, "offer-1", offerProofBody+`,"sending_domain":"em.nope.com"}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no active pmta sending profile") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if len(sender.messages) != 0 {
		t.Errorf("no message should be sent, got %d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// Creative-registry proof: explicit sending_domain overrides the creative's
// brand_code (DB → em.discountblog.com) with em.quizfiesta.com.
func TestHandleCreativeProof_ExplicitSendingDomainWins(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1") // brand_code DB
	expectProfileFor(mock, "em.quizfiesta.com", "profile-qf")

	rec := creativeProofPOST(t, h, "creative-1", `{"to_email":"op@gmail.com","sending_domain":"em.quizfiesta.com"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 1 || sender.messages[0].ProfileID != "profile-qf" {
		t.Fatalf("explicit sending_domain must route through profile-qf; sends=%d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// Unknown sending_domain on the creative path. NOTE: this endpoint has never
// 422'd an unresolved profile — HandleCreativeProof maps every
// sendProofMessage error to 200 {"status":"error"} (creative_proof_send.go),
// and the Creative Studio UI reads json.status. Pinned to that existing
// contract: no send, status=error, the error names the domain.
func TestHandleCreativeProof_UnknownSendingDomain_ErrorNoSend(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")
	expectProfileMissing(mock, "em.nope.com")

	rec := creativeProofPOST(t, h, "creative-1", `{"to_email":"op@gmail.com","sending_domain":"em.nope.com"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the existing 200/status=error contract, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"error"`) || !strings.Contains(rec.Body.String(), "em.nope.com") {
		t.Fatalf("expected status=error naming em.nope.com, got %s", rec.Body.String())
	}
	if len(sender.messages) != 0 {
		t.Errorf("no message should be sent, got %d", len(sender.messages))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

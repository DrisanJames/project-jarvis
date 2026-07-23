package api

// Tests for the proof-send transport toggle (PMTA dedicated IPs vs the
// brand's live SES tenant route) added to creative_proof_send.go /
// offer_proof_send.go / offer_proofs.go.
//
// Coverage:
//   - normalizeProofTransport: default/aliases/rejects.
//   - HandleCreativeProof transport=ses: resolves the m.<apex> via_ses=true
//     tenant profile and stamps X-SES-CONFIGURATION-SET / X-SES-TENANT on the
//     outbound message (header parity with send_worker's via_ses branch).
//   - HandleCreativeProof default (no transport): em.<apex> lookup, NO SES
//     headers — pre-existing behavior unchanged.
//   - Invalid transport → 400, no profile query, no send.
//
// Follows the sqlmock + mockSender pattern from creative_proof_gateway_test.go.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNormalizeProofTransport(t *testing.T) {
	for in, want := range map[string]string{
		"":      proofTransportPMTA,
		"pmta":  proofTransportPMTA,
		"PMTA":  proofTransportPMTA,
		" ses ": proofTransportSES,
		"SES":   proofTransportSES,
	} {
		got, err := normalizeProofTransport(in)
		if err != nil || got != want {
			t.Errorf("normalizeProofTransport(%q) = %q, %v — want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"smtp", "sparkpost", "kumo", "ses relay"} {
		if _, err := normalizeProofTransport(bad); err == nil {
			t.Errorf("normalizeProofTransport(%q) should error", bad)
		}
	}
}

// transport=ses must look up m.<apex> with via_ses=TRUE and stamp the SES
// tenant routing headers on the message.
func TestHandleCreativeProof_SESTransport(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")
	mock.ExpectQuery(`SELECT id::text, COALESCE\(from_email,''\), tracking_domain, sending_domain`).
		WithArgs("m.discountblog.com").
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "tracking_domain", "sending_domain",
			"via_ses", "ses_configuration_set", "ses_tenant_name"}).
			AddRow("profile-ses", "hello@em.discountblog.com", "t.m.discountblog.com", "m.discountblog.com",
				true, "discountblog", "discountblog"))

	rec := creativeProofPOST(t, h, "creative-1", `{"to_email":"op@gmail.com","transport":"ses"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body["status"] != "sent" {
		t.Fatalf("expected sent, got %q (%s)", body["status"], rec.Body.String())
	}
	if body["transport"] != "ses" {
		t.Errorf("response transport = %q, want ses", body["transport"])
	}

	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	msg := sender.messages[0]
	if msg.ProfileID != "profile-ses" {
		t.Errorf("sent through profile %q, want the SES tenant profile", msg.ProfileID)
	}
	if msg.Headers["X-SES-CONFIGURATION-SET"] != "discountblog" {
		t.Errorf("X-SES-CONFIGURATION-SET = %q, want discountblog", msg.Headers["X-SES-CONFIGURATION-SET"])
	}
	if msg.Headers["X-SES-TENANT"] != "discountblog" {
		t.Errorf("X-SES-TENANT = %q, want discountblog", msg.Headers["X-SES-TENANT"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// No transport field → em.<apex> lookup, no SES headers: the pre-toggle
// request body behaves exactly as before.
func TestHandleCreativeProof_DefaultTransportUnchanged(t *testing.T) {
	h, mock, sender := newGatewayProofHandler(t, "", "")

	expectCreativeLoad(mock, "creative-1")
	expectProfileWithTracking(mock) // WithArgs("em.discountblog.com"), via_ses=false

	rec := creativeProofPOST(t, h, "creative-1", `{"to_email":"op@gmail.com"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.messages))
	}
	msg := sender.messages[0]
	if _, ok := msg.Headers["X-SES-CONFIGURATION-SET"]; ok {
		t.Error("default transport must not stamp X-SES-CONFIGURATION-SET")
	}
	if _, ok := msg.Headers["X-SES-TENANT"]; ok {
		t.Error("default transport must not stamp X-SES-TENANT")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// An unknown transport is rejected up front: 400, no creative/profile queries
// consumed beyond the org-scoped creative load, and no send.
func TestHandleCreativeProof_InvalidTransport(t *testing.T) {
	h, _, sender := newGatewayProofHandler(t, "", "")

	rec := creativeProofPOST(t, h, "creative-1", `{"to_email":"op@gmail.com","transport":"pigeon"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sender.messages) != 0 {
		t.Errorf("no message should be sent on invalid transport, got %d", len(sender.messages))
	}
}

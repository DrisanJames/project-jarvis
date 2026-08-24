package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The safe idiom must NOT be blocked, and the hole shapes must be.
// Every "want clean" row here is copy an operator is supposed to write; every
// "want blocked" row is a shape that reaches an inbox broken.
func TestSubjectRenderProblem(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		blocked bool
	}{
		{"plain subject", "Insurance for your car, home, apartment & more", false},
		{"empty subject", "", false},
		// The 08/24 board's five phantom blockers.
		{"default filter guards the hole",
			`{{ first_name | default: "Homeowner" }}, see how much home equity you could access today`, false},
		{"default filter mid-sentence",
			`See what {{ first_name | default: "homeowners" }} in your area qualify for`, false},
		{"100%-coverage brand token", `A message from {{ brand.name }}`, false},
		{"100%-coverage system token", `Your quote, prepared {{ system.dispatch_date }}`, false},
		// The RR-HELOC defect the gate exists for: parses fine, renders a hole.
		{"bare custom token leaves a double space",
			"You could tap {{custom.equity_estimate}} today", true},
		{"bare first_name leaves a dangling comma",
			"{{ first_name }}, your rate is ready", true},
		{"token is the whole subject", "{{ first_name }}", true},
		{"space before punctuation",
			"Ready when you are {{ first_name }}, let's go", true},
		{"unparseable liquid", "{% if %} broken", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := subjectRenderProblem(c.subject)
			if c.blocked && msg == "" {
				t.Fatalf("expected a blocker for %q, got clean", c.subject)
			}
			if !c.blocked && msg != "" {
				t.Fatalf("expected clean for %q, got: %s", c.subject, msg)
			}
		})
	}
}

// A campaign the audience finalizer has not finished writing carries a NULL
// offer_id LEGITIMATELY: reserveCampaignForDeploy inserts the row before
// createPMTAWaveCampaign writes offer_id and sending_profile_id. Blocking
// there is what produced 6 phantom blockers on 2026-08-23.
func TestBoardGrid_MissingOfferIsPendingWhileFinalizing(t *testing.T) {
	s := &BoardGridService{}
	pending := BoardCell{
		Property: "RR", Slot: "01:01", Name: "08232026 - RR - Globe",
		Subject: "Globe Life", BrandRoot: "refinanceratesusa.com",
		Status: "finalizing_audience", PendingFinalize: true,
	}
	got := findingCodes(s.runGates(nil, "2026-08-23", []BoardCell{pending}))
	if got["MISSING_OFFER"] != 0 {
		t.Fatalf("a finalizing campaign must not blocker on MISSING_OFFER: %v", got)
	}
	if got["OFFER_PENDING"] != 1 {
		t.Fatalf("want 1 OFFER_PENDING warn, got %v", got)
	}

	// Once it settles, the SAME null offer is the real defect again.
	settled := pending
	settled.Status, settled.PendingFinalize = "scheduled", false
	got = findingCodes(s.runGates(nil, "2026-08-23", []BoardCell{settled}))
	if got["MISSING_OFFER"] != 1 {
		t.Fatalf("a scheduled campaign with no offer must still block: %v", got)
	}
	if got["OFFER_PENDING"] != 0 {
		t.Fatalf("settled campaign must not warn OFFER_PENDING: %v", got)
	}

	// The exemption still wins over both.
	exempt := pending
	exempt.Name = "aug23 - aad - KUMO-WARM - newsletter"
	got = findingCodes(s.runGates(nil, "2026-08-23", []BoardCell{exempt}))
	if got["MISSING_OFFER"] != 0 || got["OFFER_PENDING"] != 0 {
		t.Fatalf("KUMO-WARM must be exempt from both: %v", got)
	}
}

// The creative endpoint is the answer to "what will this cell SAY" — it must
// return the STORED send-truth columns, org-scoped, and never a write.
func TestBoardGridCreative_ReturnsSendTruth(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewBoardGridService(db)

	const cid = "11111111-2222-3333-4444-555555555555"
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WithArgs(cid, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "status", "sending_domain", "from_name", "from_email", "reply_to",
			"offer_name", "subject", "preview_text", "len", "html", "recipients"}).
			AddRow("08242026 - FC - HELOC", "scheduled", "m.financialcalculate.com",
				"West Capital Lending", "hello@em.financialcalculate.com", "",
				"Internal - West Capital HELOC - v4",
				`{{ first_name | default: "Homeowner" }}, see your equity`,
				"Draw only what each phase needs.", 8515, "<html>body</html>", 28114))

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/board-grid/creative?campaign_id="+cid, nil)
	rec := httptest.NewRecorder()
	svc.HandleGetCellCreative(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out boardGridCreative
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FromName != "West Capital Lending" || out.Preheader == "" || out.HTML == "" {
		t.Fatalf("send-truth fields missing: %+v", out)
	}
	// The safe idiom must render for the operator and NOT be reported broken.
	if out.SubjectRendered != "Homeowner, see your equity" {
		t.Fatalf("subject_rendered = %q", out.SubjectRendered)
	}
	if out.SubjectProblem != "" {
		t.Fatalf("safe subject must not report a problem: %s", out.SubjectProblem)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestBoardGridCreative_RequiresCampaignID(t *testing.T) {
	svc := NewBoardGridService(nil)
	rec := httptest.NewRecorder()
	svc.HandleGetCellCreative(rec, httptest.NewRequest(http.MethodGet, "/api/mailing/board-grid/creative", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

package api

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

type mockSender struct {
	mu       sync.Mutex
	messages []*worker.EmailMessage
	err      error
}

func (m *mockSender) Send(_ context.Context, msg *worker.EmailMessage) (*worker.SendResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)
	if m.err != nil {
		return nil, m.err
	}
	return &worker.SendResult{Success: true, MessageID: "mock-msg-" + msg.ProfileID}, nil
}

func TestBuildUnsubDisclaimerHTML_IncludesPhysicalAddress(t *testing.T) {
	tests := []struct {
		name            string
		brandName       string
		physicalAddress string
		wantBrand       string
		wantAddress     string
	}{
		{
			name:            "branded with custom address",
			brandName:       "DiscountBlog",
			physicalAddress: "123 Main St, Springfield, IL 62704",
			wantBrand:       "DiscountBlog",
			wantAddress:     "123 Main St, Springfield, IL 62704",
		},
		{
			name:            "empty brand uses no prefix",
			brandName:       "",
			physicalAddress: "456 Elm St",
			wantBrand:       "",
			wantAddress:     "456 Elm St",
		},
		{
			name:            "empty address falls back to default postal (no JVC)",
			brandName:       "QuizFiesta",
			physicalAddress: "",
			wantBrand:       "QuizFiesta",
			wantAddress:     "30 N Gould St, Ste R, Sheridan, WY 82801",
		},
		{
			name:            "both empty uses fallback postal, no brand prefix, no JVC",
			brandName:       "",
			physicalAddress: "",
			wantBrand:       "",
			wantAddress:     "30 N Gould St, Ste R, Sheridan, WY 82801",
		},
		{
			name:            "JVC name in caller address is stripped",
			brandName:       "DiscountBlog",
			physicalAddress: "James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801",
			wantBrand:       "DiscountBlog",
			wantAddress:     "30 N Gould St, Ste R, Sheridan, WY 82801",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			html := buildUnsubDisclaimerHTML(tc.brandName, tc.physicalAddress)

			if !strings.Contains(html, unsubDisclaimerMarker) {
				t.Error("missing idempotency marker")
			}
			if !strings.Contains(html, `{{ system.unsubscribe_url }}`) {
				t.Error("missing unsubscribe merge tag")
			}
			if !strings.Contains(html, tc.wantAddress) {
				t.Errorf("missing physical address %q in output: %s", tc.wantAddress, html)
			}
			// The corporate identity must NEVER appear in a recipient-facing footer.
			if strings.Contains(html, "James Ventures Corp") {
				t.Errorf("JVC corporate identity leaked into footer: %s", html)
			}
			if tc.wantBrand != "" && !strings.Contains(html, tc.wantBrand+" · ") {
				t.Errorf("missing brand prefix %q in output: %s", tc.wantBrand, html)
			}
			if tc.wantBrand == "" && strings.Contains(html, " · ") {
				t.Errorf("unexpected brand separator in output: %s", html)
			}
		})
	}
}

func TestAppendUnsubDisclaimer_Idempotency(t *testing.T) {
	html := `<html><body><p>Hello</p></body></html>`

	first := appendUnsubDisclaimer(html, "DiscountBlog", "123 Main St")
	if !strings.Contains(first, unsubDisclaimerMarker) {
		t.Fatal("first injection should contain marker")
	}
	if !strings.Contains(first, "123 Main St") {
		t.Fatal("first injection should contain address")
	}

	second := appendUnsubDisclaimer(first, "DiscountBlog", "123 Main St")
	if first != second {
		t.Error("second injection should be a no-op (idempotency)")
	}

	markerCount := strings.Count(second, unsubDisclaimerMarker)
	if markerCount != 1 {
		t.Errorf("expected exactly 1 disclaimer marker, got %d", markerCount)
	}

	addrCount := strings.Count(second, "123 Main St")
	if addrCount != 1 {
		t.Errorf("expected exactly 1 physical address, got %d", addrCount)
	}
}

func TestAppendUnsubDisclaimer_InsertsBeforeBodyClose(t *testing.T) {
	html := `<html><body class="dark"><p>Content</p></body></html>`
	result := appendUnsubDisclaimer(html, "TestBrand", "789 Oak Ave")

	contentIdx := strings.Index(result, "<p>Content</p>")
	markerIdx := strings.Index(result, unsubDisclaimerMarker)
	bodyCloseIdx := strings.Index(result, "</body>")

	if contentIdx < 0 || markerIdx < 0 || bodyCloseIdx < 0 {
		t.Fatalf("missing expected elements: content=%d marker=%d bodyClose=%d", contentIdx, markerIdx, bodyCloseIdx)
	}
	// Footer must be in the FOOTER: after existing content, before </body>.
	if markerIdx < contentIdx {
		t.Error("disclaimer should appear AFTER existing content (footer, not header)")
	}
	if markerIdx > bodyCloseIdx {
		t.Error("disclaimer should appear before </body>")
	}
}

func TestAppendUnsubDisclaimer_NoBodyTag(t *testing.T) {
	html := `<div>No body tag here</div>`
	result := appendUnsubDisclaimer(html, "", "")

	if !strings.HasSuffix(result, unsubDisclaimerMarker+
		`<p style="margin:0;padding:8px 20px;font-family:Arial,Helvetica,sans-serif;font-size:11px;color:#999999;text-align:center;">`+
		`Please do not reply, this email box is not monitored. To stop email subscriptions at any time please `+
		`<a href="{{ system.unsubscribe_url }}" style="color:#999999;">unsubscribe</a>.`+
		`</p>`+
		`<p style="margin:0;padding:4px 20px 8px;font-family:Arial,Helvetica,sans-serif;font-size:10px;color:#bbbbbb;text-align:center;">`+
		`30 N Gould St, Ste R, Sheridan, WY 82801`+
		`</p>`) {
		t.Errorf("without <body>, disclaimer should be appended at the end: %s", result)
	}
	if strings.Contains(result, "James Ventures Corp") {
		t.Error("JVC corporate identity must not appear")
	}
}

func TestStripJVCName(t *testing.T) {
	cases := map[string]string{
		"James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801": "30 N Gould St, Ste R, Sheridan, WY 82801",
		"James Ventures Corp · Post Falls, ID 83854":                    "Post Falls, ID 83854",
		"30 N Gould St, Ste R, Sheridan, WY 82801":                      "30 N Gould St, Ste R, Sheridan, WY 82801",
		"":                                                              "",
	}
	for in, want := range cases {
		if got := stripJVCName(in); got != want {
			t.Errorf("stripJVCName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBrandKits_AllHavePhysicalAddress(t *testing.T) {
	required := []string{"discountblog", "quizfiesta", "historythinking", "myownhealth"}
	for _, key := range required {
		kit, ok := GetBrandKit(key)
		if !ok {
			t.Errorf("brand kit %q not found", key)
			continue
		}
		if kit.PhysicalAddress == "" {
			t.Errorf("brand kit %q has empty PhysicalAddress (CAN-SPAM violation)", key)
		}
		if kit.SendingDomain == "" {
			t.Errorf("brand kit %q has empty SendingDomain", key)
		}
	}
}

func TestBuildProofRenderContext_UsesSignedUnsubURL(t *testing.T) {
	signedURL := "https://trk.example.com/track/unsubscribe/b3JnfGNhbXBhaWdu/abc123sig"
	rc := buildProofRenderContext("test@gmail.com", "https://trk.example.com", "email-uuid-123", signedURL)

	system, ok := rc["system"].(map[string]interface{})
	if !ok {
		t.Fatal("system key missing or wrong type")
	}

	unsub, ok := system["unsubscribe_url"].(string)
	if !ok {
		t.Fatal("system.unsubscribe_url missing")
	}
	if unsub != signedURL {
		t.Errorf("expected signed URL %q, got %q", signedURL, unsub)
	}
	if strings.Contains(unsub, "/proof/") {
		t.Error("unsubscribe URL should NOT contain /proof/ path (that route does not exist)")
	}
}

func TestBuildProofRenderContext_FallbackWhenNoSignedURL(t *testing.T) {
	rc := buildProofRenderContext("test@gmail.com", "https://trk.example.com", "email-uuid-123", "")

	system := rc["system"].(map[string]interface{})
	unsub := system["unsubscribe_url"].(string)

	if unsub != "https://trk.example.com/track/unsubscribe" {
		t.Errorf("expected fallback URL, got %q", unsub)
	}
}

func TestBuildProofRenderContext_SubscriberFields(t *testing.T) {
	rc := buildProofRenderContext("james@example.com", "https://trk.example.com", "eid", "https://unsub")

	if rc["email"] != "james@example.com" {
		t.Error("email not set")
	}
	if rc["email_local"] != "james" {
		t.Error("email_local not parsed")
	}
	if rc["email_domain"] != "example.com" {
		t.Error("email_domain not parsed")
	}
	if rc["first_name"] != "Proof" {
		t.Error("first_name should be Proof for proof sends")
	}

	sub := rc["subscriber"].(map[string]interface{})
	if sub["id"] != proofSubscriberID {
		t.Errorf("subscriber ID should be proof constant, got %v", sub["id"])
	}

	camp := rc["campaign"].(map[string]interface{})
	if camp["id"] != proofCampaignID {
		t.Errorf("campaign ID should be proof constant, got %v", camp["id"])
	}
}

func TestSendOneProof_FullPipeline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sender := &mockSender{}
	h := &ProofSendHandler{
		db:             db,
		sender:         sender,
		trackingURL:    "https://trk.em.discountblog.com",
		trackingSecret: "test-secret-key",
		orgID:          "00000000-0000-0000-0000-000000000001",
		smartLinks:     NewSmartLinkService(db),
	}

	creativeHTML := `<html><body>` + unsubDisclaimerMarker +
		`<p style="margin:0;padding:8px 20px;font-family:Arial,Helvetica,sans-serif;font-size:11px;color:#999999;text-align:center;">` +
		`Please do not reply, this email box is not monitored. To stop email subscriptions at any time please ` +
		`<a href="{{ system.unsubscribe_url }}" style="color:#999999;">unsubscribe</a>.</p>` +
		`<p style="margin:0;padding:4px 20px 8px;font-family:Arial,Helvetica,sans-serif;font-size:10px;color:#bbbbbb;text-align:center;">` +
		`DiscountBlog · 30 N Gould St, Ste R, Sheridan, WY 82801</p>` +
		`<h1>Big Sale!</h1><a href="https://discountblog.com/deal">Shop Now</a></body></html>`

	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_offer_creatives`).
		WithArgs("creative-1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow(creativeHTML))

	mock.ExpectQuery(`SELECT subject_line FROM mailing_offer_subject_lines`).
		WithArgs("subj-1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).AddRow("Save 50% Today!"))

	mock.ExpectQuery(`SELECT from_name FROM mailing_offer_from_names`).
		WithArgs("from-1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).AddRow("Jamie @ DiscountBlog"))

	ts := mailing.NewTemplateService()
	result := h.sendOneProof(
		context.Background(),
		ts,
		"offer-1",
		proofTransportPMTA,
		proofProfile{
			ID:            "profile-abc",
			FromEmail:     "deals@em.discountblog.com",
			TrackBase:     "https://trk.em.discountblog.com",
			SendingDomain: "em.discountblog.com",
		},
		"james@gmail.com",
		"gmail",
		proofItem{CreativeID: "creative-1", SubjectLineID: "subj-1", FromNameID: "from-1"},
		0,
	)

	if result.Status != "sent" {
		t.Fatalf("expected status 'sent', got %q (error: %s)", result.Status, result.Error)
	}
	if result.CreativeID != "creative-1" {
		t.Errorf("wrong creative ID: %s", result.CreativeID)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()

	if len(sender.messages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(sender.messages))
	}
	msg := sender.messages[0]

	// [PROOF] prefix in subject
	if !strings.HasPrefix(msg.Subject, "[PROOF] ") {
		t.Errorf("subject should start with [PROOF], got: %s", msg.Subject)
	}
	if !strings.Contains(msg.Subject, "Save 50% Today!") {
		t.Errorf("subject missing original text: %s", msg.Subject)
	}

	// From fields
	if msg.FromName != "Jamie @ DiscountBlog" {
		t.Errorf("wrong FromName: %s", msg.FromName)
	}
	if msg.FromEmail != "deals@em.discountblog.com" {
		t.Errorf("wrong FromEmail: %s", msg.FromEmail)
	}

	// Profile and ISP routing
	if msg.ProfileID != "profile-abc" {
		t.Errorf("wrong ProfileID: %s", msg.ProfileID)
	}
	if msg.RecipientISP != "gmail" {
		t.Errorf("wrong RecipientISP: %s", msg.RecipientISP)
	}

	// Unsub URL should be signed format, NOT /proof/ path
	if strings.Contains(msg.HTMLContent, "/track/unsubscribe/proof/") {
		t.Error("HTML contains old /track/unsubscribe/proof/ path (should use signed URL)")
	}

	expectedUnsub := worker.GenerateUnsubscribeURL(
		"00000000-0000-0000-0000-000000000001",
		proofCampaignID,
		proofSubscriberID,
		"https://trk.em.discountblog.com",
		"test-secret-key",
	)
	if !strings.Contains(msg.HTMLContent, expectedUnsub) {
		t.Errorf("HTML should contain signed unsub URL %q", expectedUnsub)
	}

	// List-Unsubscribe headers — shared RFC 8058 helper (2026-07-21): proofs
	// must be header-parity with production sends, carrying BOTH the
	// From-domain-aligned mailto leg and the brand https one-click leg.
	if msg.Headers["List-Unsubscribe"] == "" {
		t.Error("missing List-Unsubscribe header")
	}
	if !strings.Contains(msg.Headers["List-Unsubscribe"], "/track/unsubscribe/") {
		t.Errorf("List-Unsubscribe should use tracking route, got: %s", msg.Headers["List-Unsubscribe"])
	}
	if !strings.Contains(msg.Headers["List-Unsubscribe"], "<mailto:unsub+") ||
		!strings.Contains(msg.Headers["List-Unsubscribe"], "@em.discountblog.com?subject=unsubscribe>") {
		t.Errorf("List-Unsubscribe missing From-domain-aligned mailto leg: %s", msg.Headers["List-Unsubscribe"])
	}
	if !strings.Contains(msg.Headers["List-Unsubscribe"], "https://trk.em.discountblog.com/track/unsubscribe/") {
		t.Errorf("List-Unsubscribe https leg not on the brand tracking host: %s", msg.Headers["List-Unsubscribe"])
	}
	if msg.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("wrong List-Unsubscribe-Post: %s", msg.Headers["List-Unsubscribe-Post"])
	}

	// Physical address present (CAN-SPAM) — but the corporate identity must NOT be.
	if !strings.Contains(msg.HTMLContent, "30 N Gould St") {
		t.Error("HTML missing physical address (CAN-SPAM violation)")
	}
	if !strings.Contains(msg.HTMLContent, "Sheridan, WY") {
		t.Error("HTML missing city/state in physical address")
	}
	if strings.Contains(msg.HTMLContent, "James Ventures Corp") {
		t.Error("JVC corporate identity leaked into sent HTML")
	}

	// Proof headers
	if msg.Headers["X-Proof-Send"] != "true" {
		t.Error("missing X-Proof-Send header")
	}
	if msg.Headers["X-Offer-ID"] != "offer-1" {
		t.Errorf("wrong X-Offer-ID: %s", msg.Headers["X-Offer-ID"])
	}

	// Liquid merge tags should be resolved (not present as raw tags)
	if strings.Contains(msg.HTMLContent, "{{ system.unsubscribe_url }}") {
		t.Error("unresolved Liquid merge tag found in final HTML")
	}

	// Tracking pixel should be injected
	if !strings.Contains(msg.HTMLContent, "/track/open/") {
		t.Error("missing open tracking pixel")
	}
}

func TestSendOneProof_CreativeNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sender := &mockSender{}
	h := &ProofSendHandler{
		db:             db,
		sender:         sender,
		trackingURL:    "https://trk.example.com",
		trackingSecret: "secret",
		orgID:          "org-1",
		smartLinks:     NewSmartLinkService(db),
	}

	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_offer_creatives`).
		WithArgs("bad-creative", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow(""))

	ts := mailing.NewTemplateService()
	result := h.sendOneProof(
		context.Background(), ts,
		"offer-1", proofTransportPMTA,
		proofProfile{ID: "profile-1", FromEmail: "from@example.com",
			TrackBase: "https://trk.example.com", SendingDomain: "em.example.com"},
		"test@gmail.com", "gmail",
		proofItem{CreativeID: "bad-creative", SubjectLineID: "s1", FromNameID: "f1"},
		0,
	)

	if result.Status != "error" {
		t.Errorf("expected error status, got %q", result.Status)
	}
	if !strings.Contains(result.Error, "creative not found") {
		t.Errorf("expected 'creative not found' error, got %q", result.Error)
	}

	if len(sender.messages) != 0 {
		t.Error("no message should be sent when creative is empty")
	}
}

func TestSendOneProof_SenderFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	sender := &mockSender{err: context.DeadlineExceeded}
	h := &ProofSendHandler{
		db:             db,
		sender:         sender,
		trackingURL:    "https://trk.example.com",
		trackingSecret: "secret",
		orgID:          "org-1",
		smartLinks:     NewSmartLinkService(db),
	}

	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_offer_creatives`).
		WithArgs("c1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow("<html><body><p>Content</p></body></html>"))

	mock.ExpectQuery(`SELECT subject_line FROM mailing_offer_subject_lines`).
		WithArgs("s1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).AddRow("Test Subject"))

	mock.ExpectQuery(`SELECT from_name FROM mailing_offer_from_names`).
		WithArgs("f1", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).AddRow("Test Sender"))

	ts := mailing.NewTemplateService()
	result := h.sendOneProof(
		context.Background(), ts,
		"offer-1", proofTransportPMTA,
		proofProfile{ID: "profile-1", FromEmail: "from@example.com",
			TrackBase: "https://trk.example.com", SendingDomain: "em.example.com"},
		"test@gmail.com", "gmail",
		proofItem{CreativeID: "c1", SubjectLineID: "s1", FromNameID: "f1"},
		0,
	)

	if result.Status != "error" {
		t.Errorf("expected error status, got %q", result.Status)
	}
	if !strings.Contains(result.Error, "deadline exceeded") {
		t.Errorf("expected deadline error, got %q", result.Error)
	}
}

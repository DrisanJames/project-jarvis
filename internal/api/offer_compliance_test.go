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
			name:            "empty address falls back to default",
			brandName:       "QuizFiesta",
			physicalAddress: "",
			wantBrand:       "QuizFiesta",
			wantAddress:     "James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801",
		},
		{
			name:            "both empty uses fallback address no brand prefix",
			brandName:       "",
			physicalAddress: "",
			wantBrand:       "",
			wantAddress:     "James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801",
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
			if tc.wantBrand != "" && !strings.Contains(html, tc.wantBrand+" · ") {
				t.Errorf("missing brand prefix %q in output: %s", tc.wantBrand, html)
			}
			if tc.wantBrand == "" && strings.Contains(html, " · ") {
				t.Errorf("unexpected brand separator in output: %s", html)
			}
		})
	}
}

func TestInjectUnsubDisclaimerBrand_Idempotency(t *testing.T) {
	html := `<html><body><p>Hello</p></body></html>`

	first := injectUnsubDisclaimerBrand(html, "DiscountBlog", "123 Main St")
	if !strings.Contains(first, unsubDisclaimerMarker) {
		t.Fatal("first injection should contain marker")
	}
	if !strings.Contains(first, "123 Main St") {
		t.Fatal("first injection should contain address")
	}

	second := injectUnsubDisclaimerBrand(first, "DiscountBlog", "123 Main St")
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

func TestInjectUnsubDisclaimerBrand_InsertsAfterBodyTag(t *testing.T) {
	html := `<html><body class="dark"><p>Content</p></body></html>`
	result := injectUnsubDisclaimerBrand(html, "TestBrand", "789 Oak Ave")

	bodyClose := strings.Index(result, `class="dark">`)
	markerIdx := strings.Index(result, unsubDisclaimerMarker)
	contentIdx := strings.Index(result, "<p>Content</p>")

	if bodyClose < 0 || markerIdx < 0 || contentIdx < 0 {
		t.Fatalf("missing expected elements: bodyClose=%d marker=%d content=%d", bodyClose, markerIdx, contentIdx)
	}
	if markerIdx < bodyClose {
		t.Error("disclaimer should appear after <body> tag close")
	}
	if markerIdx > contentIdx {
		t.Error("disclaimer should appear before existing content")
	}
}

func TestInjectUnsubDisclaimerBrand_NoBodyTag(t *testing.T) {
	html := `<div>No body tag here</div>`
	result := injectUnsubDisclaimerBrand(html, "", "")

	if !strings.HasPrefix(result, unsubDisclaimerMarker) {
		t.Error("without <body>, disclaimer should be prepended")
	}
	if !strings.Contains(result, "James Ventures Corp") {
		t.Error("default address should appear when no address given")
	}
}

func TestInjectUnsubDisclaimer_DefaultSignature(t *testing.T) {
	html := `<html><body><p>Test</p></body></html>`
	result := injectUnsubDisclaimer(html)

	if !strings.Contains(result, "James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801") {
		t.Error("default wrapper should include fallback physical address")
	}
	if strings.Contains(result, " · ") {
		t.Error("default wrapper should not include brand prefix separator")
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
	}

	creativeHTML := `<html><body>` + unsubDisclaimerMarker +
		`<p style="margin:0;padding:8px 20px;font-family:Arial,Helvetica,sans-serif;font-size:11px;color:#999999;text-align:center;">` +
		`Please do not reply, this email box is not monitored. To stop email subscriptions at any time please ` +
		`<a href="{{ system.unsubscribe_url }}" style="color:#999999;">unsubscribe</a>.</p>` +
		`<p style="margin:0;padding:4px 20px 8px;font-family:Arial,Helvetica,sans-serif;font-size:10px;color:#bbbbbb;text-align:center;">` +
		`DiscountBlog · James Ventures Corp, 30 N Gould St, Ste R, Sheridan, WY 82801</p>` +
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
		"profile-abc",
		"deals@em.discountblog.com",
		"https://trk.em.discountblog.com",
		"em.discountblog.com",
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

	// List-Unsubscribe headers
	if msg.Headers["List-Unsubscribe"] == "" {
		t.Error("missing List-Unsubscribe header")
	}
	if !strings.Contains(msg.Headers["List-Unsubscribe"], "/track/unsubscribe/") {
		t.Errorf("List-Unsubscribe should use tracking route, got: %s", msg.Headers["List-Unsubscribe"])
	}
	if msg.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("wrong List-Unsubscribe-Post: %s", msg.Headers["List-Unsubscribe-Post"])
	}

	// Physical address present (CAN-SPAM)
	if !strings.Contains(msg.HTMLContent, "James Ventures Corp") {
		t.Error("HTML missing physical address (CAN-SPAM violation)")
	}
	if !strings.Contains(msg.HTMLContent, "Sheridan, WY") {
		t.Error("HTML missing city/state in physical address")
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
	}

	mock.ExpectQuery(`SELECT COALESCE\(html_content,''\) FROM mailing_offer_creatives`).
		WithArgs("bad-creative", "offer-1").
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow(""))

	ts := mailing.NewTemplateService()
	result := h.sendOneProof(
		context.Background(), ts,
		"offer-1", "profile-1", "from@example.com", "https://trk.example.com",
		"em.example.com", "test@gmail.com", "gmail",
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
		"offer-1", "profile-1", "from@example.com", "https://trk.example.com",
		"em.example.com", "test@gmail.com", "gmail",
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

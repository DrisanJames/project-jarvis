package worker

import (
	"sort"
	"strings"
	"testing"
)

// These tests pin the 2026-07-21 fix: SESSender (vendor_type='ses' profile
// sends — the SES direct-API path used by partner drips and any ses-vendor
// profile) previously built SES v2 "Simple" content WITHOUT mapping
// msg.Headers, so the RFC 8058 List-Unsubscribe / List-Unsubscribe-Post pair
// constructed by processItem was silently dropped at the API boundary even
// after the 2026-07-14 header fix. buildSESEmailInput must carry every custom
// header into Content.Simple.Headers (except X-SES-* SMTP-interface
// directives, which the API rejects as content headers).

func TestBuildSESEmailInput_CarriesListUnsubscribeHeaders(t *testing.T) {
	headers := make(map[string]string)
	BuildListUnsubscribeHeaders(
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"quizfiesta.com",
		"news@em.quizfiesta.com",
		"https://trk.em.quizfiesta.com",
		"test-secret",
		headers,
	)
	headers["X-Job"] = "22222222-2222-2222-2222-222222222222"
	headers["X-SES-CONFIGURATION-SET"] = "cfg-set" // SMTP-interface directive: must NOT pass through
	headers["X-SES-MESSAGE-TAGS"] = "a=b"          // SMTP-interface directive: must NOT pass through

	msg := &EmailMessage{
		ID:           "44444444-4444-4444-4444-444444444444",
		CampaignID:   "22222222-2222-2222-2222-222222222222",
		SubscriberID: "33333333-3333-3333-3333-333333333333",
		Email:        "human@gmail.com",
		FromName:     "Quiz Fiesta",
		FromEmail:    "news@em.quizfiesta.com",
		Subject:      "Hello",
		HTMLContent:  "<html><body>hi</body></html>",
		TextContent:  "hi",
		ReplyTo:      "reply@em.quizfiesta.com",
		RecipientISP: "gmail",
		Headers:      headers,
	}

	input := buildSESEmailInput(msg)

	got := map[string]string{}
	var names []string
	for _, h := range input.Content.Simple.Headers {
		got[*h.Name] = *h.Value
		names = append(names, *h.Name)
	}

	lu, ok := got["List-Unsubscribe"]
	if !ok || lu == "" {
		t.Fatalf("SES Simple content missing List-Unsubscribe header (the exact 2026-07-21 regression); got headers=%v", got)
	}
	if !strings.Contains(lu, "<mailto:unsub+") {
		t.Errorf("List-Unsubscribe missing mailto leg: %s", lu)
	}
	if !strings.Contains(lu, "https://trk.em.quizfiesta.com/track/unsubscribe/") {
		t.Errorf("List-Unsubscribe missing brand https one-click leg: %s", lu)
	}
	if got["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q, want %q", got["List-Unsubscribe-Post"], "List-Unsubscribe=One-Click")
	}
	if got["X-Job"] == "" {
		t.Error("custom X-Job header dropped")
	}
	for name := range got {
		if strings.HasPrefix(strings.ToUpper(name), "X-SES-") {
			t.Errorf("SMTP-interface directive %q leaked into SES API content headers", name)
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("header order not deterministic (sorted): %v", names)
	}

	// Existing behavior preserved.
	if *input.FromEmailAddress != "Quiz Fiesta <news@em.quizfiesta.com>" {
		t.Errorf("FromEmailAddress changed: %s", *input.FromEmailAddress)
	}
	if input.Content.Simple.Body.Text == nil || *input.Content.Simple.Body.Text.Data != "hi" {
		t.Error("text body not preserved")
	}
	if len(input.ReplyToAddresses) != 1 || input.ReplyToAddresses[0] != "reply@em.quizfiesta.com" {
		t.Errorf("reply-to not preserved: %v", input.ReplyToAddresses)
	}
}

func TestBuildSESEmailInput_NoHeaders(t *testing.T) {
	msg := &EmailMessage{
		Email:       "human@gmail.com",
		FromName:    "QF",
		FromEmail:   "news@em.quizfiesta.com",
		Subject:     "Hello",
		HTMLContent: "<html></html>",
	}
	input := buildSESEmailInput(msg)
	if input.Content.Simple.Headers != nil {
		t.Errorf("expected no Simple.Headers for empty msg.Headers, got %v", input.Content.Simple.Headers)
	}
}

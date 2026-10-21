package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// newComposeTestPool builds a SendWorkerPool whose per-profile lookups are
// pre-seeded in the caches, so composeMessage runs the full transform chain
// with zero DB round-trips (sqlmock will fail the test on any unexpected
// query — that is the point: composeMessage must not reach the DB when the
// caches are warm and no offer overrides are set).
func newComposeTestPool(t *testing.T, viaSES bool) (*SendWorkerPool, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	p := &SendWorkerPool{
		db:                         db,
		orgID:                      "00000000-0000-0000-0000-000000000001",
		trackingSecret:             "compose-test-secret",
		trackingURL:                "https://t.projectjarvis.io",
		profileTrackingDomainCache: map[string]string{"p1": "https://t.m.discountblog.com"},
		profileImageHostCache:      map[string]string{"p1": ""},
		profileSESCache:            map[string]profileSESInfo{"p1": {ViaSES: viaSES, ConfigSet: "discountblog", TenantName: "discountblog"}},
		profileSESCacheAt:          map[string]time.Time{"p1": time.Now()},
		subjectZWCache:             map[string]subjectZWConfig{"p1": {}},
	}
	return p, mock
}

func composeTestItem(html string) QueueItem {
	return QueueItem{
		ID:           uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		CampaignID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		SubscriberID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Email:        "someone@gmail.com",
		Subject:      "Hello {{ first_name }}",
		HTMLContent:  html,
		FromName:     "Jamie @ Discount Blog",
		FromEmail:    "hello@em.discountblog.com",
		ReplyTo:      "reply@em.discountblog.com",
		ProfileID:    "p1",
		ESPType:      "pmta",
		FirstName:    "Drisan",
		BrandRoot:    "discountblog.com",
		CampaignName: "test cell",
	}
}

const composeTestHTML = `<html><body><p>Hi {{ first_name }}</p>
<a href="https://www.codefortwo.com/offer?x=1">See the offer</a>
<img src="https://img.projectjarvis.io/a.png"></body></html>`

// TestComposeMessage_SESPath_FullChain is the regression guard for the
// 2026-10-21 extraction of processItem's render block into composeMessage:
// every transform the worker applied before the move must still be visible
// on the composed message, on the SES-relay path (the one most of the estate
// uses).
func TestComposeMessage_SESPath_FullChain(t *testing.T) {
	p, mock := newComposeTestPool(t, true)
	msg, cf := p.composeMessage(context.Background(), composeTestItem(composeTestHTML))
	if cf != nil {
		t.Fatalf("unexpected compose failure: %s: %v", cf.field, cf.err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("composeMessage hit the DB with warm caches: %v", err)
	}

	// Liquid rendered, no raw tokens anywhere.
	if msg.Subject != "Hello Drisan" {
		t.Errorf("subject = %q, want rendered", msg.Subject)
	}
	for _, s := range []string{msg.Subject, msg.HTMLContent, msg.TextContent} {
		if strings.Contains(s, "{{") || strings.Contains(s, "{%") {
			t.Errorf("unresolved Liquid token survived: %q", s)
		}
	}
	// Click wrapping: the money href is replaced by a signed /track/click URL.
	if strings.Contains(msg.HTMLContent, `href="https://www.codefortwo.com`) {
		t.Error("money link was not click-wrapped")
	}
	if !strings.Contains(msg.HTMLContent, "https://t.m.discountblog.com/track/click/") {
		t.Error("no /track/click/ link on the profile tracking host")
	}
	// Open pixel on the SES path (InjectOpenPixel via applySESTracking).
	if !strings.Contains(msg.HTMLContent, "/track/open/") {
		t.Error("open pixel missing on SES path")
	}
	// Generated text part carries the disclosure footer.
	if strings.TrimSpace(msg.TextContent) == "" {
		t.Fatal("text part was not generated from HTML")
	}
	if !strings.Contains(msg.TextContent, "This email was intended for someone@gmail.com") {
		t.Errorf("text part lacks the intended-for footer: %q", msg.TextContent)
	}
	if !strings.Contains(msg.HTMLContent, "This email was intended for") {
		t.Error("HTML lacks the intended-for footer")
	}
	// Headers the worker always sets.
	for _, h := range []string{"List-Unsubscribe", "List-Unsubscribe-Post", "X-Job", "Feedback-ID", "X-SES-CONFIGURATION-SET", "X-SES-TENANT", "X-SES-MESSAGE-TAGS"} {
		if msg.Headers[h] == "" {
			t.Errorf("header %s missing; headers=%v", h, msg.Headers)
		}
	}
	if msg.Headers["X-Job"] != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("X-Job = %q", msg.Headers["X-Job"])
	}
	if msg.Headers["X-SES-CONFIGURATION-SET"] != "discountblog" {
		t.Errorf("X-SES-CONFIGURATION-SET = %q", msg.Headers["X-SES-CONFIGURATION-SET"])
	}
	if msg.ID != "11111111-1111-1111-1111-111111111111" || msg.RecipientISP != "gmail" {
		t.Errorf("identity fields: id=%s isp=%s", msg.ID, msg.RecipientISP)
	}
}

// TestComposeMessage_DedicatedPath_NoSESHeaders: via_ses=false must not stamp
// SES routing headers and must still wrap + pixel (InjectTrackingPixelAndLinks).
func TestComposeMessage_DedicatedPath_NoSESHeaders(t *testing.T) {
	p, _ := newComposeTestPool(t, false)
	msg, cf := p.composeMessage(context.Background(), composeTestItem(composeTestHTML))
	if cf != nil {
		t.Fatalf("compose failure: %v", cf.err)
	}
	for _, h := range []string{"X-SES-CONFIGURATION-SET", "X-SES-TENANT", "X-SES-MESSAGE-TAGS"} {
		if _, ok := msg.Headers[h]; ok {
			t.Errorf("dedicated path must not set %s", h)
		}
	}
	if !strings.Contains(msg.HTMLContent, "/track/click/") || !strings.Contains(msg.HTMLContent, "/track/open/") {
		t.Error("dedicated path lost tracking injection")
	}
}

// TestComposeMessage_RenderFailureIsReported: an unrenderable body is a
// compose failure naming the field, never a message (the 2026-08-10
// raw-Liquid incident path, now surfaced through the extracted function).
func TestComposeMessage_RenderFailureIsReported(t *testing.T) {
	p, _ := newComposeTestPool(t, true)
	msg, cf := p.composeMessage(context.Background(), composeTestItem(rawLiquidCreative))
	if cf == nil {
		t.Fatalf("expected a compose failure, got message with html len %d", len(msg.HTMLContent))
	}
	if cf.field != "html_content" {
		t.Errorf("failure field = %q, want html_content", cf.field)
	}
}

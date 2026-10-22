package worker

import (
	"strings"
	"testing"
	"time"
)

// TestBuildRFC822_Golden pins the wire format the PMTA/Kumo bridge injects so
// the render-only Inbox Score path (which calls the same function with pinned
// ids) is provably the same bytes the sender produces. Custom headers are
// emitted in sorted order — the one deliberate change from the inline builder
// this replaced (Go map order was random; harmless on the wire, but it made a
// message unreproducible).
func TestBuildRFC822_Golden(t *testing.T) {
	prev := rfc822Now
	rfc822Now = func() time.Time { return time.Date(2026, 10, 22, 17, 30, 0, 0, time.UTC) }
	defer func() { rfc822Now = prev }()
	msg := &EmailMessage{
		ID:           "11111111-1111-1111-1111-111111111111",
		CampaignID:   "22222222-2222-2222-2222-222222222222",
		SubscriberID: "33333333-3333-3333-3333-333333333333",
		Email:        "someone@gmail.com",
		FromName:     "Jamie @ Discount Blog",
		FromEmail:    "hello@em.discountblog.com",
		ReplyTo:      "reply@em.discountblog.com",
		Subject:      "A plain subject",
		HTMLContent:  "<html><body><p>Hi</p></body></html>",
		TextContent:  "Hi",
		Headers: map[string]string{
			"X-Job":                 "22222222-2222-2222-2222-222222222222",
			"Feedback-ID":           "22222222:em.discountblog.com:jarvis",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
			"List-Unsubscribe":      "<mailto:unsub@em.discountblog.com>",
		},
	}
	got := BuildRFC822(msg, "mid-1@em.discountblog.com", "=_boundary1234567")

	want := "From: " + BuildFromHeader(msg.FromName, msg.FromEmail)[len("From: "):] +
		"To: someone@gmail.com\r\n" +
		BuildSubjectHeader(msg.Subject) +
		"Message-ID: <mid-1@em.discountblog.com>\r\n" +
		"Date: Thu, 22 Oct 2026 17:30:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Reply-To: reply@em.discountblog.com\r\n" +
		"X-Campaign-ID: 22222222-2222-2222-2222-222222222222\r\n" +
		"X-Subscriber-ID: 33333333-3333-3333-3333-333333333333\r\n" +
		"X-Message-ID: 11111111-1111-1111-1111-111111111111\r\n" +
		"Feedback-ID: 22222222:em.discountblog.com:jarvis\r\n" +
		"List-Unsubscribe: <mailto:unsub@em.discountblog.com>\r\n" +
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" +
		"X-Job: 22222222-2222-2222-2222-222222222222\r\n" +
		"Content-Type: multipart/alternative; boundary=\"=_boundary1234567\"\r\n" +
		"\r\n" +
		"--=_boundary1234567\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		quotedprintableEncode("Hi") + "\r\n" +
		"--=_boundary1234567\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		quotedprintableEncode(msg.HTMLContent) + "\r\n" +
		"--=_boundary1234567--\r\n"

	if got != want {
		t.Fatalf("BuildRFC822 drifted from the golden wire format.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if got != BuildRFC822(msg, "mid-1@em.discountblog.com", "=_boundary1234567") {
		t.Fatal("BuildRFC822 is not deterministic for identical input")
	}
}

// TestBuildRFC822_NoTextPart: an empty text part is omitted (the sender never
// wrote an empty text/plain section) and the HTML part is always present.
func TestBuildRFC822_NoTextPart(t *testing.T) {
	msg := &EmailMessage{Email: "a@b.c", FromEmail: "x@y.z", Subject: "s", HTMLContent: "<p>h</p>"}
	got := BuildRFC822(msg, "m@y.z", "=_b")
	if strings.Contains(got, "text/plain") {
		t.Fatalf("empty TextContent must not emit a text/plain part:\n%s", got)
	}
	if strings.Count(got, "--=_b\r\n") != 1 || !strings.HasSuffix(got, "--=_b--\r\n") {
		t.Fatalf("unexpected multipart framing:\n%s", got)
	}
}

// TestBuildRFC822_DatePinnedByCaller: a Date supplied in msg.Headers wins and
// the builder emits exactly one Date header.
func TestBuildRFC822_DatePinnedByCaller(t *testing.T) {
	msg := &EmailMessage{Email: "a@b.c", FromEmail: "x@y.z", Subject: "s", HTMLContent: "<p>h</p>",
		Headers: map[string]string{"Date": "Mon, 01 Jan 2026 00:00:00 +0000"}}
	got := BuildRFC822(msg, "m@y.z", "=_b")
	if strings.Count(got, "\r\nDate: ") != 1 || !strings.Contains(got, "Date: Mon, 01 Jan 2026 00:00:00 +0000\r\n") {
		t.Fatalf("expected exactly the caller's Date header once:\n%s", got)
	}
}

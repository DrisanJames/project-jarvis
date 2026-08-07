package worker

import (
	"strings"
	"testing"
	"time"
)

var discTestNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

const discTestUnsub = "https://t.m.quizfiesta.com/track/unsubscribe/abc/123"

func TestInjectPartnerDisclosureHeader_AfterBodyTag(t *testing.T) {
	in := `<html><head></head><body style="margin:0;"><p>offer</p></body></html>`
	out := injectPartnerDisclosureHeader(in, "Quiz Fiesta")

	if !strings.Contains(out, partnerDisclosureMarker) {
		t.Fatal("marker not injected")
	}
	if !strings.Contains(out, "Partner offer sent by <span") || !strings.Contains(out, "Quiz Fiesta</span>. You subscribed to Quiz Fiesta partner promotions.") {
		t.Fatalf("approved copy missing: %s", out)
	}
	bodyEnd := strings.Index(out, `<body style="margin:0;">`) + len(`<body style="margin:0;">`)
	if !strings.HasPrefix(out[bodyEnd:], partnerDisclosureMarker) {
		t.Fatal("header not immediately after <body> tag")
	}
}

func TestInjectPartnerDisclosureHeader_NoBodyTagPrepends(t *testing.T) {
	out := injectPartnerDisclosureHeader(`<div>bare fragment</div>`, "Quiz Fiesta")
	if !strings.HasPrefix(out, partnerDisclosureMarker) {
		t.Fatal("expected prepend when creative has no <body>")
	}
}

func TestInjectPartnerDisclosureHeader_Idempotent(t *testing.T) {
	once := injectPartnerDisclosureHeader(`<body><p>x</p></body>`, "Quiz Fiesta")
	twice := injectPartnerDisclosureHeader(once, "Quiz Fiesta")
	if once != twice {
		t.Fatal("second injection modified content")
	}
}

func TestInjectPartnerDisclosureHeader_SkipsLegacyBakedBlock(t *testing.T) {
	// s50s-era creatives carry a hand-pasted block, sometimes with raw tokens.
	legacy := `<body><div><strong>Partner offer sent by {{ brand.name }}.</strong></div><p>offer</p></body>`
	out := injectPartnerDisclosureHeader(legacy, "Quiz Fiesta")
	if out != legacy {
		t.Fatal("must not double-disclose on creatives with a legacy baked block")
	}
}

func TestInjectPartnerDisclosureHeader_EmptyLabelSkips(t *testing.T) {
	in := `<body><p>x</p></body>`
	if out := injectPartnerDisclosureHeader(in, ""); out != in {
		t.Fatal("empty brand label must skip injection, not render an empty sender line")
	}
}

func TestInjectPartnerDisclosureHeader_EscapesLabel(t *testing.T) {
	out := injectPartnerDisclosureHeader(`<body></body>`, `A<b>&Co`)
	if strings.Contains(out, "A<b>&Co") {
		t.Fatal("brand label not HTML-escaped")
	}
	if !strings.Contains(out, "A&lt;b&gt;&amp;Co") {
		t.Fatal("escaped label missing")
	}
}

func TestInjectIntendedForFooter_BeforeBodyClose(t *testing.T) {
	in := `<html><body><p>offer</p></body></html>`
	out := injectIntendedForFooter(in, "user@example.com", discTestUnsub, discTestNow)

	if !strings.Contains(out, intendedForMarker) {
		t.Fatal("marker not injected")
	}
	if !strings.Contains(out, "This email was intended for user@example.com, August 7, 2026") {
		t.Fatalf("intended-for line wrong: %s", out)
	}
	if !strings.Contains(out, `<a href="`+discTestUnsub+`"`) || !strings.Contains(out, ">unsubscribe</a>") {
		t.Fatal("unsubscribe link missing")
	}
	if idx, bodyIdx := strings.Index(out, intendedForMarker), strings.Index(out, "</body>"); idx > bodyIdx {
		t.Fatal("footer must land before </body>")
	}
}

func TestInjectIntendedForFooter_NoBodyAppends(t *testing.T) {
	out := injectIntendedForFooter(`<div>fragment</div>`, "user@example.com", discTestUnsub, discTestNow)
	if !strings.HasSuffix(out, "</div>") == false && !strings.Contains(out, intendedForMarker) {
		t.Fatal("footer not appended")
	}
	if !strings.HasPrefix(out, `<div>fragment</div>`+intendedForMarker) {
		t.Fatal("expected append when creative has no </body>")
	}
}

func TestInjectIntendedForFooter_Idempotent(t *testing.T) {
	once := injectIntendedForFooter(`<body><p>x</p></body>`, "user@example.com", discTestUnsub, discTestNow)
	twice := injectIntendedForFooter(once, "user@example.com", discTestUnsub, discTestNow)
	if once != twice {
		t.Fatal("second injection modified content")
	}
}

func TestInjectIntendedForFooter_MissingInputsSkip(t *testing.T) {
	in := `<body><p>x</p></body>`
	if out := injectIntendedForFooter(in, "", discTestUnsub, discTestNow); out != in {
		t.Fatal("empty email must skip")
	}
	if out := injectIntendedForFooter(in, "user@example.com", "", discTestNow); out != in {
		t.Fatal("empty unsub URL must skip — a footer without a working unsubscribe is worse than the CAN-SPAM fallback")
	}
}

func TestPartnerDisclosure_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_PARTNER_DISCLOSURE", "true")
	in := `<body><p>x</p></body>`
	if out := injectPartnerDisclosureHeader(in, "Quiz Fiesta"); out != in {
		t.Fatal("kill switch must suppress header injection")
	}
	if out := injectIntendedForFooter(in, "user@example.com", discTestUnsub, discTestNow); out != in {
		t.Fatal("kill switch must suppress footer injection")
	}
	if h, f := disclosureTextParts("Quiz Fiesta", "user@example.com", discTestUnsub, discTestNow); h != "" || f != "" {
		t.Fatal("kill switch must suppress text parts")
	}
}

func TestDisclosureTextParts(t *testing.T) {
	h, f := disclosureTextParts("Quiz Fiesta", "user@example.com", discTestUnsub, discTestNow)
	if h != "Partner offer sent by Quiz Fiesta. You subscribed to Quiz Fiesta partner promotions." {
		t.Fatalf("header text wrong: %q", h)
	}
	if f != "This email was intended for user@example.com, August 7, 2026. Unsubscribe: "+discTestUnsub {
		t.Fatalf("footer text wrong: %q", f)
	}
}

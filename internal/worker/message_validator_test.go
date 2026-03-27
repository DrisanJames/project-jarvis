package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRenderedMessage_CleanMessage(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Welcome to Discount Blog!",
		HTMLContent: `<html><body><p>Hello</p><a href="https://trk.em.discountblog.com/track/unsubscribe/abc/sig">Unsubscribe</a></body></html>`,
		TextContent: "Hello\n\nThis is a welcome email with enough content to pass the trivial check threshold for text plain.",
		Headers: map[string]string{
			"List-Unsubscribe":      "<mailto:unsub@discountblog.com>, <https://trk.em.discountblog.com/track/unsubscribe/abc/sig>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	assert.Empty(t, issues, "clean message should have no issues")
}

func TestValidateRenderedMessage_UnresolvedTokenInSubject(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Hello {{ first_name | default: 'there' }}",
		HTMLContent: `<html><body><a href="https://example.com/track/unsubscribe/x/y">Unsub</a></body></html>`,
		TextContent: "Text content that is long enough to not be trivial for the validator.",
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://example.com>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	// first_name is an approved platform token, should not flag
	hasUnresolvedSubject := false
	for _, i := range issues {
		if i.Code == "unresolved_token_subject" {
			hasUnresolvedSubject = true
		}
	}
	assert.False(t, hasUnresolvedSubject, "approved platform tokens should not flag")
}

func TestValidateRenderedMessage_SiteTokenLeakage(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Deals for you",
		HTMLContent: `<html><body>{{SUBJECT}} - {{PREVIEW_TEXT}}<a href="https://example.com/track/unsubscribe/x/y">Unsub</a></body></html>`,
		TextContent: "Text content that is long enough to not be trivial for the validator.",
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://example.com>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	siteLeaks := 0
	for _, i := range issues {
		if i.Code == "site_token_leakage" {
			siteLeaks++
		}
	}
	assert.Equal(t, 2, siteLeaks, "should detect both {{SUBJECT}} and {{PREVIEW_TEXT}}")
	assert.True(t, HasBlockingIssues(issues), "site token leakage is error severity")
}

func TestValidateRenderedMessage_EmptyHref(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Hello",
		HTMLContent: `<a href="">Click here</a><a href="https://example.com/track/unsubscribe/x/y">Unsub</a>`,
		TextContent: "Enough text content to pass the trivial text/plain validator threshold.",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	found := false
	for _, i := range issues {
		if i.Code == "empty_href" {
			found = true
		}
	}
	assert.True(t, found, "should detect empty href")
}

func TestValidateRenderedMessage_MissingUnsubControl(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Promo email",
		HTMLContent: `<html><body><p>Buy stuff!</p></body></html>`,
		TextContent: "Enough text content to pass the trivial text/plain validator threshold.",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	found := false
	for _, i := range issues {
		if i.Code == "missing_unsub_control" {
			found = true
		}
	}
	assert.True(t, found, "should detect missing unsubscribe control for promotional message")
}

func TestValidateRenderedMessage_TransactionalSkipsUnsubCheck(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Your receipt",
		HTMLContent: `<html><body><p>Your order has shipped.</p></body></html>`,
		TextContent: "Your order has shipped. Thank you for your purchase.",
		Headers:     map[string]string{},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "transactional")
	for _, i := range issues {
		assert.NotEqual(t, "missing_unsub_control", i.Code, "transactional should not require unsub")
		assert.NotEqual(t, "missing_list_unsubscribe", i.Code)
		assert.NotEqual(t, "missing_list_unsubscribe_post", i.Code)
	}
}

func TestValidateRenderedMessage_PreheaderFillerGlyphs(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Sale",
		HTMLContent: `<div style="display:none;max-height:0px;">Preview text &#847;&#847;&#847;</div><a href="https://x.com/track/unsubscribe/a/b">Unsub</a>`,
		TextContent: "Enough text content to pass the trivial text/plain validator threshold.",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	found := false
	for _, i := range issues {
		if i.Code == "preheader_filler_glyphs" {
			found = true
		}
	}
	assert.True(t, found, "should detect filler glyphs in hidden preheader")
}

func TestValidateRenderedMessage_TrivialTextPlain(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Hello",
		HTMLContent: `<a href="https://x.com/track/unsubscribe/a/b">Unsub</a>`,
		TextContent: "Hi",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	found := false
	for _, i := range issues {
		if i.Code == "trivial_text_plain" {
			found = true
		}
	}
	assert.True(t, found, "should detect trivially short text/plain")
}

func TestValidateRenderedMessage_EmptyTextPlain(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Hello",
		HTMLContent: `<a href="https://x.com/track/unsubscribe/a/b">Unsub</a>`,
		TextContent: "",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")
	found := false
	for _, i := range issues {
		if i.Code == "missing_text_plain" {
			found = true
		}
	}
	assert.True(t, found, "should detect missing text/plain")
}

func TestValidateRenderedMessage_EnforceModeBlocks(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Hello",
		HTMLContent: `{{SUBJECT}} leaked<a href="https://x.com/track/unsubscribe/a/b">Unsub</a>`,
		TextContent: "Enough text content to pass the trivial text/plain validator threshold.",
		Headers:     map[string]string{"List-Unsubscribe": "<https://x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}

	issues := ValidateRenderedMessage(msg, ValidatorEnforce, "promotional")
	require.True(t, HasBlockingIssues(issues))
}

func TestHasBlockingIssues(t *testing.T) {
	assert.False(t, HasBlockingIssues(nil))
	assert.False(t, HasBlockingIssues([]ValidationIssue{
		{Severity: SeverityWarning},
	}))
	assert.True(t, HasBlockingIssues([]ValidationIssue{
		{Severity: SeverityWarning},
		{Severity: SeverityError},
	}))
}

func TestIsApprovedPlatformToken(t *testing.T) {
	tests := []struct {
		token    string
		approved bool
	}{
		{"{{ first_name }}", true},
		{"{{ first_name | default: \"there\" }}", true},
		{"{{ system.unsubscribe_url }}", true},
		{"{{ system.current_year }}", true},
		{"{{ campaign.name }}", true},
		{"{{ email }}", true},
		{"{{ year }}", true},
		{"{{ unknown_var }}", false},
		{"{{ custom_thing }}", false},
		{"{{SUBJECT}}", false},
	}

	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			assert.Equal(t, tc.approved, isApprovedPlatformToken(tc.token))
		})
	}
}

func TestValidateTemplateContent_SiteTokens(t *testing.T) {
	issues := ValidateTemplateContent(
		`<html><body>{{SUBJECT}} {{PREVIEW_TEXT}}<a href="{{ system.unsubscribe_url }}">Unsub</a></body></html>`,
		"text",
		"Hello {{FIRST_NAME}}",
	)

	errorCount := 0
	for _, i := range issues {
		if i.Severity == SeverityError {
			errorCount++
		}
	}
	assert.Equal(t, 3, errorCount, "should flag SUBJECT, PREVIEW_TEXT in HTML and FIRST_NAME in subject")
}

func TestValidateTemplateContent_CleanTemplate(t *testing.T) {
	issues := ValidateTemplateContent(
		`<html><body><p>Hello {{ first_name | default: "there" }}</p><a href="{{ system.unsubscribe_url }}">Unsubscribe</a></body></html>`,
		"Hello, here is your newsletter.",
		"Welcome to Our Newsletter",
	)

	assert.Empty(t, issues, "clean template should pass validation")
}

func TestValidateTemplateContent_MissingUnsub(t *testing.T) {
	issues := ValidateTemplateContent(
		`<html><body><p>Hello</p></body></html>`,
		"text",
		"Subject",
	)

	found := false
	for _, i := range issues {
		if i.Code == "missing_unsub_in_template" {
			found = true
		}
	}
	assert.True(t, found, "should warn about missing unsubscribe control")
}

func TestValidateTemplateContent_PreheaderFiller(t *testing.T) {
	issues := ValidateTemplateContent(
		`<html><body><div style="display:none;">Preview &#847;&#847;</div><a href="{{ system.unsubscribe_url }}">Unsub</a></body></html>`,
		"text",
		"Subject",
	)

	found := false
	for _, i := range issues {
		if i.Code == "preheader_filler_in_template" {
			found = true
		}
	}
	assert.True(t, found, "should detect filler glyphs in template preheader")
}

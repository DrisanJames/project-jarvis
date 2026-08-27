package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMIMEEncodeHeader_ASCIIPassthrough(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", ""},
		{"plain ascii", "Hello World", "Hello World"},
		{"ascii with punctuation", "Sale: 50% Off!", "Sale: 50% Off!"},
		{"email subject", "Your order has shipped", "Your order has shipped"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MIMEEncodeHeader(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMIMEEncodeHeader_NonASCII(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"japanese", "こんにちは"},
		{"emoji", "🎉 Big Sale"},
		{"accented", "Café Newsletter"},
		{"mixed", "Héllo Wörld"},
		{"chinese", "优惠信息"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MIMEEncodeHeader(tc.input)
			assert.True(t, strings.HasPrefix(got, "=?UTF-8?q?") || strings.HasPrefix(got, "=?utf-8?q?"),
				"expected RFC 2047 encoding, got: %s", got)
			assert.True(t, strings.HasSuffix(got, "?="),
				"expected RFC 2047 closing, got: %s", got)
		})
	}
}

func TestBuildFromHeader(t *testing.T) {
	tests := []struct {
		name           string
		displayName    string
		email          string
		wantContain    string
		wantNotContain string
	}{
		{
			name:        "plain ascii name",
			displayName: "Jamie",
			email:       "hello@discountblog.com",
			wantContain: "From: Jamie <hello@discountblog.com>\r\n",
		},
		{
			name:        "empty display name",
			displayName: "",
			email:       "hello@discountblog.com",
			wantContain: "From: <hello@discountblog.com>\r\n",
		},
		{
			name:        "non-ascii display name gets encoded",
			displayName: "Café Blog",
			email:       "hello@cafe.com",
			wantContain: "=?UTF-8?q?",
		},
		{
			name:           "header injection stripped",
			displayName:    "Evil\r\nBcc: victim@evil.com",
			email:          "hello@example.com",
			wantNotContain: "\r\nBcc:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildFromHeader(tc.displayName, tc.email)
			assert.True(t, strings.HasPrefix(got, "From: "),
				"expected From: prefix, got: %s", got)
			assert.True(t, strings.HasSuffix(got, "\r\n"),
				"expected CRLF suffix, got: %s", got)
			if tc.wantContain != "" {
				assert.Contains(t, got, tc.wantContain)
			}
			if tc.wantNotContain != "" {
				assert.NotContains(t, got, tc.wantNotContain)
			}
		})
	}
}

func TestBuildSubjectHeader(t *testing.T) {
	tests := []struct {
		name      string
		subject   string
		wantExact string
	}{
		{
			name:      "plain ascii",
			subject:   "Your weekly digest",
			wantExact: "Subject: Your weekly digest\r\n",
		},
		{
			name:    "non-ascii encodes",
			subject: "Héllo",
		},
		{
			name:      "empty subject",
			subject:   "",
			wantExact: "Subject: \r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildSubjectHeader(tc.subject)
			assert.True(t, strings.HasPrefix(got, "Subject: "))
			assert.True(t, strings.HasSuffix(got, "\r\n"))
			if tc.wantExact != "" {
				assert.Equal(t, tc.wantExact, got)
			}
			if !isASCII(tc.subject) && tc.subject != "" {
				assert.Contains(t, got, "=?UTF-8?q?")
			}
		})
	}
}

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"normal text", "normal text"},
		{"with\r\nnewlines", "withnewlines"},
		{"with\rCR", "withCR"},
		{"with\nLF", "withLF"},
		{"", ""},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, sanitizeHeaderValue(tc.input))
		})
	}
}

func TestIsASCII(t *testing.T) {
	assert.True(t, isASCII("hello"))
	assert.True(t, isASCII(""))
	assert.True(t, isASCII("Sale: 50% off!"))
	assert.False(t, isASCII("café"))
	assert.False(t, isASCII("こんにちは"))
	assert.False(t, isASCII("🎉"))
}

// TestBuildFromHeader_QuotesSpecials guards the 2026-07-01 six-brand incident:
// display names containing RFC 5322 specials (like '@') must be emitted as a
// quoted-string, or KumoMTA treats the From header as unparseable/missing and
// rejects the message at inject with HTTP 200 + success_count=0.
func TestBuildFromHeader_QuotesSpecials(t *testing.T) {
	tests := []struct {
		name  string
		disp  string
		email string
		want  string
	}{
		{"at-sign brand name", "Grace @ Best Credit Care", "hello@em.bestcreditcare.com",
			"From: \"Grace @ Best Credit Care\" <hello@em.bestcreditcare.com>\r\n"},
		{"plain name unquoted", "My Personal Financial", "hello@em.mypersonalfinancial.com",
			"From: My Personal Financial <hello@em.mypersonalfinancial.com>\r\n"},
		{"apostrophe is atext, unquoted", "Sam's Club Partner", "hello@x.com",
			"From: Sam's Club Partner <hello@x.com>\r\n"},
		{"dot needs quoting", "Dr. Smith", "doc@x.com",
			"From: \"Dr. Smith\" <doc@x.com>\r\n"},
		{"embedded quote escaped", `The "Best" Deals`, "d@x.com",
			"From: \"The \\\"Best\\\" Deals\" <d@x.com>\r\n"},
		{"empty name", "", "bare@x.com", "From: <bare@x.com>\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, BuildFromHeader(tc.disp, tc.email))
		})
	}
}

// Non-ASCII names keep the RFC 2047 encoded-word path (encoded-words are
// atext-safe and must NOT be additionally quoted).
func TestBuildFromHeader_NonASCIIStillEncoded(t *testing.T) {
	got := BuildFromHeader("Ariana Cañas @ Deals", "a@x.com")
	assert.True(t, strings.HasPrefix(got, "From: =?UTF-8?q?"), got)
	assert.False(t, strings.Contains(got, `"`), "encoded-word must not be quoted: %s", got)
}

package worker

import (
	"fmt"
	"mime"
	"net/textproto"
	"strings"
)

// MIMEEncodeHeader applies RFC 2047 Q-encoding to a header value if it
// contains non-ASCII characters. Pure ASCII values pass through unmodified.
func MIMEEncodeHeader(value string) string {
	if value == "" {
		return ""
	}
	if isASCII(value) {
		return value
	}
	return mime.QEncoding.Encode("UTF-8", value)
}

// BuildFromHeader constructs a safe RFC 5322 From header line.
// The display name is RFC 2047 encoded if non-ASCII; the email address is
// always passed through as-is. Header injection characters are rejected.
func BuildFromHeader(displayName, email string) string {
	displayName = sanitizeHeaderValue(displayName)
	email = sanitizeHeaderValue(email)

	if displayName == "" {
		return fmt.Sprintf("From: <%s>\r\n", email)
	}
	encoded := MIMEEncodeHeader(displayName)
	return fmt.Sprintf("From: %s <%s>\r\n", encoded, email)
}

// BuildSubjectHeader constructs a safe RFC 5322 Subject header line.
// The subject is RFC 2047 encoded if it contains non-ASCII characters.
func BuildSubjectHeader(subject string) string {
	subject = sanitizeHeaderValue(subject)
	encoded := MIMEEncodeHeader(subject)
	return fmt.Sprintf("Subject: %s\r\n", encoded)
}

// BuildAddressHeader constructs a generic address header (Reply-To, To, etc.).
func BuildAddressHeader(name string, value string) string {
	name = textproto.CanonicalMIMEHeaderKey(name)
	value = sanitizeHeaderValue(value)
	return fmt.Sprintf("%s: %s\r\n", name, value)
}

// isASCII returns true if every byte in s is in the ASCII range (0-127).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// sanitizeHeaderValue strips CR and LF to prevent header injection attacks.
func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

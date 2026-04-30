package tracking

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// hashEmail produces the canonical email_hash used as the UNIQUE key on
// mailing_inbox_profiles. It MUST match the spec used by every other writer
// of that table — see emailHash in upside-down/internal/api/mailing_tracking.go
// and ingestEmailHash in upside-down/internal/engine/ingest.go.
//
// Spec: hex-encoded SHA-256 of strings.ToLower(strings.TrimSpace(email)).
// Drift between this and the other definitions will cause duplicate profile
// rows for the same recipient.
func hashEmail(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", h)
}

// domainFromEmail returns the lowercase recipient domain. Caller is expected
// to pass an already-lowercased + trimmed email; this is just the @-split.
func domainFromEmail(emailLower string) string {
	if i := strings.LastIndex(emailLower, "@"); i >= 0 {
		return emailLower[i+1:]
	}
	return ""
}

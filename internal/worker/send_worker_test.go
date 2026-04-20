package worker

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestGenerateUnsubscribeURL_Is3Part verifies the global URL shape that
// HandleTrackUnsubscribe treats as "suppress globally, flip subscriber status".
// The path must be /track/unsubscribe/<base64>/<sig> where the decoded token
// is exactly "org|campaign|subscriber" — three pipe-delimited fields.
func TestGenerateUnsubscribeURL_Is3Part(t *testing.T) {
	const (
		org        = "00000000-0000-0000-0000-000000000001"
		campaign   = "11111111-1111-1111-1111-111111111111"
		subscriber = "22222222-2222-2222-2222-222222222222"
		baseURL    = "https://trk.example.com"
		secret     = "unit-test-secret"
	)

	url := GenerateUnsubscribeURL(org, campaign, subscriber, baseURL, secret)

	if !strings.HasPrefix(url, baseURL+"/track/unsubscribe/") {
		t.Fatalf("unexpected URL prefix: %s", url)
	}

	parts := strings.TrimPrefix(url, baseURL+"/track/unsubscribe/")
	segments := strings.Split(parts, "/")
	if len(segments) != 2 {
		t.Fatalf("expected <data>/<sig>, got %d segments: %v", len(segments), segments)
	}
	encoded, sig := segments[0], segments[1]
	if len(sig) != 16 {
		t.Fatalf("sig must be 16 hex chars (matches TrackSign truncation), got %d", len(sig))
	}
	if got := TrackSign(encoded, secret); got != sig {
		t.Fatalf("signature mismatch: got %s, want %s", sig, got)
	}

	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 3 {
		t.Fatalf("global token must have 3 fields, got %d: %v", len(fields), fields)
	}
	if fields[0] != org || fields[1] != campaign || fields[2] != subscriber {
		t.Fatalf("token payload mismatch: %v", fields)
	}
}

// TestGenerateBrandUnsubscribeURL_Is4Part verifies the brand-scoped URL is
// a 4-part token (org|campaign|subscriber|brandRoot). HandleTrackUnsubscribe
// branches on len(parts)==4 to route to SuppressScoped instead of Suppress,
// so a regression that drops the brandRoot would silently convert every
// brand-scoped click into a global unsubscribe — a user-visible disaster.
func TestGenerateBrandUnsubscribeURL_Is4Part(t *testing.T) {
	const (
		org        = "00000000-0000-0000-0000-000000000001"
		campaign   = "11111111-1111-1111-1111-111111111111"
		subscriber = "22222222-2222-2222-2222-222222222222"
		brandRoot  = "discountblog.com"
		baseURL    = "https://trk.example.com"
		secret     = "unit-test-secret"
	)

	url := GenerateBrandUnsubscribeURL(org, campaign, subscriber, brandRoot, baseURL, secret)
	parts := strings.TrimPrefix(url, baseURL+"/track/unsubscribe/")
	segments := strings.Split(parts, "/")
	if len(segments) != 2 {
		t.Fatalf("expected <data>/<sig>, got %v", segments)
	}

	raw, err := base64.URLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 4 {
		t.Fatalf("brand token must have 4 fields, got %d: %v", len(fields), fields)
	}
	if fields[3] != brandRoot {
		t.Fatalf("brandRoot not in token: %v", fields)
	}

	// Signature must still verify — shares the same TrackSign codepath.
	if TrackSign(segments[0], secret) != segments[1] {
		t.Fatalf("brand-token signature did not round-trip")
	}
}

// TestGenerateBrandUnsubscribeURL_EmptyBrandFallsBackToGlobal guards a
// defensive property of generateUnsubURL: an empty brandRoot must NOT emit
// a 4-part token with a trailing "|" (which would decode as an empty field
// and let the handler accidentally interpret it as a brand scope). Empty
// brand must collapse to the 3-part shape.
func TestGenerateBrandUnsubscribeURL_EmptyBrandFallsBackToGlobal(t *testing.T) {
	url := GenerateBrandUnsubscribeURL("org", "camp", "sub", "", "https://trk.example.com", "s")
	encoded := strings.Split(strings.TrimPrefix(url, "https://trk.example.com/track/unsubscribe/"), "/")[0]
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fields := strings.Split(string(raw), "|"); len(fields) != 3 {
		t.Fatalf("empty brand must collapse to 3-part token, got %d fields: %v", len(fields), fields)
	}
}

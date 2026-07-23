package emailoversight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient points a client at a stub EO endpoint.
func newTestClient(url string) *Client {
	c := New("test-token", 1, 1)
	c.apiURL = url
	return c
}

// TestValidateRejectShape: EO's account-suppression responses use a DIFFERENT,
// lowercase-keyed shape with no ResultId:
//
//	{"result":"REJECT","reason":"Suppressed Email Address",...}
//
// Before the fix this decoded to ResultID=0, which callers misread as EO's
// retryable result 0 (3 attempts → dead_letter). It must surface as a
// distinct terminal outcome: ResultID=ResultIDRejected, IsReject()=true,
// Reason populated.
func TestValidateRejectShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"REJECT","reason":"Suppressed Email Address","email":"x@example.com"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Validate(context.Background(), "x@example.com")
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !resp.IsReject() {
		t.Errorf("IsReject() = false, want true (Result=%q)", resp.Result)
	}
	if resp.ResultID != ResultIDRejected {
		t.Errorf("ResultID = %d, want ResultIDRejected (%d)", resp.ResultID, ResultIDRejected)
	}
	if resp.Reason != "Suppressed Email Address" {
		t.Errorf("Reason = %q, want %q", resp.Reason, "Suppressed Email Address")
	}
	if ValidResults[resp.Result] {
		t.Errorf("REJECT must never be a valid result")
	}
}

// TestValidateNormalShape (regression): the standard PascalCase validation
// shape must be untouched by the reject handling.
func TestValidateNormalShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ListId":1,"Email":"a@example.com","ResultId":1,"Result":"Verified"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Validate(context.Background(), "a@example.com")
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if resp.IsReject() {
		t.Errorf("IsReject() = true for a Verified response")
	}
	if resp.ResultID != 1 {
		t.Errorf("ResultID = %d, want 1", resp.ResultID)
	}
	if resp.Result != ResultVerified {
		t.Errorf("Result = %q, want %q", resp.Result, ResultVerified)
	}
	if !ValidResults[resp.Result] {
		t.Errorf("Verified must be a valid result")
	}
}

// TestValidateRetryableZeroNotReject (regression): a genuine EO result 0
// (retry) in the normal shape must NOT be rewritten to ResultIDRejected.
func TestValidateRetryableZeroNotReject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ListId":1,"Email":"a@example.com","ResultId":0,"Result":"Retry"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	resp, err := c.Validate(context.Background(), "a@example.com")
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if resp.IsReject() {
		t.Errorf("IsReject() = true for Result=Retry")
	}
	if resp.ResultID != 0 {
		t.Errorf("ResultID = %d, want 0", resp.ResultID)
	}
}

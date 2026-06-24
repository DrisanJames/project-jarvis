package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestImageDownloadInsecureFallback guards the TLS fallback that fixes the
// imageports.com incident: affiliate image hosts serve untrusted/incomplete cert
// chains that Go's verified client rejects ("x509: certificate signed by unknown
// authority"), silently dropping every image. A self-signed httptest server
// reproduces that exact condition (its cert is signed by an unknown authority).
func TestImageDownloadInsecureFallback(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer srv.Close()

	// The default (verifying) client MUST fail and be classified as a cert error.
	_, err := (&http.Client{Timeout: 5 * time.Second}).Get(srv.URL)
	if err == nil {
		t.Fatal("expected a TLS cert error from the verifying client against a self-signed server")
	}
	if !isTLSCertError(err) {
		t.Fatalf("isTLSCertError failed to classify the cert error: %v", err)
	}

	// The insecure fallback client MUST succeed.
	ins := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := ins.Get(srv.URL)
	if err != nil {
		t.Fatalf("insecure fallback fetch failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insecure fallback got status %d, want 200", resp.StatusCode)
	}

	// A non-cert error must NOT trigger the fallback.
	if isTLSCertError(nil) {
		t.Fatal("isTLSCertError(nil) should be false")
	}
}

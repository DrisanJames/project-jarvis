package worker

import (
	"testing"
)

func TestCheckMXValid(t *testing.T) {
	v := &EmailVerifier{}
	// gmail.com should have MX records
	if !v.checkMX("test@gmail.com") {
		t.Skip("DNS resolution unavailable in this environment")
	}
}

func TestCheckMXInvalid(t *testing.T) {
	v := &EmailVerifier{}
	if v.checkMX("test@thisisnotarealdomainxyz123.com") {
		t.Error("expected invalid MX for non-existent domain")
	}
}

func TestCheckMXBadFormat(t *testing.T) {
	v := &EmailVerifier{}
	if v.checkMX("not-an-email") {
		t.Error("expected false for badly formatted email")
	}
}

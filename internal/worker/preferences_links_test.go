package worker

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

func TestGeneratePreferencesURL_SignedOnTrackingHost(t *testing.T) {
	now := time.Now()
	u := GeneratePreferencesURL("00000000-0000-0000-0000-000000000001", "55555555-5555-5555-5555-555555555555",
		"discountblog.com", "https://t.em.discountblog.com/", "s", now)
	const prefix = "https://t.em.discountblog.com/track/preferences?t="
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("url: %s", u)
	}
	c, err := preferences.Verify(strings.TrimPrefix(u, prefix), "s", now)
	if err != nil || c.Scope != preferences.ScopeBrand || c.BrandRoot != "discountblog.com" {
		t.Fatalf("token: %v %+v", err, c)
	}
	if got := GeneratePreferencesURL("org", "sub", "x.com", "https://t", "s", now); got != "https://t/track/preferences" {
		t.Fatalf("unmintable input must yield a token-less link, got %s", got)
	}
}

// The mailto leg is signed; the webhook splits on the last '.'.
func TestListUnsubMailtoIsSigned(t *testing.T) {
	h := map[string]string{}
	BuildListUnsubscribeHeaders("00000000-0000-0000-0000-000000000001", "22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333", "discountblog.com", "a@em.discountblog.com", "https://t.em.discountblog.com", "s", h)
	lu := h["List-Unsubscribe"]
	start := strings.Index(lu, "unsub+") + len("unsub+")
	local := lu[start:strings.Index(lu, "@em.discountblog.com")]
	dot := strings.LastIndex(local, ".")
	if dot < 0 {
		t.Fatalf("mailto token unsigned: %s", lu)
	}
	payload, sig := local[:dot], local[dot+1:]
	if sig != TrackSign(payload, "s") {
		t.Fatalf("mailto signature mismatch: %s", lu)
	}
	if _, err := base64.URLEncoding.DecodeString(payload); err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	if SignedMailtoToken("abc", "") != "abc" {
		t.Fatal("empty secret must yield the legacy unsigned form")
	}
}

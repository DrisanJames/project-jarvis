package mailing

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

func TestRecipientLink_SignedV1NotMD5(t *testing.T) {
	cb := NewContextBuilder(nil, "https://trk.em.discountblog.com/", "k")
	org := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	sub := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	link := cb.recipientLink(org, sub, "discountblog.com", preferences.ScopeAll, true)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "trk.em.discountblog.com" || u.Path != "/track/preferences" {
		t.Fatalf("link shape: %s", link)
	}
	if u.Query().Get("intent") != "unsubscribe" || u.Query().Get("sid") != "" {
		t.Fatalf("query: %s", u.RawQuery)
	}
	c, err := preferences.Verify(u.Query().Get("t"), "k", time.Now())
	if err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
	if c.SubscriberID != sub.String() || c.OrgID != org.String() || c.Scope != preferences.ScopeAll {
		t.Fatalf("claims: %+v", c)
	}

	// Brand scope without a brand root falls back to all-brands, not a dead link.
	link = cb.recipientLink(org, sub, "", preferences.ScopeBrand, false)
	if c, err := preferences.Verify(strings.SplitN(link, "t=", 2)[1], "k", time.Now()); err != nil || c.Scope != preferences.ScopeAll {
		t.Fatalf("brandless brand link: %v %+v", err, c)
	}

	// No signing key: no token, neutral landing (never an unsigned grant).
	if got := NewContextBuilder(nil, "https://x", "").recipientLink(org, sub, "x.com", preferences.ScopeBrand, false); got != "https://x/track/preferences" {
		t.Fatalf("no-key link: %s", got)
	}
}

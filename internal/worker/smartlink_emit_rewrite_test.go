package worker

import (
	"context"
	"strings"
	"testing"
)

// These tests pin the Wave-2 offer-minting hook inside RewriteClickLinks. They
// reuse the click-test constants from ses_click_wrap_test.go (same package):
// clickTestBase / clickTestOrg / clickTestSecret.

// setEmitter installs a package-level emitter for the duration of a test and
// restores the prior value (default nil) on cleanup. RewriteClickLinks reads
// the global offerHashEmitter; these tests are sequential (no t.Parallel), so
// swapping the global is safe.
func setEmitter(t *testing.T, e *SmartLinkEmitter) {
	t.Helper()
	prev := offerHashEmitter
	offerHashEmitter = e
	t.Cleanup(func() { offerHashEmitter = prev })
}

const craMoney = "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com"

func emitterFor(t *testing.T, normalizedDest, hash string) *SmartLinkEmitter {
	t.Helper()
	return newTestEmitter(t, func(context.Context) (map[string]string, error) {
		return map[string]string{normalizedDest: hash}, nil
	})
}

// (a) DISABLED emitter (nil AND flag-off) => output byte-identical to today.
func TestRewriteClickLinks_EmitterDisabled_ByteIdentical(t *testing.T) {
	html := `<html><body><a href="` + craMoney + `">CTA</a></body></html>`

	// Baseline: no emitter installed (offerHashEmitter is nil by default) —
	// this IS today's behavior.
	baseline := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	// A disabled emitter (flag off) must produce byte-identical output.
	setEmitter(t, NewSmartLinkEmitter(nil, 0, false))
	withDisabled := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)
	if withDisabled != baseline {
		t.Fatalf("disabled emitter changed output:\n baseline: %q\n disabled: %q", baseline, withDisabled)
	}

	// An ENABLED emitter that has NO hash for this offer must ALSO be
	// byte-identical (fallback to the wrap, link never dropped).
	setEmitter(t, emitterFor(t, normalizeOfferURL("https://www.cratoolpro.com/OTHER/OFFER"), "hash-other"))
	withEnabledNoHash := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)
	if withEnabledNoHash != baseline {
		t.Fatalf("enabled-but-no-hash changed output (fallback broken):\n baseline: %q\n got:      %q", baseline, withEnabledNoHash)
	}
	if !strings.Contains(baseline, "/track/click/") {
		t.Fatal("sanity: baseline should have /track/click wrapped the money link")
	}
}

// (b) ENABLED + a hash for the offer => emits exactly the /o/ tracking URL.
func TestRewriteClickLinks_EmitterEnabled_EmitsOfferURL(t *testing.T) {
	setEmitter(t, emitterFor(t, normalizeOfferURL(craMoney), "hash-xyz"))

	html := `<html><body><a href="` + craMoney + `">CTA</a></body></html>`
	out := RewriteClickLinks(html, "camp-9", "sub-7", "email-3", clickTestBase, clickTestOrg, clickTestSecret)

	// Brand-in-path (2026-07-22): clickTestBase = https://t.em.discountblog.com,
	// so the minted /o/ carries discountblog.com as the FIRST path segment.
	want := `href="` + clickTestBase + `/o/discountblog.com/sub-7/hash-xyz/camp-9"`
	if !strings.Contains(out, want) {
		t.Fatalf("expected minted offer URL %q in:\n%s", want, out)
	}
	// It must NOT also /track/click-wrap the same link.
	if strings.Contains(out, "/track/click/") {
		t.Fatalf("offer link should be minted, not click-wrapped:\n%s", out)
	}
}

// (c) cratoolpro link WITHOUT a hash => still /track/click wrapped (fallback).
func TestRewriteClickLinks_EmitterEnabled_NoHashFallsBackToWrap(t *testing.T) {
	// Emitter has some OTHER offer, not this one.
	setEmitter(t, emitterFor(t, normalizeOfferURL("https://www.cratoolpro.com/ZZ/ZZ"), "hash-zz"))

	html := `<html><body><a href="` + craMoney + `">CTA</a></body></html>`
	out := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	if strings.Contains(out, clickTestBase+"/o/") {
		t.Fatalf("no hash: link must NOT be minted to /o/:\n%s", out)
	}
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Fatalf("no hash: want exactly 1 /track/click wrap, got %d:\n%s", got, out)
	}
}

// (d) NON-cratoolpro link => /track/click wrapped as today, even when enabled.
func TestRewriteClickLinks_EmitterEnabled_NonCratoolproWrapped(t *testing.T) {
	// Seed a hash under a key that would only ever match a cratoolpro URL; the
	// example.com link must never touch it.
	setEmitter(t, emitterFor(t, normalizeOfferURL(craMoney), "hash-xyz"))

	money := "https://example.com/offer?sub1=x"
	html := `<html><body><a href="` + money + `">CTA</a></body></html>`
	out := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	if strings.Contains(out, clickTestBase+"/o/") {
		t.Fatalf("non-cratoolpro link must NOT be minted:\n%s", out)
	}
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Fatalf("want exactly 1 /track/click wrap, got %d:\n%s", got, out)
	}
}

// (c') newsletter-style html (no cratoolpro at all) => unaffected by minting.
func TestRewriteClickLinks_EmitterEnabled_NewsletterUnaffected(t *testing.T) {
	setEmitter(t, emitterFor(t, normalizeOfferURL(craMoney), "hash-xyz"))

	html := `<html><body>` +
		`<a href="https://em.discountblog.com/reviews/best-widgets">Read more</a>` +
		`<a href="https://em.discountblog.com/2026/07/article">Article</a>` +
		`</body></html>`

	baseline := func() string {
		prev := offerHashEmitter
		offerHashEmitter = nil
		defer func() { offerHashEmitter = prev }()
		return RewriteClickLinks(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)
	}()
	out := RewriteClickLinks(html, "c", "s", "e", clickTestBase, clickTestOrg, clickTestSecret)

	if out != baseline {
		t.Fatalf("newsletter (no cratoolpro) changed under enabled emitter:\n base: %q\n got:  %q", baseline, out)
	}
	if strings.Contains(out, clickTestBase+"/o/") {
		t.Fatalf("newsletter must not gain any /o/ link:\n%s", out)
	}
}

// (f) NETWORK-AGNOSTIC: a seeded money URL on ANY network mints /o/. The hook
// no longer gates on the cratoolpro host regex — the dictionary Lookup is the
// sole gate — so eos57ytf, k8k0hfdt, cratoolpro (and any future network) all
// mint when Enabled + seeded.
func TestRewriteClickLinks_EmitterEnabled_AllNetworksMint(t *testing.T) {
	nets := map[string]string{
		"cratoolpro":  "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}",
		"eos57ytf":    "https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?source_id=email&sub1={{subscriber.id}}",
		"k8k0hfdt":    "https://www.k8k0hfdt.com/3QJ6DW/3LKS16/?source_id=email&sub1={{subscriber.id}}",
		"codefortwo":  "https://www.codefortwo.com/K4C5ZLC/K9TM4Q/?source_id=email",
		"kj3rwth8trk": "https://www.kj3rwth8trk.com/AAA/BBB/?source_id=email",
		"muqes":       "https://www.muqes.com/CCC/DDD/?source_id=email",
	}
	for name, money := range nets {
		t.Run(name, func(t *testing.T) {
			setEmitter(t, emitterFor(t, normalizeOfferURL(money), "hash-"+name))

			html := `<html><body><a href="` + money + `">CTA</a></body></html>`
			out := RewriteClickLinks(html, "camp-9", "sub-7", "email-3", clickTestBase, clickTestOrg, clickTestSecret)

			want := `href="` + clickTestBase + `/o/discountblog.com/sub-7/hash-` + name + `/camp-9"`
			if !strings.Contains(out, want) {
				t.Fatalf("%s: expected minted offer URL %q in:\n%s", name, want, out)
			}
			if strings.Contains(out, "/track/click/") {
				t.Fatalf("%s: seeded money link must be minted, not click-wrapped:\n%s", name, out)
			}
		})
	}
}

// (g) A money URL on a seeded network but NOT itself seeded => /track/click
// wrap (Lookup misses on the specific destination). Proves the gate is the
// per-destination dictionary hit, not the host.
func TestRewriteClickLinks_EmitterEnabled_UnseededMoneyURLWrapped(t *testing.T) {
	// Seed ONE eos57ytf destination; request a DIFFERENT eos57ytf destination.
	seeded := "https://www.eos57ytf.com/K4C5ZLC/SEEDED/?source_id=email"
	setEmitter(t, emitterFor(t, normalizeOfferURL(seeded), "hash-seeded"))

	other := "https://www.eos57ytf.com/K4C5ZLC/NOTSEEDED/?source_id=email&sub1=x"
	html := `<html><body><a href="` + other + `">CTA</a></body></html>`
	out := RewriteClickLinks(html, "camp-1", "sub-1", "email-1", clickTestBase, clickTestOrg, clickTestSecret)

	if strings.Contains(out, clickTestBase+"/o/") {
		t.Fatalf("unseeded money URL must NOT be minted:\n%s", out)
	}
	if got := strings.Count(out, "/track/click/"); got != 1 {
		t.Fatalf("unseeded money URL: want exactly 1 /track/click wrap, got %d:\n%s", got, out)
	}
}

// (e) idempotency: an already-minted /o/ link is left untouched (existing skip).
func TestRewriteClickLinks_EmitterEnabled_AlreadyMintedUntouched(t *testing.T) {
	setEmitter(t, emitterFor(t, normalizeOfferURL(craMoney), "hash-xyz"))

	minted := clickTestBase + "/o/sub-7/hash-xyz/camp-9"
	html := `<html><body><a href="` + minted + `">CTA</a></body></html>`
	out := RewriteClickLinks(html, "camp-9", "sub-7", "email-3", clickTestBase, clickTestOrg, clickTestSecret)

	if !strings.Contains(out, `href="`+minted+`"`) {
		t.Fatalf("already-minted /o/ link must be left untouched:\n%s", out)
	}
	if strings.Contains(out, "/track/click/") {
		t.Fatalf("already-minted /o/ link must NOT be click-wrapped:\n%s", out)
	}
}

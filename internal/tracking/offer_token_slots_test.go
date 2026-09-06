package tracking

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Multi-slot per-recipient passthrough (?t / ?t2 / ?t3 -> {{token}} /
// {{token2}} / {{token3}}).
//
// WHY THIS EXISTS: five internal-auto lanes could not be smart-linked because
// their advertiser destinations carry TWO or THREE per-recipient values and the
// gateway had exactly one slot. Dropping any of them breaks advertiser
// attribution, so those creatives were left pointing directly at the
// advertiser — invisible to the tracking layer.
//
// The templates below are the CONVERTED form of destinations read from prod
// mailing_offer_creatives (status NOT IN ('archived','rejected')) on
// 2026-09-05: each per-recipient Liquid expression becomes a token placeholder
// in slot order of appearance, every static param is carried verbatim, and
// {{subscriber.id}} stays a free (path-derived) substitution.

const (
	// A 32-char hex email-md5 and a partner tid, both already inside the
	// sanitizer's unreserved set — no escaping, no rejection.
	tokEMD5 = "6ff7d2773e99aabbccddeeff00112233"
	tokTID  = "7552-ff2007"
)

// nthSub returns a distinct, well-formed subscriber UUID per request.
//
// Every handler assertion below compares one /o/ response against an
// INDEPENDENTLY computed render for the SAME subscriber, rather than against a
// second /o/ response. Two reasons, and the second is the one that bites:
// comparing two responses can pass vacuously if both are empty, and the
// redirect path is allowed to grow per-(subscriber, destination) suppression
// (see the burst-deduper work in click_dedupe.go) which would make a repeat
// return no Location at all. Unique subscribers plus a computed expectation
// keep these guards meaningful under either regime.
func nthSub(i int) string { return fmt.Sprintf("11111111-1111-1111-1111-%012d", i) }

// blockedLane is one of the five destinations this change unblocks.
type blockedLane struct {
	name     string // drip lane
	tmpl     string // offer_url_template as the converter would store it
	t1       string // ?t
	t2       string // ?t2
	t3       string // ?t3  ("" when the lane needs only two slots)
	want     string // byte-exact URL the advertiser must receive
	wantPars map[string]string
}

func blockedLanes(sub, brand, camp string) []blockedLane {
	return []blockedLane{
		{
			name: "v9 (home.insurance-savings-finders.com) — s11=email_md5 + tokenid=tid",
			tmpl: "https://home.insurance-savings-finders.com/success/?id=2b1529&s4=minr87&channel=REV&s11={{token}}&tokenid={{token2}}",
			t1:   tokEMD5, t2: tokTID,
			want: "https://home.insurance-savings-finders.com/success/?channel=REV&id=2b1529&s11=" + tokEMD5 +
				"&s4=minr87&source_id=email&sub1=" + sub + "&sub2=" + brand + "&sub3=" + camp + "&tokenid=" + tokTID,
			wantPars: map[string]string{"s11": tokEMD5, "tokenid": tokTID, "id": "2b1529", "s4": "minr87", "channel": "REV"},
		},
		{
			name: "gmail_v1 (simple-insure-saver.com) — s11=custom.emd5 + tokenid=custom.tid, s12 STATIC",
			tmpl: "https://simple-insure-saver.com/success/?id=90a206&s4=minr87&channel=REV&s11={{token}}&tokenid={{token2}}&s12=3",
			t1:   tokEMD5, t2: tokTID,
			want: "https://simple-insure-saver.com/success/?channel=REV&id=90a206&s11=" + tokEMD5 +
				"&s12=3&s4=minr87&source_id=email&sub1=" + sub + "&sub2=" + brand + "&sub3=" + camp + "&tokenid=" + tokTID,
			wantPars: map[string]string{"s11": tokEMD5, "tokenid": tokTID, "s12": "3", "id": "90a206"},
		},
		{
			name: "gmail_v2 (insure-resources.com) — same shape, different advertiser id",
			tmpl: "https://insure-resources.com/success/?id=dbc1b8&s4=minr87&channel=REV&s11={{token}}&tokenid={{token2}}&s12=3",
			t1:   tokEMD5, t2: tokTID,
			want: "https://insure-resources.com/success/?channel=REV&id=dbc1b8&s11=" + tokEMD5 +
				"&s12=3&s4=minr87&source_id=email&sub1=" + sub + "&sub2=" + brand + "&sub3=" + camp + "&tokenid=" + tokTID,
			wantPars: map[string]string{"s11": tokEMD5, "tokenid": tokTID, "s12": "3", "id": "dbc1b8"},
		},
		{
			name: "v10 (form.ratekick.com) — key2=email_md5 + tokenid=tid + s12 via slot 3",
			tmpl: "https://form.ratekick.com/?c=C32391&datapassv2=true&datapassv2id={{subscriber.id}}&key2={{token}}&tokenid={{token2}}&s12={{token3}}",
			t1:   tokEMD5, t2: tokTID, t3: "2",
			want: "https://form.ratekick.com/?c=C32391&datapassv2=true&datapassv2id=" + sub + "&key2=" + tokEMD5 +
				"&s12=2&source_id=email&sub1=" + sub + "&sub2=" + brand + "&sub3=" + camp + "&tokenid=" + tokTID,
			wantPars: map[string]string{"key2": tokEMD5, "tokenid": tokTID, "s12": "2", "datapassv2id": sub, "c": "C32391", "datapassv2": "true"},
		},
		{
			name: "v2/v11 file lane (insurance-savings-pro.com) — s11 + tokenid + s12 via slot 3",
			tmpl: "https://insurance-savings-pro.com/success/?id=dbc1b8&s4=minr87&channel=REV&s11={{token}}&tokenid={{token2}}&s12={{token3}}",
			t1:   tokEMD5, t2: tokTID, t3: "2",
			want: "https://insurance-savings-pro.com/success/?channel=REV&id=dbc1b8&s11=" + tokEMD5 +
				"&s12=2&s4=minr87&source_id=email&sub1=" + sub + "&sub2=" + brand + "&sub3=" + camp + "&tokenid=" + tokTID,
			wantPars: map[string]string{"s11": tokEMD5, "tokenid": tokTID, "s12": "2", "id": "dbc1b8"},
		},
	}
}

// ROUND TRIP, END TO END: a real /o/ request with the extra slots must produce
// exactly the URL the advertiser expects, for all five previously-blocked
// lanes. This is the whole point of the change.
func TestOfferSlots_BlockedLanes_RoundTripToAdvertiserURL(t *testing.T) {
	const brand = "discountblog.com"
	for _, lane := range blockedLanes(subUUID, brand, campUUID) {
		h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
			"abc123": {Destination: lane.tmpl, RiskProfile: "low", BrandRoot: brand},
		}))
		q := "t=" + lane.t1 + "&t2=" + lane.t2
		if lane.t3 != "" {
			q += "&t3=" + lane.t3
		}
		rec := doOfferT(h, "t.em."+brand, subUUID, "abc123", campUUID, uaBrowser, q)
		if rec.Code != http.StatusFound {
			t.Fatalf("%s: code = %d, want 302", lane.name, rec.Code)
		}
		loc := rec.Header().Get("Location")
		if loc != lane.want {
			t.Errorf("%s: advertiser URL mismatch:\n got  %q\n want %q", lane.name, loc, lane.want)
			continue
		}
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("%s: Location unparseable: %v", lane.name, err)
		}
		for k, v := range lane.wantPars {
			if got := u.Query().Get(k); got != v {
				t.Errorf("%s: param %s = %q, want %q", lane.name, k, got, v)
			}
		}
		// Attribution is still applied on top of the advertiser's own params.
		for k, v := range map[string]string{"source_id": "email", "sub1": subUUID, "sub2": brand, "sub3": campUUID} {
			if got := u.Query().Get(k); got != v {
				t.Errorf("%s: attribution %s = %q, want %q", lane.name, k, got, v)
			}
		}
		// No placeholder may survive into what the advertiser sees.
		if strings.Contains(loc, "{{") || strings.Contains(loc, "}}") {
			t.Errorf("%s: literal placeholder leaked: %q", lane.name, loc)
		}
	}
}

// NEGATIVE CONTROL 1 — BACKWARD COMPATIBILITY, the absolute requirement.
// Every /o/ link in production today carries only ?t=. Adding t2/t3 to the
// request must not move a single byte for any live-shaped template, whether or
// not the template uses {{token}}.
func TestOfferSlots_ExistingLinksByteIdentical(t *testing.T) {
	const brand = "discountblog.com"
	tmpls := append([]string{acpTemplate}, liveCratoolproTemplates...)
	extras := []string{
		"",                         // today's shape: no extra slots at all
		"&t2=",                     // present but empty
		"&t2=" + tokEMD5,           // present and valid
		"&t2=" + tokEMD5 + "&t3=2", // both extra slots
		"&t2=x%26foo%3Dbar&t3=" + strings.Repeat("A", 10240), // hostile + oversized
	}
	n := 0
	for _, tmpl := range tmpls {
		h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
			"abc123": {Destination: tmpl, RiskProfile: "low", BrandRoot: brand},
		}))
		for _, tok := range []string{"", tokTID} {
			for _, extra := range extras {
				n++
				sub := nthSub(n)
				// EXPECTATION = the pre-change render: slot 1 only, nothing else.
				want := renderOfferDestination(tmpl, sub, brand, campUUID, tok)
				q := extra
				if tok != "" {
					q = "t=" + tok + extra
				} else if strings.HasPrefix(extra, "&") {
					q = extra[1:]
				}
				got := doOfferT(h, "t.em."+brand, sub, "abc123", campUUID, uaBrowser, q)
				if got.Code != http.StatusFound {
					t.Errorf("%q + %q: code = %d, want 302", tmpl, q, got.Code)
					continue
				}
				if l := got.Header().Get("Location"); l != want {
					t.Errorf("template %q moved when %q was added:\n got  %q\n want %q", tmpl, q, l, want)
				}
			}
		}
	}
}

// Same guarantee at the render level, independent of the handler: the
// pre-existing 5-arg renderOfferDestination must stay a byte-exact synonym for
// the new form with only slot 1 populated.
func TestOfferSlots_LegacyRenderIsSlot1Only(t *testing.T) {
	const brand = "discountblog.com"
	for _, tmpl := range append([]string{acpTemplate}, liveCratoolproTemplates...) {
		for _, tok := range []string{"", tokTID, tokEMD5} {
			legacy := renderOfferDestination(tmpl, subUUID, brand, campUUID, tok)
			modern := renderOfferDestinationTokens(tmpl, subUUID, brand, campUUID, offerTokens{T1: tok})
			if legacy != modern {
				t.Errorf("template %q token %q:\n legacy %q\n modern %q", tmpl, tok, legacy, modern)
			}
			// And slots 2/3 are inert for a template that does not name them.
			full := renderOfferDestinationTokens(tmpl, subUUID, brand, campUUID,
				offerTokens{T1: tok, T2: tokEMD5, T3: "9"})
			if full != legacy {
				t.Errorf("template %q: unused slots leaked:\n got  %q\n want %q", tmpl, full, legacy)
			}
		}
	}
}

// NEGATIVE CONTROL 2 — a MISSING slot renders EMPTY, never the literal
// placeholder. This is the failure that would silently ship "{{token2}}" to an
// advertiser as an opaque id for every recipient.
func TestOfferSlots_MissingSlotRendersEmptyNotLiteral(t *testing.T) {
	const brand = "discountblog.com"
	const tmpl = "https://ex.com/go?a={{token}}&b={{token2}}&c={{token3}}"
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: tmpl, RiskProfile: "low", BrandRoot: brand},
	}))
	// Only slot 1 supplied; slots 2 and 3 absent from the request entirely.
	rec := doOfferT(h, "t.em."+brand, subUUID, "abc123", campUUID, uaBrowser, "t="+tokTID)
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q unparseable: %v", loc, err)
	}
	if u.Query().Get("a") != tokTID {
		t.Errorf("a = %q, want %q", u.Query().Get("a"), tokTID)
	}
	for _, k := range []string{"b", "c"} {
		if _, present := u.Query()[k]; !present {
			t.Errorf("param %s should survive as an EMPTY value, got %q", k, loc)
		}
		if v := u.Query().Get(k); v != "" {
			t.Errorf("param %s = %q, want empty", k, v)
		}
	}
	for _, bad := range []string{"{{token}}", "{{token2}}", "{{token3}}", "%7Btoken", "token2%7D"} {
		if strings.Contains(loc, bad) {
			t.Errorf("literal placeholder %q leaked into the destination: %q", bad, loc)
		}
	}
	// Structure intact and attribution still applied.
	if u.Scheme != "https" || u.Host != "ex.com" || u.Path != "/go" {
		t.Errorf("empty slots altered the destination structure: %q", loc)
	}
	if u.Query().Get("sub1") != subUUID || u.Query().Get("source_id") != "email" {
		t.Errorf("attribution lost on the empty-slot path: %q", loc)
	}
}

// {{token}} must not consume {{token2}}/{{token3}}: the substitution needles are
// prefix-independent, so the order of the three ReplaceAll calls is meaningless.
// Cross-checked by rendering with each slot populated ALONE.
func TestOfferSlots_PlaceholdersDoNotCollide(t *testing.T) {
	const brand = "discountblog.com"
	const tmpl = "https://ex.com/go?a={{token}}&b={{token2}}&c={{token3}}"
	cases := []struct {
		toks offerTokens
		want map[string]string
	}{
		{offerTokens{T1: "one"}, map[string]string{"a": "one", "b": "", "c": ""}},
		{offerTokens{T2: "two"}, map[string]string{"a": "", "b": "two", "c": ""}},
		{offerTokens{T3: "three"}, map[string]string{"a": "", "b": "", "c": "three"}},
		{offerTokens{T1: "one", T2: "two", T3: "three"}, map[string]string{"a": "one", "b": "two", "c": "three"}},
	}
	for _, c := range cases {
		got := renderOfferDestinationTokens(tmpl, subUUID, brand, campUUID, c.toks)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse %q: %v", got, err)
		}
		for k, v := range c.want {
			if u.Query().Get(k) != v {
				t.Errorf("tokens %+v: %s = %q, want %q (full %q)", c.toks, k, u.Query().Get(k), v, got)
			}
		}
	}
	// A value can never forge another slot's placeholder — the sanitizer bars
	// braces — but assert the render is brace-proof anyway (defense in depth
	// for any future non-HTTP caller).
	got := renderOfferDestinationTokens(tmpl, subUUID, brand, campUUID, offerTokens{T1: "{{token2}}", T2: "real"})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("a") != "{{token2}}" {
		t.Errorf("a = %q, want the literal value (it must NOT be re-substituted)", u.Query().Get("a"))
	}
	if u.Query().Get("b") != "real" {
		t.Errorf("b = %q, want real", u.Query().Get("b"))
	}
}

// NEGATIVE CONTROL 3 — every slot is sanitized IDENTICALLY to ?t=. The exact
// hostile and oversized vectors that TestSanitizeOfferToken /
// TestOffer_HostileToken_CannotInject pin for slot 1 are replayed against slots
// 2 and 3, and each must render as if the slot were absent.
func TestOfferSlots_SanitizerIdenticalOnEverySlot(t *testing.T) {
	const brand = "discountblog.com"
	const tmpl = "https://ex.com/go?a={{token}}&b={{token2}}&c={{token3}}"
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: tmpl, RiskProfile: "low", BrandRoot: brand},
	}))

	vectors := map[string]string{
		"param injection": "x%26foo%3Dbar",
		"path traversal":  "..%2F..%2Fetc",
		"scheme breakout": "https%3A%2F%2Fevil.com",
		"host breakout":   "%2F%2Fevil.com%2F",
		"fragment":        "x%23frag",
		"crlf header":     "x%0D%0ASet-Cookie%3A%20a%3Db",
		"null byte":       "x%00y",
		"space":           "x%20y",
		"percent":         "%2541",
		"angle brackets":  "%3Cscript%3E",
		"mustache":        "%7B%7Btoken%7D%7D",
		"non-ascii":       "h%C3%A9llo",
		"over limit by 1": strings.Repeat("z", offerTokenMaxLen+1),
		"huge":            strings.Repeat("A", 10240),
	}
	n := 0
	for _, slot := range []string{"t", "t2", "t3"} {
		for name, tok := range vectors {
			n++
			sub := nthSub(n)
			// A rejected value must render EXACTLY as if the slot were absent.
			want := renderOfferDestinationTokens(tmpl, sub, brand, campUUID, offerTokens{})
			rec := doOfferT(h, "t.em."+brand, sub, "abc123", campUUID, uaBrowser, slot+"="+tok)
			if rec.Code != http.StatusFound {
				t.Errorf("%s=%s: code = %d, want 302", slot, name, rec.Code)
				continue
			}
			loc := rec.Header().Get("Location")
			if loc != want {
				t.Errorf("%s=%s: hostile value altered the destination:\n got  %q\n want %q", slot, name, loc, want)
			}
			if strings.ContainsAny(loc, "\r\n") {
				t.Errorf("%s=%s: CRLF reached the Location header: %q", slot, name, loc)
			}
		}
		// The accepted alphabet is identical too: exactly at the length bound,
		// and the full unreserved set, are accepted verbatim on every slot.
		for _, ok := range []string{"A-B_C.D~E", strings.Repeat("z", offerTokenMaxLen)} {
			n++
			rec := doOfferT(h, "t.em."+brand, nthSub(n), "abc123", campUUID, uaBrowser, slot+"="+ok)
			if rec.Code != http.StatusFound {
				t.Fatalf("%s=%q: code = %d, want 302", slot, ok, rec.Code)
			}
			u, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("%s: parse: %v", slot, err)
			}
			key := map[string]string{"t": "a", "t2": "b", "t3": "c"}[slot]
			if u.Query().Get(key) != ok {
				t.Errorf("%s=%q -> %s = %q, want it accepted verbatim", slot, ok, key, u.Query().Get(key))
			}
		}
	}
}

// The extra slots work on BOTH route shapes, and neither shape may cloak: the
// bytes served for a hash are identical for a scanner and a browser.
func TestOfferSlots_BothRouteShapesAndNoCloaking(t *testing.T) {
	const brand = "consumerpro.net"
	const tmpl = "https://home.insurance-savings-finders.com/success/?id=2b1529&s11={{token}}&tokenid={{token2}}"
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: tmpl, RiskProfile: "high", BrandRoot: brand},
	}))
	q := "t=" + tokEMD5 + "&t2=" + tokTID
	toks := offerTokens{T1: tokEMD5, T2: tokTID}

	cases := []struct {
		name string
		sub  string
		rec  func(sub string) string
	}{
		{"legacy 4-segment", nthSub(9001), func(sub string) string {
			return doOfferT(h, "t.em."+brand, sub, "abc123", campUUID, uaBrowser, q).Header().Get("Location")
		}},
		{"brand-in-path 5-segment", nthSub(9002), func(sub string) string {
			return doOffer5T(h, "t.em."+brand, brand, sub, "abc123", campUUID, uaBrowser, q).Header().Get("Location")
		}},
		{"scanner UA (must not cloak)", nthSub(9003), func(sub string) string {
			return doOfferT(h, "t.em."+brand, sub, "abc123", campUUID, uaScanner, q).Header().Get("Location")
		}},
	}
	for _, c := range cases {
		want := renderOfferDestinationTokens(tmpl, c.sub, brand, campUUID, toks)
		got := c.rec(c.sub)
		if got != want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, want)
			continue
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%s: parse %q: %v", c.name, got, err)
		}
		if u.Query().Get("s11") != tokEMD5 || u.Query().Get("tokenid") != tokTID {
			t.Errorf("%s: multi-slot values lost: %q", c.name, got)
		}
	}
	// Non-cloaking, stated directly: scanner and browser differ only where the
	// SUBSCRIBER differs, so normalising sub1/sub2/sub3 out makes them equal.
	strip := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		qv := u.Query()
		qv.Del("sub1")
		u.RawQuery = qv.Encode()
		return u.String()
	}
	human := strip(renderOfferDestinationTokens(tmpl, nthSub(9001), brand, campUUID, toks))
	scanner := strip(renderOfferDestinationTokens(tmpl, nthSub(9003), brand, campUUID, toks))
	if human != scanner {
		t.Errorf("handoff differs by UA — that would be cloaking:\n human   %q\n scanner %q", human, scanner)
	}
}

// A dictionary MISS, a bad hash, and a hostile combination of slots must all
// still fall back to the brand root rather than 500 — the extra slots must not
// have widened the surface that runs before the hash gate.
func TestOfferSlots_FallbacksUnchanged(t *testing.T) {
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{}))
	q := "t=" + strings.Repeat("A", 10240) + "&t2=%00&t3=" + strings.Repeat("B", 10240)
	for _, hash := range []string{"nosuchhash", "bad/hash", ""} {
		rec := doOfferT(h, "t.em.discountblog.com", subUUID, hash, campUUID, uaBrowser, q)
		if rec.Code != http.StatusFound && rec.Code != http.StatusNotFound {
			t.Errorf("hash %q: code = %d, want a redirect (or a 404 from the router), never 5xx", hash, rec.Code)
		}
		if rec.Code == http.StatusFound {
			if got := rec.Header().Get("Location"); got != "https://discountblog.com/" {
				t.Errorf("hash %q: fallback = %q, want the brand root", hash, got)
			}
		}
	}
}

// GAP CLOSER for the legacy wrapper: renderOfferDestination is slot 1 and
// NOTHING else. Proved against a template that names all three placeholders —
// without such a template the wrapper could quietly fan its one argument into
// slots 2 and 3 and every other test would still pass.
func TestOfferSlots_LegacyRenderCannotFillSlots2And3(t *testing.T) {
	const brand = "discountblog.com"
	const tmpl = "https://ex.com/go?a={{token}}&b={{token2}}&c={{token3}}"
	got := renderOfferDestination(tmpl, subUUID, brand, campUUID, tokTID)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("a") != tokTID {
		t.Errorf("a = %q, want %q", u.Query().Get("a"), tokTID)
	}
	for _, k := range []string{"b", "c"} {
		if v := u.Query().Get(k); v != "" {
			t.Errorf("legacy render leaked slot 1 into %s = %q; it must be empty", k, v)
		}
	}
}

// GAP CLOSER for escaping: every slot is url.QueryEscape'd at render time,
// independently of the handler-side sanitizer — defense in depth for any future
// non-HTTP caller, mirroring TestRenderOfferDestination_TokenIsURLEscaped which
// pins the same property for slot 1.
func TestOfferSlots_EverySlotIsURLEscapedAtRender(t *testing.T) {
	const brand = "discountblog.com"
	const tmpl = "https://ex.com/go?a={{token}}&b={{token2}}&c={{token3}}"
	const hostile = "a&b=c#d /e?f=g"
	for _, c := range []struct {
		key  string
		toks offerTokens
	}{
		{"a", offerTokens{T1: hostile}},
		{"b", offerTokens{T2: hostile}},
		{"c", offerTokens{T3: hostile}},
	} {
		got := renderOfferDestinationTokens(tmpl, subUUID, brand, campUUID, c.toks)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("slot %s: unparseable %q: %v", c.key, got, err)
		}
		if u.Host != "ex.com" || u.Path != "/go" {
			t.Errorf("slot %s: destination structure altered: %q", c.key, got)
		}
		if u.Fragment != "" {
			t.Errorf("slot %s: broke out into a fragment %q: %q", c.key, u.Fragment, got)
		}
		if v := u.Query().Get(c.key); v != hostile {
			t.Errorf("slot %s: not safely encoded, decoded to %q (full %q)", c.key, v, got)
		}
		for _, injected := range []string{"b=c", "f"} {
			if _, ok := u.Query()[injected]; ok && injected != c.key {
				t.Errorf("slot %s: injected parameter %q: %q", c.key, injected, got)
			}
		}
		// The other two slots stay empty — no bleed.
		for _, other := range []string{"a", "b", "c"} {
			if other == c.key {
				continue
			}
			if v := u.Query().Get(other); v != "" {
				t.Errorf("slot %s: bled into %s = %q", c.key, other, v)
			}
		}
	}
}

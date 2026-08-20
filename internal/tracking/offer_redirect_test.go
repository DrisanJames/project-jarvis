package tracking

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// capturePublisher is a deterministic, synchronous telemetry sink for tests.
type capturePublisher struct {
	mu     sync.Mutex
	events []TrackingEvent
}

func (c *capturePublisher) Publish(_ context.Context, evt TrackingEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
}

func (c *capturePublisher) last() (TrackingEvent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return TrackingEvent{}, false
	}
	return c.events[len(c.events)-1], true
}

func stubDict(entries map[string]smartLinkEntry) *SmartLinkDictionary {
	return &SmartLinkDictionary{entries: entries}
}

const (
	uaScanner = "Mozilla/5.0 (compatible; SafeLinks; +https://outlook.com)"
	uaBrowser = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	subUUID   = "11111111-1111-1111-1111-111111111111"
	campUUID  = "22222222-2222-2222-2222-222222222222"
)

func doOffer(h *Handler, host, sub, hash, camp, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/o/"+sub+"/"+hash+"/"+camp, nil)
	req.Host = host
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// doOffer5 exercises the brand-in-path 5-segment route
// /o/<brand>/<sub>/<hash>/<campaign>.
func doOffer5(h *Handler, host, brand, sub, hash, camp, ua string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/o/"+brand+"/"+sub+"/"+hash+"/"+camp, nil)
	req.Host = host
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// The brand-in-path segment is the ground-truth sub2, WINNING over both a
// (differently-branded) dictionary row AND the projectjarvis.io Host sentinel
// CloudFront leaves behind after stripping the viewer Host. This is the whole
// point of the 2026-07-22 contract.
func TestOffer5_PathBrandWins_OverEntryAndSentinelHost(t *testing.T) {
	pub := &capturePublisher{}
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "offercatalog.com", // the WRONG dedup-winner brand
		},
	}))

	// Host is the sentinel (CloudFront stripped the real viewer Host); the path
	// brand consumerpro.net must still win.
	rec := doOffer5(h, "projectjarvis.io", "consumerpro.net", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "sub2=consumerpro.net") {
		t.Errorf("sub2 must be the path brand consumerpro.net, got Location %q", loc)
	}
	if strings.Contains(loc, "sub2=offercatalog.com") {
		t.Errorf("sub2 must NOT be the entry brand offercatalog.com, got Location %q", loc)
	}
	if strings.Contains(loc, "sub2=projectjarvis.io") {
		t.Errorf("sub2 must NOT be the Host sentinel, got Location %q", loc)
	}
}

// Backward compat: the legacy 4-segment link (already in inboxes) still resolves,
// and it resolves the SAME hash to the SAME destination as the 5-segment link.
// Here the path/host DO agree on the brand so both Locations are byte-identical.
func TestOffer_LegacyAndBrandInPath_SameDestination(t *testing.T) {
	entries := map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	}
	h4 := NewHandler(&capturePublisher{}, stubDict(entries))
	h5 := NewHandler(&capturePublisher{}, stubDict(entries))

	rec4 := doOffer(h4, "t.em.consumerpro.net", subUUID, "abc123", campUUID, uaBrowser)
	rec5 := doOffer5(h5, "t.em.consumerpro.net", "consumerpro.net", subUUID, "abc123", campUUID, uaBrowser)

	if rec4.Code != http.StatusFound || rec5.Code != http.StatusFound {
		t.Fatalf("codes: legacy=%d brand-in-path=%d, want 302/302", rec4.Code, rec5.Code)
	}
	loc4 := rec4.Header().Get("Location")
	loc5 := rec5.Header().Get("Location")
	if loc4 != loc5 {
		t.Errorf("same hash must resolve to same destination:\n legacy=%q\n brand-in-path=%q", loc4, loc5)
	}
	if !strings.Contains(loc5, "sub2=consumerpro.net") {
		t.Errorf("both routes should attribute consumerpro.net, got %q", loc5)
	}
}

// A junk {brand} path segment (not a domain apex) must NOT poison sub2 — the
// handler falls through to the Host-derived brand.
func TestOffer5_JunkPathBrand_FallsBackToHost(t *testing.T) {
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://ex.com/x?sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	}))
	rec := doOffer5(h, "t.em.quizfiesta.com", "not-a-brand", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "sub2=quizfiesta.com") {
		t.Errorf("junk path brand should fall back to Host brand quizfiesta.com, got %q", loc)
	}
}

// A 5-segment miss falls back to the PATH brand's home page (best available
// brand), even when the Host is the sentinel.
func TestOffer5_Miss_302ToPathBrand(t *testing.T) {
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{}))
	rec := doOffer5(h, "projectjarvis.io", "consumerpro.net", subUUID, "zzz999", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://consumerpro.net/" {
		t.Errorf("miss Location = %q, want https://consumerpro.net/", loc)
	}
}

func TestLooksLikeDomainApex(t *testing.T) {
	good := []string{"consumerpro.net", "historythinking.com", "em.discountblog.com", "a.co"}
	bad := []string{"", "not-a-brand", "no dot", "has/slash", "trailing.", ".leading"}
	for _, s := range good {
		if !looksLikeDomainApex(s) {
			t.Errorf("looksLikeDomainApex(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if looksLikeDomainApex(s) {
			t.Errorf("looksLikeDomainApex(%q) = true, want false", s)
		}
	}
}

func TestOffer_HitLowRisk_302WithAttribution(t *testing.T) {
	pub := &capturePublisher{}
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: "https://cratoolpro.com/BJB4Q5BF/OFFER", RiskProfile: "low", BrandRoot: "discountblog.com"},
	}))

	rec := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	for _, want := range []string{"source_id=email", "sub1=" + subUUID, "sub2=discountblog.com", "sub3=" + campUUID} {
		if !strings.Contains(loc, want) {
			t.Errorf("Location %q missing %q", loc, want)
		}
	}
}

// A high-risk hash gets the SAME unconditional 302 as any other. The bridge
// page it used to serve was removed 2026-08-20 (operator: the hop is a bad
// human experience and taxes the click a CPC offer is paid for). The
// no-cloaking property is unchanged and in fact simpler: the handoff never
// varies by VISITOR, which this pins by driving the same hash with a scanner UA
// and a browser UA and demanding byte-identical treatment.
func TestOffer_HighRisk_302sIdenticallyForScannerAndHuman(t *testing.T) {
	pub := &capturePublisher{}
	entry := smartLinkEntry{Destination: "https://ex.com/pay", RiskProfile: "high", BrandRoot: "discountblog.com"}
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{"abc123": entry}))

	recScanner := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaScanner)
	recBrowser := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser)

	if recScanner.Code != http.StatusFound || recBrowser.Code != http.StatusFound {
		t.Fatalf("high-risk codes: scanner=%d browser=%d, want 302/302 (no interstitial)", recScanner.Code, recBrowser.Code)
	}
	dest := renderOfferDestination(entry.Destination, subUUID, entry.BrandRoot, campUUID, "")
	for name, rec := range map[string]*httptest.ResponseRecorder{"scanner": recScanner, "browser": recBrowser} {
		if got := rec.Header().Get("Location"); got != dest {
			t.Errorf("%s Location = %q, want %q", name, got, dest)
		}
	}
	// NO CLOAKING: same status, same Location, no body divergence.
	if recScanner.Header().Get("Location") != recBrowser.Header().Get("Location") {
		t.Fatal("handoff differs by UA — that would be cloaking")
	}
	// And no interstitial may creep back in.
	if strings.Contains(recBrowser.Body.String(), "Continue") {
		t.Errorf("bridge page came back: %s", recBrowser.Body.String())
	}
}

func TestOffer_Miss_302ToBrandRootFromHost(t *testing.T) {
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{}))
	rec := doOffer(h, "t.em.quizfiesta.com", subUUID, "zzz999", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://quizfiesta.com/" {
		t.Errorf("miss Location = %q, want https://quizfiesta.com/", loc)
	}
}

func TestOffer_NilDictMissesGracefully(t *testing.T) {
	// nil dictionary (no DATABASE_URL path) must fall back, not panic.
	h := NewHandler(&capturePublisher{}, nil)
	rec := doOffer(h, "trk.em.historythinking.com", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://historythinking.com/" {
		t.Errorf("Location = %q, want https://historythinking.com/", loc)
	}
}

func TestOffer_BadHash_FallbackNoPanic(t *testing.T) {
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: "https://ex.com/x", RiskProfile: "low"},
	}))
	// "sh!" fails the ^[A-Za-z0-9]{6,20}$ pattern.
	rec := doOffer(h, "t.em.discountblog.com", subUUID, "sh!", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("bad-hash code = %d, want 302 fallback", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://discountblog.com/" {
		t.Errorf("bad-hash Location = %q, want https://discountblog.com/", loc)
	}
}

func TestOffer_MalformedSubscriberStillRedirects(t *testing.T) {
	// A non-UUID subscriber must NOT gate the redirect — it only affects
	// attribution/telemetry.
	h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: "https://ex.com/x", RiskProfile: "low", BrandRoot: "discountblog.com"},
	}))
	rec := doOffer(h, "t.em.discountblog.com", "not-a-uuid", "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "sub1=not-a-uuid") {
		t.Errorf("attribution should still carry the raw subscriber, got %q", loc)
	}
}

func TestOffer_TelemetryLabel_MachineVsHuman_SameServedBytes(t *testing.T) {
	entry := smartLinkEntry{Destination: "https://ex.com/x", RiskProfile: "low", BrandRoot: "discountblog.com"}

	// Scanner UA -> label "machine".
	pubM := &capturePublisher{}
	hM := NewHandler(pubM, stubDict(map[string]smartLinkEntry{"abc123": entry}))
	recM := doOffer(hM, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaScanner)
	evtM, ok := pubM.last()
	if !ok {
		t.Fatal("no telemetry event published for scanner")
	}
	if evtM.Actor != "machine" {
		t.Errorf("scanner Actor = %q, want machine", evtM.Actor)
	}

	// Browser UA -> label "human".
	pubH := &capturePublisher{}
	hH := NewHandler(pubH, stubDict(map[string]smartLinkEntry{"abc123": entry}))
	recH := doOffer(hH, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser)
	evtH, ok := pubH.last()
	if !ok {
		t.Fatal("no telemetry event published for browser")
	}
	if evtH.Actor != "human" {
		t.Errorf("browser Actor = %q, want human", evtH.Actor)
	}

	// The LABEL differs but the SERVED bytes (redirect target) are identical —
	// the no-cloaking guarantee.
	if recM.Header().Get("Location") != recH.Header().Get("Location") {
		t.Errorf("served Location differs by UA: machine=%q human=%q (cloaking!)",
			recM.Header().Get("Location"), recH.Header().Get("Location"))
	}
	// Telemetry LinkURL equals the served dest.
	if evtH.LinkURL != recH.Header().Get("Location") {
		t.Errorf("telemetry LinkURL %q != served Location %q", evtH.LinkURL, recH.Header().Get("Location"))
	}
}

// sub2 (attribution brand) must come from the REQUEST HOST, not the dictionary
// row's brand_root — offer destinations dedup across brands, so the winning row
// can carry a different sending brand than the one that actually sent this
// message. The Host is ground truth.
func TestOffer_Sub2FromHost_NotEntryBrand(t *testing.T) {
	pub := &capturePublisher{}
	// Dictionary row's brand_root is discountblog.com (a DIFFERENT brand that
	// won the destination dedup), but the request arrives on consumerpro's host.
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	}))

	rec := doOffer(h, "t.em.consumerpro.net", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "sub2=consumerpro.net") {
		t.Errorf("sub2 must be the Host-derived apex consumerpro.net, got Location %q", loc)
	}
	if strings.Contains(loc, "sub2=discountblog.com") {
		t.Errorf("sub2 must NOT be the entry brand discountblog.com, got Location %q", loc)
	}
}

// When the Host yields no usable sending brand (the projectjarvis.io sentinel
// from a malformed/missing Host), sub2 falls back to the dictionary row's
// brand_root.
func TestOffer_Sub2FallsBackToEntryBrandWhenHostHasNoBrand(t *testing.T) {
	pub := &capturePublisher{}
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email&sub2={{brand.domain}}",
			RiskProfile: "low",
			BrandRoot:   "historythinking.com",
		},
	}))

	// projectjarvis.io is the sentinel brandRootFromHost emits when the Host is
	// not a t.em/trk.em sending host — no usable sending brand.
	rec := doOffer(h, "projectjarvis.io", subUUID, "abc123", campUUID, uaBrowser)
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "sub2=historythinking.com") {
		t.Errorf("sub2 must fall back to the entry brand historythinking.com, got Location %q", loc)
	}
}

func TestBrandRootFromHost(t *testing.T) {
	cases := map[string]string{
		"t.em.discountblog.com":      "discountblog.com",
		"trk.em.quizfiesta.com":      "quizfiesta.com",
		"www.historythinking.com":    "historythinking.com",
		"t.em.discountblog.com:8081": "discountblog.com",
		"MyOwnHealth.net":            "myownhealth.net",
		"":                           "projectjarvis.io",
	}
	for in, want := range cases {
		if got := brandRootFromHost(in); got != want {
			t.Errorf("brandRootFromHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- ?t= opaque token passthrough -------------------------------------------

// acpTemplate is the shape this feature exists for: an advertiser landing URL
// carrying its OWN per-recipient id. Before {{token}} existed, routing this
// link through the gateway would have dropped tokenid and destroyed partner
// attribution on every click.
const acpTemplate = "https://autocoveragepoint.com/coverage-match?id=ff2007&s4=7552&channel=Rev&tokenid={{token}}"

func doOfferT(h *Handler, host, sub, hash, camp, ua, rawQuery string) *httptest.ResponseRecorder {
	target := "/o/" + sub + "/" + hash + "/" + camp
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func doOffer5T(h *Handler, host, brand, sub, hash, camp, ua, rawQuery string) *httptest.ResponseRecorder {
	target := "/o/" + brand + "/" + sub + "/" + hash + "/" + camp
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func acpHandler(risk string) *Handler {
	return NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
		"abc123": {Destination: acpTemplate, RiskProfile: risk, BrandRoot: "discountblog.com"},
	}))
}

func TestSanitizeOfferToken(t *testing.T) {
	// The ACCEPTED charset is exactly the RFC 3986 unreserved set.
	good := []string{
		"a", "0", "abc123", "A-B_C.D~E",
		"7552-ff2007", "tok.ID_9~x", strings.Repeat("z", 256),
	}
	for _, s := range good {
		if got := sanitizeOfferToken(s); got != s {
			t.Errorf("sanitizeOfferToken(%q) = %q, want it accepted verbatim", s, got)
		}
	}
	bad := []string{
		"", "x&foo=bar", "x=y", "../", "a/b", "a:b", "a?b", "a#b", "a%20b", "a+b",
		"a b", "a\tb", "a\rb", "a\nb", "x\r\nSet-Cookie: a=b", "a\x00b",
		"héllo", "a,b", "a;b", "a'b", "a\"b", "<script>", "{{token}}",
		strings.Repeat("z", 257), strings.Repeat("A", 10240),
	}
	for _, s := range bad {
		if got := sanitizeOfferToken(s); got != "" {
			t.Errorf("sanitizeOfferToken(%q) = %q, want \"\" (reject, never escape)", s, got)
		}
	}
}

// 1. The token rides the query string into {{token}}.
func TestOffer_TokenPassthrough(t *testing.T) {
	rec := doOfferT(acpHandler("low"), "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "t=7552-ff2007~x.y_z")
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	if u.Query().Get("tokenid") != "7552-ff2007~x.y_z" {
		t.Errorf("tokenid = %q, want 7552-ff2007~x.y_z (Location %q)", u.Query().Get("tokenid"), loc)
	}
	if u.Host != "autocoveragepoint.com" {
		t.Errorf("host = %q, want autocoveragepoint.com", u.Host)
	}
}

// 2. No ?t= at all (and an invalid one) must still reach the offer.
func TestOffer_TokenAbsentOrInvalid_OfferStillReachable(t *testing.T) {
	for _, rawQuery := range []string{"", "t=", "t=x%26foo%3Dbar", "other=1"} {
		rec := doOfferT(acpHandler("low"), "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, rawQuery)
		if rec.Code != http.StatusFound {
			t.Fatalf("query %q: code = %d, want 302", rawQuery, rec.Code)
		}
		loc := rec.Header().Get("Location")
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("query %q: Location %q unparseable: %v", rawQuery, loc, err)
		}
		if u.Scheme != "https" || u.Host != "autocoveragepoint.com" || u.Path != "/coverage-match" {
			t.Errorf("query %q: destination structure broken: %q", rawQuery, loc)
		}
		if u.Query().Get("tokenid") != "" {
			t.Errorf("query %q: tokenid should be empty, got %q", rawQuery, u.Query().Get("tokenid"))
		}
		// The advertiser's own params and our attribution both survive.
		if u.Query().Get("id") != "ff2007" || u.Query().Get("sub1") != subUUID {
			t.Errorf("query %q: params lost: %q", rawQuery, loc)
		}
	}
}

// 3. A hostile ?t= must not inject a param, change the host, escape the query,
// or alter the destination in ANY way — the served bytes must equal the
// no-token render.
func TestOffer_HostileToken_CannotInject(t *testing.T) {
	baseline := doOfferT(acpHandler("low"), "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "")
	want := baseline.Header().Get("Location")

	// Every vector is percent-encoded as it would arrive in a real request
	// query. Each decodes to a value that is NOT in the unreserved set, so
	// sanitizeOfferToken must reject it wholesale.
	vectors := map[string]string{
		"param injection": "x%26foo%3Dbar",
		"path traversal":  "..%2F..%2Fetc",
		"dot dot slash":   "../",
		"scheme breakout": "https%3A%2F%2Fevil.com",
		"host breakout":   "%2F%2Fevil.com%2F",
		"fragment":        "x%23frag",
		"crlf header":     "x%0D%0ASet-Cookie%3A%20a%3Db",
		"newline":         "x%0Ay",
		"null byte":       "x%00y",
		"space":           "x%20y",
		"percent":         "%2541",
		"quote":           "x%22onmouseover%3D%22alert(1)",
		"angle brackets":  "%3Cscript%3E",
		"mustache":        "%7B%7Btoken%7D%7D",
		"huge":            strings.Repeat("A", 10240),
		"over limit by 1": strings.Repeat("z", 257),
	}
	for name, tok := range vectors {
		rec := doOfferT(acpHandler("low"), "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "t="+tok)
		if rec.Code != http.StatusFound {
			t.Errorf("%s: code = %d, want 302", name, rec.Code)
			continue
		}
		loc := rec.Header().Get("Location")
		if loc != want {
			t.Errorf("%s: hostile token altered the destination:\n got  %q\n want %q", name, loc, want)
		}
		u, err := url.Parse(loc)
		if err != nil {
			t.Errorf("%s: Location unparseable: %v", name, err)
			continue
		}
		if u.Host != "autocoveragepoint.com" {
			t.Errorf("%s: host changed to %q", name, u.Host)
		}
		if _, ok := u.Query()["foo"]; ok {
			t.Errorf("%s: injected a parameter: %q", name, loc)
		}
		if u.Fragment != "" {
			t.Errorf("%s: broke out into a fragment: %q", name, u.Fragment)
		}
		if strings.ContainsAny(loc, "\r\n") {
			t.Errorf("%s: CRLF reached the Location header: %q", name, loc)
		}
	}
	// A RAW "&" in the request query is ordinary query parsing, not injection:
	// ?t=x&foo=bar means t="x" (a legal token) and a separate foo param that is
	// none of our business. Assert the extra param cannot reach the destination.
	rec := doOfferT(acpHandler("low"), "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "t=x&foo=bar")
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse %q: %v", loc, err)
	}
	if u.Query().Get("tokenid") != "x" {
		t.Errorf("raw-& case: tokenid = %q, want x", u.Query().Get("tokenid"))
	}
	if _, ok := u.Query()["foo"]; ok {
		t.Errorf("raw-& case: an unrelated request param reached the destination: %q", loc)
	}
	if u.Host != "autocoveragepoint.com" {
		t.Errorf("raw-& case: host = %q", u.Host)
	}
}

// 4. REGRESSION GUARD over the real live templates: none of them carries
// {{token}}, so a ?t= on the request must be a complete no-op end to end.
func TestOffer_LiveTemplates_ByteIdenticalWithAndWithoutToken(t *testing.T) {
	for _, tmpl := range liveCratoolproTemplates {
		h := NewHandler(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
			"abc123": {Destination: tmpl, RiskProfile: "low", BrandRoot: "discountblog.com"},
		}))
		base := doOfferT(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "")
		for _, tok := range []string{"t=abc123", "t=x%26foo%3Dbar", "t=" + strings.Repeat("A", 10240)} {
			got := doOfferT(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, tok)
			if got.Header().Get("Location") != base.Header().Get("Location") {
				t.Errorf("template %q moved with %q:\n got  %q\n want %q",
					tmpl, tok, got.Header().Get("Location"), base.Header().Get("Location"))
			}
		}
	}
}

// 5. Both route shapes carry the token.
func TestOffer_TokenOnBothRouteShapes(t *testing.T) {
	const tok = "7552-ff2007"
	rec4 := doOfferT(acpHandler("low"), "t.em.consumerpro.net", subUUID, "abc123", campUUID, uaBrowser, "t="+tok)
	rec5 := doOffer5T(acpHandler("low"), "t.em.consumerpro.net", "consumerpro.net", subUUID, "abc123", campUUID, uaBrowser, "t="+tok)
	if rec4.Code != http.StatusFound || rec5.Code != http.StatusFound {
		t.Fatalf("codes: legacy=%d brand-in-path=%d, want 302/302", rec4.Code, rec5.Code)
	}
	loc4, loc5 := rec4.Header().Get("Location"), rec5.Header().Get("Location")
	for _, loc := range []string{loc4, loc5} {
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse %q: %v", loc, err)
		}
		if u.Query().Get("tokenid") != tok {
			t.Errorf("tokenid = %q, want %q (Location %q)", u.Query().Get("tokenid"), tok, loc)
		}
	}
	// Both shapes agree here (path brand == host brand), so they must be equal.
	if loc4 != loc5 {
		t.Errorf("route shapes diverged:\n 4-seg %q\n 5-seg %q", loc4, loc5)
	}
}

// 6. A high-risk link still carries the rendered token through to its
// destination — the token must survive the removal of the bridge, and the
// redirect must not vary by visitor.
func TestOffer_HighRisk_TokenSurvivesAndStaysNonCloaking(t *testing.T) {
	const tok = "7552-ff2007"
	h := acpHandler("high")
	recScanner := doOfferT(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaScanner, "t="+tok)
	recBrowser := doOfferT(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser, "t="+tok)

	if recScanner.Code != http.StatusFound || recBrowser.Code != http.StatusFound {
		t.Fatalf("high-risk codes: scanner=%d browser=%d, want 302/302", recScanner.Code, recBrowser.Code)
	}
	dest := renderOfferDestination(acpTemplate, subUUID, "discountblog.com", campUUID, tok)
	if !strings.Contains(dest, "tokenid="+tok) {
		t.Fatalf("test precondition: rendered dest lost the token: %q", dest)
	}
	if got := recBrowser.Header().Get("Location"); got != dest {
		t.Errorf("Location lost the rendered token; got %q want %q", got, dest)
	}
	if recScanner.Header().Get("Location") != recBrowser.Header().Get("Location") {
		t.Fatal("handoff differs by UA — that would be cloaking")
	}
}

package tracking

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
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

func TestOffer_HitHighRisk_200BridgeNoCloaking(t *testing.T) {
	pub := &capturePublisher{}
	entry := smartLinkEntry{Destination: "https://ex.com/pay", RiskProfile: "high", BrandRoot: "discountblog.com"}
	h := NewHandler(pub, stubDict(map[string]smartLinkEntry{"abc123": entry}))

	// Same hash requested by a scanner UA and a browser UA.
	recScanner := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaScanner)
	recBrowser := doOffer(h, "t.em.discountblog.com", subUUID, "abc123", campUUID, uaBrowser)

	if recScanner.Code != http.StatusOK || recBrowser.Code != http.StatusOK {
		t.Fatalf("high-risk codes: scanner=%d browser=%d, want 200/200", recScanner.Code, recBrowser.Code)
	}
	// NO CLOAKING: byte-identical output regardless of UA.
	if recScanner.Body.String() != recBrowser.Body.String() {
		t.Fatal("high-risk bridge differs by UA — that would be cloaking")
	}
	// The bridge must carry a Continue anchor to the rendered dest.
	dest := renderOfferDestination(entry.Destination, subUUID, entry.BrandRoot, campUUID)
	wantHref := `href="` + html.EscapeString(dest) + `"`
	if !strings.Contains(recBrowser.Body.String(), wantHref) {
		t.Errorf("bridge missing Continue href; want %q in body:\n%s", wantHref, recBrowser.Body.String())
	}
	if ct := recBrowser.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("bridge Content-Type = %q, want text/html", ct)
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

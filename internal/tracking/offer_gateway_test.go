package tracking

// Offer-gateway regression suite — QA second pass, 2026-09-05.
//
// gateway_test.go (the implementer's suite) pins the rule, the classes, the
// no-store headers, the exemption and fail-open. This file does NOT restate
// those; it covers the requirements that suite leaves open, and adds a
// byte-exact second pin on the single most expensive failure available:
// SHADOW WITHHOLDING. Shadow is the shipped default, so if shadow ever
// withholds we lose real clicks the hour it deploys, silently, and the
// advertiser sees the shortfall before we do.
//
// What is new here, and why:
//   - the forwarded destination asserted BYTE-EXACT, not by host substring, so
//     a gateway that forwards but mangles sub1/sub2/sub3 is caught;
//   - GATEWAY_ENFORCE set to non-truthy values is still shadow;
//   - withheld and forwarded telemetry compared AS A PAIR (the requirement is
//     that they are distinguishable, which neither event proves alone);
//   - an unparseable client IP driven through the real HTTP path;
//   - a dictionary miss under enforcement must stay a brand-root redirect and
//     not become a withhold;
//   - an unsubscribe forwarded through the /o/ path with its Location intact;
//   - NO OUTBOUND HTTP REQUEST to the destination, on any path, in any mode —
//     a server-side fetch would itself register the click the gateway exists to
//     suppress. Asserted by live fixture and structurally.
//
// Everything asserts observable HTTP outcome through httptest against
// Handler.Routes() — never "function X was called". Helpers are reused from
// gateway_test.go (stubClassifier / seededClassifier / offerHandler /
// doOfferFromIP / doClickFromIP) rather than re-implemented.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	ipGwScanner     = "135.232.20.148" // 'scanner'   /32 carved out of a hosting /16
	ipGwUnresolved  = "135.232.20.64"  // 'unresolved' — scanner sweeps AND real people
	ipGwHosting     = "135.232.99.99"  // no /32 -> the 'hosting' /16
	ipGwUnclassifie = "8.8.8.8"        // no row at all -> "" (the NULL of ignite_ip_class)
)

// gwTokenDest carries all three attribution placeholders so a forwarded
// Location can be compared byte-for-byte.
const gwTokenDest = "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"

// gwTokenHandler resolves "tok001" to gwTokenDest.
func gwTokenHandler(t *testing.T, ipc *IPClassifier, pub eventPublisher) *Handler {
	t.Helper()
	return NewHandlerWithClassifier(pub, stubDict(map[string]smartLinkEntry{
		"tok001": {Destination: gwTokenDest, RiskProfile: "low", BrandRoot: "discountblog.com"},
		"unsub9": {Destination: "https://em.discountblog.com/unsubscribe?d={{subscriber.id}}", RiskProfile: "low", BrandRoot: "discountblog.com"},
	}), ipc)
}

// gwShadow clears BOTH gateway flags — the shipped default posture. Clearing
// GATEWAY_DISABLED too matters: a shadow test that leaves it set would pass
// because the gateway never ran, not because shadow forwarded.
func gwShadow(t *testing.T) {
	t.Helper()
	for _, k := range []string{GatewayEnforceEnv, GatewayDisabledEnv} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func gwEnforce(t *testing.T) {
	t.Helper()
	gwShadow(t)
	t.Setenv(GatewayEnforceEnv, "1")
}

// ── 1. SHADOW MUST NOT WITHHOLD, AND MUST NOT ALTER THE HANDOFF ─────────────
//
// A confirmed-'scanner' address with GATEWAY_ENFORCE unset gets 302 to the
// advertiser, and the Location is byte-identical to what an unclassified
// visitor gets. The exact-bytes comparison is the point: "not withheld" is not
// enough if the destination arrives degraded.
func TestOfferGateway_ShadowForwardsTheExactRenderedDestination(t *testing.T) {
	const wantLoc = "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email" +
		"&sub1=" + subUUID + "&sub2=discountblog.com&sub3=" + campUUID

	pub := &capturePublisher{}
	gwShadow(t)
	h := gwTokenHandler(t, seededClassifier(t), pub)

	scanner := doOfferFromIP(h, ipGwScanner, "tok001")
	if scanner.Code != http.StatusFound {
		t.Fatalf("shadow: status = %d, want 302 — SHADOW IS WITHHOLDING, every scanner-classified click is being lost", scanner.Code)
	}
	got := scanner.Header().Get("Location")
	if got != wantLoc {
		t.Fatalf("shadow must hand off the UNCHANGED advertiser URL.\n got %q\nwant %q", got, wantLoc)
	}

	// The scanner's own event — read before the second request below, which
	// would otherwise be the one pub.last() returns.
	evt, ok := pub.last()
	if !ok {
		t.Fatal("shadow forward published no telemetry — shadow that records nothing collects no evidence")
	}
	if evt.GatewayAction != GatewayActionShadowWithheld {
		t.Errorf("shadow GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionShadowWithheld)
	}

	// And identical to an unclassified visitor's, byte for byte.
	human := doOfferFromIP(h, ipGwUnclassifie, "tok001")
	if human.Header().Get("Location") != got {
		t.Fatalf("shadow altered the destination by visitor:\n scanner=%q\n human  =%q", got, human.Header().Get("Location"))
	}
}

// Enforcement is opt-IN: only a truthy flag arms it. A deploy that sets
// GATEWAY_ENFORCE=0 or =false to mean "off" must not withhold.
func TestOfferGateway_NonTruthyEnforceIsStillShadow(t *testing.T) {
	for _, v := range []string{"", "0", "false", "off", "no", "  ", "maybe", "shadow"} {
		t.Run("GATEWAY_ENFORCE="+strings.ReplaceAll(v, " ", "_"), func(t *testing.T) {
			gwShadow(t)
			t.Setenv(GatewayEnforceEnv, v)
			rec := doOfferFromIP(gwTokenHandler(t, seededClassifier(t), &capturePublisher{}), ipGwScanner, "tok001")
			if rec.Code == http.StatusNoContent {
				t.Fatalf("GATEWAY_ENFORCE=%q WITHHELD — only a truthy value may arm enforcement", v)
			}
			if rec.Header().Get("Location") == "" {
				t.Fatalf("GATEWAY_ENFORCE=%q: forwarded with no Location", v)
			}
		})
	}
}

// ── 3. UNDER ENFORCEMENT, ONLY 'scanner' WITHHOLDS ─────────────────────────
//
// 'unresolved' is the load-bearing case and the one most likely to be "fixed"
// by a future tie-breaker: those seeded /32s carry BOTH scanner sweeps AND real
// people, and forwarding them is the deliberate ruling. Asserted with the
// destination byte-exact, alongside 'hosting' and no-row-at-all, and with a
// same-run control proving enforcement really was armed.
func TestOfferGateway_OnlyScannerWithholdsUnderEnforcement(t *testing.T) {
	const wantLoc = "https://www.eos57ytf.com/K4C5ZLC/OFFER/?source_id=email" +
		"&sub1=" + subUUID + "&sub2=discountblog.com&sub3=" + campUUID

	cases := []struct{ name, ip, wantClass string }{
		{"unresolved — scanner sweeps AND real people", ipGwUnresolved, "unresolved"},
		{"hosting /16, no curated /32", ipGwHosting, "hosting"},
		{"NULL — no classification row at all", ipGwUnclassifie, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gwEnforce(t)
			ipc := seededClassifier(t)

			// Pin the class the fixture resolves, so this cannot pass for the
			// wrong reason (e.g. an 'unresolved' /32 silently falling back to
			// its hosting /16).
			if got := ipc.Classify(c.ip); got != c.wantClass {
				t.Fatalf("fixture drift: Classify(%s) = %q, want %q", c.ip, got, c.wantClass)
			}

			h := gwTokenHandler(t, ipc, &capturePublisher{})
			rec := doOfferFromIP(h, c.ip, "tok001")
			if rec.Code != http.StatusFound {
				t.Fatalf("class %q: status = %d, want 302 FORWARD", c.wantClass, rec.Code)
			}
			if got := rec.Header().Get("Location"); got != wantLoc {
				t.Fatalf("class %q: Location = %q, want %q", c.wantClass, got, wantLoc)
			}

			// Control: enforcement WAS armed in this same run.
			if ctl := doOfferFromIP(h, ipGwScanner, "tok001"); ctl.Code != http.StatusNoContent {
				t.Fatalf("control: scanner returned %d, want 204 — the gateway was not enforcing, so the forward above proves nothing", ctl.Code)
			}
		})
	}
}

// ── 6. WITHHELD AND FORWARDED TELEMETRY MUST BE TELLABLE APART ──────────────
//
// The requirement is a RELATION between the two events, so it is asserted on
// the pair: both published, and carrying different markers.
func TestOfferGateway_WithheldAndForwardedTelemetryDiffer(t *testing.T) {
	gwEnforce(t)
	pub := &capturePublisher{}
	h := gwTokenHandler(t, seededClassifier(t), pub)

	if rec := doOfferFromIP(h, ipGwScanner, "tok001"); rec.Code != http.StatusNoContent {
		t.Fatalf("setup: enforced scanner status = %d, want 204", rec.Code)
	}
	if rec := doOfferFromIP(h, ipGwUnclassifie, "tok001"); rec.Code != http.StatusFound {
		t.Fatalf("setup: unclassified status = %d, want 302", rec.Code)
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.events) != 2 {
		t.Fatalf("published %d events, want 2 — we suppress the advertiser hop, never our own data", len(pub.events))
	}
	withheld, forwarded := pub.events[0], pub.events[1]
	if withheld.GatewayAction == forwarded.GatewayAction {
		t.Fatalf("withheld and forwarded events carry the SAME GatewayAction %q — a suppressed click is invisible in our own data", withheld.GatewayAction)
	}
	if withheld.GatewayAction != GatewayActionWithheld {
		t.Errorf("withheld GatewayAction = %q, want %q", withheld.GatewayAction, GatewayActionWithheld)
	}
	if forwarded.GatewayAction != "" {
		t.Errorf("forwarded GatewayAction = %q, want \"\" so a normal click stays byte-identical on the wire", forwarded.GatewayAction)
	}
	if withheld.LinkURL == "" || withheld.SubscriberID == "" {
		t.Errorf("withheld event lost its attribution: LinkURL=%q SubscriberID=%q", withheld.LinkURL, withheld.SubscriberID)
	}
}

// ── 7. FAIL OPEN, DRIVEN THROUGH THE REAL HTTP PATH ─────────────────────────
//
// An address the classifier cannot parse is not a scanner; it forwards.
// Asserted as 302 with a Location, not as "did not panic".
func TestOfferGateway_UnparseableIPForwards(t *testing.T) {
	for _, ip := range []string{"not-an-ip", "999.999.999.999", "", "135.232.20.148.9", "::gg", "135.232.20.148/32"} {
		t.Run("XFF="+ip, func(t *testing.T) {
			gwEnforce(t)
			rec := doOfferFromIP(gwTokenHandler(t, seededClassifier(t), &capturePublisher{}), ip, "tok001")
			if rec.Code != http.StatusFound {
				t.Fatalf("unparseable IP %q: status = %d, want 302 FORWARD", ip, rec.Code)
			}
			if rec.Header().Get("Location") == "" {
				t.Fatalf("unparseable IP %q: 302 with no Location", ip)
			}
		})
	}
}

// A dictionary miss is a dead link, not a scanner verdict: under full
// enforcement, from a confirmed-'scanner' address, it must still be the
// brand-root redirect it has always been — never a 204.
func TestOfferGateway_DictionaryMissForwardsUnderEnforcement(t *testing.T) {
	gwEnforce(t)
	h := gwTokenHandler(t, seededClassifier(t), &capturePublisher{})

	for _, hash := range []string{"nosuchhash", "bad"} { // miss, then malformed hash
		rec := doOfferFromIP(h, ipGwScanner, hash)
		if rec.Code != http.StatusFound {
			t.Fatalf("hash %q: status = %d, want 302 to brand root", hash, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "https://discountblog.com/" {
			t.Fatalf("hash %q: Location = %q, want the brand root", hash, loc)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(strings.ToLower(cc), "no-store") {
			t.Fatalf("hash %q: fallback Cache-Control = %q, want no-store", hash, cc)
		}
	}
}

// ── 4. UNSUBSCRIBE FORWARDS INTACT THROUGH THE /o/ PATH ─────────────────────
//
// gateway_test.go proves the /o/ unsubscribe is not 204. This pins the stronger
// property the compliance guarantee actually needs: the visitor is handed the
// real, fully rendered opt-out URL.
func TestOfferGateway_UnsubscribeForwardsIntactThroughOfferPath(t *testing.T) {
	gwEnforce(t)
	h := gwTokenHandler(t, seededClassifier(t), &capturePublisher{})

	rec := doOfferFromIP(h, ipGwScanner, "unsub9")
	if rec.Code == http.StatusNoContent {
		t.Fatal("an unsubscribe destination was WITHHELD from a scanner-classified IP — CAN-SPAM / RFC 8058 break")
	}
	// The opt-out URL must arrive intact: right host, right path, and the
	// recipient token rendered. (The handler also appends its standard
	// source_id/sub1/sub2/sub3 attribution, as it does for every destination.)
	got := rec.Header().Get("Location")
	wantPrefix := "https://em.discountblog.com/unsubscribe?d=" + subUUID
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("unsubscribe Location = %q, want it to start with %q — a mangled opt-out is a compliance failure too", got, wantPrefix)
	}
	if strings.Contains(got, "{{") {
		t.Fatalf("unsubscribe Location still carries an unrendered placeholder: %q", got)
	}
	// Prove the exemption is what saved it, not a disarmed gateway.
	if rec2 := doOfferFromIP(h, ipGwScanner, "tok001"); rec2.Code != http.StatusNoContent {
		t.Fatalf("control: a money link from the same address returned %d, want 204 — the gateway was not actually enforcing", rec2.Code)
	}
}

// ── 8. THE HANDLER NEVER CONTACTS THE DESTINATION ───────────────────────────
//
// A server-side fetch of the advertiser URL would itself register a click on
// the advertiser's counter — the exact event this gateway exists to suppress.
// Fixture half: the destination is a live server that counts every hit.
func TestOfferGateway_NeverFetchesTheDestination(t *testing.T) {
	var hits int64
	adv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer adv.Close()

	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "enforce" {
				gwEnforce(t)
			} else {
				gwShadow(t)
			}
			atomic.StoreInt64(&hits, 0)

			ipc := seededClassifier(t)
			offer := NewHandlerWithClassifier(&capturePublisher{}, stubDict(map[string]smartLinkEntry{
				"live01": {Destination: adv.URL + "/offer?sub1={{subscriber.id}}", RiskProfile: "low", BrandRoot: "discountblog.com"},
			}), ipc)
			click := NewHandlerWithClassifier(&capturePublisher{}, nil, ipc)

			for _, ip := range []string{ipGwScanner, ipGwUnresolved, ipGwHosting, ipGwUnclassifie} {
				doOfferFromIP(offer, ip, "live01")
				doClickFromIP(click, ip, adv.URL+"/offer")
			}
			if n := atomic.LoadInt64(&hits); n != 0 {
				t.Fatalf("the handler made %d outbound request(s) to the destination — a server-side fetch IS a click", n)
			}
		})
	}
}

// Structural half: nothing on the click path may be able to construct an
// outbound request in the first place. Catches a future "just resolve the
// redirect chain" change that the fixture above would only catch if someone
// remembered to point a test at it.
func TestOfferGateway_NoHTTPClientOnThePath(t *testing.T) {
	banned := []string{
		"http.Get(", "http.Post(", "http.Head(", "http.PostForm(",
		"http.DefaultClient", "http.Client{", "&http.Client",
		"http.NewRequest(", "http.NewRequestWithContext(",
	}
	for _, f := range []string{"handler.go", "gateway.go", "dictionary.go", "click_classifier.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("cannot read %s — the click path must stay scannable: %v", f, err)
		}
		src := string(b)
		for _, bad := range banned {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q — the offer gateway must not be able to reach the destination server-side", f, bad)
			}
		}
	}
}

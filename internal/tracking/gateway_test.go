package tracking

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// =============================================================================
// Offer gateway — the rule, the default, and the five invariants
// =============================================================================
//
// These tests pin BEHAVIOUR, not implementation: what the visitor gets, what we
// publish about it, and which headers ride along. Every one of them fails if the
// gateway starts withholding something it must forward — which is the only
// failure mode on this path that costs money and cannot be retried by the human.

// stubClassifier builds a warm in-memory classifier with no DB, mirroring what
// reloadOnce produces (sorted narrowest-first).
func stubClassifier(t *testing.T, rows map[string]string) *IPClassifier {
	t.Helper()
	c := &IPClassifier{}
	c.loadFn = func(context.Context) ([]ipClassEntry, error) {
		var out []ipClassEntry
		for cidr, class := range rows {
			_, n, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatalf("bad test CIDR %q: %v", cidr, err)
			}
			out = append(out, ipClassEntry{net: n, class: class})
		}
		return out, nil
	}
	if err := c.reloadOnce(context.Background()); err != nil {
		t.Fatalf("reloadOnce: %v", err)
	}
	return c
}

// the seeded shape from cmd/server/ip_classification.go: a blanket hosting /16,
// a confirmed scanner /32 inside it, and an 'unresolved' /32 also inside it.
func seededClassifier(t *testing.T) *IPClassifier {
	t.Helper()
	return stubClassifier(t, map[string]string{
		"135.232.0.0/16":    "hosting",
		"135.232.20.148/32": "scanner",
		"135.232.20.64/32":  "unresolved",
		"74.179.0.0/16":     "hosting",
		"74.179.67.166/32":  "scanner",
	})
}

func offerHandler(t *testing.T, ipc *IPClassifier, pub eventPublisher) *Handler {
	t.Helper()
	return NewHandlerWithClassifier(pub, stubDict(map[string]smartLinkEntry{
		"abc123": {
			Destination: "https://www.cratoolpro.com/BJB4Q5BF/OFFER/",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
		"unsub01": {
			// An unsubscribe destination that reached the smart-link path.
			Destination: "https://em.discountblog.com/unsubscribe/{{subscriber.id}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	}), ipc)
}

func doOfferFromIP(h *Handler, ip, hash string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/o/"+subUUID+"/"+hash+"/"+campUUID, nil)
	req.Host = "t.em.discountblog.com"
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// clickToken builds the /track/click/ token shape: org|campaign|subscriber|email|url
func clickToken(dest string) string {
	return base64.URLEncoding.EncodeToString([]byte(strings.Join([]string{
		"00000000-0000-0000-0000-000000000001", campUUID, subUUID, campUUID, dest,
	}, "|")))
}

func doClickFromIP(h *Handler, ip, dest string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/track/click/"+clickToken(dest)+"/sig", nil)
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// -----------------------------------------------------------------------------
// THE RULE
// -----------------------------------------------------------------------------

// Only 'scanner' withholds. 'unresolved', 'hosting' and no-row-at-all forward,
// on BOTH click paths, with enforcement fully armed. The 'unresolved' case is
// the load-bearing one: those addresses carry real people, and forwarding them
// is the deliberate choice.
func TestGateway_OnlyScannerWithholds_BothPaths(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	cases := []struct {
		name         string
		ip           string
		wantWithheld bool
	}{
		{"confirmed scanner /32", "135.232.20.148", true},
		{"confirmed scanner /32 (second range)", "74.179.67.166", true},
		{"unresolved /32 — carries BOTH scanners and people", "135.232.20.64", false},
		{"hosting /16", "135.232.99.99", false},
		{"no row at all (NULL)", "8.8.8.8", false},
		{"unparseable IP", "not-an-ip", false},
	}

	for _, tc := range cases {
		t.Run("offer/"+tc.name, func(t *testing.T) {
			rec := doOfferFromIP(offerHandler(t, seededClassifier(t), &capturePublisher{}), tc.ip, "abc123")
			if tc.wantWithheld {
				assertWithheld(t, rec)
			} else {
				assertForwardedTo(t, rec, "cratoolpro.com")
			}
		})
		t.Run("click/"+tc.name, func(t *testing.T) {
			h := NewHandlerWithClassifier(&capturePublisher{}, nil, seededClassifier(t))
			rec := doClickFromIP(h, tc.ip, "https://www.cratoolpro.com/BJB4Q5BF/OFFER/")
			if tc.wantWithheld {
				assertWithheld(t, rec)
			} else {
				assertForwardedTo(t, rec, "cratoolpro.com")
			}
		})
	}
}

// Narrowest match wins, exactly as ignite_ip_class() resolves it: a curated /32
// carves its own verdict out of the blanket ownership /16 that contains it.
func TestGateway_NarrowestMatchWins(t *testing.T) {
	c := seededClassifier(t)
	for ip, want := range map[string]string{
		"135.232.20.148": "scanner",
		"135.232.20.64":  "unresolved",
		"135.232.99.99":  "hosting",
		"74.179.67.166":  "scanner",
		"8.8.8.8":        "",
	} {
		if got := c.Classify(ip); got != want {
			t.Errorf("Classify(%s) = %q, want %q", ip, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// DEFAULT IS SHADOW (the ship-it-off requirement)
// -----------------------------------------------------------------------------

// With GATEWAY_ENFORCE unset, a confirmed scanner is FORWARDED — byte-identical
// to a human's response — and only the telemetry marker records what would have
// happened. This is the shipped posture.
func TestGateway_DefaultIsShadow_WithholdsNothing(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "")

	pub := &capturePublisher{}
	rec := doOfferFromIP(offerHandler(t, seededClassifier(t), pub), "135.232.20.148", "abc123")
	assertForwardedTo(t, rec, "cratoolpro.com")

	evt, ok := pub.last()
	if !ok {
		t.Fatal("no telemetry published")
	}
	if evt.GatewayAction != GatewayActionShadowWithheld {
		t.Fatalf("GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionShadowWithheld)
	}
}

// GATEWAY_DISABLED short-circuits everything: no class lookup, no marker, no
// shadow record — and certainly no withholding.
func TestGateway_DisabledEnv_NoScoringAtAll(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")
	t.Setenv(GatewayDisabledEnv, "1")

	pub := &capturePublisher{}
	rec := doOfferFromIP(offerHandler(t, seededClassifier(t), pub), "135.232.20.148", "abc123")
	assertForwardedTo(t, rec, "cratoolpro.com")

	evt, _ := pub.last()
	if evt.GatewayAction != "" {
		t.Fatalf("GatewayAction = %q, want empty when disabled", evt.GatewayAction)
	}
}

// Both flags are read at CALL time, so enforcement flips without a deploy — and
// without rebuilding the handler.
func TestGateway_FlagsReadAtCallTime(t *testing.T) {
	h := offerHandler(t, seededClassifier(t), &capturePublisher{})

	t.Setenv(GatewayEnforceEnv, "")
	assertForwardedTo(t, doOfferFromIP(h, "135.232.20.148", "abc123"), "cratoolpro.com")

	t.Setenv(GatewayEnforceEnv, "1")
	assertWithheld(t, doOfferFromIP(h, "135.232.20.148", "abc123"))

	t.Setenv(GatewayEnforceEnv, "0")
	assertForwardedTo(t, doOfferFromIP(h, "135.232.20.148", "abc123"), "cratoolpro.com")
}

// -----------------------------------------------------------------------------
// INVARIANT 3 — unsubscribe / preference destinations are exempt
// -----------------------------------------------------------------------------

// A confirmed scanner IP, enforcement armed, and the destination is still
// forwarded because it is an unsubscribe or preference link. Withholding one is
// a CAN-SPAM / RFC 8058 problem, so the exemption is checked BEFORE the class
// lookup and cannot be overridden by any classification.
func TestGateway_UnsubscribeAndPreferenceDestinationsAreExempt(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	exempt := []string{
		"https://em.discountblog.com/unsubscribe/abc",
		"https://em.discountblog.com/email-preferences?id=abc",
		"https://affiliateaccesskey.com/opt-out/abc",
		"https://samestoreteam.com/u/abc",
		"https://EM.DISCOUNTBLOG.COM/UNSUBSCRIBE/ABC", // case-insensitive
	}
	for _, dest := range exempt {
		t.Run(dest, func(t *testing.T) {
			h := NewHandlerWithClassifier(&capturePublisher{}, nil, seededClassifier(t))
			rec := doClickFromIP(h, "135.232.20.148", dest)
			if rec.Code == http.StatusNoContent {
				t.Fatalf("exempt destination WITHHELD (CAN-SPAM/RFC 8058 problem): %s", dest)
			}
			if loc := rec.Header().Get("Location"); loc == "" {
				t.Fatalf("exempt destination not forwarded: no Location header")
			}
		})
	}

	// Same through the /o/ offer path, where the destination is resolved from
	// the dictionary rather than the token.
	h := offerHandler(t, seededClassifier(t), &capturePublisher{})
	rec := doOfferFromIP(h, "135.232.20.148", "unsub01")
	if rec.Code == http.StatusNoContent {
		t.Fatal("/o/ unsubscribe destination WITHHELD")
	}

	// And a plain money link from the same address IS withheld — proving the
	// exemption is what saved the ones above, not a disarmed gateway.
	assertWithheld(t, doOfferFromIP(h, "135.232.20.148", "abc123"))
}

// -----------------------------------------------------------------------------
// INVARIANT 2 — no-store on every response
// -----------------------------------------------------------------------------

// A cached withhold replayed to a human is silent and unrecoverable, so the
// headers ride on FORWARDED responses too — and on the fallbacks and bad links.
func TestGateway_NoStoreHeadersOnEveryResponse(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")
	h := offerHandler(t, seededClassifier(t), &capturePublisher{})

	recs := map[string]*httptest.ResponseRecorder{
		"forwarded offer": doOfferFromIP(h, "8.8.8.8", "abc123"),
		"withheld offer":  doOfferFromIP(h, "135.232.20.148", "abc123"),
		"dictionary miss": doOfferFromIP(h, "8.8.8.8", "nosuchhash"),
		"bad hash":        doOfferFromIP(h, "8.8.8.8", "!!"),
		"forwarded click": doClickFromIP(h, "8.8.8.8", "https://www.cratoolpro.com/x/"),
		"withheld click":  doClickFromIP(h, "135.232.20.148", "https://www.cratoolpro.com/x/"),
	}
	// A structurally broken click token: still a gateway response.
	badReq := httptest.NewRequest(http.MethodGet, "/track/click/@@@notbase64@@@/sig", nil)
	badRec := httptest.NewRecorder()
	h.Routes().ServeHTTP(badRec, badReq)
	recs["bad click token"] = badRec

	for name, rec := range recs {
		t.Run(name, func(t *testing.T) {
			if got := rec.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, private, max-age=0" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := rec.Header().Get("Pragma"); got != "no-cache" {
				t.Errorf("Pragma = %q", got)
			}
			if got := rec.Header().Get("Expires"); got != "0" {
				t.Errorf("Expires = %q", got)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// INVARIANT 4 — telemetry for withheld requests, distinguishable
// -----------------------------------------------------------------------------

// We suppress the ADVERTISER hop, never our own visibility: a withheld click is
// still an event, still carries subscriber/campaign/IP, and is marked.
func TestGateway_WithheldRequestIsStillPublished(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	pub := &capturePublisher{}
	assertWithheld(t, doOfferFromIP(offerHandler(t, seededClassifier(t), pub), "135.232.20.148", "abc123"))

	evt, ok := pub.last()
	if !ok {
		t.Fatal("withheld request published NO telemetry — we lost our own visibility")
	}
	if evt.GatewayAction != GatewayActionWithheld {
		t.Fatalf("GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionWithheld)
	}
	if evt.SubscriberID != subUUID || evt.CampaignID != campUUID {
		t.Fatalf("withheld event lost attribution: sub=%q camp=%q", evt.SubscriberID, evt.CampaignID)
	}
	if evt.EventType != EventClick {
		t.Fatalf("EventType = %q, want %q", evt.EventType, EventClick)
	}

	// The click path publishes the same marker.
	pub2 := &capturePublisher{}
	h := NewHandlerWithClassifier(pub2, nil, seededClassifier(t))
	assertWithheld(t, doClickFromIP(h, "135.232.20.148", "https://www.cratoolpro.com/x/"))
	if evt2, _ := pub2.last(); evt2.GatewayAction != GatewayActionWithheld {
		t.Fatalf("click GatewayAction = %q, want %q", evt2.GatewayAction, GatewayActionWithheld)
	}
}

// A forwarded event stays byte-identical on the wire to a pre-gateway one:
// gateway_action is omitempty, so the SQS consumer sees exactly what it saw
// before.
func TestGateway_ForwardedEventWireFormatUnchanged(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	pub := &capturePublisher{}
	assertForwardedTo(t, doOfferFromIP(offerHandler(t, seededClassifier(t), pub), "8.8.8.8", "abc123"), "cratoolpro.com")

	evt, _ := pub.last()
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "gateway_action") {
		t.Fatalf("forwarded event carries gateway_action on the wire: %s", b)
	}
}

// -----------------------------------------------------------------------------
// WITHHELD RESPONSE SHAPE
// -----------------------------------------------------------------------------

// 204, no Location, no body — and specifically NOT the brand site. Serving a
// scanner our own content instead of the advertiser's is the 2026-07-22
// cloaking mistake.
func TestGateway_WithheldResponseServesNothing(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	rec := doOfferFromIP(offerHandler(t, seededClassifier(t), &capturePublisher{}), "135.232.20.148", "abc123")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("withheld response carries Location: %q", loc)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("withheld response carries a body: %q", body)
	}
	if strings.Contains(rec.Body.String(), "discountblog.com") {
		t.Fatal("withheld response leaked the brand site (cloaking)")
	}
}

// -----------------------------------------------------------------------------
// INVARIANT 5 — fail open
// -----------------------------------------------------------------------------

// Every degraded state forwards. Enforcement is armed throughout, so any
// failure here is a real human's click being withheld by an accident.
func TestGateway_FailsOpen(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")

	t.Run("nil classifier (NewHandler / no DATABASE_URL)", func(t *testing.T) {
		h := offerHandler(t, nil, &capturePublisher{})
		assertForwardedTo(t, doOfferFromIP(h, "135.232.20.148", "abc123"), "cratoolpro.com")
	})

	t.Run("nil-receiver Decide", func(t *testing.T) {
		var c *IPClassifier
		if d := c.Decide("135.232.20.148", "https://x/"); d.Withhold {
			t.Fatal("nil classifier withheld")
		}
	})

	t.Run("classifier built with nil db", func(t *testing.T) {
		c := NewIPClassifier(nil, 0)
		defer c.Close()
		if got := c.Classify("135.232.20.148"); got != "" {
			t.Fatalf("nil-db Classify = %q, want empty", got)
		}
		if c.Len() != 0 {
			t.Fatalf("nil-db Len = %d", c.Len())
		}
	})

	t.Run("empty classifier set", func(t *testing.T) {
		h := offerHandler(t, &IPClassifier{}, &capturePublisher{})
		assertForwardedTo(t, doOfferFromIP(h, "135.232.20.148", "abc123"), "cratoolpro.com")
	})

	t.Run("empty IP string", func(t *testing.T) {
		if got := seededClassifier(t).Classify(""); got != "" {
			t.Fatalf("Classify(\"\") = %q", got)
		}
	})
}

// A failed load keeps the previous set; an EMPTY load keeps it too. Clearing on
// either would change behaviour silently on a bad boot.
func TestGateway_FailedOrEmptyLoadKeepsPreviousSet(t *testing.T) {
	c := seededClassifier(t)
	before := c.Len()
	if before == 0 {
		t.Fatal("fixture is empty")
	}

	c.loadFn = func(context.Context) ([]ipClassEntry, error) {
		return nil, context.DeadlineExceeded
	}
	if err := c.reloadOnce(context.Background()); err == nil {
		t.Fatal("expected the load error to be returned")
	}
	if c.Len() != before || c.Classify("135.232.20.148") != ClassScanner {
		t.Fatalf("failed load mutated the set: len=%d", c.Len())
	}

	c.loadFn = func(context.Context) ([]ipClassEntry, error) { return nil, nil }
	if err := c.reloadOnce(context.Background()); err != nil {
		t.Fatalf("empty load returned an error: %v", err)
	}
	if c.Len() != before || c.Classify("135.232.20.148") != ClassScanner {
		t.Fatalf("empty load cleared the set: len=%d", c.Len())
	}
}

// -----------------------------------------------------------------------------
// NO REGRESSION IN THE DEFAULT POSTURE
// -----------------------------------------------------------------------------

// With the gateway shipped OFF, scanner and human get byte-identical treatment
// on the offer path — the no-cloaking guarantee the /o/ contract already made.
func TestGateway_ShadowKeepsNoCloakingGuarantee(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "")
	h := offerHandler(t, seededClassifier(t), &capturePublisher{})

	scanner := doOfferFromIP(h, "135.232.20.148", "abc123")
	human := doOfferFromIP(h, "8.8.8.8", "abc123")

	if scanner.Code != human.Code {
		t.Fatalf("status differs by visitor: scanner=%d human=%d", scanner.Code, human.Code)
	}
	if scanner.Header().Get("Location") != human.Header().Get("Location") {
		t.Fatalf("destination differs by visitor: scanner=%q human=%q",
			scanner.Header().Get("Location"), human.Header().Get("Location"))
	}
	if scanner.Body.String() != human.Body.String() {
		t.Fatal("body differs by visitor")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func assertWithheld(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (withheld); Location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("withheld response carries Location: %q", loc)
	}
}

func assertForwardedTo(t *testing.T, rec *httptest.ResponseRecorder, wantHostSubstr string) {
	t.Helper()
	if rec.Code == http.StatusNoContent {
		t.Fatal("request was WITHHELD but must have been forwarded")
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("no Location header; status=%d", rec.Code)
	}
	if !strings.Contains(loc, wantHostSubstr) {
		t.Fatalf("forwarded to %q, want a URL containing %q", loc, wantHostSubstr)
	}
}

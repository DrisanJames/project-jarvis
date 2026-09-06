package tracking

// =============================================================================
// GATE 2 — session-shape fanout. The regression suite.
// =============================================================================
//
// gateway_test.go and offer_gateway_test.go pin GATE 1 (the IP-class rule).
// This file pins GATE 2 and nothing else, and it exists because gate 2 is the
// first thing on this path that can withhold a click from an address we have
// NOT classified as a scanner. Every failure mode here costs a real person a
// click they cannot retry, so the four load-bearing tests below encode the
// MEASURED production shape (gateway.go:56-72), not a preference:
//
//	residential (known humans)  7,044 sessions  1.27 links  76.4% single  1850s span
//	Microsoft                  19,840 sessions  1.89 links  30.5% single   547s span
//	AWS (already blocked)       6,162 sessions  2.16 links  10.7% single    62s span
//
//	1. UNCLASSIFIED IPs ARE NEVER FANOUT-GATED — 76.4% of them are single-link
//	   real humans. Ten links in five seconds still forwards.
//	2. THE FIRST THREE LINKS ALWAYS FORWARD — humans average 1.27 distinct
//	   links, so the threshold is what guarantees a one-click human is untouched.
//	3. SLOW FANOUT FORWARDS — humans span 1,850s, sweeps 62s. Spread is the
//	   separator; four links over an afternoon is a person reading.
//	4. DISABLED IS THE SHIPPED DEFAULT — GATEWAY_FANOUT_ENABLED unset withholds
//	   nothing, ever.
//
// Everything asserts the OBSERVABLE HTTP OUTCOME through httptest against
// Handler.Routes() — status, Location, headers, published telemetry — never
// "function X was called". Helpers are reused from gateway_test.go and
// offer_redirect_test.go (stubClassifier / stubDict / capturePublisher /
// uaBrowser) rather than re-implemented.
//
// ── HOW THESE TESTS STAY HONEST ──────────────────────────────────────────────
//
// Tests 1, 2 and 4 deliberately DO NOT set GATEWAY_FANOUT_LINKS or
// GATEWAY_FANOUT_ENABLED-adjacent knobs: they run on the SHIPPED DEFAULTS
// (defaultFanoutLinks = 4, defaultFanoutWindow = 60s, gate off when unset,
// gateway.go:141-144). A test that pins the threshold with an env var cannot
// detect a change to the default, which is the only value production runs on.
// Test 3 does set the window, to 1s, purely so the suite stays in-process and
// fast; it is broken by REMOVING the window check, which no env value can hide.
//
// Every "must forward" test carries a SAME-RUN POSITIVE CONTROL proving the
// gate was actually armed in that run. Without it, a gate that silently never
// fires would make all four pass.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── fixture ─────────────────────────────────────────────────────────────────

const (
	// The two classes gate 2 may act on (gateway.go:117-121).
	ipFanUnresolved = "135.232.20.64"  // 'unresolved' /32
	ipFanHosting    = "135.232.99.99"  // the blanket 'hosting' /16
	ipFanScanner    = "135.232.20.148" // 'scanner' /32 — gate 1's territory
	// The two classes gate 2 must NEVER act on. ipFanNull is the residential
	// population: no row at all, the NULL of ignite_ip_class().
	ipFanNull        = "8.8.8.8"
	ipFanResidential = "71.199.4.5" // an explicit 'residential-or-mobile' row
)

// fanoutClassifier is the seeded shape plus one explicit
// 'residential-or-mobile' prefix, so test 1 can cover BOTH ways an address
// reaches the never-gate population: no row at all, and a row that names it.
func fanoutClassifier(t *testing.T) *IPClassifier {
	t.Helper()
	return stubClassifier(t, map[string]string{
		"135.232.0.0/16":    "hosting",
		"135.232.20.148/32": "scanner",
		"135.232.20.64/32":  "unresolved",
		"71.199.0.0/16":     "residential-or-mobile",
	})
}

// fanHash returns the hash for the nth distinct money link ("fan001"…),
// matching handler.go:238 hashPattern ^[A-Za-z0-9]{6,20}$.
func fanHash(n int) string { return fmt.Sprintf("fan%03d", n) }

// fanoutHandler serves 12 DISTINCT advertiser destinations plus an exempt
// unsubscribe destination. Each link carries the attribution placeholders, so
// what gate 2 counts is the fully rendered destination the visitor would have
// been handed — the same string the advertiser would have seen.
func fanoutHandler(t *testing.T, ipc *IPClassifier, pub eventPublisher) *Handler {
	t.Helper()
	rows := map[string]smartLinkEntry{
		"fanuns": {
			Destination: "https://em.discountblog.com/unsubscribe?d={{subscriber.id}}",
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		},
	}
	for n := 1; n <= 12; n++ {
		rows[fanHash(n)] = smartLinkEntry{
			Destination: fmt.Sprintf(
				"https://www.cratoolpro.com/BJB4Q5BF/LINK%02d/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}", n),
			RiskProfile: "low",
			BrandRoot:   "discountblog.com",
		}
	}
	return NewHandlerWithClassifier(pub, stubDict(rows), ipc)
}

// fanSeq mints a fresh (subscriber, campaign) per session. The counter is
// process-local and keyed on that pair (gateway.go record()), so tests that
// shared a session would bleed into each other and pass or fail for reasons
// that have nothing to do with the assertion.
var fanSeq int64

func fanSession() (sub, camp string) {
	n := atomic.AddInt64(&fanSeq, 1)
	return fmt.Sprintf("fa000000-0000-4000-8000-%012d", n),
		fmt.Sprintf("fc000000-0000-4000-8000-%012d", n)
}

func doFan(h *Handler, ip, sub, hash, camp string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/o/"+sub+"/"+hash+"/"+camp, nil)
	req.Host = "t.em.discountblog.com"
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// fanoutArmed puts the process in the FULLY ARMED posture and NOTHING more:
// gate 1 enforcing, gate 2 enabled, and the threshold and window left at their
// shipped defaults. See the header note on why the defaults are not pinned by
// env here.
func fanoutArmed(t *testing.T) {
	t.Helper()
	for _, k := range []string{GatewayDisabledEnv, GatewayFanoutLinksEnv, GatewayFanoutWindowEnv} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv(GatewayEnforceEnv, "1")
	t.Setenv(GatewayFanoutEnabledEnv, "1")
}

// assertFanForwarded pins the full forward outcome for link n: 302, the
// destination for THAT link (not merely "some Location"), and no unrendered
// placeholder. Uses Errorf so a run reports every link that broke, not just the
// first.
func assertFanForwarded(t *testing.T, rec *httptest.ResponseRecorder, n int, why string) {
	t.Helper()
	if rec.Code == http.StatusNoContent {
		t.Errorf("link %d WITHHELD (204) — %s", n, why)
		return
	}
	if rec.Code != http.StatusFound {
		t.Errorf("link %d: status = %d, want 302 — %s", n, rec.Code, why)
		return
	}
	loc := rec.Header().Get("Location")
	want := fmt.Sprintf("/LINK%02d/", n)
	if !strings.Contains(loc, want) {
		t.Errorf("link %d: Location = %q, want it to contain %q — %s", n, loc, want, why)
	}
	if strings.Contains(loc, "{{") {
		t.Errorf("link %d: Location carries an unrendered placeholder: %q", n, loc)
	}
}

// assertGate2IsArmed is the same-run proof that gate 2 was
// actually armed and firing. It runs a FRESH session so it cannot disturb the
// session under test. Every "must forward" test calls it.
func assertGate2IsArmed(t *testing.T, h *Handler) {
	t.Helper()
	sub, camp := fanSession()
	for n := 1; n <= 4; n++ {
		rec := doFan(h, ipFanUnresolved, sub, fanHash(n), camp)
		if n < 4 && rec.Code != http.StatusFound {
			t.Fatalf("control: link %d of an unresolved fast burst = %d, want 302", n, rec.Code)
		}
		if n == 4 && rec.Code != http.StatusNoContent {
			t.Fatalf("CONTROL FAILED: link 4 of a fast 4-link burst from an 'unresolved' IP returned %d, want 204. "+
				"Gate 2 was NOT firing in this run, so the forwards asserted above prove nothing.", rec.Code)
		}
	}
}

// ── 1. UNCLASSIFIED IPs ARE NEVER FANOUT-GATED ──────────────────────────────
//
// Residential addresses classify as NULL (no row) — 7,044 sessions, 1.27 links
// each, 76.4% single-link, 1,850s median span. They are the provably-human
// population. Gate 2 acts on 'unresolved' and 'hosting' ONLY (gateway.go:117),
// so even ten distinct links in five seconds from an unclassified address must
// forward. If this breaks we gate the people we can prove are people.
func TestOfferFanout_UnclassifiedIPIsNeverFanoutGated(t *testing.T) {
	cases := []struct{ name, ip, wantClass string }{
		{"NULL — no classification row at all (the residential population)", ipFanNull, ""},
		{"explicit 'residential-or-mobile' row", ipFanResidential, "residential-or-mobile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fanoutArmed(t)
			ipc := fanoutClassifier(t)

			// Pin the class the fixture resolves, so this cannot pass for the
			// wrong reason (e.g. the address quietly matching a gated prefix).
			if got := ipc.Classify(c.ip); got != c.wantClass {
				t.Fatalf("fixture drift: Classify(%s) = %q, want %q", c.ip, got, c.wantClass)
			}

			h := fanoutHandler(t, ipc, &capturePublisher{})
			sub, camp := fanSession()

			// Ten DISTINCT links, back to back, well inside the 60s window —
			// 8x the threshold and a shape no human produces. Still forwards.
			for n := 1; n <= 10; n++ {
				assertFanForwarded(t, doFan(h, c.ip, sub, fanHash(n), camp), n,
					"class "+c.wantClass+" is outside gate 2 entirely: 76.4% of this population is single-link humans")
			}

			assertGate2IsArmed(t, h)
		})
	}
}

// ── 2. THE FIRST THREE LINKS ALWAYS FORWARD ─────────────────────────────────
//
// Links 1, 2 and 3 of any session forward; only the 4th and beyond can be
// withheld. Humans average 1.27 distinct links, so this threshold IS the
// guarantee that a person who clicks once — or twice, or three times — is never
// touched. Asserted on EACH of the first three individually, on both gated
// classes, running on the SHIPPED default threshold (no env override).
func TestOfferFanout_FirstThreeLinksAlwaysForward(t *testing.T) {
	classes := []struct{ name, ip, wantClass string }{
		{"unresolved", ipFanUnresolved, ClassUnresolved},
		{"hosting", ipFanHosting, ClassHosting},
	}
	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			fanoutArmed(t)
			ipc := fanoutClassifier(t)
			if got := ipc.Classify(c.ip); got != c.wantClass {
				t.Fatalf("fixture drift: Classify(%s) = %q, want %q", c.ip, got, c.wantClass)
			}
			h := fanoutHandler(t, ipc, &capturePublisher{})
			sub, camp := fanSession()

			// Fired as fast as the process allows — worst case for the gate,
			// and still inside the 60s window. Each of the three is its own
			// assertion, because "the first three forward" is three separate
			// promises to three separate humans.
			assertFanForwarded(t, doFan(h, c.ip, sub, fanHash(1), camp), 1,
				"link 1 is the ONLY link 87% of real sessions ever produce")
			assertFanForwarded(t, doFan(h, c.ip, sub, fanHash(2), camp), 2,
				"link 2 is inside the human envelope (1.27 links avg)")
			assertFanForwarded(t, doFan(h, c.ip, sub, fanHash(3), camp), 3,
				"link 3 is the last link before the threshold; withholding it costs human clicks")

			// The boundary itself: link 4 is where the gate is allowed to act,
			// and this doubles as the proof the gate was armed for links 1-3.
			if rec := doFan(h, c.ip, sub, fanHash(4), camp); rec.Code != http.StatusNoContent {
				t.Fatalf("CONTROL FAILED: link 4 returned %d, want 204. The gate never fired in this run, "+
					"so the three forwards above prove nothing about the threshold.", rec.Code)
			}
		})
	}
}

// ── 3. SLOW FANOUT FORWARDS ─────────────────────────────────────────────────
//
// Four or more distinct links spread BEYOND the window is a person reading over
// time (humans: 1,850s median span), not a sweep (AWS: 62s). The window is set
// to 1s here only so the suite stays in-process; the property under test is that
// SPREAD decides, whatever the window's value.
//
// Two shapes, because gate 2 has TWO mechanisms that can expire a window and
// only one of them is the window check itself:
//
//	record()      resets the distinct set when now - startedAt > window
//	sweepLocked() deletes a session idle longer than one window
//
// The first subtest below is the plain human shape — it goes quiet between
// clicks, so the idle sweep alone would carry it and it does NOT isolate the
// window check. The second keeps the session continuously ALIVE with repeat
// fetches of the link it is already on (the 8.4x duplicate-logging shape we
// actually see), so the sweep never fires and the ONLY thing that can stop the
// distinct set accumulating is the window check. Verified: deleting the window
// check leaves subtest one green and turns subtest two red.
func TestOfferFanout_SlowFanoutForwards(t *testing.T) {
	const window = time.Second

	t.Run("distinct links spaced beyond the window", func(t *testing.T) {
		fanoutArmed(t)
		t.Setenv(GatewayFanoutWindowEnv, "1")

		h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
		sub, camp := fanSession()

		for n := 1; n <= 4; n++ {
			if n > 1 {
				time.Sleep(1200 * time.Millisecond)
			}
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n,
				"4 distinct links spaced beyond the window is a person reading over time (humans span 1,850s), not a sweep (62s)")
		}

		assertGate2IsArmed(t, h)
	})

	// The load-bearing shape. Between each distinct link the session keeps
	// fetching the link it is ALREADY on, so it is never idle and the sweep
	// never touches it — every 1.35s hop still lands outside the 1s window that
	// began with the previous distinct link, so the distinct count never climbs
	// past 1. This is a real person: reading one page, following the next link
	// a minute later.
	t.Run("session stays alive between links (isolates the window check)", func(t *testing.T) {
		fanoutArmed(t)
		t.Setenv(GatewayFanoutWindowEnv, "1")

		h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
		sub, camp := fanSession()

		for n := 1; n <= 4; n++ {
			if n > 1 {
				// Three heartbeats on the PREVIOUS link, ~350ms apart. Each is
				// a repeat, so it adds no distinct destination, but it keeps
				// lastAt fresh so sweepLocked cannot evict the session.
				for i := 0; i < 3; i++ {
					time.Sleep(350 * time.Millisecond)
					if rec := doFan(h, ipFanUnresolved, sub, fanHash(n-1), camp); rec.Code != http.StatusFound {
						t.Fatalf("heartbeat on link %d: status = %d, want 302", n-1, rec.Code)
					}
				}
				time.Sleep(300 * time.Millisecond) // total hop ~1.35s > the 1s window
			}
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n,
				"each link opened more than one window after the last: the distinct set must restart, "+
					"or a person reading over an afternoon is gated as a sweep")
		}

		assertGate2IsArmed(t, h)
	})

	// The same four links, same class, same window — fired fast. Proves the
	// window value took effect and the threshold was reachable in this run, so
	// the forwards above are about TIMING and nothing else.
	t.Run("control — the same 4 links inside one window ARE withheld", func(t *testing.T) {
		fanoutArmed(t)
		t.Setenv(GatewayFanoutWindowEnv, "1")

		h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
		sub, camp := fanSession()
		for n := 1; n <= 4; n++ {
			rec := doFan(h, ipFanUnresolved, sub, fanHash(n), camp)
			if n < 4 && rec.Code != http.StatusFound {
				t.Fatalf("link %d: status = %d, want 302", n, rec.Code)
			}
			if n == 4 && rec.Code != http.StatusNoContent {
				t.Fatalf("CONTROL FAILED: 4 distinct links inside one %v window returned %d, want 204. "+
					"The gate was not firing, so the slow-fanout forwards prove nothing.", window, rec.Code)
			}
		}
	})
}

// ── 4. DISABLED IS THE SHIPPED DEFAULT ──────────────────────────────────────
//
// GATEWAY_FANOUT_ENABLED unset means the gate does not run at all — no counter,
// no map, no withholding — even for the worst-looking session available: ten
// distinct links in a couple of seconds from an 'unresolved' address. This is
// what ships, so it is what must be true on the day it deploys.
func TestOfferFanout_DisabledIsTheShippedDefault(t *testing.T) {
	// clear leaves every gateway flag unset, then arms only what the subtest
	// names. GATEWAY_FANOUT_ENABLED is never set in this test.
	clearGatewayEnv := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{GatewayEnforceEnv, GatewayDisabledEnv,
			GatewayFanoutEnabledEnv, GatewayFanoutLinksEnv, GatewayFanoutWindowEnv} {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}

	// The sharpest case: gate 1 fully enforcing, so the gateway IS running and
	// IS withholding — and gate 2 still does nothing, because its own flag is
	// unset. Anything withheld here is withheld by a gate nobody armed.
	t.Run("gate 1 enforcing, GATEWAY_FANOUT_ENABLED unset", func(t *testing.T) {
		clearGatewayEnv(t)
		t.Setenv(GatewayEnforceEnv, "1")

		pub := &capturePublisher{}
		h := fanoutHandler(t, fanoutClassifier(t), pub)
		sub, camp := fanSession()

		for n := 1; n <= 10; n++ {
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n,
				"GATEWAY_FANOUT_ENABLED is unset — the shape gate must not run at all")
		}

		// Nothing may be MARKED as a fanout either: a shadow_withheld_fanout
		// row here would mean the counter ran and allocated behind an unarmed
		// flag.
		pub.mu.Lock()
		for i, e := range pub.events {
			if strings.Contains(e.GatewayAction, "fanout") {
				pub.mu.Unlock()
				t.Fatalf("event %d carries GatewayAction=%q with the gate unset — gate 2 ran anyway", i, e.GatewayAction)
			}
		}
		pub.mu.Unlock()

		// Control: gate 1 really was armed in this run.
		if rec := doFan(h, ipFanScanner, sub, fanHash(11), camp); rec.Code != http.StatusNoContent {
			t.Fatalf("CONTROL FAILED: a 'scanner' IP returned %d, want 204. The gateway was not enforcing at all, "+
				"so the forwards above prove nothing about the fanout flag.", rec.Code)
		}
	})

	// The literal shipped posture: no gateway env vars set anywhere.
	t.Run("no gateway flags set at all", func(t *testing.T) {
		clearGatewayEnv(t)
		h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
		sub, camp := fanSession()
		for n := 1; n <= 10; n++ {
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n,
				"shipped default: nothing is withheld")
		}
	})

	// Arming is opt-IN: a deploy that sets GATEWAY_FANOUT_ENABLED=0 or =false
	// to mean "off" must not withhold.
	t.Run("non-truthy values are still off", func(t *testing.T) {
		for _, v := range []string{"0", "false", "off", "no", "  ", "maybe", "shadow"} {
			t.Run("GATEWAY_FANOUT_ENABLED="+strings.ReplaceAll(v, " ", "_"), func(t *testing.T) {
				clearGatewayEnv(t)
				t.Setenv(GatewayEnforceEnv, "1")
				t.Setenv(GatewayFanoutEnabledEnv, v)

				h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
				sub, camp := fanSession()
				for n := 1; n <= 6; n++ {
					if rec := doFan(h, ipFanUnresolved, sub, fanHash(n), camp); rec.Code == http.StatusNoContent {
						t.Fatalf("GATEWAY_FANOUT_ENABLED=%q WITHHELD link %d — only a truthy value may arm gate 2", v, n)
					}
				}
			})
		}
	})
}

// ── REPEATS OF ONE DESTINATION ARE NOT A FANOUT ─────────────────────────────
//
// The measured duplicate-logging rate on this path is 8.4x: one human click
// arrives as several fetches of the SAME link. If repeats accumulated, every
// ordinary human click would trip the gate on its own. Eight fetches of one
// link is one distinct destination, forever.
func TestOfferFanout_RepeatsOfSameDestinationDoNotAccumulate(t *testing.T) {
	fanoutArmed(t)
	h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
	sub, camp := fanSession()

	var first string
	for i := 1; i <= 8; i++ {
		rec := doFan(h, ipFanUnresolved, sub, fanHash(1), camp)
		if rec.Code != http.StatusFound {
			t.Fatalf("fetch %d of ONE link: status = %d, want 302 — repeats are duplicate logging (8.4x measured), not a fanout", i, rec.Code)
		}
		loc := rec.Header().Get("Location")
		if i == 1 {
			first = loc
		} else if loc != first {
			t.Fatalf("fetch %d handed off a different destination:\n got %q\nwant %q", i, loc, first)
		}
	}

	// And the same session tips the moment the destinations are DISTINCT:
	// links 2 and 3 forward, link 4 is withheld. This proves the eight
	// forwards above came from de-duplication, not from a dead gate.
	for n := 2; n <= 3; n++ {
		if rec := doFan(h, ipFanUnresolved, sub, fanHash(n), camp); rec.Code != http.StatusFound {
			t.Fatalf("distinct link %d: status = %d, want 302", n, rec.Code)
		}
	}
	if rec := doFan(h, ipFanUnresolved, sub, fanHash(4), camp); rec.Code != http.StatusNoContent {
		t.Fatalf("CONTROL FAILED: the 4th DISTINCT destination in the same session returned %d, want 204 — "+
			"the gate was not firing, so the repeat forwards above prove nothing.", rec.Code)
	}
}

// ── THE COUNTER IS SCOPED PER (SUBSCRIBER, CAMPAIGN) ────────────────────────
//
// A global or per-IP counter would gate every later visitor behind one sweep on
// a shared address. Both halves of the key are load-bearing: a different
// subscriber, and the same subscriber on a different campaign, each start at
// link 1.
func TestOfferFanout_CounterIsScopedPerSubscriberAndCampaign(t *testing.T) {
	fanoutArmed(t)
	h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})

	// Trip one session on the shared 'unresolved' address.
	sub, camp := fanSession()
	for n := 1; n <= 4; n++ {
		doFan(h, ipFanUnresolved, sub, fanHash(n), camp)
	}
	if rec := doFan(h, ipFanUnresolved, sub, fanHash(5), camp); rec.Code != http.StatusNoContent {
		t.Fatalf("setup: tripped session link 5 = %d, want 204", rec.Code)
	}

	t.Run("a different subscriber on the same address and campaign starts fresh", func(t *testing.T) {
		other, _ := fanSession()
		assertFanForwarded(t, doFan(h, ipFanUnresolved, other, fanHash(1), camp), 1,
			"a sweep on a shared address must not gate the next person behind it")
	})

	t.Run("the same subscriber on a different campaign starts fresh", func(t *testing.T) {
		_, otherCamp := fanSession()
		assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(1), otherCamp), 1,
			"a session is (subscriber, campaign); yesterday's send must not gate today's")
	})
}

// ── THE UNSUBSCRIBE EXEMPTION WINS OVER FANOUT ──────────────────────────────
//
// Invariant 3 is checked before any classification AND before the counter, so
// an opt-out is never withheld and never counted. Asserted at the 8th link of a
// session that is already deep into withholding, from both a scanner-class and
// a fanout-tripped address. Withholding an opt-out is a CAN-SPAM / RFC 8058
// break, not a lost conversion.
func TestOfferFanout_UnsubscribeExemptionWinsOverFanout(t *testing.T) {
	for _, c := range []struct{ name, ip string }{
		{"scanner-class IP (gate 1 withholding)", ipFanScanner},
		{"unresolved IP past the fanout threshold (gate 2 withholding)", ipFanUnresolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			fanoutArmed(t)
			h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
			sub, camp := fanSession()

			// Seven distinct money links first — by link 7 this session is
			// being withheld by one gate or the other.
			for n := 1; n <= 7; n++ {
				doFan(h, c.ip, sub, fanHash(n), camp)
			}
			if rec := doFan(h, c.ip, sub, fanHash(8), camp); rec.Code != http.StatusNoContent {
				t.Fatalf("setup: the 8th money link returned %d, want 204 — this session is not being withheld, "+
					"so the exemption below would prove nothing.", rec.Code)
			}

			// The 8th link is an unsubscribe. It forwards, intact.
			rec := doFan(h, c.ip, sub, "fanuns", camp)
			if rec.Code == http.StatusNoContent {
				t.Fatal("an unsubscribe destination was WITHHELD deep inside a withheld session — CAN-SPAM / RFC 8058 break")
			}
			got := rec.Header().Get("Location")
			if !strings.HasPrefix(got, "https://em.discountblog.com/unsubscribe?d="+sub) {
				t.Fatalf("unsubscribe Location = %q, want it to start with the real opt-out URL — a mangled opt-out is a compliance failure too", got)
			}
			if strings.Contains(got, "{{") {
				t.Fatalf("unsubscribe Location carries an unrendered placeholder: %q", got)
			}
		})
	}
}

// ── TELEMETRY: A FANOUT WITHHOLD IS PUBLISHED, AND IS TELLABLE APART ────────
//
// Invariant 4 extends to gate 2: we suppress the ADVERTISER hop, never our own
// visibility. The requirement is a RELATION — a shape withhold must be
// separable from an IP-class withhold in every downstream report — so it is
// asserted on the pair, in one run, with the wire values pinned literally
// (a rename of the constant's VALUE silently breaks those reports).
func TestOfferFanout_WithheldTelemetryIsDistinguishableFromIPClass(t *testing.T) {
	fanoutArmed(t)
	pub := &capturePublisher{}
	h := fanoutHandler(t, fanoutClassifier(t), pub)
	sub, camp := fanSession()

	for n := 1; n <= 4; n++ {
		doFan(h, ipFanUnresolved, sub, fanHash(n), camp)
	}
	if rec := doFan(h, ipFanUnresolved, sub, fanHash(5), camp); rec.Code != http.StatusNoContent {
		t.Fatalf("setup: link 5 = %d, want 204", rec.Code)
	}
	// An IP-class withhold in the SAME run, to compare against.
	if rec := doFan(h, ipFanScanner, sub, fanHash(6), camp); rec.Code != http.StatusNoContent {
		t.Fatalf("setup: scanner link = %d, want 204", rec.Code)
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.events) != 6 {
		t.Fatalf("published %d events, want 6 — a withheld click is still OUR data", len(pub.events))
	}
	forwarded := pub.events[0] // link 1, forwarded
	fanout := pub.events[4]    // link 5, withheld by shape
	ipClass := pub.events[5]   // the scanner, withheld by class

	if forwarded.GatewayAction != "" {
		t.Errorf("forwarded GatewayAction = %q, want \"\" so a normal click stays byte-identical on the wire", forwarded.GatewayAction)
	}
	if fanout.GatewayAction == ipClass.GatewayAction {
		t.Fatalf("a shape withhold and an IP-class withhold carry the SAME GatewayAction %q — "+
			"the two gates are indistinguishable in every downstream report", fanout.GatewayAction)
	}
	if fanout.GatewayAction != GatewayActionWithheldFanout {
		t.Errorf("fanout GatewayAction = %q, want GatewayActionWithheldFanout", fanout.GatewayAction)
	}
	// The literal wire value, pinned separately: downstream reports key on the
	// STRING, so renaming the constant's value breaks them silently.
	if fanout.GatewayAction != "withheld_fanout" {
		t.Errorf("fanout GatewayAction wire value = %q, want %q", fanout.GatewayAction, "withheld_fanout")
	}
	if ipClass.GatewayAction != GatewayActionWithheld {
		t.Errorf("IP-class GatewayAction = %q, want %q", ipClass.GatewayAction, GatewayActionWithheld)
	}
	// The suppressed click keeps its attribution, or it is not usable evidence.
	if fanout.SubscriberID != sub || fanout.CampaignID != camp || fanout.LinkURL == "" {
		t.Errorf("fanout-withheld event lost attribution: sub=%q camp=%q url=%q",
			fanout.SubscriberID, fanout.CampaignID, fanout.LinkURL)
	}
	if fanout.EventType != EventClick {
		t.Errorf("fanout-withheld EventType = %q, want %q", fanout.EventType, EventClick)
	}
}

// With gate 2 armed but enforcement NOT armed, a shape hit is shadow-recorded
// and FORWARDED — and its marker is still separable from gate 1's shadow.
// Shadow is how this gate will be calibrated in production, so a shadow that
// withholds would cost real clicks the hour it deploys.
func TestOfferFanout_ShadowRecordsButForwards(t *testing.T) {
	for _, k := range []string{GatewayEnforceEnv, GatewayDisabledEnv, GatewayFanoutLinksEnv, GatewayFanoutWindowEnv} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv(GatewayFanoutEnabledEnv, "1")

	pub := &capturePublisher{}
	h := fanoutHandler(t, fanoutClassifier(t), pub)
	sub, camp := fanSession()

	for n := 1; n <= 6; n++ {
		assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n,
			"GATEWAY_ENFORCE is unset — gate 2 shadow must forward everything")
	}

	pub.mu.Lock()
	defer pub.mu.Unlock()
	last := pub.events[len(pub.events)-1]
	if last.GatewayAction != GatewayActionShadowWithheldFanout {
		t.Fatalf("shadow GatewayAction = %q, want GatewayActionShadowWithheldFanout — a shadow that records nothing collects no evidence",
			last.GatewayAction)
	}
	if last.GatewayAction != "shadow_withheld_fanout" {
		t.Fatalf("shadow GatewayAction wire value = %q, want %q", last.GatewayAction, "shadow_withheld_fanout")
	}
	if last.GatewayAction == GatewayActionShadowWithheld {
		t.Fatal("gate 2's shadow marker collapsed into gate 1's — calibration cannot tell the gates apart")
	}
}

// ── NO-STORE ON THE FANOUT-WITHHELD RESPONSE ────────────────────────────────
//
// Invariant 2. A cached 204 replayed to a later human request for the same URL
// denies a real person their click, silently and unrecoverably.
func TestOfferFanout_WithheldResponseIsNoStoreAndEmpty(t *testing.T) {
	fanoutArmed(t)
	h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
	sub, camp := fanSession()

	for n := 1; n <= 3; n++ {
		doFan(h, ipFanUnresolved, sub, fanHash(n), camp)
	}
	rec := doFan(h, ipFanUnresolved, sub, fanHash(4), camp)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, private, max-age=0" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q", got)
	}
	if got := rec.Header().Get("Expires"); got != "0" {
		t.Errorf("Expires = %q", got)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("fanout-withheld response carries Location: %q", loc)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("fanout-withheld response carries a body: %q", body)
	}
	if strings.Contains(rec.Body.String(), "discountblog.com") {
		t.Error("fanout-withheld response leaked the brand site (cloaking)")
	}
}

// ── FAIL OPEN ───────────────────────────────────────────────────────────────
//
// Every degraded state forwards, with gate 2 fully armed throughout — so any
// failure here is a real person's click withheld by an accident.
func TestOfferFanout_FailsOpen(t *testing.T) {
	t.Run("nil classifier (NewHandler / no DATABASE_URL)", func(t *testing.T) {
		fanoutArmed(t)
		h := fanoutHandler(t, nil, &capturePublisher{})
		sub, camp := fanSession()
		for n := 1; n <= 10; n++ {
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n, "nil classifier = nil counter")
		}
	})

	t.Run("empty classifier — zero-value counter, no sessions map", func(t *testing.T) {
		fanoutArmed(t)
		h := fanoutHandler(t, &IPClassifier{}, &capturePublisher{})
		sub, camp := fanSession()
		for n := 1; n <= 10; n++ {
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n, "no prefixes loaded = no class = outside gate 2")
		}
	})

	t.Run("nil-receiver DecideSession", func(t *testing.T) {
		fanoutArmed(t)
		var c *IPClassifier
		for i := 0; i < 10; i++ {
			if d := c.DecideSession(ipFanUnresolved, fmt.Sprintf("https://adv/%d", i), "s", "c"); d.Withhold {
				t.Fatalf("nil classifier withheld on request %d", i)
			}
		}
	})

	// GATEWAY_DISABLED kills gate 2 with everything else, even with the fanout
	// flag and enforcement both armed.
	t.Run("GATEWAY_DISABLED overrides an armed fanout gate", func(t *testing.T) {
		fanoutArmed(t)
		t.Setenv(GatewayDisabledEnv, "1")
		pub := &capturePublisher{}
		h := fanoutHandler(t, fanoutClassifier(t), pub)
		sub, camp := fanSession()
		for n := 1; n <= 10; n++ {
			assertFanForwarded(t, doFan(h, ipFanUnresolved, sub, fanHash(n), camp), n, "GATEWAY_DISABLED is the master off-switch")
		}
		pub.mu.Lock()
		defer pub.mu.Unlock()
		for i, e := range pub.events {
			if e.GatewayAction != "" {
				t.Fatalf("event %d carries GatewayAction=%q with GATEWAY_DISABLED set", i, e.GatewayAction)
			}
		}
	})

	// A missing subscriber or campaign means there is no session to key on, so
	// gate 2 cannot run. Reached here through the /track/click path, whose
	// token carries both fields and can legitimately arrive with them blank —
	// the /o/ path's own contract always supplies non-empty segments, so this
	// is the honest way to exercise it end to end.
	t.Run("blank subscriber/campaign in the click token", func(t *testing.T) {
		fanoutArmed(t)
		h := NewHandlerWithClassifier(&capturePublisher{}, nil, fanoutClassifier(t))
		for n := 1; n <= 10; n++ {
			dest := fmt.Sprintf("https://www.cratoolpro.com/BJB4Q5BF/LINK%02d/", n)
			rec := doClickBlankSession(h, ipFanUnresolved, dest)
			if rec.Code == http.StatusNoContent {
				t.Fatalf("link %d WITHHELD with no session identity — gate 2 must not run without a (subscriber, campaign)", n)
			}
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("link %d: status = %d, want 307", n, rec.Code)
			}
			if rec.Header().Get("Location") != dest {
				t.Fatalf("link %d: Location = %q, want %q", n, rec.Header().Get("Location"), dest)
			}
		}
		// Control: the same address WITH a session is gated on the same path.
		sub, camp := fanSession()
		for n := 1; n <= 4; n++ {
			dest := fmt.Sprintf("https://www.cratoolpro.com/BJB4Q5BF/LINK%02d/", n)
			rec := doClickSession(h, ipFanUnresolved, sub, camp, dest)
			if n == 4 && rec.Code != http.StatusNoContent {
				t.Fatalf("CONTROL FAILED: link 4 with a real session returned %d, want 204 — "+
					"the gate was not firing, so the blank-session forwards prove nothing.", rec.Code)
			}
		}
	})
}

// ── GATE 1 IS UNCHANGED ─────────────────────────────────────────────────────
//
// Adding a second gate must not relax the first: a 'scanner' address is still
// withheld on link 1, with gate 2 armed and with gate 2 unset alike.
func TestOfferFanout_IPClassRuleStillWithholdsScannerOnLinkOne(t *testing.T) {
	for _, fanoutFlag := range []string{"1", ""} {
		name := "fanout armed"
		if fanoutFlag == "" {
			name = "fanout unset"
		}
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{GatewayDisabledEnv, GatewayFanoutLinksEnv, GatewayFanoutWindowEnv, GatewayFanoutEnabledEnv} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			t.Setenv(GatewayEnforceEnv, "1")
			if fanoutFlag != "" {
				t.Setenv(GatewayFanoutEnabledEnv, fanoutFlag)
			}

			h := fanoutHandler(t, fanoutClassifier(t), &capturePublisher{})
			sub, camp := fanSession()
			rec := doFan(h, ipFanScanner, sub, fanHash(1), camp)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("LINK 1 from a confirmed 'scanner' address returned %d, want 204 — gate 2 relaxed gate 1", rec.Code)
			}
		})
	}
}

// ── click-path helpers ──────────────────────────────────────────────────────

// clickTokenFor builds the /track/click/ token shape
// org|campaign|subscriber|email|url, so the subscriber and campaign that
// identify the gate-2 session can be varied — including blanked out.
func clickTokenFor(sub, camp, dest string) string {
	return base64.URLEncoding.EncodeToString([]byte(strings.Join([]string{
		"00000000-0000-0000-0000-000000000001", camp, sub, camp, dest,
	}, "|")))
}

func doClickSession(h *Handler, ip, sub, camp, dest string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/track/click/"+clickTokenFor(sub, camp, dest)+"/sig", nil)
	req.Header.Set("User-Agent", uaBrowser)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func doClickBlankSession(h *Handler, ip, dest string) *httptest.ResponseRecorder {
	return doClickSession(h, ip, "", "", dest)
}

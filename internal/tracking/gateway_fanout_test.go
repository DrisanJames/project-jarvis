package tracking

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// =============================================================================
// GATE 2 — session-shape fanout
// =============================================================================
//
// The gate exists because IP alone cannot clear Microsoft: real people and the
// sweep arrive from the SAME shared addresses. Shape separates them — a sweep
// pulls several DISTINCT links in seconds, a person clicks one and stops.
//
// Every test below is written from the money side: the failure that matters is
// withholding a human's click, which cannot be retried and cannot be recovered.
// So the load-bearing assertions are the FORWARDS, not the withholds.

// fanoutOn arms gate 2 AND enforcement, i.e. the fully-armed posture. Both are
// read at call time, so this is all it takes.
func fanoutOn(t *testing.T) {
	t.Helper()
	t.Setenv(GatewayEnforceEnv, "1")
	t.Setenv(GatewayFanoutEnabledEnv, "1")
}

// sweepHandler is one handler with ONE classifier, reused across requests so
// the per-(subscriber, campaign) counter actually accumulates.
func sweepHandler(t *testing.T) *Handler {
	t.Helper()
	return NewHandlerWithClassifier(&capturePublisher{}, nil, seededClassifier(t))
}

func linkN(i int) string { return fmt.Sprintf("https://www.cratoolpro.com/BJB4Q5BF/OFFER%d/", i) }

const (
	ipUnresolved = "135.232.20.64" // /32 'unresolved' — mixed population
	ipHosting    = "135.232.99.99" // inside the blanket 'hosting' /16
	ipNullClass  = "8.8.8.8"       // NO ROW AT ALL — the residential population
)

// -----------------------------------------------------------------------------
// THE RULE
// -----------------------------------------------------------------------------

// The first three DISTINCT links of a session forward; the 4th and beyond are
// withheld. Forwarding the head of the sweep is the price of never gating a
// human who clicks once — it is intended, not a tolerance.
func TestFanout_FirstThreeForward_FourthWithheld(t *testing.T) {
	fanoutOn(t)

	for _, ip := range []string{ipUnresolved, ipHosting} {
		t.Run(ip, func(t *testing.T) {
			h := sweepHandler(t)
			for i := 1; i <= 3; i++ {
				assertForwardedTo(t, doClickFromIP(h, ip, linkN(i)), "cratoolpro.com")
			}
			assertWithheld(t, doClickFromIP(h, ip, linkN(4)))
			assertWithheld(t, doClickFromIP(h, ip, linkN(5)))
		})
	}
}

// THE MOST IMPORTANT TEST IN THIS FILE. An unclassified (no-row / NULL) address
// is the residential population — 76.4% single-link humans — and is NEVER gated
// on shape, no matter how many links it pulls how fast.
func TestFanout_NullClassIsNeverGated(t *testing.T) {
	fanoutOn(t)
	h := sweepHandler(t)

	for i := 1; i <= 20; i++ {
		rec := doClickFromIP(h, ipNullClass, linkN(i))
		if rec.Code == http.StatusNoContent {
			t.Fatalf("unclassified (residential) address WITHHELD on link %d — this is a lost human conversion", i)
		}
	}
}

// Classes outside {unresolved, hosting} are outside gate 2 entirely.
func TestFanout_OtherClassesAreNeverGated(t *testing.T) {
	fanoutOn(t)

	for _, class := range []string{"residential-or-mobile", "vpn-or-proxy", "unknown"} {
		t.Run(class, func(t *testing.T) {
			ipc := stubClassifier(t, map[string]string{"203.0.113.0/24": class})
			h := NewHandlerWithClassifier(&capturePublisher{}, nil, ipc)
			for i := 1; i <= 10; i++ {
				assertForwardedTo(t, doClickFromIP(h, "203.0.113.7", linkN(i)), "cratoolpro.com")
			}
		})
	}
}

// DISTINCT is the measure. A human reloading ONE link twenty times is not a
// fanout and must never be withheld.
func TestFanout_RepeatsOfOneLinkNeverWithhold(t *testing.T) {
	fanoutOn(t)
	h := sweepHandler(t)

	for i := 0; i < 20; i++ {
		assertForwardedTo(t, doClickFromIP(h, ipUnresolved, linkN(1)), "cratoolpro.com")
	}
}

// The counter is keyed by (subscriber, campaign): one session reaching the
// threshold must not gate a DIFFERENT session, even from the same address.
func TestFanout_CounterIsPerSubscriberCampaign(t *testing.T) {
	fanoutOn(t)
	c := seededClassifier(t)

	for i := 1; i <= 6; i++ {
		c.DecideSession(ipUnresolved, linkN(i), "sub-A", "camp-1")
	}
	if d := c.DecideSession(ipUnresolved, linkN(7), "sub-A", "camp-1"); !d.Withhold {
		t.Fatal("the swept session was not gated — fixture is wrong")
	}

	for _, other := range [][2]string{{"sub-B", "camp-1"}, {"sub-A", "camp-2"}} {
		if d := c.DecideSession(ipUnresolved, linkN(9), other[0], other[1]); d.Withhold {
			t.Fatalf("session (%s,%s) gated by ANOTHER session's fanout", other[0], other[1])
		}
	}
}

// -----------------------------------------------------------------------------
// DEFAULT IS OFF (the ship-it-off requirement)
// -----------------------------------------------------------------------------

// GATEWAY_FANOUT_ENABLED unset = the gate does not exist: a full sweep forwards,
// carries NO gateway marker, and allocates no counter state.
func TestFanout_DefaultIsOff(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")
	t.Setenv(GatewayFanoutEnabledEnv, "")

	pub := &capturePublisher{}
	ipc := seededClassifier(t)
	h := NewHandlerWithClassifier(pub, nil, ipc)

	for i := 1; i <= 10; i++ {
		assertForwardedTo(t, doClickFromIP(h, ipUnresolved, linkN(i)), "cratoolpro.com")
	}
	if evt, _ := pub.last(); evt.GatewayAction != "" {
		t.Fatalf("GatewayAction = %q, want empty with the gate off", evt.GatewayAction)
	}
	if n := ipc.fanout.size(); n != 0 {
		t.Fatalf("unarmed gate allocated %d sessions — it must cost nothing", n)
	}
}

// Armed but NOT enforcing: the sweep is forwarded and only the shadow marker
// records what would have happened. This is how the gate gets measured in prod
// before it is allowed to act.
func TestFanout_ShadowWhenEnforcementUnarmed(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "")
	t.Setenv(GatewayFanoutEnabledEnv, "1")

	pub := &capturePublisher{}
	h := NewHandlerWithClassifier(pub, nil, seededClassifier(t))
	for i := 1; i <= 4; i++ {
		assertForwardedTo(t, doClickFromIP(h, ipUnresolved, linkN(i)), "cratoolpro.com")
	}
	evt, ok := pub.last()
	if !ok {
		t.Fatal("no telemetry published")
	}
	if evt.GatewayAction != GatewayActionShadowWithheldFanout {
		t.Fatalf("GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionShadowWithheldFanout)
	}
}

// GATEWAY_DISABLED kills gate 2 with everything else: no marker, no counting.
func TestFanout_DisabledEnvKillsGateTwo(t *testing.T) {
	fanoutOn(t)
	t.Setenv(GatewayDisabledEnv, "1")

	pub := &capturePublisher{}
	ipc := seededClassifier(t)
	h := NewHandlerWithClassifier(pub, nil, ipc)
	for i := 1; i <= 10; i++ {
		assertForwardedTo(t, doClickFromIP(h, ipUnresolved, linkN(i)), "cratoolpro.com")
	}
	if evt, _ := pub.last(); evt.GatewayAction != "" {
		t.Fatalf("GatewayAction = %q, want empty when disabled", evt.GatewayAction)
	}
	if n := ipc.fanout.size(); n != 0 {
		t.Fatalf("disabled gateway counted %d sessions", n)
	}
}

// All three knobs are read at CALL time — armed, tuned and dropped by an ECS env
// change, never a deploy.
func TestFanout_EnvReadAtCallTime(t *testing.T) {
	t.Setenv(GatewayEnforceEnv, "1")
	c := seededClassifier(t)

	// Gate off: four distinct links, nothing withheld.
	t.Setenv(GatewayFanoutEnabledEnv, "")
	for i := 1; i <= 4; i++ {
		if d := c.DecideSession(ipUnresolved, linkN(i), "s1", "c1"); d.Withhold {
			t.Fatal("withheld with the gate off")
		}
	}

	// Threshold lowered to 2 on the SAME classifier: the counter starts from
	// this point (nothing was recorded while the gate was off), so links 1 and
	// 2 forward and 3 trips it.
	t.Setenv(GatewayFanoutEnabledEnv, "1")
	t.Setenv(GatewayFanoutLinksEnv, "2")
	if d := c.DecideSession(ipUnresolved, linkN(11), "s1", "c1"); d.Withhold {
		t.Fatal("withheld on the first counted link")
	}
	if d := c.DecideSession(ipUnresolved, linkN(12), "s1", "c1"); !d.Withhold {
		t.Fatal("GATEWAY_FANOUT_LINKS=2 did not take effect")
	}

	// Dropped again mid-flight.
	t.Setenv(GatewayFanoutEnabledEnv, "0")
	if d := c.DecideSession(ipUnresolved, linkN(13), "s1", "c1"); d.Withhold {
		t.Fatal("gate stayed armed after being disabled")
	}
}

// A typo'd or hostile knob degrades to the measured default — never to 0, which
// would withhold on the FIRST click.
func TestFanout_BadEnvFallsBackToDefaults(t *testing.T) {
	for _, v := range []string{"", "abc", "0", "-3", "  "} {
		t.Setenv(GatewayFanoutLinksEnv, v)
		if got := gatewayFanoutLinks(); got != defaultFanoutLinks {
			t.Fatalf("GATEWAY_FANOUT_LINKS=%q -> %d, want %d", v, got, defaultFanoutLinks)
		}
		t.Setenv(GatewayFanoutWindowEnv, v)
		if got := gatewayFanoutWindow(); got != defaultFanoutWindow {
			t.Fatalf("GATEWAY_FANOUT_WINDOW_S=%q -> %v, want %v", v, got, defaultFanoutWindow)
		}
	}
	t.Setenv(GatewayFanoutLinksEnv, "7")
	if got := gatewayFanoutLinks(); got != 7 {
		t.Fatalf("GATEWAY_FANOUT_LINKS=7 -> %d", got)
	}
	t.Setenv(GatewayFanoutWindowEnv, "90")
	if got := gatewayFanoutWindow(); got != 90*time.Second {
		t.Fatalf("GATEWAY_FANOUT_WINDOW_S=90 -> %v", got)
	}
}

// -----------------------------------------------------------------------------
// INVARIANT 3 — the unsubscribe exemption still runs FIRST
// -----------------------------------------------------------------------------

// A session already past the threshold still gets its unsubscribe link. The
// exemption is checked before any classification or counting, so no amount of
// fanout can suppress a CAN-SPAM / RFC 8058 destination.
func TestFanout_UnsubscribeStillExemptMidSweep(t *testing.T) {
	fanoutOn(t)
	h := sweepHandler(t)

	for i := 1; i <= 6; i++ {
		doClickFromIP(h, ipUnresolved, linkN(i))
	}
	assertWithheld(t, doClickFromIP(h, ipUnresolved, linkN(7))) // fixture check

	for _, dest := range []string{
		"https://em.discountblog.com/unsubscribe/abc",
		"https://em.discountblog.com/email-preferences?id=abc",
	} {
		rec := doClickFromIP(h, ipUnresolved, dest)
		if rec.Code == http.StatusNoContent {
			t.Fatalf("exempt destination WITHHELD by gate 2: %s", dest)
		}
	}
}

// -----------------------------------------------------------------------------
// INVARIANT 4 — telemetry, published BEFORE the withhold and separable
// -----------------------------------------------------------------------------

// A shape withhold is still an event, still carries attribution, and carries a
// marker DISTINCT from the IP-class withhold so the two are separable in
// reporting.
func TestFanout_WithheldRequestIsPublishedWithItsOwnAction(t *testing.T) {
	fanoutOn(t)

	pub := &capturePublisher{}
	h := NewHandlerWithClassifier(pub, nil, seededClassifier(t))
	for i := 1; i <= 4; i++ {
		doClickFromIP(h, ipUnresolved, linkN(i))
	}

	evt, ok := pub.last()
	if !ok {
		t.Fatal("withheld request published NO telemetry")
	}
	if evt.GatewayAction != GatewayActionWithheldFanout {
		t.Fatalf("GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionWithheldFanout)
	}
	if evt.GatewayAction == GatewayActionWithheld {
		t.Fatal("shape withhold is indistinguishable from an IP-class withhold")
	}
	if evt.SubscriberID != subUUID || evt.CampaignID != campUUID {
		t.Fatalf("withheld event lost attribution: sub=%q camp=%q", evt.SubscriberID, evt.CampaignID)
	}
	if evt.LinkURL == "" {
		t.Fatal("withheld event lost the link")
	}
}

// The same on the /o/ offer path: it must produce a fanout marker too, and the
// no-store headers still ride on the 204.
func TestFanout_OfferPathGatesAndKeepsNoStore(t *testing.T) {
	fanoutOn(t)
	t.Setenv(GatewayFanoutLinksEnv, "2")

	pub := &capturePublisher{}
	h := NewHandlerWithClassifier(pub, stubDict(map[string]smartLinkEntry{
		"aaa111": {Destination: "https://www.cratoolpro.com/A/", BrandRoot: "discountblog.com"},
		"bbb222": {Destination: "https://www.cratoolpro.com/B/", BrandRoot: "discountblog.com"},
	}), seededClassifier(t))

	assertForwardedTo(t, doOfferFromIP(h, ipUnresolved, "aaa111"), "cratoolpro.com")
	rec := doOfferFromIP(h, ipUnresolved, "bbb222")
	assertWithheld(t, rec)
	if got := rec.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate, private, max-age=0" {
		t.Fatalf("Cache-Control on a shape-withheld 204 = %q", got)
	}
	if evt, _ := pub.last(); evt.GatewayAction != GatewayActionWithheldFanout {
		t.Fatalf("offer GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionWithheldFanout)
	}
}

// -----------------------------------------------------------------------------
// INVARIANT 5 — fail open
// -----------------------------------------------------------------------------

func TestFanout_FailsOpen(t *testing.T) {
	fanoutOn(t)

	t.Run("nil receiver", func(t *testing.T) {
		var c *IPClassifier
		if d := c.DecideSession(ipUnresolved, linkN(1), "s", "c"); d.Withhold {
			t.Fatal("nil classifier withheld")
		}
	})

	t.Run("missing subscriber or campaign", func(t *testing.T) {
		for _, id := range [][2]string{{"", "c1"}, {"s1", ""}, {"", ""}} {
			c := seededClassifier(t)
			for i := 1; i <= 12; i++ {
				if d := c.DecideSession(ipUnresolved, linkN(i), id[0], id[1]); d.Withhold {
					t.Fatalf("withheld with sub=%q camp=%q", id[0], id[1])
				}
			}
		}
	})

	t.Run("missing destination", func(t *testing.T) {
		c := seededClassifier(t)
		for i := 0; i < 12; i++ {
			if d := c.DecideSession(ipUnresolved, "", "s1", "c1"); d.Withhold {
				t.Fatal("withheld with an empty destination")
			}
		}
	})

	t.Run("Decide (no session identity) never runs gate 2", func(t *testing.T) {
		c := seededClassifier(t)
		for i := 1; i <= 12; i++ {
			if d := c.Decide(ipUnresolved, linkN(i)); d.Withhold {
				t.Fatal("Decide withheld on shape without a session")
			}
		}
	})

	t.Run("zero-value classifier counts safely", func(t *testing.T) {
		c := &IPClassifier{} // no entries: every class is "" -> outside gate 2
		for i := 1; i <= 12; i++ {
			if d := c.DecideSession(ipUnresolved, linkN(i), "s1", "c1"); d.Withhold {
				t.Fatal("empty classifier withheld")
			}
		}
	})

	t.Run("nil tracker", func(t *testing.T) {
		var f *sessionFanout
		if n := f.record("s", "c", "d", time.Now(), time.Minute); n != 0 {
			t.Fatalf("nil tracker returned %d", n)
		}
		if n := f.size(); n != 0 {
			t.Fatalf("nil tracker size = %d", n)
		}
	})
}

// -----------------------------------------------------------------------------
// THE COUNTER ITSELF — window semantics and bounds, with an injected clock
// -----------------------------------------------------------------------------

// "all of them inside a 60-second window": a sweep paced slower than the window
// restarts the count and is never gated. Deliberate — that pacing is
// indistinguishable from a person reading their mail.
func TestFanoutTracker_WindowResets(t *testing.T) {
	var f sessionFanout
	t0 := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	w := 60 * time.Second

	for i := 1; i <= 3; i++ {
		if n := f.record("s", "c", linkN(i), t0.Add(time.Duration(i)*time.Second), w); n != i {
			t.Fatalf("link %d counted %d", i, n)
		}
	}
	// Just past the window: the count restarts at 1, not 4.
	if n := f.record("s", "c", linkN(4), t0.Add(61*time.Second+time.Millisecond), w); n != 1 {
		t.Fatalf("count after the window lapsed = %d, want 1 (fresh window)", n)
	}
	// Exactly at the edge stays inside the window.
	var g sessionFanout
	g.record("s", "c", linkN(1), t0, w)
	if n := g.record("s", "c", linkN(2), t0.Add(w), w); n != 2 {
		t.Fatalf("count at the window edge = %d, want 2", n)
	}
	// A clock that goes backwards restarts rather than counting.
	if n := g.record("s", "c", linkN(3), t0.Add(-time.Hour), w); n != 1 {
		t.Fatalf("count after a backwards clock = %d, want 1", n)
	}
}

// Distinct-only, at the tracker level.
func TestFanoutTracker_DistinctOnly(t *testing.T) {
	var f sessionFanout
	now := time.Now()
	for i := 0; i < 25; i++ {
		if n := f.record("s", "c", "https://one/", now, time.Minute); n != 1 {
			t.Fatalf("repeat %d counted %d, want 1", i, n)
		}
	}
}

// Bad input never counts and never panics.
func TestFanoutTracker_BadInputReturnsZero(t *testing.T) {
	var f sessionFanout
	now := time.Now()
	for _, in := range [][3]string{{"", "c", "d"}, {"s", "", "d"}, {"s", "c", ""}} {
		if n := f.record(in[0], in[1], in[2], now, time.Minute); n != 0 {
			t.Fatalf("record%v = %d, want 0", in, n)
		}
	}
	if n := f.record("s", "c", "d", now, 0); n != 0 {
		t.Fatalf("non-positive window counted %d", n)
	}
}

// The map is BOUNDED and SWEPT: a flood of one-shot sessions cannot grow memory
// without limit, and the sessions drain once idle.
func TestFanoutTracker_BoundedAndSwept(t *testing.T) {
	var f sessionFanout
	t0 := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	w := 60 * time.Second

	// Every session lands in the same window, so nothing can be swept: the map
	// stops growing at the cap instead.
	for i := 0; i < fanoutMaxSessions+5_000; i++ {
		f.record(fmt.Sprintf("sub-%d", i), "camp", "https://x/", t0, w)
	}
	if got := f.size(); got > fanoutMaxSessions {
		t.Fatalf("session map grew to %d, past the %d bound", got, fanoutMaxSessions)
	}
	// At the bound, a NEW session is not tracked — and therefore forwards.
	if n := f.record("brand-new-session", "camp", "https://x/", t0, w); n != 0 {
		t.Fatalf("at the bound a new session counted %d, want 0 (fail open)", n)
	}

	// One window later everything is idle and drains on the next record.
	f.record("keeper", "camp", "https://x/", t0.Add(10*time.Minute), w)
	if got := f.size(); got > 1 {
		t.Fatalf("after the sweep %d sessions remain, want 1", got)
	}
}

// One session's distinct set is bounded too, and freezing it changes no
// decision — the count stays above the threshold.
func TestFanoutTracker_PerSessionDestinationsBounded(t *testing.T) {
	var f sessionFanout
	now := time.Now()
	var last int
	for i := 0; i < fanoutMaxDestsPerSession+50; i++ {
		last = f.record("s", "c", linkN(i), now, time.Minute)
	}
	if last != fanoutMaxDestsPerSession {
		t.Fatalf("distinct count = %d, want it pinned at %d", last, fanoutMaxDestsPerSession)
	}
	if last < defaultFanoutLinks {
		t.Fatal("the bound dropped the count below the threshold — a sweep would be forwarded")
	}
}

// The counter is touched from many click goroutines at once. Run with -race.
func TestFanoutTracker_ConcurrentRecordIsSafe(t *testing.T) {
	var f sessionFanout
	now := time.Now()
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				f.record(fmt.Sprintf("sub-%d", i%17), "camp", linkN(i%9), now, time.Minute)
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	if f.size() == 0 {
		t.Fatal("nothing recorded")
	}
}

// -----------------------------------------------------------------------------
// NO REGRESSION IN GATE 1
// -----------------------------------------------------------------------------

// Arming gate 2 does not change the IP-class rule: 'scanner' still withholds
// with its own marker, and the unclassified address still forwards.
func TestFanout_GateOneUnchanged(t *testing.T) {
	fanoutOn(t)

	pub := &capturePublisher{}
	h := NewHandlerWithClassifier(pub, nil, seededClassifier(t))
	assertWithheld(t, doClickFromIP(h, "135.232.20.148", "https://www.cratoolpro.com/x/"))
	if evt, _ := pub.last(); evt.GatewayAction != GatewayActionWithheld {
		t.Fatalf("scanner GatewayAction = %q, want %q", evt.GatewayAction, GatewayActionWithheld)
	}
	assertForwardedTo(t, doClickFromIP(h, ipNullClass, "https://www.cratoolpro.com/x/"), "cratoolpro.com")
}

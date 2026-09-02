package sendqueue

import "testing"

// DARK DEFAULT: with every routing env unset, SendRouteEnabled is false — gated
// paths behave byte-identically to today.
func TestSendRouteEnabled_DefaultsOff(t *testing.T) {
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	if SendRouteEnabled() {
		t.Fatal("SendRouteEnabled must default to FALSE with no routing env set")
	}
}

// Each routing env independently flips the gate ON (pure env read; no producer
// required — so bypass paths block the moment the operator turns routing on).
func TestSendRouteEnabled_EachEnvFlipsOn(t *testing.T) {
	cases := []struct {
		env string
		val string
	}{
		{"KAFKA_SEND_QUEUE_ENABLED", "1"},
		{"KAFKA_SEND_QUEUE_ENABLED", "true"},
		{"KAFKA_SEND_QUEUE_ALL", "yes"},
		{"KAFKA_SEND_QUEUE_WAVES", "wave-A"},
		{"KAFKA_SEND_QUEUE_CAMPAIGNS", "camp-Z"},
	}
	for _, c := range cases {
		t.Run(c.env+"="+c.val, func(t *testing.T) {
			t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "")
			t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
			t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
			t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
			t.Setenv(c.env, c.val)
			if !SendRouteEnabled() {
				t.Fatalf("SendRouteEnabled must be TRUE when %s=%s", c.env, c.val)
			}
		})
	}
}

// A non-truthy value for the boolean flags does NOT flip the gate.
func TestSendRouteEnabled_NonTruthyStaysOff(t *testing.T) {
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "0")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "false")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	if SendRouteEnabled() {
		t.Fatal("SendRouteEnabled must be FALSE for non-truthy boolean flags and empty lists")
	}
}

// =============================================================================
// REQ-090 — WIRING vs ROUTING are two different questions
// =============================================================================

// clearRouteEnv puts the process in the dark default for one test.
func clearRouteEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	SetSendRouteFlag(nil)
	t.Cleanup(func() { SetSendRouteFlag(nil) })
}

// THE :1077 CASE. KAFKA_SEND_QUEUE_ENABLED=1 with KAFKA_SEND_QUEUE_ALL=0 wires
// the consumer (so a backlog drains) but routes NOTHING — so the five
// direct-enqueue guards must be OPEN. This is the exact state that answered 409
// on /trigger-send all day on 2026-09-01.
func TestRouteGate_EnabledWithoutAll_WiresButDoesNotRoute(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "1")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "0")

	if !SendRouteEnabled() {
		t.Error("SendRouteEnabled (WIRING) must be TRUE with KAFKA_SEND_QUEUE_ENABLED=1")
	}
	if SendRoutingActive() {
		t.Error("SendRoutingActive (ROUTING) must be FALSE with ALL=0 and no allowlist — the guards must stay OPEN")
	}
	if SendRouteMatches("some-wave", "some-campaign") {
		t.Error("SendRouteMatches must be FALSE for an un-allowlisted wave when ALL=0")
	}
}

// The dark default: nothing set, nothing wired, nothing routed.
func TestRouteGate_DarkDefault(t *testing.T) {
	clearRouteEnv(t)
	if SendRouteEnabled() {
		t.Error("SendRouteEnabled must be FALSE in the dark default")
	}
	if SendRoutingActive() {
		t.Error("SendRoutingActive must be FALSE in the dark default")
	}
	if !SendRouteFlagOpen() {
		t.Error("SendRouteFlagOpen must be TRUE (open) when no kill switch is installed")
	}
}

// KAFKA_SEND_QUEUE_ALL routes everything — and, because any routing env also
// implies the transport must exist, it wires too.
func TestRouteGate_AllRoutesEverything(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")

	if !SendRoutingActive() {
		t.Error("ALL=1 must make routing ACTIVE")
	}
	if !SendRouteMatches("any-wave", "any-campaign") {
		t.Error("ALL=1 must route every wave")
	}
	if !SendRouteEnabled() {
		t.Error("ALL=1 must also imply WIRING (the transport carrying it must exist)")
	}
}

// Allowlists route only their own ids — but the no-id form (RouteAny, what the
// five guards ask) is TRUE, because SOMETHING routes and a direct INSERT would
// bypass the hard path for it.
func TestRouteGate_AllowlistsAreIDScoped(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "wave-A, wave-B")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "camp-Z")

	if !SendRouteMatches("wave-A", "other") {
		t.Error("wave-A is allowlisted and must route")
	}
	if SendRouteMatches("wave-C", "camp-Y") {
		t.Error("wave-C / camp-Y are NOT allowlisted and must not route")
	}
	if !SendRouteMatches("other", "camp-Z") {
		t.Error("camp-Z is allowlisted and must route")
	}
	if !SendRoutingActive() {
		t.Error("a non-empty allowlist means routing is ACTIVE for the guards")
	}
}

// A non-truthy ALL and empty allowlists route nothing.
func TestRouteGate_NonTruthyStaysOff(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "false")
	if SendRoutingActive() {
		t.Error("ALL=false must not route")
	}
}

// stubFlag is a RouteFlag whose answer the test controls.
type stubFlag struct{ on bool }

func (s stubFlag) Enabled() bool { return s.on }

// THE RUNTIME KILL SWITCH. With routing fully ON via env, an installed flag that
// reports OFF vetoes routing everywhere — dispatcher and guards alike.
func TestRouteGate_KillSwitchVetoesEnv(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")
	if !SendRoutingActive() {
		t.Fatal("precondition: ALL=1 must route before the kill switch is pulled")
	}

	SetSendRouteFlag(stubFlag{on: false})
	if SendRoutingActive() {
		t.Error("kafka:flag:send_route=0 must VETO routing even with ALL=1")
	}
	if SendRouteMatches("w", "c") {
		t.Error("the veto must apply to the per-wave predicate too")
	}
	if !SendRouteEnabled() {
		t.Error("the kill switch must NOT unwire the transport — the consumer keeps draining")
	}

	// Flag back open → routing resumes from env with no redeploy.
	SetSendRouteFlag(stubFlag{on: true})
	if !SendRoutingActive() {
		t.Error("re-opening the flag must restore env-decided routing")
	}
}

// A kill switch can only ever REMOVE send volume: flag ON with no routing env
// still routes nothing.
func TestRouteGate_KillSwitchCannotRouteWithoutEnv(t *testing.T) {
	clearRouteEnv(t)
	SetSendRouteFlag(stubFlag{on: true})
	if SendRoutingActive() {
		t.Error("an ON flag must not create routing the env does not configure")
	}
}

// /health.event_bus.send_queue.routing carries the four envs VERBATIM — "0",
// "false" and unset are three distinct operator intents.
func TestRouteGate_RoutingSnapshotIsVerbatim(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "1")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "0")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "wave-A")

	r := RoutingSnapshot()
	if r.Enabled != "1" || r.All != "0" || r.Waves != "wave-A" || r.Campaigns != "" {
		t.Errorf("routing snapshot not verbatim: %+v", r)
	}
	if !r.WiringEnabled {
		t.Error("wiring_enabled must be true (ENABLED=1)")
	}
	if !r.Active {
		t.Error("active must be true (the wave allowlist is non-empty)")
	}
	if !r.FlagOpen {
		t.Error("flag_open must default to true with no kill switch installed")
	}
}

// kill_switches lists ONLY the levers explicitly set, verbatim.
func TestRouteGate_KillSwitchInventory(t *testing.T) {
	clearRouteEnv(t)
	for _, n := range SendPathEnvNames {
		t.Setenv(n, "")
	}
	if got := SendPathKillSwitches(); len(got) != 0 {
		t.Errorf("all levers unset must yield an EMPTY kill_switches map, got %v", got)
	}

	t.Setenv("KAFKA_SEND_QUEUE_ALL", "0")
	t.Setenv("DISABLE_CAP_AWARE_CLAIM", "true")
	got := SendPathKillSwitches()
	if got["KAFKA_SEND_QUEUE_ALL"] != "0" {
		t.Errorf("KAFKA_SEND_QUEUE_ALL=0 is EXPLICITLY SET and must be listed verbatim, got %q", got["KAFKA_SEND_QUEUE_ALL"])
	}
	if got["DISABLE_CAP_AWARE_CLAIM"] != "true" {
		t.Errorf("DISABLE_CAP_AWARE_CLAIM missing from the inventory, got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("only the SET levers belong in kill_switches, got %v", got)
	}
}

// The inventory must have no duplicates and no empty names — it is the single
// source the /health block and the deploy manifest cross-check both read.
func TestRouteGate_SendPathEnvNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range SendPathEnvNames {
		if n == "" {
			t.Fatal("empty name in SendPathEnvNames")
		}
		if seen[n] {
			t.Errorf("duplicate name %q in SendPathEnvNames", n)
		}
		seen[n] = true
	}
	if len(SendPathEnvNamesSorted()) != len(SendPathEnvNames) {
		t.Error("SendPathEnvNamesSorted lost entries")
	}
}

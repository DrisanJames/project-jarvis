package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/api"
	"github.com/ignite/sparkpost-monitor/internal/eventbus/sendqueue"
)

// =============================================================================
// REQ-090 — one send-path env inventory, two consumers
// =============================================================================
// /health.event_bus.send_queue.kill_switches is rendered from
// sendqueue.SendPathEnvNames; deploy/env.manifest.json is what the deploy
// upserts. If those two lists drift, /health silently stops reporting a lever
// that can zero the send path — which is exactly how KAFKA_SEND_QUEUE_ALL=0 sat
// unreviewed for 90 minutes on 2026-09-01. This test is the join between them.

// TestEnvManifestCoversSendPathInventory: every lever /health can report must be
// declared in the manifest, with a class that says it is a lever.
func TestEnvManifestCoversSendPathInventory(t *testing.T) {
	_, byName := loadEnvManifest(t)

	var missing, wrongClass []string
	for _, name := range sendqueue.SendPathEnvNamesSorted() {
		e, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		switch e.Class {
		case "kill", "route", "capacity":
			// a lever
		default:
			wrongClass = append(wrongClass, name+" (class "+e.Class+")")
		}
	}
	if len(missing) > 0 {
		t.Errorf("send-path levers reported on /health but NOT declared in deploy/env.manifest.json:\n  %s\n"+
			"Either declare them (class kill|route|capacity + a reader) or drop them from "+
			"sendqueue.SendPathEnvNames — a lever /health names and the deploy does not know is drift.",
			strings.Join(missing, "\n  "))
	}
	if len(wrongClass) > 0 {
		sort.Strings(wrongClass)
		t.Errorf("send-path levers declared with a non-lever class:\n  %s", strings.Join(wrongClass, "\n  "))
	}
}

// TestEnvManifestSendRoutingLeversDocumented: the four routing envs plus the
// runtime kill switch must each carry a note that says what they DO, because the
// REQ-090 split (ENABLED = wiring, ALL/allowlists = routing) lives nowhere else
// an operator will read before flipping one.
func TestEnvManifestSendRoutingLeversDocumented(t *testing.T) {
	_, byName := loadEnvManifest(t)
	for _, name := range []string{
		"KAFKA_SEND_QUEUE_ENABLED",
		"KAFKA_SEND_QUEUE_ALL",
		"KAFKA_SEND_QUEUE_WAVES",
		"KAFKA_SEND_QUEUE_CAMPAIGNS",
		"KAFKA_FLAG_SEND_ROUTE",
	} {
		e, ok := byName[name]
		if !ok {
			t.Errorf("%s is not declared in the manifest", name)
			continue
		}
		if len(strings.TrimSpace(e.Note)) < 40 {
			t.Errorf("%s: note must document the kill-switch semantics (REQ-090), got %q", name, e.Note)
		}
		if !strings.Contains(e.Note, "REQ-090") {
			t.Errorf("%s: note does not reference the semantics change (REQ-090)", name)
		}
	}
	// ENABLED must say it is wiring-only; ALL must say it is routing.
	if n := byName["KAFKA_SEND_QUEUE_ENABLED"].Note; !strings.Contains(n, "WIRING") {
		t.Errorf("KAFKA_SEND_QUEUE_ENABLED note must say WIRING ONLY, got %q", n)
	}
	if n := byName["KAFKA_SEND_QUEUE_ALL"].Note; !strings.Contains(n, "ROUTING") {
		t.Errorf("KAFKA_SEND_QUEUE_ALL note must say ROUTING, got %q", n)
	}
}

// TestEnvManifestRoutingHealthWiring proves the REQ-090 block actually REACHES
// the /health JSON — a field that compiles but never reaches the payload is this
// repo's most-shipped bug (see TestEnvManifestHealthWiring for the same guard on
// build/migrations).
//
// It also pins the operator-facing meaning of the :1077 state: enabled=true with
// routing.active=false is the legitimate DRAIN state, and it must be readable as
// such rather than as "the transport is on".
func TestEnvManifestRoutingHealthWiring(t *testing.T) {
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "1")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "0")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	t.Setenv("DISABLE_SETBASED_ENQUEUE", "")
	t.Setenv("WAVE_PROCESSOR_TIMEOUT_SECONDS", "120")
	sendqueue.SetSendRouteFlag(nil)

	// The :1077 shape: the send queue is WIRED (consumer draining) but nothing
	// is routed.
	setEventBusStatus(&eventBusHandle{enabled: true, sendQueueEnabled: true, queueWriterStarted: true})
	t.Cleanup(func() { api.SetEventBusStatusProvider(nil) })

	rec := httptest.NewRecorder()
	api.NewHealthChecker(nil, nil, nil, "").
		HandleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health returned %d", rec.Code)
	}

	var payload struct {
		EventBus struct {
			Flags struct {
				SendRoute bool `json:"send_route"`
			} `json:"flags"`
			SendQueue struct {
				Enabled      bool              `json:"enabled"`
				Routing      map[string]any    `json:"routing"`
				KillSwitches map[string]string `json:"kill_switches"`
			} `json:"send_queue"`
		} `json:"event_bus"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /health: %v", err)
	}

	r := payload.EventBus.SendQueue.Routing
	if r == nil {
		t.Fatal("event_bus.send_queue.routing is absent from /health")
	}
	for _, k := range []string{"all", "enabled", "waves", "campaigns", "active", "wiring_enabled", "flag_open"} {
		if _, ok := r[k]; !ok {
			t.Errorf("routing.%s missing from /health", k)
		}
	}
	if r["all"] != "0" || r["enabled"] != "1" {
		t.Errorf("routing must carry the envs VERBATIM, got all=%v enabled=%v", r["all"], r["enabled"])
	}
	if r["active"] != false {
		t.Errorf("routing.active must be FALSE with ALL=0 (nothing routes), got %v", r["active"])
	}
	if r["wiring_enabled"] != true {
		t.Errorf("routing.wiring_enabled must be TRUE with ENABLED=1 (consumer draining), got %v", r["wiring_enabled"])
	}
	if !payload.EventBus.SendQueue.Enabled {
		t.Error("send_queue.enabled should still report the wiring state")
	}
	if !payload.EventBus.Flags.SendRoute {
		t.Error("flags.send_route must be TRUE (open) when no kill switch is installed")
	}

	ks := payload.EventBus.SendQueue.KillSwitches
	if ks["KAFKA_SEND_QUEUE_ALL"] != "0" {
		t.Errorf("kill_switches must list the explicitly-set KAFKA_SEND_QUEUE_ALL=0, got %v", ks)
	}
	if ks["WAVE_PROCESSOR_TIMEOUT_SECONDS"] != "120" {
		t.Errorf("kill_switches must list WAVE_PROCESSOR_TIMEOUT_SECONDS=120, got %v", ks)
	}
	if _, present := ks["DISABLE_SETBASED_ENQUEUE"]; present {
		t.Errorf("an UNSET lever must not appear in kill_switches, got %v", ks)
	}
}

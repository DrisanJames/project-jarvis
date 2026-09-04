package analytics

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCanonicalEventType(t *testing.T) {
	cases := map[string]string{
		"opened":         "open",
		"clicked":        "click",
		"sent":           "attempted",
		"deferred":       "delivery_delay",
		"complained":     "complaint",
		"delivered":      "delivered",      // pass-through
		"relayed_to_ses": "relayed_to_ses", // pass-through
		"hard_bounce":    "hard_bounce",    // pass-through
		"soft_bounce":    "soft_bounce",    // pass-through
	}
	for in, want := range cases {
		if got := CanonicalEventType(in); got != want {
			t.Errorf("CanonicalEventType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEventTime(t *testing.T) {
	// RFC3339 parses to the exact instant.
	got := parseEventTime("2026-06-08T22:15:30Z")
	want := time.Date(2026, 6, 8, 22, 15, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseEventTime RFC3339 = %v, want %v", got, want)
	}
	// Empty falls back to ~now (not zero).
	if parseEventTime("").IsZero() {
		t.Error("parseEventTime(\"\") should not be zero")
	}
	// Garbage falls back to ~now (not zero), never panics.
	if parseEventTime("not-a-time").IsZero() {
		t.Error("parseEventTime(garbage) should not be zero")
	}
}

// enqueue must derive dt/epoch/ingested_at and never overwrite a provided
// source. We test the derivation logic directly (no Firehose client needed).
func TestEnqueueDerivations(t *testing.T) {
	e := &Emitter{ch: make(chan Event, 4)}
	e.enqueue(Event{EventAt: "2026-06-08T22:15:30Z", Source: "pmta"})
	select {
	case got := <-e.ch:
		if got.Dt != "2026-06-08" {
			t.Errorf("dt = %q, want 2026-06-08", got.Dt)
		}
		if got.EventEpochMs != time.Date(2026, 6, 8, 22, 15, 30, 0, time.UTC).UnixMilli() {
			t.Errorf("epoch_ms = %d", got.EventEpochMs)
		}
		if got.Source != "pmta" {
			t.Errorf("source = %q, want pmta (must not be overwritten)", got.Source)
		}
		if got.IngestedAt == "" {
			t.Error("ingested_at should be set")
		}
	default:
		t.Fatal("expected an event on the channel")
	}

	// Default source when unset.
	e.enqueue(Event{EventAt: "2026-06-08T01:02:03Z"})
	got := <-e.ch
	if got.Source != "app" {
		t.Errorf("default source = %q, want app", got.Source)
	}
}

// Emit is a no-op (and must not panic) when the global emitter is unset.
func TestEmitDisabledIsNoop(t *testing.T) {
	mu.Lock()
	lake = nil
	mu.Unlock()
	Emit(Event{EventType: "delivered"}) // must not panic
	if sent, failed, dropped, enabled := Stats(); enabled || sent != 0 || failed != 0 || dropped != 0 {
		t.Errorf("disabled Stats() = (%d,%d,%d,%v), want zeros/false", sent, failed, dropped, enabled)
	}
}

// The JSON field names are the Glue column contract — lock them down.
func TestEventJSONTags(t *testing.T) {
	b, err := json.Marshal(Event{EventUID: "x", EventType: "delivered", Source: "ses", Dt: "2026-06-08"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	required := []string{
		"event_uid", "recipient_send_id", "campaign_id", "subscriber_id", "email",
		"email_domain", "brand", "isp_group", "route_type", "event_type",
		"suppression_reason", "vmta", "pool", "bounce_cat", "dsn_code", "dsn_diag",
		"link_url", "source_ip", "variant", "event_at", "event_epoch_ms",
		"ingested_at", "source", "dt",
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	for _, r := range required {
		if _, ok := m[r]; !ok {
			t.Errorf("missing JSON key %q (Glue column contract). got keys: %v", r, reflect.ValueOf(got))
		}
	}
}

// TestEvent_IsMachineClick_OmittedWhenNil pins the Firehose wire contract for
// the click label (METRIC_CONTRACT §12). The Glue column is nullable and
// `is_machine_click IS NOT NULL` is read as classification COVERAGE, so a nil
// pointer must vanish from the JSON entirely — emitting a bare `false` on the
// ~5M non-click rows/day would claim classification we never performed.
func TestEvent_IsMachineClick_OmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(Event{EventUID: "ses:x", EventType: "open"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "is_machine_click") {
		t.Fatalf("nil IsMachineClick must be omitted, got %s", b)
	}

	f := false
	b, _ = json.Marshal(Event{EventUID: "ses:y", EventType: "click", IsMachineClick: &f})
	if !strings.Contains(string(b), `"is_machine_click":false`) {
		t.Fatalf("classified-human click must emit false, got %s", b)
	}

	tr := true
	b, _ = json.Marshal(Event{EventUID: "ses:z", EventType: "click", IsMachineClick: &tr})
	if !strings.Contains(string(b), `"is_machine_click":true`) {
		t.Fatalf("classified-machine click must emit true, got %s", b)
	}
}

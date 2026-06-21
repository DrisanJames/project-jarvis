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

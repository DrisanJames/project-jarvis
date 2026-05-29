package worker

import (
	"testing"
)

// TestDispatchCampaignParallelism_Default verifies the default fan-out (8)
// kicks in whenever the env var is unset or invalid. This is the live path
// in production unless an operator explicitly overrides it during a sending
// incident.
func TestDispatchCampaignParallelism_Default(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"unset", ""},
		{"empty whitespace", "   "},
		{"non-numeric", "abc"},
		{"zero", "0"},
		{"negative", "-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISPATCH_CAMPAIGN_PARALLELISM", tc.env)
			if got := dispatchCampaignParallelism(); got != 8 {
				t.Fatalf("expected default 8, got %d", got)
			}
		})
	}
}

// TestDispatchCampaignParallelism_Override locks the operator escape hatch.
// "1" disables the parallel fan-out (legacy serial behavior, useful for
// diagnostic A/B). Above 32 we clamp so a stray value cannot exhaust the
// 40-conn DB pool.
func TestDispatchCampaignParallelism_Override(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"1", 1},
		{"4", 4},
		{"8", 8},
		{"16", 16},
		{"32", 32},
		{"64", 32},   // clamped
		{"1000", 32}, // clamped
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("DISPATCH_CAMPAIGN_PARALLELISM", tc.env)
			if got := dispatchCampaignParallelism(); got != tc.want {
				t.Fatalf("env=%s: want %d, got %d", tc.env, tc.want, got)
			}
		})
	}
}

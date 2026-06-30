package api

import (
	"math"
	"testing"
	"time"
)

// TestParseMonthArg locks the {month} path-param normalization: both "YYYY-MM"
// and "YYYY-MM-DD" collapse to the 1st of the month (UTC), and junk is rejected.
func TestParseMonthArg(t *testing.T) {
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"2026-07", "2026-07-15", " 2026-07 ", "2026-07-01"} {
		got, err := parseMonthArg(in)
		if err != nil {
			t.Fatalf("parseMonthArg(%q) unexpected error: %v", in, err)
		}
		if !got.Equal(want) {
			t.Fatalf("parseMonthArg(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "2026", "07-2026", "2026/07", "nope"} {
		if _, err := parseMonthArg(bad); err == nil {
			t.Fatalf("parseMonthArg(%q) expected error, got nil", bad)
		}
	}
}

// TestCpmMonthlyRevenue locks the CPM-billable formula: delivered/1000 × eCPM.
func TestCpmMonthlyRevenue(t *testing.T) {
	cases := []struct {
		name      string
		delivered int64
		ecpm      float64
		want      float64
	}{
		{"sams_june", 2765222, 1.00, 2765.222},          // ~2.76M delivered @ $1.00 eCPM
		{"ten_million_at_one", 10_000_000, 1.00, 10000}, // July Sam's plan basis
		{"half_eCPM", 2_000_000, 0.50, 1000},
		{"zero_delivered", 0, 1.00, 0},
		{"zero_ecpm_guard", 1_000_000, 0, 0},
		{"negative_guard", -5, 1.00, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cpmMonthlyRevenue(c.delivered, c.ecpm)
			if math.Abs(got-c.want) > 0.001 {
				t.Fatalf("cpmMonthlyRevenue(%d,%v) = %v, want %v", c.delivered, c.ecpm, got, c.want)
			}
		})
	}
}

// TestCpmMonthlyConversionsNoOverride is the regression guard for the footgun:
// per-month conversions are tracked+manual for THAT month — the lifetime
// conversions_override must NOT bleed into a single month. The helper takes no
// override param, so the rule holds by construction; this pins the arithmetic.
func TestCpmMonthlyConversions(t *testing.T) {
	cases := []struct {
		tracked, manual, want int64
	}{
		{0, 0, 0},
		{5, 0, 5},
		{0, 7, 7},
		{12, 3, 15},
	}
	for _, c := range cases {
		if got := cpmMonthlyConversions(c.tracked, c.manual); got != c.want {
			t.Fatalf("cpmMonthlyConversions(%d,%d) = %d, want %d", c.tracked, c.manual, got, c.want)
		}
	}
}

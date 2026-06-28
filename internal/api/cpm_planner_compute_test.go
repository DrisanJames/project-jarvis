package api

import (
	"math"
	"testing"
)

func i64p(v int64) *int64 { return &v }

// TestCpmActualEcpa locks the 2026-06-28 fix: eCPA Actual = full budget /
// conversions (NOT the old budget×pct_delivered form, which cancelled the
// budget out and never responded to budget edits).
func TestCpmActualEcpa(t *testing.T) {
	cases := []struct {
		name        string
		budget      float64
		conversions int64
		want        float64
	}{
		{"sams_81", 3700, 81, 45.679012}, // operator's reference: $3,700 / 81
		{"liberty", 2000, 3, 666.666666},
		{"zero_conversions_no_divzero", 2000, 0, 0},
		{"negative_guard", 2000, -5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cpmActualEcpa(c.budget, c.conversions)
			if math.Abs(got-c.want) > 0.001 {
				t.Fatalf("cpmActualEcpa(%v,%d) = %v, want %v", c.budget, c.conversions, got, c.want)
			}
		})
	}
}

// TestCpmActualEcpaRespondsToBudget is the direct regression guard for the
// reported bug: editing the budget MUST change eCPA Actual. The old formula
// cancelled the budget against planned_volume, so this would have stayed equal.
func TestCpmActualEcpaRespondsToBudget(t *testing.T) {
	const conv = 81
	lo := cpmActualEcpa(2500, conv)
	hi := cpmActualEcpa(3700, conv)
	if !(hi > lo) {
		t.Fatalf("eCPA must rise with budget: budget 2500 -> %v, 3700 -> %v", lo, hi)
	}
}

// TestCpmEffectiveConversions locks the override-vs-computed precedence.
func TestCpmEffectiveConversions(t *testing.T) {
	cases := []struct {
		name             string
		tracked, manual  int64
		override         *int64
		want             int64
	}{
		{"no_override_sums", 51, 13, nil, 64},
		{"override_wins", 51, 13, i64p(81), 81},
		{"override_zero_ignored", 51, 13, i64p(0), 64},
		{"override_negative_ignored", 51, 13, i64p(-1), 64},
		{"no_conversions", 0, 0, nil, 0},
		{"override_only", 0, 0, i64p(81), 81},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cpmEffectiveConversions(c.tracked, c.manual, c.override)
			if got != c.want {
				t.Fatalf("cpmEffectiveConversions(%d,%d,%v) = %d, want %d",
					c.tracked, c.manual, c.override, got, c.want)
			}
		})
	}
}

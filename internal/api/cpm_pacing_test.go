package api

import "testing"

func TestCpmPacingMath(t *testing.T) {
	tests := []struct {
		name                    string
		target, mtd             int64
		rate3d                  float64
		dayOfMonth, daysInMonth int
		wantRequired            float64
		wantProjected           int64
		wantOnPace              bool
	}{
		{
			// Mid-month, behind: 1M target, 300k done on day 15 of 30.
			// 700k remaining over 16 days (incl today) = 43,750/day.
			// Projection: 300k + 20k/day × 15 days after today = 600k → behind.
			name: "mid-month behind", target: 1_000_000, mtd: 300_000, rate3d: 20_000,
			dayOfMonth: 15, daysInMonth: 30,
			wantRequired: 43_750, wantProjected: 600_000, wantOnPace: false,
		},
		{
			// Ahead of pace: projection clears the target.
			name: "ahead", target: 600_000, mtd: 400_000, rate3d: 20_000,
			dayOfMonth: 15, daysInMonth: 30,
			wantRequired: 12_500, wantProjected: 700_000, wantOnPace: true,
		},
		{
			// Target already met: no required daily, still on pace.
			name: "target met", target: 500_000, mtd: 500_000, rate3d: 10_000,
			dayOfMonth: 20, daysInMonth: 31,
			wantRequired: 0, wantProjected: 610_000, wantOnPace: true,
		},
		{
			// Last day of month: no days after today — projection is just MTD;
			// required spreads over exactly 1 day (today).
			name: "last day", target: 100_000, mtd: 90_000, rate3d: 50_000,
			dayOfMonth: 30, daysInMonth: 30,
			wantRequired: 10_000, wantProjected: 90_000, wantOnPace: false,
		},
		{
			// First day: everything remaining over the full month.
			name: "first day", target: 310_000, mtd: 0, rate3d: 0,
			dayOfMonth: 1, daysInMonth: 31,
			wantRequired: 10_000, wantProjected: 0, wantOnPace: false,
		},
		{
			// No target (untargeted deal with activity): required 0, never
			// "on pace" — pct/onPace only mean something against a target.
			name: "no target", target: 0, mtd: 50_000, rate3d: 5_000,
			dayOfMonth: 10, daysInMonth: 30,
			wantRequired: 0, wantProjected: 150_000, wantOnPace: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			required, projected, pct, onPace := cpmPacingMath(tc.target, tc.mtd, tc.rate3d, tc.dayOfMonth, tc.daysInMonth)
			if required != tc.wantRequired {
				t.Errorf("requiredDaily = %v, want %v", required, tc.wantRequired)
			}
			if projected != tc.wantProjected {
				t.Errorf("projected = %v, want %v", projected, tc.wantProjected)
			}
			if onPace != tc.wantOnPace {
				t.Errorf("onPace = %v, want %v", onPace, tc.wantOnPace)
			}
			if tc.target > 0 {
				wantPct := float64(tc.wantProjected) / float64(tc.target)
				if pct != wantPct {
					t.Errorf("projectedPct = %v, want %v", pct, wantPct)
				}
			} else if pct != 0 {
				t.Errorf("projectedPct = %v, want 0 for zero target", pct)
			}
		})
	}
}

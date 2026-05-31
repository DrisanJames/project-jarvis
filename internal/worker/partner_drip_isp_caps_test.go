package worker

import "testing"

func TestIspCapForDrainHorizon(t *testing.T) {
	const wavesPerDay = 384 // 96 ticks × 4 brands @ 15m cadence

	tests := []struct {
		name       string
		ready      int
		base       int
		drainDays  int
		want       int
	}{
		{"gmail 3d refi backlog", 15591, 200, 3, 14},
		{"gmail 3d system backlog", 51758, 200, 3, 45},
		{"yahoo 3d", 9991, 20, 3, 9},
		{"sbcglobal 3d", 4668, 60, 3, 5},
		{"aol 3d", 5827, 20, 3, 6},
		{"att 2d", 1915, 60, 2, 3},
		{"empty backlog", 0, 200, 3, 0},
		{"small backlog min 1", 100, 200, 3, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ispCapForDrainHorizon(tc.ready, tc.base, tc.drainDays, wavesPerDay)
			if got != tc.want {
				t.Fatalf("ispCapForDrainHorizon(%d, %d, %d, %d) = %d, want %d",
					tc.ready, tc.base, tc.drainDays, wavesPerDay, got, tc.want)
			}
		})
	}
}

func TestIspCapForDrainHorizonQueueRefill(t *testing.T) {
	const wavesPerDay = 384
	base := 200
	days := 3

	// Queue doubles mid-drain — cap should double while staying under base.
	low := ispCapForDrainHorizon(50000, base, days, wavesPerDay)
	high := ispCapForDrainHorizon(100000, base, days, wavesPerDay)
	if high <= low {
		t.Fatalf("refilled queue should raise cap: low=%d high=%d", low, high)
	}
	if high > base {
		t.Fatalf("cap must not exceed base ceiling: got %d base %d", high, base)
	}
}

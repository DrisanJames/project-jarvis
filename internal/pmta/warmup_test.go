package pmta

import (
	"testing"
)

func TestVolumeForDay(t *testing.T) {
	tests := []struct {
		day  int
		want int
	}{
		{1, 50}, {5, 250}, {11, 1000}, {15, 2500},
		{23, 10000}, {30, 25000}, {31, 50000}, {0, 50},
	}

	for _, tt := range tests {
		got := volumeForDay(tt.day)
		if got != tt.want {
			t.Errorf("volumeForDay(%d) = %d, want %d", tt.day, got, tt.want)
		}
	}
}

func TestStageForDay(t *testing.T) {
	tests := []struct {
		day  int
		want string
	}{
		{1, "day1"}, {2, "day1"}, {3, "early"}, {7, "early"},
		{8, "building"}, {14, "building"}, {15, "ramping"}, {22, "ramping"},
		{23, "maturing"}, {30, "maturing"}, {31, "established"},
	}

	for _, tt := range tests {
		got := stageForDay(tt.day)
		if got != tt.want {
			t.Errorf("stageForDay(%d) = %q, want %q", tt.day, got, tt.want)
		}
	}
}

func TestWarmupSchedulerDefaults(t *testing.T) {
	ws := NewWarmupScheduler(nil, 0)
	if ws.SeedThreshold != 0.75 {
		t.Errorf("SeedThreshold = %f, want 0.75", ws.SeedThreshold)
	}
	if ws.ValidateThreshold != 0.50 {
		t.Errorf("ValidateThreshold = %f, want 0.50", ws.ValidateThreshold)
	}
	if ws.ExpandThreshold != 0.25 {
		t.Errorf("ExpandThreshold = %f, want 0.25", ws.ExpandThreshold)
	}
}

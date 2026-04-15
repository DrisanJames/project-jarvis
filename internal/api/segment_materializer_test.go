package api

import (
	"testing"
	"time"
)

func TestDurationUntilNext(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		hour     int
		min      int
		wantWait time.Duration
	}{
		{
			name:     "10 minutes before target",
			now:      time.Date(2026, 4, 15, 3, 50, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 10 * time.Minute,
		},
		{
			name:     "5 minutes after target wraps to next day",
			now:      time.Date(2026, 4, 15, 4, 5, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 23*time.Hour + 55*time.Minute,
		},
		{
			name:     "exactly at target wraps to next day",
			now:      time.Date(2026, 4, 15, 4, 0, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 24 * time.Hour,
		},
		{
			name:     "midnight targeting 04:00",
			now:      time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 4 * time.Hour,
		},
		{
			name:     "23:59 targeting 04:00 next day",
			now:      time.Date(2026, 4, 15, 23, 59, 0, 0, time.UTC),
			hour:     4, min: 0,
			wantWait: 4*time.Hour + 1*time.Minute,
		},
		{
			name:     "with minutes in target",
			now:      time.Date(2026, 4, 15, 4, 15, 0, 0, time.UTC),
			hour:     4, min: 30,
			wantWait: 15 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DurationUntilNext(tt.now, tt.hour, tt.min)
			if got != tt.wantWait {
				t.Errorf("DurationUntilNext(%v, %d:%02d) = %v, want %v",
					tt.now.Format("15:04"), tt.hour, tt.min, got, tt.wantWait)
			}
		})
	}
}

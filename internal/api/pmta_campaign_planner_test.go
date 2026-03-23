package api

import (
	"testing"
	"time"
)

func TestMinSpanForVolume(t *testing.T) {
	tests := []struct {
		name       string
		recipients int
		want       time.Duration
	}{
		{"0 (unlimited) uses full 8h", 0, 8 * time.Hour},
		{"negative uses full 8h", -1, 8 * time.Hour},
		{"100 recipients = 1h (floor)", 100, 1 * time.Hour},
		{"50 recipients = 1h (floor clamp)", 50, 1 * time.Hour},
		{"500 recipients = 5h", 500, 5 * time.Hour},
		{"600 recipients = 6h", 600, 6 * time.Hour},
		{"800 recipients = 8h (matches default)", 800, 8 * time.Hour},
		{"1000 recipients = 8h (cap)", 1000, 8 * time.Hour},
		{"5000 recipients = 8h (cap)", 5000, 8 * time.Hour},
		{"200 recipients = 2h", 200, 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := minSpanForVolume(tt.recipients)
			if got != tt.want {
				t.Errorf("minSpanForVolume(%d) = %v, want %v", tt.recipients, got, tt.want)
			}
		})
	}
}

func TestWaveSanityCheck_ProportionalSpan(t *testing.T) {
	now := time.Now().UTC()

	makeWaves := func(span time.Duration, totalRecipients int) []pmtaWaveSpec {
		waves := make([]pmtaWaveSpec, 4)
		perWave := totalRecipients / 4
		for i := 0; i < 4; i++ {
			planned := perWave
			if i == 3 {
				planned = totalRecipients - perWave*3
			}
			waves[i] = pmtaWaveSpec{
				WaveNumber:        i + 1,
				ScheduledAt:       now.Add(time.Duration(i) * span / 3),
				PlannedRecipients: planned,
			}
		}
		return waves
	}

	tests := []struct {
		name       string
		quota      int
		recipients int
		span       time.Duration
		wantErr    bool
	}{
		{"600 actual recipients, 6h span — passes", 2000, 600, 6 * time.Hour, false},
		{"600 actual recipients, 4h span — fails", 2000, 600, 4 * time.Hour, true},
		{"270 actual recipients (quota 1500), 7h15m span — passes (under threshold)", 1500, 270, 7*time.Hour + 15*time.Minute, false},
		{"500 actual recipients, 5h span — passes", 1500, 500, 5 * time.Hour, false},
		{"500 actual recipients, 3h span — fails", 1500, 500, 3 * time.Hour, true},
		{"1000 actual recipients, 8h span — passes", 1500, 1000, 8 * time.Hour, false},
		{"1000 actual recipients, 7h span — fails", 1500, 1000, 7 * time.Hour, true},
		{"300 actual recipients (quota 1500), any span — skipped", 1500, 300, 1 * time.Hour, false},
		{"200 actual recipients (quota 800), any span — skipped", 800, 200, 30 * time.Minute, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plans := []pmtaNormalizedPlan{{
				ISP:   "charter",
				Quota: tt.quota,
			}}
			wavesByISP := map[string][]pmtaWaveSpec{
				"charter": makeWaves(tt.span, tt.recipients),
			}
			err := waveSanityCheck(plans, wavesByISP)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWaveSanityCheck_QuotaVsActualCount(t *testing.T) {
	now := time.Now().UTC()

	t.Run("high quota low actual count skips check", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "charter",
			Quota: 1500,
		}}
		waves := map[string][]pmtaWaveSpec{
			"charter": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 70},
				{WaveNumber: 2, ScheduledAt: now.Add(15 * time.Minute), PlannedRecipients: 70},
				{WaveNumber: 3, ScheduledAt: now.Add(30 * time.Minute), PlannedRecipients: 70},
				{WaveNumber: 4, ScheduledAt: now.Add(45 * time.Minute), PlannedRecipients: 60},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("should skip: actual recipients (270) < 500 threshold, but got: %v", err)
		}
	})

	t.Run("high quota high actual count enforces check", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("should fail: 2000 recipients in 3h span, but passed")
		}
	})
}

func TestWaveSanityCheck_UserExplicitDurationBypass(t *testing.T) {
	now := time.Now().UTC()

	t.Run("user-explicit duration-calc bypasses min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "microsoft",
			Quota: 1500,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(4 * time.Hour),
				Source:  "duration-calc",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"microsoft": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 213},
				{WaveNumber: 2, ScheduledAt: now.Add(90 * time.Minute), PlannedRecipients: 213},
				{WaveNumber: 3, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 214},
				{WaveNumber: 4, ScheduledAt: now.Add(4*time.Hour + 30*time.Minute), PlannedRecipients: 213},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("user-explicit duration should bypass min-span check, got: %v", err)
		}
	})

	t.Run("user-explicit manual source bypasses min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(3 * time.Hour),
				Source:  "manual",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err != nil {
			t.Errorf("manual source should bypass min-span check, got: %v", err)
		}
	})

	t.Run("auto-generated span still enforces min-span", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "gmail",
			Quota: 5000,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(3 * time.Hour),
				Source:  "default_throttle_window",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"gmail": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 500},
				{WaveNumber: 2, ScheduledAt: now.Add(1 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 3, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 500},
				{WaveNumber: 4, ScheduledAt: now.Add(3 * time.Hour), PlannedRecipients: 500},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("auto-generated span should still enforce min-span, but passed")
		}
	})

	t.Run("user-explicit still enforces min-wave-count", func(t *testing.T) {
		plans := []pmtaNormalizedPlan{{
			ISP:   "microsoft",
			Quota: 1500,
			TimeSpans: []pmtaNormalizedTimeSpan{{
				StartAt: now,
				EndAt:   now.Add(4 * time.Hour),
				Source:  "duration-calc",
			}},
		}}
		waves := map[string][]pmtaWaveSpec{
			"microsoft": {
				{WaveNumber: 1, ScheduledAt: now, PlannedRecipients: 400},
				{WaveNumber: 2, ScheduledAt: now.Add(2 * time.Hour), PlannedRecipients: 400},
				{WaveNumber: 3, ScheduledAt: now.Add(4 * time.Hour), PlannedRecipients: 400},
			},
		}
		err := waveSanityCheck(plans, waves)
		if err == nil {
			t.Error("user-explicit should still enforce min-wave-count, but passed")
		}
	})
}

func TestIsUserExplicitSpan(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"duration-calc", true},
		{"manual", true},
		{"default_throttle_window", false},
		{"legacy_throttle_window", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			if got := isUserExplicitSpan(tt.source); got != tt.want {
				t.Errorf("isUserExplicitSpan(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

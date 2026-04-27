package worker

import (
	"testing"
	"time"
)

// Phase 2 (Welcome Series) — exercise the executeDelayNode helpers in
// isolation so we don't need to stand up the journey executor + DB just
// to verify timezone math. The helper functions are pure: given a config
// map and a `now` instant they return the wall-clock UTC time the
// enrollment should resume at.

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s) failed: %v", name, err)
	}
	return loc
}

func TestComputeDelayWaitUntil_FixedFallsBackToHours(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	cfg := map[string]interface{}{
		// no delayValue/delayUnit -> defaults to 1 hour
	}
	got := computeDelayWaitUntil(cfg, "", now)
	want := now.Add(1 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("default fixed delay: got %s want %s", got, want)
	}
}

func TestComputeDelayWaitUntil_FixedHonorsValueAndUnit(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	cfg := map[string]interface{}{
		"delayValue": float64(3),
		"delayUnit":  "days",
	}
	got := computeDelayWaitUntil(cfg, "fixed", now)
	want := now.Add(3 * 24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("fixed days delay: got %s want %s", got, want)
	}
}

func TestComputeUntilTime_DefaultsTo9amDenver(t *testing.T) {
	denver := mustLoad(t, "America/Denver")
	// 8:00am Denver -> 9:00am Denver same day
	now := time.Date(2026, 4, 27, 8, 0, 0, 0, denver)
	got := computeUntilTime(map[string]interface{}{}, now)
	wantLocal := time.Date(2026, 4, 27, 9, 0, 0, 0, denver)
	if !got.Equal(wantLocal) {
		t.Fatalf("default 9am Denver: got %s want %s", got, wantLocal)
	}
}

func TestComputeUntilTime_RollsToTomorrowWhenAlreadyPast(t *testing.T) {
	denver := mustLoad(t, "America/Denver")
	// 9:30am Denver, target 9:00 Denver -> next 9:00 = tomorrow
	now := time.Date(2026, 4, 27, 9, 30, 0, 0, denver)
	cfg := map[string]interface{}{
		"untilTime":     "09:00",
		"untilTimezone": "America/Denver",
	}
	got := computeUntilTime(cfg, now)
	wantLocal := time.Date(2026, 4, 28, 9, 0, 0, 0, denver)
	if !got.Equal(wantLocal) {
		t.Fatalf("roll to tomorrow: got %s want %s", got, wantLocal)
	}
}

func TestComputeUntilTime_RespectsConfiguredTimezone(t *testing.T) {
	pacific := mustLoad(t, "America/Los_Angeles")
	denver := mustLoad(t, "America/Denver")

	// 8:00am Pacific = 9:00am Mountain. Targeting 9:00am Pacific should
	// resolve to 1h later (in UTC), not the same instant.
	now := time.Date(2026, 4, 27, 8, 0, 0, 0, pacific)
	cfg := map[string]interface{}{
		"untilTime":     "09:00",
		"untilTimezone": "America/Los_Angeles",
	}
	got := computeUntilTime(cfg, now)
	want := time.Date(2026, 4, 27, 9, 0, 0, 0, pacific).UTC()
	if !got.Equal(want) {
		t.Fatalf("pacific 9am: got %s want %s", got, want)
	}
	// Sanity: 9am Pacific != 9am Mountain in UTC.
	mtNine := time.Date(2026, 4, 27, 9, 0, 0, 0, denver).UTC()
	if got.Equal(mtNine) {
		t.Fatal("computeUntilTime ignored timezone (resolved to MT 9am)")
	}
}

func TestComputeUntilTime_InvalidTimezoneFallsBackToUTC(t *testing.T) {
	now := time.Date(2026, 4, 27, 6, 0, 0, 0, time.UTC)
	cfg := map[string]interface{}{
		"untilTime":     "09:00",
		"untilTimezone": "Not/A/Real/TZ",
	}
	got := computeUntilTime(cfg, now)
	want := time.Date(2026, 4, 27, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("invalid tz fallback: got %s want %s", got, want)
	}
}

func TestComputeUntilTime_InvalidHHMMDefaultsToNine(t *testing.T) {
	denver := mustLoad(t, "America/Denver")
	now := time.Date(2026, 4, 27, 6, 0, 0, 0, denver)
	cfg := map[string]interface{}{
		"untilTime":     "garbage",
		"untilTimezone": "America/Denver",
	}
	got := computeUntilTime(cfg, now)
	want := time.Date(2026, 4, 27, 9, 0, 0, 0, denver)
	if !got.Equal(want) {
		t.Fatalf("garbage HH:MM fallback: got %s want %s", got, want)
	}
}

func TestComputeDelayWaitUntil_UntilTimeBranch(t *testing.T) {
	denver := mustLoad(t, "America/Denver")
	now := time.Date(2026, 4, 27, 6, 0, 0, 0, denver)
	cfg := map[string]interface{}{
		"untilTime":     "09:00",
		"untilTimezone": "America/Denver",
	}
	got := computeDelayWaitUntil(cfg, "until_time", now)
	want := time.Date(2026, 4, 27, 9, 0, 0, 0, denver)
	if !got.Equal(want) {
		t.Fatalf("until_time branch: got %s want %s", got, want)
	}
}

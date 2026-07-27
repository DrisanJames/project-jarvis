package worker

import (
	"context"
	"testing"
	"time"
)

// TestFreshBroadcastTimeUntilNextTick pins the 09:00 UTC daily anchor.
func TestFreshBroadcastTimeUntilNextTick(t *testing.T) {
	w := &FreshBroadcastWorker{}
	// Before the anchor → today 09:00.
	now := time.Date(2026, 7, 28, 6, 30, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(now); got != 2*time.Hour+30*time.Minute {
		t.Errorf("06:30 → %s, want 2h30m", got)
	}
	// At/after the anchor → tomorrow 09:00.
	now = time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(now); got != 24*time.Hour {
		t.Errorf("09:00 → %s, want 24h", got)
	}
}

// TestFreshBroadcastKillSwitch — the negative path: with the kill switch set,
// Start returns immediately and never invokes the runner.
func TestFreshBroadcastKillSwitch(t *testing.T) {
	t.Setenv("DISABLE_FRESH_BROADCAST_RUNNER", "true")
	invoked := false
	w := NewFreshBroadcastWorker(nil, nil, func(ctx context.Context, date string, dry, autoStageOnly bool, trigger string) error {
		invoked = true
		return nil
	})
	// nil db also guards, but the kill switch must fire first for a real db;
	// Start must return without blocking either way.
	done := make(chan struct{})
	go func() {
		w.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return under the kill switch")
	}
	if invoked {
		t.Error("runner invoked despite kill switch")
	}
}

// TestFreshBroadcastNilGuards — a worker with no run fn or db is inert.
func TestFreshBroadcastNilGuards(t *testing.T) {
	w := NewFreshBroadcastWorker(nil, nil, nil)
	done := make(chan struct{})
	go func() {
		w.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return with nil deps")
	}
}

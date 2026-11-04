package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

type flushCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (f *flushCounter) fn(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[id]++
	return nil
}

func (f *flushCounter) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[id]
}

// newTestTracker returns a tracker with a controllable clock and a counting
// flush hook (no S3).
func newTestTracker(t *testing.T) (*CampaignEventTracker, *flushCounter, *time.Time) {
	t.Helper()
	tr := NewCampaignEventTracker(nil, "test-bucket")
	t.Cleanup(tr.Stop)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return clock }
	fc := &flushCounter{n: map[string]int{}}
	tr.flush = fc.fn
	return tr, fc, &clock
}

func inMap(tr *CampaignEventTracker, id string) bool {
	return tr.GetMetrics(id) != nil
}

func TestCampaignEventEvict_IdleCampaignFlushedOnceThenRemoved(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, fc, clock := newTestTracker(t)

	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	*clock = clock.Add(61 * time.Minute) // past the 60-min idle window, never flushed

	tr.FlushAll(context.Background())
	if got := fc.count("c1"); got != 1 {
		t.Fatalf("final flush count = %d, want 1", got)
	}
	if inMap(tr, "c1") {
		t.Fatal("idle campaign still in map after final flush")
	}

	tr.FlushAll(context.Background())
	if got := fc.count("c1"); got != 1 {
		t.Fatalf("evicted campaign flushed again: count = %d, want 1", got)
	}
}

func TestCampaignEventFlush_UnchangedInsideWindowNotReflushed(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, fc, clock := newTestTracker(t)

	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "sent"})
	tr.FlushAll(context.Background()) // dirty -> flush
	*clock = clock.Add(5 * time.Minute)
	tr.FlushAll(context.Background()) // no new events -> skip
	*clock = clock.Add(5 * time.Minute)
	tr.FlushAll(context.Background()) // still skip

	if got := fc.count("c1"); got != 1 {
		t.Fatalf("flush count = %d, want 1 (dirty flag must suppress unchanged reports)", got)
	}
	if !inMap(tr, "c1") {
		t.Fatal("campaign inside idle window must stay in map")
	}

	// Idle past the window with nothing new: evicted with no extra write.
	*clock = clock.Add(60 * time.Minute)
	tr.FlushAll(context.Background())
	if got := fc.count("c1"); got != 1 {
		t.Fatalf("clean idle campaign written on eviction: count = %d, want 1", got)
	}
	if inMap(tr, "c1") {
		t.Fatal("clean idle campaign not evicted")
	}
}

func TestCampaignEventFlush_NewEventsFlushEveryTick(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, fc, clock := newTestTracker(t)

	for i := 0; i < 3; i++ {
		tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "open", Recipient: "a@x.com"})
		tr.FlushAll(context.Background())
		*clock = clock.Add(5 * time.Minute)
	}
	if got := fc.count("c1"); got != 3 {
		t.Fatalf("flush count = %d, want 3 (one per tick with new events)", got)
	}
	if m := tr.GetMetrics("c1"); m == nil || m.Opens != 3 {
		t.Fatalf("metrics lost: %+v", m)
	}
}

func TestCampaignEventEvict_KillSwitchRestoresOldBehavior(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "1")
	tr, fc, clock := newTestTracker(t)

	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	for i := 0; i < 4; i++ {
		*clock = clock.Add(61 * time.Minute)
		tr.FlushAll(context.Background())
	}
	if got := fc.count("c1"); got != 4 {
		t.Fatalf("kill switch: flush count = %d, want 4 (every tick)", got)
	}
	if !inMap(tr, "c1") {
		t.Fatal("kill switch: campaign must never be evicted")
	}
}

func TestCampaignEventEvict_FlushErrorKeepsCampaign(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, _, clock := newTestTracker(t)
	calls := 0
	tr.flush = func(context.Context, string) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}

	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	*clock = clock.Add(61 * time.Minute)
	tr.FlushAll(context.Background())
	if !inMap(tr, "c1") {
		t.Fatal("campaign evicted although its final flush failed")
	}
	tr.FlushAll(context.Background())
	if calls != 2 || inMap(tr, "c1") {
		t.Fatalf("retry: calls=%d inMap=%v, want 2/false", calls, inMap(tr, "c1"))
	}
}

func TestCampaignEventEvict_IdleWindowFromEnv(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_IDLE_EVICT_MINUTES", "10")
	if got := campaignIdleEvictFromEnv(); got != 10*time.Minute {
		t.Fatalf("env window = %s, want 10m", got)
	}
	t.Setenv("CAMPAIGN_EVENTS_IDLE_EVICT_MINUTES", "junk")
	if got := campaignIdleEvictFromEnv(); got != defaultCampaignIdleEvict {
		t.Fatalf("bad env = %s, want default", got)
	}
}

func TestCampaignEventEvict_LateEventFragmentNeverWritten(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, fc, clock := newTestTracker(t)

	for i := 0; i < 1000; i++ {
		tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	}
	*clock = clock.Add(61 * time.Minute)
	tr.FlushAll(context.Background()) // final flush (1000 delivered) + evict
	if fc.count("c1") != 1 || inMap(tr, "c1") {
		t.Fatalf("setup: flushes=%d inMap=%v, want 1/false", fc.count("c1"), inMap(tr, "c1"))
	}

	// Never-evicted campaign alongside it: unaffected.
	tr.RecordEvent(CampaignEvent{CampaignID: "c2", EventType: "delivered"})

	// Late open recreates c1 from zero.
	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "open", Recipient: "a@x.com"})
	tr.FlushAll(context.Background())
	*clock = clock.Add(5 * time.Minute)
	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "click", Recipient: "a@x.com"})
	tr.FlushAll(context.Background())

	if got := fc.count("c1"); got != 1 {
		t.Fatalf("resumed fragment was written: flushes=%d, want 1 (zero after the late event)", got)
	}
	if m := tr.GetMetrics("c1"); m == nil || m.Opens != 1 || m.Delivered != 0 {
		t.Fatalf("fragment must stay in memory for the dashboard: %+v", m)
	}
	if got := fc.count("c2"); got != 1 {
		t.Fatalf("never-evicted campaign flushes=%d, want 1", got)
	}

	// Fragment goes idle again -> evicted with no write.
	*clock = clock.Add(61 * time.Minute)
	tr.FlushAll(context.Background())
	if fc.count("c1") != 1 || inMap(tr, "c1") {
		t.Fatalf("re-evict: flushes=%d inMap=%v, want 1/false", fc.count("c1"), inMap(tr, "c1"))
	}
}

func TestCampaignEventEvict_EvictedMemoryPrunedAfterTTL(t *testing.T) {
	t.Setenv("CAMPAIGN_EVENTS_EVICT_DISABLED", "")
	tr, fc, clock := newTestTracker(t)

	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	*clock = clock.Add(61 * time.Minute)
	tr.FlushAll(context.Background())
	*clock = clock.Add(campaignEvictedTTL)
	tr.FlushAll(context.Background()) // prunes the evicted id
	tr.mu.Lock()
	_, remembered := tr.evicted["c1"]
	tr.mu.Unlock()
	if remembered {
		t.Fatal("evicted id not pruned after TTL")
	}
	tr.RecordEvent(CampaignEvent{CampaignID: "c1", EventType: "delivered"})
	tr.FlushAll(context.Background())
	if got := fc.count("c1"); got != 2 {
		t.Fatalf("after TTL the id is a normal campaign again: flushes=%d, want 2", got)
	}
}

package worker

// Tests for DomainThroughputTracker (SA-7, per-domain engagement engine).
//
// Coverage:
//   1. RecordsSends — basic count over a single domain.
//   2. ExpiresOldEntries — events older than windowSize fall off Snapshot.
//   3. ConcurrentSafe — 100 goroutines × 100 sends produce 10000.
//   4. MultipleDomains — interleaved sends to A, B, C return correct counts.
//
// nowFn injection lets the expiry test fast-forward without sleeping —
// real-time-based tests are flaky in CI.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDomainThroughputTracker_RecordsSends asserts a basic happy path:
// 5 sends to a single domain show up as count=5 in Snapshot.
func TestDomainThroughputTracker_RecordsSends(t *testing.T) {
	tr := NewDomainThroughputTracker(60 * time.Second)
	for i := 0; i < 5; i++ {
		tr.RecordSend("em.discountblog.com")
	}
	snap := tr.Snapshot()
	if got, want := snap["em.discountblog.com"], 5; got != want {
		t.Fatalf("snapshot count: got %d, want %d", got, want)
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot length: got %d, want 1", len(snap))
	}
}

// TestDomainThroughputTracker_ExpiresOldEntries proves the rolling window
// works: events older than windowSize are dropped on the next Snapshot.
// Uses a stubbed nowFn so we don't have to sleep 61 seconds in a unit test.
func TestDomainThroughputTracker_ExpiresOldEntries(t *testing.T) {
	tr := NewDomainThroughputTracker(60 * time.Second)
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	tr.nowFn = func() time.Time { return now }

	// 3 sends "now"
	for i := 0; i < 3; i++ {
		tr.RecordSend("em.discountblog.com")
	}

	// Advance 61s (past the window)
	now = now.Add(61 * time.Second)

	// 2 more sends after the advance
	for i := 0; i < 2; i++ {
		tr.RecordSend("em.discountblog.com")
	}

	snap := tr.Snapshot()
	if got, want := snap["em.discountblog.com"], 2; got != want {
		t.Fatalf("expected 2 sends visible after window expiry, got %d", got)
	}
}

// TestDomainThroughputTracker_ConcurrentSafe runs 100 goroutines × 100
// sends each. The mutex must prevent any lost increments — 10000 total.
// Race detector catches any unguarded map writes; the count assertion
// catches any silent corruption that escapes -race.
func TestDomainThroughputTracker_ConcurrentSafe(t *testing.T) {
	tr := NewDomainThroughputTracker(60 * time.Second)
	const goroutines = 100
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				tr.RecordSend("em.discountblog.com")
			}
		}()
	}
	wg.Wait()

	snap := tr.Snapshot()
	if got, want := snap["em.discountblog.com"], goroutines*perGoroutine; got != want {
		t.Fatalf("concurrent count: got %d, want %d", got, want)
	}
}

// TestDomainThroughputTracker_MultipleDomains: interleaved sends to three
// domains each get the right count, and FormatThroughputLog produces the
// stable alphabetical output expected by operators tailing logs.
func TestDomainThroughputTracker_MultipleDomains(t *testing.T) {
	tr := NewDomainThroughputTracker(60 * time.Second)
	pattern := []string{
		"em.discountblog.com",
		"em.historythinking.com",
		"em.myownhealth.net",
		"em.discountblog.com",
		"em.historythinking.com",
		"em.discountblog.com",
	}
	for _, d := range pattern {
		tr.RecordSend(d)
	}

	snap := tr.Snapshot()
	want := map[string]int{
		"em.discountblog.com":     3,
		"em.historythinking.com":  2,
		"em.myownhealth.net":      1,
	}
	for d, c := range want {
		if got := snap[d]; got != c {
			t.Errorf("domain %s: got %d, want %d", d, got, c)
		}
	}
	if len(snap) != len(want) {
		t.Errorf("snapshot length: got %d, want %d", len(snap), len(want))
	}

	// Format check — short tags, alphabetical order, total at the end.
	out := FormatThroughputLog(snap)
	wantSubstr := "discountblog=3 historythinking=2 myownhealth=1 (total=6)"
	if !strings.Contains(out, wantSubstr) {
		t.Errorf("FormatThroughputLog: missing expected fragment %q in %q", wantSubstr, out)
	}
}

// TestDomainThroughputTracker_EmptyDomainDropped guards the
// RecordSend(empty)-is-a-noop contract. Empty/whitespace-only input must
// not create a phantom map entry; otherwise FormatThroughputLog would emit
// "unknown=N" lines that look like a real domain to operators.
func TestDomainThroughputTracker_EmptyDomainDropped(t *testing.T) {
	tr := NewDomainThroughputTracker(60 * time.Second)
	tr.RecordSend("")
	tr.RecordSend("   ")
	tr.RecordSend("\t")
	snap := tr.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("empty/whitespace inputs should not create entries, got %v", snap)
	}
}

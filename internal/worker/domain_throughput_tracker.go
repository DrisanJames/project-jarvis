package worker

// DomainThroughputTracker — per-sending-domain rolling-window send counter
// (per-domain engagement engine, SA-7, 2026-05-09).
//
// Pure observability: SendWorkerPool calls RecordSend on every successful
// PMTA submission, and a periodic logger plus the /api/wave-processor/status
// HTTP handler call Snapshot to read the current 60-second window. No
// behavioral coupling — never read this on the hot path.
//
// Implementation: mutex-protected map[sendingDomain] -> []time.Time of recent
// send timestamps. Snapshot() prunes entries older than windowSize. The
// pruning model keeps memory bounded under realistic send rates (~100/s
// global × 60s = ~6k timestamps total at any one moment, distributed across
// ~16 sending domains) and avoids the bookkeeping cost of a fixed bucket
// ring.
//
// Concurrency: every method takes the mutex. The 25 send workers contend on
// it once per send (microseconds). The Snapshot caller (HTTP handler / log
// goroutine) contends once per minute. This is well below any contention
// threshold that would require sharding.
//
// Testability: nowFn is injectable so the time-window expiry test can
// fast-forward without sleeping.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DomainThroughputTracker counts sends per sending_domain over a rolling
// time window. Safe for concurrent use.
type DomainThroughputTracker struct {
	mu         sync.Mutex
	events     map[string][]time.Time
	windowSize time.Duration
	nowFn      func() time.Time
}

// NewDomainThroughputTracker constructs a tracker with the given rolling
// window. Production usage is 60 * time.Second.
func NewDomainThroughputTracker(windowSize time.Duration) *DomainThroughputTracker {
	return &DomainThroughputTracker{
		events:     make(map[string][]time.Time),
		windowSize: windowSize,
		nowFn:      time.Now,
	}
}

// RecordSend stamps a send for the given sending_domain. Empty/whitespace
// inputs are dropped silently — the send happened, but we have no domain to
// attribute it to. Inputs are lowercased and trimmed so callers don't have
// to normalize.
func (t *DomainThroughputTracker) RecordSend(sendingDomain string) {
	d := strings.TrimSpace(strings.ToLower(sendingDomain))
	if d == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events[d] = append(t.events[d], t.nowFn())
}

// Snapshot returns a per-domain count of sends in the last `windowSize`
// duration. Side effect: prunes expired entries (and removes empty domain
// entries) so the map doesn't grow unbounded across a long uptime.
//
// Returns a fresh map; callers are free to mutate it.
func (t *DomainThroughputTracker) Snapshot() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.nowFn().Add(-t.windowSize)
	out := make(map[string]int, len(t.events))
	for domain, ts := range t.events {
		// Drop the expired prefix. Timestamps within a single domain are
		// inserted monotonically (RecordSend always appends nowFn()), so
		// a linear scan from the front is sufficient.
		i := 0
		for i < len(ts) && ts[i].Before(cutoff) {
			i++
		}
		if i > 0 {
			t.events[domain] = ts[i:]
		}
		count := len(t.events[domain])
		if count > 0 {
			out[domain] = count
		} else {
			delete(t.events, domain)
		}
	}
	return out
}

// FormatThroughputLog renders a snapshot as a stable human-readable log
// line: "db=400 ht=350 mh=200 (total=950)". Domain order is alphabetical so
// log diffs across ticks are readable. Tags use shortDomainTag from
// pmta_wave_scheduler.go (e.g. "em.discountblog.com" -> "discountblog").
func FormatThroughputLog(snapshot map[string]int) string {
	domains := make([]string, 0, len(snapshot))
	total := 0
	for d, c := range snapshot {
		domains = append(domains, d)
		total += c
	}
	sort.Strings(domains)
	parts := make([]string, 0, len(domains))
	for _, d := range domains {
		parts = append(parts, fmt.Sprintf("%s=%d", shortDomainTag(d), snapshot[d]))
	}
	return fmt.Sprintf("%s (total=%d)", strings.Join(parts, " "), total)
}

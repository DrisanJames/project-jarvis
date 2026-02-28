package engine

import (
	"context"
	"log"
	"sync"
	"time"
)

// SignalProcessor computes per-ISP rolling-window metrics from ingested records.
// Each ISP has its own goroutine aggregating signals over 1min/5min/1hr/24hr windows.
type SignalProcessor struct {
	store    SignalStore
	orgID    string
	registry *ISPRegistry

	mu       sync.RWMutex
	windows  map[ISP]*ISPSignalWindow
	listeners []chan<- SignalSnapshot
}

// ISPSignalWindow holds the rolling-window metrics for one ISP.
type ISPSignalWindow struct {
	ISP ISP

	// Counters within sliding windows
	mu sync.Mutex

	// Per-IP metrics: map[ip]counters
	ipSent       map[string]*windowCounter
	ipBounced    map[string]*windowCounter
	ipComplaints map[string]*windowCounter
	ipDeferred   map[string]*windowCounter

	// Per-domain metrics
	domainSent    map[string]*windowCounter
	domainBounced map[string]*windowCounter
	domainDeferred map[string]*windowCounter

	// Global ISP totals
	totalSent      *windowCounter
	totalBounced   *windowCounter
	totalComplaints *windowCounter
	totalDeferred  *windowCounter

	// DSN code samples (capped ring buffer for recent observations)
	recentDSNCodes      []dsnSample
}

// windowCounter tracks events over multiple sliding windows.
type windowCounter struct {
	events []time.Time
}

type dsnSample struct {
	Code       string
	Diagnostic string
	At         time.Time
}

func newWindowCounter() *windowCounter {
	return &windowCounter{}
}

func (wc *windowCounter) add(t time.Time) {
	wc.events = append(wc.events, t)
}

func (wc *windowCounter) countSince(since time.Time) int {
	count := 0
	for _, t := range wc.events {
		if t.After(since) {
			count++
		}
	}
	return count
}

func (wc *windowCounter) prune(before time.Time) {
	pruned := wc.events[:0]
	for _, t := range wc.events {
		if t.After(before) {
			pruned = append(pruned, t)
		}
	}
	wc.events = pruned
}

// SignalSnapshot is emitted periodically with computed rates and raw counts.
type SignalSnapshot struct {
	ISP           ISP                `json:"isp"`
	Timestamp     time.Time          `json:"timestamp"`
	BounceRate1m  float64            `json:"bounce_rate_1m"`
	BounceRate5m  float64            `json:"bounce_rate_5m"`
	BounceRate1h  float64            `json:"bounce_rate_1h"`
	ComplaintRate1h float64          `json:"complaint_rate_1h"`
	DeferralRate5m float64           `json:"deferral_rate_5m"`
	DeferralRate1h float64           `json:"deferral_rate_1h"`
	IPMetrics     map[string]IPMetric `json:"ip_metrics"`

	// Raw counts for conviction micro-context
	Sent1h        int     `json:"sent_1h"`
	Sent5m        int     `json:"sent_5m"`
	Bounced1h     int     `json:"bounced_1h"`
	Deferred5m    int     `json:"deferred_5m"`
	Deferred1h    int     `json:"deferred_1h"`
	Complaints1h  int     `json:"complaints_1h"`
	Accepted1h    int     `json:"accepted_1h"`

	// DSN code samples observed in the current window (up to 10)
	RecentDSNCodes      []string `json:"recent_dsn_codes,omitempty"`
	RecentDSNDiagnostics []string `json:"recent_dsn_diagnostics,omitempty"`
}

// IPMetric holds per-IP metrics within a snapshot.
type IPMetric struct {
	IP            string  `json:"ip"`
	BounceRate1h  float64 `json:"bounce_rate_1h"`
	ComplaintRate float64 `json:"complaint_rate_24h"`
	DeferralRate  float64 `json:"deferral_rate_5m"`
	Sent1h        int     `json:"sent_1h"`
	Score         float64 `json:"score"`

	// Raw counts for micro-context
	Bounced1h    int `json:"bounced_1h"`
	Deferred5m   int `json:"deferred_5m"`
	Complaints24h int `json:"complaints_24h"`
	Accepted1h   int `json:"accepted_1h"`
}

// NewSignalProcessor creates a processor for all ISPs.
func NewSignalProcessor(store SignalStore, orgID string, registry *ISPRegistry) *SignalProcessor {
	sp := &SignalProcessor{
		store:    store,
		orgID:    orgID,
		registry: registry,
		windows:  make(map[ISP]*ISPSignalWindow),
	}
	for _, isp := range AllISPs() {
		sp.windows[isp] = newISPSignalWindow(isp)
	}
	return sp
}

func newISPSignalWindow(isp ISP) *ISPSignalWindow {
	return &ISPSignalWindow{
		ISP:             isp,
		ipSent:          make(map[string]*windowCounter),
		ipBounced:       make(map[string]*windowCounter),
		ipComplaints:    make(map[string]*windowCounter),
		ipDeferred:      make(map[string]*windowCounter),
		domainSent:      make(map[string]*windowCounter),
		domainBounced:   make(map[string]*windowCounter),
		domainDeferred:  make(map[string]*windowCounter),
		totalSent:       newWindowCounter(),
		totalBounced:    newWindowCounter(),
		totalComplaints: newWindowCounter(),
		totalDeferred:   newWindowCounter(),
	}
}

// Subscribe adds a listener for signal snapshots.
func (sp *SignalProcessor) Subscribe(ch chan<- SignalSnapshot) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.listeners = append(sp.listeners, ch)
}

// Ingest processes a classified accounting record.
func (sp *SignalProcessor) Ingest(isp ISP, rec AccountingRecord) {
	sp.mu.RLock()
	w, ok := sp.windows[isp]
	sp.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	ip := rec.SourceIP
	domain := rec.Domain

	w.mu.Lock()
	defer w.mu.Unlock()

	ensureCounter := func(m map[string]*windowCounter, key string) *windowCounter {
		if _, ok := m[key]; !ok {
			m[key] = newWindowCounter()
		}
		return m[key]
	}

	switch rec.Type {
	case "d": // delivery
		w.totalSent.add(now)
		if ip != "" {
			ensureCounter(w.ipSent, ip).add(now)
		}
		if domain != "" {
			ensureCounter(w.domainSent, domain).add(now)
		}

	case "b": // bounce
		w.totalBounced.add(now)
		w.totalSent.add(now)
		if ip != "" {
			ensureCounter(w.ipBounced, ip).add(now)
			ensureCounter(w.ipSent, ip).add(now)
		}
		if domain != "" {
			ensureCounter(w.domainBounced, domain).add(now)
			ensureCounter(w.domainSent, domain).add(now)
		}

	case "t", "tq": // transient/deferral
		w.totalDeferred.add(now)
		if ip != "" {
			ensureCounter(w.ipDeferred, ip).add(now)
		}
		if domain != "" {
			ensureCounter(w.domainDeferred, domain).add(now)
		}

	case "f": // FBL complaint
		w.totalComplaints.add(now)
		if ip != "" {
			ensureCounter(w.ipComplaints, ip).add(now)
		}
	}

	// Track DSN codes for micro-context
	if rec.DSNStatus != "" || rec.DSNDiag != "" {
		w.recentDSNCodes = append(w.recentDSNCodes, dsnSample{
			Code:       rec.DSNStatus,
			Diagnostic: rec.DSNDiag,
			At:         now,
		})
		if len(w.recentDSNCodes) > 50 {
			w.recentDSNCodes = w.recentDSNCodes[len(w.recentDSNCodes)-50:]
		}
	}
}

// Start begins the periodic snapshot computation and pruning loop.
func (sp *SignalProcessor) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		pruneTicker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		defer pruneTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sp.computeSnapshots()
			case <-pruneTicker.C:
				sp.pruneOldEvents()
			}
		}
	}()
}

func safeRate(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator) * 100
}

func (sp *SignalProcessor) computeSnapshots() {
	now := time.Now()
	sp.mu.RLock()
	defer sp.mu.RUnlock()

	for _, w := range sp.windows {
		w.mu.Lock()
		snap := SignalSnapshot{
			ISP:       w.ISP,
			Timestamp: now,
		}

		sent1m := w.totalSent.countSince(now.Add(-1 * time.Minute))
		sent5m := w.totalSent.countSince(now.Add(-5 * time.Minute))
		sent1h := w.totalSent.countSince(now.Add(-1 * time.Hour))

		bounced1m := w.totalBounced.countSince(now.Add(-1 * time.Minute))
		bounced5m := w.totalBounced.countSince(now.Add(-5 * time.Minute))
		bounced1h := w.totalBounced.countSince(now.Add(-1 * time.Hour))

		snap.BounceRate1m = safeRate(bounced1m, sent1m)
		snap.BounceRate5m = safeRate(bounced5m, sent5m)
		snap.BounceRate1h = safeRate(bounced1h, sent1h)

		complaints1h := w.totalComplaints.countSince(now.Add(-1 * time.Hour))
		snap.ComplaintRate1h = safeRate(complaints1h, sent1h)

		deferred5m := w.totalDeferred.countSince(now.Add(-5 * time.Minute))
		deferred1h := w.totalDeferred.countSince(now.Add(-1 * time.Hour))
		snap.DeferralRate5m = safeRate(deferred5m, sent5m)
		snap.DeferralRate1h = safeRate(deferred1h, sent1h)

		// Populate raw counts for conviction context
		snap.Sent1h = sent1h
		snap.Sent5m = sent5m
		snap.Bounced1h = bounced1h
		snap.Deferred5m = deferred5m
		snap.Deferred1h = deferred1h
		snap.Complaints1h = complaints1h
		snap.Accepted1h = sent1h - bounced1h

		// Collect recent DSN code samples (last 5 minutes)
		dsnCutoff := now.Add(-5 * time.Minute)
		seen := make(map[string]bool)
		for _, ds := range w.recentDSNCodes {
			if ds.At.After(dsnCutoff) {
				if ds.Code != "" && !seen[ds.Code] {
					snap.RecentDSNCodes = append(snap.RecentDSNCodes, ds.Code)
					seen[ds.Code] = true
				}
				if ds.Diagnostic != "" && len(snap.RecentDSNDiagnostics) < 10 {
					snap.RecentDSNDiagnostics = append(snap.RecentDSNDiagnostics, ds.Diagnostic)
				}
			}
		}

		snap.IPMetrics = make(map[string]IPMetric)
		for ip, sentCtr := range w.ipSent {
			ipSent1h := sentCtr.countSince(now.Add(-1 * time.Hour))
			ipBounced1h := 0
			if bc, ok := w.ipBounced[ip]; ok {
				ipBounced1h = bc.countSince(now.Add(-1 * time.Hour))
			}
			ipComplaints24h := 0
			if cc, ok := w.ipComplaints[ip]; ok {
				ipComplaints24h = cc.countSince(now.Add(-24 * time.Hour))
			}
			ipDeferred5m := 0
			if dc, ok := w.ipDeferred[ip]; ok {
				ipDeferred5m = dc.countSince(now.Add(-5 * time.Minute))
			}

			br := safeRate(ipBounced1h, ipSent1h)
			cr := safeRate(ipComplaints24h, ipSent1h)
			dr := safeRate(ipDeferred5m, ipSent1h)

			score := 100.0 - (br * 10) - (cr * 100) - (dr * 2)
			if score < 0 {
				score = 0
			}

			snap.IPMetrics[ip] = IPMetric{
				IP:            ip,
				BounceRate1h:  br,
				ComplaintRate: cr,
				DeferralRate:  dr,
				Sent1h:        ipSent1h,
				Score:         score,
				Bounced1h:     ipBounced1h,
				Deferred5m:    ipDeferred5m,
				Complaints24h: ipComplaints24h,
				Accepted1h:    ipSent1h - ipBounced1h,
			}
		}

		w.mu.Unlock()

		// Persist snapshot to DB
		sp.persistSnapshot(snap)

		// Notify listeners
		sp.mu.RLock()
		for _, ch := range sp.listeners {
			select {
			case ch <- snap:
			default: // non-blocking
			}
		}
		sp.mu.RUnlock()
	}
}

func (sp *SignalProcessor) persistSnapshot(snap SignalSnapshot) {
	if sp.store == nil {
		return
	}
	ctx := context.Background()

	metrics := []SignalMetric{
		{snap.ISP, "bounce_rate", "global", string(snap.ISP), snap.BounceRate1m, 60},
		{snap.ISP, "bounce_rate", "global", string(snap.ISP), snap.BounceRate5m, 300},
		{snap.ISP, "bounce_rate", "global", string(snap.ISP), snap.BounceRate1h, 3600},
		{snap.ISP, "complaint_rate", "global", string(snap.ISP), snap.ComplaintRate1h, 3600},
		{snap.ISP, "deferral_rate", "global", string(snap.ISP), snap.DeferralRate5m, 300},
		{snap.ISP, "deferral_rate", "global", string(snap.ISP), snap.DeferralRate1h, 3600},
	}

	if err := sp.store.PersistSignals(ctx, sp.orgID, snap, metrics); err != nil {
		log.Printf("[signals] persist error isp=%s: %v", snap.ISP, err)
	}
}

func (sp *SignalProcessor) pruneOldEvents() {
	cutoff := time.Now().Add(-25 * time.Hour)
	dsnCutoff := time.Now().Add(-10 * time.Minute)
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	for _, w := range sp.windows {
		w.mu.Lock()
		w.totalSent.prune(cutoff)
		w.totalBounced.prune(cutoff)
		w.totalComplaints.prune(cutoff)
		w.totalDeferred.prune(cutoff)
		for _, c := range w.ipSent { c.prune(cutoff) }
		for _, c := range w.ipBounced { c.prune(cutoff) }
		for _, c := range w.ipComplaints { c.prune(cutoff) }
		for _, c := range w.ipDeferred { c.prune(cutoff) }
		for _, c := range w.domainSent { c.prune(cutoff) }
		for _, c := range w.domainBounced { c.prune(cutoff) }
		for _, c := range w.domainDeferred { c.prune(cutoff) }

		pruned := w.recentDSNCodes[:0]
		for _, ds := range w.recentDSNCodes {
			if ds.At.After(dsnCutoff) {
				pruned = append(pruned, ds)
			}
		}
		w.recentDSNCodes = pruned

		w.mu.Unlock()
	}
}

// GetSnapshot returns the current signal snapshot for an ISP.
func (sp *SignalProcessor) GetSnapshot(isp ISP) SignalSnapshot {
	now := time.Now()
	sp.mu.RLock()
	w, ok := sp.windows[isp]
	sp.mu.RUnlock()
	if !ok {
		return SignalSnapshot{ISP: isp, Timestamp: now}
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	sent1h := w.totalSent.countSince(now.Add(-1 * time.Hour))
	sent5m := w.totalSent.countSince(now.Add(-5 * time.Minute))
	sent1m := w.totalSent.countSince(now.Add(-1 * time.Minute))

	return SignalSnapshot{
		ISP:             isp,
		Timestamp:       now,
		BounceRate1m:    safeRate(w.totalBounced.countSince(now.Add(-1*time.Minute)), sent1m),
		BounceRate5m:    safeRate(w.totalBounced.countSince(now.Add(-5*time.Minute)), sent5m),
		BounceRate1h:    safeRate(w.totalBounced.countSince(now.Add(-1*time.Hour)), sent1h),
		ComplaintRate1h: safeRate(w.totalComplaints.countSince(now.Add(-1*time.Hour)), sent1h),
		DeferralRate5m:  safeRate(w.totalDeferred.countSince(now.Add(-5*time.Minute)), sent5m),
		DeferralRate1h:  safeRate(w.totalDeferred.countSince(now.Add(-1*time.Hour)), sent1h),
	}
}

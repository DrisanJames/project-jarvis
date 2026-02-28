package worker

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"
)

// BackpressureMonitor checks queue depth and signals when to pause enqueueing.
// If ESPs are down, the campaign send queue can grow unbounded in PostgreSQL.
// This monitor pauses enqueueing when the queue exceeds a configurable threshold
// and resumes when it drains to 50% (hysteresis to avoid flapping).
type BackpressureMonitor struct {
	db            *sql.DB
	maxQueueDepth int64 // default 100,000
	checkInterval time.Duration
	paused        bool
	hasV2Table    bool   // whether mailing_campaign_queue_v2 exists
	v2Checked     bool   // whether we've probed for v2 yet
	mu            sync.RWMutex
}

// NewBackpressureMonitor creates a new BackpressureMonitor.
// maxDepth is the queue depth at which enqueueing is paused.
// If maxDepth <= 0 it defaults to 100,000.
func NewBackpressureMonitor(db *sql.DB, maxDepth int64) *BackpressureMonitor {
	if maxDepth <= 0 {
		maxDepth = 100000
	}
	return &BackpressureMonitor{
		db:            db,
		maxQueueDepth: maxDepth,
		checkInterval: 30 * time.Second,
	}
}

// Start runs the periodic queue-depth check loop. It blocks until ctx is cancelled.
func (bp *BackpressureMonitor) Start(ctx context.Context) {
	// Probe for v2 table on first run
	bp.probeV2Table(ctx)

	// Run an initial check immediately
	bp.check(ctx)

	ticker := time.NewTicker(bp.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bp.check(ctx)
		}
	}
}

// probeV2Table checks whether mailing_campaign_queue_v2 exists.
// The result is cached so we only query information_schema once.
func (bp *BackpressureMonitor) probeV2Table(ctx context.Context) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.v2Checked {
		return
	}

	var exists bool
	err := bp.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'mailing_campaign_queue_v2'
		)
	`).Scan(&exists)

	if err != nil {
		log.Printf("[Backpressure] Warning: could not probe for v2 table: %v", err)
		// Assume v2 does not exist; we'll only query v1.
		bp.hasV2Table = false
	} else {
		bp.hasV2Table = exists
	}
	bp.v2Checked = true

	if bp.hasV2Table {
		log.Println("[Backpressure] Monitoring both mailing_campaign_queue and mailing_campaign_queue_v2")
	} else {
		log.Println("[Backpressure] Monitoring mailing_campaign_queue (v2 table not found)")
	}
}

// check queries the current queue depth and updates the paused flag.
func (bp *BackpressureMonitor) check(ctx context.Context) {
	depth, err := bp.queryDepth(ctx)
	if err != nil {
		log.Printf("[Backpressure] Check error: %v", err)
		return
	}

	bp.mu.Lock()
	defer bp.mu.Unlock()

	wasPaused := bp.paused
	// Pause at maxQueueDepth, resume at 50% (hysteresis prevents flapping)
	if depth >= bp.maxQueueDepth {
		bp.paused = true
		if !wasPaused {
			log.Printf("BACKPRESSURE: Queue depth %d exceeds threshold %d — pausing enqueue", depth, bp.maxQueueDepth)
		}
	} else if depth < bp.maxQueueDepth/2 {
		bp.paused = false
		if wasPaused {
			log.Printf("BACKPRESSURE: Queue depth %d below resume threshold %d — resuming enqueue", depth, bp.maxQueueDepth/2)
		}
	}
	// Between 50% and 100% we keep whatever state we're in (hysteresis band).
}

// queryDepth returns the total number of queued + claimed items across queue tables.
func (bp *BackpressureMonitor) queryDepth(ctx context.Context) (int64, error) {
	bp.mu.RLock()
	hasV2 := bp.hasV2Table
	bp.mu.RUnlock()

	var depth int64

	if hasV2 {
		err := bp.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(cnt), 0) FROM (
				SELECT COUNT(*) AS cnt FROM mailing_campaign_queue WHERE status IN ('queued', 'claimed')
				UNION ALL
				SELECT COUNT(*) AS cnt FROM mailing_campaign_queue_v2 WHERE status IN ('queued', 'claimed')
			) t
		`).Scan(&depth)
		if err != nil {
			// If v2 disappeared (e.g. migration rollback), fall back to v1 only
			log.Printf("[Backpressure] Union query failed, falling back to v1 only: %v", err)
			bp.mu.Lock()
			bp.hasV2Table = false
			bp.mu.Unlock()
			return bp.queryV1Depth(ctx)
		}
		return depth, nil
	}

	return bp.queryV1Depth(ctx)
}

// queryV1Depth returns queue depth from v1 table only.
func (bp *BackpressureMonitor) queryV1Depth(ctx context.Context) (int64, error) {
	var depth int64
	err := bp.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_campaign_queue WHERE status IN ('queued', 'claimed')
	`).Scan(&depth)
	return depth, err
}

// IsPaused returns true if enqueue operations should be paused due to backpressure.
func (bp *BackpressureMonitor) IsPaused() bool {
	bp.mu.RLock()
	defer bp.mu.RUnlock()
	return bp.paused
}

// QueueDepth returns the current queue depth (useful for health checks / metrics).
func (bp *BackpressureMonitor) QueueDepth() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	depth, err := bp.queryDepth(ctx)
	if err != nil {
		return -1
	}
	return depth
}

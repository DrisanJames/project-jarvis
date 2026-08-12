package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ses_events_queue.go decouples SES event INGEST from SES event PERSISTENCE.
//
// WHY THIS EXISTS (2026-08-11 incident analysis)
// ----------------------------------------------
// SES publishes every Send/Delivery/Open/Click/Bounce/Complaint through an SNS
// HTTPS subscription to /api/mailing/webhooks/ses-events. That subscription's
// effective delivery policy is:
//
//	healthyRetryPolicy: numRetries=3, linear, 20s   →  ~60s of retry budget
//	guaranteed: false                               →  best-effort
//	RedrivePolicy: ABSENT                           →  NO dead-letter queue
//
// So a notification SNS cannot deliver within its timeout, three times over
// about a minute, is destroyed. There is no backfill path — SES does not
// re-emit events, and the lake row is written downstream of the PG insert, so
// a lost event is lost from BOTH surfaces at once.
//
// The old handler did all of its DB work synchronously before responding,
// which put the server's DB latency directly inside SNS's delivery decision.
// Measured over 24h on 2026-08-11:
//
//	NumberOfNotificationsDelivered  2,246,024
//	NumberOfNotificationsFailed        13,078   (0.58% overall)
//	worst hour (08-10 19:00 MDT)       10,660   of 62,211  = 14.6% LOST
//
// and that worst hour is exactly the hour ALB TargetResponseTime spiked to a
// 110s maximum against a 0.12–0.47s baseline. Separately, the handler swallowed
// its own persist errors and still answered 200, so DB failures that did NOT
// time out were also lost silently and SNS never retried them.
//
// THE FIX: accept-and-buffer. ServeHTTP verifies the SNS signature, decodes the
// payload, drops it in this bounded queue and returns 200 in ~1ms. A bounded
// worker pool drains the queue against a context that is NOT tied to the
// request, with retries on transient DB failures. The server's DB latency no
// longer participates in whether SNS considers the event delivered.
//
// WHAT THIS DOES NOT FIX: an in-memory buffer does not survive task death, and
// backpressure past the buffer still 503s. The durable answer is an SQS queue
// (or at minimum a RedrivePolicy DLQ on the existing subscription) — that is an
// infrastructure change and is tracked separately for the operator.

// Package-level counters. Kept as package vars (not handler fields) so the
// /health surface can read them without a handle on the handler, matching the
// eventbus_health.go pattern.
var (
	sesQueueEnqueued     uint64 // accepted into the buffer
	sesQueueRejected     uint64 // buffer full → 503 → SNS retries
	sesQueueProcessed    uint64 // drained and persisted successfully
	sesQueueRetried      uint64 // individual retry attempts made by a worker
	sesQueueFailed       uint64 // gave up after all retries — PERMANENT LOSS
	sesQueueDepth        int64  // current occupancy
	sesRetriesPending    int64  // items waiting out a backoff (not holding a worker)
	sesRollupFailed      uint64 // ses_delivered rollup upsert failed (event row is fine)
	sesEngagementFailed  uint64 // open/click side-effect write failed
	sesSyncPersistFailed uint64 // sync-fallback dispatch failure (SES_WEBHOOK_ASYNC=false)
)

// sesQueueItem is one decoded SES notification awaiting persistence. Storing
// the DECODED form (rather than the raw SNS body) keeps the buffer small — the
// raw JSON is 1–2KB per event, the decoded struct a few hundred bytes.
type sesQueueItem struct {
	env  snsEnvelope
	note sesEventNotification
	// attempt is how many times this item has already been tried. Retries are
	// re-queued rather than retried in place, so the count has to ride along.
	attempt int
}

// sesIngestQueue is a bounded buffer plus a fixed worker pool.
type sesIngestQueue struct {
	h       *SESEventsHandler
	ch      chan sesQueueItem
	stop    chan struct{}
	done    chan struct{}
	workers int
	wg      sync.WaitGroup
	// stopOnce keeps shutdown idempotent. Closing `stop` twice panics, which
	// during a graceful ECS shutdown would turn an orderly drain into a crash.
	stopOnce sync.Once
}

// envInt reads a positive integer env override, falling back to def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// sesPersistTimeout is the per-event DB budget. On the async path this no
// longer competes with an SNS delivery deadline, so it can be generous enough
// that a busy-but-healthy database still completes rather than being cancelled
// mid-write (the old 5s budget was shared across the INSERT *and* every
// engagement side-effect, so a slow INSERT silently starved the rest).
func sesPersistTimeout() time.Duration {
	return time.Duration(envInt("SES_WEBHOOK_PERSIST_TIMEOUT_SEC", 20)) * time.Second
}

// sesAsyncEnabled is the kill switch. Async is ON by default — the synchronous
// path is demonstrably lossy. SES_WEBHOOK_ASYNC=false restores the exact
// previous behavior without a code change (one-move rollback).
func sesAsyncEnabled() bool {
	v := os.Getenv("SES_WEBHOOK_ASYNC")
	return !(v == "false" || v == "0" || v == "off")
}

// startSESIngestQueue builds and starts the queue. Returns nil when the kill
// switch is off, which makes ServeHTTP fall back to synchronous processing.
func startSESIngestQueue(h *SESEventsHandler) *sesIngestQueue {
	if !sesAsyncEnabled() {
		log.Printf("[ses-events] ASYNC INGEST DISABLED (SES_WEBHOOK_ASYNC=false) — synchronous path, events may be dropped under load")
		return nil
	}
	depth := envInt("SES_WEBHOOK_QUEUE_DEPTH", 20000)
	workers := envInt("SES_WEBHOOK_WORKERS", 12)

	q := &sesIngestQueue{
		h:       h,
		ch:      make(chan sesQueueItem, depth),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		workers: workers,
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.worker(i)
	}
	go func() {
		q.wg.Wait()
		close(q.done)
	}()
	log.Printf("[ses-events] async ingest ENABLED depth=%d workers=%d persist_timeout=%s",
		depth, workers, sesPersistTimeout())
	return q
}

// enqueue offers an item to the buffer. Returns false when the buffer is full
// so the caller can 503 and let SNS retry rather than accepting-then-dropping.
func (q *sesIngestQueue) enqueue(env snsEnvelope, note sesEventNotification) bool {
	// Drop the raw inner payload: it is already decoded into `note`, and at
	// 1–2KB per event it would dominate the buffer's footprint (20k events ≈
	// 40MB of dead string). Only MessageId is read downstream, for logging.
	env.Message = ""
	env.Signature = ""
	select {
	case q.ch <- sesQueueItem{env: env, note: note}:
		atomic.AddUint64(&sesQueueEnqueued, 1)
		atomic.AddInt64(&sesQueueDepth, 1)
		return true
	default:
		return false
	}
}

// worker drains the buffer. Each item gets its own background context — NOT the
// request context, which is already cancelled by the time we run.
func (q *sesIngestQueue) worker(id int) {
	defer q.wg.Done()
	for {
		select {
		case item := <-q.ch:
			atomic.AddInt64(&sesQueueDepth, -1)
			q.process(item)
		case <-q.stop:
			// Drain whatever is still buffered before exiting so an ECS task
			// rotation does not discard in-flight events.
			for {
				select {
				case item := <-q.ch:
					atomic.AddInt64(&sesQueueDepth, -1)
					q.process(item)
				default:
					return
				}
			}
		}
	}
}

// maxSESAttempts bounds retries of one notification.
//
// This was 3 attempts with linear backoff — about 6 seconds of coverage. That
// is enough for a brief hiccup but NOT for a database brownout, and on
// 2026-08-12 a 4-minute estate-wide brownout (968 statement-timeout/deadlock
// errors across 12+ subsystems: ingest-db, AcctSummary, PMTAWaveScheduler,
// wave_processor…) exhausted it and destroyed two OPEN events outright:
//
//	[ses-events] PERMANENT LOSS after 3 attempts ... persist opened: pq: canceling statement
//
// Exponential backoff to ~2 minutes rides out a brownout of that shape. The
// extra wait costs nothing in the common case (attempt 1 succeeds) and does not
// hammer a sick database, because each worker is SLEEPING, not querying.
//
// If a brownout outlasts even this, the design degrades safely rather than
// losing data: workers stay busy, the queue fills, ServeHTTP starts returning
// 503, SNS retries, and anything it cannot deliver lands in the dead-letter
// queue for replay by cmd/ses-dlq-replay. Permanent loss is the last resort.
func maxSESAttempts() int { return envInt("SES_WEBHOOK_MAX_ATTEMPTS", 6) }

// sesRetryBackoff is exponential and capped, so 6 attempts span roughly
// 2+4+8+16+30 = 60s of waiting plus the per-attempt DB budget.
func sesRetryBackoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// process makes ONE attempt and, on failure, schedules a re-queue instead of
// sleeping.
//
// Retrying in place was a capacity bug: a worker that slept through its backoff
// was unavailable to drain anything else, so 12 workers x up to 30s of backoff
// collapsed throughput exactly when the queue was filling. Observed during a DB
// brownout — `enqueued` raced from 2,323 to 3,577 while `processed` crawled
// from 1,636 to 1,674.
//
// Now a failed attempt returns the worker to the pool immediately and a timer
// puts the item back on the queue when its backoff expires. The retry BUDGET is
// unchanged; only the worker occupancy is.
func (q *sesIngestQueue) process(item sesQueueItem) {
	attempt := item.attempt + 1
	maxAttempts := maxSESAttempts()

	ctx, cancel := context.WithTimeout(context.Background(), sesPersistTimeout()+5*time.Second)
	err := q.h.dispatchNotification(ctx, item.env, item.note)
	cancel()
	if err == nil {
		atomic.AddUint64(&sesQueueProcessed, 1)
		return
	}

	if attempt >= maxAttempts {
		// Exhausted. This is the one place an event is knowingly lost — it is
		// counted and logged loudly rather than disappearing.
		atomic.AddUint64(&sesQueueFailed, 1)
		log.Printf("[ses-events] PERMANENT LOSS after %d attempts MessageId=%s: %v",
			maxAttempts, item.env.MessageId, err)
		return
	}

	// The retry is idempotent: the tracking-event id is a deterministic SHA1 of
	// (campaign, send id, type, timestamp) and the INSERT is ON CONFLICT DO
	// NOTHING, so a retry after a partial success is a no-op, not a double count.
	atomic.AddUint64(&sesQueueRetried, 1)
	item.attempt = attempt
	q.scheduleRetry(item, sesRetryBackoff(attempt))
}

// scheduleRetry re-queues an item after a delay WITHOUT holding a worker.
func (q *sesIngestQueue) scheduleRetry(item sesQueueItem, delay time.Duration) {
	atomic.AddInt64(&sesRetriesPending, 1)
	t := time.AfterFunc(delay, func() {
		defer atomic.AddInt64(&sesRetriesPending, -1)
		select {
		case <-q.stop:
			// Shutting down: make one last synchronous attempt so a graceful
			// drain does not silently abandon the event.
			ctx, cancel := context.WithTimeout(context.Background(), sesPersistTimeout())
			if err := q.h.dispatchNotification(ctx, item.env, item.note); err != nil {
				atomic.AddUint64(&sesQueueFailed, 1)
				log.Printf("[ses-events] PERMANENT LOSS during shutdown MessageId=%s: %v",
					item.env.MessageId, err)
			} else {
				atomic.AddUint64(&sesQueueProcessed, 1)
			}
			cancel()
			return
		default:
		}
		select {
		case q.ch <- item:
			atomic.AddInt64(&sesQueueDepth, 1)
		default:
			// Buffer full. Losing the retry is bad, but blocking a timer
			// goroutine forever is worse; the DLQ covers the overflow path.
			atomic.AddUint64(&sesQueueFailed, 1)
			log.Printf("[ses-events] PERMANENT LOSS — retry could not re-queue (buffer full) MessageId=%s",
				item.env.MessageId)
		}
	})
	_ = t
}

// shutdown signals the workers and waits, bounded, for the buffer to drain.
func (q *sesIngestQueue) shutdown(wait time.Duration) {
	q.stopOnce.Do(func() { close(q.stop) })
	select {
	case <-q.done:
	case <-time.After(wait):
		log.Printf("[ses-events] shutdown drain timed out with %d events still buffered",
			atomic.LoadInt64(&sesQueueDepth))
	}
}

// ---------------------------------------------------------------------------
// /health surface
// ---------------------------------------------------------------------------

// globalSESQueue lets the health handler and the process shutdown hook reach
// the queue without threading a handle through the server struct.
var (
	sesQueueMu sync.RWMutex
	globalSESQueue *sesIngestQueue
)

func registerSESQueue(q *sesIngestQueue) {
	sesQueueMu.Lock()
	globalSESQueue = q
	sesQueueMu.Unlock()
}

// ---------------------------------------------------------------------------
// boot-time route readiness
// ---------------------------------------------------------------------------

var (
	sesHandlerMu    sync.RWMutex
	globalSESHandler *SESEventsHandler
	sesNotReadyHits uint64
)

// registerSESHandler publishes the live handler to the boot-time route.
func registerSESHandler(h *SESEventsHandler) {
	sesHandlerMu.Lock()
	globalSESHandler = h
	sesHandlerMu.Unlock()
	log.Printf("[ses-events] webhook handler wired — endpoint is now serving")
}

// serveSESEventsWhenReady is registered on the public router at construction so
// the path ALWAYS exists. Before the mailing DB is wired it answers 503, which
// SNS treats as retryable — rather than 401, which is what the path used to
// return by falling through to the auth-protected router during every task
// start (953 SNS delivery failures in one such window, 2026-08-12).
func serveSESEventsWhenReady(w http.ResponseWriter, r *http.Request) {
	sesHandlerMu.RLock()
	h := globalSESHandler
	sesHandlerMu.RUnlock()
	if h == nil {
		n := atomic.AddUint64(&sesNotReadyHits, 1)
		if n == 1 || n%200 == 0 {
			log.Printf("[ses-events] endpoint not ready yet (mailing DB still wiring) — 503, SNS will retry (hit #%d)", n)
		}
		w.Header().Set("Retry-After", "20")
		http.Error(w, "ses webhook not ready", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

// ShutdownSESIngest drains buffered SES events. Safe no-op when async ingest is
// disabled or was never started. Called from the server's graceful-shutdown path.
func ShutdownSESIngest() {
	sesQueueMu.RLock()
	q := globalSESQueue
	sesQueueMu.RUnlock()
	if q == nil {
		return
	}
	log.Printf("[ses-events] draining ingest queue (%d buffered)...", atomic.LoadInt64(&sesQueueDepth))
	q.shutdown(15 * time.Second)
}

// SESWebhookStatus is the /health "ses_webhook" block. Any non-zero `failed`,
// `rejected` or `engagement_failed` means telemetry was lost or degraded and
// the numbers in the portal for that window are under-reported.
type SESWebhookStatus struct {
	AsyncEnabled      bool   `json:"async_enabled"`
	Workers           int    `json:"workers"`
	QueueDepth        int64  `json:"queue_depth"`
	RetriesPending    int64  `json:"retries_pending"`
	Enqueued          uint64 `json:"enqueued"`
	Processed         uint64 `json:"processed"`
	Retried           uint64 `json:"retried"`
	Rejected          uint64 `json:"rejected_queue_full"`
	Failed            uint64 `json:"failed_permanent"`
	RollupFailed      uint64 `json:"rollup_failed"`
	EngagementFailed  uint64 `json:"engagement_failed"`
	SyncPersistFailed uint64 `json:"sync_persist_failed"`
}

// CurrentSESWebhookStatus is a cheap, read-only snapshot. No I/O.
func CurrentSESWebhookStatus() SESWebhookStatus {
	sesQueueMu.RLock()
	q := globalSESQueue
	sesQueueMu.RUnlock()

	st := SESWebhookStatus{
		Enqueued:          atomic.LoadUint64(&sesQueueEnqueued),
		Processed:         atomic.LoadUint64(&sesQueueProcessed),
		Retried:           atomic.LoadUint64(&sesQueueRetried),
		Rejected:          atomic.LoadUint64(&sesQueueRejected),
		Failed:            atomic.LoadUint64(&sesQueueFailed),
		RollupFailed:      atomic.LoadUint64(&sesRollupFailed),
		EngagementFailed:  atomic.LoadUint64(&sesEngagementFailed),
		SyncPersistFailed: atomic.LoadUint64(&sesSyncPersistFailed),
		QueueDepth:        atomic.LoadInt64(&sesQueueDepth),
		RetriesPending:    atomic.LoadInt64(&sesRetriesPending),
	}
	if q != nil {
		st.AsyncEnabled = true
		st.Workers = q.workers
	}
	return st
}

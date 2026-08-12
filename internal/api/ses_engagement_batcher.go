package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ses_engagement_batcher.go removes the hot-row bottleneck from SES event ingest.
//
// THE PROBLEM (measured in production 2026-08-11)
// ----------------------------------------------
// Every SES event used to do a synchronous read-modify-write against a SINGLE
// shared row:
//
//	UPDATE mailing_campaigns SET open_count = open_count + 1 WHERE id = $1
//
// At ~90k events/hour across 24 ingest workers, every event for the same
// campaign serialized on that one row's lock. Sampling pg_stat_activity while
// engagement UPDATEs were running:
//
//	Lock/transactionid            45   blocked on another txn's row lock
//	Lock/tuple                    19   blocked on tuple lock
//	LWLock/MultiXact* (5 kinds)   18   many txns contending for ONE tuple
//	LWLock/BufferContent          14   same buffer page
//
// A third of the time these updates were simply waiting. Because all of an
// event's side-effects shared ONE context budget sequentially, the waits
// consumed it and the LAST writes in the chain (subscriber denorm, inbox
// profile) failed — ~8% of them, invisibly, because their errors were discarded.
//
// Making the individual queries faster cannot fix this: it is a SERIALIZATION
// bottleneck, not a slow-query problem. N concurrent writers to one row take N
// turns no matter how fast each turn is.
//
// THE FIX
// -------
// Accumulate counter deltas in memory and flush them periodically as ONE
// statement covering ALL campaigns (and one for all subscribers). A flush that
// folds 5,000 events into 40 campaign rows takes 40 row locks instead of 5,000,
// each held for microseconds. Contention collapses by the fold factor.
//
// Counters become eventually-consistent within the flush interval. That is
// sound: mailing_tracking_events is the authoritative record (written first,
// synchronously, with retries), and these columns are denormalized reporting
// rollups derived from it. The metric contract already treats the event table
// as truth.
//
// Durability: deltas are drained on graceful shutdown. A hard task kill can
// lose at most one flush interval of counter increments — recoverable by
// recounting from mailing_tracking_events, which is why the event row is
// written first and never batched.

// campaignDelta accumulates one campaign's pending counter increments.
type campaignDelta struct {
	opens, uniqueOpens     int64
	clicks, uniqueClicks   int64
	delivered              int64
	bounces, hard, soft    int64
	complaints             int64
}

func (d campaignDelta) isZero() bool {
	return d.opens == 0 && d.uniqueOpens == 0 && d.clicks == 0 && d.uniqueClicks == 0 &&
		d.delivered == 0 && d.bounces == 0 && d.hard == 0 && d.soft == 0 && d.complaints == 0
}

// subscriberDelta accumulates one subscriber's pending denorm updates.
type subscriberDelta struct {
	opens, clicks int64
	lastOpen      time.Time
	lastClick     time.Time
}

var (
	sesFlushes         uint64 // flush cycles completed
	sesFlushCampaigns  uint64 // campaign rows written
	sesFlushSubscribers uint64 // subscriber rows written
	sesFlushFailed     uint64 // flush statements that errored (deltas retained)
	sesFoldedEvents    uint64 // event-level increments folded into batches
)

// sesEngagementBatcher folds per-event counter increments into periodic batched
// UPDATEs. All methods are safe for concurrent use by the ingest workers.
type sesEngagementBatcher struct {
	db *sql.DB

	mu          sync.Mutex
	campaigns   map[uuid.UUID]*campaignDelta
	subscribers map[uuid.UUID]*subscriberDelta

	interval time.Duration
	maxRows  int // force a flush when either map exceeds this

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newSESEngagementBatcher(db *sql.DB) *sesEngagementBatcher {
	b := &sesEngagementBatcher{
		db:          db,
		campaigns:   make(map[uuid.UUID]*campaignDelta),
		subscribers: make(map[uuid.UUID]*subscriberDelta),
		interval:    time.Duration(envInt("SES_ENGAGEMENT_FLUSH_SEC", 3)) * time.Second,
		maxRows:     envInt("SES_ENGAGEMENT_MAX_ROWS", 5000),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go b.run()
	log.Printf("[ses-engagement] batcher started interval=%s max_rows=%d", b.interval, b.maxRows)
	return b
}

func (b *sesEngagementBatcher) run() {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.flush()
		case <-b.stop:
			b.flush() // final drain
			return
		}
	}
}

// addCampaign folds one event's campaign-counter delta into the pending batch.
func (b *sesEngagementBatcher) addCampaign(id uuid.UUID, apply func(*campaignDelta)) {
	if id == uuid.Nil {
		return
	}
	b.mu.Lock()
	d := b.campaigns[id]
	if d == nil {
		d = &campaignDelta{}
		b.campaigns[id] = d
	}
	apply(d)
	over := len(b.campaigns) > b.maxRows
	b.mu.Unlock()
	atomic.AddUint64(&sesFoldedEvents, 1)
	if over {
		b.flush()
	}
}

// addSubscriber folds one event's subscriber denorm delta into the pending batch.
func (b *sesEngagementBatcher) addSubscriber(id uuid.UUID, opens, clicks int64, ts time.Time) {
	if id == uuid.Nil {
		return
	}
	b.mu.Lock()
	d := b.subscribers[id]
	if d == nil {
		d = &subscriberDelta{}
		b.subscribers[id] = d
	}
	d.opens += opens
	d.clicks += clicks
	if opens > 0 && ts.After(d.lastOpen) {
		d.lastOpen = ts
	}
	if clicks > 0 && ts.After(d.lastClick) {
		d.lastClick = ts
	}
	over := len(b.subscribers) > b.maxRows
	b.mu.Unlock()
	if over {
		b.flush()
	}
}

// flush swaps out the pending maps and writes them. On failure the deltas are
// merged BACK so a transient DB error delays the rollup rather than losing it.
func (b *sesEngagementBatcher) flush() {
	b.mu.Lock()
	camps := b.campaigns
	subs := b.subscribers
	b.campaigns = make(map[uuid.UUID]*campaignDelta)
	b.subscribers = make(map[uuid.UUID]*subscriberDelta)
	b.mu.Unlock()

	if len(camps) == 0 && len(subs) == 0 {
		return
	}
	// Generous budget: this runs on its own goroutine, off the ingest path, and
	// is not racing any SNS deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if len(camps) > 0 {
		if err := b.flushCampaigns(ctx, camps); err != nil {
			atomic.AddUint64(&sesFlushFailed, 1)
			log.Printf("[ses-engagement] campaign flush failed (%d rows, deltas retained): %v", len(camps), err)
			b.mergeBackCampaigns(camps)
		} else {
			atomic.AddUint64(&sesFlushCampaigns, uint64(len(camps)))
		}
	}
	if len(subs) > 0 {
		if err := b.flushSubscribers(ctx, subs); err != nil {
			atomic.AddUint64(&sesFlushFailed, 1)
			log.Printf("[ses-engagement] subscriber flush failed (%d rows, deltas retained): %v", len(subs), err)
			b.mergeBackSubscribers(subs)
		} else {
			atomic.AddUint64(&sesFlushSubscribers, uint64(len(subs)))
		}
	}
	atomic.AddUint64(&sesFlushes, 1)
}

func (b *sesEngagementBatcher) mergeBackCampaigns(camps map[uuid.UUID]*campaignDelta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Bound the retry set: if we are this far behind, dropping the oldest
	// rollup is better than growing memory without limit. The event rows are
	// still on disk, so the counters remain rebuildable.
	if len(b.campaigns) > b.maxRows {
		return
	}
	for id, d := range camps {
		cur := b.campaigns[id]
		if cur == nil {
			b.campaigns[id] = d
			continue
		}
		cur.opens += d.opens
		cur.uniqueOpens += d.uniqueOpens
		cur.clicks += d.clicks
		cur.uniqueClicks += d.uniqueClicks
		cur.delivered += d.delivered
		cur.bounces += d.bounces
		cur.hard += d.hard
		cur.soft += d.soft
		cur.complaints += d.complaints
	}
}

func (b *sesEngagementBatcher) mergeBackSubscribers(subs map[uuid.UUID]*subscriberDelta) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) > b.maxRows {
		return
	}
	for id, d := range subs {
		cur := b.subscribers[id]
		if cur == nil {
			b.subscribers[id] = d
			continue
		}
		cur.opens += d.opens
		cur.clicks += d.clicks
		if d.lastOpen.After(cur.lastOpen) {
			cur.lastOpen = d.lastOpen
		}
		if d.lastClick.After(cur.lastClick) {
			cur.lastClick = d.lastClick
		}
	}
}

// flushCampaigns writes every pending campaign delta in ONE statement.
// This is the change that removes the hot-row contention: N events against one
// campaign become a single +N increment holding the row lock once.
func (b *sesEngagementBatcher) flushCampaigns(ctx context.Context, camps map[uuid.UUID]*campaignDelta) error {
	// Postgres accepts at most 65535 bind parameters per statement. At 10
	// params per campaign that is ~6.5k rows, and the default max_rows is 5000 —
	// close enough that a single config bump would start failing entire
	// flushes. Chunk so the statement size can never depend on tuning.
	const chunkRows = 500

	// ORDER MATTERS. A multi-row UPDATE takes row locks in the order the rows
	// are presented, and Go randomizes map iteration — so two tasks flushing
	// overlapping campaigns would grab the same locks in opposite orders and
	// deadlock. Observed in production immediately after this shipped:
	//   [ses-engagement] campaign flush failed (37 rows): pq: deadlock detected
	// Sorting the ids gives every writer one global lock order, which makes a
	// deadlock between batchers impossible rather than merely unlikely.
	ids := sortedCampaignIDs(camps)

	for start := 0; start < len(ids); start += chunkRows {
		end := start + chunkRows
		if end > len(ids) {
			end = len(ids)
		}
		if err := b.flushCampaignChunk(ctx, ids[start:end], camps); err != nil {
			return err
		}
	}
	return nil
}

// sortedCampaignIDs returns the map's keys in a stable, global order.
func sortedCampaignIDs(m map[uuid.UUID]*campaignDelta) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	return ids
}

// sortedSubscriberIDs returns the map's keys in a stable, global order.
func sortedSubscriberIDs(m map[uuid.UUID]*subscriberDelta) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	return ids
}

func (b *sesEngagementBatcher) flushCampaignChunk(ctx context.Context, ids []uuid.UUID, camps map[uuid.UUID]*campaignDelta) error {
	var (
		tuples []string
		args   []interface{}
		n      int
	)
	for _, id := range ids {
		d := camps[id]
		if d == nil || d.isZero() {
			continue
		}
		base := n * 10
		// Cast the first tuple so Postgres types the whole VALUES list.
		if n == 0 {
			tuples = append(tuples, fmt.Sprintf(
				"($%d::uuid,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint,$%d::bigint)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10))
		} else {
			tuples = append(tuples, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10))
		}
		args = append(args, id, d.opens, d.uniqueOpens, d.clicks, d.uniqueClicks,
			d.delivered, d.bounces, d.hard, d.soft, d.complaints)
		n++
	}
	if n == 0 {
		return nil
	}

	q := `
		UPDATE mailing_campaigns c SET
			open_count         = COALESCE(c.open_count, 0)         + v.opens,
			unique_open_count  = COALESCE(c.unique_open_count, 0)  + v.uopens,
			click_count        = COALESCE(c.click_count, 0)        + v.clicks,
			unique_click_count = COALESCE(c.unique_click_count, 0) + v.uclicks,
			delivered_count    = COALESCE(c.delivered_count, 0)    + v.delivered,
			bounce_count       = COALESCE(c.bounce_count, 0)       + v.bounces,
			hard_bounce_count  = COALESCE(c.hard_bounce_count, 0)  + v.hard,
			soft_bounce_count  = COALESCE(c.soft_bounce_count, 0)  + v.soft,
			complaint_count    = COALESCE(c.complaint_count, 0)    + v.complaints,
			updated_at = NOW()
		FROM (VALUES ` + strings.Join(tuples, ",") + `)
			AS v(id, opens, uopens, clicks, uclicks, delivered, bounces, hard, soft, complaints)
		WHERE c.id = v.id`
	_, err := b.db.ExecContext(ctx, q, args...)
	return err
}

// flushSubscribers writes every pending subscriber denorm delta in ONE
// statement, recomputing engagement_score inline. Computing the score in SQL
// removes the SELECT+UPDATE round-trip pair that used to run per event.
//
// The formula mirrors recomputeSubscriberEngagementScore exactly:
//
//	score = openRate*0.4 + clickRate*0.6 (+20 if opened <7d, +10 if <30d), capped 100
//
// on the 0–100 mailing_subscribers scale (NOT the 0–1 inbox-profile scale).
func (b *sesEngagementBatcher) flushSubscribers(ctx context.Context, subs map[uuid.UUID]*subscriberDelta) error {
	const chunkRows = 500 // see flushCampaigns — bind-parameter ceiling

	// Sorted for the same reason as campaigns: a global lock order prevents
	// deadlocks between concurrent flushes.
	ids := sortedSubscriberIDs(subs)

	for start := 0; start < len(ids); start += chunkRows {
		end := start + chunkRows
		if end > len(ids) {
			end = len(ids)
		}
		if err := b.flushSubscriberChunk(ctx, ids[start:end], subs); err != nil {
			return err
		}
	}
	return nil
}

func (b *sesEngagementBatcher) flushSubscriberChunk(ctx context.Context, ids []uuid.UUID, subs map[uuid.UUID]*subscriberDelta) error {
	var (
		tuples []string
		args   []interface{}
		n      int
	)
	epoch := time.Unix(0, 0).UTC()
	for _, id := range ids {
		d := subs[id]
		if d == nil || (d.opens == 0 && d.clicks == 0) {
			continue
		}
		lo, lc := d.lastOpen, d.lastClick
		if lo.IsZero() {
			lo = epoch
		}
		if lc.IsZero() {
			lc = epoch
		}
		base := n * 5
		if n == 0 {
			tuples = append(tuples, fmt.Sprintf("($%d::uuid,$%d::bigint,$%d::bigint,$%d::timestamptz,$%d::timestamptz)",
				base+1, base+2, base+3, base+4, base+5))
		} else {
			tuples = append(tuples, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d)", base+1, base+2, base+3, base+4, base+5))
		}
		args = append(args, id, d.opens, d.clicks, lo, lc)
		n++
	}
	if n == 0 {
		return nil
	}

	q := `
		UPDATE mailing_subscribers s SET
			total_opens  = COALESCE(s.total_opens, 0)  + v.opens,
			total_clicks = COALESCE(s.total_clicks, 0) + v.clicks,
			last_open_at = CASE WHEN v.opens > 0
				THEN GREATEST(COALESCE(s.last_open_at, 'epoch'::timestamptz), v.last_open)
				ELSE s.last_open_at END,
			last_click_at = CASE WHEN v.clicks > 0
				THEN GREATEST(COALESCE(s.last_click_at, 'epoch'::timestamptz), v.last_click)
				ELSE s.last_click_at END,
			engagement_score = LEAST(100, (
				  ((COALESCE(s.total_opens,0)  + v.opens)::numeric
					/ GREATEST(COALESCE(s.total_emails_received, 1), 1) * 100 * 0.4)
				+ ((COALESCE(s.total_clicks,0) + v.clicks)::numeric
					/ GREATEST(COALESCE(s.total_emails_received, 1), 1) * 100 * 0.6)
				+ (CASE
					WHEN GREATEST(COALESCE(s.last_open_at, 'epoch'::timestamptz), v.last_open) > NOW() - INTERVAL '7 days'  THEN 20
					WHEN GREATEST(COALESCE(s.last_open_at, 'epoch'::timestamptz), v.last_open) > NOW() - INTERVAL '30 days' THEN 10
					ELSE 0 END)
			)),
			updated_at = NOW()
		FROM (VALUES ` + strings.Join(tuples, ",") + `)
			AS v(id, opens, clicks, last_open, last_click)
		WHERE s.id = v.id`
	_, err := b.db.ExecContext(ctx, q, args...)
	return err
}

// pending reports current backlog depth for /health.
func (b *sesEngagementBatcher) pending() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.campaigns), len(b.subscribers)
}

func (b *sesEngagementBatcher) shutdown(wait time.Duration) {
	b.stopOnce.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-time.After(wait):
		c, s := b.pending()
		log.Printf("[ses-engagement] shutdown drain timed out, %d campaign / %d subscriber deltas unflushed", c, s)
	}
}

// ---------------------------------------------------------------------------
// registry for /health + shutdown
// ---------------------------------------------------------------------------

var (
	sesBatcherMu     sync.RWMutex
	globalSESBatcher *sesEngagementBatcher
)

func registerSESBatcher(b *sesEngagementBatcher) {
	sesBatcherMu.Lock()
	globalSESBatcher = b
	sesBatcherMu.Unlock()
}

// ShutdownSESEngagement flushes pending counter deltas. Must run BEFORE the
// process exits or the last interval's increments are lost (recoverable from
// mailing_tracking_events, but only by an explicit recount).
func ShutdownSESEngagement() {
	sesBatcherMu.RLock()
	b := globalSESBatcher
	sesBatcherMu.RUnlock()
	if b == nil {
		return
	}
	c, s := b.pending()
	log.Printf("[ses-engagement] draining %d campaign / %d subscriber deltas...", c, s)
	b.shutdown(20 * time.Second)
}

// SESEngagementStatus is the /health "ses_engagement" block.
type SESEngagementStatus struct {
	Enabled            bool   `json:"enabled"`
	IntervalSeconds    int    `json:"interval_seconds"`
	PendingCampaigns   int    `json:"pending_campaigns"`
	PendingSubscribers int    `json:"pending_subscribers"`
	Flushes            uint64 `json:"flushes"`
	CampaignRows       uint64 `json:"campaign_rows_written"`
	SubscriberRows     uint64 `json:"subscriber_rows_written"`
	FlushFailed        uint64 `json:"flush_failed"`
	FoldedEvents       uint64 `json:"folded_events"`
	FoldRatio          string `json:"fold_ratio"`
}

// CurrentSESEngagementStatus is a cheap read-only snapshot. fold_ratio is the
// headline number: events folded per campaign row actually written, i.e. how
// many row-lock acquisitions were avoided.
func CurrentSESEngagementStatus() SESEngagementStatus {
	sesBatcherMu.RLock()
	b := globalSESBatcher
	sesBatcherMu.RUnlock()

	st := SESEngagementStatus{
		Flushes:        atomic.LoadUint64(&sesFlushes),
		CampaignRows:   atomic.LoadUint64(&sesFlushCampaigns),
		SubscriberRows: atomic.LoadUint64(&sesFlushSubscribers),
		FlushFailed:    atomic.LoadUint64(&sesFlushFailed),
		FoldedEvents:   atomic.LoadUint64(&sesFoldedEvents),
	}
	if b != nil {
		st.Enabled = true
		st.IntervalSeconds = int(b.interval / time.Second)
		st.PendingCampaigns, st.PendingSubscribers = b.pending()
	}
	if st.CampaignRows > 0 {
		st.FoldRatio = fmt.Sprintf("%.1fx", float64(st.FoldedEvents)/float64(st.CampaignRows))
	} else {
		st.FoldRatio = "n/a"
	}
	return st
}

// ---------------------------------------------------------------------------
// campaign -> sending domain cache
// ---------------------------------------------------------------------------

// ResolveSendingDomainForCampaign issues a DB query per call, for a value that
// effectively never changes for a given campaign. At SES ingest volume that was
// one wasted round-trip on every open and click. Cache it.
type sdCacheEntry struct {
	domain string
	at     time.Time
}

var (
	sdCacheMu  sync.RWMutex
	sdCache    = map[uuid.UUID]sdCacheEntry{}
	sdCacheTTL = 30 * time.Minute
)

func cachedSendingDomain(ctx context.Context, db *sql.DB, campaignID uuid.UUID,
	resolve func(context.Context, *sql.DB, uuid.UUID) string) string {

	sdCacheMu.RLock()
	e, ok := sdCache[campaignID]
	sdCacheMu.RUnlock()
	if ok && time.Since(e.at) < sdCacheTTL {
		return e.domain
	}
	d := resolve(ctx, db, campaignID)
	sdCacheMu.Lock()
	// Bound the cache — campaigns are finite per day but the process is long-lived.
	if len(sdCache) > 20000 {
		sdCache = map[uuid.UUID]sdCacheEntry{}
	}
	sdCache[campaignID] = sdCacheEntry{domain: d, at: time.Now()}
	sdCacheMu.Unlock()
	return d
}

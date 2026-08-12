package api

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// ses_engagement_batcher_test.go pins the behavior that removes hot-row
// contention from SES ingest. The property that matters: N events against one
// campaign must produce ONE row write, not N.

func newBatcherForTest(t *testing.T) (*sesEngagementBatcher, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	atomic.StoreUint64(&sesFlushes, 0)
	atomic.StoreUint64(&sesFlushCampaigns, 0)
	atomic.StoreUint64(&sesFlushSubscribers, 0)
	atomic.StoreUint64(&sesFlushFailed, 0)
	atomic.StoreUint64(&sesFoldedEvents, 0)

	// Build directly (no goroutine) so flushes are explicit and deterministic.
	b := &sesEngagementBatcher{
		db:          db,
		campaigns:   make(map[uuid.UUID]*campaignDelta),
		subscribers: make(map[uuid.UUID]*subscriberDelta),
		interval:    time.Hour,
		maxRows:     100000,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	return b, mock
}

// TestBatcher_FoldsManyEventsIntoOneWrite is the core guarantee. 500 opens on
// one campaign previously meant 500 serialized UPDATEs against a single row —
// the measured source of Lock/transactionid + MultiXact contention. It must
// now be a single write carrying +500.
func TestBatcher_FoldsManyEventsIntoOneWrite(t *testing.T) {
	b, mock := newBatcherForTest(t)
	camp := uuid.New()

	for i := 0; i < 500; i++ {
		b.addCampaign(camp, func(d *campaignDelta) { d.opens++ })
	}

	// Exactly ONE statement, carrying the folded total.
	mock.ExpectExec("UPDATE mailing_campaigns").
		WithArgs(camp, int64(500), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	b.flush()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected exactly one folded UPDATE: %v", err)
	}
	if got := atomic.LoadUint64(&sesFlushCampaigns); got != 1 {
		t.Fatalf("campaign rows written = %d, want 1 (500 events must fold to 1 row)", got)
	}
	if got := atomic.LoadUint64(&sesFoldedEvents); got != 500 {
		t.Fatalf("folded events = %d, want 500", got)
	}
}

// TestBatcher_AllCountersFoldIndependently guards the delta arithmetic — a
// mis-mapped column would corrupt reported campaign performance.
func TestBatcher_AllCountersFoldIndependently(t *testing.T) {
	b, mock := newBatcherForTest(t)
	camp := uuid.New()

	for i := 0; i < 3; i++ {
		b.addCampaign(camp, func(d *campaignDelta) { d.opens++; d.uniqueOpens++ })
	}
	for i := 0; i < 2; i++ {
		b.addCampaign(camp, func(d *campaignDelta) { d.clicks++ })
	}
	b.addCampaign(camp, func(d *campaignDelta) { d.clicks++; d.uniqueClicks++ })
	for i := 0; i < 7; i++ {
		b.addCampaign(camp, func(d *campaignDelta) { d.delivered++ })
	}
	b.addCampaign(camp, func(d *campaignDelta) { d.bounces++; d.hard++ })
	b.addCampaign(camp, func(d *campaignDelta) { d.bounces++; d.soft++ })
	b.addCampaign(camp, func(d *campaignDelta) { d.complaints++ })

	// opens=3 uopens=3 clicks=3 uclicks=1 delivered=7 bounces=2 hard=1 soft=1 complaints=1
	mock.ExpectExec("UPDATE mailing_campaigns").
		WithArgs(camp, int64(3), int64(3), int64(3), int64(1), int64(7), int64(2), int64(1), int64(1), int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	b.flush()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("counter fold mismatch: %v", err)
	}
}

// TestBatcher_RetainsDeltasOnFlushFailure proves a transient DB error DELAYS
// the rollup instead of destroying it. Dropping deltas here would be the same
// class of silent loss the whole SES effort exists to eliminate.
func TestBatcher_RetainsDeltasOnFlushFailure(t *testing.T) {
	b, mock := newBatcherForTest(t)
	camp := uuid.New()

	b.addCampaign(camp, func(d *campaignDelta) { d.opens += 10 })

	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnError(errors.New("canceling statement due to user request"))
	b.flush()

	if got := atomic.LoadUint64(&sesFlushFailed); got != 1 {
		t.Fatalf("flush_failed = %d, want 1", got)
	}
	c, _ := b.pending()
	if c != 1 {
		t.Fatalf("pending campaigns = %d, want 1 — deltas must be retained for retry", c)
	}

	// Next flush succeeds and carries the SAME total.
	mock.ExpectExec("UPDATE mailing_campaigns").
		WithArgs(camp, int64(10), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	b.flush()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("retained deltas not replayed: %v", err)
	}
}

// TestBatcher_SubscriberFlushKeepsLatestTimestamps guards last_open_at, which
// gates the Welcome-saturation segment (prior_sends > 8 AND last_open_at IS
// NULL). A subscriber whose SES open never lands there reads as a never-opener.
func TestBatcher_SubscriberFlushKeepsLatestTimestamps(t *testing.T) {
	b, mock := newBatcherForTest(t)
	sub := uuid.New()

	early := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)

	b.addSubscriber(sub, 1, 0, early)
	b.addSubscriber(sub, 1, 0, late)  // later open wins
	b.addSubscriber(sub, 1, 0, early) // out-of-order arrival must NOT regress it
	b.addSubscriber(sub, 0, 1, late)

	mock.ExpectExec("UPDATE mailing_subscribers").
		WithArgs(sub, int64(3), int64(1), late, late).
		WillReturnResult(sqlmock.NewResult(0, 1))

	b.flush()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("subscriber fold/timestamp mismatch: %v", err)
	}
}

// TestBatcher_SubscriberScoreFormulaPresent guards the engagement-score SQL
// that replaced recomputeSubscriberEngagementScore. The 0-100 subscriber scale
// weights opens 0.4 / clicks 0.6 with a recency bonus — mixing it up with the
// 0-1 inbox-profile scale is a documented footgun in this codebase.
//
// sqlmock's default matcher is regexp-based against the ACTUAL executed SQL, so
// each fragment below is a real assertion on the statement the batcher emits.
func TestBatcher_SubscriberScoreFormulaPresent(t *testing.T) {
	for _, frag := range []string{
		`engagement_score = LEAST\(100`,
		`\* 100 \* 0\.4`,
		`\* 100 \* 0\.6`,
		`INTERVAL '7 days'`,
		`INTERVAL '30 days'`,
		`GREATEST\(COALESCE\(s\.total_emails_received, 1\), 1\)`,
	} {
		t.Run(frag, func(t *testing.T) {
			b, mock := newBatcherForTest(t)
			sub := uuid.New()
			b.addSubscriber(sub, 1, 0, time.Now().UTC())

			mock.ExpectExec(frag).WillReturnResult(sqlmock.NewResult(0, 1))
			b.flush()

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("engagement-score SQL missing %q — formula drifted: %v", frag, err)
			}
		})
	}
}

// TestBatcher_ConcurrentAddsAreRaceFree exercises the mutex under the same
// concurrency the ingest worker pool applies. Run with -race.
func TestBatcher_ConcurrentAddsAreRaceFree(t *testing.T) {
	b, mock := newBatcherForTest(t)
	camps := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	var wg sync.WaitGroup
	for w := 0; w < 24; w++ { // matches the 12 workers x 2 tasks in production
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				b.addCampaign(camps[i%len(camps)], func(d *campaignDelta) { d.opens++ })
			}
		}(w)
	}
	wg.Wait()

	// 24 workers x 100 events = 2400, spread over 3 campaigns = 800 each,
	// written as 3 rows total.
	mock.ExpectExec("UPDATE mailing_campaigns").WillReturnResult(sqlmock.NewResult(0, 3))
	b.flush()

	if got := atomic.LoadUint64(&sesFoldedEvents); got != 2400 {
		t.Fatalf("folded events = %d, want 2400 (lost increments under concurrency)", got)
	}
	if got := atomic.LoadUint64(&sesFlushCampaigns); got != 3 {
		t.Fatalf("campaign rows = %d, want 3", got)
	}
}

// TestBatcher_EmptyFlushIsNoOp keeps idle servers from issuing pointless writes.
func TestBatcher_EmptyFlushIsNoOp(t *testing.T) {
	b, mock := newBatcherForTest(t)
	b.flush()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty flush should issue no statements: %v", err)
	}
	if got := atomic.LoadUint64(&sesFlushes); got != 0 {
		t.Fatalf("flushes = %d, want 0", got)
	}
}

// TestSendingDomainCache_AvoidsRepeatQueries proves the per-event campaign
// lookup is cached — it was one DB round-trip per open and click for a value
// that never changes.
func TestSendingDomainCache_AvoidsRepeatQueries(t *testing.T) {
	sdCacheMu.Lock()
	sdCache = map[uuid.UUID]sdCacheEntry{}
	sdCacheMu.Unlock()

	camp := uuid.New()
	calls := 0
	fn := func(ctx context.Context, _ *sql.DB, id uuid.UUID) string {
		calls++
		return "em.discountblog.com"
	}
	for i := 0; i < 50; i++ {
		if got := cachedSendingDomain(context.Background(), nil, camp, fn); got != "em.discountblog.com" {
			t.Fatalf("got %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (cache not effective)", calls)
	}
}

package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// handlers_ses_events_async_test.go pins the behavior of the async SES ingest
// queue (ses_events_queue.go).
//
// The bug these guard against: the webhook used to do all of its DB work
// synchronously on the SNS request path and then answer 200 REGARDLESS of
// whether the write succeeded. Because the SNS HTTPS subscription allows only
// 3 retries at 20s and has NO dead-letter queue, a 200 is a promise we can
// never take back — every swallowed persist error, and every request that ran
// long enough for SNS to time out, destroyed an open/click/delivery event
// permanently. 13,078 events were lost this way in the 24h to 2026-08-11.

// resetSESCounters zeroes the package-level counters so each test observes only
// its own effects.
func resetSESCounters() {
	atomic.StoreUint64(&sesQueueEnqueued, 0)
	atomic.StoreUint64(&sesQueueRejected, 0)
	atomic.StoreUint64(&sesQueueProcessed, 0)
	atomic.StoreUint64(&sesQueueRetried, 0)
	atomic.StoreUint64(&sesQueueFailed, 0)
	atomic.StoreUint64(&sesRollupFailed, 0)
	atomic.StoreUint64(&sesEngagementFailed, 0)
	atomic.StoreUint64(&sesSyncPersistFailed, 0)
	atomic.StoreInt64(&sesQueueDepth, 0)
	atomic.StoreInt64(&sesRetriesPending, 0)
}

// newAsyncHandlerForTest builds a handler with the async queue ENABLED and a
// single worker (so sqlmock sees serialized access and assertions are
// deterministic).
func newAsyncHandlerForTest(t *testing.T) (*SESEventsHandler, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Setenv("SES_WEBHOOK_DISABLE_SIG", "true")
	t.Setenv("SES_WEBHOOK_ASYNC", "true")
	t.Setenv("SES_WEBHOOK_WORKERS", "1")
	resetSESCounters()
	resetSendingDomainCache()

	hub := engine.NewGlobalSuppressionHub(db, "00000000-0000-0000-0000-000000000001", "")
	h := NewSESEventsHandler(db, hub, "00000000-0000-0000-0000-000000000001")
	t.Cleanup(func() {
		if h.queue != nil {
			h.queue.shutdown(5 * time.Second)
		}
	})
	return h, mock, db
}

// openNotification is a minimal, fully-tagged SES OPEN event.
func openNotification(t *testing.T, campaignID string) []byte {
	t.Helper()
	return snsNotificationBody(t, map[string]interface{}{
		"eventType": "Open",
		"mail": map[string]interface{}{
			"timestamp": "2026-08-11T19:00:00.000Z",
			"source":    "news@em.discountblog.com",
			"tags": map[string][]string{
				"campaign_id":       {campaignID},
				"recipient_send_id": {"send-abc"},
			},
			"commonHeaders": map[string]interface{}{
				"to": []string{"human@gmail.com"},
			},
		},
		"open": map[string]interface{}{
			"timestamp": "2026-08-11T19:00:05.000Z",
			"ipAddress": "66.249.84.1",
			"userAgent": "Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 Safari/605.1.15",
		},
	})
}

const testCampaignID = "11111111-1111-1111-1111-111111111111"

// TestSESAsync_PersistsAfterHandlerReturns is the load-bearing test for the
// whole fix. The async worker MUST NOT use the request context: that context is
// cancelled the instant ServeHTTP returns, so if the worker inherited it every
// DB write would be cancelled and the fix would silently lose MORE events than
// the bug it replaced. Here the request context is cancelled before the worker
// can run, and the INSERT must still happen.
func TestSESAsync_PersistsAfterHandlerReturns(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)

	// Counters are batched (ses_engagement_batcher.go). This notification has no
	// subscriber_id tag, so subID stays nil and the subscriber-scoped work
	// (unique check, sending-domain lookup) is skipped — leaving the
	// authoritative INSERT plus the inbox profile.
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(10, 1, 0, nil))
	mock.ExpectExec("UPDATE mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))

	// A request context that is ALREADY cancelled by the time the worker runs.
	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
		bytes.NewReader(openNotification(t, testCampaignID)))
	h.ServeHTTP(rr, req.WithContext(ctx))
	cancel() // request is over — this must NOT abort the queued work

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	// Drain and assert the write actually landed.
	h.queue.shutdown(5 * time.Second)

	if got := atomic.LoadUint64(&sesQueueProcessed); got != 1 {
		t.Fatalf("processed = %d, want 1 (the queued event must persist after the request ends)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("tracking-event INSERT did not run after the handler returned: %v", err)
	}
}

// TestSESAsync_RespondsWithoutTouchingTheDB proves the SNS request path no
// longer performs DB work. sqlmock is given ZERO expectations, so any query
// issued during ServeHTTP fails the test. This is what keeps the server's DB
// latency out of SNS's delivery timeout.
func TestSESAsync_RespondsWithoutTouchingTheDB(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
		bytes.NewReader(openNotification(t, testCampaignID)))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := atomic.LoadUint64(&sesQueueEnqueued); got != 1 {
		t.Fatalf("enqueued = %d, want 1", got)
	}
	// Nothing should have been executed on the request path.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB work on the SNS request path: %v", err)
	}
}

// TestSESAsync_QueueFullReturns503 proves that when the buffer cannot accept an
// event we tell SNS to retry instead of accepting-then-dropping. A 200 here
// would be the original bug in a new place: SNS would consider the event
// delivered and never send it again.
func TestSESAsync_QueueFullReturns503(t *testing.T) {
	h, _, _ := newAsyncHandlerForTest(t)

	// Replace the running queue with a depth-1 queue that has NO workers, so
	// nothing drains it and the second enqueue is guaranteed to be refused.
	h.queue.shutdown(5 * time.Second)
	h.queue = &sesIngestQueue{
		h:    h,
		ch:   make(chan sesQueueItem, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	resetSESCounters()

	post := func() int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
			bytes.NewReader(openNotification(t, testCampaignID)))
		h.ServeHTTP(rr, req.WithContext(context.Background()))
		return rr.Code
	}

	if code := post(); code != http.StatusOK {
		t.Fatalf("first post status = %d, want 200 (buffer has room)", code)
	}
	if code := post(); code != http.StatusServiceUnavailable {
		t.Fatalf("second post status = %d, want 503 so SNS RETRIES rather than dropping the event", code)
	}
	if got := atomic.LoadUint64(&sesQueueRejected); got != 1 {
		t.Fatalf("rejected = %d, want 1", got)
	}
	// The stand-in queue has no workers and therefore never closes `done`;
	// clear it so the cleanup hook does not block waiting for a drain.
	h.queue = nil
}

// TestSESPersist_ReturnsErrorOnDBFailure is the direct regression guard on the
// original defect. persistSESEvent used to log-and-return on an INSERT failure,
// giving its caller no way to know the event had been lost. It must now surface
// the error so the ingest worker can retry it.
//
// Negative control: revert the `return fmt.Errorf(...)` in persistSESEvent back
// to a bare `return` and this test goes RED.
func TestSESPersist_ReturnsErrorOnDBFailure(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)

	boom := errors.New("pq: canceling statement due to user request")
	mock.ExpectExec("INSERT INTO mailing_tracking_events").WillReturnError(boom)

	err := h.persistSESEvent(context.Background(), "opened", sesEventNotification{},
		testCampaignID, "", "send-abc", "human@gmail.com", "", "", "", "", "", "", "2026-08-11T19:00:00.000Z")

	if err == nil {
		t.Fatal("persistSESEvent returned nil on a failed INSERT — the event would be silently lost and never retried")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error did not wrap the driver error: %v", err)
	}
}

// TestSESAsync_RetryDoesNotHoldAWorker proves a failed attempt returns the
// worker to the pool immediately and schedules a re-queue, rather than sleeping
// through its backoff.
//
// Retrying in place was a capacity bug: during a DB brownout, 12 workers each
// sleeping up to 30s collapsed throughput exactly when the queue was filling —
// `enqueued` raced 2,323 -> 3,577 while `processed` crawled 1,636 -> 1,674.
func TestSESAsync_RetryDoesNotHoldAWorker(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)
	t.Setenv("SES_WEBHOOK_MAX_ATTEMPTS", "6")

	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WillReturnError(errors.New("canceling statement due to user request"))

	start := time.Now()
	h.queue.process(sesQueueItem{
		env:  snsEnvelope{MessageId: "msg-retry"},
		note: openNote(),
	})
	elapsed := time.Since(start)

	// process() must return promptly — the backoff happens on a timer, not here.
	if elapsed > time.Second {
		t.Fatalf("process() blocked for %s — the worker slept through its backoff "+
			"instead of scheduling a re-queue", elapsed)
	}
	if got := atomic.LoadUint64(&sesQueueRetried); got != 1 {
		t.Fatalf("retried = %d, want 1", got)
	}
	if got := atomic.LoadUint64(&sesQueueFailed); got != 0 {
		t.Fatalf("failed_permanent = %d, want 0 — attempts remain", got)
	}
	if got := atomic.LoadInt64(&sesRetriesPending); got != 1 {
		t.Fatalf("retries_pending = %d, want 1 (retry must be scheduled, not dropped)", got)
	}
}

// TestSESAsync_CountsPermanentLossOnLastAttempt proves the final attempt gives
// up loudly and countably rather than silently.
func TestSESAsync_CountsPermanentLossOnLastAttempt(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)
	t.Setenv("SES_WEBHOOK_MAX_ATTEMPTS", "3")

	mock.ExpectExec("INSERT INTO mailing_tracking_events").
		WillReturnError(errors.New("context deadline exceeded"))

	// attempt=2 means this call is the 3rd and final one.
	h.queue.process(sesQueueItem{
		env:     snsEnvelope{MessageId: "msg-last"},
		note:    openNote(),
		attempt: 2,
	})

	if got := atomic.LoadUint64(&sesQueueFailed); got != 1 {
		t.Fatalf("failed_permanent = %d, want 1 — a lost event MUST be counted", got)
	}
	if got := atomic.LoadInt64(&sesRetriesPending); got != 0 {
		t.Fatalf("retries_pending = %d, want 0 on the final attempt", got)
	}
}

// openNote builds a minimal tagged OPEN notification for queue-level tests.
func openNote() sesEventNotification {
	var n sesEventNotification
	n.EventType = "Open"
	n.Mail.Tags = map[string][]string{"campaign_id": {testCampaignID}}
	return n
}

// TestSESEngagementFailure_IsCounted proves the open/click side-effect writes no
// longer fail silently. They were fire-and-forget with their errors discarded,
// so under DB pressure open_count/click_count/last_open_at simply stopped being
// updated and nothing recorded that it had happened.
func TestSESEngagementFailure_IsCounted(t *testing.T) {
	h, mock, _ := newAsyncHandlerForTest(t)

	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnError(errors.New("context deadline exceeded"))

	h.sesExec(context.Background(), "open_count",
		`UPDATE mailing_campaigns SET open_count = 1 WHERE id = $1`, testCampaignID)

	if got := atomic.LoadUint64(&sesEngagementFailed); got != 1 {
		t.Fatalf("engagement_failed = %d, want 1", got)
	}
}

// TestSESWebhookStatus_ExposedOnHealth proves the counters are reachable from
// the /health surface — an operator must be able to SEE loss, not infer it from
// a log grep after the fact.
func TestSESWebhookStatus_ExposedOnHealth(t *testing.T) {
	h, _, _ := newAsyncHandlerForTest(t)
	_ = h

	atomic.StoreUint64(&sesQueueFailed, 7)
	atomic.StoreUint64(&sesEngagementFailed, 3)

	st := CurrentSESWebhookStatus()
	if !st.AsyncEnabled {
		t.Fatal("async_enabled = false, want true when the queue is running")
	}
	if st.Failed != 7 {
		t.Fatalf("failed_permanent = %d, want 7", st.Failed)
	}
	if st.EngagementFailed != 3 {
		t.Fatalf("engagement_failed = %d, want 3", st.EngagementFailed)
	}
}

// TestSESAsync_KillSwitchRestoresSyncPath proves SES_WEBHOOK_ASYNC=false is a
// working one-move rollback to the previous behavior.
func TestSESAsync_KillSwitchRestoresSyncPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Setenv("SES_WEBHOOK_DISABLE_SIG", "true")
	t.Setenv("SES_WEBHOOK_ASYNC", "false")
	resetSESCounters()
	resetSendingDomainCache()

	hub := engine.NewGlobalSuppressionHub(db, "00000000-0000-0000-0000-000000000001", "")
	h := NewSESEventsHandler(db, hub, "00000000-0000-0000-0000-000000000001")

	if h.queue != nil {
		t.Fatal("queue should be nil when SES_WEBHOOK_ASYNC=false")
	}

	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec("INSERT INTO mailing_tracking_events").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("FROM mailing_inbox_profiles").
		WillReturnRows(sqlmock.NewRows([]string{"total_sends", "total_opens", "total_clicks", "last_open_at"}).
			AddRow(10, 1, 0, nil))
	mock.ExpectExec("UPDATE mailing_inbox_profiles").WillReturnResult(sqlmock.NewResult(1, 1))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
		bytes.NewReader(openNotification(t, testCampaignID)))
	h.ServeHTTP(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// On the sync path the INSERT must have happened INLINE.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sync path did not persist inline: %v", err)
	}
}

// TestSESRetryBudget_SpansABrownout pins the retry window. The original 3
// attempts with linear backoff covered ~6s, which a 4-minute estate-wide DB
// brownout blew straight through on 2026-08-12, permanently destroying two
// OPEN events. The budget must be measured in minutes, not seconds.
func TestSESRetryBudget_SpansABrownout(t *testing.T) {
	var total time.Duration
	n := maxSESAttempts()
	for i := 1; i < n; i++ {
		total += sesRetryBackoff(i)
	}
	if n < 5 {
		t.Fatalf("maxSESAttempts = %d, want >= 5", n)
	}
	if total < 45*time.Second {
		t.Fatalf("retry budget = %s across %d attempts, want >= 45s — a DB brownout "+
			"lasting minutes would exhaust it and destroy events", total, n)
	}
	// And it must stay bounded so a worker cannot park forever.
	if total > 5*time.Minute {
		t.Fatalf("retry budget = %s, want <= 5m (workers would stop draining)", total)
	}
}

// TestSESWebhook_503sBeforeHandlerIsWired guards the boot-time route.
//
// The route used to be registered only inside the async SetMailingDB block, so
// during every task start a POST from SNS fell through to the auth-protected
// router and got 401. Measured 2026-08-12 on an unplanned task replacement:
// 953 NumberOfNotificationsFailed in one 5-minute bucket, entirely inside that
// window, while the endpoint served 100% 200s before and after.
//
// The path must ALWAYS exist and must answer 503 (retryable, honest) until the
// handler is wired — never 401, which reads as an auth problem.
func TestSESWebhook_503sBeforeHandlerIsWired(t *testing.T) {
	sesHandlerMu.Lock()
	saved := globalSESHandler
	globalSESHandler = nil
	sesHandlerMu.Unlock()
	t.Cleanup(func() {
		sesHandlerMu.Lock()
		globalSESHandler = saved
		sesHandlerMu.Unlock()
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
		bytes.NewReader(openNotification(t, testCampaignID)))
	serveSESEventsWhenReady(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so SNS retries (401 reads as auth failure and "+
			"burns the subscription's 3-attempt budget)", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After — tell SNS when to come back")
	}
}

// TestSESWebhook_DelegatesOnceWired proves the boot-time route hands off to the
// real handler as soon as SetMailingDB publishes it.
func TestSESWebhook_DelegatesOnceWired(t *testing.T) {
	h, _, _ := newAsyncHandlerForTest(t)
	sesHandlerMu.Lock()
	saved := globalSESHandler
	globalSESHandler = h
	sesHandlerMu.Unlock()
	t.Cleanup(func() {
		sesHandlerMu.Lock()
		globalSESHandler = saved
		sesHandlerMu.Unlock()
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/ses-events",
		bytes.NewReader(openNotification(t, testCampaignID)))
	serveSESEventsWhenReady(rr, req.WithContext(context.Background()))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the handler is wired", rr.Code)
	}
}

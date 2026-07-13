package worker

// REQ-005 regression guards (findings 2026-07-13-B §1, §2): QueueRecovery's
// stale-age vs the send pool's claim-to-process tail (duplicate-send window),
// and the attempts/retry_count counter unification that stranded legacy
// 'failed' rows (never retried, never dead-lettered, invisible to the
// Delivery Queue dead-letter panel).

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueueRecovery_StaleAgeCoversClaimToProcessTail pins the derivation in
// queue_recovery.go: recovery must never requeue a row a live worker is
// still holding. Worst-case claimed-but-unprocessed backlog is the work
// channel buffer (batchSize*2 = 200) plus the dispatcher's parallel claim
// batches (8 campaigns × batch 100), drained by 25 workers each pinned at
// processItem's 30s per-item timeout.
func TestQueueRecovery_StaleAgeCoversClaimToProcessTail(t *testing.T) {
	const (
		workChBuffer        = 100 * 2 // batchSize*2, send_worker.go Start()
		dispatchParallelism = 8       // dispatchCampaignParallelism default
		batchSize           = 100
		workers             = 25 // NewSendWorkerPool(mailingDB, 25) at boot
		perItemTimeout      = 30 * time.Second
	)
	claimedBacklog := workChBuffer + dispatchParallelism*batchSize // 1,000
	worstCaseTail := time.Duration(claimedBacklog/workers) * perItemTimeout

	assert.Equal(t, 20*time.Minute, worstCaseTail, "buffer-math derivation drifted — update DefaultStaleAge's comment AND value")
	assert.Greater(t, int64(DefaultStaleAge), int64(worstCaseTail),
		"DefaultStaleAge must exceed the worst-case claim-to-process tail or recovery requeues rows live workers still hold (duplicate-send window)")
	assert.Equal(t, 25*time.Minute, DefaultStaleAge)
}

// TestQueueRecovery_PassesUseAttemptsCounterAndNewStaleAge runs a full
// recovery sweep and pins, per pass: (1) the v1 requeue keys and increments
// `attempts` — the SAME counter markFailed uses — never retry_count; (2) rows
// younger than DefaultStaleAge (25m) are protected by the `< NOW() - $1`
// bound, proven by the "25m0s" interval argument; (3) legacy 'failed' rows
// are requeued for retry only while their campaign is still 'sending';
// (4) the dead-letter pass keys on `attempts`, landing rows in
// status='dead_letter' — the status the Delivery Queue dead-letter panel
// lists (internal/api/outbox_admin.go).
func TestQueueRecovery_PassesUseAttemptsCounterAndNewStaleAge(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	staleArg := DefaultStaleAge.String() // "25m0s"

	// 1a. v1 stuck-row requeue: attempts counter, staleAge bound, crash
	// consumes one attempt.
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'queued',\s*worker_id = NULL,\s*claimed_at = NULL,\s*locked_at = NULL,\s*attempts = COALESCE\(attempts, 0\) \+ 1\s+WHERE status IN \('claimed', 'sending'\)\s+AND COALESCE\(locked_at, claimed_at\) < NOW\(\) - \$1::interval\s+AND COALESCE\(attempts, 0\) < \$2`).
		WithArgs(staleArg, MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 2))

	// 1a-bis. legacy 'failed' retry: campaign must still be 'sending';
	// attempts NOT incremented (markFailed already counted the attempt).
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue q\s+SET status = 'queued',\s*worker_id = NULL,\s*claimed_at = NULL,\s*locked_at = NULL\s+WHERE q\.status = 'failed'\s+AND COALESCE\(q\.last_attempt_at, q\.created_at\) < NOW\(\) - \$1::interval\s+AND COALESCE\(q\.attempts, 0\) < \$2\s+AND EXISTS \(\s*SELECT 1 FROM mailing_campaigns c\s+WHERE c\.id = q\.campaign_id AND c\.status = 'sending'\s*\)`).
		WithArgs(staleArg, MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 1b. v1 dead-letter keys on attempts and covers 'failed'.
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'dead_letter'\s+WHERE status IN \('claimed', 'sending', 'failed'\)\s+AND COALESCE\(attempts, 0\) >= \$1`).
		WithArgs(MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2a/2b. v2 passes untouched (retry_count on both sides — its only
	// writers, SendWorkerPoolV2/CampaignProcessor, use retry_count too).
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue_v2.*retry_count = retry_count \+ 1`).
		WithArgs(staleArg, MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue_v2\s+SET status = 'dead_letter'.*retry_count >= \$1`).
		WithArgs(MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))

	qr := NewQueueRecoveryWorker(db)
	qr.recoverStuckItems(context.Background())

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestQueueRecovery_FailedRowRecoveryKillSwitch: DISABLE_FAILED_ROW_RECOVERY
// reverts to the pre-fix pass set without a redeploy (send-path kill-switch
// rule). The failed-row requeue must NOT run; everything else is unchanged.
func TestQueueRecovery_FailedRowRecoveryKillSwitch(t *testing.T) {
	t.Setenv("DISABLE_FAILED_ROW_RECOVERY", "true")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	staleArg := DefaultStaleAge.String()

	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'queued'.*status IN \('claimed', 'sending'\)`).
		WithArgs(staleArg, MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// NOTE: no q.status = 'failed' requeue expectation — an Exec against it
	// would be an unexpected call and fail ExpectationsWereMet ordering.
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'dead_letter'`).
		WithArgs(MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE mailing_campaign_queue_v2`).
		WithArgs(staleArg, MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue_v2\s+SET status = 'dead_letter'`).
		WithArgs(MaxRetryCount).
		WillReturnResult(sqlmock.NewResult(0, 0))

	qr := NewQueueRecoveryWorker(db)
	qr.recoverStuckItems(context.Background())

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestFailedRowLifecycle_NonTransientFailureRetriedThenDeadLettered pins the
// legacy-mode contract end to end at the SQL level: a non-transient send
// error parks the row at status='failed' with `attempts` incremented (the
// unified counter), and once attempts+1 reaches MaxRetryCount the SAME
// markFailed path lands it in status='dead_letter' — visible to the
// dead-letter panel. Recovery's failed-row pass (tested above) is what makes
// the intermediate 'failed' parks claim-eligible again.
func TestFailedRowLifecycle_NonTransientFailureRetriedThenDeadLettered(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	pool := NewSendWorkerPool(db, 1) // legacy mode (outboxMode zero value)
	itemID := uuid.New()
	const errMsg = "550 5.1.1 user unknown" // non-transient: not IsPMTATransient

	// Attempts 0..3: parked at 'failed', attempts incremented — the retry
	// budget markFailed and recovery now share.
	for prior := 0; prior < MaxRetryCount-1; prior++ {
		mock.ExpectQuery(`SELECT COALESCE\(attempts, 0\) FROM mailing_campaign_queue WHERE id = \$1`).
			WithArgs(itemID).
			WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(prior))
		mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'failed', error_message = \$2, attempts = attempts \+ 1, last_attempt_at = NOW\(\)\s+WHERE id = \$1`).
			WithArgs(itemID, errMsg).
			WillReturnResult(sqlmock.NewResult(0, 1))
		require.NoError(t, pool.markFailed(context.Background(), itemID, errMsg))
	}

	// Attempt 4 (attempts+1 == MaxRetryCount): honest terminal status the
	// outbox dead-letter listing surfaces.
	mock.ExpectQuery(`SELECT COALESCE\(attempts, 0\) FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(itemID).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(MaxRetryCount - 1))
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'dead_letter', error_message = \$2, attempts = attempts \+ 1, last_attempt_at = NOW\(\)\s+WHERE id = \$1`).
		WithArgs(itemID, errMsg).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, pool.markFailed(context.Background(), itemID, errMsg))

	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestDeferStrictPool_UsesScheduledAtPushbackNotRetryAfter pins REQ-005
// criterion 4 (findings 2026-07-13-B §10): strict-pool deferral backs off via
// the scheduled_at pushback the claim query already honors
// (q.scheduled_at <= NOW(), claimISPForOne) instead of writing the dead
// retry_after column, and clears worker_id/locked_at so the flat ~5-minute
// locked_at shadow can no longer override the computed 30s→300s ladder. A
// deferred row therefore cannot be claimed before its backoff expires, with
// zero new claim predicates or indexes.
func TestDeferStrictPool_UsesScheduledAtPushbackNotRetryAfter(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	pool := NewSendWorkerPool(db, 1)
	item := QueueItem{ID: uuid.New(), CampaignID: uuid.New(), Email: "x@gmail.com"}

	mock.ExpectQuery(`SELECT COALESCE\(attempts, 0\) FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(item.ID).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(0))
	// The regex requires GREATEST(scheduled_at, $3) + worker_id/locked_at
	// clears, and — by matching the full SET clause — proves retry_after is
	// no longer written.
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_queue\s+SET status = 'queued', error_message = \$2, attempts = attempts \+ 1,\s*last_attempt_at = NOW\(\),\s*scheduled_at = GREATEST\(scheduled_at, \$3\),\s*worker_id = NULL,\s*locked_at = NULL\s+WHERE id = \$1`).
		WithArgs(item.ID, "deferred_strict_pool: no capacity", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, pool.deferStrictPool(context.Background(), item, "deferred_strict_pool: no capacity"))
	assert.NoError(t, mock.ExpectationsWereMet())
}

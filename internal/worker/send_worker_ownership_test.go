package worker

// REQ-005 criterion 1 regression guards (findings 2026-07-13-B §1): the
// duplicate-send window. A claimed item can wait in the work channel past
// QueueRecovery's stale-age; if recovery requeues it and another worker
// re-claims it, the original worker must NOT submit it to the transport.
// processItem re-verifies ownership (worker_id + claim status) immediately
// before sender.Send.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSender counts transport submissions — the thing the ownership
// re-check exists to prevent.
type recordingSender struct {
	calls int32
	fail  bool
}

func (r *recordingSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	atomic.AddInt32(&r.calls, 1)
	if r.fail {
		return &SendResult{Success: false}, fmt.Errorf("boom")
	}
	return &SendResult{Success: true, MessageID: "msg-1"}, nil
}

func newOwnershipTestPool(t *testing.T) (*SendWorkerPool, sqlmock.Sqlmock, *recordingSender) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	pool := NewSendWorkerPool(db, 1)
	pool.ctx = context.Background() // processItem derives its timeout from p.ctx (set by Start in prod)
	sender := &recordingSender{}
	pool.SetESPSenders(nil, sender, nil, nil) // ses slot — default for ESPType ""
	return pool, mock, sender
}

// expectProcessItemPreamble mocks the queries processItem runs before the
// ownership re-check for a minimal item (no ProfileID → tracking/SES/image
// helpers short-circuit; no suppression lists; nil hub/offer/creative).
func expectProcessItemPreamble(mock sqlmock.Sqlmock, campaignID uuid.UUID) {
	mock.ExpectQuery(`SELECT status FROM mailing_campaigns WHERE id=\$1`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("sending"))
	mock.ExpectQuery(`SELECT suppression_list_ids::text FROM mailing_campaigns WHERE id = \$1`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"suppression_list_ids"}).AddRow(nil))
}

// TestOwnershipRecheck_RequeuedRowSkipsSubmission: QueueRecovery requeued the
// row (worker_id NULL, status back to 'queued') while it waited in the work
// channel — the original worker must abort before the transport call.
func TestOwnershipRecheck_RequeuedRowSkipsSubmission(t *testing.T) {
	pool, mock, sender := newOwnershipTestPool(t)
	item := QueueItem{ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(), Email: "x@gmail.com"}

	expectProcessItemPreamble(mock, item.CampaignID)
	mock.ExpectQuery(`SELECT worker_id, status FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(item.ID).
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "status"}).AddRow(nil, "queued"))

	require.NoError(t, pool.processItem(item))
	assert.Equal(t, int32(0), atomic.LoadInt32(&sender.calls),
		"a requeued row must never be submitted by the worker that lost it")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestOwnershipRecheck_ReclaimedElsewhereSkipsSubmission: another worker
// already re-claimed the row (foreign worker_id, claim status present) —
// submitting here would deliver the email twice.
func TestOwnershipRecheck_ReclaimedElsewhereSkipsSubmission(t *testing.T) {
	pool, mock, sender := newOwnershipTestPool(t)
	item := QueueItem{ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(), Email: "x@gmail.com"}

	expectProcessItemPreamble(mock, item.CampaignID)
	mock.ExpectQuery(`SELECT worker_id, status FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(item.ID).
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "status"}).AddRow("worker-somebody-else", "sending"))

	require.NoError(t, pool.processItem(item))
	assert.Equal(t, int32(0), atomic.LoadInt32(&sender.calls),
		"a row re-claimed by another worker must not be double-submitted")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestOwnershipRecheck_OwnedRowProceedsToSubmission: the happy path is
// untouched — a row still carrying our worker_id and claim status submits
// exactly once. (Post-send bookkeeping DB calls receive unexpected-call
// errors from sqlmock; every one of them is log-and-continue in processItem,
// so the submission itself is the assertion.)
func TestOwnershipRecheck_OwnedRowProceedsToSubmission(t *testing.T) {
	pool, mock, sender := newOwnershipTestPool(t)
	item := QueueItem{ID: uuid.New(), CampaignID: uuid.New(), SubscriberID: uuid.New(), Email: "x@gmail.com"}

	expectProcessItemPreamble(mock, item.CampaignID)
	mock.ExpectQuery(`SELECT worker_id, status FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(item.ID).
		WillReturnRows(sqlmock.NewRows([]string{"worker_id", "status"}).AddRow(pool.workerID, "sending"))

	require.NoError(t, pool.processItem(item))
	assert.Equal(t, int32(1), atomic.LoadInt32(&sender.calls), "an owned row must submit exactly once")
}

// TestStillOwnsItem_StatusAndModeMatrix pins the helper's contract: legacy
// claims own 'sending', durable claims own 'submitting', and any other
// (worker_id, status) combination means ownership was lost.
func TestStillOwnsItem_StatusAndModeMatrix(t *testing.T) {
	cases := []struct {
		name     string
		durable  bool
		workerID interface{} // row value; set to "self" for pool.workerID
		status   string
		want     bool
	}{
		{"legacy owned", false, "self", "sending", true},
		{"legacy wrong status (recovery flipped to queued)", false, "self", "queued", false},
		{"legacy durable-status row not ours to send", false, "self", "submitting", false},
		{"durable owned", true, "self", "submitting", true},
		{"durable wrong status", true, "self", "sending", false},
		{"foreign worker", false, "worker-other", "sending", false},
		{"worker_id cleared by recovery", false, nil, "queued", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, mock, _ := newOwnershipTestPool(t)
			if tc.durable {
				pool.SetOutboxMode("durable")
			}
			rowWorker := tc.workerID
			if rowWorker == "self" {
				rowWorker = pool.workerID
			}
			itemID := uuid.New()
			mock.ExpectQuery(`SELECT worker_id, status FROM mailing_campaign_queue WHERE id = \$1`).
				WithArgs(itemID).
				WillReturnRows(sqlmock.NewRows([]string{"worker_id", "status"}).AddRow(rowWorker, tc.status))
			assert.Equal(t, tc.want, pool.stillOwnsItem(context.Background(), itemID))
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// TestStillOwnsItem_FailsOpenOnDBError: a DB blip must never halt sending —
// the re-check is a best-effort duplicate guard.
func TestStillOwnsItem_FailsOpenOnDBError(t *testing.T) {
	pool, mock, _ := newOwnershipTestPool(t)
	itemID := uuid.New()
	mock.ExpectQuery(`SELECT worker_id, status FROM mailing_campaign_queue WHERE id = \$1`).
		WithArgs(itemID).
		WillReturnError(fmt.Errorf("connection reset"))
	assert.True(t, pool.stillOwnsItem(context.Background(), itemID),
		"ownership re-check must fail OPEN on query error")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestStillOwnsItem_KillSwitch: DISABLE_SEND_OWNERSHIP_RECHECK=true restores
// pre-check behavior without a redeploy (send-path kill-switch rule) — no
// query is issued at all.
func TestStillOwnsItem_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_SEND_OWNERSHIP_RECHECK", "true")
	pool, mock, _ := newOwnershipTestPool(t)
	assert.True(t, pool.stillOwnsItem(context.Background(), uuid.New()))
	assert.NoError(t, mock.ExpectationsWereMet(), "kill switch must short-circuit before any DB read")
}

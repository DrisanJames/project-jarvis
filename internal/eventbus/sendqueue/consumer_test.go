package sendqueue

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// newCmd builds a SendCommand with a fresh deterministic key for a test.
func newCmd() SendCommand {
	return SendCommand{
		IdempotencyKey: uuid.New(),
		CampaignID:     uuid.New(),
		SubscriberID:   uuid.New(),
		WaveID:         uuid.New(),
		Recipient:      "user@example.com",
		ISP:            "gmail",
		Subject:        "hi",
	}
}

// --- ① fresh key: claim + send + record ---------------------------------------

func TestProcessSend_FreshKey_ClaimsSendsRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := newCmd()

	// ① CLAIM inserts and RETURNs the key (we own it). The 6th arg is the
	// marshaled command JSONB (GAP 1); AnyArg since exact bytes aren't load-bearing.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, cmd.CampaignID, cmd.SubscriberID, cmd.WaveID, "w1", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).AddRow(cmd.IdempotencyKey))

	// ③ RECORD claimed -> sent.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, "pmta-"+cmd.IdempotencyKey.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sink := NewCountingSink()
	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, cmd)
	if err != nil {
		t.Fatalf("processSend: %v", err)
	}
	if skipped {
		t.Fatal("fresh key should NOT be skipped")
	}
	if sink.Sends() != 1 || sink.DistinctKeys() != 1 {
		t.Fatalf("sink: sends=%d distinct=%d, want 1/1", sink.Sends(), sink.DistinctKeys())
	}
	if sink.DuplicateKeySends() != 0 {
		t.Fatalf("DuplicateKeySends=%d, want 0", sink.DuplicateKeySends())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- ② duplicate (already sent): SKIP, sink never called -----------------------

func TestProcessSend_AlreadySent_SkipsWithoutSink(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := newCmd()

	// ① CLAIM conflicts (no rows returned).
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, cmd.CampaignID, cmd.SubscriberID, cmd.WaveID, "w1", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	// status read -> 'sent'
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, worker_id")).
		WithArgs(cmd.IdempotencyKey, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "worker_id", "stale"}).AddRow("sent", "wOther", false))

	sink := NewCountingSink()
	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, cmd)
	if err != nil {
		t.Fatalf("processSend: %v", err)
	}
	if !skipped {
		t.Fatal("already-sent key MUST be skipped")
	}
	if sink.Sends() != 0 {
		t.Fatalf("sink was called %d times for an already-sent key; want 0", sink.Sends())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- live owner (claimed by someone else, not stale): SKIP --------------------

func TestProcessSend_LiveOwner_Skips(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := newCmd()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, worker_id")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "worker_id", "stale"}).AddRow("claimed", "wOther", false))

	sink := NewCountingSink()
	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, cmd)
	if err != nil {
		t.Fatalf("processSend: %v", err)
	}
	if !skipped {
		t.Fatal("live-owner claimed key MUST be skipped")
	}
	if sink.Sends() != 0 {
		t.Fatalf("sink called %d times; want 0", sink.Sends())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- failed status: re-claim then send ----------------------------------------

func TestProcessSend_Failed_ReclaimsAndSends(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := newCmd()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, worker_id")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "worker_id", "stale"}).AddRow("failed", "wOld", false))
	// reclaim UPDATE ... WHERE status='failed' RETURNING key. 4th arg is the
	// command-backfill JSONB (GAP 1).
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, "w1", "failed", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).AddRow(cmd.IdempotencyKey))
	// record sent
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, "pmta-"+cmd.IdempotencyKey.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sink := NewCountingSink()
	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, cmd)
	if err != nil {
		t.Fatalf("processSend: %v", err)
	}
	if skipped {
		t.Fatal("failed key should be re-claimed and sent, not skipped")
	}
	if sink.Sends() != 1 || sink.DuplicateKeySends() != 0 {
		t.Fatalf("sink sends=%d dup=%d, want 1/0", sink.Sends(), sink.DuplicateKeySends())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- send error: leaves claimed, returns error (no commit) --------------------

func TestProcessSend_SendError_LeavesClaimedNoCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := newCmd()
	// claim succeeds
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).AddRow(cmd.IdempotencyKey))
	// NO record UPDATE expected — send fails before ③.

	wantErr := errors.New("transport down")
	sink := NewCountingSink()
	sink.BeforeSend = func(SendCommand) error { return wantErr }

	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, cmd)
	if err == nil {
		t.Fatal("expected error when send fails")
	}
	if skipped {
		t.Fatal("send error must NOT be a skip (offset must not commit)")
	}
	if sink.Sends() != 0 {
		t.Fatalf("BeforeSend rejected, so no accepted send; got %d", sink.Sends())
	}
	// ExpectationsWereMet: the record UPDATE was never issued — good (row stays claimed).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- TWO concurrent processSend on the SAME key -> exactly ONE sends ----------
//
// The ON CONFLICT serializes: one INSERT wins (RETURNING the key), the other
// conflicts and reads status='claimed' owned by the WINNER (not us, not stale)
// -> SKIP. So the sink is asked to send exactly once, and DuplicateKeySends==0.
func TestProcessSend_ConcurrentSameKey_ExactlyOneSend(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	cmd := newCmd()

	// Winner: claim returns the key, then records sent.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, cmd.CampaignID, cmd.SubscriberID, cmd.WaveID, "winner", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).AddRow(cmd.IdempotencyKey))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, "pmta-"+cmd.IdempotencyKey.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Loser: claim conflicts, status read shows the WINNER owns a live 'claimed'.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO mailing_send_ledger")).
		WithArgs(cmd.IdempotencyKey, cmd.CampaignID, cmd.SubscriberID, cmd.WaveID, "loser", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, worker_id")).
		WithArgs(cmd.IdempotencyKey, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "worker_id", "stale"}).AddRow("claimed", "winner", false))

	sink := NewCountingSink()
	sink.Delay = 5 * time.Millisecond // widen the window

	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i, w := range []string{"winner", "loser"} {
		wg.Add(1)
		go func(i int, w string) {
			defer wg.Done()
			results[i], errs[i] = processSend(context.Background(), db, sink, w, time.Minute, cmd)
		}(i, w)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("worker %d errored: %v", i, e)
		}
	}
	// Exactly one accepted send, no duplicates ever reached the sink.
	if sink.Sends() != 1 {
		t.Fatalf("sink.Sends()=%d, want exactly 1 (no double-send)", sink.Sends())
	}
	if sink.DistinctKeys() != 1 {
		t.Fatalf("sink.DistinctKeys()=%d, want 1", sink.DistinctKeys())
	}
	if sink.DuplicateKeySends() != 0 {
		t.Fatalf("sink.DuplicateKeySends()=%d, want 0 — INVARIANT VIOLATED", sink.DuplicateKeySends())
	}
	// One of the two must be a skip (the loser), the other a real send.
	if results[0] == results[1] {
		t.Fatalf("expected exactly one skip; got skipped=%v/%v", results[0], results[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// --- zero key: poison record skipped without DB I/O ---------------------------

func TestProcessSend_ZeroKey_Skips(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No DB expectations: a zero key must short-circuit before any query.
	sink := NewCountingSink()
	skipped, err := processSend(context.Background(), db, sink, "w1", time.Minute, SendCommand{})
	if err != nil {
		t.Fatalf("processSend: %v", err)
	}
	if !skipped {
		t.Fatal("zero key must be skipped")
	}
	if sink.Sends() != 0 {
		t.Fatalf("sink called for zero key; want 0")
	}
}

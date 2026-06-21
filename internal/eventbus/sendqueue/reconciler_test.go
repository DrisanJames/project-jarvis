package sendqueue

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// The commit pass is UNCHANGED: a stale 'claimed' row carrying a message_id is
// committed to 'sent' (the crash-window send already reached the wire). Assert
// the literal SQL still guards on message_id IS NOT NULL + status='claimed'.
func TestReconcileCommitSQL_CarriesGuards(t *testing.T) {
	if !regexp.MustCompile(`message_id IS NOT NULL`).MatchString(reconcileCommitSQL) {
		t.Fatal("commit SQL must guard on message_id IS NOT NULL")
	}
	if !regexp.MustCompile(`status = 'claimed'`).MatchString(reconcileCommitSQL) {
		t.Fatal("commit SQL must guard on status='claimed'")
	}
}

// GAP 1 — a stranded row gets its stored command RE-PRODUCED onto the topic and
// is moved to 'requeued' (NOT orphaned at terminal 'failed'). Uses a fake
// Producer to capture the re-produced record.
func TestReconcilePass_RedrivesStranded_NotOrphaned(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	key := uuid.New()
	cmdJSON := []byte(`{"idempotency_key":"` + key.String() + `","recipient":"u@example.com"}`)
	grace := 10 * time.Minute

	// (1) commit pass: no crash-window sends to commit.
	mock.ExpectExec(regexp.QuoteMeta("SET status = 'sent', sent_at = COALESCE(sent_at, NOW())")).
		WithArgs(grace.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// (2) select stranded: one row, well under the redrive cap, WITH a command.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key, command, attempts")).
		WithArgs(grace.String()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "command", "attempts"}).
			AddRow(key, cmdJSON, 1))

	// after a SUCCESSFUL re-produce, mark the row 'requeued' (not 'failed'/'dead').
	mock.ExpectExec(regexp.QuoteMeta("SET status = 'requeued'")).
		WithArgs(key).
		WillReturnResult(sqlmock.NewResult(0, 1))

	prod := &eventbus.FakeProducer{}
	reconcilePass(context.Background(), db, prod, TopicSendCommands, grace, DefaultMaxRedriveAttempts)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	// The command must have been re-produced onto the send topic, keyed by the
	// idempotency key.
	recs := prod.Records()
	if len(recs) != 1 {
		t.Fatalf("re-produced %d records, want exactly 1 (guaranteed re-drive)", len(recs))
	}
	if recs[0].Topic != TopicSendCommands {
		t.Fatalf("re-produced to topic %q, want %q", recs[0].Topic, TopicSendCommands)
	}
	gotKey := make([]byte, len(key))
	copy(gotKey, key[:])
	if string(recs[0].Key) != string(gotKey) {
		t.Fatal("re-produced record not keyed by idempotency_key bytes")
	}
	if string(recs[0].Value) != string(cmdJSON) {
		t.Fatalf("re-produced value=%s, want stored command", recs[0].Value)
	}
}

// GAP 1 — the attempt cap moves a row to terminal 'dead' after N re-drives so a
// poison command cannot loop forever. Here attempts(=cap) >= maxRedrives so the
// row is dead-lettered and NOT re-produced.
func TestReconcilePass_AttemptCap_DeadLetters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	key := uuid.New()
	cmdJSON := []byte(`{"idempotency_key":"` + key.String() + `"}`)
	grace := 10 * time.Minute
	const cap = 3

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'sent', sent_at = COALESCE(sent_at, NOW())")).
		WithArgs(grace.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// attempts == cap -> budget exhausted -> dead-letter, no re-produce.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key, command, attempts")).
		WithArgs(grace.String()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "command", "attempts"}).
			AddRow(key, cmdJSON, cap))

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'dead'")).
		WithArgs(key).
		WillReturnResult(sqlmock.NewResult(0, 1))

	prod := &eventbus.FakeProducer{}
	reconcilePass(context.Background(), db, prod, TopicSendCommands, grace, cap)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if n := prod.Count(); n != 0 {
		t.Fatalf("a capped/poison row was re-produced %d times; want 0 (must dead-letter)", n)
	}
}

// GAP 1 — a row whose stored command is NULL (legacy pre-GAP-1 row) cannot be
// re-produced; it is dead-lettered rather than orphaned at 'failed'.
func TestReconcilePass_NullCommand_DeadLetters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	key := uuid.New()
	grace := 10 * time.Minute

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'sent', sent_at = COALESCE(sent_at, NOW())")).
		WithArgs(grace.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key, command, attempts")).
		WithArgs(grace.String()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "command", "attempts"}).
			AddRow(key, nil, 0)) // NULL command

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'dead'")).
		WithArgs(key).
		WillReturnResult(sqlmock.NewResult(0, 1))

	prod := &eventbus.FakeProducer{}
	reconcilePass(context.Background(), db, prod, TopicSendCommands, grace, DefaultMaxRedriveAttempts)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if n := prod.Count(); n != 0 {
		t.Fatalf("null-command row re-produced %d times; want 0", n)
	}
}

// GAP 1 — if re-production FAILS (broker down), the row is left as-is (NOT
// requeued, NOT dead) so the next pass retries. Nothing is orphaned or lost.
func TestReconcilePass_ProduceFails_LeavesRowForRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	key := uuid.New()
	cmdJSON := []byte(`{"idempotency_key":"` + key.String() + `"}`)
	grace := 10 * time.Minute

	mock.ExpectExec(regexp.QuoteMeta("SET status = 'sent', sent_at = COALESCE(sent_at, NOW())")).
		WithArgs(grace.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key, command, attempts")).
		WithArgs(grace.String()).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "command", "attempts"}).
			AddRow(key, cmdJSON, 1))
	// NO requeue/dead UPDATE expected — produce failed, so the row is untouched.

	prod := &eventbus.FakeProducer{Err: context.DeadlineExceeded}
	reconcilePass(context.Background(), db, prod, TopicSendCommands, grace, DefaultMaxRedriveAttempts)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (a row was wrongly transitioned despite produce failure): %v", err)
	}
}

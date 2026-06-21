package sendqueue

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// queueInsertSQL must be the SAME shape the wave dispatcher INSERTs: same column
// list and ON CONFLICT (idempotency_key) DO NOTHING. If this drifts from the
// dispatcher, the byte-identical-row guarantee is broken.
func TestQueueInsertSQL_CarriesConflictGuardAndColumns(t *testing.T) {
	if !regexp.MustCompile(`INSERT INTO mailing_campaign_queue`).MatchString(queueInsertSQL) {
		t.Fatal("queueInsertSQL must INSERT INTO mailing_campaign_queue")
	}
	if !regexp.MustCompile(`ON CONFLICT \(idempotency_key\) WHERE idempotency_key IS NOT NULL DO NOTHING`).MatchString(queueInsertSQL) {
		t.Fatal("queueInsertSQL must carry the ON CONFLICT (idempotency_key) DO NOTHING guard")
	}
	for _, col := range []string{"campaign_id", "subscriber_id", "subject", "html_content", "plain_content",
		"isp_plan_id", "wave_id", "recipient_isp", "selection_rank", "audience_source_type",
		"audience_source_id", "idempotency_key", "content_snapshot_id"} {
		if !regexp.MustCompile(col).MatchString(queueInsertSQL) {
			t.Fatalf("queueInsertSQL missing column %q", col)
		}
	}
}

func sampleCmd() SendCommand {
	return SendCommand{
		IdempotencyKey:     uuid.New(),
		QueueID:            uuid.New(),
		CampaignID:         uuid.New(),
		SubscriberID:       uuid.New(),
		WaveID:             uuid.New(),
		ISPPlanID:          uuid.New(),
		RecipientISP:       "gmail",
		SelectionRank:      7,
		AudienceSourceType: "segment",
		AudienceSourceID:   uuid.New().String(),
		Subject:            "Hi",
		ContentSnapshotID:  uuid.New(),
		ScheduledAtUnix:    1_700_000_000,
		Priority:           5,
	}
}

// First delivery: the INSERT lands a new row (RowsAffected==1). Handle returns
// nil → the offset is committable.
func TestQueueWriter_FirstDelivery_Inserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	value, _ := json.Marshal(cmd)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err != nil {
		t.Fatalf("Handle returned error on first delivery: %v", err)
	}
	if ins, conf, fail := c.Stats(); ins != 1 || conf != 0 || fail != 0 {
		t.Fatalf("want inserted=1 conflicts=0 failed=0, got %d/%d/%d", ins, conf, fail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// REDELIVERY IDEMPOTENCE: the SAME command delivered twice. The second INSERT
// hits ON CONFLICT DO NOTHING (RowsAffected==0). Handle STILL returns nil (a
// conflict is the idempotent no-op of redelivery, i.e. success → commit), and no
// duplicate row is produced. This is the no-double-row invariant.
func TestQueueWriter_Redelivery_IdempotentNoDuplicate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	value, _ := json.Marshal(cmd)

	// Delivery 1: new row.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Delivery 2 (redelivery): ON CONFLICT → 0 rows affected.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err != nil {
		t.Fatalf("Handle err (delivery 1): %v", err)
	}
	if err := c.Handle(context.Background(), nil, value); err != nil {
		t.Fatalf("Handle err on REDELIVERY (must be a committable no-op): %v", err)
	}
	ins, conf, fail := c.Stats()
	if ins != 1 || conf != 1 || fail != 0 {
		t.Fatalf("redelivery: want inserted=1 conflicts=1 failed=0, got %d/%d/%d", ins, conf, fail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// A DB error on the INSERT must make Handle return the error so the framework
// does NOT commit the offset (the record redelivers → no drop).
func TestQueueWriter_DBError_ReturnsErrorNoCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	value, _ := json.Marshal(cmd)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnError(context.DeadlineExceeded)

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err == nil {
		t.Fatal("Handle must return an error on DB failure so the offset is NOT committed")
	}
	if _, _, fail := c.Stats(); fail != 1 {
		t.Fatalf("want failed=1, got %d", fail)
	}
}

// A zero idempotency_key is poison: Handle returns an error (routed to DLQ) and
// never INSERTs (no mock expectation set → any Exec would fail the test).
func TestQueueWriter_ZeroKey_IsPoison(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	cmd.IdempotencyKey = uuid.Nil
	value, _ := json.Marshal(cmd)

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err == nil {
		t.Fatal("zero idempotency_key must be poison (error → DLQ), not silently committed")
	}
}

// QueueInsertArgs applies the parity NULL semantics: empty html/plain → SQL NULL
// (the set-based path's NULL,NULL), zero content_snapshot/source → NULL, priority
// 0 → 5, zero queue id → a fresh non-nil id.
func TestQueueInsertArgs_NullSemantics(t *testing.T) {
	cmd := SendCommand{
		IdempotencyKey: uuid.New(),
		// HTMLContent / PlainContent empty → NULL; ContentSnapshotID nil → NULL;
		// AudienceSourceID "" → NULL; Priority 0 → 5; QueueID nil → fresh.
	}
	args := QueueInsertArgs(cmd)
	// $1 queue id (index 0) must be a non-nil uuid.
	if id, ok := args[0].(uuid.UUID); !ok || id == uuid.Nil {
		t.Fatalf("$1 queue id must be a fresh non-nil uuid, got %v", args[0])
	}
	// $5 html_content (index 4) must be NULL.
	if args[4] != nil {
		t.Fatalf("$5 html_content must be NULL for empty HTML, got %v", args[4])
	}
	// $13 plain_content (index 12) must be NULL.
	if args[12] != nil {
		t.Fatalf("$13 plain_content must be NULL for empty plain, got %v", args[12])
	}
	// $15 content_snapshot_id (index 14) must be NULL.
	if args[14] != nil {
		t.Fatalf("$15 content_snapshot_id must be NULL for zero snapshot, got %v", args[14])
	}
	// $12 audience_source_id (index 11) must be NULL.
	if args[11] != nil {
		t.Fatalf("$12 audience_source_id must be NULL for empty source, got %v", args[11])
	}
	// $16 priority (index 15) must default to 5.
	if args[15] != 5 {
		t.Fatalf("$16 priority must default to 5, got %v", args[15])
	}
}

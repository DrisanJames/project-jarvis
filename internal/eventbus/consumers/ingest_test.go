package consumers

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// The handler INSERTs with ON CONFLICT DO NOTHING and is idempotent on replay.
func TestIngestHandler_InsertsWithOnConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	campaign := uuid.NewString()
	sub := uuid.NewString()
	payload := mustJSON(t, ingestPayload{
		EventUID:     "ses:" + uuid.NewString(),
		CampaignID:   campaign,
		SubscriberID: sub,
		Email:        "user@example.com",
		EventType:    "open",
		ISPGroup:     "gmail",
		EventAt:      "2026-06-20T10:00:00Z",
	})

	// Expect the INSERT ... ON CONFLICT (id, event_at) DO NOTHING.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ingest_events_shadow")).
		WithArgs(
			sqlmock.AnyArg(), // id (deterministic uuid)
			campaign,
			sub,
			"user@example.com",
			"open",
			"gmail",
			sqlmock.AnyArg(), // event_at
			sqlmock.AnyArg(), // kafka_event_uid
			nil,              // kafka_offset
			nil,              // kafka_partition
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := NewIngestShadowConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Replaying the same event yields the SAME deterministic id, so the ON CONFLICT
// makes the second insert a no-op (RowsAffected 0) — the handler still returns
// nil. This proves idempotency at the dedup-key level.
func TestIngestHandler_IdempotentOnReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	uid := "pmta:job123:rcpt@x.com:b:2026-06-20T10:00:00Z"
	payload := mustJSON(t, ingestPayload{
		EventUID:  uid,
		Email:     "rcpt@x.com",
		EventType: "bounce",
		ISPGroup:  "yahoo",
		EventAt:   "2026-06-20T10:00:00Z",
	})

	wantID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(uid)).String()

	// First insert: a real row.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ingest_events_shadow")).
		WithArgs(wantID, nil, nil, "rcpt@x.com", "bounce", "yahoo",
			sqlmock.AnyArg(), uid, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Replay: conflict -> no-op (0 rows). Handler must still succeed.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ingest_events_shadow")).
		WithArgs(wantID, nil, nil, "rcpt@x.com", "bounce", "yahoo",
			sqlmock.AnyArg(), uid, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := NewIngestShadowConsumer(db, eventbus.Config{})
	for i := 0; i < 2; i++ {
		if err := c.Handle(context.Background(), nil, payload); err != nil {
			t.Fatalf("Handle replay %d: %v", i, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// A non-UUID campaign_id (PMTA JobID) must land as SQL NULL, not fail.
func TestIngestHandler_NonUUIDCampaignBecomesNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	payload := mustJSON(t, ingestPayload{
		EventUID:   "pmta:job-not-a-uuid:r:b:t",
		CampaignID: "job-not-a-uuid",
		Email:      "r@x.com",
		EventType:  "bounce",
		EventAt:    "2026-06-20T10:00:00Z",
	})

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO ingest_events_shadow")).
		WithArgs(sqlmock.AnyArg(), nil /*campaign NULL*/, nil, "r@x.com",
			"bounce", nil, sqlmock.AnyArg(), sqlmock.AnyArg(), nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := NewIngestShadowConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestIngestHandler_RejectsBadJSON(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	c := NewIngestShadowConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, []byte("{not json")); err == nil {
		t.Fatal("expected error on bad json")
	}
}

func TestIngestHandler_RejectsEmptyEventUID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	c := NewIngestShadowConsumer(db, eventbus.Config{})
	payload := mustJSON(t, ingestPayload{Email: "x@y.com", EventType: "open"})
	if err := c.Handle(context.Background(), nil, payload); err == nil {
		t.Fatal("expected error on empty event_uid")
	}
}

// Start is a no-op when the bus is disabled (no brokers) — dark-by-default.
func TestIngestStart_NoopWhenDisabled(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	c := NewIngestShadowConsumer(db, eventbus.Config{}) // no brokers
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start should no-op when disabled, got %v", err)
	}
	c.Stop() // safe when never started
}

package consumers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// ingestExecer is the tiny DB seam the ingest handler writes through, so the
// handler can be unit-tested with sqlmock (or any fake) and never needs a real
// *sql.DB. *sql.DB satisfies it.
type ingestExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ingestShadowInsert mirrors the live ingest dedup: ON CONFLICT (id, event_at)
// DO NOTHING. Reprocessing a Kafka partition (consumer-group reset, redelivery)
// is therefore a no-op and can't inflate parity counts.
const ingestShadowInsert = `
INSERT INTO ingest_events_shadow
    (id, campaign_id, subscriber_id, recipient, event_type, isp, event_at,
     kafka_event_uid, kafka_offset, kafka_partition)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id, event_at) DO NOTHING`

// ingestPayload is the JSON shape the ingest taps publish — a marshaled
// analytics.Event (handlers_ses_events.go / engine/ingest.go). We unmarshal
// only the fields the shadow table needs; the json tags MUST match the emitter.
type ingestPayload struct {
	EventUID     string `json:"event_uid"`
	CampaignID   string `json:"campaign_id"`
	SubscriberID string `json:"subscriber_id"`
	Email        string `json:"email"`
	EventType    string `json:"event_type"`
	ISPGroup     string `json:"isp_group"`
	EventAt      string `json:"event_at"`
}

// IngestShadowConsumer projects evt.ingest.v1 into ingest_events_shadow. It is
// idempotent on replay (ON CONFLICT) and at-least-once via the framework.
type IngestShadowConsumer struct {
	db  ingestExecer
	cfg eventbus.Config

	consumer *eventbus.Consumer
	dlqProd  eventbus.Producer
}

// NewIngestShadowConsumer constructs the consumer with its deps explicit. db is
// the shadow-table writer (a *sql.DB in prod, sqlmock/fake in tests). cfg
// supplies brokers/group/topics for Start; it can be the zero Config in handler
// unit tests (which call Handle directly and never Start).
func NewIngestShadowConsumer(db ingestExecer, cfg eventbus.Config) *IngestShadowConsumer {
	return &IngestShadowConsumer{db: db, cfg: cfg}
}

// Handle is the eventbus.Handler. key is unused (the row identity comes from the
// payload's deterministic event_uid). value is the marshaled analytics.Event.
//
// The shadow row id is a deterministic UUID derived from the event_uid (which
// is itself deterministic per live row: "ses:<uuid>" / "pmta:<jobid:rcpt:type:
// time>"). Pairing it with event_at reproduces the live PRIMARY KEY (id,
// event_at) dedup so a Kafka replay collapses to a no-op.
func (c *IngestShadowConsumer) Handle(ctx context.Context, _, value []byte) error {
	var p ingestPayload
	if err := json.Unmarshal(value, &p); err != nil {
		return fmt.Errorf("ingest-shadow: unmarshal: %w", err)
	}
	if p.EventUID == "" {
		return fmt.Errorf("ingest-shadow: empty event_uid")
	}

	eventAt := parseShadowTime(p.EventAt)
	// Deterministic, replay-stable row id from the deterministic event_uid.
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(p.EventUID))

	_, err := c.db.ExecContext(ctx, ingestShadowInsert,
		id,                         // id
		nullUUID(p.CampaignID),     // campaign_id
		nullUUID(p.SubscriberID),   // subscriber_id
		nullStr(p.Email),           // recipient
		p.EventType,                // event_type
		nullStr(p.ISPGroup),        // isp
		eventAt,                    // event_at
		nullStr(p.EventUID),        // kafka_event_uid
		nil,                        // kafka_offset (not surfaced by the Handler seam)
		nil,                        // kafka_partition
	)
	if err != nil {
		return fmt.Errorf("ingest-shadow: insert: %w", err)
	}
	return nil
}

// Start constructs the real group consumer and runs its loop in a goroutine.
// It is a no-op (returns nil) when the bus is disabled (no brokers) — the
// dark-by-default path. The DLQ parks dead records on evt.ingest.v1.dlq.
func (c *IngestShadowConsumer) Start(ctx context.Context) error {
	if !c.cfg.Enabled() {
		return nil
	}
	cfg := c.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicIngest}
	}

	prod, err := eventbus.NewKgoProducer(cfg)
	if err != nil {
		return fmt.Errorf("ingest-shadow: dlq producer: %w", err)
	}
	c.dlqProd = prod

	con, err := eventbus.NewConsumer(cfg, c.Handle, NewProducerDLQ(prod), eventbus.ConsumerOptions{})
	if err != nil {
		_ = prod.Close()
		return fmt.Errorf("ingest-shadow: consumer: %w", err)
	}
	c.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer and its DLQ producer. Safe to call when never
// started.
func (c *IngestShadowConsumer) Stop() {
	if c.consumer != nil {
		_ = c.consumer.Close()
	}
	if c.dlqProd != nil {
		_ = c.dlqProd.Close()
	}
}

// parseShadowTime parses the emitter's event_at (RFC3339), falling back to now.
func parseShadowTime(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// nullStr returns a *string that is NULL when empty, so empty payload fields map
// to SQL NULL rather than "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullUUID returns the string when it parses as a UUID, else nil (SQL NULL).
// campaign_id/subscriber_id are UUID columns; PMTA ingest carries a non-UUID
// JobID as campaign_id, which must land as NULL rather than fail the insert.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	if _, err := uuid.Parse(s); err != nil {
		return nil
	}
	return s
}

package consumers

// dataingest.go is the counters consumer for the Data Ingest dashboard
// (REQ 2026-09-20, U3). It reads data.ingest.v1 and applies each operation
// event to Redis via internal/dataingest.Counters.
//
// Modeled EXACTLY on ingest.go: explicit deps in the constructor, a Handle
// suitable for eventbus.Consumer (unit-testable with miniredis and NO broker),
// Start/Stop that build the real group consumer with a DLQ on
// "data.ingest.v1.dlq", and idempotency AT THE SINK.
//
// Idempotency lives in Counters.Apply (SETNX di:op:<op_id>, 72h), so a Kafka
// replay or a consumer-group reset re-applies nothing. Handle returns nil for a
// duplicate — the offset may commit; the work was already done.
//
// A payload that fails Validate is a PERMANENT failure: retrying it cannot make
// it valid, so it goes to the DLQ rather than blocking the partition. A Redis
// error is TRANSIENT and is returned as-is, so the framework retries and, only
// after its bounded retries, parks the record.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// DataIngestCountersConsumer projects data.ingest.v1 into the Redis counters.
type DataIngestCountersConsumer struct {
	counters *dataingest.Counters
	cfg      eventbus.Config

	consumer *eventbus.Consumer
	dlqProd  eventbus.Producer

	applied    atomic.Uint64
	duplicates atomic.Uint64
	failed     atomic.Uint64

	statHook func(applied, duplicates, failed uint64)
}

// NewDataIngestCountersConsumer constructs the consumer. rdb is the counters'
// Redis client — a NIL client yields a consumer whose Handle returns
// dataingest.ErrNoRedis for every record, which is why the boot wiring only
// starts it when rdb != nil. cfg supplies brokers/group/topic for Start and can
// be the zero Config in handler unit tests (which call Handle directly).
func NewDataIngestCountersConsumer(rdb *redis.Client, cfg eventbus.Config) *DataIngestCountersConsumer {
	return &DataIngestCountersConsumer{
		counters: dataingest.NewCounters(rdb).WithHub(dataingest.DefaultHub()),
		cfg:      cfg,
	}
}

// WithStatHook registers a callback fired after every record with the running
// totals, so /health can render applied/duplicates/failed without reaching into
// the consumer. Mirrors sendqueue.QueueWriterConsumer.WithStatHook.
func (c *DataIngestCountersConsumer) WithStatHook(fn func(applied, duplicates, failed uint64)) *DataIngestCountersConsumer {
	c.statHook = fn
	return c
}

// Handle is the eventbus.Handler. key is unused — the identity of an operation
// is its op_id inside the payload, never the partition key.
func (c *DataIngestCountersConsumer) Handle(ctx context.Context, _, value []byte) error {
	var ev dataingest.Event
	if err := json.Unmarshal(value, &ev); err != nil {
		c.bump(&c.failed)
		return fmt.Errorf("data-ingest-counters: unmarshal: %w", err)
	}
	ev.Normalize()
	if err := ev.Validate(); err != nil {
		c.bump(&c.failed)
		return fmt.Errorf("data-ingest-counters: invalid event: %w", err)
	}
	applied, err := c.counters.Apply(ctx, ev)
	if err != nil {
		c.bump(&c.failed)
		return fmt.Errorf("data-ingest-counters: apply: %w", err)
	}
	if applied {
		c.bump(&c.applied)
	} else {
		c.bump(&c.duplicates)
	}
	return nil
}

func (c *DataIngestCountersConsumer) bump(ctr *atomic.Uint64) {
	ctr.Add(1)
	if c.statHook != nil {
		c.statHook(c.applied.Load(), c.duplicates.Load(), c.failed.Load())
	}
}

// Stats returns the lifetime counters for this task.
func (c *DataIngestCountersConsumer) Stats() (applied, duplicates, failed uint64) {
	return c.applied.Load(), c.duplicates.Load(), c.failed.Load()
}

// Snapshot is the LIVENESS reading (not a boot boolean — see eventbus/health.go
// for why that distinction exists). Returns the never-ran zero when Start has
// not built a consumer.
func (c *DataIngestCountersConsumer) Snapshot() eventbus.ConsumerSnapshot {
	if c == nil || c.consumer == nil {
		return eventbus.ConsumerSnapshot{Name: "data-ingest-counters", TaskID: eventbus.TaskID()}
	}
	return c.consumer.Snapshot()
}

// Start constructs the real group consumer and runs its loop in a goroutine.
// No-op (nil) when the bus is disabled — the dark-by-default path.
func (c *DataIngestCountersConsumer) Start(ctx context.Context) error {
	if !c.cfg.Enabled() {
		return nil
	}
	cfg := c.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicDataIngest}
	}

	prod, err := eventbus.NewKgoProducer(cfg)
	if err != nil {
		return fmt.Errorf("data-ingest-counters: dlq producer: %w", err)
	}
	c.dlqProd = prod

	con, err := eventbus.NewConsumer(cfg, c.Handle, NewProducerDLQ(prod), eventbus.ConsumerOptions{})
	if err != nil {
		_ = prod.Close()
		return fmt.Errorf("data-ingest-counters: consumer: %w", err)
	}
	c.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer and its DLQ producer. Safe when never started.
func (c *DataIngestCountersConsumer) Stop() {
	if c.consumer != nil {
		_ = c.consumer.Close()
	}
	if c.dlqProd != nil {
		_ = c.dlqProd.Close()
	}
}

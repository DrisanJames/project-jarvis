package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Handler processes one record. Returning nil means the record is handled and
// its offset may be committed. Returning an error triggers bounded retries and,
// if still failing, the DLQ. A panic inside Handler is recovered per-record and
// treated like an error (so one poison record can never crash the consumer).
type Handler func(ctx context.Context, key, value []byte) error

// DLQ receives records that a Handler could not process after all retries (or
// that panicked on every attempt). Implementations must NOT lose the record —
// the plan's contract is "do not lose the record": persist it somewhere durable
// (a *.dlq topic, a table, etc.). The error is the last failure observed.
type DLQ interface {
	Dead(ctx context.Context, topic string, key, value []byte, cause error) error
}

// DLQFunc adapts a function to the DLQ interface.
type DLQFunc func(ctx context.Context, topic string, key, value []byte, cause error) error

// Dead implements DLQ.
func (f DLQFunc) Dead(ctx context.Context, topic string, key, value []byte, cause error) error {
	return f(ctx, topic, key, value, cause)
}

// ConsumerOptions tune the at-least-once processing loop. Zero values pick sane
// defaults.
type ConsumerOptions struct {
	// MaxRetries is the number of ADDITIONAL attempts after the first failure
	// before a record goes to the DLQ. 0 => 3.
	MaxRetries int
	// RetryBackoff is the wait between attempts. 0 => 200ms.
	RetryBackoff time.Duration
}

func (o ConsumerOptions) maxRetries() int {
	if o.MaxRetries > 0 {
		return o.MaxRetries
	}
	return 3
}

func (o ConsumerOptions) retryBackoff() time.Duration {
	if o.RetryBackoff > 0 {
		return o.RetryBackoff
	}
	return 200 * time.Millisecond
}

// errPanic wraps a recovered panic so the DLQ cause is informative.
type errPanic struct{ v any }

func (e errPanic) Error() string { return fmt.Sprintf("handler panic: %v", e.v) }

// processRecord runs the handler with at-least-once semantics for ONE record:
// per-record panic recovery, bounded retries, and DLQ on permanent failure. It
// is broker-free and is the unit-tested core of the consumer. It returns nil
// once the record is either handled OR durably sent to the DLQ (i.e. the offset
// is safe to commit); it returns a non-nil error ONLY if the DLQ itself failed,
// in which case the caller must NOT commit (so the record is redelivered rather
// than lost).
func processRecord(ctx context.Context, h Handler, dlq DLQ, opts ConsumerOptions, topic string, key, value []byte) error {
	attempts := opts.maxRetries() + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := safeHandle(ctx, h, key, value); err != nil {
			lastErr = err
			if i < attempts-1 {
				select {
				case <-ctx.Done():
					lastErr = ctx.Err()
					i = attempts // break out; go to DLQ with ctx error
				case <-time.After(opts.retryBackoff()):
				}
			}
			continue
		}
		return nil // handled successfully -> commit
	}

	// Permanent failure: do not lose the record.
	if dlq == nil {
		// No DLQ configured. We must still not lose the record: refuse to commit
		// by returning the error so the loop re-delivers. (A consumer wired
		// without a DLQ should be rare; the plan always pairs one.)
		return fmt.Errorf("eventbus: handler failed and no DLQ configured: %w", lastErr)
	}
	if err := dlq.Dead(ctx, topic, key, value, lastErr); err != nil {
		// DLQ write failed too -> do NOT commit; let redelivery retry.
		return fmt.Errorf("eventbus: DLQ write failed (record retained): %w", err)
	}
	return nil // record is durably parked in the DLQ -> safe to commit
}

// safeHandle invokes the handler with per-call panic recovery.
func safeHandle(ctx context.Context, h Handler, key, value []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errPanic{v: r}
		}
	}()
	return h(ctx, key, value)
}

// Consumer joins a consumer group and processes records at-least-once. The
// franz-go client is constructed only here (from a broker list), but all
// per-record semantics live in processRecord, which is tested without a broker.
type Consumer struct {
	client  *kgo.Client
	handler Handler
	dlq     DLQ
	opts    ConsumerOptions
}

// NewConsumer constructs the real group consumer. It errors when disabled (no
// brokers), no group, or no topics — callers gate on cfg.Enabled() and the
// per-flow FlagGate first, so a dark deploy never constructs one.
//
// Offsets are committed MANUALLY, only after a record is successfully handled
// (or durably parked in the DLQ) — see Run. AutoCommit is disabled so a crash
// re-delivers in-flight records rather than silently advancing past them.
func NewConsumer(cfg Config, handler Handler, dlq DLQ, opts ConsumerOptions) (*Consumer, error) {
	if !cfg.Enabled() {
		return nil, errors.New("eventbus: NewConsumer called with no brokers (disabled)")
	}
	if cfg.Group == "" {
		return nil, errors.New("eventbus: NewConsumer requires a consumer group")
	}
	if len(cfg.Topics) == 0 {
		return nil, errors.New("eventbus: NewConsumer requires at least one topic")
	}
	if handler == nil {
		return nil, errors.New("eventbus: NewConsumer requires a handler")
	}

	o := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topics...),
		// Manual commit: we commit only after handling. franz-go's
		// DisableAutoCommit + CommitRecords gives at-least-once.
		kgo.DisableAutoCommit(),
	}
	// Auth + TLS (incl. AWS MSK IAM-SASL) — same posture as the producer (sasl.go).
	auth, err := authOpts(cfg)
	if err != nil {
		return nil, err
	}
	o = append(o, auth...)
	client, err := kgo.NewClient(o...)
	if err != nil {
		return nil, fmt.Errorf("eventbus: kgo.NewClient (consumer): %w", err)
	}
	return &Consumer{client: client, handler: handler, dlq: dlq, opts: opts}, nil
}

// Run polls and processes records until ctx is cancelled. Each successfully
// handled (or DLQ-parked) record's offset is committed; a record whose DLQ
// write also fails is NOT committed and will be redelivered. Panics in the
// handler are isolated per-record.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fetches := c.client.PollFetches(ctx)
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				if errors.Is(e.Err, context.Canceled) {
					return ctx.Err()
				}
				log.Printf("[eventbus-consumer] fetch error topic=%s partition=%d: %v", e.Topic, e.Partition, e.Err)
			}
		}
		iter := fetches.RecordIter()
		for !iter.Done() {
			rec := iter.Next()
			if err := processRecord(ctx, c.handler, c.dlq, c.opts, rec.Topic, rec.Key, rec.Value); err != nil {
				// Record retained (handler+DLQ both failed): skip commit so it
				// is redelivered. Do not lose it.
				log.Printf("[eventbus-consumer] record retained (uncommitted) topic=%s: %v", rec.Topic, err)
				continue
			}
			// Commit AFTER successful handle / durable DLQ park.
			if err := c.client.CommitRecords(ctx, rec); err != nil {
				log.Printf("[eventbus-consumer] commit error topic=%s: %v", rec.Topic, err)
			}
		}
	}
}

// Close releases the client.
func (c *Consumer) Close() error {
	if c.client == nil {
		return nil
	}
	c.client.Close()
	return nil
}

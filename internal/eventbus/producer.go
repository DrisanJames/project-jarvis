package eventbus

import (
	"context"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Producer is the minimal record-publishing seam. Everything in this package
// (and every future hot-path tap) depends on this interface, never on a
// concrete client, so it is fully unit-testable without a broker.
type Producer interface {
	// Produce publishes one record to topic. key may be nil (partition is then
	// chosen by the client's partitioner); a non-nil key pins ordering for that
	// key. Produce should be treated as potentially blocking I/O — hot-path
	// callers must go through a Tap, never call this directly.
	Produce(ctx context.Context, topic string, key, value []byte) error

	// Close flushes and releases client resources. Safe to call once.
	Close() error
}

// BatchProducer is the OPTIONAL batching extension of Producer: one broker
// round trip for many records instead of one per record. Callers type-assert
// for it and fall back to a Produce loop when the concrete producer does not
// implement it (eventbus.FakeProducer, for instance, does not — its per-record
// capture is what the unit tests assert on).
//
// It exists for the SK-4 wave route (REQ-089): a routed wave hands its whole
// recipient set over at once, and N sequential ProduceSync calls at ~1-3ms each
// is minutes of wall clock for a 45k-recipient wave.
type BatchProducer interface {
	Producer

	// ProduceBatch publishes len(values) records to topic in ONE flush. keys
	// must be the same length as values (keys[i] may be nil). It returns the
	// first error; partial success is possible, which is safe for the send
	// path because every command is idempotent on its key.
	ProduceBatch(ctx context.Context, topic string, keys, values [][]byte) error
}

// kgoProducer is the franz-go-backed Producer. It is configured as an
// idempotent producer (acks=all to all in-sync replicas) so duplicate-free,
// ordered delivery per partition is guaranteed at the broker. Idempotency is
// franz-go's default and is asserted explicitly below for clarity.
type kgoProducer struct {
	client *kgo.Client
}

// compile-time assertion: kgoProducer satisfies Producer.
var _ Producer = (*kgoProducer)(nil)

// NewKgoProducer constructs the real franz-go producer from Config. It returns
// an error if no brokers are configured (callers should check cfg.Enabled()
// first and skip construction when disabled — that is the dark-by-default path).
//
// Idempotence: franz-go enables the idempotent producer by default. We
// additionally pin RequiredAcks to AllISRAcks() (acks=all) so a record is only
// acknowledged once written to every in-sync replica — the durability posture
// the backbone plan specifies (min.insync.replicas=2, acks=all).
//
// AWS MSK IAM-SASL hook (left as a config option, NOT hardcoded): when
// cfg.SASLMechanism == "aws_msk_iam", wire the franz-go option
//
//	import "github.com/twmb/franz-go/pkg/sasl/aws"
//	opts = append(opts, kgo.SASL(aws.ManagedStreamingIAM(func(ctx context.Context) (aws.Auth, error) {
//	    // resolve from the ECS task role via the default AWS credential chain;
//	    // never read static creds from Config.
//	})))
//
// That dependency is intentionally NOT imported here so the dark package stays
// minimal; document-and-defer until the producer is actually wired in prod.
func NewKgoProducer(cfg Config) (Producer, error) {
	if !cfg.Enabled() {
		return nil, errors.New("eventbus: NewKgoProducer called with no brokers (disabled)")
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		// Idempotent, durable producer: acks from all in-sync replicas.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// AllISRAcks requires idempotency to be ON (franz-go default). We do NOT
		// call DisableIdempotentWrite, so the idempotent producer is active.
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
	}

	if cfg.AllowAutoTopics {
		opts = append(opts, kgo.AllowAutoTopicCreation())
	}

	// Auth + TLS (incl. AWS MSK IAM-SASL) — see sasl.go.
	auth, err := authOpts(cfg)
	if err != nil {
		return nil, err
	}
	opts = append(opts, auth...)

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("eventbus: kgo.NewClient: %w", err)
	}
	return &kgoProducer{client: client}, nil
}

// Produce publishes one record synchronously (waits for the broker ack). The
// idempotent producer guarantees no duplicates and per-key ordering. Hot-path
// callers must use a Tap so this blocking call never touches ingestion latency.
func (p *kgoProducer) Produce(ctx context.Context, topic string, key, value []byte) error {
	rec := &kgo.Record{Topic: topic, Key: key, Value: value}
	return p.client.ProduceSync(ctx, rec).FirstErr()
}

// ProduceBatch publishes many records in ONE ProduceSync: franz-go batches them
// into as few broker requests as the partitioner allows and waits for all acks.
// Same durability posture as Produce (idempotent producer, acks=all).
func (p *kgoProducer) ProduceBatch(ctx context.Context, topic string, keys, values [][]byte) error {
	if len(values) == 0 {
		return nil
	}
	if len(keys) != len(values) {
		return fmt.Errorf("eventbus: ProduceBatch keys/values length mismatch (%d vs %d)", len(keys), len(values))
	}
	recs := make([]*kgo.Record, 0, len(values))
	for i := range values {
		recs = append(recs, &kgo.Record{Topic: topic, Key: keys[i], Value: values[i]})
	}
	return p.client.ProduceSync(ctx, recs...).FirstErr()
}

// compile-time assertion: the real producer implements the batch extension.
var _ BatchProducer = (*kgoProducer)(nil)

// Close flushes buffered records and closes the underlying client.
func (p *kgoProducer) Close() error {
	if p.client == nil {
		return nil
	}
	p.client.Close()
	return nil
}

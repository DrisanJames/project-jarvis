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

// Close flushes buffered records and closes the underlying client.
func (p *kgoProducer) Close() error {
	if p.client == nil {
		return nil
	}
	p.client.Close()
	return nil
}

package consumers

import (
	"context"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// EnvShadowFirehoseStream names the SHADOW Firehose delivery stream the lake
// sink re-emits to. When unset, the sink is a no-op (dark default). Parity
// compares the kafka-shadow lake partition against the live lake partition.
const EnvShadowFirehoseStream = "ANALYTICS_FIREHOSE_STREAM_SHADOW"

// firehosePutter is the seam the lake sink writes through, so the handler is
// unit-testable with a fake (no AWS). *firehose.Client satisfies it.
type firehosePutter interface {
	PutRecordBatch(ctx context.Context, in *firehose.PutRecordBatchInput, optFns ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error)
}

// LakeShadowSink re-emits each consumed evt.lake.v1 record to a SHADOW Firehose
// stream. The live lake (Firehose -> S3/Glue ignite_analytics.email_events)
// dedups downstream on event_uid, so at-least-once replay is safe. The sink is
// a NO-OP unless ANALYTICS_FIREHOSE_STREAM_SHADOW is set — keeps it dark.
type LakeShadowSink struct {
	cfg    eventbus.Config
	stream string
	fh     firehosePutter

	consumer *eventbus.Consumer
	dlqProd  eventbus.Producer
}

// NewLakeShadowSink constructs the sink. stream is the shadow Firehose stream
// name (typically os.Getenv(EnvShadowFirehoseStream)); an empty stream makes
// the sink a permanent no-op. fh is the Firehose client; pass nil to have Start
// build a real one from the default AWS config (region falls back to the chain).
// In tests, pass a fake firehosePutter and a non-empty stream to exercise the
// put path, or an empty stream to assert the no-op.
func NewLakeShadowSink(cfg eventbus.Config, stream string, fh firehosePutter) *LakeShadowSink {
	return &LakeShadowSink{cfg: cfg, stream: stream, fh: fh}
}

// NewLakeShadowSinkFromEnv is the convenience constructor SA-6 uses: it reads
// the shadow stream name from EnvShadowFirehoseStream. If unset, the returned
// sink is a no-op.
func NewLakeShadowSinkFromEnv(cfg eventbus.Config) *LakeShadowSink {
	return NewLakeShadowSink(cfg, os.Getenv(EnvShadowFirehoseStream), nil)
}

// Enabled reports whether the sink will actually emit (shadow stream set).
func (s *LakeShadowSink) Enabled() bool { return s.stream != "" }

// Handle is the eventbus.Handler. value is the raw lake-event JSON the producer
// tapped (PublishLake publishes the clean Firehose JSON, no trailing newline).
// We re-emit it to the shadow stream as-is, appending the newline Firehose
// expects between JSON records — byte-for-byte parity with the live emitter so
// the shadow partition is directly comparable.
//
// No-op when the shadow stream is unset (dark default). At-least-once: the lake
// dedups on event_uid downstream, so replays collapse.
func (s *LakeShadowSink) Handle(ctx context.Context, _, value []byte) error {
	if s.stream == "" {
		return nil // dark: shadow stream unset -> no-op
	}
	if len(value) == 0 {
		return nil
	}
	if s.fh == nil {
		return fmt.Errorf("lake-shadow: no firehose client")
	}
	// Mirror lake_emitter.flush: one record carries the JSON + trailing newline.
	data := make([]byte, 0, len(value)+1)
	data = append(data, value...)
	data = append(data, '\n')

	if err := s.putBatch(ctx, []fhtypes.Record{{Data: data}}); err != nil {
		return fmt.Errorf("lake-shadow: put: %w", err)
	}
	return nil
}

// putBatch is a thin local mirror of analytics.Emitter.flush's Firehose path
// (the live helper is unexported and bound to the global Emitter): PutRecordBatch
// with a single retry for the failed subset. Batch size honors the lake's 500
// cap, though the per-record Handler emits one record at a time.
func (s *LakeShadowSink) putBatch(ctx context.Context, recs []fhtypes.Record) error {
	if len(recs) == 0 {
		return nil
	}
	if len(recs) > maxShadowBatch {
		recs = recs[:maxShadowBatch]
	}
	out, err := s.fh.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
		DeliveryStreamName: &s.stream,
		Records:            recs,
	})
	if err != nil {
		return err
	}
	if out.FailedPutCount == nil || *out.FailedPutCount == 0 {
		return nil
	}
	// One retry for the failed subset (mirrors lake_emitter).
	var retry []fhtypes.Record
	for i, rr := range out.RequestResponses {
		if i < len(recs) && rr.ErrorCode != nil {
			retry = append(retry, recs[i])
		}
	}
	if len(retry) == 0 {
		return nil
	}
	out2, err := s.fh.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
		DeliveryStreamName: &s.stream,
		Records:            retry,
	})
	if err != nil {
		return err
	}
	if out2.FailedPutCount != nil && *out2.FailedPutCount > 0 {
		return fmt.Errorf("lake-shadow: %d records still failed after retry", *out2.FailedPutCount)
	}
	return nil
}

// maxShadowBatch matches the live lake's PutRecordBatch cap.
const maxShadowBatch = 500

// Start constructs the real group consumer and runs it. It is a no-op when the
// bus is disabled (no brokers) OR when the shadow stream is unset — both keep it
// dark. When enabled and fh is nil, it builds a real Firehose client from the
// default AWS config. DLQ parks dead records on evt.lake.v1.dlq.
func (s *LakeShadowSink) Start(ctx context.Context) error {
	if !s.cfg.Enabled() || s.stream == "" {
		return nil
	}
	if s.fh == nil {
		awscfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("lake-shadow: aws config: %w", err)
		}
		s.fh = firehose.NewFromConfig(awscfg)
	}

	cfg := s.cfg
	if len(cfg.Topics) == 0 {
		cfg.Topics = []string{eventbus.TopicLake}
	}

	prod, err := eventbus.NewKgoProducer(cfg)
	if err != nil {
		return fmt.Errorf("lake-shadow: dlq producer: %w", err)
	}
	s.dlqProd = prod

	con, err := eventbus.NewConsumer(cfg, s.Handle, NewProducerDLQ(prod), eventbus.ConsumerOptions{})
	if err != nil {
		_ = prod.Close()
		return fmt.Errorf("lake-shadow: consumer: %w", err)
	}
	s.consumer = con
	go func() { _ = con.Run(ctx) }()
	return nil
}

// Stop closes the consumer and its DLQ producer. Safe when never started.
func (s *LakeShadowSink) Stop() {
	if s.consumer != nil {
		_ = s.consumer.Close()
	}
	if s.dlqProd != nil {
		_ = s.dlqProd.Close()
	}
}

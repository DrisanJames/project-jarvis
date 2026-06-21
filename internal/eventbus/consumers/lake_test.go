package consumers

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// fakeFirehose records PutRecordBatch calls and returns scripted outputs.
type fakeFirehose struct {
	calls   int
	lastIn  *firehose.PutRecordBatchInput
	outputs []*firehose.PutRecordBatchOutput
	err     error
}

func (f *fakeFirehose) PutRecordBatch(_ context.Context, in *firehose.PutRecordBatchInput, _ ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error) {
	f.lastIn = in
	idx := f.calls
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if idx < len(f.outputs) {
		return f.outputs[idx], nil
	}
	zero := int32(0)
	return &firehose.PutRecordBatchOutput{FailedPutCount: &zero}, nil
}

func i32(v int32) *int32 { return &v }

// No-op when the shadow stream is unset: nothing is emitted to Firehose.
func TestLakeSink_NoopWhenStreamUnset(t *testing.T) {
	fh := &fakeFirehose{}
	s := NewLakeShadowSink(eventbus.Config{}, "" /*no stream*/, fh)
	if s.Enabled() {
		t.Fatal("Enabled() should be false with no stream")
	}
	if err := s.Handle(context.Background(), nil, []byte(`{"event_uid":"ses:abc"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fh.calls != 0 {
		t.Fatalf("expected 0 firehose calls when stream unset, got %d", fh.calls)
	}
}

// With a stream set, the consumed JSON is re-emitted with a trailing newline.
func TestLakeSink_ReemitsWithNewline(t *testing.T) {
	fh := &fakeFirehose{}
	s := NewLakeShadowSink(eventbus.Config{}, "ignite-email-events-shadow", fh)
	if !s.Enabled() {
		t.Fatal("Enabled() should be true with a stream")
	}
	body := []byte(`{"event_uid":"ses:abc","email":"x@y.com"}`)
	if err := s.Handle(context.Background(), nil, body); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fh.calls != 1 {
		t.Fatalf("expected 1 firehose call, got %d", fh.calls)
	}
	if *fh.lastIn.DeliveryStreamName != "ignite-email-events-shadow" {
		t.Fatalf("wrong stream: %q", *fh.lastIn.DeliveryStreamName)
	}
	if len(fh.lastIn.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(fh.lastIn.Records))
	}
	got := string(fh.lastIn.Records[0].Data)
	if got != string(body)+"\n" {
		t.Fatalf("record data = %q, want JSON + newline", got)
	}
}

// Empty value is a no-op (no Firehose call).
func TestLakeSink_EmptyValueNoop(t *testing.T) {
	fh := &fakeFirehose{}
	s := NewLakeShadowSink(eventbus.Config{}, "stream", fh)
	if err := s.Handle(context.Background(), nil, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fh.calls != 0 {
		t.Fatalf("expected 0 calls on empty value, got %d", fh.calls)
	}
}

// A partial failure triggers exactly one retry of the failed subset.
func TestLakeSink_RetriesFailedSubset(t *testing.T) {
	fh := &fakeFirehose{
		outputs: []*firehose.PutRecordBatchOutput{
			{ // first call: one record failed
				FailedPutCount: i32(1),
				RequestResponses: []fhtypes.PutRecordBatchResponseEntry{
					{ErrorCode: stringp("ServiceUnavailable")},
				},
			},
			{ // retry succeeds
				FailedPutCount: i32(0),
			},
		},
	}
	s := NewLakeShadowSink(eventbus.Config{}, "stream", fh)
	if err := s.Handle(context.Background(), nil, []byte(`{"event_uid":"x"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if fh.calls != 2 {
		t.Fatalf("expected 2 calls (put + retry), got %d", fh.calls)
	}
}

// A hard PutRecordBatch error surfaces so the framework can retry/DLQ.
func TestLakeSink_PutErrorSurfaces(t *testing.T) {
	fh := &fakeFirehose{err: errors.New("boom")}
	s := NewLakeShadowSink(eventbus.Config{}, "stream", fh)
	if err := s.Handle(context.Background(), nil, []byte(`{"x":1}`)); err == nil {
		t.Fatal("expected error to surface")
	}
}

func TestLakeStart_NoopWhenDisabledOrNoStream(t *testing.T) {
	// disabled bus
	s1 := NewLakeShadowSink(eventbus.Config{}, "stream", &fakeFirehose{})
	if err := s1.Start(context.Background()); err != nil {
		t.Fatalf("Start should no-op when bus disabled, got %v", err)
	}
	s1.Stop()

	// enabled-looking bus but no stream
	s2 := NewLakeShadowSink(eventbus.Config{Brokers: []string{"b:9092"}}, "", &fakeFirehose{})
	if err := s2.Start(context.Background()); err != nil {
		t.Fatalf("Start should no-op when stream unset, got %v", err)
	}
	s2.Stop()
}

func stringp(s string) *string { return &s }

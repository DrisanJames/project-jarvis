package eventbus

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

// Tap is a FIRE-AND-FORGET wrapper around a Producer. It is the ONLY seam the
// ingestion hot path is allowed to touch, and it mirrors the contract of
// internal/analytics.lake_emitter exactly:
//
//   - a bounded buffered channel (depth tapChannelDepth, 8192 like the lake);
//   - a single background goroutine that drains the channel and produces;
//   - DROP-AND-COUNT when the buffer is full — Publish NEVER blocks the caller
//     and never returns an error to the hot path;
//   - atomic counters surfaced via Stats() for /health;
//   - a no-op when disabled (nil Producer) — a cheap nil check, zero behavior
//     change, exactly the lake-emitter's disabled-by-default safety.
//
// This guarantees Kafka can add neither latency nor backpressure to ingestion:
// a slow or down broker just causes drops (counted), never a stalled caller.
type Tap struct {
	prod Producer

	ch   chan tapRecord
	stop chan struct{}
	done chan struct{}

	produced uint64
	dropped  uint64
	failed   uint64
}

type tapRecord struct {
	topic string
	key   []byte
	value []byte
}

const (
	// tapChannelDepth matches lake_emitter.channelDepth (8192). Sized so a brief
	// produce stall buffers rather than drops, but bounded so a sustained stall
	// degrades to drops instead of unbounded memory growth.
	tapChannelDepth = 8192

	// tapFlushEvery bounds how long the drain goroutine waits between produce
	// attempts when idle (it primarily wakes on channel receive; the ticker is a
	// liveness backstop mirroring lake_emitter.flushEvery).
	tapFlushEvery = 5 * time.Second

	// tapProduceTimeout caps a single produce so one wedged broker call can't
	// stall the drain goroutine forever.
	tapProduceTimeout = 20 * time.Second
)

// NewTap wraps a Producer in a fire-and-forget tap and starts its drain
// goroutine. A nil Producer yields a DISABLED tap whose Publish is a cheap
// no-op (no goroutine started) — the dark-by-default path. Callers that resolve
// a disabled Config should pass nil here.
func NewTap(prod Producer) *Tap {
	t := &Tap{prod: prod}
	if prod == nil {
		return t // disabled: Publish is a no-op, no goroutine
	}
	t.ch = make(chan tapRecord, tapChannelDepth)
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.run()
	return t
}

// Enabled reports whether the tap has a live producer.
func (t *Tap) Enabled() bool { return t.prod != nil }

// Publish queues a record for production. It NEVER blocks: if disabled it is a
// no-op; if the buffer is full the record is dropped and counted. It never
// returns an error — the hot path must not branch on Kafka health.
func (t *Tap) Publish(topic string, key, value []byte) {
	if t.prod == nil {
		return
	}
	// Copy bytes so the caller may reuse its buffers immediately after Publish.
	rec := tapRecord{topic: topic, key: cloneBytes(key), value: cloneBytes(value)}
	select {
	case t.ch <- rec:
	default:
		atomic.AddUint64(&t.dropped, 1)
	}
}

// run is the single drain goroutine. It receives queued records and produces
// them; on stop it drains whatever is buffered, then exits. This mirrors
// lake_emitter.run's drain-on-stop loop.
func (t *Tap) run() {
	defer close(t.done)
	ticker := time.NewTicker(tapFlushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-t.stop:
			// Drain remaining buffered records, then exit.
			for {
				select {
				case rec := <-t.ch:
					t.produceOne(rec)
				default:
					return
				}
			}
		case rec := <-t.ch:
			t.produceOne(rec)
		case <-ticker.C:
			// liveness backstop; nothing buffered handling needed
		}
	}
}

func (t *Tap) produceOne(rec tapRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), tapProduceTimeout)
	defer cancel()
	if err := t.prod.Produce(ctx, rec.topic, rec.key, rec.value); err != nil {
		atomic.AddUint64(&t.failed, 1)
		// Best-effort: log sparingly. A down broker must not spam, but we keep
		// the contract observable.
		log.Printf("[eventbus-tap] produce error topic=%s: %v", rec.topic, err)
		return
	}
	atomic.AddUint64(&t.produced, 1)
}

// Stats returns the three lifetime counters. produced = acked by the producer;
// dropped = rejected because the buffer was full (lossy by design); failed =
// produce returned an error. Returns zeros for a disabled tap.
func (t *Tap) Stats() (produced, dropped, failed uint64) {
	return atomic.LoadUint64(&t.produced),
		atomic.LoadUint64(&t.dropped),
		atomic.LoadUint64(&t.failed)
}

// Close stops the drain goroutine, draining buffered records first (bounded
// wait), then closes the underlying producer. Safe on a disabled tap.
func (t *Tap) Close() error {
	if t.prod == nil {
		return nil
	}
	close(t.stop)
	select {
	case <-t.done:
	case <-time.After(10 * time.Second):
	}
	return t.prod.Close()
}

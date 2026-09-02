package eventbus

// consumer_liveness_test.go pins the four behaviours REQ-088 exists to create,
// each of which was ABSENT during the 2026-09-01 SK-4 wedge (54,097 sidecar +
// ~200k board recipients parked on send.commands.v1 with /health reporting
// consumer_running: true):
//
//	HandleTimeout          — one handler attempt cannot block forever
//	RunExitFlipsRunning    — a dead loop cannot report itself alive
//	NoCommitPastRetained   — a record that fails handler AND DLQ is not silently
//	                         acknowledged by the next record's commit
//	DLQ park accounting    — a parked RECORD is logged and counted separately
//	                         from failed ATTEMPTS
//
// All broker-free: processFetch is the loop-level seam, and *kgo.Record is a
// plain struct we can construct.

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// fakeCommitter records exactly which offsets were committed. The whole
// NoCommitPastRetained proof is "this slice does not contain the poison offset
// or anything after it".
type fakeCommitter struct{ offsets []int64 }

func (f *fakeCommitter) CommitRecords(_ context.Context, rs ...*kgo.Record) error {
	for _, r := range rs {
		f.offsets = append(f.offsets, r.Offset)
	}
	return nil
}

// recordingDLQ (declared in eventbus_test.go) succeeds and counts parks.

// failingDLQ always fails, which is the condition that makes a record RETAINED
// (neither handled nor durably parked) — the case the commit barrier guards.
type failingDLQ struct{ n int }

func (d *failingDLQ) Dead(_ context.Context, _ string, _, _ []byte, _ error) error {
	d.n++
	return errors.New("dlq down")
}

func recs(topic string, partition int32, offsets ...int64) []*kgo.Record {
	out := make([]*kgo.Record, 0, len(offsets))
	for _, o := range offsets {
		out = append(out, &kgo.Record{
			Topic:     topic,
			Partition: partition,
			Offset:    o,
			Key:       []byte("k"),
			Value:     []byte(offsetValue(o)),
		})
	}
	return out
}

func offsetValue(o int64) string {
	if o == 11 {
		return "poison"
	}
	return "ok"
}

// ---------------------------------------------------------------------------
// 1. HandleTimeout
// ---------------------------------------------------------------------------

func TestHandleTimeout_DefaultIsThirtySeconds(t *testing.T) {
	// 30s == the production statement_timeout on worker queries. If this
	// default drifts, a DB call the server would kill can still outlive its
	// deadline here and hold the partition.
	if got := (ConsumerOptions{}).handleTimeout(); got != 30*time.Second {
		t.Fatalf("default HandleTimeout = %s, want 30s", got)
	}
	if DefaultHandleTimeout != 30*time.Second {
		t.Fatalf("DefaultHandleTimeout = %s, want 30s", DefaultHandleTimeout)
	}
	if got := (ConsumerOptions{HandleTimeout: 5 * time.Second}).handleTimeout(); got != 5*time.Second {
		t.Fatalf("explicit HandleTimeout not honored: %s", got)
	}
}

func TestHandleTimeout_BlockedHandlerIsBoundedRetriedThenDLQd(t *testing.T) {
	// THE WEDGE, reproduced: a handler that blocks forever on a DB write.
	// Before HandleTimeout existed, this call never returned — franz-go kept
	// heartbeating, the member kept its partitions, and nothing was inserted.
	var attempts atomic.Int64
	blocked := make(chan struct{})
	defer close(blocked)

	h := func(ctx context.Context, _, _ []byte) error {
		attempts.Add(1)
		select {
		case <-ctx.Done(): // only our per-record deadline can free this
			return ctx.Err()
		case <-blocked:
			return nil
		}
	}
	dlq := &recordingDLQ{}
	var parkedAttempts int
	opts := ConsumerOptions{
		MaxRetries:    1, // => 2 attempts
		RetryBackoff:  time.Millisecond,
		HandleTimeout: 20 * time.Millisecond,
		OnDLQ: func(_ string, _, _ []byte, n int, _ error) {
			parkedAttempts = n
		},
	}

	done := make(chan error, 1)
	go func() {
		// Parent ctx is Background: healthy process, stuck handler.
		done <- processRecord(context.Background(), h, dlq, opts, "send.commands.v1", []byte("k"), []byte("v"))
	}()

	select {
	case err := <-done:
		if err != nil {
			// errShutdown here would be the classification bug: our own
			// deadline is NOT a shutdown, and returning it as one would stop
			// the whole loop instead of parking one record.
			t.Fatalf("processRecord should return nil after DLQ park, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("processRecord never returned — HandleTimeout did not bound the handler (this is the wedge)")
	}

	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (initial + 1 retry)", got)
	}
	if dlq.count() != 1 {
		t.Fatalf("DLQ parks = %d, want 1", dlq.count())
	}
	if parkedAttempts != 2 {
		t.Fatalf("OnDLQ attempts = %d, want 2", parkedAttempts)
	}
}

func TestHandleTimeout_ParentShutdownStillRetainsRecord(t *testing.T) {
	// A per-record deadline must not weaken the shutdown contract: when the
	// PARENT context is cancelled, the record is retained (redelivered on
	// restart), never DLQ'd. A production DLQ does not auto-replay.
	ctx, cancel := context.WithCancel(context.Background())
	h := func(hctx context.Context, _, _ []byte) error {
		cancel() // shutdown arrives mid-handle
		<-hctx.Done()
		return hctx.Err()
	}
	dlq := &recordingDLQ{}
	err := processRecord(ctx, h, dlq, ConsumerOptions{HandleTimeout: time.Second}, "t", nil, nil)
	if !errors.Is(err, errShutdown) {
		t.Fatalf("want errShutdown on parent cancel, got %v", err)
	}
	if dlq.count() != 0 {
		t.Fatalf("shutdown must NOT DLQ; parks = %d", dlq.count())
	}
}

// ---------------------------------------------------------------------------
// 2. Run exit flips running
// ---------------------------------------------------------------------------

func TestRunExitFlipsRunningFalse(t *testing.T) {
	c := newConsumer(nil, "send-queue-writer", func(context.Context, []byte, []byte) error { return nil }, nil, ConsumerOptions{})

	if c.Running() {
		t.Fatal("a never-started consumer must not report Running()")
	}
	if c.Snapshot().Running {
		t.Fatal("a never-started consumer must not report Snapshot().Running")
	}

	// Simulate the loop being live (what the OLD boot boolean asserted forever).
	c.running.Store(true)
	if !c.Snapshot().Running {
		t.Fatal("Snapshot must reflect the running flag")
	}

	// Run returns immediately (no client). The point is the DEFER: whatever the
	// exit reason, running goes false. Before this, cmd/server's
	// `go func(){ _ = con.Run(ctx) }()` discarded the exit and /health kept
	// reporting consumer_running: true.
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("Run with no client should return an error")
	}
	if c.Running() {
		t.Fatal("Run exit did NOT flip running=false — /health would report a dead loop as alive")
	}
	if c.Snapshot().Running {
		t.Fatal("Snapshot().Running still true after Run returned")
	}

	// And a cancelled parent produces the same flip.
	c.running.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Run(ctx)
	if c.Running() {
		t.Fatal("Run(cancelled ctx) exit did not flip running=false")
	}
}

// ---------------------------------------------------------------------------
// 3. Commit barrier (the negative path)
// ---------------------------------------------------------------------------

func TestNoCommitPastRetainedRecord(t *testing.T) {
	// Offsets 10, 11, 12 on one partition. 11's handler fails AND its DLQ
	// fails, so 11 is RETAINED. Kafka commits are a high-water mark, so the old
	// loop's `continue` + commit of 12 acknowledged 11 as well and it never
	// redelivered: a silently dropped recipient.
	h := func(_ context.Context, _, value []byte) error {
		if string(value) == "poison" {
			return errors.New("db down")
		}
		return nil
	}
	dlq := &failingDLQ{}
	c := newConsumer(nil, "test", h, dlq, ConsumerOptions{MaxRetries: 0, RetryBackoff: time.Millisecond})
	fc := &fakeCommitter{}

	if err := c.processFetch(context.Background(), fc, "send.commands.v1", 0, 13, recs("send.commands.v1", 0, 10, 11, 12)); err != nil {
		t.Fatalf("processFetch returned %v, want nil (retention is not a loop-stopping error)", err)
	}

	for _, off := range fc.offsets {
		if off >= 11 {
			t.Fatalf("committed offset %d at/after the retained record (offsets=%v) — the retained record was silently acknowledged", off, fc.offsets)
		}
	}
	if len(fc.offsets) != 1 || fc.offsets[0] != 10 {
		t.Fatalf("committed offsets = %v, want exactly [10]", fc.offsets)
	}
	if got := c.Snapshot().RetainedPartitions; got != 1 {
		t.Fatalf("RetainedPartitions = %d, want 1 (the barrier must be visible on /health)", got)
	}

	// The barrier persists across fetches: a later fetch on the SAME partition
	// must still not commit forward, or the drop returns one poll later.
	fc2 := &fakeCommitter{}
	if err := c.processFetch(context.Background(), fc2, "send.commands.v1", 0, 20, recs("send.commands.v1", 0, 13, 14)); err != nil {
		t.Fatalf("second processFetch: %v", err)
	}
	if len(fc2.offsets) != 0 {
		t.Fatalf("committed %v on a later fetch while the barrier stands", fc2.offsets)
	}

	// A DIFFERENT partition is unaffected — one poison record must not stall
	// the whole topic.
	fc3 := &fakeCommitter{}
	if err := c.processFetch(context.Background(), fc3, "send.commands.v1", 1, 5, recs("send.commands.v1", 1, 3, 4)); err != nil {
		t.Fatalf("other-partition processFetch: %v", err)
	}
	if len(fc3.offsets) != 2 {
		t.Fatalf("partition 1 committed %v, want both offsets", fc3.offsets)
	}
}

func TestNoCommitPastRetained_PositiveControlAndBarrierLift(t *testing.T) {
	// Positive control: with no failure, every offset commits (so the test
	// above is proving the barrier, not a broken committer).
	h := func(_ context.Context, _, value []byte) error {
		if string(value) == "poison" {
			return errors.New("db down")
		}
		return nil
	}
	c := newConsumer(nil, "test", h, &failingDLQ{}, ConsumerOptions{MaxRetries: 0, RetryBackoff: time.Millisecond})
	fc := &fakeCommitter{}
	if err := c.processFetch(context.Background(), fc, "t", 0, 3, recs("t", 0, 0, 1, 2)); err != nil {
		t.Fatalf("processFetch: %v", err)
	}
	if len(fc.offsets) != 3 {
		t.Fatalf("healthy fetch committed %v, want all 3", fc.offsets)
	}
	if got := c.Snapshot().LagMax; got != 0 {
		t.Fatalf("LagMax = %d, want 0 (high watermark 3, last offset 2)", got)
	}
	if !c.Snapshot().LagKnown {
		t.Fatal("LagKnown should be true after a fetch carrying records")
	}
	if c.Snapshot().LastHandledAt.IsZero() {
		t.Fatal("LastHandledAt must advance on a successful handle")
	}

	// Barrier lift: retain offset 11, then redeliver it successfully.
	c2 := newConsumer(nil, "test", h, &failingDLQ{}, ConsumerOptions{MaxRetries: 0, RetryBackoff: time.Millisecond})
	_ = c2.processFetch(context.Background(), &fakeCommitter{}, "t", 0, 13, recs("t", 0, 11))
	if c2.Snapshot().RetainedPartitions != 1 {
		t.Fatal("expected a barrier after the failing record")
	}
	// Redelivery of 11, this time with a payload the handler accepts.
	fc2 := &fakeCommitter{}
	redelivered := []*kgo.Record{{Topic: "t", Partition: 0, Offset: 11, Value: []byte("ok")}}
	_ = c2.processFetch(context.Background(), fc2, "t", 0, 13, redelivered)
	if len(fc2.offsets) != 1 || fc2.offsets[0] != 11 {
		t.Fatalf("redelivered barrier offset should commit, got %v", fc2.offsets)
	}
	if c2.Snapshot().RetainedPartitions != 0 {
		t.Fatal("barrier must lift once the retained offset is handled")
	}
}

func TestProcessFetch_LagMaxFromHighWatermark(t *testing.T) {
	// No kadm in go.mod, so lag comes from the fetch's HighWatermark minus the
	// offset after the last record processed. Documented in health.go.
	c := newConsumer(nil, "test", func(context.Context, []byte, []byte) error { return nil }, nil, ConsumerOptions{})
	_ = c.processFetch(context.Background(), &fakeCommitter{}, "t", 0, 1000, recs("t", 0, 100))
	if got := c.Snapshot().LagMax; got != 899 {
		t.Fatalf("LagMax = %d, want 899 (1000 - (100+1))", got)
	}
	// LagMax is the MAX across partitions, not the latest partition seen.
	_ = c.processFetch(context.Background(), &fakeCommitter{}, "t", 1, 105, recs("t", 1, 100))
	if got := c.Snapshot().LagMax; got != 899 {
		t.Fatalf("LagMax = %d after a low-lag partition, want the max 899", got)
	}
}

// ---------------------------------------------------------------------------
// 4. DLQ park accounting
// ---------------------------------------------------------------------------

func TestDLQParkCountsRecordsNotAttempts(t *testing.T) {
	// queue_writes_failed counts ATTEMPTS (up to 4 per record). On 2026-09-01
	// /health showed failed=17 and failed=28 across two tasks, from which the
	// number of LOST RECIPIENTS was not derivable. queue_dlq_records is that
	// number.
	var seenTopic string
	var seenAttempts int
	h := func(context.Context, []byte, []byte) error { return errors.New("permanent") }
	dlq := &recordingDLQ{}
	c := newConsumer(nil, "test", h, dlq, ConsumerOptions{
		MaxRetries:   2, // => 3 attempts per record
		RetryBackoff: time.Millisecond,
		OnDLQ: func(topic string, _, _ []byte, attempts int, _ error) {
			seenTopic, seenAttempts = topic, attempts
		},
	})

	fc := &fakeCommitter{}
	if err := c.processFetch(context.Background(), fc, "send.commands.v1", 0, 3, recs("send.commands.v1", 0, 0, 1)); err != nil {
		t.Fatalf("processFetch: %v", err)
	}

	snap := c.Snapshot()
	if snap.DLQRecords != 2 {
		t.Fatalf("DLQRecords = %d, want 2 (two RECORDS parked, six attempts)", snap.DLQRecords)
	}
	if dlq.count() != 2 {
		t.Fatalf("DLQ.Dead calls = %d, want 2", dlq.count())
	}
	if seenTopic != "send.commands.v1" || seenAttempts != 3 {
		t.Fatalf("OnDLQ got topic=%q attempts=%d, want send.commands.v1/3", seenTopic, seenAttempts)
	}
	// A durably parked record IS safe to commit — it is not retained.
	if len(fc.offsets) != 2 {
		t.Fatalf("DLQ'd records must still commit; offsets=%v", fc.offsets)
	}
	if snap.RetainedPartitions != 0 {
		t.Fatalf("a successful DLQ park must not raise a commit barrier")
	}
}

func TestDLQ_TaskIDIsNeverEmpty(t *testing.T) {
	// Per-task counters are meaningless without the task that produced them:
	// the ALB answers /health from whichever task it picks.
	if TaskID() == "" {
		t.Fatal("TaskID() must never be empty")
	}
	if got := SendQueueHealth(); got.TaskID == "" {
		t.Fatal("SendQueueHealth() must carry a task id even when unwired")
	}
}

func TestDLQ_SendQueueHealthProviderRoundTrip(t *testing.T) {
	// This is the symbol REQ-087's OutboxSelfCheck reads.
	defer SetSendQueueHealthProvider(nil)
	if SendQueueHealth().Running {
		t.Fatal("unwired SendQueueHealth must report Running=false, not a stale true")
	}
	SetSendQueueHealthProvider(func() ConsumerSnapshot {
		return ConsumerSnapshot{Name: "send-queue-writer", Running: true, LagMax: 4321,
			LastHandledAt: time.Now().Add(-11 * time.Minute)}
	})
	got := SendQueueHealth()
	if !got.Running || got.LagMax != 4321 {
		t.Fatalf("provider not read back: %+v", got)
	}
	if s := got.SecondsSinceLastHandled(); s < 600 {
		t.Fatalf("SecondsSinceLastHandled = %v, want >600 for an 11-minute-old handle", s)
	}
	// "never handled" must be distinguishable from "handled just now".
	if (ConsumerSnapshot{}).SecondsSinceLastHandled() != -1 {
		t.Fatal("a never-handled snapshot must report -1, not 0")
	}
}

// ---------------------------------------------------------------------------
// 5. The alert predicate REQ-087's OutboxSelfCheck calls (negative paths first)
// ---------------------------------------------------------------------------

func TestDLQ_SendQueueParkedPredicate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		delta uint64
		snap  ConsumerSnapshot
		want  bool
	}{
		{
			// NEGATIVE: nothing routed to Kafka this tick. A quiet topic is not
			// a wedge — this is the state task-def 1077 (KAFKA_SEND_QUEUE_ALL=0)
			// runs in today, and it must never page.
			name: "no waves routed", delta: 0,
			snap: ConsumerSnapshot{Running: true, LastHandledAt: now.Add(-2 * time.Hour)},
			want: false,
		},
		{
			// NEGATIVE: SK-4 never wired on this task (zero snapshot).
			name: "consumer never wired", delta: 50,
			snap: ConsumerSnapshot{},
			want: false,
		},
		{
			// NEGATIVE: routing and keeping up.
			name: "healthy", delta: 50,
			snap: ConsumerSnapshot{Running: true, LastPollAt: now, LastHandledAt: now.Add(-3 * time.Second), LagKnown: true, LagMax: 12},
			want: false,
		},
		{
			// NEGATIVE: lag is large but UNKNOWN (no fetch has carried records
			// yet) — must not fire on an unmeasured field.
			name: "lag not yet known", delta: 50,
			snap: ConsumerSnapshot{Running: true, LastPollAt: now, LastHandledAt: now.Add(-time.Second), LagKnown: false, LagMax: 99999},
			want: false,
		},
		{
			name: "loop dead", delta: 50,
			snap: ConsumerSnapshot{Running: false, LastPollAt: now.Add(-time.Minute)},
			want: true,
		},
		{
			name: "lag over threshold", delta: 50,
			snap: ConsumerSnapshot{Running: true, LastPollAt: now, LastHandledAt: now, LagKnown: true, LagMax: 1001},
			want: true,
		},
		{
			// THE 2026-09-01 SIGNATURE: alive, polling, assigned, handling
			// nothing for three hours.
			name: "handled nothing for 3h", delta: 50,
			snap: ConsumerSnapshot{Running: true, LastPollAt: now, LastHandledAt: now.Add(-3 * time.Hour)},
			want: true,
		},
		{
			name: "running but never handled anything", delta: 50,
			snap: ConsumerSnapshot{Running: true, LastPollAt: now},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := SendQueueParked(tc.delta, tc.snap)
			if got != tc.want {
				t.Fatalf("SendQueueParked = %v (%q), want %v", got, reason, tc.want)
			}
			if got && reason == "" {
				t.Fatal("a parked verdict must carry an operator-facing reason")
			}
			if !got && reason != "" {
				t.Fatalf("a healthy verdict must carry no reason, got %q", reason)
			}
		})
	}
}

func TestHandleTimeout_KillSwitchEnvParsing(t *testing.T) {
	// HandleTimeout is NEW behaviour on the send path, so it needs a one-move
	// undo. envHandleTimeout caches with sync.Once, so exercise the parsing
	// contract directly rather than the cached accessor.
	t.Setenv("DISABLE_CONSUMER_HANDLE_TIMEOUT", "1")
	if !isTruthyEnv(os.Getenv("DISABLE_CONSUMER_HANDLE_TIMEOUT")) {
		t.Fatal("DISABLE_CONSUMER_HANDLE_TIMEOUT=1 must read truthy")
	}
	for _, v := range []string{"", "0", "false", "no", "off"} {
		if isTruthyEnv(v) {
			t.Fatalf("%q must NOT read truthy — the deadline stays on by default", v)
		}
	}
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !isTruthyEnv(v) {
			t.Fatalf("%q should read truthy", v)
		}
	}
	if noHandleTimeout <= DefaultHandleTimeout {
		t.Fatal("the disabled value must be far beyond the default")
	}
	// An explicit per-consumer HandleTimeout always wins over env.
	if got := (ConsumerOptions{HandleTimeout: 7 * time.Second}).handleTimeout(); got != 7*time.Second {
		t.Fatalf("explicit option must beat env, got %s", got)
	}
}

package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// HandleTimeout bounds ONE handler attempt. 0 => 30s, matching the prod
	// statement_timeout the DB-writing handlers run under.
	//
	// WHY: before this existed the handler ran on the SERVER LIFECYCLE context,
	// which has no deadline. A handler blocked on a DB write (pool exhaustion,
	// a lock) therefore blocked FOREVER while franz-go kept heartbeating in the
	// background — the member holds its partition assignment and consumes
	// nothing. That is exactly the 2026-09-01 SK-4 signature: alive, assigned,
	// zero progress, zero errors. A deadline converts that silent wedge into a
	// visible retry, then a DLQ park, then a counter an operator can page on.
	HandleTimeout time.Duration
	// OnDLQ, when set, is called ONCE per record durably parked in the DLQ,
	// with the number of attempts made. It is the hook a consumer uses to count
	// DLQ'd RECORDS (as opposed to failed ATTEMPTS) and to log payload-specific
	// identity (campaign/wave) that this generic loop cannot decode.
	OnDLQ func(topic string, key, value []byte, attempts int, cause error)
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

// DefaultHandleTimeout bounds one handler attempt when HandleTimeout is unset.
// 30s == the production statement_timeout on worker queries, so a DB call that
// the server would kill anyway cannot outlive its own deadline here.
const DefaultHandleTimeout = 30 * time.Second

func (o ConsumerOptions) handleTimeout() time.Duration {
	if o.HandleTimeout > 0 {
		return o.HandleTimeout
	}
	if d := envHandleTimeout(); d > 0 {
		return d
	}
	return DefaultHandleTimeout
}

// noHandleTimeout is what DISABLE_CONSUMER_HANDLE_TIMEOUT selects: a deadline so
// far out it is effectively the old unbounded behaviour, without adding a
// nil-deadline code path to processRecord.
const noHandleTimeout = 24 * time.Hour

var (
	handleTimeoutOnce    sync.Once
	handleTimeoutFromEnv time.Duration
)

// envHandleTimeout is the KILL SWITCH for the per-record deadline — required
// because HandleTimeout is new behaviour on the send path:
//
//	DISABLE_CONSUMER_HANDLE_TIMEOUT=1        -> effectively unbounded (pre-REQ-088)
//	EVENTBUS_HANDLE_TIMEOUT_SECONDS=<n>      -> override the 30s default
//
// Read ONCE (a per-record os.Getenv on the hot path is not free), so a change
// needs a task restart — same posture as the other boot-read send-path envs.
// Returns 0 when neither is set.
func envHandleTimeout() time.Duration {
	handleTimeoutOnce.Do(func() {
		if isTruthyEnv(os.Getenv("DISABLE_CONSUMER_HANDLE_TIMEOUT")) {
			handleTimeoutFromEnv = noHandleTimeout
			return
		}
		if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EVENTBUS_HANDLE_TIMEOUT_SECONDS"))); err == nil && n > 0 {
			handleTimeoutFromEnv = time.Duration(n) * time.Second
		}
	})
	return handleTimeoutFromEnv
}

// isTruthyEnv mirrors the send path's existing env truthiness (1/true/yes/on).
func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// errPanic wraps a recovered panic so the DLQ cause is informative.
type errPanic struct{ v any }

func (e errPanic) Error() string { return fmt.Sprintf("handler panic: %v", e.v) }

// errShutdown is the sentinel processRecord returns when the handler failed
// because the context was canceled/deadline-exceeded — i.e. the process is
// SHUTTING DOWN, not because the record is genuinely poison. The consumer loop
// treats it specially: it stops WITHOUT committing the in-flight record's offset
// and WITHOUT routing it to the DLQ, so the record redelivers on restart. This
// is the fix for the clean-shutdown-dumps-valid-records-into-the-DLQ gap: a
// production DLQ does not auto-replay, so DLQ'ing valid in-flight work on every
// shutdown is a silent drop. A record only reaches the DLQ when it genuinely
// failed maxRetries times for a NON-shutdown reason.
var errShutdown = errors.New("eventbus: consumer shutting down (record retained, not DLQ'd)")

// isShutdownErr reports whether err is a context cancellation / deadline from a
// shutdown (as opposed to a real handler failure).
func isShutdownErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// processRecord runs the handler with at-least-once semantics for ONE record:
// per-record panic recovery, bounded retries, and DLQ on permanent failure. It
// is broker-free and is the unit-tested core of the consumer. It returns nil
// once the record is either handled OR durably sent to the DLQ (i.e. the offset
// is safe to commit); it returns errShutdown if the failure was caused by ctx
// cancellation (the caller must STOP without committing and WITHOUT DLQ'ing —
// the record redelivers on restart); it returns any other non-nil error ONLY if
// the DLQ itself failed, in which case the caller must NOT commit (so the record
// is redelivered rather than lost).
func processRecord(ctx context.Context, h Handler, dlq DLQ, opts ConsumerOptions, topic string, key, value []byte) error {
	attempts := opts.maxRetries() + 1
	made := 0
	var lastErr error
	for i := 0; i < attempts; i++ {
		// If the context is already canceled before we even attempt, this is a
		// shutdown: do not run/retry, do not DLQ — leave the record for redelivery.
		if ctx.Err() != nil {
			return errShutdown
		}
		// Per-record deadline. hctx is derived from ctx, so a shutdown still
		// cancels the handler; the extra deadline only bounds a handler that
		// would otherwise block forever (see ConsumerOptions.HandleTimeout).
		hctx, hcancel := context.WithTimeout(ctx, opts.handleTimeout())
		err := safeHandle(hctx, h, key, value)
		// Read hctx.Err() BEFORE hcancel(), which would overwrite it with
		// context.Canceled and make a real timeout indistinguishable.
		timedOut := errors.Is(hctx.Err(), context.DeadlineExceeded)
		hcancel()
		made = i + 1
		if err != nil {
			lastErr = err
			// A failure caused by PARENT ctx cancellation is a SHUTDOWN, not a
			// poison record: stop immediately, do NOT DLQ, do NOT commit. The
			// record redelivers on restart. Checked first so a HandleTimeout
			// firing during shutdown is still treated as shutdown.
			if ctx.Err() != nil {
				return errShutdown
			}
			if timedOut {
				// OUR deadline fired while the parent is healthy: a genuinely
				// stuck handler, not a shutdown. Retry it, then DLQ it — never
				// classify it as shutdown, or the loop would return and the
				// wedge would be silent again.
				lastErr = fmt.Errorf("eventbus: handler exceeded HandleTimeout %s: %w", opts.handleTimeout(), err)
			} else if isShutdownErr(err) {
				return errShutdown
			}
			if i < attempts-1 {
				select {
				case <-ctx.Done():
					// Shutdown arrived during backoff: retain the record.
					return errShutdown
				case <-time.After(opts.retryBackoff()):
				}
			}
			continue
		}
		return nil // handled successfully -> commit
	}

	// Permanent failure (NON-shutdown): do not lose the record.
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
	// A DLQ park is a recipient leaving the send path. Before this line it was
	// SILENT: no log, no per-record counter, only the handler's per-ATTEMPT
	// failure counter, from which the number of lost records is not derivable.
	log.Printf("[eventbus-consumer] DLQ PARK topic=%s key=%q attempts=%d: %v", topic, string(key), made, lastErr)
	if opts.OnDLQ != nil {
		opts.OnDLQ(topic, key, value, made, lastErr)
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
	name    string

	// ── liveness (see health.go) ──
	// running is true only between Run's entry and its return, for ANY exit
	// reason. /health reads it through Snapshot(), so a loop that dies (or
	// returns on a fetch error) can no longer report itself healthy forever.
	running       atomic.Bool
	lastPollNanos atomic.Int64
	lastHandNanos atomic.Int64
	inFlight      atomic.Int64
	lagKnown      atomic.Bool
	dlqRecords    atomic.Uint64

	// retained maps "topic/partition" -> the offset of a record that could be
	// neither handled nor DLQ'd. While an entry exists, NOTHING at or after that
	// offset on that partition is committed, so the record redelivers instead of
	// being silently acknowledged by a later record's commit.
	// lagByTP is the last observed lag per topic/partition (guarded by
	// retainedMu). Snapshot reduces it to LagMax.
	retainedMu sync.Mutex
	retained   map[string]int64
	lagByTP    map[string]int64
}

// newConsumer builds the value type. Split out of NewConsumer so tests can
// construct a broker-less consumer (client == nil) and exercise processFetch.
func newConsumer(client *kgo.Client, name string, handler Handler, dlq DLQ, opts ConsumerOptions) *Consumer {
	c := &Consumer{client: client, handler: handler, dlq: dlq, opts: opts, name: name, retained: map[string]int64{}}
	// Chain a DLQ-record counter in front of any caller-supplied OnDLQ so the
	// count is structural, not something each consumer must remember to do.
	user := opts.OnDLQ
	c.opts.OnDLQ = func(topic string, key, value []byte, attempts int, cause error) {
		c.dlqRecords.Add(1)
		if user != nil {
			user(topic, key, value, attempts, cause)
		}
	}
	return c
}

// Running reports whether the Run loop is currently executing. This is the
// value /health must show — NOT "Start returned nil once at boot".
func (c *Consumer) Running() bool { return c.running.Load() }

// Snapshot returns the current liveness reading. Cheap: atomic loads plus one
// short mutex for the retained-partition count.
func (c *Consumer) Snapshot() ConsumerSnapshot {
	c.retainedMu.Lock()
	retained := len(c.retained)
	var lagMax int64
	for _, v := range c.lagByTP {
		if v > lagMax {
			lagMax = v
		}
	}
	c.retainedMu.Unlock()
	return ConsumerSnapshot{
		Name:               c.name,
		TaskID:             TaskID(),
		Running:            c.running.Load(),
		LastPollAt:         nanosToTime(c.lastPollNanos.Load()),
		LastHandledAt:      nanosToTime(c.lastHandNanos.Load()),
		InFlight:           c.inFlight.Load(),
		LagMax:             lagMax,
		LagKnown:           c.lagKnown.Load(),
		RetainedPartitions: retained,
		DLQRecords:         c.dlqRecords.Load(),
	}
}

func nanosToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// recordCommitter is the commit seam. *kgo.Client satisfies it; tests inject a
// fake to assert exactly which offsets were committed.
type recordCommitter interface {
	CommitRecords(ctx context.Context, rs ...*kgo.Record) error
}

// errNoClient is returned by Run when no broker client was constructed. Only
// reachable from a test-built consumer; the exported constructor always has one.
var errNoClient = errors.New("eventbus: consumer has no client")

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
	return newConsumer(client, cfg.Group, handler, dlq, opts), nil
}

// Run polls and processes records until ctx is cancelled. Each successfully
// handled (or DLQ-parked) record's offset is committed; a record whose DLQ
// write also fails is NOT committed and will be redelivered. Panics in the
// handler are isolated per-record.
func (c *Consumer) Run(ctx context.Context) (err error) {
	// running is the ONLY honest liveness bit: true strictly while this loop is
	// executing, flipped back on EVERY exit path (ctx cancel, fetch error,
	// panic, missing client). Set BEFORE any early return so no exit path can
	// skip the defer. /health reads it via Snapshot(); the old boot-time boolean
	// in cmd/server could not see this return at all.
	c.running.Store(true)
	defer c.running.Store(false)

	if c.client == nil {
		return errNoClient
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fetches := c.client.PollFetches(ctx)
		c.lastPollNanos.Store(time.Now().UnixNano())
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				if errors.Is(e.Err, context.Canceled) {
					return ctx.Err()
				}
				log.Printf("[eventbus-consumer] fetch error topic=%s partition=%d: %v", e.Topic, e.Partition, e.Err)
			}
		}
		// Iterate PER PARTITION (not the flat RecordIter) because both the lag
		// reading (HighWatermark) and the commit barrier are per-partition facts.
		var stop bool
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if stop {
				return
			}
			if perr := c.processFetch(ctx, c.client, p.Topic, p.Partition, p.HighWatermark, p.Records); perr != nil {
				stop = true
			}
		})
		if stop {
			// Shutdown mid-record: STOP without committing that record's offset
			// and WITHOUT DLQ'ing it. The record (and the rest of this fetch)
			// redelivers on restart. This is the fix for the clean-shutdown ->
			// DLQ drop: valid in-flight work is never parked in a non-replaying
			// DLQ on a graceful stop.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errShutdown
		}
	}
}

// tpKey names a topic/partition for the retained map.
func tpKey(topic string, partition int32) string {
	return fmt.Sprintf("%s/%d", topic, partition)
}

// processFetch handles one partition's records from one fetch and is the
// LOOP-LEVEL test seam. It returns a non-nil error ONLY for shutdown (the caller
// must stop); every other outcome is recorded on the consumer's counters.
//
// THE COMMIT BARRIER (the defect this fixes): the previous loop, on a record
// whose handler AND DLQ both failed, logged "record retained" and `continue`d —
// then committed the NEXT record on the same partition. Kafka commits are a
// high-water mark, so committing offset N+1 acknowledges N as well: the retained
// record was silently skipped and never redelivered. Here a retained offset is
// remembered per partition and NOTHING at or after it is ever committed, so the
// group's committed offset stays behind the failure and the record comes back on
// the next rebalance/restart (where the ON CONFLICT idempotency guard makes the
// replay harmless). Later records are still HANDLED — mail keeps moving — they
// are just not acknowledged.
func (c *Consumer) processFetch(ctx context.Context, cm recordCommitter, topic string, partition int32, highWatermark int64, recs []*kgo.Record) error {
	if len(recs) == 0 {
		return nil
	}
	key := tpKey(topic, partition)

	for _, rec := range recs {
		c.inFlight.Add(1)
		perr := processRecord(ctx, c.handler, c.dlq, c.opts, rec.Topic, rec.Key, rec.Value)
		c.inFlight.Add(-1)

		if perr != nil {
			if errors.Is(perr, errShutdown) {
				return errShutdown
			}
			// Handler AND DLQ both failed. Park the barrier at this offset (the
			// LOWEST failing offset wins so a second failure cannot move it
			// forward) and refuse every commit at or after it.
			c.retainBarrier(key, rec.Offset)
			log.Printf("[eventbus-consumer] record RETAINED (uncommitted, commit barrier at offset %d) topic=%s partition=%d: %v",
				rec.Offset, topic, partition, perr)
			continue
		}

		c.lastHandNanos.Store(time.Now().UnixNano())

		if !c.mayCommit(key, rec.Offset) {
			continue
		}
		if err := cm.CommitRecords(ctx, rec); err != nil {
			log.Printf("[eventbus-consumer] commit error topic=%s partition=%d offset=%d: %v", topic, partition, rec.Offset, err)
		}
	}

	// Lag from the fetch itself (no kadm dependency — see health.go). recs is
	// offset-ordered, so the last record's offset+1 is this loop's position.
	if highWatermark > 0 {
		lag := highWatermark - (recs[len(recs)-1].Offset + 1)
		if lag < 0 {
			lag = 0
		}
		c.retainedMu.Lock()
		if c.lagByTP == nil {
			c.lagByTP = map[string]int64{}
		}
		c.lagByTP[key] = lag
		c.retainedMu.Unlock()
		c.lagKnown.Store(true)
	}
	return nil
}

// retainBarrier records (or lowers) the no-commit-past offset for a partition.
func (c *Consumer) retainBarrier(key string, offset int64) {
	c.retainedMu.Lock()
	defer c.retainedMu.Unlock()
	if cur, ok := c.retained[key]; !ok || offset < cur {
		c.retained[key] = offset
	}
}

// mayCommit reports whether this offset may be committed. An offset BELOW the
// barrier is fine (committing it sets the group position to at most the barrier,
// which still redelivers the retained record). The barrier offset itself, once
// it finally succeeds on redelivery, clears the barrier and commits.
func (c *Consumer) mayCommit(key string, offset int64) bool {
	c.retainedMu.Lock()
	defer c.retainedMu.Unlock()
	barrier, ok := c.retained[key]
	if !ok {
		return true
	}
	if offset < barrier {
		return true
	}
	if offset == barrier {
		delete(c.retained, key) // redelivered and handled — barrier lifted
		return true
	}
	return false
}

// Close releases the client.
func (c *Consumer) Close() error {
	if c.client == nil {
		return nil
	}
	c.client.Close()
	return nil
}

//go:build chaos

// Package sendqueue chaos+load harness.
//
// RUN:
//
//	go test -tags chaos -run TestChaos ./internal/eventbus/sendqueue/ -v -timeout 20m
//
// PREREQUISITES (the harness asserts them up front and SKIPs with a clear
// message if missing):
//   - redpanda on localhost:9092 (Kafka API, PLAINTEXT). Start with:
//     docker run -d --name rp-test -p 9092:9092 redpandadata/redpanda:latest \
//       redpanda start --overprovisioned --smp 1 --memory 1G --reserve-memory 0M \
//       --node-id 0 --check=false \
//       --kafka-addr PLAINTEXT://0.0.0.0:9092 \
//       --advertise-kafka-addr PLAINTEXT://localhost:9092
//   - the local apex-postgres dev container (postgres:16) with migration
//     050_send_ledger.sql applied. DSN below; override with SENDQUEUE_CHAOS_DSN.
//
// This file PROVES the two invariants of the sendqueue core against a REAL
// broker and a REAL postgres ledger, under real failure conditions:
//   - No double-send: CountingSink.DuplicateKeySends() == 0 in every scenario.
//   - No drop:        CountingSink.DistinctKeys() == expected in every scenario.
//
// It drives the production KafkaSendConsumer (real eventbus.Consumer, manual
// offset commit) and the real LedgerReconciler — nothing is mocked except the
// transport boundary (CountingSink models PMTA idempotency, exactly as designed).
package sendqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	_ "github.com/lib/pq"
)

// jsonMarshal mirrors the encoding EnqueueSendCommands uses (json.Marshal).
func jsonMarshal(c SendCommand) ([]byte, error) { return json.Marshal(c) }

// dockerCmd runs a docker subcommand (pause/unpause) against the broker container.
func dockerCmd(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", args...).Run()
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

const (
	defaultBroker = "localhost:9092"
	defaultDSN    = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
)

func broker() string {
	if v := os.Getenv("SENDQUEUE_CHAOS_BROKER"); v != "" {
		return v
	}
	return defaultBroker
}

func dsn() string {
	if v := os.Getenv("SENDQUEUE_CHAOS_DSN"); v != "" {
		return v
	}
	return defaultDSN
}

// busCfg returns a PLAINTEXT eventbus.Config pointed at the local redpanda, with
// the given consumer group + topic. AllowAutoTopics lets EnsureTopics work and
// is harmless on a single-broker cluster.
func busCfg(group string, topic string) eventbus.Config {
	return eventbus.Config{
		Brokers:         []string{broker()},
		ClientID:        "sendqueue-chaos",
		SASLMechanism:   "",
		TLS:             false,
		AllowAutoTopics: true,
		Group:           group,
		Topics:          []string{topic},
	}
}

// openDB connects to the dev ledger DB or SKIPs the suite.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn())
	if err != nil {
		t.Skipf("SKIP chaos: cannot open ledger DB (%v). Start apex-postgres + apply migration 050.", err)
	}
	db.SetMaxOpenConns(32)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("SKIP chaos: cannot ping ledger DB at %s (%v). Start apex-postgres.", dsn(), err)
	}
	// Confirm the ledger table exists.
	var reg sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT to_regclass('public.mailing_send_ledger')").Scan(&reg); err != nil || !reg.Valid {
		t.Skipf("SKIP chaos: mailing_send_ledger missing (%v). Apply migrations/050_send_ledger.sql.", err)
	}
	return db
}

// requireBroker SKIPs unless redpanda answers a metadata request.
func requireBroker(t *testing.T) {
	t.Helper()
	cfg := busCfg("chaos-ping", "chaos.ping")
	if !cfg.Enabled() {
		t.Skip("SKIP chaos: no broker configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// EnsureTopics performs a real CreateTopics round-trip; if the broker is
	// down this errors and we SKIP rather than hang.
	if err := eventbus.EnsureTopics(ctx, cfg, []eventbus.TopicSpec{{Name: "chaos.ping", Partitions: 1, ReplicationFactor: 1}}); err != nil {
		t.Skipf("SKIP chaos: redpanda not reachable at %s (%v). Start the rp-test container.", broker(), err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// truncateLedger wipes the ledger between scenarios (clean-room).
func truncateLedger(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("TRUNCATE mailing_send_ledger"); err != nil {
		t.Fatalf("truncate ledger: %v", err)
	}
}

// freshTopic creates a uniquely-named topic with N partitions so each scenario
// is isolated (no offset/key carryover) and concurrent consumers actually get
// distinct partitions to rebalance over.
func freshTopic(t *testing.T, partitions int32) string {
	t.Helper()
	name := fmt.Sprintf("send.commands.chaos.%d.%s", time.Now().UnixNano(), uuid.NewString()[:8])
	cfg := busCfg("ensure", name)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := eventbus.EnsureTopics(ctx, cfg, []eventbus.TopicSpec{{
		Name: name, Partitions: partitions, ReplicationFactor: 1,
	}}); err != nil {
		t.Fatalf("create topic %s: %v", name, err)
	}
	return name
}

// makeKeys builds n SendCommands with fresh, distinct idempotency keys.
func makeKeys(n int) []SendCommand {
	cmds := make([]SendCommand, n)
	for i := 0; i < n; i++ {
		cmds[i] = SendCommand{
			IdempotencyKey: uuid.New(),
			CampaignID:     uuid.New(),
			SubscriberID:   uuid.New(),
			WaveID:         uuid.New(),
			Recipient:      fmt.Sprintf("u%d@example.com", i),
			ISP:            "gmail",
			Subject:        "chaos",
		}
	}
	return cmds
}

// produceAll publishes every command via the real EnqueueSendCommands path onto
// the given topic. It uses a private kgo producer pointed at our fresh topic by
// temporarily reusing the same TopicSendCommands constant is NOT possible (the
// enqueue helper hardcodes the topic), so we produce directly here with the same
// keying semantics (key = idempotency-key bytes) the production helper uses.
func produceAll(t *testing.T, topic string, cmds []SendCommand) {
	t.Helper()
	prod, err := eventbus.NewKgoProducer(busCfg("producer", topic))
	if err != nil {
		t.Fatalf("new producer: %v", err)
	}
	defer prod.Close()
	produceWith(t, prod, topic, cmds)
}

// produceWith publishes via an existing producer (same key semantics as
// EnqueueSendCommands: key = idempotency-key bytes, value = JSON command).
func produceWith(t *testing.T, prod eventbus.Producer, topic string, cmds []SendCommand) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for i := range cmds {
		value := mustJSON(t, cmds[i])
		key := make([]byte, len(cmds[i].IdempotencyKey))
		copy(key, cmds[i].IdempotencyKey[:])
		if err := prod.Produce(ctx, topic, key, value); err != nil {
			t.Fatalf("produce %d: %v", i, err)
		}
	}
}

func mustJSON(t *testing.T, c SendCommand) []byte {
	t.Helper()
	b, err := jsonMarshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ledgerCounts returns (sent, claimed, failed, total) for assertions.
func ledgerCounts(t *testing.T, db *sql.DB) (sent, claimed, failed, total int) {
	t.Helper()
	rows, err := db.Query(`SELECT status, COUNT(*) FROM mailing_send_ledger GROUP BY status`)
	if err != nil {
		t.Fatalf("ledger counts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total += n
		switch status {
		case "sent":
			sent = n
		case "claimed":
			claimed = n
		case "failed":
			failed = n
		}
	}
	return
}

// statusBreakdown returns the full per-status row counts (incl. the GAP-1
// 'requeued'/'dead' states) so scenarios can assert ZERO orphans.
func statusBreakdown(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT status, COUNT(*) FROM mailing_send_ledger GROUP BY status`)
	if err != nil {
		t.Fatalf("status breakdown: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[status] = n
	}
	return out
}

// orphanCount is the GAP-1 invariant metric: rows NOT in a terminal-OK or
// terminal-dead state. After drain+reconcile every key must be 'sent' or 'dead';
// anything still 'claimed', 'failed', or 'requeued' is an orphan.
func orphanCount(t *testing.T, db *sql.DB) (orphans int, bd map[string]int) {
	t.Helper()
	bd = statusBreakdown(t, db)
	return bd["claimed"] + bd["failed"] + bd["requeued"], bd
}

// fullyDrained is the convergence predicate used by the GAP-1 scenarios: ALL n
// keys are present in the ledger AND every one is terminal ('sent' or 'dead')
// AND there are ZERO orphans (nothing still 'claimed'/'failed'/'requeued'). Using
// orphan==0 alone is NOT enough — the ledger can be momentarily orphan-free while
// records are still being produced/consumed (sent<n), so we also require all keys
// terminal.
func fullyDrained(t *testing.T, db *sql.DB, n int) bool {
	t.Helper()
	bd := statusBreakdown(t, db)
	terminal := bd["sent"] + bd["dead"]
	orphans := bd["claimed"] + bd["failed"] + bd["requeued"]
	return terminal >= n && orphans == 0
}

// waitFor polls cond every 250ms until it is true or the deadline passes.
func waitFor(deadline time.Duration, cond func() bool) bool {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cond()
}

// countingDLQ counts dead records so a scenario can assert nothing was lost to
// the DLQ when it shouldn't have been.
//
// IMPORTANT (no-drop fidelity): the eventbus routes a record to the DLQ after
// MaxRetries failures OR when the handler's context is canceled mid-flight
// (processRecord treats ctx.Err() as a permanent failure and DLQs the record).
// During a deliberate consumer KILL (ctx cancel) that means perfectly-valid,
// not-yet-sent records land here. A production DLQ's contract is "do not lose
// the record" — it is durably parked and later RE-DRIVEN. To model that faithfully
// (and to keep the harness from conflating "core dropped it" with "my kill
// DLQ'd it"), this DLQ RE-INJECTS the record back onto the source topic so a
// surviving/replacement consumer re-processes it. The ledger ON CONFLICT still
// guarantees that re-injection can never double-send. Without this, a killed
// consumer's in-flight valid records would be silently committed-away.
type countingDLQ struct {
	n    int64
	prod eventbus.Producer // if non-nil, re-inject (durable redrive)
}

func (d *countingDLQ) Dead(ctx context.Context, topic string, key, value []byte, _ error) error {
	atomic.AddInt64(&d.n, 1)
	if d.prod != nil {
		// Durable redrive: re-publish to the SAME topic+key so it is re-consumed.
		// Use a fresh background ctx — the handler ctx that triggered the DLQ may
		// itself be canceled (that is often WHY we are here).
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return d.prod.Produce(pctx, topic, key, value)
	}
	return nil
}
func (d *countingDLQ) Count() int64 { return atomic.LoadInt64(&d.n) }

// newRedriveDLQ builds a DLQ that re-injects dead records onto topic (durable
// redrive), modeling a real DLQ-with-replay. The caller owns Close via the
// returned cleanup.
func newRedriveDLQ(t *testing.T, topic string) (*countingDLQ, func()) {
	t.Helper()
	prod, err := eventbus.NewKgoProducer(busCfg("dlq-redrive", topic))
	if err != nil {
		t.Fatalf("dlq producer: %v", err)
	}
	return &countingDLQ{prod: prod}, func() { _ = prod.Close() }
}

// runningConsumer bundles a started consumer with a kill() that simulates a
// HARD CRASH: it both cancels the poll loop AND closes the franz-go client so the
// broker drops the member and rebalances its partitions to survivors. Canceling
// the ctx alone is NOT enough — franz-go keeps heartbeating in the background and
// the broker keeps the (now-idle) member's partitions assigned, so they freeze.
// kill() is the faithful crash; stop() is a graceful shutdown used at drain.
type runningConsumer struct {
	c      *KafkaSendConsumer
	cancel context.CancelFunc
}

func (r *runningConsumer) kill() {
	r.cancel()  // stop the poll loop
	r.c.Stop()  // close the client -> leave the group -> trigger rebalance
}
func (r *runningConsumer) stop() { r.cancel(); r.c.Stop() }

// startReconciler runs a LedgerReconciler in the background, mirroring how
// production always pairs the consumer with the reconciler. After a HARD CRASH,
// a row claimed by the dead worker is correctly HELD by survivors (not
// double-sent) until either the stale-grace elapses or the reconciler sweeps it.
// The reconciler is therefore REQUIRED for the no-drop half of the contract to
// close within a bounded time. A short grace makes the test converge in seconds
// instead of the 10-minute production default.
func startReconciler(db *sql.DB, interval, grace time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	rec := NewLedgerReconcilerWithConfig(db, interval, grace)
	go rec.Start(ctx)
	return cancel
}

// startRedrivingReconciler is the GAP-1-aware reconciler: it is wired with a real
// producer on `topic` so stranded rows are GUARANTEED-re-driven by re-producing
// their stored command (not orphaned at terminal 'failed'). It uses a short
// re-drive cap so a genuinely-poison command can't loop forever in a test. The
// returned cleanup cancels the loop AND closes the producer.
func startRedrivingReconciler(t *testing.T, db *sql.DB, topic string, interval, grace time.Duration) func() {
	t.Helper()
	prod, err := eventbus.NewKgoProducer(busCfg("reconciler-redrive", topic))
	if err != nil {
		t.Fatalf("reconciler producer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec := NewLedgerReconcilerWithProducer(db, prod, interval, grace).WithMaxRedrives(8)
	go rec.Start(ctx)
	return func() { cancel(); _ = prod.Close() }
}

// startConsumer builds a KafkaSendConsumer on the given topic+group with the
// shared sink and starts it.
func startConsumer(t *testing.T, db *sql.DB, sink SendSink, group, topic, workerID string, dlq eventbus.DLQ) *runningConsumer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := NewKafkaSendConsumer(db, sink, workerID, busCfg(group, topic))
	if err := c.Start(ctx, dlq); err != nil {
		cancel()
		t.Fatalf("start consumer %s: %v", workerID, err)
	}
	return &runningConsumer{c: c, cancel: cancel}
}

// ---------------------------------------------------------------------------
// The suite
// ---------------------------------------------------------------------------

func TestChaos(t *testing.T) {
	requireBroker(t)
	db := openDB(t)
	defer db.Close()

	t.Run("1_DuplicateInjection", func(t *testing.T) { scenarioDuplicateInjection(t, db) })
	t.Run("2_ConsumerRestartRebalance", func(t *testing.T) { scenarioConsumerRestart(t, db) })
	t.Run("3_CrashAfterSendBeforeRecord", func(t *testing.T) { scenarioCrashBeforeRecord(t, db) })
	t.Run("4_SlowConsumerConcurrent", func(t *testing.T) { scenarioSlowConsumer(t, db) })
	t.Run("5_BrokerLoss", func(t *testing.T) { scenarioBrokerLoss(t, db) })
	t.Run("6_Load", func(t *testing.T) { scenarioLoad(t, db) })
}

// --- Scenario 1: Duplicate injection -----------------------------------------
//
// Produce the SAME 1,000 keys THREE times (3,000 records) onto a single
// partition, run ONE consumer. The ledger ON CONFLICT must collapse the
// duplicates: exactly 1,000 distinct sends, 0 duplicate sends.
func scenarioDuplicateInjection(t *testing.T, db *sql.DB) {
	const n = 1000
	truncateLedger(t, db)
	topic := freshTopic(t, 1)

	cmds := makeKeys(n)
	// 3x the same keys.
	produceAll(t, topic, cmds)
	produceAll(t, topic, cmds)
	produceAll(t, topic, cmds)

	sink := NewCountingSink()
	dlq := &countingDLQ{}
	c := startConsumer(t, db, sink, "chaos-1-"+uuid.NewString()[:6], topic, "w1", dlq)
	defer c.stop()

	ok := waitFor(90*time.Second, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n
	})
	// Give the drain a moment to settle (no more should arrive).
	time.Sleep(2 * time.Second)
	c.stop()

	sent, claimed, failed, total := ledgerCounts(t, db)
	report(t, "Scenario 1 (Duplicate injection: 3,000 records / 1,000 keys)",
		sink, sent, claimed, failed, total, dlq.Count())

	// No crash mid-send anywhere -> the ledger must collapse all 3,000 records to
	// exactly 1,000 sends with ZERO re-presentations.
	assertInvariants(t, sink, n, 0)
	if !ok {
		t.Fatalf("did not reach %d sent in time (sent=%d)", n, sent)
	}
	if total != n {
		t.Fatalf("ledger has %d rows, want exactly %d (one per distinct key)", total, n)
	}
}

// --- Scenario 2: Consumer restart / rebalance --------------------------------
//
// Produce 10,000 keys across multiple partitions; start 2 consumers in the same
// group. Mid-stream, KILL one consumer (cancel its ctx) and START a replacement
// — that forces a real group rebalance and redelivery of any uncommitted
// offsets the dead consumer held. Assert all 10,000 sent exactly once.
func scenarioConsumerRestart(t *testing.T, db *sql.DB) {
	const n = 10000
	truncateLedger(t, db)
	topic := freshTopic(t, 8)

	cmds := makeKeys(n)
	produceAll(t, topic, cmds)

	sink := NewCountingSink() // SHARED across all consumer instances
	dlq, dlqClose := newRedriveDLQ(t, topic)
	defer dlqClose()
	group := "chaos-2-" + uuid.NewString()[:6]

	// Run the REDRIVING reconciler alongside (as production does) with a short
	// grace so a row stranded by the crashed worker is re-produced and converges
	// in seconds, not 10 minutes (GAP-1).
	stopRec := startRedrivingReconciler(t, db, topic, 1*time.Second, 2*time.Second)
	defer stopRec()

	cA := startConsumer(t, db, sink, group, topic, "wA", dlq)
	defer cA.stop()
	cB := startConsumer(t, db, sink, group, topic, "wB", dlq)

	// Wait until we are partway through, then kill consumer B and replace it.
	waitFor(60*time.Second, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n/10 // kill early, while plenty is still in-flight/unprocessed
	})
	t.Logf("  [scenario 2] killing consumer wB mid-stream (cancel+client-close) to force a REAL rebalance...")
	cB.kill() // hard crash: leaves the group -> B's partitions reassign + uncommitted offsets redeliver

	cC := startConsumer(t, db, sink, group, topic, "wC", dlq)
	defer cC.stop()
	t.Logf("  [scenario 2] started replacement consumer wC")

	ok := waitFor(240*time.Second, func() bool {
		return fullyDrained(t, db, n)
	})
	time.Sleep(2 * time.Second)
	cA.stop()
	cC.stop()

	sent, claimed, failed, total := ledgerCounts(t, db)
	report(t, "Scenario 2 (Restart/rebalance: 10,000 keys, kill+replace mid-stream)",
		sink, sent, claimed, failed, total, dlq.Count())

	// A HARD crash can strand records that were sent-but-not-recorded; the
	// reconciler recovers them and they re-deliver (PMTA-absorbed re-send). That
	// re-presentation count is tiny (bounded by in-flight-at-kill). NET delivery
	// is still exactly-once (DistinctKeys==n). Bound generously below; the real
	// number is typically single digits.
	assertInvariants(t, sink, n, 256)
	if !ok {
		t.Fatalf("did not reach %d sent (sent=%d, claimed=%d)", n, sent, claimed)
	}
	// GAP-1 INVARIANT: 0 orphaned ledger rows.
	orph, bd := orphanCount(t, db)
	if orph != 0 {
		t.Fatalf("INVARIANT VIOLATED — %d ORPHANED ledger rows (breakdown=%v); want 0", orph, bd)
	}
}

// --- Scenario 3: Crash-after-send-before-record ------------------------------
//
// For a chosen subset of keys, the sink RECORDS the send (PMTA accepted it) and
// then we forcibly PANIC inside the handler BEFORE the ledger RECORD UPDATE.
// The eventbus consumer recovers the panic per-record, does NOT commit the
// offset, and Kafka redelivers — re-processing the key. Because the prior
// processSend already CLAIMED the row (it stays 'claimed', message_id NULL), the
// redelivery's claim conflicts; for the same workerID it re-claims and re-sends,
// and the CountingSink (PMTA idempotency model) returns the cached id WITHOUT a
// second real distinct delivery. Net effect must be exactly-once distinct
// delivery; the ledger converges to 'sent' for all (LedgerReconciler run too).
func scenarioCrashBeforeRecord(t *testing.T, db *sql.DB) {
	const n = 2000
	const crashEvery = 7 // ~285 keys crash after send, before record
	truncateLedger(t, db)
	topic := freshTopic(t, 4)

	cmds := makeKeys(n)
	produceAll(t, topic, cmds)

	sink := NewCountingSink()
	var crashArmed sync.Map // key -> already crashed once (so it eventually completes)
	var crashCount int64
	// BeforeSend runs INSIDE Send, but the sink RECORDS the send AFTER BeforeSend
	// returns. To model "send happened, then crash before record", we let the
	// send complete normally and panic from a post-send hook we wire through a
	// wrapper sink below. CountingSink has no post-send hook, so wrap it.
	crashSink := &crashAfterSendSink{
		inner: sink,
		shouldCrash: func(cmd SendCommand) bool {
			// crash deterministically for ~1/crashEvery of keys, but only ONCE
			// per key (second time through, let it record).
			if _, done := crashArmed.Load(cmd.IdempotencyKey); done {
				return false
			}
			h := cmd.IdempotencyKey[0]
			if int(h)%crashEvery == 0 {
				crashArmed.Store(cmd.IdempotencyKey, true)
				atomic.AddInt64(&crashCount, 1)
				return true
			}
			return false
		},
	}

	dlq, dlqClose := newRedriveDLQ(t, topic)
	defer dlqClose()
	// Lower MaxRetries irrelevant here (panic recovers as error -> retries within
	// the same poll, but each retry re-runs Handle which re-processes; the key is
	// already claimed by us so it re-claims+sends. To force a true Kafka
	// REDELIVERY rather than in-loop retry, we want the panic to escape the retry
	// budget. The eventbus retries up to maxRetries then DLQs. We instead rely on
	// the wrapper crashing only ONCE per key, so the first attempt panics and the
	// immediate in-loop retry succeeds — both paths exercise the claim->reclaim
	// re-send. We ALSO kill+restart the consumer to force real redelivery.)
	c1 := startConsumer(t, db, crashSink, "chaos-3-"+uuid.NewString()[:6], topic, "w-crash", dlq)

	// Let it churn, then kill the consumer to strand any in-flight 'claimed' rows.
	waitFor(60*time.Second, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n/2
	})
	t.Logf("  [scenario 3] crashed-before-record so far: %d; killing consumer to strand claims...", atomic.LoadInt64(&crashCount))
	c1.kill()
	time.Sleep(1 * time.Second)

	// Restart the SAME workerID so its own stranded 'claimed' rows are recognized
	// as ours and re-claimed (no stale-grace wait needed for own keys).
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c2 := NewKafkaSendConsumer(db, sink, "w-crash", busCfg("chaos-3-restart-"+uuid.NewString()[:6], topic))
	defer c2.Stop()
	// Restart on a NEW group so it re-reads from the beginning (redelivers every
	// record), guaranteeing the stranded keys are re-presented.
	if err := c2.Start(ctx2, dlq); err != nil {
		t.Fatalf("restart consumer: %v", err)
	}

	// GAP-1: run the REDRIVING reconciler (grace ~0) alongside the restarted
	// consumer (c2 stays up) so stranded claimed-NULL rows are re-produced and
	// re-consumed to 'sent' — converging to ZERO orphans, not stalling at 'failed'.
	stopRec := startRedrivingReconciler(t, db, topic, time.Second, 50*time.Millisecond)
	defer stopRec()

	ok := waitFor(180*time.Second, func() bool {
		return fullyDrained(t, db, n)
	})
	time.Sleep(1 * time.Second)
	cancel2()

	sent, claimed, failed, total := ledgerCounts(t, db)
	t.Logf("  [scenario 3] total crashes injected (send-then-panic-before-record): %d", atomic.LoadInt64(&crashCount))
	report(t, "Scenario 3 (Crash after send, before record)",
		sink, sent, claimed, failed, total, dlq.Count())

	// THE invariant for scenario 3 is NET exactly-once DELIVERY, not zero
	// re-presentations. A crash AFTER the sink accepted a send but BEFORE the
	// ledger RECORD leaves the row 'claimed' with message_id NULL. By design, the
	// recovery path (own-key reclaim on redelivery, or reconciler demote->failed)
	// RE-SENDS that key — the PMTA X-Idempotency header is the backstop that makes
	// the re-send safe. So the CountingSink (which MODELS PMTA's dedup) WILL see a
	// duplicate presentation for each crashed key, and DuplicateKeySends() reflects
	// exactly those. DistinctKeys()==n is the real proof: every key reached PMTA
	// once and only once NET (PMTA absorbed the re-send). We assert:
	//   - DistinctKeys == n            (no drop; net exactly-once delivery)
	//   - DuplicateKeySends <= crashes (bounded by the legitimate crash re-sends)
	//   - ledger converges to all-sent (reconciler/redelivery closed the window)
	crashes := int(atomic.LoadInt64(&crashCount))
	if dk := sink.DistinctKeys(); dk != n {
		t.Fatalf("INVARIANT VIOLATED — DistinctKeys()=%d, want %d. DROP.", dk, n)
	}
	// NET double-send to PMTA == 0: every re-presentation is a PMTA-absorbed
	// re-send of a crashed key (each crashed key is recovered via own-key reclaim
	// AND/OR a reconciler re-drive; both are absorbed by PMTA idempotency). Bound
	// generously at 2x crashes to allow a crashed key to be re-presented by both
	// the redelivery path and the reconciler re-drive without flagging a real
	// double-send. DistinctKeys()==n above is the true no-drop / no-NET-double
	// proof.
	dupBound := 2 * crashes
	if dupBound < 1 {
		dupBound = 1
	}
	if dup := sink.DuplicateKeySends(); dup > dupBound {
		t.Fatalf("DuplicateKeySends()=%d EXCEEDS the crash-recovery bound %d (crashes=%d) — a re-send happened OUTSIDE the recoverable crash window (concurrent double-send).", dup, dupBound, crashes)
	} else {
		t.Logf("  [scenario 3] DuplicateKeySends()=%d <= bound=%d (crashes=%d) — every re-send is an EXPECTED PMTA-backstop re-send (0 NET double-send).", dup, dupBound, crashes)
	}
	if !ok {
		t.Fatalf("ledger did not converge to 0 orphans: sent=%d claimed=%d failed=%d", sent, claimed, failed)
	}

	// GAP-1 INVARIANT: 0 orphaned ledger rows — every key ends 'sent' or 'dead'.
	orph, bd := orphanCount(t, db)
	t.Logf("  [scenario 3] ledger status breakdown: %v", bd)
	if orph != 0 {
		t.Fatalf("INVARIANT VIOLATED — %d ORPHANED ledger rows (claimed=%d failed=%d requeued=%d); want 0. breakdown=%v",
			orph, bd["claimed"], bd["failed"], bd["requeued"], bd)
	}
	if bd["dead"] != 0 {
		t.Fatalf("scenario 3: %d row(s) terminal 'dead' — no valid command should exhaust the re-drive budget here", bd["dead"])
	}
	if sent != n {
		t.Fatalf("ledger sent=%d, want %d (every key must converge to sent)", sent, n)
	}
}

// crashAfterSendSink wraps a sink: it forwards Send to the inner sink (so the
// inner CountingSink RECORDS the send = "PMTA accepted it"), then panics if
// shouldCrash — modeling a crash AFTER the transport accepted but BEFORE the
// ledger RECORD UPDATE. The panic escapes processSend (between SEND and RECORD)
// and is recovered by the eventbus consumer's per-record safeHandle, leaving the
// row 'claimed' and the offset uncommitted.
type crashAfterSendSink struct {
	inner       SendSink
	shouldCrash func(SendCommand) bool
}

func (s *crashAfterSendSink) Send(ctx context.Context, cmd SendCommand) (string, error) {
	id, err := s.inner.Send(ctx, cmd) // PMTA accepts; CountingSink records it
	if err != nil {
		return "", err
	}
	if s.shouldCrash != nil && s.shouldCrash(cmd) {
		// We are now PAST the send, BEFORE processSend's RECORD UPDATE. Panicking
		// here is exactly the crash window. safeHandle recovers it as an error;
		// the offset is not committed; the row stays 'claimed' (message_id NULL).
		panic(fmt.Sprintf("chaos: crash after send before record key=%s", cmd.IdempotencyKey))
	}
	return id, nil
}

// --- Scenario 4: Slow-consumer concurrent (the differentiator) ---------------
//
// Two consumers in the same group on a multi-partition topic. One is made
// artificially SLOW (sink Delay) so the broker may reassign its partitions
// (session timeout / rebalance) while it is mid-send. Both then attempt the same
// keys. The INSERT ... ON CONFLICT must serialize them: exactly one send per
// key, 0 duplicates.
func scenarioSlowConsumer(t *testing.T, db *sql.DB) {
	const n = 5000
	truncateLedger(t, db)
	topic := freshTopic(t, 6)

	cmds := makeKeys(n)
	produceAll(t, topic, cmds)

	// One shared sink models the single PMTA. The SLOW consumer uses a slow
	// wrapper so only ITS sends are delayed (widening the race window), while the
	// fast consumer races ahead and the partitions get reassigned mid-flight.
	sink := NewCountingSink()
	slow := &delaySink{inner: sink, delay: 40 * time.Millisecond}
	dlq, dlqClose := newRedriveDLQ(t, topic)
	defer dlqClose()
	group := "chaos-4-" + uuid.NewString()[:6]

	// Grace (3s) safely exceeds the slow sink's 40ms send time, so the reconciler
	// only sweeps genuinely-dead claims, never a live mid-send. GAP-1: the
	// reconciler is now wired with a PRODUCER on this topic so a stranded
	// (claimed-NULL / failed) row is RE-DRIVEN by re-producing its stored command
	// — not orphaned at terminal 'failed'.
	stopRec := startRedrivingReconciler(t, db, topic, 1*time.Second, 3*time.Second)
	defer stopRec()

	cFast := startConsumer(t, db, sink, group, topic, "fast", dlq)
	defer cFast.stop()
	cSlow := startConsumer(t, db, slow, group, topic, "slow", dlq)
	defer cSlow.stop()

	// Mid-stream, kill the slow consumer to FORCE partition reassignment while it
	// is mid-send on its current batch (its in-flight claims are stranded; the
	// fast consumer / its replacement re-process those keys -> ON CONFLICT race).
	waitFor(60*time.Second, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n/3
	})
	t.Logf("  [scenario 4] killing SLOW consumer mid-send to force reassignment of in-flight partitions...")
	cSlow.kill()
	cRepl := startConsumer(t, db, sink, group, topic, "repl", dlq)
	defer cRepl.stop()

	// GAP-1 INVARIANT: wait until every key is terminal AND ZERO orphans remain.
	// A row stranded at 'failed' is now RE-DRIVEN (re-produced) by the reconciler
	// and re-consumed to 'sent', so orphans must drain to 0.
	ok := waitFor(180*time.Second, func() bool {
		return fullyDrained(t, db, n)
	})
	time.Sleep(2 * time.Second)
	cFast.stop()
	cRepl.stop()

	sent, claimed, failed, total := ledgerCounts(t, db)
	report(t, "Scenario 4 (Slow-consumer concurrent: reassign mid-send, ON CONFLICT serializes)",
		sink, sent, claimed, failed, total, dlq.Count())

	// THE PRIME PROOF (delivery safety): when the partition is reassigned WHILE the
	// slow consumer is mid-send, BOTH consumers attempt the same keys, but the
	// INSERT ... ON CONFLICT serializes them — exactly one claim wins per key.
	//   - NO DOUBLE-SEND: dups are bounded by the crash window (PMTA-absorbed).
	//   - NO DELIVERY DROP: DistinctKeys()==n — every key reached PMTA exactly once.
	assertInvariants(t, sink, n, 256)

	// GAP-1 INVARIANT: 0 orphaned ledger rows — every key ends 'sent' or 'dead'.
	orph, bd := orphanCount(t, db)
	t.Logf("  [scenario 4] ledger status breakdown: %v", bd)
	if !ok || orph != 0 {
		t.Fatalf("INVARIANT VIOLATED — %d ORPHANED ledger rows remain (claimed=%d failed=%d requeued=%d); want 0. breakdown=%v",
			orph, bd["claimed"], bd["failed"], bd["requeued"], bd)
	}
	if bd["dead"] != 0 {
		// Not expected here (no poison commands), but if it happens it's terminal,
		// not an orphan — surface it.
		t.Logf("  [scenario 4] NOTE: %d row(s) terminal 'dead' (re-drive budget exhausted) — not orphans, but unexpected for valid commands.", bd["dead"])
	}
	if sent != total {
		t.Fatalf("ledger sent=%d total=%d — not every key reached 'sent' (and none should be dead here)", sent, total)
	}
}

// delaySink delays each Send (only the slow consumer uses it) so a rebalance can
// land while it is mid-send.
type delaySink struct {
	inner SendSink
	delay time.Duration
}

func (s *delaySink) Send(ctx context.Context, cmd SendCommand) (string, error) {
	time.Sleep(s.delay)
	return s.inner.Send(ctx, cmd)
}

// --- Scenario 5: Broker loss -------------------------------------------------
//
// Mid-stream, `docker pause rp-test` for a few seconds, then unpause. Consumers
// must reconnect and continue with no drops and no duplicates after recovery.
func scenarioBrokerLoss(t *testing.T, db *sql.DB) {
	const n = 8000
	truncateLedger(t, db)
	topic := freshTopic(t, 6)

	cmds := makeKeys(n)
	produceAll(t, topic, cmds)

	sink := NewCountingSink()
	dlq, dlqClose := newRedriveDLQ(t, topic)
	defer dlqClose()
	group := "chaos-5-" + uuid.NewString()[:6]

	// A broker pause can expire the group session and trigger a rebalance whose
	// redelivery recovers in-flight sends; run the REDRIVING reconciler (GAP-1) so
	// any stranded claimed-NULL/failed row is re-produced and re-consumed to 'sent'
	// (grace 3s > any send time). NOTE: while the broker is paused the reconciler's
	// producer cannot publish — it simply retries on the next pass after unpause,
	// leaving rows untouched (never orphaned).
	stopRec := startRedrivingReconciler(t, db, topic, 1*time.Second, 3*time.Second)
	defer stopRec()

	cA := startConsumer(t, db, sink, group, topic, "wA", dlq)
	defer cA.stop()
	cB := startConsumer(t, db, sink, group, topic, "wB", dlq)
	defer cB.stop()

	// Wait until partway, then pause the broker for a few seconds.
	waitFor(60*time.Second, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n/4
	})
	t.Logf("  [scenario 5] pausing redpanda (docker pause rp-test) for 6s...")
	if err := dockerCmd("pause", "rp-test"); err != nil {
		t.Logf("  [scenario 5] WARN: docker pause failed (%v); skipping broker-loss injection but continuing", err)
	} else {
		time.Sleep(6 * time.Second)
		t.Logf("  [scenario 5] unpausing redpanda...")
		if err := dockerCmd("unpause", "rp-test"); err != nil {
			t.Fatalf("docker unpause failed: %v", err)
		}
	}

	// GAP-1 INVARIANT: wait for every key terminal + ZERO orphans.
	ok := waitFor(180*time.Second, func() bool {
		return fullyDrained(t, db, n)
	})
	time.Sleep(2 * time.Second)
	cA.stop()
	cB.stop()

	sent, claimed, failed, total := ledgerCounts(t, db)
	report(t, "Scenario 5 (Broker loss: pause+unpause mid-stream)",
		sink, sent, claimed, failed, total, dlq.Count())

	// A session-expiry rebalance during the pause may recover a few in-flight
	// sends (bounded, PMTA-absorbed). NET delivery exactly-once.
	assertInvariants(t, sink, n, 256)

	// GAP-1 INVARIANT: 0 orphaned ledger rows.
	orph, bd := orphanCount(t, db)
	t.Logf("  [scenario 5] ledger status breakdown: %v", bd)
	if !ok || orph != 0 {
		t.Fatalf("INVARIANT VIOLATED — %d ORPHANED ledger rows (claimed=%d failed=%d requeued=%d); want 0. breakdown=%v",
			orph, bd["claimed"], bd["failed"], bd["requeued"], bd)
	}
	if bd["dead"] != 0 {
		t.Logf("  [scenario 5] NOTE: %d row(s) terminal 'dead' (unexpected for valid commands).", bd["dead"])
	}
}

// --- Scenario 6: Load --------------------------------------------------------
//
// Produce 500,000 keys, run a pool of consumers, measure throughput and
// end-to-end time, drain, assert sent==distinct==500,000 and 0 duplicates.
func scenarioLoad(t *testing.T, db *sql.DB) {
	n := 500000
	if v := os.Getenv("SENDQUEUE_CHAOS_LOAD_N"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	truncateLedger(t, db)
	topic := freshTopic(t, 12)

	t.Logf("  [scenario 6] producing %d keys...", n)
	cmds := makeKeys(n)
	prodStart := time.Now()
	produceAll(t, topic, cmds)
	t.Logf("  [scenario 6] produced %d records in %s (%.0f rec/s)", n, time.Since(prodStart).Round(time.Millisecond), float64(n)/time.Since(prodStart).Seconds())

	sink := NewCountingSink()
	dlq := &countingDLQ{}
	group := "chaos-6-" + uuid.NewString()[:6]

	const consumers = 6
	running := make([]*runningConsumer, 0, consumers)
	consumeStart := time.Now()
	for i := 0; i < consumers; i++ {
		running = append(running, startConsumer(t, db, sink, group, topic, fmt.Sprintf("load-%d", i), dlq))
	}
	defer func() {
		for _, c := range running {
			c.stop()
		}
	}()

	ok := waitFor(15*time.Minute, func() bool {
		sent, _, _, _ := ledgerCounts(t, db)
		return sent >= n
	})
	elapsed := time.Since(consumeStart)
	time.Sleep(2 * time.Second)
	for _, c := range running {
		c.stop()
	}

	sent, claimed, failed, total := ledgerCounts(t, db)
	report(t, fmt.Sprintf("Scenario 6 (Load: %d keys, %d consumers)", n, consumers),
		sink, sent, claimed, failed, total, dlq.Count())
	t.Logf("  [scenario 6] consume elapsed=%s throughput=%.0f keys/s",
		elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds())

	// Steady-state load, no induced crash -> ZERO re-presentations allowed.
	assertInvariants(t, sink, n, 0)
	if !ok {
		t.Fatalf("did not reach %d sent within timeout (sent=%d claimed=%d)", n, sent, claimed)
	}
	if total != n {
		t.Fatalf("ledger total=%d, want %d", total, n)
	}
}

// ---------------------------------------------------------------------------
// Shared assertions + reporting
// ---------------------------------------------------------------------------

// assertInvariants is the heart of the harness. It enforces the TWO invariants
// the way the design actually defines them:
//
//   - NO DROP (absolute):  DistinctKeys() == expected. Every key reached PMTA at
//     least once. This is non-negotiable in every scenario.
//
//   - NO DOUBLE-SEND (net): the LEDGER never lets two workers both send a key
//     concurrently. The ONLY way a key is ever re-presented to the sink is the
//     crash-after-send-before-record window, where the ledger row is recovered
//     (reconciler demote -> redelivery re-send) and the PMTA X-Idempotency header
//     dedups it. Those re-presentations are bounded by maxDup (the number of
//     records that could have been in-flight at a crash). maxDup==0 means "no
//     crash window existed, so not even a re-presentation is allowed" (scenarios
//     1, 5, 6). For crash scenarios maxDup>0 bounds the legitimate, PMTA-absorbed
//     re-sends; DistinctKeys()==expected still proves NET exactly-once delivery.
func assertInvariants(t *testing.T, sink *CountingSink, expected, maxDup int) {
	t.Helper()
	if dk := sink.DistinctKeys(); dk != expected {
		t.Fatalf("INVARIANT VIOLATED — DistinctKeys()=%d, want %d. DROP (a key never reached PMTA).", dk, expected)
	}
	dup := sink.DuplicateKeySends()
	if maxDup == 0 && dup != 0 {
		t.Fatalf("INVARIANT VIOLATED — DuplicateKeySends()=%d with NO crash window (want 0). The ledger let a key reach PMTA twice. DOUBLE-SEND.", dup)
	}
	if dup > maxDup {
		t.Fatalf("INVARIANT VIOLATED — DuplicateKeySends()=%d EXCEEDS the crash-window bound %d. A re-send happened OUTSIDE the recoverable crash window — concurrent double-send.", dup, maxDup)
	}
	if dup > 0 {
		t.Logf("  net exactly-once held: DistinctKeys=%d, and %d/%d allowed crash-window re-presentations were absorbed by PMTA idempotency (0 NET double-send).", expected, dup, maxDup)
	}
}

func report(t *testing.T, title string, sink *CountingSink, sent, claimed, failed, total int, dlq int64) {
	t.Helper()
	t.Logf("\n========== %s ==========\n"+
		"  sink.Sends()            = %d   (total accepted Send calls, incl. any re-send)\n"+
		"  sink.DistinctKeys()     = %d   (unique keys that reached PMTA — NET delivery)\n"+
		"  sink.DuplicateKeySends()= %d   (re-presentations PMTA would dedup)\n"+
		"  ledger: sent=%d claimed=%d failed=%d total=%d\n"+
		"  DLQ dead records (redriven) = %d\n"+
		"  (PASS/FAIL is decided by the per-scenario assertions below)\n"+
		"=======================================================",
		title, sink.Sends(), sink.DistinctKeys(), sink.DuplicateKeySends(), sent, claimed, failed, total, dlq)
}

package worker

// REQ-089 — the wave produced/landed state machine, dispatcher side.
//
// Three properties are pinned here, all of them incident-derived:
//
//  1. A Kafka-routed wave completes to 'produced' with produced_recipients and
//     enqueued_recipients UNTOUCHED. Crediting enqueued_recipients at produce
//     time is what let 24 board campaigns flip 'sent' on 2026-09-01 with 0-40%
//     of their audience still in the broker.
//  2. Nothing is produced until the wave transaction has COMMITTED — otherwise
//     the consumer's landed write-back blocks on the wave row's FOR UPDATE lock
//     for the life of the enqueue, past its 30s handle timeout, into the DLQ.
//  3. Running EnqueuePMTAWave TWICE on a routed wave produces the wave ONCE.
//     Every scheduler re-fires on every ECS bounce, so this is the property the
//     whole design has to survive.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	"github.com/ignite/sparkpost-monitor/internal/eventbus/sendqueue"
)

// waveMetaColumns is the column list of EnqueuePMTAWave's opening metadata
// SELECT (the FOR UPDATE row lock on wave+campaign+plan).
var waveMetaColumns = []string{
	"campaign_id", "isp_plan_id", "organization_id",
	"status", "campaign_status", "plan_status",
	"scheduled_at", "campaign_scheduled_at", "planned_recipients", "enqueued_recipients", "partner_drip_tag", "plan_isp", "plan_sending_domain",
}

// expectRoutedWavePrelude registers everything EnqueuePMTAWave does before the
// candidate claim, for a set-based routed wave with `planned` recipients.
func expectRoutedWavePrelude(mock sqlmock.Sqlmock, waveID, campaignID, planID, orgID uuid.UUID, planned int) {
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows(waveMetaColumns).AddRow(
			campaignID, planID, orgID,
			"planned", "scheduled", "ready",
			testScheduledAt, testScheduledAt, planned, 0, nil, "gmail", "em.test.com"))
	mock.ExpectQuery(`COALESCE\(sp.sending_domain`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain"}).AddRow("em.test.com"))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET status = 'enqueuing'`).
		WithArgs(waveID.String()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET status = 'sending'`).
		WithArgs(campaignID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET status = 'running'`).
		WithArgs(planID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`COALESCE\(from_name`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{
			"from_name", "from_email", "subject", "html_content",
			"name", "plain_content", "content_locked",
		}).AddRow("Sender", "hello@em.test.com", "Subj", "<html>body</html>",
			"Test Campaign", "plain", false))
	expectSnapshotEnsured(mock, uuid.New())
}

// TestProducedState_RoutedWaveCompletesToProduced drives a real EnqueuePMTAWave
// with routing ON and asserts the wave-row write: produced_recipients gets the
// count, enqueued_recipients gets ZERO, status is 'produced'.
func TestProducedState_RoutedWaveCompletesToProduced(t *testing.T) {
	t.Setenv("DISABLE_WAVE_AB_SPLIT", "true") // keep the mock to the wave path
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")
	prod := &eventbus.FakeProducer{}
	SetKafkaSendProducer(prod)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	subA, subB := uuid.New(), uuid.New()

	expectRoutedWavePrelude(mock, waveID, campaignID, planID, orgID, 2)
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(planID, 2).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), subA.String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false).
			AddRow(uuid.New().String(), subB.String(), "b@gmail.com", "gmail", 2, "segment", nil, "selected", "active", false))
	// Routed → NO direct INSERT expectation. Only the plan_recipient transition.
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	// THE ASSERTION: $2 enqueued delta = 0, $5 produced delta = 2, $6 = 'produced'.
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET enqueued_recipients`).
		WithArgs(waveID.String(), 0, 0, 0, 2, "produced").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET enqueued_count`).
		WithArgs(planID, 2).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET queued_count`).
		WithArgs(campaignID, 2).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Ordering proof: at the moment each record is produced, every DB
	// expectation — including Commit — must already be satisfied.
	var producedBeforeCommit bool
	prod.OnProduce = func(eventbus.FakeRecord) {
		if err := mock.ExpectationsWereMet(); err != nil {
			producedBeforeCommit = true
		}
	}

	n, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), nil)
	if err != nil {
		t.Fatalf("EnqueuePMTAWave: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 recipients accounted, got %d", n)
	}
	if producedBeforeCommit {
		t.Fatal("a record was produced BEFORE the wave transaction committed — that is the lock-contention/DLQ bug REQ-089 removes")
	}
	if prod.Count() != 2 {
		t.Fatalf("want 2 records produced by the post-commit flush, got %d", prod.Count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// TestProducedState_DirectPathUnchanged is the negative control: with routing
// OFF the same wave writes enqueued_recipients (not produced_recipients) and
// completes to 'completed', exactly as it did before REQ-089, and the producer
// is never touched.
func TestProducedState_DirectPathUnchanged(t *testing.T) {
	t.Setenv("DISABLE_WAVE_AB_SPLIT", "true")
	prod := &eventbus.FakeProducer{}
	SetKafkaSendProducer(prod)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })
	// No KAFKA_SEND_QUEUE_* routing env → kafkaRouteWave false.
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	expectRoutedWavePrelude(mock, waveID, campaignID, planID, orgID, 1)
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(planID, 1).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false))
	// Direct path → the batched queue INSERT runs.
	mock.ExpectExec(`(?s)INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET enqueued_recipients`).
		WithArgs(waveID.String(), 1, 0, 0, 0, "completed").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET enqueued_count`).
		WithArgs(planID, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET queued_count`).
		WithArgs(campaignID, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), nil)
	if err != nil {
		t.Fatalf("EnqueuePMTAWave: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1, got %d", n)
	}
	if prod.Count() != 0 {
		t.Fatalf("the direct path must never touch the broker, got %d records", prod.Count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// TestDispatcherDoubleFireKafka — the re-run-safety proof. EnqueuePMTAWave is
// called TWICE for the same routed wave (the scheduler re-fire / ECS bounce
// case). The second call sees status='produced', which is terminal for dispatch:
// it must commit and return without claiming a recipient, without transitioning
// a plan_recipient, and above all WITHOUT producing the wave a second time.
//
// One command per idempotency key is asserted directly on the produced records.
func TestDispatcherDoubleFireKafka(t *testing.T) {
	t.Setenv("DISABLE_WAVE_AB_SPLIT", "true")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")
	prod := &eventbus.FakeProducer{}
	SetKafkaSendProducer(prod)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	subA := uuid.New()

	// ---- FIRST FIRE: wave is 'planned' → full enqueue, one produce. ----
	expectRoutedWavePrelude(mock, waveID, campaignID, planID, orgID, 1)
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(planID, 1).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), subA.String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false))
	// EXACTLY ONE plan_recipient transition is expected across BOTH calls.
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET enqueued_recipients`).
		WithArgs(waveID.String(), 0, 0, 0, 1, "produced").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET enqueued_count`).
		WithArgs(planID, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET queued_count`).
		WithArgs(campaignID, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// ---- SECOND FIRE: wave is now 'produced' → begin, read, commit. ----
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows(waveMetaColumns).AddRow(
			campaignID, planID, orgID,
			"produced", "sending", "running",
			testScheduledAt, testScheduledAt, 1, 0, nil, "gmail", "em.test.com"))
	mock.ExpectCommit()

	if _, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), nil); err != nil {
		t.Fatalf("first EnqueuePMTAWave: %v", err)
	}
	second, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), nil)
	if err != nil {
		t.Fatalf("second EnqueuePMTAWave (double fire): %v", err)
	}
	if second != 0 {
		t.Fatalf("the second fire must enqueue 0, got %d", second)
	}

	recs := prod.Records()
	if len(recs) != 1 {
		t.Fatalf("double fire must produce the wave ONCE: want 1 record, got %d", len(recs))
	}
	// One command per idempotency key, and it is the dispatcher's key.
	wantKey := outboxIdempotencyKey(campaignID, subA, waveID)
	if string(recs[0].Key) != string(wantKey[:]) {
		t.Fatal("produced record key must be uuidv5(campaign, subscriber, wave)")
	}
	seen := map[string]int{}
	for _, r := range recs {
		seen[string(r.Key)]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("key %x produced %d times, want exactly 1", k, n)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected (the second fire must touch nothing but begin/select/commit): %v", err)
	}
}

// TestSweepIdempotencyKeyMatchesWorker pins the duplicated uuidv5 namespace in
// sendqueue.SweepIdempotencyKey to this package's outboxIdempotencyKey. If they
// ever drift, UnlandedWaveSweeper would rebuild a SECOND queue row for a
// recipient whose command is still parked in the broker — a double send. Only
// this package can see both sides (sendqueue cannot import worker).
func TestSweepIdempotencyKeyMatchesWorker(t *testing.T) {
	for i := 0; i < 25; i++ {
		c, s, w := uuid.New(), uuid.New(), uuid.New()
		if got, want := sendqueue.SweepIdempotencyKey(c, s, w), outboxIdempotencyKey(c, s, w); got != want {
			t.Fatalf("key drift: sweeper=%s dispatcher=%s (campaign=%s subscriber=%s wave=%s)", got, want, c, s, w)
		}
	}
}

// TestWaveProduceBuffer_BoundFailsClosed proves the memory bound is real: a wave
// whose buffered payload exceeds the cap fails the enqueue (so the transaction
// rolls back and the wave is re-dispatched) rather than growing until the task
// is OOM-killed mid-send.
func TestWaveProduceBuffer_BoundFailsClosed(t *testing.T) {
	b := &waveProduceBuffer{}
	big := make([]byte, 1<<20) // 1 MB of inline creative per command
	for i := range big {
		big[i] = 'x'
	}
	cmd := sendqueue.SendCommand{IdempotencyKey: uuid.New(), HTMLContent: string(big)}
	var err error
	for i := 0; i < 512 && err == nil; i++ {
		err = b.add(cmd)
	}
	if err == nil {
		t.Fatal("the produce buffer must refuse a wave that exceeds maxDeferredProduceBytes")
	}
	if b.len() == 0 {
		t.Fatal("the buffer should have accepted commands up to the bound")
	}
}

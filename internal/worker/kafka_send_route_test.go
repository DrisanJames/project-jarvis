package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// THE DARK-DEFAULT GUARD. With no producer wired and no routing env set,
// kafkaRouteWave returns false → EnqueuePMTAWave routes nothing → the direct
// INSERT runs → byte-identical to today. This test pins that default.
func TestKafkaRouteWave_DefaultsOff(t *testing.T) {
	SetKafkaSendProducer(nil) // ensure dark
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	// No KAFKA_SEND_QUEUE_* env set in the test process.
	if kafkaRouteWave("any-wave", "any-campaign") {
		t.Fatal("kafkaRouteWave must default to FALSE (dark) with no producer and no routing env")
	}
	if KafkaSendQueueEnabled() {
		t.Fatal("KafkaSendQueueEnabled must default to FALSE with no routing env")
	}
}

// Even if a routing env is set, with NO producer wired kafkaRouteWave stays
// false (mis-config safety: never route when there is no transport to route to).
func TestKafkaRouteWave_NoProducer_StaysOff(t *testing.T) {
	SetKafkaSendProducer(nil)
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "1")
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	if kafkaRouteWave("w", "c") {
		t.Fatal("kafkaRouteWave must be FALSE when no producer is wired, even with KAFKA_SEND_QUEUE_ALL=1")
	}
	// But the wiring gate IS on (so cmd/server would install the producer).
	if !KafkaSendQueueEnabled() {
		t.Fatal("KafkaSendQueueEnabled must be TRUE when KAFKA_SEND_QUEUE_ALL=1")
	}
}

// Allowlist routing: with a producer wired and the wave in KAFKA_SEND_QUEUE_WAVES,
// the wave routes; a wave NOT in the list does not. Campaign allowlist likewise.
func TestKafkaRouteWave_Allowlists(t *testing.T) {
	SetKafkaSendProducer(&eventbus.FakeProducer{})
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "wave-A, wave-B")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "camp-Z")

	if !kafkaRouteWave("wave-A", "other") {
		t.Fatal("wave-A is in the wave allowlist → must route")
	}
	if !kafkaRouteWave("wave-B", "other") {
		t.Fatal("wave-B is in the wave allowlist → must route")
	}
	if kafkaRouteWave("wave-C", "other") {
		t.Fatal("wave-C is NOT allowlisted → must NOT route")
	}
	if !kafkaRouteWave("other", "camp-Z") {
		t.Fatal("camp-Z is in the campaign allowlist → must route")
	}
	if kafkaRouteWave("other", "camp-Y") {
		t.Fatal("camp-Y is NOT allowlisted → must NOT route")
	}
}

// KAFKA_SEND_QUEUE_ALL routes every wave (with a producer wired).
func TestKafkaRouteWave_AllSwitch(t *testing.T) {
	SetKafkaSendProducer(&eventbus.FakeProducer{})
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "true")
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	if !kafkaRouteWave("any", "any") {
		t.Fatal("KAFKA_SEND_QUEUE_ALL=true must route every wave")
	}
}

// PRODUCER-FAILURE FALLBACK. A routed set-based wave whose producer FAILS must
// fall back to the direct INSERT — no recipient is dropped. We wire a failing
// producer, route the wave, and assert the direct INSERT into
// mailing_campaign_queue still runs for the (single) recipient.
func TestEnqueueWaveSetBased_RoutedProducerFailure_FallsBackToInsert(t *testing.T) {
	failing := &eventbus.FakeProducer{Err: context.DeadlineExceeded}
	SetKafkaSendProducer(failing)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 1
	p.routeToKafka = true // routed → producer attempted first

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false))

	// Producer failed → the recipient FALLS BACK to the direct INSERT. The
	// queued-transition UPDATE always follows.
	mock.ExpectExec(`INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	queued, skipped, _, _, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p)
	if err != nil {
		t.Fatalf("enqueueWaveSetBased (fallback): %v", err)
	}
	if queued != 1 || skipped != 0 {
		t.Fatalf("want queued=1 skipped=0, got queued=%d skipped=%d", queued, skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (the direct INSERT fallback must have run): %v", err)
	}
}

// SUCCESSFUL ROUTING. A routed set-based wave whose producer SUCCEEDS must NOT
// run the direct INSERT (the row went to Kafka) — only the plan_recipient
// queued-transition UPDATE follows. We assert no INSERT expectation is consumed
// and the producer captured exactly one record.
func TestEnqueueWaveSetBased_RoutedProducerSuccess_NoDirectInsert(t *testing.T) {
	ok := &eventbus.FakeProducer{}
	SetKafkaSendProducer(ok)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 1
	p.routeToKafka = true

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), uuid.New().String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false))

	// NO INSERT expectation: the row went to Kafka. Only the queued UPDATE runs.
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	queued, _, _, _, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p)
	if err != nil {
		t.Fatalf("enqueueWaveSetBased (routed): %v", err)
	}
	if queued != 1 {
		t.Fatalf("want queued=1, got %d", queued)
	}
	if ok.Count() != 1 {
		t.Fatalf("producer must have captured exactly 1 routed command, got %d", ok.Count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet (a direct INSERT must NOT have run): %v", err)
	}
}

// Sanity: the produced record value is a SendCommand that round-trips back to the
// SAME idempotency key the dispatcher computes, so the QueueWriterConsumer would
// INSERT the identical row.
func TestRoutedCommand_CarriesDispatcherIdempotencyKey(t *testing.T) {
	ok := &eventbus.FakeProducer{}
	SetKafkaSendProducer(ok)
	t.Cleanup(func() { SetKafkaSendProducer(nil) })

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := setBasedTestParams()
	p.remaining = 1
	p.routeToKafka = true
	subID := uuid.New()

	mock.ExpectBegin()
	expectSnapshotEnsured(mock, uuid.New())
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(claimColumns).
			AddRow(uuid.New().String(), subID.String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected", "active", false))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	tx, _ := db.Begin()
	if _, _, _, _, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p); err != nil {
		t.Fatalf("enqueueWaveSetBased: %v", err)
	}

	recs := ok.Records()
	if len(recs) != 1 {
		t.Fatalf("want 1 produced record, got %d", len(recs))
	}
	wantKey := outboxIdempotencyKey(p.campaignID, subID, p.waveUUID)
	// The Kafka record KEY is the idempotency-key bytes.
	if string(recs[0].Key) != string(wantKey[:]) {
		t.Fatal("produced record key must be the dispatcher's idempotency-key bytes")
	}
}

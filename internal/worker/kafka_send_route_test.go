package worker

import (
	"context"
	"strings"
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

// SK-5 HARD SEND PATH: a routed set-based wave whose producer FAILS must NOT
// fall back to a direct INSERT — it must return an error so the surrounding
// transaction rolls back and the wave is re-dispatched (no recipient dropped;
// the rows are written later, via Kafka). We wire a failing producer, route the
// wave, and assert: (a) an error is returned, and (b) NO direct INSERT into
// mailing_campaign_queue runs (no INSERT expectation is registered, so the mock
// would error if one were attempted).
func TestEnqueueWaveSetBased_RoutedProducerFailure_NoFallbackErrors(t *testing.T) {
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

	// NO INSERT expectation and NO queued-UPDATE expectation: produce failed, so
	// the function returns the error immediately. If any INSERT were attempted the
	// sqlmock would fail the test (unexpected exec).

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	queued, _, _, _, err := enqueueWaveSetBased(context.Background(), tx, db, nil, p)
	if err == nil {
		t.Fatal("routed produce-failure MUST return an error (Kafka is the hard send path; no fallback)")
	}
	if queued != 0 {
		t.Fatalf("want queued=0 on produce-failure, got %d", queued)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected (no direct INSERT must run on produce-failure): %v", err)
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

// SK-5 HARD SEND PATH (legacy row-at-a-time path): a routed wave whose producer
// FAILS must return an error and run NO direct INSERT — same no-fallback contract
// as the set-based path. We drive enqueueWaveRowAtATime (DISABLE_SETBASED_ENQUEUE
// path) with a failing producer and a single compliant recipient.
func TestEnqueueWaveRowAtATime_RoutedProducerFailure_NoFallbackErrors(t *testing.T) {
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
	p.routeToKafka = true
	subID := uuid.New()

	mock.ExpectBegin()
	// Legacy claim SELECT (no compliance LEFT JOIN; 8 columns).
	legacyClaimCols := []string{
		"id", "subscriber_id", "email", "recipient_isp", "selection_rank",
		"audience_source_type", "audience_source_id", "status",
	}
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients\s+WHERE isp_plan_id`).
		WithArgs(p.ispPlanID, p.remaining).
		WillReturnRows(sqlmock.NewRows(legacyClaimCols).
			AddRow(uuid.New().String(), subID.String(), "a@gmail.com", "gmail", 1, "segment", nil, "selected"))
	// Per-recipient compliance round trips (both pass): subscriber active, not suppressed.
	mock.ExpectQuery(`SELECT COALESCE\(status, 'active'\) FROM mailing_subscribers`).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("active"))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM mailing_global_suppressions`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// NO INSERT expectation: produce fails → error returned, no fallback INSERT.

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	queued, _, _, _, err := enqueueWaveRowAtATime(context.Background(), tx, nil, p)
	if err == nil {
		t.Fatal("routed produce-failure MUST return an error (no fallback INSERT)")
	}
	if queued != 0 {
		t.Fatalf("want queued=0 on produce-failure, got %d", queued)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected (no direct INSERT must run on produce-failure): %v", err)
	}
}

// SK-5 BLOCK: a bypass enqueue path (worker.EnqueueCampaign, the legacy
// campaign-processor path) must error WITHOUT touching the DB when Kafka routing
// is ON. We turn routing on via env and assert the kafka-route block fires before
// any DB access (db is nil — if the block did not fire, the function would panic
// or error differently).
func TestEnqueueCampaign_BlockedWhenRoutingOn(t *testing.T) {
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "1")
	_, err := EnqueueCampaign(context.Background(), nil, uuid.New().String())
	if err == nil {
		t.Fatal("EnqueueCampaign must be BLOCKED (error) when Kafka routing is ON")
	}
	if !strings.Contains(err.Error(), "kafka-route") {
		t.Fatalf("want a kafka-route block error, got: %v", err)
	}
}

// SK-5 DARK DEFAULT: with routing OFF (no env set), EnqueueCampaign must NOT be
// blocked — it proceeds past the gate (and then fails downstream on the nil DB /
// invalid id), proving the guard is byte-identical-no-op when dark. We assert the
// returned error is NOT the kafka-route block.
func TestEnqueueCampaign_NotBlockedWhenRoutingOff(t *testing.T) {
	// Ensure no routing env leaks in from the process.
	t.Setenv("KAFKA_SEND_QUEUE_ENABLED", "")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")

	// An invalid campaign id makes the function return early AFTER the gate but
	// before any DB use, so we can assert the gate did not fire.
	_, err := EnqueueCampaign(context.Background(), nil, "not-a-uuid")
	if err == nil {
		t.Fatal("expected a downstream error (invalid campaign id), got nil")
	}
	if strings.Contains(err.Error(), "kafka-route") {
		t.Fatalf("guard must be a no-op when routing is OFF; got kafka-route block: %v", err)
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

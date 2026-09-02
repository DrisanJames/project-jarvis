package sendqueue

// REQ-089 — the landed side of the wave state machine.
//
//	LandedCounter*  : the queue writer credits a wave ONLY for rows it actually
//	                  created, and promotes 'produced' -> 'completed' in the same
//	                  statement.
//	SweepUnlanded*  : the DB-side reconciler that replaced LedgerReconciler
//	                  rebuilds rows that were produced but never landed, is a
//	                  no-op on a second run, and refuses to guess at content.

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// A NEW row → exactly one landed write-back for that wave.
func TestLandedCounter_IncrementsOnInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	value, _ := json.Marshal(cmd)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 1)) // a row landed
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_campaign_waves")).
		WithArgs(cmd.WaveID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if ins, _, _ := c.Stats(); ins != 1 {
		t.Fatalf("want inserted=1, got %d", ins)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the landed write-back must run on a NEW row: %v", err)
	}
}

// A CONFLICT (redelivery of a row that already landed) must NOT credit the wave.
// Double-counting here would promote a wave to 'completed' while recipients are
// still in flight — the exact failure landed_recipients exists to prevent.
func TestLandedCounter_DoesNotIncrementOnConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	cmd := sampleCmd()
	value, _ := json.Marshal(cmd)

	// ON CONFLICT DO NOTHING → RowsAffected 0. NO wave UPDATE is registered, so
	// sqlmock reports an unexpected call if one is attempted.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	c := NewQueueWriterConsumer(db, eventbus.Config{})
	if err := c.Handle(context.Background(), nil, value); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, conf, _ := c.Stats(); conf != 1 {
		t.Fatalf("want conflicts=1, got %d", conf)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// The promotion + counter semantics live in ONE statement, under the wave row's
// lock, guarded on status='produced'. Pin the shape: two ECS tasks and the sweep
// all write through this, so a drift here is a silent double-promotion.
func TestLandedCounter_SQLCarriesPromotionGuards(t *testing.T) {
	for _, want := range []string{
		`landed_recipients = landed_recipients \+ 1`,
		`status = CASE WHEN status = 'produced' AND landed_recipients \+ 1 >= produced_recipients`,
		`enqueued_recipients = CASE WHEN status = 'produced'`,
	} {
		if !regexp.MustCompile(want).MatchString(markLandedSQL) {
			t.Fatalf("markLandedSQL missing %q", want)
		}
	}
}

// ── sweepUnlandedWaves ──────────────────────────────────────────────────────

// sweepTestWave registers the candidate-wave query returning ONE produced wave.
func expectCandidateWave(mock sqlmock.Sqlmock, w unlandedWave) {
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_campaign_waves")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "campaign_id", "isp_plan_id", "scheduled_at", "produced_recipients", "landed_recipients",
		}).AddRow(w.id, w.campaignID, w.ispPlanID, w.scheduledAt, w.produced, w.landed))
}

// The core case: a wave produced 2 recipients, 1 landed, 1 is missing. The sweep
// rebuilds exactly the missing one through the SAME queueInsertSQL, then credits
// the wave by 1.
func TestSweepUnlandedWaves_RebuildsMissingRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	w := unlandedWave{
		id: uuid.New(), campaignID: uuid.New(), ispPlanID: uuid.New(),
		scheduledAt: time.Now(), produced: 2, landed: 1,
	}
	subLanded, subMissing := uuid.New(), uuid.New()
	landedKey := SweepIdempotencyKey(w.campaignID, subLanded, w.id)

	expectCandidateWave(mock, w)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_campaign_plan_recipients pr")).
		WillReturnRows(sqlmock.NewRows([]string{
			"subscriber_id", "recipient_isp", "selection_rank", "audience_source_type", "audience_source_id",
		}).
			AddRow(subLanded, "gmail", 1, "segment", "").
			AddRow(subMissing, "gmail", 2, "segment", ""))
	// Only the already-landed key comes back from the queue.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key FROM mailing_campaign_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).AddRow(landedKey))
	// Content resolution: a landed sibling row supplies the snapshot + priority.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content_snapshot_id, priority")).
		WillReturnRows(sqlmock.NewRows([]string{"content_snapshot_id", "priority"}).AddRow(uuid.New(), 5))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(c.subject, '')")).
		WillReturnRows(sqlmock.NewRows([]string{"subject"}).AddRow("Best deals today"))
	// The rebuild — the canonical INSERT, one row.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mailing_campaign_queue")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Credit the wave by exactly the number rebuilt.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_campaign_waves")).
		WithArgs(w.id, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := NewUnlandedWaveSweeper(nil, 0, 0)
	sweepUnlandedWaves(context.Background(), db, s)

	st := s.Stats()
	if st.RowsRebuilt != 1 {
		t.Fatalf("want 1 row rebuilt, got %d", st.RowsRebuilt)
	}
	if st.Errors != 0 {
		t.Fatalf("want 0 errors, got %d", st.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// SECOND RUN INSERTS 0. Same wave, but by now every key is present in the queue
// (the first sweep's rows, or the Kafka records finally landing). The sweep must
// insert nothing — and, since nothing is owed, promote the wave.
func TestSweepUnlandedWaves_SecondRunInsertsZeroAndPromotes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	w := unlandedWave{
		id: uuid.New(), campaignID: uuid.New(), ispPlanID: uuid.New(),
		scheduledAt: time.Now(), produced: 2, landed: 1,
	}
	subA, subB := uuid.New(), uuid.New()

	expectCandidateWave(mock, w)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_campaign_plan_recipients pr")).
		WillReturnRows(sqlmock.NewRows([]string{
			"subscriber_id", "recipient_isp", "selection_rank", "audience_source_type", "audience_source_id",
		}).
			AddRow(subA, "gmail", 1, "segment", "").
			AddRow(subB, "gmail", 2, "segment", ""))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key FROM mailing_campaign_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}).
			AddRow(SweepIdempotencyKey(w.campaignID, subA, w.id)).
			AddRow(SweepIdempotencyKey(w.campaignID, subB, w.id)))
	// NO INSERT expectation and NO content lookups: nothing is missing, so the
	// sweep must not even resolve content. Promotion is the only write.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mailing_campaign_waves w")).
		WithArgs(w.id).
		WillReturnResult(sqlmock.NewResult(0, 1))

	s := NewUnlandedWaveSweeper(nil, 0, 0)
	sweepUnlandedWaves(context.Background(), db, s)

	st := s.Stats()
	if st.RowsRebuilt != 0 {
		t.Fatalf("a second run must insert 0 rows, got %d", st.RowsRebuilt)
	}
	if st.WavesDone != 1 {
		t.Fatalf("a wave with nothing outstanding must be promoted, got %d", st.WavesDone)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// A wave with missing rows but NO resolvable content reference must be REFUSED,
// not rebuilt: a queue row with neither a content snapshot nor inline html sends
// an empty email. The wave stays 'produced' so the send-liveness monitors keep
// showing it.
func TestSweepUnlandedWaves_RefusesWithoutContent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	w := unlandedWave{
		id: uuid.New(), campaignID: uuid.New(), ispPlanID: uuid.New(),
		scheduledAt: time.Now(), produced: 1, landed: 0,
	}

	expectCandidateWave(mock, w)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_campaign_plan_recipients pr")).
		WillReturnRows(sqlmock.NewRows([]string{
			"subscriber_id", "recipient_isp", "selection_rank", "audience_source_type", "audience_source_id",
		}).AddRow(uuid.New(), "gmail", 1, "segment", ""))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key FROM mailing_campaign_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"idempotency_key"}))
	// No landed sibling, and no snapshot for the wave.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content_snapshot_id, priority")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_content_snapshots")).
		WillReturnError(sql.ErrNoRows)
	// NO INSERT, NO wave UPDATE: an unexpected call fails the test.

	s := NewUnlandedWaveSweeper(nil, 0, 0)
	sweepUnlandedWaves(context.Background(), db, s)

	st := s.Stats()
	if st.RowsRebuilt != 0 {
		t.Fatalf("must not rebuild without content, got %d rows", st.RowsRebuilt)
	}
	if st.Unresolved != 1 {
		t.Fatalf("want the wave counted as unresolved, got %d", st.Unresolved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// The sweep must only ever consider waves in 'produced' past the grace window,
// and must bound both the wave count and the per-wave row count — an unbounded
// sweep on the wave table is a 31.9s query (measured on prod 2026-09-01).
func TestSweepSQL_ScopeAndBounds(t *testing.T) {
	if !regexp.MustCompile(`w\.status = 'produced'`).MatchString(selectUnlandedWavesSQL) {
		t.Fatal("the sweep must scope to status='produced'")
	}
	if !regexp.MustCompile(`w\.completed_at < NOW\(\) - \$1::interval`).MatchString(selectUnlandedWavesSQL) {
		t.Fatal("the sweep must respect the grace window")
	}
	if !regexp.MustCompile(`LIMIT \$2`).MatchString(selectUnlandedWavesSQL) {
		t.Fatal("the candidate query must be bounded")
	}
	if !regexp.MustCompile(`LIMIT \$2`).MatchString(selectUnlandedPlanRecipientsSQL) {
		t.Fatal("the per-wave recipient query must be bounded")
	}
	if DefaultSweepGrace != 10*time.Minute {
		t.Fatalf("REQ-089 specifies a 10 minute grace, got %s", DefaultSweepGrace)
	}
}

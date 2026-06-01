package worker

// Unit tests for JourneyEventEnroller.
//
// The worker drains rows from mailing_journey_event_triggers inside a
// transaction (BEGIN … FOR UPDATE SKIP LOCKED … COMMIT) and either
// enrolls the subscriber into the configured click journey or marks
// the trigger row 'skipped' with a reason. Tests drive w.tick(ctx)
// directly so the polling loop and its goroutine are out of scope.
//
// Conventions mirrored from journey_segment_enroller_test.go and
// journey_email_activator_test.go:
//   - sqlmock with the default regex/substring query matcher
//   - regexp.QuoteMeta on a distinctive SQL substring per expectation
//   - ExpectBegin / ExpectCommit around the per-tick transaction
//   - sqlmock.AnyArg() for values the worker generates (uuid-prefixed
//     enrollment id, metadata JSON whose map ordering isn't stable,
//     uuid values whose Scan→Value roundtrip we don't need to assert)

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newEventEnrollerMockDB returns a sqlmock-backed DB using the default
// substring-via-regexp matcher already used by other worker tests.
func newEventEnrollerMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// pendingRowCols are the 10 columns the worker's SELECT-pending query
// returns (and Scans into the local trigger struct).
func pendingRowCols() []string {
	return []string{
		"id", "everflow_offer_id", "subscriber_id", "subscriber_email",
		"sub2_brand", "sub3_campaign_id", "click_id",
		"sending_profile_id", "sending_domain", "click_url",
	}
}

// TestJourneyEventEnroller_FirstExecutableNodeID exercises the pure
// node-graph extractor. The worker depends on it to pick the entry
// node for the new enrollment; getting this wrong either loops
// forever on the trigger node or panics downstream.
func TestJourneyEventEnroller_FirstExecutableNodeID(t *testing.T) {
	tests := []struct {
		name      string
		nodesJSON string
		wantID    string
		wantOK    bool
	}{
		{
			name:      "trigger then delay returns delay id",
			nodesJSON: `[{"id":"trig","type":"trigger"},{"id":"delay1","type":"delay"}]`,
			wantID:    "delay1",
			wantOK:    true,
		},
		{
			name:      "single email without trigger still resolves",
			nodesJSON: `[{"id":"e1","type":"email"}]`,
			wantID:    "e1",
			wantOK:    true,
		},
		{
			name:      "only trigger node yields not-ok",
			nodesJSON: `[{"id":"trig","type":"trigger"}]`,
			wantOK:    false,
		},
		{
			name:      "empty string is invalid json",
			nodesJSON: "",
			wantOK:    false,
		},
		{
			name:      "malformed json yields not-ok",
			nodesJSON: "not json",
			wantOK:    false,
		},
		{
			name:      "empty array yields not-ok",
			nodesJSON: "[]",
			wantOK:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := firstExecutableNodeID(tc.nodesJSON)
			require.Equal(t, tc.wantOK, gotOK)
			if tc.wantOK {
				require.Equal(t, tc.wantID, gotID)
			}
		})
	}
}

// TestJourneyEventEnroller_Tick_NoPendingRows_NoOp confirms the tick
// opens and commits a transaction even when the SELECT returns zero
// rows. No follow-up queries are issued and lifetime counters stay
// at zero — this is the steady-state idle case.
func TestJourneyEventEnroller_Tick_NoPendingRows_NoOp(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(5).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(5)
	w.tick(context.Background())

	processed, enrolled, skipped, errs := w.Stats()
	require.Zero(t, processed, "no rows means no processing")
	require.Zero(t, enrolled)
	require.Zero(t, skipped)
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_InvalidSubscriberID_MarksSkipped covers
// the first guard in processOne: a malformed subscriber_id (e.g. a
// truncated postback or DB corruption) is fast-failed before any
// downstream lookups so a single bad row can't burn lookup budget on
// every tick.
func TestJourneyEventEnroller_Tick_InvalidSubscriberID_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-bad", "EV-001", "not-a-uuid", "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-bad", "invalid_subscriber_id").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	processed, enrolled, skipped, errs := w.Stats()
	require.Equal(t, int64(1), processed)
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_OfferNoLongerMapped_MarksSkipped covers
// the race where an operator removes the offer→journey mapping after
// the click postback is queued. We must not enroll into a journey that
// no longer exists in the map.
func TestJourneyEventEnroller_Tick_OfferNoLongerMapped_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-unmapped", "EV-GONE", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-GONE").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-unmapped", "offer_unmapped_at_processing").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	processed, enrolled, skipped, _ := w.Stats()
	require.Equal(t, int64(1), processed)
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_OfferDisabled_MarksSkipped covers the
// "operator flipped the kill switch since the postback was queued"
// path. The map row exists but enabled=false, so we abort early.
func TestJourneyEventEnroller_Tick_OfferDisabled_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-disabled", "EV-OFF", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-OFF").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-001", "CPM", false))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-disabled", "offer_journey_disabled_at_processing").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	_, enrolled, skipped, _ := w.Stats()
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_CPCOffer_MarksSkipped covers the
// payout-type guard: CPC offers are paid per click, not per
// conversion, so a reminder-drip flow makes no business sense and
// would inflate the click count on Everflow. The worker must refuse.
func TestJourneyEventEnroller_Tick_CPCOffer_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-cpc", "EV-CPC", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-CPC").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-cpc", "CPC", true))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-cpc", "cpc_offer_at_processing").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	_, enrolled, skipped, _ := w.Stats()
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_AlreadyConverted_MarksSkipped covers
// the conversion-race: the subscriber's conversion postback landed in
// the seconds between the click postback being queued and this
// worker's lookup. We re-check suppression here so we never reminder-
// drip a subscriber who already converted.
func TestJourneyEventEnroller_Tick_AlreadyConverted_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()
	offerID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-conv", "EV-CONV", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-CONV").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-001", "CPM", true))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs("EV-CONV").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(offerID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_suppressions`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-conv", "already_converted_at_processing").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	_, enrolled, skipped, _ := w.Stats()
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_JourneyInactive_MarksSkipped covers
// the case where the journey row was deleted, marked status!='active',
// or is otherwise missing by the time we look it up. We must skip,
// not error, so the trigger doesn't sit in pending forever.
func TestJourneyEventEnroller_Tick_JourneyInactive_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-noj", "EV-NOJ", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-NOJ").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-001", "CPM", true))
	// mailing_offers returns no rows so offerUUID stays nil and the
	// suppression-count query is skipped entirely (matches the worker's
	// `if offerUUID != uuid.Nil` guard).
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs("EV-NOJ").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journeys`)).
		WithArgs("jrn-001").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-noj", "journey_not_found_or_inactive").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	_, enrolled, skipped, _ := w.Stats()
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_NoExecutableNode_MarksSkipped covers
// a misconfigured journey: trigger exists but no downstream node. An
// enrollment with current_node_id=trigger would never advance, so we
// reject before insert.
func TestJourneyEventEnroller_Tick_NoExecutableNode_MarksSkipped(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-nonode", "EV-001", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-001").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-001", "CPM", true))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs("EV-001").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journeys`)).
		WithArgs("jrn-001").
		WillReturnRows(sqlmock.NewRows([]string{"nodes"}).
			AddRow(`[{"id":"trig","type":"trigger"}]`))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-nonode", "no_executable_node").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	_, enrolled, skipped, _ := w.Stats()
	require.Zero(t, enrolled)
	require.Equal(t, int64(1), skipped)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_HappyPath_EnrollsAndMarksProcessed is
// the green-light smoke test: a well-formed pending row resolves to
// an enabled CPM offer with an active journey graph, the enrollment
// gets inserted, and the trigger is marked processed. The metadata
// JSON arg is matched loosely because Go map ordering is not stable.
func TestJourneyEventEnroller_Tick_HappyPath_EnrollsAndMarksProcessed(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	subID := uuid.New()
	offerID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-happy", "EV-001", subID.String(), "user@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com"))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-001").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-001", "CPM", true))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs("EV-001").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(offerID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_suppressions`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journeys`)).
		WithArgs("jrn-001").
		WillReturnRows(sqlmock.NewRows([]string{"nodes"}).
			AddRow(`[{"id":"trig","type":"trigger"},{"id":"delay1h","type":"delay"}]`))
	// enrollment id is generated via uuid.NewString — AnyArg().
	// metadata blob's key order is map-iteration-dependent — AnyArg().
	// The remaining args are deterministic and worth asserting on.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_journey_enrollments`)).
		WithArgs(sqlmock.AnyArg(), "jrn-001", "user@example.com", "delay1h", sqlmock.AnyArg(), "EV-001").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='processed'`)).
		WithArgs("trig-happy").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	processed, enrolled, skipped, errs := w.Stats()
	require.Equal(t, int64(1), processed)
	require.Equal(t, int64(1), enrolled)
	require.Zero(t, skipped)
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyEventEnroller_Tick_BatchOfThree_MixedOutcomes is the
// concurrency-of-outcomes test: one happy enrollment, one CPC skip,
// one invalid-uuid skip — all drained in a single transaction. The
// asserted stats (enrolled=1, skipped=2, processed=3) prove the
// switch on enrollmentOutcomeKind dispatches every row correctly.
func TestJourneyEventEnroller_Tick_BatchOfThree_MixedOutcomes(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	happyID := uuid.New()
	cpcID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_event_triggers`)).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows(pendingRowCols()).
			AddRow("trig-happy", "EV-OK", happyID.String(), "happy@example.com",
				"DB", "cmp-1", "clk-1", nil, "em.example.com", "https://example.com").
			AddRow("trig-cpc", "EV-CPC", cpcID.String(), "cpc@example.com",
				"DB", "cmp-2", "clk-2", nil, "em.example.com", "https://example.com").
			AddRow("trig-bad", "EV-BAD", "not-a-uuid", "bad@example.com",
				"DB", "cmp-3", "clk-3", nil, "em.example.com", "https://example.com"))

	// Row 1: happy path — full enrollment.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-OK").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-happy", "CPM", true))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offers`)).
		WithArgs("EV-OK").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journeys`)).
		WithArgs("jrn-happy").
		WillReturnRows(sqlmock.NewRows([]string{"nodes"}).
			AddRow(`[{"id":"trig","type":"trigger"},{"id":"email1","type":"email"}]`))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_journey_enrollments`)).
		WithArgs(sqlmock.AnyArg(), "jrn-happy", "happy@example.com", "email1", sqlmock.AnyArg(), "EV-OK").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='processed'`)).
		WithArgs("trig-happy").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Row 2: CPC offer — skipped after the map lookup.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WithArgs("EV-CPC").
		WillReturnRows(sqlmock.NewRows([]string{"click_journey_id", "payout_type", "enabled"}).
			AddRow("jrn-cpc", "CPC", true))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-cpc", "cpc_offer_at_processing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Row 3: invalid subscriber id — skipped without any lookup.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_journey_event_triggers`)+`.*`+regexp.QuoteMeta(`SET status='skipped'`)).
		WithArgs("trig-bad", "invalid_subscriber_id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectCommit()

	w := NewJourneyEventEnroller(db).WithBatchLimit(10)
	w.tick(context.Background())

	processed, enrolled, skipped, errs := w.Stats()
	require.Equal(t, int64(3), processed, "all 3 rows were dispatched into processOne")
	require.Equal(t, int64(1), enrolled, "only the happy row enrolled")
	require.Equal(t, int64(2), skipped, "cpc and invalid-uuid both skipped")
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

package worker

// Unit tests for JourneyLaneGovernor (Unit D, 2026-07-10).
//
// Conventions mirrored from journey_event_enroller_test.go: sqlmock with the
// default regexp matcher, regexp.QuoteMeta on a distinctive SQL substring per
// expectation, t.Setenv for env-driven behavior. tick(ctx) is driven directly
// so the polling loop stays out of scope.

import (
	"time"
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestLaneRecommendation is the table-driven contract for the pure
// recommendation rule: only CPA/CPL lanes with a real 30d sample and ZERO
// conversions redirect; CPM lanes always stay active; the redirect target is
// never redirected into itself.
func TestLaneRecommendation(t *testing.T) {
	const minSample = 50
	const target = "420"

	tests := []struct {
		name           string
		offerID        string
		payoutType     string
		enrollments30  int
		conversionsWin int
		laneMature     bool
		want           string
	}{
		{"CPA, sample met, zero conversions, mature → redirect", "9539", "CPA", 50, 0, true, "redirect:420"},
		{"CPL, sample met, zero conversions, mature → redirect", "9178", "CPL", 120, 0, true, "redirect:420"},
		{"CPA below sample stays active", "9539", "CPA", 49, 0, true, "active"},
		{"CPA with conversions stays active", "9539", "CPA", 500, 1, true, "active"},
		{"CPA young lane never redirected (conversion latency)", "9539", "CPA", 500, 0, false, "active"},
		{"CPM always active even at zero conversions", "7667", "CPM", 10000, 0, true, "active"},
		{"eCPM always active", "7667", "eCPM", 10000, 0, true, "active"},
		{"UNKNOWN payout stays active", "1111", "UNKNOWN", 10000, 0, true, "active"},
		{"target offer itself never redirected", "420", "CPA", 10000, 0, true, "active"},
		{"zero enrollments stays active", "9539", "CPA", 0, 0, true, "active"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := laneRecommendation(tc.offerID, tc.payoutType, tc.enrollments30, tc.conversionsWin, minSample, target, tc.laneMature)
			require.Equal(t, tc.want, got)
		})
	}
}

// laneCols is the shape of the loadLanes SELECT.
func laneCols() []string {
	return []string{"everflow_offer_id", "payout_type", "routing_state", "created_at"}
}

// matureLaneAge is a created_at older than any conversion window used in
// these tests, so lane maturity never suppresses a recommendation unless a
// test sets it deliberately.
var matureLaneAge = time.Now().Add(-365 * 24 * time.Hour)

// expectGovernorLockAndLanes registers the advisory-lock acquire and the
// journey-map scan that open every tick.
func expectGovernorLockAndLanes(mock sqlmock.Sqlmock, laneRows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(`pg_try_advisory_lock`)).
		WithArgs(laneGovernorLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_journey_map`)).
		WillReturnRows(laneRows)
}

// expectLaneStats registers the two per-lane count queries.
func expectLaneStats(mock sqlmock.Sqlmock, offerID string, enrollments30, active, conversions30, touches30 int) {
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_journey_enrollments`)).
		WithArgs(offerID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"enrollments_30d", "active", "conversions_win"}).
			AddRow(enrollments30, active, conversions30))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_message_log`)).
		WithArgs(shadowCampaignID(offerID)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(touches30))
}

// TestJourneyLaneGovernor_Tick_AdviseOnly_DoesNotWriteRoutingState: default
// mode (CLICKDRIP_DYNAMIC_ROUTING unset) writes lane_stats and the
// recommendation but NEVER routing_state / redirect_offer_id — even when the
// recommendation is a redirect.
func TestJourneyLaneGovernor_Tick_AdviseOnly_DoesNotWriteRoutingState(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	expectGovernorLockAndLanes(mock, sqlmock.NewRows(laneCols()).
		AddRow("9539", "CPA", "active", matureLaneAge))
	// Sample met, zero conversions → recommendation is redirect:420, but the
	// advise-mode UPDATE carries only stats + recommendation (3 args).
	expectLaneStats(mock, "9539", 200, 5, 0, 800)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_offer_journey_map`)+`.*`+regexp.QuoteMeta(`SET lane_stats=`)).
		WithArgs("9539", sqlmock.AnyArg(), "redirect:420").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`pg_advisory_unlock`)).
		WithArgs(laneGovernorLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := NewJourneyLaneGovernor(db)
	w.tick(context.Background())

	lanes, recs, enforced, errs := w.Stats()
	require.Equal(t, int64(1), lanes)
	require.Equal(t, int64(1), recs, "redirect recommendation counted")
	require.Zero(t, enforced, "advise mode never enforces")
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyLaneGovernor_Tick_Enforce_WritesRoutingState: with
// CLICKDRIP_DYNAMIC_ROUTING=enforce a redirect recommendation also flips
// routing_state='redirect' + redirect_offer_id, and a healthy lane is
// (re)asserted 'active' with the redirect target cleared.
func TestJourneyLaneGovernor_Tick_Enforce_WritesRoutingState(t *testing.T) {
	t.Setenv("CLICKDRIP_DYNAMIC_ROUTING", "enforce")
	db, mock := newEventEnrollerMockDB(t)

	expectGovernorLockAndLanes(mock, sqlmock.NewRows(laneCols()).
		AddRow("9539", "CPA", "active", matureLaneAge).
		AddRow("7667", "CPM", "redirect", matureLaneAge))

	// Lane 1: dead CPA lane → enforced redirect (state changes active→redirect).
	expectLaneStats(mock, "9539", 200, 5, 0, 800)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_offer_journey_map`)+`.*`+regexp.QuoteMeta(`routing_state=$4`)).
		WithArgs("9539", sqlmock.AnyArg(), "redirect:420", "redirect", "420").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Lane 2: CPM lane previously redirected → enforced back to active
	// (state changes redirect→active, redirect target cleared).
	expectLaneStats(mock, "7667", 5000, 40, 0, 12000)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_offer_journey_map`)+`.*`+regexp.QuoteMeta(`routing_state=$4`)).
		WithArgs("7667", sqlmock.AnyArg(), "active", "active", "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(regexp.QuoteMeta(`pg_advisory_unlock`)).
		WithArgs(laneGovernorLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := NewJourneyLaneGovernor(db)
	w.tick(context.Background())

	lanes, recs, enforced, errs := w.Stats()
	require.Equal(t, int64(2), lanes)
	require.Equal(t, int64(1), recs)
	require.Equal(t, int64(2), enforced, "both lanes changed routing_state")
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyLaneGovernor_Tick_Enforce_ManualPauseWins: an operator-set
// paused_auto lane is never overwritten in enforce mode — stats and the
// recommendation still refresh via the advise-shape UPDATE.
func TestJourneyLaneGovernor_Tick_Enforce_ManualPauseWins(t *testing.T) {
	t.Setenv("CLICKDRIP_DYNAMIC_ROUTING", "enforce")
	db, mock := newEventEnrollerMockDB(t)

	expectGovernorLockAndLanes(mock, sqlmock.NewRows(laneCols()).
		AddRow("9178", "CPL", "paused_auto", matureLaneAge))
	expectLaneStats(mock, "9178", 200, 0, 0, 100)
	// Advise-shape UPDATE (3 args, no routing_state) despite enforce mode.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_offer_journey_map`)+`.*`+regexp.QuoteMeta(`SET lane_stats=`)).
		WithArgs("9178", sqlmock.AnyArg(), "redirect:420").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`pg_advisory_unlock`)).
		WithArgs(laneGovernorLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := NewJourneyLaneGovernor(db)
	w.tick(context.Background())

	lanes, recs, enforced, errs := w.Stats()
	require.Equal(t, int64(1), lanes)
	require.Equal(t, int64(1), recs)
	require.Zero(t, enforced, "paused_auto is operator territory")
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestJourneyLaneGovernor_Tick_LockContended_NoOp: when another instance
// holds the advisory lock, the tick exits without scanning anything.
func TestJourneyLaneGovernor_Tick_LockContended_NoOp(t *testing.T) {
	db, mock := newEventEnrollerMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta(`pg_try_advisory_lock`)).
		WithArgs(laneGovernorLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	w := NewJourneyLaneGovernor(db)
	w.tick(context.Background())

	lanes, recs, enforced, errs := w.Stats()
	require.Zero(t, lanes)
	require.Zero(t, recs)
	require.Zero(t, enforced)
	require.Zero(t, errs)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestLaneGovernorEnvDefaults pins the env-var parsing contract:
// CLICKDRIP_GOVERNOR_MIN_SAMPLE default 50 (garbage tolerated),
// CLICKDRIP_REDIRECT_TARGET_OFFER default "420", and only the literal
// "enforce" enables enforcement.
func TestLaneGovernorEnvDefaults(t *testing.T) {
	require.Equal(t, 50, laneGovernorMinSample())
	t.Setenv("CLICKDRIP_GOVERNOR_MIN_SAMPLE", "25")
	require.Equal(t, 25, laneGovernorMinSample())
	t.Setenv("CLICKDRIP_GOVERNOR_MIN_SAMPLE", "junk")
	require.Equal(t, 50, laneGovernorMinSample())
	t.Setenv("CLICKDRIP_GOVERNOR_MIN_SAMPLE", "0")
	require.Equal(t, 50, laneGovernorMinSample())

	require.Equal(t, "420", laneGovernorRedirectTarget())
	t.Setenv("CLICKDRIP_REDIRECT_TARGET_OFFER", "7667")
	require.Equal(t, "7667", laneGovernorRedirectTarget())

	require.False(t, laneGovernorEnforceEnabled())
	t.Setenv("CLICKDRIP_DYNAMIC_ROUTING", "true")
	require.False(t, laneGovernorEnforceEnabled(), "only the literal 'enforce' enables")
	t.Setenv("CLICKDRIP_DYNAMIC_ROUTING", "enforce")
	require.True(t, laneGovernorEnforceEnabled())
}

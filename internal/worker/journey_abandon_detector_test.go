package worker

// Tests for JourneyAbandonDetector. sqlmock for the tick wiring + SQL pins for
// the decision predicate (the predicate lives in one INSERT statement so the
// PK collapses concurrent double-fires — the pins keep its clauses from
// silently drifting).
//
// Pinned behaviors (operator spec 2026-07-27):
//   - N-hour math: threshold parameterized, first_event_at anchored, 14-day
//     late-arrival cap
//   - converted-session exclusion at detect time AND retroactively (a session
//     that converts after detection is retired, never abandon-touched)
//   - once per session (PK + ON CONFLICT DO NOTHING)
//   - attributability: sub1 OR email required; sub1→email resolution guarded
//     against non-uuid garbage

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAbandonDetector(t *testing.T) (*JourneyAbandonDetector, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewJourneyAbandonDetector(db), mock
}

// ----- SQL predicate pins ---------------------------------------------------

func TestAbandonDetectPredicatePins(t *testing.T) {
	s := abandonDetectSQL
	// N-hour math is parameterized and anchored on the session's FIRST event.
	assert.Contains(t, s, "s.first_event_at < NOW() - make_interval(hours => $1)")
	// Late-arrival cap: ancient sessions never enroll.
	assert.Contains(t, s, "s.first_event_at > NOW() - INTERVAL '14 days'")
	// Attributability: sub1 or email.
	assert.Contains(t, s, "(s.sub1 <> '' OR s.email <> '')")
	// Converted-session exclusion at detect time.
	assert.Contains(t, s, "c.event_type = 'lead_accepted' AND c.transid = s.transid")
	assert.Contains(t, s, "NOT EXISTS")
	// Once per session.
	assert.Contains(t, s, "ON CONFLICT (session_id) DO NOTHING")
	// Only progress events define a session.
	assert.Contains(t, s, "event_type = 'session_progress' AND session_id <> ''")
}

func TestAbandonLateConversionPins(t *testing.T) {
	s := abandonLateConversionSQL
	assert.Contains(t, s, "SET status = 'converted'")
	assert.Contains(t, s, "a.status = 'pending'")
	assert.Contains(t, s, "c.event_type = 'lead_accepted' AND c.transid = a.transid")
}

func TestAbandonEmailResolutionPins(t *testing.T) {
	s := abandonResolveEmailSQL
	// Fallback order: only rows the funnel could NOT identify by email.
	assert.Contains(t, s, "a.email = '' AND a.sub1 <> ''")
	// Garbage-sub1 guard: never a cast error mid-sweep.
	assert.Contains(t, s, `a.sub1 ~ '^[0-9a-fA-F-]{36}$'`)
	assert.Contains(t, s, "s.id = a.sub1::uuid")
	// Only pending rows are touched.
	assert.Contains(t, s, "a.status = 'pending'")
}

// ----- N-hour threshold config ---------------------------------------------

func TestAbandonHoursFromEnv(t *testing.T) {
	t.Setenv("JOURNEY_ABANDON_HOURS", "")
	assert.Equal(t, 4, abandonHoursFromEnv(), "default is 4h")
	t.Setenv("JOURNEY_ABANDON_HOURS", "12")
	assert.Equal(t, 12, abandonHoursFromEnv())
	t.Setenv("JOURNEY_ABANDON_HOURS", "0")
	assert.Equal(t, 4, abandonHoursFromEnv(), "non-positive falls back")
	t.Setenv("JOURNEY_ABANDON_HOURS", "nope")
	assert.Equal(t, 4, abandonHoursFromEnv(), "garbage falls back")
}

// ----- tick wiring ----------------------------------------------------------

func TestAbandonTickRunsDetectResolveConvertInOrder(t *testing.T) {
	w, mock := newAbandonDetector(t)
	w.WithAbandonHours(6)

	// Order matters: detect → resolve email → late-conversion retirement (so a
	// lead_accepted racing the detect INSERT still retires the row same-tick).
	mock.ExpectExec(`INSERT INTO mailing_journey_abandon_state`).
		WithArgs(6).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE mailing_journey_abandon_state a\s+SET email = lower`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_journey_abandon_state a\s+SET status = 'converted'`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	detected, resolved, converted, errors := w.Stats()
	assert.Equal(t, int64(2), detected)
	assert.Equal(t, int64(1), resolved)
	assert.Equal(t, int64(1), converted)
	assert.Equal(t, int64(0), errors)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestAbandonTickDetectFailureAbortsTick(t *testing.T) {
	w, mock := newAbandonDetector(t)
	mock.ExpectExec(`INSERT INTO mailing_journey_abandon_state`).
		WillReturnError(context.DeadlineExceeded)
	// No further expectations: resolve/convert must NOT run on detect failure.
	w.tick(context.Background())
	_, _, _, errors := w.Stats()
	assert.Equal(t, int64(1), errors)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestAbandonDetectorStopIsIdempotent(t *testing.T) {
	w, _ := newAbandonDetector(t)
	w.WithInterval(time.Hour)
	w.Stop()
	w.Stop() // second call must not panic
}

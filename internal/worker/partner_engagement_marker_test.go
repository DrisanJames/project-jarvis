package worker

// Regression guards for the partner_clean_queue.engaged_at marker.
//
// 2026-07-26 per-record dataset re-key: the legacy sweep attributed clicks
// through mailing_campaigns.partner_dataset_id — a stamp written ONLY by the
// drip orchestrator, and only with the vertical's single dominant dataset.
// Board-deployed sends (no stamp) and non-dominant shared-vertical datasets
// never marked (spicy-clickers: 14/496,936; term_life: 60/85,878 measured on
// prod). The fix computes the REQ-034 pickDatasetID rule in SQL from
// partner_clean_queue memberships at mark time. These tests pin:
//   - the generated statements (golden SQL, no live PG needed — the verdict
//     functions exist only in prod),
//   - the board-mailed/unstamped-campaign regression,
//   - REQ-036 verdict-filter preservation in BOTH modes,
//   - set-once idempotency (engaged_at IS NULL guard, single SET column),
//   - the kill-switch and index-latch fallbacks to the legacy statement.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyMarkSQLGolden is the pre-2026-07-26 statement, byte-for-byte as
// buildEngagementMarkSQL emitted it (verdict-filtered, windowed). The kill
// switch DISABLE_MARKER_PER_RECORD_DATASET must reproduce it exactly — it is
// the known-bounded, known-behavior fallback.
const legacyMarkSQLGolden = `
			UPDATE partner_clean_queue q
			SET engaged_at = e.first_click
			FROM (
				SELECT te.subscriber_id, c.partner_dataset_id, MIN(te.event_at) AS first_click
				FROM mailing_tracking_events te
				JOIN mailing_campaigns c ON c.id = te.campaign_id
				WHERE te.event_type = 'clicked'
				  AND c.partner_dataset_id IS NOT NULL
				  AND ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))
				  AND te.event_at > NOW() - make_interval(mins => $1)
				GROUP BY te.subscriber_id, c.partner_dataset_id
			) e
			WHERE q.subscriber_id = e.subscriber_id
			  AND q.dataset_id = e.partner_dataset_id
			  AND q.engaged_at IS NULL`

func TestBuildEngagementMarkSQL_LegacyKillSwitchPath_ByteStable(t *testing.T) {
	assert.Equal(t, legacyMarkSQLGolden, buildEngagementMarkSQL(30, true, false),
		"legacy (perRecord=false) statement must stay byte-stable — it is the operator's kill-switch fallback")
}

// TestPerRecordSQL_MarksBoardMailedClicks_Regression is THE regression pin
// for the proven blind spot: a human click on a BOARD-deployed campaign
// (mailing_campaigns.partner_dataset_id IS NULL — never stamped by the drip
// orchestrator) must now be able to mark, attributed to the subscriber's
// pcq membership instead of the campaign stamp.
func TestPerRecordSQL_MarksBoardMailedClicks_Regression(t *testing.T) {
	legacy := buildEngagementMarkSQL(30, true, false)
	perRec := buildEngagementMarkSQL(30, true, true)

	// The legacy statement structurally CANNOT mark a board-mailed click:
	// an unstamped campaign is dropped by the stamp-required predicate, and
	// the queue row is keyed to the campaign's stamp.
	assert.Contains(t, legacy, "c.partner_dataset_id IS NOT NULL")
	assert.Contains(t, legacy, "JOIN mailing_campaigns c ON c.id = te.campaign_id")
	assert.Contains(t, legacy, "q.dataset_id = e.partner_dataset_id")

	// The per-record statement removes every one of those exclusions:
	// no stamp-required filter anywhere...
	assert.NotContains(t, perRec, "partner_dataset_id IS NOT NULL")
	// ...campaigns joined LEFT so a missing/unstamped campaign row cannot
	// drop the click...
	assert.Contains(t, perRec, "LEFT JOIN mailing_campaigns c ON c.id = k.campaign_id")
	// ...and the queue row is keyed to the dataset RESOLVED FROM MEMBERSHIP
	// (the LATERAL), not the campaign stamp.
	assert.Contains(t, perRec, "q.dataset_id = e.dataset_id")
	assert.NotContains(t, perRec, "q.dataset_id = e.partner_dataset_id")
	// Click universe is pre-filtered to pcq members (bounded work), not to
	// stamped campaigns.
	assert.Contains(t, perRec, "WHERE pm.subscriber_id = te.subscriber_id")
}

// TestPerRecordSQL_PinsREQ034AttributionRule pins the LATERAL resolution to
// the exact pickDatasetID (send_worker.go) rule: campaign's dataset when the
// subscriber is a member of it, else earliest-ingested membership, ties
// broken by smallest dataset id — deterministic, one dataset per click.
func TestPerRecordSQL_PinsREQ034AttributionRule(t *testing.T) {
	perRec := buildEngagementMarkSQL(30, true, true)
	assert.Contains(t, perRec,
		"ORDER BY (m.dataset_id = c.partner_dataset_id) DESC NULLS LAST,\n\t\t\t\t\tm.ingested_at ASC, m.dataset_id ASC",
		"membership pick must prefer campaign-member match, then earliest ingest, then smallest dataset id (pickDatasetID parity)")
	assert.Contains(t, perRec, "LIMIT 1", "exactly one dataset resolves per (subscriber, campaign) click")
	// NULLS LAST is load-bearing: an unstamped campaign makes the match
	// expression NULL for every membership row, which must not outrank real
	// orderings under DESC (PG default is NULLS FIRST on DESC).
	assert.Contains(t, perRec, "DESC NULLS LAST")
}

// TestBuildEngagementMarkSQL_VerdictFilterPreserved pins REQ-036 semantics in
// BOTH modes: the canonical HUMAN_VERDICT_PG predicate is present exactly
// when verdict filtering is on, absent when the REQ-036 kill switch reverts
// to raw clicks — the per-record re-key must not alter this in either
// direction.
func TestBuildEngagementMarkSQL_VerdictFilterPreserved(t *testing.T) {
	for _, perRecord := range []bool{false, true} {
		on := buildEngagementMarkSQL(30, true, perRecord)
		off := buildEngagementMarkSQL(30, false, perRecord)
		assert.Contains(t, on, "AND "+humanVerdictSQL, "perRecord=%v: verdict predicate missing", perRecord)
		assert.Equal(t, 1, strings.Count(on, "ignite_verdict_is_human"), "perRecord=%v: verdict predicate must appear exactly once", perRecord)
		assert.NotContains(t, off, "ignite_verdict_is_human", "perRecord=%v: kill-switch path must not verdict-filter", perRecord)
	}
	// The predicate itself must remain the canonical byte sequence.
	assert.Equal(t, "ignite_verdict_is_human(ignite_event_verdict(te.user_agent, te.ip_address))", humanVerdictSQL)
}

// TestBuildEngagementMarkSQL_SetOnceIdempotency pins the double-fire/regress
// guards in BOTH modes: only NULL engaged_at rows are eligible (set once,
// never regressed — a second sweep over the same window matches zero rows),
// and engaged_at is the ONLY column written.
func TestBuildEngagementMarkSQL_SetOnceIdempotency(t *testing.T) {
	for _, perRecord := range []bool{false, true} {
		for _, verdict := range []bool{false, true} {
			q := buildEngagementMarkSQL(30, verdict, perRecord)
			assert.Contains(t, q, "AND q.engaged_at IS NULL",
				"perRecord=%v verdict=%v: missing set-once guard", perRecord, verdict)
			assert.Equal(t, 1, strings.Count(q, "SET "),
				"perRecord=%v verdict=%v: exactly one SET clause", perRecord, verdict)
			assert.Contains(t, q, "SET engaged_at = e.first_click")
		}
	}
}

// TestBuildEngagementMarkSQL_WindowShape pins the bounded-sweep contract:
// lookbackMins>0 emits the $1 make_interval window (per-tick + boot catch-up
// paths), <=0 emits no time filter (manual backfill shape only).
func TestBuildEngagementMarkSQL_WindowShape(t *testing.T) {
	for _, perRecord := range []bool{false, true} {
		windowed := buildEngagementMarkSQL(30, true, perRecord)
		assert.Contains(t, windowed, "AND te.event_at > NOW() - make_interval(mins => $1)")
		unbounded := buildEngagementMarkSQL(0, true, perRecord)
		assert.NotContains(t, unbounded, "make_interval")
	}
}

// --- markOnce wiring (sqlmock) ----------------------------------------------

func newMarkerWithMock(t *testing.T) (*PartnerEngagementMarker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return NewPartnerEngagementMarker(db), mock
}

var markerProbeRe = `SELECT i\.indisvalid\s+FROM pg_class c\s+JOIN pg_index i ON i\.indexrelid = c\.oid\s+WHERE c\.relname = 'idx_pcq_subscriber_id'`

// TestMarkOnce_PerRecordActive: index valid → the per-record statement runs
// with the window arg; the latch caches validity so the second sweep skips
// the probe.
func TestMarkOnce_PerRecordActive_AndLatch(t *testing.T) {
	m, mock := newMarkerWithMock(t)
	mock.ExpectQuery(markerProbeRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("CROSS JOIN LATERAL")).
		WithArgs(30).
		WillReturnResult(sqlmock.NewResult(0, 5))
	m.markOnce(context.Background(), 30)

	// Second sweep: no probe expected — latch holds.
	mock.ExpectExec(regexp.QuoteMeta("CROSS JOIN LATERAL")).
		WithArgs(30).
		WillReturnResult(sqlmock.NewResult(0, 0))
	m.markOnce(context.Background(), 30)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkOnce_KillSwitch_RevertsToLegacy: the operator switch forces the
// legacy campaign-join statement and skips the index probe entirely.
func TestMarkOnce_KillSwitch_RevertsToLegacy(t *testing.T) {
	t.Setenv("DISABLE_MARKER_PER_RECORD_DATASET", "true")
	m, mock := newMarkerWithMock(t)
	mock.ExpectExec(regexp.QuoteMeta("AND q.dataset_id = e.partner_dataset_id")).
		WithArgs(30).
		WillReturnResult(sqlmock.NewResult(0, 1))
	m.markOnce(context.Background(), 30)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkOnce_IndexNotReady_FallsBackLegacy: probe failure fails CLOSED to
// the legacy statement (never a pcq seq scan), and the probe TTL suppresses
// an immediate re-probe on the next sweep.
func TestMarkOnce_IndexNotReady_FallsBackLegacy(t *testing.T) {
	m, mock := newMarkerWithMock(t)
	mock.ExpectQuery(markerProbeRe).WillReturnError(errors.New("no such index"))
	mock.ExpectExec(regexp.QuoteMeta("AND q.dataset_id = e.partner_dataset_id")).
		WithArgs(30).
		WillReturnResult(sqlmock.NewResult(0, 0))
	m.markOnce(context.Background(), 30)

	// Within the TTL: no second probe, still legacy.
	require.True(t, time.Now().Before(m.perRecordNextProbe), "probe TTL must be armed after a failed probe")
	mock.ExpectExec(regexp.QuoteMeta("AND q.dataset_id = e.partner_dataset_id")).
		WithArgs(30).
		WillReturnResult(sqlmock.NewResult(0, 0))
	m.markOnce(context.Background(), 30)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkOnce_ExecErrorIsSwallowed: a failed sweep logs and returns — the
// ticker loop must survive transient DB errors (statement_timeout, failover).
func TestMarkOnce_ExecErrorIsSwallowed(t *testing.T) {
	m, mock := newMarkerWithMock(t)
	mock.ExpectQuery(markerProbeRe).
		WillReturnRows(sqlmock.NewRows([]string{"indisvalid"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta("CROSS JOIN LATERAL")).
		WithArgs(30).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	assert.NotPanics(t, func() { m.markOnce(context.Background(), 30) })
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestVerdictAndPerRecordSwitches_Values pins the accepted env spellings.
func TestVerdictAndPerRecordSwitches_Values(t *testing.T) {
	t.Setenv("DISABLE_MARKER_PER_RECORD_DATASET", "")
	assert.False(t, perRecordDatasetMarkerDisabled())
	t.Setenv("DISABLE_MARKER_PER_RECORD_DATASET", "1")
	assert.True(t, perRecordDatasetMarkerDisabled())
	t.Setenv("DISABLE_MARKER_PER_RECORD_DATASET", "true")
	assert.True(t, perRecordDatasetMarkerDisabled())
}

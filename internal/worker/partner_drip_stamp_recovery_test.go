package worker

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stampUpdateRe matches the ladder-advance UPDATE **including its idempotency
// guard**. Every markMailed test below expects this pattern, so deleting the
// guard breaks all of them — that is deliberate.
const stampUpdateRe = `UPDATE partner_clean_queue[\s\S]*touch_count = LEAST\(COALESCE\(touch_count, 0\) \+ 1, \$4\)[\s\S]*WHERE id = ANY\(\$1::uuid\[\]\)[\s\S]*AND last_touch_campaign_id IS DISTINCT FROM \$2::uuid`

// expectStampAttempt queues one full withDBTimeout attempt (BEGIN, SET LOCAL,
// UPDATE, COMMIT/ROLLBACK) at the elevated orchestrator statement_timeout.
func expectStampAttempt(mock sqlmock.Sqlmock) *sqlmock.ExpectedExec {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	return mock.ExpectExec(stampUpdateRe)
}

func newStampMock(t *testing.T) (*PartnerDripOrchestrator, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	po := &PartnerDripOrchestrator{db: db}
	po.cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"
	return po, mock, func() { db.Close() }
}

func recs(ids ...string) []claimedRecord {
	out := make([]claimedRecord, len(ids))
	for i, id := range ids {
		out[i] = claimedRecord{id: id}
	}
	return out
}

// -----------------------------------------------------------------------------
// 1. statement timeout on the first attempt -> retried -> the stamp lands
// -----------------------------------------------------------------------------

// TestMarkMailed_RetriesStatementTimeoutAndLands reproduces the exact 2026-08-20
// failure (`pq: canceling statement due to statement timeout`, SQLSTATE 57014,
// campaign ff01ad90-e3fc-4d0e-bc55-239e8ce35d69) and pins the new behavior: the
// stamp is retried and the ladder DOES advance. Before this fix the single
// attempt was logged and dropped, leaving 314 mailed people at touch_count=1 with
// next_touch_at in the past — re-mailable.
func TestMarkMailed_RetriesStatementTimeoutAndLands(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	// Attempt 1 — statement timeout, transaction rolled back.
	expectStampAttempt(mock).WillReturnError(&pq.Error{
		Code: "57014", Message: "canceling statement due to statement timeout",
	})
	mock.ExpectRollback()
	// Attempt 2 — lands.
	expectStampAttempt(mock).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	err := po.markMailed(context.Background(), recs("r1", "r2", "r3"),
		"ff01ad90-e3fc-4d0e-bc55-239e8ce35d69", "tot")
	require.NoError(t, err, "a retryable timeout must not lose the ladder advance")

	st := po.StampStats()
	assert.Equal(t, int64(3), st.RowsStamped)
	assert.Equal(t, int64(0), st.RowsLost)
	assert.Equal(t, int64(1), st.ChunkRetries)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkMailed_ChunksLargeWaves pins the chunking: a wave larger than
// markMailedChunkSize must be stamped as several bounded statements, never as one
// unbounded ANY($1::uuid[]) against the 11.8M-row queue. It also pins that each
// chunk commits on its own, so a later failure cannot un-commit earlier progress.
func TestMarkMailed_ChunksLargeWaves(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	total := markMailedChunkSize + 7 // 2 chunks
	ids := make([]string, total)
	for i := range ids {
		ids[i] = "00000000-0000-0000-0000-" + strings.Repeat("0", 12-len(itoa(i))) + itoa(i)
	}

	expectStampAttempt(mock).WillReturnResult(sqlmock.NewResult(0, int64(markMailedChunkSize)))
	mock.ExpectCommit()
	expectStampAttempt(mock).WillReturnResult(sqlmock.NewResult(0, 7))
	mock.ExpectCommit()

	require.NoError(t, po.markMailed(context.Background(), recs(ids...), "camp-1", "db"))
	assert.Equal(t, int64(total), po.StampStats().RowsStamped)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// -----------------------------------------------------------------------------
// 2. IDEMPOTENCY — double application increments touch_count exactly once
// -----------------------------------------------------------------------------

// TestMarkMailedStampSQL_IdempotencyGuard is the semantic half of the proof.
// sqlmock does not execute SQL, so the guarantee has to be pinned on the
// statement itself: the stamp increments touch_count, and it is gated on
// `last_touch_campaign_id IS DISTINCT FROM $2::uuid` — the same $2 it then writes
// into last_touch_campaign_id. That is what makes the statement self-disabling:
// once applied for a campaign, re-applying it matches zero rows.
//
// One campaign is one wave is one touch (deployWaveGroup mints a fresh campaign
// per routing group), so the guard can never suppress a legitimate second advance.
func TestMarkMailedStampSQL_IdempotencyGuard(t *testing.T) {
	require.Regexp(t, regexp.MustCompile(`touch_count = LEAST\(COALESCE\(touch_count, 0\) \+ 1, \$4\)`), markMailedStampSQL)
	require.Regexp(t, regexp.MustCompile(`last_touch_campaign_id = \$2::uuid`), markMailedStampSQL)
	require.Regexp(t, regexp.MustCompile(`WHERE id = ANY\(\$1::uuid\[\]\)\s+AND last_touch_campaign_id IS DISTINCT FROM \$2::uuid`), markMailedStampSQL,
		"the ladder stamp MUST be gated on the campaign it is about to write, or a retry double-increments touch_count")
	// IS DISTINCT FROM, not <>: last_touch_campaign_id is NULL on a first touch
	// and `NULL <> $2` is NULL (never true), which would silently stamp nothing.
	require.NotContains(t, markMailedStampSQL, "last_touch_campaign_id <> ")
}

// TestMarkMailed_DoubleApplicationAdvancesOnce is the behavioral half: apply the
// SAME stamp twice (the scheduler re-fire / retry / double-deploy case). The
// second application is a zero-row no-op — which is exactly what Postgres returns
// once the guard matches — so the recorded advance totals 3, not 6.
func TestMarkMailed_DoubleApplicationAdvancesOnce(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	const campaign = "ff01ad90-e3fc-4d0e-bc55-239e8ce35d69"

	// First application: 3 rows advance.
	expectStampAttempt(mock).WithArgs("{r1,r2,r3}", campaign, "tot", MaxTouchCount, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	// Second application, same campaign: the guard has already matched, 0 rows.
	expectStampAttempt(mock).WithArgs("{r1,r2,r3}", campaign, "tot", MaxTouchCount, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	require.NoError(t, po.markMailed(context.Background(), recs("r1", "r2", "r3"), campaign, "tot"))
	require.NoError(t, po.markMailed(context.Background(), recs("r1", "r2", "r3"), campaign, "tot"))

	st := po.StampStats()
	assert.Equal(t, int64(3), st.RowsStamped, "touch_count must move for 3 rows exactly once, not 6")
	assert.Equal(t, int64(0), st.RowsLost, "a no-op re-application is success, not a lost stamp")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// 3. permanent failure -> surfaced loudly, not swallowed
// -----------------------------------------------------------------------------

// TestMarkMailed_PermanentFailureSurfacesAndRecords pins that a NON-retryable
// fault (here undefined_column, 42703) is not retried, returns an error to the
// caller, and — the thing that was missing on 2026-08-20 — writes a durable,
// operator-queryable row to partner_drip_stamp_failures. Before this, the ONLY
// evidence of 314 lost ladder advances was a single log line.
func TestMarkMailed_PermanentFailureSurfacesAndRecords(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	expectStampAttempt(mock).WillReturnError(&pq.Error{Code: "42703", Message: `column "nope" does not exist`})
	mock.ExpectRollback()
	// The durable signal.
	mock.ExpectExec(`INSERT INTO partner_drip_stamp_failures[\s\S]*ON CONFLICT \(campaign_id\) DO UPDATE`).
		WithArgs("camp-x", "tot", int64(2), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.markMailed(context.Background(), recs("r1", "r2"), "camp-x", "tot")
	require.Error(t, err, "a permanently failed stamp must NOT be swallowed")
	assert.Contains(t, err.Error(), "unstamped")

	st := po.StampStats()
	assert.Equal(t, int64(2), st.RowsLost)
	assert.Equal(t, int64(1), st.WavesFailed)
	assert.Equal(t, int64(0), st.ChunkRetries, "a permanent fault must not burn retries")
	assert.False(t, st.LastFailureAt.IsZero())
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestIsRetryableStampError(t *testing.T) {
	assert.True(t, isRetryableStampError(&pq.Error{Code: "57014"}), "statement timeout")
	assert.True(t, isRetryableStampError(&pq.Error{Code: "40P01"}), "deadlock")
	assert.True(t, isRetryableStampError(&pq.Error{Code: "40001"}), "serialization failure")
	assert.False(t, isRetryableStampError(&pq.Error{Code: "23505"}), "unique violation is permanent")
	assert.False(t, isRetryableStampError(&pq.Error{Code: "42703"}), "undefined column is permanent")
	assert.False(t, isRetryableStampError(nil))
}

// -----------------------------------------------------------------------------
// 4. reconciliation stamps ONLY rows present in mailing_message_log
// -----------------------------------------------------------------------------

// TestRecoverUnstampedTouches_JoinsMessageLogOnly pins the hard constraint: a row
// is only stamped when mailing_message_log proves the campaign actually sent to
// that subscriber. Both the find and the apply statement drive off
// `ml.campaign_id` (idx_message_log_campaign) and probe partner_clean_queue by
// subscriber_id (idx_pcq_subscriber_id) — never a status/time inference.
//
// It also pins the dataset_id predicate. partner_clean_queue is unique on
// (vertical, email_md5), so one human in two verticals has two rows sharing a
// subscriber_id. Measured on the live incident campaign: 314 correct rows matched
// AND 48 rows in OTHER datasets (touch_count up to 5, two of them 'held') matched
// on subscriber_id alone. Dropping `q.dataset_id = $2` corrupts those 48.
func TestRecoverUnstampedTouches_JoinsMessageLogOnly(t *testing.T) {
	for _, sql := range []string{stampRecoveryFindSQL, stampRecoveryApplySQL} {
		// Send truth: the ONLY source of candidate subscribers is the message log,
		// selected by campaign_id. No status/time inference anywhere.
		assert.Regexp(t, `FROM mailing_message_log\s+WHERE campaign_id = \$1::uuid`, sql)
		// The plan is pinned so the queue is probed by subscriber_id, never scanned
		// by dataset_id.
		assert.Regexp(t, `WITH sent AS MATERIALIZED`, sql)
		assert.Regexp(t, `q\.subscriber_id = (s|ml)\.subscriber_id`, sql)
		// Cross-vertical disambiguation (measured: 48 wrong-dataset rows matched on
		// subscriber_id alone for the incident campaign).
		assert.Regexp(t, `q\.dataset_id = \$2::uuid`, sql)
		// Idempotency.
		assert.Regexp(t, `q\.last_touch_campaign_id IS DISTINCT FROM \$1::uuid`, sql)
	}
	assert.NotContains(t, stampRecoveryApplySQL, "DELETE")
}

func TestRecoverUnstampedTouches_StampsFromMessageLog(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	const campaign = "ff01ad90-e3fc-4d0e-bc55-239e8ce35d69"
	const dataset = "9502c7c4-68e7-4dcf-91f5-103a1480fe68"

	// Worklist — rides idx_campaigns_org_status_created.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaigns c[\s\S]*c\.organization_id = \$1::uuid[\s\S]*c\.partner_drip_tag IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "dataset", "name"}).
			AddRow(campaign, dataset, "[partner-drip] consumer tot 20260820T213025 31c8ba47 33004d62 [t2]").
			// A campaign with NO partner_dataset_id must be SKIPPED, never guessed at.
			AddRow("no-dataset-campaign", "", "[partner-drip] remodel db 20260820T210000 aa bb"))
	mock.ExpectCommit()

	// Find for the recoverable campaign.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`WITH sent AS MATERIALIZED[\s\S]*FROM mailing_message_log[\s\S]*JOIN partner_clean_queue q ON q\.subscriber_id = s\.subscriber_id`).
		WithArgs(campaign, dataset).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(int64(314)))
	mock.ExpectCommit()

	// Apply — brand resolved from the campaign name, gap in hours from the ladder.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`WITH sent AS MATERIALIZED[\s\S]*UPDATE partner_clean_queue q[\s\S]*FROM sent ml`).
		WithArgs(campaign, dataset, "tot", MaxTouchCount, followupTouchGapHours).
		WillReturnResult(sqlmock.NewResult(0, 314))
	mock.ExpectCommit()
	mock.ExpectExec(`UPDATE partner_drip_stamp_failures[\s\S]*recovered_at = NOW\(\)`).
		WithArgs(campaign, int64(314)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rep, err := po.RecoverUnstampedTouches(context.Background(), 48, true)
	require.NoError(t, err)
	assert.Equal(t, "apply", rep.Mode)
	assert.Equal(t, 2, rep.CampaignsScanned)
	assert.Equal(t, 1, rep.CampaignsWithLoss, "the dataset-less campaign must be skipped, not stamped")
	assert.Equal(t, int64(314), rep.RowsFound)
	assert.Equal(t, int64(314), rep.RowsFixed)
	assert.Equal(t, int64(314), po.StampStats().RowsRecovered)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// 5. reconciliation is idempotent (and dry mode never writes)
// -----------------------------------------------------------------------------

// TestRecoverUnstampedTouches_SecondPassIsNoOp pins re-run safety: after a
// successful pass every candidate row has last_touch_campaign_id = the campaign,
// so the find query returns 0 and NO update is issued at all. This is what makes
// the sweep safe to leave armed across the ECS bounces that re-fire this worker.
func TestRecoverUnstampedTouches_SecondPassIsNoOp(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	const campaign = "ff01ad90-e3fc-4d0e-bc55-239e8ce35d69"
	const dataset = "9502c7c4-68e7-4dcf-91f5-103a1480fe68"

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "dataset", "name"}).
			AddRow(campaign, dataset, "[partner-drip] consumer tot 20260820T213025 aa bb"))
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`WITH sent AS MATERIALIZED[\s\S]*JOIN partner_clean_queue q`).
		WithArgs(campaign, dataset).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(int64(0)))
	mock.ExpectCommit()
	// No apply exec, no ledger write — mock is ordered and strict.

	rep, err := po.RecoverUnstampedTouches(context.Background(), 48, true)
	require.NoError(t, err)
	assert.Equal(t, 0, rep.CampaignsWithLoss)
	assert.Equal(t, int64(0), rep.RowsFixed)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestRecoverUnstampedTouches_DryModeNeverWrites pins that the dry pass reports
// the same row count it would fix while issuing zero writes to the queue.
func TestRecoverUnstampedTouches_DryModeNeverWrites(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	const campaign = "ff01ad90-e3fc-4d0e-bc55-239e8ce35d69"
	const dataset = "9502c7c4-68e7-4dcf-91f5-103a1480fe68"

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "dataset", "name"}).
			AddRow(campaign, dataset, "[partner-drip] consumer tot 20260820T213025 aa bb"))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`WITH sent AS MATERIALIZED[\s\S]*JOIN partner_clean_queue q`).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(int64(314)))
	mock.ExpectCommit()

	rep, err := po.RecoverUnstampedTouches(context.Background(), 48, false)
	require.NoError(t, err)
	assert.Equal(t, "dry", rep.Mode)
	assert.Equal(t, int64(314), rep.RowsFound, "dry mode must report what apply would fix")
	assert.Equal(t, int64(0), rep.RowsFixed)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestStampRecoveryModeDefaultsOn pins the 2026-08-22 gate inversion: with no
// env set the sweep RUNS (apply) — reconcileShippedClaims makes rows mailed
// again, so recovery cannot depend on an operator arming a flag. The gate is
// now an opt-OUT kill switch (PARTNER_DRIP_STAMP_RECOVERY_DISABLED); the
// legacy var survives only as a mode override.
func TestStampRecoveryModeDefaultsOn(t *testing.T) {
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY_DISABLED", "")
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "")
	assert.Equal(t, stampRecoveryApply, currentStampRecoveryMode(), "unset must mean ALWAYS ON")
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "dry")
	assert.Equal(t, stampRecoveryDry, currentStampRecoveryMode())
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "apply")
	assert.Equal(t, stampRecoveryApply, currentStampRecoveryMode())
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "off")
	assert.Equal(t, stampRecoveryOff, currentStampRecoveryMode(), "legacy explicit off still honored")
	// The kill switch wins over everything.
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "apply")
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY_DISABLED", "1")
	assert.Equal(t, stampRecoveryOff, currentStampRecoveryMode(), "kill switch must override the mode var")
}

// TestMaybeRunStampRecovery_InertWhenDisabled proves the kill switch: the tick
// hook issues no query at all — no DB expectations are set, so any query fails.
func TestMaybeRunStampRecovery_InertWhenDisabled(t *testing.T) {
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY_DISABLED", "1")
	po, mock, done := newStampMock(t)
	defer done()
	po.maybeRunStampRecovery(context.Background())
	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// observability — the queryable signal
// -----------------------------------------------------------------------------

// TestEmitStampHeartbeat_ErrorWhileLossOutstanding pins the always-on health
// surface: mailing_worker_heartbeats row 'partner_drip_ladder_stamp' beats 'ok'
// normally and flips to 'error' — with the recovery worklist in last_error —
// while any dropped ladder advance is still outstanding. That row is what
// worker_health.go surfaces in the portal, so the operator no longer depends on
// catching one log line.
func TestEmitStampHeartbeat_ErrorWhileLossOutstanding(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()
	po.cfg.TickInterval = 15 * time.Second

	// Healthy tick.
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(stampHeartbeatWorkerName, "ok", nil, 15).
		WillReturnResult(sqlmock.NewResult(0, 1))
	po.emitStampHeartbeat(context.Background())

	// A wave loses its stamp -> the heartbeat must go 'error' and name the fix.
	po.stamp.rowsLost.Add(314)
	po.stamp.wavesFailed.Add(1)
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(stampHeartbeatWorkerName, "error", sqlmock.AnyArg(), 15).
		WillReturnResult(sqlmock.NewResult(0, 1))
	po.emitStampHeartbeat(context.Background())

	// Once recovered, it self-heals back to 'ok' without operator action.
	po.stamp.rowsRecovered.Add(314)
	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(stampHeartbeatWorkerName, "ok", nil, 15).
		WillReturnResult(sqlmock.NewResult(0, 1))
	po.emitStampHeartbeat(context.Background())

	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// 6. context cancellation mid-retry aborts cleanly
// -----------------------------------------------------------------------------

// TestMarkMailed_ContextCancelDuringRetryAbortsCleanly pins the DoD requirement:
// when the worker is shutting down (ECS bounce mid-wave) the retry loop must
// abort on the FIRST cancellation rather than grinding through its remaining
// attempts, must not start another transaction, and must still record the loss so
// the recovery pass can pick it up on the next boot.
func TestMarkMailed_ContextCancelDuringRetryAbortsCleanly(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	// The deadline (50ms) is shorter than the first backoff (300ms), so the
	// cancellation lands INSIDE the retry wait — not before the first attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	expectStampAttempt(mock).WillReturnError(&pq.Error{
		Code: "57014", Message: "canceling statement due to statement timeout",
	})
	mock.ExpectRollback()
	// No second BEGIN — the loop must not retry a cancelled context.
	mock.ExpectExec(`INSERT INTO partner_drip_stamp_failures`).
		WithArgs("camp-cancel", "tot", int64(2), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	start := time.Now()
	err := po.markMailed(ctx, recs("r1", "r2"), "camp-cancel", "tot")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "must abort on cancellation, not burn every attempt")

	st := po.StampStats()
	assert.Equal(t, int64(2), st.RowsLost)
	assert.Equal(t, int64(1), st.WavesFailed)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkMailed_AlreadyCancelledContextDoesNothing covers the other end: a
// context already dead on entry must not open a transaction at all.
func TestMarkMailed_AlreadyCancelledContextDoesNothing(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock.ExpectExec(`INSERT INTO partner_drip_stamp_failures`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.markMailed(ctx, recs("r1"), "camp-dead", "tot")
	require.Error(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// brand resolution
// -----------------------------------------------------------------------------

func TestDripBrandFromCampaignName(t *testing.T) {
	assert.Equal(t, "tot", dripBrandFromCampaignName(
		"[partner-drip] consumer tot 20260820T213025 31c8ba47 33004d62 [ses:7da27070] [t2]"))
	assert.Equal(t, "db", dripBrandFromCampaignName("[partner-drip] remodel db 20260820T210000 aa bb"))
	// Unknown token -> "" so mailed_brand (which the per-brand daily budget
	// ledger counts on) is never poisoned with a guess.
	assert.Equal(t, "", dripBrandFromCampaignName("[partner-drip] remodel notabrand 2026 aa bb"))
	assert.Equal(t, "", dripBrandFromCampaignName("OFR-CLK db 20260820"))
	assert.Equal(t, "", dripBrandFromCampaignName(""))
}

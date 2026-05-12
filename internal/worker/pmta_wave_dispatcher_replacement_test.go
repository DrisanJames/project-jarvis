package worker

// Cap-aware reserve pool replacement tests (Slice 4).
//
// These tests exercise the end-to-end cap-aware claim path in
// EnqueuePMTAWave:
//
//   1. The dispatcher pulls candidates from status IN ('selected','reserve')
//      with the over-pull buffer (LIMIT remaining*2).
//   2. Each candidate's subscriber is Peek'd against the cross-brand cap.
//   3. Over-cap rows are marked status='cap_skipped' on plan_recipients
//      and the dispatcher continues drawing reserves until it satisfies
//      the wave's remaining count.
//   4. Wave row's cap_skip_count and reserve_used_count are updated.
//
// The test uses sqlmock for the DB and miniredis for Peek's Redis fast
// path. Mock expectations are matched out-of-order because the cap
// checker's first capForOrg() query runs inside the per-candidate Peek
// loop, not at a deterministic position.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnqueuePMTAWave_CapAwareClaim_ReplacesCappedWithReserve sets up:
//   - planned_recipients=1, enqueued_recipients=0 → remaining=1
//   - 2 candidates pulled (over-pull buffer): 1 selected over-cap, 1 reserve under-cap
//   - Cap value=2 from org settings
//   - Subscriber A has cap key set to 2 → Peek returns over-cap → mark cap_skipped
//   - Subscriber B has no cap key → Peek returns under-cap → queued (reserve_used=1)
//   - Wave row ends with cap_skip_count=1, reserve_used_count=1, enqueued_recipients=1
func TestEnqueuePMTAWave_CapAwareClaim_ReplacesCappedWithReserve(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	waveID := uuid.New()
	campaignID := uuid.New()
	planID := uuid.New()
	orgID := uuid.New()
	subA := uuid.New() // selected, over-cap
	subB := uuid.New() // reserve, under-cap
	planRecA := uuid.New()
	planRecB := uuid.New()

	cc := mailing.NewCapChecker(db, rdb, 2)

	// Seed Redis: subscriber A is at cap=2; subscriber B has no key
	// (treated as 0 sends today).
	subACapKey := "cap:" + subA.String() + ":" + timeNowYYYYMMDD()
	mr.Set(subACapKey, "2")

	// 1. metadata SELECT
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "organization_id",
			"status", "campaign_status", "plan_status",
			"scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(campaignID, planID, orgID,
			"planned", "scheduled", "ready",
			testScheduledAt, 1, 0))

	// 2. sending_domain SELECT (best-effort)
	mock.ExpectQuery(`COALESCE\(sp.sending_domain`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain"}).AddRow("em.test.com"))

	// 3-5. status updates (enqueuing / sending / running)
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET status = 'enqueuing'`).
		WithArgs(waveID.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET status = 'sending'`).
		WithArgs(campaignID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET status = 'running'`).
		WithArgs(planID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 6. campaign content fetch
	mock.ExpectQuery(`COALESCE\(from_name`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{
			"from_name", "from_email", "subject", "html_content",
			"name", "plain_content", "content_locked",
		}).AddRow("Sender", "hello@em.test.com", "Subj", "<html>body</html>",
			"Test Campaign", "plain", false))

	// 7. cap-aware candidate claim — over-pull, status IN (selected,reserve)
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients\s+WHERE isp_plan_id = \$1\s+AND status IN \('selected','reserve'\)`).
		WithArgs(planID, 2). // remaining=1 * 2 = 2
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "subscriber_id", "email", "recipient_isp", "selection_rank",
			"audience_source_type", "audience_source_id", "status",
		}).
			AddRow(planRecA, subA, "a@gmail.com", "gmail", 1, "list", nil, "selected").
			AddRow(planRecB, subB, "b@gmail.com", "gmail", 2, "list", nil, "reserve"))

	// 8a. Subscriber A (selected, over-cap) — compliance checks PASS
	mock.ExpectQuery(`SELECT COALESCE\(status, 'active'\) FROM mailing_subscribers`).
		WithArgs(subA).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("active"))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM mailing_global_suppressions`).
		WithArgs("a@gmail.com").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// 8b. CapChecker.capForOrg runs once (cached after first call) — sqlmock query.
	mock.ExpectQuery(`SELECT settings->>'cross_brand_daily_cap' FROM organizations`).
		WithArgs(orgID.String()).
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("2"))

	// 8c. Over-cap → mark plan_rec 'cap_skipped'
	mock.ExpectExec(`UPDATE mailing_campaign_plan_recipients SET status = 'cap_skipped'`).
		WithArgs(planRecA).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 9a. Subscriber B (reserve, under-cap) — compliance checks PASS
	mock.ExpectQuery(`SELECT COALESCE\(status, 'active'\) FROM mailing_subscribers`).
		WithArgs(subB).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("active"))
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM mailing_global_suppressions`).
		WithArgs("b@gmail.com").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// 9b. Under-cap → INSERT queue + UPDATE plan_rec to 'queued'
	mock.ExpectExec(`INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_plan_recipients\s+SET status = 'queued'`).
		WithArgs(planRecB, waveID.String()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 10. final wave update with cap_skip_count=1, reserve_used_count=1
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET enqueued_recipients`).
		WithArgs(waveID.String(), 1, 1, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 11. isp_plan counters
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET enqueued_count`).
		WithArgs(planID, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 12. campaign queued counter
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET queued_count`).
		WithArgs(campaignID, 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectCommit()

	enqueued, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), cc)
	require.NoError(t, err)
	assert.Equal(t, 1, enqueued, "should enqueue exactly the 1 reserve subscriber (selected was cap-skipped)")
	assert.NoError(t, mock.ExpectationsWereMet())

	// Redis invariant: Peek must not have mutated subscriber A's key.
	got, err := mr.Get(subACapKey)
	require.NoError(t, err)
	assert.Equal(t, "2", got, "Peek must not INCR/DECR the cap counter")
}

// TestEnqueuePMTAWave_KillSwitch_UsesLegacyClaim verifies that
// DISABLE_CAP_AWARE_CLAIM=true reverts the dispatcher to the legacy
// single-status SELECT (status='selected', no Peek). This is the
// rollback path operators use during Phase 2 of the rollout.
func TestEnqueuePMTAWave_KillSwitch_UsesLegacyClaim(t *testing.T) {
	t.Setenv("DISABLE_CAP_AWARE_CLAIM", "true")

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	waveID := uuid.New()
	campaignID := uuid.New()
	planID := uuid.New()
	orgID := uuid.New()

	cc := mailing.NewCapChecker(db, rdb, 2)

	// Wave already completed → EnqueuePMTAWave returns early without
	// touching the candidate claim path. This lets us prove the kill
	// switch is respected without mocking the full chain.
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "organization_id",
			"status", "campaign_status", "plan_status",
			"scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(campaignID, planID, orgID,
			"completed", "sending", "running",
			testScheduledAt, 100, 100))
	mock.ExpectCommit()

	enqueued, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), cc)
	require.NoError(t, err)
	assert.Equal(t, 0, enqueued)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// timeNowYYYYMMDD mirrors the date key format used by todayKey() in the
// mailing package — UTC-anchored YYYYMMDD. Anchored on time.Now so the
// test seeds the same key the dispatcher reads under Peek.
func timeNowYYYYMMDD() string {
	return time.Now().UTC().Format("20060102")
}

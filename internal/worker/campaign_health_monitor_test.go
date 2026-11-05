package worker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthMonitor_LogsWarningButNeverPauses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	startedAt := time.Now().Add(-10 * time.Minute)
	campID := "camp-high-bounce"

	mock.ExpectQuery("SELECT id, sent_count").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "sent_count", "bounce_count", "hard_bounce_count", "started_at",
		}).AddRow(campID, 200, 25, 20, startedAt)) // 12.5% bounce rate

	// Bounce breakdown query from pmta_acct_daily_summary
	mock.ExpectQuery("SELECT COALESCE").
		WillReturnRows(sqlmock.NewRows([]string{"hard", "rep", "soft"}).
			AddRow(5, 15, 5))

	// Auto-pause is disabled — only a health_warning update expected (bounce rate > 5%)
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))

	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.checkCampaigns()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHealthMonitor_NoPauseBelowThreshold(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	startedAt := time.Now().Add(-5 * time.Minute)

	mock.ExpectQuery("SELECT id, sent_count").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "sent_count", "bounce_count", "hard_bounce_count", "started_at",
		}).AddRow("camp-ok", 500, 10, 5, startedAt)) // 2% bounce rate — below both thresholds

	// No pause or warning expected

	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.checkCampaigns()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHealthMonitor_NoPauseAfter30Minutes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	startedAt := time.Now().Add(-45 * time.Minute) // 45 min ago, outside the window

	mock.ExpectQuery("SELECT id, sent_count").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "sent_count", "bounce_count", "hard_bounce_count", "started_at",
		}).AddRow("camp-old", 500, 60, 50, startedAt)) // 12% bounce rate but past 30 min window

	// Bounce breakdown query from pmta_acct_daily_summary
	mock.ExpectQuery("SELECT COALESCE").
		WillReturnRows(sqlmock.NewRows([]string{"hard", "rep", "soft"}).
			AddRow(10, 40, 10))

	// Expect warning update (>5%) but no auto-pause
	mock.ExpectExec("UPDATE mailing_campaigns").
		WillReturnResult(sqlmock.NewResult(0, 1))

	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.checkCampaigns()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordCompletedCampaignMetrics(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT r.id, r.executed_campaign_id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "executed_campaign_id", "status",
			"sent_count", "delivered_count", "bounce_count",
			"hard_bounce_count", "soft_bounce_count",
			"open_count", "click_count", "complaint_count",
		}).AddRow(
			"rec-1", "camp-1", "completed",
			1000, 950, 20, 15, 5, 300, 50, 2,
		))

	mock.ExpectExec("UPDATE agent_campaign_recommendations").
		WithArgs("rec-1", sqlmock.AnyArg(), "completed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.recordCompletedMetrics()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func testContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// fakeAlerter records every SendSMS call so tests can assert on them.
type fakeAlerter struct {
	calls []struct{ To, Body string }
	err   error
}

func (f *fakeAlerter) SendSMS(ctx context.Context, to, body string) (string, error) {
	f.calls = append(f.calls, struct{ To, Body string }{to, body})
	if f.err != nil {
		return "", f.err
	}
	return "SM-test", nil
}

func TestCheckLateCampaigns_FiresSMSAndStampsDedup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	scheduled := time.Now().Add(-12 * time.Minute)

	mock.ExpectQuery(`SELECT id, COALESCE\(name, ''\), scheduled_at\s+FROM mailing_campaigns`).
		WithArgs(300, 21600). // 5min threshold, 6h realert
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "scheduled_at"}).
			AddRow("camp-late-1", "Apr22 QF Welcome", scheduled))
	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM mailing_campaigns`).
		WithArgs(300, 21600).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// Only one recipient → expect exactly one stamp after one successful post.
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET late_alert_sent_at = NOW\(\), updated_at = NOW\(\)\s+WHERE id = ANY\(\$1::uuid\[\]\)`).
		WithArgs(pq.Array([]string{"camp-late-1"})).
		WillReturnResult(sqlmock.NewResult(0, 1))

	alerter := &fakeAlerter{}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.SetLatenessAlerter(alerter, []string{"+18777804236"}, 5*time.Minute, 6*time.Hour)
	m.checkLateCampaigns()

	require.Len(t, alerter.calls, 1, "expected one post")
	assert.Equal(t, "+18777804236", alerter.calls[0].To)
	lines := strings.Split(alerter.calls[0].Body, "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "1 campaign late · worst +12m", lines[0])
	assert.Equal(t, "`Apr22 QF Welcome` · +12m", lines[1])
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Roll-up shape: 7 late in the LIMIT-25 select, 31 late in total →
// ONE post, headline carries the separate COUNT and the worst delay, body is
// the top 5 by delay, and ONE UPDATE stamps all 7 reported ids.
func TestCheckLateCampaigns_RollUpShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "name", "scheduled_at"})
	var ids []string
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("camp-%d", i)
		ids = append(ids, id)
		// ORDER BY scheduled_at ASC: camp-0 is the oldest (worst).
		rows.AddRow(id, fmt.Sprintf("Cell %d", i), now.Add(-time.Duration(90-i*10)*time.Minute))
	}
	mock.ExpectQuery(`SELECT id, COALESCE\(name, ''\), scheduled_at\s+FROM mailing_campaigns`).
		WithArgs(900, 86400).
		WillReturnRows(rows)
	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM mailing_campaigns`).
		WithArgs(900, 86400).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(31))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET late_alert_sent_at`).
		WithArgs(pq.Array(ids)).
		WillReturnResult(sqlmock.NewResult(0, 7))

	alerter := &fakeAlerter{}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.SetLatenessAlerter(alerter, []string{"slack"}, 15*time.Minute, 24*time.Hour)
	m.checkLateCampaigns()

	require.Len(t, alerter.calls, 1, "one roll-up per tick, not one per campaign")
	lines := strings.Split(alerter.calls[0].Body, "\n")
	require.Len(t, lines, 1+lateLimitBodyLines, "headline + top 5")
	assert.Equal(t, "31 campaigns late · worst +90m", lines[0])
	assert.Equal(t, "`Cell 0` · +90m", lines[1])
	assert.Equal(t, "`Cell 4` · +50m", lines[5])
	assert.NotContains(t, alerter.calls[0].Body, "Cell 5")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckLateCampaigns_Disabled_NoQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	// No queries expected — the worker should short-circuit.

	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.checkLateCampaigns()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckLateCampaigns_AllSMSFailed_DoesNotStampDedup(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	scheduled := time.Now().Add(-10 * time.Minute)
	mock.ExpectQuery(`SELECT id, COALESCE\(name, ''\), scheduled_at\s+FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "scheduled_at"}).
			AddRow("camp-late-2", "stuck", scheduled))
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// NO UPDATE expected — dedup is only stamped after a successful SMS.

	alerter := &fakeAlerter{err: assertErr("twilio down")}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.SetLatenessAlerter(alerter, []string{"+18777804236"}, 5*time.Minute, 6*time.Hour)
	m.checkLateCampaigns()

	require.Len(t, alerter.calls, 1)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckLateCampaigns_MultipleRecipients_OneSuccessStillStamps(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	scheduled := time.Now().Add(-8 * time.Minute)
	mock.ExpectQuery(`SELECT id, COALESCE\(name, ''\), scheduled_at\s+FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "scheduled_at"}).
			AddRow("camp-late-3", "", scheduled))
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET late_alert_sent_at`).
		WithArgs(pq.Array([]string{"camp-late-3"})).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Fail first recipient, succeed second. We simulate this by flipping the
	// alerter's err off after the first call.
	alerter := &flakyAlerter{failUntil: 1}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.SetLatenessAlerter(alerter, []string{"+15550001", "+15550002"}, 5*time.Minute, 6*time.Hour)
	m.checkLateCampaigns()

	require.Len(t, alerter.calls, 2)
	assert.NoError(t, mock.ExpectationsWereMet())
}

type flakyAlerter struct {
	calls     []struct{ To, Body string }
	n         int
	failUntil int // fail the first N calls
}

func (f *flakyAlerter) SendSMS(ctx context.Context, to, body string) (string, error) {
	f.calls = append(f.calls, struct{ To, Body string }{to, body})
	f.n++
	if f.n <= f.failUntil {
		return "", assertErr("transient")
	}
	return "SM-ok", nil
}

type assertErr string

func (a assertErr) Error() string { return string(a) }

// The CTE must come FIRST. The :1232 build matched "SELECT ... WITH q AS" here,
// which Postgres rejects (`syntax error at or near "WITH"`) — the alert was dead
// in prod for an hour while 19 campaigns sat on 265k overdue rows. Anchor the
// regex at the start of the statement so the SELECT-before-WITH shape can never
// pass again.
const stalledDrainRowsRE = `^\s*WITH q AS \(\s+SELECT campaign_id, COUNT\(\*\) AS queued, MIN\(scheduled_at\) AS oldest_due\s+FROM mailing_campaign_queue\s+WHERE status = 'queued'\s+AND scheduled_at < NOW\(\) - interval '30 minutes'\s+GROUP BY campaign_id\s+\)\s+SELECT c.id, COALESCE\(c.name, ''\), q.queued, q.oldest_due\s+FROM q`

const stalledDrainCountRE = `^\s*WITH q AS \(.*\)\s+SELECT COUNT\(\*\), COALESCE\(SUM\(q.queued\), 0\)\s+FROM q`

// TestStalledDrainSQLComposes pins the statement order of both composed queries:
// WITH first, exactly one SELECT after the CTE's closing paren, FROM q after it.
func TestStalledDrainSQLComposes(t *testing.T) {
	for name, sql := range map[string]string{"rows": stalledDrainRowsSQL(), "count": stalledDrainCountSQL()} {
		s := strings.TrimSpace(sql)
		require.Truef(t, strings.HasPrefix(s, "WITH q AS ("), "%s: must start with the CTE, got %q", name, s[:30])
		cteEnd := strings.Index(s, "\n\t\t)")
		require.Greaterf(t, cteEnd, 0, "%s: CTE close not found", name)
		rest := s[cteEnd:]
		require.Equalf(t, 1, strings.Count(rest, "SELECT c") + strings.Count(rest, "SELECT COUNT"), "%s: exactly one outer SELECT after the CTE", name)
		require.Lessf(t, strings.Index(rest, "SELECT"), strings.Index(rest, "FROM q"), "%s: SELECT must precede FROM q", name)
		require.NotContainsf(t, rest, "WITH", "%s: no second WITH after the CTE", name)
	}
}

func drainRows(now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "queued", "oldest_due"}).
		AddRow("camp-d1", "11042026 - DB - InsHome", int64(3585), now.Add(-7*time.Hour)).
		AddRow("camp-d2", "11042026 - MH - InsHome", int64(415), now.Add(-2*time.Hour))
}

func TestStalledDrainSQL_AgeBoundAndCriterion(t *testing.T) {
	assert.Contains(t, stalledDrainFrom, "c.scheduled_at > NOW() - interval '2 days'")
	assert.Contains(t, stalledDrainCTE, "scheduled_at < NOW() - interval '30 minutes'")
	assert.Contains(t, stalledDrainFrom, "wv.status = 'planned'")
	assert.NotContains(t, stalledDrainFrom, "window_end_at")
	assert.Equal(t, 5, stalledDrainEveryTicks)
}

func newDrainMonitor(t *testing.T, alerter SMSAlerter) (*CampaignHealthMonitor, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	t.Cleanup(m.cancel)
	m.SetLatenessAlerter(alerter, []string{"slack"}, 15*time.Minute, 24*time.Hour)
	return m, mock
}

func TestCheckStalledDrain_PostsOneRollUp(t *testing.T) {
	alerter := &fakeAlerter{}
	m, mock := newDrainMonitor(t, alerter)

	now := time.Now()
	// < LIMIT rows → N/M from the rows, no companion aggregate.
	mock.ExpectQuery(stalledDrainRowsRE).WithArgs(pq.Array([]string{})).WillReturnRows(drainRows(now))
	m.checkStalledDrain()

	require.Len(t, alerter.calls, 1)
	lines := strings.Split(alerter.calls[0].Body, "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, fmt.Sprintf("2 campaigns past window still draining · 4000 recipients · oldest due %s UTC",
		now.Add(-7*time.Hour).UTC().Format("15:04")), lines[0])
	assert.Contains(t, lines[1], "`11042026 - DB - InsHome` · 3585 queued")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// LIMIT hit → headline N/M come from the companion aggregate; body stays ≤5.
func TestCheckStalledDrain_LimitHitUsesAggregate(t *testing.T) {
	alerter := &fakeAlerter{}
	m, mock := newDrainMonitor(t, alerter)

	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "name", "queued", "oldest_due"})
	for i := 0; i < stalledDrainLimit; i++ {
		rows.AddRow(fmt.Sprintf("camp-%d", i), fmt.Sprintf("Cell %d", i), int64(100), now.Add(-time.Duration(300-i)*time.Minute))
	}
	mock.ExpectQuery(stalledDrainRowsRE).WillReturnRows(rows)
	mock.ExpectQuery(stalledDrainCountRE).
		WillReturnRows(sqlmock.NewRows([]string{"count", "sum"}).AddRow(40, 95768))
	m.checkStalledDrain()

	require.Len(t, alerter.calls, 1)
	lines := strings.Split(alerter.calls[0].Body, "\n")
	require.Len(t, lines, 1+lateLimitBodyLines)
	assert.True(t, strings.HasPrefix(lines[0], "40 campaigns past window still draining · 95768 recipients · oldest due "), lines[0])
	assert.Len(t, m.drainAlertedAt, stalledDrainLimit, "only returned rows are muted")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckStalledDrain_CooldownSuppressesSecondTick(t *testing.T) {
	alerter := &fakeAlerter{}
	m, mock := newDrainMonitor(t, alerter)

	now := time.Now()
	mock.ExpectQuery(stalledDrainRowsRE).WithArgs(pq.Array([]string{})).WillReturnRows(drainRows(now))
	m.checkStalledDrain()
	require.Len(t, alerter.calls, 1)
	require.Len(t, m.drainAlertedAt, 2)

	// Second tick inside the 1h cooldown: both ids are passed as excluded,
	// the DB returns nothing reportable → no post.
	mock.ExpectQuery(stalledDrainRowsRE).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "queued", "oldest_due"}))
	m.checkStalledDrain()
	require.Len(t, alerter.calls, 1, "second tick inside cooldown must not post")

	// Cooldown expired → ids are no longer excluded and the next tick posts.
	for id := range m.drainAlertedAt {
		m.drainAlertedAt[id] = now.Add(-stalledDrainCooldown - time.Minute)
	}
	mock.ExpectQuery(stalledDrainRowsRE).WithArgs(pq.Array([]string{})).WillReturnRows(drainRows(now))
	m.checkStalledDrain()
	require.Len(t, alerter.calls, 2, "post again after cooldown")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckStalledDrain_MutedIdsPassedAsExcluded(t *testing.T) {
	alerter := &fakeAlerter{}
	m, mock := newDrainMonitor(t, alerter)
	m.drainAlertedAt = map[string]time.Time{"camp-d1": time.Now()}

	mock.ExpectQuery(stalledDrainRowsRE).
		WithArgs(pq.Array([]string{"camp-d1"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "queued", "oldest_due"}))
	m.checkStalledDrain()

	assert.Empty(t, alerter.calls)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckStalledDrain_FailedPostDoesNotMute(t *testing.T) {
	alerter := &fakeAlerter{err: assertErr("slack down")}
	m, mock := newDrainMonitor(t, alerter)

	mock.ExpectQuery(stalledDrainRowsRE).WillReturnRows(drainRows(time.Now()))
	m.checkStalledDrain()

	require.Len(t, alerter.calls, 1)
	assert.Empty(t, m.drainAlertedAt, "failed post must retry next tick")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Negative path: latenessEnabled=false → neither check queries or posts.
func TestLateAndDrain_Disabled_NoQueryNoPost(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	alerter := &fakeAlerter{}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	// nil recipients → SetLatenessAlerter leaves latenessEnabled=false.
	m.SetLatenessAlerter(alerter, nil, 15*time.Minute, 24*time.Hour)
	require.False(t, m.latenessEnabled)
	m.checkLateCampaigns()
	m.checkStalledDrain()

	assert.Empty(t, alerter.calls)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// --- complete-finished janitor (2026-11-05 audit) ---

const completeFinishedRE = `UPDATE mailing_campaigns SET status = 'sent', completed_at = COALESCE\(completed_at, NOW\(\)\), updated_at = NOW\(\)\s+WHERE id IN \(\s+SELECT c.id FROM mailing_campaigns c\s+WHERE c.status = 'sending'`

func TestCompleteFinishedCampaigns_RunsBoundedUpdate(t *testing.T) {
	t.Setenv("CAMPAIGN_COMPLETE_JANITOR_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	t.Cleanup(m.cancel)
	mock.ExpectExec(completeFinishedRE).WithArgs(completeFinishedLimit).WillReturnResult(sqlmock.NewResult(0, 37))
	m.completeFinishedCampaigns()
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCompleteFinishedCampaigns_PredicateMatchesBootMigration(t *testing.T) {
	// The predicate must keep every REQ-082/REQ-089 guard: active queue rows,
	// active waves INCLUDING 'produced', a completed wave, and the landed check.
	for _, frag := range []string{
		"q.status IN ('queued','sending','claimed')",
		"w.status IN ('planned','enqueuing','dispatched','produced')",
		"w2.status = 'completed'",
		"SUM(w3.enqueued_recipients)",
		"<= COALESCE(c.queued_count, 0)",
		"FOR UPDATE OF c SKIP LOCKED",
		"LIMIT $1",
	} {
		assert.Contains(t, completeFinishedCampaignsSQL, frag)
	}
	assert.Equal(t, 200, completeFinishedLimit)
}

func TestCompleteFinishedCampaigns_KillSwitch(t *testing.T) {
	t.Setenv("CAMPAIGN_COMPLETE_JANITOR_DISABLED", "1")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	t.Cleanup(m.cancel)
	m.completeFinishedCampaigns() // no ExpectExec: any query would fail the mock
	require.NoError(t, mock.ExpectationsWereMet())
}

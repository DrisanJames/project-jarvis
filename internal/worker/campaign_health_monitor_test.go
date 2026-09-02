package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

	// Only one recipient → expect exactly one stamp after one successful SMS.
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET late_alert_sent_at`).
		WithArgs("camp-late-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	alerter := &fakeAlerter{}
	m := NewCampaignHealthMonitor(db)
	m.ctx, m.cancel = testContext()
	defer m.cancel()
	m.SetLatenessAlerter(alerter, []string{"+18777804236"}, 5*time.Minute, 6*time.Hour)
	m.checkLateCampaigns()

	require.Len(t, alerter.calls, 1, "expected one SMS")
	assert.Equal(t, "+18777804236", alerter.calls[0].To)
	assert.Contains(t, alerter.calls[0].Body, "campaign late")
	assert.Contains(t, alerter.calls[0].Body, "camp-late-1")
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
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET late_alert_sent_at`).
		WithArgs("camp-late-3").
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

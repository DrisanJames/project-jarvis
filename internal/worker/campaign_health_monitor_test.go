package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthMonitor_AutoPauseHighBounceRate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	startedAt := time.Now().Add(-10 * time.Minute) // 10 min ago, within the 30 min window
	campID := "camp-high-bounce"

	mock.ExpectQuery("SELECT id, sent_count").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "sent_count", "bounce_count", "hard_bounce_count", "started_at",
		}).AddRow(campID, 200, 25, 20, startedAt)) // 12.5% bounce rate

	// Expect pause: campaign, queue, plans, waves
	mock.ExpectExec("UPDATE mailing_campaigns SET status = 'paused'").
		WithArgs(campID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaign_queue SET status = 'paused'").
		WithArgs(campID).
		WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectExec("UPDATE mailing_campaign_isp_plans SET status = 'paused'").
		WithArgs(campID).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("UPDATE mailing_campaign_waves SET status = 'cancelled'").
		WithArgs(campID).
		WillReturnResult(sqlmock.NewResult(0, 3))

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

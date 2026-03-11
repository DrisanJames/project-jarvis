package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testScheduledAt = time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

func TestEnqueuePMTAWave_AlreadyCompleted(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(
			"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"11111111-2222-3333-4444-555555555555",
			"completed", "sending", "running",
			testScheduledAt, 100, 100,
		))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, "wave-1")
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEnqueuePMTAWave_CampaignCancelled(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-2").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(
			"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"11111111-2222-3333-4444-555555555555",
			"planned", "cancelled", "planned",
			testScheduledAt, 100, 0,
		))
	mock.ExpectExec("UPDATE mailing_campaign_waves").
		WithArgs("wave-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, "wave-2")
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEnqueuePMTAWave_PlanPaused(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-3").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(
			"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"11111111-2222-3333-4444-555555555555",
			"planned", "sending", "paused",
			testScheduledAt, 100, 0,
		))
	mock.ExpectExec("UPDATE mailing_campaign_waves").
		WithArgs("wave-3").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, "wave-3")
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEnqueuePMTAWave_ZeroRemaining(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-4").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(
			"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			"11111111-2222-3333-4444-555555555555",
			"planned", "sending", "running",
			testScheduledAt, 100, 100,
		))
	mock.ExpectExec("UPDATE mailing_campaign_waves").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaigns").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaign_isp_plans").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_campaign_waves").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, "wave-4")
	assert.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEnqueuePMTAWave_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("nonexistent").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}))
	mock.ExpectRollback()

	_, err = EnqueuePMTAWave(context.Background(), db, "nonexistent")
	assert.Error(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEnqueuePMTAWave_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	campID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	planID := "11111111-2222-3333-4444-555555555555"
	subID := "22222222-3333-4444-5555-666666666666"

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-happy").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(campID, planID, "planned", "scheduled", "planned", testScheduledAt, 1, 0))

	// Status transitions
	mock.ExpectExec("UPDATE mailing_campaign_waves").WillReturnResult(sqlmock.NewResult(0, 1))      // enqueuing
	mock.ExpectExec("UPDATE mailing_campaigns").WillReturnResult(sqlmock.NewResult(0, 1))            // sending
	mock.ExpectExec("UPDATE mailing_campaign_isp_plans").WillReturnResult(sqlmock.NewResult(0, 1))   // running

	// Load campaign defaults
	mock.ExpectQuery("SELECT COALESCE\\(from_name").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"from_name", "subject", "html_content"}).
			AddRow("Test Sender", "Hello", "<html>body</html>"))

	// Load variants (none = fallback to campaign defaults)
	mock.ExpectQuery("SELECT COALESCE\\(v.variant_name").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"variant_name", "from_name", "subject", "html_content"}))

	// Query plan recipients
	mock.ExpectQuery("SELECT id, subscriber_id, email").
		WithArgs(sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "subscriber_id", "email", "recipient_isp", "selection_rank",
			"audience_source_type", "audience_source_id",
		}).AddRow(
			"33333333-4444-5555-6666-777777777777", subID,
			"user@gmail.com", "gmail", 1, "list", nil,
		))

	// Insert into queue
	mock.ExpectExec("INSERT INTO mailing_campaign_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	// Update plan recipient to queued
	mock.ExpectExec("UPDATE mailing_campaign_plan_recipients").WillReturnResult(sqlmock.NewResult(0, 1))
	// Update wave enqueued count
	mock.ExpectExec("UPDATE mailing_campaign_waves").WillReturnResult(sqlmock.NewResult(0, 1))
	// Update ISP plan enqueued count
	mock.ExpectExec("UPDATE mailing_campaign_isp_plans").WillReturnResult(sqlmock.NewResult(0, 1))
	// Update campaign queued count
	mock.ExpectExec("UPDATE mailing_campaigns").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := EnqueuePMTAWave(context.Background(), db, "wave-happy")
	assert.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

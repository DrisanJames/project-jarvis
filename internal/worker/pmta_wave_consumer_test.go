package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessWaveMessage_ValidMessage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	msg := PMTAWaveMessage{
		WaveID:         "wave-valid",
		CampaignID:     uuid.New().String(),
		ISPPlanID:      uuid.New().String(),
		IdempotencyKey: "key-1",
	}
	body, _ := json.Marshal(msg)

	// EnqueuePMTAWave succeeds: wave already completed → returns early
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-valid").
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(uuid.New(), uuid.New(), "completed", "sending", "running",
			testScheduledAt, 100, 100))
	mock.ExpectCommit()

	shouldDelete := processWaveMessage(context.Background(), db, string(body))
	assert.True(t, shouldDelete)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestProcessWaveMessage_InvalidJSON(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	shouldDelete := processWaveMessage(context.Background(), db, "not-json{{{")
	assert.True(t, shouldDelete, "bad payloads should be deleted to prevent infinite retries")
}

func TestProcessWaveMessage_EnqueueFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	msg := PMTAWaveMessage{
		WaveID:         "wave-fail",
		CampaignID:     uuid.New().String(),
		ISPPlanID:      uuid.New().String(),
		IdempotencyKey: "key-2",
	}
	body, _ := json.Marshal(msg)

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs("wave-fail").
		WillReturnError(assert.AnError)
	mock.ExpectRollback()

	shouldDelete := processWaveMessage(context.Background(), db, string(body))
	assert.False(t, shouldDelete, "transient failures should NOT delete the message so SQS retries")
	assert.NoError(t, mock.ExpectationsWereMet())
}

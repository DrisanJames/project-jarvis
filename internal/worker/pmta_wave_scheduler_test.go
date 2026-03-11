package worker

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testScheduler(db *sql.DB, rdb *redis.Client) *PMTAWaveScheduler {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	return &PMTAWaveScheduler{
		db:          db,
		redisClient: rdb,
		ctx:         ctx,
		cancel:      cancel,
	}
}

func TestDispatchDueWaves_DueWavesFound(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	wave1, wave2, wave3 := uuid.New(), uuid.New(), uuid.New()

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(wave1).AddRow(wave2).AddRow(wave3))

	for _, w := range []uuid.UUID{wave1, wave2, wave3} {
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT w.campaign_id").
			WithArgs(w.String()).
			WillReturnRows(sqlmock.NewRows([]string{
				"campaign_id", "isp_plan_id", "status", "campaign_status",
				"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
			}).AddRow(uuid.New(), uuid.New(), "completed", "sending", "running",
				testScheduledAt, 100, 100))
		mock.ExpectCommit()
	}

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_NoDueWaves(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_LockContention(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	wave1 := uuid.New()

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(wave1))

	// Pre-set the lock key so the scheduler can't acquire it
	mr.Set("lock:pmta-wave:"+wave1.String(), "held-by-another")

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	// No EnqueuePMTAWave expectations — wave was skipped due to lock contention
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDispatchDueWaves_EnqueueError(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	wave1, wave2 := uuid.New(), uuid.New()

	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(wave1).AddRow(wave2))

	// Wave 1: EnqueuePMTAWave fails (DB error on SELECT)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs(wave1.String()).
		WillReturnError(fmt.Errorf("connection reset"))
	mock.ExpectRollback()

	// Wave 2: succeeds (already completed, so returns early)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT w.campaign_id").
		WithArgs(wave2.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"campaign_id", "isp_plan_id", "status", "campaign_status",
			"plan_status", "scheduled_at", "planned_recipients", "enqueued_recipients",
		}).AddRow(uuid.New(), uuid.New(), "completed", "sending", "running",
			testScheduledAt, 100, 100))
	mock.ExpectCommit()

	s := testScheduler(db, rdb)
	defer s.cancel()
	s.dispatchDueWaves()

	assert.NoError(t, mock.ExpectationsWereMet())
}

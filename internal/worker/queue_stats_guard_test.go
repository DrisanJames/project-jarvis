package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

type fakeLock struct {
	grant    bool
	err      error
	acquires int
	releases int
}

func (l *fakeLock) Acquire(context.Context) (bool, error) { l.acquires++; return l.grant, l.err }
func (l *fakeLock) Release(context.Context) error         { l.releases++; return nil }

const reCatalog = `FROM pg_class c\s+WHERE c\.relname IN`

func catalogRows(active, sched int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"relname", "reltuples"}).
		AddRow("idx_queue_active_by_campaign", active).
		AddRow("idx_queue_campaign_status_scheduled", sched)
}

func expectAnalyze(mock sqlmock.Sqlmock, err error) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout = '120000ms'`).WillReturnResult(sqlmock.NewResult(0, 0))
	if err != nil {
		mock.ExpectExec(`ANALYZE mailing_campaign_queue`).WillReturnError(err)
		mock.ExpectRollback()
		return
	}
	mock.ExpectExec(`ANALYZE mailing_campaign_queue`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
}

func newGuard(t *testing.T, depth int64) (*QueueStatsGuard, sqlmock.Sqlmock, *fakeLock, *capturingAlerter, *time.Time) {
	t.Helper()
	db, mock := newSelfCheckMockDB(t)
	lock := &fakeLock{grant: true}
	g := NewQueueStatsGuard(db, lock)
	al := &capturingAlerter{}
	g.SetAlerter(al, []string{"slack"})
	g.depth = func() (int64, bool) { return depth, true }
	now := time.Now()
	g.now = func() time.Time { return now }
	return g, mock, lock, al, &now
}

func TestQueueStatsGuard_StaleTwice_OneAnalyzeOneNotify(t *testing.T) {
	g, mock, lock, al, _ := newGuard(t, 652000)
	before := statsGuardAnalyzes.Load()
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	expectAnalyze(mock, nil)
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0)) // still stale (stats not
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0)) // re-read in a mock): cooldown holds

	g.runOnce(context.Background())
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	g.runOnce(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 1, lock.acquires)
	require.Equal(t, 1, lock.releases)
	require.Equal(t, before+1, statsGuardAnalyzes.Load())
	msgs := al.drain()
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Body, "Queue planner statistics · were stale, refreshed")
	require.Contains(t, msgs[0].Body, "Live queued: 652,000 rows")
	require.NotNil(t, CurrentSendLiveness().StatsGuardLastAt)
}

func TestQueueStatsGuard_StaleOnce_None(t *testing.T) {
	g, mock, lock, al, _ := newGuard(t, 652000)
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(640000, 600000)) // fresh again
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))           // streak restarts at 1
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Zero(t, lock.acquires)
	require.Empty(t, al.drain())
}

func TestQueueStatsGuard_OneIndexStaleIsStale(t *testing.T) {
	g, mock, lock, _, _ := newGuard(t, 652000)
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(640000, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(640000, 0))
	expectAnalyze(mock, nil)
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 1, lock.acquires)
}

func TestQueueStatsGuard_BelowFloorOrUnknown_None(t *testing.T) {
	g, mock, lock, al, _ := newGuard(t, 19999)
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	g.depth = func() (int64, bool) { return 0, false } // unmeasured depth
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet(), "no catalog read below the floor")
	require.Zero(t, lock.acquires)
	require.Empty(t, al.drain())
}

func TestQueueStatsGuard_LockNotAcquired_None(t *testing.T) {
	g, mock, lock, al, _ := newGuard(t, 652000)
	lock.grant = false
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet(), "no ANALYZE without the lock")
	require.Equal(t, 1, lock.acquires)
	require.Zero(t, lock.releases)
	require.Empty(t, al.drain())
}

func TestQueueStatsGuard_ObserveMode_NoExec(t *testing.T) {
	t.Setenv("QUEUE_STATS_GUARD", "observe")
	g, mock, lock, al, _ := newGuard(t, 652000)
	require.Equal(t, "observe", g.Mode())
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet(), "observe never runs ANALYZE")
	require.Zero(t, lock.acquires)
	msgs := al.drain()
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Body, "observe mode")
}

func TestQueueStatsGuard_Off(t *testing.T) {
	t.Setenv("QUEUE_STATS_GUARD", "off")
	g, mock, lock, _, _ := newGuard(t, 652000)
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Zero(t, lock.acquires)
}

func TestQueueStatsGuard_ExecError_CountedRetriedAfterCooldown(t *testing.T) {
	g, mock, lock, al, now := newGuard(t, 652000)
	before := statsGuardErrors.Load()
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	expectAnalyze(mock, errors.New("pq: canceling statement due to statement timeout"))
	require.NotPanics(t, func() {
		g.runOnce(context.Background())
		g.runOnce(context.Background())
	})
	require.Equal(t, before+1, statsGuardErrors.Load())
	require.Empty(t, al.drain(), "a failed ANALYZE does not claim a refresh")

	// Inside cooldown: stale twice again, no exec.
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	g.runOnce(context.Background())
	g.runOnce(context.Background())
	require.Equal(t, 1, lock.acquires)

	// After cooldown: retried.
	*now = now.Add(queueStatsGuardCooldown + time.Second)
	mock.ExpectQuery(reCatalog).WillReturnRows(catalogRows(0, 0))
	expectAnalyze(mock, nil)
	g.runOnce(context.Background())
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 2, lock.acquires)
	require.Len(t, al.drain(), 1)
}

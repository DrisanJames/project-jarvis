package worker

import (
	"bytes"
	"context"
	"log"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
)

// The lease holds its advisory lock on a DEDICATED connection (db.Conn) for
// the tick's duration and unlocks on that SAME connection — the 2026-08-22
// incident was Acquire/Release landing on different pooled connections, which
// left the lock held by an idle conn and wedged every tick on BOTH ECS tasks.
// sqlmock serves db.Conn transparently, so these tests exercise the real path.

// Kill switch restores dual-tick: no DB traffic at all.
func TestAcquireTickLease_DisabledSkipsLock(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "1")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead)
	release()
	assert.NoError(t, mock.ExpectationsWereMet(), "disabled lease must issue zero queries")
}

// Lease held elsewhere: this instance must NOT lead, the dedicated conn goes
// back to the pool, and NO unlock is issued (we never owned the lock).
func TestAcquireTickLease_NotAcquiredSkips(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.False(t, lead)
	assert.Nil(t, release, "not-acquired returns a nil release")
	assert.Equal(t, 0, db.Stats().InUse, "dedicated conn must be closed (returned to pool)")
	assert.NoError(t, mock.ExpectationsWereMet(), "no unlock may be issued when not acquired")
}

// Acquired: leads; release unlocks on the same conn and the unlock reports true.
func TestAcquireTickLease_AcquiredLeadsAndReleases(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`pg_advisory_unlock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead)
	release()
	assert.Equal(t, 0, db.Stats().InUse, "dedicated conn must be closed after release")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Unlock returning false (lock not held by this session) logs a WARNING and
// must not panic — this is the exact silent-no-op signature of the incident.
func TestAcquireTickLease_UnlockFalseWarnsNoPanic(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`pg_advisory_unlock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(false))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead)

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	assert.NotPanics(t, release)
	log.SetOutput(prev)

	assert.Contains(t, buf.String(), "WARNING", "unlock=false must log a warning")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Dedicated-conn checkout error: FAIL OPEN — the drip must tick.
func TestAcquireTickLease_ConnErrorFailsOpen(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, _, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	db.Close() // db.Conn now errors (sql.ErrConnDone path)
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead, "conn checkout error must fail open")
	assert.NotPanics(t, release)
}

// Acquire-query error: FAIL OPEN — the drip must tick, conn returned to pool.
func TestAcquireTickLease_AcquireErrorFailsOpen(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).WillReturnError(assert.AnError)
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead, "acquire error must fail open")
	assert.NotPanics(t, release)
	assert.Equal(t, 0, db.Stats().InUse, "dedicated conn must be closed on acquire error")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// REGRESSION (the 2026-08-22 wedge): two sequential acquire/release cycles on
// the SAME orchestrator must BOTH succeed. Under the pooled-lock code the
// first release was a silent no-op on the wrong connection, so cycle 2 (and
// every tick after it, on both tasks) got acquired=false forever.
func TestAcquireTickLease_TwoSequentialCyclesBothAcquire(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	// lock, unlock, lock, unlock — in order, all served.
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`pg_advisory_unlock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery(`pg_advisory_unlock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}

	release1, lead1 := po.acquireTickLease()
	assert.True(t, lead1, "cycle 1 must acquire")
	release1()

	release2, lead2 := po.acquireTickLease()
	assert.True(t, lead2, "cycle 2 must acquire — the incident wedge was here")
	release2()

	assert.Equal(t, 0, db.Stats().InUse)
	assert.NoError(t, mock.ExpectationsWereMet())
}

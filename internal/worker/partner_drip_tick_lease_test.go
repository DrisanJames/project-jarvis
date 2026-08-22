package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
)

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

// Lease held elsewhere: this instance must NOT lead.
func TestAcquireTickLease_NotAcquiredSkips(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	_, lead := po.acquireTickLease()
	assert.False(t, lead)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Acquired: leads, and release unlocks.
func TestAcquireTickLease_AcquiredLeadsAndReleases(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectExec(`pg_advisory_unlock`).WillReturnResult(sqlmock.NewResult(0, 0))
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	release, lead := po.acquireTickLease()
	assert.True(t, lead)
	release()
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Lock-infra error: FAIL OPEN — the drip must tick.
func TestAcquireTickLease_ErrorFailsOpen(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	assert.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`pg_try_advisory_lock`).WillReturnError(assert.AnError)
	po := &PartnerDripOrchestrator{db: db, ctx: context.Background()}
	_, lead := po.acquireTickLease()
	assert.True(t, lead, "acquire error must fail open")
	assert.NoError(t, mock.ExpectationsWereMet())
}

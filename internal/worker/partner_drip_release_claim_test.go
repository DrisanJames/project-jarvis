package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReleaseClaim_RestoresLadderStatus pins the 2026-08-05 outage fix: a
// released claim must return a FOLLOW-UP record (touch_count >= 1, already
// t1-mailed) to 'mailed' — the pool the t2..tN pass claims from — and only a
// true first-touch claim to 'ready'. A flat SET status='ready' ejects
// mid-ladder records into the first-touch pool (duplicate t1s, follow-up
// famine); this test fails if the CASE ladder-restore is ever removed.
func TestReleaseClaim_RestoresLadderStatus(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	// The release UPDATE must branch on touch_count + mailed_campaign_id,
	// restoring 'mailed' for mid-ladder records.
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = CASE\s+WHEN COALESCE\(touch_count, 0\) >= 1 AND mailed_campaign_id IS NOT NULL\s+THEN 'mailed'\s+ELSE 'ready'\s+END`).
		WithArgs("{rec-1,rec-2}").
		WillReturnResult(sqlmock.NewResult(0, 2))

	po := &PartnerDripOrchestrator{db: db}
	err = po.releaseClaim(context.Background(), []claimedRecord{
		{id: "rec-1"}, {id: "rec-2"},
	})
	require.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

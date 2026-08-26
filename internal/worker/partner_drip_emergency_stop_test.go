package worker

// REQ-004 regression guards (findings 2026-07-13-E §1): the Data Partners
// "Emergency stop" flag (partner_datasets.paused_emergency) must be honored
// by every partner_clean_queue claim query, not just the ingest slicer.
// Before the fix, already-sliced 'ready'/'mailed' rows of a stopped dataset
// kept being claimed and MAILED on subsequent orchestrator ticks.
//
// The behavioral contract pinned here: each claim's SQL carries the
// NOT EXISTS(partner_datasets.paused_emergency) predicate (the sqlmock
// regexes below fail if the predicate is removed), the FOR UPDATE SKIP LOCKED
// clause still locks only partner_clean_queue (partner_datasets never enters
// a FROM clause — it appears only inside the NOT EXISTS subquery), and a
// resumed dataset's rows claim again exactly as before.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var dripClaimCols = []string{"id", "email", "email_md5", "isp_family", "dataset_id", "partner_id", "batch_id", "extra_metadata"}

// pausePredicateRe matches the emergency-stop exclusion inside a claim CTE
// followed by the (unchanged) SKIP LOCKED claim semantics. If either the
// predicate or the locking clause is dropped, the expectation fails.
const pausePredicateRe = `(?s)NOT EXISTS \(\s*SELECT 1 FROM partner_datasets d\s*WHERE d\.id = partner_clean_queue\.dataset_id\s*AND d\.paused_emergency\s*\).*FOR UPDATE SKIP LOCKED`

func TestEmergencyStop_ClaimRecordsExcludesPausedDatasets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	// Paused: the predicate filters the dataset's ready rows out at the DB —
	// the claim returns nothing and nothing is flipped to 'claimed'.
	mock.ExpectQuery(`(?s)WITH picked AS.*status = 'ready' AND vertical = \$1.*`+pausePredicateRe).
		WithArgs("refi_heloc", 100).
		WillReturnRows(sqlmock.NewRows(dripClaimCols))
	got, err := po.claimRecords(context.Background(), "refi_heloc", 100)
	require.NoError(t, err)
	assert.Empty(t, got, "ready rows of an emergency-stopped dataset must not be claimed")

	// Resumed (paused_emergency=false): the same rows are claimable again.
	mock.ExpectQuery(pausePredicateRe).
		WithArgs("refi_heloc", 100).
		WillReturnRows(sqlmock.NewRows(dripClaimCols).
			AddRow("rec-1", "a@example.com", "d41d8cd9", "gmail", "ds-1", "p-1", "b-1", []byte(`{}`)))
	got, err = po.claimRecords(context.Background(), "refi_heloc", 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ds-1", got[0].datasetID)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEmergencyStop_ClaimRecordsByISPCapsExcludesPausedDatasets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	// The predicate must appear in BOTH the ranked CTE (so paused rows don't
	// consume per-ISP rank slots) and the picked re-check (matching the
	// status='ready' re-check idiom).
	// Anchored on `ranked AS` rather than `WITH ranked AS`: the caps CTE was
	// moved ahead of ranked on 2026-08-11 so ranked can bucket unknown ISPs to
	// 'other' (see TestClaimByISPCaps_UnknownISPBucketsToOther). CTE ORDER is not
	// what this test is about — both paused_emergency guards still are, and both
	// are still required below.
	claimRe := `(?s)ranked AS.*status = 'ready' AND vertical = \$1\s*AND NOT EXISTS.*d\.paused_emergency.*picked AS.*status = 'ready'\s*AND NOT EXISTS.*d\.paused_emergency.*FOR UPDATE SKIP LOCKED`

	// Paused dataset: no rows come back.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(claimRe).
		WillReturnRows(sqlmock.NewRows(dripClaimCols))
	mock.ExpectCommit()
	got, err := po.claimRecordsByISPCaps(context.Background(), "personal_loans", "db", map[string]int{"gmail": 50}, 200)
	require.NoError(t, err)
	assert.Empty(t, got, "ISP-cap claim must not pick rows of an emergency-stopped dataset")

	// Resumed: rows flow again.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(claimRe).
		WillReturnRows(sqlmock.NewRows(dripClaimCols).
			AddRow("rec-2", "b@example.com", "abc123", "gmail", "ds-2", "p-1", "b-2", []byte(`{}`)))
	mock.ExpectCommit()
	got, err = po.claimRecordsByISPCaps(context.Background(), "personal_loans", "db", map[string]int{"gmail": 50}, 200)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ds-2", got[0].datasetID)

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestEmergencyStop_ClaimFollowupRecordsExcludesPausedDatasets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}

	// Phase 1: dominant-touch pick (unchanged, no pause predicate needed —
	// it selects nothing, only sizes the wave).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Rotation (2026-08-24) made this pick return every eligible touch with its
	// due count, so the wave can take its turn instead of always serving the
	// biggest pool. Shape changed; the pause guarantee below did not.
	mock.ExpectQuery(`(?s)SELECT touch_count, COUNT\(\*\) AS due\s+FROM partner_clean_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"touch_count", "due"}).AddRow(2, 10))
	mock.ExpectCommit()

	// Phase 2: the follow-up claim — touch 2/3/4 sends of a stopped dataset
	// must stop too, so the same predicate guards ranked AND picked here.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)WITH ranked AS.*status = 'mailed'.*terminal_reason IS NULL\s*AND NOT EXISTS.*d\.paused_emergency.*picked AS.*terminal_reason IS NULL\s*AND NOT EXISTS.*d\.paused_emergency.*FOR UPDATE SKIP LOCKED`).
		WillReturnRows(sqlmock.NewRows(dripClaimCols))
	mock.ExpectCommit()

	got, err := po.claimFollowupRecordsByISPCaps(context.Background(), "refi_heloc", "db", map[string]int{"gmail": 25}, 100)
	require.NoError(t, err)
	assert.Empty(t, got, "follow-up touches of an emergency-stopped dataset must not be claimed")
	assert.NoError(t, mock.ExpectationsWereMet())
}

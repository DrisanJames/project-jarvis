package worker

// Intake/sending split (operator ruling, brain #3823): the slicer is an INTAKE
// gate and reads partner_datasets.intake_paused; the drip claim predicates are
// SENDING gates and stay on partner_datasets.paused_emergency. All 62 prod
// datasets are paused for sending today and must keep slicing into
// partner_clean_queue as 'ready'.
//
// sqlmock cannot evaluate a WHERE clause, so the contract is pinned on the
// query TEXT: the claim SQL must reference intake_paused and must not
// reference paused_emergency (and vice versa for the drip predicate).

import (
	"context"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func slicerIntakeMockDB(t *testing.T) (*sqlmock.Sqlmock, *PartnerSlicer) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(_ string, actual string) error {
		if strings.Contains(actual, "paused_emergency") {
			t.Errorf("slicer intake gate must not read paused_emergency (the SENDING pause):\n%s", actual)
		}
		if !strings.Contains(actual, "intake_paused") {
			t.Errorf("slicer intake gate must read intake_paused:\n%s", actual)
		}
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &mock, NewPartnerSlicer(db, nil, "bucket", nil, PartnerSlicerConfig{})
}

func TestPartnerSlicer_ClaimNextBatchGatesOnIntakePaused(t *testing.T) {
	mock, ps := slicerIntakeMockDB(t)
	(*mock).ExpectBegin()
	(*mock).ExpectQuery("claimNextBatch").WillReturnRows(sqlmock.NewRows([]string{
		"id", "dataset_id", "partner_id", "vertical",
		"s3_bucket", "s3_key", "record_count", "next_record_offset",
		"landing_status", "supply_class", "source_path",
	}))
	(*mock).ExpectRollback()

	b, err := ps.claimNextBatch(context.Background())
	require.NoError(t, err)
	require.Nil(t, b)
	require.NoError(t, (*mock).ExpectationsWereMet())
}

func TestPartnerSlicer_IsDatasetPausedReadsIntakePaused(t *testing.T) {
	for _, paused := range []bool{true, false} {
		mock, ps := slicerIntakeMockDB(t)
		(*mock).ExpectQuery("isDatasetPaused").WithArgs("ds-1").
			WillReturnRows(sqlmock.NewRows([]string{"intake_paused"}).AddRow(paused))
		got, err := ps.isDatasetPaused(context.Background(), "ds-1")
		require.NoError(t, err)
		require.Equal(t, paused, got)
		require.NoError(t, (*mock).ExpectationsWereMet())
	}
}

// Drip claims are SENDING gates: the predicate stays on paused_emergency.
func TestDripClaimPredicate_StaysOnSendingPause(t *testing.T) {
	require.Contains(t, datasetNotEmergencyPausedSQL, "d.paused_emergency")
	require.NotContains(t, datasetNotEmergencyPausedSQL, "intake_paused")
}

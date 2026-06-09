package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIspCapForDrainHorizon(t *testing.T) {
	const wavesPerDay = 384 // 96 ticks × 4 brands @ 15m cadence

	tests := []struct {
		name       string
		ready      int
		base       int
		drainDays  int
		want       int
	}{
		{"gmail 3d refi backlog", 15591, 200, 3, 14},
		{"gmail 3d system backlog", 51758, 200, 3, 45},
		{"yahoo 3d", 9991, 20, 3, 9},
		{"sbcglobal 3d", 4668, 60, 3, 5},
		{"aol 3d", 5827, 20, 3, 6},
		{"att 2d", 1915, 60, 2, 3},
		{"empty backlog", 0, 200, 3, 0},
		{"small backlog min 1", 100, 200, 3, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ispCapForDrainHorizon(tc.ready, tc.base, tc.drainDays, wavesPerDay)
			if got != tc.want {
				t.Fatalf("ispCapForDrainHorizon(%d, %d, %d, %d) = %d, want %d",
					tc.ready, tc.base, tc.drainDays, wavesPerDay, got, tc.want)
			}
		})
	}
}

func TestIspCapForDrainHorizonQueueRefill(t *testing.T) {
	const wavesPerDay = 384
	base := 200
	days := 3

	// Queue doubles mid-drain — cap should double while staying under base.
	low := ispCapForDrainHorizon(50000, base, days, wavesPerDay)
	high := ispCapForDrainHorizon(100000, base, days, wavesPerDay)
	if high <= low {
		t.Fatalf("refilled queue should raise cap: low=%d high=%d", low, high)
	}
	if high > base {
		t.Fatalf("cap must not exceed base ceiling: got %d base %d", high, base)
	}
}

// TestResolvePerISPCaps_DatasetOverride verifies that a per-dataset
// max_per_wave row in partner_isp_distribution_overrides REPLACES the global
// per-ISP cap for that dataset only, and that the drain-horizon clamp is then
// computed against the overridden base (so a raised cap is honored at high
// backlog). yahoo: global 8 -> override 20; aol: global 8 -> override 39.
func TestResolvePerISPCaps_DatasetOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "6cb7292a-0702-4497-b63f-e1fb5006227d"
	const vertical = "samsclub_internal"

	// 1) Dataset override fetch (inside withDBTimeout txn).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"}).
			AddRow("yahoo", 20).
			AddRow("aol", 39))
	mock.ExpectCommit()

	// 2) readyByISP recompute for drain-horizon (yahoo is in PerISPDrainDays).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 1015828).
			AddRow("aol", 175139))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "aol": 8, "other": 40},
		PerISPDrainDays:  map[string]int{"yahoo": 3},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)

	// yahoo: override base 20; drain-horizon ceil(1015828 / (384*3)=1152)=882, min(20,882)=20.
	assert.Equal(t, 20, caps["yahoo"], "yahoo override base honored under high backlog")
	// aol: override base 39, not in PerISPDrainDays -> stays at the override.
	assert.Equal(t, 39, caps["aol"], "aol override applied verbatim")
	// other: untouched global cap.
	assert.Equal(t, 40, caps["other"], "non-overridden ISP keeps global cap")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResolvePerISPCaps_NoDatasetOverride confirms that when a dataset has no
// override rows, the global reputation-protective caps are preserved (yahoo 8).
func TestResolvePerISPCaps_NoDatasetOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "00000000-0000-0000-0000-000000000abc"
	const vertical = "refi_heloc"

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"})) // empty
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 1000000))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "other": 40},
		PerISPDrainDays:  map[string]int{"yahoo": 3},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)
	assert.Equal(t, 8, caps["yahoo"], "no override -> global protective cap preserved")
	assert.NoError(t, mock.ExpectationsWereMet())
}

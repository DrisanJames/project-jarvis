package worker

// Per-drip daily ISP budget tests (drip-specific caps doctrine, core.md §14
// 2026-08-06). A daily_cap row in partner_isp_distribution_overrides gives a
// dataset a lane-owned daily budget for that ISP — counted per (dataset, ISP)
// across all brands — with precedence per-drip override → global per-brand env
// default. The legacy per-brand behavior must stay byte-identical whenever no
// override row exists (the doctrine's migration constraint).

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const laneTestDataset = "4fdf1a6d-e375-4bc5-a739-ae1a425298d2"

func laneCapPO(t *testing.T, envCaps map[string]int) (*PartnerDripOrchestrator, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		NewRecordDailyISPCaps: envCaps,
	}}, mock
}

func expectLaneOverrideLoad(mock sqlmock.Sqlmock, datasetID string, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(rows)
	mock.ExpectCommit()
}

// A lane daily_cap REPLACES the shared per-brand env value for that ISP, and
// usage is counted per (dataset, ISP) — not per brand.
func TestLaneDailyCap_OverrideReplacesEnvAndCountsPerDataset(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	po, mock := laneCapPO(t, map[string]int{"gmail": 1000})

	expectLaneOverrideLoad(mock, laneTestDataset,
		sqlmock.NewRows([]string{"isp", "daily_cap"}).AddRow("gmail", 500))
	// Lane usage count: keyed by DATASET (all brands), not mailed_brand.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(laneTestDataset, "gmail").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(450))
	mock.ExpectCommit()

	in := map[string]int{"gmail": 200, "yahoo": 32}
	out := po.applyNewRecordDailyBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 50, out["gmail"], "lane budget 500-450=50 replaces the env 1000 entirely")
	assert.Equal(t, 32, out["yahoo"], "ISP with no lane row and no env cap is untouched")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// daily_cap=0 hard-suppresses the ISP for the lane with no usage-count query.
func TestLaneDailyCap_ZeroHardSuppressesLane(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	po, mock := laneCapPO(t, nil)

	expectLaneOverrideLoad(mock, laneTestDataset,
		sqlmock.NewRows([]string{"isp", "daily_cap"}).AddRow("gmail", 0))

	in := map[string]int{"gmail": 200, "yahoo": 32}
	out := po.applyNewRecordDailyBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 0, out["gmail"], "lane daily_cap=0 must zero the per-wave cap")
	assert.Equal(t, 32, out["yahoo"], "other ISPs untouched")
	assert.NoError(t, mock.ExpectationsWereMet(), "cap==0 must not run a usage count")
}

// With a dataset but NO override rows, the legacy per-brand env path must run
// byte-identical: same query shape, same (brand, isp) args.
func TestLaneDailyCap_NoOverrideRowsLegacyByteIdentical(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	po, mock := laneCapPO(t, map[string]int{"yahoo": 100})

	expectLaneOverrideLoad(mock, laneTestDataset,
		sqlmock.NewRows([]string{"isp", "daily_cap"}))
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("db", "yahoo"). // per-BRAND args — the legacy scope
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(90))
	mock.ExpectCommit()

	in := map[string]int{"yahoo": 32}
	out := po.applyNewRecordDailyBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 10, out["yahoo"], "legacy per-brand clamp 100-90=10")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A loader error must degrade to the global env behavior, never block sending.
func TestLaneDailyCap_LoaderErrorFallsBackToEnv(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	po, mock := laneCapPO(t, map[string]int{"yahoo": 100})

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_isp_distribution_overrides`).
		WithArgs(laneTestDataset).
		WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("db", "yahoo").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectCommit()

	in := map[string]int{"yahoo": 32}
	out := po.applyNewRecordDailyBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 32, out["yahoo"], "env cap 100 with 0 used leaves the wave cap alone")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// The one-move kill switch restores pure env behavior with no override load.
func TestLaneDailyCap_KillSwitch(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "1")
	po, mock := laneCapPO(t, map[string]int{"yahoo": 100})

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("db", "yahoo").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(90))
	mock.ExpectCommit()

	in := map[string]int{"yahoo": 32}
	out := po.applyNewRecordDailyBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 10, out["yahoo"], "env behavior intact under the kill switch")
	assert.NoError(t, mock.ExpectationsWereMet(), "override table must not be read when disabled")
}

// The brand-allow gate is the platform ceiling: a lane override cannot bypass
// it — a gated ISP on a non-allowed brand is zeroed before any budget math.
func TestLaneDailyCap_BrandAllowStillGates(t *testing.T) {
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		NewRecordDailyISPCaps:  map[string]int{},
		NewRecordISPBrandAllow: map[string]map[string]bool{"gmail": {"db": true}},
	}}

	expectLaneOverrideLoad(mock, laneTestDataset,
		sqlmock.NewRows([]string{"isp", "daily_cap"}).AddRow("gmail", 500))

	in := map[string]int{"gmail": 200}
	out := po.applyNewRecordDailyBudget(context.Background(), "yih", laneTestDataset, in)

	assert.Equal(t, 0, out["gmail"], "yih is not gmail-allowed; the lane override must not bypass brand routing")
	assert.NoError(t, mock.ExpectationsWereMet(), "no usage count for a brand-gated ISP")
}

// Follow-up touches are governed by the same lane budget, counted per dataset.
func TestLaneDailyCap_FollowupOverrideCountsPerDataset(t *testing.T) {
	t.Setenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED", "")
	t.Setenv("PARTNER_DRIP_LANE_DAILY_CAPS_DISABLED", "")
	po, mock := laneCapPO(t, map[string]int{"gmail": 0})

	expectLaneOverrideLoad(mock, laneTestDataset,
		sqlmock.NewRows([]string{"isp", "daily_cap"}).AddRow("microsoft", 300))
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(laneTestDataset, "microsoft").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(280))
	mock.ExpectCommit()

	in := map[string]int{"gmail": 200, "microsoft": 200, "yahoo": 32}
	out := po.applyFollowupDailyISPBudget(context.Background(), "db", laneTestDataset, in)

	assert.Equal(t, 0, out["gmail"], "env gmail=0 hard suppression preserved")
	assert.Equal(t, 20, out["microsoft"], "lane budget 300-280=20 clamps the follow-up wave")
	assert.Equal(t, 32, out["yahoo"], "unmanaged ISP untouched")
	assert.NoError(t, mock.ExpectationsWereMet())
}

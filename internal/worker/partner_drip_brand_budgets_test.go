package worker

// Per-(sending-domain × ISP) introduction-budget ledger tests (operator
// 2026-08-15). Semantics pinned:
//   - no ledger row for (brand, isp)  -> cap untouched (pre-ledger behavior)
//   - hold or daily_budget<=0         -> cap 0, and NO spend query
//   - otherwise                       -> cap = min(cap, daily - introduced today)
//   - spend-count failure             -> caps unchanged (fail-open)
//   - PARTNER_DRIP_BRAND_BUDGETS_DISABLED -> ledger fully bypassed
//   - weighted roster: weight N repeats the brand N times in the rotation

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func budgetPO(t *testing.T, cache map[string]map[string]brandBudgetRow) (*PartnerDripOrchestrator, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	po := &PartnerDripOrchestrator{db: db}
	po.brandBudgetCache = cache
	return po, mock
}

func expectBudgetSpend(mock sqlmock.Sqlmock, brand string, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).WithArgs(brand).WillReturnRows(rows)
	mock.ExpectCommit()
}

func TestBrandBudget_ClampsToRemaining(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 500}},
	})
	expectBudgetSpend(mock, "fc",
		sqlmock.NewRows([]string{"isp", "n"}).AddRow("yahoo", 450).AddRow("gmail", 9999))

	in := map[string]int{"yahoo": 200, "gmail": 50}
	out := po.applyBrandIntroBudgets(context.Background(), "FC", in)

	assert.Equal(t, 50, out["yahoo"], "yahoo clamps to 500-450=50")
	assert.Equal(t, 50, out["gmail"], "gmail has no ledger row — untouched even though spend exists")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBrandBudget_HoldAndZeroBudgetSuppressWithoutSpendQuery(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"tot": {"yahoo": {daily: 800, hold: true}, "microsoft": {daily: 0}},
	})
	// No spend expectation registered: neither cell may trigger a count.
	in := map[string]int{"yahoo": 200, "microsoft": 300, "aol": 40}
	out := po.applyBrandIntroBudgets(context.Background(), "tot", in)

	assert.Equal(t, 0, out["yahoo"], "hold=true zeroes the cell")
	assert.Equal(t, 0, out["microsoft"], "daily_budget=0 zeroes the cell")
	assert.Equal(t, 40, out["aol"], "unbudgeted ISP untouched")
	assert.NoError(t, mock.ExpectationsWereMet(), "hold/zero cells must not run a spend query")
}

func TestBrandBudget_NoRowsForBrandIsUntouchedNoQueries(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 500}},
	})
	in := map[string]int{"yahoo": 200}
	out := po.applyBrandIntroBudgets(context.Background(), "db", in)
	assert.Equal(t, 200, out["yahoo"], "brand absent from ledger — untouched")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBrandBudget_KillSwitchBypassesLedger(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "1")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 1, hold: true}},
	})
	in := map[string]int{"yahoo": 200}
	out := po.applyBrandIntroBudgets(context.Background(), "fc", in)
	assert.Equal(t, 200, out["yahoo"], "kill switch must bypass even a hold row")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBrandBudget_SpendFailureFailsOpen(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 500}},
	})
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).WithArgs("fc").
		WillReturnError(errors.New("statement timeout"))
	mock.ExpectRollback()

	in := map[string]int{"yahoo": 200}
	out := po.applyBrandIntroBudgets(context.Background(), "fc", in)
	assert.Equal(t, 200, out["yahoo"], "count failure keeps static caps (fail-open)")
}

func TestBrandBudget_OverspendClampsToZero(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 400}},
	})
	expectBudgetSpend(mock, "fc", sqlmock.NewRows([]string{"isp", "n"}).AddRow("yahoo", 612))
	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200})
	assert.Equal(t, 0, out["yahoo"], "spend past budget clamps to zero, never negative")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCapsAnyPositive(t *testing.T) {
	assert.True(t, capsAnyPositive(map[string]int{"a": 0, "b": 3}))
	assert.False(t, capsAnyPositive(map[string]int{"a": 0, "b": 0}))
	assert.False(t, capsAnyPositive(nil))
}

func TestLoadBrandBudgets_PopulatesCacheAndKeepsPreviousOnError(t *testing.T) {
	po, mock := budgetPO(t, nil)

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).WillReturnRows(
		sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold"}).
			AddRow("fc", "yahoo", 500, false).
			AddRow("fc", "aol", 300, true).
			AddRow("db", "microsoft", 900, false))
	mock.ExpectCommit()
	po.loadBrandBudgets(context.Background())

	po.brandBudgetMu.RLock()
	assert.Equal(t, brandBudgetRow{daily: 500}, po.brandBudgetCache["fc"]["yahoo"])
	assert.Equal(t, brandBudgetRow{daily: 300, hold: true}, po.brandBudgetCache["fc"]["aol"])
	assert.Equal(t, brandBudgetRow{daily: 900}, po.brandBudgetCache["db"]["microsoft"])
	po.brandBudgetMu.RUnlock()

	// Second load fails -> previous cache kept (fail-open overlay).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).WillReturnError(errors.New("relation does not exist"))
	mock.ExpectRollback()
	po.loadBrandBudgets(context.Background())

	po.brandBudgetMu.RLock()
	assert.Equal(t, brandBudgetRow{daily: 500}, po.brandBudgetCache["fc"]["yahoo"], "error load keeps previous cache")
	po.brandBudgetMu.RUnlock()
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadVerticalRosters_WeightRepeatsBrand(t *testing.T) {
	po, mock := budgetPO(t, nil)
	t.Cleanup(func() { setDynamicRoster(map[string][]string{}) })

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).WillReturnRows(
		sqlmock.NewRows([]string{"vertical", "brand", "weight"}).
			AddRow("heloc", "wcl", 3).
			AddRow("heloc", "fc", 1))
	mock.ExpectCommit()
	po.loadVerticalRosters(context.Background())

	r, ok := dynamicRosterFor("heloc")
	require.True(t, ok)
	assert.Equal(t, []string{"wcl", "wcl", "wcl", "fc"}, r, "weight 3 repeats wcl 3x in rotation order")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadVerticalRosters_FallsBackToUnweightedOnError(t *testing.T) {
	po, mock := budgetPO(t, nil)
	t.Cleanup(func() { setDynamicRoster(map[string][]string{}) })

	// Weighted query fails (e.g. weight column missing pre-migration)...
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).WillReturnError(errors.New(`column "weight" does not exist`))
	mock.ExpectRollback()
	// ...legacy unweighted query succeeds.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_vertical_roster`).WillReturnRows(
		sqlmock.NewRows([]string{"vertical", "brand"}).AddRow("heloc", "wcl"))
	mock.ExpectCommit()
	po.loadVerticalRosters(context.Background())

	r, ok := dynamicRosterFor("heloc")
	require.True(t, ok)
	assert.Equal(t, []string{"wcl"}, r, "fallback keeps the roster overlay alive without weights")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestAdvanceBrandRotation_UpsertsPointerOnly(t *testing.T) {
	po, mock := budgetPO(t, nil)
	mock.ExpectExec(`INSERT INTO partner_drip_state`).
		WithArgs("refi_heloc", 7).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, po.advanceBrandRotation(context.Background(), "refi_heloc", 7))
	assert.NoError(t, mock.ExpectationsWereMet())
}

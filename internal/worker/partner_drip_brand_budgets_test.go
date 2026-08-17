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
	// Warm, healthy global-hold mirror (I-5): flag read succeeded, hold off.
	// Cold-start (nil) fail-closed behavior is pinned by its own fixtures.
	held := false
	po.globalHold = &held
	return po, mock
}

// expectGlobalHoldRead registers the loadGlobalHold tx that loadBrandBudgets
// now always issues after the budget query.
func expectGlobalHoldRead(mock sqlmock.Sqlmock, value bool) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM property_ledger_flags`).WillReturnRows(
		sqlmock.NewRows([]string{"value"}).AddRow(value))
	mock.ExpectCommit()
}

func expectGlobalHoldReadError(mock sqlmock.Sqlmock, err error) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM property_ledger_flags`).WillReturnError(err)
	mock.ExpectRollback()
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
	expectGlobalHoldRead(mock, false)
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
	expectGlobalHoldRead(mock, false)
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

// ── Property Ledger I-5 permanent fixtures (plan rev4 Step 9; I-11: these are
// committed failing-fixtures, never invert-and-restore) ──────────────────────

func TestBrandIntroBudget_GlobalHoldZeroesCapsEvenWithKillSwitch(t *testing.T) {
	// The kill switch disables the OVERLAY, never the emergency stop: with
	// global_hold=TRUE, caps zero EVEN THOUGH the kill switch is set.
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "1")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 500}},
	})
	held := true
	po.globalHold = &held

	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200, "gmail": 50})
	assert.Equal(t, 0, out["yahoo"], "global hold zeroes budgeted lanes despite the kill switch")
	assert.Equal(t, 0, out["gmail"], "global hold zeroes unbudgeted lanes too — it is an all-stop")
	assert.NoError(t, mock.ExpectationsWereMet(), "held estate must not run a spend query")
}

func TestBrandIntroBudget_GlobalHoldColdStartFailClosed(t *testing.T) {
	// nil globalHold = the flag has never been readable this process (I-5):
	// treat as HELD — zero caps, no spend query.
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, map[string]map[string]brandBudgetRow{
		"fc": {"yahoo": {daily: 500}},
	})
	po.globalHold = nil

	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200, "aol": 40})
	assert.Equal(t, 0, out["yahoo"], "cold-start unreadable flag fails CLOSED")
	assert.Equal(t, 0, out["aol"], "cold-start unreadable flag zeroes every lane")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGlobalHold_ColdStartReadErrorStaysHeld(t *testing.T) {
	po, mock := budgetPO(t, nil)
	po.globalHold = nil // cold start

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).WillReturnRows(
		sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold"}))
	mock.ExpectCommit()
	expectGlobalHoldReadError(mock, errors.New(`relation "property_ledger_flags" does not exist`))
	po.loadBrandBudgets(context.Background())

	po.brandBudgetMu.RLock()
	assert.Nil(t, po.globalHold, "cold-start read error leaves the mirror nil (= held)")
	po.brandBudgetMu.RUnlock()

	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200})
	assert.Equal(t, 0, out["yahoo"], "still fail-closed after the failed tick")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGlobalHold_WarmCacheReadErrorKeepsPreviousValue(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_BUDGETS_DISABLED", "")
	po, mock := budgetPO(t, nil) // helper sets a warm globalHold=false

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).WillReturnRows(
		sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold"}))
	mock.ExpectCommit()
	expectGlobalHoldReadError(mock, errors.New("statement timeout"))
	po.loadBrandBudgets(context.Background())

	po.brandBudgetMu.RLock()
	require.NotNil(t, po.globalHold)
	assert.False(t, *po.globalHold, "warm mirror keeps previous value on read error")
	po.brandBudgetMu.RUnlock()

	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200})
	assert.Equal(t, 200, out["yahoo"], "warm hold=false + read error stays open")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGlobalHold_SuccessfulReadUpdatesMirror(t *testing.T) {
	po, mock := budgetPO(t, nil) // warm false

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_drip_brand_budgets`).WillReturnRows(
		sqlmock.NewRows([]string{"brand", "isp", "daily_budget", "hold"}))
	mock.ExpectCommit()
	expectGlobalHoldRead(mock, true)
	po.loadBrandBudgets(context.Background())

	po.brandBudgetMu.RLock()
	require.NotNil(t, po.globalHold)
	assert.True(t, *po.globalHold, "flag flip is visible after one tick")
	po.brandBudgetMu.RUnlock()

	out := po.applyBrandIntroBudgets(context.Background(), "fc", map[string]int{"yahoo": 200})
	assert.Equal(t, 0, out["yahoo"], "hold engages within one cache tick")
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

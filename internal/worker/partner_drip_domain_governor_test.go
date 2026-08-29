package worker

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func govRow() domainGovernorRow {
	return domainGovernorRow{brand: "ht", isp: "gmail", dailyCap: 31000, coldCap: 4000,
		allowed: regexp.MustCompile(`^internal_auto_insurance`), laneDailyCap: 4000, laneWindowCap: 250, windowMinutes: 15, enforce: true}
}

func TestDomainGovernorDecide(t *testing.T) {
	row := govRow()
	cases := []struct {
		name     string
		vertical string
		capIn    int
		sp       domainGovernorSpend
		want     int
		wantWhy  string
	}{
		{"unconstrained wave", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 20000, drips: 100, laneToday: 100, laneWindow: 0}, 100, "lane cap"},
		{"15-min window binds", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 20000, drips: 1000, laneToday: 1000, laneWindow: 200}, 50, "lane_window_cap"},
		{"window exhausted", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{laneWindow: 250}, 0, "lane_window_cap"},
		{"lane daily binds", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{laneToday: 3950}, 50, "lane_daily_cap"},
		{"cold cap binds across lanes", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{drips: 3980, laneToday: 1000}, 20, "cold_cap"},
		{"board ate the global cap", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 30990, drips: 0}, 10, "daily_cap"},
		{"board over cap -> zero, never negative", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 40000}, 0, "daily_cap"},
		{"non-internal vertical banned", "refi_heloc", 100, domainGovernorSpend{}, 0, "vertical not allowed"},
		{"other internal auto lane allowed", "internal_auto_insurance_v5", 100, domainGovernorSpend{}, 100, "lane cap"},
		{"cap already zero stays zero", "internal_auto_insurance_gmail_v1", 0, domainGovernorSpend{}, 0, "cap already 0"},
	}
	for _, tc := range cases {
		got, why := domainGovernorDecide(row, tc.vertical, tc.capIn, tc.sp)
		if got != tc.want {
			t.Errorf("%s: cap=%d want %d (%s)", tc.name, got, tc.want, why)
		}
		if len(why) < len(tc.wantWhy) || why[:len(tc.wantWhy)] != tc.wantWhy {
			t.Errorf("%s: reason %q, want prefix %q", tc.name, why, tc.wantWhy)
		}
	}
}

func TestDomainGovernorShadowLeavesCaps(t *testing.T) {
	// Shadow rows must never change the cap; enforce rows must.
	row := govRow()
	row.enforce = false
	capIn := 100
	got, _ := domainGovernorDecide(row, "refi_heloc", capIn, domainGovernorSpend{})
	if got != 0 {
		t.Fatalf("decide is mode-agnostic; expected 0 got %d", got)
	}
	// applyDomainGovernor is what honours shadow; verified by the mode branch
	// in code review (no DB in unit scope). Guard the kill switch here.
	t.Setenv("PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED", "true")
	if !domainGovernorDisabled() {
		t.Fatal("kill switch not honoured")
	}
}

// Every constraint that can bind must be able to bind to ZERO, and the reason
// string must name the one that did. Regression cover for the 2026-08-27 leak
// class: a cap that reads 0 but sends anyway is exactly what went unnoticed for
// two weeks, so each zero here is pinned to its own binding constraint.
func TestDomainGovernorDecide_ZeroCapsNameTheBindingConstraint(t *testing.T) {
	row := govRow()
	cases := []struct {
		name     string
		row      domainGovernorRow
		vertical string
		capIn    int
		sp       domainGovernorSpend
		wantWhy  string
	}{
		{"cap already zero", row, "internal_auto_insurance_aol_rotate", 0, domainGovernorSpend{}, "cap already 0"},
		{"negative cap in", row, "internal_auto_insurance_aol_rotate", -25, domainGovernorSpend{}, "cap already 0"},
		{"vertical not allowed", row, "refi_heloc", 500, domainGovernorSpend{}, "vertical not allowed"},
		{"cold_cap fully spent", row, "internal_auto_insurance_aol_rotate", 500, domainGovernorSpend{drips: 4000}, "cold_cap 4000 − drips today 4000"},
		{"cold_cap overspent", row, "internal_auto_insurance_aol_rotate", 500, domainGovernorSpend{drips: 4600}, "cold_cap 4000 − drips today 4600"},
		{"daily_cap fully spent by board+drips", row, "internal_auto_insurance_aol_rotate", 500,
			domainGovernorSpend{board: 29000, drips: 2000}, "daily_cap 31000 − board 29000 − drips 2000"},
		{"lane_daily_cap fully spent", row, "internal_auto_insurance_aol_rotate", 500,
			domainGovernorSpend{laneToday: 4000}, "lane_daily_cap 4000 − lane today 4000"},
		{"lane_window_cap fully spent", row, "internal_auto_insurance_aol_rotate", 500,
			domainGovernorSpend{laneWindow: 250}, "lane_window_cap 250 − last 15m 250"},
	}
	for _, tc := range cases {
		got, why := domainGovernorDecide(tc.row, tc.vertical, tc.capIn, tc.sp)
		if got != 0 {
			t.Errorf("%s: cap=%d want 0 (%s)", tc.name, got, why)
		}
		if !strings.HasPrefix(why, tc.wantWhy) {
			t.Errorf("%s: reason %q, want prefix %q", tc.name, why, tc.wantWhy)
		}
	}
}

// The governor may only LOWER a cap. Anything else would turn a ledger row into
// a volume amplifier — the opposite of what the recovery cap exists to do.
func TestDomainGovernorDecide_NeverRaisesCapIn(t *testing.T) {
	row := govRow()
	verticals := []string{"internal_auto_insurance_aol_rotate", "internal_auto_insurance_v5", "refi_heloc"}
	caps := []int{1, 10, 250, 4000, 100000}
	spends := []domainGovernorSpend{
		{},
		{board: 30000},
		{drips: 3999},
		{laneToday: 3999},
		{laneWindow: 249},
		{board: 999999, drips: 999999, laneToday: 999999, laneWindow: 999999},
		{board: -1, drips: -1, laneToday: -1, laneWindow: -1}, // nonsense input must not raise
	}
	for _, v := range verticals {
		for _, capIn := range caps {
			for _, sp := range spends {
				got, why := domainGovernorDecide(row, v, capIn, sp)
				if got > capIn {
					t.Fatalf("vertical=%s capIn=%d spend=%+v: raised cap to %d (%s)", v, capIn, sp, got, why)
				}
				if got < 0 {
					t.Fatalf("vertical=%s capIn=%d spend=%+v: negative cap %d (%s)", v, capIn, sp, got, why)
				}
			}
		}
	}
}

// governorPO builds an orchestrator whose domainGov cache is warm, with a
// sqlmock DB for the spend reads — same construction device as budgetPO.
func governorPO(t *testing.T, rows map[string]map[string]domainGovernorRow) (*PartnerDripOrchestrator, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	po := &PartnerDripOrchestrator{db: db}
	po.domainGov.rows, po.domainGov.ready = rows, true
	return po, mock
}

// expectGovernorSpend registers the four reads domainGovernorSpendToday issues
// inside one withDBTimeout tx, in order.
func expectGovernorSpend(mock sqlmock.Sqlmock, sp domainGovernorSpend, laneAll, laneFirst int) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).WillReturnRows(
		sqlmock.NewRows([]string{"n"}).AddRow(sp.board))
	mock.ExpectQuery(`FROM partner_clean_queue`).WillReturnRows(
		sqlmock.NewRows([]string{"n"}).AddRow(sp.drips))
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnRows(
		sqlmock.NewRows([]string{"n"}).AddRow(laneAll))
	mock.ExpectQuery(`FROM partner_clean_queue`).WillReturnRows(
		sqlmock.NewRows([]string{"n"}).AddRow(laneFirst))
	mock.ExpectQuery(`FROM mailing_campaigns`).WillReturnRows(
		sqlmock.NewRows([]string{"today", "window"}).AddRow(sp.laneToday, sp.laneWindow))
	mock.ExpectCommit()
}

// expectGovernorDecisionWrite registers the observability-only decision-ledger
// upsert applyDomainGovernor issues after every decision, in BOTH modes.
func expectGovernorDecisionWrite(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO partner_drip_domain_governor_decisions`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// Phase 1 is SHADOW: the ledger computes and logs, and the wave's caps come out
// byte-identical. Only mode='enforce' may lower them.
func TestApplyDomainGovernor_ShadowIsNoOpEnforceClamps(t *testing.T) {
	t.Setenv("PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED", "")
	const vertical = "internal_auto_insurance_aol_rotate"
	// laneWindow 150 of 250 ⇒ the decision is 100, well under capIn 200.
	spend := domainGovernorSpend{board: 0, drips: 0, laneToday: 0, laneWindow: 150}

	shadowRow := govRow()
	shadowRow.isp = "aol"
	shadowRow.enforce = false
	po, mock := governorPO(t, map[string]map[string]domainGovernorRow{"ht": {"aol": shadowRow}})
	expectGovernorSpend(mock, spend, 0, 0)
	expectGovernorDecisionWrite(mock)
	out := po.applyDomainGovernor(context.Background(), "HT", vertical, "aol_rotate", map[string]int{"aol": 200})
	assert.Equal(t, 200, out["aol"], "shadow mode must leave the cap untouched")
	assert.NoError(t, mock.ExpectationsWereMet(), "shadow still reads spend (it logs the decision)")

	enforceRow := shadowRow
	enforceRow.enforce = true
	po2, mock2 := governorPO(t, map[string]map[string]domainGovernorRow{"ht": {"aol": enforceRow}})
	expectGovernorSpend(mock2, spend, 0, 0)
	expectGovernorDecisionWrite(mock2)
	out2 := po2.applyDomainGovernor(context.Background(), "HT", vertical, "aol_rotate", map[string]int{"aol": 200})
	assert.Equal(t, 100, out2["aol"], "enforce mode clamps to lane_window_cap remainder")
	assert.NoError(t, mock2.ExpectationsWereMet())
}

// An ISP with no ledger row is untouched in either mode, and a spend-read
// failure fails CLOSED under enforce / OPEN under shadow.
func TestApplyDomainGovernor_UngovernedISPAndSpendFailure(t *testing.T) {
	t.Setenv("PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED", "")
	const vertical = "internal_auto_insurance_aol_rotate"

	row := govRow()
	row.isp = "aol"
	row.enforce = true
	po, mock := governorPO(t, map[string]map[string]domainGovernorRow{"ht": {"aol": row}})
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).WillReturnError(errors.New("statement timeout"))
	mock.ExpectRollback()
	expectGovernorDecisionWrite(mock)
	out := po.applyDomainGovernor(context.Background(), "ht", vertical, "aol_rotate",
		map[string]int{"aol": 200, "yahoo": 300})
	assert.Equal(t, 0, out["aol"], "enforce + spend error fails CLOSED")
	assert.Equal(t, 300, out["yahoo"], "ISP with no ledger row is never touched")
	assert.NoError(t, mock.ExpectationsWereMet())

	shadow := row
	shadow.enforce = false
	po2, mock2 := governorPO(t, map[string]map[string]domainGovernorRow{"ht": {"aol": shadow}})
	mock2.ExpectBegin()
	mock2.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock2.ExpectQuery(`FROM mailing_campaign_plan_recipients`).WillReturnError(errors.New("statement timeout"))
	mock2.ExpectRollback()
	expectGovernorDecisionWrite(mock2)
	out2 := po2.applyDomainGovernor(context.Background(), "ht", vertical, "aol_rotate", map[string]int{"aol": 200})
	assert.Equal(t, 200, out2["aol"], "shadow + spend error changes nothing")
	assert.NoError(t, mock2.ExpectationsWereMet())
}

// ── decision ledger (G5) ────────────────────────────────────────────────────
// The upsert is aggregate-in-place and runs from TWO ECS tasks at once, so its
// shape IS the correctness argument: the ON CONFLICT target must be the table's
// full primary key (anything narrower is a constraint error at runtime, anything
// wider is not a real conflict target), counters must accumulate in SQL rather
// than be read-modify-written in Go, and the cap envelope must fold with
// LEAST/GREATEST. None of that is observable from a passing INSERT, so it is
// pinned here rather than discovered in production.

func normalizeSQLList(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, ",", " , ")), " ")
}

func TestDomainGovernorDecisionUpsert_ConflictTargetIsThePrimaryKey(t *testing.T) {
	pk := regexp.MustCompile(`(?is)PRIMARY\s+KEY\s*\(([^)]*)\)`).
		FindStringSubmatch(PartnerDripDomainGovernorDecisionsDDL)
	require.Len(t, pk, 2, "DDL must declare a PRIMARY KEY")
	conflict := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(([^)]*)\)`).
		FindStringSubmatch(domainGovernorDecisionUpsertSQL)
	require.Len(t, conflict, 2, "upsert must have an ON CONFLICT target")
	assert.Equal(t, normalizeSQLList(pk[1]), normalizeSQLList(conflict[1]),
		"ON CONFLICT target must be exactly the primary key")
	assert.Equal(t, "day , brand , isp , vertical , pass", normalizeSQLList(pk[1]))
}

func TestDomainGovernorDecisionUpsert_Shape(t *testing.T) {
	q := domainGovernorDecisionUpsertSQL
	assert.Contains(t, q, "INSERT INTO partner_drip_domain_governor_decisions",
		"writes the decision ledger table")
	assert.NotContains(t, q, ";", "must stay ONE statement (aggregation is atomic per row)")
	assert.Contains(t, q, "DO UPDATE SET", "must aggregate, not error, on a repeat decision")

	// Counters accumulate in SQL. A Go-side read-modify-write would lose
	// increments the moment both ECS tasks decide the same cell.
	for _, col := range []string{"decisions", "clamped", "zeroed"} {
		re := regexp.MustCompile(`(?i)` + col + `\s*=\s*partner_drip_domain_governor_decisions\.` + col + `\s*\+\s*EXCLUDED\.` + col)
		assert.Regexp(t, re, q, "counter %q must increment with + EXCLUDED.%s", col, col)
	}
	// The cap envelope folds; it is never overwritten.
	for _, c := range []struct{ col, fn string }{
		{"cap_in_min", "LEAST"}, {"cap_in_max", "GREATEST"},
		{"cap_out_min", "LEAST"}, {"cap_out_max", "GREATEST"},
	} {
		re := regexp.MustCompile(`(?i)` + c.col + `\s*=\s*` + c.fn + `\(partner_drip_domain_governor_decisions\.` + c.col + `\s*,\s*EXCLUDED\.` + c.col + `\)`)
		assert.Regexp(t, re, q, "%s must fold with %s", c.col, c.fn)
	}
	// Most-recent fields overwrite.
	assert.Regexp(t, regexp.MustCompile(`(?i)binding_reason\s*=\s*EXCLUDED\.binding_reason`), q)
	assert.Regexp(t, regexp.MustCompile(`(?i)mode\s*=\s*EXCLUDED\.mode`), q)

	// Every inserted column must exist in the DDL — a typo here is a runtime
	// error on the first governed wave, in a path that swallows its errors.
	cols := regexp.MustCompile(`(?is)partner_drip_domain_governor_decisions\s*\(([^)]*)\)`).FindStringSubmatch(q)
	require.Len(t, cols, 2)
	for _, c := range strings.Split(cols[1], ",") {
		c = strings.TrimSpace(c)
		require.NotEmpty(t, c)
		assert.Regexp(t, regexp.MustCompile(`(?im)^\s*`+c+`\s+`), PartnerDripDomainGovernorDecisionsDDL,
			"column %q is inserted but not declared in the DDL", c)
	}
}

// The DDL must stay a single CREATE TABLE IF NOT EXISTS: runStartupMigrations'
// skip probe classifies an entry by its LEADING keyword, so a trailing
// CREATE INDEX in the same string would never land once the table exists.
func TestDomainGovernorDecisionsDDL_IsOneIdempotentCreateTable(t *testing.T) {
	ddl := strings.TrimSpace(PartnerDripDomainGovernorDecisionsDDL)
	assert.True(t, strings.HasPrefix(ddl, "CREATE TABLE IF NOT EXISTS partner_drip_domain_governor_decisions"),
		"must be idempotent and probe-classifiable as CREATE TABLE")
	assert.NotContains(t, ddl, ";")
	assert.Equal(t, 1, strings.Count(strings.ToUpper(ddl), "CREATE "), "one statement only")
}

// recordDomainGovernorDecision derives clamped/zeroed from the decision. An
// unclamped decision must record clamped=0 — otherwise every ungoverned wave
// would read as a throttle event and the phase-2 sizing would be nonsense.
func TestRecordDomainGovernorDecision_ClampedFlags(t *testing.T) {
	sp := domainGovernorSpend{board: 11, drips: 22, laneToday: 33, laneWindow: 44}
	cases := []struct {
		name                  string
		enforce               bool
		capIn, capOut         int
		wantMode              string
		wantClamped, wantZero int
	}{
		{"cap_out == cap_in records clamped=0", false, 200, 200, "shadow", 0, 0},
		{"clamped but non-zero", true, 200, 100, "enforce", 1, 0},
		{"clamped to zero counts both", true, 200, 0, "enforce", 1, 1},
		{"cap_in already 0 is neither", true, 0, 0, "enforce", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			po := &PartnerDripOrchestrator{db: db}
			row := govRow()
			row.enforce = tc.enforce

			mock.ExpectBegin()
			mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`INSERT INTO partner_drip_domain_governor_decisions`).
				WithArgs(sqlmock.AnyArg(), "ht", "gmail", "internal_auto_insurance_v5", "followup",
					tc.wantMode, tc.capIn, tc.capOut, tc.wantClamped, tc.wantZero, "lane cap",
					11, 22, 33, 44).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			po.recordDomainGovernorDecision(context.Background(), row,
				"internal_auto_insurance_v5", "followup", tc.capIn, tc.capOut, "lane cap", &sp)
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// A ledger write that fails must be a log line and nothing else — never an
// error the caller can act on, and never a change to the decision.
func TestRecordDomainGovernorDecision_WriteFailureIsSwallowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO partner_drip_domain_governor_decisions`).
		WillReturnError(errors.New("statement timeout"))
	mock.ExpectRollback()
	// No panic, no return value: the only contract is that it comes back.
	po.recordDomainGovernorDecision(context.Background(), govRow(), "v", "welcome", 100, 50, "lane cap", nil)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// The fail-closed path has no spend read, so the components go in as NULL
// rather than as zeros — a zero would be indistinguishable from "nothing sent".
func TestRecordDomainGovernorDecision_NilSpendWritesNulls(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO partner_drip_domain_governor_decisions`).
		WithArgs(sqlmock.AnyArg(), "ht", "gmail", "v", "welcome", "enforce",
			200, 0, 1, 1, "spend read failed: boom", nil, nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	po.recordDomainGovernorDecision(context.Background(), govRow(), "v", "welcome", 200, 0, "spend read failed: boom", nil)
	assert.NoError(t, mock.ExpectationsWereMet())
	// And the NULLs must not clobber the last known-good spend on conflict.
	for _, col := range []string{"board_spend", "drip_spend", "lane_today", "lane_window"} {
		assert.Regexp(t, regexp.MustCompile(`(?i)`+col+`\s*=\s*COALESCE\(EXCLUDED\.`+col+`\s*,\s*partner_drip_domain_governor_decisions\.`+col+`\)`),
			domainGovernorDecisionUpsertSQL, "%s must keep its previous value when the new one is NULL", col)
	}
}

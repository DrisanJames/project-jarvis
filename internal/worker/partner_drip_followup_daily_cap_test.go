package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFollowupDailyISPBudget_GmailZeroSuppressesFollowups is the regression
// guard for the 2026-07-15 bug: gmail=0 in PARTNER_DRIP_DAILY_ISP_CAPS blocked
// enrollment/touch-1 but let follow-up touches (t2..t5) keep shipping to
// already-enrolled gmail recipients (934 gmail follow-up deliveries observed).
// A cap of 0 must now force the follow-up per-wave cap to 0 (so
// claimFollowupRecordsByISPCaps drops gmail from its VALUES list — no touch of
// any number ships), while a non-capped ISP is left exactly as-is.
func TestFollowupDailyISPBudget_GmailZeroSuppressesFollowups(t *testing.T) {
	t.Setenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED", "")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		// The real bug config: PARTNER_DRIP_DAILY_ISP_CAPS holds gmail=0.
		NewRecordDailyISPCaps: map[string]int{"gmail": 0},
	}}

	// Follow-up wave caps: gmail carries a positive per-wave cap (e.g. from a
	// dataset override) — this is exactly how gmail follow-ups escaped before.
	in := map[string]int{"gmail": 200, "yahoo": 32, "other": 40}
	out := po.applyFollowupDailyISPBudget(context.Background(), "db", in)

	assert.Equal(t, 0, out["gmail"], "gmail (daily cap 0) must be suppressed on follow-up touches")
	assert.Equal(t, 32, out["yahoo"], "non-capped yahoo still sends (unchanged)")
	assert.Equal(t, 40, out["other"], "non-capped other still sends (unchanged)")
	// cap==0 is resolved with no DB round-trip; no yahoo/other daily budget set.
	assert.NoError(t, mock.ExpectationsWereMet(), "no DB queries expected for the cap==0 / non-capped paths")
}

// TestFollowupDailyISPBudget_PositiveCapClampsByRemaining verifies a positive
// daily cap clamps the follow-up per-wave cap by the remaining daily budget
// (cap - already-mailed-today), counted per (brand, ISP) in the Denver day.
func TestFollowupDailyISPBudget_PositiveCapClampsByRemaining(t *testing.T) {
	t.Setenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED", "")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		NewRecordDailyISPCaps: map[string]int{"gmail": 0, "yahoo": 100},
	}}

	// yahoo daily budget 100, 90 already mailed today -> remaining 10 -> clamp 32->10.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("db", "yahoo").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(90))
	mock.ExpectCommit()

	in := map[string]int{"gmail": 200, "yahoo": 32, "other": 40}
	out := po.applyFollowupDailyISPBudget(context.Background(), "db", in)

	assert.Equal(t, 0, out["gmail"], "gmail=0 still hard-suppressed")
	assert.Equal(t, 10, out["yahoo"], "yahoo clamped to remaining daily budget (100-90)")
	assert.Equal(t, 40, out["other"], "other not in daily map -> unchanged")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestFollowupDailyISPBudget_KillSwitch confirms the one-move rollback:
// PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED=1 restores the legacy behavior where
// follow-up touches bypass the daily cap entirely (gmail keeps its per-wave cap).
func TestFollowupDailyISPBudget_KillSwitch(t *testing.T) {
	t.Setenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED", "1")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		NewRecordDailyISPCaps: map[string]int{"gmail": 0, "yahoo": 100},
	}}

	in := map[string]int{"gmail": 200, "yahoo": 32, "other": 40}
	out := po.applyFollowupDailyISPBudget(context.Background(), "db", in)

	assert.Equal(t, in, out, "kill switch on -> caps returned unchanged (legacy follow-up behavior)")
	assert.NoError(t, mock.ExpectationsWereMet(), "no DB queries when disabled")
}

// TestFollowupDailyISPBudget_BrandGate verifies the brand-allow gate mirrors the
// welcome path: a gated ISP (gmail restricted to mature-4) is zeroed on the
// follow-up path for a non-allowed brand even if the daily cap were positive.
func TestFollowupDailyISPBudget_BrandGate(t *testing.T) {
	t.Setenv("PARTNER_DRIP_FOLLOWUP_DAILY_CAPS_DISABLED", "")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		NewRecordDailyISPCaps:  map[string]int{"gmail": 500},
		NewRecordISPBrandAllow: map[string]map[string]bool{"gmail": {"db": true, "ht": true, "mh": true, "qf": true}},
	}}

	// brand "yih" is NOT in the gmail allow-set -> gmail zeroed with no DB call.
	in := map[string]int{"gmail": 200, "other": 40}
	out := po.applyFollowupDailyISPBudget(context.Background(), "yih", in)
	assert.Equal(t, 0, out["gmail"], "gmail zeroed for a non-allow-listed brand")
	assert.Equal(t, 40, out["other"], "other unchanged")
	assert.NoError(t, mock.ExpectationsWereMet())
}

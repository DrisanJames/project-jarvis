package worker

// partner_drip_wp5_governor_test.go — REQ-118 WP5, the orchestrator's
// governor/veto contract at MODE=on. Three defects are pinned here:
//
//   D1  applyBrandIntroBudgets is on the mediator's BYPASS list, but its
//       early `return nil` fired before grantWaveCapacity — so the budget's
//       VALUE was bypassed while its VETO still killed a contract-funded wave.
//   D2  the domain governor's numeric ceiling was discarded on the enforced
//       branch (`eff[isp] = g`, not min(g, ceiling)); only a governor ZERO bound.
//   D3  GmailHoldGovernor and SESQuotaGovernor shipped unit-tested and wired
//       nowhere, so the reservation path had no SES daily-quota ceiling at all.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// wp5GovernorFixture wires ONE enforcing (brand, isp) governor row and queues
// the five spend reads + the decision-ledger upsert that one row costs.
func wp5GovernorFixture(t *testing.T, mock sqlmock.Sqlmock, brand, isp string, row domainGovernorRow) *PartnerDripOrchestrator {
	t.Helper()
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "1")

	// spend read: one tx, five queries.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectQuery(`FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"a", "b"}).AddRow(0, 0))
	mock.ExpectCommit()
	// decision ledger: observability only, but it is a real statement.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`partner_drip_domain_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{isp: 300, "other": 40},
	}}
	po.domainGov.ready = true
	po.domainGov.rows = map[string]map[string]domainGovernorRow{
		brand: {isp: row},
	}
	return po
}

func wp5EnforcingGovernorRow(brand, isp string, laneWindowCap int) domainGovernorRow {
	return domainGovernorRow{
		brand: brand, isp: isp,
		dailyCap: 1000000, coldCap: 1000000,
		laneDailyCap: 0, laneWindowCap: laneWindowCap, windowMinutes: 15,
		enforce: true,
	}
}

// D2 — THE REGRESSION GUARD. Governor ceiling 50, contract grant 500.
// Non-negotiable 4: "governors may only reduce, never raise". Before the fix
// this returned 500 and the comment at partner_drip_orchestrator.go:1660-1666
// asserting otherwise was false.
func TestWP5DomainGovernorStillCapsAnEnforcedGrant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := wp5GovernorFixture(t, mock, "db", "aol", wp5EnforcingGovernorRow("db", "aol", 50))
	po.db = db

	v := verticalState{vertical: "refi_heloc"}
	kept, govCeil := po.keptCapLayersWithCeiling(context.Background(), v, "db", "welcome", false)
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Equal(t, 50, kept["aol"], "the governor clamps the kept map too")
	assert.Equal(t, 50, govCeil["aol"], "the governor's own ceiling must travel as a NUMBER")

	// chainCaps["aol"]=5 is a BYPASSED layer (a daily-budget clamp). It must
	// not bind. govCeil["aol"]=50 is a governor. It must.
	eff := enforcedEffectiveCaps(
		map[string]int{"aol": 5, "other": 40},
		kept, govCeil,
		map[string]int{"aol": 500},
	)
	assert.Equal(t, 50, eff["aol"], "a governor ceiling of 50 must clamp a grant of 500")
}

// NEGATIVE CONTROL 1 — a governor must never RAISE. Ceiling 900, grant 500 → 500.
// Without this, TestWP5DomainGovernorStillCapsAnEnforcedGrant would also pass
// on a naive `eff[isp] = govCeil[isp]`.
func TestWP5DomainGovernorNegativeControlNeverRaisesAGrant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := wp5GovernorFixture(t, mock, "db", "aol", wp5EnforcingGovernorRow("db", "aol", 900))
	po.db = db

	v := verticalState{vertical: "refi_heloc"}
	kept, govCeil := po.keptCapLayersWithCeiling(context.Background(), v, "db", "welcome", false)
	require.NoError(t, mock.ExpectationsWereMet())
	assert.Equal(t, 900, govCeil["aol"])

	eff := enforcedEffectiveCaps(
		map[string]int{"aol": 5},
		kept, govCeil,
		map[string]int{"aol": 500},
	)
	assert.Equal(t, 500, eff["aol"], "a governor above the grant must be ignored, not applied")
}

// NEGATIVE CONTROL 2 — the BYPASSED layers must still be bypassed. This is the
// test that fails if the min() is applied to the kept map as a whole (or to the
// chain map) instead of to the governor ceiling: doing that re-imposes exactly
// the legacy per-wave caps REQ-118 replaces.
func TestWP5EnforcedGrantIgnoresTheBypassedChainValues(t *testing.T) {
	kept := map[string]int{"aol": 8, "gmail": 0, "other": 40} // base per-wave caps
	eff := enforcedEffectiveCaps(
		map[string]int{"aol": 5, "gmail": 150, "other": 40}, // chain, incl. a daily-budget clamp
		kept,
		nil, // no governor has an opinion
		map[string]int{"aol": 500, "gmail": 500},
	)
	assert.Equal(t, 500, eff["aol"], "neither the chain value (5) nor the base cap (8) may bind")
	assert.Equal(t, 0, eff["gmail"], "a KEPT-layer ZERO still binds absolutely")
	assert.Equal(t, 40, eff["other"], "an ISP the mediator did not grant keeps its chain value")
}

// A SHADOW governor row must clamp nothing — same as it clamps nothing today.
func TestWP5ShadowGovernorPublishesNoCeiling(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	row := wp5EnforcingGovernorRow("db", "aol", 50)
	row.enforce = false
	po := wp5GovernorFixture(t, mock, "db", "aol", row)
	po.db = db

	kept, govCeil := po.keptCapLayersWithCeiling(context.Background(),
		verticalState{vertical: "refi_heloc"}, "db", "welcome", false)
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Equal(t, 300, kept["aol"], "shadow leaves the kept map alone")
	assert.Nil(t, govCeil, "a shadow row must publish no ceiling")

	eff := enforcedEffectiveCaps(map[string]int{"aol": 5}, kept, govCeil, map[string]int{"aol": 500})
	assert.Equal(t, 500, eff["aol"])
}

// -----------------------------------------------------------------------------
// D1 — the pre-mediator veto
// -----------------------------------------------------------------------------

func wp5MediatorAtMode(db *sqlmock.Sqlmock, mode dripsupply.Mode) *dripsupply.Mediator {
	_ = db
	return dripsupply.NewMediator(nil, nil, dripsupply.MediatorConfig{
		Mode: mode, AlertsDisabled: true, OutcomesDisabled: true,
	})
}

// A wave the contract funds must NOT be vetoed by a held brand budget at
// MODE=on: applyBrandIntroBudgets is a BYPASSED layer, so its veto has no
// standing once the mediator owns the cell.
func TestWP5IntroBudgetVetoIsDeferredWhenEnforcing(t *testing.T) {
	po := &PartnerDripOrchestrator{}
	po.SetCapacityMediator(wp5MediatorAtMode(nil, dripsupply.ModeOn))
	assert.True(t, po.introVetoDeferredToMediator(),
		"MODE=on: the layer-7 veto must wait for the mediator's answer")

	po.SetCapacityMediator(wp5MediatorAtMode(nil, dripsupply.ModeCanary))
	assert.True(t, po.introVetoDeferredToMediator(),
		"MODE=canary: deferred too — the mediator decides per cell, then the veto runs if it declined")
}

// NEGATIVE CONTROL — the same wave IS vetoed at MODE=shadow (and MODE=off, and
// with a nil mediator). This is the rollback path; if it ever returns true the
// test above proves nothing.
func TestWP5IntroBudgetVetoStillFiresWhenNotEnforcing(t *testing.T) {
	po := &PartnerDripOrchestrator{}

	po.SetCapacityMediator(wp5MediatorAtMode(nil, dripsupply.ModeShadow))
	assert.False(t, po.introVetoDeferredToMediator(), "MODE=shadow must veto before the mediator, as today")

	po.SetCapacityMediator(wp5MediatorAtMode(nil, dripsupply.ModeOff))
	assert.False(t, po.introVetoDeferredToMediator(), "MODE=off must be byte-identical to pre-REQ-118")

	nilMed := &PartnerDripOrchestrator{}
	assert.False(t, nilMed.introVetoDeferredToMediator(), "a nil mediator is MODE=off")
}

// KILL SWITCH — PARTNER_DRIP_INTRO_VETO_ALWAYS=1 restores the pre-fix veto at
// every mode, with no deploy.
func TestWP5IntroBudgetVetoKillSwitchRestoresTheOldVeto(t *testing.T) {
	t.Setenv("PARTNER_DRIP_INTRO_VETO_ALWAYS", "1")
	po := &PartnerDripOrchestrator{}
	po.SetCapacityMediator(wp5MediatorAtMode(nil, dripsupply.ModeOn))
	assert.False(t, po.introVetoDeferredToMediator(),
		"kill switch on: the veto fires before the mediator even at MODE=on")
}

// KILL SWITCH — PARTNER_DRIP_GOVERNOR_CEILING_DISABLED=1 restores the pre-fix
// behaviour (only a governor ZERO binds). This is the D2 rollback, and it is
// also the negative control that proves the min() above is live code and not a
// constant: with the switch on, the identical inputs yield 500.
func TestWP5GovernorCeilingKillSwitchRestoresTheDiscard(t *testing.T) {
	t.Setenv("PARTNER_DRIP_GOVERNOR_CEILING_DISABLED", "1")
	eff := enforcedEffectiveCaps(
		map[string]int{"aol": 5},
		map[string]int{"aol": 50},
		map[string]int{"aol": 50},
		map[string]int{"aol": 500},
	)
	assert.Equal(t, 500, eff["aol"], "kill switch on: the governor ceiling is discarded again")
}

// The veto's EFFECT is unchanged wherever it runs: rotation advance + an
// outcome row carrying skip_budget_exhausted. Pins the extraction that lets the
// pre-mediator and the deferred veto share one code path.
func TestWP5SkipBudgetExhaustedAdvancesAndRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`INSERT INTO partner_drip_state`).
		WithArgs("refi_heloc", 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WithArgs(
			sqlmock.AnyArg(), // tick
			"refi_heloc",
			dripsupply.PassWelcome,
			dripsupply.OutcomeSkipped,
			"budget_exhausted brand=db",
			sqlmock.AnyArg(), // caps_seen
			0,
			nil,
			sqlmock.AnyArg(), // priority
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	po := &PartnerDripOrchestrator{db: db}
	po.SetCapacityMediator(dripsupply.NewMediator(db, nil,
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOn, AlertsDisabled: true}))

	po.skipBudgetExhausted(context.Background(),
		verticalState{vertical: "refi_heloc"},
		passContext{stateKey: "refi_heloc", pass: dripsupply.PassWelcome},
		"db", 3, map[string]int{"aol": 39})

	assert.NoError(t, mock.ExpectationsWereMet())
}

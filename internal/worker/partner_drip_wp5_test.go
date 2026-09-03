package worker

// partner_drip_wp5_test.go — REQ-118 WP5, orchestrator half.
//
// Three things are pinned here:
//
//  1. GOLDEN: with DRIP_SUPPLY_CHAIN_MODE=off the caps a wave computes are
//     byte-identical to the pre-REQ-118 chain, and the mediator issues ZERO
//     database calls. This is the rollback, and it is worth nothing unless a
//     test can fail on it.
//  2. §8.2 test 11 / REQ-116: a wave that produced nothing writes an outcome
//     row WITH a reason and ADVANCES the brand rotation. Negative control:
//     PARTNER_DRIP_ZERO_ADVANCE_DISABLED=1 restores the old silence, and the
//     assertion that the pointer moved then fails.
//  3. The kept/bypassed cap-layer split when the mediator enforces.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// wp5GoldenCaps is the cap map the pre-REQ-118 chain produces for the fixture
// below. It is written out literally, not derived, so a change to any layer
// shows up here as a diff instead of as "both sides moved together".
var wp5GoldenCaps = map[string]int{
	"yahoo": 20, // dataset override 20; drain horizon ceil(1015828/1152)=882 -> min = 20
	"aol":   39, // dataset override, not in PerISPDrainDays
	"other": 40, // untouched global
}

func wp5Fixture(t *testing.T, db sqlmock.Sqlmock) {
	t.Helper()
	const datasetID = "6cb7292a-0702-4497-b63f-e1fb5006227d"
	const vertical = "samsclub_internal"

	db.ExpectBegin()
	db.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	db.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"}).
			AddRow("yahoo", 20).
			AddRow("aol", 39))
	db.ExpectCommit()

	db.ExpectBegin()
	db.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	db.ExpectQuery(`FROM partner_datasets`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"express_dispatch"}).AddRow(false))
	db.ExpectCommit()

	db.ExpectBegin()
	db.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	db.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 1015828).
			AddRow("aol", 175139))
	db.ExpectCommit()
}

// TestWP5GoldenCapsUnchangedWithModeOff is the rollback guard. The mediator is
// wired but MODE=off, so grantWaveCapacity must hand the chain's map straight
// back and must not queue a single statement — sqlmock fails the test on any
// unexpected query, which is what makes "zero cost" a measured claim.
func TestWP5GoldenCapsUnchangedWithModeOff(t *testing.T) {
	const datasetID = "6cb7292a-0702-4497-b63f-e1fb5006227d"
	const vertical = "samsclub_internal"

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	wp5Fixture(t, mock)

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "aol": 8, "other": 40},
		PerISPDrainDays:  map[string]int{"yahoo": 3},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}
	// MODE=off, wired exactly as cmd/server/main.go wires it.
	po.SetCapacityMediator(dripsupply.NewMediator(db, dripsupply.NewService(db),
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true, OutcomesDisabled: true}))

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)
	assert.Equal(t, wp5GoldenCaps, caps, "the pre-REQ-118 chain moved")

	v := verticalState{vertical: vertical, datasetID: datasetID, readyCount: 5000}
	alloc, eff, gErr := po.grantWaveCapacity(context.Background(), v, "db",
		dripsupply.PassWelcome, dripsupply.TouchClassIntro, "", 500, caps, false)
	require.NoError(t, gErr)
	assert.Nil(t, alloc, "MODE=off must return no allocation")
	assert.Equal(t, wp5GoldenCaps, eff, "MODE=off must return the chain's caps unchanged")
	assert.Nil(t, alloc.EnforcedCaps(), "a nil allocation must read as 'the old chain decides'")

	// Nothing beyond the three fixture round-trips was issued.
	assert.NoError(t, mock.ExpectationsWereMet())
}

// A nil mediator (every test written before WP5, and any boot where the
// orchestrator is constructed without one) behaves exactly like MODE=off.
func TestWP5NilMediatorIsModeOff(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "other": 40},
	}}
	assert.Equal(t, dripsupply.ModeOff, po.mediator.Mode())

	caps := map[string]int{"yahoo": 8, "other": 40}
	alloc, eff, gErr := po.grantWaveCapacity(context.Background(), verticalState{vertical: "l"}, "db",
		dripsupply.PassWelcome, dripsupply.TouchClassIntro, "", 500, caps, false)
	require.NoError(t, gErr)
	assert.Nil(t, alloc)
	assert.Equal(t, caps, eff)

	// TickStart and Outcome on a nil mediator must also be inert.
	po.mediator.TickStart(context.Background(), time.Now())
	po.tickOutcome(context.Background(), "l", dripsupply.PassWelcome, dripsupply.OutcomeZero, "x", "db", caps, 0, "")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// §8.2 test 11 / REQ-116 — a zero wave records a reason AND advances
// -----------------------------------------------------------------------------

// zeroWave must do two things a silent `return nil` did not: write the outcome
// row with a reason, and move partner_drip_state.next_brand_index.
func TestWP5ZeroWaveRecordsReasonAndAdvancesRotation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_drip_state`).
		WithArgs("samsclub_internal", 7).
		WillReturnResult(sqlmock.NewResult(0, 1))

	po := &PartnerDripOrchestrator{db: db}
	po.SetCapacityMediator(dripsupply.NewMediator(db, nil,
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true}))

	po.zeroWave(context.Background(),
		verticalState{vertical: "samsclub_internal"},
		passContext{stateKey: "samsclub_internal", pass: dripsupply.PassWelcome},
		"db", 7,
		dripsupply.OutcomeZero, dripsupply.ZeroNoRecordsClaimed,
		map[string]int{"yahoo": 20, "aol": 39})

	// Both statements ran: the outcome row AND the rotation advance. Without
	// the advance, mock.ExpectationsWereMet() reports the unfulfilled
	// partner_drip_state expectation and this fails.
	assert.NoError(t, mock.ExpectationsWereMet())
}

// NEGATIVE CONTROL. With the kill switch on, the pointer must NOT move — and
// the un-met partner_drip_state expectation proves the assertion above can
// actually fail.
func TestWP5ZeroAdvanceKillSwitchStopsTheAdvance(t *testing.T) {
	t.Setenv("PARTNER_DRIP_ZERO_ADVANCE_DISABLED", "1")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_drip_state`).
		WithArgs("samsclub_internal", 7).
		WillReturnResult(sqlmock.NewResult(0, 1))

	po := &PartnerDripOrchestrator{db: db}
	po.SetCapacityMediator(dripsupply.NewMediator(db, nil,
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true}))

	po.zeroWave(context.Background(),
		verticalState{vertical: "samsclub_internal"},
		passContext{stateKey: "samsclub_internal", pass: dripsupply.PassWelcome},
		"db", 7, dripsupply.OutcomeZero, dripsupply.ZeroNoRecordsClaimed, nil)

	assert.Error(t, mock.ExpectationsWereMet(),
		"with the kill switch on the rotation must NOT advance — if this passes, the previous test proves nothing")
}

// The outcome row still lands when the outcomes kill switch is OFF and the
// reason is never blank: a `zero` with no reason is the silence REQ-116 was
// filed for, wearing a row.
func TestWP5OutcomeAlwaysCarriesAReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WithArgs(
			sqlmock.AnyArg(), // tick
			"samsclub_internal",
			dripsupply.PassWelcome,
			dripsupply.OutcomeZero,
			"no_records_claimed brand=db",
			sqlmock.AnyArg(), // caps_seen
			0,
			nil,
			1, // priority(zero)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	po := &PartnerDripOrchestrator{db: db}
	po.SetCapacityMediator(dripsupply.NewMediator(db, nil,
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true}))

	po.tickOutcome(context.Background(), "samsclub_internal", dripsupply.PassWelcome,
		dripsupply.OutcomeZero, dripsupply.ZeroNoRecordsClaimed, "db",
		map[string]int{"aol": 0}, 0, "")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// -----------------------------------------------------------------------------
// Kept vs bypassed cap layers
// -----------------------------------------------------------------------------

// keptCapLayers must carry the layers §2.7 keeps and none of the ones it
// bypasses. The apple ban is the cheapest one to prove without a database.
func TestWP5KeptCapLayersKeepsTheAppleBan(t *testing.T) {
	t.Setenv("PARTNER_DRIP_APPLE_BANNED_VERTICALS", "fidelity_life")
	t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", "db,ht,mh,qf")

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"apple": 50, "gmail": 150, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
	}}

	caps := po.keptCapLayers(context.Background(),
		verticalState{vertical: "fidelity_life"}, "db", "welcome", false)
	assert.Equal(t, 0, caps["apple"], "layer 12 (apple-banned verticals) must survive enforcement")

	// And the brand allow-list (layer 8): a non-allowed brand loses gmail.
	capsRB := po.keptCapLayers(context.Background(),
		verticalState{vertical: "refi_heloc"}, "rb", "welcome", false)
	assert.Equal(t, 0, capsRB["gmail"], "layer 8 (brand allow-list) must survive enforcement")
	assert.Greater(t, capsRB["other"], 0, "an ungated ISP must still be requestable")
	// Control for the apple assertion above: on a NON-banned vertical apple
	// must survive, or "apple == 0" would prove nothing about the ban.
	assert.Greater(t, capsRB["apple"], 0, "apple must only be zeroed on a banned vertical")
}

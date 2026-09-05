package worker

// partner_drip_aol_rotate_mediation_test.go — REQ-118, the AOL rotated pass.
//
// What these tests pin, in order of what an operator cares about:
//
//  1. THE DEFECT IS REAL AND STILL PRESENT. processAOLRotated reserves NOTHING
//     against drip_capacity_ledger in ANY mediator mode, MODE=on included. The
//     test asserts that as the current truth rather than pretending otherwise,
//     so the day the grant path lands it fails and has to be updated.
//  2. WHY IT CANNOT BE ROUTED FROM THIS FILE. keptCapLayers — the layer set
//     grantWaveCapacity asks for a grant on — zeroes AOL for every intro touch
//     on a rotation-active vertical. Routing this AOL-only wave through the
//     shared helper would request capacity for the wrong ISPs and write those
//     grants back into an AOL-only cap map.
//  3. THE SHIPPED CONTROL: the fence refuses before the first write, is read at
//     call time, and defaults to today's behaviour.
//  4. A tick outcome row lands in EVERY mode, off and shadow included.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// gated prefix (defaultGatedVerticalPrefixes) => aolRotationActive is true.
const aolRotateTestVertical = "internal_auto_insurance_v7"

// aolRotateTestPO builds an orchestrator whose intro-budget layer fails closed
// (globalHold is nil = "never read since boot"), so the cap chain deterministically
// zeroes out and the pass stops at the caps gate without needing a live queue.
func aolRotateTestPO(t *testing.T, mode dripsupply.Mode) (*PartnerDripOrchestrator, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"aol": 39, "other": 40},
		MaxWaveSize:      500,
	}}
	po.SetCapacityMediator(dripsupply.NewMediator(db, dripsupply.NewService(db),
		dripsupply.MediatorConfig{Mode: mode, AlertsDisabled: true}))
	return po, mock, func() { db.Close() }
}

// -----------------------------------------------------------------------------
// 1. THE DEFECT — still unmetered, in every mode
// -----------------------------------------------------------------------------

// NEGATIVE CONTROL ON THE WHOLE REQUIREMENT. At MODE=on the rotated AOL wave
// must reserve against drip_domain_contracts; it does not. The expectation on
// drip_capacity_ledger goes UNFULFILLED, and that unfulfilled expectation is
// the evidence. When the grant path is unblocked this test fails and its
// assertion flips to assert.NoError.
func TestAOLRotatedReservesNothingEvenAtModeOn_KNOWN_DEFECT(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "")
	po, mock, done := aolRotateTestPO(t, dripsupply.ModeOn)
	defer done()
	// Unordered: the outcome row must be allowed to satisfy its own
	// expectation, so the ONLY thing left unfulfilled is the reservation.
	mock.MatchExpectationsInOrder(false)

	// If the pass reserved, THIS is the statement it would issue.
	mock.ExpectQuery(`INSERT INTO drip_capacity_ledger`).
		WillReturnRows(sqlmock.NewRows([]string{"allocation_id"}))
	// It does write a tick outcome (see test 4).
	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	require.NoError(t, err)

	assert.Error(t, mock.ExpectationsWereMet(),
		"drip_capacity_ledger was written — the AOL rotated pass is now metered; "+
			"delete this test and assert the reservation instead")
}

// -----------------------------------------------------------------------------
// 2. WHY IT CANNOT BE ROUTED THROUGH grantWaveCapacity FROM THIS FILE
// -----------------------------------------------------------------------------

// grantWaveCapacity asks for a grant on keptCapLayers' positive ISPs. For an
// intro touch (phase "welcome") on a rotation-active vertical that set has AOL
// ZEROED — partner_drip_orchestrator.go:5681-5683 — and every other ISP
// positive. Calling the shared helper from the AOL-only pass would therefore
// reserve capacity for gmail/microsoft/... and then write those grants into a
// cap map that is supposed to contain nothing but AOL.
func TestKeptCapLayersZeroesAOLForIntro_BlocksSharedGrantHelper(t *testing.T) {
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"aol": 39, "gmail": 150, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
	}}

	gated := po.keptCapLayers(context.Background(),
		verticalState{vertical: aolRotateTestVertical}, "db", "welcome", false)
	assert.Equal(t, 0, gated["aol"],
		"the layer set grantWaveCapacity grants on zeroes AOL for an intro touch on a rotating lane")
	assert.Greater(t, gated["other"], 0,
		"...while non-AOL ISPs stay requestable — which is exactly what makes the shared helper unusable here")

	// CONTROL: on a lane that does NOT rotate, AOL survives. Without this the
	// assertion above could be explained by any unrelated zeroing.
	notGated := po.keptCapLayers(context.Background(),
		verticalState{vertical: "refi_heloc"}, "db", "welcome", false)
	assert.Greater(t, notGated["aol"], 0, "AOL is only zeroed because rotation is active")
}

// -----------------------------------------------------------------------------
// 3. THE FENCE
// -----------------------------------------------------------------------------

// Armed: refuse before the first write. The cap-chain reads and the claim never
// run — sqlmock errors on any unexpected statement, and the only statement this
// test allows is the outcome row.
func TestAOLRotateFenceRefusesBeforeFirstWrite(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "1")
	po, mock, done := aolRotateTestPO(t, dripsupply.ModeOff)
	defer done()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WithArgs(
			sqlmock.AnyArg(), // tick
			aolRotateTestVertical,
			dripsupply.PassAOLRotate,
			dripsupply.OutcomeSkipped,
			sqlmock.AnyArg(), // "aol_rotate_fenced brand=<rotated brand>"
			"{}",
			0,
			nil,
			sqlmock.AnyArg(), // priority
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errAOLRotateFenced), "fence must return errAOLRotateFenced, got %v", err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// NEGATIVE CONTROL for the fence: unset flag = today's behaviour. The pass runs
// the legacy chain (which fails closed here because globalHold was never read)
// and exits at the caps gate — it does NOT return the fence error, proving the
// refusal above is caused by the flag and not by the fixture.
func TestAOLRotateFenceUnsetRunsTheLegacyPath(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "")
	po, mock, done := aolRotateTestPO(t, dripsupply.ModeOff)
	defer done()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WithArgs(
			sqlmock.AnyArg(),
			aolRotateTestVertical,
			dripsupply.PassAOLRotate,
			dripsupply.OutcomeSkipped,
			sqlmock.AnyArg(), // "budget_exhausted brand=..."
			sqlmock.AnyArg(), // caps_seen: the AOL-only map, all zero
			0,
			nil,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	require.NoError(t, err)
	assert.False(t, errors.Is(err, errAOLRotateFenced))
	assert.NoError(t, mock.ExpectationsWereMet(),
		"unfenced, the pass must reach the cap chain and record budget_exhausted")
}

// The flag is read at CALL time, not cached at construction.
func TestAOLRotateFenceReadAtCallTime(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "")
	assert.False(t, aolRotateFenceEnabled(), "default must be today's behaviour")
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "1")
	assert.True(t, aolRotateFenceEnabled())
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "true")
	assert.False(t, aolRotateFenceEnabled(), "only the literal 1 arms the fence, same as the broadcast fence")
}

// -----------------------------------------------------------------------------
// 4. A tick outcome lands in EVERY mode
// -----------------------------------------------------------------------------

// The outcomes table is the always-on surface: off and shadow must record the
// pass just like on. Without this, a fenced or exhausted AOL lane is invisible.
func TestAOLRotateTickOutcomeWrittenInEveryMode(t *testing.T) {
	for _, mode := range []dripsupply.Mode{dripsupply.ModeOff, dripsupply.ModeShadow, dripsupply.ModeOn} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "")
			po, mock, done := aolRotateTestPO(t, mode)
			defer done()

			mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
				WillReturnResult(sqlmock.NewResult(0, 1))

			require.NoError(t, po.processAOLRotated(context.Background(),
				verticalState{vertical: aolRotateTestVertical, readyCount: 5000}))
			assert.NoError(t, mock.ExpectationsWereMet(),
				"mode=%s wrote no drip_tick_outcomes row", mode)
		})
	}
}

// NEGATIVE CONTROL for the outcome writes: with outcomes disabled (or no
// mediator attached) the pass is silent again, which proves the assertions
// above are observing the write and not an unconditional sqlmock pass.
func TestAOLRotateOutcomeSilentWhenOutcomesDisabled(t *testing.T) {
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "1")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db}
	po.SetCapacityMediator(dripsupply.NewMediator(db, nil,
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true, OutcomesDisabled: true}))

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).WillReturnResult(sqlmock.NewResult(0, 1))

	fErr := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	assert.True(t, errors.Is(fErr, errAOLRotateFenced))
	assert.Error(t, mock.ExpectationsWereMet(),
		"OutcomesDisabled must suppress the row — if this passes, the every-mode test proves nothing")
}

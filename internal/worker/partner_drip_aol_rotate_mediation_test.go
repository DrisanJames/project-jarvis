package worker

// partner_drip_aol_rotate_mediation_test.go — REQ-118, the AOL rotated pass.
//
// HISTORY OF THIS FILE. It was written to pin a KNOWN DEFECT: processAOLRotated
// was the last unmetered spender in the drip supply chain — it ran the legacy
// cap chain and claimed directly, reserving nothing against
// drip_domain_contracts in ANY mediator mode, MODE=on included. It could not be
// routed through the shared mediator helper because grantWaveCapacity DERIVED
// its phase from the touch class (anything not TouchClassFollowup was phase
// "welcome"), and the kept layers zero AOL for phase "welcome" on a rotating
// lane — so the shared helper would have asked for a grant on every ISP EXCEPT
// aol and written those grants back into an AOL-only cap map.
//
// THAT IS NOW FIXED, and the reasoning above is kept because it is what the
// fix had to satisfy. grantWaveCapacity takes the phase EXPLICITLY, and
// phaseAOLRotate is a third phase whose kept layers keep AOL and keep nothing
// else. What these tests pin, in order of what an operator cares about:
//
//  1. THE PHASES ROUTE AOL IN OPPOSITE DIRECTIONS, and only the phase decides:
//     phaseWelcome still zeroes AOL on a rotating lane (unchanged), while
//     phaseAOLRotate keeps it.
//  2. THE WIDENING IS STRUCTURALLY IMPOSSIBLE: the grant ISP list for
//     phaseAOLRotate is aol and ONLY aol.
//  3. A GOVERNOR STILL WINS. The phase keeps AOL; a domain governor may still
//     zero it (non-negotiable 4 — a governor may only reduce, never raise).
//  4. THE PASS IS METERED: at MODE=on with no contract it fails CLOSED and
//     claims nothing. Negative control: at MODE=off it claims exactly as before.
//  5. MODE=off IS BIT-IDENTICAL: the chain's caps come back untouched.
//  6. THE FENCE IS UNCHANGED: still default-off, still read at call time, still
//     refusing before the first write.
//  7. A tick outcome row lands in EVERY mode, off and shadow included.

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
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

// aolRotateOpenChainPO is the OPPOSITE fixture: every legacy layer opens, so
// the pass reaches the mediator instead of dying at the caps gate.
//   - globalHold = false        → applyBrandIntroBudgets no longer fails closed
//   - brand rotation pinned     → the wall-clock rotation cannot pick a brand
//     the ISP allow-list would gate
//   - domainGov not ready       → the governor abstains (its own test is below)
//   - CONTRACT_TOKEN_KEY unset  → §1.5: no contract can be verified, so MODE=on
//     must fail CLOSED. That is the metering, observed.
func aolRotateOpenChainPO(t *testing.T, mode dripsupply.Mode) (*PartnerDripOrchestrator, sqlmock.Sqlmock, func()) {
	t.Helper()
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "")
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_BRANDS", "db")
	t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", "db,ht,mh,qf")
	t.Setenv(contractmeta.KeyEnvVar, "")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	open := false
	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"aol": 39, "gmail": 150, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
		MaxWaveSize:            500,
	}}
	po.globalHold = &open
	po.SetCapacityMediator(dripsupply.NewMediator(db, dripsupply.NewService(db),
		dripsupply.MediatorConfig{Mode: mode, AlertsDisabled: true}))
	return po, mock, func() { db.Close() }
}

// -----------------------------------------------------------------------------
// 1. THE PHASES ROUTE AOL IN OPPOSITE DIRECTIONS
// -----------------------------------------------------------------------------

// The roster welcome wave surrenders AOL on a rotating lane; the rotated
// companion wave picks it up. Same orchestrator, same lane, same brand — the
// ONLY difference between the two calls is the phase, which is what makes this
// its own negative control.
func TestKeptCapLayersAOLRoutingIsDecidedByThePhase(t *testing.T) {
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_ENGAGEMENT_GATE_VERTICALS", "")

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"aol": 39, "gmail": 150, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
	}}
	v := verticalState{vertical: aolRotateTestVertical}

	// UNCHANGED: the roster intro wave still gives AOL up on a rotating lane.
	welcome := po.keptCapLayers(context.Background(), v, "db", phaseWelcome, false)
	assert.Equal(t, 0, welcome["aol"],
		"phaseWelcome must still zero AOL on a rotating lane — the companion pass owns it")
	assert.Greater(t, welcome["other"], 0, "...while every other ISP stays claimable")

	// NEW: the companion wave keeps it.
	rotate := po.keptCapLayers(context.Background(), v, "db", phaseAOLRotate, false)
	assert.Greater(t, rotate["aol"], 0,
		"phaseAOLRotate must KEEP aol — it mails exactly the AOL the roster wave gave up")

	// CONTROL on the welcome branch: on a lane that does NOT rotate, AOL
	// survives phaseWelcome too, so the zero above is caused by the rotation
	// and not by an unrelated layer.
	notGated := po.keptCapLayers(context.Background(),
		verticalState{vertical: "refi_heloc"}, "db", phaseWelcome, false)
	assert.Greater(t, notGated["aol"], 0, "AOL is only zeroed because rotation is active")
}

// -----------------------------------------------------------------------------
// 2. THE GRANT ASK IS AOL AND ONLY AOL — structurally, not by convention
// -----------------------------------------------------------------------------

// grantWaveCapacity asks the mediator for capacity on positiveISPKeys(kept).
// For phaseAOLRotate that list must be exactly ["aol"]: this is the failure
// mode the shipped fence comment named — a grant that widens an AOL-only
// companion wave into gmail/microsoft/… — and it has to be impossible, not
// merely unlikely.
func TestGrantISPListForAOLRotateIsAOLOnly(t *testing.T) {
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", "db,ht,mh,qf")

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"aol": 39, "gmail": 150, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
	}}
	v := verticalState{vertical: aolRotateTestVertical}

	ask := positiveISPKeys(po.keptCapLayers(context.Background(), v, "db", phaseAOLRotate, false))
	sort.Strings(ask)
	assert.Equal(t, []string{"aol"}, ask,
		"the AOL companion wave may reserve for aol and nothing else")

	// NEGATIVE CONTROL: the same expression under phaseWelcome asks for a wide
	// list that does NOT include aol — i.e. the map really does carry other
	// ISPs, and the single-element result above is the phase's doing.
	wide := positiveISPKeys(po.keptCapLayers(context.Background(), v, "db", phaseWelcome, false))
	assert.Greater(t, len(wide), 1, "phaseWelcome asks for a wide ISP list")
	assert.NotContains(t, wide, "aol", "…and never for aol on a rotating lane")

	// The expression tested above must be the one grantWaveCapacity evaluates.
	// Without this guard the assertions prove a property of a helper nobody
	// calls.
	src, err := os.ReadFile("partner_drip_orchestrator.go")
	require.NoError(t, err)
	body := string(src)
	i := strings.Index(body, "func (po *PartnerDripOrchestrator) grantWaveCapacity(")
	require.Greater(t, i, 0, "grantWaveCapacity not found — update this guard alongside the refactor")
	fn := body[i:]
	if j := strings.Index(fn, "\n}\n"); j > 0 {
		fn = fn[:j]
	}
	assert.Contains(t, fn, "po.keptCapLayersWithCeiling(ctx, v, brand, phase, yahooNewsletter)",
		"grantWaveCapacity must take the phase from its caller, never derive it from the touch class")
	assert.Contains(t, fn, "isps := positiveISPKeys(kept)",
		"the grant ISP list must be the kept map's positive keys")
	assert.NotContains(t, fn, `phase := "welcome"`,
		"the phase derivation is what made the AOL pass unroutable — it must not come back")
}

// -----------------------------------------------------------------------------
// 3. A GOVERNOR STILL WINS OVER THE PHASE
// -----------------------------------------------------------------------------

// aolRotateGovernorFixture wires ONE enforcing (brand, aol) governor row and
// queues the five spend reads + the decision-ledger upsert it costs.
func aolRotateGovernorFixture(t *testing.T, mock sqlmock.Sqlmock, row domainGovernorRow) *PartnerDripOrchestrator {
	t.Helper()
	t.Setenv("PARTNER_DRIP_AOL_ROTATE_DISABLED", "")
	t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", "db,ht,mh,qf")

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
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`partner_drip_domain_governor_decisions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave:       map[string]int{"aol": 300, "other": 40},
		NewRecordISPBrandAllow: DefaultNewRecordISPBrandAllow(),
	}}
	po.domainGov.ready = true
	po.domainGov.rows = map[string]map[string]domainGovernorRow{
		"db": {"aol": row},
	}
	return po
}

// REQ-118 non-negotiable 4: a governor may only REDUCE. phaseAOLRotate keeps
// AOL, but a domain governor that allows 0 must still take it to 0 — and its
// numeric ceiling must still travel so an enforced grant is clamped by it.
func TestAOLRotatePhaseDoesNotOutrankTheDomainGovernor(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// cold_cap 0 with zero spend ⇒ the governor allows 0 for this (brand, isp).
	po := aolRotateGovernorFixture(t, mock, domainGovernorRow{
		brand: "db", isp: "aol",
		dailyCap: 1000000, coldCap: 0,
		laneDailyCap: 0, laneWindowCap: 0, windowMinutes: 15,
		enforce: true,
	})
	po.db = db

	kept, govCeil := po.keptCapLayersWithCeiling(context.Background(),
		verticalState{vertical: aolRotateTestVertical}, "db", phaseAOLRotate, false)
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Equal(t, 0, kept["aol"], "a governor that allows 0 must zero AOL even under phaseAOLRotate")
	assert.Equal(t, 0, govCeil["aol"], "…and publish 0 as its ceiling")
	assert.Empty(t, positiveISPKeys(kept), "a zeroed governor leaves nothing to reserve for")

	// The ceiling must still clamp an enforced grant, not merely a zero.
	eff := enforcedEffectiveCaps(map[string]int{"aol": 39}, kept, govCeil, map[string]int{"aol": 500})
	assert.Equal(t, 0, eff["aol"], "a grant of 500 cannot outrank a governor of 0")
}

// NEGATIVE CONTROL for the governor test: the identical fixture with a POSITIVE
// governor leaves AOL claimable. Without this, "aol == 0" above could be caused
// by the fixture rather than by the governor's decision.
func TestAOLRotatePhaseKeepsAOLWhenTheGovernorAllowsIt(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := aolRotateGovernorFixture(t, mock, wp5EnforcingGovernorRow("db", "aol", 50)) // allows 50
	po.db = db

	kept, govCeil := po.keptCapLayersWithCeiling(context.Background(),
		verticalState{vertical: aolRotateTestVertical}, "db", phaseAOLRotate, false)
	require.NoError(t, mock.ExpectationsWereMet())

	assert.Equal(t, 50, kept["aol"], "the governor clamps but does not close the lane")
	assert.Equal(t, []string{"aol"}, positiveISPKeys(kept), "and the ask is still aol only")
	eff := enforcedEffectiveCaps(map[string]int{"aol": 39}, kept, govCeil, map[string]int{"aol": 500})
	assert.Equal(t, 50, eff["aol"], "the governor ceiling clamps the grant; it never raises it")
}

// -----------------------------------------------------------------------------
// 4. THE PASS IS METERED (this replaces the KNOWN_DEFECT test)
// -----------------------------------------------------------------------------

// At MODE=on the rotated AOL wave is governed by the contract system. With no
// verifiable contract (§1.5: CONTRACT_TOKEN_KEY unset) the mediator fails
// CLOSED, so the pass records a skip and claims NOTHING — sqlmock fails on any
// unexpected statement, so "no claim was issued" is measured, not asserted.
// Before the fix this same fixture ran the legacy chain and claimed anyway.
func TestAOLRotateIsMeteredAtModeOn(t *testing.T) {
	po, mock, done := aolRotateOpenChainPO(t, dripsupply.ModeOn)
	defer done()

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).
		WithArgs(
			sqlmock.AnyArg(), // tick
			aolRotateTestVertical,
			dripsupply.PassAOLRotate,
			dripsupply.OutcomeSkipped,
			dripsupply.SkipNoContractKey+" brand=db", // §1.5: the CONTRACT decided, not the legacy chain
			sqlmock.AnyArg(),                         // caps_seen
			0,
			nil,
			sqlmock.AnyArg(), // priority
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	require.NoError(t, err, "a fail-closed grant is a skip, not an error")
	assert.NoError(t, mock.ExpectationsWereMet(),
		"the pass must record the mediator's skip and issue no claim")
}

// NEGATIVE CONTROL for the metering: the SAME open chain at MODE=off reaches
// the claim. This proves the refusal above comes from the contract layer and
// not from the fixture, and it is the rollback guard — mode-off behaviour is
// the pre-REQ-118 path, byte for byte.
func TestAOLRotateModeOffStillClaims(t *testing.T) {
	po, mock, done := aolRotateOpenChainPO(t, dripsupply.ModeOff)
	defer done()
	mock.MatchExpectationsInOrder(false)

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_clean_queue`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	require.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet(),
		"MODE=off must run the legacy claim exactly as before")
}

// -----------------------------------------------------------------------------
// 5. MODE=off returns the chain's caps untouched
// -----------------------------------------------------------------------------

// The rollback guard at the helper boundary: with an unenforced allocation
// (MODE=off, and equally shadow or a non-canary cell) grantWaveCapacity must
// hand phaseAOLRotate's caller back the map it was given — NOT the AOL-only
// kept map. The kept map exists to shape the ASK and the enforced branch; it
// must never leak into the caps the legacy chain claims with.
func TestGrantWaveCapacityModeOffIsIdentityForAOLRotate(t *testing.T) {
	po, _, done := aolRotateOpenChainPO(t, dripsupply.ModeOff)
	defer done()

	chain := map[string]int{"aol": 39, "gmail": 7, "other": 3}
	alloc, eff, gErr := po.grantWaveCapacity(context.Background(),
		verticalState{vertical: aolRotateTestVertical}, "db",
		dripsupply.PassAOLRotate, phaseAOLRotate, dripsupply.TouchClassIntro, "", 500, chain, false)
	require.NoError(t, gErr)
	assert.Nil(t, alloc, "MODE=off must return no allocation")
	assert.Equal(t, map[string]int{"aol": 39, "gmail": 7, "other": 3}, eff,
		"MODE=off must return the chain's caps unchanged, key for key")
	assert.Nil(t, alloc.EnforcedCaps(), "a nil allocation reads as 'the old chain decides'")
}

// -----------------------------------------------------------------------------
// 6. THE FENCE — unchanged by the metering
// -----------------------------------------------------------------------------

// Armed: refuse before the first write. The cap-chain reads, the grant and the
// claim never run — sqlmock errors on any unexpected statement, and the only
// statement this test allows is the outcome row.
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

// The fence still outranks a fully open chain and an enforcing mediator: it is
// an operator stop switch, not a fallback for the unmetered path it replaced.
func TestAOLRotateFenceOutranksTheMediator(t *testing.T) {
	po, mock, done := aolRotateOpenChainPO(t, dripsupply.ModeOn)
	defer done()
	t.Setenv("DRIP_SUPPLY_AOL_ROTATE_FENCE", "1")

	mock.ExpectExec(`INSERT INTO drip_tick_outcomes`).WillReturnResult(sqlmock.NewResult(0, 1))

	err := po.processAOLRotated(context.Background(),
		verticalState{vertical: aolRotateTestVertical, readyCount: 5000})
	assert.True(t, errors.Is(err, errAOLRotateFenced))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// NEGATIVE CONTROL for the fence: unset flag = the pass runs. On the fail-closed
// fixture it exits at the caps gate — it does NOT return the fence error,
// proving the refusal above is caused by the flag and not by the fixture.
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
// 7. A tick outcome lands in EVERY mode
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

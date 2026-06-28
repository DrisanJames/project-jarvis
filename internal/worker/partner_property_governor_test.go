package worker

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// govCfg is the standard governed-pass config: 15m ticks, GovernedBrandsPerTick=2.
// The governed pass's per-brand cadence is ticksPerDay(96) * GovernedBrandsPerTick
// / len(subscribedGovernedRoster). Includes the PerISPCapPerWave / PerISPDrainDays
// the floor gate reads.
func govCfg() PartnerDripOrchestratorConfig {
	return PartnerDripOrchestratorConfig{
		TickInterval:          15 * time.Minute,
		BrandsPerTick:         4,
		GovernedBrandsPerTick: 2,
		PerISPCapPerWave:      map[string]int{"yahoo": 16, "aol": 30, "apple": 100000, "other": 40},
		PerISPDrainDays:       map[string]int{"yahoo": 7, "aol": 7},
	}
}

// expectDailyCount queues a governedDailyCount() expectation (inside withDBTimeout):
// Begin → SET LOCAL → the COUNT query → Commit, returning `used`.
func expectDailyCount(mock sqlmock.Sqlmock, brand, vertical, isp string, used int) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(brand, vertical, isp).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(used))
	mock.ExpectCommit()
}

func governorFor(brand string) propertyGovernor {
	return propertyGovernor{
		brand:          brand,
		perISPDaily:    500,
		windowHours:    6,
		gmailHeld:      true,
		perISPOverride: map[string]int{},
		subscribed: map[string]bool{
			"samsclub_internal": true, "direct_offer": true, "clickers_samsclub": true,
		},
		active: true,
	}
}

func mpfGovernor() propertyGovernor { return governorFor("mpf") }

// fullGovernorCache: all 9 governed brands subscribed to the 3 Sam's verticals.
// orderedSubscribedGoverned returns all 9 for a subscribed vertical →
// governedWavesPerDay = 96*2/9 = 21 → wavesInWindow = 21*6/24 = 5 →
// paceCap(500) = ceil(500/5) = 100.
func fullGovernorCache() map[string]propertyGovernor {
	m := map[string]propertyGovernor{}
	for _, b := range governedBrandsList {
		m[b] = governorFor(b)
	}
	return m
}

// (a) applyPropertyGovernor clamps each ISP to min(base, remaining, paceCap). With
// the full 9-brand roster the pace slice is 100; a large `used` makes remaining
// bind below the pace slice.
func TestApplyPropertyGovernor_ClampsToBudget(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false) // map-iteration order over ISPs is nondeterministic
	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(), governorCache: fullGovernorCache()}

	const vertical = "samsclub_internal"
	// governedWavesPerDay = 96*2/9 = 21; wavesInWindow = 21*6/24 = 5 →
	// paceCap = ceil(500/5) = 100.
	const paceCap = 100

	// yahoo: 0 used → remaining 500; final = min(100000, 500, 100) = 100 (pace binds).
	// apple: 470 used → remaining 30; final = min(100000, 30, 100) = 30 (remaining binds).
	expectDailyCount(mock, "mpf", vertical, "yahoo", 0)
	expectDailyCount(mock, "mpf", vertical, "apple", 470)

	caps := map[string]int{"yahoo": 100000, "apple": 100000}
	out := po.applyPropertyGovernor(context.Background(), "mpf", vertical, caps)

	assert.Equal(t, paceCap, out["yahoo"], "yahoo: paced slice binds when budget is full")
	assert.Equal(t, 30, out["apple"], "apple: remaining-after-used (30) binds below paceCap")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestApplyPropertyGovernor_RemainingZeroWhenExhausted: when used >= dailyCap the
// per-wave cap is 0 (claim nothing more today).
func TestApplyPropertyGovernor_RemainingZeroWhenExhausted(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.MatchExpectationsInOrder(false) // map-iteration order over ISPs is nondeterministic
	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(),
		governorCache: map[string]propertyGovernor{"mpf": mpfGovernor()}}

	const vertical = "direct_offer"
	expectDailyCount(mock, "mpf", vertical, "yahoo", 500) // exactly at cap
	expectDailyCount(mock, "mpf", vertical, "aol", 650)   // over cap

	caps := map[string]int{"yahoo": 100000, "aol": 100000}
	out := po.applyPropertyGovernor(context.Background(), "mpf", vertical, caps)
	assert.Equal(t, 0, out["yahoo"], "at cap → 0 remaining")
	assert.Equal(t, 0, out["aol"], "over cap → clamped to 0, never negative")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// (b) gmail forced to 0 when gmail_held — no daily count is even issued for it.
func TestApplyPropertyGovernor_GmailHeld(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(), governorCache: fullGovernorCache()}

	const vertical = "clickers_samsclub"
	// Only yahoo issues a count; gmail is short-circuited to 0 before any query.
	expectDailyCount(mock, "mpf", vertical, "yahoo", 0)

	caps := map[string]int{"gmail": 100000, "yahoo": 100000}
	out := po.applyPropertyGovernor(context.Background(), "mpf", vertical, caps)
	assert.Equal(t, 0, out["gmail"], "gmail held to 0 regardless of base cap")
	assert.Greater(t, out["yahoo"], 0, "non-gmail ISP still flows")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// per_isp_overrides REPLACES per_isp_daily_cap for that ISP; gmail_held still wins
// over an override.
func TestApplyPropertyGovernor_PerISPOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	cache := fullGovernorCache()
	g := cache["mpf"]
	g.perISPOverride = map[string]int{"yahoo": 1000, "gmail": 9999}
	cache["mpf"] = g
	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(), governorCache: cache}

	const vertical = "samsclub_internal"
	// roster 9 → wavesInWindow 5; yahoo override 1000 → paceCap = ceil(1000/5) = 200;
	// 0 used → final min(100000,1000,200)=200.
	expectDailyCount(mock, "mpf", vertical, "yahoo", 0)

	caps := map[string]int{"yahoo": 100000, "gmail": 100000}
	out := po.applyPropertyGovernor(context.Background(), "mpf", vertical, caps)
	assert.Equal(t, 200, out["yahoo"], "override raises the per-ISP daily cap (paced)")
	assert.Equal(t, 0, out["gmail"], "gmail_held overrides even a gmail per-isp override")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// (c) a brand NOT subscribed to a vertical yields an all-zero cap map and issues
// NO daily-count queries.
func TestApplyPropertyGovernor_NotSubscribed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(),
		governorCache: map[string]propertyGovernor{"mpf": mpfGovernor()}}

	// mpf is NOT subscribed to remodel.
	caps := map[string]int{"yahoo": 100000, "apple": 100000, "other": 40}
	out := po.applyPropertyGovernor(context.Background(), "mpf", "remodel", caps)
	for isp, v := range out {
		assert.Equal(t, 0, v, "isp %s zeroed when brand not subscribed", isp)
	}
	assert.Len(t, out, len(caps), "all base ISPs present, all zero")
	assert.NoError(t, mock.ExpectationsWereMet(), "no DB queries for an unsubscribed brand")
}

// Inactive governor / missing brand → all-zero (fail-safe), no DB queries.
func TestApplyPropertyGovernor_InactiveOrMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	g := mpfGovernor()
	g.active = false
	po := &PartnerDripOrchestrator{db: db, cfg: govCfg(),
		governorCache: map[string]propertyGovernor{"mpf": g}}

	caps := map[string]int{"yahoo": 100000}
	out := po.applyPropertyGovernor(context.Background(), "mpf", "samsclub_internal", caps)
	assert.Equal(t, 0, out["yahoo"], "inactive governor → cap 0")

	// Missing brand entirely.
	out = po.applyPropertyGovernor(context.Background(), "pmd", "samsclub_internal", caps)
	assert.Equal(t, 0, out["yahoo"], "brand absent from cache → cap 0")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// (d) perWaveCapFromPacing math: spread dailyCap across windowHours of the brand's
// governed waves; ceil division; floors and guards.
func TestPerWaveCapFromPacing(t *testing.T) {
	tests := []struct {
		name        string
		dailyCap    int
		windowHours int
		wavesPerDay int
		want        int
	}{
		// Full 9-brand roster: 96*2/9 = 21 waves/day; window 6h → 21*6/24 = 5 → ceil(500/5)=100.
		{"seed 500/6h @9-roster", 500, 6, 21, 100},
		// Wider window spreads thinner: 12h → 21*12/24 = 10 → ceil(500/10) = 50.
		{"500/12h", 500, 12, 21, 50},
		// Few waves in window → wavesInWindow floors to 1 → whole cap per wave.
		{"sparse waves floor to 1", 500, 6, 2, 500},
		{"zero daily cap", 0, 6, 21, 0},
		{"window clamps to 24 when invalid", 500, 99, 96, 6}, // clamps to 24h → 96 waves → ceil(500/96)=6
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := perWaveCapFromPacing(tc.dailyCap, tc.windowHours, tc.wavesPerDay)
			assert.Equal(t, tc.want, got)
		})
	}
}

// SAFEGUARD (a) FLOOR GATE: thin pool (drain-horizon cap < static base) → governed
// ISP cap forced 0; huge pool (drainCap >= base) → governed proceeds (cap kept).
// govCfg: yahoo/aol in PerISPDrainDays(7); BrandsPerTick=4 → wavesPerVerticalPerDay
// = 96*4 = 384; horizonWaves = 384*7 = 2688.
//   - yahoo base 16: thin pool ready=100 → drainCap=ceil(100/2688)=1 < 16 → GATED (0).
//   - aol   base 30: huge pool ready=1_200_000 → drainCap=min(ceil(1.2M/2688)=447,30)=30
//     == base (NOT < base) → ALLOWED (kept).
//   - apple: NOT in PerISPDrainDays → no drain horizon → always binds → ALLOWED.
func TestApplyGovernedFloorGate(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: govCfg()}

	const vertical = "samsclub_internal"
	const datasetID = "" // no dataset → applyDatasetISPCapOverrides skipped (datasetID=="")

	// readyCountByISP (inside withDBTimeout).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 100).   // thin → drain-horizon binds below base
			AddRow("aol", 1200000). // huge → static base binds
			AddRow("apple", 5000))
	mock.ExpectCommit()

	caps := map[string]int{"yahoo": 16, "aol": 30, "apple": 100000}
	out := po.applyGovernedFloorGate(context.Background(), vertical, datasetID, caps)

	assert.Equal(t, 0, out["yahoo"], "thin pool → drain-horizon binds → governed ISP gated to 0")
	assert.Equal(t, 30, out["aol"], "huge pool → static base binds → governed proceeds (cap kept)")
	assert.Equal(t, 100000, out["apple"], "ISP not in PerISPDrainDays → no drain horizon → allowed")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// Floor gate fail-safe: ready-count error → gate ALL drain-managed ISPs (claim
// nothing on a vertical it can't measure), non-drain ISPs untouched.
func TestApplyGovernedFloorGate_FailSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	po := &PartnerDripOrchestrator{db: db, cfg: govCfg()}

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs("samsclub_internal").
		WillReturnError(assertErr("count blew up"))
	mock.ExpectRollback()

	caps := map[string]int{"yahoo": 16, "aol": 30, "apple": 100000}
	out := po.applyGovernedFloorGate(context.Background(), "samsclub_internal", "", caps)
	assert.Equal(t, 0, out["yahoo"], "drain-managed yahoo gated on measurement failure")
	assert.Equal(t, 0, out["aol"], "drain-managed aol gated on measurement failure")
	assert.Equal(t, 100000, out["apple"], "non-drain apple untouched on failure")
}

// (b for the suite) REVERT PROOF: governed brands NEVER appear in brandRosterFor's
// output — for subscribed verticals, unsubscribed verticals, or warm-up rosters.
// (Option A coupling fully reverted; governed brands ride tickGoverned only.)
func TestBrandRosterFor_NeverIncludesGovernedBrands(t *testing.T) {
	// Even with a full governor cache, brandRosterFor (a free function, no cache
	// access) returns exactly dripBrands for a normal vertical.
	got := brandRosterFor("samsclub_internal")
	assert.Equal(t, dripBrands, got, "subscribed vertical → plain dripBrands, no governed brands")
	for _, b := range got {
		assert.False(t, governedBrands[b], "governed brand %s must not appear in brandRosterFor output", b)
	}

	got = brandRosterFor("remodel")
	assert.Equal(t, dripBrands, got, "unsubscribed vertical → plain dripBrands")

	// A dedicated warm-up roster (if any) is returned verbatim and also free of
	// governed brands (verified structurally by TestGovernedBrandsDisjoint).
	for v := range verticalBrandRoster {
		r := brandRosterFor(v)
		for _, b := range r {
			assert.False(t, governedBrands[b], "governed brand %s leaked into verticalBrandRoster[%s]", b, v)
		}
	}
}

// orderedSubscribedGoverned returns the subscribed governed brands in list order,
// excludes inactive governors, and is empty for unsubscribed verticals / empty cache.
func TestOrderedSubscribedGoverned(t *testing.T) {
	po := &PartnerDripOrchestrator{cfg: govCfg(), governorCache: map[string]propertyGovernor{
		"mpf": mpfGovernor(),
		"bcc": governorFor("bcc"),
		"pmd": func() propertyGovernor { g := governorFor("pmd"); g.active = false; return g }(), // inactive
	}}

	got := po.orderedSubscribedGoverned("samsclub_internal")
	assert.Equal(t, []string{"mpf", "bcc"}, got, "list order, active+subscribed only (pmd inactive excluded)")

	assert.Empty(t, po.orderedSubscribedGoverned("remodel"), "unsubscribed vertical → empty")

	po.governorCache = map[string]propertyGovernor{}
	assert.Empty(t, po.orderedSubscribedGoverned("samsclub_internal"), "empty cache → empty (fail-safe)")
}

// WELCOME-PARITY: the welcome entrypoint processVertical builds a passContext
// that is byte-identical to the pre-refactor behavior — roster = brandRosterFor
// (no governed brands), stateKey = the bare vertical — and a normal brand picked
// from that roster never enters the governor branch (it isn't in governedBrands).
// This proves the 16-brand welcome path is unchanged by the parameterization.
func TestWelcomePassContextParity(t *testing.T) {
	po := &PartnerDripOrchestrator{cfg: govCfg(), governorCache: fullGovernorCache()}

	const vertical = "samsclub_internal"
	// The welcome passContext processVertical delegates with.
	welcomeRoster := brandRosterFor(vertical)
	welcomeStateKey := vertical

	// 1) Roster identity: brandRosterFor is exactly dripBrands (no governed tail),
	//    so the welcome rotation length/order is the pre-P1 value.
	assert.Equal(t, dripBrands, welcomeRoster, "welcome roster = dripBrands (unchanged)")

	// 2) State-key isolation: the welcome key is the bare vertical; the governed
	//    pass uses a DISTINCT key, so the two rotations never share a pointer.
	assert.NotEqual(t, governedStateKey(vertical), welcomeStateKey,
		"governed state key must differ from the welcome (bare-vertical) key")
	assert.Equal(t, "samsclub_internal:governed", governedStateKey(vertical))

	// 3) pickNextBrand over the welcome roster picks a normal (non-governed) brand,
	//    which means processVerticalWith takes the normal cap chain — NOT the
	//    governor branch — exactly as the old processVertical did.
	v := verticalState{vertical: vertical, brandIndex: 0}
	brand, next, err := po.pickNextBrand(context.Background(), v, welcomeRoster)
	require.NoError(t, err)
	assert.False(t, governedBrands[brand], "welcome roster yields a non-governed brand → normal cap chain")
	assert.Equal(t, dripBrands[0], brand, "first welcome brand is dripBrands[0] (rotation unchanged)")
	assert.Equal(t, 1, next, "next index advances over the 16-brand roster")

	// 4) Every brand the welcome roster can yield is non-governed (full sweep).
	for i := 0; i < len(welcomeRoster); i++ {
		vv := verticalState{vertical: vertical, brandIndex: i}
		b, _, e := po.pickNextBrand(context.Background(), vv, welcomeRoster)
		require.NoError(t, e)
		assert.False(t, governedBrands[b], "welcome roster brand %q must never be governed", b)
	}
}

// dripBrands and verticalBrandRoster brands must never overlap the governed set
// (governed brands fire welcome-only via tickGoverned, and must be absent from the
// welcome rotation AND the follow-up rotation, which walks dripBrands directly).
func TestGovernedBrandsDisjointFromDripBrands(t *testing.T) {
	for _, b := range dripBrands {
		assert.False(t, governedBrands[b], "governed brand %s must not be in dripBrands", b)
	}
	for v, brands := range verticalBrandRoster {
		for _, b := range brands {
			assert.False(t, governedBrands[b], "governed brand %s must not be in verticalBrandRoster[%s]", b, v)
		}
	}
	// governedBrandsList and governedBrands set agree.
	assert.Equal(t, len(governedBrands), len(governedBrandsList))
	for _, b := range governedBrandsList {
		assert.True(t, governedBrands[b], "list entry %s missing from set", b)
	}
}

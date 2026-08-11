package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIspCapForDrainHorizon(t *testing.T) {
	const wavesPerDay = 384 // 96 ticks × 4 brands @ 15m cadence

	tests := []struct {
		name       string
		ready      int
		base       int
		drainDays  int
		want       int
	}{
		{"gmail 3d refi backlog", 15591, 200, 3, 14},
		{"gmail 3d system backlog", 51758, 200, 3, 45},
		{"yahoo 3d", 9991, 20, 3, 9},
		{"sbcglobal 3d", 4668, 60, 3, 5},
		{"aol 3d", 5827, 20, 3, 6},
		{"att 2d", 1915, 60, 2, 3},
		{"empty backlog", 0, 200, 3, 0},
		{"small backlog min 1", 100, 200, 3, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ispCapForDrainHorizon(tc.ready, tc.base, tc.drainDays, wavesPerDay)
			if got != tc.want {
				t.Fatalf("ispCapForDrainHorizon(%d, %d, %d, %d) = %d, want %d",
					tc.ready, tc.base, tc.drainDays, wavesPerDay, got, tc.want)
			}
		})
	}
}

func TestIspCapForDrainHorizonQueueRefill(t *testing.T) {
	const wavesPerDay = 384
	base := 200
	days := 3

	// Queue doubles mid-drain — cap should double while staying under base.
	low := ispCapForDrainHorizon(50000, base, days, wavesPerDay)
	high := ispCapForDrainHorizon(100000, base, days, wavesPerDay)
	if high <= low {
		t.Fatalf("refilled queue should raise cap: low=%d high=%d", low, high)
	}
	if high > base {
		t.Fatalf("cap must not exceed base ceiling: got %d base %d", high, base)
	}
}

// TestResolvePerISPCaps_DatasetOverride verifies that a per-dataset
// max_per_wave row in partner_isp_distribution_overrides REPLACES the global
// per-ISP cap for that dataset only, and that the drain-horizon clamp is then
// computed against the overridden base (so a raised cap is honored at high
// backlog). yahoo: global 8 -> override 20; aol: global 8 -> override 39.
func TestResolvePerISPCaps_DatasetOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "6cb7292a-0702-4497-b63f-e1fb5006227d"
	const vertical = "samsclub_internal"

	// 1) Dataset override fetch (inside withDBTimeout txn).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"}).
			AddRow("yahoo", 20).
			AddRow("aol", 39))
	mock.ExpectCommit()

	// 2) express_dispatch probe — NOT express, so the drain horizon stays on.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_datasets`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"express_dispatch"}).AddRow(false))
	mock.ExpectCommit()

	// 3) readyByISP recompute for drain-horizon (yahoo is in PerISPDrainDays).
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 1015828).
			AddRow("aol", 175139))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "aol": 8, "other": 40},
		PerISPDrainDays:  map[string]int{"yahoo": 3},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)

	// yahoo: override base 20; drain-horizon ceil(1015828 / (384*3)=1152)=882, min(20,882)=20.
	assert.Equal(t, 20, caps["yahoo"], "yahoo override base honored under high backlog")
	// aol: override base 39, not in PerISPDrainDays -> stays at the override.
	assert.Equal(t, 39, caps["aol"], "aol override applied verbatim")
	// other: untouched global cap.
	assert.Equal(t, 40, caps["other"], "non-overridden ISP keeps global cap")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestResolvePerISPCaps_NoDatasetOverride confirms that when a dataset has no
// override rows, the global reputation-protective caps are preserved (yahoo 8).
func TestResolvePerISPCaps_NoDatasetOverride(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "00000000-0000-0000-0000-000000000abc"
	const vertical = "refi_heloc"

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"})) // empty
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_datasets`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"express_dispatch"}).AddRow(false))
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_clean_queue`).
		WithArgs(vertical).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "count"}).
			AddRow("yahoo", 1000000))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"yahoo": 8, "other": 40},
		PerISPDrainDays:  map[string]int{"yahoo": 3},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)
	assert.Equal(t, 8, caps["yahoo"], "no override -> global protective cap preserved")
	assert.NoError(t, mock.ExpectationsWereMet())
}


// TestBrandISPSESProfiles + TestPartitionWaveBySESProfile cover the per-(brand,ISP)
// SES tenant routing that replaced the HT→Microsoft block (operator 2026-06-16:
// "route all HT Microsoft traffic through AWS SES").
func TestBrandISPSESProfiles(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES", "")
	m := dripBrandISPSESProfiles()
	assert.Equal(t, "c24a8455-e893-4895-a8ad-4556d9013003", m["ht"]["microsoft"], "default ht microsoft → HT SES tenant")
	assert.Empty(t, m["ht"]["apple"], "ht apple not pinned by default")
	assert.Nil(t, m["db"], "db has no SES pins by default")

	// Env override REPLACES the default (must re-list ht to keep it).
	t.Setenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES", "ht=microsoft=AAA,mh=microsoft=BBB")
	m = dripBrandISPSESProfiles()
	assert.Equal(t, "AAA", m["ht"]["microsoft"])
	assert.Equal(t, "BBB", m["mh"]["microsoft"])
}

func TestPartitionWaveBySESProfile(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES", "ht=microsoft=PROF_HT_MS")
	t.Setenv("PARTNER_DRIP_ROUTE_ALL_SES", "false") // test the per-pin PMTA-default behavior
	recs := []claimedRecord{
		{id: "1", ispFamily: "microsoft"},
		{id: "2", ispFamily: "apple"},
		{id: "3", ispFamily: "microsoft"},
		{id: "4", ispFamily: "yahoo"},
	}
	subs := []string{"s1", "s2", "s3", "s4"}

	// ht: microsoft peels into its own SES group; the rest stay PMTA ("").
	groups := partitionWaveBySESProfile("ht", recs, subs)
	require.Len(t, groups, 2)
	assert.Equal(t, "", groups[0].profileID, "PMTA group leads")
	assert.ElementsMatch(t, []string{"2", "4"}, idsOf(groups[0].recs))
	assert.Equal(t, []string{"s2", "s4"}, groups[0].subIDs, "subIDs stay aligned to recs")
	assert.Equal(t, "PROF_HT_MS", groups[1].profileID)
	assert.ElementsMatch(t, []string{"1", "3"}, idsOf(groups[1].recs))

	// db: no pins → a single PMTA group holding everything (pre-split behavior).
	groups = partitionWaveBySESProfile("db", recs, subs)
	require.Len(t, groups, 1)
	assert.Equal(t, "", groups[0].profileID)
	assert.Len(t, groups[0].recs, 4)
}

// TestPartitionWaveBySESProfile_RouteAllSES: with the route-all-SES default ON
// (operator 2026-06-27), every record defaults to the brand's SES profile; an
// explicit (brand,ISP) pin still wins, and there is NO PMTA group.
func TestPartitionWaveBySESProfile_RouteAllSES(t *testing.T) {
	t.Setenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES", "ht=microsoft=PROF_HT_MS")
	t.Setenv("PARTNER_DRIP_ROUTE_ALL_SES", "") // default ON
	htSES := dripBrandSESProfiles()["ht"]
	require.NotEmpty(t, htSES)
	recs := []claimedRecord{
		{id: "1", ispFamily: "microsoft"},
		{id: "2", ispFamily: "apple"},
		{id: "3", ispFamily: "yahoo"},
	}
	subs := []string{"s1", "s2", "s3"}
	groups := partitionWaveBySESProfile("ht", recs, subs)
	byProf := map[string][]string{}
	for _, g := range groups {
		byProf[g.profileID] = idsOf(g.recs)
	}
	assert.NotContains(t, byProf, "", "no PMTA group when route-all-SES is on")
	assert.ElementsMatch(t, []string{"1"}, byProf["PROF_HT_MS"], "microsoft uses the explicit pin")
	assert.ElementsMatch(t, []string{"2", "3"}, byProf[htSES], "apple+yahoo default to the brand SES relay")
}

func idsOf(recs []claimedRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.id
	}
	return out
}

// TestApplyThroughputSafety_SESBypassesThrottle verifies the critical guard for
// the HT→Microsoft SES reroute: an SES-pinned (brand,isp) bypasses the PMTA
// throttle deferral (mailing_isp_throttle_state), so a 0-msgs/hr collapsed
// Microsoft pipe does NOT defer the records we're deliberately routing to SES;
// a non-SES brand's same ISP is still deferred.
func TestApplyThroughputSafety_SESBypassesThrottle(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	t.Setenv("PARTNER_DRIP_BRAND_ISP_SES_PROFILES", "ht=microsoft=PROF")
	po := NewPartnerDripOrchestrator(db, PartnerDripOrchestratorConfig{ThrottledISPRateThreshold: 50})

	recs := []claimedRecord{{id: "1", ispFamily: "microsoft"}, {id: "2", ispFamily: "apple"}}
	caps := map[string]int{"microsoft": 100000, "apple": 100000, "other": 40}
	throttleRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"isp", "msgs_per_hour"}).AddRow("microsoft", 0.0).AddRow("apple", 14.0)
	}

	// brand=ht: microsoft is SES-routed → bypasses throttle (kept); apple is
	// throttled and NOT SES-routed → deferred.
	mock.ExpectQuery(`FROM mailing_isp_throttle_state`).WillReturnRows(throttleRows())
	keepHT, defHT, _, err := po.applyThroughputSafety(context.Background(), "ht", recs, caps)
	require.NoError(t, err)
	assert.Equal(t, []string{"1"}, idsOf(keepHT), "ht microsoft kept (SES bypass)")
	assert.Equal(t, []string{"2"}, idsOf(defHT), "apple deferred (throttled, not SES)")

	// brand=db: microsoft NOT SES-routed → throttle applies → both deferred.
	mock.ExpectQuery(`FROM mailing_isp_throttle_state`).WillReturnRows(throttleRows())
	keepDB, _, _, err := po.applyThroughputSafety(context.Background(), "db", recs, caps)
	require.NoError(t, err)
	assert.Empty(t, idsOf(keepDB), "db: microsoft+apple both throttled → none kept")
}

// TestResolvePerISPCaps_ExpressBypassesDrainHorizon pins the 2026-08-11 fix.
//
// An express_dispatch dataset mails on arrival, so the multi-day drain horizon
// must NOT apply. Reproduces the internal_auto_insurance shape that motivated
// it: AOL base cap 400, drain_days 2, a 290-record ready pool. With the horizon
// on, ceil(290/(384*2)) clamps AOL to 1/wave and the queue grows faster than it
// drains; express must yield the full base cap instead.
//
// Negative control: revert the express check in resolvePerISPCaps and this
// fails with caps["aol"] == 1.
func TestResolvePerISPCaps_ExpressBypassesDrainHorizon(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "99137b10-969c-4c9b-84a6-28042b779a07"
	const vertical = "internal_auto_insurance"

	// Dataset override fetch — none configured for this lane.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`partner_isp_distribution_overrides`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "max_per_wave"}))
	mock.ExpectCommit()

	// express_dispatch probe -> TRUE, so resolvePerISPCaps must return the base
	// caps and never run the readyByISP recompute below.
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_datasets`).
		WithArgs(datasetID).
		WillReturnRows(sqlmock.NewRows([]string{"express_dispatch"}).AddRow(true))
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{
		PerISPCapPerWave: map[string]int{"aol": 400, "microsoft": 800, "other": 400},
		PerISPDrainDays:  map[string]int{"aol": 2, "yahoo": 2, "att": 2, "sbcglobal": 2},
		TickInterval:     15 * time.Minute,
		BrandsPerTick:    4,
	}}

	caps, err := po.resolvePerISPCaps(context.Background(), vertical, datasetID, ispCapBacklogReady)
	require.NoError(t, err)

	assert.Equal(t, 400, caps["aol"], "express lane must keep the full AOL base cap, not the 2-day drain clamp")
	assert.Equal(t, 800, caps["microsoft"], "non-drained ISP unchanged")
	// Every expectation met proves the readyByISP recompute never ran.
	assert.NoError(t, mock.ExpectationsWereMet())

	// The counterfactual this fix exists to remove: with the horizon applied to
	// the same 290-record AOL pool, the cap collapses to 1/wave — which at the
	// 15m cadence is what pinned the live lane to a flat 16/hour against
	// 60-100/hour of inflow. Pinned here so the magnitude cannot regress
	// silently if the horizon is ever re-applied to express lanes.
	clamped := ispCapForDrainHorizon(290, 400, 2, 384)
	assert.Equal(t, 1, clamped, "drain horizon would clamp this pool to 1/wave")
	assert.Less(t, clamped, caps["aol"], "express bypass must be strictly larger than the clamped cap")
}

// TestDatasetIsExpress_FailsSafe: a lookup error must leave the drain horizon
// ON. Removing a reputation guard because a query lost an IO race is the wrong
// direction to fail.
func TestDatasetIsExpress_FailsSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const datasetID = "99137b10-969c-4c9b-84a6-28042b779a07"

	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`FROM partner_datasets`).
		WithArgs(datasetID).
		WillReturnError(errors.New("statement timeout"))
	mock.ExpectRollback()

	po := &PartnerDripOrchestrator{db: db, cfg: PartnerDripOrchestratorConfig{}}
	assert.False(t, po.datasetIsExpress(context.Background(), datasetID),
		"lookup failure must report NOT express so the drain horizon stays on")
}

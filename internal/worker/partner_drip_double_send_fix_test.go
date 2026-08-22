package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// =============================================================================
// DEFECT 1 — post-commit-verification 500 double-send (proven 2026-08-21).
//
// HandleDeployCampaign COMMITS the campaign row and only then runs a
// post-commit verification query; when that query times out the handler
// returns 500 with the campaign already live. deployWaveGroups used to treat
// ANY deploy error as a failure and releaseClaim the whole group, so the next
// tick re-claimed and RE-MAILED the same people (55 double-mailed).
//
// The fix: on a deploy error (other than the typed ErrDeployNameReused), probe
// mailing_campaigns by the wave group's unique name. Found in a live state →
// the deploy succeeded: stamp attribution + markMailed, NO releaseClaim.
// Absent → the release path is preserved.
// =============================================================================

// expectDeployPreamble queues the createWaveSegment writes every
// deployWaveGroup issues before DeployFn runs (segment row, member rows,
// build-ledger upsert). Uses the strict-ordered regexp matcher shared with the
// other partner-drip tests.
func expectDeployPreamble(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO mailing_segments`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_segment_members`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_segment_build_ledger`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func newDeployTestOrchestrator(t *testing.T, deploy CampaignDeployFn) (*PartnerDripOrchestrator, sqlmock.Sqlmock, func()) {
	t.Helper()
	// Force the single default-PMTA routing group so exactly ONE
	// deployWaveGroup call fires per test.
	t.Setenv("PARTNER_DRIP_ROUTE_ALL_SES", "false")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	po := &PartnerDripOrchestrator{db: db}
	po.cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"
	po.cfg.WindowHours = 16
	po.cfg.DeployFn = deploy
	return po, mock, func() { db.Close() }
}

var deployTestVertical = verticalState{
	vertical:    "consumer",
	datasetID:   "9502c7c4-68e7-4dcf-91f5-103a1480fe68",
	partnerSlug: "acme",
}

var deployTestCreative = creativeRec{
	filename: "test-creative.html",
	fromName: "Discount Blog",
	subject:  "hello",
	htmlBody: "<html>body</html>",
}

// TestDeployWaveGroups_DeployErrorButCampaignExists_MarksMailed pins the fix:
// deploy 500s, the campaign is found live by name → the group is treated as
// DEPLOYED (attribution stamped, markMailed runs) and releaseClaim is NEVER
// called. The strict ordered mock is the negative control: a releaseClaim
// UPDATE would be an unexpected statement and fail the test.
func TestDeployWaveGroups_DeployErrorButCampaignExists_MarksMailed(t *testing.T) {
	const recovered = "caa1f42d-86da-4de7-842d-e582e4234d99"
	po, mock, done := newDeployTestOrchestrator(t, func(ctx context.Context, in engine.PMTACampaignInput) (string, error) {
		return "", fmt.Errorf("HandleDeployCampaign returned 500: post-commit verification timeout")
	})
	defer done()

	expectDeployPreamble(mock)
	// The by-name probe. Must exclude terminal states — a failed/cancelled row
	// is NOT evidence the wave shipped.
	mock.ExpectQuery(`SELECT id::text\s+FROM mailing_campaigns\s+WHERE organization_id = \$1::uuid\s+AND name = \$2\s+AND status NOT IN \('failed', 'cancelled', 'deleted'\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(recovered))
	// Attribution stamp on the RECOVERED campaign id.
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET partner_drip_tag`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// markMailed advances the ladder against the recovered campaign.
	expectStampAttempt(mock).
		WithArgs("{r1,r2}", recovered, "db", MaxTouchCount, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	// NO releaseClaim — nothing further is expected.

	cid, count := po.deployWaveGroups(context.Background(), deployTestVertical, "db", deployTestCreative,
		[]claimedRecord{{id: "r1", email: "a@x.com", ispFamily: "aol"}, {id: "r2", email: "b@x.com", ispFamily: "aol"}},
		[]string{"sub-1", "sub-2"}, "")

	assert.Equal(t, recovered, cid, "the recovered campaign id must be reported as the wave's campaign")
	assert.Equal(t, 2, count, "the group counts as deployed")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestDeployWaveGroups_DeployErrorCampaignAbsent_Releases pins the preserved
// behavior: deploy fails AND no campaign exists under the name → the claim is
// released exactly as before (no attribution, no markMailed).
func TestDeployWaveGroups_DeployErrorCampaignAbsent_Releases(t *testing.T) {
	po, mock, done := newDeployTestOrchestrator(t, func(ctx context.Context, in engine.PMTACampaignInput) (string, error) {
		return "", fmt.Errorf("HandleDeployCampaign returned 500: insert failed")
	})
	defer done()

	expectDeployPreamble(mock)
	// By-name probe finds nothing.
	mock.ExpectQuery(`SELECT id::text\s+FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // zero rows -> sql.ErrNoRows
	// The release path — and ONLY the release path.
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = CASE`).
		WithArgs("{r1}").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cid, count := po.deployWaveGroups(context.Background(), deployTestVertical, "db", deployTestCreative,
		[]claimedRecord{{id: "r1", email: "a@x.com", ispFamily: "aol"}},
		[]string{"sub-1"}, "")

	assert.Empty(t, cid)
	assert.Equal(t, 0, count)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestDeployWaveGroups_NameReusedError_ReleasesWithoutByNameRecovery is the
// mandatory negative path: ErrDeployNameReused means a campaign with this name
// EXISTS — but it is another group's campaign, so the by-name recovery must
// NOT run (it would stamp this group's records onto a foreign audience, the
// 2026-08-11 349-record burn). Expected: straight to releaseClaim, with NO
// mailing_campaigns probe — the strict ordered mock fails if one is issued.
func TestDeployWaveGroups_NameReusedError_ReleasesWithoutByNameRecovery(t *testing.T) {
	po, mock, done := newDeployTestOrchestrator(t, func(ctx context.Context, in engine.PMTACampaignInput) (string, error) {
		return "", fmt.Errorf("%w (converged on campaign caa1f42d)", ErrDeployNameReused)
	})
	defer done()

	expectDeployPreamble(mock)
	// NO by-name query. Straight to release.
	mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = CASE`).
		WithArgs("{r1}").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cid, count := po.deployWaveGroups(context.Background(), deployTestVertical, "db", deployTestCreative,
		[]claimedRecord{{id: "r1", email: "a@x.com", ispFamily: "aol"}},
		[]string{"sub-1"}, "")

	assert.Empty(t, cid)
	assert.Equal(t, 0, count)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// =============================================================================
// DEFECT 2 — reconcileShippedClaims must advance the ladder (2026-08-17
// triple-fire) — and both janitor UPDATEs must be batch-bounded.
// =============================================================================

// TestReconcileShippedClaimsSQL_AdvancesLadder pins the statement itself: the
// residual reconcile flip must mirror markMailedStampSQL's ladder transition
// (touch_count+1 capped at MaxTouchCount, next_touch_at pushed a full gap or
// NULLed with terminal_reason='completed' at the ceiling) and must be bounded
// with LIMIT + FOR UPDATE SKIP LOCKED. Deleting any of these reintroduces
// either the triple-fire or the whole-backlog lock hold.
func TestReconcileShippedClaimsSQL_AdvancesLadder(t *testing.T) {
	assert.Contains(t, reconcileShippedClaimsSQL, "SET status = 'mailed'")
	assert.Regexp(t, `touch_count = LEAST\(COALESCE\(q\.touch_count, 0\) \+ 1, \$1\)`, reconcileShippedClaimsSQL,
		"the reconcile flip MUST advance touch_count or the row is instantly re-claimable (2026-08-17 triple-fire)")
	assert.Regexp(t, `next_touch_at = CASE\s+WHEN COALESCE\(q\.touch_count, 0\) \+ 1 < \$1\s+THEN NOW\(\) \+ make_interval\(hours => \$2::int\)\s+ELSE NULL\s+END`, reconcileShippedClaimsSQL)
	assert.Regexp(t, `WHEN COALESCE\(q\.touch_count, 0\) \+ 1 >= \$1 THEN 'completed'`, reconcileShippedClaimsSQL,
		"a row advanced to the ceiling must be retired, mirroring markMailedStampSQL")
	assert.Regexp(t, `LIMIT \$3\s+FOR UPDATE SKIP LOCKED`, reconcileShippedClaimsSQL,
		"the reconcile must be batch-bounded and skip rows locked by concurrent claims")
	assert.Regexp(t, `c\.status IN \('sending', 'sent', 'completed', 'completed_with_errors'\)`, reconcileShippedClaimsSQL,
		"only a campaign that provably reached the sending stage may flip a claim")
	assert.NotContains(t, reconcileShippedClaimsSQL, "last_touch_campaign_id =",
		"the reconcile must NOT invent a campaign id — the true id is what markMailed failed to record; stamp recovery owns that repair")
}

// TestReconcileShippedClaims_BatchesAndStops pins the loop mechanics: a full
// batch (claimSweepBatchSize rows) triggers another bounded statement; a
// short batch stops the loop. Each batch is its own short transaction.
func TestReconcileShippedClaims_BatchesAndStops(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()

	expectBatch := func(rows int64) {
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`UPDATE partner_clean_queue q[\s\S]*LIMIT \$3\s+FOR UPDATE SKIP LOCKED`).
			WithArgs(MaxTouchCount, followupTouchGapHours, claimSweepBatchSize).
			WillReturnResult(sqlmock.NewResult(0, rows))
		mock.ExpectCommit()
	}
	expectBatch(int64(claimSweepBatchSize)) // full batch -> keep going
	expectBatch(3)                          // short batch -> stop

	n, err := po.reconcileShippedClaims(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(claimSweepBatchSize+3), n)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestReleaseStaleClaims_BatchesAndStops pins the same bounding for the
// claimed janitor: LIMIT + FOR UPDATE SKIP LOCKED sub-select, loop until a
// short batch.
func TestReleaseStaleClaims_BatchesAndStops(t *testing.T) {
	po, mock, done := newStampMock(t)
	defer done()
	po.cfg.ClaimedJanitorMaxAge = 45 * time.Minute

	expectBatch := func(rows int64) {
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`UPDATE partner_clean_queue\s+SET status = 'ready'[\s\S]*LIMIT \$2\s+FOR UPDATE SKIP LOCKED`).
			WithArgs(sqlmock.AnyArg(), claimSweepBatchSize).
			WillReturnResult(sqlmock.NewResult(0, rows))
		mock.ExpectCommit()
	}
	expectBatch(int64(claimSweepBatchSize))
	expectBatch(0)

	n, err := po.releaseStaleClaims(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(claimSweepBatchSize), n)
	assert.NoError(t, mock.ExpectationsWereMet())

	// The stale-claim predicate itself must be unchanged: only pre-promote
	// zombies (no subscriber_id, no mailed_campaign_id) may go back to 'ready'.
	assert.Regexp(t, `subscriber_id IS NULL`, releaseStaleClaimsSQL)
	assert.Regexp(t, `mailed_campaign_id IS NULL`, releaseStaleClaimsSQL)
}

// =============================================================================
// CONTENTION 1 — throttle snapshot cached per tick.
// =============================================================================

// TestApplyThroughputSafety_UsesTickCache pins that once loadThrottledISPs has
// run (as tickOnce does), applyThroughputSafety issues ZERO queries — the mock
// has exactly one expectation, and two subsequent wave calls ride the cache.
func TestApplyThroughputSafety_UsesTickCache(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	po := NewPartnerDripOrchestrator(db, PartnerDripOrchestratorConfig{ThrottledISPRateThreshold: 50})

	// ONE read at tick start...
	mock.ExpectQuery(`FROM mailing_isp_throttle_state`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "msgs_per_hour"}).AddRow("aol", 10.0))
	po.loadThrottledISPs(context.Background())

	// ...then N waves with no further DB traffic.
	recsIn := []claimedRecord{{id: "1", ispFamily: "aol"}, {id: "2", ispFamily: "comcast"}}
	caps := map[string]int{"aol": 100, "comcast": 100, "other": 40}
	for i := 0; i < 3; i++ {
		keep, deferred, reasons, err := po.applyThroughputSafety(context.Background(), "db", recsIn, caps)
		require.NoError(t, err)
		assert.Equal(t, []string{"2"}, idsOf(keep), "comcast unthrottled -> kept")
		assert.Equal(t, []string{"1"}, idsOf(deferred), "aol throttled at 10 msgs/hr -> deferred, from the CACHE")
		assert.Contains(t, reasons["aol"], "throttled")
	}
	assert.NoError(t, mock.ExpectationsWereMet(), "applyThroughputSafety must not query the DB when the tick cache is loaded")
}

// TestApplyThroughputSafety_CachedErrorSurfaces pins semantics preservation:
// a failed tick load surfaces the SAME error to applyThroughputSafety (whose
// callers proceed without deferral), instead of silently meaning "no throttling".
func TestApplyThroughputSafety_CachedErrorSurfaces(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	po := NewPartnerDripOrchestrator(db, PartnerDripOrchestratorConfig{ThrottledISPRateThreshold: 50})

	mock.ExpectQuery(`FROM mailing_isp_throttle_state`).
		WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	po.loadThrottledISPs(context.Background())

	_, _, _, err = po.applyThroughputSafety(context.Background(), "db",
		[]claimedRecord{{id: "1", ispFamily: "aol"}}, map[string]int{"aol": 10})
	require.Error(t, err, "a failed throttle read must surface, not read as 'nothing throttled'")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestApplyThroughputSafety_FallsBackWithoutCache is the negative control for
// paths that never went through tickOnce (and pins that the pre-cache direct
// fetch still works): with no snapshot loaded the DB IS queried.
func TestApplyThroughputSafety_FallsBackWithoutCache(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	po := NewPartnerDripOrchestrator(db, PartnerDripOrchestratorConfig{ThrottledISPRateThreshold: 50})

	mock.ExpectQuery(`FROM mailing_isp_throttle_state`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "msgs_per_hour"}).AddRow("aol", 10.0))

	_, deferred, _, err := po.applyThroughputSafety(context.Background(), "db",
		[]claimedRecord{{id: "1", ispFamily: "aol"}}, map[string]int{"aol": 10})
	require.NoError(t, err)
	assert.Equal(t, []string{"1"}, idsOf(deferred))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestLoadThrottledISPs_DisabledShortCircuit pins that the
// ThrottleDeferralDisabled short-circuit survives the cache: the load issues
// no query and the cached snapshot defers nothing.
func TestLoadThrottledISPs_DisabledShortCircuit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()
	po := NewPartnerDripOrchestrator(db, PartnerDripOrchestratorConfig{ThrottleDeferralDisabled: true})

	po.loadThrottledISPs(context.Background()) // no expectations -> any query fails

	keep, deferred, _, err := po.applyThroughputSafety(context.Background(), "db",
		[]claimedRecord{{id: "1", ispFamily: "aol"}}, map[string]int{"aol": 10})
	require.NoError(t, err)
	assert.Len(t, keep, 1)
	assert.Empty(t, deferred)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// =============================================================================
// CLARITY — routeLabel is honest about the transport.
// =============================================================================

// TestRouteLabel_GovernedBrandsAreKumo pins the label fix: governed (Kumo)
// brands route through KumoMTA on their default by-domain route, so the log
// label must say "kumo", never "pmta". SES pins still win regardless of brand.
func TestRouteLabel_GovernedBrandsAreKumo(t *testing.T) {
	assert.Equal(t, "pmta", routeLabel("", "db"), "16-brand default route stays pmta")
	assert.Equal(t, "kumo", routeLabel("", "mpf"), "governed brand's default route is the Kumo box")
	assert.Equal(t, "kumo", routeLabel("", " HTM "), "case/whitespace-insensitive, like every other brand lookup")
	assert.Equal(t, "ses:prof-1", routeLabel("prof-1", "mpf"), "an SES pin outranks the by-domain route even for a governed brand")
	assert.Equal(t, "ses:prof-1", routeLabel("prof-1", "db"))
	assert.Equal(t, "pmta", routeLabel("", "notabrand"), "unknown brand defaults to pmta, never kumo")
}

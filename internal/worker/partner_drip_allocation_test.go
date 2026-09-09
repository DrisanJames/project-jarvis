package worker

// partner_drip_allocation_test.go — the two 2026-09-08 yahoo_family defects.
//
//	D1 (R1): a refreshVerticalState error shared its exit with the two
//	         legitimate "nothing left" cases, so one timed-out ready-count cut a
//	         vertical to a single wave for the whole tick and wrote NO
//	         drip_tick_outcomes row.
//	D2 (R2): the enforced claim spent ONE hardCap across every granted ISP, so
//	         the oldest ISP's backlog could eat the whole wave and the other
//	         grants were released unclaimed.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// -----------------------------------------------------------------------------
// R2 — the pure allocator
// -----------------------------------------------------------------------------

func TestAllocate(t *testing.T) {
	cases := []struct {
		name   string
		grants map[string]int
		budget int
		want   map[string]int
	}{
		{
			// Exact fit: the grants sum to the budget, so every ISP gets all of it.
			name:   "exact fit",
			grants: map[string]int{"yahoo": 2000, "aol": 2000, "gmail": 1000},
			budget: 5000,
			want:   map[string]int{"yahoo": 2000, "aol": 2000, "gmail": 1000},
		},
		{
			// Under-subscribed is the same rule: nobody competes, nobody is clamped.
			name:   "budget larger than grants",
			grants: map[string]int{"yahoo": 100, "aol": 50},
			budget: 5000,
			want:   map[string]int{"yahoo": 100, "aol": 50},
		},
		{
			// THE yahoo_family SHAPE: six ISPs each granted 5,000 against one
			// 5,000-row wave. Pre-fix the oldest ISP could take all 5,000.
			name: "budget smaller than total grants — proportional",
			grants: map[string]int{
				"yahoo": 5000, "aol": 5000, "att": 5000,
				"comcast": 5000, "sbcglobal": 5000, "cox": 5000,
			},
			budget: 5000,
			// 833 each (floor), the 2 remaining rows to the first two ISPs
			// in the deterministic (name) order the equal remainders leave.
			want: map[string]int{
				"yahoo": 833, "aol": 834, "att": 834,
				"comcast": 833, "sbcglobal": 833, "cox": 833,
			},
		},
		{
			// Uneven grants apportion by size, not evenly.
			name:   "budget smaller than total grants — uneven",
			grants: map[string]int{"yahoo": 9000, "aol": 1000},
			budget: 1000,
			want:   map[string]int{"yahoo": 900, "aol": 100},
		},
		{
			name:   "single isp takes the whole budget",
			grants: map[string]int{"yahoo": 9000},
			budget: 5000,
			want:   map[string]int{"yahoo": 5000},
		},
		{
			name:   "single isp under budget keeps its grant",
			grants: map[string]int{"yahoo": 12},
			budget: 5000,
			want:   map[string]int{"yahoo": 12},
		},
		{
			name:   "zero budget allocates nothing",
			grants: map[string]int{"yahoo": 5000, "aol": 5000},
			budget: 0,
			want:   map[string]int{},
		},
		{
			name:   "negative budget allocates nothing",
			grants: map[string]int{"yahoo": 5000},
			budget: -1,
			want:   map[string]int{},
		},
		{
			name:   "zero grants allocate nothing",
			grants: map[string]int{},
			budget: 5000,
			want:   map[string]int{},
		},
		{
			name:   "nil grants allocate nothing",
			grants: nil,
			budget: 5000,
			want:   map[string]int{},
		},
		{
			// A zero/negative grant is not an ISP the wave may claim from.
			name:   "non-positive grants are dropped",
			grants: map[string]int{"yahoo": 0, "gmail": -5, "aol": 400},
			budget: 5000,
			want:   map[string]int{"aol": 400},
		},
		{
			// Budget smaller than the number of ISPs: largest-remainder, no
			// ISP over its grant, total exactly the budget.
			name:   "budget smaller than isp count",
			grants: map[string]int{"a": 10, "b": 10, "c": 10},
			budget: 2,
			want:   map[string]int{"a": 1, "b": 1, "c": 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Purity: the input map must come back untouched.
			var before map[string]int
			if tc.grants != nil {
				before = make(map[string]int, len(tc.grants))
				for k, v := range tc.grants {
					before[k] = v
				}
			}

			got := allocate(tc.grants, tc.budget)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, before, tc.grants, "allocate mutated its input")

			total := 0
			for isp, n := range got {
				assert.LessOrEqual(t, n, tc.grants[isp], "isp %s exceeded its grant", isp)
				assert.GreaterOrEqual(t, n, 0, "isp %s went negative", isp)
				total += n
			}
			ceiling := tc.budget
			if ceiling < 0 {
				ceiling = 0
			}
			assert.LessOrEqual(t, total, ceiling, "total allocated exceeded the budget")

			// Determinism.
			assert.Equal(t, got, allocate(tc.grants, tc.budget))
		})
	}
}

// TestAllocateNeverOverspends sweeps the invariants across a range of budgets
// rather than trusting the hand-written table alone.
func TestAllocateNeverOverspends(t *testing.T) {
	grants := map[string]int{"yahoo": 5000, "aol": 3000, "att": 1500, "cox": 7, "other": 250}
	sum := 0
	for _, g := range grants {
		sum += g
	}
	for budget := 0; budget <= sum+100; budget += 37 {
		got := allocate(grants, budget)
		total := 0
		for isp, n := range got {
			require.LessOrEqualf(t, n, grants[isp], "budget=%d isp=%s over grant", budget, isp)
			total += n
		}
		require.LessOrEqualf(t, total, budget, "budget=%d overspent", budget)
		if budget < sum {
			require.Equalf(t, budget, total, "budget=%d must be spent in full while over-subscribed", budget)
		} else {
			require.Equalf(t, sum, total, "budget=%d must hand out every grant", budget)
		}
	}
}

func TestUnclaimedGrants(t *testing.T) {
	assert.Equal(t, "", unclaimedGrants(map[string]int{"yahoo": 10}, map[string]int{"yahoo": 10}))
	assert.Equal(t, "", unclaimedGrants(nil, nil))
	assert.Equal(t, "grant_unclaimed=aol:5,yahoo:10",
		unclaimedGrants(map[string]int{"yahoo": 10, "aol": 5, "att": 3}, map[string]int{"att": 3}))
	// A claim that over-delivers on one ISP does not net off another's shortfall.
	assert.Equal(t, "grant_unclaimed=aol:5",
		unclaimedGrants(map[string]int{"yahoo": 10, "aol": 5}, map[string]int{"yahoo": 99}))
}

// -----------------------------------------------------------------------------
// R2 — the claim is issued PER GRANTED ISP
// -----------------------------------------------------------------------------

// TestClaimWaveByCapsClaimsPerGrantedISP pins the D2 fix at the seam that
// matters: three granted ISPs produce THREE claims, each carrying its own
// budget, instead of one claim whose single LIMIT the oldest backlog can drain.
func TestClaimWaveByCapsClaimsPerGrantedISP(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	caps := map[string]int{"yahoo": 3000, "aol": 3000, "att": 3000}
	capsCopy := map[string]int{"yahoo": 3000, "aol": 3000, "att": 3000}
	alloc := &dripsupply.Allocation{Enforced: true, Caps: caps, ID: uuid.New()}

	cols := []string{"id", "email", "email_md5", "isp_family", "dataset_id", "partner_id", "batch_id", "extra_metadata"}
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Claims are issued in sorted ISP order: aol, att, yahoo.
	for _, isp := range []string{"aol", "att", "yahoo"} {
		mock.ExpectQuery(`UPDATE partner_clean_queue`).
			WillReturnRows(sqlmock.NewRows(cols).
				AddRow("id-"+isp, isp+"@x.com", "md5", isp, "", "", "", nil))
	}
	mock.ExpectCommit()

	po := &PartnerDripOrchestrator{db: db, transitions: dripsupply.NewTransitions()}
	claimed, err := po.claimWaveByCaps(context.Background(), "yahoo_family", "db", caps, 900, alloc)
	require.NoError(t, err)

	require.Len(t, claimed, 3, "one ISP's backlog consumed the others' budgets")
	got := tallyISPs(claimed)
	assert.Equal(t, map[string]int{"aol": 1, "att": 1, "yahoo": 1}, got)
	assert.Equal(t, capsCopy, caps, "claimWaveByCaps mutated the caller's caps map")
	assert.NoError(t, mock.ExpectationsWereMet(), "expected one claim per granted ISP")
}

// A wave whose every grant is zero still short-circuits to ErrNoPositiveGrant,
// which the caller turns into a `zero` outcome + rotation advance.
func TestClaimWaveByCapsNoPositiveGrant(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	caps := map[string]int{"yahoo": 0, "aol": 0}
	alloc := &dripsupply.Allocation{Enforced: true, Caps: caps, ID: uuid.New()}
	po := &PartnerDripOrchestrator{db: db, transitions: dripsupply.NewTransitions()}

	_, err = po.claimWaveByCaps(context.Background(), "yahoo_family", "db", caps, 5000, alloc)
	assert.ErrorIs(t, err, dripsupply.ErrNoPositiveGrant)
}

// -----------------------------------------------------------------------------
// R1 — a refresh failure is audited and does not truncate the tick
// -----------------------------------------------------------------------------

// outcomeCapture is a sqlmock argument matcher that records every value passed
// to the drip_tick_outcomes upsert. It matches everything; the assertions are
// made on what it captured, so the test does not depend on which of the tick's
// many other statements happen to be stubbed.
type outcomeCapture struct{ seen *[]string }

func (c outcomeCapture) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		*c.seen = append(*c.seen, s)
	}
	return true
}

// TestTickRefreshVerticalStateFailureIsAuditedAndDoesNotTruncate drives
// tickOnce with two verticals whose ready-count refresh times out (the D1
// symptom: `COUNT(*) ... status='ready'` over a 200k+ row queue). Pre-fix that
// error broke the brand loop in silence. Post-fix each failure writes a
// drip_tick_outcomes row naming refresh_vertical_state_failed, and the second
// vertical is still processed.
func TestTickRefreshVerticalStateFailureIsAuditedAndDoesNotTruncate(t *testing.T) {
	t.Setenv("PARTNER_DRIP_TICK_LOCK_DISABLED", "1")
	t.Setenv("PARTNER_DRIP_STAMP_RECOVERY", "off")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	// The tick's preamble (7 cache loads + reconcile + heartbeat) all runs
	// before the vertical loop and is fail-safe by design. Give it transaction
	// scaffolding to consume; its queries match nothing below, so each load
	// errors and logs exactly as it does in production under a bad read.
	for i := 0; i < 40; i++ {
		mock.ExpectBegin()
		mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectRollback()
		mock.ExpectCommit()
	}

	// activeVerticalsWithBacklog phase 1 — two verticals with backlog.
	mock.ExpectQuery(`GROUP BY vertical`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "next_brand_index", "ready_total", "oldest_at"}).
			AddRow("wp5_lane_a", 0, 250000, nil).
			AddRow("wp5_lane_b", 0, 250000, nil))
	// phase 2 — dominant-dataset metadata, one per vertical.
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(`LEFT JOIN partner_datasets`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "slug", "pslug", "pname", "flush", "offer"}).
				AddRow(nil, nil, nil, nil, nil, ""))
	}

	// THE DEFECT: the per-vertical ready-count refresh times out, for both.
	for _, lane := range []string{"wp5_lane_a", "wp5_lane_b"} {
		mock.ExpectQuery(`s\.vertical = \$1`).WithArgs(lane).
			WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	}

	// Every drip_tick_outcomes write is captured rather than asserted in place.
	var seen []string
	for i := 0; i < 20; i++ {
		mock.ExpectExec(`drip_tick_outcomes`).
			WithArgs(outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen},
				outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen},
				outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen}, outcomeCapture{seen: &seen}).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	po := &PartnerDripOrchestrator{
		db:  db,
		ctx: context.Background(),
		cfg: PartnerDripOrchestratorConfig{
			// waveSize 0 makes each wave decline immediately, so the tick reaches
			// the refresh — the statement under test — without a full wave fixture.
			MinWaveSize:      0,
			MaxWaveSize:      0,
			BrandsPerTick:    3,
			FollowupDisabled: true,
			GovernedDisabled: true,
			DeployFn: func(context.Context, engine.PMTACampaignInput) (string, error) {
				return "", nil
			},
		},
	}
	po.SetCapacityMediator(dripsupply.NewMediator(db, dripsupply.NewService(db),
		dripsupply.MediatorConfig{Mode: dripsupply.ModeOff, AlertsDisabled: true}))

	po.tickOnce()

	joined := fmt.Sprint(seen)
	assert.Contains(t, joined, reasonRefreshVerticalState,
		"a refresh failure must leave a drip_tick_outcomes row, not silence")
	// Both verticals are represented: the first one's failure did not end the tick.
	assert.Contains(t, seen, "wp5_lane_a")
	assert.Contains(t, seen, "wp5_lane_b")

	// And the row is a `failed`, not a skip that reads as healthy.
	assert.Contains(t, seen, dripsupply.OutcomeFailed)
}

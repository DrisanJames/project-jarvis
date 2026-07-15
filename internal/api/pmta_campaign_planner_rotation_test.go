package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"sort"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// =============================================================================
// Rotating cold-ramp audience selection (operator directive 2026-07-15)
// =============================================================================
// The master-selection cold-fallback (streamSDSColdFallback) selects
// first-touch subscribers — those with NO mailing_subscriber_domain_state
// row for this sending domain. Its prior ORDER BY hashed only
// (subscriber, sending_domain), which is STABLE across days, so the same
// members sorted to the front of the pool every day and the same quota-sized
// prefix was drawn each send (FIFO-equivalent). Fresh members deeper in the
// pool were never tried.
//
// The fix salts the stripe with the Denver calendar day so the daily draw
// ROTATES, while staying stable/reproducible WITHIN a day, and preserving the
// disjoint-brand property (still keyed on the sending domain). It is gated by
// rotatingAudienceSelectionEnabled(); DISABLE_ROTATING_AUDIENCE_SELECTION=true
// is the one-move rollback.
//
// NOTE ON TEST DEPTH: the deterministic suite runs on sqlmock, which regex-
// matches the emitted SQL but does NOT execute ORDER BY against rows. So the
// "different recipient set on two real Denver days" behavior is a property of
// the Postgres `now() AT TIME ZONE 'America/Denver'` + `hashtext` sort that
// only a live DB exhibits. These tests therefore prove the contract in the
// two ways the harness allows:
//   1. STRUCTURAL (against the real code) — the emitted SQL carries the
//      Denver-day rotation salt by default and reverts to the prior stable
//      ordering under the kill switch. (Default-on shape is also pinned by
//      pmta_campaign_planner_coldfallback_test.go.)
//   2. MODELED (the ordering KEY) — a Go model of the exact key composition
//      (id || domain || day) demonstrates the rotate-across-days /
//      stable-within-day property the composition guarantees, over a pool
//      larger than the quota.

// TestPlanPMTAAudience_ColdFallback_KillSwitchRevertsToStableOrdering proves
// the one-move rollback: with DISABLE_ROTATING_AUDIENCE_SELECTION=true the
// cold-fallback emits the PRIOR day-stable ordering
// (hashtext(sub.id::text || $1) ASC) with NO Denver-day salt. The positive
// regex `\$1\) ASC` requires a closing paren immediately after $1, which the
// salted form (`$1 || to_char(...)`) cannot satisfy — so a regression that
// left the salt in place would fail this expectation.
func TestPlanPMTAAudience_ColdFallback_KillSwitchRevertsToStableOrdering(t *testing.T) {
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "true")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "cccccccc-0000-0000-0000-000000000010"
	sendingDomain := "em.rollback.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Cold-fallback must emit the prior day-stable ordering, salt removed.
	mock.ExpectQuery(`ORDER BY hashtext\(sub\.id::text \|\| \$1\) ASC,\s+sub\.engagement_score DESC NULLS LAST`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("aa-0000-0000-0000-000000000001", "a@gmail.com").
			AddRow("aa-0000-0000-0000-000000000002", "b@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 2},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 2}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["gmail"] != 2 {
		t.Errorf("gmail count = %d, want 2", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (kill switch reverts to stable ordering): %v", err)
	}
}

// TestColdFallbackRotation_LeavesListPathDeterministic guards the operator's
// explicit constraint: the rotation is scoped to the master-selection
// cold-fallback (SendingDomain set). The list-path fallback that historical
// tests depend on — buildSDSEligibilityClause with SendingDomain unset —
// must stay the deterministic `ORDER BY s.id`, regardless of the rotation
// switch (which only governs the cold-fallback SQL).
func TestColdFallbackRotation_LeavesListPathDeterministic(t *testing.T) {
	if cl := buildSDSEligibilityClause("", "s", 1); cl.OrderBy != "ORDER BY s.id" {
		t.Fatalf("list-path (SendingDomain unset) OrderBy = %q, want %q", cl.OrderBy, "ORDER BY s.id")
	}

	// Rotation explicitly ON must not leak into the list-path fallback.
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "false")
	if cl := buildSDSEligibilityClause("", "s", 1); cl.OrderBy != "ORDER BY s.id" {
		t.Fatalf("list-path OrderBy with rotation on = %q, want %q", cl.OrderBy, "ORDER BY s.id")
	}

	// Rotation explicitly OFF likewise leaves the list-path fallback intact.
	t.Setenv("DISABLE_ROTATING_AUDIENCE_SELECTION", "true")
	if cl := buildSDSEligibilityClause("", "s", 1); cl.OrderBy != "ORDER BY s.id" {
		t.Fatalf("list-path OrderBy with rotation off = %q, want %q", cl.OrderBy, "ORDER BY s.id")
	}
}

// TestColdFallbackRotation_DaySaltRotatesAcrossDenverDays MODELS the cold-
// fallback ordering key — same input composition as the emitted SQL,
// hashtext(sub.id::text || $domain || <Denver-day>) — to prove the property
// the composition guarantees: from a pool LARGER than the quota, two
// different Denver days select a DIFFERENT prefix (rotation), while the SAME
// day selects an IDENTICAL prefix (within-day reproducibility). See the
// NOTE ON TEST DEPTH above for why this is modeled rather than executed.
func TestColdFallbackRotation_DaySaltRotatesAcrossDenverDays(t *testing.T) {
	// Ordering key: mirrors the SQL's hash INPUT composition (id||domain||day).
	// sha256 stands in for Postgres hashtext() — a strong block hash that
	// avalanches fully when ANY part of the input changes (unlike a byte-at-
	// a-time rolling hash, which barely perturbs on a differing suffix and
	// would misrepresent hashtext). The property under test — a different day
	// yields an independent ordering — is what hashtext guarantees.
	key := func(id, domain, day string) uint64 {
		sum := sha256.Sum256([]byte(id + domain + day))
		return binary.BigEndian.Uint64(sum[:8])
	}

	const poolSize = 200
	const quota = 40 // pool > quota: rotation is only meaningful when it can leave members behind
	domain := "em.brand-a.com"

	pool := make([]string, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		pool = append(pool, fmt.Sprintf("11111111-0000-0000-0000-%012d", i))
	}

	selectPrefix := func(day string) []string {
		ids := append([]string(nil), pool...)
		sort.SliceStable(ids, func(a, b int) bool {
			ka, kb := key(ids[a], domain, day), key(ids[b], domain, day)
			if ka != kb {
				return ka < kb // ORDER BY hashtext(...) ASC
			}
			return ids[a] < ids[b] // deterministic tie-break stand-in
		})
		return ids[:quota]
	}

	dayA := selectPrefix("2026-07-15")
	dayASame := selectPrefix("2026-07-15")
	dayB := selectPrefix("2026-07-16")

	// Within-day reproducibility: identical inputs on the same Denver day
	// must select the identical set (retries/re-finalizes are idempotent).
	if !sameSet(dayA, dayASame) {
		t.Fatalf("within-day selection changed across runs; expected identical set")
	}

	// Cross-day rotation: a different Denver day must move the selected
	// prefix off the same members (the whole point of the fix).
	overlap := overlapCount(dayA, dayB)
	if overlap >= quota {
		t.Fatalf("selection did not rotate across Denver days: overlap=%d of quota=%d (FIFO not fixed)", overlap, quota)
	}
	// Sanity: with independent day salts over a pool 5x the quota, overlap
	// should be small; assert it is a strict minority so a near-stable
	// ordering (a weak salt) also fails loudly.
	if overlap > quota/2 {
		t.Fatalf("rotation too weak: overlap=%d of quota=%d (expected a minority)", overlap, quota)
	}

	// Same-domain disjoint-brand property still holds: a different domain
	// on the same day yields a different stripe (regression guard on the
	// 2026-04-20 cross-brand overlap fix — the domain stays in the key).
	otherDomain := "em.brand-b.com"
	dayAOther := func() []string {
		ids := append([]string(nil), pool...)
		sort.SliceStable(ids, func(a, b int) bool {
			ka, kb := key(ids[a], otherDomain, "2026-07-15"), key(ids[b], otherDomain, "2026-07-15")
			if ka != kb {
				return ka < kb
			}
			return ids[a] < ids[b]
		})
		return ids[:quota]
	}()
	if overlapCount(dayA, dayAOther) >= quota {
		t.Fatalf("two brands on the same day selected the identical set; disjoint-brand stripe lost")
	}
}

// =============================================================================
// Capped inclusion-segment rotation (streamSegment) — the CONDUIT ramp case
// =============================================================================
// The family CONDUIT ramp is a CAPPED inclusion-segment send: each cell draws
// a finite volume (e.g. 900) from a CONDUIT-<ISP>-<BRAND> pool (~5,400
// members), use_master_selection=true. Those members are read through
// streamSegment (mailing_segment_members), NOT the SDS cold-fallback — once a
// member is mailed it has an SDS row, so it is excluded from the cold-fallback
// (NOT EXISTS sds) pool. streamSegment previously had NO ORDER BY, so
// allQuotasMet() cut the SAME heap-order front-N every day (FIFO). These tests
// pin the fix: a FULLY BOUNDED send (all ISP quotas finite) rotates the segment
// draw by the Denver day; an UNBOUNDED (volume:0) send is left untouched.

// TestPlanPMTAAudience_CappedSegment_RotatesDaily proves that a capped
// (all-finite-quota) inclusion-segment send emits the day-salted rotating
// ORDER BY + bounding LIMIT on the mailing_segment_members read. The prelude
// fills the quota so no SDS query follows.
func TestPlanPMTAAudience_CappedSegment_RotatesDaily(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "dddddddd-0000-0000-0000-000000000010"
	segmentID := "eeeeeeee-0000-0000-0000-0000000000c0"
	sendingDomain := "em.conduit.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// The capped segment read MUST carry the Denver-day-salted rotating
	// ORDER BY and a bounding LIMIT. WithArgs stays a single bind ($1 =
	// segmentID): the salt is subscriber_id + the SQL-side Denver day, so no
	// extra parameter is introduced.
	mock.ExpectQuery(`FROM mailing_segment_members WHERE segment_id = \$1 ORDER BY hashtext\(subscriber_id::text \|\| to_char\(now\(\) AT TIME ZONE 'America/Denver', 'YYYY-MM-DD'\)\) ASC LIMIT \d+`).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("aaaaaaaa-0000-0000-0000-000000000001", "c1@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		SendPriority: []engine.PriorityItem{
			{Type: "segment", ID: segmentID},
		},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 1},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 1}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["gmail"] != 1 {
		t.Errorf("gmail count = %d, want 1", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (capped segment rotates): %v", err)
	}
}

// TestPlanPMTAAudience_UnlimitedSegment_NoOrderBy proves the operator's
// guardrail: an UNBOUNDED send (any volume:0 ISP) leaves the segment read
// EXACTLY as before — no ORDER BY, no LIMIT. The end-anchored regex
// (`segment_id = \$1$`) matches ONLY the bare read; if the code wrongly added
// a sort to the unbounded path (the work_mem / statement_timeout risk) the
// expectation would not match and the test fails.
func TestPlanPMTAAudience_UnlimitedSegment_NoOrderBy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "dddddddd-0000-0000-0000-000000000011"
	segmentID := "eeeeeeee-0000-0000-0000-0000000000c1"
	sendingDomain := "em.uncapped.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	// Non-master path keeps the flow simple (inclusion segment → streamSegment,
	// no SDS passes) so the assertion isolates the segment read shape. The
	// volume:0 quota is what drives the no-ORDER-BY branch, independent of the
	// selection path.
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))

	mock.ExpectQuery(`FROM mailing_segment_members WHERE segment_id = \$1$`).
		WithArgs(segmentID).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "email"}).
			AddRow("bbbbbbbb-0000-0000-0000-000000000001", "u1@gmail.com").
			AddRow("bbbbbbbb-0000-0000-0000-000000000002", "u2@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:        campaignID,
		SendingDomain:     sendingDomain,
		InclusionSegments: []string{segmentID},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 0}, // volume:0 = unlimited → no rotation, no sort
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 0}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["gmail"] != 2 {
		t.Errorf("gmail count = %d, want 2 (unlimited streams all segment members)", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (unlimited segment unchanged): %v", err)
	}
}

// TestCappedSegmentRotation_DaySaltRotatesAcrossDenverDays MODELS the
// streamSegment ordering key — subscriber_id || Denver-day — to prove, from a
// pool LARGER than the quota, two Denver days select a DIFFERENT prefix
// (rotation) while the SAME day selects an IDENTICAL prefix (within-day
// reproducibility). See the NOTE ON TEST DEPTH at the top of this file for why
// the row-level rotation is modeled rather than executed under sqlmock.
func TestCappedSegmentRotation_DaySaltRotatesAcrossDenverDays(t *testing.T) {
	// Mirrors the SQL hash INPUT: hashtext(subscriber_id::text || <Denver-day>).
	// sha256 stands in for hashtext (strong avalanche on any input change).
	key := func(id, day string) uint64 {
		sum := sha256.Sum256([]byte(id + day))
		return binary.BigEndian.Uint64(sum[:8])
	}

	const poolSize = 5400 // ~CONDUIT-<ISP>-<BRAND> pool size
	const quota = 900     // volume:900 per cell

	pool := make([]string, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		pool = append(pool, fmt.Sprintf("cccccccc-0000-0000-0000-%012d", i))
	}
	selectPrefix := func(day string) []string {
		ids := append([]string(nil), pool...)
		sort.SliceStable(ids, func(a, b int) bool {
			ka, kb := key(ids[a], day), key(ids[b], day)
			if ka != kb {
				return ka < kb
			}
			return ids[a] < ids[b]
		})
		return ids[:quota]
	}

	dayA := selectPrefix("2026-07-15")
	dayASame := selectPrefix("2026-07-15")
	dayB := selectPrefix("2026-07-16")

	if !sameSet(dayA, dayASame) {
		t.Fatalf("within-day segment selection changed across runs; expected identical set")
	}
	overlap := overlapCount(dayA, dayB)
	if overlap >= quota {
		t.Fatalf("CONDUIT segment did not rotate across Denver days: overlap=%d of quota=%d (front-N FIFO not fixed)", overlap, quota)
	}
	// Independent day salts over a pool 6x the quota → expected overlap ~150;
	// require a strict minority so a near-stable ordering also fails loudly.
	if overlap > quota/2 {
		t.Fatalf("segment rotation too weak: overlap=%d of quota=%d (expected a minority)", overlap, quota)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	return overlapCount(a, b) == len(a)
}

func overlapCount(a, b []string) int {
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	n := 0
	for _, y := range b {
		if _, ok := set[y]; ok {
			n++
		}
	}
	return n
}

package api

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// =============================================================================
// Per-ISP + per-brand hash-stripe cold fallback (2026-04-20)
// =============================================================================
// These tests pin the behavior of streamSDSColdFallback after the refactor
// from a single global ORDER BY engagement_score query to one targeted
// query per ISP that has a remaining shortfall. The refactor fixes the
// cross-brand audience overlap observed when four welcome campaigns launched
// simultaneously and all deterministically picked the same top-engagement
// subscribers from the cold pool.
//
// Invariants the tests enforce:
//   1. One per-ISP query per ISP with a remaining shortfall. ISPs that are
//      fully satisfied after the primary SDS pass do NOT fire a fallback.
//   2. Per-ISP queries pass sub.isp = $N as an argument so the planner's
//      SQL cannot silently drift from the Go isp.GroupFromDomain classifier.
//   3. The ORDER BY clause uses hashtext(sub.id::text || $1) — the same
//      subscriber picks differently for two different sending domains.
//   4. A catch-all pass (sub.isp IS NULL OR sub.isp = '') runs only when
//      the per-ISP passes leave residual shortfall — ensures correctness
//      for subscribers the ISP backfill worker has not yet classified.
//   5. Cold-fallback emits ORDER BY by the sending_domain parameter so each
//      brand gets a disjoint stripe of the same ISP pool.

// TestPlanPMTAAudience_MasterSelection_PerISPStripeUsesHashtext verifies the
// generated SQL includes the hashtext(sub.id::text || $1) ordering. This is
// the core of the per-brand disjoint selection; without it, two brands
// launching at the same minute grab the same top-engagement subscribers.
func TestPlanPMTAAudience_MasterSelection_PerISPStripeUsesHashtext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000010"
	sendingDomain := "em.brand-a.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Assert the ORDER BY contains the hashtext expression keyed on $1,
	// which is the sending domain argument. Matching fails if the planner
	// reverts to the old engagement_score ordering or drops the $1 key.
	mock.ExpectQuery(`ORDER BY hashtext\(sub\.id::text \|\| \$1\) ASC`).
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
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_MasterSelection_MultipleISPsFireDisjointQueries
// verifies that a campaign with multiple ISP plans fires one cold-fallback
// query PER ISP, each scoped by sub.isp, so quotas for different ISPs are
// satisfied independently. Before the refactor, a single global query
// ordered by engagement_score could never guarantee per-ISP coverage when
// the pool's ISP mix was skewed (e.g. mostly gmail but the campaign needed
// yahoo quota too).
func TestPlanPMTAAudience_MasterSelection_MultipleISPsFireDisjointQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000011"
	sendingDomain := "em.multi-isp.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Cold-fallback fires in shortfall-desc order. yahoo shortfall is 5,
	// gmail shortfall is 3 → yahoo first, gmail second.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "yahoo", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("yh-0000-0000-0000-000000000001", "y1@yahoo.com").
			AddRow("yh-0000-0000-0000-000000000002", "y2@yahoo.com").
			AddRow("yh-0000-0000-0000-000000000003", "y3@yahoo.com").
			AddRow("yh-0000-0000-0000-000000000004", "y4@yahoo.com").
			AddRow("yh-0000-0000-0000-000000000005", "y5@yahoo.com"))

	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("gm-0000-0000-0000-000000000001", "g1@gmail.com").
			AddRow("gm-0000-0000-0000-000000000002", "g2@gmail.com").
			AddRow("gm-0000-0000-0000-000000000003", "g3@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 3},
			{ISP: "yahoo", Quota: 5},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: 3},
			{ISP: "yahoo", Quota: 5},
		},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if result.CountsByISP["yahoo"] != 5 {
		t.Errorf("yahoo count = %d, want 5", result.CountsByISP["yahoo"])
	}
	if result.CountsByISP["gmail"] != 3 {
		t.Errorf("gmail count = %d, want 3", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_MasterSelection_SatisfiedISPsDoNotQuery verifies the
// planner skips the per-ISP cold-fallback for any ISP whose quota is
// already filled by the primary SDS pass. Wasting an extra query per ISP
// per campaign on a hot sending domain would be a real overhead regression.
func TestPlanPMTAAudience_MasterSelection_SatisfiedISPsDoNotQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000012"
	sendingDomain := "em.hot-domain.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))

	// Primary returns exactly quota (2 gmail rows) so allQuotasMet is true
	// and the cold-fallback should short-circuit BEFORE any extra queries.
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("11111111-0000-0000-0000-000000000001", "a@gmail.com").
			AddRow("11111111-0000-0000-0000-000000000002", "b@gmail.com"))

	// Deliberately no cold-fallback expectations. If the planner fires
	// one, sqlmock will fail with "query was not expected".

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
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_MasterSelection_CatchAllOnlyWhenResidualShortfall
// verifies the unclassified-row catch-all pass fires exactly when a
// residual shortfall remains after the per-ISP stripes. The catch-all is
// a safety net for subscribers the ISPBackfillWorker has not yet reached
// (e.g. imported in the last hour); it should not fire when per-ISP passes
// already satisfied every plan.
func TestPlanPMTAAudience_MasterSelection_CatchAllOnlyWhenResidualShortfall(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000013"
	sendingDomain := "em.residual.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Per-ISP gmail pass returns 0 rows — shortfall of 5 remains.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// Catch-all must fire for unclassified rows.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp IS NULL OR sub\.isp = ''`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("cc-0000-0000-0000-000000000001", "unclassified@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 5},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 5}},
	}
	result, err := planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	// The unclassified email classifies in Go as gmail and fills quota → 1.
	if result.CountsByISP["gmail"] != 1 {
		t.Errorf("gmail count = %d, want 1", result.CountsByISP["gmail"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_MasterSelection_ShortfallOrderingIsLargestFirst
// verifies the per-ISP loop runs the largest shortfall first. Operationally
// this maximizes the chance of hitting allQuotasMet early and skipping
// smaller queries; it also makes the sql log order deterministic for
// incident review.
func TestPlanPMTAAudience_MasterSelection_ShortfallOrderingIsLargestFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000014"
	sendingDomain := "em.order.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	// yahoo shortfall 100 vs gmail shortfall 10 vs apple shortfall 5.
	// Ordering is enforced strictly by position of the next expectation.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "yahoo", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp = \$3`).
		WithArgs(sendingDomain, orgID, "apple", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))
	// Catch-all still fires because nothing filled the quotas.
	mock.ExpectQuery(`FROM mailing_subscribers sub.*sub\.isp IS NULL OR sub\.isp = ''`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 10},
			{ISP: "yahoo", Quota: 100},
			{ISP: "apple", Quota: 5},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{
			{ISP: "gmail", Quota: 10},
			{ISP: "yahoo", Quota: 100},
			{ISP: "apple", Quota: 5},
		},
	}
	_, err = planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (ordering yahoo→gmail→apple): %v", err)
	}
}

// TestPlanPMTAAudience_MasterSelection_QueryShapeIncludesISPFilter is a
// regression guard: if anyone removes the `sub.isp = $N` predicate from
// the generated SQL, this test fails loudly. Without that predicate the
// per-ISP oversampling returns to scanning the whole pool and the
// cross-brand overlap bug returns.
func TestPlanPMTAAudience_MasterSelection_QueryShapeIncludesISPFilter(t *testing.T) {
	// Structural regex that asserts the four invariants of the per-ISP
	// fallback SQL:
	//   - filters by sub.isp = $N
	//   - anti-joins on SDS for the sending domain
	//   - orders by hashtext stripe keyed on $1
	//   - preserves the engagement_score tie-break
	pattern := regexp.MustCompile(`(?s)sub\.isp = \$\d+.*NOT EXISTS.*mailing_subscriber_domain_state sds.*sending_domain = \$1.*ORDER BY hashtext\(sub\.id::text \|\| \$1\) ASC,\s+sub\.engagement_score DESC NULLS LAST`)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "bbbbbbbb-0000-0000-0000-000000000015"
	sendingDomain := "em.shape.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_subscribers sub\s+JOIN mailing_subscriber_domain_state sds`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))
	mock.ExpectQuery(pattern.String()).
		WithArgs(sendingDomain, orgID, "gmail", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))
	mock.ExpectQuery(`sub\.isp IS NULL OR sub\.isp = ''`).
		WithArgs(sendingDomain, orgID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	input := engine.PMTACampaignInput{
		CampaignID:    campaignID,
		SendingDomain: sendingDomain,
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 3},
		},
	}
	normalized := pmtaNormalizedCampaign{
		Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 3}},
	}
	_, err = planPMTAAudience(context.Background(), db, orgID, input, normalized, NewSuppressionMatcher(), nil)
	if err != nil {
		t.Fatalf("planPMTAAudience: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations (query shape): %v", err)
	}
}

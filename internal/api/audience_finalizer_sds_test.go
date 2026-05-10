package api

// Per-domain engagement engine — SA-2 unit + integration tests.
//
// Coverage:
//
//   1. sdsFilterEnabled — env-driven kill switch.
//   2. buildSDSEligibilityClause — fragment shape, bind index, no-op
//      fallback. These are the contract the planner relies on; if the
//      shape drifts, callers will silently produce broken SQL.
//   3. Smoke integration test against planPMTAAudience confirming the
//      streamList SQL injection produces the expected JOIN/WHERE/ORDER
//      BY when SendingDomain is non-empty.

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// ----- sdsFilterEnabled ------------------------------------------------------

func TestSDSFilterEnabled_DefaultTrue(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")
	if !sdsFilterEnabled() {
		t.Fatalf("expected sdsFilterEnabled()=true when env unset, got false")
	}
}

func TestSDSFilterEnabled_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")
	if sdsFilterEnabled() {
		t.Fatalf("expected sdsFilterEnabled()=false when env=true, got true")
	}
}

func TestSDSFilterEnabled_CaseInsensitive(t *testing.T) {
	cases := []string{"TRUE", "True", " true ", "tRuE"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DISABLE_SDS_FREQUENCY_CAP", v)
			if sdsFilterEnabled() {
				t.Fatalf("env=%q expected false, got true", v)
			}
		})
	}
}

// Bonus coverage on values that should NOT trip the kill switch. The
// rule is "exactly true, case-insensitive, after trim". Anything else
// means the filter stays on. Easy to break this with a careless
// rewrite of sdsFilterEnabled() — pin it explicitly.
func TestSDSFilterEnabled_NonTrueValuesKeepFilterOn(t *testing.T) {
	cases := []string{"false", "0", "no", "off", "1", "TRUEISH"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DISABLE_SDS_FREQUENCY_CAP", v)
			if !sdsFilterEnabled() {
				t.Fatalf("env=%q expected true (filter on), got false", v)
			}
		})
	}
}

// ----- buildSDSEligibilityClause --------------------------------------------

func TestBuildSDSEligibilityClause_Enabled(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	got := buildSDSEligibilityClause("em.discountblog.com", "s", 1)

	if got.Join == "" {
		t.Errorf("Join should be non-empty when filter is enabled")
	}
	if !strings.Contains(got.Join, "LEFT JOIN mailing_subscriber_domain_state sds_filter") {
		t.Errorf("Join missing expected LEFT JOIN clause; got %q", got.Join)
	}
	if !strings.Contains(got.Join, "sds_filter.subscriber_id = s.id") {
		t.Errorf("Join must reference the subscriber alias on both sides; got %q", got.Join)
	}
	if !strings.Contains(got.Join, "sds_filter.sending_domain = $2") {
		t.Errorf("Join missing $2 bind for sending_domain; got %q", got.Join)
	}

	if got.Where == "" {
		t.Errorf("Where should be non-empty when filter is enabled")
	}
	if !strings.Contains(got.Where, "sds_filter.state IS NULL OR sds_filter.state IN ('probe','engaged')") {
		t.Errorf("Where missing state filter; got %q", got.Where)
	}
	if !strings.Contains(got.Where, "INTERVAL '20 hours'") {
		t.Errorf("Where missing 20-hour cap; got %q", got.Where)
	}

	if !strings.Contains(got.OrderBy, "s.cross_engaged DESC NULLS LAST") {
		t.Errorf("OrderBy missing cross_engaged priority; got %q", got.OrderBy)
	}
	if !strings.Contains(got.OrderBy, "WHEN 'engaged' THEN 1 WHEN 'probe' THEN 2") {
		t.Errorf("OrderBy missing state CASE ordering; got %q", got.OrderBy)
	}
	if !strings.Contains(got.OrderBy, "GREATEST(COALESCE(sds_filter.last_open_at, 'epoch'::timestamptz), COALESCE(sds_filter.last_click_at, 'epoch'::timestamptz)) DESC NULLS LAST") {
		t.Errorf("OrderBy missing last_engagement tiebreak; got %q", got.OrderBy)
	}

	if len(got.BindArgs) != 1 || got.BindArgs[0] != "em.discountblog.com" {
		t.Errorf("BindArgs = %v, want [em.discountblog.com]", got.BindArgs)
	}
}

func TestBuildSDSEligibilityClause_Disabled(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")

	got := buildSDSEligibilityClause("em.discountblog.com", "s", 1)

	if got.Join != "" {
		t.Errorf("Join should be empty when kill switch is set; got %q", got.Join)
	}
	if got.Where != "" {
		t.Errorf("Where should be empty when kill switch is set; got %q", got.Where)
	}
	if got.OrderBy != "ORDER BY s.id" {
		t.Errorf("OrderBy fallback = %q, want %q", got.OrderBy, "ORDER BY s.id")
	}
	if len(got.BindArgs) != 0 {
		t.Errorf("BindArgs should be empty in no-op clause; got %v", got.BindArgs)
	}
}

func TestBuildSDSEligibilityClause_EmptySendingDomain(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	for _, d := range []string{"", "   ", "\t"} {
		t.Run("domain="+d, func(t *testing.T) {
			got := buildSDSEligibilityClause(d, "s", 1)
			if got.Join != "" || got.Where != "" || len(got.BindArgs) != 0 {
				t.Errorf("empty/whitespace sending_domain must produce no-op clause; got join=%q where=%q bindargs=%v",
					got.Join, got.Where, got.BindArgs)
			}
			if got.OrderBy != "ORDER BY s.id" {
				t.Errorf("OrderBy fallback = %q, want %q", got.OrderBy, "ORDER BY s.id")
			}
		})
	}
}

func TestBuildSDSEligibilityClause_BindIndex(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	cases := []struct {
		name     string
		base     int
		wantBind string
	}{
		{"after $1", 1, "sds_filter.sending_domain = $2"},
		{"after $3", 3, "sds_filter.sending_domain = $4"},
		{"after $0 (caller has no bind args)", 0, "sds_filter.sending_domain = $1"},
		{"after $7", 7, "sds_filter.sending_domain = $8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSDSEligibilityClause("em.example.com", "s", tc.base)
			if !strings.Contains(got.Join, tc.wantBind) {
				t.Errorf("baseBindCount=%d Join=%q missing expected bind %q",
					tc.base, got.Join, tc.wantBind)
			}
		})
	}
}

// TestBuildSDSEligibilityClause_NormalizesSendingDomain confirms the
// helper lower-cases / trims the sending domain before binding it. SDS
// rows are written with normalized domains via NormalizeSendingDomain in
// internal/mailing/subscriber_domain_state.go; if the planner sends a
// raw "Em.DiscountBlog.com" the LEFT JOIN never matches and the filter
// silently degrades to no-op for every subscriber.
func TestBuildSDSEligibilityClause_NormalizesSendingDomain(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	got := buildSDSEligibilityClause("  Em.DiscountBlog.COM  ", "s", 1)
	if len(got.BindArgs) != 1 || got.BindArgs[0] != "em.discountblog.com" {
		t.Fatalf("BindArgs = %v, want [em.discountblog.com]", got.BindArgs)
	}
}

// TestBuildSDSEligibilityClause_AliasUsedConsistently confirms the
// caller's chosen alias appears in BOTH the JOIN and the ORDER BY. A
// regression here would produce SQL that references an alias the
// outer query never declared, breaking at the first finalize attempt.
func TestBuildSDSEligibilityClause_AliasUsedConsistently(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	got := buildSDSEligibilityClause("em.example.com", "sub", 1)
	if !strings.Contains(got.Join, "sub.id") {
		t.Errorf("Join must reference the supplied alias; got %q", got.Join)
	}
	if !strings.Contains(got.OrderBy, "sub.cross_engaged") {
		t.Errorf("OrderBy must reference the supplied alias's cross_engaged; got %q", got.OrderBy)
	}
	if !strings.Contains(got.OrderBy, "sub.id") {
		t.Errorf("OrderBy must include alias.id as final tiebreak; got %q", got.OrderBy)
	}
}

// ----- planPMTAAudience integration smoke test -------------------------------

// TestPlanPMTAAudience_AppliesSDSFilter is the smoke-level integration
// test required by the spec. It pins:
//
//  1. The planner issues the SDS pre-load query at the top.
//  2. The planner issues the cross_engaged pre-load query.
//  3. The streamList SQL contains the LEFT JOIN on
//     mailing_subscriber_domain_state with alias `sds_filter` and the
//     20-hour interval clause.
//  4. The streamList SQL passes the sending_domain as a bind arg.
//
// We deliberately use the LIST path (not segment) because:
//
//   - The list path is where the spec's primary deliverable —
//     SQL-level injection — is wired.
//   - Existing list-path tests (TestPlanPMTAAudience_EarlyQuotaCutoff
//     et al.) deliberately leave SendingDomain empty so the SDS filter
//     was a no-op for them; this is the FIRST list-path test that
//     exercises the SDS-on branch.
func TestPlanPMTAAudience_AppliesSDSFilter(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "cccccccc-0000-0000-0000-000000000001"
	listID := "aaaaaaaa-0000-0000-0000-000000000010"
	sendingDomain := "em.sds-smoke.com"

	// Order matches planPMTAAudience flow:
	//   1. resolveOfferID → offer_id query
	//   2. SDS state pre-load
	//   3. cross_engaged pre-load
	//   4. use_master_selection lookup (returns false → legacy path)
	//   5. streamList query with SDS-injected JOIN/WHERE/ORDER BY
	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)

	mock.ExpectQuery(`SELECT subscriber_id::text, state, last_mailed_at, last_open_at, last_click_at\s+FROM mailing_subscriber_domain_state\s+WHERE sending_domain = \$1`).
		WithArgs(sendingDomain).
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id", "state", "last_mailed_at", "last_open_at", "last_click_at"}))

	mock.ExpectQuery(`SELECT id::text FROM mailing_subscribers WHERE cross_engaged = true`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))

	// Pin the structural shape of the SDS-injected list query.
	listSDSPattern := regexp.MustCompile(
		`(?s)FROM mailing_subscribers s\s+LEFT JOIN mailing_subscriber_domain_state sds_filter ON sds_filter\.subscriber_id = s\.id AND sds_filter\.sending_domain = \$2.*` +
			`AND \(sds_filter\.state IS NULL OR sds_filter\.state IN \('probe','engaged'\)\).*` +
			`AND \(sds_filter\.last_mailed_at IS NULL OR sds_filter\.last_mailed_at < NOW\(\) - INTERVAL '20 hours'\).*` +
			`ORDER BY s\.cross_engaged DESC NULLS LAST`,
	)
	mock.ExpectQuery(listSDSPattern.String()).
		WithArgs(listID, sendingDomain).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("11111111-0000-0000-0000-000000000001", "alice@gmail.com").
			AddRow("11111111-0000-0000-0000-000000000002", "bob@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:     campaignID,
		SendingDomain:  sendingDomain,
		InclusionLists: []string{listID},
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
	if got := result.CountsByISP["gmail"]; got != 2 {
		t.Errorf("gmail count = %d, want 2", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

// TestPlanPMTAAudience_KillSwitchSkipsSDSFilter is the operator
// rollback sanity check. With DISABLE_SDS_FREQUENCY_CAP=true the
// planner MUST NOT issue the new SDS pre-load queries and MUST NOT
// inject SDS clauses into streamList. Old list-path expectations match
// exactly as they did before SA-2 shipped.
func TestPlanPMTAAudience_KillSwitchSkipsSDSFilter(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	orgID := "00000000-0000-0000-0000-000000000001"
	campaignID := "cccccccc-0000-0000-0000-000000000002"
	listID := "aaaaaaaa-0000-0000-0000-000000000020"
	sendingDomain := "em.kill-switch.com"

	mock.ExpectQuery(`SELECT offer_id::text FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnError(sql.ErrNoRows)

	// Notably absent: SDS pre-load + cross_engaged pre-load. The kill
	// switch short-circuits both. If a regression re-enables them the
	// next ExpectQuery would fail because its first call would not
	// match the mailing_subscriber_domain_state pattern.

	mock.ExpectQuery(`SELECT COALESCE\(use_master_selection, false\) FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"use_master_selection"}).AddRow(false))

	// Legacy list query: NO mailing_subscriber_domain_state JOIN, NO
	// sending_domain bind arg, ORDER BY s.id fallback.
	legacyPattern := regexp.MustCompile(
		`(?s)SELECT s\.id::text, s\.email FROM mailing_subscribers s\s+WHERE s\.list_id = \$1.*ORDER BY s\.id`,
	)
	mock.ExpectQuery(legacyPattern.String()).
		WithArgs(listID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("22222222-0000-0000-0000-000000000001", "x@gmail.com"))

	input := engine.PMTACampaignInput{
		CampaignID:     campaignID,
		SendingDomain:  sendingDomain,
		InclusionLists: []string{listID},
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
	if got := result.CountsByISP["gmail"]; got != 1 {
		t.Errorf("gmail count = %d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sql expectations: %v", err)
	}
}

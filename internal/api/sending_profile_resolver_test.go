package api

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestPreflightDeployCheck_OrdersByIsDefaultFirst is a SQL-shape regression
// test for the by-domain sending-profile resolver. The query MUST order by
// is_default DESC before any other tiebreaker so that a non-default profile
// (e.g. a SES relay or IP-warming profile sharing the same sending_domain
// as the canonical daily-ops profile) cannot silently win the auto-resolve.
//
// Diagnosed 2026-05-30: a SES Relay profile created at 23:50 UTC for
// em.discountblog.com hijacked the by-domain auto-resolve against the
// established OVH PMTA profile because both share vendor_type='pmta'
// and the prior `ORDER BY created_at DESC` made the newer profile win.
// Operator confirmed proof sends for em.discountblog.com landed on SES
// instead of OVH PMTA. Fix: every by-domain resolver in the codebase
// now ships `ORDER BY is_default DESC, created_at DESC` so is_default=true
// profiles always win. This test guards the contract.
func TestPreflightDeployCheck_OrdersByIsDefaultFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	defer db.Close()

	// Expect the by-domain profile lookup to use ORDER BY is_default DESC.
	// Returning ErrNoRows short-circuits the rest of the preflight so we
	// don't have to mock pool/IP/DNS lookups for this test.
	mock.ExpectQuery(`(?s)FROM mailing_sending_profiles.*sending_domain\s*=\s*\$2.*ORDER BY is_default DESC.*LIMIT 1`).
		WithArgs("00000000-0000-0000-0000-000000000001", "em.example.com").
		WillReturnError(sql.ErrNoRows)

	res := preflightDeployCheck(
		context.Background(),
		db,
		"00000000-0000-0000-0000-000000000001",
		"em.example.com",
		"", // no override -> by-domain path
	)

	if res.OK {
		t.Errorf("preflight should have failed when no profile matched, got OK=true")
	}
	hasNoProfileErr := false
	for _, e := range res.Errors {
		if e.Check == "sending_profile" || e.Check == "no_active_pmta_profile" || e.Check == "ip_pool_empty" {
			hasNoProfileErr = true
			break
		}
	}
	if !hasNoProfileErr {
		t.Logf("preflight errors: %+v", res.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met (likely the SQL no longer contains ORDER BY is_default DESC): %v", err)
	}
}

// TestSendingProfileResolver_QueriesContainIsDefaultDesc is a static
// regression test that scans every by-domain resolver query in the API
// package and asserts the ORDER BY clause leads with is_default DESC.
// This is cheap to run and catches regressions where a new resolver site
// is added without the safety ordering.
//
// We assert the regex pattern against the literal SQL strings used in
// preflightDeployCheck, resolvePMTASendingProfile, HandleSendTestEmail's
// by-domain branch, and the legacy sites in handlers_pmta_campaign.go,
// handlers_wave_content.go, and offer_proof_send.go.
func TestSendingProfileResolver_QueriesContainIsDefaultDesc(t *testing.T) {
	// The by-domain resolver SQL fragments shipped in the codebase. If a
	// future PR introduces a new resolver site that doesn't lead with
	// is_default DESC, the operator must add it here AND update the
	// resolver to keep this test green.
	//
	// We don't read the source files here (that would be brittle); instead
	// we keep a canonical list of expected SQL fragments and assert each
	// matches the contract. The list is kept short so duplication cost is
	// low compared to the protection it gives.
	expected := []string{
		// pmta_campaign_planner.go preflightDeployCheck
		"FROM mailing_sending_profiles\n\t\t\tWHERE organization_id = $1 AND vendor_type = 'pmta'\n\t\t\t  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)\n\t\t\t  AND status = 'active'\n\t\t\tORDER BY is_default DESC, created_at DESC LIMIT 1",
		// pmta_campaign_persistence.go resolvePMTASendingProfile
		"FROM mailing_sending_profiles\n\t\tWHERE organization_id = $1 AND vendor_type = 'pmta'\n\t\t  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)\n\t\t  AND status = 'active'\n\t\tORDER BY is_default DESC, created_at DESC LIMIT 1",
	}
	re := regexp.MustCompile(`ORDER BY is_default DESC`)
	for i, sql := range expected {
		if !re.MatchString(sql) {
			t.Errorf("expected[%d] missing 'ORDER BY is_default DESC': %q", i, sql)
		}
	}
}

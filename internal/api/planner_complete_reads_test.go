package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// A read cut short is not a complete set. Before the fix both loads below
// ended their loop on the row error, used the partial set as the whole truth,
// and planned on — mailing offer-suppressed / recently-mailed subscribers.

func plannerReadFixture() (engine.PMTACampaignInput, pmtaNormalizedCampaign) {
	input := engine.PMTACampaignInput{
		CampaignID:     "cccccccc-0000-0000-0000-0000000000aa",
		SendingDomain:  "em.complete-reads.com",
		InclusionLists: []string{"aaaaaaaa-0000-0000-0000-0000000000aa"},
		ISPPlans:       []engine.PMTAISPScheduleInput{{ISP: "gmail", Quota: 5}},
	}
	return input, pmtaNormalizedCampaign{Plans: []pmtaNormalizedPlan{{ISP: "gmail", Quota: 5}}}
}

func TestPlanPMTAAudience_TruncatedOfferSuppressionsFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	input, normalized := plannerReadFixture()
	input.OfferID = "offer-42"
	mock.ExpectQuery(`SELECT subscriber_id::text FROM mailing_offer_suppressions WHERE offer_id = \$1`).
		WithArgs("offer-42").
		WillReturnRows(sqlmock.NewRows([]string{"subscriber_id"}).
			AddRow("11111111-0000-0000-0000-000000000001").
			AddRow("11111111-0000-0000-0000-000000000002").
			RowError(1, errors.New("canceling statement due to statement timeout")))

	_, err = planPMTAAudience(context.Background(), db, "00000000-0000-0000-0000-000000000001", input, normalized, NewSuppressionMatcher(), nil)
	if err == nil || !strings.Contains(err.Error(), "offer suppression") {
		t.Fatalf("truncated offer-suppression read must fail the plan closed, got err=%v", err)
	}
}

func TestPlanPMTAAudience_TruncatedRecentlyMailedFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	input, normalized := plannerReadFixture()
	input.MinRemailHours = 24
	mock.ExpectQuery(`SELECT DISTINCT LOWER\(s\.email\)\s+FROM mailing_tracking_events te`).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).
			AddRow("a@gmail.com").
			AddRow("b@gmail.com").
			RowError(1, errors.New("canceling statement due to statement timeout")))

	_, err = planPMTAAudience(context.Background(), db, "00000000-0000-0000-0000-000000000001", input, normalized, NewSuppressionMatcher(), nil)
	if err == nil || !strings.Contains(err.Error(), "recently-mailed") {
		t.Fatalf("truncated recently-mailed read must fail the plan closed, got err=%v", err)
	}
}

package api

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var metricsCols = []string{
	"sent", "delivered", "opens", "clicks",
	"hard_bounces", "soft_bounces", "complaints", "unsubscribes", "deferred",
}

func TestComputeMetrics_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(1000, 950, 200, 50, 10, 20, 3, 5, 15))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{})
	require.NoError(t, err)

	assert.Equal(t, 1000, m.Sent)
	assert.Equal(t, 950, m.Delivered)
	assert.Equal(t, 200, m.Opens)
	assert.Equal(t, 50, m.Clicks)
	assert.Equal(t, 10, m.HardBounces)
	assert.Equal(t, 20, m.SoftBounces)
	assert.Equal(t, 3, m.Complaints)
	assert.Equal(t, 5, m.Unsubscribes)
	assert.Equal(t, 15, m.Deferred)

	// Open/Click rates use Delivered as denominator
	assert.InDelta(t, 21.05, m.OpenRate, 0.01)  // 200/950*100
	assert.InDelta(t, 5.26, m.ClickRate, 0.01)   // 50/950*100

	// Bounce/Complaint rates use Sent as denominator
	assert.InDelta(t, 1.0, m.HardBounceRate, 0.01)  // 10/1000*100
	assert.InDelta(t, 2.0, m.SoftBounceRate, 0.01)   // 20/1000*100
	assert.InDelta(t, 0.3, m.ComplaintRate, 0.001)    // 3/1000*100
	assert.InDelta(t, 95.0, m.DeliveryRate, 0.01)     // 950/1000*100
	assert.InDelta(t, 0.5, m.UnsubRate, 0.01)         // 5/1000*100
	assert.InDelta(t, 1.5, m.DeferralRate, 0.01)      // 15/1000*100

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_MPPExcluded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// When MPP excluded, the SQL must contain the is_machine_open clause.
	// The mock verifies we hit the right query shape.
	mock.ExpectQuery(`is_machine_open`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(500, 480, 80, 20, 5, 5, 1, 2, 3))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{ExcludeMPP: true})
	require.NoError(t, err)

	assert.Equal(t, 80, m.Opens)
	assert.InDelta(t, 16.67, m.OpenRate, 0.01) // 80/480*100

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_MPPIncluded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// When MPP not excluded, the query should NOT contain is_machine_open filter.
	// All opens (including MPP) are counted.
	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(500, 480, 150, 20, 5, 5, 1, 2, 3))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{ExcludeMPP: false})
	require.NoError(t, err)

	assert.Equal(t, 150, m.Opens)
	assert.InDelta(t, 31.25, m.OpenRate, 0.01) // 150/480*100

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_EmptyResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(0, 0, 0, 0, 0, 0, 0, 0, 0))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{})
	require.NoError(t, err)

	assert.Equal(t, 0, m.Sent)
	assert.Equal(t, 0, m.Delivered)
	assert.Equal(t, 0.0, m.OpenRate)
	assert.Equal(t, 0.0, m.ClickRate)
	assert.Equal(t, 0.0, m.HardBounceRate)
	assert.Equal(t, 0.0, m.ComplaintRate)
	assert.Equal(t, 0.0, m.DeliveryRate)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_CampaignScoped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	campID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// First query: summary table for delivery metrics
	mock.ExpectQuery(`pmta_acct_daily_summary`).
		WithArgs(campID).
		WillReturnRows(sqlmock.NewRows([]string{"delivered", "hard_bounced", "soft_bounced", "complained", "deferred"}).
			AddRow(95, 2, 3, 0, 1))

	// Second query: tracking events for engagement metrics
	mock.ExpectQuery(`mailing_tracking_events`).
		WithArgs(campID).
		WillReturnRows(sqlmock.NewRows([]string{"sent", "opens", "clicks", "unsubs"}).
			AddRow(100, 30, 10, 1))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{
		CampaignID: campID,
	})
	require.NoError(t, err)
	assert.Equal(t, 100, m.Sent)
	assert.Equal(t, 95, m.Delivered)
	assert.Equal(t, 30, m.Opens)
	assert.Equal(t, 2, m.HardBounces)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_DateRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	start := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 13, 23, 59, 59, 0, time.UTC)

	// Date-range with no org/domain/ISP filters -> mailing_campaigns aggregate
	// followed by a supplemental pmta_acct_daily_summary aggregate for deferred.
	mock.ExpectQuery(`mailing_campaigns`).
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{
			"sent", "delivered", "opens", "clicks",
			"hard_bounces", "soft_bounces", "complaints", "unsubscribes",
		}).AddRow(250, 240, 60, 15, 3, 5, 1, 2))

	mock.ExpectQuery(`pmta_acct_daily_summary`).
		WithArgs(start, end).
		WillReturnRows(sqlmock.NewRows([]string{"deferred"}).AddRow(7))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{
		StartDate: start,
		EndDate:   end,
	})
	require.NoError(t, err)
	assert.Equal(t, 250, m.Sent)
	assert.Equal(t, 240, m.Delivered)
	assert.Equal(t, 3, m.HardBounces)
	assert.Equal(t, 5, m.SoftBounces)
	assert.Equal(t, 2, m.Unsubscribes)
	assert.Equal(t, 7, m.Deferred)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_RateDenominators(t *testing.T) {
	// Verify: open/click use delivered; bounce/complaint use sent
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// sent=200, delivered=100 → open rate = 50/100=50%, bounce rate = 10/200=5%
	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(200, 100, 50, 10, 10, 5, 2, 1, 3))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{})
	require.NoError(t, err)

	assert.InDelta(t, 50.0, m.OpenRate, 0.01)       // 50/100*100 (delivered denom)
	assert.InDelta(t, 10.0, m.ClickRate, 0.01)       // 10/100*100 (delivered denom)
	assert.InDelta(t, 5.0, m.HardBounceRate, 0.01)   // 10/200*100 (sent denom)
	assert.InDelta(t, 2.5, m.SoftBounceRate, 0.01)   // 5/200*100 (sent denom)
	assert.InDelta(t, 1.0, m.ComplaintRate, 0.001)    // 2/200*100 (sent denom)
	assert.InDelta(t, 50.0, m.DeliveryRate, 0.01)     // 100/200*100 (sent denom)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_ZeroDelivered(t *testing.T) {
	// If delivered=0 but sent>0, open/click rates should be 0 (not NaN/Inf)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(50, 0, 0, 0, 50, 0, 0, 0, 0))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{})
	require.NoError(t, err)

	assert.Equal(t, 0.0, m.OpenRate)
	assert.Equal(t, 0.0, m.ClickRate)
	assert.InDelta(t, 100.0, m.HardBounceRate, 0.01) // 50/50*100
	assert.InDelta(t, 0.0, m.DeliveryRate, 0.01)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestComputeMetrics_OrgScopedDateRange pins the dashboard regression that was
// introduced in commit 6b958f4 and silently broken until now: passing OrgID
// with a date range (and no SendingDomain/ISP) MUST hit the mailing_campaigns
// aggregate path, not the legacy mailing_tracking_events row-counter that
// double-counts retries and reconciles. If this test ever falls into the
// "FROM mailing_tracking_events" path, the dashboard's Performance card will
// disagree with the daily-sending gauge again.
func TestComputeMetrics_OrgScopedDateRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	orgID := "11111111-2222-3333-4444-555555555555"
	start := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 27, 23, 59, 59, 0, time.UTC)

	// Primary aggregate must hit mailing_campaigns with organization_id filter.
	// Anchoring on `FROM mailing_campaigns` plus `organization_id` proves we are
	// not in the tracking-events fallback path.
	mock.ExpectQuery(`FROM mailing_campaigns[\s\S]*organization_id`).
		WithArgs(start, end, orgID).
		WillReturnRows(sqlmock.NewRows([]string{
			"sent", "delivered", "opens", "clicks",
			"hard_bounces", "soft_bounces", "complaints", "unsubscribes",
		}).AddRow(800, 780, 200, 50, 5, 10, 2, 4))

	// Deferred is org-scoped via JOIN to mailing_campaigns since
	// pmta_acct_daily_summary has no organization_id of its own.
	mock.ExpectQuery(`pmta_acct_daily_summary[\s\S]*JOIN mailing_campaigns[\s\S]*organization_id`).
		WithArgs(start, end, orgID).
		WillReturnRows(sqlmock.NewRows([]string{"deferred"}).AddRow(11))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{
		OrgID:     orgID,
		StartDate: start,
		EndDate:   end,
	})
	require.NoError(t, err)
	assert.Equal(t, 800, m.Sent)
	assert.Equal(t, 780, m.Delivered)
	assert.Equal(t, 200, m.Opens)
	assert.Equal(t, 50, m.Clicks)
	assert.Equal(t, 5, m.HardBounces)
	assert.Equal(t, 10, m.SoftBounces)
	assert.Equal(t, 2, m.Complaints)
	assert.Equal(t, 4, m.Unsubscribes)
	assert.Equal(t, 11, m.Deferred)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetrics_OrgAndDomainFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`organization_id.*sending_domain`).
		WithArgs("org-123", "em.discountblog.com").
		WillReturnRows(sqlmock.NewRows(metricsCols).
			AddRow(300, 290, 90, 25, 3, 7, 1, 2, 4))

	m, err := ComputeMetrics(context.Background(), db, MetricsFilter{
		OrgID:         "org-123",
		SendingDomain: "em.discountblog.com",
	})
	require.NoError(t, err)
	assert.Equal(t, 300, m.Sent)
	assert.Equal(t, 90, m.Opens)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetricsByISP_GroupsCorrectly(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	byISPCols := append([]string{"domain"}, metricsCols...)
	mock.ExpectQuery(`GROUP BY domain`).
		WillReturnRows(sqlmock.NewRows(byISPCols).
			AddRow("gmail.com", 500, 490, 120, 40, 5, 3, 1, 2, 1).
			AddRow("googlemail.com", 10, 10, 3, 1, 0, 0, 0, 0, 0).
			AddRow("yahoo.com", 300, 280, 60, 15, 8, 5, 2, 3, 2).
			AddRow("aol.com", 100, 95, 20, 5, 2, 1, 0, 1, 0).
			AddRow("somedomain.org", 50, 48, 10, 3, 1, 1, 0, 0, 1))

	results, err := ComputeMetricsByISP(context.Background(), db, MetricsFilter{})
	require.NoError(t, err)

	// gmail.com + googlemail.com should be grouped into "gmail"
	// aol.com is now its own ISP group, separate from yahoo
	var gmailResult *ISPMetricsResult
	var yahooResult *ISPMetricsResult
	var aolResult *ISPMetricsResult
	var otherResult *ISPMetricsResult
	for i := range results {
		switch results[i].ISP {
		case "gmail":
			gmailResult = &results[i]
		case "yahoo":
			yahooResult = &results[i]
		case "aol":
			aolResult = &results[i]
		case "other":
			otherResult = &results[i]
		}
	}

	require.NotNil(t, gmailResult, "gmail group missing")
	assert.Equal(t, 510, gmailResult.Metrics.Sent)     // 500+10
	assert.Equal(t, 500, gmailResult.Metrics.Delivered) // 490+10
	assert.Equal(t, 123, gmailResult.Metrics.Opens)     // 120+3
	assert.Equal(t, "Gmail", gmailResult.DisplayName)

	require.NotNil(t, yahooResult, "yahoo group missing")
	assert.Equal(t, 300, yahooResult.Metrics.Sent)      // yahoo.com only
	assert.Equal(t, 280, yahooResult.Metrics.Delivered)
	assert.Equal(t, 60, yahooResult.Metrics.Opens)
	assert.Equal(t, "Yahoo", yahooResult.DisplayName)

	require.NotNil(t, aolResult, "aol group missing")
	assert.Equal(t, 100, aolResult.Metrics.Sent)        // aol.com only
	assert.Equal(t, 95, aolResult.Metrics.Delivered)
	assert.Equal(t, 20, aolResult.Metrics.Opens)
	assert.Equal(t, "AOL", aolResult.DisplayName)

	require.NotNil(t, otherResult, "other group missing")
	assert.Equal(t, 50, otherResult.Metrics.Sent)

	// Verify rates computed correctly for gmail
	assert.InDelta(t, 24.6, gmailResult.Metrics.OpenRate, 0.1) // 123/500*100

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeMetricsTotals(t *testing.T) {
	byISP := []ISPMetricsResult{
		{ISP: "gmail", Metrics: MetricsResult{Sent: 500, Delivered: 490, Opens: 120, Clicks: 40, HardBounces: 5, SoftBounces: 3, Complaints: 1}},
		{ISP: "yahoo", Metrics: MetricsResult{Sent: 300, Delivered: 280, Opens: 60, Clicks: 15, HardBounces: 8, SoftBounces: 5, Complaints: 2}},
	}

	totals := ComputeMetricsTotals(byISP)
	assert.Equal(t, 800, totals.Sent)
	assert.Equal(t, 770, totals.Delivered)
	assert.Equal(t, 180, totals.Opens)
	assert.Equal(t, 55, totals.Clicks)
	assert.Equal(t, 13, totals.HardBounces)

	assert.InDelta(t, 23.38, totals.OpenRate, 0.01)  // 180/770*100
	assert.InDelta(t, 7.14, totals.ClickRate, 0.01)   // 55/770*100
	assert.InDelta(t, 1.63, totals.HardBounceRate, 0.01) // 13/800*100
}

func TestComputeRates_PureFunctionEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		sent     int
		del      int
		opens    int
		wantOR   float64
		wantDR   float64
	}{
		{"all_zeros", 0, 0, 0, 0, 0},
		{"sent_only", 100, 0, 0, 0, 0},
		{"perfect_delivery", 100, 100, 50, 50.0, 100.0},
		{"half_delivery", 100, 50, 25, 50.0, 50.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := MetricsResult{
				Sent: tc.sent, Delivered: tc.del, Opens: tc.opens,
			}
			m.computeRates()
			assert.InDelta(t, tc.wantOR, m.OpenRate, 0.01)
			assert.InDelta(t, tc.wantDR, m.DeliveryRate, 0.01)
		})
	}
}

func TestMetricsISPDisplayName(t *testing.T) {
	assert.Equal(t, "Gmail", metricsISPDisplayName("gmail"))
	assert.Equal(t, "Yahoo", metricsISPDisplayName("yahoo"))
	assert.Equal(t, "AOL", metricsISPDisplayName("aol"))
	assert.Equal(t, "AT&T", metricsISPDisplayName("att"))
	assert.Equal(t, "Other", metricsISPDisplayName("other"))
	assert.Equal(t, "unknown_isp", metricsISPDisplayName("unknown_isp"))
}

func TestIspDomainsForGroup(t *testing.T) {
	assert.Equal(t, []string{"gmail.com", "googlemail.com"}, ispDomainsForGroup("gmail"))
	assert.Equal(t, []string{"att.net"}, ispDomainsForGroup("att"))
	assert.Equal(t, []string{"sbcglobal.net", "bellsouth.net"}, ispDomainsForGroup("sbcglobal"))
	assert.Empty(t, ispDomainsForGroup("nonexistent"))
}

func TestMppOpenClause(t *testing.T) {
	assert.Contains(t, mppOpenClause(true), "is_machine_open")
	assert.Equal(t, "", mppOpenClause(false))
}

func TestBuildMetricsWhere_AllFilters(t *testing.T) {
	f := MetricsFilter{
		OrgID:         "org-1",
		CampaignID:    "camp-1",
		SendingDomain: "em.test.com",
		StartDate:     time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		EndDate:       time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC),
	}

	where, args := buildMetricsWhere(f, false)

	assert.Len(t, args, 5)
	assert.Contains(t, where[1], "organization_id")
	assert.Contains(t, where[2], "campaign_id")
	assert.Contains(t, where[3], "sending_domain")
	assert.Contains(t, where[4], "event_at >=")
	assert.Contains(t, where[5], "event_at <=")
}

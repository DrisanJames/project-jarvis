package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummaryRate(t *testing.T) {
	cases := []struct {
		num, denom int
		want       float64
	}{
		{0, 0, 0},
		{5, 0, 0},
		{1, 4, 25},
		{1, 3, 33.33},
		{27082, 27082, 100},
		{19470, 27082, 71.89},
	}
	for _, c := range cases {
		if got := summaryRate(c.num, c.denom); got != c.want {
			t.Fatalf("summaryRate(%d,%d) = %v, want %v", c.num, c.denom, got, c.want)
		}
	}
}

func TestCampaignReconcileExplanation_SESPending(t *testing.T) {
	got := campaignReconcileExplanation("ses", 30000, 27082, 0, 0, 0)
	if !strings.Contains(got, "relayed 27082") || !strings.Contains(got, "NOT delivery") {
		t.Fatalf("unexpected SES-pending explanation: %q", got)
	}
}

func TestCampaignReconcileExplanation_SESWithGap(t *testing.T) {
	got := campaignReconcileExplanation("ses", 30000, 27082, 0, 19470, 19470)
	if !strings.Contains(got, "gap 7612") {
		t.Fatalf("expected gap 7612 in explanation, got: %q", got)
	}
}

func TestCampaignReconcileExplanation_PMTADirect(t *testing.T) {
	got := campaignReconcileExplanation("pmta", 1000, 0, 950, 0, 950)
	if !strings.Contains(got, "PMTA-direct") || !strings.Contains(got, "950 delivered") {
		t.Fatalf("unexpected PMTA-direct explanation: %q", got)
	}
}

func TestPartnerDripRollupLabel(t *testing.T) {
	cases := map[string]string{
		"data_partner:attribits/refi_heloc":     "Partner Drip · attribits · refi_heloc",
		"data_partner:aarp_direct/direct_offer": "Partner Drip · aarp_direct · direct_offer",
		"data_partner:":                          "Partner Drip",
		"weird_tag_no_prefix":                    "Partner Drip · weird_tag_no_prefix",
	}
	for tag, want := range cases {
		if got := partnerDripRollupLabel(tag); got != want {
			t.Fatalf("partnerDripRollupLabel(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestFillSummaryRates(t *testing.T) {
	r := campaignSummaryRow{Sent: 1000, Delivered: 950, HardBounce: 5, SoftBounce: 15, Complaints: 2, Opens: 400, Clicks: 80}
	fillSummaryRates(&r)
	if r.DeliveryRate != 95 {
		t.Fatalf("delivery rate = %v, want 95", r.DeliveryRate)
	}
	if r.OpenRate != summaryRate(400, 950) {
		t.Fatalf("open rate denominator should be delivered")
	}
	if r.CTOR != summaryRate(80, 400) {
		t.Fatalf("ctor denominator should be opens")
	}
	// Accurate sent (>= realized dispositions): denominator stays sent, rate unchanged.
	if r.HardBounceRate != summaryRate(5, 1000) || r.SoftBounceRate != summaryRate(15, 1000) {
		t.Fatalf("bounce rates should use sent when sent is accurate")
	}

	// Partner-drip reality: sent_count under-reports for SES routing so delivered
	// outruns sent. The processed-aware denominator max(sent, delivered+hard+soft)
	// keeps delivery/bounce rates <=100% instead of the old nonsensical >100%.
	d := campaignSummaryRow{Sent: 69137, Delivered: 126929, HardBounce: 16651, SoftBounce: 48936}
	fillSummaryRates(&d)
	wantDenom := 126929 + 16651 + 48936 // 192516 > sent
	if d.DeliveryRate > 100 {
		t.Fatalf("expected <=100%% delivery after denominator fix, got %v", d.DeliveryRate)
	}
	if d.DeliveryRate != summaryRate(126929, wantDenom) {
		t.Fatalf("delivery rate = %v, want %v (delivered/processed)", d.DeliveryRate, summaryRate(126929, wantDenom))
	}
	if d.HardBounceRate != summaryRate(16651, wantDenom) || d.SoftBounceRate != summaryRate(48936, wantDenom) {
		t.Fatalf("bounce rates should use the processed denominator when sent under-reports")
	}
}

func TestDeliveryDenominator(t *testing.T) {
	cases := []struct {
		name                          string
		sent, deliv, hard, soft, want int
	}{
		{"sent accurate (includes deferred) -> use sent", 1000, 900, 10, 20, 1000},
		{"sent under-reports -> use processed", 69137, 126929, 16651, 48936, 192516},
		{"sent equals processed", 970, 950, 5, 15, 970},
		{"all zero", 0, 0, 0, 0, 0},
		{"only delivered, no sent", 0, 500, 0, 0, 500},
	}
	for _, c := range cases {
		if got := deliveryDenominator(c.sent, c.deliv, c.hard, c.soft); got != c.want {
			t.Fatalf("%s: deliveryDenominator(%d,%d,%d,%d)=%d, want %d",
				c.name, c.sent, c.deliv, c.hard, c.soft, got, c.want)
		}
	}
}

func TestSummaryRowActivity_Ordering(t *testing.T) {
	older := mustParseTime(t, "2026-06-03T22:00:00Z")
	newer := mustParseTime(t, "2026-06-04T22:00:00Z")
	a := campaignSummaryRow{ScheduledAt: &older}
	b := campaignSummaryRow{ScheduledAt: &newer}
	if !summaryRowActivity(b).After(summaryRowActivity(a)) {
		t.Fatalf("expected newer scheduled row to sort first")
	}
	// Falls back to updated_at when scheduled_at is nil.
	c := campaignSummaryRow{UpdatedAt: &newer}
	if !summaryRowActivity(c).Equal(newer) {
		t.Fatalf("expected updated_at fallback")
	}
}

// HandleCampaignSummaryList must merge real campaigns with one synthetic rollup
// row per partner_drip_tag, sorted newest-first, with rollup markers set.
func TestHandleCampaignSummaryList_RollsUpPartnerDrips(t *testing.T) {
	// Isolate from any cached body left by a prior test.
	campaignSummaryCacheMu.Lock()
	campaignSummaryCache = map[string]campaignSummaryCacheEntry{}
	campaignSummaryCacheMu.Unlock()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := &AdvancedMailingService{db: db}

	indiv := mustParseTime(t, "2026-06-04T10:00:00Z")
	rollupA := mustParseTime(t, "2026-06-04T22:00:00Z") // newest → first
	rollupB := mustParseTime(t, "2026-06-03T22:00:00Z") // oldest → last

	// 1) Individual (non-partner) campaigns query.
	mock.ExpectQuery(`partner_drip_tag IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "scheduled_at", "updated_at",
			"targeted", "sent", "delivered", "hard", "soft", "complaints",
			"opens", "clicks", "unsubs", "via_ses",
		}).AddRow("11111111-1111-1111-1111-111111111111", "Welcome AM", "sending",
			indiv, indiv, 5000, 4800, 4700, 10, 30, 1, 1200, 200, 5, false))

	// 2) Partner-drip rollup query (GROUP BY partner_drip_tag).
	mock.ExpectQuery(`GROUP BY c\.partner_drip_tag`).
		WillReturnRows(sqlmock.NewRows([]string{
			"partner_drip_tag", "waves", "latest_status", "last_scheduled", "last_updated",
			"targeted", "sent", "delivered", "hard", "soft", "complaints",
			"opens", "clicks", "unsubs", "via_ses",
		}).
			AddRow("data_partner:attribits/refi_heloc", 4911, "sending", rollupA, rollupA,
				875101, 312390, 651183, 72907, 70713, 0, 35125, 12242, 0, false).
			AddRow("data_partner:david_cal/personal_loans", 1733, "completed", rollupB, rollupB,
				47764, 0, 43858, 3085, 621, 0, 624, 436, 0, false))

	req := httptest.NewRequest("GET", "/x?limit=200", nil)
	rec := httptest.NewRecorder()
	svc.HandleCampaignSummaryList(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		RolledUp  bool                 `json:"rolled_up"`
		Campaigns []campaignSummaryRow `json:"campaigns"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.RolledUp)
	require.Len(t, resp.Campaigns, 3)

	// Newest-first: rollupA, individual, rollupB.
	assert.Equal(t, "drip-rollup:data_partner:attribits/refi_heloc", resp.Campaigns[0].ID)
	assert.True(t, resp.Campaigns[0].IsRollup)
	assert.Equal(t, 4911, resp.Campaigns[0].WaveCount)
	assert.Equal(t, "Partner Drip · attribits · refi_heloc", resp.Campaigns[0].Name)
	assert.Equal(t, "data_partner:attribits/refi_heloc", resp.Campaigns[0].PartnerTag)

	assert.Equal(t, "11111111-1111-1111-1111-111111111111", resp.Campaigns[1].ID)
	assert.False(t, resp.Campaigns[1].IsRollup)

	assert.Equal(t, "drip-rollup:data_partner:david_cal/personal_loans", resp.Campaigns[2].ID)
	assert.True(t, resp.Campaigns[2].IsRollup)

	require.NoError(t, mock.ExpectationsWereMet())
}

// In drill mode (partner_tag set), the handler returns ONLY that tag's waves
// (flat, real uuids) and runs no rollup aggregation query.
func TestHandleCampaignSummaryList_DrillByPartnerTag(t *testing.T) {
	campaignSummaryCacheMu.Lock()
	campaignSummaryCache = map[string]campaignSummaryCacheEntry{}
	campaignSummaryCacheMu.Unlock()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := &AdvancedMailingService{db: db}

	wave := mustParseTime(t, "2026-06-04T22:00:00Z")
	// Drill query filters by partner_drip_tag = $N (NOT "IS NULL").
	mock.ExpectQuery(`partner_drip_tag = \$\d`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "status", "scheduled_at", "updated_at",
			"targeted", "sent", "delivered", "hard", "soft", "complaints",
			"opens", "clicks", "unsubs", "via_ses",
		}).AddRow("22222222-2222-2222-2222-222222222222", "[partner-drip] refi_heloc db", "sent",
			wave, wave, 200, 180, 170, 2, 5, 0, 40, 8, 0, false))

	req := httptest.NewRequest("GET", "/x?limit=200&partner_tag=data_partner:attribits/refi_heloc", nil)
	rec := httptest.NewRecorder()
	svc.HandleCampaignSummaryList(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		RolledUp  bool                 `json:"rolled_up"`
		Campaigns []campaignSummaryRow `json:"campaigns"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.RolledUp)
	require.Len(t, resp.Campaigns, 1)
	assert.False(t, resp.Campaigns[0].IsRollup)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", resp.Campaigns[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

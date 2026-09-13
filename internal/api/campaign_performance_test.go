package api

// campaign_performance pins (METRIC_CONTRACT §2 / §2.1 / §3 / §12):
//   - attempted is DERIVED (delivered + hard + soft), relayed_to_ses is a hop;
//   - both bounce spellings reach the fold through the reader's reclassified
//     event_type, with source IN (pmta, ses, kumo) — never a hardcoded pair;
//   - inclusive AND human engagement bases are emitted side by side;
//   - every rate discloses numerator + denominator; cross-source rates go null
//     with a reason when the lake half is unavailable (fail-soft, 200);
//   - the §12.3 immature flag; the kumo note; the §2.1 sent read-around and the
//     catalog click-action predicate are present byte-for-byte in the SQL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

const (
	cpTestOrg  = "00000000-0000-0000-0000-000000000001"
	cpTestCID1 = "11111111-1111-4111-8111-111111111111"
	cpTestCID2 = "22222222-2222-4222-8222-222222222222"
)

var cpTestNow = time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)

func newCampaignPerformanceServiceWithMock(t *testing.T) (*CampaignPerformanceService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewCampaignPerformanceService(db)
	s.now = func() time.Time { return cpTestNow }
	s.engWorkers = 1 // sequential so sqlmock's ordered expectations hold
	return s, mock
}

func cpCampaignColumns() []string {
	return []string{"id", "name", "status", "scheduled_at", "created_at", "sending_domain", "transport"}
}

func cpEngagementColumns() []string {
	return []string{"campaign_id", "sent_pg", "sent_all", "opens", "clicks", "openers", "clickers", "action",
		"h_opens", "h_clicks", "h_openers", "h_clickers", "h_action"}
}

func cpGet(t *testing.T, s *CampaignPerformanceService, query string) (*httptest.ResponseRecorder, cpResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/mailing/campaign-performance?"+query, nil)
	req.Header.Set("X-Organization-ID", cpTestOrg)
	rec := httptest.NewRecorder()
	s.HandleGet(rec, req)
	var resp cpResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, resp
}

// cpStandardMocks: campaign 1 (pmta, scheduled 1 day ago) + campaign 2 (kumo,
// scheduled 10 days ago); one engagement row for campaign 1; one conversion.
// cpExpectEngagement mirrors loadEngagementOne: one READ ONLY tx per campaign,
// SET LOCAL statement_timeout, the grouped scan, rollback.
func cpExpectEngagement(mock sqlmock.Sqlmock, pattern string, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectExec(`SET LOCAL statement_timeout`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(pattern).WillReturnRows(rows)
	mock.ExpectRollback()
}

func cpStandardMocks(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows(cpCampaignColumns()).
			AddRow(cpTestCID1, "09112026 - DB - OFR-CLK", "sent", cpTestNow.Add(-24*time.Hour), cpTestNow.Add(-30*time.Hour), "em.discountblog.com", "pmta").
			AddRow(cpTestCID2, "09022026 - AAD - KUMO", "sent", cpTestNow.Add(-10*24*time.Hour), cpTestNow.Add(-11*24*time.Hour), "em.aadwd.com", "kumo"))
	cpExpectEngagement(mock, `FROM mailing_tracking_events`, sqlmock.NewRows(cpEngagementColumns()).
		AddRow(cpTestCID1, 1000, 1900, 400, 90, 300, 60, 45, 120, 30, 100, 20, 15))
	cpExpectEngagement(mock, `FROM mailing_tracking_events`, sqlmock.NewRows(cpEngagementColumns()))
	mock.ExpectQuery(`FROM mailing_everflow_conversions`).
		WillReturnRows(sqlmock.NewRows([]string{"campaign_id", "n", "payout"}).
			AddRow(cpTestCID1, 2, 45.5))
}

func cpRow(cid, eventType, source string, n int64) analytics.BreakdownRow {
	return analytics.BreakdownRow{Keys: map[string]string{"campaign_id": cid, "event_type": eventType, "source": source}, Count: n}
}

func cpFindCampaign(t *testing.T, resp cpResponse, id string) cpEntry {
	t.Helper()
	for _, c := range resp.Campaigns {
		if c.Campaign.ID == id {
			return c
		}
	}
	t.Fatalf("campaign %s missing from response", id)
	return cpEntry{}
}

func TestCampaignPerformance_ClickActionPredicateMirrorsCatalog(t *testing.T) {
	// agents/dbknowledge/_db.py PG_CLICK_ACTION, composed exactly as the f-string does.
	want := `(event_type = 'clicked' AND NOT (COALESCE(link_url,'') = '' OR link_url ~* 'unsub|optout|opt-out|preference|/privacy' OR link_url ~* '^everflow-import:' OR (link_url ~* '^https?://t\.em\.' AND link_url !~* '^https?://(t|trk)\.e?m\.[^/]+/o/')) AND NOT (link_url ~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)' OR link_url ~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)'))`
	if pgClickActionSQL != want {
		t.Fatalf("PG_CLICK_ACTION drifted from the catalog:\n got %s\nwant %s", pgClickActionSQL, want)
	}
	sql := campaignPerformanceEngagementSQL(false)
	for _, frag := range []string{
		"substring(id::text,15,1) = '4'",                                        // §2.1 read-around
		"ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address))", // §6 verdict
		"event_at >= $3 AND event_at < $4",                                      // §4 partition bound
		"campaign_id = ANY($2::uuid[])",
		"organization_id = $1",
		pgClickActionSQL,
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("engagement SQL missing %q", frag)
		}
	}
	if strings.Contains(sql, "is_machine_open") || strings.Contains(sql, "is_machine_click") {
		t.Errorf("engagement SQL must not filter on the inert machine labels (§12.1)")
	}
}

func TestCampaignPerformance_DerivedAttemptedBothSpellingsAndBases(t *testing.T) {
	s, mock := newCampaignPerformanceServiceWithMock(t)
	cpStandardMocks(mock)

	var captured []analytics.BreakdownFilter
	s.SetLakeBreakdown(func(_ context.Context, f analytics.BreakdownFilter) ([]analytics.BreakdownRow, error) {
		captured = append(captured, f)
		return []analytics.BreakdownRow{
			cpRow(cpTestCID1, "delivered", "pmta", 100),
			cpRow(cpTestCID1, "delivered", "ses", 50),
			cpRow(cpTestCID1, "hard_bounce", "pmta", 2),     // PMTA spelling
			cpRow(cpTestCID1, "hard_bounce", "ses", 5),      // SES 'bounced'+bounce_cat, reclassified by the reader
			cpRow(cpTestCID1, "soft_bounce", "kumo", 3),     // kumo spelling
			cpRow(cpTestCID1, "relayed_to_ses", "pmta", 40), // hop — never delivered
			cpRow(cpTestCID1, "reputation_block", "pmta", 7),
			cpRow(cpTestCID1, "complaint", "ses", 1),
			cpRow(cpTestCID2, "delivered", "kumo", 20),
			cpRow(cpTestCID2, "soft_bounce", "kumo", 4),
			cpRow(cpTestCID2, "delivery_delay", "kumo", 9),
		}, nil
	})

	rec, resp := cpGet(t, s, "ids="+cpTestCID1+","+cpTestCID2)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.ContractVersion != "METRIC_CONTRACT 2026-09-12" {
		t.Errorf("contract_version = %q", resp.ContractVersion)
	}
	if len(captured) != 1 {
		t.Fatalf("expected one lake breakdown call, got %d", len(captured))
	}
	f := captured[0]
	if strings.Join(f.SourceIn, ",") != "pmta,ses,kumo" {
		t.Errorf("SourceIn = %v, want pmta,ses,kumo (kumo is a first-class transport)", f.SourceIn)
	}
	if strings.Join(f.GroupBy, ",") != "campaign_id,event_type,source" {
		t.Errorf("GroupBy = %v — event_type is the reader's reclassified dimension that folds both bounce spellings", f.GroupBy)
	}
	if len(f.CampaignIDs) != 2 {
		t.Errorf("CampaignIDs = %v", f.CampaignIDs)
	}

	c1 := cpFindCampaign(t, resp, cpTestCID1)
	d, _ := c1.Delivery.(map[string]interface{})
	if d == nil {
		t.Fatalf("delivery not an object: %#v", c1.Delivery)
	}
	if d["delivered"].(float64) != 150 || d["hard"].(float64) != 7 || d["soft"].(float64) != 3 || d["complaint"].(float64) != 1 {
		t.Errorf("delivery fold wrong: %v", d)
	}
	if d["attempted_derived"].(float64) != 160 {
		t.Errorf("attempted_derived = %v, want 160 (delivered+hard+soft; relayed_to_ses and reputation_block excluded)", d["attempted_derived"])
	}
	excl := d["excluded"].(map[string]interface{})
	if excl["relayed_to_ses"].(float64) != 40 || excl["reputation_block"].(float64) != 7 {
		t.Errorf("excluded classes not disclosed: %v", excl)
	}
	if got := d["sources_seen"].([]interface{}); len(got) != 3 {
		t.Errorf("sources_seen = %v, want [kumo pmta ses]", got)
	}

	// Both engagement bases, side by side (§12).
	e := c1.Engagement
	if e.SentPG != 1000 || e.SentPGAllWriters != 1900 {
		t.Errorf("sent_pg = %d / all_writers = %d", e.SentPG, e.SentPGAllWriters)
	}
	if e.Inclusive.Opens != 400 || e.Inclusive.UniqueOpeners != 300 || e.Inclusive.UniqueClickers != 60 || e.Inclusive.ActionClicks != 45 {
		t.Errorf("inclusive base wrong: %+v", e.Inclusive)
	}
	if e.Human.Opens != 120 || e.Human.UniqueOpeners != 100 || e.Human.UniqueClickers != 20 || e.Human.ActionClicks != 15 {
		t.Errorf("human base wrong: %+v", e.Human)
	}
	if e.Human.Opens >= e.Inclusive.Opens {
		t.Errorf("human must be a subset of inclusive")
	}

	// Rates disclose numerator + denominator (§2).
	r := c1.Rates["open_rate"]
	if r.Value == nil || *r.Value != 2 || r.DenominatorName != "delivered" || r.Denominator == nil || *r.Denominator != 150 || r.NumeratorName != "inclusive.unique_openers" {
		t.Errorf("open_rate = %+v", r)
	}
	r = c1.Rates["hard_rate"]
	if r.Value == nil || *r.Value != 0.04375 || r.DenominatorName != "attempted_derived" || *r.Denominator != 160 {
		t.Errorf("hard_rate = %+v", r)
	}
	r = c1.Rates["ctor"]
	if r.Value == nil || *r.Value != 0.2 || r.DenominatorName != "inclusive.unique_openers" {
		t.Errorf("ctor = %+v", r)
	}
	if c1.Rates["human_click_rate"].DenominatorName != "delivered" {
		t.Errorf("human_click_rate must divide by delivered")
	}

	// Conversions: unjoined, campaign-attributed.
	if c1.Conversions == nil || c1.Conversions.Conversions != 2 || c1.Conversions.Payout != 45.5 || c1.Conversions.Counting != "unjoined" {
		t.Errorf("conversions = %+v", c1.Conversions)
	}

	// Maturity (§12.3): 1 day → immature, 10 days → mature.
	if !c1.Maturity.Immature || c1.Maturity.DaysSinceScheduled == nil || *c1.Maturity.DaysSinceScheduled != 1 {
		t.Errorf("campaign 1 maturity = %+v, want immature at 1.0 days", c1.Maturity)
	}
	c2 := cpFindCampaign(t, resp, cpTestCID2)
	if c2.Maturity.Immature || *c2.Maturity.DaysSinceScheduled != 10 {
		t.Errorf("campaign 2 maturity = %+v, want mature at 10 days", c2.Maturity)
	}

	// Kumo: delivered over terminal outcomes, delay excluded, note present.
	d2 := c2.Delivery.(map[string]interface{})
	if d2["attempted_derived"].(float64) != 24 || d2["excluded"].(map[string]interface{})["delivery_delay"].(float64) != 9 {
		t.Errorf("kumo delivery fold: %v", d2)
	}
	if c2.Campaign.Transport != "kumo" || !cpHasNote(c2.Notes, "kumo") {
		t.Errorf("kumo campaign must carry the no-sent/open/click note: %v", c2.Notes)
	}
	if c2.Engagement.Source != "pg" || c2.Engagement.Inclusive.Opens != 0 {
		t.Errorf("campaign with no PG rows should have a zero PG engagement block: %+v", c2.Engagement)
	}
	if c2.Conversions != nil {
		t.Errorf("no conversion rows → block omitted, got %+v", c2.Conversions)
	}
	if c1.Campaign.Brand != "discountblog.com" {
		t.Errorf("brand = %q, want apex root", c1.Campaign.Brand)
	}
	if resp.Definitions["engagement.sent_pg"] == "" || resp.Definitions["delivery.attempted_derived"] == "" {
		t.Errorf("definitions map incomplete: %v", resp.Definitions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func cpHasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

func TestCampaignPerformance_LakeDisabledFailsSoft(t *testing.T) {
	s, mock := newCampaignPerformanceServiceWithMock(t)
	cpStandardMocks(mock)
	s.SetLakeBreakdown(nil) // models analytics.ReaderEnabled() == false

	rec, resp := cpGet(t, s, "ids="+cpTestCID1)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail-soft must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	c1 := cpFindCampaign(t, resp, cpTestCID1)
	d, _ := c1.Delivery.(map[string]interface{})
	if d == nil || d["unavailable"] != "lake reader disabled" {
		t.Fatalf("delivery = %#v, want {unavailable: lake reader disabled}", c1.Delivery)
	}
	// PG half still served.
	if c1.Engagement.Inclusive.Opens != 400 || c1.Engagement.Human.Opens != 120 {
		t.Errorf("PG engagement must still be returned: %+v", c1.Engagement)
	}
	// Cross-source rates null WITH a reason; PG-over-PG CTOR still computed.
	for _, k := range []string{"open_rate", "click_rate", "delivery_rate", "hard_rate", "complaint_rate", "human_open_rate"} {
		r := c1.Rates[k]
		if r.Value != nil || !strings.Contains(r.Reason, "lake reader disabled") {
			t.Errorf("%s = %+v, want null value with the lake reason", k, r)
		}
	}
	if r := c1.Rates["ctor"]; r.Value == nil || *r.Value != 0.2 {
		t.Errorf("ctor should still compute from PG: %+v", r)
	}
	if !cpHasNote(resp.Notes, "lake reader disabled") {
		t.Errorf("top-level note missing: %v", resp.Notes)
	}
}

func TestCampaignPerformance_NameModeResolvesByOrgAndDenverRange(t *testing.T) {
	s, mock := newCampaignPerformanceServiceWithMock(t)
	mock.ExpectQuery(`(?s)FROM mailing_campaigns c.*c\.name ILIKE`).
		WithArgs(cpTestOrg, `OFR\_CLK 100\%`, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(cpCampaignColumns()).
			AddRow(cpTestCID1, "OFR_CLK 100%", "sent", cpTestNow.Add(-48*time.Hour), cpTestNow.Add(-50*time.Hour), "em.discountblog.com", "ses"))
	cpExpectEngagement(mock, `FROM mailing_tracking_events`, sqlmock.NewRows(cpEngagementColumns()))
	mock.ExpectQuery(`FROM mailing_everflow_conversions`).WillReturnRows(sqlmock.NewRows([]string{"campaign_id", "n", "payout"}))
	s.SetLakeBreakdown(func(context.Context, analytics.BreakdownFilter) ([]analytics.BreakdownRow, error) { return nil, nil })

	rec, resp := cpGet(t, s, "name=OFR_CLK+100%25&from=2026-09-08&to=2026-09-12")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if resp.Window.From != "2026-09-08" || resp.Window.To != "2026-09-12" || resp.Window.Timezone != "America/Denver" {
		t.Errorf("window = %+v", resp.Window)
	}
	if len(resp.Campaigns) != 1 {
		t.Fatalf("campaigns = %d", len(resp.Campaigns))
	}
	// A campaign with zero lake rows still gets a delivery object (zeros) and
	// zero-denominator rates carry a reason rather than dividing by zero.
	if r := resp.Campaigns[0].Rates["open_rate"]; r.Value != nil || r.Reason != "denominator is zero" {
		t.Errorf("open_rate on zero delivered = %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestCampaignPerformance_InputValidation(t *testing.T) {
	s, _ := newCampaignPerformanceServiceWithMock(t)
	cases := map[string]string{
		"neither":   "",
		"bad uuid":  "ids=not-a-uuid",
		"breakdown": "ids=" + cpTestCID1 + "&breakdown=brand",
	}
	for name, q := range cases {
		rec, _ := cpGet(t, s, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	// >50 ids rejected before any query.
	ids := make([]string, 0, maxCampaignPerformanceIDs+1)
	for i := 0; i <= maxCampaignPerformanceIDs; i++ {
		ids = append(ids, "11111111-1111-4111-8111-"+strings.Repeat("0", 10)+padCP(i))
	}
	rec, _ := cpGet(t, s, "ids="+strings.Join(ids, ","))
	if rec.Code != http.StatusBadRequest {
		t.Errorf(">50 ids: status %d, want 400", rec.Code)
	}
}

func padCP(i int) string {
	s := "00" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	return s[len(s)-2:]
}

func TestCampaignPerformance_ISPBreakdownJoinsOnLabel(t *testing.T) {
	s, mock := newCampaignPerformanceServiceWithMock(t)
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows(cpCampaignColumns()).
			AddRow(cpTestCID1, "x", "sent", cpTestNow.Add(-5*24*time.Hour), cpTestNow.Add(-6*24*time.Hour), "em.discountblog.com", "pmta"))
	cols := append([]string{"campaign_id", "recipient_domain"}, cpEngagementColumns()[1:]...)
	cpExpectEngagement(mock, `COALESCE\(recipient_domain,''\)`, sqlmock.NewRows(cols).
		AddRow(cpTestCID1, "gmail.com", 10, 10, 8, 2, 6, 2, 1, 3, 1, 3, 1, 1).
		AddRow(cpTestCID1, "googlemail.com", 5, 5, 2, 0, 2, 0, 0, 1, 0, 1, 0, 0).
		AddRow(cpTestCID1, "", 1, 1, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0))
	mock.ExpectQuery(`FROM mailing_everflow_conversions`).WillReturnRows(sqlmock.NewRows([]string{"campaign_id", "n", "payout"}))

	calls := 0
	s.SetLakeBreakdown(func(_ context.Context, f analytics.BreakdownFilter) ([]analytics.BreakdownRow, error) {
		calls++
		if len(f.GroupBy) == 3 && f.GroupBy[1] == "isp" {
			return []analytics.BreakdownRow{
				{Keys: map[string]string{"campaign_id": cpTestCID1, "isp": "gmail", "event_type": "delivered"}, Count: 20},
				{Keys: map[string]string{"campaign_id": cpTestCID1, "isp": "gmail", "event_type": "hard_bounce"}, Count: 1},
				{Keys: map[string]string{"campaign_id": cpTestCID1, "isp": "yahoo", "event_type": "delivered"}, Count: 4},
			}, nil
		}
		return []analytics.BreakdownRow{cpRow(cpTestCID1, "delivered", "ses", 24), cpRow(cpTestCID1, "hard_bounce", "ses", 1)}, nil
	})

	rec, resp := cpGet(t, s, "ids="+cpTestCID1+"&breakdown=isp")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Errorf("expected totals + isp lake passes, got %d", calls)
	}
	c1 := resp.Campaigns[0]
	// Totals still fold across recipient_domain buckets.
	if c1.Engagement.Inclusive.Opens != 11 || c1.Engagement.Inclusive.UniqueOpeners != 9 || c1.Engagement.SentPG != 16 {
		t.Errorf("campaign total engagement = %+v", c1.Engagement.Inclusive)
	}
	byISP := map[string]cpISPRow{}
	for _, r := range c1.ISP {
		byISP[r.ISP] = r
	}
	g := byISP["gmail"]
	if g.Engagement.Inclusive.Opens != 10 || g.Engagement.Inclusive.UniqueOpeners != 8 {
		t.Errorf("gmail.com + googlemail.com must fold into gmail: %+v", g.Engagement.Inclusive)
	}
	if r := g.Rates["open_rate"]; r.Value == nil || *r.Value != 0.4 || *r.Denominator != 20 {
		t.Errorf("gmail open_rate = %+v (8 openers / 20 delivered)", r)
	}
	if r := g.Rates["hard_rate"]; r.Value == nil || *r.Denominator != 21 {
		t.Errorf("gmail hard_rate denominator = %+v, want attempted_derived 21", r)
	}
	if y, ok := byISP["yahoo"]; !ok || y.Rates["open_rate"].Value == nil || *y.Rates["open_rate"].Value != 0 {
		t.Errorf("yahoo (lake only) should still get a row with 0/4: %+v", byISP["yahoo"])
	}
	u, ok := byISP[campaignPerformanceISPUnresolved]
	if !ok {
		t.Fatalf("NULL recipient_domain must surface as %s, not fold into other: %v", campaignPerformanceISPUnresolved, byISP)
	}
	if r := u.Rates["open_rate"]; r.Value != nil || !strings.Contains(r.Reason, "no lake delivery rows") {
		t.Errorf("unresolved row must not divide across scopes: %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

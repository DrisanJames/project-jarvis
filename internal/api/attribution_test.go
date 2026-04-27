package api

// Tests for the attribution matcher: CSV parsing, match logic with sqlmock,
// and the bot-scanner / fallback bucket categorization. The HTTP handler is
// covered separately in attribution_handler_test.go.

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- CSV parsers ----------

func TestParseEverflowClicksCSV_HappyPath(t *testing.T) {
	body := `Date,Offer,Offer URL,Error Code,Error Message,Unique,Revenue,Revenue Type,Referrer,Source ID,Sub1,Sub2,Sub3,Sub4,Sub5,Coupon Code,IP Address,Transaction ID,Browser,Brand,OS Version,Model,Platform,Language,Device Type,City,Country,Country Code,DMA,ISP,Mobile Connection,Region,IDFA,IDFA MD5,IDFA SHA1,Google Ad ID,Google Ad ID MD5,Google Ad ID SHA1,Android ID,Android ID MD5,Android ID SHA1
04/27/2026 00:02:07 EDT,TruGreen $9.95 - Sensitive IO Req,,0,N/A,Yes,$0.00,CPA,,email,,,,,,,169.197.59.74,aa07fd04269144ada400445357f8c527,Safari,Apple,10.15,,macOS,en,PC,Encinitas,United States,US,0,Ting Fiber Inc.,No,California,,,,,,,,,
`
	rows, err := ParseEverflowClicksCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 1)

	r := rows[0]
	assert.Equal(t, "169.197.59.74", r.IPAddress)
	assert.Equal(t, "aa07fd04269144ada400445357f8c527", r.TransactionID)
	assert.Equal(t, "Ting Fiber Inc.", r.ISP)
	assert.Equal(t, "Safari", r.Browser)

	// Timestamp: 04/27/2026 00:02:07 EDT == 04/27/2026 04:02:07 UTC
	expected := time.Date(2026, 4, 27, 4, 2, 7, 0, time.UTC)
	assert.True(t, r.Timestamp.Equal(expected),
		"expected %s, got %s", expected, r.Timestamp)
}

func TestParseEverflowClicksCSV_RejectsMissingHeader(t *testing.T) {
	body := "foo,bar\n1,2\n"
	_, err := ParseEverflowClicksCSV(strings.NewReader(body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required column")
}

func TestParseEverflowConversionsCSV_HappyPath(t *testing.T) {
	body := `conversion_id,transaction_id,date,click_date,delta_hours,offer_id,offer_name,offer_url_id,creative_id,event_id,event_name,revenue,revenue_type,source_id,sub1,sub2,sub3,sub4,sub5,session_user_ip,conversion_user_ip,http_user_agent,country,country_code,region,city,dma,carrier,platform,os_version,device_type,brand,browser,language,isp,referer,coupon_code,app_id,idfa,idfa_md5,idfa_sha1,google_ad_id,google_ad_id_md5,google_ad_id_sha1,android_id,android_id_md5,android_id_sha1,currency_id,is_view_through,order_id,adv1,adv2,adv3,adv4,adv5,sale_amount,network_id
68a0e1332ab241b1a514980f58a79039,ba0b69ac3773415893120dbb6ba71299,2026-04-24 07:57:00,2026-04-23 21:57:46,9.99,5620,TruGreen $9.95,0,0,0,,50.00,CPA,email,,,,,,2607:fb91:15ad:d398:ad2:1c57:50de:2d7,52.38.76.248,"Mozilla/5.0",United States,US,Washington,Seattle,1561,T-Mobile USA,Android,,Mobile,Unknown,Edge,en,t-mobile usa inc.,,,,,,,,,,,,,USD,0,,8687325c-1901-4411-b9f1-e5926f12adab,,,,,.00,1405
`
	rows, err := ParseEverflowConversionsCSV(strings.NewReader(body))
	require.NoError(t, err)
	require.Len(t, rows, 1)

	r := rows[0]
	assert.Equal(t, "68a0e1332ab241b1a514980f58a79039", r.ConversionID)
	assert.Equal(t, "ba0b69ac3773415893120dbb6ba71299", r.TransactionID)
	assert.Equal(t, "2607:fb91:15ad:d398:ad2:1c57:50de:2d7", r.SessionUserIP)
	assert.Equal(t, "52.38.76.248", r.ConversionUserIP)
	assert.InDelta(t, 50.0, r.Revenue, 0.001)

	// click_date 2026-04-23 21:57:46 EDT (Apr 23 was DST -> EDT) -> UTC: 2026-04-24 01:57:46
	expected := time.Date(2026, 4, 24, 1, 57, 46, 0, time.UTC)
	assert.True(t, r.ClickTime.Equal(expected),
		"expected %s, got %s", expected, r.ClickTime)
}

// ---------- Bot scanner detection ----------

func TestIsBotScannerIP(t *testing.T) {
	cases := map[string]bool{
		"135.232.20.64":  true,  // Microsoft Boydton
		"40.107.0.5":     true,  // Outlook
		"66.249.66.1":    true,  // Google prefetch
		"169.197.59.74":  false, // Ting Fiber subscriber
		"68.57.56.68":    false, // Comcast subscriber
		"":               false,
		"not-an-ip":      false,
	}
	for ip, want := range cases {
		assert.Equal(t, want, IsBotScannerIP(ip), "ip=%s", ip)
	}
}

// ---------- Match logic with sqlmock ----------

// trackingEventCols is the projection used by lookupClickEvent. Kept in
// sync by hand because go-sqlmock doesn't introspect the SELECT clause.
var trackingEventCols = []string{
	"event_id", "subscriber_id", "campaign_id", "campaign_name", "link_url", "event_at",
}

var subscriberCols = []string{
	"id", "email", "first_name", "last_name", "status", "created_at", "last_engaged_at",
}

func TestMatchAttribution_TightWindowMatchesClick(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	clickTS := time.Date(2026, 4, 27, 4, 2, 7, 0, time.UTC)
	subID := "11111111-2222-3333-4444-555555555555"
	camID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	eventAt := clickTS.Add(3 * time.Second) // inside the tight window

	// Tight-window query: regex anchors on FROM mailing_tracking_events plus
	// the event_at BETWEEN clause so we can be sure we're not accidentally
	// matching a different SELECT.
	mock.ExpectQuery(`FROM mailing_tracking_events[\s\S]*event_at BETWEEN`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols).
			AddRow("evt-1", subID, camID, "TruGreen Daily", "https://5620.example/?aff=tracker", eventAt))

	// Subscriber lookup follows.
	mock.ExpectQuery(`FROM mailing_subscribers`).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows(subscriberCols).
			AddRow(subID, "user@example.com", "Pat", "Doe", "confirmed", clickTS.Add(-30*24*time.Hour), eventAt))

	clicks := []ClickRow{{
		RowIndex:  1,
		Timestamp: clickTS,
		IPAddress: "169.197.59.74",
		OfferName: "TruGreen $9.95",
	}}

	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{})
	require.NoError(t, err)
	require.Len(t, res.MatchedClicks, 1)
	assert.Empty(t, res.UnmatchedClicks)

	m := res.MatchedClicks[0]
	assert.Equal(t, "tight", m.ConfidenceTier)
	assert.Equal(t, "user@example.com", m.Subscriber.Email)
	assert.Equal(t, subID, m.Subscriber.SubscriberID)
	assert.Equal(t, int64(3), m.OffsetSeconds)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMatchAttribution_BotScannerIPGetsCategorized(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// Both tight and fallback windows return no rows.
	mock.ExpectQuery(`FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols))
	mock.ExpectQuery(`FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols))

	clicks := []ClickRow{{
		RowIndex:  1,
		Timestamp: time.Date(2026, 4, 27, 5, 30, 0, 0, time.UTC),
		IPAddress: "135.232.20.64", // Microsoft Boydton
		OfferName: "TruGreen $9.95",
	}}

	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{})
	require.NoError(t, err)
	require.Empty(t, res.MatchedClicks)
	require.Len(t, res.UnmatchedClicks, 1)

	u := res.UnmatchedClicks[0]
	assert.Equal(t, ReasonBotScannerIP, u.Reason)
	assert.Equal(t, 1, res.UnmatchedReasons[ReasonBotScannerIP])

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMatchAttribution_FallbackTierWhenTightMisses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	clickTS := time.Date(2026, 4, 27, 4, 2, 7, 0, time.UTC)
	subID := "11111111-2222-3333-4444-555555555555"
	camID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	eventAt := clickTS.Add(15 * time.Minute) // outside tight 2min window

	// Tight: empty.
	mock.ExpectQuery(`FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols))
	// Fallback: hit.
	mock.ExpectQuery(`FROM mailing_tracking_events`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols).
			AddRow("evt-1", subID, camID, "TruGreen Daily", "https://5620.example/", eventAt))
	// Subscriber lookup.
	mock.ExpectQuery(`FROM mailing_subscribers`).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows(subscriberCols).
			AddRow(subID, "user@example.com", "", "", "confirmed", clickTS, eventAt))

	clicks := []ClickRow{{
		RowIndex:  1,
		Timestamp: clickTS,
		IPAddress: "169.197.59.74",
	}}

	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{})
	require.NoError(t, err)
	require.Len(t, res.MatchedClicks, 1)
	assert.Equal(t, "fallback", res.MatchedClicks[0].ConfidenceTier)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMatchAttribution_OrgScopeFlowsThroughBothQueries(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	orgID := "11111111-2222-3333-4444-555555555555"
	subID := "22222222-3333-4444-5555-666666666666"
	clickTS := time.Date(2026, 4, 27, 4, 2, 7, 0, time.UTC)

	// Tight click query must include the e.organization_id filter.
	mock.ExpectQuery(`FROM mailing_tracking_events[\s\S]*organization_id`).
		WillReturnRows(sqlmock.NewRows(trackingEventCols).
			AddRow("evt-1", subID, "cam-1", "TruGreen", "https://5620.example/", clickTS))
	// Subscriber query must also be org-scoped.
	mock.ExpectQuery(`FROM mailing_subscribers[\s\S]*organization_id`).
		WillReturnRows(sqlmock.NewRows(subscriberCols).
			AddRow(subID, "u@example.com", "", "", "confirmed", clickTS, clickTS))

	clicks := []ClickRow{{
		RowIndex: 1, Timestamp: clickTS, IPAddress: "169.197.59.74",
	}}

	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{
		OrgID: orgID,
	})
	require.NoError(t, err)
	require.Len(t, res.MatchedClicks, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMatchAttribution_HandlesNeitherClicksNorConversions(t *testing.T) {
	// No DB calls expected when both inputs are empty.
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	res, err := MatchAttribution(context.Background(), db, nil, nil, AttributionOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalClicks)
	assert.Equal(t, 0, res.TotalConversions)
	assert.Empty(t, res.MatchedClicks)
	assert.Empty(t, res.MatchedConversions)
}

func TestMatchAttribution_InvalidIPCategorized(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// No queries should run for the invalid-IP row, so we register no
	// expectations. If lookupClickEvent is called sqlmock will error.

	clicks := []ClickRow{{
		RowIndex:  1,
		Timestamp: time.Now(),
		IPAddress: "not-an-ip",
	}}
	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{})
	require.NoError(t, err)
	require.Len(t, res.UnmatchedClicks, 1)
	assert.Equal(t, ReasonInvalidIP, res.UnmatchedClicks[0].Reason)
}

func TestMatchAttribution_DBErrorFlowsThroughAsReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_tracking_events`).
		WillReturnError(sql.ErrConnDone)

	clicks := []ClickRow{{
		RowIndex:  1,
		Timestamp: time.Now(),
		IPAddress: "169.197.59.74",
	}}
	res, err := MatchAttribution(context.Background(), db, clicks, nil, AttributionOptions{})
	require.NoError(t, err) // matcher is fault-tolerant, surfaces per-row
	require.Len(t, res.UnmatchedClicks, 1)
	assert.Equal(t, ReasonDBError, res.UnmatchedClicks[0].Reason)
}

// Quick smoke that the on-disk CSV bytes round-trip through the parser
// without panicking. Catches regression where a future Everflow column
// reorder breaks header lookup.
func TestParseEverflowClicksCSV_TolerantOfLazyQuotes(t *testing.T) {
	body := `Date,Offer,IP Address,Transaction ID,Browser,Country,ISP,Error Code,Error Message
04/27/2026 12:00:00 EDT,"TruGreen ""Special"" $9.95",1.2.3.4,abc,Chrome,US,Comcast,0,
`
	rows, err := ParseEverflowClicksCSV(bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "1.2.3.4", rows[0].IPAddress)
	assert.Contains(t, rows[0].OfferName, "Special")
}

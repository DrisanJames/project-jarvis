package worker

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// clickDripDisclosureHarness drives one JourneyClickDripSender.Send through
// the full sqlmock sequence with the profile's raw_creative flag set as
// given, and returns the message captured at the ESP layer.
func clickDripDisclosureHarness(t *testing.T, rawCreative bool) *EmailMessage {
	t.Helper()
	t.Setenv("DISABLE_BRAND_IMAGE_HOST_SWAP", "1")

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const (
		profileID = "44444444-4444-4444-4444-444444444444"
		subID     = "55555555-5555-5555-5555-555555555555"
		orgID     = "00000000-0000-0000-0000-000000000001"
		offerID   = "9539"
	)

	cap := &captureESPSender{}
	ps := &ProfileBasedSender{
		db:          db,
		senderCache: map[string]ESPSender{profileID + ":pmta-api": cap},
	}
	s := NewJourneyClickDripSender(db, ps, "https://t.global.example", "test-secret")

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT organization_id::text FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(orgID))

	campaignID := shadowCampaignID(offerID, "", "")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(campaignID))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_sending_profiles WHERE id=$1`)).
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"tracking_domain", "sending_domain"}).
			AddRow("t.em.discountblog.com", "em.discountblog.com"))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(raw_creative, FALSE) FROM mailing_sending_profiles WHERE id=$1`)).
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"raw_creative"}).AddRow(rawCreative))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT vendor_type,`)).
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{
			"vendor_type", "api_key", "api_secret", "sending_domain", "api_endpoint",
			"smtp_host", "smtp_port", "smtp_username", "smtp_password",
			"pool_prefix", "ip_pool", "routing_mode",
		}).AddRow("pmta", "", "", "em.discountblog.com", "http://127.0.0.1:19099",
			nil, nil, nil, nil, "db", "", ""))

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_message_log`)).
		WillReturnResult(sqlmock.NewResult(1, 1))

	sendErr := s.Send(context.Background(), ClickDripSendParams{
		JourneyID:       "click-drip-4touch-72h",
		EverflowOfferID: offerID,
		ReminderSeq:     0,
		SubscriberID:    subID,
		SubscriberEmail: "human@outlook.com",
		Subject:         "Your offer is waiting",
		FromName:        "Diane",
		FromEmail:       "deals@em.discountblog.com",
		ProfileID:       profileID,
		HTMLContent:     `<html><body><p>Big savings today.</p></body></html>`,
	})
	require.NoError(t, sendErr)
	require.NotNil(t, cap.msg, "ESP sender was never invoked")
	require.NoError(t, mock.ExpectationsWereMet())
	return cap.msg
}

// TestClickDripSend_PartnerDisclosureInjected pins the 2026-08-07 systematic
// disclosure: every journey touch ships the sender-ID header and the
// intended-for + unsubscribe footer, rendered CONCRETE (the 2026-08-04
// incident shipped literal '{{ brand.name }}' on exactly this path).
func TestClickDripSend_PartnerDisclosureInjected(t *testing.T) {
	msg := clickDripDisclosureHarness(t, false)

	require.Contains(t, msg.HTMLContent, partnerDisclosureMarker)
	require.Contains(t, msg.HTMLContent, "Partner offer sent by")
	require.Contains(t, msg.HTMLContent, "Discount Blog</span>. You subscribed to Discount Blog partner promotions.",
		"brand label must be the rendered display name from brand.Label, never a token")
	require.Contains(t, msg.HTMLContent, intendedForMarker)
	require.Contains(t, msg.HTMLContent, "This email was intended for human@outlook.com,")
	require.Contains(t, msg.HTMLContent, "/track/unsubscribe/")
	require.NotContains(t, msg.HTMLContent, "{{", "no raw tokens may survive to the ESP layer")

	// Text part is generated from the FINAL html, so it inherits both lines.
	require.Contains(t, msg.TextContent, "Partner offer sent by")
	require.Contains(t, msg.TextContent, "This email was intended for human@outlook.com,")
}

// TestClickDripSend_RawCreativeProfileSkipsDisclosure is the negative-path
// guard for the first-party exemption (em.wcl-heloc.com): a raw_creative
// profile must ship the creative without either injected block.
func TestClickDripSend_RawCreativeProfileSkipsDisclosure(t *testing.T) {
	msg := clickDripDisclosureHarness(t, true)

	require.NotContains(t, msg.HTMLContent, partnerDisclosureMarker,
		"raw_creative profile must not receive the disclosure header")
	require.NotContains(t, msg.HTMLContent, intendedForMarker,
		"raw_creative profile must not receive the intended-for footer")
	require.NotContains(t, msg.HTMLContent, "Partner offer sent by")
}

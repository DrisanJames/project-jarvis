package worker

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// captureESPSender is a fake ESPSender that records the message it was asked
// to deliver, standing in for the PMTA API sender via ProfileBasedSender's
// senderCache seam.
type captureESPSender struct {
	msg *EmailMessage
}

func (c *captureESPSender) Send(_ context.Context, msg *EmailMessage) (*SendResult, error) {
	c.msg = msg
	return &SendResult{Success: true, MessageID: "cap-1"}, nil
}

// TestClickDripSend_IncludesPlainTextAlternative is the regression guard for
// the 2026-07-22 gap: JourneyClickDripSender built its EmailMessage with
// HTMLContent only, so every journey touch shipped multipart/alternative with
// a single HTML part (the ESP builders skip the text part when
// msg.TextContent == ""). The message handed to the ESP layer must now carry
// a non-empty text/plain alternative, generated from the FINAL html (after
// merge-tag and tracking rewrites) so it contains no raw {{ }} tokens.
func TestClickDripSend_IncludesPlainTextAlternative(t *testing.T) {
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

	// 1) resolveOrgID
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT organization_id::text FROM mailing_subscribers WHERE id=$1`)).
		WithArgs(subID).
		WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(orgID))

	// 2) ensureShadowCampaign fast path — campaign already exists.
	campaignID := shadowCampaignID(offerID, "", "")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(campaignID))

	// 3) resolveTrackingURL — profile carries a tracking domain.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_sending_profiles WHERE id=$1`)).
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{"tracking_domain", "sending_domain"}).
			AddRow("t.em.discountblog.com", "em.discountblog.com"))

	// 4) ProfileBasedSender profile lookup — pmta vendor with an API endpoint
	//    so routing lands on the pre-seeded ":pmta-api" cache entry.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT vendor_type,`)).
		WithArgs(profileID).
		WillReturnRows(sqlmock.NewRows([]string{
			"vendor_type", "api_key", "api_secret", "sending_domain", "api_endpoint",
			"smtp_host", "smtp_port", "smtp_username", "smtp_password",
			"pool_prefix", "ip_pool", "routing_mode",
		}).AddRow("pmta", "", "", "em.discountblog.com", "http://127.0.0.1:19099",
			nil, nil, nil, nil, "db", "", ""))

	// 5) writeMessageLog
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
		HTMLContent: `<html><body><p>Big savings today.</p>` +
			`<a href="https://track.cratoolpro.com/x?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}">Claim now</a>` +
			`</body></html>`,
	})
	require.NoError(t, sendErr)
	require.NotNil(t, cap.msg, "ESP sender was never invoked")

	require.NotEmpty(t, strings.TrimSpace(cap.msg.TextContent),
		"journey touch must carry a text/plain alternative — HTML-only multipart is the bug this test pins")
	require.Contains(t, cap.msg.TextContent, "Big savings today.",
		"text part must be derived from the creative body")
	require.NotContains(t, cap.msg.TextContent, "{{",
		"text part must be generated AFTER merge-tag/tracking rewrites — no raw tokens")

	require.NoError(t, mock.ExpectationsWereMet())
}

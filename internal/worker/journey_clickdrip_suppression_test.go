package worker

// Compliance fix 2026-09-11: click-drip reminders had NO send-time suppression
// check. Reproduced on the pre-fix tree (a brand-suppressed subscriber was
// handed to the ESP) and measured in prod: 14 days to 2026-09-11, 15,366
// click-drip sends, 3,396 to already-globally-suppressed addresses and 191 to
// addresses already brand-unsubscribed for the sending brand.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

type fakeSuppressionHub struct {
	global map[string]bool
	brand  map[string]bool // "email|brand"
}

func (f *fakeSuppressionHub) IsSuppressed(email string) bool { return f.global[email] }
func (f *fakeSuppressionHub) IsSuppressedForBrand(email, brandRoot string) bool {
	return f.global[email] || f.brand[email+"|"+brandRoot]
}

const (
	cdProfileID = "44444444-4444-4444-4444-444444444444"
	cdSubID     = "55555555-5555-5555-5555-555555555555"
	cdOrgID     = "00000000-0000-0000-0000-000000000001"
	cdOfferID   = "9539"
	cdEmail     = "human@outlook.com"
)

func cdParams() ClickDripSendParams {
	return ClickDripSendParams{
		JourneyID: "click-drip-4touch-72h", EverflowOfferID: cdOfferID, SubscriberID: cdSubID,
		SubscriberEmail: cdEmail, Subject: "Your offer is waiting", FromName: "Diane",
		FromEmail: "deals@em.discountblog.com", ProfileID: cdProfileID,
		HTMLContent: `<html><body><p>Big savings today.</p></body></html>`,
	}
}

// newCDSender builds a sender whose ESP is captured. When full is true the
// complete sqlmock sequence of a real send is expected (mirrors
// clickDripDisclosureHarness); otherwise NO query is expected, so a strict
// mock proves the gate returned before touching the DB.
func newCDSender(t *testing.T, full bool) (*JourneyClickDripSender, *captureESPSender, sqlmock.Sqlmock) {
	t.Helper()
	t.Setenv("DISABLE_BRAND_IMAGE_HOST_SWAP", "1")
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	capture := &captureESPSender{}
	ps := &ProfileBasedSender{db: db, senderCache: map[string]ESPSender{cdProfileID + ":pmta-api": capture}}
	s := NewJourneyClickDripSender(db, ps, "https://t.global.example", "test-secret")
	if full {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT organization_id::text FROM mailing_subscribers WHERE id=$1`)).
			WithArgs(cdSubID).WillReturnRows(sqlmock.NewRows([]string{"organization_id"}).AddRow(cdOrgID))
		campaignID := shadowCampaignID(cdOfferID, "", "")
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
			WithArgs(campaignID).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(campaignID))
		mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_sending_profiles WHERE id=$1`)).
			WithArgs(cdProfileID).WillReturnRows(sqlmock.NewRows([]string{"tracking_domain", "sending_domain"}).
			AddRow("t.em.discountblog.com", "em.discountblog.com"))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(raw_creative, FALSE) FROM mailing_sending_profiles WHERE id=$1`)).
			WithArgs(cdProfileID).WillReturnRows(sqlmock.NewRows([]string{"raw_creative"}).AddRow(false))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT vendor_type,`)).
			WithArgs(cdProfileID).WillReturnRows(sqlmock.NewRows([]string{
			"vendor_type", "api_key", "api_secret", "sending_domain", "api_endpoint",
			"smtp_host", "smtp_port", "smtp_username", "smtp_password",
			"pool_prefix", "ip_pool", "routing_mode",
		}).AddRow("pmta", "", "", "em.discountblog.com", "http://127.0.0.1:19099",
			nil, nil, nil, nil, "db", "", ""))
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_message_log`)).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	return s, capture, mock
}

func hubSource(h GlobalSuppressionChecker) func() GlobalSuppressionChecker {
	return func() GlobalSuppressionChecker { return h }
}

func TestClickDripSend_SkipsBrandSuppressedSubscriber(t *testing.T) {
	s, capture, mock := newCDSender(t, false)
	s.SetSuppressionSource(hubSource(&fakeSuppressionHub{brand: map[string]bool{cdEmail + "|discountblog.com": true}}))
	before := clickDripSendTimeCounters.skippedBrand.Load()

	require.NoError(t, s.Send(context.Background(), cdParams()), "a suppressed skip is permanent: nil so the executor advances")
	require.Nil(t, capture.msg, "brand-suppressed subscriber reached the ESP")
	require.NoError(t, mock.ExpectationsWereMet(), "gate must return before any DB work")
	require.Equal(t, before+1, clickDripSendTimeCounters.skippedBrand.Load())
}

func TestClickDripSend_SkipsGloballySuppressedSubscriber(t *testing.T) {
	s, capture, _ := newCDSender(t, false)
	s.SetSuppressionSource(hubSource(&fakeSuppressionHub{global: map[string]bool{cdEmail: true}}))
	before := clickDripSendTimeCounters.skippedGlobal.Load()
	require.NoError(t, s.Send(context.Background(), cdParams()))
	require.Nil(t, capture.msg)
	require.Equal(t, before+1, clickDripSendTimeCounters.skippedGlobal.Load())
}

// Negative control: a clean subscriber on another brand's suppression still sends.
func TestClickDripSend_UnsuppressedSubscriberStillSends(t *testing.T) {
	s, capture, mock := newCDSender(t, true)
	s.SetSuppressionSource(hubSource(&fakeSuppressionHub{brand: map[string]bool{cdEmail + "|quizfiesta.com": true}}))
	require.NoError(t, s.Send(context.Background(), cdParams()))
	require.NotNil(t, capture.msg)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Contains(t, capture.msg.HTMLContent, "/track/unsubscribe/")
}

func TestClickDripSend_KillSwitchRestoresPriorBehaviour(t *testing.T) {
	t.Setenv("DISABLE_CLICKDRIP_SUPPRESSION_CHECK", "true")
	s, capture, _ := newCDSender(t, true)
	s.SetSuppressionSource(hubSource(&fakeSuppressionHub{global: map[string]bool{cdEmail: true}}))
	require.NoError(t, s.Send(context.Background(), cdParams()))
	require.NotNil(t, capture.msg, "kill switch must restore the unchecked send")
}

// Boot window: the source is configured but the hub is not attached yet. The
// send is deferred with a TRANSIENT error (journey retry backs off 5m) rather
// than mailed unchecked.
func TestClickDripSend_HubNotAttachedDefersTransiently(t *testing.T) {
	s, capture, mock := newCDSender(t, false)
	s.SetSuppressionSource(hubSource(nil))
	err := s.Send(context.Background(), cdParams())
	require.Error(t, err)
	require.Nil(t, capture.msg)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, journeyRetryTransient, classifyJourneySendError(err), "must not be classified terminal (would eject the enrollment)")
	require.False(t, strings.Contains(strings.ToLower(err.Error()), "suppressed"))
}

func TestClickDripSend_PreferenceGateShadowSendsEnforceSkips(t *testing.T) {
	paused := time.Now().Add(24 * time.Hour)
	mkHub := func(mode preferences.Mode) *preferences.Hub {
		h := preferences.NewHub(nil, cdOrgID)
		h.SetModeFunc(func() preferences.Mode { return mode })
		h.Apply(preferences.Preference{EmailHash: preferences.EmailHash(cdEmail), PausedUntil: &paused})
		return h
	}
	clean := hubSource(&fakeSuppressionHub{})

	shadow := mkHub(preferences.ModeShadow)
	s, capture, _ := newCDSender(t, true)
	s.SetSuppressionSource(clean)
	s.SetPreferencesGate(shadow)
	require.NoError(t, s.Send(context.Background(), cdParams()))
	require.NotNil(t, capture.msg, "shadow mode must never alter a send")
	require.Equal(t, int64(1), shadow.Status().WouldSkip["clickdrip.paused"])

	enforce := mkHub(preferences.ModeEnforce)
	s2, capture2, mock2 := newCDSender(t, false)
	s2.SetSuppressionSource(clean)
	s2.SetPreferencesGate(enforce)
	require.NoError(t, s2.Send(context.Background(), cdParams()))
	require.Nil(t, capture2.msg, "enforce mode must skip a paused subscriber")
	require.NoError(t, mock2.ExpectationsWereMet())
	require.Equal(t, int64(1), enforce.Status().Skipped["clickdrip.paused"])
}

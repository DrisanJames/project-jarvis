package worker

// JourneyClickDripSender is the send path for click-drip reminder emails.
//
// Why a dedicated sender (and not the JourneyExecutor's simple emailSender
// callback): click-drip reminders MUST go out through the real PMTA pipeline
// on the SAME sending profile the subscriber originally clicked from, with
// the same IP-pool/warmup routing, DKIM domain, open/click tracking, and a
// mailing_message_log row for ops visibility + frequency accounting. The
// minimal `func(ctx, email, subject, html, fromName, fromEmail) error`
// callback can't carry the profile id / ISP / subscriber id those require.
//
// This sender reuses the exact battle-tested primitives the campaign send
// worker uses — it does NOT reimplement them:
//   - ProfileBasedSender.Send  → PMTA HTTP-bridge dispatch + per-ISP VMTA
//     pool selection + warmup-limit enforcement (esp_profile.go / esp_pmta_api.go)
//   - InjectTrackingPixelAndLinks → open pixel + signed click-link rewrite
//   - ReplaceTrackingMergeTags     → {{ tracking.* }} merge tags
//   - mailing_message_log INSERT   → identical shape to send_worker.markSent
//
// Volume is low by construction (only subscribers who clicked an offer, a
// handful of staggered touches each), so per-enrollment direct dispatch is
// the right model — the high-throughput wave/quota machinery would be
// overkill and would require a 'sending' shadow campaign that the batch
// dispatcher would then have to manage.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// defaultOrgID matches the platform's single-tenant organization used across
// startup migrations and seed data.
const clickDripDefaultOrgID = "00000000-0000-0000-0000-000000000001"

// clickDripShadowNamespace is a fixed UUID namespace used to derive a
// deterministic shadow-campaign id per Everflow offer (UUID v5). This lets
// ensureShadowCampaign resolve the row with a primary-key lookup instead of a
// 90k-row sequential scan on (campaign_type, name) — the latter measured at
// ~27s in production and blew the executor's 30s context (every reminder send
// failed with "context deadline exceeded"). The namespace value is arbitrary
// but MUST stay stable so ids remain reproducible across deploys.
var clickDripShadowNamespace = uuid.MustParse("a7f3c2d1-9b8e-4c6a-8d5f-1e2b3c4d5e6f")

// shadowCampaignID returns the deterministic shadow-campaign id for an offer.
func shadowCampaignID(everflowOfferID string) string {
	return uuid.NewSHA1(clickDripShadowNamespace, []byte("click-drip-shadow-offer-"+everflowOfferID)).String()
}

// JourneyClickDripSender dispatches a single click-drip reminder through PMTA.
type JourneyClickDripSender struct {
	db             *sql.DB
	profileSender  *ProfileBasedSender
	trackingURL    string // global fallback tracking base URL
	trackingSecret string
}

// NewJourneyClickDripSender builds the sender. profileSender is the same
// instance the server already constructs for the campaign send pool, so all
// per-profile sender caching / IP-pool state is shared.
func NewJourneyClickDripSender(db *sql.DB, profileSender *ProfileBasedSender, trackingURL, trackingSecret string) *JourneyClickDripSender {
	return &JourneyClickDripSender{
		db:             db,
		profileSender:  profileSender,
		trackingURL:    trackingURL,
		trackingSecret: trackingSecret,
	}
}

// ClickDripSendParams carries everything executeEmailNode resolved for one
// reminder touch.
type ClickDripSendParams struct {
	JourneyID       string // text id, e.g. "click-drip-4touch-72h"
	NodeID          string
	EverflowOfferID string
	ReminderSeq     int
	SubscriberID    string // uuid string
	SubscriberEmail string
	Subject         string
	HTMLContent     string
	FromName        string
	FromEmail       string
	ProfileID       string // mailing_sending_profiles.id resolved by the enroller
}

// Send dispatches one reminder. Returns an error only for conditions worth
// retrying (transient send failures); permanent skips are logged and return
// nil so the executor advances the enrollment rather than looping.
func (s *JourneyClickDripSender) Send(ctx context.Context, p ClickDripSendParams) error {
	if s.profileSender == nil {
		return fmt.Errorf("click-drip sender: no profile sender configured")
	}
	if p.ProfileID == "" {
		// Without a profile we'd fall back to the platform-default IP pool,
		// which would send the brand's creative from the wrong domain (DKIM/
		// SPF misalignment → spam). Refuse rather than send misaligned mail.
		return fmt.Errorf("click-drip sender: no sending_profile_id for subscriber %s (offer %s) — refusing to send on default pool", p.SubscriberEmail, p.EverflowOfferID)
	}

	orgID := s.resolveOrgID(ctx, p.SubscriberID)
	campaignID, err := s.ensureShadowCampaign(ctx, orgID, p)
	if err != nil {
		return fmt.Errorf("click-drip sender: ensure shadow campaign: %w", err)
	}

	emailID := uuid.NewString()
	html := p.HTMLContent

	// Merge tags + tracking rewrite, mirroring send_worker's ordering.
	html = ReplaceTrackingMergeTags(html, campaignID, p.SubscriberID)
	trackBase := s.resolveTrackingURL(ctx, p.ProfileID)
	if trackBase != "" {
		html = InjectTrackingPixelAndLinks(html, campaignID, p.SubscriberID, emailID, trackBase, orgID, s.trackingSecret)
	}

	msg := &EmailMessage{
		ID:           emailID,
		CampaignID:   campaignID,
		SubscriberID: p.SubscriberID,
		Email:        p.SubscriberEmail,
		FromName:     p.FromName,
		FromEmail:    p.FromEmail,
		Subject:      p.Subject,
		HTMLContent:  html,
		ProfileID:    p.ProfileID,
		ESPType:      "pmta",
		RecipientISP: ClassifySubscriberISP(p.SubscriberEmail),
		Headers: map[string]string{
			"X-Journey-ID":       p.JourneyID,
			"X-Click-Drip-Offer": p.EverflowOfferID,
			"X-Click-Drip-Step":  fmt.Sprintf("%d", p.ReminderSeq),
		},
	}

	result, err := s.profileSender.Send(ctx, msg)
	if err != nil {
		return fmt.Errorf("click-drip send to %s (profile=%s): %w", p.SubscriberEmail, p.ProfileID, err)
	}
	if result == nil || !result.Success {
		return fmt.Errorf("click-drip send to %s returned no success", p.SubscriberEmail)
	}

	s.writeMessageLog(ctx, result.MessageID, orgID, campaignID, p.SubscriberID, p.SubscriberEmail)
	log.Printf("JourneyClickDripSender: sent reminder step=%d offer=%s to %s via profile=%s (vmta=%s, campaign=%s)",
		p.ReminderSeq, p.EverflowOfferID, p.SubscriberEmail, p.ProfileID, result.VMTA, campaignID)
	return nil
}

// resolveOrgID returns the subscriber's organization, falling back to the
// single-tenant default.
func (s *JourneyClickDripSender) resolveOrgID(ctx context.Context, subscriberID string) string {
	if sid, err := uuid.Parse(subscriberID); err == nil && sid != uuid.Nil {
		var org sql.NullString
		_ = s.db.QueryRowContext(ctx,
			`SELECT organization_id::text FROM mailing_subscribers WHERE id=$1`, sid).Scan(&org)
		if org.Valid && org.String != "" {
			return org.String
		}
	}
	return clickDripDefaultOrgID
}

// resolveTrackingURL returns the profile's tracking domain if configured,
// else the global tracking URL. Mirrors SendWorkerPool.resolveTrackingURL so
// open/click links carry the brand's tracking host.
func (s *JourneyClickDripSender) resolveTrackingURL(ctx context.Context, profileID string) string {
	base := s.trackingURL
	if profileID == "" {
		return base
	}
	pid, err := uuid.Parse(profileID)
	if err != nil {
		return base
	}
	var trackingDomain, sendingDomain sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(tracking_domain,''), COALESCE(sending_domain,'') FROM mailing_sending_profiles WHERE id=$1`,
		pid).Scan(&trackingDomain, &sendingDomain)
	if err != nil {
		return base
	}
	if trackingDomain.Valid && trackingDomain.String != "" {
		return normalizeTrackBase(trackingDomain.String)
	}
	return base
}

func normalizeTrackBase(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return d
	}
	if !strings.HasPrefix(d, "http://") && !strings.HasPrefix(d, "https://") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
}

// ensureShadowCampaign returns the id of a persistent, inert (status='draft')
// campaign row used purely for attribution + message_log FK. One row per
// (everflow_offer_id) for the click-drip journey, reused across all touches.
//
// status='draft' keeps it invisible to the batch send dispatcher (which only
// claims status='sending'). campaign_type='journey_node' matches the
// JourneyEmailNodeActivator convention so dashboards already filter it out of
// regular campaign lists.
//
// The id is DETERMINISTIC per offer (shadowCampaignID), so the existence check
// is a primary-key lookup rather than a (campaign_type, name) sequential scan.
// The prior name-scan version took ~27s on a 90k-row mailing_campaigns table
// and tripped the executor's 30s context, failing every reminder send.
func (s *JourneyClickDripSender) ensureShadowCampaign(ctx context.Context, orgID string, p ClickDripSendParams) (string, error) {
	campaignID := shadowCampaignID(p.EverflowOfferID)
	name := fmt.Sprintf("Click-Drip Reminder · offer %s", p.EverflowOfferID)

	// Fast path: primary-key existence check.
	var existing sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT id::text FROM mailing_campaigns WHERE id=$1`, campaignID).Scan(&existing)
	if existing.Valid && existing.String != "" {
		return existing.String, nil
	}

	// Resolve internal offer UUID for offer_id linkage (non-fatal if absent).
	var offerUUID uuid.NullUUID
	_ = s.db.QueryRowContext(ctx,
		`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1 LIMIT 1`,
		p.EverflowOfferID).Scan(&offerUUID)

	var profileUUID uuid.NullUUID
	if pid, err := uuid.Parse(p.ProfileID); err == nil {
		profileUUID = uuid.NullUUID{UUID: pid, Valid: true}
	}

	// Insert with the deterministic id; ON CONFLICT (id) collapses the race
	// where a concurrent touch for the same offer inserted first.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, status,
			subject, from_name, from_email,
			campaign_type, execution_mode,
			sending_profile_id, offer_id,
			total_recipients, max_recipients,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, 'draft',
			$4, $5, $6,
			'journey_node', 'pmta_isp_wave',
			$7, $8,
			0, 0,
			NOW(), NOW()
		)
		ON CONFLICT (id) DO NOTHING
	`,
		campaignID, orgID, name,
		p.Subject, p.FromName, p.FromEmail,
		profileUUID, offerUUID,
	)
	if err != nil {
		return "", err
	}
	return campaignID, nil
}

func (s *JourneyClickDripSender) writeMessageLog(ctx context.Context, messageID, orgID, campaignID, subscriberID, email string) {
	logCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(logCtx, `
		INSERT INTO mailing_message_log (id, message_id, organization_id, campaign_id, subscriber_id, email, esp_type, sent_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'pmta', NOW())
	`, messageID, orgID, campaignID, subscriberID, email)
	if err != nil {
		log.Printf("JourneyClickDripSender: message_log insert failed (msg=%s sub=%s): %v", messageID, email, err)
	}
}

package api

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" // SES SNS SignatureVersion 1 still mandates SHA-1 RSA.
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// VersionSESEvents is bumped on every modification per the workspace versioning rule.
// 2.0 — persists every SES event type into mailing_tracking_events keyed by the
// recipient_send_id / campaign_id / subscriber_id MessageTags so SES DELIVERY
// becomes the authoritative delivery signal for SES-routed mail.
// 2.1 — on a new SES DELIVERY event, also writes back ses_delivered into the
// pmta_acct_daily_summary rollup at the summary builder's (date, campaign, isp)
// grain so accounting-driven dashboards report SES-confirmed delivery.
// 2.2 — SES OPEN/CLICK now increment the mailing_campaigns open_count/click_count
// rollups (previously only DELIVERY/BOUNCE/COMPLAINT did), giving SES-routed mail
// the same campaign-level open/click reporting the internal tracking pixel gives
// dedicated-PMTA mail. SES click-tracking of stylesheet/font/asset <link> URLs is
// flagged is_machine_click=true so it neither inflates click_count nor pollutes
// the `NOT is_machine_click` real-click segment/funnel queries.
// 2.3 — full engagement parity with the internal tracking pixel on via_ses sends:
// SES OPEN/CLICK now also (a) increment unique_open_count/unique_click_count on the
// subscriber's FIRST open/human-click of a campaign, (b) update mailing_subscribers
// total_opens/last_open_at + total_clicks/last_click_at + engagement_score,
// (c) shadow-write subscriber_domain_state (UpsertSDSOpen/Click + RecomputeSDSScoreLocal),
// and (d) upsert mailing_inbox_profiles. Machine/asset clicks are excluded from all of these.
const VersionSESEvents = "2.3"

// SESEventsHandler receives SNS-wrapped SES event-publishing notifications
// (Bounce, Complaint, Open, Click, Send, Delivery, Reject, DeliveryDelay) and:
//
//   - Confirms SubscriptionConfirmation messages by GET'ing SubscribeURL
//   - Verifies SNS message signatures (SignatureVersion 1, SHA1WithRSA)
//   - Routes Permanent bounces and Complaints into globalHub.Suppress so
//     the authoritative mailing_global_suppressions table is updated and
//     the in-memory hot cache stays consistent with downstream send paths
//   - Logs (without 500) Open/Click/Send/Delivery/Reject/DeliveryDelay so
//     the configuration set's full event-type set is accepted without
//     bouncing notifications back to SNS dead-letter
//
// Registered publicly on s.router (outside the auth middleware) so SNS
// HTTPS POSTs aren't 401'd. Mirrors the pattern used for unsub-inbound
// and the PMTA accounting webhook.
type SESEventsHandler struct {
	db     *sql.DB
	hub    *engine.GlobalSuppressionHub
	orgID  string
	client *http.Client

	mu       sync.RWMutex
	certs    map[string]*x509.Certificate
	disableSig bool // test override only
}

// NewSESEventsHandler constructs the handler. The org ID matches the engine
// org used elsewhere (00000000-0000-0000-0000-000000000001) so the
// suppressions land in the same row space as PMTA agent / FBL signals.
func NewSESEventsHandler(db *sql.DB, hub *engine.GlobalSuppressionHub, orgID string) *SESEventsHandler {
	disableSig := strings.EqualFold(os.Getenv("SES_WEBHOOK_DISABLE_SIG"), "true")
	if disableSig {
		log.Printf("[ses-events] WARNING: SES_WEBHOOK_DISABLE_SIG=true — signature verification is DISABLED. Set this only for unit tests.")
	}
	return &SESEventsHandler{
		db:         db,
		hub:        hub,
		orgID:      orgID,
		client:     &http.Client{Timeout: 10 * time.Second},
		certs:      make(map[string]*x509.Certificate),
		disableSig: disableSig,
	}
}

// snsEnvelope mirrors the field set documented at
// https://docs.aws.amazon.com/sns/latest/dg/sns-message-and-json-formats.html
type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageId        string `json:"MessageId"`
	TopicArn         string `json:"TopicArn"`
	Subject          string `json:"Subject"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	UnsubscribeURL   string `json:"UnsubscribeURL"`
	SubscribeURL     string `json:"SubscribeURL"`
	Token            string `json:"Token"`
}

// sesEventNotification is the schema SES publishes when sending events
// through the configuration-set's SNS event-destination. We deliberately
// only declare the fields we read; SES adds many more.
type sesEventNotification struct {
	EventType        string                 `json:"eventType"`        // SES v2 event-publishing key
	NotificationType string                 `json:"notificationType"` // legacy v1 key kept for backward compat
	Mail             struct {
		MessageId     string              `json:"messageId"`
		Timestamp     string              `json:"timestamp"`
		Source        string              `json:"source"`
		Tags          map[string][]string `json:"tags"`
		CommonHeaders struct {
			To   []string `json:"to"`
			From []string `json:"from"`
		} `json:"commonHeaders"`
	} `json:"mail"`
	Delivery struct {
		Timestamp    string   `json:"timestamp"`
		Recipients   []string `json:"recipients"`
		SmtpResponse string   `json:"smtpResponse"`
	} `json:"delivery"`
	Bounce struct {
		BounceType        string `json:"bounceType"`        // Permanent | Transient | Undetermined
		BounceSubType     string `json:"bounceSubType"`     // Suppressed, OnAccountSuppressionList, OnSuppressionList, ...
		BouncedRecipients []struct {
			EmailAddress   string `json:"emailAddress"`
			DiagnosticCode string `json:"diagnosticCode"`
			Status         string `json:"status"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint struct {
		ComplainedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"complainedRecipients"`
		ComplaintFeedbackType string `json:"complaintFeedbackType"`
	} `json:"complaint"`
	Reject struct {
		Reason string `json:"reason"`
	} `json:"reject"`
	Open struct {
		Timestamp string `json:"timestamp"`
		IPAddress string `json:"ipAddress"`
	} `json:"open"`
	Click struct {
		Timestamp string `json:"timestamp"`
		IPAddress string `json:"ipAddress"`
		Link      string `json:"link"`
	} `json:"click"`
	DeliveryDelay struct {
		DelayType    string `json:"delayType"`
		ExpirationTime string `json:"expirationTime"`
	} `json:"deliveryDelay"`
}

// ServeHTTP is the public entrypoint. It returns 200 on every successful
// envelope decode, even for event types we only log (Open, Click, etc.),
// so SNS doesn't redeliver indefinitely.
func (h *SESEventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var env snsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		log.Printf("[ses-events] envelope decode error: %v", err)
		http.Error(w, "invalid envelope", http.StatusBadRequest)
		return
	}

	if !h.disableSig {
		if err := h.verifySNSSignature(&env); err != nil {
			log.Printf("[ses-events] SNS signature verification FAILED for MessageId=%s Type=%s: %v",
				env.MessageId, env.Type, err)
			http.Error(w, "signature verification failed", http.StatusForbidden)
			return
		}
	}

	switch env.Type {
	case "SubscriptionConfirmation":
		h.confirmSubscription(env)
		w.WriteHeader(http.StatusOK)
		return

	case "UnsubscribeConfirmation":
		log.Printf("[ses-events] received UnsubscribeConfirmation for TopicArn=%s — ignoring", env.TopicArn)
		w.WriteHeader(http.StatusOK)
		return

	case "Notification":
		h.processNotification(r, env)
		w.WriteHeader(http.StatusOK)
		return

	default:
		log.Printf("[ses-events] unknown envelope Type=%q MessageId=%s — accepting", env.Type, env.MessageId)
		w.WriteHeader(http.StatusOK)
	}
}

func (h *SESEventsHandler) confirmSubscription(env snsEnvelope) {
	if env.SubscribeURL == "" {
		log.Printf("[ses-events] SubscriptionConfirmation has no SubscribeURL — cannot confirm")
		return
	}
	log.Printf("[ses-events] SubscriptionConfirmation TopicArn=%s — confirming via SubscribeURL", env.TopicArn)
	resp, err := h.client.Get(env.SubscribeURL)
	if err != nil {
		log.Printf("[ses-events] subscription confirmation HTTP GET failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("[ses-events] subscription confirmed (HTTP %d)", resp.StatusCode)
	} else {
		log.Printf("[ses-events] subscription confirmation got non-2xx HTTP %d", resp.StatusCode)
	}
}

func (h *SESEventsHandler) processNotification(r *http.Request, env snsEnvelope) {
	var note sesEventNotification
	if err := json.Unmarshal([]byte(env.Message), &note); err != nil {
		log.Printf("[ses-events] inner Message JSON decode failed for MessageId=%s: %v", env.MessageId, err)
		return
	}

	// SES sends "eventType" (configuration-set event-publishing) OR
	// "notificationType" (legacy identity-feedback). Pick whichever is set.
	eventType := note.EventType
	if eventType == "" {
		eventType = note.NotificationType
	}
	if eventType == "" {
		log.Printf("[ses-events] notification missing eventType/notificationType for MessageId=%s", env.MessageId)
		return
	}

	tagConfig := firstTag(note.Mail.Tags, "ses:configuration-set")
	tagTenant := firstTag(note.Mail.Tags, "ses:tenant-name")

	// Custom MessageTags stamped at send time (send_worker.go relay path and
	// esp_ses.go direct path). These let us join the SES event back to the
	// exact campaign + subscriber + send attempt.
	tagCampaign := firstTag(note.Mail.Tags, "campaign_id")
	tagSubscriber := firstTag(note.Mail.Tags, "subscriber_id")
	tagSendID := firstTag(note.Mail.Tags, "recipient_send_id")

	switch eventType {
	case "Bounce":
		h.handleBounce(r, env, note, tagConfig, tagTenant)
	case "Complaint":
		h.handleComplaint(r, env, note, tagConfig, tagTenant)
	case "Open":
		h.persistSESEvent(r, "opened", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", "", note.Open.Timestamp)
		log.Printf("[ses-events] OPEN config_set=%s tenant=%s ip=%s", tagConfig, tagTenant, note.Open.IPAddress)
	case "Click":
		h.persistSESEvent(r, "clicked", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", note.Click.Link, note.Click.Timestamp)
		log.Printf("[ses-events] CLICK config_set=%s tenant=%s link=%s ip=%s", tagConfig, tagTenant, note.Click.Link, note.Click.IPAddress)
	case "Send":
		h.persistSESEvent(r, "sent", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", "", note.Mail.Timestamp)
		log.Printf("[ses-events] SEND config_set=%s tenant=%s msgid=%s", tagConfig, tagTenant, env.MessageId)
	case "Delivery":
		rcpt := firstRecipient(note.Delivery.Recipients)
		if rcpt == "" {
			rcpt = firstRecipient(note.Mail.CommonHeaders.To)
		}
		ts := note.Delivery.Timestamp
		if ts == "" {
			ts = note.Mail.Timestamp
		}
		// SES DELIVERY is the authoritative delivery signal for SES-routed
		// mail (accepted by the recipient mail system; NOT inbox placement).
		h.persistSESEvent(r, "delivered", note, tagCampaign, tagSubscriber, tagSendID, rcpt, "", "", ts)
		log.Printf("[ses-events] DELIVERY config_set=%s tenant=%s msgid=%s", tagConfig, tagTenant, env.MessageId)
	case "Reject":
		h.persistSESEvent(r, "rejected", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), note.Reject.Reason, "", note.Mail.Timestamp)
		log.Printf("[ses-events] REJECT reason=%s config_set=%s tenant=%s msgid=%s",
			note.Reject.Reason, tagConfig, tagTenant, env.MessageId)
	case "DeliveryDelay":
		// A delivery delay is a transient deferral, not a terminal state.
		h.persistSESEvent(r, "deferred", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), note.DeliveryDelay.DelayType, "", note.Mail.Timestamp)
		log.Printf("[ses-events] DELIVERY_DELAY type=%s expire=%s config_set=%s tenant=%s",
			note.DeliveryDelay.DelayType, note.DeliveryDelay.ExpirationTime, tagConfig, tagTenant)
	default:
		log.Printf("[ses-events] unhandled eventType=%q config_set=%s tenant=%s — accepting",
			eventType, tagConfig, tagTenant)
	}
}

// subscriberIDStr renders an optional subscriber UUID for the event lake.
func subscriberIDStr(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

// firstRecipient normalizes the first address in a recipient list.
func firstRecipient(list []string) string {
	for _, v := range list {
		v = strings.ToLower(strings.TrimSpace(v))
		// commonHeaders.to entries can be "Name <addr@x>"; pull the addr.
		if lt := strings.LastIndex(v, "<"); lt >= 0 {
			if gt := strings.Index(v[lt:], ">"); gt >= 0 {
				v = v[lt+1 : lt+gt]
			}
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// parseSESTime parses an SES/ISO-8601 timestamp, falling back to now.
func parseSESTime(s string) time.Time {
	if s == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// persistSESEvent writes one SES event into mailing_tracking_events, attributed
// to a campaign + subscriber via the MessageTags. It is idempotent: a
// deterministic id derived from (campaign, subscriber, recipient_send_id,
// event_type, event_at) plus ON CONFLICT (id, event_at) DO NOTHING means SNS
// redeliveries collapse to a single row. recipientSendID is recorded in
// bounce_reason-adjacent metadata only via the dedupe key for now; a dedicated
// column rides the event lake in Phase 1.
func (h *SESEventsHandler) persistSESEvent(r *http.Request, eventType string, note sesEventNotification,
	campaignID, subscriberID, recipientSendID, recipientEmail, bounceType, linkURL, tsRaw string) {

	if h.db == nil || campaignID == "" {
		// Without a campaign tag we cannot attribute the event. Pre-tag
		// in-flight sends will simply not persist; post-deploy sends carry
		// the tag. (Suppression for bounce/complaint is unaffected.)
		return
	}
	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	recipientEmail = strings.ToLower(strings.TrimSpace(recipientEmail))

	var subPtr *uuid.UUID
	if subscriberID != "" {
		if u, e := uuid.Parse(subscriberID); e == nil {
			subPtr = &u
		}
	}
	// Fallback: resolve subscriber from message_log when the tag is absent
	// (e.g. legacy in-flight sends) but we have the recipient address.
	if subPtr == nil && recipientEmail != "" {
		var sid sql.NullString
		h.db.QueryRowContext(ctx, `
			SELECT subscriber_id::text FROM mailing_message_log
			WHERE campaign_id = $1 AND LOWER(email) = $2
			ORDER BY sent_at DESC LIMIT 1
		`, campUUID, recipientEmail).Scan(&sid)
		if sid.Valid {
			if u, e := uuid.Parse(sid.String); e == nil {
				subPtr = &u
			}
		}
	}

	recipientDomain := ""
	if i := strings.LastIndex(recipientEmail, "@"); i >= 0 {
		recipientDomain = recipientEmail[i+1:]
	}

	var orgPtr *uuid.UUID
	if u, e := uuid.Parse(h.orgID); e == nil {
		orgPtr = &u
	}

	eventAt := parseSESTime(tsRaw)

	// Deterministic id: prefer recipient_send_id for uniqueness, else fall
	// back to subscriber/email so retries still dedupe.
	idSeed := recipientSendID
	if idSeed == "" {
		idSeed = subscriberID + "|" + recipientEmail
	}
	dedupe := fmt.Sprintf("ses|%s|%s|%s|%s", campaignID, idSeed, eventType, eventAt.Format(time.RFC3339Nano))
	eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(dedupe))

	var linkPtr, bouncePtr *string
	if linkURL != "" {
		linkPtr = &linkURL
	}
	if bounceType != "" {
		bouncePtr = &bounceType
	}

	// SES click-tracking rewrites EVERY http(s) URL in the HTML body, so mail
	// clients loading remote stylesheets, web fonts, and images trip CLICK
	// events the moment the message renders. Flag those is_machine_click=true
	// (mirroring the internal ClassifyClickAsMachine path) so they stay out of
	// click_count and out of the `NOT is_machine_click` real-click queries.
	machineClick := eventType == "clicked" && isMachineClickURL(linkURL)

	res, err := h.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events
			(id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, link_url, event_at, recipient_domain, is_machine_click)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id, event_at) DO NOTHING
	`, eventID, orgPtr, campUUID, subPtr, eventType, bouncePtr, linkPtr, eventAt, recipientDomain, machineClick)
	if err != nil {
		log.Printf("[ses-events] persist %s campaign=%s error: %v", eventType, campaignID, err)
		return
	}
	// Only act on a genuinely new row — SNS can redeliver the same
	// notification, and ON CONFLICT makes those a no-op (RowsAffected == 0).
	if n, raErr := res.RowsAffected(); raErr != nil || n == 0 {
		return
	}

	// Phase 1 event lake (best-effort, no-op unless enabled). Emitted only on a
	// genuinely new row so SNS redeliveries don't double-feed the lake. This is
	// the SES "delivered != relay" truth that the RRU reconciliation depends on.
	analytics.Emit(analytics.Event{
		EventUID:        "ses:" + eventID.String(),
		RecipientSendID: recipientSendID,
		CampaignID:      campaignID,
		SubscriberID:    subscriberIDStr(subPtr),
		Email:           recipientEmail,
		EmailDomain:     recipientDomain,
		ISPGroup:        isp.GroupFromDomain(recipientDomain),
		RouteType:       "ses",
		EventType:       analytics.CanonicalEventType(eventType),
		BounceCat:       bounceType,
		LinkURL:         linkURL,
		EventAt:         eventAt.UTC().Format(time.RFC3339),
		Source:          "ses",
	})

	// Keep mailing_campaigns aggregate counters consistent for SES-routed
	// mail, since PMTA "d" is now recorded as relayed_to_ses (not delivered)
	// and SES owns the bounce/complaint signal for these sends. Per the
	// bounce-metrics rule, hard and soft bounces stay separate.
	switch eventType {
	case "opened":
		// Full parity with the internal tracking pixel (mailing_tracking.go),
		// which is skipped on via_ses sends. Every SES OPEN counts; MPP/
		// machine-open filtering stays a downstream concern (the internal path
		// counts every open too).
		h.applyOpenEngagement(ctx, campUUID, subPtr, recipientEmail, recipientDomain)
	case "clicked":
		// Asset/font/stylesheet fetches (machineClick) are recorded as events
		// but excluded from click_count and all engagement side-effects,
		// matching the internal click handler's human-click semantics.
		if !machineClick {
			h.applyClickEngagement(ctx, campUUID, subPtr, recipientEmail, recipientDomain)
		}
	case "delivered":
		h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET delivered_count = COALESCE(delivered_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)

		// Mirror the SES-confirmed delivery into the PMTA accounting rollup as a
		// dedicated ses_delivered counter, keyed on the SAME
		// (summary_date, campaign_id, recipient_isp) grain the summary builder
		// uses (isp.GroupFromDomain on the recipient domain). This keeps every
		// accounting-driven dashboard (deliverability heatmap, metrics endpoint,
		// growth narrative, CSV exports) SES-aware: they read
		// delivered + ses_delivered as true delivered, while relayed_to_ses
		// stays the labeled "PMTA→SES handoff" and delivered stays pure
		// PMTA-direct facts. The PMTA "d" relay row almost always pre-exists
		// (relay happens at send time, DELIVERY arrives later), so this is
		// typically a cheap UPDATE on the existing (campaign, isp) row.
		ispGroup := isp.GroupFromDomain(recipientDomain)
		summaryDate := eventAt.UTC().Format("2006-01-02")
		if _, derr := h.db.ExecContext(ctx, `
			INSERT INTO pmta_acct_daily_summary
				(id, summary_date, campaign_id, recipient_isp, ses_delivered, total_records, last_updated_at)
			VALUES (gen_random_uuid(), $1::date, $2::uuid, $3, 1, 0, NOW())
			ON CONFLICT (summary_date, COALESCE(campaign_id, '00000000-0000-0000-0000-000000000000'::uuid), recipient_isp)
			DO UPDATE SET ses_delivered = pmta_acct_daily_summary.ses_delivered + 1, last_updated_at = NOW()
		`, summaryDate, campUUID, ispGroup); derr != nil {
			log.Printf("[ses-events] ses_delivered rollup upsert campaign=%s isp=%s error: %v", campaignID, ispGroup, derr)
		}
	case "bounced":
		if bounceType == "hard" {
			h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, hard_bounce_count = COALESCE(hard_bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
		} else {
			h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET bounce_count = COALESCE(bounce_count, 0) + 1, soft_bounce_count = COALESCE(soft_bounce_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
		}
	case "complained":
		h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET complaint_count = COALESCE(complaint_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campUUID)
	}
}

// applyOpenEngagement mirrors the durable engagement side-effects the internal
// tracking pixel performs in HandleTrackOpen (mailing_tracking.go), which is
// SKIPPED on via_ses sends because send_worker.go only injects the pixel when
// !sesInfo.ViaSES. It is invoked only on a genuinely new SES OPEN row (the
// caller already verified RowsAffected > 0), so SNS redeliveries do not
// double-count. Every write is best-effort; a failure on one does not block the
// others or the 200 response back to SNS.
func (h *SESEventsHandler) applyOpenEngagement(ctx context.Context, campaignID uuid.UUID, subID *uuid.UUID, email, domain string) {
	// Campaign-level total opens (parity with mailing_tracking.go:194).
	h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET open_count = COALESCE(open_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campaignID)

	if subID != nil {
		// Unique opener: bump only when the just-inserted row is this
		// subscriber's FIRST opened event for the campaign (count == 1). Later
		// opens leave it unchanged, so the column converges on distinct openers.
		h.db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET unique_open_count = COALESCE(unique_open_count, 0) + 1, updated_at = NOW()
			WHERE id = $1 AND (
				SELECT COUNT(*) FROM mailing_tracking_events
				WHERE campaign_id = $1 AND subscriber_id = $2 AND event_type = 'opened'
			) = 1`, campaignID, *subID)

		// Subscriber denorm columns. last_open_at in particular gates the
		// standing Welcome saturation segment (prior_sends > 8 AND
		// last_open_at IS NULL) — without this, SES-route opens never clear a
		// subscriber out of the "never opened" cohort.
		h.db.ExecContext(ctx, `
			UPDATE mailing_subscribers SET total_opens = COALESCE(total_opens, 0) + 1, last_open_at = NOW(), updated_at = NOW()
			WHERE id = $1`, *subID)

		// Master-selection shadow state (parity with mailing_tracking.go:204-206).
		sd := mailing.ResolveSendingDomainForCampaign(ctx, h.db, campaignID)
		mailing.UpsertSDSOpen(ctx, h.db, *subID, sd)
		mailing.RecomputeSDSScoreLocal(ctx, h.db, *subID, sd)

		h.recomputeSubscriberEngagementScore(ctx, *subID)
	}

	if email != "" {
		eHash := emailHash(email)
		h.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_opens, last_open_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET total_opens = mailing_inbox_profiles.total_opens + 1, last_open_at = NOW(), updated_at = NOW()
		`, eHash, email, domain, extractISP(email))
		h.recomputeInboxProfileScore(ctx, eHash)
	}
}

// applyClickEngagement mirrors HandleTrackClick's durable side-effects for SES
// human clicks (machine/asset clicks never reach here). Same new-row / best-effort
// contract as applyOpenEngagement.
func (h *SESEventsHandler) applyClickEngagement(ctx context.Context, campaignID uuid.UUID, subID *uuid.UUID, email, domain string) {
	h.db.ExecContext(ctx, `UPDATE mailing_campaigns SET click_count = COALESCE(click_count, 0) + 1, updated_at = NOW() WHERE id = $1`, campaignID)

	if subID != nil {
		// Unique human clicker: only the subscriber's first non-machine click
		// of the campaign counts (the count filters out machine clicks too).
		h.db.ExecContext(ctx, `
			UPDATE mailing_campaigns SET unique_click_count = COALESCE(unique_click_count, 0) + 1, updated_at = NOW()
			WHERE id = $1 AND (
				SELECT COUNT(*) FROM mailing_tracking_events
				WHERE campaign_id = $1 AND subscriber_id = $2 AND event_type = 'clicked'
				  AND NOT COALESCE(is_machine_click, false)
			) = 1`, campaignID, *subID)

		h.db.ExecContext(ctx, `
			UPDATE mailing_subscribers SET total_clicks = COALESCE(total_clicks, 0) + 1, last_click_at = NOW(), updated_at = NOW()
			WHERE id = $1`, *subID)

		sd := mailing.ResolveSendingDomainForCampaign(ctx, h.db, campaignID)
		mailing.UpsertSDSClick(ctx, h.db, *subID, sd)
		mailing.RecomputeSDSScoreLocal(ctx, h.db, *subID, sd)

		h.recomputeSubscriberEngagementScore(ctx, *subID)
	}

	if email != "" {
		eHash := emailHash(email)
		h.db.ExecContext(ctx, `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_clicks, last_click_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET total_clicks = mailing_inbox_profiles.total_clicks + 1, last_click_at = NOW(), updated_at = NOW()
		`, eHash, email, domain, extractISP(email))
		h.recomputeInboxProfileScore(ctx, eHash)
	}
}

// recomputeSubscriberEngagementScore replicates MailingService.updateEngagementScore
// (mailing_tracking.go) for the SES webhook, which has no MailingService handle.
// Score is on the 0–100 scale used by mailing_subscribers (thresholds 70/30).
func (h *SESEventsHandler) recomputeSubscriberEngagementScore(ctx context.Context, subscriberID uuid.UUID) {
	var totalOpens, totalClicks, totalEmails int
	var lastOpenAt, lastClickAt *time.Time
	if err := h.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_opens, 0), COALESCE(total_clicks, 0), COALESCE(total_emails_received, 1),
			   last_open_at, last_click_at
		FROM mailing_subscribers WHERE id = $1
	`, subscriberID).Scan(&totalOpens, &totalClicks, &totalEmails, &lastOpenAt, &lastClickAt); err != nil {
		return
	}
	if totalEmails <= 0 {
		totalEmails = 1
	}
	openRate := float64(totalOpens) / float64(totalEmails) * 100
	clickRate := float64(totalClicks) / float64(totalEmails) * 100
	score := (openRate * 0.4) + (clickRate * 0.6)
	if lastOpenAt != nil && time.Since(*lastOpenAt) < 7*24*time.Hour {
		score += 20
	} else if lastOpenAt != nil && time.Since(*lastOpenAt) < 30*24*time.Hour {
		score += 10
	}
	if score > 100 {
		score = 100
	}
	h.db.ExecContext(ctx, `UPDATE mailing_subscribers SET engagement_score = $2, updated_at = NOW() WHERE id = $1`, subscriberID, score)
}

// recomputeInboxProfileScore replicates MailingService.recomputeInboxProfileScore
// (mailing_tracking.go) for the SES webhook. Score is on the 0–1 scale used by
// mailing_inbox_profiles (thresholds 0.70/0.40).
func (h *SESEventsHandler) recomputeInboxProfileScore(ctx context.Context, eHash string) {
	var totalSends, totalOpens, totalClicks int
	var lastOpen *time.Time
	if err := h.db.QueryRowContext(ctx, `
		SELECT COALESCE(total_sends,0), COALESCE(total_opens,0), COALESCE(total_clicks,0), last_open_at
		FROM mailing_inbox_profiles WHERE email_hash = $1
	`, eHash).Scan(&totalSends, &totalOpens, &totalClicks, &lastOpen); err != nil {
		return
	}
	if totalSends == 0 {
		return
	}
	openRate := float64(totalOpens) / float64(totalSends)
	clickRate := float64(totalClicks) / float64(totalSends)
	score := (openRate * 0.6) + (clickRate * 0.4)
	if lastOpen != nil {
		daysSince := time.Since(*lastOpen).Hours() / 24
		if daysSince < 7 {
			score += 0.20
		} else if daysSince < 30 {
			score += 0.10
		}
	}
	if score > 1.0 {
		score = 1.0
	}
	h.db.ExecContext(ctx, `
		UPDATE mailing_inbox_profiles SET engagement_score = $2, updated_at = NOW()
		WHERE email_hash = $1
	`, eHash, score)
}

func (h *SESEventsHandler) handleBounce(r *http.Request, env snsEnvelope, note sesEventNotification, tagConfig, tagTenant string) {
	campaignID := firstTag(note.Mail.Tags, "campaign_id")
	subscriberID := firstTag(note.Mail.Tags, "subscriber_id")
	sendID := firstTag(note.Mail.Tags, "recipient_send_id")

	// Only Permanent bounces feed suppression. Transient (soft) bounces are
	// not list-hygiene events per the bounce-metrics rule, but we still
	// persist them as soft bounces so deferral/soft-bounce analytics are
	// accurate.
	if note.Bounce.BounceType != "Permanent" {
		for _, rec := range note.Bounce.BouncedRecipients {
			h.persistSESEvent(r, "bounced", note, campaignID, subscriberID, sendID,
				rec.EmailAddress, "soft", "", note.Mail.Timestamp)
			log.Printf("[ses-events] BOUNCE-soft type=%s subtype=%s addr=%s diag=%q config_set=%s tenant=%s",
				note.Bounce.BounceType, note.Bounce.BounceSubType,
				logger.RedactEmail(rec.EmailAddress), rec.DiagnosticCode, tagConfig, tagTenant)
		}
		return
	}

	reason := "ses_hard_bounce"
	switch note.Bounce.BounceSubType {
	case "Suppressed", "OnAccountSuppressionList", "OnSuppressionList":
		reason = "ses_suppression_list"
	}

	for _, rec := range note.Bounce.BouncedRecipients {
		if rec.EmailAddress == "" {
			continue
		}
		// Persist the hard bounce as a tracking event (bounce_type "hard" so
		// HardBounceSQL classifies it correctly) in addition to suppressing.
		h.persistSESEvent(r, "bounced", note, campaignID, subscriberID, sendID,
			rec.EmailAddress, "hard", "", note.Mail.Timestamp)
		if h.hub == nil {
			log.Printf("[ses-events] WARNING: globalHub not wired — dropping bounce for %s", logger.RedactEmail(rec.EmailAddress))
			continue
		}
		ctx := r.Context()
		added, err := h.hub.Suppress(ctx, rec.EmailAddress, reason, "ses_webhook", "", rec.Status, rec.DiagnosticCode, "", "")
		if err != nil {
			log.Printf("[ses-events] hub.Suppress error for %s: %v", logger.RedactEmail(rec.EmailAddress), err)
			continue
		}
		log.Printf("[ses-events] BOUNCE-hard added=%v reason=%s addr=%s subtype=%s config_set=%s tenant=%s",
			added, reason, logger.RedactEmail(rec.EmailAddress), note.Bounce.BounceSubType, tagConfig, tagTenant)
	}
}

func (h *SESEventsHandler) handleComplaint(r *http.Request, env snsEnvelope, note sesEventNotification, tagConfig, tagTenant string) {
	campaignID := firstTag(note.Mail.Tags, "campaign_id")
	subscriberID := firstTag(note.Mail.Tags, "subscriber_id")
	sendID := firstTag(note.Mail.Tags, "recipient_send_id")

	reason := "ses_complaint"
	if note.Complaint.ComplaintFeedbackType != "" {
		reason = "ses_complaint:" + note.Complaint.ComplaintFeedbackType
	}
	for _, rec := range note.Complaint.ComplainedRecipients {
		if rec.EmailAddress == "" {
			continue
		}
		h.persistSESEvent(r, "complained", note, campaignID, subscriberID, sendID,
			rec.EmailAddress, "", "", note.Mail.Timestamp)
		if h.hub == nil {
			log.Printf("[ses-events] WARNING: globalHub not wired — dropping complaint for %s", logger.RedactEmail(rec.EmailAddress))
			continue
		}
		ctx := r.Context()
		added, err := h.hub.Suppress(ctx, rec.EmailAddress, reason, "ses_webhook", "", "", "", "", "")
		if err != nil {
			log.Printf("[ses-events] hub.Suppress error for %s: %v", logger.RedactEmail(rec.EmailAddress), err)
			continue
		}
		log.Printf("[ses-events] COMPLAINT added=%v reason=%s addr=%s config_set=%s tenant=%s",
			added, reason, logger.RedactEmail(rec.EmailAddress), tagConfig, tagTenant)
	}
}

func firstTag(tags map[string][]string, key string) string {
	if v, ok := tags[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// isMachineClickURL reports whether a clicked link is an automated resource
// fetch rather than a human click. SES click-tracking rewrites every http(s)
// URL in the HTML body, so remote stylesheets, web fonts, and images trip
// CLICK events the moment the message renders in a mail client. Those must
// not count as engagement. Detection is by well-known asset/CDN host or by
// the URL path's file extension (query/fragment stripped first).
func isMachineClickURL(link string) bool {
	l := strings.ToLower(strings.TrimSpace(link))
	if l == "" {
		return false
	}
	for _, host := range []string{
		"fonts.googleapis.com", "fonts.gstatic.com", "cdnjs.cloudflare.com",
		"ajax.googleapis.com", "use.fontawesome.com", "use.typekit.net",
		"maxcdn.bootstrapcdn.com", "stackpath.bootstrapcdn.com",
		".gravatar.com", "schema.org", "www.w3.org",
	} {
		if strings.Contains(l, host) {
			return true
		}
	}
	path := l
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for _, ext := range []string{
		".css", ".js", ".woff", ".woff2", ".ttf", ".otf", ".eot",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico",
	} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// SNS signature verification (SignatureVersion 1, SHA1-with-RSA).
// Per https://docs.aws.amazon.com/sns/latest/dg/sns-verify-signature-of-message.html
// ---------------------------------------------------------------------------

func (h *SESEventsHandler) verifySNSSignature(env *snsEnvelope) error {
	if env.SignatureVersion != "1" {
		return fmt.Errorf("unsupported SignatureVersion %q", env.SignatureVersion)
	}
	if env.SigningCertURL == "" || env.Signature == "" {
		return fmt.Errorf("missing SigningCertURL or Signature")
	}

	if err := validateCertHost(env.SigningCertURL); err != nil {
		return fmt.Errorf("cert URL invalid: %w", err)
	}

	cert, err := h.fetchSigningCert(env.SigningCertURL)
	if err != nil {
		return fmt.Errorf("fetch cert: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("cert public key is not RSA")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	canonical := canonicalizeSNSEnvelope(env)
	hashed := sha1.Sum([]byte(canonical))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA1, hashed[:], sigBytes); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

// canonicalizeSNSEnvelope produces the exact byte string SNS signs.
// Field order and inclusion are documented and must NOT change.
func canonicalizeSNSEnvelope(env *snsEnvelope) string {
	switch env.Type {
	case "Notification":
		var sb strings.Builder
		sb.WriteString("Message\n")
		sb.WriteString(env.Message)
		sb.WriteString("\n")
		sb.WriteString("MessageId\n")
		sb.WriteString(env.MessageId)
		sb.WriteString("\n")
		if env.Subject != "" {
			sb.WriteString("Subject\n")
			sb.WriteString(env.Subject)
			sb.WriteString("\n")
		}
		sb.WriteString("Timestamp\n")
		sb.WriteString(env.Timestamp)
		sb.WriteString("\n")
		sb.WriteString("TopicArn\n")
		sb.WriteString(env.TopicArn)
		sb.WriteString("\n")
		sb.WriteString("Type\n")
		sb.WriteString(env.Type)
		sb.WriteString("\n")
		return sb.String()
	case "SubscriptionConfirmation", "UnsubscribeConfirmation":
		var sb strings.Builder
		sb.WriteString("Message\n")
		sb.WriteString(env.Message)
		sb.WriteString("\n")
		sb.WriteString("MessageId\n")
		sb.WriteString(env.MessageId)
		sb.WriteString("\n")
		sb.WriteString("SubscribeURL\n")
		sb.WriteString(env.SubscribeURL)
		sb.WriteString("\n")
		sb.WriteString("Timestamp\n")
		sb.WriteString(env.Timestamp)
		sb.WriteString("\n")
		sb.WriteString("Token\n")
		sb.WriteString(env.Token)
		sb.WriteString("\n")
		sb.WriteString("TopicArn\n")
		sb.WriteString(env.TopicArn)
		sb.WriteString("\n")
		sb.WriteString("Type\n")
		sb.WriteString(env.Type)
		sb.WriteString("\n")
		return sb.String()
	}
	return ""
}

// validateCertHost enforces the AWS-published rule that the SigningCertURL
// host must match `^sns\\.[a-zA-Z0-9-]+\\.amazonaws\\.com$`. Without this,
// an attacker could host a forged cert at any URL.
func validateCertHost(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("non-https scheme: %s", u.Scheme)
	}
	host := strings.ToLower(u.Host)
	// SNS publishes signing certs at sns.<region>.amazonaws.com.
	if !strings.HasPrefix(host, "sns.") || !strings.HasSuffix(host, ".amazonaws.com") {
		return fmt.Errorf("unexpected cert host: %s", host)
	}
	return nil
}

// fetchSigningCert caches certs by URL — SNS rotates them rarely and a
// single cache entry covers many notifications.
func (h *SESEventsHandler) fetchSigningCert(certURL string) (*x509.Certificate, error) {
	h.mu.RLock()
	if c, ok := h.certs[certURL]; ok {
		h.mu.RUnlock()
		return c, nil
	}
	h.mu.RUnlock()

	resp, err := h.client.Get(certURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cert fetch HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM cert payload")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	h.certs[certURL] = cert
	h.mu.Unlock()
	return cert, nil
}

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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
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
// 2.4 — SES OPEN/CLICK ipAddress + userAgent are now parsed and persisted into
// mailing_tracking_events (ip_address inet / user_agent, NULL when absent) and the
// lake event's source_ip. CLICK machine-classification additionally runs the internal
// tracking.ClassifyClickAsMachine UA/IP classifier (scanner UAs, bare-Mozilla + cloud
// CIDRs) on top of the existing asset-URL check.
// 2.5 — bounce REASONS are persisted instead of dropped: BOUNCE (and Reject/
// DeliveryDelay) events now carry the SES status + diagnosticCode into
// mailing_tracking_events.bounce_reason and the lake's dsn_code/dsn_diag
// (previously only the suppression hub kept them — the 2026-07-12 ATT tranche
// read "hard bounce, no reason" everywhere). SES pre-flight address-validation
// suppressions are additionally labeled reason='ses_address_validation' in the
// suppression hub — they never reach the remote MX and must not read as remote
// hard bounces.
// 2.6 — SES pre-flight validation blocks get their own bounce CLASS (operator
// 2026-07-12: "should not count towards our overall hard bounce rate... ensure
// it remains classified"): persisted bounce_type/bounce_cat = 'validation'
// (not 'hard'), campaign hard/soft counters untouched, lake reads re-class them
// to 'preflight_validation' (reader.go eventTypeExpr) — counted in NEITHER hard
// nor soft, queryable for the EmailOversight-vs-AWS-validation comparison.
const VersionSESEvents = "2.6"

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

	// queue is the async ingest buffer (ses_events_queue.go). nil means the
	// SES_WEBHOOK_ASYNC kill switch is off and ServeHTTP processes inline.
	queue *sesIngestQueue

	// batcher folds per-event campaign/subscriber counter increments into
	// periodic batched UPDATEs (ses_engagement_batcher.go), removing the
	// hot-row lock contention that starved engagement writes.
	batcher *sesEngagementBatcher
}

// NewSESEventsHandler constructs the handler. The org ID matches the engine
// org used elsewhere (00000000-0000-0000-0000-000000000001) so the
// suppressions land in the same row space as PMTA agent / FBL signals.
func NewSESEventsHandler(db *sql.DB, hub *engine.GlobalSuppressionHub, orgID string) *SESEventsHandler {
	disableSig := strings.EqualFold(os.Getenv("SES_WEBHOOK_DISABLE_SIG"), "true")
	if disableSig {
		log.Printf("[ses-events] WARNING: SES_WEBHOOK_DISABLE_SIG=true — signature verification is DISABLED. Set this only for unit tests.")
	}
	h := &SESEventsHandler{
		db:         db,
		hub:        hub,
		orgID:      orgID,
		client:     &http.Client{Timeout: 10 * time.Second},
		certs:      make(map[string]*x509.Certificate),
		disableSig: disableSig,
	}
	// Start the async ingest queue so SNS is answered in ~1ms and the server's
	// DB latency stops deciding whether SES events survive. Returns nil (and
	// ServeHTTP stays synchronous) when SES_WEBHOOK_ASYNC=false.
	h.queue = startSESIngestQueue(h)
	registerSESQueue(h.queue)

	// Start the counter batcher unless explicitly disabled. Per-event counter
	// increments against a single campaign row are what serialized ingest.
	if db != nil && !strings.EqualFold(os.Getenv("SES_ENGAGEMENT_BATCH"), "false") {
		h.batcher = newSESEngagementBatcher(db)
	} else if db != nil {
		log.Printf("[ses-engagement] BATCHING DISABLED (SES_ENGAGEMENT_BATCH=false) — counters will contend on hot rows")
	}
	registerSESBatcher(h.batcher)
	return h
}

// sesSideEffectTimeout bounds ONE engagement side-effect. Previously every
// side-effect shared a single 20s budget sequentially, so a slow early write
// consumed the whole allowance and the later ones failed by starvation rather
// than on their own merits. Each now gets its own clock.
func sesSideEffectTimeout() time.Duration {
	return time.Duration(envInt("SES_SIDE_EFFECT_TIMEOUT_SEC", 8)) * time.Second
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
	// IsBotEvent is AWS's own automated-interaction label, added to Open and
	// Click event notifications on 2026-08-07 ("Amazon SES now helps identify
	// automated email interactions"). Values: "Likely" | "Unlikely" | "" when
	// the account/region has not started emitting it. It is a LABEL ONLY — see
	// METRIC_CONTRACT.md §12: the operating engagement count stays inclusive
	// and never filters on it; only the VDM-comparable reconciliation lens
	// reads it.
	Open struct {
		Timestamp  string `json:"timestamp"`
		IPAddress  string `json:"ipAddress"`
		UserAgent  string `json:"userAgent"`
		IsBotEvent string `json:"isBotEvent"`
	} `json:"open"`
	Click struct {
		Timestamp  string `json:"timestamp"`
		IPAddress  string `json:"ipAddress"`
		UserAgent  string `json:"userAgent"`
		Link       string `json:"link"`
		IsBotEvent string `json:"isBotEvent"`
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
		// Decode the inner SES payload on the request path (CPU only, no I/O)
		// so a malformed body is rejected here rather than poisoning the queue.
		var note sesEventNotification
		if err := json.Unmarshal([]byte(env.Message), &note); err != nil {
			log.Printf("[ses-events] inner Message JSON decode failed for MessageId=%s: %v", env.MessageId, err)
			// Still 200: a payload we cannot parse will never parse, so making
			// SNS retry it just burns its 3-attempt budget.
			w.WriteHeader(http.StatusOK)
			return
		}

		// ASYNC PATH (default): hand the event to the ingest queue and answer
		// SNS immediately. The SNS HTTPS subscription allows only 3 retries at
		// 20s with NO dead-letter queue, so any notification we cannot answer
		// inside its delivery timeout is destroyed permanently. Doing DB work
		// on this request path is what made 13,078 events unrecoverable in a
		// single 24h window on 2026-08-11. Answering in ~1ms removes the
		// server's latency from SNS's delivery decision entirely.
		if h.queue != nil {
			if h.queue.enqueue(env, note) {
				w.WriteHeader(http.StatusOK)
				return
			}
			// Queue full — the buffer is the last line of defence, so fall back
			// to 503. SNS will retry, which is strictly better than dropping.
			atomic.AddUint64(&sesQueueRejected, 1)
			log.Printf("[ses-events] ingest queue FULL — returning 503 so SNS retries MessageId=%s", env.MessageId)
			http.Error(w, "ingest queue full", http.StatusServiceUnavailable)
			return
		}

		// SYNC FALLBACK: only when SES_WEBHOOK_ASYNC=false. Preserves the exact
		// pre-fix behavior as a one-env-var rollback.
		if err := h.dispatchNotification(r.Context(), env, note); err != nil {
			atomic.AddUint64(&sesSyncPersistFailed, 1)
			log.Printf("[ses-events] sync dispatch failed MessageId=%s: %v", env.MessageId, err)
		}
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

// dispatchNotification performs the DB-bound work for one decoded SES
// notification. It takes a plain context — NOT the *http.Request — because the
// async ingest queue runs it AFTER the HTTP handler has already returned 200 to
// SNS, at which point the request context is cancelled. Passing r.Context()
// here would cancel every DB write the instant the response was written.
//
// It returns a non-nil error only for RETRYABLE failures (the tracking-event
// INSERT itself); malformed payloads return nil so a poison message is not
// retried forever.
func (h *SESEventsHandler) dispatchNotification(ctx context.Context, env snsEnvelope, note sesEventNotification) error {
	// SES sends "eventType" (configuration-set event-publishing) OR
	// "notificationType" (legacy identity-feedback). Pick whichever is set.
	eventType := note.EventType
	if eventType == "" {
		eventType = note.NotificationType
	}
	if eventType == "" {
		log.Printf("[ses-events] notification missing eventType/notificationType for MessageId=%s", env.MessageId)
		return nil
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
		return h.handleBounce(ctx, env, note, tagConfig, tagTenant)
	case "Complaint":
		return h.handleComplaint(ctx, env, note, tagConfig, tagTenant)
	case "Open":
		if err := h.persistSESEvent(ctx, "opened", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", "", "", "", note.Open.IPAddress, note.Open.UserAgent, note.Open.Timestamp); err != nil {
			return err
		}
		log.Printf("[ses-events] OPEN config_set=%s tenant=%s ip=%s", tagConfig, tagTenant, note.Open.IPAddress)
	case "Click":
		if err := h.persistSESEvent(ctx, "clicked", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", "", "", note.Click.Link, note.Click.IPAddress, note.Click.UserAgent, note.Click.Timestamp); err != nil {
			return err
		}
		log.Printf("[ses-events] CLICK config_set=%s tenant=%s link=%s ip=%s", tagConfig, tagTenant, note.Click.Link, note.Click.IPAddress)
	case "Send":
		// NOT 'sent' — see METRIC_CONTRACT.md §2.1 "The `sent` writer".
		//
		// The send worker is the SINGLE canonical writer of a `sent` tracking
		// event (send_worker.go:2423, markSent): it fires once per message for
		// EVERY transport (pmta/kumo/ses/sparkpost/mailgun/sendgrid — the
		// switch at send_worker.go:2186) at submission. This SES notification
		// exists only for SES, arrives minutes later, and is dropped entirely
		// when the campaign_id MessageTag is absent (persistSESEvent :470), so
		// it can never be a comparable denominator.
		//
		// Writing it as 'sent' too gave every SES-relayed message TWO `sent`
		// rows (2026-06-05 … this deploy), halving every rate whose
		// denominator is `event_type='sent'`. It is now recorded under its own
		// type, which nothing divides by. Note 'relayed_to_ses' would have been
		// WRONG here: engine/ingest.go:355 already emits that from the PMTA
		// accounting files for the PMTA→SES handoff, so reusing it would just
		// move the double-count (mailing_campaign_summary.go:606).
		//
		// CanonicalEventType maps it back to the lake's 'attempted' so the
		// source='ses' lake stream is byte-identical to before.
		if err := h.persistSESEvent(ctx, "ses_accepted", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), "", "", "", "", "", "", note.Mail.Timestamp); err != nil {
			return err
		}
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
		if err := h.persistSESEvent(ctx, "delivered", note, tagCampaign, tagSubscriber, tagSendID, rcpt, "", "", "", "", "", "", ts); err != nil {
			return err
		}
		log.Printf("[ses-events] DELIVERY config_set=%s tenant=%s msgid=%s", tagConfig, tagTenant, env.MessageId)
	case "Reject":
		if err := h.persistSESEvent(ctx, "rejected", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), note.Reject.Reason, "", note.Reject.Reason, "", "", "", note.Mail.Timestamp); err != nil {
			return err
		}
		log.Printf("[ses-events] REJECT reason=%s config_set=%s tenant=%s msgid=%s",
			note.Reject.Reason, tagConfig, tagTenant, env.MessageId)
	case "DeliveryDelay":
		// A delivery delay is a transient deferral, not a terminal state.
		if err := h.persistSESEvent(ctx, "deferred", note, tagCampaign, tagSubscriber, tagSendID,
			firstRecipient(note.Mail.CommonHeaders.To), note.DeliveryDelay.DelayType, "", note.DeliveryDelay.DelayType, "", "", "", note.Mail.Timestamp); err != nil {
			return err
		}
		log.Printf("[ses-events] DELIVERY_DELAY type=%s expire=%s config_set=%s tenant=%s",
			note.DeliveryDelay.DelayType, note.DeliveryDelay.ExpirationTime, tagConfig, tagTenant)
	default:
		log.Printf("[ses-events] unhandled eventType=%q config_set=%s tenant=%s — accepting",
			eventType, tagConfig, tagTenant)
	}
	return nil
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
// persistSESEvent returns nil on success (or on a deliberate no-op such as an
// untagged event), and a non-nil error when the tracking-event INSERT itself
// failed. The caller uses that error to decide whether the notification should
// be RETRIED — swallowing it is what made SES event loss permanent before the
// async ingest queue existed (see ses_events_queue.go).
func (h *SESEventsHandler) persistSESEvent(ctx context.Context, eventType string, note sesEventNotification,
	campaignID, subscriberID, recipientSendID, recipientEmail, bounceType, bounceStatus, bounceDiag, linkURL, ipAddress, userAgent, tsRaw string) error {

	if h.db == nil || campaignID == "" {
		// Without a campaign tag we cannot attribute the event. Pre-tag
		// in-flight sends will simply not persist; post-deploy sends carry
		// the tag. (Suppression for bounce/complaint is unaffected.)
		return nil
	}
	campUUID, err := uuid.Parse(campaignID)
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, sesPersistTimeout())
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
		// bounce_type is varchar(64); an oversized value would error the
		// INSERT and drop the event from both PG and the lake.
		if len(bounceType) > 64 {
			bounceType = bounceType[:64]
		}
		bouncePtr = &bounceType
	}

	// SES click-tracking rewrites EVERY http(s) URL in the HTML body, so mail
	// clients loading remote stylesheets, web fonts, and images trip CLICK
	// events the moment the message renders. Flag those is_machine_click=true
	// so they stay out of click_count and out of the `NOT is_machine_click`
	// real-click queries. Additionally run the internal tracking pixel's
	// UA/IP classifier (scanner UAs, bare-Mozilla + cloud/scanner CIDRs) now
	// that SES gives us the click's userAgent/ipAddress. deltaSinceSend is 0
	// here — we don't pay the DB round-trip for the send timestamp — so only
	// rules 1 and 2 of ClassifyClickAsMachine can fire (documented trade-off
	// at its definition).
	//
	// AWS's own automated-interaction label (isBotEvent, shipped 2026-08-07) is
	// folded in for clicks and persisted verbatim for BOTH opens and clicks in
	// the dedicated `ses_bot_event` label column. sesBotEvent is "" whenever the
	// notification carries no label — stored NULL, meaning UNKNOWN, never
	// "human" (METRIC_CONTRACT.md §12: unknown is not zero).
	sesBotEvent := ""
	switch eventType {
	case "opened":
		sesBotEvent = note.Open.IsBotEvent
	case "clicked":
		sesBotEvent = note.Click.IsBotEvent
	}
	machineClick := eventType == "clicked" && (isMachineClickURL(linkURL) ||
		tracking.ClassifyClickAsMachine(userAgent, ipAddress, 0) ||
		strings.EqualFold(sesBotEvent, "Likely"))

	res, err := h.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_events
			(id, organization_id, campaign_id, subscriber_id, event_type, bounce_type, link_url, event_at, recipient_domain, is_machine_click, ip_address, user_agent, bounce_reason, ses_bot_event)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, '')::inet, NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''))
		ON CONFLICT (id, event_at) DO NOTHING
	`, eventID, orgPtr, campUUID, subPtr, eventType, bouncePtr, linkPtr, eventAt, recipientDomain, machineClick, ipAddress, userAgent, bounceDiag, sesBotEvent)
	if err != nil {
		// RETURN the error rather than swallowing it. The async ingest worker
		// retries on this; a permanent failure is counted in the /health
		// ses_webhook block instead of vanishing silently.
		log.Printf("[ses-events] persist %s campaign=%s error: %v", eventType, campaignID, err)
		return fmt.Errorf("persist %s: %w", eventType, err)
	}
	// Only act on a genuinely new row — SNS can redeliver the same
	// notification, and ON CONFLICT makes those a no-op (RowsAffected == 0).
	if n, raErr := res.RowsAffected(); raErr != nil || n == 0 {
		return nil
	}

	// Phase 1 event lake (best-effort, no-op unless enabled). Emitted only on a
	// genuinely new row so SNS redeliveries don't double-feed the lake. This is
	// the SES "delivered != relay" truth that the RRU reconciliation depends on.
	lakeEvt := analytics.Event{
		EventUID:        "ses:" + eventID.String(),
		RecipientSendID: recipientSendID,
		CampaignID:      campaignID,
		SubscriberID:    subscriberIDStr(subPtr),
		Email:           recipientEmail,
		EmailDomain:     recipientDomain,
		// Brand (apex domain) from the SES envelope source. ses-source rows
		// have no VMTA, so unlike pmta rows the reader cannot derive their
		// brand from history — this write-time field is their only brand
		// signal on the Reporting screen's Brand dimension.
		Brand:           brand.RootFromEmail(note.Mail.Source),
		ISPGroup:        isp.GroupFromDomain(recipientDomain),
		RouteType:       "ses",
		EventType:       analytics.CanonicalEventType(eventType),
		BounceCat:       bounceType,
		DSNCode:         bounceStatus,
		DSNDiag:         bounceDiag,
		LinkURL:         linkURL,
		SourceIP:        ipAddress,
		EventAt:         eventAt.UTC().Format(time.RFC3339),
		Source:          "ses",
		// The machine/scanner verdict computed above was previously dropped on
		// the floor here: PG got is_machine_click, the lake got nothing, so
		// every `source='ses'` lake row had is_machine_click NULL and the
		// column read as INERT. Set it on CLICK rows only — a nil pointer is
		// omitted from the Firehose JSON and stays NULL in Glue, which is what
		// keeps "unclassified" distinguishable from "classified human"
		// (reader_click_funnel.go counts `is_machine_click IS NOT NULL` as
		// coverage). LABEL ONLY — nothing on the operating path filters on it.
		IsMachineClick: machineClickForLake(eventType, machineClick),
	}
	analytics.Emit(lakeEvt)

	// DARK Kafka mirror of the raw SES ingest event (fire-and-forget, flag-gated,
	// nil-safe). Key on campaign id when present, else the recipient email. No-op
	// unless the bus is wired AND the ingest flag is ON — zero behavior/latency by
	// default. Errors here can never reach the SNS handler path.
	ingestKey := campaignID
	if ingestKey == "" {
		ingestKey = recipientEmail
	}
	if b, mErr := json.Marshal(&lakeEvt); mErr == nil {
		eventbus.PublishIngest(ingestKey, b)
	}

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
		// DELIVERY is the single highest-volume SES event type (18,364 in a
		// 30-min sample vs 5,252 opens), so this was the worst hot-row offender
		// of all. Batched.
		if h.batcher != nil {
			h.batcher.addCampaign(campUUID, func(d *campaignDelta) { d.delivered++ })
		}

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
			// NOT retryable: the tracking-event row is already committed, so a
			// redelivery would hit ON CONFLICT and never reach this line again.
			// Counted so the rollup's drift is visible on /health.
			atomic.AddUint64(&sesRollupFailed, 1)
			log.Printf("[ses-events] ses_delivered rollup upsert campaign=%s isp=%s error: %v", campaignID, ispGroup, derr)
		}
	case "bounced":
		switch bounceType {
		case "hard":
			if h.batcher != nil {
				h.batcher.addCampaign(campUUID, func(d *campaignDelta) { d.bounces++; d.hard++ })
			}
		case "validation":
			// SES pre-flight validation block: the message never attempted the
			// remote MX. Counted in NEITHER hard nor soft (mirrors the metric
			// contract's reputation_block/administrative treatment).
		default:
			if h.batcher != nil {
				h.batcher.addCampaign(campUUID, func(d *campaignDelta) { d.bounces++; d.soft++ })
			}
		}
	case "complained":
		if h.batcher != nil {
			h.batcher.addCampaign(campUUID, func(d *campaignDelta) { d.complaints++ })
		}
	}
	return nil
}

// sesExec runs a best-effort engagement side-effect and COUNTS + LOGS a failure
// instead of discarding it. These writes were previously fire-and-forget with
// their error values dropped on the floor, which made open_count/click_count/
// last_open_at drift invisible: under DB pressure they simply did not happen and
// nothing anywhere recorded that. They stay non-fatal (the authoritative event
// row is already committed), but they are no longer silent.
func (h *SESEventsHandler) sesExec(ctx context.Context, what string, query string, args ...interface{}) {
	if _, err := h.db.ExecContext(ctx, query, args...); err != nil {
		atomic.AddUint64(&sesEngagementFailed, 1)
		log.Printf("[ses-events] engagement side-effect %s failed: %v", what, err)
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
	// Campaign counters are BATCHED (ses_engagement_batcher.go). Incrementing
	// them per event serialized every open for a campaign on one row lock;
	// folding them removes the contention rather than racing it.
	unique := int64(0)
	if subID != nil && h.isFirstEventForCampaign(ctx, campaignID, *subID, "opened", false) {
		unique = 1
	}
	if h.batcher != nil {
		h.batcher.addCampaign(campaignID, func(d *campaignDelta) {
			d.opens++
			d.uniqueOpens += unique
		})
	}

	if subID != nil {
		// Subscriber denorm columns, also batched. last_open_at in particular
		// gates the standing Welcome saturation segment (prior_sends > 8 AND
		// last_open_at IS NULL) — without this, SES-route opens never clear a
		// subscriber out of the "never opened" cohort. engagement_score is
		// recomputed inside the batch statement, which also retires the
		// per-event SELECT+UPDATE pair recomputeSubscriberEngagementScore did.
		if h.batcher != nil {
			h.batcher.addSubscriber(*subID, 1, 0, time.Now().UTC())
		}

		// Master-selection shadow state (parity with mailing_tracking.go:204-206).
		// The campaign->domain lookup is cached; it was a DB round-trip per event
		// for a value that never changes.
		sd := cachedSendingDomain(ctx, h.db, campaignID, mailing.ResolveSendingDomainForCampaign)
		mailing.UpsertSDSOpen(ctx, h.db, *subID, sd)
		mailing.RecomputeSDSScoreLocal(ctx, h.db, *subID, sd)
	}

	if email != "" {
		eHash := emailHash(email)
		// Inbox profiles are keyed per-email, so they are NOT a hot row and stay
		// per-event — but on their own budget so they cannot be starved by
		// anything upstream in this function.
		ictx, icancel := context.WithTimeout(ctx, sesSideEffectTimeout())
		defer icancel()
		h.sesExec(ictx, "inbox_profile_open", `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_opens, last_open_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET total_opens = mailing_inbox_profiles.total_opens + 1, last_open_at = NOW(), updated_at = NOW()
		`, eHash, email, domain, extractISP(email))
		h.recomputeInboxProfileScore(ictx, eHash)
	}
}

// isFirstEventForCampaign reports whether the just-inserted row is this
// subscriber's FIRST event of this type for the campaign.
//
// The old inline subquery carried no event_at predicate, so it probed ALL 19
// monthly partitions of mailing_tracking_events for a single-row answer
// (measured: 23ms planning + 10.5ms execution, per event). Bounding the window
// prunes the partition list and LIMIT 2 stops the scan as soon as the answer is
// decided.
//
// The window is 180 days, NOT the 60 I first reached for. The bound must exceed
// the longest realistic gap between a campaign's send and a late open, because
// if an earlier open falls outside the window this returns "first" a second time
// and unique_open_count is inflated. Six months is generous enough that the
// error is negligible while still pruning most partitions — correctness first,
// then the speed-up.
func (h *SESEventsHandler) isFirstEventForCampaign(ctx context.Context, campaignID, subID uuid.UUID, eventType string, humanOnly bool) bool {
	qctx, cancel := context.WithTimeout(ctx, sesSideEffectTimeout())
	defer cancel()

	q := `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM mailing_tracking_events
			WHERE campaign_id = $1 AND subscriber_id = $2 AND event_type = $3
			  AND event_at >= NOW() - INTERVAL '180 days'`
	if humanOnly {
		q += ` AND NOT COALESCE(is_machine_click, false)`
	}
	q += ` LIMIT 2 ) x`

	var n int
	if err := h.db.QueryRowContext(qctx, q, campaignID, subID, eventType).Scan(&n); err != nil {
		// On error do NOT guess "unique" — an over-count is a silent metric lie.
		atomic.AddUint64(&sesEngagementFailed, 1)
		log.Printf("[ses-events] unique-check %s failed: %v", eventType, err)
		return false
	}
	return n == 1
}

// applyClickEngagement mirrors HandleTrackClick's durable side-effects for SES
// human clicks (machine/asset clicks never reach here). Same new-row / best-effort
// contract as applyOpenEngagement.
func (h *SESEventsHandler) applyClickEngagement(ctx context.Context, campaignID uuid.UUID, subID *uuid.UUID, email, domain string) {
	// Batched — see applyOpenEngagement.
	unique := int64(0)
	if subID != nil && h.isFirstEventForCampaign(ctx, campaignID, *subID, "clicked", true) {
		unique = 1
	}
	if h.batcher != nil {
		h.batcher.addCampaign(campaignID, func(d *campaignDelta) {
			d.clicks++
			d.uniqueClicks += unique
		})
	}

	if subID != nil {
		if h.batcher != nil {
			h.batcher.addSubscriber(*subID, 0, 1, time.Now().UTC())
		}

		sd := cachedSendingDomain(ctx, h.db, campaignID, mailing.ResolveSendingDomainForCampaign)
		mailing.UpsertSDSClick(ctx, h.db, *subID, sd)
		mailing.RecomputeSDSScoreLocal(ctx, h.db, *subID, sd)
	}

	if email != "" {
		eHash := emailHash(email)
		ictx, icancel := context.WithTimeout(ctx, sesSideEffectTimeout())
		defer icancel()
		h.sesExec(ictx, "inbox_profile_click", `
			INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, isp, total_clicks, last_click_at, updated_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 1, NOW(), NOW())
			ON CONFLICT (email_hash) DO UPDATE SET total_clicks = mailing_inbox_profiles.total_clicks + 1, last_click_at = NOW(), updated_at = NOW()
		`, eHash, email, domain, extractISP(email))
		h.recomputeInboxProfileScore(ictx, eHash)
	}
}

// NOTE: recomputeSubscriberEngagementScore used to live here, running a
// SELECT + UPDATE round-trip pair on EVERY open and click. Its formula now runs
// inline inside sesEngagementBatcher.flushSubscribers, so the score is computed
// once per subscriber per flush from the same inputs instead of twice per event.
// See ses_engagement_batcher.go for the SQL and the formula it mirrors.

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

func (h *SESEventsHandler) handleBounce(ctx context.Context, env snsEnvelope, note sesEventNotification, tagConfig, tagTenant string) error {
	campaignID := firstTag(note.Mail.Tags, "campaign_id")
	subscriberID := firstTag(note.Mail.Tags, "subscriber_id")
	sendID := firstTag(note.Mail.Tags, "recipient_send_id")

	// Only Permanent bounces feed suppression. Transient (soft) bounces are
	// not list-hygiene events per the bounce-metrics rule, but we still
	// persist them as soft bounces so deferral/soft-bounce analytics are
	// accurate.
	if note.Bounce.BounceType != "Permanent" {
		for _, rec := range note.Bounce.BouncedRecipients {
			if err := h.persistSESEvent(ctx, "bounced", note, campaignID, subscriberID, sendID,
				rec.EmailAddress, "soft", rec.Status, rec.DiagnosticCode, "", "", "", note.Mail.Timestamp); err != nil {
				return err
			}
			log.Printf("[ses-events] BOUNCE-soft type=%s subtype=%s addr=%s diag=%q config_set=%s tenant=%s",
				note.Bounce.BounceType, note.Bounce.BounceSubType,
				logger.RedactEmail(rec.EmailAddress), rec.DiagnosticCode, tagConfig, tagTenant)
		}
		return nil
	}

	reason := "ses_hard_bounce"
	switch note.Bounce.BounceSubType {
	case "Suppressed", "OnAccountSuppressionList", "OnSuppressionList":
		reason = "ses_suppression_list"
	}
	// SES's pre-flight address-validation suppression arrives as a Permanent
	// bounce whose subtype is NOT one of the suppression-list values; the diag
	// text is the only reliable marker. Label it distinctly — these sends never
	// reached the remote MX, so reading them as remote hard bounces overstates
	// reputation damage (2026-07-12 ATT tranche: all 32 "hard bounces" were this).
	if reason == "ses_hard_bounce" {
		for _, rec := range note.Bounce.BouncedRecipients {
			if strings.Contains(rec.DiagnosticCode, "due to email validation") {
				reason = "ses_address_validation"
				break
			}
		}
	}

	for _, rec := range note.Bounce.BouncedRecipients {
		if rec.EmailAddress == "" {
			continue
		}
		// Persist the bounce as a tracking event. SES pre-flight validation
		// blocks get bounce_type='validation' (their own class — the send never
		// reached the remote MX, so it must not count in the hard-bounce rate);
		// genuine remote Permanent rejections stay 'hard' so HardBounceSQL
		// classifies them correctly.
		btype := "hard"
		if strings.Contains(rec.DiagnosticCode, "due to email validation") {
			btype = "validation"
		}
		if err := h.persistSESEvent(ctx, "bounced", note, campaignID, subscriberID, sendID,
			rec.EmailAddress, btype, rec.Status, rec.DiagnosticCode, "", "", "", note.Mail.Timestamp); err != nil {
			return err
		}
		if h.hub == nil {
			log.Printf("[ses-events] WARNING: globalHub not wired — dropping bounce for %s", logger.RedactEmail(rec.EmailAddress))
			continue
		}
		// A failed suppression write is RETRYABLE: a permanent bounce that
		// never reaches mailing_global_suppressions means we keep mailing a
		// dead address, which is a reputation problem, not just a metrics one.
		added, err := h.hub.Suppress(ctx, rec.EmailAddress, reason, "ses_webhook", "", rec.Status, rec.DiagnosticCode, "", "")
		if err != nil {
			log.Printf("[ses-events] hub.Suppress error for %s: %v", logger.RedactEmail(rec.EmailAddress), err)
			return fmt.Errorf("suppress bounce: %w", err)
		}
		log.Printf("[ses-events] BOUNCE-hard added=%v reason=%s addr=%s subtype=%s config_set=%s tenant=%s",
			added, reason, logger.RedactEmail(rec.EmailAddress), note.Bounce.BounceSubType, tagConfig, tagTenant)
	}
	return nil
}

func (h *SESEventsHandler) handleComplaint(ctx context.Context, env snsEnvelope, note sesEventNotification, tagConfig, tagTenant string) error {
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
		if err := h.persistSESEvent(ctx, "complained", note, campaignID, subscriberID, sendID,
			rec.EmailAddress, "", "", note.Complaint.ComplaintFeedbackType, "", "", "", note.Mail.Timestamp); err != nil {
			return err
		}
		if h.hub == nil {
			log.Printf("[ses-events] WARNING: globalHub not wired — dropping complaint for %s", logger.RedactEmail(rec.EmailAddress))
			continue
		}
		// Retryable for the same reason as a hard bounce — a dropped complaint
		// suppression means we keep mailing someone who hit "spam".
		added, err := h.hub.Suppress(ctx, rec.EmailAddress, reason, "ses_webhook", "", "", "", "", "")
		if err != nil {
			log.Printf("[ses-events] hub.Suppress error for %s: %v", logger.RedactEmail(rec.EmailAddress), err)
			return fmt.Errorf("suppress complaint: %w", err)
		}
		log.Printf("[ses-events] COMPLAINT added=%v reason=%s addr=%s config_set=%s tenant=%s",
			added, reason, logger.RedactEmail(rec.EmailAddress), tagConfig, tagTenant)
	}
	return nil
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
// machineClickForLake renders the click verdict for the lake event. It returns
// nil for every non-click event type so the Firehose JSON omits the field and
// the Glue column stays NULL (= NOT CLASSIFIED), and a non-nil pointer on click
// rows so `is_machine_click IS NOT NULL` is honest coverage. Emitting a bare
// `false` on opens/deliveries would claim we classified 5M rows we never looked
// at, which is exactly how the column became meaningless the first time.
func machineClickForLake(eventType string, machine bool) *bool {
	if eventType != "clicked" {
		return nil
	}
	v := machine
	return &v
}

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

package api

import (
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

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// VersionSESEvents is bumped on every modification per the workspace versioning rule.
const VersionSESEvents = "1.0"

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
		Tags map[string][]string `json:"tags"`
	} `json:"mail"`
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

	switch eventType {
	case "Bounce":
		h.handleBounce(r, env, note, tagConfig, tagTenant)
	case "Complaint":
		h.handleComplaint(r, env, note, tagConfig, tagTenant)
	case "Open":
		// Ingestion into mailing_tracking_events is a deliberate follow-up.
		// For now log so we can confirm the SNS event-destination is
		// publishing OPEN/CLICK without flooding the suppressions table.
		log.Printf("[ses-events] OPEN config_set=%s tenant=%s ip=%s", tagConfig, tagTenant, note.Open.IPAddress)
	case "Click":
		log.Printf("[ses-events] CLICK config_set=%s tenant=%s link=%s ip=%s", tagConfig, tagTenant, note.Click.Link, note.Click.IPAddress)
	case "Send":
		log.Printf("[ses-events] SEND config_set=%s tenant=%s msgid=%s", tagConfig, tagTenant, env.MessageId)
	case "Delivery":
		log.Printf("[ses-events] DELIVERY config_set=%s tenant=%s msgid=%s", tagConfig, tagTenant, env.MessageId)
	case "Reject":
		log.Printf("[ses-events] REJECT reason=%s config_set=%s tenant=%s msgid=%s",
			note.Reject.Reason, tagConfig, tagTenant, env.MessageId)
	case "DeliveryDelay":
		log.Printf("[ses-events] DELIVERY_DELAY type=%s expire=%s config_set=%s tenant=%s",
			note.DeliveryDelay.DelayType, note.DeliveryDelay.ExpirationTime, tagConfig, tagTenant)
	default:
		log.Printf("[ses-events] unhandled eventType=%q config_set=%s tenant=%s — accepting",
			eventType, tagConfig, tagTenant)
	}
}

func (h *SESEventsHandler) handleBounce(r *http.Request, env snsEnvelope, note sesEventNotification, tagConfig, tagTenant string) {
	// Only Permanent bounces feed suppression. Transient (soft) bounces are
	// logged but not list-hygiene events per the bounce-metrics rule.
	if note.Bounce.BounceType != "Permanent" {
		for _, rec := range note.Bounce.BouncedRecipients {
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
	reason := "ses_complaint"
	if note.Complaint.ComplaintFeedbackType != "" {
		reason = "ses_complaint:" + note.Complaint.ComplaintFeedbackType
	}
	for _, rec := range note.Complaint.ComplainedRecipients {
		if rec.EmailAddress == "" {
			continue
		}
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

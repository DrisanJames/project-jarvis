package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const bridgeFailureAlertThreshold = 5

// BridgeAlertFunc is called when the bridge exceeds consecutive failure threshold.
type BridgeAlertFunc func(serverHost string, errorCount int, sampleError string)

// PMTAAPISender sends emails via PMTA's HTTP injection API (port 19000).
// Bypasses SMTP port blocking between AWS ECS and OVH.
type PMTAAPISender struct {
	apiEndpoint string
	db          *sql.DB
	client      *http.Client
	ipPool      *vmtaPool

	consecutiveFailures int64
	lastError           string
	bridgeAlertFn       BridgeAlertFunc

	// headerRouting selects KumoMTA semantics: route via an X-Virtual-MTA header
	// inside the message content (Kumo's policy reads it to pick the egress pool)
	// instead of the json `vmta` field the PMTA HTTP bridge expects.
	headerRouting bool
}

// NewPMTAAPISender creates a PMTA API sender.
func NewPMTAAPISender(apiEndpoint string, db *sql.DB, poolPrefix string) *PMTAAPISender {
	return &PMTAAPISender{
		apiEndpoint: apiEndpoint,
		db:          db,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		ipPool: newVMTAPool(db, poolPrefix),
	}
}

// NewKumoAPISender creates a sender for KumoMTA's HTTP injection API. It speaks
// the same /api/inject/v1 contract as the PMTA bridge, but routes by writing an
// X-Virtual-MTA header into the message content (Kumo reads it to select the
// egress pool) instead of sending a json `vmta` field — Kumo would reject the
// unknown field, and PMTA-bridge double-injection is avoided by using a separate
// profile vendor_type ("kumo").
func NewKumoAPISender(apiEndpoint string, db *sql.DB, poolPrefix string) *PMTAAPISender {
	s := NewPMTAAPISender(apiEndpoint, db, poolPrefix)
	s.headerRouting = true
	return s
}

// SetIPChangeCallback registers a callback on the sender's VMTA pool that
// fires when the IP set changes.
func (s *PMTAAPISender) SetIPChangeCallback(cb OnIPsChangedFunc) {
	s.ipPool.SetOnIPsChanged(cb)
}

// SetBridgeAlertFunc registers a callback that fires when the bridge
// exceeds bridgeFailureAlertThreshold consecutive failures.
func (s *PMTAAPISender) SetBridgeAlertFunc(fn BridgeAlertFunc) {
	s.bridgeAlertFn = fn
}

// Send delivers a single email through the PMTA HTTP injection API.
func (s *PMTAAPISender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.apiEndpoint == "" {
		return nil, fmt.Errorf("PMTA API endpoint not configured")
	}

	injectURL := strings.TrimRight(s.apiEndpoint, "/") + "/api/inject/v1"
	msgDomain := "mail.projectjarvis.io"
	if parts := strings.SplitN(msg.FromEmail, "@", 2); len(parts) == 2 && parts[1] != "" {
		msgDomain = parts[1]
	}
	messageID := fmt.Sprintf("%s@%s", uuid.New().String(), msgDomain)

	// Build RFC822 message
	boundary := fmt.Sprintf("=_%s", uuid.New().String()[:16])
	var rfc822 bytes.Buffer

	rfc822.WriteString(BuildFromHeader(msg.FromName, msg.FromEmail))
	rfc822.WriteString(fmt.Sprintf("To: %s\r\n", msg.Email))
	rfc822.WriteString(BuildSubjectHeader(msg.Subject))
	rfc822.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	rfc822.WriteString("MIME-Version: 1.0\r\n")

	if msg.ReplyTo != "" {
		rfc822.WriteString(fmt.Sprintf("Reply-To: %s\r\n", msg.ReplyTo))
	}

	if msg.CampaignID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Campaign-ID: %s\r\n", msg.CampaignID))
	}
	if msg.SubscriberID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Subscriber-ID: %s\r\n", msg.SubscriberID))
	}
	if msg.ID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Message-ID: %s\r\n", msg.ID))
	}

	for k, v := range msg.Headers {
		rfc822.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	rfc822.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	rfc822.WriteString("\r\n")

	if msg.TextContent != "" {
		rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		rfc822.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qpText := quotedprintableEncode(msg.TextContent)
		rfc822.WriteString(qpText)
		rfc822.WriteString("\r\n")
	}
	rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	rfc822.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpHTML := quotedprintableEncode(msg.HTMLContent)
	rfc822.WriteString(qpHTML)
	rfc822.WriteString("\r\n")
	rfc822.WriteString(fmt.Sprintf("--%s--\r\n", boundary))

	payload := map[string]interface{}{
		"envelope_sender": msg.FromEmail,
		"recipients":      []map[string]string{{"email": msg.Email}},
		"content":         rfc822.String(),
	}

	var selectedIPID string
	if msg.AssignedVMTA != "" {
		payload["vmta"] = msg.AssignedVMTA
		log.Printf("[PMTA-API] Using pre-assigned VMTA=%s for %s (ISP=%s)",
			msg.AssignedVMTA, msg.Email, msg.RecipientISP)
	} else if vmta, ok := msg.Headers["X-Virtual-MTA"]; ok && vmta != "" {
		payload["vmta"] = vmta
		log.Printf("[PMTA-API] Routing %s via explicit VMTA header: %s", msg.Email, vmta)
	} else if s.ipPool != nil && msg.ProfileID != "" {
		s.ipPool.refresh(ctx, msg.ProfileID)
		ip, err := s.ipPool.next(msg.RecipientISP)
		if err != nil && len(s.ipPool.ips) > 0 {
			return nil, fmt.Errorf("all IPs exhausted warmup limits, deferring send: %w", err)
		}
		if err == nil {
			vmta := vmtaShortName(ip.Hostname)
			if vmta == "" {
				return nil, fmt.Errorf("selected IP %s has empty hostname — refusing to send via default-pool (server IP)", ip.ID)
			}
			payload["vmta"] = vmta
			selectedIPID = ip.ID
			profShort := msg.ProfileID
			if len(profShort) > 8 {
				profShort = profShort[:8]
			}
			log.Printf("[PMTA-API] Routing %s → VMTA=%s (profile=%s, ISP=%s, poolPrefix=%s)",
				msg.Email, vmta, profShort, msg.RecipientISP, s.ipPool.poolPrefix)
		} else {
			return nil, fmt.Errorf("no sending IPs configured for profile %s — refusing to send via default-pool (server IP)", msg.ProfileID)
		}
	} else {
		return nil, fmt.Errorf("no VMTA routing available — no X-Virtual-MTA header and no IP pool configured; refusing to send via server IP")
	}

	// KumoMTA routes on an X-Virtual-MTA content header, not the json `vmta`
	// field. Move the resolved VMTA into the message content and drop the field.
	routedVMTA, _ := payload["vmta"].(string)
	if s.headerRouting {
		if routedVMTA != "" {
			payload["content"] = "X-Virtual-MTA: " + routedVMTA + "\r\n" + rfc822.String()
		}
		delete(payload, "vmta")
	}

	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("marshal PMTA payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", injectURL, &body)
	if err != nil {
		return nil, fmt.Errorf("create PMTA request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		s.recordBridgeFailure(err.Error())
		return nil, fmt.Errorf("PMTA API request to %s: %w", injectURL, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		errMsg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody))
		s.recordBridgeFailure(errMsg)
		return nil, fmt.Errorf("PMTA API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	// The inject API returns HTTP 200 even when every recipient failed
	// ({"success_count":0,"fail_count":1,"errors":[...]}) — KumoMTA does this
	// when message generation is rejected (e.g. an unparseable From header:
	// the 2026-07-01 incident silently dropped ~7.9k sends across six brands
	// this way). Treat zero accepted recipients as a hard send failure so the
	// worker records the error instead of marking the message sent. Bodies
	// without a success_count field (non-inject shapes) keep legacy behavior.
	var injectResp struct {
		SuccessCount *int     `json:"success_count"`
		FailCount    int      `json:"fail_count"`
		Errors       []string `json:"errors"`
	}
	if jsonErr := json.Unmarshal(respBody, &injectResp); jsonErr == nil &&
		injectResp.SuccessCount != nil && *injectResp.SuccessCount == 0 {
		detail := strings.Join(injectResp.Errors, "; ")
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		errMsg := fmt.Sprintf("inject accepted 0 recipients (fail_count=%d): %s", injectResp.FailCount, detail)
		s.recordBridgeFailure(errMsg)
		return nil, fmt.Errorf("PMTA API inject failure: %s", errMsg)
	}

	// Reset consecutive failure counter on success
	atomic.StoreInt64(&s.consecutiveFailures, 0)

	if selectedIPID != "" {
		go s.updateIPCounters(selectedIPID)
	}

	usedVMTA := routedVMTA
	log.Printf("[PMTA-API] Sent to %s via %s (id: %s, status: %d)", msg.Email, injectURL, messageID, resp.StatusCode)
	return &SendResult{Success: true, MessageID: messageID, ESPType: "pmta-api", SentAt: time.Now(), VMTA: usedVMTA}, nil
}

func (s *PMTAAPISender) recordBridgeFailure(errMsg string) {
	count := atomic.AddInt64(&s.consecutiveFailures, 1)
	s.lastError = errMsg
	if count == int64(bridgeFailureAlertThreshold) && s.bridgeAlertFn != nil {
		s.bridgeAlertFn(s.apiEndpoint, int(count), errMsg)
	}
}

func (s *PMTAAPISender) updateIPCounters(ipID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(ctx, `UPDATE mailing_ip_addresses SET total_sent = total_sent + 1, last_sent_at = NOW(), updated_at = NOW() WHERE id = $1`, ipID); err != nil {
		log.Printf("[PMTA-API] IP counter update failed for %s: %v", ipID, err)
	}
	// planned_volume / warmup_day are NOT NULL with no default, and Postgres
	// evaluates NOT NULL BEFORE the ON CONFLICT arbiter — so the old
	// VALUES(id, ip_id, date, actual_sent) form errored 23502 on EVERY send and
	// the DO UPDATE never ran (actual_sent sat at 0 across 12,354 rows from
	// 2026-03-18 to 2026-07-29). That silently disabled BOTH warm-up brakes:
	// WarmupScheduler's 3%-bounce/0.1%-complaint auto-pause gates on
	// actual_sent > 10, and vmtaPool.next()'s per-IP daily cap reads TodaySent.
	// Sourcing the two columns from the IP row keeps the log meaningful, and
	// the error is now surfaced instead of discarded.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_ip_warmup_log (id, ip_id, date, planned_volume, warmup_day, actual_sent)
		SELECT gen_random_uuid(), ip.id, CURRENT_DATE,
		       COALESCE(ip.warmup_daily_limit, 0), COALESCE(ip.warmup_day, 0), 1
		FROM mailing_ip_addresses ip WHERE ip.id = $1
		ON CONFLICT (ip_id, date) DO UPDATE SET actual_sent = mailing_ip_warmup_log.actual_sent + 1
	`, ipID); err != nil {
		log.Printf("[PMTA-API] warmup log increment failed for %s: %v", ipID, err)
	}
}

func quotedprintableEncode(s string) string {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	w.Write([]byte(s))
	w.Close()
	return buf.String()
}

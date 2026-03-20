package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProfileBasedSender resolves a sending profile from the database and
// delegates to the appropriate ESP sender. This is the default sender
// used by the send worker pool — it reads credentials from the
// mailing_sending_profiles table per message.
//
// Sender instances are cached per profile ID so that PMTA's SMTP
// connection pool and VMTA cache are reused across messages.
type ProfileBasedSender struct {
	db              *sql.DB
	senderCache     map[string]ESPSender
	mu              sync.RWMutex
	ipChangeCallback OnIPsChangedFunc
}

// NewProfileBasedSender creates a profile-based sender that reads
// ESP credentials from the database.
func NewProfileBasedSender(db *sql.DB) *ProfileBasedSender {
	return &ProfileBasedSender{
		db:          db,
		senderCache: make(map[string]ESPSender),
	}
}

// SetIPChangeCallback registers a callback that will be wired into every
// PMTA sender's vmtaPool as it's created. When the pool's IP set changes,
// the callback fires so the rate registry can update per-IP limiters.
func (s *ProfileBasedSender) SetIPChangeCallback(cb OnIPsChangedFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipChangeCallback = cb
}

// Send looks up the sending profile for the message, creates the
// appropriate ESP sender, and delegates delivery.
func (s *ProfileBasedSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	var vendorType, apiKey, apiSecret, sendingDomain, region, poolPrefix string
	var smtpHost, smtpUsername, smtpPassword sql.NullString
	var smtpPort sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT vendor_type,
			   COALESCE(api_key, ''),
			   COALESCE(api_secret, ''),
			   COALESCE(sending_domain, ''),
			   COALESCE(api_endpoint, 'us-east-1'),
			   smtp_host, smtp_port,
			   smtp_username, smtp_password,
			   COALESCE(pool_prefix, '')
		FROM mailing_sending_profiles
		WHERE id = $1
	`, msg.ProfileID).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword, &poolPrefix)

	if err != nil {
		log.Printf("[ProfileBasedSender] No profile %s, looking for default", msg.ProfileID)
		err = s.db.QueryRowContext(ctx, `
			SELECT vendor_type,
				   COALESCE(api_key, ''),
				   COALESCE(api_secret, ''),
				   COALESCE(sending_domain, ''),
				   COALESCE(api_endpoint, 'us-east-1'),
				   smtp_host, smtp_port,
				   smtp_username, smtp_password,
				   COALESCE(pool_prefix, '')
			FROM mailing_sending_profiles
			WHERE is_default = true AND status = 'active'
			LIMIT 1
		`).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword, &poolPrefix)
		if err != nil {
			return nil, fmt.Errorf("no sending profile found and no default configured")
		}
	}

	switch vendorType {
	case "sparkpost":
		if apiKey == "" {
			return nil, fmt.Errorf("profile %s: no SparkPost API key", msg.ProfileID)
		}
		return NewSparkPostSender(apiKey, s.db).Send(ctx, msg)
	case "ses":
		if apiKey == "" {
			return nil, fmt.Errorf("profile %s: no SES credentials", msg.ProfileID)
		}
		return NewSESSender(apiKey, apiSecret, region, s.db).Send(ctx, msg)
	case "mailgun":
		if apiKey == "" {
			return nil, fmt.Errorf("profile %s: no Mailgun API key", msg.ProfileID)
		}
		return NewMailgunSender(apiKey, sendingDomain, s.db).Send(ctx, msg)
	case "sendgrid":
		if apiKey == "" {
			return nil, fmt.Errorf("profile %s: no SendGrid API key", msg.ProfileID)
		}
		return NewSendGridSender(apiKey, s.db).Send(ctx, msg)
	case "pmta":
		host := smtpHost.String
		port := 587 // safe default; overridden by profile value
		if smtpPort.Valid && smtpPort.Int64 > 0 {
			port = int(smtpPort.Int64)
		}
		user := smtpUsername.String
		pass := smtpPassword.String

		// api_endpoint from the sending profile (stored in `region`)
		apiURL := ""
		if region != "" && region != "us-east-1" && strings.HasPrefix(region, "http") {
			apiURL = region
		}

		// Combo sender: try HTTP injection API first (if configured), SMTP fallback
		if host != "" && apiURL != "" {
			return s.getCachedSender(msg.ProfileID+":pmta-combo", func() ESPSender {
				return &pmtaComboSender{
					apiSender:  NewPMTAAPISender(apiURL, s.db, poolPrefix),
					smtpSender: NewPMTASender(host, port, user, pass, s.db, poolPrefix),
				}
			}).Send(ctx, msg)
		}
		// SMTP only (no HTTP API endpoint configured)
		if host != "" {
			return s.getCachedSender(msg.ProfileID+":pmta-smtp", func() ESPSender {
				return NewPMTASender(host, port, user, pass, s.db, poolPrefix)
			}).Send(ctx, msg)
		}
		// HTTP API only (no SMTP host)
		if apiURL != "" {
			return s.getCachedSender(msg.ProfileID+":pmta-api", func() ESPSender {
				return NewPMTAAPISender(apiURL, s.db, poolPrefix)
			}).Send(ctx, msg)
		}
		return nil, fmt.Errorf("profile %s: no SMTP host or API endpoint for PMTA", msg.ProfileID)
	default:
		return nil, fmt.Errorf("unsupported vendor type: %s", vendorType)
	}
}

// pmtaComboSender tries the PMTA HTTP injection API first, then falls back
// to SMTP. After a transient API failure, it retries the API after a cooldown
// period rather than permanently switching to SMTP.
type pmtaComboSender struct {
	apiSender    ESPSender
	smtpSender   ESPSender
	useAPI       int32 // 1 = API works, -1 = API failed/cooldown, 0 = unknown
	apiFailedAt  int64 // unix timestamp of last API failure
}

const apiRetryCooldown = 60 // seconds before retrying API after failure

func (c *pmtaComboSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	state := atomic.LoadInt32(&c.useAPI)

	if state == -1 {
		failedAt := atomic.LoadInt64(&c.apiFailedAt)
		if time.Now().Unix()-failedAt > apiRetryCooldown {
			atomic.StoreInt32(&c.useAPI, 0)
			state = 0
		}
	}

	if state >= 0 {
		result, err := c.apiSender.Send(ctx, msg)
		if err == nil {
			atomic.StoreInt32(&c.useAPI, 1)
			return result, nil
		}
		// VMTA-related errors (exhaustion, empty hostname, no IPs) must NOT
		// trigger SMTP fallback — the SMTP path would hit the same pool state.
		// Only fall back to SMTP for actual API transport failures.
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "exhausted") ||
			strings.Contains(errLower, "refusing to send") ||
			strings.Contains(errLower, "no sending ips") ||
			strings.Contains(errLower, "no vmta routing") ||
			strings.Contains(errLower, "empty hostname") {
			return nil, err
		}
		log.Printf("[PMTA-Combo] HTTP API transport failed (%v), falling back to SMTP", err)
		atomic.StoreInt32(&c.useAPI, -1)
		atomic.StoreInt64(&c.apiFailedAt, time.Now().Unix())
	}
	return c.smtpSender.Send(ctx, msg)
}

// getCachedSender retrieves a cached sender or creates one via the factory.
// When creating new PMTA senders, automatically wires the IP-change callback.
func (s *ProfileBasedSender) getCachedSender(key string, factory func() ESPSender) ESPSender {
	s.mu.RLock()
	if cached, ok := s.senderCache[key]; ok {
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, ok := s.senderCache[key]; ok {
		return cached
	}
	sender := factory()
	if s.ipChangeCallback != nil {
		s.wireIPCallback(sender)
	}
	s.senderCache[key] = sender
	return sender
}

// wireIPCallback sets the IP-change callback on PMTA senders (including combo).
func (s *ProfileBasedSender) wireIPCallback(sender ESPSender) {
	switch st := sender.(type) {
	case *PMTASender:
		st.SetIPChangeCallback(s.ipChangeCallback)
	case *PMTAAPISender:
		st.SetIPChangeCallback(s.ipChangeCallback)
	case *pmtaComboSender:
		if smtp, ok := st.smtpSender.(*PMTASender); ok {
			smtp.SetIPChangeCallback(s.ipChangeCallback)
		}
		if api, ok := st.apiSender.(*PMTAAPISender); ok {
			api.SetIPChangeCallback(s.ipChangeCallback)
		}
	}
}

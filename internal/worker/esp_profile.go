package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// ProfileBasedSender resolves a sending profile from the database and
// delegates to the appropriate ESP sender. This is the default sender
// used by the send worker pool — it reads credentials from the
// mailing_sending_profiles table per message.
//
// Sender instances are cached per profile ID so that PMTA's SMTP
// connection pool and VMTA cache are reused across messages.
type ProfileBasedSender struct {
	db          *sql.DB
	senderCache map[string]ESPSender
	mu          sync.RWMutex
}

// NewProfileBasedSender creates a profile-based sender that reads
// ESP credentials from the database.
func NewProfileBasedSender(db *sql.DB) *ProfileBasedSender {
	return &ProfileBasedSender{
		db:          db,
		senderCache: make(map[string]ESPSender),
	}
}

// Send looks up the sending profile for the message, creates the
// appropriate ESP sender, and delegates delivery.
func (s *ProfileBasedSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	var vendorType, apiKey, apiSecret, sendingDomain, region string
	var smtpHost, smtpUsername, smtpPassword sql.NullString
	var smtpPort sql.NullInt64

	err := s.db.QueryRowContext(ctx, `
		SELECT vendor_type,
			   COALESCE(api_key, ''),
			   COALESCE(api_secret, ''),
			   COALESCE(sending_domain, ''),
			   COALESCE(api_endpoint, 'us-east-1'),
			   smtp_host, smtp_port,
			   smtp_username, smtp_password
		FROM mailing_sending_profiles
		WHERE id = $1
	`, msg.ProfileID).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword)

	if err != nil {
		log.Printf("[ProfileBasedSender] No profile %s, looking for default", msg.ProfileID)
		err = s.db.QueryRowContext(ctx, `
			SELECT vendor_type,
				   COALESCE(api_key, ''),
				   COALESCE(api_secret, ''),
				   COALESCE(sending_domain, ''),
				   COALESCE(api_endpoint, 'us-east-1'),
				   smtp_host, smtp_port,
				   smtp_username, smtp_password
			FROM mailing_sending_profiles
			WHERE is_default = true AND status = 'active'
			LIMIT 1
		`).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword)
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
		port := 587
		if smtpPort.Valid && smtpPort.Int64 > 0 {
			port = int(smtpPort.Int64)
		}
		user := smtpUsername.String
		pass := smtpPassword.String

		// Determine HTTP API endpoint (explicit or derived from SMTP host)
		apiURL := ""
		if region != "" && region != "us-east-1" && strings.HasPrefix(region, "http") {
			apiURL = region
		} else if host != "" {
			apiURL = fmt.Sprintf("http://%s:19000", host)
		}

		// Always use combo sender: HTTP API first, SMTP fallback
		if host != "" {
			return s.getCachedSender(msg.ProfileID+":pmta-combo", func() ESPSender {
				return &pmtaComboSender{
					apiSender:  NewPMTAAPISender(apiURL, s.db),
					smtpSender: NewPMTASender(host, port, user, pass, s.db),
				}
			}).Send(ctx, msg)
		}
		// No SMTP host — try HTTP API alone
		if apiURL != "" {
			return s.getCachedSender(msg.ProfileID+":pmta-api", func() ESPSender {
				return NewPMTAAPISender(apiURL, s.db)
			}).Send(ctx, msg)
		}
		return nil, fmt.Errorf("profile %s: no SMTP host or API endpoint for PMTA", msg.ProfileID)
	default:
		return nil, fmt.Errorf("unsupported vendor type: %s", vendorType)
	}
}

// pmtaComboSender tries the PMTA HTTP injection API first, then falls back
// to SMTP. This ensures delivery works even when AWS blocks port 25 or the
// PMTA HTTP API isn't available on the target host.
type pmtaComboSender struct {
	apiSender  ESPSender
	smtpSender ESPSender
	useAPI     int32 // 1 = API works, -1 = API failed/skip, 0 = unknown
}

func (c *pmtaComboSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if atomic.LoadInt32(&c.useAPI) >= 0 {
		result, err := c.apiSender.Send(ctx, msg)
		if err == nil {
			atomic.StoreInt32(&c.useAPI, 1)
			return result, nil
		}
		log.Printf("[PMTA-Combo] HTTP API failed (%v), falling back to SMTP", err)
		atomic.StoreInt32(&c.useAPI, -1)
	}
	return c.smtpSender.Send(ctx, msg)
}

// getCachedSender retrieves a cached sender or creates one via the factory.
func (s *ProfileBasedSender) getCachedSender(key string, factory func() ESPSender) ESPSender {
	s.mu.RLock()
	if cached, ok := s.senderCache[key]; ok {
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after write lock
	if cached, ok := s.senderCache[key]; ok {
		return cached
	}
	sender := factory()
	s.senderCache[key] = sender
	return sender
}

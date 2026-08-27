package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
)

// ProfileBasedSender resolves a sending profile from the database and
// delegates to the appropriate ESP sender. This is the default sender
// used by the send worker pool — it reads credentials from the
// mailing_sending_profiles table per message.
//
// Sender instances are cached per profile ID so that PMTA's SMTP
// connection pool and VMTA cache are reused across messages.
type ProfileBasedSender struct {
	db               *sql.DB
	senderCache      map[string]ESPSender
	mu               sync.RWMutex
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
	var vendorType, apiKey, apiSecret, sendingDomain, region, poolPrefix, ipPool, routingMode string
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
			   COALESCE(pool_prefix, ''),
			   COALESCE(ip_pool, ''),
			   COALESCE(routing_mode, '')
		FROM mailing_sending_profiles
		WHERE id = $1
	`, msg.ProfileID).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword, &poolPrefix, &ipPool, &routingMode)

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
				   COALESCE(pool_prefix, ''),
				   COALESCE(ip_pool, ''),
				   COALESCE(routing_mode, '')
			FROM mailing_sending_profiles
			WHERE is_default = true AND status = 'active'
			LIMIT 1
		`).Scan(&vendorType, &apiKey, &apiSecret, &sendingDomain, &region, &smtpHost, &smtpPort, &smtpUsername, &smtpPassword, &poolPrefix, &ipPool, &routingMode)
		if err != nil {
			return nil, fmt.Errorf("no sending profile found and no default configured")
		}
	}

	// Bare-pool fallback: when a profile has no pool_prefix (so no per-ISP
	// VMTA pools like db-gmail-pool / db-general-pool) but does have ip_pool
	// set to a literal PMTA virtual-mta-pool name (e.g. "ses-relay-a"), use
	// that name directly as the X-Virtual-MTA hint. This is how SES-relay
	// and other "single-pool" profiles route — PMTA owns the underlying
	// fanout, the SaaS just tells PMTA which pool to use.
	//
	// Skipped when:
	//   - msg.AssignedVMTA already set by send_worker (ipAllocations path)
	//   - msg.Headers["X-Virtual-MTA"] already set upstream
	//   - poolPrefix non-empty (standard per-ISP routing wins)
	//   - ipPool empty (nothing to route to)
	if poolPrefix == "" && ipPool != "" && msg.AssignedVMTA == "" {
		alreadyHinted := false
		if msg.Headers != nil {
			if v, ok := msg.Headers["X-Virtual-MTA"]; ok && v != "" {
				alreadyHinted = true
			}
		}
		if !alreadyHinted {
			msg.AssignedVMTA = ipPool
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

		apiURL := ""
		if region != "" && region != "us-east-1" && strings.HasPrefix(region, "http") {
			apiURL = region
		}

		// API-only injection: when an API endpoint is configured, always
		// use it. SMTP fallback is intentionally removed — a delayed send
		// (requeue) is safer than an SMTP injection that risks DMARC failure.
		if apiURL != "" {
			// routing_mode='kumo' selects the KumoMTA injector (X-Virtual-MTA
			// content header) instead of the PMTA HTTP bridge (json vmta field).
			// The profile keeps vendor_type='pmta' so every deploy/preflight/wave
			// gate (which filters vendor_type='pmta') accepts it unchanged.
			if routingMode == "kumo" {
				return s.getCachedSender(msg.ProfileID+":kumo-api", func() ESPSender {
					return NewKumoAPISender(apiURL, s.db, poolPrefix)
				}).Send(ctx, msg)
			}
			return s.getCachedSender(msg.ProfileID+":pmta-api", func() ESPSender {
				return NewPMTAAPISender(apiURL, s.db, poolPrefix)
			}).Send(ctx, msg)
		}
		// SMTP-only: legacy path for profiles without an API endpoint.
		if host != "" {
			log.Printf("[ProfileBasedSender] WARN: profile %s has no api_endpoint, using SMTP-only (DMARC risk)", msg.ProfileID)
			return s.getCachedSender(msg.ProfileID+":pmta-smtp", func() ESPSender {
				return NewPMTASender(host, port, user, pass, s.db, poolPrefix)
			}).Send(ctx, msg)
		}
		return nil, fmt.Errorf("profile %s: no SMTP host or API endpoint for PMTA", msg.ProfileID)
	default:
		return nil, fmt.Errorf("unsupported vendor type: %s", vendorType)
	}
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

// wireIPCallback sets the IP-change callback on PMTA senders.
func (s *ProfileBasedSender) wireIPCallback(sender ESPSender) {
	switch st := sender.(type) {
	case *PMTASender:
		st.SetIPChangeCallback(s.ipChangeCallback)
	case *PMTAAPISender:
		st.SetIPChangeCallback(s.ipChangeCallback)
	}
}

package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// ProfileBasedSender resolves a sending profile from the database and
// delegates to the appropriate ESP sender. This is the default sender
// used by the send worker pool — it reads credentials from the
// mailing_sending_profiles table per message.
type ProfileBasedSender struct {
	db          *sql.DB
	senderCache map[string]ESPSender
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
		if region != "" && region != "us-east-1" && strings.HasPrefix(region, "http") {
			// api_endpoint is stored in the 'region' variable (COALESCE(api_endpoint, 'us-east-1'))
			sender := NewPMTAAPISender(region, s.db)
			return sender.Send(ctx, msg)
		}
		host := smtpHost.String
		if host == "" {
			return nil, fmt.Errorf("profile %s: no SMTP host for PMTA", msg.ProfileID)
		}
		port := 25
		if smtpPort.Valid && smtpPort.Int64 > 0 {
			port = int(smtpPort.Int64)
		}
		user := smtpUsername.String
		pass := smtpPassword.String
		return NewPMTASender(host, port, user, pass, s.db).Send(ctx, msg)
	default:
		return nil, fmt.Errorf("unsupported vendor type: %s", vendorType)
	}
}

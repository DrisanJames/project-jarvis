package mailing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TrackingDomainService manages custom tracking domains for organizations
type TrackingDomainService struct {
	db              *sql.DB
	platformDomain  string // e.g., "tracking.yourplatform.com"
	defaultTracking string // default tracking URL when no custom domain
	awsInfra        *AWSInfrastructureService // AWS infrastructure for SSL/CloudFront
	originServer    string // Origin server for CloudFront (API server)
}

// TrackingDomain represents a custom tracking domain configuration
type TrackingDomain struct {
	ID               string      `json:"id"`
	OrgID            string      `json:"org_id"`
	Domain           string      `json:"domain"`          // e.g., "track.example.com"
	Verified         bool        `json:"verified"`
	SSLProvisioned   bool        `json:"ssl_provisioned"`
	SSLStatus        string      `json:"ssl_status"`        // pending, validating, active, failed
	CloudFrontID     string      `json:"cloudfront_id,omitempty"`
	CloudFrontDomain string      `json:"cloudfront_domain,omitempty"`
	ACMCertARN       string      `json:"acm_cert_arn,omitempty"`
	OriginServer     string      `json:"origin_server,omitempty"`
	DNSRecords       []DNSRecord `json:"dns_records"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// DNSRecord represents a DNS record required for domain verification
type DNSRecord struct {
	Type   string `json:"type"`   // CNAME, TXT
	Name   string `json:"name"`   // Full record name
	Value  string `json:"value"`  // Expected value
	Status string `json:"status"` // pending, verified
}

// DNSRecordsJSON is a helper type for JSON marshaling/unmarshaling
type DNSRecordsJSON []DNSRecord

// Scan implements the sql.Scanner interface for DNSRecordsJSON
func (d *DNSRecordsJSON) Scan(value interface{}) error {
	if value == nil {
		*d = []DNSRecord{}
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("cannot scan type %T into DNSRecordsJSON", value)
	}
	return json.Unmarshal(b, d)
}

// NewTrackingDomainService creates a new TrackingDomainService
func NewTrackingDomainService(db *sql.DB, platformDomain, defaultTracking string) *TrackingDomainService {
	return &TrackingDomainService{
		db:              db,
		platformDomain:  platformDomain,
		defaultTracking: defaultTracking,
	}
}

// NewTrackingDomainServiceWithAWS creates a TrackingDomainService with AWS infrastructure support
func NewTrackingDomainServiceWithAWS(db *sql.DB, platformDomain, defaultTracking, originServer string, awsInfra *AWSInfrastructureService) *TrackingDomainService {
	return &TrackingDomainService{
		db:              db,
		platformDomain:  platformDomain,
		defaultTracking: defaultTracking,
		awsInfra:        awsInfra,
		originServer:    originServer,
	}
}

// SetAWSInfrastructure sets the AWS infrastructure service
func (s *TrackingDomainService) SetAWSInfrastructure(awsInfra *AWSInfrastructureService, originServer string) {
	s.awsInfra = awsInfra
	s.originServer = originServer
}

// RegisterDomain registers a new custom tracking domain for an organization
func (s *TrackingDomainService) RegisterDomain(ctx context.Context, orgID, domain string) (*TrackingDomain, error) {
	// Validate domain format
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := validateDomainFormat(domain); err != nil {
		return nil, err
	}

	// Check if domain already exists
	var existingID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM mailing_tracking_domains WHERE domain = $1
	`, domain).Scan(&existingID)
	if err == nil {
		return nil, fmt.Errorf("domain %s is already registered", domain)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("failed to check existing domain: %w", err)
	}

	// Generate verification token
	verifyToken, err := generateVerificationToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate verification token: %w", err)
	}

	// Create DNS records
	dnsRecords := []DNSRecord{
		{
			Type:   "CNAME",
			Name:   domain,
			Value:  s.platformDomain,
			Status: "pending",
		},
		{
			Type:   "TXT",
			Name:   fmt.Sprintf("_verify.%s", domain),
			Value:  fmt.Sprintf("verify=%s", verifyToken),
			Status: "pending",
		},
	}

	dnsRecordsJSON, err := json.Marshal(dnsRecords)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DNS records: %w", err)
	}

	// Insert into database
	id := uuid.New().String()
	now := time.Now()

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_tracking_domains (id, org_id, domain, verified, ssl_provisioned, ssl_status, origin_server, dns_records, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id, orgID, domain, false, false, "pending", s.originServer, dnsRecordsJSON, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create tracking domain: %w", err)
	}

	return &TrackingDomain{
		ID:             id,
		OrgID:          orgID,
		Domain:         domain,
		Verified:       false,
		SSLProvisioned: false,
		SSLStatus:      "pending",
		OriginServer:   s.originServer,
		DNSRecords:     dnsRecords,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// RegisterDomainWithAWS registers a domain and provisions AWS infrastructure
func (s *TrackingDomainService) RegisterDomainWithAWS(ctx context.Context, orgID, domain string) (*TrackingDomain, error) {
	// First register the domain
	td, err := s.RegisterDomain(ctx, orgID, domain)
	if err != nil {
		return nil, err
	}

	// If AWS infrastructure is configured, start provisioning
	if s.awsInfra != nil && s.originServer != "" {
		go func() {
			provCtx := context.Background()
			err := s.awsInfra.ProvisionTrackingDomain(provCtx, orgID, domain, s.originServer)
			if err != nil {
				fmt.Printf("AWS provisioning failed for domain %s: %v\n", domain, err)
				// Update status to failed
				s.db.ExecContext(provCtx, `
					UPDATE mailing_tracking_domains SET ssl_status = 'failed' WHERE id = $1
				`, td.ID)
			}
		}()
		td.SSLStatus = "provisioning"
	}

	return td, nil
}

// VerifyDNS verifies the DNS records for a tracking domain
func (s *TrackingDomainService) VerifyDNS(ctx context.Context, domainID string) (*TrackingDomain, error) {
	// Get the domain from database
	td, err := s.getDomainByID(ctx, domainID)
	if err != nil {
		return nil, err
	}

	// Verify each DNS record
	allVerified := true
	for i := range td.DNSRecords {
		record := &td.DNSRecords[i]
		verified := false

		switch record.Type {
		case "CNAME":
			verified = s.verifyCNAME(record.Name, record.Value)
		case "TXT":
			verified = s.verifyTXT(record.Name, record.Value)
		}

		if verified {
			record.Status = "verified"
		} else {
			record.Status = "pending"
			allVerified = false
		}
	}

	// Update the domain in database
	dnsRecordsJSON, err := json.Marshal(td.DNSRecords)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal DNS records: %w", err)
	}

	now := time.Now()
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_tracking_domains 
		SET verified = $1, dns_records = $2, updated_at = $3
		WHERE id = $4
	`, allVerified, dnsRecordsJSON, now, domainID)
	if err != nil {
		return nil, fmt.Errorf("failed to update tracking domain: %w", err)
	}

	td.Verified = allVerified
	td.UpdatedAt = now

	// If fully verified, trigger SSL provisioning (async in production)
	if allVerified && !td.SSLProvisioned {
		go s.provisionSSL(td.ID, td.Domain)
	}

	return td, nil
}

// GetOrgDomains returns all tracking domains for an organization
func (s *TrackingDomainService) GetOrgDomains(ctx context.Context, orgID string) ([]TrackingDomain, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, org_id, domain, verified, ssl_provisioned, 
		       COALESCE(ssl_status, 'pending'), cloudfront_id, cloudfront_domain, acm_cert_arn, origin_server,
		       dns_records, created_at, updated_at
		FROM mailing_tracking_domains
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to query tracking domains: %w", err)
	}
	defer rows.Close()

	var domains []TrackingDomain
	for rows.Next() {
		var td TrackingDomain
		var dnsRecordsJSON DNSRecordsJSON
		var sslStatus, cloudfrontID, cloudfrontDomain, acmCertARN, originServer sql.NullString

		err := rows.Scan(
			&td.ID,
			&td.OrgID,
			&td.Domain,
			&td.Verified,
			&td.SSLProvisioned,
			&sslStatus,
			&cloudfrontID,
			&cloudfrontDomain,
			&acmCertARN,
			&originServer,
			&dnsRecordsJSON,
			&td.CreatedAt,
			&td.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan tracking domain: %w", err)
		}

		td.DNSRecords = dnsRecordsJSON
		td.SSLStatus = sslStatus.String
		td.CloudFrontID = cloudfrontID.String
		td.CloudFrontDomain = cloudfrontDomain.String
		td.ACMCertARN = acmCertARN.String
		td.OriginServer = originServer.String
		domains = append(domains, td)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating tracking domains: %w", err)
	}

	return domains, nil
}

// GetTrackingURL returns the tracking URL for an organization
// It returns the custom tracking domain if one is verified, otherwise the default
func (s *TrackingDomainService) GetTrackingURL(ctx context.Context, orgID string) (string, error) {
	// Try to get a verified custom domain with active SSL
	var domain string
	var sslProvisioned bool
	var sslStatus sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT domain, ssl_provisioned, ssl_status 
		FROM mailing_tracking_domains 
		WHERE org_id = $1 AND verified = true
		ORDER BY created_at ASC
		LIMIT 1
	`, orgID).Scan(&domain, &sslProvisioned, &sslStatus)

	if err == sql.ErrNoRows {
		// No custom domain, return default
		return s.defaultTracking, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to query tracking domain: %w", err)
	}

	// Return the custom domain URL
	// Use HTTPS if SSL is provisioned or status is active
	protocol := "http"
	if sslProvisioned || sslStatus.String == "active" {
		protocol = "https"
	}
	return fmt.Sprintf("%s://%s", protocol, domain), nil
}

// GetAWSStatus gets the AWS provisioning status for a tracking domain
func (s *TrackingDomainService) GetAWSStatus(ctx context.Context, domainID string) (map[string]interface{}, error) {
	td, err := s.getDomainByID(ctx, domainID)
	if err != nil {
		return nil, err
	}

	status := map[string]interface{}{
		"domain":            td.Domain,
		"ssl_status":        td.SSLStatus,
		"ssl_provisioned":   td.SSLProvisioned,
		"cloudfront_id":     td.CloudFrontID,
		"cloudfront_domain": td.CloudFrontDomain,
		"acm_cert_arn":      td.ACMCertARN,
		"verified":          td.Verified,
	}

	// If we have AWS infrastructure service, get more details
	if s.awsInfra != nil && td.ACMCertARN != "" {
		certStatus, err := s.awsInfra.CheckCertificateStatus(ctx, td.ACMCertARN)
		if err == nil {
			status["acm_status"] = certStatus
		}
	}

	if s.awsInfra != nil && td.CloudFrontID != "" {
		distStatus, err := s.awsInfra.GetDistributionStatus(ctx, td.CloudFrontID)
		if err == nil {
			status["cloudfront_status"] = distStatus
		}
	}

	return status, nil
}

// RefreshAWSStatus refreshes the AWS status from AWS APIs and updates the database
func (s *TrackingDomainService) RefreshAWSStatus(ctx context.Context, domainID string) (*TrackingDomain, error) {
	td, err := s.getDomainByID(ctx, domainID)
	if err != nil {
		return nil, err
	}

	// Check ACM certificate status
	if s.awsInfra != nil && td.ACMCertARN != "" {
		certStatus, err := s.awsInfra.CheckCertificateStatus(ctx, td.ACMCertARN)
		if err == nil {
			if certStatus == "ISSUED" {
				td.SSLStatus = "active"
			} else if certStatus == "FAILED" || certStatus == "VALIDATION_TIMED_OUT" {
				td.SSLStatus = "failed"
			} else {
				td.SSLStatus = "validating"
			}
		}
	}

	// Check CloudFront distribution status
	if s.awsInfra != nil && td.CloudFrontID != "" {
		distStatus, err := s.awsInfra.GetDistributionStatus(ctx, td.CloudFrontID)
		if err == nil && distStatus == "Deployed" {
			td.SSLProvisioned = true
		}
	}

	// Update database
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_tracking_domains 
		SET ssl_status = $1, ssl_provisioned = $2, updated_at = NOW()
		WHERE id = $3
	`, td.SSLStatus, td.SSLProvisioned, domainID)
	if err != nil {
		return nil, fmt.Errorf("failed to update tracking domain: %w", err)
	}

	return td, nil
}

// DeleteDomain deletes a tracking domain
func (s *TrackingDomainService) DeleteDomain(ctx context.Context, domainID string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM mailing_tracking_domains WHERE id = $1
	`, domainID)
	if err != nil {
		return fmt.Errorf("failed to delete tracking domain: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("tracking domain not found")
	}

	return nil
}

// GetDomainByID returns a tracking domain by its ID
func (s *TrackingDomainService) GetDomainByID(ctx context.Context, domainID string) (*TrackingDomain, error) {
	return s.getDomainByID(ctx, domainID)
}

// getDomainByID is an internal helper to get a domain by ID
func (s *TrackingDomainService) getDomainByID(ctx context.Context, domainID string) (*TrackingDomain, error) {
	var td TrackingDomain
	var dnsRecordsJSON DNSRecordsJSON
	var sslStatus, cloudfrontID, cloudfrontDomain, acmCertARN, originServer sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, domain, verified, ssl_provisioned, 
		       COALESCE(ssl_status, 'pending'), cloudfront_id, cloudfront_domain, acm_cert_arn, origin_server,
		       dns_records, created_at, updated_at
		FROM mailing_tracking_domains
		WHERE id = $1
	`, domainID).Scan(
		&td.ID,
		&td.OrgID,
		&td.Domain,
		&td.Verified,
		&td.SSLProvisioned,
		&sslStatus,
		&cloudfrontID,
		&cloudfrontDomain,
		&acmCertARN,
		&originServer,
		&dnsRecordsJSON,
		&td.CreatedAt,
		&td.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("tracking domain not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query tracking domain: %w", err)
	}

	td.DNSRecords = dnsRecordsJSON
	td.SSLStatus = sslStatus.String
	td.CloudFrontID = cloudfrontID.String
	td.CloudFrontDomain = cloudfrontDomain.String
	td.ACMCertARN = acmCertARN.String
	td.OriginServer = originServer.String
	return &td, nil
}

// verifyCNAME checks if a CNAME record exists and points to the expected value
func (s *TrackingDomainService) verifyCNAME(name, expectedValue string) bool {
	cname, err := net.LookupCNAME(name)
	if err != nil {
		return false
	}

	// Remove trailing dot from CNAME result
	cname = strings.TrimSuffix(cname, ".")
	expectedValue = strings.TrimSuffix(expectedValue, ".")

	return strings.EqualFold(cname, expectedValue)
}

// verifyTXT checks if a TXT record exists with the expected value
func (s *TrackingDomainService) verifyTXT(name, expectedValue string) bool {
	records, err := net.LookupTXT(name)
	if err != nil {
		return false
	}

	for _, record := range records {
		if strings.Contains(record, expectedValue) || record == expectedValue {
			return true
		}
	}

	return false
}

// provisionSSL triggers SSL certificate provisioning for a verified domain
// In production, this would integrate with Let's Encrypt, Cloudflare, or similar
func (s *TrackingDomainService) provisionSSL(domainID, domain string) {
	// This is a placeholder for SSL provisioning logic
	// In production, this would:
	// 1. Request a certificate from Let's Encrypt or similar
	// 2. Configure the load balancer/reverse proxy
	// 3. Update the database once complete

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Simulate SSL provisioning delay
	time.Sleep(2 * time.Second)

	// Update the database (in production, only after successful provisioning)
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_tracking_domains 
		SET ssl_provisioned = true, updated_at = $1
		WHERE id = $2
	`, time.Now(), domainID)
	if err != nil {
		// Log error in production
		fmt.Printf("Failed to update SSL provisioned status for domain %s: %v\n", domain, err)
	}
}

// validateDomainFormat validates the format of a domain name
func validateDomainFormat(domain string) error {
	if domain == "" {
		return fmt.Errorf("domain cannot be empty")
	}

	if len(domain) > 253 {
		return fmt.Errorf("domain name too long (max 253 characters)")
	}

	// Check for valid characters and structure
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid domain format: must contain at least one dot")
	}

	for _, part := range parts {
		if len(part) == 0 {
			return fmt.Errorf("invalid domain format: empty label")
		}
		if len(part) > 63 {
			return fmt.Errorf("invalid domain format: label too long (max 63 characters)")
		}
		// Check first and last character
		if part[0] == '-' || part[len(part)-1] == '-' {
			return fmt.Errorf("invalid domain format: labels cannot start or end with hyphen")
		}
		// Check all characters
		for _, c := range part {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return fmt.Errorf("invalid domain format: invalid character '%c'", c)
			}
		}
	}

	return nil
}

// generateVerificationToken generates a random verification token
func generateVerificationToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// CheckDomainOwnership verifies that a domain belongs to the specified organization
func (s *TrackingDomainService) CheckDomainOwnership(ctx context.Context, domainID, orgID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_tracking_domains 
		WHERE id = $1 AND org_id = $2
	`, domainID, orgID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check domain ownership: %w", err)
	}
	return count > 0, nil
}

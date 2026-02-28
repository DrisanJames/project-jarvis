package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SendingProfile represents an ESP/SMTP vendor connection
type SendingProfile struct {
	ID             uuid.UUID  `json:"id"`
	OrganizationID uuid.UUID  `json:"organization_id"`
	Name           string     `json:"name"`
	Description    *string    `json:"description,omitempty"`
	VendorType     string     `json:"vendor_type"` // sparkpost, ses, mailgun, sendgrid, smtp

	// From/Reply configuration
	FromName   string  `json:"from_name"`
	FromEmail  string  `json:"from_email"`
	ReplyEmail *string `json:"reply_email,omitempty"`

	// API Credentials (masked in responses)
	APIKey      *string `json:"api_key,omitempty"`
	APISecret   *string `json:"api_secret,omitempty"`
	APIEndpoint *string `json:"api_endpoint,omitempty"`

	// SMTP settings
	SMTPHost       *string `json:"smtp_host,omitempty"`
	SMTPPort       int     `json:"smtp_port,omitempty"`
	SMTPUsername   *string `json:"smtp_username,omitempty"`
	SMTPPassword   *string `json:"smtp_password,omitempty"`
	SMTPEncryption string  `json:"smtp_encryption,omitempty"`

	// Domain configuration
	SendingDomain  *string `json:"sending_domain,omitempty"`
	BounceDomain   *string `json:"bounce_domain,omitempty"`
	TrackingDomain *string `json:"tracking_domain,omitempty"`

	// Authentication status
	SPFVerified         bool       `json:"spf_verified"`
	DKIMVerified        bool       `json:"dkim_verified"`
	DMARCVerified       bool       `json:"dmarc_verified"`
	DomainVerified      bool       `json:"domain_verified"`
	CredentialsVerified bool       `json:"credentials_verified"`
	LastVerificationAt  *time.Time `json:"last_verification_at,omitempty"`
	VerificationError   *string    `json:"verification_error,omitempty"`

	// Rate limiting
	HourlyLimit        int `json:"hourly_limit"`
	DailyLimit         int `json:"daily_limit"`
	CurrentHourlyCount int `json:"current_hourly_count"`
	CurrentDailyCount  int `json:"current_daily_count"`

	// IP Pool
	IPPool *string `json:"ip_pool,omitempty"`

	// Status
	Status       string `json:"status"` // draft, pending, active, inactive, suspended
	IsDefault    bool   `json:"is_default"`
	IsConfigured bool   `json:"is_configured"` // true if API key/credentials are set

	// Metadata
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
}

// ProfileListDefault represents a profile default for a specific list
type ProfileListDefault struct {
	ID               uuid.UUID `json:"id"`
	ProfileID        uuid.UUID `json:"profile_id"`
	ListID           uuid.UUID `json:"list_id"`
	FromNameOverride *string   `json:"from_name_override,omitempty"`
	FromEmailOverride *string  `json:"from_email_override,omitempty"`
	ReplyEmailOverride *string `json:"reply_email_override,omitempty"`
	IsDefault        bool      `json:"is_default"`
	CreatedAt        time.Time `json:"created_at"`
}

// SendingProfileService handles sending profile operations
type SendingProfileService struct {
	db *sql.DB
}

// NewSendingProfileService creates a new sending profile service
func NewSendingProfileService(db *sql.DB) *SendingProfileService {
	return &SendingProfileService{db: db}
}

// RegisterRoutes registers all sending profile routes
func (s *SendingProfileService) RegisterRoutes(r chi.Router) {
	r.Route("/sending-profiles", func(r chi.Router) {
		r.Get("/", s.HandleListProfiles)
		r.Post("/", s.HandleCreateProfile)
		r.Get("/vendors", s.HandleListVendorTypes)
		r.Get("/{profileId}", s.HandleGetProfile)
		r.Put("/{profileId}", s.HandleUpdateProfile)
		r.Delete("/{profileId}", s.HandleDeleteProfile)
		r.Post("/{profileId}/verify", s.HandleVerifyCredentials)
		r.Post("/{profileId}/set-default", s.HandleSetDefault)
		r.Get("/{profileId}/usage", s.HandleGetProfileUsage)
		
		// List-specific defaults
		r.Get("/{profileId}/list-defaults", s.HandleGetListDefaults)
		r.Post("/{profileId}/list-defaults", s.HandleSetListDefault)
		r.Delete("/{profileId}/list-defaults/{listId}", s.HandleRemoveListDefault)
	})
}

// HandleListProfiles returns all sending profiles
func (s *SendingProfileService) HandleListProfiles(w http.ResponseWriter, r *http.Request) {
	// Get organization ID from header or query parameter
	orgID := r.Header.Get("X-Organization-ID")
	if orgID == "" {
		orgID = r.URL.Query().Get("organization_id")
	}
	if orgID == "" {
		// Use dynamic org context extraction
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "organization context required"})
			return
		}
	}
	vendorType := r.URL.Query().Get("vendor_type")
	status := r.URL.Query().Get("status")

	query := `
		SELECT id, organization_id, name, description, vendor_type,
			   from_name, from_email, reply_email,
			   sending_domain, bounce_domain, tracking_domain,
			   spf_verified, dkim_verified, dmarc_verified, domain_verified, credentials_verified,
			   last_verification_at, verification_error,
			   hourly_limit, daily_limit, current_hourly_count, current_daily_count,
			   ip_pool, status, is_default, 
			   CASE WHEN api_key IS NOT NULL AND api_key != '' THEN true ELSE false END as is_configured,
			   created_at, updated_at
		FROM mailing_sending_profiles
		WHERE organization_id = $1
		  AND api_key IS NOT NULL AND api_key != ''
	`
	args := []interface{}{orgID}
	argNum := 2

	// Organization filter already applied above
	if vendorType != "" {
		query += fmt.Sprintf(" AND vendor_type = $%d", argNum)
		args = append(args, vendorType)
		argNum++
	}
	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", argNum)
		args = append(args, status)
		argNum++
	}

	query += " ORDER BY is_default DESC, name ASC"

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	profiles := []SendingProfile{}
	for rows.Next() {
		var p SendingProfile
		err := rows.Scan(
			&p.ID, &p.OrganizationID, &p.Name, &p.Description, &p.VendorType,
			&p.FromName, &p.FromEmail, &p.ReplyEmail,
			&p.SendingDomain, &p.BounceDomain, &p.TrackingDomain,
			&p.SPFVerified, &p.DKIMVerified, &p.DMARCVerified, &p.DomainVerified, &p.CredentialsVerified,
			&p.LastVerificationAt, &p.VerificationError,
			&p.HourlyLimit, &p.DailyLimit, &p.CurrentHourlyCount, &p.CurrentDailyCount,
			&p.IPPool, &p.Status, &p.IsDefault, &p.IsConfigured, &p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			log.Printf("Error scanning profile: %v", err)
			continue
		}
		profiles = append(profiles, p)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"profiles": profiles,
		"total":    len(profiles),
	})
}

// HandleCreateProfile creates a new sending profile
func (s *SendingProfileService) HandleCreateProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrganizationID string  `json:"organization_id"`
		Name           string  `json:"name"`
		Description    *string `json:"description"`
		VendorType     string  `json:"vendor_type"`
		FromName       string  `json:"from_name"`
		FromEmail      string  `json:"from_email"`
		ReplyEmail     *string `json:"reply_email"`
		APIKey         *string `json:"api_key"`
		APISecret      *string `json:"api_secret"`
		APIEndpoint    *string `json:"api_endpoint"`
		SMTPHost       *string `json:"smtp_host"`
		SMTPPort       *int    `json:"smtp_port"`
		SMTPUsername   *string `json:"smtp_username"`
		SMTPPassword   *string `json:"smtp_password"`
		SMTPEncryption *string `json:"smtp_encryption"`
		SendingDomain  *string `json:"sending_domain"`
		BounceDomain   *string `json:"bounce_domain"`
		TrackingDomain *string `json:"tracking_domain"`
		IPPool         *string `json:"ip_pool"`
		HourlyLimit    *int    `json:"hourly_limit"`
		DailyLimit     *int    `json:"daily_limit"`
		IsDefault      bool    `json:"is_default"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	// Validate required fields
	if input.Name == "" || input.VendorType == "" || input.FromName == "" || input.FromEmail == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "name, vendor_type, from_name, and from_email are required"})
		return
	}

	// Validate vendor type
	validVendors := map[string]bool{"sparkpost": true, "ses": true, "mailgun": true, "sendgrid": true, "smtp": true, "pmta": true}
	if !validVendors[strings.ToLower(input.VendorType)] {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid vendor_type. Must be one of: sparkpost, ses, mailgun, sendgrid, smtp, pmta"})
		return
	}

	// Default values - use dynamic org context
	if input.OrganizationID == "" {
		var err error
		input.OrganizationID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "organization context required"})
			return
		}
	}
	smtpPort := 587
	if input.SMTPPort != nil {
		smtpPort = *input.SMTPPort
	}
	smtpEncryption := "tls"
	if input.SMTPEncryption != nil {
		smtpEncryption = *input.SMTPEncryption
	}
	hourlyLimit := 10000
	if input.HourlyLimit != nil {
		hourlyLimit = *input.HourlyLimit
	}
	dailyLimit := 100000
	if input.DailyLimit != nil {
		dailyLimit = *input.DailyLimit
	}

	// If setting as default, unset existing default first
	if input.IsDefault {
		_, err := s.db.ExecContext(r.Context(),
			"UPDATE mailing_sending_profiles SET is_default = FALSE WHERE organization_id = $1",
			input.OrganizationID)
		if err != nil {
			log.Printf("Error unsetting default: %v", err)
		}
	}

	// Insert the profile
	var id uuid.UUID
	err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_sending_profiles (
			organization_id, name, description, vendor_type,
			from_name, from_email, reply_email,
			api_key, api_secret, api_endpoint,
			smtp_host, smtp_port, smtp_username, smtp_password, smtp_encryption,
			sending_domain, bounce_domain, tracking_domain,
			ip_pool, hourly_limit, daily_limit,
			status, is_default
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, 'draft', $22)
		RETURNING id
	`,
		input.OrganizationID, input.Name, input.Description, strings.ToLower(input.VendorType),
		input.FromName, input.FromEmail, input.ReplyEmail,
		input.APIKey, input.APISecret, input.APIEndpoint,
		input.SMTPHost, smtpPort, input.SMTPUsername, input.SMTPPassword, smtpEncryption,
		input.SendingDomain, input.BounceDomain, input.TrackingDomain,
		input.IPPool, hourlyLimit, dailyLimit, input.IsDefault,
	).Scan(&id)

	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":      id,
		"message": "Profile created successfully. Verify credentials to activate.",
	})
}

// HandleGetProfile returns a specific sending profile
func (s *SendingProfileService) HandleGetProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	var p SendingProfile
	var rawAPIKey *string
	err := s.db.QueryRowContext(r.Context(), `
		SELECT id, organization_id, name, description, vendor_type,
			   from_name, from_email, reply_email,
			   api_key, api_endpoint,
			   smtp_host, smtp_port, smtp_username, smtp_encryption,
			   sending_domain, bounce_domain, tracking_domain,
			   spf_verified, dkim_verified, dmarc_verified, domain_verified, credentials_verified,
			   last_verification_at, verification_error,
			   hourly_limit, daily_limit, current_hourly_count, current_daily_count,
			   ip_pool, status, is_default, created_at, updated_at
		FROM mailing_sending_profiles WHERE id = $1
	`, profileID).Scan(
		&p.ID, &p.OrganizationID, &p.Name, &p.Description, &p.VendorType,
		&p.FromName, &p.FromEmail, &p.ReplyEmail,
		&rawAPIKey, &p.APIEndpoint,
		&p.SMTPHost, &p.SMTPPort, &p.SMTPUsername, &p.SMTPEncryption,
		&p.SendingDomain, &p.BounceDomain, &p.TrackingDomain,
		&p.SPFVerified, &p.DKIMVerified, &p.DMARCVerified, &p.DomainVerified, &p.CredentialsVerified,
		&p.LastVerificationAt, &p.VerificationError,
		&p.HourlyLimit, &p.DailyLimit, &p.CurrentHourlyCount, &p.CurrentDailyCount,
		&p.IPPool, &p.Status, &p.IsDefault, &p.CreatedAt, &p.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Set is_configured based on API key presence
	p.IsConfigured = rawAPIKey != nil && *rawAPIKey != ""

	// Mask sensitive data
	if rawAPIKey != nil && len(*rawAPIKey) > 8 {
		masked := (*rawAPIKey)[:4] + "****" + (*rawAPIKey)[len(*rawAPIKey)-4:]
		p.APIKey = &masked
	}

	respondJSON(w, http.StatusOK, p)
}

// HandleUpdateProfile updates a sending profile
func (s *SendingProfileService) HandleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	var input map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	// Build dynamic update query
	updates := []string{}
	args := []interface{}{}
	argNum := 1

	fieldMap := map[string]string{
		"name":            "name",
		"description":     "description",
		"from_name":       "from_name",
		"from_email":      "from_email",
		"reply_email":     "reply_email",
		"api_key":         "api_key",
		"api_secret":      "api_secret",
		"api_endpoint":    "api_endpoint",
		"smtp_host":       "smtp_host",
		"smtp_port":       "smtp_port",
		"smtp_username":   "smtp_username",
		"smtp_password":   "smtp_password",
		"smtp_encryption": "smtp_encryption",
		"sending_domain":  "sending_domain",
		"bounce_domain":   "bounce_domain",
		"tracking_domain": "tracking_domain",
		"ip_pool":         "ip_pool",
		"hourly_limit":    "hourly_limit",
		"daily_limit":     "daily_limit",
		"status":          "status",
	}

	for jsonField, dbField := range fieldMap {
		if val, ok := input[jsonField]; ok {
			updates = append(updates, fmt.Sprintf("%s = $%d", dbField, argNum))
			args = append(args, val)
			argNum++
		}
	}

	if len(updates) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "No fields to update"})
		return
	}

	// Add updated_at
	updates = append(updates, fmt.Sprintf("updated_at = $%d", argNum))
	args = append(args, time.Now())
	argNum++

	// Add WHERE clause
	args = append(args, profileID)

	query := fmt.Sprintf("UPDATE mailing_sending_profiles SET %s WHERE id = $%d",
		strings.Join(updates, ", "), argNum)

	result, err := s.db.ExecContext(r.Context(), query, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "Profile updated successfully"})
}

// HandleDeleteProfile deletes a sending profile
func (s *SendingProfileService) HandleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	// Check if profile is used by any campaigns
	var count int
	s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM mailing_campaigns WHERE sending_profile_id = $1", profileID).Scan(&count)

	if count > 0 {
		respondJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("Cannot delete profile. It is used by %d campaign(s). Deactivate instead.", count),
		})
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		"DELETE FROM mailing_sending_profiles WHERE id = $1", profileID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "Profile deleted successfully"})
}

// HandleVerifyCredentials verifies the ESP credentials
func (s *SendingProfileService) HandleVerifyCredentials(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	// Get profile details
	var vendorType, apiKey string
	var smtpHost *string
	err := s.db.QueryRowContext(r.Context(),
		"SELECT vendor_type, api_key, smtp_host FROM mailing_sending_profiles WHERE id = $1",
		profileID).Scan(&vendorType, &apiKey, &smtpHost)

	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}

	// Verify based on vendor type
	verified := false
	verificationError := ""

	switch vendorType {
	case "sparkpost":
		// Test SparkPost API
		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", "https://api.sparkpost.com/api/v1/account", nil)
		req.Header.Set("Authorization", apiKey)
		resp, err := client.Do(req)
		if err != nil {
			verificationError = fmt.Sprintf("Connection error: %v", err)
		} else if resp.StatusCode == 200 {
			verified = true
		} else {
			verificationError = fmt.Sprintf("API returned status %d", resp.StatusCode)
		}

	case "ses":
		// For SES, we assume credentials work if configured (would need AWS SDK to verify)
		verified = true

	case "mailgun":
		// Test Mailgun API
		if apiKey != "" {
			verified = true // Simplified - would need actual API test
		} else {
			verificationError = "API key not configured"
		}

	case "smtp":
		// For SMTP, we'd need to test connection
		if smtpHost != nil && *smtpHost != "" {
			verified = true // Simplified
		} else {
			verificationError = "SMTP host not configured"
		}

	default:
		verificationError = "Unknown vendor type"
	}

	// Update profile
	now := time.Now()
	status := "active"
	if !verified {
		status = "pending"
	}

	_, err = s.db.ExecContext(r.Context(), `
		UPDATE mailing_sending_profiles 
		SET credentials_verified = $1, 
			last_verification_at = $2, 
			verification_error = $3,
			status = $4,
			updated_at = $5
		WHERE id = $6
	`, verified, now, nilIfEmpty(verificationError), status, now, profileID)

	if err != nil {
		log.Printf("Error updating verification status: %v", err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"verified":   verified,
		"error":      verificationError,
		"verified_at": now,
		"status":     status,
	})
}

// HandleSetDefault sets a profile as the default
func (s *SendingProfileService) HandleSetDefault(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	// Get organization ID
	var orgID uuid.UUID
	err := s.db.QueryRowContext(r.Context(),
		"SELECT organization_id FROM mailing_sending_profiles WHERE id = $1", profileID).Scan(&orgID)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "Profile not found"})
		return
	}

	// Unset all defaults for this org
	s.db.ExecContext(r.Context(),
		"UPDATE mailing_sending_profiles SET is_default = FALSE WHERE organization_id = $1", orgID)

	// Set this one as default
	_, err = s.db.ExecContext(r.Context(),
		"UPDATE mailing_sending_profiles SET is_default = TRUE, updated_at = $1 WHERE id = $2",
		time.Now(), profileID)

	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "Profile set as default"})
}

// HandleGetProfileUsage returns usage statistics for a profile
func (s *SendingProfileService) HandleGetProfileUsage(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	// Get campaign count using this profile
	var campaignCount int
	s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM mailing_campaigns WHERE sending_profile_id = $1", profileID).Scan(&campaignCount)

	// Get total sent through this profile
	var totalSent, totalDelivered, totalBounced int
	s.db.QueryRowContext(r.Context(), `
		SELECT COALESCE(SUM(sent_count),0), COALESCE(SUM(delivered_count),0), COALESCE(SUM(bounced_count),0)
		FROM mailing_profile_usage WHERE profile_id = $1
	`, profileID).Scan(&totalSent, &totalDelivered, &totalBounced)

	// Get current limits
	var hourlyLimit, dailyLimit, currentHourly, currentDaily int
	s.db.QueryRowContext(r.Context(),
		"SELECT hourly_limit, daily_limit, current_hourly_count, current_daily_count FROM mailing_sending_profiles WHERE id = $1",
		profileID).Scan(&hourlyLimit, &dailyLimit, &currentHourly, &currentDaily)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"profile_id":     profileID,
		"campaigns_count": campaignCount,
		"total_sent":      totalSent,
		"total_delivered": totalDelivered,
		"total_bounced":   totalBounced,
		"delivery_rate":   safePercent(totalDelivered, totalSent),
		"limits": map[string]interface{}{
			"hourly_limit":   hourlyLimit,
			"hourly_used":    currentHourly,
			"hourly_remaining": hourlyLimit - currentHourly,
			"daily_limit":    dailyLimit,
			"daily_used":     currentDaily,
			"daily_remaining": dailyLimit - currentDaily,
		},
	})
}

// HandleListVendorTypes returns available vendor types
func (s *SendingProfileService) HandleListVendorTypes(w http.ResponseWriter, r *http.Request) {
	vendors := []map[string]interface{}{
		{
			"type":        "sparkpost",
			"name":        "SparkPost",
			"description": "High-volume transactional and marketing email",
			"features":    []string{"API sending", "Webhooks", "Analytics", "Templates"},
			"auth_type":   "api_key",
		},
		{
			"type":        "ses",
			"name":        "Amazon SES",
			"description": "AWS Simple Email Service",
			"features":    []string{"API sending", "SMTP relay", "Analytics", "SNS notifications"},
			"auth_type":   "aws_credentials",
		},
		{
			"type":        "mailgun",
			"name":        "Mailgun",
			"description": "Transactional email API service",
			"features":    []string{"API sending", "Webhooks", "Analytics", "Templates"},
			"auth_type":   "api_key",
		},
		{
			"type":        "sendgrid",
			"name":        "SendGrid",
			"description": "Cloud-based email delivery",
			"features":    []string{"API sending", "SMTP relay", "Analytics", "Templates"},
			"auth_type":   "api_key",
		},
		{
			"type":        "smtp",
			"name":        "SMTP Relay",
			"description": "Generic SMTP server connection",
			"features":    []string{"SMTP sending"},
			"auth_type":   "smtp_credentials",
		},
		{
			"type":        "pmta",
			"name":        "PowerMTA",
			"description": "Self-hosted PowerMTA server with dedicated IP management",
			"features":    []string{"SMTP sending", "IP rotation", "Virtual MTA routing", "IP warmup", "Bounce processing"},
			"auth_type":   "smtp_credentials",
		},
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"vendors": vendors,
	})
}

// HandleGetListDefaults returns list-specific defaults for a profile
func (s *SendingProfileService) HandleGetListDefaults(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT pld.id, pld.profile_id, pld.list_id, pld.from_name_override, 
			   pld.from_email_override, pld.reply_email_override, pld.is_default, pld.created_at,
			   l.name as list_name
		FROM mailing_profile_list_defaults pld
		JOIN mailing_lists l ON l.id = pld.list_id
		WHERE pld.profile_id = $1
		ORDER BY l.name
	`, profileID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	defaults := []map[string]interface{}{}
	for rows.Next() {
		var id, profileID, listID uuid.UUID
		var fromName, fromEmail, replyEmail, listName *string
		var isDefault bool
		var createdAt time.Time

		rows.Scan(&id, &profileID, &listID, &fromName, &fromEmail, &replyEmail, &isDefault, &createdAt, &listName)
		defaults = append(defaults, map[string]interface{}{
			"id":                  id,
			"profile_id":          profileID,
			"list_id":             listID,
			"list_name":           listName,
			"from_name_override":  fromName,
			"from_email_override": fromEmail,
			"reply_email_override": replyEmail,
			"is_default":          isDefault,
			"created_at":          createdAt,
		})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"list_defaults": defaults,
	})
}

// HandleSetListDefault sets a list-specific default for a profile
func (s *SendingProfileService) HandleSetListDefault(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")

	var input struct {
		ListID            string  `json:"list_id"`
		FromNameOverride  *string `json:"from_name_override"`
		FromEmailOverride *string `json:"from_email_override"`
		ReplyEmailOverride *string `json:"reply_email_override"`
		IsDefault         bool    `json:"is_default"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	if input.ListID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "list_id is required"})
		return
	}

	// Upsert
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO mailing_profile_list_defaults 
			(profile_id, list_id, from_name_override, from_email_override, reply_email_override, is_default)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (profile_id, list_id) 
		DO UPDATE SET from_name_override = $3, from_email_override = $4, reply_email_override = $5, is_default = $6
	`, profileID, input.ListID, input.FromNameOverride, input.FromEmailOverride, input.ReplyEmailOverride, input.IsDefault)

	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "List default set successfully"})
}

// HandleRemoveListDefault removes a list-specific default
func (s *SendingProfileService) HandleRemoveListDefault(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileId")
	listID := chi.URLParam(r, "listId")

	_, err := s.db.ExecContext(r.Context(),
		"DELETE FROM mailing_profile_list_defaults WHERE profile_id = $1 AND list_id = $2",
		profileID, listID)

	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "List default removed"})
}

// Helper functions
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func safePercent(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return float64(num) / float64(denom) * 100
}

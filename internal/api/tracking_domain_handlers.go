package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// getOrgIDString returns the organization ID as a string using the dynamic org context
func getOrgIDString(r *http.Request) string {
	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil {
		return "" // Return empty string on error - caller should handle
	}
	return orgID
}

// getOrgUUID returns the organization ID as a UUID using the dynamic org context
func getOrgUUID(r *http.Request) uuid.UUID {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		return uuid.Nil // Return nil UUID on error - caller should handle
	}
	return orgID
}

// TrackingDomainHandlers contains handlers for tracking domain API endpoints
type TrackingDomainHandlers struct {
	service  *mailing.TrackingDomainService
	awsInfra *mailing.AWSInfrastructureService
	db       *sql.DB
}

// NewTrackingDomainHandlers creates a new TrackingDomainHandlers instance
func NewTrackingDomainHandlers(db *sql.DB, platformDomain, defaultTracking string) *TrackingDomainHandlers {
	return &TrackingDomainHandlers{
		service: mailing.NewTrackingDomainService(db, platformDomain, defaultTracking),
		db:      db,
	}
}

// NewTrackingDomainHandlersWithAWS creates handlers with AWS infrastructure support
func NewTrackingDomainHandlersWithAWS(db *sql.DB, platformDomain, defaultTracking, originServer string, awsInfra *mailing.AWSInfrastructureService) *TrackingDomainHandlers {
	svc := mailing.NewTrackingDomainService(db, platformDomain, defaultTracking)
	svc.SetAWSInfrastructure(awsInfra, originServer)
	return &TrackingDomainHandlers{
		service:  svc,
		awsInfra: awsInfra,
		db:       db,
	}
}

// SetAWSInfrastructure sets the AWS infrastructure service
func (h *TrackingDomainHandlers) SetAWSInfrastructure(awsInfra *mailing.AWSInfrastructureService, originServer string) {
	h.awsInfra = awsInfra
	h.service.SetAWSInfrastructure(awsInfra, originServer)
}

// GetService returns the tracking domain service
func (h *TrackingDomainHandlers) GetService() *mailing.TrackingDomainService {
	return h.service
}

// RegisterTrackingDomainRequest represents a request to register a new tracking domain
type RegisterTrackingDomainRequest struct {
	Domain string `json:"domain"`
}

// RegisterDomain handles POST /api/mailing/tracking-domains
// Registers a new custom tracking domain for the organization
func (h *TrackingDomainHandlers) RegisterDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req RegisterTrackingDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Domain == "" {
		writeJSONError(w, "domain is required", http.StatusBadRequest)
		return
	}

	// Get organization ID from context (in production, this would come from auth middleware)
	orgID := getOrgIDString(r)

	domain, err := h.service.RegisterDomain(ctx, orgID, req.Domain)
	if err != nil {
		// Check for specific error types
		if isConflictError(err) {
			writeJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		if isValidationError(err) {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONError(w, "failed to register domain: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(domain)
}

// ListDomains handles GET /api/mailing/tracking-domains
// Returns all tracking domains for the organization
func (h *TrackingDomainHandlers) ListDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)

	domains, err := h.service.GetOrgDomains(ctx, orgID)
	if err != nil {
		writeJSONError(w, "failed to fetch domains: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return empty array instead of null
	if domains == nil {
		domains = []mailing.TrackingDomain{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"domains": domains,
		"total":   len(domains),
	})
}

// VerifyDomain handles POST /api/mailing/tracking-domains/{id}/verify
// Verifies the DNS records for a tracking domain
func (h *TrackingDomainHandlers) VerifyDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		writeJSONError(w, "domain ID is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	// Check domain ownership
	owned, err := h.service.CheckDomainOwnership(ctx, domainID, orgID)
	if err != nil {
		writeJSONError(w, "failed to verify domain ownership: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !owned {
		writeJSONError(w, "domain not found or not owned by organization", http.StatusNotFound)
		return
	}

	domain, err := h.service.VerifyDNS(ctx, domainID)
	if err != nil {
		if isNotFoundError(err) {
			writeJSONError(w, "domain not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "failed to verify domain: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(domain)
}

// GetDomain handles GET /api/mailing/tracking-domains/{id}
// Returns a specific tracking domain
func (h *TrackingDomainHandlers) GetDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		writeJSONError(w, "domain ID is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	// Check domain ownership
	owned, err := h.service.CheckDomainOwnership(ctx, domainID, orgID)
	if err != nil {
		writeJSONError(w, "failed to verify domain ownership: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !owned {
		writeJSONError(w, "domain not found", http.StatusNotFound)
		return
	}

	domain, err := h.service.GetDomainByID(ctx, domainID)
	if err != nil {
		if isNotFoundError(err) {
			writeJSONError(w, "domain not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "failed to fetch domain: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(domain)
}

// DeleteDomain handles DELETE /api/mailing/tracking-domains/{id}
// Deletes a tracking domain
func (h *TrackingDomainHandlers) DeleteDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		writeJSONError(w, "domain ID is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	// Check domain ownership
	owned, err := h.service.CheckDomainOwnership(ctx, domainID, orgID)
	if err != nil {
		writeJSONError(w, "failed to verify domain ownership: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !owned {
		writeJSONError(w, "domain not found or not owned by organization", http.StatusNotFound)
		return
	}

	if err := h.service.DeleteDomain(ctx, domainID); err != nil {
		if isNotFoundError(err) {
			writeJSONError(w, "domain not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "failed to delete domain: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "domain deleted successfully",
	})
}

// GetTrackingURL handles GET /api/mailing/tracking-domains/active-url
// Returns the active tracking URL for the organization
func (h *TrackingDomainHandlers) GetTrackingURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)

	trackingURL, err := h.service.GetTrackingURL(ctx, orgID)
	if err != nil {
		writeJSONError(w, "failed to get tracking URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tracking_url": trackingURL,
	})
}

// RegisterTrackingDomainRoutes adds tracking domain routes to the router
func RegisterTrackingDomainRoutes(r chi.Router, db *sql.DB, platformDomain, defaultTracking string) {
	h := NewTrackingDomainHandlers(db, platformDomain, defaultTracking)

	r.Route("/tracking-domains", func(r chi.Router) {
		r.Post("/", h.RegisterDomain)
		r.Get("/", h.ListDomains)
		r.Get("/active-url", h.GetTrackingURL)
		r.Get("/suggestions", h.HandleTrackingDomainSuggestions)
		r.Get("/{id}", h.GetDomain)
		r.Post("/{id}/verify", h.VerifyDomain)
		r.Delete("/{id}", h.DeleteDomain)
		// AWS-specific routes
		r.Get("/{id}/aws-status", h.GetAWSStatus)
		r.Post("/{id}/refresh-aws-status", h.RefreshAWSStatus)
	})
}

// RegisterTrackingDomainRoutesWithAWS adds tracking domain routes with AWS infrastructure support
func RegisterTrackingDomainRoutesWithAWS(r chi.Router, db *sql.DB, platformDomain, defaultTracking, originServer string, awsInfra *mailing.AWSInfrastructureService) *TrackingDomainHandlers {
	h := NewTrackingDomainHandlersWithAWS(db, platformDomain, defaultTracking, originServer, awsInfra)

	r.Route("/tracking-domains", func(r chi.Router) {
		r.Post("/", h.RegisterDomainWithAWS)
		r.Get("/", h.ListDomains)
		r.Get("/active-url", h.GetTrackingURL)
		r.Get("/suggestions", h.HandleTrackingDomainSuggestions)
		r.Get("/{id}", h.GetDomain)
		r.Post("/{id}/verify", h.VerifyDomain)
		r.Delete("/{id}", h.DeleteDomain)
		// AWS-specific routes
		r.Get("/{id}/aws-status", h.GetAWSStatus)
		r.Post("/{id}/refresh-aws-status", h.RefreshAWSStatus)
	})

	return h
}

// RegisterDomainWithAWS handles POST /api/mailing/tracking-domains with AWS provisioning
func (h *TrackingDomainHandlers) RegisterDomainWithAWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req RegisterTrackingDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Domain == "" {
		writeJSONError(w, "domain is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	domain, err := h.service.RegisterDomainWithAWS(ctx, orgID, req.Domain)
	if err != nil {
		if isConflictError(err) {
			writeJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		if isValidationError(err) {
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONError(w, "failed to register domain: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"domain":  domain,
		"message": "Domain registered and AWS provisioning started",
		"note":    "SSL certificate and CloudFront distribution are being provisioned. Check status using the aws-status endpoint.",
	})
}

// GetAWSStatus handles GET /api/mailing/tracking-domains/{id}/aws-status
func (h *TrackingDomainHandlers) GetAWSStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		writeJSONError(w, "domain ID is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	// Check domain ownership
	owned, err := h.service.CheckDomainOwnership(ctx, domainID, orgID)
	if err != nil {
		writeJSONError(w, "failed to verify domain ownership: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !owned {
		writeJSONError(w, "domain not found or not owned by organization", http.StatusNotFound)
		return
	}

	status, err := h.service.GetAWSStatus(ctx, domainID)
	if err != nil {
		writeJSONError(w, "failed to get AWS status: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// RefreshAWSStatus handles POST /api/mailing/tracking-domains/{id}/refresh-aws-status
func (h *TrackingDomainHandlers) RefreshAWSStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		writeJSONError(w, "domain ID is required", http.StatusBadRequest)
		return
	}

	orgID := getOrgIDString(r)

	// Check domain ownership
	owned, err := h.service.CheckDomainOwnership(ctx, domainID, orgID)
	if err != nil {
		writeJSONError(w, "failed to verify domain ownership: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !owned {
		writeJSONError(w, "domain not found or not owned by organization", http.StatusNotFound)
		return
	}

	domain, err := h.service.RefreshAWSStatus(ctx, domainID)
	if err != nil {
		writeJSONError(w, "failed to refresh AWS status: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(domain)
}

// Helper functions (writeJSONError, isConflictError, contains are defined in custom_fields_handlers.go)

func isValidationError(err error) bool {
	return err != nil && (contains(err.Error(), "invalid") || contains(err.Error(), "cannot be empty"))
}

func isNotFoundError(err error) bool {
	return err != nil && contains(err.Error(), "not found")
}

// =============================================================================
// TRACKING DOMAIN SUGGESTIONS
// Auto-suggests track.{sending_domain} for each ESP sending profile,
// cross-referenced with existing provisioned tracking domains.
// =============================================================================

// trackingDomainSuggestion represents a suggested tracking domain
type trackingDomainSuggestion struct {
	SendingDomain          string  `json:"sending_domain"`
	SuggestedTrackingDomain string `json:"suggested_tracking_domain"`
	ProfileName            string  `json:"profile_name"`
	Status                 string  `json:"status"`    // not_provisioned, pending, provisioning, active, failed
	DomainID               *string `json:"domain_id"` // nil if not yet provisioned
	SSLStatus              string  `json:"ssl_status,omitempty"`
	CloudFrontDomain       string  `json:"cloudfront_domain,omitempty"`
	Verified               bool    `json:"verified"`
	SSLProvisioned         bool    `json:"ssl_provisioned"`
}

// HandleTrackingDomainSuggestions returns suggested tracking domains based on sending profiles.
// For each unique sending_domain, it suggests track.{sending_domain} and checks provisioning status.
func (h *TrackingDomainHandlers) HandleTrackingDomainSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)
	if orgID == "" {
		writeJSONError(w, "organization context required", http.StatusBadRequest)
		return
	}

	// Query distinct sending domains from active sending profiles
	rows, err := h.db.QueryContext(ctx, `
		SELECT DISTINCT ON (sending_domain) sending_domain, name
		FROM mailing_sending_profiles
		WHERE organization_id = $1
		  AND sending_domain IS NOT NULL
		  AND sending_domain != ''
		  AND status = 'active'
		ORDER BY sending_domain, name
	`, orgID)
	if err != nil {
		writeJSONError(w, "Failed to query sending profiles: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type profileDomain struct {
		sendingDomain string
		profileName   string
	}
	var domains []profileDomain
	for rows.Next() {
		var d profileDomain
		if err := rows.Scan(&d.sendingDomain, &d.profileName); err != nil {
			continue
		}
		domains = append(domains, d)
	}
	if rows.Err() != nil {
		writeJSONError(w, "Error reading sending profiles", http.StatusInternalServerError)
		return
	}

	// For each sending domain, generate suggestion and check provisioning status
	var suggestions []trackingDomainSuggestion
	for _, d := range domains {
		suggestedDomain := "track." + d.sendingDomain
		s := trackingDomainSuggestion{
			SendingDomain:           d.sendingDomain,
			SuggestedTrackingDomain: suggestedDomain,
			ProfileName:             d.profileName,
			Status:                  "not_provisioned",
			DomainID:                nil,
			Verified:                false,
		}

		// Check if this tracking domain already exists
		var domainID string
		var sslStatus sql.NullString
		var verified, sslProvisioned bool
		var cfDomain sql.NullString
		err := h.db.QueryRowContext(ctx, `
			SELECT id, COALESCE(ssl_status, 'pending'), verified, ssl_provisioned, cloudfront_domain
			FROM mailing_tracking_domains
			WHERE org_id = $1 AND domain = $2
			LIMIT 1
		`, orgID, suggestedDomain).Scan(&domainID, &sslStatus, &verified, &sslProvisioned, &cfDomain)

		if err == nil {
			s.DomainID = &domainID
			s.SSLStatus = sslStatus.String
			s.Verified = verified
			s.SSLProvisioned = sslProvisioned
			s.CloudFrontDomain = cfDomain.String

			// Map ssl_status to user-friendly status
			switch sslStatus.String {
			case "active":
				s.Status = "active"
			case "provisioning":
				s.Status = "provisioning"
			case "validating":
				s.Status = "provisioning"
			case "pending":
				s.Status = "pending"
			case "failed":
				s.Status = "failed"
			default:
				s.Status = "pending"
			}
		}
		// If sql.ErrNoRows, keep defaults (not_provisioned)

		suggestions = append(suggestions, s)
	}

	if suggestions == nil {
		suggestions = []trackingDomainSuggestion{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suggestions": suggestions,
		"total":       len(suggestions),
	})
}

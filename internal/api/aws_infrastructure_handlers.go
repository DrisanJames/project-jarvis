package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// AWSInfrastructureHandlers contains handlers for AWS infrastructure API endpoints
type AWSInfrastructureHandlers struct {
	db               *sql.DB
	awsInfra         *mailing.AWSInfrastructureService
	trackingService  *mailing.TrackingDomainService
	imageCDNService  *mailing.ImageCDNService
}

// NewAWSInfrastructureHandlers creates a new AWSInfrastructureHandlers instance
func NewAWSInfrastructureHandlers(db *sql.DB, awsInfra *mailing.AWSInfrastructureService) *AWSInfrastructureHandlers {
	return &AWSInfrastructureHandlers{
		db:       db,
		awsInfra: awsInfra,
	}
}

// SetTrackingService sets the tracking domain service
func (h *AWSInfrastructureHandlers) SetTrackingService(svc *mailing.TrackingDomainService) {
	h.trackingService = svc
}

// SetImageCDNService sets the image CDN service
func (h *AWSInfrastructureHandlers) SetImageCDNService(svc *mailing.ImageCDNService) {
	h.imageCDNService = svc
}

// RegisterRoutes registers all AWS infrastructure routes
func (h *AWSInfrastructureHandlers) RegisterRoutes(r chi.Router) {
	r.Route("/aws", func(r chi.Router) {
		// Tracking domain AWS provisioning
		r.Post("/tracking-domains/{id}/provision", h.ProvisionTrackingDomain)
		r.Get("/tracking-domains/{id}/status", h.GetTrackingDomainAWSStatus)
		r.Post("/tracking-domains/{id}/refresh-status", h.RefreshTrackingDomainStatus)

		// Image CDN AWS provisioning
		r.Post("/image-domains/provision", h.ProvisionImageDomain)
		r.Get("/image-domains/{id}/status", h.GetImageDomainAWSStatus)

		// S3 bucket management
		r.Post("/s3-buckets", h.CreateS3Bucket)
		r.Get("/s3-buckets", h.ListS3Buckets)

		// ACM certificate management
		r.Post("/certificates", h.RequestCertificate)
		r.Get("/certificates/{arn}/status", h.GetCertificateStatus)
		r.Post("/certificates/{arn}/validate", h.CreateValidationRecords)

		// CloudFront distributions
		r.Get("/distributions", h.ListDistributions)
		r.Get("/distributions/{id}/status", h.GetDistributionStatus)

		// Route53 records
		r.Post("/dns-records", h.CreateDNSRecord)
		r.Get("/dns-records", h.ListDNSRecords)

		// Provisioning logs
		r.Get("/provisioning-logs", h.GetProvisioningLogs)
	})
}

// ProvisionTrackingDomainRequest represents a request to provision tracking domain
type ProvisionTrackingDomainRequest struct {
	OriginServer string `json:"origin_server"` // API server URL for tracking
}

// ProvisionTrackingDomain handles POST /api/mailing/aws/tracking-domains/{id}/provision
func (h *AWSInfrastructureHandlers) ProvisionTrackingDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	var req ProvisionTrackingDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Use default origin server if not provided
		req.OriginServer = "api.ignitemailing.com"
	}

	orgID := getOrgIDString(r)

	// Get the tracking domain
	if h.trackingService == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Tracking service not configured")
		return
	}

	domain, err := h.trackingService.GetDomainByID(ctx, domainID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Domain not found")
		return
	}

	// Check ownership
	if domain.OrgID != orgID {
		respondWithError(w, http.StatusForbidden, "Domain not owned by organization")
		return
	}

	// Start AWS provisioning
	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	err = h.awsInfra.ProvisionTrackingDomain(ctx, orgID, domain.Domain, req.OriginServer)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to start provisioning: "+err.Error())
		return
	}

	// Update domain status
	h.db.ExecContext(ctx, `
		UPDATE mailing_tracking_domains 
		SET ssl_status = 'provisioning', origin_server = $1, updated_at = NOW()
		WHERE id = $2
	`, req.OriginServer, domainID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"message":      "AWS provisioning started",
		"domain":       domain.Domain,
		"status":       "provisioning",
		"origin_server": req.OriginServer,
	})
}

// GetTrackingDomainAWSStatus handles GET /api/mailing/aws/tracking-domains/{id}/status
func (h *AWSInfrastructureHandlers) GetTrackingDomainAWSStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	if h.trackingService == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Tracking service not configured")
		return
	}

	status, err := h.trackingService.GetAWSStatus(ctx, domainID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get AWS status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// RefreshTrackingDomainStatus handles POST /api/mailing/aws/tracking-domains/{id}/refresh-status
func (h *AWSInfrastructureHandlers) RefreshTrackingDomainStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	if h.trackingService == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Tracking service not configured")
		return
	}

	domain, err := h.trackingService.RefreshAWSStatus(ctx, domainID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to refresh status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(domain)
}

// ProvisionImageDomainRequest represents a request to provision image CDN
type ProvisionImageDomainRequest struct {
	Domain     string `json:"domain"`
	BucketName string `json:"bucket_name"`
}

// ProvisionImageDomain handles POST /api/mailing/aws/image-domains/provision
func (h *AWSInfrastructureHandlers) ProvisionImageDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req ProvisionImageDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Domain == "" {
		respondWithError(w, http.StatusBadRequest, "Domain is required")
		return
	}

	if req.BucketName == "" {
		req.BucketName = "ignite-email-images-prod"
	}

	orgID := getOrgIDString(r)

	if h.imageCDNService == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Image CDN service not configured")
		return
	}

	domain, err := h.imageCDNService.ProvisionImageDomainWithAWS(ctx, orgID, req.Domain, req.BucketName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to provision image domain: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"message":     "AWS provisioning started for image CDN",
		"domain":      domain,
		"bucket_name": req.BucketName,
	})
}

// GetImageDomainAWSStatus handles GET /api/mailing/aws/image-domains/{id}/status
func (h *AWSInfrastructureHandlers) GetImageDomainAWSStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	if h.imageCDNService == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Image CDN service not configured")
		return
	}

	status, err := h.imageCDNService.GetImageDomainAWSStatus(ctx, domainID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get AWS status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// CreateS3BucketRequest represents a request to create an S3 bucket
type CreateS3BucketRequest struct {
	BucketName string `json:"bucket_name"`
	BucketType string `json:"bucket_type"` // image_cdn, email_templates, exports
}

// CreateS3Bucket handles POST /api/mailing/aws/s3-buckets
func (h *AWSInfrastructureHandlers) CreateS3Bucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateS3BucketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.BucketName == "" {
		respondWithError(w, http.StatusBadRequest, "Bucket name is required")
		return
	}

	if req.BucketType == "" {
		req.BucketType = "image_cdn"
	}

	orgID := getOrgIDString(r)

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	bucket, err := h.awsInfra.CreateImageBucket(ctx, orgID, req.BucketName)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create bucket: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(bucket)
}

// ListS3Buckets handles GET /api/mailing/aws/s3-buckets
func (h *AWSInfrastructureHandlers) ListS3Buckets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, org_id, bucket_name, bucket_type, region, public_access_blocked, versioning_enabled, created_at
		FROM mailing_s3_buckets
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to query buckets: "+err.Error())
		return
	}
	defer rows.Close()

	var buckets []mailing.S3Bucket
	for rows.Next() {
		var b mailing.S3Bucket
		if err := rows.Scan(&b.ID, &b.OrgID, &b.BucketName, &b.BucketType, &b.Region, &b.PublicAccessBlocked, &b.VersioningEnabled, &b.CreatedAt); err != nil {
			continue
		}
		buckets = append(buckets, b)
	}

	if buckets == nil {
		buckets = []mailing.S3Bucket{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"buckets": buckets,
		"total":   len(buckets),
	})
}

// RequestCertificateRequest represents a request to create an ACM certificate
type RequestCertificateRequest struct {
	Domain string `json:"domain"`
}

// RequestCertificate handles POST /api/mailing/aws/certificates
func (h *AWSInfrastructureHandlers) RequestCertificate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req RequestCertificateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Domain == "" {
		respondWithError(w, http.StatusBadRequest, "Domain is required")
		return
	}

	orgID := getOrgIDString(r)

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	cert, err := h.awsInfra.RequestCertificate(ctx, orgID, req.Domain)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to request certificate: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(cert)
}

// GetCertificateStatus handles GET /api/mailing/aws/certificates/{arn}/status
func (h *AWSInfrastructureHandlers) GetCertificateStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// ARN is URL encoded, so we need to decode it
	certARN := chi.URLParam(r, "arn")
	if certARN == "" {
		certARN = r.URL.Query().Get("arn")
	}
	if certARN == "" {
		respondWithError(w, http.StatusBadRequest, "Certificate ARN is required")
		return
	}

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	status, err := h.awsInfra.CheckCertificateStatus(ctx, certARN)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get certificate status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"certificate_arn": certARN,
		"status":          status,
	})
}

// CreateValidationRecords handles POST /api/mailing/aws/certificates/{arn}/validate
func (h *AWSInfrastructureHandlers) CreateValidationRecords(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	certARN := chi.URLParam(r, "arn")
	if certARN == "" {
		certARN = r.URL.Query().Get("arn")
	}
	if certARN == "" {
		respondWithError(w, http.StatusBadRequest, "Certificate ARN is required")
		return
	}

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	err := h.awsInfra.CreateValidationRecords(ctx, certARN)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create validation records: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Validation records created in Route53",
	})
}

// ListDistributions handles GET /api/mailing/aws/distributions
func (h *AWSInfrastructureHandlers) ListDistributions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)
	distType := r.URL.Query().Get("type") // tracking, image_cdn

	query := `
		SELECT id, org_id, distribution_type, domain, cloudfront_id, cloudfront_domain, 
		       origin_domain, acm_cert_arn, status, enabled, price_class, created_at
		FROM mailing_cloudfront_distributions
		WHERE org_id = $1
	`
	args := []interface{}{orgID}

	if distType != "" {
		query += " AND distribution_type = $2"
		args = append(args, distType)
	}

	query += " ORDER BY created_at DESC"

	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to query distributions: "+err.Error())
		return
	}
	defer rows.Close()

	var distributions []mailing.CloudFrontDist
	for rows.Next() {
		var d mailing.CloudFrontDist
		var certARN sql.NullString
		if err := rows.Scan(
			&d.ID, &d.OrgID, &d.DistributionType, &d.Domain, &d.CloudFrontID, &d.CloudFrontDomain,
			&d.OriginDomain, &certARN, &d.Status, &d.Enabled, &d.PriceClass, &d.CreatedAt,
		); err != nil {
			continue
		}
		d.ACMCertARN = certARN.String
		distributions = append(distributions, d)
	}

	if distributions == nil {
		distributions = []mailing.CloudFrontDist{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"distributions": distributions,
		"total":         len(distributions),
	})
}

// GetDistributionStatus handles GET /api/mailing/aws/distributions/{id}/status
func (h *AWSInfrastructureHandlers) GetDistributionStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	distID := chi.URLParam(r, "id")
	if distID == "" {
		respondWithError(w, http.StatusBadRequest, "Distribution ID is required")
		return
	}

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	status, err := h.awsInfra.GetDistributionStatus(ctx, distID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get distribution status: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"distribution_id": distID,
		"status":          status,
	})
}

// CreateDNSRecordRequest represents a request to create a DNS record
type CreateDNSRecordRequest struct {
	RecordName  string `json:"record_name"`
	RecordType  string `json:"record_type"`  // A, AAAA, CNAME, TXT, MX
	RecordValue string `json:"record_value"`
	TTL         int    `json:"ttl"`
}

// CreateDNSRecord handles POST /api/mailing/aws/dns-records
func (h *AWSInfrastructureHandlers) CreateDNSRecord(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateDNSRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.RecordName == "" || req.RecordType == "" || req.RecordValue == "" {
		respondWithError(w, http.StatusBadRequest, "record_name, record_type, and record_value are required")
		return
	}

	if req.TTL <= 0 {
		req.TTL = 300
	}

	orgID := getOrgIDString(r)

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	record, err := h.awsInfra.CreateDNSRecord(ctx, orgID, req.RecordName, req.RecordType, req.RecordValue, req.TTL)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create DNS record: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(record)
}

// ListDNSRecords handles GET /api/mailing/aws/dns-records
func (h *AWSInfrastructureHandlers) ListDNSRecords(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	orgID := getOrgIDString(r)

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, org_id, hosted_zone_id, record_name, record_type, record_value, ttl, change_id, status, resource_type, resource_id, created_at
		FROM mailing_route53_records
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to query DNS records: "+err.Error())
		return
	}
	defer rows.Close()

	var records []mailing.Route53Record
	for rows.Next() {
		var rec mailing.Route53Record
		var changeID, resourceType, resourceID sql.NullString
		if err := rows.Scan(
			&rec.ID, &rec.OrgID, &rec.HostedZoneID, &rec.RecordName, &rec.RecordType, &rec.RecordValue,
			&rec.TTL, &changeID, &rec.Status, &resourceType, &resourceID, &rec.CreatedAt,
		); err != nil {
			continue
		}
		rec.ChangeID = changeID.String
		rec.ResourceType = resourceType.String
		rec.ResourceID = resourceID.String
		records = append(records, rec)
	}

	if records == nil {
		records = []mailing.Route53Record{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"records": records,
		"total":   len(records),
	})
}

// GetProvisioningLogs handles GET /api/mailing/aws/provisioning-logs
func (h *AWSInfrastructureHandlers) GetProvisioningLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resourceType := r.URL.Query().Get("resource_type")
	resourceID := r.URL.Query().Get("resource_id")

	if resourceType == "" || resourceID == "" {
		// Return all logs for the org
		orgID := getOrgIDString(r)
		rows, err := h.db.QueryContext(ctx, `
			SELECT id, resource_type, resource_id, action, aws_resource_id, aws_resource_arn, status, request_params, error_message, started_at, completed_at
			FROM mailing_aws_provisioning_log
			WHERE resource_id IN (
				SELECT id::text FROM mailing_tracking_domains WHERE org_id = $1
				UNION SELECT id::text FROM mailing_image_domains WHERE org_id = $1
			)
			ORDER BY started_at DESC
			LIMIT 50
		`, orgID)
		if err != nil {
			respondWithError(w, http.StatusInternalServerError, "Failed to query logs: "+err.Error())
			return
		}
		defer rows.Close()

		var logs []map[string]interface{}
		for rows.Next() {
			var log map[string]interface{}
			var id, resType, resID, action, status string
			var awsResID, awsResARN, errMsg sql.NullString
			var reqParams []byte
			var startedAt sql.NullTime
			var completedAt sql.NullTime

			if err := rows.Scan(&id, &resType, &resID, &action, &awsResID, &awsResARN, &status, &reqParams, &errMsg, &startedAt, &completedAt); err != nil {
				continue
			}

			log = map[string]interface{}{
				"id":              id,
				"resource_type":   resType,
				"resource_id":     resID,
				"action":          action,
				"aws_resource_id": awsResID.String,
				"aws_resource_arn": awsResARN.String,
				"status":          status,
				"error_message":   errMsg.String,
			}
			if startedAt.Valid {
				log["started_at"] = startedAt.Time
			}
			if completedAt.Valid {
				log["completed_at"] = completedAt.Time
			}
			logs = append(logs, log)
		}

		if logs == nil {
			logs = []map[string]interface{}{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"logs":  logs,
			"total": len(logs),
		})
		return
	}

	if h.awsInfra == nil {
		respondWithError(w, http.StatusServiceUnavailable, "AWS infrastructure service not configured")
		return
	}

	logs, err := h.awsInfra.ListProvisioningLogs(ctx, resourceType, resourceID, 20)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get logs: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":  logs,
		"total": len(logs),
	})
}

// getOrgIDStringFromContext extracts org ID from context using dynamic org context
func getOrgIDStringFromContext(ctx context.Context) string {
	orgID := GetOrgIDFromContext(ctx)
	if orgID == uuid.Nil {
		return "" // Return empty string on error - caller should handle
	}
	return orgID.String()
}

// validateUUID checks if a string is a valid UUID
func validateUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

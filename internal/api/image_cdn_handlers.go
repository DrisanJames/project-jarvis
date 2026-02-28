package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// ImageCDNHandlers contains handlers for image CDN API endpoints
type ImageCDNHandlers struct {
	imageCDN     *mailing.ImageCDNService
	db           *sql.DB
	s3Configured bool
}

// NewImageCDNHandlers creates a new ImageCDNHandlers instance
func NewImageCDNHandlers(db *sql.DB, s3Client *s3.Client, bucket, cdnDomain, region string) *ImageCDNHandlers {
	return &ImageCDNHandlers{
		imageCDN:     mailing.NewImageCDNService(db, s3Client, bucket, cdnDomain, region),
		db:           db,
		s3Configured: s3Client != nil,
	}
}

// requireS3 checks if S3 is configured and returns an error response if not
func (h *ImageCDNHandlers) requireS3(w http.ResponseWriter) bool {
	if !h.s3Configured {
		respondWithError(w, http.StatusServiceUnavailable, "Image CDN service requires S3 configuration")
		return false
	}
	return true
}

// UploadImage handles POST /api/mailing/images - Upload image (multipart form)
func (h *ImageCDNHandlers) UploadImage(w http.ResponseWriter, r *http.Request) {
	if !h.requireS3(w) {
		return
	}
	ctx := r.Context()

	// Parse multipart form (max 10MB)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse form: "+err.Error())
		return
	}

	// Get organization ID from form or dynamic context
	orgID := r.FormValue("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(orgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	// Get the file from form
	file, header, err := r.FormFile("file")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "No file provided: "+err.Error())
		return
	}
	defer file.Close()

	// Parse upload options
	opts := mailing.DefaultUploadOptions()
	if r.FormValue("generate_thumbnails") == "false" {
		opts.GenerateThumbnails = false
	}
	if r.FormValue("optimize") == "false" {
		opts.OptimizeForWeb = false
	}
	if q := r.FormValue("quality"); q != "" {
		if quality, err := strconv.Atoi(q); err == nil && quality >= 1 && quality <= 100 {
			opts.Quality = quality
		}
	}

	// Upload the image
	hostedImage, err := h.imageCDN.UploadImageWithOptions(ctx, orgID, header.Filename, file, opts)
	if err != nil {
		log.Printf("ERROR: failed to upload image: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to upload image")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(hostedImage)
}

// ListImages handles GET /api/mailing/images - List images (paginated)
func (h *ImageCDNHandlers) ListImages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get organization ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(orgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	// Parse pagination
	page := 1
	limit := 20
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	// Get images
	images, total, err := h.imageCDN.ListImages(ctx, orgID, page, limit)
	if err != nil {
		log.Printf("ERROR: failed to list images: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to list images")
		return
	}

	// Calculate pagination info
	totalPages := (total + limit - 1) / limit

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"images":      images,
		"total":       total,
		"page":        page,
		"limit":       limit,
		"total_pages": totalPages,
	})
}

// GetImage handles GET /api/mailing/images/{id} - Get image details
func (h *ImageCDNHandlers) GetImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	imageID := chi.URLParam(r, "id")
	if imageID == "" {
		respondWithError(w, http.StatusBadRequest, "Image ID is required")
		return
	}

	// Validate image ID format
	if _, err := uuid.Parse(imageID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid image ID")
		return
	}

	// Get image
	image, err := h.imageCDN.GetImage(ctx, imageID)
	if err != nil {
		if err.Error() == "image not found" {
			respondWithError(w, http.StatusNotFound, "Image not found")
			return
		}
		log.Printf("ERROR: failed to get image: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to get image")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(image)
}

// DeleteImage handles DELETE /api/mailing/images/{id} - Delete image
func (h *ImageCDNHandlers) DeleteImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	imageID := chi.URLParam(r, "id")
	if imageID == "" {
		respondWithError(w, http.StatusBadRequest, "Image ID is required")
		return
	}

	// Validate image ID format
	if _, err := uuid.Parse(imageID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid image ID")
		return
	}

	// Delete image
	if err := h.imageCDN.DeleteImage(ctx, imageID); err != nil {
		if err.Error() == "image not found" {
			respondWithError(w, http.StatusNotFound, "Image not found")
			return
		}
		log.Printf("ERROR: failed to delete image: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to delete image")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Image deleted successfully",
	})
}

// RegisterImageDomain handles POST /api/mailing/image-domains - Register custom domain
func (h *ImageCDNHandlers) RegisterImageDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var input struct {
		OrgID  string `json:"org_id"`
		Domain string `json:"domain"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if input.OrgID == "" {
		var err error
		input.OrgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(input.OrgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	if input.Domain == "" {
		respondWithError(w, http.StatusBadRequest, "Domain is required")
		return
	}

	// Register domain
	domain, err := h.imageCDN.RegisterImageDomain(ctx, input.OrgID, input.Domain)
	if err != nil {
		log.Printf("ERROR: failed to register image domain: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to register domain")
		return
	}

	// Add verification instructions
	response := map[string]interface{}{
		"domain": domain,
		"verification_instructions": map[string]interface{}{
			"method": "dns_txt",
			"instructions": fmt.Sprintf(
				"Add a TXT record to your DNS with the following values:\n"+
					"Host/Name: _mailing-verification.%s\n"+
					"Value: %s\n"+
					"TTL: 300 (or lowest available)",
				domain.Domain, domain.VerificationToken,
			),
			"expected_record": fmt.Sprintf("_mailing-verification.%s", domain.Domain),
			"expected_value":  domain.VerificationToken,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)
}

// ListImageDomains handles GET /api/mailing/image-domains - List domains
func (h *ImageCDNHandlers) ListImageDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get organization ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(orgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	// Get domains
	domains, err := h.imageCDN.ListImageDomains(ctx, orgID)
	if err != nil {
		log.Printf("ERROR: failed to list image domains: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to list domains")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"domains": domains,
		"total":   len(domains),
	})
}

// VerifyImageDomain handles POST /api/mailing/image-domains/{id}/verify - Verify domain
func (h *ImageCDNHandlers) VerifyImageDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	// Validate domain ID format
	if _, err := uuid.Parse(domainID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid domain ID")
		return
	}

	// Verify domain
	domain, err := h.imageCDN.VerifyImageDomain(ctx, domainID)
	if err != nil {
		if err.Error() == "domain not found" {
			respondWithError(w, http.StatusNotFound, "Domain not found")
			return
		}
		log.Printf("ERROR: failed to verify image domain %s: %v", domainID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to verify domain")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"domain":   domain,
		"verified": domain.Verified,
		"message":  "Domain verified successfully",
	})
}

// GetImageStorageStats handles GET /api/mailing/images/stats - Get storage statistics
func (h *ImageCDNHandlers) GetImageStorageStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get organization ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(orgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	// Get stats
	stats, err := h.imageCDN.GetStorageStats(ctx, orgID)
	if err != nil {
		log.Printf("ERROR: failed to get storage stats: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to get stats")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// GetImageCDNURL handles GET /api/mailing/images/{id}/cdn-url - Get CDN URL for image
func (h *ImageCDNHandlers) GetImageCDNURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	imageID := chi.URLParam(r, "id")
	if imageID == "" {
		respondWithError(w, http.StatusBadRequest, "Image ID is required")
		return
	}

	// Validate image ID format
	if _, err := uuid.Parse(imageID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid image ID")
		return
	}

	// Get organization ID from query param or dynamic context
	orgID := r.URL.Query().Get("org_id")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Get CDN URL
	cdnURL, err := h.imageCDN.GetCDNURL(ctx, orgID, imageID)
	if err != nil {
		if err.Error() == "image not found" {
			respondWithError(w, http.StatusNotFound, "Image not found")
			return
		}
		log.Printf("ERROR: failed to get CDN URL for image %s: %v", imageID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to retrieve CDN URL")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cdn_url": cdnURL,
	})
}

// DeleteImageDomain handles DELETE /api/mailing/image-domains/{id} - Delete domain
func (h *ImageCDNHandlers) DeleteImageDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	domainID := chi.URLParam(r, "id")
	if domainID == "" {
		respondWithError(w, http.StatusBadRequest, "Domain ID is required")
		return
	}

	// Validate domain ID format
	if _, err := uuid.Parse(domainID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid domain ID")
		return
	}

	// Delete from database
	result, err := h.db.ExecContext(ctx, `DELETE FROM mailing_image_domains WHERE id = $1`, domainID)
	if err != nil {
		log.Printf("ERROR: failed to delete image domain: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to delete domain")
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		respondWithError(w, http.StatusNotFound, "Domain not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Domain deleted successfully",
	})
}

// =============================================================================
// IMAGE REHOSTING FOR EVERFLOW CREATIVES
// Downloads external images from creative HTML, uploads to our CDN,
// rewrites <img src> attributes so emails serve images from our domain.
// =============================================================================

// imgSrcRegex matches <img ... src="..." ...> patterns in HTML
// Captures the full src attribute value (URL) in group 1
var imgSrcRegex = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)

// rehostHTMLRequest is the JSON body for POST /api/mailing/images/rehost-html
type rehostHTMLRequest struct {
	HTML  string `json:"html"`
	OrgID string `json:"org_id"`
}

// rehostHTMLResponse is the JSON response from the rehost endpoint
type rehostHTMLResponse struct {
	HTML           string `json:"html"`
	ImagesRehosted int    `json:"images_rehosted"`
	ImagesCached   int    `json:"images_cached"`
	ImagesSkipped  int    `json:"images_skipped"`
	ImagesFailed   int    `json:"images_failed"`
}

// HandleRehostCreativeImages accepts HTML content, downloads all external images,
// uploads them to S3/CDN, and returns the HTML with rewritten image URLs.
// This ensures email creatives serve images from our sending domain's CDN
// rather than third-party hosts like imageports.com.
func (h *ImageCDNHandlers) HandleRehostCreativeImages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse request body
	var input rehostHTMLRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	// Resolve org ID: prefer request context, fallback to body
	orgID := input.OrgID
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			respondWithError(w, http.StatusUnauthorized, "organization context required")
			return
		}
	}

	// Validate org ID format
	if _, err := uuid.Parse(orgID); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid organization ID")
		return
	}

	// If HTML is empty, return immediately
	if strings.TrimSpace(input.HTML) == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rehostHTMLResponse{
			HTML: input.HTML,
		})
		return
	}

	// If S3 is not configured, return HTML as-is (graceful degradation)
	if !h.s3Configured {
		log.Printf("[ImageRehost] S3 not configured, returning original HTML without rehosting")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rehostHTMLResponse{
			HTML: input.HTML,
		})
		return
	}

	// Check for org-specific verified image domain (e.g., img.horoscopeinfo.com)
	var orgImageDomain string
	h.db.QueryRowContext(ctx, `
		SELECT domain FROM mailing_image_domains
		WHERE org_id = $1 AND verified = true AND ssl_status = 'active'
		ORDER BY created_at ASC LIMIT 1
	`, orgID).Scan(&orgImageDomain)

	// Get CDN domain to skip images already on our CDN
	cdnDomain := h.imageCDN.GetCDNDomain()
	s3Bucket := h.imageCDN.GetBucket()

	// Find all <img src="..."> in the HTML
	matches := imgSrcRegex.FindAllStringSubmatch(input.HTML, -1)
	if len(matches) == 0 {
		log.Printf("[ImageRehost] No <img> tags found in HTML (%d bytes)", len(input.HTML))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rehostHTMLResponse{
			HTML: input.HTML,
		})
		return
	}

	log.Printf("[ImageRehost] Found %d <img> tags in HTML, processing for org %s", len(matches), orgID)

	processedHTML := input.HTML
	var rehosted, cached, skipped, failed int

	// HTTP client with timeout for downloading external images
	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Track already-processed URLs to avoid duplicate work in same request
	urlToCDN := make(map[string]string)

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		originalURL := match[1]

		// Skip data URIs
		if strings.HasPrefix(originalURL, "data:") {
			skipped++
			continue
		}

		// Skip relative URLs (no host)
		if !strings.HasPrefix(originalURL, "http://") && !strings.HasPrefix(originalURL, "https://") {
			skipped++
			continue
		}

		// Skip images already on our CDN
		if cdnDomain != "" && strings.Contains(originalURL, cdnDomain) {
			skipped++
			continue
		}

		// Skip images already on our S3 bucket
		if s3Bucket != "" && strings.Contains(originalURL, s3Bucket) {
			skipped++
			continue
		}

		// Check if we already processed this URL in this request
		if cdnURL, ok := urlToCDN[originalURL]; ok {
			processedHTML = strings.Replace(processedHTML, originalURL, cdnURL, 1)
			cached++
			continue
		}

		// URL-encode spaces for the download URL
		downloadURL := strings.ReplaceAll(originalURL, " ", "%20")

		// Download the image
		log.Printf("[ImageRehost] Downloading: %s", downloadURL)
		resp, err := httpClient.Get(downloadURL)
		if err != nil {
			log.Printf("[ImageRehost] WARNING: Failed to download %s: %v", downloadURL, err)
			failed++
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Printf("[ImageRehost] WARNING: Download returned HTTP %d for %s", resp.StatusCode, downloadURL)
			failed++
			continue
		}

		// Read image bytes (limit to 10MB to prevent OOM on oversized remote images)
		imageData, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		resp.Body.Close()
		if err != nil {
			log.Printf("[ImageRehost] WARNING: Failed to read image data from %s: %v", downloadURL, err)
			failed++
			continue
		}

		if len(imageData) == 0 {
			log.Printf("[ImageRehost] WARNING: Empty image data from %s", downloadURL)
			failed++
			continue
		}

		// Compute SHA-256 checksum for deduplication
		hash := sha256.Sum256(imageData)
		checksum := hex.EncodeToString(hash[:])

		// Check for existing image with same checksum (dedup)
		existingImage, err := h.imageCDN.FindImageByChecksum(ctx, orgID, checksum)
		if err != nil {
			log.Printf("[ImageRehost] WARNING: Checksum lookup failed: %v", err)
			// Continue with upload anyway
		}

		var cdnURL string

		if existingImage != nil {
			// Image already hosted - use existing CDN URL
			cdnURL = existingImage.CDNURL
			log.Printf("[ImageRehost] Cached: %s -> %s (checksum match)", originalURL, cdnURL)
			cached++
		} else {
			// Extract filename from URL
			filename := extractFilenameFromURL(originalURL)

			// Upload via ImageCDNService
			opts := mailing.DefaultUploadOptions()
			opts.GenerateThumbnails = false // Email images don't need thumbnails
			opts.OptimizeForWeb = false     // Preserve original quality for email

			hostedImage, err := h.imageCDN.UploadImageWithOptions(ctx, orgID, filename, bytes.NewReader(imageData), opts)
			if err != nil {
				log.Printf("[ImageRehost] WARNING: Failed to upload %s: %v", originalURL, err)
				failed++
				continue
			}

			cdnURL = hostedImage.CDNURL
			log.Printf("[ImageRehost] Rehosted: %s -> %s (%d bytes)", originalURL, cdnURL, len(imageData))
			rehosted++
		}

		// If an org-specific image domain is active, rewrite CDN URL to use it
		if orgImageDomain != "" && cdnURL != "" {
			cdnURL = rewriteToCustomDomain(cdnURL, orgImageDomain, cdnDomain)
		}

		// Replace the original URL with the CDN URL in the HTML
		processedHTML = strings.ReplaceAll(processedHTML, originalURL, cdnURL)

		// Cache for dedup within this request
		urlToCDN[originalURL] = cdnURL
	}

	log.Printf("[ImageRehost] Complete: rehosted=%d, cached=%d, skipped=%d, failed=%d",
		rehosted, cached, skipped, failed)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rehostHTMLResponse{
		HTML:           processedHTML,
		ImagesRehosted: rehosted,
		ImagesCached:   cached,
		ImagesSkipped:  skipped,
		ImagesFailed:   failed,
	})
}

// extractFilenameFromURL extracts a clean filename from a URL path
func extractFilenameFromURL(rawURL string) string {
	// Remove query string and fragment
	cleanURL := rawURL
	if idx := strings.Index(cleanURL, "?"); idx > -1 {
		cleanURL = cleanURL[:idx]
	}
	if idx := strings.Index(cleanURL, "#"); idx > -1 {
		cleanURL = cleanURL[:idx]
	}

	// Decode URL-encoded characters for the filename
	cleanURL = strings.ReplaceAll(cleanURL, "%20", " ")

	// Get the base filename
	filename := path.Base(cleanURL)
	if filename == "" || filename == "/" || filename == "." {
		filename = "image.png" // fallback
	}

	return filename
}

// rewriteToCustomDomain rewrites a CDN URL to use an org's custom image domain.
// For example: https://d1234.cloudfront.net/path/image.png -> https://img.example.com/path/image.png
func rewriteToCustomDomain(cdnURL, customDomain, defaultCDN string) string {
	if customDomain == "" {
		return cdnURL
	}
	customBase := "https://" + customDomain
	if defaultCDN != "" && strings.Contains(cdnURL, defaultCDN) {
		return strings.Replace(cdnURL, "https://"+defaultCDN, customBase, 1)
	}
	// If the CDN URL uses any cloudfront domain, replace it
	if strings.Contains(cdnURL, ".cloudfront.net") {
		// Extract path from URL
		parts := strings.SplitN(cdnURL, ".cloudfront.net", 2)
		if len(parts) == 2 {
			return customBase + parts[1]
		}
	}
	return cdnURL
}

// =============================================================================
// IMAGE DOMAIN SUGGESTIONS
// Auto-suggests img.{sending_domain} for each ESP sending profile,
// cross-referenced with existing provisioned image domains.
// =============================================================================

// imageDomainSuggestion represents a suggested image domain derived from a sending profile
type imageDomainSuggestion struct {
	SendingDomain        string  `json:"sending_domain"`
	SuggestedImageDomain string  `json:"suggested_image_domain"`
	ProfileName          string  `json:"profile_name"`
	Status               string  `json:"status"`    // not_provisioned, pending, provisioning, active, failed
	DomainID             *string `json:"domain_id"` // nil if not yet provisioned
	SSLStatus            string  `json:"ssl_status,omitempty"`
	CloudFrontDomain     string  `json:"cloudfront_domain,omitempty"`
	Verified             bool    `json:"verified"`
}

// HandleImageDomainSuggestions returns suggested image domains based on sending profiles.
// For each unique sending_domain in mailing_sending_profiles, it suggests img.{sending_domain}
// and checks if that image domain has already been provisioned.
func (h *ImageCDNHandlers) HandleImageDomainSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get org ID
	orgID, err := GetOrgIDStringFromRequest(r)
	if err != nil || orgID == "" {
		respondWithError(w, http.StatusBadRequest, "organization context required")
		return
	}

	// Query distinct sending domains from sending profiles
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
		log.Printf("ERROR: failed to query sending profiles for image domain suggestions: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to retrieve sending profiles")
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
		respondWithError(w, http.StatusInternalServerError, "Error reading sending profiles")
		return
	}

	// For each sending domain, generate suggestion and check provisioning status
	var suggestions []imageDomainSuggestion
	for _, d := range domains {
		suggestedDomain := "img." + d.sendingDomain
		s := imageDomainSuggestion{
			SendingDomain:        d.sendingDomain,
			SuggestedImageDomain: suggestedDomain,
			ProfileName:          d.profileName,
			Status:               "not_provisioned",
			DomainID:             nil,
			Verified:             false,
		}

		// Check if this image domain already exists
		var domainID string
		var sslStatus string
		var verified bool
		var cfDomain sql.NullString
		err := h.db.QueryRowContext(ctx, `
			SELECT id, ssl_status, verified, cloudfront_domain
			FROM mailing_image_domains
			WHERE org_id = $1 AND domain = $2
			LIMIT 1
		`, orgID, suggestedDomain).Scan(&domainID, &sslStatus, &verified, &cfDomain)

		if err == nil {
			s.DomainID = &domainID
			s.SSLStatus = sslStatus
			s.Verified = verified
			s.CloudFrontDomain = cfDomain.String

			// Map ssl_status to user-friendly status
			switch sslStatus {
			case "active":
				s.Status = "active"
			case "provisioning":
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
		suggestions = []imageDomainSuggestion{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suggestions": suggestions,
		"total":       len(suggestions),
	})
}

// RegisterImageCDNRoutes adds image CDN routes to the router
func RegisterImageCDNRoutes(r chi.Router, db *sql.DB, s3Client *s3.Client, bucket, cdnDomain, region string) {
	h := NewImageCDNHandlers(db, s3Client, bucket, cdnDomain, region)

	r.Route("/images", func(r chi.Router) {
		// Image management
		r.Post("/", h.UploadImage)
		r.Get("/", h.ListImages)
		r.Get("/stats", h.GetImageStorageStats)
		r.Get("/{id}", h.GetImage)
		r.Delete("/{id}", h.DeleteImage)
		r.Get("/{id}/cdn-url", h.GetImageCDNURL)

		// Image rehosting for Everflow creatives
		r.Post("/rehost-html", h.HandleRehostCreativeImages)
	})

	r.Route("/image-domains", func(r chi.Router) {
		// Domain management
		r.Post("/", h.RegisterImageDomain)
		r.Get("/", h.ListImageDomains)
		r.Get("/suggestions", h.HandleImageDomainSuggestions)
		r.Post("/{id}/verify", h.VerifyImageDomain)
		r.Delete("/{id}", h.DeleteImageDomain)
	})
}

// respondWithError sends a JSON error response
func respondWithError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": message,
	})
}

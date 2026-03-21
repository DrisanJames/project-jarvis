package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

type OfferCreativeAssetsHandlers struct {
	db       *sql.DB
	imageCDN *mailing.ImageCDNService
}

// RegisterOfferCreativeAssetRoutes adds creative asset routes under /offer-center/offers/{id}/assets
func RegisterOfferCreativeAssetRoutes(r chi.Router, db *sql.DB, s3Client *s3.Client, bucket, cdnDomain, region string) {
	var imageCDN *mailing.ImageCDNService
	if s3Client != nil {
		imageCDN = mailing.NewImageCDNService(db, s3Client, bucket, cdnDomain, region)
	}
	h := &OfferCreativeAssetsHandlers{db: db, imageCDN: imageCDN}

	r.Get("/offer-center/offers/{id}/assets", h.HandleListAssets)
	r.Post("/offer-center/offers/{id}/assets", h.HandleUploadAsset)
	r.Post("/offer-center/offers/{id}/assets/upload-zip", h.HandleUploadZipBundle)
	r.Delete("/offer-center/offers/{id}/assets/all", h.HandleDeleteAllAssets)
	r.Delete("/offer-center/offers/{id}/assets/{assetId}", h.HandleDeleteAsset)
}

type CreativeAsset struct {
	ID               string    `json:"id"`
	OfferID          string    `json:"offer_id"`
	HostedImageID    string    `json:"hosted_image_id"`
	AssetRole        string    `json:"asset_role"`
	Label            string    `json:"label"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	CDNURL           string    `json:"cdn_url"`
	CDNURLMedium     string    `json:"cdn_url_medium,omitempty"`
	CDNURLThumbnail  string    `json:"cdn_url_thumbnail,omitempty"`
	OriginalFilename string    `json:"original_filename"`
	FileSize         int64     `json:"file_size"`
	MimeType         string    `json:"mime_type"`
	SortOrder        int       `json:"sort_order"`
	CreatedAt        time.Time `json:"created_at"`
}

func classifyAssetRole(width, height int) string {
	ratio := float64(width) / float64(height)
	switch {
	case width >= 550 && width <= 700 && height >= 200 && height <= 350:
		return "email_header"
	case width >= 750 && width <= 850 && height >= 350 && height <= 450:
		return "email_hero"
	case width >= 550 && width <= 650 && height >= 250 && height <= 350:
		return "email_content"
	case width >= 550 && width <= 650 && height >= 1000:
		return "landing_hero"
	case width >= 900 && height <= 300:
		return "landing_banner"
	case width >= 700 && width <= 750 && height >= 200 && height <= 300:
		return "landing_billboard"
	case width >= 280 && width <= 340 && height >= 400 && height <= 520:
		return "mobile_interstitial"
	case width >= 280 && width <= 320 && height >= 500 && height <= 700:
		return "content_block"
	case width >= 280 && width <= 340 && height <= 60:
		return "mobile_banner"
	case width <= 200 && height <= 200:
		return "thumbnail"
	case width <= 200 && height >= 500:
		return "sidebar"
	case width >= 280 && width <= 320 && ratio >= 0.8 && ratio <= 1.4:
		return "content_block"
	case ratio >= 1.5:
		return "banner"
	default:
		return "content"
	}
}

func assetRoleLabel(role string) string {
	labels := map[string]string{
		"email_hero":          "Email Hero Image",
		"email_header":        "Email Header Banner",
		"email_content":       "Email Content Block",
		"landing_hero":        "Landing Page Hero",
		"landing_banner":      "Landing Page Banner",
		"landing_billboard":   "Landing Page Billboard",
		"mobile_interstitial": "Mobile Interstitial",
		"mobile_banner":       "Mobile Banner",
		"thumbnail":           "Thumbnail",
		"sidebar":             "Sidebar Skyscraper",
		"content_block":       "Content Block",
		"banner":              "Banner",
		"content":             "General Content",
	}
	if l, ok := labels[role]; ok {
		return l
	}
	return "Creative Asset"
}

// HandleUploadAsset — POST /offer-center/offers/{id}/assets
func (h *OfferCreativeAssetsHandlers) HandleUploadAsset(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}
	ctx := r.Context()

	if h.imageCDN == nil {
		respondError(w, http.StatusServiceUnavailable, "Image CDN not configured — set JARVIS_S3_BUCKET")
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		respondError(w, http.StatusBadRequest, "Failed to parse form: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "No file provided: "+err.Error())
		return
	}
	defer file.Close()

	orgID := "00000000-0000-0000-0000-000000000001"

	opts := mailing.DefaultUploadOptions()
	opts.GenerateThumbnails = true
	opts.OptimizeForWeb = false

	hostedImage, err := h.imageCDN.UploadImageWithOptions(ctx, orgID, header.Filename, file, opts)
	if err != nil {
		log.Printf("[CreativeAssets] upload failed for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to upload image: "+err.Error())
		return
	}

	role := classifyAssetRole(hostedImage.Width, hostedImage.Height)
	if overrideRole := r.FormValue("asset_role"); overrideRole != "" {
		role = overrideRole
	}
	label := r.FormValue("label")
	if label == "" {
		label = fmt.Sprintf("%s (%dx%d)", assetRoleLabel(role), hostedImage.Width, hostedImage.Height)
	}

	assetID := uuid.New().String()
	_, err = h.db.ExecContext(ctx, `
		INSERT INTO mailing_offer_creative_assets
		(id, offer_id, hosted_image_id, asset_role, label, width, height,
		 cdn_url, cdn_url_medium, cdn_url_thumbnail, original_filename,
		 file_size, mime_type, sort_order, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
		ON CONFLICT (offer_id, hosted_image_id) DO NOTHING
	`, assetID, offerID, hostedImage.ID, role, label,
		hostedImage.Width, hostedImage.Height,
		hostedImage.CDNURL, hostedImage.CDNURLMedium, hostedImage.CDNURLThumbnail,
		hostedImage.OriginalFilename, hostedImage.Size, hostedImage.ContentType, 0)
	if err != nil {
		log.Printf("[CreativeAssets] db insert failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to link asset to offer")
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":         assetID,
		"cdn_url":    hostedImage.CDNURL,
		"width":      hostedImage.Width,
		"height":     hostedImage.Height,
		"asset_role": role,
		"label":      label,
		"filename":   hostedImage.OriginalFilename,
	})
}

// HandleUploadZipBundle — POST /offer-center/offers/{id}/assets/upload-zip
// Accepts a multipart zip file, extracts images and text content, uploads images to S3/CDN
const maxZipSize = 200 << 20 // 200 MB

var imageExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
}
var textExtensions = map[string]bool{
	".txt": true,
}
var htmlExtensions = map[string]bool{
	".html": true, ".htm": true,
}

// zipPathSafe rejects entries with path traversal or absolute paths
var unsafePathPattern = regexp.MustCompile(`(^|/)\.\.(/|$)`)

func zipPathSafe(name string) bool {
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return false
	}
	return !unsafePathPattern.MatchString(name)
}

func (h *OfferCreativeAssetsHandlers) HandleUploadZipBundle(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}
	ctx := r.Context()

	if h.imageCDN == nil {
		respondError(w, http.StatusServiceUnavailable, "Image CDN not configured — set JARVIS_S3_BUCKET")
		return
	}

	if err := r.ParseMultipartForm(maxZipSize); err != nil {
		respondError(w, http.StatusBadRequest, "Failed to parse form (max 200MB): "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "No file provided: "+err.Error())
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		respondError(w, http.StatusBadRequest, "Only .zip files are accepted")
		return
	}

	zipBytes, err := io.ReadAll(io.LimitReader(file, maxZipSize+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Failed to read uploaded file: "+err.Error())
		return
	}
	if int64(len(zipBytes)) > maxZipSize {
		respondError(w, http.StatusRequestEntityTooLarge, "Zip file exceeds 200MB limit")
		return
	}

	zipReader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		respondError(w, http.StatusBadRequest, "Invalid zip file: "+err.Error())
		return
	}

	orgID := "00000000-0000-0000-0000-000000000001"
	opts := mailing.DefaultUploadOptions()
	opts.GenerateThumbnails = true
	opts.OptimizeForWeb = false

	// First pass: detect bundle type (template vs image) by checking for HTML files
	hasHTMLFiles := false
	for _, zf := range zipReader.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(zf.Name))
		if htmlExtensions[ext] {
			hasHTMLFiles = true
			break
		}
	}

	if hasHTMLFiles {
		h.processTemplateBundleZip(ctx, w, zipReader, offerID, orgID)
		return
	}

	// Image bundle: existing behavior
	var uploaded []CreativeAsset
	var uploadErrors []string
	var adCopyParts []string
	var taglineParts []string

	for _, zf := range zipReader.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if !zipPathSafe(zf.Name) {
			log.Printf("[CreativeAssets] zip: skipping unsafe path %q", zf.Name)
			continue
		}

		baseName := filepath.Base(zf.Name)
		ext := strings.ToLower(filepath.Ext(baseName))

		if textExtensions[ext] {
			rc, err := zf.Open()
			if err != nil {
				continue
			}
			content, err := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			if err != nil {
				continue
			}
			text := strings.TrimSpace(string(content))
			if text == "" {
				continue
			}
			lowerName := strings.ToLower(baseName)
			if strings.Contains(lowerName, "tagline") || strings.Contains(lowerName, "subject") {
				taglineParts = append(taglineParts, text)
			} else {
				adCopyParts = append(adCopyParts, text)
			}
			log.Printf("[CreativeAssets] zip: extracted text from %s (%d bytes)", baseName, len(content))
			continue
		}

		if !imageExtensions[ext] {
			continue
		}

		if zf.UncompressedSize64 > uint64(mailing.MaxFileSizeMB)<<20 {
			uploadErrors = append(uploadErrors, fmt.Sprintf("skipped %s: exceeds %dMB single-file limit", baseName, mailing.MaxFileSizeMB))
			continue
		}

		rc, err := zf.Open()
		if err != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("failed to open %s in zip: %v", baseName, err))
			continue
		}

		hostedImage, err := h.imageCDN.UploadImageWithOptions(ctx, orgID, baseName, rc, opts)
		rc.Close()
		if err != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("upload failed for %s: %v", baseName, err))
			continue
		}

		role := classifyAssetRole(hostedImage.Width, hostedImage.Height)
		label := fmt.Sprintf("%s (%dx%d)", assetRoleLabel(role), hostedImage.Width, hostedImage.Height)

		assetID := uuid.New().String()
		_, dbErr := h.db.ExecContext(ctx, `
			INSERT INTO mailing_offer_creative_assets
			(id, offer_id, hosted_image_id, asset_role, label, width, height,
			 cdn_url, cdn_url_medium, cdn_url_thumbnail, original_filename,
			 file_size, mime_type, sort_order, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
			ON CONFLICT (offer_id, hosted_image_id) DO NOTHING
		`, assetID, offerID, hostedImage.ID, role, label,
			hostedImage.Width, hostedImage.Height,
			hostedImage.CDNURL, hostedImage.CDNURLMedium, hostedImage.CDNURLThumbnail,
			hostedImage.OriginalFilename, hostedImage.Size, hostedImage.ContentType, 0)
		if dbErr != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("db insert failed for %s: %v", baseName, dbErr))
			continue
		}

		uploaded = append(uploaded, CreativeAsset{
			ID:               assetID,
			OfferID:          offerID,
			HostedImageID:    hostedImage.ID,
			AssetRole:        role,
			Label:            label,
			Width:            hostedImage.Width,
			Height:           hostedImage.Height,
			CDNURL:           hostedImage.CDNURL,
			CDNURLMedium:     hostedImage.CDNURLMedium,
			CDNURLThumbnail:  hostedImage.CDNURLThumbnail,
			OriginalFilename: hostedImage.OriginalFilename,
			FileSize:         hostedImage.Size,
			MimeType:         hostedImage.ContentType,
		})

		log.Printf("[CreativeAssets] zip: uploaded %s -> %s (%dx%d, role=%s)", baseName, hostedImage.CDNURL, hostedImage.Width, hostedImage.Height, role)
	}

	if adCopy := strings.Join(adCopyParts, "\n\n---\n\n"); adCopy != "" {
		if _, err := h.db.ExecContext(ctx, `UPDATE mailing_offers SET approved_ad_copy=$1, updated_at=NOW() WHERE id=$2`, adCopy, offerID); err != nil {
			log.Printf("[CreativeAssets] warning: failed to update ad copy for offer %s: %v", offerID, err)
		}
	}
	if taglines := strings.Join(taglineParts, "\n"); taglines != "" {
		if _, err := h.db.ExecContext(ctx, `UPDATE mailing_offers SET approved_taglines=$1, updated_at=NOW() WHERE id=$2`, taglines, offerID); err != nil {
			log.Printf("[CreativeAssets] warning: failed to update taglines for offer %s: %v", offerID, err)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"uploaded":       len(uploaded),
		"errors":         len(uploadErrors),
		"text_extracted": len(adCopyParts) + len(taglineParts),
		"assets":         uploaded,
		"error_details":  uploadErrors,
	})
}

// processTemplateBundleZip handles zip files containing HTML email templates.
// Groups files by subfolder, processes each HTML through classifyAndRewriteHTML
// (image rehost, CTA link replacement, unsub replacement, pixel stripping),
// and imports each template as a creative in mailing_offer_creatives.
func (h *OfferCreativeAssetsHandlers) processTemplateBundleZip(
	ctx context.Context, w http.ResponseWriter,
	zipReader *zip.Reader, offerID, orgID string,
) {
	// Load offer metadata for link rewriting
	var trackingLink, offerOptoutLink, webProperty string
	h.db.QueryRowContext(ctx,
		`SELECT COALESCE(tracking_link_template,''), COALESCE(offer_optout_link,''), COALESCE(web_property,'')
		 FROM mailing_offers WHERE id=$1`, offerID,
	).Scan(&trackingLink, &offerOptoutLink, &webProperty)

	imageDomain := ""
	if webProperty != "" {
		if kit, ok := GetBrandKit(webProperty); ok {
			imageDomain = ResolveImageDomainForBrand(h.db, kit.ImageDomain)
		}
	}
	if imageDomain == "" {
		imageDomain = os.Getenv("IMAGE_CDN_DOMAIN")
	}

	// Group files by subfolder
	type subfolderFiles struct {
		htmlFile    *zip.File
		txtFile     *zip.File // plain text version of the creative
		subjectFile *zip.File
		fromFile    *zip.File
	}
	folders := make(map[string]*subfolderFiles)

	for _, zf := range zipReader.File {
		if zf.FileInfo().IsDir() || !zipPathSafe(zf.Name) {
			continue
		}
		dir := filepath.Dir(zf.Name)
		baseName := strings.ToLower(filepath.Base(zf.Name))
		ext := strings.ToLower(filepath.Ext(baseName))

		if folders[dir] == nil {
			folders[dir] = &subfolderFiles{}
		}
		sf := folders[dir]

		switch {
		case htmlExtensions[ext]:
			sf.htmlFile = zf
		case strings.HasPrefix(baseName, "subject_line") || strings.HasPrefix(baseName, "subject-line"):
			sf.subjectFile = zf
		case strings.HasPrefix(baseName, "from_line") || strings.HasPrefix(baseName, "from-line"):
			sf.fromFile = zf
		case ext == ".txt":
			sf.txtFile = zf
		}
	}

	cache := newImageCache()
	var (
		importedCreatives  int
		rehostedImages     int
		subjectLinesCount  int
		fromNamesCount     int
		allSubjectLines    = make(map[string]bool) // deduplicate across subfolders
		allFromNames       = make(map[string]bool)
		uploadErrors       []string
	)

	for dir, sf := range folders {
		if sf.htmlFile == nil {
			continue
		}

		// Extract variant name from folder name (strip common prefixes like "123456_")
		variantName := filepath.Base(dir)
		if idx := strings.Index(variantName, "_"); idx > 0 && idx < 10 {
			variantName = variantName[idx+1:]
		}
		variantName = strings.ReplaceAll(variantName, "_", " ")

		// Read HTML content
		htmlContent, err := readZipFileContent(sf.htmlFile, 2<<20)
		if err != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("read %s: %v", sf.htmlFile.Name, err))
			continue
		}

		// Process HTML through the import engine
		result := classifyAndRewriteHTML(ctx, htmlContent, trackingLink, offerOptoutLink, imageDomain, orgID, h.imageCDN, cache)
		uploadErrors = append(uploadErrors, result.Errors...)
		rehostedImages += result.RehostedImages

		// Insert as a creative
		creativeID := uuid.New().String()
		_, dbErr := h.db.ExecContext(ctx, `
			INSERT INTO mailing_offer_creatives
			(id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
			VALUES ($1, $2, 1, $3, 'imported', $4, NOW(), NOW())`,
			creativeID, offerID, result.RewrittenHTML,
			fmt.Sprintf("Imported from bundle: %s", variantName))
		if dbErr != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("db insert creative %s: %v", variantName, dbErr))
			continue
		}
		importedCreatives++
		log.Printf("[HTMLImport] imported creative %s from %s (%d images rehosted)", creativeID, variantName, result.RehostedImages)

		// Process subject lines
		if sf.subjectFile != nil {
			lines, _ := readZipFileLines(sf.subjectFile)
			for _, line := range lines {
				if line != "" && !allSubjectLines[line] {
					allSubjectLines[line] = true
					subjectLinesCount++
				}
			}
		}

		// Process from names
		if sf.fromFile != nil {
			lines, _ := readZipFileLines(sf.fromFile)
			for _, line := range lines {
				if line != "" && !allFromNames[line] {
					allFromNames[line] = true
					fromNamesCount++
				}
			}
		}
	}

	// Bulk insert deduplicated subject lines
	for line := range allSubjectLines {
		slID := uuid.New().String()
		h.db.ExecContext(ctx, `
			INSERT INTO mailing_offer_subject_lines (id, offer_id, subject_line, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'active', NOW(), NOW())
			ON CONFLICT DO NOTHING`, slID, offerID, line)
	}

	// Bulk insert deduplicated from names
	for name := range allFromNames {
		fnID := uuid.New().String()
		h.db.ExecContext(ctx, `
			INSERT INTO mailing_offer_from_names (id, offer_id, from_name, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'active', NOW(), NOW())
			ON CONFLICT DO NOTHING`, fnID, offerID, name)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"imported_creatives":     importedCreatives,
		"uploaded_images":        rehostedImages,
		"subject_lines_imported": subjectLinesCount,
		"from_names_imported":    fromNamesCount,
		"errors":                 len(uploadErrors),
		"error_details":          uploadErrors,
	})
}

func readZipFileContent(zf *zip.File, maxBytes int64) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func readZipFileLines(zf *zip.File) ([]string, error) {
	content, err := readZipFileContent(zf, 1<<20)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// HandleListAssets — GET /offer-center/offers/{id}/assets
func (h *OfferCreativeAssetsHandlers) HandleListAssets(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}
	ctx := r.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, offer_id, hosted_image_id, asset_role, label, width, height,
			   cdn_url, COALESCE(cdn_url_medium,''), COALESCE(cdn_url_thumbnail,''),
			   original_filename, file_size, COALESCE(mime_type,''), sort_order, created_at
		FROM mailing_offer_creative_assets
		WHERE offer_id = $1
		ORDER BY sort_order ASC, created_at DESC
	`, offerID)
	if err != nil {
		log.Printf("[CreativeAssets] list query failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to list assets")
		return
	}
	defer rows.Close()

	var assets []CreativeAsset
	for rows.Next() {
		var a CreativeAsset
		if err := rows.Scan(
			&a.ID, &a.OfferID, &a.HostedImageID, &a.AssetRole, &a.Label,
			&a.Width, &a.Height, &a.CDNURL, &a.CDNURLMedium, &a.CDNURLThumbnail,
			&a.OriginalFilename, &a.FileSize, &a.MimeType, &a.SortOrder, &a.CreatedAt,
		); err != nil {
			continue
		}
		assets = append(assets, a)
	}
	if assets == nil {
		assets = []CreativeAsset{}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"assets": assets,
		"total":  len(assets),
	})
}

// HandleDeleteAsset — DELETE /offer-center/offers/{id}/assets/{assetId}
func (h *OfferCreativeAssetsHandlers) HandleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	assetID := chi.URLParam(r, "assetId")
	if offerID == "" || assetID == "" {
		respondError(w, http.StatusBadRequest, "offer id and asset id required")
		return
	}
	ctx := r.Context()

	result, err := h.db.ExecContext(ctx, `DELETE FROM mailing_offer_creative_assets WHERE id=$1 AND offer_id=$2`, assetID, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to delete asset")
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondError(w, http.StatusNotFound, "Asset not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// HandleDeleteAllAssets — DELETE /offer-center/offers/{id}/assets/all
func (h *OfferCreativeAssetsHandlers) HandleDeleteAllAssets(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}
	result, err := h.db.ExecContext(r.Context(),
		`DELETE FROM mailing_offer_creative_assets WHERE offer_id=$1`, offerID)
	if err != nil {
		log.Printf("[CreativeAssets] delete all failed for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to delete assets")
		return
	}
	affected, _ := result.RowsAffected()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"deleted": affected,
	})
}

// LoadOfferAssets loads all creative assets for an offer, grouped by role
func LoadOfferAssets(db *sql.DB, offerID string) ([]CreativeAsset, error) {
	rows, err := db.Query(`
		SELECT id, offer_id, hosted_image_id, asset_role, label, width, height,
			   cdn_url, COALESCE(cdn_url_medium,''), COALESCE(cdn_url_thumbnail,''),
			   original_filename, file_size, COALESCE(mime_type,''), sort_order, created_at
		FROM mailing_offer_creative_assets
		WHERE offer_id = $1
		ORDER BY sort_order ASC, width DESC
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assets []CreativeAsset
	for rows.Next() {
		var a CreativeAsset
		if err := rows.Scan(
			&a.ID, &a.OfferID, &a.HostedImageID, &a.AssetRole, &a.Label,
			&a.Width, &a.Height, &a.CDNURL, &a.CDNURLMedium, &a.CDNURLThumbnail,
			&a.OriginalFilename, &a.FileSize, &a.MimeType, &a.SortOrder, &a.CreatedAt,
		); err != nil {
			continue
		}
		assets = append(assets, a)
	}
	return assets, nil
}

// ResolveImageDomain looks up a verified img.{domain} from mailing_image_domains for the org.
// Falls back to the default IMAGE_CDN_DOMAIN env var if no verified domain exists.
func ResolveImageDomain(db *sql.DB, orgID string) string {
	var domain string
	err := db.QueryRow(`
		SELECT domain FROM mailing_image_domains
		WHERE org_id = $1 AND verified = true
		ORDER BY created_at ASC LIMIT 1
	`, orgID).Scan(&domain)
	if err == nil && domain != "" {
		return domain
	}
	return ""
}

// ResolveImageDomainForBrand checks whether a specific brand image domain has a
// verified CloudFront distribution. Returns the brand domain if verified, otherwise
// falls back to IMAGE_CDN_DOMAIN (img.projectjarvis.io) so URLs actually resolve.
func ResolveImageDomainForBrand(db *sql.DB, brandDomain string) string {
	if brandDomain == "" {
		return os.Getenv("IMAGE_CDN_DOMAIN")
	}
	var verified bool
	err := db.QueryRow(`
		SELECT verified FROM mailing_image_domains
		WHERE domain = $1 LIMIT 1
	`, brandDomain).Scan(&verified)
	if err == nil && verified {
		return brandDomain
	}
	fallback := os.Getenv("IMAGE_CDN_DOMAIN")
	if fallback != "" {
		return fallback
	}
	return brandDomain
}

// rewriteCDNURL replaces the host in a CDN/S3 URL with the given image domain.
// Handles both https://{cdn-or-s3-host}/images/... and bare S3 URLs.
func rewriteCDNURL(originalURL, imageDomain string) string {
	if imageDomain == "" || originalURL == "" {
		return originalURL
	}
	idx := strings.Index(originalURL, "/images/")
	if idx < 0 {
		return originalURL
	}
	return "https://" + imageDomain + originalURL[idx:]
}

// BuildAssetPromptSection generates a prompt section describing available image assets.
// If imageDomain is non-empty, all CDN URLs are rewritten to use that domain.
func BuildAssetPromptSection(assets []CreativeAsset, imageDomain string) string {
	if len(assets) == 0 {
		return ""
	}

	sorted := make([]CreativeAsset, len(assets))
	copy(sorted, assets)
	sort.Slice(sorted, func(i, j int) bool {
		roleOrder := map[string]int{
			"email_hero": 0, "email_header": 1, "email_content": 2,
			"landing_hero": 3, "landing_banner": 4, "landing_billboard": 5,
			"content_block": 6, "mobile_interstitial": 7, "mobile_banner": 8,
			"thumbnail": 9, "sidebar": 10, "banner": 11, "content": 12,
		}
		oi, oj := roleOrder[sorted[i].AssetRole], roleOrder[sorted[j].AssetRole]
		if oi != oj {
			return oi < oj
		}
		return sorted[i].Width*sorted[i].Height > sorted[j].Width*sorted[j].Height
	})

	var sb strings.Builder
	sb.WriteString("\nAVAILABLE BRAND CREATIVE ASSETS (use these REAL images — do NOT use placeholder URLs):\n")
	sb.WriteString("These are high-quality, professionally designed brand images. Use them to create a rich visual experience.\n\n")

	for _, a := range sorted {
		url := rewriteCDNURL(a.CDNURL, imageDomain)
		fmt.Fprintf(&sb, "  - %s | %dx%d | Role: %s\n    URL: %s\n",
			a.Label, a.Width, a.Height, a.AssetRole, url)
	}

	sb.WriteString(`
IMAGE PLACEMENT GUIDELINES:
- For email HERO image: use the largest landscape image (600-800px wide). Set width="100%" max-width="600px" for responsive scaling.
- For email HEADER banner: use 665x256 or similar header-sized image at full width.
- For email INLINE content: use 300x250 images with float or centered layout. Set explicit width/height.
- For landing page HERO: use the tallest vertical image (600x1200) or widest landscape (800x400, 970x250).
- For landing page SECTIONS: use 600x300 or 728x250 images between content sections.
- For landing page PRODUCT image: use 300x250 or 300x600.
- ALWAYS include width and height attributes on <img> tags to prevent layout shift.
- ALWAYS include descriptive alt text.
- Use the EXACT CDN URLs provided — do not modify them.
`)

	return sb.String()
}

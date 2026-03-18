package api

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
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
		respondError(w, http.StatusServiceUnavailable, "Image CDN not configured — set IGNITE_S3_BUCKET")
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
	".txt": true, ".html": true, ".htm": true,
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
		respondError(w, http.StatusServiceUnavailable, "Image CDN not configured — set IGNITE_S3_BUCKET")
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
			content, err := io.ReadAll(io.LimitReader(rc, 1<<20)) // 1 MB max per text file
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

		if zf.UncompressedSize64 > 50<<20 {
			uploadErrors = append(uploadErrors, fmt.Sprintf("skipped %s: exceeds 50MB single-file limit", baseName))
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

// BuildAssetPromptSection generates a prompt section describing available image assets
func BuildAssetPromptSection(assets []CreativeAsset) string {
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
		fmt.Fprintf(&sb, "  - %s | %dx%d | Role: %s\n    URL: %s\n",
			a.Label, a.Width, a.Height, a.AssetRole, a.CDNURL)
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

package mailing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // WebP decode support
)

// ImageSize represents different image size variants
type ImageSize string

const (
	ImageSizeOriginal  ImageSize = "original"
	ImageSizeLarge     ImageSize = "large"    // 1200w
	ImageSizeMedium    ImageSize = "medium"   // 600w
	ImageSizeThumbnail ImageSize = "thumbnail" // 150w
)

// Default size widths
const (
	DefaultLargeWidth     = 1200
	DefaultMediumWidth    = 600
	DefaultThumbnailWidth = 150
	DefaultJPEGQuality    = 85
	MaxFileSizeMB         = 10
)

// Supported content types
var SupportedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// ImageCDNService handles image hosting with CDN support
type ImageCDNService struct {
	db        *sql.DB
	s3Client  *s3.Client
	cfClient  *cloudfront.Client
	acmClient *acm.Client
	bucket    string
	cdnDomain string
	region    string
	awsInfra  *AWSInfrastructureService // AWS infrastructure for full provisioning
}

// ImageCDNConfig holds configuration for the Image CDN service
type ImageCDNConfig struct {
	Region             string
	BucketName         string
	CDNDomain          string
	DefaultCacheTTL    int64
	MaxCacheTTL        int64
	EnableCompression  bool
	PriceClass         string // "PriceClass_100", "PriceClass_200", "PriceClass_All"
}

// DefaultImageCDNConfig returns sensible defaults
func DefaultImageCDNConfig() ImageCDNConfig {
	return ImageCDNConfig{
		Region:            "us-east-1",
		DefaultCacheTTL:   86400,    // 1 day
		MaxCacheTTL:       31536000, // 1 year
		EnableCompression: true,
		PriceClass:        "PriceClass_100", // US, Canada, Europe
	}
}

// CloudFrontDistribution represents a CloudFront distribution configuration
type CloudFrontDistribution struct {
	ID                  string    `json:"id"`
	DomainName          string    `json:"domain_name"`
	Status              string    `json:"status"`
	CustomDomain        string    `json:"custom_domain,omitempty"`
	CertificateARN      string    `json:"certificate_arn,omitempty"`
	OriginBucket        string    `json:"origin_bucket"`
	Enabled             bool      `json:"enabled"`
	CreatedAt           time.Time `json:"created_at"`
}

// HostedImage represents an uploaded and hosted image
type HostedImage struct {
	ID              string    `json:"id"`
	OrgID           string    `json:"org_id"`
	Filename        string    `json:"filename"`
	OriginalFilename string   `json:"original_filename"`
	ContentType     string    `json:"content_type"`
	Size            int64     `json:"size"`
	Width           int       `json:"width"`
	Height          int       `json:"height"`
	S3Key           string    `json:"s3_key"`
	S3KeyThumbnail  string    `json:"s3_key_thumbnail,omitempty"`
	S3KeyMedium     string    `json:"s3_key_medium,omitempty"`
	S3KeyLarge      string    `json:"s3_key_large,omitempty"`
	CDNURL          string    `json:"cdn_url"`
	CDNURLThumbnail string    `json:"cdn_url_thumbnail,omitempty"`
	CDNURLMedium    string    `json:"cdn_url_medium,omitempty"`
	CDNURLLarge     string    `json:"cdn_url_large,omitempty"`
	Checksum        string    `json:"checksum,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ImageDomain represents a custom CDN domain for image hosting
type ImageDomain struct {
	ID                      string     `json:"id"`
	OrgID                   string     `json:"org_id"`
	Domain                  string     `json:"domain"`
	Verified                bool       `json:"verified"`
	VerificationToken       string     `json:"verification_token,omitempty"`
	VerificationMethod      string     `json:"verification_method"`
	SSLStatus               string     `json:"ssl_status"`
	S3Bucket                string     `json:"s3_bucket,omitempty"`
	CloudFrontDistID        string     `json:"cloudfront_distribution_id,omitempty"`
	CloudFrontDomain        string     `json:"cloudfront_domain,omitempty"`
	ACMCertARN              string     `json:"acm_cert_arn,omitempty"`
	LastVerifiedAt          *time.Time `json:"last_verified_at,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
}

// ImageUploadOptions contains options for image upload
type ImageUploadOptions struct {
	GenerateThumbnails bool
	OptimizeForWeb     bool
	StripMetadata      bool
	Quality            int // JPEG quality 1-100
}

// DefaultUploadOptions returns default upload options
func DefaultUploadOptions() ImageUploadOptions {
	return ImageUploadOptions{
		GenerateThumbnails: true,
		OptimizeForWeb:     true,
		StripMetadata:      true,
		Quality:            DefaultJPEGQuality,
	}
}

// ImageStorageStats contains storage statistics for an organization
type ImageStorageStats struct {
	TotalImages       int64   `json:"total_images"`
	TotalSizeBytes    int64   `json:"total_size_bytes"`
	TotalSizeMB       float64 `json:"total_size_mb"`
	ImagesThisMonth   int64   `json:"images_this_month"`
	SizeThisMonthBytes int64  `json:"size_this_month_bytes"`
}

// NewImageCDNService creates a new ImageCDNService instance
func NewImageCDNService(db *sql.DB, s3Client *s3.Client, bucket, cdnDomain, region string) *ImageCDNService {
	return &ImageCDNService{
		db:        db,
		s3Client:  s3Client,
		bucket:    bucket,
		cdnDomain: cdnDomain,
		region:    region,
	}
}

// NewImageCDNServiceWithAWS creates a new ImageCDNService with full AWS SDK clients
func NewImageCDNServiceWithAWS(ctx context.Context, db *sql.DB, cfg ImageCDNConfig) (*ImageCDNService, error) {
	// Load AWS config from default profile
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	// Create S3 client
	s3Client := s3.NewFromConfig(awsCfg)

	// Create CloudFront client (CloudFront API is global, but use us-east-1)
	cfCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("loading CloudFront AWS config: %w", err)
	}
	cfClient := cloudfront.NewFromConfig(cfCfg)

	// Create ACM client (must be us-east-1 for CloudFront certificates)
	acmClient := acm.NewFromConfig(cfCfg)

	service := &ImageCDNService{
		db:        db,
		s3Client:  s3Client,
		cfClient:  cfClient,
		acmClient: acmClient,
		bucket:    cfg.BucketName,
		cdnDomain: cfg.CDNDomain,
		region:    cfg.Region,
	}

	return service, nil
}

// GetCDNDomain returns the configured CDN domain for URL matching
func (s *ImageCDNService) GetCDNDomain() string {
	return s.cdnDomain
}

// GetBucket returns the configured S3 bucket name
func (s *ImageCDNService) GetBucket() string {
	return s.bucket
}

// BuildPublicCDNURL constructs the public CDN URL for a given S3 key
func (s *ImageCDNService) BuildPublicCDNURL(key string) string {
	return s.buildCDNURL(key)
}

// FindImageByChecksum looks up an existing hosted image by SHA-256 checksum and org
func (s *ImageCDNService) FindImageByChecksum(ctx context.Context, orgID, checksum string) (*HostedImage, error) {
	var img HostedImage
	var createdAt time.Time
	var s3KeyThumb, s3KeyMed, s3KeyLarge sql.NullString
	var cdnURLThumb, cdnURLMed, cdnURLLarge sql.NullString
	var existingChecksum sql.NullString
	var width, height sql.NullInt32

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, filename, original_filename, content_type, size,
			   width, height, s3_key, s3_key_thumbnail, s3_key_medium, s3_key_large,
			   cdn_url, cdn_url_thumbnail, cdn_url_medium, cdn_url_large,
			   checksum, created_at
		FROM mailing_hosted_images
		WHERE org_id = $1 AND checksum = $2
		LIMIT 1
	`, orgID, checksum).Scan(
		&img.ID, &img.OrgID, &img.Filename, &img.OriginalFilename, &img.ContentType, &img.Size,
		&width, &height, &img.S3Key, &s3KeyThumb, &s3KeyMed, &s3KeyLarge,
		&img.CDNURL, &cdnURLThumb, &cdnURLMed, &cdnURLLarge,
		&existingChecksum, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil // Not found, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("querying image by checksum: %w", err)
	}

	img.Width = int(width.Int32)
	img.Height = int(height.Int32)
	img.S3KeyThumbnail = s3KeyThumb.String
	img.S3KeyMedium = s3KeyMed.String
	img.S3KeyLarge = s3KeyLarge.String
	img.CDNURLThumbnail = cdnURLThumb.String
	img.CDNURLMedium = cdnURLMed.String
	img.CDNURLLarge = cdnURLLarge.String
	img.Checksum = existingChecksum.String
	img.CreatedAt = createdAt

	return &img, nil
}

// SetCloudFrontClient allows setting the CloudFront client after creation
func (s *ImageCDNService) SetCloudFrontClient(cfClient *cloudfront.Client) {
	s.cfClient = cfClient
}

// SetACMClient allows setting the ACM client after creation
func (s *ImageCDNService) SetACMClient(acmClient *acm.Client) {
	s.acmClient = acmClient
}

// SetAWSInfrastructure sets the AWS infrastructure service for full provisioning
func (s *ImageCDNService) SetAWSInfrastructure(awsInfra *AWSInfrastructureService) {
	s.awsInfra = awsInfra
}

// ProvisionImageDomainWithAWS creates full AWS infrastructure for an image domain
func (s *ImageCDNService) ProvisionImageDomainWithAWS(ctx context.Context, orgID, domain, bucketName string) (*ImageDomain, error) {
	if s.awsInfra == nil {
		return nil, fmt.Errorf("AWS infrastructure service not configured")
	}

	// First register the domain in the database
	imgDomain, err := s.RegisterImageDomain(ctx, orgID, domain)
	if err != nil {
		return nil, err
	}

	// Update with S3 bucket info
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_image_domains 
		SET s3_bucket = $1, ssl_status = 'provisioning', updated_at = NOW()
		WHERE id = $2
	`, bucketName, imgDomain.ID)
	if err != nil {
		return nil, fmt.Errorf("updating domain with bucket: %w", err)
	}

	imgDomain.S3Bucket = bucketName
	imgDomain.SSLStatus = "provisioning"

	// Start async AWS provisioning
	go func() {
		provCtx := context.Background()
		err := s.awsInfra.ProvisionImageCDN(provCtx, orgID, domain, bucketName)
		if err != nil {
			log.Printf("AWS provisioning failed for image domain %s: %v", domain, err)
			s.db.ExecContext(provCtx, `
				UPDATE mailing_image_domains SET ssl_status = 'failed' WHERE id = $1
			`, imgDomain.ID)
		}
	}()

	return imgDomain, nil
}

// GetImageDomainAWSStatus gets the AWS provisioning status for an image domain
func (s *ImageCDNService) GetImageDomainAWSStatus(ctx context.Context, domainID string) (map[string]interface{}, error) {
	var domain ImageDomain
	var lastVerifiedAt sql.NullTime
	var s3Bucket, cfDistID, cfDomain, acmCertARN sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, domain, verified, ssl_status, s3_bucket, 
		       cloudfront_distribution_id, cloudfront_domain, acm_cert_arn, last_verified_at, created_at
		FROM mailing_image_domains
		WHERE id = $1
	`, domainID).Scan(
		&domain.ID, &domain.OrgID, &domain.Domain, &domain.Verified, &domain.SSLStatus,
		&s3Bucket, &cfDistID, &cfDomain, &acmCertARN, &lastVerifiedAt, &domain.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("domain not found")
	}
	if err != nil {
		return nil, fmt.Errorf("querying domain: %w", err)
	}

	status := map[string]interface{}{
		"domain":                    domain.Domain,
		"verified":                  domain.Verified,
		"ssl_status":                domain.SSLStatus,
		"s3_bucket":                 s3Bucket.String,
		"cloudfront_distribution_id": cfDistID.String,
		"cloudfront_domain":         cfDomain.String,
		"acm_cert_arn":              acmCertARN.String,
	}

	// Get live status from AWS if available
	if s.awsInfra != nil && acmCertARN.Valid && acmCertARN.String != "" {
		certStatus, err := s.awsInfra.CheckCertificateStatus(ctx, acmCertARN.String)
		if err == nil {
			status["acm_status"] = certStatus
		}
	}

	if s.awsInfra != nil && cfDistID.Valid && cfDistID.String != "" {
		distStatus, err := s.awsInfra.GetDistributionStatus(ctx, cfDistID.String)
		if err == nil {
			status["cloudfront_status"] = distStatus
		}
	}

	return status, nil
}

// CreateS3BucketForOrg creates an S3 bucket for an organization's images
func (s *ImageCDNService) CreateS3BucketForOrg(ctx context.Context, orgID, bucketName string) error {
	if s.awsInfra == nil {
		return fmt.Errorf("AWS infrastructure service not configured")
	}

	_, err := s.awsInfra.CreateImageBucket(ctx, orgID, bucketName)
	return err
}

// UploadImage uploads an image and creates resized variants
func (s *ImageCDNService) UploadImage(ctx context.Context, orgID string, filename string, file io.Reader) (*HostedImage, error) {
	return s.UploadImageWithOptions(ctx, orgID, filename, file, DefaultUploadOptions())
}

// UploadImageWithOptions uploads an image with custom options
func (s *ImageCDNService) UploadImageWithOptions(ctx context.Context, orgID string, filename string, file io.Reader, opts ImageUploadOptions) (*HostedImage, error) {
	// Read the file into memory with size limit to prevent OOM on large uploads
	maxBytes := int64(MaxFileSizeMB*1024*1024) + 1
	limitedReader := io.LimitReader(file, maxBytes)
	data, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	// Check file size
	if len(data) > MaxFileSizeMB*1024*1024 {
		return nil, fmt.Errorf("file size exceeds maximum of %d MB", MaxFileSizeMB)
	}

	// Detect content type
	contentType := detectContentType(data)
	if !SupportedImageTypes[contentType] {
		return nil, fmt.Errorf("unsupported image type: %s", contentType)
	}

	// Decode image to get dimensions
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Generate unique ID and S3 keys
	imageID := uuid.New().String()
	now := time.Now()
	year := now.Format("2006")
	month := now.Format("01")
	ext := getExtension(contentType)
	sanitizedFilename := sanitizeFilename(filename)

	baseKey := fmt.Sprintf("images/%s/%s/%s/%s", orgID, year, month, imageID)
	s3Key := fmt.Sprintf("%s_original%s", baseKey, ext)

	// Calculate checksum
	hash := sha256.Sum256(data)
	checksum := hex.EncodeToString(hash[:])

	// Upload original image
	if err := s.uploadToS3(ctx, s3Key, data, contentType); err != nil {
		return nil, fmt.Errorf("uploading original to S3: %w", err)
	}

	// Create hosted image record
	hostedImage := &HostedImage{
		ID:               imageID,
		OrgID:            orgID,
		Filename:         sanitizedFilename,
		OriginalFilename: filename,
		ContentType:      contentType,
		Size:             int64(len(data)),
		Width:            width,
		Height:           height,
		S3Key:            s3Key,
		CDNURL:           s.buildCDNURL(s3Key),
		Checksum:         checksum,
		CreatedAt:        now,
	}

	// Generate resized variants if enabled
	if opts.GenerateThumbnails {
		// Generate large (1200w) if original is larger
		if width > DefaultLargeWidth {
			largeKey := fmt.Sprintf("%s_1200w%s", baseKey, ext)
			if resizedData, err := s.resizeImage(img, DefaultLargeWidth, format, opts.Quality); err == nil {
				if err := s.uploadToS3(ctx, largeKey, resizedData, contentType); err == nil {
					hostedImage.S3KeyLarge = largeKey
					hostedImage.CDNURLLarge = s.buildCDNURL(largeKey)
				}
			}
		}

		// Generate medium (600w) if original is larger
		if width > DefaultMediumWidth {
			mediumKey := fmt.Sprintf("%s_600w%s", baseKey, ext)
			if resizedData, err := s.resizeImage(img, DefaultMediumWidth, format, opts.Quality); err == nil {
				if err := s.uploadToS3(ctx, mediumKey, resizedData, contentType); err == nil {
					hostedImage.S3KeyMedium = mediumKey
					hostedImage.CDNURLMedium = s.buildCDNURL(mediumKey)
				}
			}
		}

		// Generate thumbnail (150w)
		thumbKey := fmt.Sprintf("%s_150w%s", baseKey, ext)
		if resizedData, err := s.resizeImage(img, DefaultThumbnailWidth, format, opts.Quality); err == nil {
			if err := s.uploadToS3(ctx, thumbKey, resizedData, contentType); err == nil {
				hostedImage.S3KeyThumbnail = thumbKey
				hostedImage.CDNURLThumbnail = s.buildCDNURL(thumbKey)
			}
		}
	}

	// Save to database
	if err := s.saveHostedImage(ctx, hostedImage); err != nil {
		return nil, fmt.Errorf("saving to database: %w", err)
	}

	return hostedImage, nil
}

// GetImage retrieves an image by ID
func (s *ImageCDNService) GetImage(ctx context.Context, imageID string) (*HostedImage, error) {
	var img HostedImage
	var createdAt time.Time
	var s3KeyThumb, s3KeyMed, s3KeyLarge sql.NullString
	var cdnURLThumb, cdnURLMed, cdnURLLarge sql.NullString
	var checksum sql.NullString
	var width, height sql.NullInt32

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, filename, original_filename, content_type, size,
			   width, height, s3_key, s3_key_thumbnail, s3_key_medium, s3_key_large,
			   cdn_url, cdn_url_thumbnail, cdn_url_medium, cdn_url_large,
			   checksum, created_at
		FROM mailing_hosted_images
		WHERE id = $1
	`, imageID).Scan(
		&img.ID, &img.OrgID, &img.Filename, &img.OriginalFilename, &img.ContentType, &img.Size,
		&width, &height, &img.S3Key, &s3KeyThumb, &s3KeyMed, &s3KeyLarge,
		&img.CDNURL, &cdnURLThumb, &cdnURLMed, &cdnURLLarge,
		&checksum, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("image not found")
	}
	if err != nil {
		return nil, fmt.Errorf("querying image: %w", err)
	}

	img.Width = int(width.Int32)
	img.Height = int(height.Int32)
	img.S3KeyThumbnail = s3KeyThumb.String
	img.S3KeyMedium = s3KeyMed.String
	img.S3KeyLarge = s3KeyLarge.String
	img.CDNURLThumbnail = cdnURLThumb.String
	img.CDNURLMedium = cdnURLMed.String
	img.CDNURLLarge = cdnURLLarge.String
	img.Checksum = checksum.String
	img.CreatedAt = createdAt

	return &img, nil
}

// ListImages returns paginated images for an organization
func (s *ImageCDNService) ListImages(ctx context.Context, orgID string, page, limit int) ([]HostedImage, int, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	// Get total count
	var total int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_hosted_images WHERE org_id = $1
	`, orgID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting images: %w", err)
	}

	// Get images
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, org_id, filename, original_filename, content_type, size,
			   width, height, s3_key, s3_key_thumbnail, s3_key_medium, s3_key_large,
			   cdn_url, cdn_url_thumbnail, cdn_url_medium, cdn_url_large,
			   checksum, created_at
		FROM mailing_hosted_images
		WHERE org_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("querying images: %w", err)
	}
	defer rows.Close()

	var images []HostedImage
	for rows.Next() {
		var img HostedImage
		var createdAt time.Time
		var s3KeyThumb, s3KeyMed, s3KeyLarge sql.NullString
		var cdnURLThumb, cdnURLMed, cdnURLLarge sql.NullString
		var checksum sql.NullString
		var width, height sql.NullInt32

		if err := rows.Scan(
			&img.ID, &img.OrgID, &img.Filename, &img.OriginalFilename, &img.ContentType, &img.Size,
			&width, &height, &img.S3Key, &s3KeyThumb, &s3KeyMed, &s3KeyLarge,
			&img.CDNURL, &cdnURLThumb, &cdnURLMed, &cdnURLLarge,
			&checksum, &createdAt,
		); err != nil {
			continue
		}

		img.Width = int(width.Int32)
		img.Height = int(height.Int32)
		img.S3KeyThumbnail = s3KeyThumb.String
		img.S3KeyMedium = s3KeyMed.String
		img.S3KeyLarge = s3KeyLarge.String
		img.CDNURLThumbnail = cdnURLThumb.String
		img.CDNURLMedium = cdnURLMed.String
		img.CDNURLLarge = cdnURLLarge.String
		img.Checksum = checksum.String
		img.CreatedAt = createdAt

		images = append(images, img)
	}

	if images == nil {
		images = []HostedImage{}
	}

	return images, total, nil
}

// DeleteImage deletes an image and its variants from S3 and database
func (s *ImageCDNService) DeleteImage(ctx context.Context, imageID string) error {
	// Get image details first
	img, err := s.GetImage(ctx, imageID)
	if err != nil {
		return err
	}

	// Delete from S3 - collect all keys to delete
	keysToDelete := []string{img.S3Key}
	if img.S3KeyThumbnail != "" {
		keysToDelete = append(keysToDelete, img.S3KeyThumbnail)
	}
	if img.S3KeyMedium != "" {
		keysToDelete = append(keysToDelete, img.S3KeyMedium)
	}
	if img.S3KeyLarge != "" {
		keysToDelete = append(keysToDelete, img.S3KeyLarge)
	}

	// Delete each S3 object
	for _, key := range keysToDelete {
		_, err := s.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			// Log but continue - we still want to delete from DB
			fmt.Printf("Warning: failed to delete S3 object %s: %v\n", key, err)
		}
	}

	// Delete from database
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM mailing_hosted_images WHERE id = $1
	`, imageID)
	if err != nil {
		return fmt.Errorf("deleting from database: %w", err)
	}

	return nil
}

// GetCDNURL returns the CDN URL for an image, using custom domain if available
func (s *ImageCDNService) GetCDNURL(ctx context.Context, orgID, imageID string) (string, error) {
	// Check for custom verified domain
	var customDomain sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT domain FROM mailing_image_domains
		WHERE org_id = $1 AND verified = true
		ORDER BY created_at ASC
		LIMIT 1
	`, orgID).Scan(&customDomain)

	// Get image
	img, err := s.GetImage(ctx, imageID)
	if err != nil {
		return "", err
	}

	// If custom domain exists and is verified, use it
	if customDomain.Valid && customDomain.String != "" {
		return fmt.Sprintf("https://%s/%s", customDomain.String, imageID), nil
	}

	return img.CDNURL, nil
}

// RegisterImageDomain registers a custom domain for image hosting
func (s *ImageCDNService) RegisterImageDomain(ctx context.Context, orgID, domain string) (*ImageDomain, error) {
	// Validate domain format
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("domain cannot be empty")
	}

	// Generate verification token
	token := uuid.New().String()

	id := uuid.New().String()
	now := time.Now()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_image_domains (id, org_id, domain, verification_token, verification_method, ssl_status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'dns_txt', 'pending', $5, $5)
	`, id, orgID, domain, token, now)
	if err != nil {
		if strings.Contains(err.Error(), "unique") {
			return nil, fmt.Errorf("domain already registered")
		}
		return nil, fmt.Errorf("registering domain: %w", err)
	}

	return &ImageDomain{
		ID:                 id,
		OrgID:              orgID,
		Domain:             domain,
		Verified:           false,
		VerificationToken:  token,
		VerificationMethod: "dns_txt",
		SSLStatus:          "pending",
		CreatedAt:          now,
	}, nil
}

// VerifyImageDomain attempts to verify a custom domain
func (s *ImageCDNService) VerifyImageDomain(ctx context.Context, domainID string) (*ImageDomain, error) {
	// Get domain details
	var domain ImageDomain
	var lastVerifiedAt sql.NullTime
	var createdAt time.Time

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, domain, verified, verification_token, verification_method, ssl_status, last_verified_at, created_at
		FROM mailing_image_domains
		WHERE id = $1
	`, domainID).Scan(
		&domain.ID, &domain.OrgID, &domain.Domain, &domain.Verified,
		&domain.VerificationToken, &domain.VerificationMethod, &domain.SSLStatus,
		&lastVerifiedAt, &createdAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("domain not found")
	}
	if err != nil {
		return nil, fmt.Errorf("querying domain: %w", err)
	}

	domain.CreatedAt = createdAt
	if lastVerifiedAt.Valid {
		domain.LastVerifiedAt = &lastVerifiedAt.Time
	}

	// In a real implementation, we would:
	// 1. Do DNS lookup for TXT record matching verification token
	// 2. Or check for CNAME pointing to our CDN
	// 3. Or check for verification file at /.well-known/
	
	// For now, we'll simulate verification (in production, implement actual DNS verification)
	// This would typically involve:
	// - net.LookupTXT(domain.Domain) to find TXT records
	// - Checking if any TXT record contains the verification token

	// Mark as verified (simulate successful verification)
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_image_domains
		SET verified = true, last_verified_at = $1, ssl_status = 'active', updated_at = $1
		WHERE id = $2
	`, now, domainID)
	if err != nil {
		return nil, fmt.Errorf("updating domain verification: %w", err)
	}

	domain.Verified = true
	domain.SSLStatus = "active"
	domain.LastVerifiedAt = &now

	return &domain, nil
}

// ListImageDomains returns all domains for an organization
func (s *ImageCDNService) ListImageDomains(ctx context.Context, orgID string) ([]ImageDomain, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, org_id, domain, verified, verification_token, verification_method, ssl_status, 
		       s3_bucket, cloudfront_distribution_id, cloudfront_domain, acm_cert_arn,
		       last_verified_at, created_at
		FROM mailing_image_domains
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("querying domains: %w", err)
	}
	defer rows.Close()

	var domains []ImageDomain
	for rows.Next() {
		var domain ImageDomain
		var lastVerifiedAt sql.NullTime
		var createdAt time.Time
		var s3Bucket, cfDistID, cfDomain, acmCertARN sql.NullString

		if err := rows.Scan(
			&domain.ID, &domain.OrgID, &domain.Domain, &domain.Verified,
			&domain.VerificationToken, &domain.VerificationMethod, &domain.SSLStatus,
			&s3Bucket, &cfDistID, &cfDomain, &acmCertARN,
			&lastVerifiedAt, &createdAt,
		); err != nil {
			continue
		}

		domain.CreatedAt = createdAt
		domain.S3Bucket = s3Bucket.String
		domain.CloudFrontDistID = cfDistID.String
		domain.CloudFrontDomain = cfDomain.String
		domain.ACMCertARN = acmCertARN.String
		if lastVerifiedAt.Valid {
			domain.LastVerifiedAt = &lastVerifiedAt.Time
		}

		domains = append(domains, domain)
	}

	if domains == nil {
		domains = []ImageDomain{}
	}

	return domains, nil
}

// GetStorageStats returns storage statistics for an organization
func (s *ImageCDNService) GetStorageStats(ctx context.Context, orgID string) (*ImageStorageStats, error) {
	var stats ImageStorageStats

	err := s.db.QueryRowContext(ctx, `
		SELECT * FROM get_image_storage_stats($1)
	`, orgID).Scan(
		&stats.TotalImages,
		&stats.TotalSizeBytes,
		&stats.TotalSizeMB,
		&stats.ImagesThisMonth,
		&stats.SizeThisMonthBytes,
	)
	if err != nil {
		// If function doesn't exist, calculate manually
		err = s.db.QueryRowContext(ctx, `
			SELECT 
				COUNT(*)::BIGINT,
				COALESCE(SUM(size), 0)::BIGINT,
				ROUND(COALESCE(SUM(size), 0) / 1048576.0, 2),
				COUNT(*) FILTER (WHERE created_at >= date_trunc('month', NOW()))::BIGINT,
				COALESCE(SUM(size) FILTER (WHERE created_at >= date_trunc('month', NOW())), 0)::BIGINT
			FROM mailing_hosted_images
			WHERE org_id = $1
		`, orgID).Scan(
			&stats.TotalImages,
			&stats.TotalSizeBytes,
			&stats.TotalSizeMB,
			&stats.ImagesThisMonth,
			&stats.SizeThisMonthBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("calculating stats: %w", err)
		}
	}

	return &stats, nil
}

// TrackImageUsage records image usage in a campaign or template
func (s *ImageCDNService) TrackImageUsage(ctx context.Context, imageID string, campaignID, templateID *string, usageType string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_image_usage (image_id, campaign_id, template_id, usage_type)
		VALUES ($1, $2, $3, $4)
	`, imageID, campaignID, templateID, usageType)
	if err != nil {
		return fmt.Errorf("tracking usage: %w", err)
	}
	return nil
}

// Helper methods

func (s *ImageCDNService) uploadToS3(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(data),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("public, max-age=31536000"), // 1 year cache
	})
	return err
}

func (s *ImageCDNService) buildCDNURL(key string) string {
	if s.cdnDomain != "" {
		return fmt.Sprintf("https://%s/%s", s.cdnDomain, key)
	}
	// Fallback to direct S3 URL
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, key)
}

func (s *ImageCDNService) saveHostedImage(ctx context.Context, img *HostedImage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_hosted_images (
			id, org_id, filename, original_filename, content_type, size,
			width, height, s3_key, s3_key_thumbnail, s3_key_medium, s3_key_large,
			cdn_url, cdn_url_thumbnail, cdn_url_medium, cdn_url_large,
			checksum, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, $18
		)
	`,
		img.ID, img.OrgID, img.Filename, img.OriginalFilename, img.ContentType, img.Size,
		img.Width, img.Height, img.S3Key, nullIfEmpty(img.S3KeyThumbnail), nullIfEmpty(img.S3KeyMedium), nullIfEmpty(img.S3KeyLarge),
		img.CDNURL, nullIfEmpty(img.CDNURLThumbnail), nullIfEmpty(img.CDNURLMedium), nullIfEmpty(img.CDNURLLarge),
		img.Checksum, img.CreatedAt,
	)
	return err
}

func (s *ImageCDNService) resizeImage(img image.Image, maxWidth int, format string, quality int) ([]byte, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Calculate new dimensions maintaining aspect ratio
	if width <= maxWidth {
		return nil, fmt.Errorf("image already smaller than target")
	}

	newWidth := maxWidth
	newHeight := int(float64(height) * float64(maxWidth) / float64(width))

	// Create new image
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	// Encode to buffer
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	case "png":
		if err := png.Encode(&buf, dst); err != nil {
			return nil, err
		}
	case "gif":
		if err := gif.Encode(&buf, dst, nil); err != nil {
			return nil, err
		}
	default:
		// Default to JPEG for unknown formats
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// Utility functions

func detectContentType(data []byte) string {
	// Check magic bytes
	if len(data) >= 2 {
		if data[0] == 0xFF && data[1] == 0xD8 {
			return "image/jpeg"
		}
	}
	if len(data) >= 8 {
		if data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
			return "image/png"
		}
	}
	if len(data) >= 6 {
		if (data[0] == 'G' && data[1] == 'I' && data[2] == 'F') {
			return "image/gif"
		}
	}
	if len(data) >= 12 {
		if data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
			data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P' {
			return "image/webp"
		}
	}
	return "application/octet-stream"
}

func getExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

func sanitizeFilename(filename string) string {
	// Remove path components
	filename = filepath.Base(filename)
	// Remove dangerous characters
	filename = strings.ReplaceAll(filename, "..", "")
	filename = strings.ReplaceAll(filename, "/", "")
	filename = strings.ReplaceAll(filename, "\\", "")
	// Limit length
	if len(filename) > 200 {
		ext := filepath.Ext(filename)
		filename = filename[:200-len(ext)] + ext
	}
	return filename
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

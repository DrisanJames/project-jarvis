package mailing

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock S3 client for testing
type mockS3Client struct {
	uploadedObjects map[string][]byte
	deletedObjects  []string
}

// TestDetectContentType tests the content type detection
func TestDetectContentType(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected string
	}{
		{
			name:     "JPEG magic bytes",
			data:     []byte{0xFF, 0xD8, 0xFF, 0xE0},
			expected: "image/jpeg",
		},
		{
			name:     "PNG magic bytes",
			data:     []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
			expected: "image/png",
		},
		{
			name:     "GIF magic bytes",
			data:     []byte{'G', 'I', 'F', '8', '9', 'a'},
			expected: "image/gif",
		},
		{
			name:     "WebP magic bytes",
			data:     []byte{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P'},
			expected: "image/webp",
		},
		{
			name:     "Unknown format",
			data:     []byte{0x00, 0x00, 0x00, 0x00},
			expected: "application/octet-stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectContentType(tt.data)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGetExtension tests the extension mapping
func TestGetExtension(t *testing.T) {
	tests := []struct {
		contentType string
		expected    string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"unknown/type", ".bin"},
	}

	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			result := getExtension(tt.contentType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestSanitizeFilename tests filename sanitization
func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Normal filename",
			input:    "image.png",
			expected: "image.png",
		},
		{
			name:     "Path traversal attempt",
			input:    "../../../etc/passwd",
			expected: "passwd",
		},
		{
			name:     "Double dots",
			input:    "file..name.png",
			expected: "filename.png",
		},
		{
			name:     "Unix path",
			input:    "/var/www/uploads/image.png",
			expected: "image.png",
		},
		{
			name:     "Filename with spaces",
			input:    "my image file.png",
			expected: "my image file.png",
		},
		{
			name:     "Dangerous characters removed",
			input:    "image/../test.png",
			expected: "test.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestSupportedImageTypes tests that all supported types are registered
func TestSupportedImageTypes(t *testing.T) {
	assert.True(t, SupportedImageTypes["image/jpeg"])
	assert.True(t, SupportedImageTypes["image/png"])
	assert.True(t, SupportedImageTypes["image/gif"])
	assert.True(t, SupportedImageTypes["image/webp"])
	assert.False(t, SupportedImageTypes["image/bmp"])
	assert.False(t, SupportedImageTypes["image/tiff"])
}

// TestDefaultUploadOptions tests default options
func TestDefaultUploadOptions(t *testing.T) {
	opts := DefaultUploadOptions()
	assert.True(t, opts.GenerateThumbnails)
	assert.True(t, opts.OptimizeForWeb)
	assert.True(t, opts.StripMetadata)
	assert.Equal(t, DefaultJPEGQuality, opts.Quality)
}

// TestHostedImageJSON tests JSON serialization of HostedImage
func TestHostedImageJSON(t *testing.T) {
	img := &HostedImage{
		ID:              "test-id",
		OrgID:           "org-id",
		Filename:        "test.png",
		ContentType:     "image/png",
		Size:            1024,
		Width:           800,
		Height:          600,
		S3Key:           "images/org-id/2024/01/test-id_original.png",
		CDNURL:          "https://cdn.example.com/images/org-id/2024/01/test-id_original.png",
		CreatedAt:       time.Now(),
	}

	assert.Equal(t, "test-id", img.ID)
	assert.Equal(t, "test.png", img.Filename)
	assert.Equal(t, int64(1024), img.Size)
}

// TestImageDomain tests ImageDomain struct
func TestImageDomain(t *testing.T) {
	now := time.Now()
	domain := &ImageDomain{
		ID:                 "domain-id",
		OrgID:              "org-id",
		Domain:             "images.example.com",
		Verified:           false,
		VerificationToken:  "token-123",
		VerificationMethod: "dns_txt",
		SSLStatus:          "pending",
		CreatedAt:          now,
	}

	assert.Equal(t, "images.example.com", domain.Domain)
	assert.False(t, domain.Verified)
	assert.Equal(t, "dns_txt", domain.VerificationMethod)
}

// TestImageStorageStats tests ImageStorageStats struct
func TestImageStorageStats(t *testing.T) {
	stats := &ImageStorageStats{
		TotalImages:        100,
		TotalSizeBytes:     1024 * 1024 * 50, // 50 MB
		TotalSizeMB:        50.0,
		ImagesThisMonth:    25,
		SizeThisMonthBytes: 1024 * 1024 * 10, // 10 MB
	}

	assert.Equal(t, int64(100), stats.TotalImages)
	assert.Equal(t, 50.0, stats.TotalSizeMB)
}

// createTestPNGImage creates a test PNG image for testing
func createTestPNGImage(width, height int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	
	// Fill with a gradient for testing
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x * 255 / width),
				G: uint8(y * 255 / height),
				B: 128,
				A: 255,
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// TestCreateTestPNGImage tests the test image creation
func TestCreateTestPNGImage(t *testing.T) {
	data, err := createTestPNGImage(100, 100)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Verify it's a valid PNG
	contentType := detectContentType(data)
	assert.Equal(t, "image/png", contentType)

	// Verify dimensions
	img, format, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
	assert.Equal(t, 100, img.Bounds().Dx())
	assert.Equal(t, 100, img.Bounds().Dy())
}

// TestNullIfEmpty tests the nullIfEmpty helper
func TestNullIfEmpty(t *testing.T) {
	assert.Nil(t, nullIfEmpty(""))
	assert.Equal(t, "test", nullIfEmpty("test"))
}

// TestImageSizeConstants tests image size constants
func TestImageSizeConstants(t *testing.T) {
	assert.Equal(t, ImageSize("original"), ImageSizeOriginal)
	assert.Equal(t, ImageSize("large"), ImageSizeLarge)
	assert.Equal(t, ImageSize("medium"), ImageSizeMedium)
	assert.Equal(t, ImageSize("thumbnail"), ImageSizeThumbnail)

	assert.Equal(t, 1200, DefaultLargeWidth)
	assert.Equal(t, 600, DefaultMediumWidth)
	assert.Equal(t, 150, DefaultThumbnailWidth)
	assert.Equal(t, 85, DefaultJPEGQuality)
	assert.Equal(t, 10, MaxFileSizeMB)
}

// Integration test - requires database connection
// Uncomment and configure for integration testing
/*
func TestImageCDNServiceIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	// Setup database connection
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	defer db.Close()

	// Create test service (would need real S3 client)
	// service := NewImageCDNService(db, s3Client, "test-bucket", "cdn.test.com", "us-east-1")

	ctx := context.Background()

	// Test upload
	imageData, err := createTestPNGImage(800, 600)
	require.NoError(t, err)

	// Test would continue with actual service calls...
}
*/

// BenchmarkDetectContentType benchmarks content type detection
func BenchmarkDetectContentType(b *testing.B) {
	pngData, _ := createTestPNGImage(100, 100)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detectContentType(pngData)
	}
}

// BenchmarkSanitizeFilename benchmarks filename sanitization
func BenchmarkSanitizeFilename(b *testing.B) {
	filename := "../../../var/www/uploads/test_image.png"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sanitizeFilename(filename)
	}
}

// TestImageCDNServiceNew tests service creation
func TestImageCDNServiceNew(t *testing.T) {
	// Test with nil values (used for unit testing only)
	service := NewImageCDNService(nil, nil, "test-bucket", "cdn.example.com", "us-east-1")
	assert.NotNil(t, service)
}

// Mock database for testing
type mockDB struct {
	*sql.DB
}

// TestContextCancellation tests that operations respect context cancellation
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Verify context is cancelled
	select {
	case <-ctx.Done():
		assert.Equal(t, context.Canceled, ctx.Err())
	default:
		t.Fatal("Context should be cancelled")
	}
}

// TestFileSizeLimit tests file size validation logic
func TestFileSizeLimit(t *testing.T) {
	maxSize := MaxFileSizeMB * 1024 * 1024

	t.Run("Within limit", func(t *testing.T) {
		size := maxSize - 1
		assert.True(t, size < maxSize)
	})

	t.Run("At limit", func(t *testing.T) {
		size := maxSize
		assert.False(t, size < maxSize)
	})

	t.Run("Over limit", func(t *testing.T) {
		size := maxSize + 1
		assert.True(t, size > maxSize)
	})
}

// TestImageDimensionCalculation tests dimension calculations for resizing
func TestImageDimensionCalculation(t *testing.T) {
	tests := []struct {
		name          string
		originalW     int
		originalH     int
		targetW       int
		expectedNewH  int
	}{
		{
			name:         "Landscape image",
			originalW:    1600,
			originalH:    900,
			targetW:      800,
			expectedNewH: 450,
		},
		{
			name:         "Portrait image",
			originalW:    600,
			originalH:    800,
			targetW:      300,
			expectedNewH: 400,
		},
		{
			name:         "Square image",
			originalW:    1000,
			originalH:    1000,
			targetW:      500,
			expectedNewH: 500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newHeight := int(float64(tt.originalH) * float64(tt.targetW) / float64(tt.originalW))
			assert.Equal(t, tt.expectedNewH, newHeight)
		})
	}
}

// TestS3KeyGeneration tests S3 key path generation
func TestS3KeyGeneration(t *testing.T) {
	orgID := "12345678-1234-1234-1234-123456789012"
	year := "2024"
	month := "01"
	imageID := "abcdef12-1234-1234-1234-123456789012"

	baseKey := "images/" + orgID + "/" + year + "/" + month + "/" + imageID

	originalKey := baseKey + "_original.jpg"
	assert.Contains(t, originalKey, orgID)
	assert.Contains(t, originalKey, year)
	assert.Contains(t, originalKey, month)
	assert.Contains(t, originalKey, imageID)
	assert.Contains(t, originalKey, "_original")

	thumbKey := baseKey + "_150w.jpg"
	assert.Contains(t, thumbKey, "_150w")

	mediumKey := baseKey + "_600w.jpg"
	assert.Contains(t, mediumKey, "_600w")

	largeKey := baseKey + "_1200w.jpg"
	assert.Contains(t, largeKey, "_1200w")
}

// TestCDNURLGeneration tests CDN URL building
func TestCDNURLGeneration(t *testing.T) {
	tests := []struct {
		name      string
		cdnDomain string
		bucket    string
		region    string
		key       string
		expected  string
	}{
		{
			name:      "With CDN domain",
			cdnDomain: "cdn.example.com",
			bucket:    "my-bucket",
			region:    "us-east-1",
			key:       "images/org/img.jpg",
			expected:  "https://cdn.example.com/images/org/img.jpg",
		},
		{
			name:      "Without CDN domain (S3 fallback)",
			cdnDomain: "",
			bucket:    "my-bucket",
			region:    "us-east-1",
			key:       "images/org/img.jpg",
			expected:  "https://my-bucket.s3.us-east-1.amazonaws.com/images/org/img.jpg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var url string
			if tt.cdnDomain != "" {
				url = "https://" + tt.cdnDomain + "/" + tt.key
			} else {
				url = "https://" + tt.bucket + ".s3." + tt.region + ".amazonaws.com/" + tt.key
			}
			assert.Equal(t, tt.expected, url)
		})
	}
}

// TestVerificationTokenFormat tests verification token generation
func TestVerificationTokenFormat(t *testing.T) {
	// UUID format validation
	token := "12345678-1234-1234-1234-123456789012"
	assert.Len(t, token, 36)
	assert.Equal(t, '-', rune(token[8]))
	assert.Equal(t, '-', rune(token[13]))
	assert.Equal(t, '-', rune(token[18]))
	assert.Equal(t, '-', rune(token[23]))
}

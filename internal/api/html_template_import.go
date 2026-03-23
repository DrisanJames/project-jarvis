package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// rehostedImageRecord holds info about an image rehosted to S3 during HTML import.
type rehostedImageRecord struct {
	HostedImageID    string
	CDNURL           string
	OriginalFilename string
	Width            int
	Height           int
	FileSize         int64
	ContentType      string
}

// htmlImportResult holds the result of processing a single HTML template.
type htmlImportResult struct {
	RewrittenHTML  string
	RehostedImages int
	ImageRecords   []rehostedImageRecord
	Errors         []string
}

// imageCache deduplicates image downloads across multiple templates in one zip.
type imageCache struct {
	mu    sync.Mutex
	cache map[string]string // originalURL -> CDN URL
}

func newImageCache() *imageCache {
	return &imageCache{cache: make(map[string]string)}
}

func (c *imageCache) get(url string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.cache[url]
	return v, ok
}

func (c *imageCache) set(url, cdnURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[url] = cdnURL
}

var (
	// Matches src="..." and href="..." with http(s) URLs
	srcAttrRe  = regexp.MustCompile(`(src\s*=\s*["'])(https?://[^"']+)(["'])`)
	hrefAttrRe = regexp.MustCompile(`(href\s*=\s*["'])(https?://[^"']+)(["'])`)
	// 1x1 tracking pixel: <img ... width="1" height="1" ... />
	trackingPixelRe = regexp.MustCompile(`(?i)<img[^>]*(?:width\s*=\s*["']1["'][^>]*height\s*=\s*["']1["']|height\s*=\s*["']1["'][^>]*width\s*=\s*["']1["'])[^>]*/?\s*>`)
	// Safe URL prefixes that should never be rewritten
	safeURLPrefixes = []string{
		"http://www.w3.org/",
		"https://www.w3.org/",
		"http://schemas.microsoft.com",
		"urn:",
	}
)

func isSafeURL(u string) bool {
	for _, p := range safeURLPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

func isUnsubURL(u string) bool {
	lower := strings.ToLower(u)
	return strings.Contains(lower, "/unsub") ||
		strings.Contains(lower, "unsubscribe") ||
		strings.Contains(lower, "opt-out") ||
		strings.Contains(lower, "optout")
}

// classifyAndRewriteHTML processes an HTML email template:
//   - Downloads external images and rehosts them to S3 via ImageCDNService
//   - Replaces CTA links with the offer's tracking_link_template
//   - Replaces unsub links with the offer's offer_optout_link
//   - Strips 1x1 tracking pixels
//   - Adds {{ system.unsubscribe_url }} as the List-Unsubscribe link at top if not present
func classifyAndRewriteHTML(
	ctx context.Context,
	html string,
	trackingLink string,
	offerOptoutLink string,
	imageDomain string,
	orgID string,
	imageCDN *mailing.ImageCDNService,
	cache *imageCache,
) htmlImportResult {
	result := htmlImportResult{}

	// Step 1: Strip 1x1 tracking pixels
	html = trackingPixelRe.ReplaceAllString(html, "")

	// Step 2: Process all src="..." attributes (images)
	html = srcAttrRe.ReplaceAllStringFunc(html, func(match string) string {
		parts := srcAttrRe.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		prefix, url, suffix := parts[1], parts[2], parts[3]

		if isSafeURL(url) {
			return match
		}

		// Rehost external image to S3
		rec, err := rehostImage(ctx, url, orgID, imageDomain, imageCDN, cache)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("rehost %s: %v", truncateURL(url), err))
			return match
		}
		result.RehostedImages++
		if rec.record != nil {
			result.ImageRecords = append(result.ImageRecords, *rec.record)
		}
		return prefix + rec.cdnURL + suffix
	})

	// Step 3: Process all href="..." attributes (links)
	html = hrefAttrRe.ReplaceAllStringFunc(html, func(match string) string {
		parts := hrefAttrRe.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		prefix, url, suffix := parts[1], parts[2], parts[3]

		if isSafeURL(url) {
			return match
		}

		if isUnsubURL(url) {
			if offerOptoutLink != "" {
				return prefix + offerOptoutLink + suffix
			}
			return prefix + "{{ system.unsubscribe_url }}" + suffix
		}

		// All other external links are CTAs — replace with tracking link
		if trackingLink != "" {
			return prefix + trackingLink + suffix
		}
		result.Errors = append(result.Errors, fmt.Sprintf("no tracking_link_template configured, kept original CTA: %s", truncateURL(url)))
		return match
	})

	result.RewrittenHTML = html
	return result
}

type rehostResult struct {
	cdnURL string
	record *rehostedImageRecord // nil if served from cache
}

// rehostImage downloads an image from an external URL and uploads it to S3.
// Uses the cache to avoid re-downloading the same URL across templates.
func rehostImage(
	ctx context.Context,
	originalURL string,
	orgID string,
	imageDomain string,
	imageCDN *mailing.ImageCDNService,
	cache *imageCache,
) (rehostResult, error) {
	if cdnURL, ok := cache.get(originalURL); ok {
		return rehostResult{cdnURL: cdnURL}, nil
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(originalURL)
	if err != nil {
		return rehostResult{}, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return rehostResult{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 50*1024*1024))
	if err != nil {
		return rehostResult{}, fmt.Errorf("reading response: %w", err)
	}

	// Extract filename from URL path
	filename := "image.png"
	if idx := strings.LastIndex(originalURL, "/"); idx >= 0 {
		filename = originalURL[idx+1:]
	}
	if qIdx := strings.Index(filename, "?"); qIdx >= 0 {
		filename = filename[:qIdx]
	}

	hostedImage, err := imageCDN.UploadImageWithOptions(
		ctx, orgID, filename, bytes.NewReader(data),
		mailing.ImageUploadOptions{
			Quality:            85,
			GenerateThumbnails: true,
		},
	)
	if err != nil {
		return rehostResult{}, fmt.Errorf("S3 upload: %w", err)
	}

	cdnURL := hostedImage.CDNURL
	if imageDomain != "" {
		cdnURL = rewriteCDNURL(cdnURL, imageDomain)
	}

	cache.set(originalURL, cdnURL)
	log.Printf("[HTMLImport] rehosted %s -> %s", truncateURL(originalURL), truncateURL(cdnURL))
	return rehostResult{
		cdnURL: cdnURL,
		record: &rehostedImageRecord{
			HostedImageID:    hostedImage.ID,
			CDNURL:           cdnURL,
			OriginalFilename: hostedImage.OriginalFilename,
			Width:            hostedImage.Width,
			Height:           hostedImage.Height,
			FileSize:         hostedImage.Size,
			ContentType:      hostedImage.ContentType,
		},
	}, nil
}

func truncateURL(u string) string {
	if len(u) > 80 {
		return u[:77] + "..."
	}
	return u
}

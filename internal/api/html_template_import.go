package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
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

	// Step 4: Append the compliance footer (do-not-reply + unsubscribe + brand ·
	// postal address) at the BOTTOM of the email — never the header, never the
	// corporate identity. The brand is the {{ brand.domain }} merge tag, which the
	// send worker resolves per-recipient to the ACTUAL sending brand domain
	// (item.BrandRoot, e.g. ratesbazar.com / historythinking.com) — so a creative
	// reused across brands always shows the brand it was actually mailed from.
	html = appendUnsubDisclaimer(html, "{{ brand.domain }}", "")

	result.RewrittenHTML = html
	return result
}

const unsubDisclaimerMarker = `<!-- unsub-disclaimer -->`

func buildUnsubDisclaimerHTML(brandName, physicalAddress string) string {
	// Postal address only — the corporate identity ("James Ventures Corp") must
	// never appear in a recipient-facing email. The sender shown is the brand
	// (senderLine), and a valid physical postal address satisfies CAN-SPAM.
	addrLine := "30 N Gould St, Ste R, Sheridan, WY 82801"
	if physicalAddress != "" {
		addrLine = stripJVCName(physicalAddress)
	}
	senderLine := ""
	if brandName != "" {
		senderLine = brandName + " · "
	}
	return unsubDisclaimerMarker +
		`<p style="margin:0;padding:8px 20px;font-family:Arial,Helvetica,sans-serif;font-size:11px;color:#999999;text-align:center;">` +
		`Please do not reply, this email box is not monitored. To stop email subscriptions at any time please ` +
		`<a href="{{ system.unsubscribe_url }}" style="color:#999999;">unsubscribe</a>.` +
		`</p>` +
		`<p style="margin:0;padding:4px 20px 8px;font-family:Arial,Helvetica,sans-serif;font-size:10px;color:#bbbbbb;text-align:center;">` +
		senderLine + addrLine +
		`</p>`
}

// stripJVCName removes the corporate identity ("James Ventures Corp") from a
// caller-supplied postal address so it can never reach a recipient-facing email,
// while preserving the legally-required street/postal address. Idempotent.
func stripJVCName(addr string) string {
	for _, prefix := range []string{
		"James Ventures Corp, ",
		"James Ventures Corp · ",
		"James Ventures Corp &bull; ",
		"James Ventures Corp ",
		"James Ventures Corp",
	} {
		if strings.HasPrefix(addr, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(addr, prefix))
		}
	}
	return addr
}

// appendUnsubDisclaimer inserts the do-not-reply / unsubscribe + brand · postal-
// address block at the BOTTOM of the email — before </body>, or appended to the
// end when the creative is a fragment with no body tag (network creatives usually
// are). brandName identifies the sender (the sending brand/domain); the postal
// address shows only the street address, never the corporate identity (CAN-SPAM).
// This is the ONLY compliance-footer injector — always bottom, never the header.
func appendUnsubDisclaimer(html, brandName, physicalAddress string) string {
	if strings.Contains(html, unsubDisclaimerMarker) {
		return html
	}
	disclaimer := buildUnsubDisclaimerHTML(brandName, physicalAddress)
	lower := strings.ToLower(html)
	if idx := strings.LastIndex(lower, "</body>"); idx >= 0 {
		return html[:idx] + disclaimer + html[idx:]
	}
	return html + disclaimer
}

// stripUnsubDisclaimer removes the injected compliance-footer block (marker +
// its two <p> paragraphs). The inverse of appendUnsubDisclaimer, for
// raw_creative sending profiles (operator 2026-07-25, wcl-heloc): the creative
// ships AS-IS and owns its own compliance. No-op when the marker is absent.
func stripUnsubDisclaimer(html string) string {
	idx := strings.Index(html, unsubDisclaimerMarker)
	if idx < 0 {
		return html
	}
	rest := html[idx+len(unsubDisclaimerMarker):]
	end := 0
	for i := 0; i < 2; i++ {
		p := strings.Index(strings.ToLower(rest[end:]), "</p>")
		if p < 0 {
			break
		}
		end += p + len("</p>")
	}
	return html[:idx] + rest[end:]
}

// rawCreativeDomain reports whether the active sending profile on
// `sendingDomain` (any of em.<apex> / m.<apex> / bare apex forms are checked
// as-given plus m.<apex>) has raw_creative set — the per-domain footer bypass.
func rawCreativeDomain(ctx context.Context, db *sql.DB, sendingDomain string) bool {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	if d == "" {
		return false
	}
	apex := strings.TrimPrefix(strings.TrimPrefix(d, "em."), "m.")
	var raw bool
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(bool_or(COALESCE(raw_creative, FALSE)), FALSE)
		   FROM mailing_sending_profiles
		  WHERE status = 'active' AND sending_domain IN ($1, $2, $3)`,
		d, "m."+apex, "em."+apex).Scan(&raw)
	if err != nil {
		return false
	}
	return raw
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

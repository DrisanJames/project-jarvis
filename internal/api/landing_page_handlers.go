package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type LandingPageHandlers struct {
	db *sql.DB
}

// ---------------------------------------------------------------------------
// Structured content types — these map to the Next.js ReviewArticle model
// ---------------------------------------------------------------------------

type ReviewSection struct {
	Heading  string `json:"heading"`
	Content  string `json:"content"`
	ImageURL string `json:"imageUrl,omitempty"`
	ImageAlt string `json:"imageAlt,omitempty"`
}

type FeatureRating struct {
	Name        string  `json:"name"`
	Rating      float64 `json:"rating"`
	Description string  `json:"description"`
}

type ReviewProduct struct {
	Name       string   `json:"name"`
	Rank       int      `json:"rank"`
	Rating     float64  `json:"rating"`
	ImageURL   string   `json:"imageUrl"`
	Badge      string   `json:"badge"`
	QuickTake  string   `json:"quickTake"`
	KeyFeature string   `json:"keyFeature"`
	Pros       []string `json:"pros"`
	Cons       []string `json:"cons"`
	CTAUrl     string   `json:"ctaUrl"`
	CTAText    string   `json:"ctaText"`
	IsPromoted bool     `json:"isPromoted"`
}

type ReviewVerdict struct {
	Summary        string `json:"summary"`
	Recommendation string `json:"recommendation"`
}

type ReviewFAQ struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type StructuredReviewContent struct {
	Title           string          `json:"title"`
	Subtitle        string          `json:"subtitle"`
	MetaDescription string          `json:"meta_description"`
	HeroImageURL    string          `json:"hero_image_url"`
	Category        string          `json:"category"`
	OverallRating   float64         `json:"overall_rating"`
	QuickVerdict    string          `json:"quick_verdict"`
	Sections        []ReviewSection `json:"sections"`
	Features        []FeatureRating `json:"features"`
	Pros            []string        `json:"pros"`
	Cons            []string        `json:"cons"`
	Products        []ReviewProduct `json:"products"`
	Verdict         ReviewVerdict   `json:"verdict"`
	FAQ             []ReviewFAQ     `json:"faq"`
	AuthorBio       string          `json:"author_bio"`
}

// ---------------------------------------------------------------------------
// HandleGenerateLandingPage — POST /offer-center/offers/{id}/landing-page/generate
// ---------------------------------------------------------------------------

func (h *LandingPageHandlers) HandleGenerateLandingPage(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}

	ctx := r.Context()

	var name, description, webProperty, htmlCreative, trackingLink string
	err := h.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(description,''), COALESCE(web_property,''),
			COALESCE(original_html_creative,''), COALESCE(tracking_link_template,'')
		FROM mailing_offers WHERE id=$1
	`, offerID).Scan(&name, &description, &webProperty, &htmlCreative, &trackingLink)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[LandingPage] db error loading offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "failed to load offer")
		return
	}

	if webProperty == "" {
		respondError(w, http.StatusBadRequest, "offer must have a web property set")
		return
	}

	kit, ok := GetBrandKit(webProperty)
	if !ok {
		respondError(w, http.StatusBadRequest, "unknown web property: "+webProperty)
		return
	}

	// If offer has no original_html_creative, fall back to the first imported creative
	if htmlCreative == "" {
		h.db.QueryRowContext(ctx,
			`SELECT html_content FROM mailing_offer_creatives
			 WHERE offer_id=$1 AND html_content != '' ORDER BY created_at ASC LIMIT 1`,
			offerID).Scan(&htmlCreative)
	}

	slug := generateSlug(name)
	productImageURL := extractProductImage(htmlCreative)

	landerInfo := scrapeLanderPage(trackingLink)

	review, err := generateStructuredReview(name, description, htmlCreative, trackingLink, productImageURL, kit, landerInfo)
	if err != nil {
		log.Printf("[LandingPage] AI generation error for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to generate landing page: "+err.Error())
		return
	}

	liveURL, err := postStructuredReview(kit, slug, review)
	if err != nil {
		log.Printf("[LandingPage] POST to Next.js failed for offer %s: %v", offerID, err)
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI content generated but failed to publish to %s: %v", kit.SiteDomain, err))
		return
	}
	if liveURL == "" {
		liveURL = fmt.Sprintf("https://%s/reviews/%s", kit.SiteDomain, slug)
	}

	reviewJSON, _ := json.Marshal(review)
	_, err = h.db.ExecContext(ctx, `
		UPDATE mailing_offers
		SET landing_page_slug=$1, landing_page_url=$2, landing_page_html=$3, updated_at=NOW()
		WHERE id=$4
	`, slug, liveURL, string(reviewJSON), offerID)
	if err != nil {
		log.Printf("[LandingPage] db error updating offer %s: %v", offerID, err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"slug":         slug,
		"url":          liveURL,
		"title":        review.Title,
		"web_property": webProperty,
	})
}

// ---------------------------------------------------------------------------
// HandleRepublishLandingPage — POST /offer-center/offers/{id}/landing-page/republish
// ---------------------------------------------------------------------------

func (h *LandingPageHandlers) HandleRepublishLandingPage(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}

	ctx := r.Context()

	var name, webProperty, slug, storedJSON string
	err := h.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(web_property,''), COALESCE(landing_page_slug,''), COALESCE(landing_page_html,'')
		FROM mailing_offers WHERE id=$1
	`, offerID).Scan(&name, &webProperty, &slug, &storedJSON)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[LandingPage] db error loading offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "failed to load offer")
		return
	}

	if storedJSON == "" {
		respondError(w, http.StatusBadRequest, "no landing page content stored — generate one first")
		return
	}
	if webProperty == "" {
		respondError(w, http.StatusBadRequest, "offer must have a web property set")
		return
	}
	if slug == "" {
		slug = generateSlug(name)
	}

	kit, ok := GetBrandKit(webProperty)
	if !ok {
		respondError(w, http.StatusBadRequest, "unknown web property: "+webProperty)
		return
	}

	// Try to parse as structured review first; fall back to legacy HTML
	var review StructuredReviewContent
	if err := json.Unmarshal([]byte(storedJSON), &review); err != nil || review.Title == "" {
		review = StructuredReviewContent{
			Title:           name,
			MetaDescription: fmt.Sprintf("Review of %s", name),
			Category:        "Reviews",
		}
	}

	liveURL, err := postStructuredReview(kit, slug, &review)
	if err != nil {
		log.Printf("[LandingPage] republish POST to Next.js failed for offer %s: %v", offerID, err)
		respondError(w, http.StatusBadGateway, fmt.Sprintf("Failed to publish to %s: %v", kit.SiteDomain, err))
		return
	}
	if liveURL == "" {
		liveURL = fmt.Sprintf("https://%s/reviews/%s", kit.SiteDomain, slug)
	}

	_, err = h.db.ExecContext(ctx, `
		UPDATE mailing_offers SET landing_page_url=$1, updated_at=NOW() WHERE id=$2
	`, liveURL, offerID)
	if err != nil {
		log.Printf("[LandingPage] db error updating offer %s: %v", offerID, err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"slug":         slug,
		"url":          liveURL,
		"web_property": webProperty,
		"republished":  true,
	})
}

// ---------------------------------------------------------------------------
// AI Structured Review Generation
// ---------------------------------------------------------------------------

func generateStructuredReview(offerName, description, htmlCreative, trackingLink, productImageURL string, kit BrandKit, landerInfo *LanderPageInfo) (*StructuredReviewContent, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set — add it to GitHub Secrets and redeploy")
	}

	prompt := buildReviewPrompt(offerName, description, htmlCreative, trackingLink, productImageURL, kit, landerInfo)

	systemPrompt := fmt.Sprintf(`You are the lead editorial writer for %s (%s — "%s").

Your writing voice: %s

You produce premium, SEO-optimized listicle articles that rival Wirecutter, CNET, and Tom's Guide. Each article is a ranked list of products/services with rich detail, honest analysis, and compelling calls to action. You write from first-person editorial authority — you have tested these products and stand behind your rankings.

You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation — output ONLY the raw JSON object.`, kit.SiteName, kit.SiteDomain, kit.Tagline, kit.Voice)

	reqBody := map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 8000,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Anthropic request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		keySuffix := apiKey
		if len(keySuffix) > 4 {
			keySuffix = keySuffix[len(keySuffix)-4:]
		}
		return nil, fmt.Errorf("Anthropic returned %d (key …%s): %s", resp.StatusCode, keySuffix, string(body))
	}

	var aiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return nil, fmt.Errorf("failed to decode Anthropic response: %v", err)
	}

	if len(aiResp.Content) == 0 {
		return nil, fmt.Errorf("no AI response received")
	}

	content := strings.TrimSpace(aiResp.Content[0].Text)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var result StructuredReviewContent
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("failed to parse AI response: %v\nraw content: %.800s", err, content)
	}

	return &result, nil
}

func buildReviewPrompt(offerName, description, htmlCreative, trackingLink, productImageURL string, kit BrandKit, landerInfo *LanderPageInfo) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, `PUBLICATION: %s (%s)
TAGLINE: "%s"
BRAND COLORS: primary %s, secondary %s, accent %s
BRAND FONTS: headings in %s, body in %s

PROMOTED PRODUCT: %s
`, kit.SiteName, kit.SiteDomain, kit.Tagline, kit.PrimaryColor, kit.SecondaryColor, kit.AccentColor, kit.HeadingFont, kit.BodyFont, offerName)

	if description != "" {
		fmt.Fprintf(&sb, "PRODUCT DESCRIPTION: %s\n", description)
	}

	if htmlCreative != "" {
		plainText := stripHTMLTags(htmlCreative)
		if len(plainText) > 1500 {
			plainText = plainText[:1500]
		}
		fmt.Fprintf(&sb, "\nPRODUCT CREATIVE COPY (extracted from email creative):\n%s\n", plainText)
	}

	if productImageURL != "" {
		fmt.Fprintf(&sb, "\nPRODUCT IMAGE URL: %s\n", productImageURL)
	}

	if trackingLink != "" {
		fmt.Fprintf(&sb, "\nPROMOTED CTA URL: %s\n", trackingLink)
	}

	if landerInfo != nil && landerInfo.Found {
		fmt.Fprintf(&sb, "\n--- LANDER PAGE INTELLIGENCE (scraped from the conversion URL) ---\n")
		if landerInfo.FinalURL != "" {
			fmt.Fprintf(&sb, "LANDER URL: %s\n", landerInfo.FinalURL)
		}
		if landerInfo.Title != "" {
			fmt.Fprintf(&sb, "LANDER TITLE: %s\n", landerInfo.Title)
		}
		if landerInfo.MetaDescription != "" {
			fmt.Fprintf(&sb, "LANDER META DESCRIPTION: %s\n", landerInfo.MetaDescription)
		}
		if len(landerInfo.LogoURLs) > 0 {
			fmt.Fprintf(&sb, "BRAND LOGOS FOUND: %s\n", strings.Join(landerInfo.LogoURLs, ", "))
		}
		if landerInfo.CompanyName != "" {
			fmt.Fprintf(&sb, "COMPANY NAME: %s\n", landerInfo.CompanyName)
		}
		if len(landerInfo.Headlines) > 0 {
			fmt.Fprintf(&sb, "KEY HEADLINES: %s\n", strings.Join(landerInfo.Headlines, " | "))
		}
		if landerInfo.BodySnippet != "" {
			fmt.Fprintf(&sb, "LANDER BODY EXCERPT:\n%s\n", landerInfo.BodySnippet)
		}
		fmt.Fprintf(&sb, "--- END LANDER INTELLIGENCE ---\n")
		fmt.Fprintf(&sb, "\nIMPORTANT: Use the lander page intelligence above to write an accurate, detailed review. Reference actual brand names, features, pricing, and value propositions from the lander. If logo URLs were found, use them for product #1's imageUrl.\n")
	}

	fmt.Fprintf(&sb, `
TASK: Write a rich, ranked listicle for %s. Format: "Top 5 Best [Category] in 2026" style.
The promoted product is #1. The remaining 4 are REAL competing products/services in the same space.

This must read like a premium editorial — authoritative, detailed, and persuasive. Not a thin affiliate list.

LISTICLE REQUIREMENTS:

1. TITLE: "Top 5 Best [Specific Category] in 2026" or "[Number] Best [Category] — [Year] Expert Picks". Must include the year. Hook the reader.

2. SUBTITLE: One line that promises value. Example: "We tested 12 services so you don't have to."

3. QUICK VERDICT: 2-3 sentences. Name the #1 pick and say why. Lead with the conclusion. Be specific.

4. SECTIONS (5-6 editorial sections, each 150-250 words of HTML):
   - Section 1: Introduce the category — why this matters to the reader right now
   - Section 2: How we evaluated — criteria, methodology, what we looked for
   - Section 3: Deep dive on the #1 pick — usage experience, standout features, real value
   - Section 4: The runner-up — honest comparison, where it wins and loses vs #1
   - Section 5: Budget / alternative pick — for readers with different priorities
   - Section 6: Final thoughts — who should buy what
   HTML content should use <p>, <strong>, <em>, <ul>/<li> tags. Write with confidence and specificity.

5. FEATURE RATINGS: 6-8 specific, measurable features. Rate each 1.0-5.0 with a one-sentence verdict.

6. PROS (6-7 specific): Substantive, not generic. Reference real features, numbers, or experiences.
   CONS (3-4 honest): Builds reader trust. Mention genuine trade-offs.

7. PRODUCTS (exactly 5, ranked #1 through #5):
   The promoted product is ALWAYS rank #1 with isPromoted=true.
   Ranks 2-5 are REAL competing products/services that actually exist.

   For EVERY product:
   - name: Full product/service name
   - rank: 1-5
   - rating: #1 gets 4.7-4.9, others distributed 3.6-4.5
   - imageUrl: Use the product image URL for #1, empty string for others
   - badge: #1="Editor's Choice", #2="Runner-Up", #3="Best Value", #4/#5 contextual
   - quickTake: 3-4 sentences of honest editorial opinion
   - keyFeature: One standout feature in a short phrase
   - pros: 4-5 specific pros
   - cons: 3-4 specific cons
   - ctaUrl: Use the promoted CTA URL for #1, "#" for others
   - ctaText: Action-oriented, e.g. "Get Best Price →", "Try Free →", "See Latest Deal →"
   - isPromoted: true only for #1

8. VERDICT: 3-4 sentences summarizing the list + a clear one-sentence recommendation.

9. FAQ: 6 realistic consumer questions with 2-3 sentence expert answers. Include questions about price, alternatives, "is it worth it", compatibility, returns/guarantees.

10. AUTHOR BIO: 1-2 sentences written in the %s brand voice. Reference the publication by name.

QUALITY RULES:
- Write with editorial authority. You tested these. You have opinions.
- Specific numbers and feature names > vague praise.
- NO phrases: "in today's market", "in conclusion", "it's worth noting", "whether you're a..."
- Competitors must be REAL products/services a consumer could actually buy.
- The listicle should make the reader want to click the #1 pick's CTA. Persuasive but not pushy.
- Reference %s and its editorial standards throughout.

`, kit.SiteName, kit.SiteName, kit.SiteName)

	fmt.Fprintf(&sb, `RETURN THIS EXACT JSON STRUCTURE:
{
  "title": "string",
  "subtitle": "string",
  "meta_description": "string (max 155 chars)",
  "hero_image_url": "",
  "category": "string (e.g. Telecom, Finance, Health, Shopping)",
  "overall_rating": 4.8,
  "quick_verdict": "string (2-3 sentences)",
  "sections": [
    {
      "heading": "string",
      "content": "string (HTML paragraphs, 150+ words each)"
    }
  ],
  "features": [
    { "name": "string", "rating": 4.5, "description": "string" }
  ],
  "pros": ["string", "string"],
  "cons": ["string", "string"],
  "products": [
    {
      "name": "string",
      "rank": 1,
      "rating": 4.8,
      "imageUrl": "%s",
      "badge": "Editor's Choice",
      "quickTake": "string (3-4 sentences)",
      "keyFeature": "string",
      "pros": ["string"],
      "cons": ["string"],
      "ctaUrl": "%s",
      "ctaText": "Get Best Price →",
      "isPromoted": true
    }
  ],
  "verdict": {
    "summary": "string (3-4 sentences)",
    "recommendation": "string (clear recommendation)"
  },
  "faq": [
    { "question": "string", "answer": "string" }
  ],
  "author_bio": "string"
}`, productImageURL, trackingLink)

	return sb.String()
}

// ---------------------------------------------------------------------------
// POST structured review to Next.js
// ---------------------------------------------------------------------------

func postStructuredReview(kit BrandKit, slug string, review *StructuredReviewContent) (string, error) {
	if kit.SiteAPIURL == "" {
		return "", fmt.Errorf("no API URL configured for %s", kit.Key)
	}

	payload := map[string]interface{}{
		"slug":             slug,
		"title":            review.Title,
		"subtitle":         review.Subtitle,
		"meta_description": review.MetaDescription,
		"hero_image_url":   review.HeroImageURL,
		"category":         review.Category,
		"overall_rating":   review.OverallRating,
		"quick_verdict":    review.QuickVerdict,
		"sections":         review.Sections,
		"features":         review.Features,
		"pros":             review.Pros,
		"cons":             review.Cons,
		"products":         review.Products,
		"verdict":          review.Verdict,
		"faq":              review.FAQ,
		"author_name":      getAuthorName(kit.Key),
		"author_bio":       review.AuthorBio,
		"status":           "published",
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %v", err)
	}

	req, err := http.NewRequest("POST", kit.SiteAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if kit.SiteAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+kit.SiteAPIKey)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %v", kit.SiteAPIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Next.js API returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		URL string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.URL, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractProductImage(htmlCreative string) string {
	if htmlCreative == "" {
		return ""
	}
	re := regexp.MustCompile(`<img[^>]+src=["']([^"']+)["']`)
	matches := re.FindAllStringSubmatch(htmlCreative, -1)
	for _, match := range matches {
		src := match[1]
		if strings.Contains(src, "tracking") || strings.Contains(src, "pixel") || strings.Contains(src, "1x1") {
			continue
		}
		return src
	}
	return ""
}

func stripHTMLTags(html string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(html, " ")
	text = strings.Join(strings.Fields(text), " ")
	return text
}

func getAuthorName(key string) string {
	switch key {
	case "discountblog":
		return "Jamie"
	case "quizfiesta":
		return "Alex"
	case "historythinking":
		return "Professor Morgan"
	case "myownhealth":
		return "Dr. Sarah"
	default:
		return "Staff Writer"
	}
}

// ---------------------------------------------------------------------------
// Lander Page Scraper — follows the conversion URL and extracts branding intel
// ---------------------------------------------------------------------------

type LanderPageInfo struct {
	Found           bool
	FinalURL        string
	Title           string
	MetaDescription string
	LogoURLs        []string
	CompanyName     string
	Headlines       []string
	BodySnippet     string
}

func scrapeLanderPage(trackingLink string) *LanderPageInfo {
	if trackingLink == "" {
		return nil
	}

	cleanURL := trackingLink
	for _, tag := range []string{"{{DATE_MMDDYYYY}}", "{{MAILING_ID}}", "{{SUBSCRIBER_ID}}", "{{CAMPAIGN_ID}}", "{{CREATIVE_ID}}"} {
		cleanURL = strings.ReplaceAll(cleanURL, tag, "test")
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequest("GET", cleanURL, nil)
	if err != nil {
		log.Printf("[LanderScrape] failed to create request for %s: %v", cleanURL, err)
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[LanderScrape] request failed for %s: %v", cleanURL, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[LanderScrape] non-200 response (%d) from %s", resp.StatusCode, cleanURL)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		log.Printf("[LanderScrape] error reading body from %s: %v", cleanURL, err)
		return nil
	}

	html := string(body)
	info := &LanderPageInfo{
		Found:    true,
		FinalURL: resp.Request.URL.String(),
	}

	if m := regexp.MustCompile(`<title[^>]*>([^<]+)</title>`).FindStringSubmatch(html); len(m) > 1 {
		info.Title = strings.TrimSpace(m[1])
	}

	if m := regexp.MustCompile(`<meta[^>]+name=["']description["'][^>]+content=["']([^"']+)["']`).FindStringSubmatch(html); len(m) > 1 {
		info.MetaDescription = strings.TrimSpace(m[1])
	}
	if info.MetaDescription == "" {
		if m := regexp.MustCompile(`<meta[^>]+content=["']([^"']+)["'][^>]+name=["']description["']`).FindStringSubmatch(html); len(m) > 1 {
			info.MetaDescription = strings.TrimSpace(m[1])
		}
	}

	logoRe := regexp.MustCompile(`<img[^>]+src=["']([^"']+)["'][^>]*>`)
	for _, m := range logoRe.FindAllStringSubmatch(html, -1) {
		src := m[1]
		tag := strings.ToLower(m[0])
		if strings.Contains(tag, "logo") || strings.Contains(src, "logo") ||
			strings.Contains(tag, "brand") || strings.Contains(src, "brand") {
			if !strings.HasPrefix(src, "http") {
				if strings.HasPrefix(src, "//") {
					src = "https:" + src
				} else if strings.HasPrefix(src, "/") {
					src = resp.Request.URL.Scheme + "://" + resp.Request.URL.Host + src
				}
			}
			info.LogoURLs = append(info.LogoURLs, src)
		}
	}

	ogSiteName := regexp.MustCompile(`<meta[^>]+property=["']og:site_name["'][^>]+content=["']([^"']+)["']`)
	if m := ogSiteName.FindStringSubmatch(html); len(m) > 1 {
		info.CompanyName = strings.TrimSpace(m[1])
	}

	headlineRe := regexp.MustCompile(`<h[1-3][^>]*>([^<]+)</h[1-3]>`)
	for _, m := range headlineRe.FindAllStringSubmatch(html, 5) {
		h := strings.TrimSpace(m[1])
		if h != "" && len(h) > 5 {
			info.Headlines = append(info.Headlines, h)
		}
	}

	bodyText := stripHTMLTags(html)
	if len(bodyText) > 1500 {
		bodyText = bodyText[:1500]
	}
	info.BodySnippet = bodyText

	log.Printf("[LanderScrape] scraped %s: title=%q, company=%q, logos=%d, headlines=%d",
		info.FinalURL, info.Title, info.CompanyName, len(info.LogoURLs), len(info.Headlines))

	return info
}

func generateSlug(name string) string {
	slug := strings.ToLower(name)
	slug = strings.ReplaceAll(slug, " ", "-")

	var result strings.Builder
	for _, ch := range slug {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			result.WriteRune(ch)
		}
	}
	slug = result.String()

	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")

	if len(slug) > 100 {
		slug = slug[:100]
	}
	return slug + "-review"
}

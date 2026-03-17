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

	slug := generateSlug(name)
	productImageURL := extractProductImage(htmlCreative)

	review, err := generateStructuredReview(name, description, htmlCreative, trackingLink, productImageURL, kit)
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

func generateStructuredReview(offerName, description, htmlCreative, trackingLink, productImageURL string, kit BrandKit) (*StructuredReviewContent, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	prompt := buildReviewPrompt(offerName, description, htmlCreative, trackingLink, productImageURL, kit)

	reqBody := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{"role": "system", "content": `You are an expert product reviewer and comparison writer. You write thorough, persuasive, evidence-based reviews that help real consumers make purchasing decisions. Your reviews read like professional publications (Wirecutter, CNET, Tom's Guide).

You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation, just the JSON object.`},
			{"role": "user", "content": prompt},
		},
		"temperature":    0.7,
		"max_tokens":     8000,
		"response_format": map[string]string{"type": "json_object"},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenAI returned %d: %s", resp.StatusCode, string(body))
	}

	var aiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return nil, fmt.Errorf("failed to decode OpenAI response: %v", err)
	}

	if len(aiResp.Choices) == 0 {
		return nil, fmt.Errorf("no AI response received")
	}

	content := strings.TrimSpace(aiResp.Choices[0].Message.Content)
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

func buildReviewPrompt(offerName, description, htmlCreative, trackingLink, productImageURL string, kit BrandKit) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, `You are writing a comprehensive product review article for %s (%s — "%s").

BRAND VOICE: %s

PRODUCT TO REVIEW: %s
`, kit.SiteName, kit.SiteDomain, kit.Tagline, kit.Voice, offerName)

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
		fmt.Fprintf(&sb, "\nPRODUCT IMAGE URL (use this for the promoted product): %s\n", productImageURL)
	}

	if trackingLink != "" {
		fmt.Fprintf(&sb, "\nTRACKING LINK (use as the CTA URL for the promoted product): %s\n", trackingLink)
	}

	sb.WriteString(`
TASK: Generate a comprehensive, persuasive product comparison review. The article must read like a professional publication — the kind that would convince a skeptical consumer to buy. NOT a generic listicle.

CONTENT REQUIREMENTS:
1. TITLE: Engaging, SEO-optimized headline with current year. Not generic — should hook the reader.
2. SUBTITLE: A compelling one-liner that promises value to the reader.
3. QUICK VERDICT: 2-3 sentences summarizing your recommendation. Lead with the conclusion.
4. IN-DEPTH SECTIONS (4-6 sections): Each should be a substantial paragraph (150+ words). Cover:
   - What the product actually does and who it's for
   - Real-world usage scenarios and value proposition
   - How it compares to alternatives in specific ways (price, features, reliability)
   - Any notable experience details, pricing tiers, or special deals
   - Why it stands out (or doesn't)
   Content should use <p> tags for paragraphs and can include <strong>, <em>, <ul>/<li> for emphasis.
5. FEATURE RATINGS: 5-7 specific features rated 1-5 with one-sentence explanations.
6. PROS/CONS: 5-6 specific, substantive pros. 3-4 honest cons (builds credibility).
7. COMPARISON PRODUCTS: The promoted product ranked #1 plus 2-3 real competing alternatives.
   - Each product needs: specific pros (4-5), specific cons (3-4), a detailed quickTake (2-3 sentences), a keyFeature highlight
   - Promoted product: rating 4.7-4.9, badge "Best Overall"
   - Competitors: real products/services in the same category, ratings 3.8-4.4
   - Competitor CTA URLs should be "#"
8. VERDICT: A compelling final summary + clear recommendation statement.
9. FAQ: 5-6 realistic questions a consumer would ask, with helpful 2-3 sentence answers.
10. AUTHOR BIO: A brief (1-2 sentence) bio for the author in the brand voice.

QUALITY STANDARDS:
- Write as if you've personally tested the product. Use confident, specific language.
- NO generic filler like "in today's market" or "in conclusion". Get to the point.
- Include specific numbers, features, and comparisons where possible.
- The review should make someone want to click the CTA. It's persuasive but not sleazy.
- Competitors should be REAL products/services that exist in this category.

`)

	fmt.Fprintf(&sb, `RETURN THIS EXACT JSON STRUCTURE:
{
  "title": "string",
  "subtitle": "string",
  "meta_description": "string (max 155 chars)",
  "hero_image_url": "%s",
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
      "badge": "Best Overall",
      "quickTake": "string (2-3 sentences)",
      "keyFeature": "string",
      "pros": ["string"],
      "cons": ["string"],
      "ctaUrl": "%s",
      "ctaText": "Get Best Price →",
      "isPromoted": true
    }
  ],
  "verdict": {
    "summary": "string (2-3 sentences)",
    "recommendation": "string (clear recommendation)"
  },
  "faq": [
    { "question": "string", "answer": "string" }
  ],
  "author_bio": "string"
}`, productImageURL, productImageURL, trackingLink)

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

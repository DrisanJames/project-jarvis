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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// LandingPageHandlers manages AI-generated landing pages for offers.
type LandingPageHandlers struct {
	db *sql.DB
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// AILandingPageResult is the structured response from OpenAI for a listicle.
type AILandingPageResult struct {
	Title              string              `json:"title"`
	MetaDescription    string              `json:"meta_description"`
	HTMLContent        string              `json:"html_content"`
	ComparisonProducts []ComparisonProduct `json:"comparison_products"`
	Category           string              `json:"category"`
}

// ComparisonProduct represents one product in the comparison listicle.
type ComparisonProduct struct {
	Name       string  `json:"name"`
	Rank       int     `json:"rank"`
	Rating     float64 `json:"rating"`
	CTAUrl     string  `json:"cta_url"`
	IsPromoted bool    `json:"is_promoted"`
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

	aiResult, err := generateLandingPageContent(name, description, htmlCreative, trackingLink, kit)
	if err != nil {
		log.Printf("[LandingPage] AI generation error for offer %s: %v", offerID, err)
		respondError(w, http.StatusInternalServerError, "Failed to generate landing page: "+err.Error())
		return
	}

	liveURL, err := postToNextJS(kit, slug, aiResult)
	if err != nil {
		log.Printf("[LandingPage] POST to Next.js failed for offer %s: %v", offerID, err)
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI content generated but failed to publish to %s: %v", kit.SiteDomain, err))
		return
	}
	if liveURL == "" {
		liveURL = fmt.Sprintf("https://%s/reviews/%s", kit.SiteDomain, slug)
	}

	_, err = h.db.ExecContext(ctx, `
		UPDATE mailing_offers
		SET landing_page_slug=$1, landing_page_url=$2, landing_page_html=$3, updated_at=NOW()
		WHERE id=$4
	`, slug, liveURL, aiResult.HTMLContent, offerID)
	if err != nil {
		log.Printf("[LandingPage] db error updating offer %s: %v", offerID, err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"slug":         slug,
		"url":          liveURL,
		"title":        aiResult.Title,
		"web_property": webProperty,
	})
}

// ---------------------------------------------------------------------------
// AI content generation (OpenAI)
// ---------------------------------------------------------------------------

func generateLandingPageContent(offerName, description, htmlCreative, trackingLink string, kit BrandKit) (*AILandingPageResult, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	prompt := fmt.Sprintf(`You are a content writer for %s (%s). %s

Generate a product review/comparison listicle article. The article should compare the promoted product to 2-3 real alternatives in the same category.

Promoted Product: %s
Description: %s

Requirements:
1. Title: SEO-friendly, includes the year (2026)
2. Article body: HTML formatted listicle. The promoted product must be ranked #1 with "Best Overall" badge, 4.8-5.0 rating, and the strongest persuasive copy.
3. Include 2-3 competing products ranked #2, #3 etc. with slightly lower ratings (4.2-4.6).
4. Each product section should have: name, rating (stars), pros/cons list, brief review (2-3 sentences), and a CTA button.
5. The CTA button for the promoted product links to: %s (this will have tracking parameters appended)
6. CTA buttons for competitors should link to "#" with text "Check Price"
7. Write in the brand voice: %s
8. HTML should use clean semantic markup, no inline styles except for rating stars and badges.
9. Add data-everflow="true" attribute to the promoted product's CTA link.

Return a JSON object with these fields:
- title: string (article title)
- meta_description: string (155 chars max)
- html_content: string (the article body HTML — not a full page, just the article content)
- comparison_products: array of { name, rank, rating, cta_url, is_promoted }
- category: string (what category this belongs to, e.g. "Finance", "Health", "Shopping")

IMPORTANT: Return ONLY the JSON object, no markdown code fences.`,
		kit.SiteName, kit.Tagline, kit.Voice,
		offerName, description, trackingLink, kit.Voice)

	reqBody := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a professional content writer. Output valid JSON only."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.7,
		"max_tokens":  4000,
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

	client := &http.Client{Timeout: 120 * time.Second}
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

	var result AILandingPageResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("failed to parse AI response: %v\nraw content: %.500s", err, content)
	}

	return &result, nil
}

// ---------------------------------------------------------------------------
// POST to Next.js site API
// ---------------------------------------------------------------------------

func postToNextJS(kit BrandKit, slug string, content *AILandingPageResult) (string, error) {
	if kit.SiteAPIURL == "" {
		return "", fmt.Errorf("no API URL configured for %s", kit.Key)
	}

	payload := map[string]interface{}{
		"slug":                slug,
		"title":               content.Title,
		"meta_description":    content.MetaDescription,
		"html_content":        content.HTMLContent,
		"comparison_products": content.ComparisonProducts,
		"category":            content.Category,
		"author_name":         getAuthorName(kit.Key),
		"status":              "published",
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

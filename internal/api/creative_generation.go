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
	"github.com/google/uuid"
)

type GeneratedCreative struct {
	VariantName string `json:"variant_name"`
	Angle       string `json:"angle"`
	SubjectLine string `json:"subject_line"`
	PreHeader   string `json:"pre_header"`
	HTML        string `json:"html"`
}

type CreativeGenerationResult struct {
	Creatives []GeneratedCreative `json:"creatives"`
}

// HandleGenerateCreatives — POST /offer-center/offers/{id}/creatives/generate
// Analyzes the offer's original image creative, researches the product, and
// generates ~10 text-heavy HTML email creatives with proper branding and
// personalization tags.
func (och *OfferCenterHandlers) HandleGenerateCreatives(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer id required")
		return
	}

	ctx := r.Context()

	var name, description, webProperty, htmlCreative, trackingLink string
	err := och.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(description,''), COALESCE(web_property,''),
			COALESCE(original_html_creative,''), COALESCE(tracking_link_template,'')
		FROM mailing_offers WHERE id=$1
	`, offerID).Scan(&name, &description, &webProperty, &htmlCreative, &trackingLink)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		log.Printf("[CreativeGen] db error loading offer %s: %v", offerID, err)
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

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		respondError(w, http.StatusInternalServerError, "ANTHROPIC_API_KEY not set — add to GitHub Secrets and redeploy")
		return
	}

	creativeCopy := extractCreativeCopy(htmlCreative)
	productImage := extractProductImage(htmlCreative)

	prompt := buildCreativeGenerationPrompt(name, description, creativeCopy, trackingLink, productImage, kit)

	systemPrompt := fmt.Sprintf(`You are the head email marketing creative director for %s (%s — "%s").

Brand voice: %s

You design high-converting, text-heavy HTML email creatives that feel personal, look clean, and drive action. Every email you write gets opened, read, and clicked — because it feels like a message from a trusted friend, not a corporate blast.

Your emails are mobile-first, dark-mode compatible, and render perfectly in Gmail, Outlook, Apple Mail, and Yahoo Mail. You use inline CSS only.

You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation — output ONLY the raw JSON object.`,
		kit.SiteName, kit.SiteDomain, kit.Tagline, kit.Voice)

	reqBody := map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 64000,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to marshal AI request")
		return
	}

	log.Printf("[CreativeGen] generating creatives for offer %s (%s on %s)", offerID, name, kit.SiteName)

	req2, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create AI request")
		return
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", apiKey)
	req2.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(req2)
	if err != nil {
		log.Printf("[CreativeGen] Anthropic request failed: %v", err)
		respondError(w, http.StatusBadGateway, "AI generation request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[CreativeGen] Anthropic returned %d: %s", resp.StatusCode, string(body))
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI service returned %d", resp.StatusCode))
		return
	}

	var aiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to decode AI response")
		return
	}
	if len(aiResp.Content) == 0 {
		respondError(w, http.StatusInternalServerError, "empty AI response")
		return
	}

	content := strings.TrimSpace(aiResp.Content[0].Text)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	// If the response was truncated, try to salvage partial JSON by closing the array
	if !strings.HasSuffix(content, "}") {
		if idx := strings.LastIndex(content, "}"); idx > 0 {
			content = content[:idx+1]
			if !strings.HasSuffix(strings.TrimSpace(content), "]}") {
				content = strings.TrimSpace(content) + "]}"
			}
		}
	}

	var result CreativeGenerationResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		log.Printf("[CreativeGen] parse error: %v\nraw start: %.500s\nraw end: %.500s", err, content[:min(500, len(content))], content[max(0, len(content)-500):])
		respondError(w, http.StatusInternalServerError, "failed to parse AI response — retry may help")
		return
	}

	if len(result.Creatives) == 0 {
		respondError(w, http.StatusInternalServerError, "AI returned no creatives")
		return
	}

	var inserted []map[string]interface{}
	for i, c := range result.Creatives {
		id := uuid.New().String()
		now := time.Now()

		var version int
		err := och.db.QueryRowContext(ctx,
			`INSERT INTO mailing_offer_creatives
			 (id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
			 VALUES ($1, $2, COALESCE((SELECT MAX(version) FROM mailing_offer_creatives WHERE offer_id = $2), 0) + 1,
			         $3, 'generated', $4, $5, $6)
			 RETURNING version`,
			id, offerID, c.HTML, fmt.Sprintf("[%s] %s", c.VariantName, c.Angle), now, now).Scan(&version)
		if err != nil {
			log.Printf("[CreativeGen] error inserting creative %d: %v", i+1, err)
			continue
		}

		inserted = append(inserted, map[string]interface{}{
			"id":           id,
			"version":      version,
			"variant_name": c.VariantName,
			"angle":        c.Angle,
			"subject_line": c.SubjectLine,
			"pre_header":   c.PreHeader,
			"status":       "generated",
		})
	}

	log.Printf("[CreativeGen] generated %d creatives for offer %s", len(inserted), offerID)

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"generated": len(inserted),
		"creatives": inserted,
	})
}

func extractCreativeCopy(htmlCreative string) string {
	if htmlCreative == "" {
		return ""
	}
	re := regexp.MustCompile(`<[^>]*>`)
	text := re.ReplaceAllString(htmlCreative, " ")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 2000 {
		text = text[:2000]
	}
	return text
}

func buildCreativeGenerationPrompt(offerName, description, creativeCopy, trackingLink, productImage string, kit BrandKit) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, `BRAND: %s (%s)
TAGLINE: "%s"
SENDING DOMAIN: %s
COLORS: primary %s · secondary %s · accent %s
FONTS: headings %s · body %s
LOGO: %s

OFFER: %s
`, kit.SiteName, kit.SiteDomain, kit.Tagline, kit.SendingDomain,
		kit.PrimaryColor, kit.SecondaryColor, kit.AccentColor,
		kit.HeadingFont, kit.BodyFont, kit.LogoHTML, offerName)

	if description != "" {
		fmt.Fprintf(&sb, "DESCRIPTION: %s\n", description)
	}

	if creativeCopy != "" {
		fmt.Fprintf(&sb, "\nORIGINAL CREATIVE COPY (extracted from the image-heavy email — analyze the product language, claims, value props, and tone):\n%s\n", creativeCopy)
	}

	if productImage != "" {
		fmt.Fprintf(&sb, "\nPRODUCT IMAGE: %s\n", productImage)
	}

	if trackingLink != "" {
		fmt.Fprintf(&sb, "\nCTA TRACKING URL: %s\n", trackingLink)
	}

	fmt.Fprintf(&sb, `
TASK: Generate exactly 10 text-heavy HTML email creatives for the offer above.

Each creative must be a complete, production-ready HTML email that:
- Is primarily TEXT-BASED (not image-heavy) — text does the selling
- May include the product image once, but the layout is text-driven
- Uses the %s brand identity (colors, fonts, voice, logo)
- Feels personal — like a recommendation from a friend
- Includes personalization merge tags where natural
- Has a strong, clear CTA button linking to the tracking URL
- Is mobile-responsive (600px max-width, scales down)
- Uses only inline CSS (no <style> blocks)
- Includes an unsubscribe footer
- Is 100%% email-client compatible (Gmail, Outlook, Apple Mail, Yahoo)

PERSONALIZATION TAGS (use naturally — don't force all into every email):
- {%% if first_name %%}Hi {{ first_name }},{%% else %%}Hey there,{%% endif %%}
- {{ first_name }} — subscriber's first name
- {{ email }} — subscriber's email
- {{ system.current_year }} — current year
- {{ system.unsubscribe_url }} — unsubscribe link (REQUIRED in footer)

CREATIVE ANGLES — each variant should use a DIFFERENT persuasion strategy:
1. "The Personal Pick" — warm, conversational recommendation from %s staff
2. "Urgency/FOMO" — limited-time deal, act now
3. "Social Proof" — reviews, ratings, what others are saying
4. "Problem → Solution" — pain point first, then the product as the fix
5. "Comparison Winner" — why this beats the alternatives
6. "Money Saver" — focus on savings, value, ROI
7. "Quick Hit" — ultra-short, punchy, 3-sentence email with bold CTA
8. "Story" — mini narrative about someone who benefited
9. "FAQ Style" — addresses objections head-on in Q&A format
10. "VIP/Exclusive" — makes the reader feel like they're getting insider access

HTML TEMPLATE REQUIREMENTS:
- Outer table: width 100%%, background #f4f4f4 (light) or #1a1a2e (dark depending on brand)
- Inner table: max-width 600px, centered
- Header: brand logo using the LogoHTML provided, styled with brand heading font
- Body: text-driven content with 16-18px body font, 1.6 line-height, brand body font
- CTA button: prominent, brand primary color background, white text, border-radius 8px, padding 14px 32px, full-width on mobile
- Footer: small text with sender info, unsubscribe link using {{ system.unsubscribe_url }}, physical address
- All images must have alt text
- Use <!--[if mso]> conditionals for Outlook button rendering

For the CTA URL, use: %s
Replace {mailing_id}, {subscriber_id}, {campaign_id} with the literal placeholder text — they're replaced at send time.

RETURN THIS JSON:
{
  "creatives": [
    {
      "variant_name": "The Personal Pick",
      "angle": "Warm recommendation from Jamie at DiscountBlog",
      "subject_line": "string (compelling, 40-60 chars)",
      "pre_header": "string (preview text, 60-90 chars)",
      "html": "string (complete HTML email document)"
    }
  ]
}

Generate EXACTLY 10 creatives. Each html must be a complete <!DOCTYPE html> email document.
`, kit.SiteName, kit.SiteName, trackingLink)

	return sb.String()
}

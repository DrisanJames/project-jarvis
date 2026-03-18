package api

import (
	"bytes"
	"context"
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
// Kicks off async creative generation. Returns immediately with status.
// The frontend polls GET /creatives to see when they appear.
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

	// Mark generation in progress via a placeholder row
	jobID := uuid.New().String()
	och.db.ExecContext(ctx,
		`INSERT INTO mailing_offer_creatives
		 (id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
		 VALUES ($1, $2, 0, '', 'generating', 'AI generation in progress…', $3, $3)`,
		jobID, offerID, time.Now())

	// Fire async generation
	go och.runCreativeGeneration(jobID, offerID, name, description, htmlCreative, trackingLink, apiKey, kit)

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":  "generating",
		"message": "Creative generation started — poll the creatives list for results",
		"job_id":  jobID,
	})
}

func (och *OfferCenterHandlers) runCreativeGeneration(jobID, offerID, name, description, htmlCreative, trackingLink, apiKey string, kit BrandKit) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fail := func(msg string) {
		log.Printf("[CreativeGen] job %s FAILED: %s", jobID, msg)
		och.db.ExecContext(ctx,
			`UPDATE mailing_offer_creatives SET status='failed', approval_notes=$1, updated_at=NOW() WHERE id=$2`,
			"Generation failed: "+msg, jobID)
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
		"max_tokens": 32000,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		fail(fmt.Sprintf("marshal error: %v", err))
		return
	}

	log.Printf("[CreativeGen] job %s: calling Anthropic for offer %s (%s)", jobID, offerID, name)

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		fail(fmt.Sprintf("request creation error: %v", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 8 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fail(fmt.Sprintf("Anthropic request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fail(fmt.Sprintf("Anthropic returned %d: %s", resp.StatusCode, string(body[:min(500, len(body))])))
		return
	}

	var aiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		fail(fmt.Sprintf("decode error: %v", err))
		return
	}
	if len(aiResp.Content) == 0 {
		fail("empty AI response")
		return
	}

	content := strings.TrimSpace(aiResp.Content[0].Text)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	// Salvage truncated JSON by finding the last complete creative object
	var result CreativeGenerationResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		// Try to find the array start and extract valid creatives
		arrStart := strings.Index(content, `"creatives"`)
		if arrStart == -1 {
			log.Printf("[CreativeGen] parse error (no creatives key): %v\nraw start: %.500s", err, content[:min(500, len(content))])
			fail("failed to parse AI response")
			return
		}

		bracketStart := strings.Index(content[arrStart:], "[")
		if bracketStart == -1 {
			log.Printf("[CreativeGen] parse error (no array): %v", err)
			fail("failed to parse AI response")
			return
		}
		arrContent := content[arrStart+bracketStart:]

		// Find each complete creative object by tracking brace depth
		var creatives []GeneratedCreative
		depth := 0
		objStart := -1
		for i, ch := range arrContent {
			if ch == '{' {
				if depth == 0 {
					objStart = i
				}
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 && objStart >= 0 {
					var gc GeneratedCreative
					if e2 := json.Unmarshal([]byte(arrContent[objStart:i+1]), &gc); e2 == nil && gc.HTML != "" {
						creatives = append(creatives, gc)
					}
					objStart = -1
				}
			}
		}

		if len(creatives) == 0 {
			log.Printf("[CreativeGen] parse error: extracted 0 valid creatives\nraw start: %.500s\nraw end: %.500s",
				content[:min(500, len(content))], content[max(0, len(content)-500):])
			fail("failed to parse AI response")
			return
		}
		log.Printf("[CreativeGen] recovered %d creatives from partial JSON", len(creatives))
		result.Creatives = creatives
	}

	if len(result.Creatives) == 0 {
		fail("AI returned no creatives")
		return
	}

	count := 0
	for i, c := range result.Creatives {
		id := uuid.New().String()
		now := time.Now()

		_, err := och.db.ExecContext(ctx,
			`INSERT INTO mailing_offer_creatives
			 (id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
			 VALUES ($1, $2, COALESCE((SELECT MAX(version) FROM mailing_offer_creatives WHERE offer_id = $2), 0) + 1,
			         $3, 'generated', $4, $5, $6)`,
			id, offerID, c.HTML, fmt.Sprintf("[%s] %s", c.VariantName, c.Angle), now, now)
		if err != nil {
			log.Printf("[CreativeGen] error inserting creative %d: %v", i+1, err)
			continue
		}
		count++
	}

	// Remove the placeholder row
	och.db.ExecContext(ctx, `DELETE FROM mailing_offer_creatives WHERE id=$1`, jobID)

	log.Printf("[CreativeGen] job %s complete: generated %d creatives for offer %s", jobID, count, offerID)
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

IMPORTANT: Keep each HTML email compact — no extra whitespace, no unnecessary comments, minimize attribute repetition. Each email should be 3-6KB. Focus on CONTENT quality, not code verbosity.

Generate EXACTLY 10 creatives. Each html must be a complete <!DOCTYPE html> email document.
`, kit.SiteName, kit.SiteName, trackingLink)

	return sb.String()
}

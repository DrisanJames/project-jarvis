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

	var name, description, webProperty, htmlCreative, trackingLink, approvedAdCopy, approvedTaglines string
	err := och.db.QueryRowContext(ctx, `
		SELECT name, COALESCE(description,''), COALESCE(web_property,''),
			COALESCE(original_html_creative,''), COALESCE(tracking_link_template,''),
			COALESCE(approved_ad_copy,''), COALESCE(approved_taglines,'')
		FROM mailing_offers WHERE id=$1
	`, offerID).Scan(&name, &description, &webProperty, &htmlCreative, &trackingLink, &approvedAdCopy, &approvedTaglines)
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

	// If offer has no original_html_creative, fall back to the first imported creative
	if htmlCreative == "" {
		och.db.QueryRowContext(ctx,
			`SELECT html_content FROM mailing_offer_creatives
			 WHERE offer_id=$1 AND html_content != '' ORDER BY created_at ASC LIMIT 1`,
			offerID).Scan(&htmlCreative)
	}

	assets, _ := LoadOfferAssets(och.db, offerID)
	imageDomain := ResolveImageDomainForBrand(och.db, kit.ImageDomain)
	assetPrompt := BuildAssetPromptSection(assets, imageDomain)

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		respondError(w, http.StatusInternalServerError, "ANTHROPIC_API_KEY not set — add to GitHub Secrets and redeploy")
		return
	}

	// Clean up stale placeholders from previous failed/stuck runs
	och.db.ExecContext(ctx,
		`DELETE FROM mailing_offer_creatives WHERE offer_id=$1 AND status IN ('generating','failed') AND version=0`, offerID)

	// Mark generation in progress via a placeholder row
	jobID := uuid.New().String()
	och.db.ExecContext(ctx,
		`INSERT INTO mailing_offer_creatives
		 (id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
		 VALUES ($1, $2, 0, '', 'generating', 'AI generation in progress…', $3, $3)`,
		jobID, offerID, time.Now())

	// Fire async generation
	go och.runCreativeGeneration(jobID, offerID, name, description, htmlCreative, trackingLink,
		approvedAdCopy, approvedTaglines, assetPrompt, apiKey, kit)

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":  "generating",
		"message": "Creative generation started — poll the creatives list for results",
		"job_id":  jobID,
	})
}

func (och *OfferCenterHandlers) runCreativeGeneration(jobID, offerID, name, description, htmlCreative, trackingLink, approvedAdCopy, approvedTaglines, assetPrompt, apiKey string, kit BrandKit) {
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
	prompt := buildCreativeGenerationPrompt(name, description, creativeCopy, trackingLink, productImage,
		approvedAdCopy, approvedTaglines, assetPrompt, kit)

	systemPrompt := fmt.Sprintf(`You are the head email marketing creative director for %s (%s — "%s").

Brand voice: %s

You design high-converting HTML email creatives in multiple styles — image-rich co-branded, text-heavy editorial, and hybrid layouts. Every email you write gets opened, read, and clicked because it feels authentic to the brand and delivers real value.

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

		htmlWithUnsub := injectUnsubDisclaimer(c.HTML)
		_, err := och.db.ExecContext(ctx,
			`INSERT INTO mailing_offer_creatives
			 (id, offer_id, version, html_content, status, approval_notes, created_at, updated_at)
			 VALUES ($1, $2, COALESCE((SELECT MAX(version) FROM mailing_offer_creatives WHERE offer_id = $2), 0) + 1,
			         $3, 'generated', $4, $5, $6)`,
			id, offerID, htmlWithUnsub, fmt.Sprintf("[%s] %s", c.VariantName, c.Angle), now, now)
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

func buildCreativeGenerationPrompt(offerName, description, creativeCopy, trackingLink, productImage, approvedAdCopy, approvedTaglines, assetPrompt string, kit BrandKit) string {
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
		fmt.Fprintf(&sb, "\nORIGINAL CREATIVE COPY (extracted from the advertiser email — analyze the product language, claims, value props, and tone):\n%s\n", creativeCopy)
	}

	if approvedAdCopy != "" {
		copy := approvedAdCopy
		if len(copy) > 3000 {
			copy = copy[:3000]
		}
		fmt.Fprintf(&sb, "\nAPPROVED AD COPY (use this as the primary selling language — do NOT invent new claims):\n%s\n", copy)
	}

	if approvedTaglines != "" {
		fmt.Fprintf(&sb, "\nAPPROVED TAGLINES/SUBJECT LINES (use these as inspiration for subject lines and headers):\n%s\n", approvedTaglines)
	}

	if productImage != "" {
		fmt.Fprintf(&sb, "\nPRODUCT IMAGE: %s\n", productImage)
	}

	if trackingLink != "" {
		fmt.Fprintf(&sb, "\nCTA TRACKING URL: %s\n", trackingLink)
	}

	if assetPrompt != "" {
		sb.WriteString(assetPrompt)
	}

	hasAssets := assetPrompt != ""

	if hasAssets {
		fmt.Fprintf(&sb, `
TASK: Generate exactly 10 HTML email creatives for the offer above, split into THREE TIERS:

═══ TIER 1: CO-BRANDED IMAGE-RICH (variants 1-5) ═══
These emails showcase the uploaded brand creative assets prominently.
- Use hero/header images (email_hero, email_header roles) at full width at the top
- Use content/inline images (email_content, content_block roles) within the body
- Use the EXACT CDN URLs listed above in <img> tags — do NOT use placeholder URLs
- Set explicit width and height attributes on every <img> tag
- Weave in the approved ad copy as the primary selling language
- Rich visual layout: brand header with logo, hero image, styled product cards, accent-colored CTA button
- Each of the 5 must use a DIFFERENT persuasion angle:
  1. "The Personal Pick" — warm recommendation from %s staff, hero image + editorial intro
  2. "Urgency/FOMO" — limited-time deal with bold imagery, countdown language
  3. "Social Proof" — reviews/ratings with product imagery
  4. "Problem → Solution" — pain point intro then product hero with solution
  5. "Comparison Winner" — side-by-side value proposition with product images

═══ TIER 2: TEXT-HEAVY EDITORIAL (variants 6-7) ═══
Minimal or no images. Copy-driven, personal voice.
- At most ONE product image (the smallest available, or none)
- Feels like a personal email from a friend, not a marketing blast
- Use approved ad copy and taglines to craft the narrative
- Each must use a DIFFERENT angle:
  6. "Quick Hit" — ultra-short, 3-4 sentences with a bold CTA
  7. "Story" — mini narrative about someone who benefited from this product

═══ TIER 3: HYBRID (variants 8-10) ═══
Strategic 1-2 image placements with strong text copy.
- ONE hero or banner image at the top from the uploaded assets
- Text-driven body paragraphs with approved ad copy woven in
- Balance between visual appeal and editorial depth
- Each must use a DIFFERENT angle:
  8. "FAQ Style" — addresses objections in Q&A format with a product banner
  9. "VIP/Exclusive" — insider access feel with a premium hero image
  10. "Money Saver" — savings-focused with a deal banner image

`, kit.SiteName)
	} else {
		fmt.Fprintf(&sb, `
TASK: Generate exactly 10 HTML email creatives for the offer above, split into THREE TIERS:

NOTE: No brand images were uploaded for this offer. Generate ALL creatives as text-driven layouts.

═══ TIER 1: EDITORIAL (variants 1-5) ═══
Text-driven with strong brand styling (colors, fonts, logo). No stock images.
- Use the brand identity (colors, fonts, logo) heavily in the layout
- Each of the 5 must use a DIFFERENT persuasion angle:
  1. "The Personal Pick" — warm recommendation from %s staff
  2. "Urgency/FOMO" — limited-time deal, act now
  3. "Social Proof" — reviews, ratings, what others are saying
  4. "Problem → Solution" — pain point first, then the product as the fix
  5. "Comparison Winner" — why this beats the alternatives

═══ TIER 2: MINIMAL (variants 6-7) ═══
Ultra-clean, plain-text feel. Almost no formatting.
  6. "Quick Hit" — 3-4 sentences, bold CTA, nothing else
  7. "Story" — short narrative, friendly tone, single CTA at the end

═══ TIER 3: STRUCTURED (variants 8-10) ═══
Styled cards, sections, or Q&A blocks — text only, no images.
  8. "FAQ Style" — addresses objections in Q&A format
  9. "VIP/Exclusive" — insider access feel, premium styling
  10. "Money Saver" — savings-focused with deal callouts

`, kit.SiteName)
	}

	fmt.Fprintf(&sb, `ALL CREATIVES MUST:
- Use the %s brand identity (colors, fonts, voice, logo)
- Feel personal — like a recommendation from a trusted friend
- Include personalization merge tags where natural
- Have a strong, clear CTA button linking to the tracking URL
- Be mobile-responsive (600px max-width, scales down)
- Use only inline CSS (no <style> blocks)
- Include an unsubscribe footer
- Be 100%% email-client compatible (Gmail, Outlook, Apple Mail, Yahoo)

PERSONALIZATION TAGS (use naturally — don't force all into every email):
- {%% if first_name %%}Hi {{ first_name }},{%% else %%}Hey there,{%% endif %%}
- {{ first_name }} — subscriber's first name
- {{ email }} — subscriber's email
- {{ system.current_year }} — current year
- {{ system.unsubscribe_url }} — unsubscribe link (REQUIRED in footer)

HTML TEMPLATE REQUIREMENTS:
- Outer table: width 100%%, background #f4f4f4 (light) or #1a1a2e (dark depending on brand)
- Inner table: max-width 600px, centered
- Header: brand logo using the LogoHTML provided, styled with brand heading font
- Body: 16-18px body font, 1.6 line-height, brand body font
- CTA button: brand primary color background, white text, border-radius 8px, padding 14px 32px
- Footer: small text with sender info, unsubscribe link using {{ system.unsubscribe_url }}, physical address
- All images must have alt text and explicit width/height attributes
- Use <!--[if mso]> conditionals for Outlook button rendering
`, kit.SiteName)

	if trackingLink != "" {
		fmt.Fprintf(&sb, `
For the CTA URL, use exactly: %s
This URL may contain merge tags. Keep these EXACT placeholders as-is:
- {{DATE_MMDDYYYY}}, {{MAILING_ID}}, {{SUBSCRIBER_ID}}, {{CAMPAIGN_ID}}, {{CREATIVE_ID}}
Do NOT change, remove, or re-encode these placeholders.
`, trackingLink)
	}

	sb.WriteString(`
RETURN THIS JSON:
{
  "creatives": [
    {
      "variant_name": "The Personal Pick",
      "angle": "Warm recommendation with hero image and editorial intro",
      "subject_line": "string (compelling, 40-60 chars)",
      "pre_header": "string (preview text, 60-90 chars)",
      "html": "string (complete HTML email document)"
    }
  ]
}

IMPORTANT: Keep each HTML email compact — no extra whitespace, no unnecessary comments. Each email should be 3-6KB. Focus on CONTENT quality, not code verbosity.

Generate EXACTLY 10 creatives. Each html must be a complete <!DOCTYPE html> email document.
`)

	return sb.String()
}

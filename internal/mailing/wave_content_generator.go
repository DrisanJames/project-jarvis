package mailing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// WaveVariation holds a fully rendered email variation for a single wave.
type WaveVariation struct {
	WaveIndex   int    `json:"wave_index"`
	Subject     string `json:"subject"`
	PreviewText string `json:"preview_text"`
	FromName    string `json:"from_name"`
	HTMLContent string `json:"html_content"`
}

// WaveContentRequest describes what the generator should produce.
type WaveContentRequest struct {
	SendingDomain string `json:"sending_domain"`
	BrandName     string `json:"brand_name"`
	NumWaves      int    `json:"num_waves"`
	CampaignType  string `json:"campaign_type"`
	ContentPool   []BlogExcerpt
	BrandInfo     *BrandIntelligence
}

// WaveContentGenerator produces structurally unique email content per wave.
type WaveContentGenerator struct {
	ai *AIContentService
}

func NewWaveContentGenerator(ai *AIContentService) *WaveContentGenerator {
	return &WaveContentGenerator{ai: ai}
}

// BuildPrompt constructs the AI prompt for wave-level content generation.
// Returned separately so the caller can review it before generation.
func (g *WaveContentGenerator) BuildPrompt(req WaveContentRequest) string {
	brand := req.BrandInfo
	if brand == nil {
		brand = &BrandIntelligence{Domain: req.SendingDomain, Colors: []string{}}
	}

	nonce := shortHex(4)
	fromName := coalesce(req.BrandName, brand.Title, "Newsletter")

	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(`You are an elite email designer and deliverability engineer. You must generate %d COMPLETELY UNIQUE email variations — one for each "wave" of a staggered campaign.

MISSION: Every wave must look and feel like a DIFFERENT email to spam filters and inbox providers. Gmail, Outlook, and Yahoo fingerprint emails by HTML structure, CSS patterns, DOM depth, and content hashes. Your job is to make each wave structurally distinct while maintaining brand consistency.

`, req.NumWaves))

	sb.WriteString(fmt.Sprintf(`CRITICAL ANTI-FINGERPRINT RULES:
1. Each wave MUST have a DIFFERENT HTML document structure — vary the nesting depth, table layout, number of content sections
2. Each wave MUST use DIFFERENT CSS class names and inline style values — append the nonce suffix "%s" to all class names
3. Each wave MUST have DIFFERENT whitespace patterns — vary blank lines, HTML comments, spacing between elements
4. Each wave MUST have a DIFFERENT subject line and preview text — same theme, different wording
5. Each wave MUST reorder content blocks — if Wave 1 shows Article A then B, Wave 2 shows B then A (or different articles entirely)
6. Vary padding/margin values by 1-3px between waves (e.g., padding: 20px vs padding: 22px vs padding: 18px)
7. Vary color shades subtly between waves (e.g., #2563EB vs #2564EA vs #2462EC)
8. Use DIFFERENT HTML comment strings in each wave as structural salt
9. Each wave MUST be a complete, self-contained HTML document (<!DOCTYPE html>, <html>, <head>, <body>)
10. TABLE-BASED LAYOUT only — email-client compatible (Outlook, Gmail, Yahoo, Apple Mail)
11. ALL CSS must be INLINE — no <style> blocks, no external stylesheets
12. Maximum width: 600px, centered
13. Return ONLY valid JSON — no markdown, no code fences, no explanation

`, nonce))

	campaignDesc := describeWaveCampaignType(req.CampaignType)
	sb.WriteString(fmt.Sprintf("CAMPAIGN TYPE: %s\n\n", campaignDesc))

	sb.WriteString("BRAND INTELLIGENCE:\n")
	sb.WriteString(fmt.Sprintf("- Domain: %s\n", brand.Domain))
	if req.BrandName != "" {
		sb.WriteString(fmt.Sprintf("- Brand Name: %s\n", req.BrandName))
	} else if brand.Title != "" {
		sb.WriteString(fmt.Sprintf("- Brand Name: %s\n", brand.Title))
	}
	if brand.Description != "" {
		sb.WriteString(fmt.Sprintf("- Brand Description: %s\n", brand.Description))
	}
	if brand.LogoURL != "" {
		sb.WriteString(fmt.Sprintf("- Logo URL: %s\n", brand.LogoURL))
	}
	if len(brand.Colors) > 0 {
		sb.WriteString(fmt.Sprintf("- Brand Colors: %s\n", strings.Join(brand.Colors, ", ")))
	}
	if brand.FontFamily != "" {
		sb.WriteString(fmt.Sprintf("- Font Family: %s\n", brand.FontFamily))
	}

	pool := req.ContentPool
	if len(pool) == 0 {
		pool = brand.BlogPosts
	}
	if len(pool) > 0 {
		sb.WriteString("\nFRESH CONTENT POOL (use different articles in each wave, reorder, and paraphrase):\n")
		for i, p := range pool {
			sb.WriteString(fmt.Sprintf("  [%d] Title: %s\n", i+1, p.Title))
			if p.Excerpt != "" {
				sb.WriteString(fmt.Sprintf("      Excerpt: %s\n", p.Excerpt))
			}
			if p.URL != "" {
				sb.WriteString(fmt.Sprintf("      Link: %s\n", p.URL))
			}
		}
		sb.WriteString("\nIMPORTANT: Each wave should feature different articles from this pool, or present the same articles in a different order with paraphrased descriptions. Never use identical text across waves.\n")
	}

	sb.WriteString(`
PERSONALIZATION (LIQUID MERGE TAGS — include literally in the HTML):
- Use {{ first_name | default: "there" }} for greeting
- Use {{ email }} for subscriber email
- Use {{ system.unsubscribe_url }} for mandatory unsubscribe link (REQUIRED in every wave)
- Use {{ system.preferences_url }} for manage-preferences link
- The unsubscribe link MUST appear in every template footer

STRUCTURAL VARIATION STRATEGY:
`)

	for i := 0; i < req.NumWaves; i++ {
		switch i {
		case 0:
			sb.WriteString(fmt.Sprintf("- Wave %d: Classic editorial layout — hero section with headline, 2-3 content cards below, clean footer. Use a serif-inspired heading feel.\n", i+1))
		case 1:
			sb.WriteString(fmt.Sprintf("- Wave %d: Bold magazine layout — large typography-driven header, alternating left-right content blocks, prominent CTA button. Use a modern sans-serif feel.\n", i+1))
		case 2:
			sb.WriteString(fmt.Sprintf("- Wave %d: Minimal personal letter layout — conversational tone, single-column flowing text with inline links, feels like a note from a friend. Light and airy.\n", i+1))
		default:
			sb.WriteString(fmt.Sprintf("- Wave %d: Unique layout — structurally distinct from all previous waves, different section count and arrangement.\n", i+1))
		}
	}

	sb.WriteString(fmt.Sprintf(`
SUBJECT LINE RULES:
- Each wave MUST have a DIFFERENT subject line (same theme, different angle/wording)
- Keep under 60 characters
- Use curiosity, urgency, or personalization — vary the approach per wave
- Include {{ first_name }} in at least one subject line

PREVIEW TEXT RULES:
- Each wave MUST have a DIFFERENT preview_text (the preheader snippet visible in inbox)
- Keep under 100 characters
- Should complement but NOT repeat the subject line
- Entice the reader to open

FROM NAME: Use "%s" or a natural variation of the brand name.

RESPOND WITH EXACTLY THIS JSON STRUCTURE (no wrapping, no markdown):
[
`, fromName))

	for i := 0; i < req.NumWaves; i++ {
		comma := ","
		if i == req.NumWaves-1 {
			comma = ""
		}
		sb.WriteString(fmt.Sprintf(`  {
    "wave_index": %d,
    "from_name": "...",
    "subject": "...",
    "preview_text": "...",
    "html_content": "<!DOCTYPE html>..."
  }%s
`, i, comma))
	}
	sb.WriteString("]\n")

	return sb.String()
}

// Generate calls the AI with the built prompt and returns wave variations.
func (g *WaveContentGenerator) Generate(ctx context.Context, req WaveContentRequest) ([]WaveVariation, error) {
	prompt := g.BuildPrompt(req)
	return g.generateFromPrompt(ctx, prompt)
}

// GenerateFromPrompt generates wave variations from a pre-built (possibly reviewed) prompt.
func (g *WaveContentGenerator) GenerateFromPrompt(ctx context.Context, prompt string) ([]WaveVariation, error) {
	return g.generateFromPrompt(ctx, prompt)
}

func (g *WaveContentGenerator) generateFromPrompt(ctx context.Context, prompt string) ([]WaveVariation, error) {
	var variations []WaveVariation
	var err error

	if g.ai.anthropicKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			variations, err = g.callClaudeForWaves(ctx, prompt)
			if err == nil && len(variations) > 0 {
				return variations, nil
			}
			if attempt == 0 {
				log.Printf("Claude wave generation attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
		log.Printf("Claude wave generation failed, falling back to OpenAI: %v", err)
	}

	if g.ai.openaiKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			variations, err = g.callOpenAIForWaves(ctx, prompt)
			if err == nil && len(variations) > 0 {
				return variations, nil
			}
			if attempt == 0 {
				log.Printf("OpenAI wave generation attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("wave content generation failed: %w", err)
	}
	return nil, fmt.Errorf("wave content generation produced no variations (no API keys configured)")
}

func (g *WaveContentGenerator) callClaudeForWaves(ctx context.Context, prompt string) ([]WaveVariation, error) {
	raw, err := g.ai.callClaudeRaw(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return parseWaveVariations(raw)
}

func (g *WaveContentGenerator) callOpenAIForWaves(ctx context.Context, prompt string) ([]WaveVariation, error) {
	raw, err := g.ai.callOpenAIRaw(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return parseWaveVariations(raw)
}

func parseWaveVariations(raw string) ([]WaveVariation, error) {
	cleaned := sanitizeAIJSON(raw)
	var variations []WaveVariation
	if err := json.Unmarshal([]byte(cleaned), &variations); err != nil {
		return nil, fmt.Errorf("failed to parse wave variations: %w (raw length: %d)", err, len(raw))
	}
	return variations, nil
}

func describeWaveCampaignType(ct string) string {
	switch strings.ToLower(ct) {
	case "newsletter":
		return "Newsletter — curated content digest with articles, tips, and updates from the brand's blog"
	case "promotional":
		return "Promotional — deals, discounts, and product highlights designed to drive conversions"
	case "welcome":
		return "Welcome — warm introduction to the brand for new subscribers"
	case "winback":
		return "Win-back — re-engage lapsed subscribers with compelling reasons to return"
	default:
		return "Newsletter — curated content digest with articles, tips, and updates"
	}
}

func shortHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

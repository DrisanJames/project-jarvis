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
	Voice         string
	Audience      string
	DesignSystem  string
	HTMLTemplate  string
	ContentPool   []BlogExcerpt
	BrandInfo     *BrandIntelligence
}

// WaveEditorialContent holds the editorial copy for one wave, produced by Phase 1.
type WaveEditorialContent struct {
	WaveIndex       int              `json:"wave_index"`
	Subject         string           `json:"subject"`
	PreviewText     string           `json:"preview_text"`
	Intro           string           `json:"intro"`
	ArticleSections []ArticleSection `json:"article_sections"`
	ClosingLine     string           `json:"closing_line"`
}

// ArticleSection is one article summary within a wave's editorial content.
type ArticleSection struct {
	Headline string `json:"headline"`
	Summary  string `json:"summary"`
	CTAText  string `json:"cta_text"`
	URL      string `json:"url"`
}

// WaveContentGenerator produces structurally unique email content per wave
// using a two-phase AI pipeline: editorial writing then HTML rendering.
type WaveContentGenerator struct {
	ai *AIContentService
}

func NewWaveContentGenerator(ai *AIContentService) *WaveContentGenerator {
	return &WaveContentGenerator{ai: ai}
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// BuildPrompt returns the Phase 1 (editorial) prompt for review.
func (g *WaveContentGenerator) BuildPrompt(req WaveContentRequest) string {
	return g.buildEditorialPrompt(req)
}

// Generate runs the full two-phase pipeline: editorial writing then HTML rendering.
// Returns variations and a diagnostic report for review.
func (g *WaveContentGenerator) Generate(ctx context.Context, req WaveContentRequest) ([]WaveVariation, *GenerationReport, error) {
	report := &GenerationReport{
		Brand:     req.BrandName,
		StartedAt: time.Now(),
		Version:   GeneratorVersion,
	}

	// Phase 1: Editorial
	log.Printf("[wave-gen] Phase 1: generating editorial copy for %d waves of %s", req.NumWaves, req.BrandName)
	p1Start := time.Now()
	editorial, err := g.generateEditorial(ctx, req)
	report.Phase1Report.DurationMs = time.Since(p1Start).Milliseconds()
	report.Phase1Report.Model = g.activeModel()
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("Phase 1 failed: %v", err))
		report.FinishedAt = time.Now()
		report.TotalDurationMs = time.Since(report.StartedAt).Milliseconds()
		LogReport(report)
		return nil, report, fmt.Errorf("Phase 1 (editorial) failed: %w", err)
	}
	log.Printf("[wave-gen] Phase 1 complete: got %d editorial wave(s) in %dms", len(editorial), report.Phase1Report.DurationMs)

	// Phase 2: Mechanical template fill (no AI — preserves exact brand styling)
	log.Printf("[wave-gen] Phase 2: filling template for %d editorial waves", len(editorial))
	p2Start := time.Now()
	variations, err := g.renderHTML(ctx, editorial, req)
	report.Phase2Report.DurationMs = time.Since(p2Start).Milliseconds()
	report.Phase2Report.Model = "template-fill"
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("Phase 2 failed: %v", err))
		report.FinishedAt = time.Now()
		report.TotalDurationMs = time.Since(report.StartedAt).Milliseconds()
		LogReport(report)
		return nil, report, fmt.Errorf("Phase 2 (template fill) failed: %w", err)
	}
	log.Printf("[wave-gen] Phase 2 complete: got %d variation(s) in %dms (template fill, no AI)", len(variations), report.Phase2Report.DurationMs)

	report.FinishedAt = time.Now()
	report.TotalDurationMs = time.Since(report.StartedAt).Milliseconds()
	LogReport(report)

	return variations, report, nil
}

func (g *WaveContentGenerator) activeModel() string {
	if g.ai.anthropicKey != "" {
		return "claude"
	}
	if g.ai.openaiKey != "" {
		return "openai"
	}
	return "none"
}

// GenerateFromPrompt is kept for backward compatibility (single-shot mode).
func (g *WaveContentGenerator) GenerateFromPrompt(ctx context.Context, prompt string) ([]WaveVariation, error) {
	return g.callAIForWaves(ctx, prompt)
}

// ---------------------------------------------------------------------------
// Phase 1: Editorial Content Writer
// ---------------------------------------------------------------------------

func (g *WaveContentGenerator) buildEditorialPrompt(req WaveContentRequest) string {
	brand := req.BrandInfo
	if brand == nil {
		brand = &BrandIntelligence{Domain: req.SendingDomain, Colors: []string{}}
	}

	var sb strings.Builder

	// Voice is the entire personality definition — put it front and center
	if req.Voice != "" {
		sb.WriteString(fmt.Sprintf("YOUR IDENTITY:\n%s\n\n", req.Voice))
	}
	if req.Audience != "" {
		sb.WriteString(fmt.Sprintf("WHO YOU'RE WRITING TO:\n%s\n\n", req.Audience))
	}

	sb.WriteString(fmt.Sprintf("You must write %d newsletter editions for \"%s\". Each one is a different \"wave\" sent to a different slice of subscribers, so they MUST be meaningfully different — different article selection, different angle, different subject line.\n\n", req.NumWaves, coalesce(req.BrandName, brand.Title)))

	pool := req.ContentPool
	if len(pool) == 0 {
		pool = brand.BlogPosts
	}
	if len(pool) > 0 {
		sb.WriteString("=== SOURCE MATERIAL (your ONLY source — do NOT invent content) ===\n\n")
		for i, p := range pool {
			sb.WriteString(fmt.Sprintf("--- [%d] %s ---\n", i+1, p.Title))
			if p.URL != "" {
				sb.WriteString(fmt.Sprintf("URL: %s\n", p.URL))
			}
			if p.FullText != "" {
				sb.WriteString(fmt.Sprintf("\n%s\n", p.FullText))
			} else if p.Excerpt != "" {
				sb.WriteString(fmt.Sprintf("%s\n", p.Excerpt))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(`=== QUALITY STANDARDS (this is what separates good from garbage) ===

THE INTRO must feel like a real person wrote it. NOT "Hey there! Hope you're having a great week!" — that's spam. Start with something specific: a real observation, a personal anecdote, a surprising stat, or a direct question that relates to the articles. Make the reader think "oh, this is actually interesting."

ARTICLE SUMMARIES must provide genuine value. Each one should:
- Lead with the most surprising or useful insight from the article (a number, a technique, a counterintuitive finding)
- Explain WHY it matters to the reader in their daily life
- Be 3-5 sentences of real substance — not vague teasers
- Use language that matches the brand voice exactly

CTA TEXT must be specific to the article, NOT generic. "Read more" and "Learn more" are banned. Use the article's actual promise: "See the 30-day plan", "Get the Disney budget breakdown", "Try the $20 date ideas."

THE CLOSING must feel like a real sign-off, not a template. Relate it back to something in this specific edition.

SUBJECT LINES must be specific and intriguing. They should reference actual content. "Your weekly update" is garbage. "We did Disney for $3,500 (family of six)" is good. Keep under 60 chars.

PREVIEW TEXT complements the subject — adds a second reason to open. Never repeats the subject. Keep under 90 chars.

=== PERSONALIZATION ===
- Use {{ first_name | default: "there" }} for the subscriber's name
- Use {{ first_name }} in at least one subject line across the waves

=== OUTPUT FORMAT ===
Respond with ONLY this JSON array (no markdown, no explanation, no code fences):
[
`)

	for i := 0; i < req.NumWaves; i++ {
		comma := ","
		if i == req.NumWaves-1 {
			comma = ""
		}
		sb.WriteString(fmt.Sprintf(`  {
    "wave_index": %d,
    "subject": "specific, intriguing, under 60 chars",
    "preview_text": "complements subject, under 90 chars",
    "intro": "2-3 sentences — personal, specific, sets the tone",
    "article_sections": [
      {
        "headline": "paraphrased article headline",
        "summary": "3-5 sentences of real substance from the source material",
        "cta_text": "specific action — NOT 'read more'",
        "url": "real URL from source material"
      }
    ],
    "closing_line": "warm, specific sign-off"
  }%s
`, i, comma))
	}
	sb.WriteString("]\n")

	return sb.String()
}

func (g *WaveContentGenerator) generateEditorial(ctx context.Context, req WaveContentRequest) ([]WaveEditorialContent, error) {
	prompt := g.buildEditorialPrompt(req)
	var editorial []WaveEditorialContent
	var err error

	if g.ai.anthropicKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			raw, callErr := g.ai.callClaudeRaw(ctx, prompt)
			if callErr == nil {
				editorial, err = parseEditorial(raw)
				if err == nil && len(editorial) > 0 {
					return editorial, nil
				}
			} else {
				err = callErr
			}
			if attempt == 0 {
				log.Printf("[wave-gen] Phase 1 Claude attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
		log.Printf("[wave-gen] Phase 1 Claude failed, falling back to OpenAI: %v", err)
	}

	if g.ai.openaiKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			raw, callErr := g.ai.callOpenAIRaw(ctx, prompt)
			if callErr == nil {
				editorial, err = parseEditorial(raw)
				if err == nil && len(editorial) > 0 {
					return editorial, nil
				}
			} else {
				err = callErr
			}
			if attempt == 0 {
				log.Printf("[wave-gen] Phase 1 OpenAI attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
	}

	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("editorial generation produced no results")
}

func parseEditorial(raw string) ([]WaveEditorialContent, error) {
	cleaned := sanitizeAIJSON(raw)
	var editorial []WaveEditorialContent
	if err := json.Unmarshal([]byte(cleaned), &editorial); err != nil {
		return nil, fmt.Errorf("failed to parse editorial JSON: %w (raw length: %d)", err, len(raw))
	}
	return editorial, nil
}

// ---------------------------------------------------------------------------
// Phase 2: Mechanical Template Fill + Programmatic CSS Variation
// ---------------------------------------------------------------------------

func (g *WaveContentGenerator) renderHTML(_ context.Context, editorial []WaveEditorialContent, req WaveContentRequest) ([]WaveVariation, error) {
	if req.HTMLTemplate == "" {
		return nil, fmt.Errorf("no HTML template provided — cannot render")
	}
	return templateFillWithVariation(editorial, req), nil
}

// templateFillWithVariation fills the HTML template with editorial content and
// applies per-wave CSS micro-variations for anti-fingerprinting. The layout and
// design stay pixel-perfect; only padding nudges, tiny color shifts, and
// structural HTML comments change between waves.
func templateFillWithVariation(editorial []WaveEditorialContent, req WaveContentRequest) []WaveVariation {
	fromName := coalesce(req.BrandName, "Newsletter")
	brandKey := brandKeyFromName(req.BrandName)
	var variations []WaveVariation

	for waveIdx, e := range editorial {
		html := req.HTMLTemplate

		// --- Content substitution ---
		html = strings.ReplaceAll(html, "{{SUBJECT}}", e.Subject)
		html = strings.ReplaceAll(html, "{{PREVIEW_TEXT}}", e.PreviewText)
		html = strings.ReplaceAll(html, "{{INTRO}}", e.Intro)
		html = strings.ReplaceAll(html, "{{CLOSING_LINE}}", e.ClosingLine)

		for i, a := range e.ArticleSections {
			n := fmt.Sprintf("%d", i+1)
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_HEADLINE}}", a.Headline)
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_SUMMARY}}", a.Summary)
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_CTA}}", a.CTAText)
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_URL}}", a.URL)
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_CATEGORY}}", pickCategory(brandKey, i))
			html = strings.ReplaceAll(html, "{{ARTICLE_"+n+"_EMOJI}}", pickEmoji(brandKey, i))
		}

		// --- Remove unused article blocks (entire HTML section, not just placeholders) ---
		for i := len(e.ArticleSections) + 1; i <= 5; i++ {
			html = removeBlock(html, fmt.Sprintf("ARTICLE_%d", i))
		}

		// --- Programmatic CSS variation (anti-fingerprint) ---
		html = applyCSSVariation(html, waveIdx)

		variations = append(variations, WaveVariation{
			WaveIndex:   e.WaveIndex,
			Subject:     e.Subject,
			PreviewText: e.PreviewText,
			FromName:    fromName,
			HTMLContent: html,
		})
	}

	return variations
}

// removeBlock strips everything between <!-- BLOCK:name --> and <!-- /BLOCK:name -->
// inclusive, removing the entire article row when unused.
func removeBlock(html, name string) string {
	start := "<!-- BLOCK:" + name + " -->"
	end := "<!-- /BLOCK:" + name + " -->"
	startIdx := strings.Index(html, start)
	endIdx := strings.Index(html, end)
	if startIdx < 0 || endIdx < 0 || endIdx < startIdx {
		return html
	}
	return html[:startIdx] + html[endIdx+len(end):]
}

// applyCSSVariation applies subtle per-wave CSS tweaks that keep the design
// identical to the human eye but make each wave's HTML structurally unique.
func applyCSSVariation(html string, waveIdx int) string {
	nonce := shortHex(3)

	// 1. Unique HTML comment at the top (structural salt)
	salt := fmt.Sprintf("<!-- w%d-%s -->", waveIdx, nonce)
	html = strings.Replace(html, "<!-- preview text -->", salt+"\n<!-- preview text -->", 1)

	// 2. Nudge container padding by ±1-2px per wave
	padShifts := []struct{ from, to string }{
		{"padding:28px 32px 20px 32px", fmt.Sprintf("padding:%dpx 32px %dpx 32px", 28+waveIdx%3, 20+waveIdx%2)},
		{"padding:28px 32px 0 32px", fmt.Sprintf("padding:%dpx 32px 0 32px", 28+(waveIdx+1)%3)},
		{"padding:12px 32px", fmt.Sprintf("padding:%dpx 32px", 12+waveIdx%3)},
		{"padding:16px 32px 28px 32px", fmt.Sprintf("padding:%dpx 32px %dpx 32px", 16+waveIdx%2, 28+waveIdx%3)},
		{"padding:24px 32px", fmt.Sprintf("padding:%dpx 32px", 24+waveIdx%3)},
		{"padding:24px 28px 16px 28px", fmt.Sprintf("padding:%dpx 28px %dpx 28px", 24+waveIdx%3, 16+waveIdx%2)},
		{"padding:24px 28px 8px 28px", fmt.Sprintf("padding:%dpx 28px %dpx 28px", 24+(waveIdx+1)%3, 8+waveIdx%2)},
		{"padding:16px 28px", fmt.Sprintf("padding:%dpx 28px", 16+waveIdx%3)},
		{"padding:8px 28px", fmt.Sprintf("padding:%dpx 28px", 8+waveIdx%2)},
	}
	for _, ps := range padShifts {
		html = strings.Replace(html, ps.from, ps.to, 1)
	}

	// 3. Vary a minor color channel by ±1-2 per wave (invisible to the eye)
	colorShifts := []struct{ from, to string }{
		{"#FF7B7B", fmt.Sprintf("#FF%02X%02X", 0x7B+waveIdx%3, 0x7B+(waveIdx+1)%3)},
		{"#5FCCB8", fmt.Sprintf("#5F%02XB8", 0xCC+waveIdx%3)},
		{"#8B5CF6", fmt.Sprintf("#8B%02XF6", 0x5C+waveIdx%3)},
		{"#39FF14", fmt.Sprintf("#39FF%02X", 0x14+waveIdx%3)},
	}
	for _, cs := range colorShifts {
		html = strings.Replace(html, cs.from, cs.to, 1)
	}

	// 4. Add a trailing comment after the closing tag (unique per wave)
	html = strings.Replace(html, "</html>", fmt.Sprintf("</html>\n<!-- %s -->", shortHex(4)), 1)

	return html
}

// pickCategory returns a contextual category label by brand and article position.
func pickCategory(brandKey string, articleIdx int) string {
	switch brandKey {
	case "discountblog":
		cats := []string{"SAVINGS TIPS", "SMART SHOPPING", "FAMILY FINANCE", "BUDGETING", "DEALS"}
		if articleIdx < len(cats) {
			return cats[articleIdx]
		}
		return "FEATURED"
	case "quizfiesta":
		cats := []string{"GAME MODE", "CHALLENGE", "LEADERBOARD", "MULTIPLAYER", "ARCADE"}
		if articleIdx < len(cats) {
			return cats[articleIdx]
		}
		return "PLAY NOW"
	default:
		return "FEATURED"
	}
}

// pickEmoji returns a contextual emoji by brand and article position.
func pickEmoji(brandKey string, articleIdx int) string {
	switch brandKey {
	case "discountblog":
		emojis := []string{"💰", "🛒", "🏠", "📊", "🔥"}
		if articleIdx < len(emojis) {
			return emojis[articleIdx]
		}
		return "📰"
	case "quizfiesta":
		emojis := []string{"🎮", "💀", "⚡", "🏆", "👾"}
		if articleIdx < len(emojis) {
			return emojis[articleIdx]
		}
		return "🕹️"
	default:
		return "📰"
	}
}

func brandKeyFromName(name string) string {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "discount") {
		return "discountblog"
	}
	if strings.Contains(lower, "quiz") || strings.Contains(lower, "fiesta") {
		return "quizfiesta"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Shared AI call helpers
// ---------------------------------------------------------------------------

func (g *WaveContentGenerator) callAIForWaves(ctx context.Context, prompt string) ([]WaveVariation, error) {
	var variations []WaveVariation
	var err error

	if g.ai.anthropicKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			raw, callErr := g.ai.callClaudeRaw(ctx, prompt)
			if callErr == nil {
				variations, err = parseWaveVariations(raw)
				if err == nil && len(variations) > 0 {
					return variations, nil
				}
			} else {
				err = callErr
			}
			if attempt == 0 {
				log.Printf("[wave-gen] HTML render Claude attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
		log.Printf("[wave-gen] HTML render Claude failed, falling back to OpenAI: %v", err)
	}

	if g.ai.openaiKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			raw, callErr := g.ai.callOpenAIRaw(ctx, prompt)
			if callErr == nil {
				variations, err = parseWaveVariations(raw)
				if err == nil && len(variations) > 0 {
					return variations, nil
				}
			} else {
				err = callErr
			}
			if attempt == 0 {
				log.Printf("[wave-gen] HTML render OpenAI attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("HTML rendering failed: %w", err)
	}
	return nil, fmt.Errorf("HTML rendering produced no variations (no API keys configured)")
}

func parseWaveVariations(raw string) ([]WaveVariation, error) {
	cleaned := sanitizeAIJSON(raw)
	var variations []WaveVariation
	if err := json.Unmarshal([]byte(cleaned), &variations); err != nil {
		return nil, fmt.Errorf("failed to parse wave variations: %w (raw length: %d)", err, len(raw))
	}
	return variations, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
	case "trivia":
		return "Trivia/Interactive — fun, engaging email featuring quiz modes, challenges, and leaderboard updates"
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

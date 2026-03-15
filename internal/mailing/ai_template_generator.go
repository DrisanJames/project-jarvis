package mailing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// TemplateGenerationRequest holds the input for AI-driven email template generation.
type TemplateGenerationRequest struct {
	CampaignType  string `json:"campaign_type"`  // welcome, winback, newsletter, promotional, re-engagement, announcement
	SendingDomain string `json:"sending_domain"` // domain to scrape for brand intelligence
	ReferenceHTML string `json:"reference_html"`  // optional existing template HTML as style inspiration
}

// GeneratedVariation is one AI-produced email template.
type GeneratedVariation struct {
	VariantName string `json:"variant_name"` // A, B, C, D, E
	FromName    string `json:"from_name"`
	Subject     string `json:"subject"`
	HTMLContent string `json:"html_content"`
}

// TemplateGenerationResult wraps the full generation response.
type TemplateGenerationResult struct {
	Variations  []GeneratedVariation `json:"variations"`
	BrandInfo   *BrandIntelligence   `json:"brand_info"`
	GeneratedAt string               `json:"generated_at"`
}

// BrandIntelligence holds scraped brand data from a domain.
type BrandIntelligence struct {
	Domain      string   `json:"domain"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	LogoURL     string   `json:"logo_url"`
	Colors      []string `json:"colors"`
	FontFamily  string   `json:"font_family"`
	BlogPosts   []BlogExcerpt `json:"blog_posts"`
}

// BlogExcerpt holds a scraped blog post summary.
type BlogExcerpt struct {
	Title    string `json:"title"`
	Excerpt  string `json:"excerpt"`
	URL      string `json:"url"`
	FullText string `json:"full_text,omitempty"`
}

// GenerateEmailTemplates produces 5 HTML email template variations using AI.
// Tries Anthropic (Claude) first, falls back to OpenAI (GPT-5.2).
func (s *AIContentService) GenerateEmailTemplates(ctx context.Context, req TemplateGenerationRequest) (*TemplateGenerationResult, error) {
	if s.anthropicKey == "" && s.openaiKey == "" {
		return nil, fmt.Errorf("no AI API key configured (set ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	}

	brand := s.scrapeBrandIntelligence(ctx, req.SendingDomain)
	prompt := buildTemplateGenerationPrompt(req.CampaignType, brand)
	if req.ReferenceHTML != "" {
		trimmed := req.ReferenceHTML
		if len(trimmed) > 4000 {
			trimmed = trimmed[:4000] + "\n...[truncated]"
		}
		prompt += "\n\n## REFERENCE TEMPLATE (use as style/layout inspiration — DO NOT copy verbatim)\n\nStudy this existing template's layout, color scheme, font choices, button styles, and overall design language. Your new templates should feel like they belong to the same brand family while being fresh and distinct.\n\n```html\n" + trimmed + "\n```"
	}

	var variations []GeneratedVariation
	var err error

	if s.anthropicKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			variations, err = s.callClaudeForTemplates(ctx, prompt)
			if err == nil && len(variations) > 0 {
				break
			}
			if attempt == 0 {
				log.Printf("Claude template generation attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
		if err != nil || len(variations) == 0 {
			log.Printf("Claude template generation failed after retries, falling back to OpenAI: %v", err)
		}
	}

	if len(variations) == 0 && s.openaiKey != "" {
		for attempt := 0; attempt < 2; attempt++ {
			variations, err = s.callOpenAIForTemplates(ctx, prompt)
			if err == nil && len(variations) > 0 {
				break
			}
			if attempt == 0 {
				log.Printf("OpenAI template generation attempt %d failed: %v — retrying", attempt+1, err)
				time.Sleep(2 * time.Second)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("template generation failed: %w", err)
		}
	}

	if len(variations) == 0 {
		return nil, fmt.Errorf("template generation produced no variations")
	}

	return &TemplateGenerationResult{
		Variations:  variations,
		BrandInfo:   brand,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// scrapeBrandIntelligence fetches a domain's homepage and extracts brand signals.
func (s *AIContentService) scrapeBrandIntelligence(ctx context.Context, domain string) *BrandIntelligence {
	brand := &BrandIntelligence{
		Domain: domain,
		Colors: []string{},
	}

	url := "https://" + domain
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		log.Printf("brand scrape: failed to build request for %s: %v", domain, err)
		return brand
	}
	req.Header.Set("User-Agent", "JarvisBot/1.0 (brand-intelligence)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("brand scrape: failed to fetch %s: %v", domain, err)
		return brand
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return brand
	}
	html := string(bodyBytes)

	brand.Title = extractMetaOrTag(html, "title")
	brand.Description = extractMeta(html, "description")
	brand.LogoURL = extractLogo(html, domain)
	brand.Colors = extractColors(html)
	brand.FontFamily = extractFontFamily(html)
	brand.BlogPosts = extractBlogExcerpts(html, domain)

	return brand
}

func extractMetaOrTag(html, tagName string) string {
	re := regexp.MustCompile(`(?i)<` + tagName + `[^>]*>(.*?)</` + tagName + `>`)
	m := re.FindStringSubmatch(html)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func extractMeta(html, name string) string {
	re := regexp.MustCompile(`(?i)<meta\s[^>]*name=["']` + name + `["'][^>]*content=["']([^"']*)["']`)
	m := re.FindStringSubmatch(html)
	if len(m) > 1 {
		return m[1]
	}
	re2 := regexp.MustCompile(`(?i)<meta\s[^>]*content=["']([^"']*)["'][^>]*name=["']` + name + `["']`)
	m2 := re2.FindStringSubmatch(html)
	if len(m2) > 1 {
		return m2[1]
	}
	return ""
}

func extractLogo(html, domain string) string {
	patterns := []string{
		`(?i)<link[^>]*rel=["'](?:icon|shortcut icon|apple-touch-icon)["'][^>]*href=["']([^"']+)["']`,
		`(?i)<img[^>]*(?:class|id|alt)=["'][^"']*logo[^"']*["'][^>]*src=["']([^"']+)["']`,
		`(?i)<img[^>]*src=["']([^"']*logo[^"']*)["']`,
	}
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		m := re.FindStringSubmatch(html)
		if len(m) > 1 {
			href := m[1]
			if strings.HasPrefix(href, "//") {
				return "https:" + href
			}
			if strings.HasPrefix(href, "/") {
				return "https://" + domain + href
			}
			if strings.HasPrefix(href, "http") {
				return href
			}
			return "https://" + domain + "/" + href
		}
	}
	return ""
}

func extractColors(html string) []string {
	colorSet := map[string]bool{}
	hexRe := regexp.MustCompile(`#[0-9a-fA-F]{6}`)
	matches := hexRe.FindAllString(html, 30)
	for _, c := range matches {
		c = strings.ToLower(c)
		if c != "#000000" && c != "#ffffff" && c != "#fff" {
			colorSet[c] = true
		}
	}
	var colors []string
	for c := range colorSet {
		colors = append(colors, c)
		if len(colors) >= 5 {
			break
		}
	}
	return colors
}

func extractFontFamily(html string) string {
	re := regexp.MustCompile(`(?i)font-family:\s*['"]?([^;'"}\n]+)`)
	m := re.FindStringSubmatch(html)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return "Arial, Helvetica, sans-serif"
}

func extractBlogExcerpts(html, domain string) []BlogExcerpt {
	var posts []BlogExcerpt
	articleRe := regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
	articles := articleRe.FindAllStringSubmatch(html, 5)
	for _, a := range articles {
		if len(a) < 2 {
			continue
		}
		title := extractMetaOrTag(a[1], "h2")
		if title == "" {
			title = extractMetaOrTag(a[1], "h3")
		}
		pRe := regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
		pMatch := pRe.FindStringSubmatch(a[1])
		excerpt := ""
		if len(pMatch) > 1 {
			tagRe := regexp.MustCompile(`<[^>]+>`)
			excerpt = strings.TrimSpace(tagRe.ReplaceAllString(pMatch[1], ""))
		}
		linkRe := regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
		linkMatch := linkRe.FindStringSubmatch(a[1])
		link := ""
		if len(linkMatch) > 1 {
			link = linkMatch[1]
			if strings.HasPrefix(link, "/") {
				link = "https://" + domain + link
			}
		}
		if title != "" {
			posts = append(posts, BlogExcerpt{Title: title, Excerpt: excerpt, URL: link})
		}
	}
	return posts
}

func buildTemplateGenerationPrompt(campaignType string, brand *BrandIntelligence) string {
	campaignDescriptions := map[string]string{
		"welcome":        "Welcome Series — a warm, introductory email for new subscribers. Introduce the brand, set expectations, highlight key value propositions, and include a clear CTA.",
		"winback":        "Win-Back Campaign — a re-engagement email for dormant subscribers. Acknowledge absence, showcase what they've missed, include an incentive or compelling reason to return.",
		"newsletter":     "Newsletter — a content-driven update email. Mix of blog highlights, industry insights, tips, and a curated feel. Should feel like valuable reading, not a sales pitch.",
		"promotional":    "Promotional Campaign — a conversion-focused email with a clear offer. Feature deals, discounts, or limited-time offers with urgency elements and strong CTAs.",
		"re-engagement":  "Re-Engagement Campaign — a gentle nudge for subscribers who haven't opened recently. Use curiosity, a special offer, or a 'we miss you' angle.",
		"announcement":   "Product/Feature Announcement — an exciting reveal of something new. Build anticipation, explain value clearly, and drive action.",
		"trivia":         "Trivia/Interactive Campaign — a fun, engaging email with trivia questions. Include 3-5 trivia questions with reveal answers, encouraging clicks and engagement.",
	}

	desc, ok := campaignDescriptions[strings.ToLower(campaignType)]
	if !ok {
		desc = "General marketing email campaign."
	}

	var sb strings.Builder

	sb.WriteString(`You are an expert email designer and copywriter. Generate 5 COMPLETE, production-ready HTML email templates.

CRITICAL RULES:
1. Each template MUST be a complete, self-contained HTML document with <!DOCTYPE html>, <html>, <head>, and <body> tags
2. Use TABLE-BASED LAYOUT for email client compatibility (Outlook, Gmail, Yahoo, Apple Mail)
3. All CSS must be INLINE — no <style> blocks, no external stylesheets
4. Maximum width: 600px, centered
5. Mobile responsive using a single-column layout that scales
6. Every image tag must have alt text
7. Return ONLY valid JSON — no markdown, no code fences, no explanation

`)

	sb.WriteString(fmt.Sprintf("CAMPAIGN TYPE: %s\n\n", desc))

	sb.WriteString("BRAND INTELLIGENCE:\n")
	sb.WriteString(fmt.Sprintf("- Domain: %s\n", brand.Domain))
	if brand.Title != "" {
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
	if len(brand.BlogPosts) > 0 {
		sb.WriteString("\nBLOG CONTENT FROM THE SITE (use in templates where relevant):\n")
		for _, p := range brand.BlogPosts {
			sb.WriteString(fmt.Sprintf("  - Title: %s\n", p.Title))
			if p.Excerpt != "" {
				sb.WriteString(fmt.Sprintf("    Excerpt: %s\n", p.Excerpt))
			}
			if p.URL != "" {
				sb.WriteString(fmt.Sprintf("    Link: %s\n", p.URL))
			}
		}
	}

	sb.WriteString(`

PERSONALIZATION (LIQUID MERGE TAGS):
Every template MUST include these Liquid merge tags for dynamic personalization:
- Use {{ first_name | default: "there" }} for greeting the subscriber by name
- Use {{ email }} where the subscriber's email should appear
- Use {{ system.unsubscribe_url }} for the mandatory unsubscribe link (CAN-SPAM required)
- Use {{ system.preferences_url }} for a manage-preferences link (optional but recommended)
- Use {% if custom.company %}{{ custom.company }}{% endif %} for conditional company name
- The unsubscribe link MUST appear in every template footer, e.g.: <a href="{{ system.unsubscribe_url }}">Unsubscribe</a>
- These tags MUST appear literally in the HTML — they get processed server-side at send time

DESIGN PHILOSOPHY:
- Take layout inspiration from award-winning email designs on reallygoodemails.com
- For Welcome Series: clean hero section, value pillars, warm personal tone
- For Newsletters: magazine-style with clear content blocks, editorial headers, read-more links
- For all types: strong visual hierarchy, generous white space, clear CTAs
- Keep creatives more TEXT FOCUSED — words should carry the design
- Voice: happy, encouraging, professional, concise, enticing, engaging, fun
- Use the brand's actual colors, font family, and logo in the template
- If blog content is provided, weave it naturally into the email body
- If it's a discount/promotional campaign, display deals prominently
- If it's a trivia campaign, display fun trivia questions with hidden answers (CSS hover or "click to reveal" CTA)

VARIATION STRATEGY:
Each of the 5 variations should differ meaningfully:
- Variation A: Classic editorial layout with hero image placeholder, clean sections
- Variation B: Bold, modern design with strong color blocks and typography-driven
- Variation C: Minimal and elegant — lots of white space, sophisticated feel
- Variation D: Warm and personal — conversational tone, feels like a letter from a friend
- Variation E: Dynamic and energetic — bright accents, multiple CTAs, action-oriented

FROM NAME: Derive from the brand name — use something like "Team [Brand]" or "[Brand] News" or a friendly human-sounding name that matches the brand voice.
SUBJECT LINE: Craft compelling, curiosity-driven subject lines tailored to the campaign type. Keep under 60 characters. Each variation should have a DIFFERENT subject line.

RESPOND WITH EXACTLY THIS JSON STRUCTURE (no wrapping, no markdown):
[
  {
    "variant_name": "A",
    "from_name": "...",
    "subject": "...",
    "html_content": "<!DOCTYPE html>..."
  },
  {
    "variant_name": "B",
    "from_name": "...",
    "subject": "...",
    "html_content": "<!DOCTYPE html>..."
  },
  {
    "variant_name": "C",
    "from_name": "...",
    "subject": "...",
    "html_content": "<!DOCTYPE html>..."
  },
  {
    "variant_name": "D",
    "from_name": "...",
    "subject": "...",
    "html_content": "<!DOCTYPE html>..."
  },
  {
    "variant_name": "E",
    "from_name": "...",
    "subject": "...",
    "html_content": "<!DOCTYPE html>..."
  }
]
`)

	return sb.String()
}

func (s *AIContentService) callClaudeForTemplates(ctx context.Context, prompt string) ([]GeneratedVariation, error) {
	reqBody := map[string]interface{}{
		"model":      "claude-opus-4-6",
		"max_tokens": 16000,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	body, _ := json.Marshal(reqBody)

	genCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(genCtx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", s.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("anthropic error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to parse anthropic response: %w", err)
	}
	if len(anthropicResp.Content) == 0 {
		return nil, fmt.Errorf("no content in anthropic response")
	}

	raw := strings.TrimSpace(anthropicResp.Content[0].Text)
	if strings.HasPrefix(raw, "```json") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	} else if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}

	raw = sanitizeAIJSON(raw)

	var variations []GeneratedVariation
	if err := json.Unmarshal([]byte(raw), &variations); err != nil {
		return nil, fmt.Errorf("failed to parse generated templates: %w (response length: %d)", err, len(raw))
	}

	return variations, nil
}

// callOpenAIForTemplates generates email template variations via the OpenAI chat completions API.
func (s *AIContentService) callOpenAIForTemplates(ctx context.Context, prompt string) ([]GeneratedVariation, error) {
	reqBody := map[string]interface{}{
		"model": "gpt-5.2",
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are an expert email designer and copywriter. Always respond with valid JSON only — no markdown, no code fences, no explanation.",
			},
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature":          0.7,
		"max_completion_tokens": 16000,
	}

	body, _ := json.Marshal(reqBody)

	genCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(genCtx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.openaiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("failed to parse openai response: %w", err)
	}
	if len(openAIResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in openai response")
	}

	raw := strings.TrimSpace(openAIResp.Choices[0].Message.Content)
	if strings.HasPrefix(raw, "```json") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	} else if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}

	raw = sanitizeAIJSON(raw)

	var variations []GeneratedVariation
	if err := json.Unmarshal([]byte(raw), &variations); err != nil {
		return nil, fmt.Errorf("failed to parse generated templates: %w (response length: %d)", err, len(raw))
	}

	return variations, nil
}

// callClaudeRaw sends a prompt to Claude and returns the raw text response.
func (s *AIContentService) callClaudeRaw(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]interface{}{
		"model":      "claude-opus-4-6",
		"max_tokens": 32000,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	body, _ := json.Marshal(reqBody)

	genCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(genCtx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", s.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return "", fmt.Errorf("failed to parse anthropic response: %w", err)
	}
	if len(anthropicResp.Content) == 0 {
		return "", fmt.Errorf("no content in anthropic response")
	}

	raw := strings.TrimSpace(anthropicResp.Content[0].Text)
	if strings.HasPrefix(raw, "```json") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	} else if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}

	return raw, nil
}

// callOpenAIRaw sends a prompt to OpenAI and returns the raw text response.
func (s *AIContentService) callOpenAIRaw(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]interface{}{
		"model": "gpt-5.2",
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are an expert email designer and deliverability engineer. Always respond with valid JSON only — no markdown, no code fences, no explanation.",
			},
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature":           0.8,
		"max_completion_tokens": 32000,
	}

	body, _ := json.Marshal(reqBody)

	genCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(genCtx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.openaiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("openai error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return "", fmt.Errorf("failed to parse openai response: %w", err)
	}
	if len(openAIResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in openai response")
	}

	raw := strings.TrimSpace(openAIResp.Choices[0].Message.Content)
	if strings.HasPrefix(raw, "```json") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	} else if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}

	return raw, nil
}

// ScrapeBrandIntelligence is the public accessor for brand intelligence scraping.
// It scrapes the homepage first, then tries common blog paths if not enough posts
// were found, then deep-scrapes the top articles for full body text.
func (s *AIContentService) ScrapeBrandIntelligence(ctx context.Context, domain string) *BrandIntelligence {
	brand := s.scrapeBrandIntelligence(ctx, domain)
	client := &http.Client{Timeout: 10 * time.Second}

	if len(brand.BlogPosts) < 3 {
		blogPaths := []string{"/blog", "/posts", "/articles", "/news"}
		for _, path := range blogPaths {
			blogURL := "https://" + domain + path
			req, err := http.NewRequestWithContext(ctx, "GET", blogURL, nil)
			if err != nil {
				continue
			}
			req.Header.Set("User-Agent", "JarvisBot/1.0 (brand-intelligence)")
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != 200 {
				if resp != nil {
					resp.Body.Close()
				}
				continue
			}
			bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
			resp.Body.Close()
			if err != nil {
				continue
			}
			html := string(bodyBytes)

			allLinks := extractAllBlogLinks(html, domain)
			posts := extractBlogExcerptsWithLinks(html, domain, allLinks)
			if len(posts) == 0 {
				posts = extractCardPostsWithLinks(html, domain, allLinks)
			}
			if len(posts) > len(brand.BlogPosts) {
				brand.BlogPosts = posts
				log.Printf("brand scrape: found %d posts from %s%s", len(posts), domain, path)
				break
			}
		}
	}

	deepScrapeCount := 5
	if len(brand.BlogPosts) < deepScrapeCount {
		deepScrapeCount = len(brand.BlogPosts)
	}
	scraped := 0
	for i := 0; i < len(brand.BlogPosts) && scraped < deepScrapeCount; i++ {
		if brand.BlogPosts[i].URL == "" {
			continue
		}
		fullText := scrapeFullArticle(ctx, client, brand.BlogPosts[i].URL)
		if fullText != "" {
			brand.BlogPosts[i].FullText = fullText
			scraped++
			log.Printf("brand scrape: deep-scraped %s (%d words)", brand.BlogPosts[i].URL, len(strings.Fields(fullText)))
		}
	}

	return brand
}

// extractAllBlogLinks collects every /blog/... href from a page and maps slug→full URL.
// This solves the problem where <a> tags wrap cards rather than appearing inside them.
func extractAllBlogLinks(html, domain string) map[string]string {
	linkRe := regexp.MustCompile(`(?i)href=["'](/(?:blog|posts|articles)/[^"'#?]+)["']`)
	matches := linkRe.FindAllStringSubmatch(html, 100)
	links := map[string]string{}
	for _, m := range matches {
		path := m[1]
		if strings.Contains(path, "/category/") || strings.Contains(path, "/tag/") {
			continue
		}
		slug := path[strings.LastIndex(path, "/")+1:]
		links[slug] = "https://" + domain + path
	}
	return links
}

// matchTitleToLink tries to find a URL for a blog post title by matching slugified
// words against the collected link slugs.
func matchTitleToLink(title string, linkMap map[string]string) string {
	titleLower := strings.ToLower(title)
	for slug, url := range linkMap {
		slugLower := strings.ToLower(strings.ReplaceAll(slug, "-", " "))
		words := strings.Fields(slugLower)
		if len(words) < 2 {
			continue
		}
		matchCount := 0
		for _, w := range words {
			if len(w) > 3 && strings.Contains(titleLower, w) {
				matchCount++
			}
		}
		if matchCount >= len(words)/2 && matchCount >= 2 {
			return url
		}
	}
	return ""
}

// extractBlogExcerptsWithLinks is like extractBlogExcerpts but uses the global
// link map to resolve URLs when the <article> itself doesn't contain an href.
func extractBlogExcerptsWithLinks(html, domain string, linkMap map[string]string) []BlogExcerpt {
	posts := extractBlogExcerpts(html, domain)
	for i := range posts {
		if posts[i].URL == "" {
			posts[i].URL = matchTitleToLink(posts[i].Title, linkMap)
		}
	}
	return posts
}

// extractCardPostsWithLinks is like extractCardPosts but uses the global link
// map to resolve URLs when cards don't contain direct hrefs.
func extractCardPostsWithLinks(html, domain string, linkMap map[string]string) []BlogExcerpt {
	var posts []BlogExcerpt
	seen := map[string]bool{}

	cardRe := regexp.MustCompile(`(?is)<(?:article|div)[^>]*class="[^"]*card[^"]*"[^>]*>(.*?)</(?:article|div)>`)
	cards := cardRe.FindAllStringSubmatch(html, 20)
	for _, c := range cards {
		if len(c) < 2 {
			continue
		}
		title := extractMetaOrTag(c[1], "h3")
		if title == "" {
			title = extractMetaOrTag(c[1], "h2")
		}
		if title == "" || seen[title] {
			continue
		}
		seen[title] = true

		pRe := regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
		pMatch := pRe.FindStringSubmatch(c[1])
		excerpt := ""
		if len(pMatch) > 1 {
			tagRe := regexp.MustCompile(`<[^>]+>`)
			excerpt = strings.TrimSpace(tagRe.ReplaceAllString(pMatch[1], ""))
		}

		link := matchTitleToLink(title, linkMap)

		posts = append(posts, BlogExcerpt{Title: title, Excerpt: excerpt, URL: link})
		if len(posts) >= 10 {
			break
		}
	}
	return posts
}

// scrapeFullArticle fetches a blog post URL, extracts the article body text
// (filtering out sidebar/widget noise), and returns ~800 words.
func scrapeFullArticle(ctx context.Context, client *http.Client, articleURL string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", articleURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "JarvisBot/1.0 (brand-intelligence)")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return ""
	}
	html := string(bodyBytes)

	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = reScript.ReplaceAllString(html, "")
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = reStyle.ReplaceAllString(html, "")
	reNav := regexp.MustCompile(`(?is)<(?:nav|header|footer|aside)[^>]*>.*?</(?:nav|header|footer|aside)>`)
	html = reNav.ReplaceAllString(html, "")

	articleRe := regexp.MustCompile(`(?is)<article[^>]*>(.*?)</article>`)
	articleMatch := articleRe.FindStringSubmatch(html)
	if len(articleMatch) > 1 {
		html = articleMatch[1]
	}

	tagRe := regexp.MustCompile(`<[^>]+>`)
	decodeHTML := func(text string) string {
		text = strings.ReplaceAll(text, "&amp;", "&")
		text = strings.ReplaceAll(text, "&lt;", "<")
		text = strings.ReplaceAll(text, "&gt;", ">")
		text = strings.ReplaceAll(text, "&quot;", `"`)
		text = strings.ReplaceAll(text, "&#39;", "'")
		text = strings.ReplaceAll(text, "&#x27;", "'")
		text = strings.ReplaceAll(text, "&nbsp;", " ")
		return text
	}

	priceRe := regexp.MustCompile(`\$\d+\.\d{2}`)

	var paragraphs []string
	pRe := regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
	pMatches := pRe.FindAllStringSubmatch(html, 80)
	for _, m := range pMatches {
		text := strings.TrimSpace(tagRe.ReplaceAllString(m[1], ""))
		text = decodeHTML(text)
		if len(text) < 40 {
			continue
		}
		if priceRe.FindString(text) != "" && len(priceRe.FindAllString(text, -1)) >= 2 {
			continue
		}
		if strings.Contains(text, "Free Shipping") || strings.Contains(text, "w/ S&S") || strings.Contains(text, "reg $") {
			continue
		}
		paragraphs = append(paragraphs, text)
	}

	fullText := strings.Join(paragraphs, "\n\n")

	words := strings.Fields(fullText)
	if len(words) > 800 {
		fullText = strings.Join(words[:800], " ") + "..."
	}

	return fullText
}

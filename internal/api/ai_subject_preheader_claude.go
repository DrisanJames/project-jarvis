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
)

type subjectPreheaderRequest struct {
	SendingDomain string `json:"sending_domain"`
	HTMLContent   string `json:"html_content"`
	CampaignType  string `json:"campaign_type"`
	Count         int    `json:"count"`
}

type subjectPreheaderSuggestion struct {
	Subject     string `json:"subject"`
	PreviewText string `json:"preview_text"`
	Reasoning   string `json:"reasoning"`
	Category    string `json:"category"`
}

type subjectPreheaderResponse struct {
	Suggestions []subjectPreheaderSuggestion `json:"suggestions"`
	Brand       string                       `json:"brand"`
	GeneratedAt string                       `json:"generated_at"`
}

// HandleSuggestSubjectPreheader returns an http.HandlerFunc that uses Claude
// to generate brand-aware subject line + preheader pairs.
func HandleSuggestSubjectPreheader(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req subjectPreheaderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if req.SendingDomain == "" {
			respondError(w, http.StatusBadRequest, "sending_domain is required")
			return
		}
		if req.Count <= 0 || req.Count > 10 {
			req.Count = 3
		}

		kit, brandFound := GetBrandKitBySendingDomain(req.SendingDomain)

		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			log.Println("[AI-SubjectPreheader] ANTHROPIC_API_KEY not set, returning fallback suggestions")
			respondJSON(w, http.StatusOK, subjectPreheaderResponse{
				Suggestions: fallbackSubjectPreheaderSuggestions(kit, brandFound),
				Brand:       kit.SiteName,
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		systemPrompt := buildSubjectPreheaderSystemPrompt(kit, brandFound)
		userPrompt := buildSubjectPreheaderUserPrompt(req, kit, brandFound)

		log.Printf("[AI-SubjectPreheader] calling Claude for domain=%s brand=%s count=%d", req.SendingDomain, kit.SiteName, req.Count)

		suggestions, err := callClaudeForSubjectPreheader(apiKey, systemPrompt, userPrompt)
		if err != nil {
			log.Printf("[AI-SubjectPreheader] Claude call failed: %v — returning fallback", err)
			respondJSON(w, http.StatusOK, subjectPreheaderResponse{
				Suggestions: fallbackSubjectPreheaderSuggestions(kit, brandFound),
				Brand:       kit.SiteName,
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		if req.Count < len(suggestions) {
			suggestions = suggestions[:req.Count]
		}

		respondJSON(w, http.StatusOK, subjectPreheaderResponse{
			Suggestions: suggestions,
			Brand:       kit.SiteName,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
}

func buildSubjectPreheaderSystemPrompt(kit BrandKit, brandFound bool) string {
	if !brandFound {
		return `You are an expert email marketing copywriter who specializes in crafting high-performing subject lines and preheader text. You write subject lines that get opened and preheaders that reinforce the open. You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation — output ONLY the raw JSON array.`
	}

	return fmt.Sprintf(`You are the head email copywriter for %s (%s — "%s").

Brand voice: %s

You specialize in crafting subject lines and preheader text that feel authentic to the %s brand. Every subject line you write gets opened because it sounds like a real person the subscriber trusts, not a marketing robot.

Rules:
- Subject lines are 40–70 characters. Shorter is better.
- Preheader text is 50–120 characters. It COMPLEMENTS the subject — never repeats it.
- Together, subject + preheader tell a mini-story visible in the inbox.
- Use the brand voice above in every word choice.

You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation — output ONLY the raw JSON array.`,
		kit.SiteName, kit.SiteDomain, kit.Tagline, kit.Voice, kit.SiteName)
}

func buildSubjectPreheaderUserPrompt(req subjectPreheaderRequest, kit BrandKit, brandFound bool) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Generate %d paired subject line + preheader suggestions.\n\n", req.Count))

	if req.CampaignType != "" {
		sb.WriteString(fmt.Sprintf("CAMPAIGN TYPE: %s\n\n", req.CampaignType))
	}

	if req.HTMLContent != "" {
		summary := extractContentSummary(req.HTMLContent, 800)
		if summary != "" {
			sb.WriteString("EMAIL BODY CONTENT SUMMARY (base your suggestions on this):\n")
			sb.WriteString(summary)
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString(`AVAILABLE PERSONALIZATION TAGS (Liquid syntax — use naturally, don't force all):
- {{ first_name }} — subscriber's first name
- {{ first_name | default: "there" }} — first name with fallback
- {{ last_name }} — subscriber's last name
- {{ email }} — subscriber's email address
- {{ system.current_year }} — current year (e.g. 2026)
- Conditional: {%- if first_name -%}Hi {{ first_name }},{%- else -%}Hey there,{%- endif -%}

CONSTRAINTS:
- Subject lines: 40–70 characters, compelling, authentic to brand voice
- Preheader: 50–120 characters, complements subject, adds value/curiosity
- At least one suggestion should use a personalization tag
- Vary the approach: mix curiosity, urgency, benefit, question, and personalized categories
- Do NOT use generic filler — every word must earn its place

`)

	sb.WriteString(`RETURN THIS JSON ARRAY (no wrapping object, just the array):
[
  {
    "subject": "compelling subject line here",
    "preview_text": "complementary preheader text here",
    "reasoning": "brief explanation of the approach",
    "category": "one of: curiosity, urgency, benefit, personalized, question"
  }
]

Generate now:`)

	return sb.String()
}

func callClaudeForSubjectPreheader(apiKey, systemPrompt, userPrompt string) ([]subjectPreheaderSuggestion, error) {
	reqBody := map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 2000,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	httpReq, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("request creation error: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		truncated := string(body)
		if len(truncated) > 500 {
			truncated = truncated[:500]
		}
		return nil, fmt.Errorf("Anthropic returned %d: %s", resp.StatusCode, truncated)
	}

	var aiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}
	if len(aiResp.Content) == 0 {
		return nil, fmt.Errorf("empty AI response")
	}

	raw := strings.TrimSpace(aiResp.Content[0].Text)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	raw = sanitizeAIJSONString(raw)

	var suggestions []subjectPreheaderSuggestion
	if err := json.Unmarshal([]byte(raw), &suggestions); err != nil {
		return nil, fmt.Errorf("failed to parse suggestions JSON: %w (raw: %.300s)", err, raw)
	}

	return suggestions, nil
}

func fallbackSubjectPreheaderSuggestions(kit BrandKit, brandFound bool) []subjectPreheaderSuggestion {
	if brandFound {
		return []subjectPreheaderSuggestion{
			{
				Subject:     fmt.Sprintf("{{ first_name | default: \"Hey\" }}, new from %s", kit.SiteName),
				PreviewText: fmt.Sprintf("Fresh content from %s — take a look.", kit.SiteName),
				Reasoning:   "Personalized greeting with brand name recognition",
				Category:    "personalized",
			},
			{
				Subject:     fmt.Sprintf("You won't want to miss this, {{ first_name }}"),
				PreviewText: fmt.Sprintf("Something special from the %s team.", kit.SiteName),
				Reasoning:   "Curiosity-driven open with personalization",
				Category:    "curiosity",
			},
			{
				Subject:     fmt.Sprintf("This week's must-read from %s", kit.SiteName),
				PreviewText: "We picked the best — so you don't have to.",
				Reasoning:   "Value proposition with curated content angle",
				Category:    "benefit",
			},
		}
	}

	return []subjectPreheaderSuggestion{
		{
			Subject:     "{{ first_name | default: \"Hey\" }}, check this out",
			PreviewText: "We put this together just for you.",
			Reasoning:   "Simple personalization with casual tone",
			Category:    "personalized",
		},
		{
			Subject:     "You'll want to see this, {{ first_name }}",
			PreviewText: "Open for something worth your time.",
			Reasoning:   "Curiosity with personalization",
			Category:    "curiosity",
		},
		{
			Subject:     "Quick — this won't last long",
			PreviewText: "{{ first_name }}, act before it's gone.",
			Reasoning:   "Urgency messaging to drive immediate opens",
			Category:    "urgency",
		},
	}
}

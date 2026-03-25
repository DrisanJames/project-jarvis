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
)

// ---------------------------------------------------------------------------
// Rich brand copy profiles — detailed Voice + Audience + style guidance
// sourced from the ContentBrand registrations in main.go
// ---------------------------------------------------------------------------

type brandCopyProfile struct {
	Voice        string
	Audience     string
	SubjectStyle string
}

var brandCopyProfiles = map[string]brandCopyProfile{
	"discountblog": {
		Voice: `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. First-person storytelling. You say things like "My wife and I tried this..." and "Here's what actually worked for our family." Pull specific numbers from the articles — dollar amounts, percentages, timeframes. Never generic. Every sentence should teach or reveal something useful. Tone: warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate. Think: personal finance blog meets friendly text message.`,
		Audience: `Budget-conscious families, young professionals figuring out adulting, and savvy deal hunters. They're busy, skeptical of hype, and want actionable advice — not listicles. They read Discount Blog because it feels real, not manufactured.`,
		SubjectStyle: "First-person, conversational. Pull specific dollar amounts and store names when the email content mentions them. Think personal finance text from a friend, not a marketing email. 'I saved $47 at Target this week' beats 'Great deals inside!'",
	},
	"quizfiesta": {
		Voice: `You are the voice of QuizFiesta — a retro-arcade trivia platform. Write like an arcade machine that gained sentience and got really into hyping people up. Short punchy sentences. Direct challenges to the reader. Arcade/gaming lingo: "INSERT COIN", "GAME OVER", "player", "high score", "streak", "level up." Competitive but encouraging — you want them to play, not feel bad. Use ALL CAPS sparingly for emphasis on key words (one per paragraph max). Think: the text on an arcade cabinet's attract screen mixed with a friend trash-talking you at game night.`,
		Audience: `Trivia lovers, casual gamers, friend groups who want game night content, and competitive players chasing leaderboard spots. They're on their phone, they have 2 minutes, and you need to get them excited enough to tap PLAY.`,
		SubjectStyle: "Short, punchy, competitive. Use direct challenges and gaming lingo. One ALL CAPS word max. 'Think you can beat 94%?' beats 'New trivia available'. Subject should feel like an arcade attract screen.",
	},
	"historythinking": {
		Voice: `You are the voice of History Thinking — a scholarly yet accessible history publication. Write like a brilliant professor who tells stories at dinner parties that leave everyone speechless. Every article must surprise: reveal hidden connections, debunk myths, or reframe well-known events. Use vivid period detail and primary sources. Tone: authoritative but never dry, passionate about making history feel alive and relevant. Think: a documentary narrator who can't help but lean in when the story gets good.`,
		Audience: `History enthusiasts who want surprising, well-researched stories that go beyond textbook summaries. They're curious, educated, and love "I didn't know that" moments.`,
		SubjectStyle: "Lead with the most surprising historical fact or reframe. Use specific dates, names, and consequences. 'The letter that started WWI was 3 sentences' beats 'History you need to know'. Tone: authoritative intrigue, like a documentary title card.",
	},
	"myownhealth": {
		Voice: `You are the voice of My Own Health — a no-nonsense, evidence-based health publication. Write like a sports medicine doctor who also lifts. Direct, actionable, zero fluff. Every claim needs a mechanism: don't just say "drink water," explain what dehydration does to cortisol and recovery. Use specific numbers: grams, reps, minutes, studies. Challenge bro-science and wellness hype with actual data. Tone: confident, slightly irreverent, respects the reader's intelligence. Think: a coach who reads PubMed for fun.`,
		Audience: `Health-conscious adults who want actionable tips without fluff. They're tired of vague "eat clean, train hard" advice and want specific protocols backed by evidence.`,
		SubjectStyle: "Lead with a specific number, mechanism, or myth-bust. '3g creatine daily: the study results' beats 'Health tips inside'. Challenge conventional wisdom. Tone: confident coach who cites sources.",
	},
}

// ---------------------------------------------------------------------------
// Request / Response types
// ---------------------------------------------------------------------------

type subjectPreheaderRequest struct {
	SendingDomain string `json:"sending_domain"`
	HTMLContent   string `json:"html_content"`
	CampaignType  string `json:"campaign_type"`
	Count         int    `json:"count"`
}

type subjectPreheaderSuggestion struct {
	Subject       string `json:"subject"`
	PlainSubject  string `json:"plain_subject"`
	PreviewText   string `json:"preview_text"`
	Reasoning     string `json:"reasoning"`
	Category      string `json:"category"`
	SubjectLength int    `json:"subject_length"`
}

type subjectPreheaderResponse struct {
	Suggestions []subjectPreheaderSuggestion `json:"suggestions"`
	Brand       string                       `json:"brand"`
	GeneratedAt string                       `json:"generated_at"`
}

// ---------------------------------------------------------------------------
// Content signal extraction — structured parsing of HTML email content
// ---------------------------------------------------------------------------

type contentSignals struct {
	Headlines []string
	CTAs      []string
	Links     []string
	TextBody  string
}

var (
	reHeading = regexp.MustCompile(`(?i)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	reAnchor  = regexp.MustCompile(`(?i)<a[^>]*>(.*?)</a>`)
	reHref    = regexp.MustCompile(`(?i)href="([^"]+)"`)
	reTag     = regexp.MustCompile(`<[^>]+>`)
	reSpaces  = regexp.MustCompile(`\s+`)
)

func extractContentSignals(html string) contentSignals {
	var cs contentSignals

	for _, match := range reHeading.FindAllStringSubmatch(html, 20) {
		text := strings.TrimSpace(reTag.ReplaceAllString(match[1], ""))
		if text != "" && len(text) > 3 {
			cs.Headlines = append(cs.Headlines, text)
		}
	}

	for _, match := range reAnchor.FindAllStringSubmatch(html, 30) {
		fullTag := match[0]
		linkText := strings.TrimSpace(reTag.ReplaceAllString(match[1], ""))

		isButton := strings.Contains(strings.ToLower(fullTag), "button") ||
			strings.Contains(strings.ToLower(fullTag), "btn") ||
			strings.Contains(strings.ToLower(fullTag), "cta") ||
			strings.Contains(strings.ToLower(fullTag), "background-color") ||
			strings.Contains(strings.ToLower(fullTag), "border-radius")

		if isButton && linkText != "" && len(linkText) > 2 && len(linkText) < 60 {
			cs.CTAs = append(cs.CTAs, linkText)
		}

		hrefMatch := reHref.FindStringSubmatch(fullTag)
		if hrefMatch != nil && !strings.Contains(hrefMatch[1], "unsubscribe") && !strings.HasPrefix(hrefMatch[1], "{{") {
			if linkText != "" && len(linkText) > 3 && len(linkText) < 80 {
				cs.Links = append(cs.Links, linkText)
			}
		}
	}

	body := reTag.ReplaceAllString(html, " ")
	body = strings.ReplaceAll(body, "&nbsp;", " ")
	body = strings.ReplaceAll(body, "&amp;", "&")
	body = reSpaces.ReplaceAllString(body, " ")
	body = strings.TrimSpace(body)
	if len(body) > 1200 {
		body = body[:1200]
	}
	cs.TextBody = body

	return cs
}

// ---------------------------------------------------------------------------
// Performance data from DB
// ---------------------------------------------------------------------------

type domainPerfData struct {
	TopSubjects []struct {
		Subject  string
		OpenRate float64
	}
	Patterns []struct {
		PatternType string
		AvgOpenRate float64
		SampleSize  int
	}
}

func fetchDomainPerformance(ctx context.Context, db *sql.DB, sendingDomain string) domainPerfData {
	var data domainPerfData

	rows, err := db.QueryContext(ctx, `
		SELECT subject,
			CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END as open_rate
		FROM mailing_campaigns
		WHERE status = 'completed'
			AND sent_count >= 50
			AND created_at > NOW() - INTERVAL '60 days'
			AND subject IS NOT NULL AND subject != ''
			AND sending_domain = $1
		ORDER BY (open_count::float / NULLIF(sent_count, 0)) DESC
		LIMIT 5`, sendingDomain)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var s struct {
				Subject  string
				OpenRate float64
			}
			if rows.Scan(&s.Subject, &s.OpenRate) == nil {
				data.TopSubjects = append(data.TopSubjects, s)
			}
		}
	}

	rows2, err := db.QueryContext(ctx, `
		SELECT
			CASE
				WHEN subject ILIKE '%?%' THEN 'Question-based'
				WHEN subject ILIKE '%save%' OR subject ILIKE '%deal%' OR subject ILIKE '%%off%' THEN 'Deal/savings'
				WHEN subject ILIKE '%last%' OR subject ILIKE '%hurry%' OR subject ILIKE '%ending%' OR subject ILIKE '%miss%' THEN 'Urgency'
				WHEN subject ~* '\d' THEN 'Contains numbers'
				ELSE 'Standard'
			END as pattern_type,
			AVG(CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END) as avg_open_rate,
			COUNT(*) as sample_size
		FROM mailing_campaigns
		WHERE status = 'completed' AND sent_count >= 50
			AND created_at > NOW() - INTERVAL '60 days'
			AND sending_domain = $1
		GROUP BY 1 HAVING COUNT(*) >= 2
		ORDER BY avg_open_rate DESC
		LIMIT 5`, sendingDomain)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var p struct {
				PatternType string
				AvgOpenRate float64
				SampleSize  int
			}
			if rows2.Scan(&p.PatternType, &p.AvgOpenRate, &p.SampleSize) == nil {
				data.Patterns = append(data.Patterns, p)
			}
		}
	}

	return data
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

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
		profile := brandCopyProfiles[kit.Key]

		ctx := r.Context()
		perfData := fetchDomainPerformance(ctx, db, req.SendingDomain)

		var signals contentSignals
		if req.HTMLContent != "" {
			signals = extractContentSignals(req.HTMLContent)
		}

		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			log.Println("[AI-SubjectPreheader] ANTHROPIC_API_KEY not set, returning fallback suggestions")
			respondJSON(w, http.StatusOK, subjectPreheaderResponse{
				Suggestions: enrichSuggestions(fallbackSubjectPreheaderSuggestions(kit, brandFound)),
				Brand:       kit.SiteName,
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		systemPrompt := buildSubjectPreheaderSystemPrompt(kit, brandFound, profile)
		userPrompt := buildSubjectPreheaderUserPrompt(req, kit, brandFound, profile, signals, perfData)

		log.Printf("[AI-SubjectPreheader] calling Claude for domain=%s brand=%s count=%d headlines=%d perf_subjects=%d",
			req.SendingDomain, kit.SiteName, req.Count, len(signals.Headlines), len(perfData.TopSubjects))

		suggestions, err := callClaudeForSubjectPreheader(apiKey, systemPrompt, userPrompt)
		if err != nil {
			log.Printf("[AI-SubjectPreheader] Claude call failed: %v — returning fallback", err)
			respondJSON(w, http.StatusOK, subjectPreheaderResponse{
				Suggestions: enrichSuggestions(fallbackSubjectPreheaderSuggestions(kit, brandFound)),
				Brand:       kit.SiteName,
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		if req.Count < len(suggestions) {
			suggestions = suggestions[:req.Count]
		}

		respondJSON(w, http.StatusOK, subjectPreheaderResponse{
			Suggestions: enrichSuggestions(suggestions),
			Brand:       kit.SiteName,
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// ---------------------------------------------------------------------------
// Post-processing: add PlainSubject + SubjectLength
// ---------------------------------------------------------------------------

func enrichSuggestions(suggestions []subjectPreheaderSuggestion) []subjectPreheaderSuggestion {
	for i := range suggestions {
		suggestions[i].PlainSubject = stripMergeTags(suggestions[i].Subject)
		suggestions[i].SubjectLength = len(suggestions[i].PlainSubject)
	}
	return suggestions
}

// ---------------------------------------------------------------------------
// System prompt — expert-level email marketing knowledge
// ---------------------------------------------------------------------------

func buildSubjectPreheaderSystemPrompt(kit BrandKit, brandFound bool, profile brandCopyProfile) string {
	expertRules := `
EMAIL SUBJECT LINE SCIENCE (follow strictly):
- Subject lines with 6–10 words have the highest open rates. Target under 50 characters to avoid truncation on mobile.
- Personalized subject lines (using subscriber's first name) increase open rates by 26%.
- 47% of recipients decide to open based on the subject line ALONE. 69% mark emails as spam based on it.
- The subject line must stand on its own — Apple Intelligence now generates AI summaries that can REPLACE preview text for iOS 18+ users.

PREHEADER / PREVIEW TEXT SCIENCE (follow strictly):
- Preview text is the subject line's SIDEKICK — together they should read like one flowing thought, like "peanut butter and jelly."
- Keep preview text under 90 characters. Some clients show as few as 40 characters.
- NEVER repeat the subject line in the preview text. Extend it, add context, create curiosity, or include a soft CTA.
- Preview text should add VALUE the subject line couldn't fit: a specific number, a tease, a personal touch, or a deadline.

CATEGORY STRATEGIES (use one per suggestion):
- CURIOSITY: Be intentionally intriguing. Cliffhangers, open loops, "the secret to..." — make them NEED to open. 
- URGENCY: Scarcity drives action. Deadlines, limited quantities, "last chance" — but be honest, never fake.
- BENEFIT: Lead with what's in it for THEM. Solve a problem, save money/time, teach something useful.
- PERSONALIZED: Use {{ first_name }} naturally. Make it feel like a 1:1 message, not a blast.
- QUESTION: Questions create an open loop the brain wants to close. Use genuine questions tied to content.
- SOCIAL_PROOF: Numbers, results, what others are doing. "12,000 readers loved this" or specific stats.

SPAM AVOIDANCE (hard rules — violating these kills deliverability):
- NEVER use ALL CAPS for entire words (one strategic word is OK for some brands).
- NEVER use multiple exclamation marks (!!!). One is the absolute maximum.
- NEVER use spam trigger phrases: "Act now", "Buy now", "Free", "Save $", "Click here", "Limited time offer", "No obligation", "Winner", "Congratulations".
- NEVER use excessive punctuation or special characters in subject lines.
- Avoid starting with "Re:" or "Fwd:" to fake a reply thread.

OUTPUT FORMAT (strict):
You MUST return valid JSON matching the exact schema provided. No markdown fences, no explanation, no preamble — output ONLY the raw JSON array.`

	if !brandFound {
		return fmt.Sprintf(`You are a senior email marketing copywriter who has written subject lines for 500+ campaigns with an average 28%% open rate. You understand inbox psychology, mobile rendering, and modern AI email summarization.

%s`, expertRules)
	}

	voice := kit.Voice
	if profile.Voice != "" {
		voice = profile.Voice
	}

	var brandSection strings.Builder
	fmt.Fprintf(&brandSection, "BRAND: %s (%s)\nTAGLINE: \"%s\"\nSENDING DOMAIN: %s\n\n", kit.SiteName, kit.SiteDomain, kit.Tagline, kit.SendingDomain)
	fmt.Fprintf(&brandSection, "BRAND VOICE:\n%s\n\n", voice)
	if profile.Audience != "" {
		fmt.Fprintf(&brandSection, "TARGET AUDIENCE:\n%s\n\n", profile.Audience)
	}
	if profile.SubjectStyle != "" {
		fmt.Fprintf(&brandSection, "SUBJECT LINE STYLE FOR THIS BRAND:\n%s\n\n", profile.SubjectStyle)
	}

	return fmt.Sprintf(`You are the head email copywriter for %s. You have written every subject line this brand has ever sent. You know the voice intimately — every word must sound like it came from the brand, not from a marketing tool.

%s
%s`, kit.SiteName, brandSection.String(), expertRules)
}

// ---------------------------------------------------------------------------
// User prompt — content signals, performance data, campaign guidance
// ---------------------------------------------------------------------------

var campaignTypeGuidance = map[string]string{
	"welcome":       "This is a WELCOME email for brand-new subscribers. The subject must prove immediate value — they're skeptical and one tap from unsubscribing. Lead with the strongest hook from the content. Preview text should reinforce 'you made the right choice.'",
	"newsletter":    "This is a NEWSLETTER with editorial content. Tease the most interesting piece — make them curious enough to open. Use content-specific hooks, not generic 'this week's update' language. Preview text should spotlight a secondary article or surprising fact.",
	"promotional":   "This is a PROMOTIONAL email with an offer. Lead with the most compelling number (discount %, dollar amount, savings). Preview text adds urgency or specifics the subject couldn't fit. Avoid sounding like spam — be specific, not generic.",
	"winback":       "This is a WIN-BACK email for dormant subscribers. Acknowledge the absence without guilt-tripping. Lead with what they've MISSED that's genuinely good. Preview text should create FOMO with a specific piece of content or deal.",
	"re-engagement": "This is a RE-ENGAGEMENT email — a gentle nudge. Soft, personal tone. Question-based subjects work well. Preview text should hint at value without pressure.",
	"announcement":  "This is an ANNOUNCEMENT email — something new. Lead with the news itself, not a tease. Be direct and exciting. Preview text adds the 'so what' — why should they care?",
	"trivia":        "This is a TRIVIA / INTERACTIVE email. Challenge the reader directly. Use competitive language. Preview text should include a specific question or stat that makes them want to prove themselves.",
}

func buildSubjectPreheaderUserPrompt(req subjectPreheaderRequest, kit BrandKit, brandFound bool, profile brandCopyProfile, signals contentSignals, perf domainPerfData) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Generate %d paired subject line + preheader suggestions for this specific email.\n\n", req.Count))

	// Campaign type guidance
	if req.CampaignType != "" {
		sb.WriteString(fmt.Sprintf("CAMPAIGN TYPE: %s\n", req.CampaignType))
		if guidance, ok := campaignTypeGuidance[req.CampaignType]; ok {
			sb.WriteString(guidance)
			sb.WriteString("\n\n")
		} else {
			sb.WriteString("\n")
		}
	}

	// Content signals from HTML
	if len(signals.Headlines) > 0 {
		sb.WriteString("ARTICLE HEADLINES / SECTIONS IN THIS EMAIL:\n")
		for i, h := range signals.Headlines {
			if i >= 8 {
				break
			}
			sb.WriteString(fmt.Sprintf("  - %s\n", h))
		}
		sb.WriteString("\n")
	}

	if len(signals.CTAs) > 0 {
		sb.WriteString("CTA BUTTONS IN THIS EMAIL:\n")
		for i, c := range signals.CTAs {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("  - %s\n", c))
		}
		sb.WriteString("\n")
	}

	if len(signals.Links) > 0 {
		sb.WriteString("KEY LINK TEXT IN THIS EMAIL:\n")
		seen := map[string]bool{}
		count := 0
		for _, l := range signals.Links {
			lower := strings.ToLower(l)
			if seen[lower] || count >= 6 {
				continue
			}
			seen[lower] = true
			sb.WriteString(fmt.Sprintf("  - %s\n", l))
			count++
		}
		sb.WriteString("\n")
	}

	if signals.TextBody != "" {
		body := signals.TextBody
		if len(body) > 600 {
			body = body[:600] + "..."
		}
		sb.WriteString("EMAIL BODY TEXT (summarized):\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}

	// Performance data
	if len(perf.TopSubjects) > 0 {
		sb.WriteString("=== REAL PERFORMANCE DATA FOR THIS DOMAIN ===\n\n")
		sb.WriteString("TOP-PERFORMING SUBJECT LINES (last 60 days):\n")
		for _, ts := range perf.TopSubjects {
			sb.WriteString(fmt.Sprintf("  - \"%s\" (%.1f%% open rate)\n", ts.Subject, ts.OpenRate))
		}
		sb.WriteString("\nStudy these. Mirror their tone, structure, and specificity. Do NOT copy them verbatim.\n\n")
	}

	if len(perf.Patterns) > 0 {
		sb.WriteString("WHAT PATTERNS WORK FOR THIS BRAND:\n")
		for _, p := range perf.Patterns {
			sb.WriteString(fmt.Sprintf("  - %s: %.1f%% avg open rate (%d campaigns)\n", p.PatternType, p.AvgOpenRate, p.SampleSize))
		}
		sb.WriteString("\nWeight your suggestions toward the higher-performing patterns.\n\n")

		if len(perf.TopSubjects) > 0 {
			sb.WriteString("=== END PERFORMANCE DATA ===\n\n")
		}
	}

	// Personalization tags
	sb.WriteString(`AVAILABLE PERSONALIZATION TAGS (Liquid syntax — use naturally, don't force all into every suggestion):
- {{ first_name }} — subscriber's first name
- {{ first_name | default: "there" }} — first name with safe fallback
- {{ last_name }} — subscriber's last name  
- {{ email }} — subscriber's email address
- {{ system.current_year }} — current year (2026)
- Conditional block: {%- if first_name -%}Hi {{ first_name }},{%- else -%}Hey there,{%- endif -%}

CRITICAL CONSTRAINTS:
- Subject: 6–10 words, under 50 characters when possible. Absolute max 60.
- Preheader: 40–90 characters. Must COMPLEMENT (not repeat) the subject.
- Subject + preheader together should read like one coherent inbox message.
- At least one suggestion MUST use {{ first_name }} personalization.
- At least one suggestion MUST reference specific content from the email body above.
- Each suggestion MUST use a DIFFERENT category strategy.
- Every word must earn its place — no filler, no generic marketing speak.
- Sound like the brand voice, not like an AI.

`)

	sb.WriteString(`RETURN THIS JSON ARRAY (no wrapping object, no markdown fences, just the raw array):
[
  {
    "subject": "the actual subject line",
    "preview_text": "the actual preheader text",
    "reasoning": "1–2 sentence explanation of the strategy and why it works for this brand",
    "category": "one of: curiosity, urgency, benefit, personalized, question, social_proof"
  }
]

Generate now:`)

	return sb.String()
}

// ---------------------------------------------------------------------------
// Claude API call
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Fallback suggestions (when API key missing or Claude fails)
// ---------------------------------------------------------------------------

func fallbackSubjectPreheaderSuggestions(kit BrandKit, brandFound bool) []subjectPreheaderSuggestion {
	if brandFound {
		return []subjectPreheaderSuggestion{
			{
				Subject:     fmt.Sprintf("{%% if first_name %%}{{ first_name }}, new{%% else %%}New{%% endif %%} from %s", kit.SiteName),
				PreviewText: fmt.Sprintf("Fresh content worth your time — take a look."),
				Reasoning:   "Personalized greeting with brand recognition. Conditional avoids empty name rendering.",
				Category:    "personalized",
			},
			{
				Subject:     "You won't want to miss this one",
				PreviewText: fmt.Sprintf("{{ first_name | default: \"Hey\" }}, the %s team picked this for you.", kit.SiteName),
				Reasoning:   "Curiosity-driven subject with personalized preheader that adds warmth.",
				Category:    "curiosity",
			},
			{
				Subject:     fmt.Sprintf("This week's best from %s", kit.SiteName),
				PreviewText: "We curated the top picks so you don't have to search.",
				Reasoning:   "Benefit-forward curation angle. Saves them time — clear value proposition.",
				Category:    "benefit",
			},
		}
	}

	return []subjectPreheaderSuggestion{
		{
			Subject:     "{{ first_name | default: \"Hey\" }}, this is for you",
			PreviewText: "We put something together you'll actually want to read.",
			Reasoning:   "Personalization with casual, non-corporate tone.",
			Category:    "personalized",
		},
		{
			Subject:     "Worth 2 minutes of your day",
			PreviewText: "{{ first_name }}, open for something that'll stick with you.",
			Reasoning:   "Specific time commitment reduces friction. Curiosity in preheader.",
			Category:    "curiosity",
		},
		{
			Subject:     "Before your inbox buries this...",
			PreviewText: "{{ first_name }}, this one's time-sensitive.",
			Reasoning:   "Gentle urgency without spam triggers. Acknowledges inbox reality.",
			Category:    "urgency",
		},
	}
}

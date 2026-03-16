package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// brandConfig holds per-brand settings for wave content generation.
type brandConfig struct {
	Key              string // short key for diagnostics (e.g., "discountblog", "quizfiesta")
	SendingDomain    string
	SESFromDomain    string
	BlogDomain       string
	BrandName        string
	FromName         string
	CampaignType     string
	Voice            string
	Audience         string
	DesignSystem     string
	HTMLTemplate     string
	FallbackContent  []mailing.BlogExcerpt
}

// knownBrands returns the configured brand roster.
func knownBrands() map[string]brandConfig {
	return map[string]brandConfig{
		"discountblog": {
			Key:           "discountblog",
			SendingDomain: "em.discountblog.com",
			SESFromDomain: "m.discountblog.com",
			BlogDomain:    "discountblog.com",
			BrandName:     "Discount Blog",
			FromName:      "Jamie @ Discount Blog",
			CampaignType:  "newsletter",
			HTMLTemplate: DiscountBlogHTMLTemplate,
			Voice: `You are writing as "Jamie" from Discount Blog — a relatable, practical person who genuinely loves saving money and sharing what works. First-person storytelling. You say things like "My wife and I tried this..." and "Here's what actually worked for our family." Pull specific numbers from the articles — dollar amounts, percentages, timeframes. Never generic. Every sentence should teach or reveal something useful. Tone: warm, honest, slightly conspiratorial (like sharing a secret with a friend). NOT salesy, NOT clickbaity, NOT corporate. Think: personal finance blog meets friendly text message.`,
			Audience: `Budget-conscious families, young professionals figuring out adulting, and savvy deal hunters. They're busy, skeptical of hype, and want actionable advice — not listicles. They read Discount Blog because it feels real, not manufactured.`,
			DesignSystem: `DISCOUNT BLOG EMAIL DESIGN SYSTEM — follow these rules EXACTLY:

COLORS:
- Background: #FFFFFF (clean white)
- Card backgrounds: #FAFAFA with 1px solid #E5E7EB border
- Primary accent (logo, CTAs, category badges): #FF6B6B (coral/salmon)
- Secondary accent (highlights, "NEW" badges): #4ECDC4 (teal)
- Headline text: #1A1A1A (near-black)
- Body text: #6B7280 (warm gray)
- Price/savings callouts: #EF4444 (red) for sale price, #9CA3AF (gray strikethrough) for original
- CTA button: background #FF6B6B, text #FFFFFF, border-radius 6px, padding 12px 24px
- Footer background: #F9FAFB, text #9CA3AF

TYPOGRAPHY:
- Headlines: Georgia, "Times New Roman", serif — gives editorial credibility
- Body: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif
- Headline sizes: H1=28px, H2=22px, H3=18px — all in #1A1A1A
- Body: 15px line-height 1.6 — generous, readable
- Category badges: 11px uppercase, #FF6B6B on #FFF0F0 background, border-radius 12px, padding 3px 10px

LAYOUT RULES:
- Max width: 600px, centered on #F3F4F6 outer background
- Article sections: white card with subtle border, 20px padding, 12px margin-bottom
- Header: "discount blog" in lowercase, 24px, #FF6B6B, Georgia font — NOT a logo image
- Each article block: category badge on top, then headline (serif, bold), then 2-3 sentence summary, then CTA link in coral
- Article CTA style: text link with arrow "Read the guide →" in #FF6B6B, NOT big buttons (except one primary CTA)
- Footer: brand name, domain, unsubscribe link — simple and clean, not cluttered
- Generous whitespace between sections (24-32px)
- NO heavy imagery — this is a text-forward, editorial newsletter
- Subtle top border on each article card: 3px solid #FF6B6B for featured, #E5E7EB for others`,
		},
		"quizfiesta": {
			Key:           "quizfiesta",
			SendingDomain: "em.quizfiesta.com",
			SESFromDomain: "m.quizfiesta.com",
			BlogDomain:    "quizfiesta.com",
			BrandName:     "QuizFiesta",
			FromName:      "QuizFiesta Team",
			CampaignType:  "trivia",
			HTMLTemplate: QuizFiestaHTMLTemplate,
			Voice: `You are the voice of QuizFiesta — a retro-arcade trivia platform. Write like an arcade machine that gained sentience and got really into hyping people up. Short punchy sentences. Direct challenges to the reader. Arcade/gaming lingo: "INSERT COIN", "GAME OVER", "player", "high score", "streak", "level up." Competitive but encouraging — you want them to play, not feel bad. Use ALL CAPS sparingly for emphasis on key words (one per paragraph max). Think: the text on an arcade cabinet's attract screen mixed with a friend trash-talking you at game night.`,
			Audience: `Trivia lovers, casual gamers, friend groups who want game night content, and competitive players chasing leaderboard spots. They're on their phone, they have 2 minutes, and you need to get them excited enough to tap PLAY.`,
			DesignSystem: `QUIZFIESTA EMAIL DESIGN SYSTEM — follow these rules EXACTLY:

COLORS:
- Background: #0A0014 (deep purple-black) — NOT pure black, NOT white
- Card backgrounds: #1A0A2E (dark purple) with 1px solid #8B5CF6 (purple) border
- Primary accent: #8B5CF6 (purple) — borders, section dividers
- Neon green: #39FF14 — scores, streaks, important numbers, "PLAY" CTAs
- Gold/yellow: #FFD700 — "INSERT COIN" text, special callouts
- Cyan: #38BDF8 — secondary highlights, links
- Hot pink: #D946EF — accent bars, badges
- Text primary: #FFFFFF (white headlines)
- Text secondary: #A0A0B0 (gray body text)
- CTA button: background #8B5CF6, text #FFFFFF, border 2px solid #39FF14, padding 14px 28px
- Secondary CTA: background transparent, border 2px solid #8B5CF6, text #39FF14

TYPOGRAPHY:
- ALL text: "Courier New", Courier, monospace — this is non-negotiable, the whole email uses monospace
- Headlines: 26px, #FFFFFF, uppercase, letter-spacing 2px
- Body: 14px, #A0A0B0, line-height 1.5
- CTAs: 14px, uppercase, letter-spacing 3px, bold
- Score/stat numbers: 32px, #39FF14, bold

LAYOUT RULES:
- Max width: 600px, centered on #000000 outer background
- Header: "QUIZ FIESTA" in monospace, 20px, #FFFFFF, letter-spacing 4px, with thin #8B5CF6 bottom border
- Section dividers: 1px solid #8B5CF6 with slight purple glow effect (box-shadow: 0 0 5px #8B5CF660)
- Game mode cards: #1A0A2E background, 1px #8B5CF6 border, 16px padding
- Each game mode: emoji icon + mode name in #FFFFFF uppercase + description in #A0A0B0 + CTA
- Leaderboard sections: green monospace text (#39FF14) on #0A0014 background — terminal style
- Footer: "© 2026 PLAYER_ONE // ALL RIGHTS RESERVED" style, #666666, monospace
- Add "INSERT COIN TO CONTINUE" as CTA text or section separator
- Use ═══ or ─── monospace line separators between sections
- NO rounded corners anywhere — sharp edges only (border-radius: 0)
- NO serif fonts, NO friendly warmth — this is an arcade, not a coffee shop`,
			FallbackContent: []mailing.BlogExcerpt{
				{Title: "Classic Mode — Test Your Knowledge", Excerpt: "15 questions. 30 seconds each. Streak multipliers and adaptive difficulty. How high can you score? The all-time leaderboard awaits.", URL: "https://quizfiesta.com/play"},
				{Title: "Survival Mode — One Wrong Answer and It's Over", Excerpt: "3 lives. Questions get harder the longer you last. Stack streak bonuses for massive multipliers. Current record: 47 questions. Think you can beat it?", URL: "https://quizfiesta.com/play"},
				{Title: "Speed Run — 30 Seconds. Go.", Excerpt: "The clock is ticking. Answer as many as you can before time runs out. Every correct answer adds to your score. Every second wasted is points lost.", URL: "https://quizfiesta.com/play"},
				{Title: "AI Challenge — A.P.E.X. Is Watching", Excerpt: "Race the machine through 20 levels. Our AI adapts to your skill in real-time. It learns your weaknesses. Can you outsmart something that's studying you?", URL: "https://quizfiesta.com/play"},
				{Title: "Multiplayer Duels — Settle It in Real-Time", Excerpt: "Share a room code. Go head-to-head. Real-time trivia battles where every millisecond of response time matters. Bragging rights guaranteed.", URL: "https://quizfiesta.com/play"},
				{Title: "Party Mode — Up to 8 Players. Total Chaos.", Excerpt: "Create a room. Share the code. Jackbox-style trivia with friends, family, or strangers. Perfect for game night when someone says 'I'm bored.'", URL: "https://quizfiesta.com/play"},
				{Title: "Weekly Leaderboard — The Arcade's Finest", Excerpt: "New leaderboard resets every Monday. Current top 3 are averaging 94% accuracy. Where do you rank among the arcade's finest?", URL: "https://quizfiesta.com/leaderboard"},
			},
		},
	}
}

// HandleWaveContentTest generates structurally unique email content per wave
// using AI, then deploys a test campaign. Each brand is isolated — its own
// scrape, its own prompt, its own generation cycle.
//
// Query params:
//
//	mode=prompt   → scrapes blog, builds prompt, returns it for review
//	mode=generate → scrapes blog, generates via AI, deploys campaign
//	brand         → "discountblog", "quizfiesta", or "all" (default: all)
//	email         → test recipient (default: drisanjames@gmail.com)
//	waves         → number of waves per brand (default: 3)
//	send          → "scheduler" (default) or "direct" (bypass scheduler, send via SES)
func (s *PMTACampaignService) HandleWaveContentTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "prompt"
	}
	brandFilter := r.URL.Query().Get("brand")
	if brandFilter == "" {
		brandFilter = "all"
	}
	testEmail := r.URL.Query().Get("email")
	if testEmail == "" {
		testEmail = "drisanjames@gmail.com"
	}
	sendMode := r.URL.Query().Get("send")
	if sendMode == "" {
		sendMode = "direct"
	}
	numWaves := 3
	if nw := r.URL.Query().Get("waves"); nw != "" {
		if v, err := strconv.Atoi(nw); err == nil && v > 0 && v <= 10 {
			numWaves = v
		}
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		for _, cfgPath := range []string{"config/config.yaml", "/app/config/config.yaml"} {
			if cfg, err := config.Load(cfgPath); err == nil && cfg.OpenAI.APIKey != "" {
				openaiKey = cfg.OpenAI.APIKey
				log.Printf("[wave-content-test] loaded OpenAI key from %s", cfgPath)
				break
			}
		}
	}
	if anthropicKey == "" && openaiKey == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no AI API key configured (need ANTHROPIC_API_KEY or OPENAI_API_KEY env var, or openai.api_key in config.yaml)",
		})
		return
	}

	aiSvc := mailing.NewAIContentService(s.db, anthropicKey, openaiKey)
	waveGen := mailing.NewWaveContentGenerator(aiSvc)

	brands := knownBrands()
	var selectedBrands []brandConfig

	if brandFilter == "all" {
		for _, b := range brands {
			selectedBrands = append(selectedBrands, b)
		}
	} else {
		b, ok := brands[brandFilter]
		if !ok {
			respondJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown brand %q — use discountblog, quizfiesta, or all", brandFilter),
			})
			return
		}
		selectedBrands = []brandConfig{b}
	}

	if mode == "prompt" {
		var brandPrompts []map[string]interface{}

		for _, b := range selectedBrands {
			log.Printf("[wave-content-test] scraping brand intelligence from %s", b.BlogDomain)
			brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)

			contentPool := brand.BlogPosts
			if len(contentPool) < 3 && len(b.FallbackContent) > 0 {
				contentPool = b.FallbackContent
			}

			req := mailing.WaveContentRequest{
				SendingDomain: b.SendingDomain,
				BrandName:     b.BrandName,
				NumWaves:      numWaves,
				CampaignType:  b.CampaignType,
				Voice:         b.Voice,
				Audience:      b.Audience,
				DesignSystem:  b.DesignSystem,
				HTMLTemplate:  b.HTMLTemplate,
				BrandInfo:     brand,
				ContentPool:   contentPool,
			}
			prompt := waveGen.BuildPrompt(req)

			brandPrompts = append(brandPrompts, map[string]interface{}{
				"brand_name":          b.BrandName,
				"from_name":          b.FromName,
				"sending_domain":     b.SendingDomain,
				"blog_domain":        b.BlogDomain,
				"campaign_type":      b.CampaignType,
				"prompt":             prompt,
				"prompt_length_chars": len(prompt),
				"brand_info":         brand,
			})
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"mode":       "prompt",
			"brands":     brandPrompts,
			"num_waves":  numWaves,
			"test_email": testEmail,
			"note":       "Review each brand's prompt above. When ready, call with mode=generate to create and deploy.",
		})
		return
	}

	if mode != "generate" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "mode must be 'prompt' or 'generate'",
		})
		return
	}

	var allBrandResults []map[string]interface{}

	for _, b := range selectedBrands {
		scrapeStart := time.Now()
		log.Printf("[wave-content-test] scraping brand intelligence from %s", b.BlogDomain)
		brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)

		contentPool := brand.BlogPosts
		usedFallback := false
		if len(contentPool) < 3 && len(b.FallbackContent) > 0 {
			log.Printf("[wave-content-test] %s: scraped %d posts, using %d fallback content items", b.BrandName, len(contentPool), len(b.FallbackContent))
			contentPool = b.FallbackContent
			usedFallback = true
		}
		scrapeReport := mailing.BuildScrapeReport(b.BlogDomain, brand, usedFallback, time.Since(scrapeStart))

		req := mailing.WaveContentRequest{
			SendingDomain: b.SendingDomain,
			BrandName:     b.BrandName,
			NumWaves:      numWaves,
			CampaignType:  b.CampaignType,
			Voice:         b.Voice,
			Audience:      b.Audience,
			DesignSystem:  b.DesignSystem,
			HTMLTemplate:  b.HTMLTemplate,
			BrandInfo:     brand,
			ContentPool:   contentPool,
		}

		log.Printf("[wave-content-test] generating %d wave variations for %s via AI (content pool: %d items)...", numWaves, b.BrandName, len(contentPool))
		variations, report, err := waveGen.Generate(ctx, req)
		if report != nil {
			report.ScrapeReport = scrapeReport
		}
		if err != nil {
			allBrandResults = append(allBrandResults, map[string]interface{}{
				"brand":      b.BrandName,
				"error":      fmt.Sprintf("AI generation failed: %v", err),
				"diagnostics": report,
			})
			continue
		}

		// Validate each wave against brand template
		var waveReports []mailing.WaveReport
		for _, v := range variations {
			wr := mailing.ValidateWave(v, b.Key, contentPool)
			waveReports = append(waveReports, wr)
		}
		if report != nil {
			report.WaveReports = waveReports
			mailing.LogReport(report)
		}

		var waveResults []map[string]interface{}
		if sendMode == "direct" {
			results := sendWavesDirect(ctx, b, variations, numWaves, testEmail)
			waveResults = results
		} else {
			results := s.scheduleWaves(ctx, b, variations, numWaves, testEmail, orgID)
			waveResults = results
		}

		allBrandResults = append(allBrandResults, map[string]interface{}{
			"brand":       b.BrandName,
			"from_name":   b.FromName,
			"domain":      b.SendingDomain,
			"send_mode":   sendMode,
			"waves":       waveResults,
			"brand_info":  brand,
			"diagnostics": report,
		})
	}

	totalWaves := numWaves * len(selectedBrands)
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"message":         fmt.Sprintf("Deployed %d wave campaigns across %d brands (send=%s)", totalWaves, len(selectedBrands), sendMode),
		"test_email":      testEmail,
		"total_campaigns": totalWaves,
		"send_mode":       sendMode,
		"brands":          allBrandResults,
	})
}

// HandleWaveContentCache returns cached pre-generated wave content for review.
func (s *PMTACampaignService) HandleWaveContentCache(w http.ResponseWriter, r *http.Request) {
	brandKey := r.URL.Query().Get("brand")
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if v, err := strconv.Atoi(limitStr); err == nil && v > 0 && v <= 100 {
		limit = v
	}

	query := `
		SELECT id, brand_key, wave_index, subject, preview_text, from_name,
		       LENGTH(html_content) as html_size, diagnostics, generated_at, used_at, version
		FROM mailing_wave_content_cache
	`
	var args []interface{}
	if brandKey != "" {
		query += " WHERE brand_key = $1"
		args = append(args, brandKey)
		query += " ORDER BY generated_at DESC LIMIT $2"
		args = append(args, limit)
	} else {
		query += " ORDER BY generated_at DESC LIMIT $1"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	var entries []map[string]interface{}
	for rows.Next() {
		var (
			id          int
			bKey        string
			waveIndex   int
			subject     string
			previewText string
			fromName    string
			htmlSize    int
			diagnostics sql.NullString
			generatedAt time.Time
			usedAt      sql.NullTime
			version     string
		)
		if err := rows.Scan(&id, &bKey, &waveIndex, &subject, &previewText, &fromName, &htmlSize, &diagnostics, &generatedAt, &usedAt, &version); err != nil {
			continue
		}
		entry := map[string]interface{}{
			"id":           id,
			"brand_key":    bKey,
			"wave_index":   waveIndex,
			"subject":      subject,
			"preview_text": previewText,
			"from_name":    fromName,
			"html_size":    htmlSize,
			"generated_at": generatedAt.Format(time.RFC3339),
			"version":      version,
			"used":         usedAt.Valid,
		}
		if diagnostics.Valid {
			var diag interface{}
			if json.Unmarshal([]byte(diagnostics.String), &diag) == nil {
				entry["diagnostics"] = diag
			}
		}
		entries = append(entries, entry)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"count":   len(entries),
		"entries": entries,
	})
}

// HandleRefreshWaveCache triggers an immediate content generation cycle for one
// or all brands and stores the results in mailing_wave_content_cache. Use this
// to pre-populate the cache before campaigns fire.
//
// Query params:
//
//	brand  — "discountblog", "quizfiesta", or "all" (default: all)
//	waves  — number of waves to generate per brand (default: 5)
func (s *PMTACampaignService) HandleRefreshWaveCache(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	brandFilter := r.URL.Query().Get("brand")
	if brandFilter == "" {
		brandFilter = "all"
	}
	numWaves := 5
	if nw := r.URL.Query().Get("waves"); nw != "" {
		if v, err := strconv.Atoi(nw); err == nil && v > 0 && v <= 10 {
			numWaves = v
		}
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		for _, cfgPath := range []string{"config/config.yaml", "/app/config/config.yaml"} {
			if cfg, err := config.Load(cfgPath); err == nil && cfg.OpenAI.APIKey != "" {
				openaiKey = cfg.OpenAI.APIKey
				break
			}
		}
	}
	if anthropicKey == "" && openaiKey == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "no AI API key configured",
		})
		return
	}

	aiSvc := mailing.NewAIContentService(s.db, anthropicKey, openaiKey)
	waveGen := mailing.NewWaveContentGenerator(aiSvc)
	brands := knownBrands()

	var selectedBrands []brandConfig
	if brandFilter == "all" {
		for _, b := range brands {
			selectedBrands = append(selectedBrands, b)
		}
	} else {
		b, ok := brands[brandFilter]
		if !ok {
			respondJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown brand %q", brandFilter),
			})
			return
		}
		selectedBrands = []brandConfig{b}
	}

	var results []map[string]interface{}
	totalStored := 0

	for _, b := range selectedBrands {
		log.Printf("[wave-cache-refresh] generating %d waves for %s", numWaves, b.BrandName)
		brand := aiSvc.ScrapeBrandIntelligence(ctx, b.BlogDomain)
		contentPool := brand.BlogPosts
		if len(contentPool) < 3 && len(b.FallbackContent) > 0 {
			contentPool = b.FallbackContent
		}

		req := mailing.WaveContentRequest{
			SendingDomain: b.SendingDomain,
			BrandName:     b.BrandName,
			NumWaves:      numWaves,
			CampaignType:  b.CampaignType,
			Voice:         b.Voice,
			Audience:      b.Audience,
			DesignSystem:  b.DesignSystem,
			HTMLTemplate:  b.HTMLTemplate,
			BrandInfo:     brand,
			ContentPool:   contentPool,
		}

		variations, report, err := waveGen.Generate(ctx, req)
		if err != nil {
			results = append(results, map[string]interface{}{
				"brand": b.BrandName,
				"error": err.Error(),
			})
			continue
		}

		stored := 0
		for _, v := range variations {
			wr := mailing.ValidateWave(v, b.Key, contentPool)
			diagJSON, _ := json.Marshal(map[string]interface{}{
				"wave_report":    wr,
				"template_match": wr.TemplateMatch,
				"version":        mailing.GeneratorVersion,
			})

			_, dbErr := s.db.ExecContext(ctx, `
				INSERT INTO mailing_wave_content_cache
					(brand_key, wave_index, subject, preview_text, from_name, html_content, diagnostics, version)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, b.Key, v.WaveIndex, v.Subject, v.PreviewText, v.FromName, v.HTMLContent, diagJSON, mailing.GeneratorVersion)
			if dbErr != nil {
				log.Printf("[wave-cache-refresh] failed to store wave %d for %s: %v", v.WaveIndex, b.BrandName, dbErr)
				continue
			}
			stored++
		}
		totalStored += stored

		entry := map[string]interface{}{
			"brand":   b.BrandName,
			"stored":  stored,
			"total":   len(variations),
		}
		if report != nil {
			entry["editorial_model"] = report.Phase1Report.Model
			entry["html_model"] = report.Phase2Report.Model
			entry["duration_ms"] = report.TotalDurationMs
		}
		results = append(results, entry)
		log.Printf("[wave-cache-refresh] %s: stored %d/%d waves", b.BrandName, stored, len(variations))
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":      fmt.Sprintf("Generated and cached %d waves across %d brands", totalStored, len(selectedBrands)),
		"total_stored": totalStored,
		"brands":       results,
	})
}

// HandleDeployCachedWaves pulls pre-generated content from the wave cache and
// creates real PMTA ISP wave campaigns targeting the specified list. This is the
// production path for daily warmup sends.
//
// Query params:
//
//	brand     — "discountblog", "quizfiesta", or "all" (required)
//	list_id   — target subscriber list UUID (required)
//	waves     — number of waves to deploy per brand (default: 2)
//	max       — max recipients per campaign (default: 250)
//	stagger   — minutes between campaign start times (default: 30)
func (s *PMTACampaignService) HandleDeployCachedWaves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := s.orgID

	brandFilter := r.URL.Query().Get("brand")
	if brandFilter == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "brand is required (discountblog, quizfiesta, or all)"})
		return
	}
	listID := r.URL.Query().Get("list_id")
	if listID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "list_id is required"})
		return
	}
	numWaves := 2
	if nw := r.URL.Query().Get("waves"); nw != "" {
		if v, err := strconv.Atoi(nw); err == nil && v > 0 && v <= 5 {
			numWaves = v
		}
	}
	maxRecip := 250
	if mr := r.URL.Query().Get("max"); mr != "" {
		if v, err := strconv.Atoi(mr); err == nil && v > 0 {
			maxRecip = v
		}
	}
	staggerMin := 30
	if sm := r.URL.Query().Get("stagger"); sm != "" {
		if v, err := strconv.Atoi(sm); err == nil && v > 0 {
			staggerMin = v
		}
	}

	brandKeys := []string{}
	if brandFilter == "all" {
		brandKeys = []string{"discountblog", "quizfiesta"}
	} else {
		brandKeys = []string{brandFilter}
	}

	brands := knownBrands()

	var allResults []map[string]interface{}
	campaignIdx := 0

	for _, bk := range brandKeys {
		b, ok := brands[bk]
		if !ok {
			allResults = append(allResults, map[string]interface{}{"brand": bk, "error": "unknown brand"})
			continue
		}

		// Pull unused waves from cache
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, wave_index, subject, preview_text, from_name, html_content
			FROM mailing_wave_content_cache
			WHERE brand_key = $1 AND used_at IS NULL AND version = $2
			ORDER BY generated_at DESC
			LIMIT $3
		`, bk, mailing.GeneratorVersion, numWaves)
		if err != nil {
			allResults = append(allResults, map[string]interface{}{"brand": bk, "error": err.Error()})
			continue
		}

		type cachedWave struct {
			cacheID     int
			waveIndex   int
			subject     string
			previewText string
			fromName    string
			htmlContent string
		}
		var waves []cachedWave
		for rows.Next() {
			var cw cachedWave
			if rows.Scan(&cw.cacheID, &cw.waveIndex, &cw.subject, &cw.previewText, &cw.fromName, &cw.htmlContent) == nil {
				waves = append(waves, cw)
			}
		}
		rows.Close()

		if len(waves) == 0 {
			allResults = append(allResults, map[string]interface{}{"brand": bk, "error": "no unused cached waves found"})
			continue
		}

		for _, cw := range waves {
			scheduledAt := time.Now().Add(time.Duration(campaignIdx*staggerMin) * time.Minute)
			campaignName := fmt.Sprintf("%s Warmup Wave %d - %s", b.BrandName, cw.waveIndex+1, scheduledAt.Format("2006-01-02"))

			// Find sending profile
			var profileID sql.NullString
			s.db.QueryRowContext(ctx, `
				SELECT id FROM mailing_sending_profiles
				WHERE organization_id = $1 AND vendor_type = 'pmta'
				  AND (sending_domain = $2 OR from_email LIKE '%%@' || $2)
				  AND status = 'active'
				ORDER BY created_at DESC LIMIT 1
			`, orgID, b.SendingDomain).Scan(&profileID)

			campaignID := uuid.New().String()
			espQuotas, _ := json.Marshal(map[string]interface{}{
				"target_isps": []map[string]string{
					{"name": "Gmail", "domain": "gmail.com"},
					{"name": "Yahoo", "domain": "yahoo.com"},
					{"name": "Microsoft", "domain": "outlook.com"},
				},
				"sending_domain":    b.SendingDomain,
				"max_recipients":    maxRecip,
				"cadence_minutes":   10,
			})
			listIDs, _ := json.Marshal([]string{listID})

			_, insertErr := s.db.ExecContext(ctx, `
				INSERT INTO mailing_campaigns (
					id, organization_id, name, status, scheduled_at,
					from_name, from_email, subject, preview_text, html_content,
					sending_profile_id, esp_quotas, list_id, list_ids,
					execution_mode, send_type, total_recipients, max_recipients,
					created_at, updated_at
				) VALUES (
					$1, $2, $3, 'scheduled', $4,
					$5, $6, $7, $8, $9,
					$10, $11, $12, $13,
					'standard', 'blast', $14, $14,
					NOW(), NOW()
				)
			`, campaignID, orgID, campaignName, scheduledAt,
				cw.fromName, fmt.Sprintf("newsletter@%s", b.SendingDomain), cw.subject, cw.previewText, cw.htmlContent,
				profileID, string(espQuotas), listID, string(listIDs),
				maxRecip,
			)

			status := "scheduled"
			if insertErr != nil {
				status = "error: " + insertErr.Error()
				log.Printf("[deploy-cached] %s insert error: %v", campaignName, insertErr)
			} else {
				// Mark cache entry as used
				s.db.ExecContext(ctx, `UPDATE mailing_wave_content_cache SET used_at = NOW() WHERE id = $1`, cw.cacheID)
				log.Printf("[deploy-cached] created campaign %s (%s) scheduled for %s", campaignName, campaignID, scheduledAt.Format(time.RFC3339))
			}

			allResults = append(allResults, map[string]interface{}{
				"brand":         bk,
				"campaign_id":   campaignID,
				"campaign_name": campaignName,
				"scheduled_at":  scheduledAt.Format(time.RFC3339),
				"subject":       cw.subject,
				"preview_text":  cw.previewText,
				"html_size":     len(cw.htmlContent),
				"max_recipients": maxRecip,
				"status":        status,
			})
			campaignIdx++
		}
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"message":    fmt.Sprintf("Deployed %d campaigns", len(allResults)),
		"campaigns":  allResults,
	})
}

// HandlePipelineHealth returns a comprehensive view of lists, segments,
// suppression, and campaign queue status for operational review.
func (s *PMTACampaignService) HandlePipelineHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result := map[string]interface{}{}

	// --- Lists ---
	listRows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.name, COALESCE(l.status, 'active'),
		       COALESCE(l.subscriber_count, 0), COALESCE(l.active_count, 0),
		       COALESCE(l.mailed_to, 0), l.last_refreshed_at,
		       (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = l.id) AS live_count,
		       (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = l.id AND status IN ('active','confirmed')) AS live_active
		FROM mailing_lists l
		WHERE l.organization_id = $1
		ORDER BY l.name
	`, s.orgID)
	if err != nil {
		result["lists_error"] = err.Error()
	} else {
		defer listRows.Close()
		var lists []map[string]interface{}
		for listRows.Next() {
			var id, name, status string
			var subCount, activeCount, mailedTo int
			var lastRefreshed sql.NullTime
			var liveCount, liveActive int
			if listRows.Scan(&id, &name, &status, &subCount, &activeCount, &mailedTo, &lastRefreshed, &liveCount, &liveActive) != nil {
				continue
			}
			entry := map[string]interface{}{
				"id": id, "name": name, "status": status,
				"cached_subscriber_count": subCount,
				"cached_active_count":     activeCount,
				"cached_mailed_to":        mailedTo,
				"live_total_count":        liveCount,
				"live_active_count":       liveActive,
				"counts_match":            subCount == liveCount && activeCount == liveActive,
			}
			if lastRefreshed.Valid {
				entry["last_refreshed_at"] = lastRefreshed.Time.Format(time.RFC3339)
			}
			lists = append(lists, entry)
		}
		result["lists"] = lists
	}

	// --- Segments ---
	segRows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.segment_type, COALESCE(s.subscriber_count, 0),
		       s.last_calculated_at, s.status
		FROM mailing_segments s
		WHERE s.organization_id = $1
		ORDER BY s.name
	`, s.orgID)
	if err != nil {
		result["segments_error"] = err.Error()
	} else {
		defer segRows.Close()
		var segments []map[string]interface{}
		for segRows.Next() {
			var id, name, segType, status string
			var subCount int
			var lastCalc sql.NullTime
			if segRows.Scan(&id, &name, &segType, &subCount, &lastCalc, &status) != nil {
				continue
			}
			entry := map[string]interface{}{
				"id": id, "name": name, "type": segType,
				"subscriber_count": subCount, "status": status,
			}
			if lastCalc.Valid {
				entry["last_calculated_at"] = lastCalc.Time.Format(time.RFC3339)
				entry["age_minutes"] = int(time.Since(lastCalc.Time).Minutes())
			}
			segments = append(segments, entry)
		}
		result["segments"] = segments
	}

	// --- Suppression stats ---
	var globalSuppressionCount int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1`, s.orgID).Scan(&globalSuppressionCount)

	var recentSuppressions int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 AND created_at >= NOW() - INTERVAL '24 hours'`, s.orgID).Scan(&recentSuppressions)

	var suppressionByReason []map[string]interface{}
	reasonRows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(reason, 'unknown'), COUNT(*)
		FROM mailing_global_suppressions
		WHERE organization_id = $1
		GROUP BY reason ORDER BY COUNT(*) DESC LIMIT 10
	`, s.orgID)
	if err == nil {
		defer reasonRows.Close()
		for reasonRows.Next() {
			var reason string
			var count int
			if reasonRows.Scan(&reason, &count) == nil {
				suppressionByReason = append(suppressionByReason, map[string]interface{}{"reason": reason, "count": count})
			}
		}
	}
	result["suppression"] = map[string]interface{}{
		"global_total":   globalSuppressionCount,
		"last_24h":       recentSuppressions,
		"by_reason":      suppressionByReason,
	}

	// --- Campaign queue ---
	var queueStats []map[string]interface{}
	qRows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM mailing_campaign_queue
		GROUP BY status ORDER BY COUNT(*) DESC
	`)
	if err == nil {
		defer qRows.Close()
		for qRows.Next() {
			var status string
			var count int
			if qRows.Scan(&status, &count) == nil {
				queueStats = append(queueStats, map[string]interface{}{"status": status, "count": count})
			}
		}
	}
	result["queue"] = queueStats

	// --- Recent campaigns ---
	var recentCampaigns []map[string]interface{}
	cRows, err := s.db.QueryContext(ctx, `
		SELECT id, name, status, COALESCE(sent_count, 0), COALESCE(total_recipients, 0),
		       COALESCE(execution_mode, 'standard'), scheduled_at
		FROM mailing_campaigns
		WHERE organization_id = $1
		ORDER BY COALESCE(scheduled_at, created_at) DESC LIMIT 10
	`, s.orgID)
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var id, name, status, execMode string
			var sentCount, totalRecip int
			var scheduledAt sql.NullTime
			if cRows.Scan(&id, &name, &status, &sentCount, &totalRecip, &execMode, &scheduledAt) != nil {
				continue
			}
			entry := map[string]interface{}{
				"id": id, "name": name, "status": status,
				"sent_count": sentCount, "total_recipients": totalRecip,
				"execution_mode": execMode,
			}
			if scheduledAt.Valid {
				entry["scheduled_at"] = scheduledAt.Time.Format(time.RFC3339)
			}
			recentCampaigns = append(recentCampaigns, entry)
		}
	}
	result["recent_campaigns"] = recentCampaigns

	respondJSON(w, http.StatusOK, result)
}

// resolveTestMergeTags replaces Liquid-style merge tags with actual test values.
// In production, the campaign scheduler handles this; for direct test sends we do it here.
func resolveTestMergeTags(html, subject, previewText, testEmail, firstName string) (string, string, string) {
	replacer := strings.NewReplacer(
		`{{ first_name | default: "there" }}`, firstName,
		`{{first_name | default: "there"}}`, firstName,
		`{{ first_name | default: 'there' }}`, firstName,
		`{{ first_name }}`, firstName,
		`{{first_name}}`, firstName,
		`{{ email }}`, testEmail,
		`{{email}}`, testEmail,
		`{{ system.unsubscribe_url }}`, "#unsubscribe",
		`{{system.unsubscribe_url}}`, "#unsubscribe",
		`{{ system.preferences_url }}`, "#preferences",
		`{{system.preferences_url}}`, "#preferences",
	)
	return replacer.Replace(html), replacer.Replace(subject), replacer.Replace(previewText)
}

// sendWavesDirect sends each wave variation immediately via AWS SES.
func sendWavesDirect(ctx context.Context, b brandConfig, variations []mailing.WaveVariation, numWaves int, testEmail string) []map[string]interface{} {
	sesRegion := os.Getenv("SES_REGION")
	if sesRegion == "" {
		sesRegion = "us-west-1"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(sesRegion))
	if err != nil {
		log.Printf("[wave-content-test] failed to load AWS config: %v", err)
		return []map[string]interface{}{{"error": err.Error()}}
	}
	sesClient := sesv2.NewFromConfig(awsCfg)

	fromEmail := fmt.Sprintf("hello@%s", b.SESFromDomain)
	fromAddr := fmt.Sprintf("%s <%s>", b.FromName, fromEmail)

	var results []map[string]interface{}
	for i, v := range variations {
		if i >= numWaves {
			break
		}

		htmlBody, subj, preview := resolveTestMergeTags(v.HTMLContent, v.Subject, v.PreviewText, testEmail, "James")
		v.Subject = subj
		v.PreviewText = preview
		if !strings.Contains(htmlBody, "<html") {
			htmlBody = "<html><body>" + htmlBody + "</body></html>"
		}

		brandTag := strings.Map(func(r rune) rune {
			if r == ' ' {
				return '_'
			}
			return r
		}, b.BrandName)

		input := &sesv2.SendEmailInput{
			FromEmailAddress: &fromAddr,
			Destination: &sestypes.Destination{
				ToAddresses: []string{testEmail},
			},
			Content: &sestypes.EmailContent{
				Simple: &sestypes.Message{
					Subject: &sestypes.Content{Data: &v.Subject},
					Body: &sestypes.Body{
						Html: &sestypes.Content{Data: &htmlBody},
					},
				},
			},
			EmailTags: []sestypes.MessageTag{
				{Name: strPtr("wave"), Value: strPtr(fmt.Sprintf("%d", i+1))},
				{Name: strPtr("brand"), Value: &brandTag},
			},
		}

		resp, sendErr := sesClient.SendEmail(ctx, input)

		result := map[string]interface{}{
			"wave":         i + 1,
			"subject":      v.Subject,
			"preview_text": v.PreviewText,
			"from_name":    b.FromName,
			"from_email":   fromEmail,
			"html_length":  len(v.HTMLContent),
		}

		if sendErr != nil {
			result["status"] = "error"
			result["error"] = sendErr.Error()
			log.Printf("[wave-content-test] SES send failed for %s wave %d: %v", b.BrandName, i+1, sendErr)
		} else {
			msgID := ""
			if resp.MessageId != nil {
				msgID = *resp.MessageId
			}
			result["status"] = "sent"
			result["message_id"] = msgID
			log.Printf("[wave-content-test] SES sent %s wave %d/%d → %s (msgId=%s)", b.BrandName, i+1, numWaves, testEmail, msgID)
		}

		results = append(results, result)

		if i < numWaves-1 {
			time.Sleep(2 * time.Second)
		}
	}
	return results
}

// scheduleWaves creates campaign records for the scheduler to pick up.
func (s *PMTACampaignService) scheduleWaves(ctx context.Context, b brandConfig, variations []mailing.WaveVariation, numWaves int, testEmail, orgID string) []map[string]interface{} {
	testListID := ensureWaveTestRecipients(ctx, s.db, orgID, testEmail)
	if testListID == "" {
		return []map[string]interface{}{{"error": "failed to create test recipient list"}}
	}

	var profileID, fromEmail sql.NullString
	s.db.QueryRowContext(ctx, `
		SELECT id, from_email
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta'
		  AND (sending_domain = $2 OR from_email LIKE '%%@' || $2)
		  AND status = 'active'
		ORDER BY created_at DESC LIMIT 1
	`, orgID, b.SendingDomain).Scan(&profileID, &fromEmail)

	baseTime := time.Now().Add(5 * time.Minute)
	cadenceMinutes := 3

	var results []map[string]interface{}
	for i, v := range variations {
		if i >= numWaves {
			break
		}

		campaignID := uuid.New().String()
		campaignName := fmt.Sprintf("%s Wave %d/%d", b.BrandName, i+1, numWaves)
		scheduledAt := baseTime.Add(time.Duration(i*cadenceMinutes) * time.Minute)

		fEmail := ""
		if fromEmail.Valid {
			fEmail = fromEmail.String
		}

		espQuotas, _ := json.Marshal(map[string]interface{}{
			"target_isps":    []map[string]string{{"name": "Gmail", "domain": "gmail.com"}},
			"sending_domain": b.SendingDomain,
		})
		inclusionListsJSON, _ := json.Marshal([]string{testListID})

		_, insertErr := s.db.ExecContext(ctx, `
			INSERT INTO mailing_campaigns (
				id, organization_id, name, status, scheduled_at,
				from_name, from_email, subject, preview_text, html_content,
				sending_profile_id, esp_quotas, list_id, list_ids,
				send_type, created_at, updated_at
			) VALUES (
				$1, $2, $3, 'scheduled', $4,
				$5, $6, $7, $8, $9,
				$10, $11, $12, $13,
				'blast', NOW(), NOW()
			)
		`, campaignID, orgID, campaignName, scheduledAt,
			b.FromName, fEmail, v.Subject, v.PreviewText, v.HTMLContent,
			profileID, string(espQuotas), testListID, string(inclusionListsJSON),
		)

		status := "scheduled"
		if insertErr != nil {
			status = "error: " + insertErr.Error()
			log.Printf("[wave-content-test] %s wave %d insert error: %v", b.BrandName, i+1, insertErr)
		}

		results = append(results, map[string]interface{}{
			"wave":          i + 1,
			"campaign_id":   campaignID,
			"campaign_name": campaignName,
			"scheduled_at":  scheduledAt.Format(time.RFC3339),
			"subject":       v.Subject,
			"preview_text":  v.PreviewText,
			"from_name":     b.FromName,
			"html_length":   len(v.HTMLContent),
			"status":        status,
		})
	}
	return results
}

func strPtr(s string) *string { return &s }

// ensureWaveTestRecipients creates or finds a test list with the given email.
// The campaign scheduler resolves recipients via mailing_subscribers.list_id
// (NOT the join table), so we must set list_id on the subscriber row directly.
func ensureWaveTestRecipients(ctx context.Context, db *sql.DB, orgID, email string) string {
	listName := "Wave Content Test List"

	var listID string
	err := db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_lists
		WHERE organization_id = $1 AND name = $2
		LIMIT 1
	`, orgID, listName).Scan(&listID)

	if err != nil {
		listID = uuid.New().String()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_lists (id, organization_id, name, description, status, created_at, updated_at)
			VALUES ($1, $2, $3, 'Auto-created for wave content testing', 'active', NOW(), NOW())
			ON CONFLICT DO NOTHING
		`, listID, orgID, listName)
		if err != nil {
			log.Printf("[wave-content-test] failed to create test list: %v", err)
			return ""
		}
	}

	var actualSubID string
	db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_subscribers WHERE organization_id = $1 AND email = $2 LIMIT 1
	`, orgID, email).Scan(&actualSubID)

	if actualSubID == "" {
		actualSubID = uuid.New().String()
		_, err := db.ExecContext(ctx, `
			INSERT INTO mailing_subscribers (id, organization_id, email, first_name, last_name, status, engagement_score, list_id, subscribed_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'James', 'Ventures', 'confirmed', 80, $4, NOW(), NOW(), NOW())
			ON CONFLICT (organization_id, email) DO UPDATE SET status = 'confirmed', list_id = $4, updated_at = NOW()
		`, actualSubID, orgID, email, listID)
		if err != nil {
			log.Printf("[wave-content-test] failed to insert subscriber: %v", err)
		}
		db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_subscribers WHERE organization_id = $1 AND email = $2 LIMIT 1
		`, orgID, email).Scan(&actualSubID)
	} else {
		db.ExecContext(ctx, `
			UPDATE mailing_subscribers SET list_id = $1, status = 'confirmed', updated_at = NOW() WHERE id = $2
		`, listID, actualSubID)
	}

	if actualSubID != "" {
		db.ExecContext(ctx, `
			INSERT INTO mailing_list_subscribers (list_id, subscriber_id, subscribed_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT DO NOTHING
		`, listID, actualSubID)
	}

	var count int
	db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'
	`, listID).Scan(&count)
	log.Printf("[wave-content-test] test list %s has %d confirmed subscriber(s) (via list_id)", listID, count)

	return listID
}

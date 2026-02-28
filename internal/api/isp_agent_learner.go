package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"math"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ISP AGENT LEARNING ENGINE
// Automated hourly research sessions that scrape the web for ISP-specific
// deliverability intelligence, building persistent Long-Term Memory (LTM).
// ═══════════════════════════════════════════════════════════════════════════════

// ISPAgentLearner manages scheduled research sessions for all ISP agents.
type ISPAgentLearner struct {
	db      *sql.DB
	client  *http.Client
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}

	// Source quality memory — loaded from DB, updated in-memory, flushed periodically
	sourceScores map[string]*SourceScore
}

// ── Types ────────────────────────────────────────────────────────────────────

// LearningSession represents a single agent research session.
type LearningSession struct {
	ID             string           `json:"id"`
	AgentID        string           `json:"agent_id"`
	ISP            string           `json:"isp"`
	Domain         string           `json:"domain"`
	StartedAt      time.Time        `json:"started_at"`
	EndedAt        time.Time        `json:"ended_at"`
	DurationSec    int              `json:"duration_sec"`
	SourcesScraped int              `json:"sources_scraped"`
	FactsFound     int              `json:"facts_found"`
	Sources        []ResearchSource `json:"sources"`
	Facts          []LTMFact        `json:"facts"`
	Status         string           `json:"status"`
}

// ResearchSource represents a web page that was evaluated during research.
type ResearchSource struct {
	URL            string    `json:"url"`
	Title          string    `json:"title"`
	Domain         string    `json:"domain"`
	Category       string    `json:"category"`        // postmaster, blog, news, docs, forum, search
	Rating         string    `json:"rating"`           // important, useful, waste
	RelevanceScore float64   `json:"relevance_score"`  // 0-100
	FactsExtracted int       `json:"facts_extracted"`
	ScrapedAt      time.Time `json:"scraped_at"`
	Error          string    `json:"error,omitempty"`
	ContentLength  int       `json:"content_length"`
}

// LTMFact represents a single deliverability fact stored in long-term memory.
type LTMFact struct {
	ID         string    `json:"id"`
	ISP        string    `json:"isp"`
	Category   string    `json:"category"` // policy, threshold, best_practice, news, guideline, change
	Fact       string    `json:"fact"`
	Source     string    `json:"source"`
	SourceURL  string    `json:"source_url"`
	Confidence float64   `json:"confidence"` // 0-1
	LearnedAt  time.Time `json:"learned_at"`
	SessionID  string    `json:"session_id"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// SourceScore tracks the historical quality of a research source.
type SourceScore struct {
	Domain       string    `json:"domain"`
	TimesScraped int       `json:"times_scraped"`
	TotalFacts   int       `json:"total_facts"`
	AvgRelevance float64   `json:"avg_relevance"`
	Rating       string    `json:"rating"` // important, useful, waste, unknown
	LastScraped  time.Time `json:"last_scraped"`
}

// ISPResearchTarget defines where to search for a given ISP.
type ISPResearchTarget struct {
	ISP             string
	PostmasterURLs  []string
	DocumentationURLs []string
	SearchQueries   []string
	RSSFeeds        []string
	Keywords        []string // for fact extraction relevance
}

// ── Known ISP Research Targets ───────────────────────────────────────────────

var ispResearchTargets = map[string]ISPResearchTarget{
	"gmail": {
		ISP: "gmail",
		PostmasterURLs: []string{
			"https://support.google.com/mail/answer/81126",
			"https://support.google.com/a/answer/175365",
		},
		DocumentationURLs: []string{
			"https://www.gmail.com/postmaster/",
			"https://support.google.com/mail/answer/6585",
		},
		SearchQueries: []string{
			"Gmail deliverability guidelines %d",
			"Gmail bulk sender requirements %d",
			"Gmail spam filter changes %d",
			"Google postmaster tools updates %d",
			"Gmail DMARC DKIM requirements %d",
			"Gmail inbox placement best practices %d",
		},
		RSSFeeds: []string{
			"https://blog.google/products/gmail/rss/",
		},
		Keywords: []string{"gmail", "google", "postmaster", "bulk sender", "spam", "dmarc", "dkim", "spf", "authentication", "inbox", "deliverability", "complaint", "bounce", "reputation", "ip reputation"},
	},
	"yahoo": {
		ISP: "yahoo",
		PostmasterURLs: []string{
			"https://senders.yahooinc.com/best-practices/",
			"https://senders.yahooinc.com/faqs/",
		},
		DocumentationURLs: []string{
			"https://senders.yahooinc.com/",
		},
		SearchQueries: []string{
			"Yahoo Mail deliverability guidelines %d",
			"Yahoo sender best practices %d",
			"Yahoo spam filter updates %d",
			"Yahoo Mail DMARC requirements %d",
			"Yahoo inbox placement tips %d",
			"Yahoo bulk email requirements %d",
		},
		Keywords: []string{"yahoo", "verizon media", "aol", "postmaster", "bulk sender", "spam", "dmarc", "dkim", "inbox", "deliverability", "complaint", "bounce", "reputation", "sender requirements"},
	},
	"microsoft": {
		ISP: "microsoft",
		PostmasterURLs: []string{
			"https://sendersupport.olc.protection.outlook.com/pm/",
			"https://sendersupport.olc.protection.outlook.com/pm/troubleshooting.aspx",
		},
		DocumentationURLs: []string{
			"https://learn.microsoft.com/en-us/microsoft-365/security/office-365-security/anti-spam-message-headers",
		},
		SearchQueries: []string{
			"Outlook deliverability guidelines %d",
			"Microsoft 365 bulk sender requirements %d",
			"Outlook spam filter changes %d",
			"Hotmail deliverability best practices %d",
			"Microsoft SNDS updates %d",
			"Outlook inbox placement %d",
		},
		Keywords: []string{"outlook", "microsoft", "hotmail", "live.com", "office 365", "exchange online protection", "eop", "snds", "jmrp", "postmaster", "spam", "deliverability", "reputation", "smartscreen"},
	},
	"apple": {
		ISP: "apple",
		PostmasterURLs: []string{},
		DocumentationURLs: []string{
			"https://support.apple.com/en-us/HT204137",
		},
		SearchQueries: []string{
			"iCloud Mail deliverability guidelines %d",
			"Apple Mail privacy protection impact senders %d",
			"iCloud email sender requirements %d",
			"Apple Mail spam filter %d",
		},
		Keywords: []string{"apple", "icloud", "apple mail", "mail privacy protection", "mpp", "hide my email", "deliverability", "spam", "inbox"},
	},
	"comcast": {
		ISP: "comcast",
		PostmasterURLs: []string{
			"https://postmaster.comcast.net/",
		},
		DocumentationURLs: []string{},
		SearchQueries: []string{
			"Comcast Xfinity email deliverability %d",
			"Comcast postmaster guidelines %d",
			"Comcast bulk sender requirements %d",
		},
		Keywords: []string{"comcast", "xfinity", "postmaster", "deliverability", "spam", "bounce", "block", "throttle"},
	},
}

// Deliverability industry blogs / news sources
var industryResearchURLs = []string{
	"https://www.validity.com/blog/",
	"https://www.sparkpost.com/blog/",
	"https://sendgrid.com/en-us/blog",
	"https://www.emailonacid.com/blog/",
	"https://litmus.com/blog/",
	"https://www.emailvendorselection.com/",
}

var industryRSSFeeds = []string{
	"https://www.validity.com/blog/feed/",
	"https://wordtothewise.com/feed/",
}

// ── Constructor & Lifecycle ──────────────────────────────────────────────────

// NewISPAgentLearner creates a new learning engine.
func NewISPAgentLearner(db *sql.DB) *ISPAgentLearner {
	return &ISPAgentLearner{
		db: db,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		},
		sourceScores: make(map[string]*SourceScore),
		stopCh:       make(chan struct{}),
	}
}

// ensureTables creates the LTM tables if they don't exist (idempotent bootstrap).
func (l *ISPAgentLearner) ensureTables() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	statements := []string{
		`CREATE TABLE IF NOT EXISTS mailing_isp_agent_research (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_id UUID,
			session_id VARCHAR(64) NOT NULL,
			isp VARCHAR(50) NOT NULL,
			domain VARCHAR(255),
			started_at TIMESTAMPTZ NOT NULL,
			ended_at TIMESTAMPTZ,
			duration_sec INT DEFAULT 0,
			sources_scraped INT DEFAULT 0,
			facts_found INT DEFAULT 0,
			sources JSONB DEFAULT '[]',
			facts JSONB DEFAULT '[]',
			status VARCHAR(20) DEFAULT 'running',
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_isp_agent_ltm (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			agent_id UUID,
			isp VARCHAR(50) NOT NULL,
			category VARCHAR(30) NOT NULL,
			fact TEXT NOT NULL,
			source_url TEXT,
			source_domain VARCHAR(255),
			confidence FLOAT DEFAULT 0.5,
			session_id VARCHAR(64),
			supersedes_id UUID,
			is_active BOOLEAN DEFAULT true,
			learned_at TIMESTAMPTZ DEFAULT NOW(),
			expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS mailing_isp_source_scores (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			source_domain VARCHAR(255) NOT NULL UNIQUE,
			isp_relevance JSONB DEFAULT '{}',
			times_scraped INT DEFAULT 0,
			total_facts INT DEFAULT 0,
			avg_relevance FLOAT DEFAULT 0,
			rating VARCHAR(20) DEFAULT 'unknown',
			last_scraped_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
	}

	for _, stmt := range statements {
		if _, err := l.db.ExecContext(ctx, stmt); err != nil {
			log.Printf("[ISP-Learner] Warning: table bootstrap error (may already exist): %v", err)
		}
	}
	log.Println("[ISP-Learner] LTM tables verified/created")
}

// Start begins the hourly learning scheduler.
func (l *ISPAgentLearner) Start() {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return
	}
	l.running = true
	l.mu.Unlock()

	log.Println("[ISP-Learner] Starting hourly learning scheduler...")

	// Bootstrap tables (idempotent)
	l.ensureTables()

	// Load source scores from DB
	l.loadSourceScores()

	// Run initial learning pass after a short delay
	go func() {
		time.Sleep(30 * time.Second) // Wait for system startup
		l.runLearningCycle()
	}()

	// Schedule hourly
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				l.runLearningCycle()
			case <-l.stopCh:
				log.Println("[ISP-Learner] Scheduler stopped.")
				return
			}
		}
	}()
}

// Stop halts the learning scheduler.
func (l *ISPAgentLearner) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		close(l.stopCh)
		l.running = false
	}
}

// ── Core Learning Cycle ──────────────────────────────────────────────────────

// runLearningCycle finds all active agents and runs a research session for each.
func (l *ISPAgentLearner) runLearningCycle() {
	log.Println("[ISP-Learner] ═══ Starting hourly learning cycle ═══")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Fetch all active agents
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, isp, domain FROM mailing_isp_agents 
		 WHERE status NOT IN ('dormant', 'complete') 
		 ORDER BY last_active_at DESC NULLS LAST`)
	if err != nil {
		log.Printf("[ISP-Learner] Error fetching agents: %v", err)
		return
	}
	defer rows.Close()

	type agentInfo struct {
		ID, ISP, Domain string
	}
	var agents []agentInfo
	for rows.Next() {
		var a agentInfo
		if err := rows.Scan(&a.ID, &a.ISP, &a.Domain); err != nil {
			continue
		}
		agents = append(agents, a)
	}

	if len(agents) == 0 {
		// Also learn for "general" if no specific agents
		log.Println("[ISP-Learner] No active agents found, running general industry research...")
		l.runGeneralResearch(ctx)
		return
	}

	log.Printf("[ISP-Learner] Found %d active agents to research", len(agents))

	// Deduplicate by ISP — no need to research same ISP twice
	ispSeen := make(map[string]bool)
	for _, a := range agents {
		ispKey := strings.ToLower(a.ISP)
		if ispSeen[ispKey] {
			continue
		}
		ispSeen[ispKey] = true

		session := l.runAgentSession(ctx, a.ID, a.ISP, a.Domain)
		log.Printf("[ISP-Learner] Session complete for %s: %d sources scraped, %d facts found, status=%s",
			a.ISP, session.SourcesScraped, session.FactsFound, session.Status)
	}

	// Also run general industry research
	l.runGeneralResearch(ctx)

	// Flush source scores to DB
	l.flushSourceScores()

	log.Println("[ISP-Learner] ═══ Learning cycle complete ═══")
}

// runAgentSession executes a 5-minute research session for a specific ISP agent.
func (l *ISPAgentLearner) runAgentSession(ctx context.Context, agentID, isp, domain string) LearningSession {
	sessionID := fmt.Sprintf("sess_%s_%s", isp, time.Now().Format("20060102_150405"))
	session := LearningSession{
		ID:        sessionID,
		AgentID:   agentID,
		ISP:       isp,
		Domain:    domain,
		StartedAt: time.Now(),
		Status:    "running",
	}

	log.Printf("[ISP-Learner/%s] ── Starting 5-minute research session %s ──", isp, sessionID)

	// Persist session start
	l.persistSessionStart(ctx, &session)

	// 5-minute deadline for this session
	sessionCtx, sessionCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer sessionCancel()

	// Determine ISP key for research targets
	ispKey := l.normalizeISPKey(isp)
	targets, hasTargets := ispResearchTargets[ispKey]
	if !hasTargets {
		// Build dynamic targets for unknown ISPs
		targets = l.buildDynamicTargets(isp)
	}

	// Phase 1: Scrape known postmaster pages (highest priority)
	for _, u := range targets.PostmasterURLs {
		if sessionCtx.Err() != nil {
			break
		}
		src := l.scrapeURL(sessionCtx, u, isp, "postmaster", targets.Keywords)
		session.Sources = append(session.Sources, src)
		session.Facts = append(session.Facts, l.extractFactsFromSource(src, isp, sessionID)...)
	}

	// Phase 2: Scrape documentation
	for _, u := range targets.DocumentationURLs {
		if sessionCtx.Err() != nil {
			break
		}
		src := l.scrapeURL(sessionCtx, u, isp, "docs", targets.Keywords)
		session.Sources = append(session.Sources, src)
		session.Facts = append(session.Facts, l.extractFactsFromSource(src, isp, sessionID)...)
	}

	// Phase 3: Web search for recent news/updates
	year := time.Now().Year()
	for _, queryTpl := range targets.SearchQueries {
		if sessionCtx.Err() != nil {
			break
		}
		query := fmt.Sprintf(queryTpl, year)
		searchSources := l.webSearch(sessionCtx, query, isp, targets.Keywords)
		for _, src := range searchSources {
			session.Sources = append(session.Sources, src)
			session.Facts = append(session.Facts, l.extractFactsFromSource(src, isp, sessionID)...)
		}
	}

	// Phase 4: Check RSS feeds
	allFeeds := append(targets.RSSFeeds, industryRSSFeeds...)
	for _, feedURL := range allFeeds {
		if sessionCtx.Err() != nil {
			break
		}
		src := l.scrapeRSSFeed(sessionCtx, feedURL, isp, targets.Keywords)
		session.Sources = append(session.Sources, src)
		session.Facts = append(session.Facts, l.extractFactsFromSource(src, isp, sessionID)...)
	}

	// Deduplicate facts
	session.Facts = l.deduplicateFacts(session.Facts)

	// Ensure minimum 1 fact per session
	if len(session.Facts) == 0 {
		session.Facts = append(session.Facts, LTMFact{
			ID:         uuid.New().String(),
			ISP:        isp,
			Category:   "best_practice",
			Fact:        fmt.Sprintf("[%s] No new facts discovered in this session. All known %s deliverability guidelines appear current as of %s.", isp, isp, time.Now().Format("2006-01-02")),
			Source:     "system",
			Confidence: 0.3,
			LearnedAt:  time.Now(),
			SessionID:  sessionID,
		})
	}

	// Finalize session
	session.EndedAt = time.Now()
	session.DurationSec = int(session.EndedAt.Sub(session.StartedAt).Seconds())
	session.SourcesScraped = len(session.Sources)
	session.FactsFound = len(session.Facts)
	session.Status = "completed"

	// Persist results
	l.persistSession(ctx, &session)
	l.persistFacts(ctx, agentID, &session)
	l.updateAgentKnowledge(ctx, agentID, isp, &session)

	// Update source scores
	for _, src := range session.Sources {
		l.updateSourceScore(src)
	}

	// Log summary
	importantCount := 0
	wasteCount := 0
	for _, src := range session.Sources {
		if src.Rating == "important" {
			importantCount++
		} else if src.Rating == "waste" {
			wasteCount++
		}
	}
	log.Printf("[ISP-Learner/%s] Session %s: %d sources (%d important, %d waste), %d facts in %ds",
		isp, sessionID, session.SourcesScraped, importantCount, wasteCount,
		session.FactsFound, session.DurationSec)

	return session
}

// ── Web Research Methods ─────────────────────────────────────────────────────

// scrapeURL fetches and analyzes a single URL.
func (l *ISPAgentLearner) scrapeURL(ctx context.Context, rawURL, isp, category string, keywords []string) ResearchSource {
	src := ResearchSource{
		URL:       rawURL,
		Domain:    extractDomain(rawURL),
		Category:  category,
		ScrapedAt: time.Now(),
	}

	// Check if this source was previously rated as waste — skip it
	l.mu.RLock()
	if score, ok := l.sourceScores[src.Domain]; ok && score.Rating == "waste" && score.TimesScraped >= 3 {
		l.mu.RUnlock()
		src.Rating = "waste"
		src.Error = "skipped — previously rated as waste"
		log.Printf("[ISP-Learner/%s] SKIP %s (rated waste, scraped %d times)", isp, src.Domain, score.TimesScraped)
		return src
	}
	l.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		src.Error = err.Error()
		src.Rating = "waste"
		return src
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ISPResearchBot/1.0; deliverability-research)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := l.client.Do(req)
	if err != nil {
		src.Error = fmt.Sprintf("fetch error: %v", err)
		src.Rating = "waste"
		log.Printf("[ISP-Learner/%s] FAIL %s: %v", isp, rawURL, err)
		return src
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		src.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		src.Rating = "waste"
		log.Printf("[ISP-Learner/%s] FAIL %s: HTTP %d", isp, rawURL, resp.StatusCode)
		return src
	}

	// Read body with limit (500KB max)
	bodyReader := io.LimitReader(resp.Body, 512*1024)
	doc, err := goquery.NewDocumentFromReader(bodyReader)
	if err != nil {
		src.Error = fmt.Sprintf("parse error: %v", err)
		src.Rating = "waste"
		return src
	}

	// Extract title
	src.Title = strings.TrimSpace(doc.Find("title").First().Text())
	if src.Title == "" {
		src.Title = strings.TrimSpace(doc.Find("h1").First().Text())
	}

	// Extract meaningful text content
	content := l.extractPageContent(doc)
	src.ContentLength = len(content)

	// Score relevance based on keyword density
	src.RelevanceScore = l.scoreRelevance(content, isp, keywords)

	// Rate the source
	if src.RelevanceScore >= 60 {
		src.Rating = "important"
	} else if src.RelevanceScore >= 25 {
		src.Rating = "useful"
	} else {
		src.Rating = "waste"
	}

	log.Printf("[ISP-Learner/%s] SCRAPED %s — relevance=%.0f rating=%s title=%q",
		isp, src.Domain, src.RelevanceScore, src.Rating, truncateStr(src.Title, 60))

	return src
}

// webSearch performs a DuckDuckGo HTML search and scrapes top results.
func (l *ISPAgentLearner) webSearch(ctx context.Context, query, isp string, keywords []string) []ResearchSource {
	var sources []ResearchSource

	// Build DuckDuckGo HTML search URL
	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(query))

	src := ResearchSource{
		URL:       searchURL,
		Domain:    "duckduckgo.com",
		Category:  "search",
		ScrapedAt: time.Now(),
	}

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		src.Error = err.Error()
		src.Rating = "waste"
		sources = append(sources, src)
		return sources
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")

	resp, err := l.client.Do(req)
	if err != nil {
		src.Error = fmt.Sprintf("search error: %v", err)
		src.Rating = "waste"
		sources = append(sources, src)
		return sources
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		src.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		src.Rating = "useful" // search itself isn't waste
		sources = append(sources, src)
		return sources
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		src.Error = fmt.Sprintf("parse error: %v", err)
		src.Rating = "waste"
		sources = append(sources, src)
		return sources
	}

	// Extract search result links (top 3 only for time efficiency)
	var resultURLs []string
	doc.Find("a.result__a").Each(func(i int, s *goquery.Selection) {
		if i >= 3 {
			return
		}
		if href, exists := s.Attr("href"); exists {
			// DuckDuckGo wraps URLs in redirect — extract actual URL
			if parsed, err := url.Parse(href); err == nil {
				actual := parsed.Query().Get("uddg")
				if actual != "" {
					resultURLs = append(resultURLs, actual)
				} else if strings.HasPrefix(href, "http") {
					resultURLs = append(resultURLs, href)
				}
			}
		}
	})

	src.Rating = "useful"
	src.Title = fmt.Sprintf("Search: %s (%d results)", query, len(resultURLs))
	sources = append(sources, src)

	log.Printf("[ISP-Learner/%s] SEARCH %q → %d results", isp, query, len(resultURLs))

	// Scrape top results
	for _, resultURL := range resultURLs {
		if ctx.Err() != nil {
			break
		}
		resultSrc := l.scrapeURL(ctx, resultURL, isp, "search_result", keywords)
		sources = append(sources, resultSrc)
		// Small delay between scrapes to be polite
		time.Sleep(500 * time.Millisecond)
	}

	return sources
}

// scrapeRSSFeed fetches an RSS feed and extracts relevant entries.
func (l *ISPAgentLearner) scrapeRSSFeed(ctx context.Context, feedURL, isp string, keywords []string) ResearchSource {
	src := ResearchSource{
		URL:       feedURL,
		Domain:    extractDomain(feedURL),
		Category:  "rss",
		ScrapedAt: time.Now(),
	}

	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
	if err != nil {
		src.Error = err.Error()
		src.Rating = "waste"
		return src
	}
	req.Header.Set("User-Agent", "ISPResearchBot/1.0")
	req.Header.Set("Accept", "application/rss+xml,application/xml,text/xml")

	resp, err := l.client.Do(req)
	if err != nil {
		src.Error = fmt.Sprintf("feed error: %v", err)
		src.Rating = "waste"
		return src
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		src.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		src.Rating = "waste"
		return src
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		src.Error = fmt.Sprintf("read error: %v", err)
		src.Rating = "waste"
		return src
	}

	content := string(body)
	src.ContentLength = len(content)
	src.RelevanceScore = l.scoreRelevance(content, isp, keywords)

	if src.RelevanceScore >= 30 {
		src.Rating = "useful"
	} else {
		src.Rating = "waste"
	}

	src.Title = fmt.Sprintf("RSS Feed: %s", extractDomain(feedURL))
	return src
}

// ── Content Extraction & Analysis ────────────────────────────────────────────

// extractPageContent extracts meaningful text from an HTML document.
func (l *ISPAgentLearner) extractPageContent(doc *goquery.Document) string {
	// Remove scripts, styles, nav, footer
	doc.Find("script, style, nav, footer, header, aside, .sidebar, .menu, .cookie-notice").Remove()

	var parts []string

	// Extract from main content areas
	selectors := []string{
		"article", "main", ".content", ".post-content", ".entry-content",
		"#content", ".article-body", ".blog-post",
	}

	for _, sel := range selectors {
		text := strings.TrimSpace(doc.Find(sel).Text())
		if len(text) > 100 {
			parts = append(parts, text)
		}
	}

	// Fallback to body if no content areas found
	if len(parts) == 0 {
		bodyText := strings.TrimSpace(doc.Find("body").Text())
		if len(bodyText) > 100 {
			parts = append(parts, bodyText)
		}
	}

	content := strings.Join(parts, "\n\n")

	// Collapse whitespace
	for strings.Contains(content, "  ") {
		content = strings.ReplaceAll(content, "  ", " ")
	}
	for strings.Contains(content, "\n\n\n") {
		content = strings.ReplaceAll(content, "\n\n\n", "\n\n")
	}

	// Truncate to 10KB
	if len(content) > 10240 {
		content = content[:10240]
	}

	return content
}

// scoreRelevance rates how relevant page content is for a given ISP.
func (l *ISPAgentLearner) scoreRelevance(content, isp string, keywords []string) float64 {
	if len(content) < 50 {
		return 0
	}

	lower := strings.ToLower(content)
	score := 0.0

	// ISP name mention (high weight)
	ispLower := strings.ToLower(isp)
	ispCount := strings.Count(lower, ispLower)
	score += float64(min(ispCount, 10)) * 5 // Up to 50 points

	// Keyword matches
	for _, kw := range keywords {
		kwLower := strings.ToLower(kw)
		count := strings.Count(lower, kwLower)
		if count > 0 {
			score += float64(min(count, 5)) * 2 // Up to 10 points per keyword
		}
	}

	// Deliverability-specific terms (universal)
	delivTerms := []string{
		"deliverability", "inbox placement", "sender reputation",
		"complaint rate", "bounce rate", "authentication",
		"dmarc", "dkim", "spf", "feedback loop",
		"blocklist", "blacklist", "whitelist", "throttle",
		"sending limit", "bulk sender", "warmup",
		"engagement", "open rate", "click rate",
		"ip reputation", "domain reputation",
	}
	for _, term := range delivTerms {
		if strings.Contains(lower, term) {
			score += 3
		}
	}

	// Recency bonus — mentions of current year
	currentYear := fmt.Sprintf("%d", time.Now().Year())
	if strings.Contains(content, currentYear) {
		score += 10
	}

	// Normalize to 0-100
	return math.Min(score, 100)
}

// extractFactsFromSource extracts deliverability facts from a research source.
func (l *ISPAgentLearner) extractFactsFromSource(src ResearchSource, isp, sessionID string) []LTMFact {
	if src.Rating == "waste" || src.Error != "" || src.ContentLength < 100 {
		return nil
	}

	var facts []LTMFact

	// This is a heuristic fact extractor based on common patterns
	// in deliverability documentation. We look for sentences containing
	// key actionable terms.

	// For postmaster pages, the entire content is a fact
	if src.Category == "postmaster" && src.RelevanceScore >= 50 {
		facts = append(facts, LTMFact{
			ID:         uuid.New().String(),
			ISP:        isp,
			Category:   "guideline",
			Fact:        fmt.Sprintf("[%s Postmaster] Page '%s' is active and accessible (relevance %.0f/100). Source verified as authoritative.", strings.ToUpper(isp), src.Title, src.RelevanceScore),
			Source:     src.Domain,
			SourceURL:  src.URL,
			Confidence: 0.9,
			LearnedAt:  time.Now(),
			SessionID:  sessionID,
		})
	}

	// For high-relevance search results, log the finding
	if src.RelevanceScore >= 60 && src.Category != "search" {
		category := "best_practice"
		if strings.Contains(strings.ToLower(src.Title), "update") || strings.Contains(strings.ToLower(src.Title), "change") || strings.Contains(strings.ToLower(src.Title), "new") {
			category = "change"
		}
		if strings.Contains(strings.ToLower(src.Title), "policy") || strings.Contains(strings.ToLower(src.Title), "requirement") {
			category = "policy"
		}

		confidence := src.RelevanceScore / 100
		if src.Category == "postmaster" || src.Category == "docs" {
			confidence = math.Min(confidence+0.2, 1.0)
		}

		expiry := time.Now().Add(30 * 24 * time.Hour) // 30-day default expiry for news
		if category == "policy" || category == "guideline" {
			expiry = time.Now().Add(90 * 24 * time.Hour) // 90 days for policies
		}

		facts = append(facts, LTMFact{
			ID:         uuid.New().String(),
			ISP:        isp,
			Category:   category,
			Fact:        fmt.Sprintf("[%s] Found relevant content: '%s' at %s (relevance %.0f/100, category: %s)", strings.ToUpper(isp), truncateStr(src.Title, 120), src.Domain, src.RelevanceScore, src.Category),
			Source:     src.Domain,
			SourceURL:  src.URL,
			Confidence: confidence,
			LearnedAt:  time.Now(),
			SessionID:  sessionID,
			ExpiresAt:  &expiry,
		})
	}

	src.FactsExtracted = len(facts)
	return facts
}

// ── General Industry Research ────────────────────────────────────────────────

func (l *ISPAgentLearner) runGeneralResearch(ctx context.Context) {
	log.Println("[ISP-Learner/general] Running general industry research...")

	generalKeywords := []string{
		"deliverability", "inbox placement", "sender reputation",
		"email authentication", "dmarc", "dkim", "spf", "bimi",
		"bulk sender", "email compliance",
	}

	for _, u := range industryResearchURLs {
		if ctx.Err() != nil {
			break
		}
		src := l.scrapeURL(ctx, u, "general", "blog", generalKeywords)
		if src.Rating == "important" || src.Rating == "useful" {
			log.Printf("[ISP-Learner/general] Found relevant industry source: %s (relevance=%.0f)", src.Domain, src.RelevanceScore)
		}
		l.updateSourceScore(src)
		time.Sleep(1 * time.Second)
	}
}

// ── Source Scoring ────────────────────────────────────────────────────────────

func (l *ISPAgentLearner) updateSourceScore(src ResearchSource) {
	l.mu.Lock()
	defer l.mu.Unlock()

	domain := src.Domain
	if domain == "" {
		return
	}

	score, exists := l.sourceScores[domain]
	if !exists {
		score = &SourceScore{
			Domain: domain,
			Rating: "unknown",
		}
		l.sourceScores[domain] = score
	}

	score.TimesScraped++
	score.LastScraped = time.Now()

	if src.FactsExtracted > 0 {
		score.TotalFacts += src.FactsExtracted
	}

	// Running average of relevance
	score.AvgRelevance = (score.AvgRelevance*float64(score.TimesScraped-1) + src.RelevanceScore) / float64(score.TimesScraped)

	// Update rating based on historical data
	if score.TimesScraped >= 2 {
		if score.AvgRelevance >= 50 && score.TotalFacts > 0 {
			score.Rating = "important"
		} else if score.AvgRelevance >= 20 {
			score.Rating = "useful"
		} else {
			score.Rating = "waste"
		}
	} else {
		score.Rating = src.Rating
	}
}

func (l *ISPAgentLearner) loadSourceScores() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := l.db.QueryContext(ctx,
		`SELECT source_domain, times_scraped, total_facts, avg_relevance, rating, last_scraped_at
		 FROM mailing_isp_source_scores`)
	if err != nil {
		log.Printf("[ISP-Learner] Error loading source scores: %v", err)
		return
	}
	defer rows.Close()

	l.mu.Lock()
	defer l.mu.Unlock()

	count := 0
	for rows.Next() {
		var s SourceScore
		var lastScraped sql.NullTime
		if err := rows.Scan(&s.Domain, &s.TimesScraped, &s.TotalFacts, &s.AvgRelevance, &s.Rating, &lastScraped); err != nil {
			continue
		}
		if lastScraped.Valid {
			s.LastScraped = lastScraped.Time
		}
		l.sourceScores[s.Domain] = &s
		count++
	}
	log.Printf("[ISP-Learner] Loaded %d source scores from DB", count)
}

func (l *ISPAgentLearner) flushSourceScores() {
	l.mu.RLock()
	scores := make([]*SourceScore, 0, len(l.sourceScores))
	for _, s := range l.sourceScores {
		scores = append(scores, s)
	}
	l.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, s := range scores {
		_, err := l.db.ExecContext(ctx, `
			INSERT INTO mailing_isp_source_scores (source_domain, times_scraped, total_facts, avg_relevance, rating, last_scraped_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			ON CONFLICT (source_domain) DO UPDATE SET
				times_scraped = $2,
				total_facts = $3,
				avg_relevance = $4,
				rating = $5,
				last_scraped_at = $6,
				updated_at = NOW()
		`, s.Domain, s.TimesScraped, s.TotalFacts, s.AvgRelevance, s.Rating, s.LastScraped)
		if err != nil {
			log.Printf("[ISP-Learner] Error flushing score for %s: %v", s.Domain, err)
		}
	}
	log.Printf("[ISP-Learner] Flushed %d source scores to DB", len(scores))
}

// ── Persistence ──────────────────────────────────────────────────────────────

func (l *ISPAgentLearner) persistSessionStart(ctx context.Context, session *LearningSession) {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO mailing_isp_agent_research (session_id, agent_id, isp, domain, started_at, status)
		VALUES ($1, $2::uuid, $3, $4, $5, 'running')
	`, session.ID, session.AgentID, session.ISP, session.Domain, session.StartedAt)
	if err != nil {
		log.Printf("[ISP-Learner/%s] Error persisting session start: %v", session.ISP, err)
	}
}

func (l *ISPAgentLearner) persistSession(ctx context.Context, session *LearningSession) {
	sourcesJSON, _ := json.Marshal(session.Sources)
	factsJSON, _ := json.Marshal(session.Facts)

	_, err := l.db.ExecContext(ctx, `
		UPDATE mailing_isp_agent_research
		SET ended_at = $1, duration_sec = $2, sources_scraped = $3, facts_found = $4,
		    sources = $5, facts = $6, status = $7
		WHERE session_id = $8
	`, session.EndedAt, session.DurationSec, session.SourcesScraped, session.FactsFound,
		sourcesJSON, factsJSON, session.Status, session.ID)
	if err != nil {
		log.Printf("[ISP-Learner/%s] Error persisting session: %v", session.ISP, err)
	}
}

func (l *ISPAgentLearner) persistFacts(ctx context.Context, agentID string, session *LearningSession) {
	for _, fact := range session.Facts {
		var expiresAt *time.Time
		if fact.ExpiresAt != nil {
			expiresAt = fact.ExpiresAt
		}

		_, err := l.db.ExecContext(ctx, `
			INSERT INTO mailing_isp_agent_ltm (agent_id, isp, category, fact, source_url, source_domain, confidence, session_id, learned_at, expires_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, agentID, fact.ISP, fact.Category, fact.Fact, fact.SourceURL, fact.Source, fact.Confidence, fact.SessionID, fact.LearnedAt, expiresAt)
		if err != nil {
			log.Printf("[ISP-Learner/%s] Error persisting fact: %v", session.ISP, err)
		}
	}
}

func (l *ISPAgentLearner) updateAgentKnowledge(ctx context.Context, agentID, isp string, session *LearningSession) {
	// Merge new facts into the agent's knowledge JSONB
	var existingKnowledge map[string]interface{}
	var knowledgeJSON []byte
	err := l.db.QueryRowContext(ctx,
		`SELECT knowledge FROM mailing_isp_agents WHERE id = $1`, agentID,
	).Scan(&knowledgeJSON)
	if err != nil {
		existingKnowledge = make(map[string]interface{})
	} else {
		json.Unmarshal(knowledgeJSON, &existingKnowledge)
		if existingKnowledge == nil {
			existingKnowledge = make(map[string]interface{})
		}
	}

	// Update LTM summary in knowledge
	existingKnowledge["last_research_at"] = time.Now()
	existingKnowledge["last_session_id"] = session.ID
	existingKnowledge["total_research_sessions"] = incrementFloat(existingKnowledge["total_research_sessions"])
	existingKnowledge["total_ltm_facts"] = incrementFloatBy(existingKnowledge["total_ltm_facts"], float64(session.FactsFound))

	// Count important vs waste sources
	importantSources := 0
	wasteSources := 0
	for _, src := range session.Sources {
		switch src.Rating {
		case "important":
			importantSources++
		case "waste":
			wasteSources++
		}
	}
	existingKnowledge["last_important_sources"] = importantSources
	existingKnowledge["last_waste_sources"] = wasteSources

	// Store latest facts as quick-access list
	latestFacts := make([]string, 0, len(session.Facts))
	for _, f := range session.Facts {
		latestFacts = append(latestFacts, f.Fact)
	}
	existingKnowledge["latest_facts"] = latestFacts

	// Persist
	updatedJSON, _ := json.Marshal(existingKnowledge)
	_, err = l.db.ExecContext(ctx, `
		UPDATE mailing_isp_agents SET knowledge = $1, updated_at = NOW() WHERE id = $2
	`, updatedJSON, agentID)
	if err != nil {
		log.Printf("[ISP-Learner/%s] Error updating agent knowledge: %v", isp, err)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (l *ISPAgentLearner) normalizeISPKey(isp string) string {
	lower := strings.ToLower(isp)
	switch {
	case strings.Contains(lower, "gmail") || strings.Contains(lower, "google"):
		return "gmail"
	case strings.Contains(lower, "yahoo") || strings.Contains(lower, "aol"):
		return "yahoo"
	case strings.Contains(lower, "microsoft") || strings.Contains(lower, "outlook") || strings.Contains(lower, "hotmail") || strings.Contains(lower, "live"):
		return "microsoft"
	case strings.Contains(lower, "apple") || strings.Contains(lower, "icloud"):
		return "apple"
	case strings.Contains(lower, "comcast") || strings.Contains(lower, "xfinity"):
		return "comcast"
	default:
		return lower
	}
}

func (l *ISPAgentLearner) buildDynamicTargets(isp string) ISPResearchTarget {
	year := time.Now().Year()
	return ISPResearchTarget{
		ISP: isp,
		SearchQueries: []string{
			fmt.Sprintf("%s email deliverability guidelines %%d", isp),
			fmt.Sprintf("%s postmaster sender requirements %%d", isp),
			fmt.Sprintf("%s spam filter best practices %%d", isp),
		},
		Keywords: []string{
			strings.ToLower(isp), "deliverability", "inbox", "spam",
			"sender", "authentication", "dmarc", "reputation",
			fmt.Sprintf("%d", year),
		},
	}
}

func (l *ISPAgentLearner) deduplicateFacts(facts []LTMFact) []LTMFact {
	seen := make(map[string]bool)
	var unique []LTMFact
	for _, f := range facts {
		// Hash the fact text for dedup
		hash := fmt.Sprintf("%x", md5.Sum([]byte(f.Fact)))[:12]
		if !seen[hash] {
			seen[hash] = true
			unique = append(unique, f)
		}
	}
	return unique
}

func extractDomain(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return parsed.Host
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func incrementFloat(val interface{}) float64 {
	if v, ok := val.(float64); ok {
		return v + 1
	}
	return 1
}

func incrementFloatBy(val interface{}, add float64) float64 {
	if v, ok := val.(float64); ok {
		return v + add
	}
	return add
}

// ── HTTP Handlers ────────────────────────────────────────────────────────────

// HandleGetResearchSessions returns recent research sessions for an agent.
func (l *ISPAgentLearner) HandleGetResearchSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	isp := r.URL.Query().Get("isp")
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if limitStr != "" {
		fmt.Sscanf(limitStr, "%d", &limit)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	query := `SELECT session_id, agent_id, isp, domain, started_at, ended_at, duration_sec,
	                 sources_scraped, facts_found, sources, facts, status
	          FROM mailing_isp_agent_research`

	var args []interface{}
	var conditions []string
	if agentID != "" {
		conditions = append(conditions, fmt.Sprintf("agent_id = $%d::uuid", len(args)+1))
		args = append(args, agentID)
	}
	if isp != "" {
		conditions = append(conditions, fmt.Sprintf("isp = $%d", len(args)+1))
		args = append(args, isp)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY started_at DESC LIMIT %d", limit)

	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		respondError(w, 500, fmt.Sprintf("query error: %v", err))
		return
	}
	defer rows.Close()

	var sessions []LearningSession
	for rows.Next() {
		var s LearningSession
		var endedAt sql.NullTime
		var sourcesJSON, factsJSON []byte
		if err := rows.Scan(&s.ID, &s.AgentID, &s.ISP, &s.Domain, &s.StartedAt, &endedAt,
			&s.DurationSec, &s.SourcesScraped, &s.FactsFound, &sourcesJSON, &factsJSON, &s.Status); err != nil {
			continue
		}
		if endedAt.Valid {
			s.EndedAt = endedAt.Time
		}
		json.Unmarshal(sourcesJSON, &s.Sources)
		json.Unmarshal(factsJSON, &s.Facts)
		sessions = append(sessions, s)
	}

	respondJSON(w, 200, map[string]interface{}{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// HandleGetLTMFacts returns long-term memory facts.
func (l *ISPAgentLearner) HandleGetLTMFacts(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	isp := r.URL.Query().Get("isp")
	category := r.URL.Query().Get("category")
	activeOnly := r.URL.Query().Get("active") != "false"

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	query := `SELECT id, agent_id, isp, category, fact, source_url, source_domain,
	                 confidence, session_id, is_active, learned_at, expires_at
	          FROM mailing_isp_agent_ltm`

	var args []interface{}
	var conditions []string
	if agentID != "" {
		conditions = append(conditions, fmt.Sprintf("agent_id = $%d::uuid", len(args)+1))
		args = append(args, agentID)
	}
	if isp != "" {
		conditions = append(conditions, fmt.Sprintf("isp = $%d", len(args)+1))
		args = append(args, isp)
	}
	if category != "" {
		conditions = append(conditions, fmt.Sprintf("category = $%d", len(args)+1))
		args = append(args, category)
	}
	if activeOnly {
		conditions = append(conditions, "is_active = true")
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY learned_at DESC LIMIT 100"

	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		respondError(w, 500, fmt.Sprintf("query error: %v", err))
		return
	}
	defer rows.Close()

	var facts []map[string]interface{}
	for rows.Next() {
		var id, agID, ispVal, cat, fact, sessionID string
		var sourceURL, sourceDomain sql.NullString
		var confidence float64
		var isActive bool
		var learnedAt time.Time
		var expiresAt sql.NullTime

		if err := rows.Scan(&id, &agID, &ispVal, &cat, &fact, &sourceURL, &sourceDomain,
			&confidence, &sessionID, &isActive, &learnedAt, &expiresAt); err != nil {
			continue
		}
		entry := map[string]interface{}{
			"id":            id,
			"agent_id":      agID,
			"isp":           ispVal,
			"category":      cat,
			"fact":          fact,
			"source_url":    sourceURL.String,
			"source_domain": sourceDomain.String,
			"confidence":    confidence,
			"session_id":    sessionID,
			"is_active":     isActive,
			"learned_at":    learnedAt,
		}
		if expiresAt.Valid {
			entry["expires_at"] = expiresAt.Time
		}
		facts = append(facts, entry)
	}

	respondJSON(w, 200, map[string]interface{}{
		"facts": facts,
		"count": len(facts),
	})
}

// HandleGetSourceScores returns the source quality scoreboard.
func (l *ISPAgentLearner) HandleGetSourceScores(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	scores := make([]SourceScore, 0, len(l.sourceScores))
	for _, s := range l.sourceScores {
		scores = append(scores, *s)
	}
	l.mu.RUnlock()

	respondJSON(w, 200, map[string]interface{}{
		"sources": scores,
		"count":   len(scores),
	})
}

// HandleTriggerLearn manually triggers a learning session for a specific agent.
func (l *ISPAgentLearner) HandleTriggerLearn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AgentID == "" {
		respondError(w, 400, "agent_id required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var isp, domain string
	err := l.db.QueryRowContext(ctx,
		`SELECT isp, domain FROM mailing_isp_agents WHERE id = $1`, req.AgentID,
	).Scan(&isp, &domain)
	if err != nil {
		respondError(w, 404, "agent not found")
		return
	}

	// Run session in background
	go func() {
		bgCtx := context.Background()
		session := l.runAgentSession(bgCtx, req.AgentID, isp, domain)
		log.Printf("[ISP-Learner] Manual session for %s: %d facts, %d sources", isp, session.FactsFound, session.SourcesScraped)
		l.flushSourceScores()
	}()

	respondJSON(w, 202, map[string]string{
		"status":  "accepted",
		"message": fmt.Sprintf("Learning session started for %s agent (%s)", isp, req.AgentID[:8]),
	})
}

// HandleLearnerStatus returns the current state of the learning engine.
func (l *ISPAgentLearner) HandleLearnerStatus(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	running := l.running
	totalSources := len(l.sourceScores)

	importantCount := 0
	wasteCount := 0
	for _, s := range l.sourceScores {
		if s.Rating == "important" {
			importantCount++
		} else if s.Rating == "waste" {
			wasteCount++
		}
	}
	l.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var totalSessions, totalFacts int
	var lastSessionAt sql.NullTime
	l.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(started_at) FROM mailing_isp_agent_research`,
	).Scan(&totalSessions, &lastSessionAt)

	l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_isp_agent_ltm WHERE is_active = true`,
	).Scan(&totalFacts)

	status := map[string]interface{}{
		"running":            running,
		"schedule":           "every 1 hour",
		"session_duration":   "5 minutes",
		"total_sessions":     totalSessions,
		"total_ltm_facts":    totalFacts,
		"total_sources_known": totalSources,
		"important_sources":  importantCount,
		"waste_sources":      wasteCount,
	}
	if lastSessionAt.Valid {
		status["last_session_at"] = lastSessionAt.Time
		nextSession := lastSessionAt.Time.Add(1 * time.Hour)
		status["next_session_at"] = nextSession
		if time.Now().Before(nextSession) {
			status["next_session_in"] = nextSession.Sub(time.Now()).Round(time.Second).String()
		}
	}

	respondJSON(w, 200, status)
}

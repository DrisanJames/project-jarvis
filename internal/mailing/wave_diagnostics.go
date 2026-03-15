package mailing

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// GenerationReport captures diagnostic data for a full generation run.
type GenerationReport struct {
	Brand           string           `json:"brand"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at"`
	TotalDurationMs int64            `json:"total_duration_ms"`
	ScrapeReport    ScrapeReport     `json:"scrape_report"`
	Phase1Report    PhaseReport      `json:"phase1_editorial"`
	Phase2Report    PhaseReport      `json:"phase2_html_render"`
	WaveReports     []WaveReport     `json:"wave_reports"`
	Errors          []string         `json:"errors,omitempty"`
	Version         string           `json:"version"`
}

// ScrapeReport captures deep scraper results.
type ScrapeReport struct {
	Domain            string `json:"domain"`
	PostsFound        int    `json:"posts_found"`
	PostsWithFullText int    `json:"posts_with_full_text"`
	AvgFullTextWords  int    `json:"avg_full_text_words"`
	UsedFallback      bool   `json:"used_fallback"`
	DurationMs        int64  `json:"duration_ms"`
}

// PhaseReport captures timing and model info for an AI phase.
type PhaseReport struct {
	Model      string `json:"model"`
	DurationMs int64  `json:"duration_ms"`
	Attempts   int    `json:"attempts"`
	PromptLen  int    `json:"prompt_length_chars"`
	OutputLen  int    `json:"output_length_chars"`
}

// WaveReport captures per-wave quality diagnostics.
type WaveReport struct {
	WaveIndex        int      `json:"wave_index"`
	Subject          string   `json:"subject"`
	SubjectLen       int      `json:"subject_length"`
	PreviewText      string   `json:"preview_text"`
	PreviewLen       int      `json:"preview_length"`
	HTMLLen          int      `json:"html_length"`
	ArticleCount     int      `json:"article_count"`
	HasBrandHeader   bool     `json:"has_brand_header"`
	HasUnsubLink     bool     `json:"has_unsub_link"`
	HasMergeTags     bool     `json:"has_merge_tags"`
	HasRealURLs      bool     `json:"has_real_urls"`
	TemplateMatch    float64  `json:"template_match_score"`
	Issues           []string `json:"issues,omitempty"`
}

const GeneratorVersion = "3.0.0"

// BuildScrapeReport creates a diagnostic report from scrape results.
func BuildScrapeReport(domain string, brand *BrandIntelligence, usedFallback bool, dur time.Duration) ScrapeReport {
	r := ScrapeReport{
		Domain:       domain,
		DurationMs:   dur.Milliseconds(),
		UsedFallback: usedFallback,
	}
	if brand != nil {
		r.PostsFound = len(brand.BlogPosts)
		totalWords := 0
		for _, p := range brand.BlogPosts {
			if p.FullText != "" {
				r.PostsWithFullText++
				totalWords += len(strings.Fields(p.FullText))
			}
		}
		if r.PostsWithFullText > 0 {
			r.AvgFullTextWords = totalWords / r.PostsWithFullText
		}
	}
	return r
}

// ValidateWave checks a single wave variation for quality and brand conformance.
func ValidateWave(v WaveVariation, brandKey string, contentPool []BlogExcerpt) WaveReport {
	r := WaveReport{
		WaveIndex:   v.WaveIndex,
		Subject:     v.Subject,
		SubjectLen:  len(v.Subject),
		PreviewText: v.PreviewText,
		PreviewLen:  len(v.PreviewText),
		HTMLLen:     len(v.HTMLContent),
	}

	html := v.HTMLContent
	htmlLower := strings.ToLower(html)

	// Check subject length
	if r.SubjectLen > 60 {
		r.Issues = append(r.Issues, fmt.Sprintf("subject too long: %d chars (max 60)", r.SubjectLen))
	}
	if r.SubjectLen == 0 {
		r.Issues = append(r.Issues, "subject is empty")
	}

	// Check preview text
	if r.PreviewLen > 100 {
		r.Issues = append(r.Issues, fmt.Sprintf("preview_text too long: %d chars (max 100)", r.PreviewLen))
	}

	// Brand header check (case-insensitive, allows minor color variations)
	switch brandKey {
	case "discountblog":
		hasName := strings.Contains(htmlLower, "discount") && strings.Contains(htmlLower, "blog")
		hasCoral := strings.Contains(htmlLower, "#ff7") || strings.Contains(htmlLower, "#ff6") || strings.Contains(htmlLower, "coral")
		r.HasBrandHeader = hasName && hasCoral
	case "quizfiesta":
		r.HasBrandHeader = (strings.Contains(htmlLower, "quiz") && strings.Contains(htmlLower, "fiesta")) ||
			strings.Contains(htmlLower, "quizfiesta")
	default:
		r.HasBrandHeader = true
	}
	if !r.HasBrandHeader {
		r.Issues = append(r.Issues, "brand header not detected in HTML")
	}

	// Unsubscribe link
	r.HasUnsubLink = strings.Contains(html, "system.unsubscribe_url")
	if !r.HasUnsubLink {
		r.Issues = append(r.Issues, "missing unsubscribe link ({{ system.unsubscribe_url }})")
	}

	// Merge tags
	r.HasMergeTags = strings.Contains(html, "first_name") || strings.Contains(html, "email")

	// Real URLs from content pool
	for _, p := range contentPool {
		if p.URL != "" && strings.Contains(html, p.URL) {
			r.HasRealURLs = true
			break
		}
	}
	if !r.HasRealURLs && len(contentPool) > 0 {
		r.Issues = append(r.Issues, "no real URLs from content pool found in HTML")
	}

	// Count article sections (rough: look for CTA links or major headings)
	r.ArticleCount = strings.Count(htmlLower, "</a>") - 2 // subtract footer links
	if r.ArticleCount < 0 {
		r.ArticleCount = 0
	}

	// Template match score
	r.TemplateMatch = computeTemplateMatch(html, brandKey)

	if r.TemplateMatch < 0.5 {
		r.Issues = append(r.Issues, fmt.Sprintf("low template match score: %.2f (expected >0.7)", r.TemplateMatch))
	}

	return r
}

// computeTemplateMatch scores how well the HTML matches the expected brand template.
func computeTemplateMatch(html, brandKey string) float64 {
	var markers []string

	switch brandKey {
	case "discountblog":
		markers = []string{
			"#FF7B7B", "#5FCCB8", "Georgia",
			"discountblog.com", "discount",
			"#FAFAFA", "#E5E7EB",
			"serif", "#1F2937", "#6B7280",
		}
	case "quizfiesta":
		markers = []string{
			"#8B5CF6", "#39FF14", "#0A0014",
			"Courier New", "monospace",
			"QUIZ", "FIESTA", "#FFD700",
			"PLAYER_ONE", "INSERT COIN",
		}
	default:
		return 1.0
	}

	found := 0
	htmlLower := strings.ToLower(html)
	for _, m := range markers {
		if strings.Contains(htmlLower, strings.ToLower(m)) {
			found++
		}
	}

	return float64(found) / float64(len(markers))
}

// LogReport prints the generation report as structured log lines.
func LogReport(r *GenerationReport) {
	log.Printf("[wave-diag] ====== Generation Report: %s (v%s) ======", r.Brand, r.Version)
	log.Printf("[wave-diag] Duration: %dms total | Scrape: %dms | Phase1: %dms | Phase2: %dms",
		r.TotalDurationMs, r.ScrapeReport.DurationMs, r.Phase1Report.DurationMs, r.Phase2Report.DurationMs)
	log.Printf("[wave-diag] Scrape: %d posts found, %d with full text (avg %d words), fallback=%v",
		r.ScrapeReport.PostsFound, r.ScrapeReport.PostsWithFullText, r.ScrapeReport.AvgFullTextWords, r.ScrapeReport.UsedFallback)
	log.Printf("[wave-diag] Phase1: model=%s, attempts=%d, prompt=%d chars, output=%d chars",
		r.Phase1Report.Model, r.Phase1Report.Attempts, r.Phase1Report.PromptLen, r.Phase1Report.OutputLen)
	log.Printf("[wave-diag] Phase2: model=%s, attempts=%d, prompt=%d chars, output=%d chars",
		r.Phase2Report.Model, r.Phase2Report.Attempts, r.Phase2Report.PromptLen, r.Phase2Report.OutputLen)

	for _, w := range r.WaveReports {
		status := "OK"
		if len(w.Issues) > 0 {
			status = fmt.Sprintf("WARN(%d)", len(w.Issues))
		}
		log.Printf("[wave-diag] Wave %d [%s]: subj=%d chars, html=%d chars, articles=%d, brand=%v, unsub=%v, urls=%v, template=%.0f%%",
			w.WaveIndex, status, w.SubjectLen, w.HTMLLen, w.ArticleCount,
			w.HasBrandHeader, w.HasUnsubLink, w.HasRealURLs, w.TemplateMatch*100)
		for _, issue := range w.Issues {
			log.Printf("[wave-diag]   ISSUE: %s", issue)
		}
	}

	if len(r.Errors) > 0 {
		for _, e := range r.Errors {
			log.Printf("[wave-diag] ERROR: %s", e)
		}
	}
	log.Printf("[wave-diag] ====== End Report ======")
}

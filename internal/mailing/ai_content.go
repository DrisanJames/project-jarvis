package mailing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// AIContentService provides AI-powered content optimization for email campaigns
type AIContentService struct {
	db           *sql.DB
	anthropicKey string
	openaiKey    string
	defaultModel string
	httpClient   *http.Client
}

// NewAIContentService creates a new AI content service
func NewAIContentService(db *sql.DB, anthropicKey, openaiKey string) *AIContentService {
	model := "claude-sonnet-4-20250514"
	if anthropicKey == "" && openaiKey != "" {
		model = "gpt-4o"
	}
	return &AIContentService{
		db:           db,
		anthropicKey: anthropicKey,
		openaiKey:    openaiKey,
		defaultModel: model,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// SubjectParams contains parameters for subject line generation
type SubjectParams struct {
	HTMLContent   string `json:"html_content"`
	ProductInfo   string `json:"product_info"`
	TargetTone    string `json:"target_tone"`    // professional, casual, urgent, curious
	AudienceType  string `json:"audience_type"`  // b2b, b2c, mixed
	MaxLength     int    `json:"max_length"`
	Count         int    `json:"count"`
	IncludeEmoji  bool   `json:"include_emoji"`
	Industry      string `json:"industry"`
	CampaignType  string `json:"campaign_type"` // newsletter, promotional, transactional, announcement
}

// SubjectSuggestion represents a generated subject line suggestion
type SubjectSuggestion struct {
	Subject           string  `json:"subject"`
	PredictedOpenRate float64 `json:"predicted_open_rate"`
	Tone              string  `json:"tone"` // professional, casual, urgent, curious
	Reasoning         string  `json:"reasoning"`
}

// ContentAnalysis represents the analysis of email content
type ContentAnalysis struct {
	SpamScore        float64  `json:"spam_score"`        // 0-100
	ReadabilityScore float64  `json:"readability_score"` // Flesch-Kincaid
	SentimentScore   float64  `json:"sentiment_score"`   // -1 to 1
	SpamTriggers     []string `json:"spam_triggers"`     // Words that may trigger spam filters
	Suggestions      []string `json:"suggestions"`
	WordCount        int      `json:"word_count"`
	ImageTextRatio   float64  `json:"image_text_ratio"`
}

// PerformancePrediction represents predicted campaign performance
type PerformancePrediction struct {
	OpenRate        float64            `json:"predicted_open_rate"`
	ClickRate       float64            `json:"predicted_click_rate"`
	UnsubscribeRate float64            `json:"predicted_unsubscribe_rate"`
	Confidence      float64            `json:"confidence"`
	Factors         []PredictionFactor `json:"factors"`
}

// PredictionFactor represents a factor affecting performance prediction
type PredictionFactor struct {
	Factor      string  `json:"factor"`
	Impact      float64 `json:"impact"` // positive or negative
	Description string  `json:"description"`
}

// SegmentRecommendation represents a recommended segment for targeting
type SegmentRecommendation struct {
	SegmentName    string   `json:"segment_name"`
	EstimatedSize  int      `json:"estimated_size"`
	PredictedValue float64  `json:"predicted_value"`
	Criteria       []string `json:"criteria"`
	Reasoning      string   `json:"reasoning"`
}

// Common spam trigger words
var spamTriggerWords = []string{
	// Money/Finance
	"free", "winner", "cash", "money", "credit", "debt", "mortgage", "loan",
	"earn", "income", "profit", "investment", "wealthy", "rich", "million",
	"billion", "fortune", "jackpot", "prize", "bonus", "discount", "cheap",
	"bargain", "deal", "save", "cost", "price", "affordable", "budget",

	// Urgency
	"urgent", "act now", "limited time", "expires", "deadline", "hurry",
	"immediately", "instant", "now", "quick", "fast", "rush", "asap",
	"don't miss", "last chance", "final", "ending soon", "today only",
	"while supplies last", "order now", "call now", "apply now",

	// Spam-like phrases
	"click here", "click below", "open immediately", "read immediately",
	"no obligation", "no cost", "no fees", "no catch", "no risk",
	"100%", "guarantee", "guaranteed", "satisfaction guaranteed",
	"risk free", "risk-free", "double your", "triple your",
	"as seen on", "amazing", "incredible", "unbelievable", "miracle",
	"secret", "hidden", "exclusive", "special", "selected", "chosen",

	// Medical/Health
	"weight loss", "lose weight", "diet", "pills", "cure", "remedy",
	"medicine", "prescription", "pharmacy", "viagra", "cialis",

	// Suspicious actions
	"remove", "unsubscribe", "opt out", "stop receiving",
	"spam", "not spam", "bulk email", "mass email",

	// Personal/Legal
	"nigerian", "prince", "inheritance", "lawyer", "attorney",
	"confidential", "private", "personal", "sensitive",

	// Caps/Punctuation patterns (detected separately)
	// ALL CAPS, !!!, $$$, %%%
}

// GenerateSubjectLines generates AI-powered subject line suggestions
func (s *AIContentService) GenerateSubjectLines(ctx context.Context, params SubjectParams) ([]SubjectSuggestion, error) {
	// Set defaults
	if params.Count == 0 {
		params.Count = 5
	}
	if params.MaxLength == 0 {
		params.MaxLength = 60
	}
	if params.TargetTone == "" {
		params.TargetTone = "professional"
	}

	// Fetch historical performance data for context
	historicalData := s.fetchHistoricalPerformance(ctx)

	// Build prompt
	prompt := s.buildSubjectLinePrompt(params, historicalData)

	// Try Anthropic first
	if s.anthropicKey != "" {
		suggestions, err := s.callAnthropicForSubjects(ctx, prompt, params.Count)
		if err == nil {
			return suggestions, nil
		}
		log.Printf("Anthropic failed, falling back to OpenAI: %v", err)
	}

	// Fall back to OpenAI
	if s.openaiKey != "" {
		suggestions, err := s.callOpenAIForSubjects(ctx, prompt, params.Count)
		if err == nil {
			return suggestions, nil
		}
		log.Printf("OpenAI failed: %v", err)
	}

	// Return fallback suggestions if both fail
	return s.getFallbackSubjectSuggestions(params), nil
}

// AnalyzeContent analyzes email content for spam triggers, readability, and sentiment
func (s *AIContentService) AnalyzeContent(ctx context.Context, htmlContent string) (*ContentAnalysis, error) {
	analysis := &ContentAnalysis{
		SpamTriggers: []string{},
		Suggestions:  []string{},
	}

	// Extract text from HTML
	text := s.extractTextFromHTML(htmlContent)
	analysis.WordCount = s.countWords(text)

	// Calculate readability score (Flesch-Kincaid)
	analysis.ReadabilityScore = s.calculateReadability(text)

	// Detect spam triggers
	analysis.SpamTriggers = s.detectSpamTriggers(text)
	analysis.SpamScore = s.calculateSpamScore(htmlContent, analysis.SpamTriggers)

	// Calculate image-to-text ratio
	analysis.ImageTextRatio = s.calculateImageTextRatio(htmlContent, analysis.WordCount)

	// Get AI-powered sentiment analysis
	sentiment, err := s.analyzeContentWithAI(ctx, text)
	if err != nil {
		log.Printf("AI sentiment analysis failed: %v", err)
		analysis.SentimentScore = 0.0 // Neutral default
	} else {
		analysis.SentimentScore = sentiment
	}

	// Generate suggestions based on analysis
	analysis.Suggestions = s.generateContentSuggestions(analysis)

	return analysis, nil
}

// PredictPerformance predicts campaign performance based on content and historical data
func (s *AIContentService) PredictPerformance(ctx context.Context, campaign *Campaign) (*PerformancePrediction, error) {
	prediction := &PerformancePrediction{
		Factors: []PredictionFactor{},
	}

	// Get historical metrics for this organization
	historicalMetrics := s.getOrgHistoricalMetrics(ctx, campaign.OrganizationID)

	// Base rates from historical data
	baseOpenRate := historicalMetrics.avgOpenRate
	baseClickRate := historicalMetrics.avgClickRate
	baseUnsubRate := historicalMetrics.avgUnsubRate

	// Analyze content factors
	contentAnalysis, _ := s.AnalyzeContent(ctx, campaign.HTMLContent)

	// Factor 1: Subject line length
	subjectLen := len(campaign.Subject)
	if subjectLen >= 30 && subjectLen <= 50 {
		prediction.Factors = append(prediction.Factors, PredictionFactor{
			Factor:      "subject_length",
			Impact:      0.05,
			Description: "Optimal subject line length (30-50 chars)",
		})
		baseOpenRate *= 1.05
	} else if subjectLen > 60 {
		prediction.Factors = append(prediction.Factors, PredictionFactor{
			Factor:      "subject_length",
			Impact:      -0.10,
			Description: "Subject line too long (>60 chars may be truncated)",
		})
		baseOpenRate *= 0.90
	}

	// Factor 2: Spam score
	if contentAnalysis != nil {
		if contentAnalysis.SpamScore > 50 {
			prediction.Factors = append(prediction.Factors, PredictionFactor{
				Factor:      "spam_risk",
				Impact:      -0.20,
				Description: fmt.Sprintf("High spam score (%.0f) - may impact deliverability", contentAnalysis.SpamScore),
			})
			baseOpenRate *= 0.80
		} else if contentAnalysis.SpamScore < 20 {
			prediction.Factors = append(prediction.Factors, PredictionFactor{
				Factor:      "spam_risk",
				Impact:      0.05,
				Description: "Low spam risk - good deliverability expected",
			})
			baseOpenRate *= 1.05
		}

		// Factor 3: Readability
		if contentAnalysis.ReadabilityScore >= 60 && contentAnalysis.ReadabilityScore <= 80 {
			prediction.Factors = append(prediction.Factors, PredictionFactor{
				Factor:      "readability",
				Impact:      0.03,
				Description: "Good readability score - content is accessible",
			})
			baseClickRate *= 1.03
		}

		// Factor 4: Image-text ratio
		if contentAnalysis.ImageTextRatio > 0.6 {
			prediction.Factors = append(prediction.Factors, PredictionFactor{
				Factor:      "image_heavy",
				Impact:      -0.15,
				Description: "Image-heavy email may trigger spam filters",
			})
			baseOpenRate *= 0.85
		}
	}

	// Factor 5: Personalization
	if strings.Contains(campaign.Subject, "{{") || strings.Contains(campaign.HTMLContent, "{{") {
		prediction.Factors = append(prediction.Factors, PredictionFactor{
			Factor:      "personalization",
			Impact:      0.15,
			Description: "Personalization detected - increases engagement",
		})
		baseOpenRate *= 1.15
		baseClickRate *= 1.10
	}

	// Factor 6: Send time optimization
	if campaign.AISendTimeOptimization {
		prediction.Factors = append(prediction.Factors, PredictionFactor{
			Factor:      "send_time_optimization",
			Impact:      0.10,
			Description: "AI send time optimization enabled",
		})
		baseOpenRate *= 1.10
	}

	// Factor 7: Preview text
	if campaign.PreviewText != "" {
		prediction.Factors = append(prediction.Factors, PredictionFactor{
			Factor:      "preview_text",
			Impact:      0.08,
			Description: "Preview text adds context in inbox",
		})
		baseOpenRate *= 1.08
	}

	// Calculate confidence based on historical data availability
	confidence := 0.5
	if historicalMetrics.sampleSize > 1000 {
		confidence = 0.85
	} else if historicalMetrics.sampleSize > 100 {
		confidence = 0.70
	} else if historicalMetrics.sampleSize > 10 {
		confidence = 0.55
	}

	prediction.OpenRate = math.Min(baseOpenRate, 100)
	prediction.ClickRate = math.Min(baseClickRate, prediction.OpenRate)
	prediction.UnsubscribeRate = math.Min(baseUnsubRate, 5)
	prediction.Confidence = confidence

	return prediction, nil
}

// RecommendSegments recommends audience segments based on goal
func (s *AIContentService) RecommendSegments(ctx context.Context, orgID string, goal string) ([]SegmentRecommendation, error) {
	recommendations := []SegmentRecommendation{}

	// Parse org ID
	parsedOrgID, err := uuid.Parse(orgID)
	if err != nil {
		return nil, fmt.Errorf("invalid org ID: %w", err)
	}

	// Get subscriber statistics
	stats := s.getSubscriberStats(ctx, parsedOrgID)

	// Goal-based recommendations
	switch strings.ToLower(goal) {
	case "revenue", "sales", "conversions":
		// Focus on high-engagement subscribers
		if stats.highEngagement > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "High-Value Engaged",
				EstimatedSize:  stats.highEngagement,
				PredictedValue: 0.025, // $0.025 per send
				Criteria: []string{
					"engagement_score >= 70",
					"total_opens >= 5",
					"last_open_at > 30 days ago",
				},
				Reasoning: "High-engagement subscribers have 3x higher conversion rates. Prioritize for revenue-focused campaigns.",
			})
		}

		// Recent clickers
		if stats.recentClickers > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "Recent Clickers",
				EstimatedSize:  stats.recentClickers,
				PredictedValue: 0.035,
				Criteria: []string{
					"last_click_at > 14 days ago",
					"total_clicks >= 2",
				},
				Reasoning: "Subscribers who clicked recently show strong purchase intent. Best segment for conversion campaigns.",
			})
		}

	case "engagement", "open_rate":
		// Active openers
		recommendations = append(recommendations, SegmentRecommendation{
			SegmentName:    "Active Openers",
			EstimatedSize:  stats.mediumEngagement + stats.highEngagement,
			PredictedValue: 0.015,
			Criteria: []string{
				"engagement_score >= 40",
				"last_open_at > 60 days ago",
			},
			Reasoning: "Consistent openers maintain healthy engagement metrics. Good for maintaining sender reputation.",
		})

		// Re-engagement candidates
		if stats.dormant > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "Re-engagement Candidates",
				EstimatedSize:  stats.dormant,
				PredictedValue: 0.005,
				Criteria: []string{
					"last_open_at BETWEEN 60 and 120 days ago",
					"total_opens >= 3",
				},
				Reasoning: "Previously engaged subscribers who went dormant. Worth a re-engagement campaign before removing.",
			})
		}

	case "list_growth", "retention":
		// New subscribers
		if stats.newSubscribers > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "Welcome Series",
				EstimatedSize:  stats.newSubscribers,
				PredictedValue: 0.020,
				Criteria: []string{
					"subscribed_at > 30 days ago",
					"total_emails_received < 5",
				},
				Reasoning: "New subscribers need nurturing. Welcome series converts 50% better than regular campaigns.",
			})
		}

		// At-risk subscribers
		if stats.atRisk > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "At-Risk Subscribers",
				EstimatedSize:  stats.atRisk,
				PredictedValue: 0.008,
				Criteria: []string{
					"engagement_score < 30",
					"last_open_at > 90 days ago",
					"total_emails_received >= 10",
				},
				Reasoning: "These subscribers may unsubscribe soon. Consider a win-back campaign or sunset them to protect deliverability.",
			})
		}

	default:
		// Default: Tiered recommendations
		if stats.highEngagement > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "VIP Subscribers",
				EstimatedSize:  stats.highEngagement,
				PredictedValue: 0.030,
				Criteria: []string{
					"engagement_score >= 70",
				},
				Reasoning: "Your most engaged subscribers. Best for exclusive offers and early access campaigns.",
			})
		}

		if stats.mediumEngagement > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "Regular Engagers",
				EstimatedSize:  stats.mediumEngagement,
				PredictedValue: 0.015,
				Criteria: []string{
					"engagement_score BETWEEN 30 AND 69",
				},
				Reasoning: "Consistent but not highly engaged. Good for general newsletters and promotions.",
			})
		}

		if stats.lowEngagement > 0 {
			recommendations = append(recommendations, SegmentRecommendation{
				SegmentName:    "Low Engagement",
				EstimatedSize:  stats.lowEngagement,
				PredictedValue: 0.005,
				Criteria: []string{
					"engagement_score < 30",
					"last_open_at > 30 days ago",
				},
				Reasoning: "Low engagement subscribers. Consider reducing frequency or running re-engagement campaigns.",
			})
		}
	}

	// Sort by predicted value
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].PredictedValue > recommendations[j].PredictedValue
	})

	return recommendations, nil
}

// ImproveContent suggests improvements for email content
func (s *AIContentService) ImproveContent(ctx context.Context, htmlContent string, goal string) (string, []string, error) {
	// First analyze the current content
	analysis, err := s.AnalyzeContent(ctx, htmlContent)
	if err != nil {
		return "", nil, fmt.Errorf("content analysis failed: %w", err)
	}

	improvements := []string{}

	// Generate improvement suggestions based on analysis
	if analysis.SpamScore > 30 {
		improvements = append(improvements, fmt.Sprintf("Reduce spam triggers: %s", strings.Join(analysis.SpamTriggers[:min(3, len(analysis.SpamTriggers))], ", ")))
	}

	if analysis.ReadabilityScore < 50 {
		improvements = append(improvements, "Simplify language - aim for 8th-grade reading level")
	} else if analysis.ReadabilityScore > 90 {
		improvements = append(improvements, "Content may be too simple - consider adding more detail")
	}

	if analysis.WordCount < 50 {
		improvements = append(improvements, "Consider adding more content - emails under 50 words often underperform")
	} else if analysis.WordCount > 500 {
		improvements = append(improvements, "Consider shortening content - long emails see drop-off in engagement")
	}

	if analysis.ImageTextRatio > 0.6 {
		improvements = append(improvements, "Reduce image-to-text ratio - aim for 40/60 image/text balance")
	}

	// Try AI-powered content improvement
	if s.anthropicKey != "" || s.openaiKey != "" {
		improvedContent, aiSuggestions, err := s.getAIContentImprovements(ctx, htmlContent, goal, analysis)
		if err == nil {
			improvements = append(improvements, aiSuggestions...)
			return improvedContent, improvements, nil
		}
		log.Printf("AI content improvement failed: %v", err)
	}

	return htmlContent, improvements, nil
}

// GenerateCTAs generates call-to-action suggestions
func (s *AIContentService) GenerateCTAs(ctx context.Context, productInfo string, count int) ([]string, error) {
	if count == 0 {
		count = 5
	}

	// Try AI-powered CTA generation
	if s.anthropicKey != "" {
		ctas, err := s.generateCTAsWithAnthropic(ctx, productInfo, count)
		if err == nil {
			return ctas, nil
		}
		log.Printf("Anthropic CTA generation failed: %v", err)
	}

	if s.openaiKey != "" {
		ctas, err := s.generateCTAsWithOpenAI(ctx, productInfo, count)
		if err == nil {
			return ctas, nil
		}
		log.Printf("OpenAI CTA generation failed: %v", err)
	}

	// Fallback CTAs
	return []string{
		"Shop Now",
		"Learn More",
		"Get Started",
		"Claim Your Offer",
		"See Details",
	}, nil
}

// Helper methods

func (s *AIContentService) extractTextFromHTML(html string) string {
	// Remove script tags with content
	reScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	text := reScript.ReplaceAllString(html, "")

	// Remove style tags with content
	reStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	text = reStyle.ReplaceAllString(text, "")

	// Remove HTML tags
	reTags := regexp.MustCompile(`<[^>]+>`)
	text = reTags.ReplaceAllString(text, " ")

	// Decode HTML entities
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")

	// Normalize whitespace
	reWhitespace := regexp.MustCompile(`\s+`)
	text = reWhitespace.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}

func (s *AIContentService) countWords(text string) int {
	words := strings.Fields(text)
	return len(words)
}

func (s *AIContentService) calculateReadability(text string) float64 {
	// Flesch-Kincaid Reading Ease
	// 206.835 - 1.015 * (words/sentences) - 84.6 * (syllables/words)

	words := strings.Fields(text)
	if len(words) == 0 {
		return 50.0
	}

	// Count sentences (roughly by punctuation)
	sentences := float64(strings.Count(text, ".") + strings.Count(text, "!") + strings.Count(text, "?"))
	if sentences == 0 {
		sentences = 1
	}

	// Count syllables (approximation)
	totalSyllables := 0
	for _, word := range words {
		totalSyllables += s.countSyllables(word)
	}

	wordsCount := float64(len(words))
	syllablesPerWord := float64(totalSyllables) / wordsCount
	wordsPerSentence := wordsCount / sentences

	score := 206.835 - 1.015*wordsPerSentence - 84.6*syllablesPerWord

	// Clamp to 0-100 range
	return math.Max(0, math.Min(100, score))
}

func (s *AIContentService) countSyllables(word string) int {
	word = strings.ToLower(word)
	if len(word) <= 3 {
		return 1
	}

	// Count vowel groups
	vowels := "aeiouy"
	count := 0
	prevVowel := false

	for _, char := range word {
		isVowel := strings.ContainsRune(vowels, char)
		if isVowel && !prevVowel {
			count++
		}
		prevVowel = isVowel
	}

	// Adjust for silent e
	if strings.HasSuffix(word, "e") {
		count--
	}

	// Minimum 1 syllable
	if count < 1 {
		count = 1
	}

	return count
}

func (s *AIContentService) detectSpamTriggers(text string) []string {
	triggers := []string{}
	textLower := strings.ToLower(text)

	for _, trigger := range spamTriggerWords {
		if strings.Contains(textLower, strings.ToLower(trigger)) {
			triggers = append(triggers, trigger)
		}
	}

	// Check for ALL CAPS words
	words := strings.Fields(text)
	capsCount := 0
	for _, word := range words {
		if len(word) > 3 && word == strings.ToUpper(word) && isAlpha(word) {
			capsCount++
		}
	}
	if capsCount > 2 {
		triggers = append(triggers, "EXCESSIVE CAPS")
	}

	// Check for excessive punctuation
	if strings.Count(text, "!") > 3 {
		triggers = append(triggers, "excessive exclamation marks")
	}
	if strings.Count(text, "$") > 2 {
		triggers = append(triggers, "excessive dollar signs")
	}

	return triggers
}

func isAlpha(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func (s *AIContentService) calculateSpamScore(html string, triggers []string) float64 {
	score := 0.0

	// Each trigger adds to spam score
	score += float64(len(triggers)) * 5

	// Check for suspicious patterns
	htmlLower := strings.ToLower(html)

	// Invisible text
	if strings.Contains(htmlLower, "display:none") || strings.Contains(htmlLower, "visibility:hidden") {
		score += 15
	}

	// Tiny font sizes
	if strings.Contains(htmlLower, "font-size:0") || strings.Contains(htmlLower, "font-size: 0") {
		score += 15
	}

	// External tracking pixels (suspicious quantity)
	imgCount := strings.Count(htmlLower, "<img")
	if imgCount > 10 {
		score += float64(imgCount-10) * 2
	}

	// Form elements (unusual in marketing emails)
	if strings.Contains(htmlLower, "<form") {
		score += 10
	}

	// JavaScript (should never be in marketing emails)
	if strings.Contains(htmlLower, "<script") {
		score += 25
	}

	return math.Min(100, score)
}

func (s *AIContentService) calculateImageTextRatio(html string, wordCount int) float64 {
	// Count images
	imgCount := strings.Count(strings.ToLower(html), "<img")

	if wordCount == 0 && imgCount > 0 {
		return 1.0
	}
	if imgCount == 0 {
		return 0.0
	}

	// Rough estimation: assume average image "replaces" ~50 words of content
	imgEquivalentWords := float64(imgCount * 50)
	totalContent := float64(wordCount) + imgEquivalentWords

	return imgEquivalentWords / totalContent
}

func (s *AIContentService) generateContentSuggestions(analysis *ContentAnalysis) []string {
	suggestions := []string{}

	// Spam-related suggestions
	if analysis.SpamScore > 50 {
		suggestions = append(suggestions, "High spam risk detected. Remove or rephrase spam trigger words.")
	} else if analysis.SpamScore > 30 {
		suggestions = append(suggestions, "Moderate spam risk. Consider reviewing trigger words.")
	}

	// Readability suggestions
	if analysis.ReadabilityScore < 40 {
		suggestions = append(suggestions, "Content is difficult to read. Use shorter sentences and simpler words.")
	} else if analysis.ReadabilityScore < 60 {
		suggestions = append(suggestions, "Readability could be improved. Aim for 8th-grade reading level.")
	}

	// Length suggestions
	if analysis.WordCount < 50 {
		suggestions = append(suggestions, "Email is quite short. Consider adding more value-driven content.")
	} else if analysis.WordCount > 500 {
		suggestions = append(suggestions, "Email is long. Consider breaking into multiple emails or using bullet points.")
	}

	// Image ratio suggestions
	if analysis.ImageTextRatio > 0.7 {
		suggestions = append(suggestions, "Too many images relative to text. Add more text content for better deliverability.")
	} else if analysis.ImageTextRatio < 0.1 && analysis.WordCount > 100 {
		suggestions = append(suggestions, "Consider adding images to break up text and increase engagement.")
	}

	// Sentiment suggestions
	if analysis.SentimentScore < -0.3 {
		suggestions = append(suggestions, "Content has negative sentiment. Consider a more positive tone.")
	}

	return suggestions
}

type historicalPerformanceData struct {
	topSubjects []struct {
		Subject  string
		OpenRate float64
	}
	avgOpenRate  float64
	avgClickRate float64
}

func (s *AIContentService) fetchHistoricalPerformance(ctx context.Context) historicalPerformanceData {
	data := historicalPerformanceData{
		avgOpenRate:  15.0,
		avgClickRate: 2.5,
	}

	// Fetch top performing subjects
	query := `
		SELECT subject, 
			   CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END as open_rate
		FROM mailing_campaigns
		WHERE status = 'sent' AND sent_count >= 100
		ORDER BY (open_count::float / NULLIF(sent_count, 0)) DESC
		LIMIT 5
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var subject string
			var openRate float64
			if rows.Scan(&subject, &openRate) == nil {
				data.topSubjects = append(data.topSubjects, struct {
					Subject  string
					OpenRate float64
				}{subject, openRate})
			}
		}
	}

	// Fetch average rates
	avgQuery := `
		SELECT 
			COALESCE(AVG(CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END), 15) as avg_open,
			COALESCE(AVG(CASE WHEN sent_count > 0 THEN (click_count::float / sent_count::float) * 100 ELSE 0 END), 2.5) as avg_click
		FROM mailing_campaigns
		WHERE status = 'sent' AND sent_count >= 100
	`
	s.db.QueryRowContext(ctx, avgQuery).Scan(&data.avgOpenRate, &data.avgClickRate)

	return data
}

type orgHistoricalMetrics struct {
	avgOpenRate   float64
	avgClickRate  float64
	avgUnsubRate  float64
	sampleSize    int
}

func (s *AIContentService) getOrgHistoricalMetrics(ctx context.Context, orgID uuid.UUID) orgHistoricalMetrics {
	metrics := orgHistoricalMetrics{
		avgOpenRate:   15.0,
		avgClickRate:  2.5,
		avgUnsubRate:  0.3,
		sampleSize:    0,
	}

	query := `
		SELECT 
			COALESCE(AVG(CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END), 15) as avg_open,
			COALESCE(AVG(CASE WHEN sent_count > 0 THEN (click_count::float / sent_count::float) * 100 ELSE 0 END), 2.5) as avg_click,
			COALESCE(AVG(CASE WHEN sent_count > 0 THEN (unsubscribe_count::float / sent_count::float) * 100 ELSE 0 END), 0.3) as avg_unsub,
			COUNT(*) as sample_size
		FROM mailing_campaigns
		WHERE organization_id = $1 AND status = 'sent' AND sent_count >= 100
	`
	s.db.QueryRowContext(ctx, query, orgID).Scan(&metrics.avgOpenRate, &metrics.avgClickRate, &metrics.avgUnsubRate, &metrics.sampleSize)

	return metrics
}

type subscriberStats struct {
	total            int
	highEngagement   int
	mediumEngagement int
	lowEngagement    int
	recentClickers   int
	newSubscribers   int
	dormant          int
	atRisk           int
}

func (s *AIContentService) getSubscriberStats(ctx context.Context, orgID uuid.UUID) subscriberStats {
	stats := subscriberStats{}

	// Total subscribers
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed'`, orgID).Scan(&stats.total)

	// By engagement
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND engagement_score >= 70`, orgID).Scan(&stats.highEngagement)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND engagement_score >= 30 AND engagement_score < 70`, orgID).Scan(&stats.mediumEngagement)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND engagement_score < 30`, orgID).Scan(&stats.lowEngagement)

	// Recent clickers
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND last_click_at > NOW() - INTERVAL '14 days'`, orgID).Scan(&stats.recentClickers)

	// New subscribers
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND subscribed_at > NOW() - INTERVAL '30 days'`, orgID).Scan(&stats.newSubscribers)

	// Dormant
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND last_open_at < NOW() - INTERVAL '60 days' AND last_open_at > NOW() - INTERVAL '120 days'`, orgID).Scan(&stats.dormant)

	// At risk
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailing_subscribers WHERE organization_id = $1 AND status = 'confirmed' AND engagement_score < 30 AND last_open_at < NOW() - INTERVAL '90 days'`, orgID).Scan(&stats.atRisk)

	return stats
}

func (s *AIContentService) buildSubjectLinePrompt(params SubjectParams, historical historicalPerformanceData) string {
	var sb strings.Builder

	sb.WriteString("Generate email subject line suggestions based on the following:\n\n")

	if len(historical.topSubjects) > 0 {
		sb.WriteString("TOP PERFORMING SUBJECT LINES (for reference):\n")
		for _, ts := range historical.topSubjects {
			sb.WriteString(fmt.Sprintf("- \"%s\" (%.1f%% open rate)\n", ts.Subject, ts.OpenRate))
		}
		sb.WriteString("\n")
	}

	if params.HTMLContent != "" {
		contentSummary := s.extractTextFromHTML(params.HTMLContent)
		if len(contentSummary) > 500 {
			contentSummary = contentSummary[:500] + "..."
		}
		sb.WriteString(fmt.Sprintf("EMAIL CONTENT SUMMARY:\n%s\n\n", contentSummary))
	}

	if params.ProductInfo != "" {
		sb.WriteString(fmt.Sprintf("PRODUCT/OFFER INFO:\n%s\n\n", params.ProductInfo))
	}

	sb.WriteString(fmt.Sprintf("REQUIREMENTS:\n"))
	sb.WriteString(fmt.Sprintf("- Tone: %s\n", params.TargetTone))
	sb.WriteString(fmt.Sprintf("- Audience: %s\n", params.AudienceType))
	sb.WriteString(fmt.Sprintf("- Max length: %d characters\n", params.MaxLength))
	if params.IncludeEmoji {
		sb.WriteString("- Include 1-2 relevant emojis\n")
	}
	if params.Industry != "" {
		sb.WriteString(fmt.Sprintf("- Industry: %s\n", params.Industry))
	}
	if params.CampaignType != "" {
		sb.WriteString(fmt.Sprintf("- Campaign type: %s\n", params.CampaignType))
	}

	sb.WriteString(fmt.Sprintf("\nGenerate %d unique subject line suggestions.\n\n", params.Count))

	sb.WriteString(`For each suggestion, respond in this exact JSON format:
[
  {
    "subject": "The subject line text",
    "predicted_open_rate": 18.5,
    "tone": "professional",
    "reasoning": "Brief explanation of why this works"
  }
]

Tones: professional, casual, urgent, curious

Generate suggestions now:`)

	return sb.String()
}

func (s *AIContentService) callAnthropicForSubjects(ctx context.Context, prompt string, count int) ([]SubjectSuggestion, error) {
	reqBody := map[string]interface{}{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 2000,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": prompt,
			},
		},
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("x-api-key", s.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Anthropic error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var anthropicResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to parse Anthropic response: %w", err)
	}

	if len(anthropicResp.Content) == 0 {
		return nil, fmt.Errorf("no content in Anthropic response")
	}

	return s.parseSubjectSuggestionsJSON(anthropicResp.Content[0].Text)
}

func (s *AIContentService) callOpenAIForSubjects(ctx context.Context, prompt string, count int) ([]SubjectSuggestion, error) {
	reqBody := map[string]interface{}{
		"model": "gpt-4o",
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are an expert email marketing copywriter. Always respond with valid JSON.",
			},
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature": 0.8,
		"max_tokens":  2000,
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+s.openaiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OpenAI error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAI response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in OpenAI response")
	}

	return s.parseSubjectSuggestionsJSON(openAIResp.Choices[0].Message.Content)
}

func (s *AIContentService) parseSubjectSuggestionsJSON(content string) ([]SubjectSuggestion, error) {
	content = strings.TrimSpace(content)

	// Handle markdown code blocks
	if strings.HasPrefix(content, "```json") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	} else if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	}

	var suggestions []SubjectSuggestion
	if err := json.Unmarshal([]byte(content), &suggestions); err != nil {
		return nil, fmt.Errorf("failed to parse suggestions: %w", err)
	}

	return suggestions, nil
}

func (s *AIContentService) getFallbackSubjectSuggestions(params SubjectParams) []SubjectSuggestion {
	suggestions := []SubjectSuggestion{
		{
			Subject:           "{{ first_name }}, check out what's new",
			PredictedOpenRate: 18.0,
			Tone:              "professional",
			Reasoning:         "Personalization with curiosity element",
		},
		{
			Subject:           "Don't miss this exclusive offer",
			PredictedOpenRate: 17.5,
			Tone:              "urgent",
			Reasoning:         "Creates urgency and exclusivity",
		},
		{
			Subject:           "Quick update for you, {{ first_name }}",
			PredictedOpenRate: 19.0,
			Tone:              "casual",
			Reasoning:         "Personalized casual tone works well for engagement",
		},
		{
			Subject:           "Is this what you're looking for?",
			PredictedOpenRate: 20.5,
			Tone:              "curious",
			Reasoning:         "Question format drives curiosity and opens",
		},
		{
			Subject:           "Your weekly digest is here",
			PredictedOpenRate: 16.0,
			Tone:              "professional",
			Reasoning:         "Clear and straightforward for regular updates",
		},
	}

	if params.IncludeEmoji {
		suggestions[0].Subject = "✨ " + suggestions[0].Subject
		suggestions[1].Subject = "🔥 " + suggestions[1].Subject
		suggestions[2].Subject = "👋 " + suggestions[2].Subject
		suggestions[3].Subject = "🤔 " + suggestions[3].Subject
		suggestions[4].Subject = "📬 " + suggestions[4].Subject
	}

	if params.Count < len(suggestions) {
		return suggestions[:params.Count]
	}
	return suggestions
}

func (s *AIContentService) analyzeContentWithAI(ctx context.Context, text string) (float64, error) {
	if s.anthropicKey == "" && s.openaiKey == "" {
		return 0.0, fmt.Errorf("no AI API key configured")
	}

	prompt := fmt.Sprintf(`Analyze the sentiment of this email content and return ONLY a single number between -1 and 1.
-1 = very negative
0 = neutral
1 = very positive

Content:
%s

Respond with only the number, nothing else.`, text)

	var response string
	var err error

	if s.anthropicKey != "" {
		response, err = s.callAnthropicSimple(ctx, prompt)
		if err != nil && s.openaiKey != "" {
			response, err = s.callOpenAISimple(ctx, prompt)
		}
	} else {
		response, err = s.callOpenAISimple(ctx, prompt)
	}

	if err != nil {
		return 0.0, err
	}

	// Parse the number
	response = strings.TrimSpace(response)
	var sentiment float64
	if _, err := fmt.Sscanf(response, "%f", &sentiment); err != nil {
		return 0.0, fmt.Errorf("failed to parse sentiment: %v", err)
	}

	return math.Max(-1, math.Min(1, sentiment)), nil
}

func (s *AIContentService) callAnthropicSimple(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]interface{}{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 100,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("x-api-key", s.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Anthropic error: %s", string(respBody))
	}

	var anthropicResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &anthropicResp); err != nil {
		return "", err
	}
	if len(anthropicResp.Content) == 0 {
		return "", fmt.Errorf("no content")
	}
	return anthropicResp.Content[0].Text, nil
}

func (s *AIContentService) callOpenAISimple(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]interface{}{
		"model": "gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": 100,
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+s.openaiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI error: %s", string(respBody))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return "", err
	}
	if len(openAIResp.Choices) == 0 {
		return "", fmt.Errorf("no choices")
	}
	return openAIResp.Choices[0].Message.Content, nil
}

func (s *AIContentService) getAIContentImprovements(ctx context.Context, htmlContent string, goal string, analysis *ContentAnalysis) (string, []string, error) {
	text := s.extractTextFromHTML(htmlContent)

	prompt := fmt.Sprintf(`Improve this email content for the goal: %s

Current analysis:
- Spam score: %.0f
- Readability: %.0f
- Spam triggers found: %s

Content to improve:
%s

Provide:
1. Improved version of the content
2. List of specific improvements made

Respond in JSON:
{
  "improved_content": "...",
  "improvements": ["improvement 1", "improvement 2"]
}`, goal, analysis.SpamScore, analysis.ReadabilityScore, strings.Join(analysis.SpamTriggers, ", "), text)

	var response string
	var err error

	if s.anthropicKey != "" {
		response, err = s.callAnthropicSimple(ctx, prompt)
		if err != nil && s.openaiKey != "" {
			response, err = s.callOpenAISimple(ctx, prompt)
		}
	} else {
		response, err = s.callOpenAISimple(ctx, prompt)
	}

	if err != nil {
		return htmlContent, nil, err
	}

	// Parse response
	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		response = strings.TrimPrefix(response, "```json")
		response = strings.TrimPrefix(response, "```")
		response = strings.TrimSuffix(response, "```")
	}

	var result struct {
		ImprovedContent string   `json:"improved_content"`
		Improvements    []string `json:"improvements"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return htmlContent, nil, err
	}

	return result.ImprovedContent, result.Improvements, nil
}

func (s *AIContentService) generateCTAsWithAnthropic(ctx context.Context, productInfo string, count int) ([]string, error) {
	prompt := fmt.Sprintf(`Generate %d compelling call-to-action (CTA) button texts for this product/offer:

%s

Requirements:
- Keep CTAs short (2-4 words)
- Make them action-oriented
- Vary the tone (urgent, curious, benefit-focused)

Respond with a JSON array of strings:
["CTA 1", "CTA 2", ...]`, count, productInfo)

	response, err := s.callAnthropicSimple(ctx, prompt)
	if err != nil {
		return nil, err
	}

	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		response = strings.TrimPrefix(response, "```json")
		response = strings.TrimPrefix(response, "```")
		response = strings.TrimSuffix(response, "```")
	}

	var ctas []string
	if err := json.Unmarshal([]byte(response), &ctas); err != nil {
		return nil, err
	}

	return ctas, nil
}

func (s *AIContentService) generateCTAsWithOpenAI(ctx context.Context, productInfo string, count int) ([]string, error) {
	prompt := fmt.Sprintf(`Generate %d compelling call-to-action (CTA) button texts for this product/offer:

%s

Requirements:
- Keep CTAs short (2-4 words)
- Make them action-oriented
- Vary the tone (urgent, curious, benefit-focused)

Respond with a JSON array of strings:
["CTA 1", "CTA 2", ...]`, count, productInfo)

	response, err := s.callOpenAISimple(ctx, prompt)
	if err != nil {
		return nil, err
	}

	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		response = strings.TrimPrefix(response, "```json")
		response = strings.TrimPrefix(response, "```")
		response = strings.TrimSuffix(response, "```")
	}

	var ctas []string
	if err := json.Unmarshal([]byte(response), &ctas); err != nil {
		return nil, err
	}

	return ctas, nil
}

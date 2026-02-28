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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/config"
)

// AISubjectSuggestionService provides AI-powered subject line suggestions
type AISubjectSuggestionService struct {
	db         *sql.DB
	openAIKey  string
	model      string
	httpClient *http.Client
}

// PerformanceInsight represents learned performance data for AI context
type PerformanceInsight struct {
	TopSubjects      []TopPerformingSubject `json:"top_subjects"`
	BestOffers       []OfferPerformance     `json:"best_offers"`
	AudienceInsights []string               `json:"audience_insights"`
	Recommendations  []string               `json:"recommendations"`
}

// TopPerformingSubject represents a high-performing subject line
type TopPerformingSubject struct {
	Subject     string  `json:"subject"`
	OpenRate    float64 `json:"open_rate"`
	ClickRate   float64 `json:"click_rate"`
	CampaignName string `json:"campaign_name"`
}

// OfferPerformance represents offer-level performance data
type OfferPerformance struct {
	OfferName      string  `json:"offer_name"`
	ConversionRate float64 `json:"conversion_rate"`
	Revenue        float64 `json:"revenue"`
	ECPM           float64 `json:"ecpm"`
}

// NewAISubjectSuggestionService creates a new AI subject suggestion service
func NewAISubjectSuggestionService(db *sql.DB, openAICfg config.OpenAIConfig) *AISubjectSuggestionService {
	model := openAICfg.Model
	if model == "" {
		model = "gpt-4o-mini" // Cost-effective default for suggestions
	}
	return &AISubjectSuggestionService{
		db:        db,
		openAIKey: openAICfg.APIKey,
		model:     model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegisterRoutes registers AI suggestion routes
func (s *AISubjectSuggestionService) RegisterRoutes(r chi.Router) {
	r.Post("/subject-suggestions", s.HandleGenerateSubjectSuggestions)
	r.Post("/preheader-suggestions", s.HandleGeneratePreheaderSuggestions)
}

// SubjectSuggestionRequest represents the request for subject suggestions
type SubjectSuggestionRequest struct {
	// Content context
	HTMLContent   string `json:"html_content,omitempty"`
	CurrentSubject string `json:"current_subject,omitempty"`
	Preheader     string `json:"preheader,omitempty"`
	
	// Campaign context
	CampaignName  string `json:"campaign_name,omitempty"`
	CampaignType  string `json:"campaign_type,omitempty"` // newsletter, promotional, transactional, announcement
	
	// Audience context
	Industry      string `json:"industry,omitempty"`
	AudienceType  string `json:"audience_type,omitempty"` // b2b, b2c, mixed
	
	// Tone/Style preferences
	Tone          string `json:"tone,omitempty"` // professional, casual, urgent, friendly, playful
	
	// Constraints
	MaxLength     int    `json:"max_length,omitempty"`
	IncludeEmoji  bool   `json:"include_emoji,omitempty"`
	
	// Number of suggestions to generate
	Count         int    `json:"count,omitempty"`
}

// SubjectSuggestion represents a single subject line suggestion
type SubjectSuggestion struct {
	Subject           string   `json:"subject"`
	PlainSubject      string   `json:"plain_subject"`      // Without personalization for preview
	PersonalizationTags []string `json:"personalization_tags"` // Which merge tags are used
	PredictedOpenRate float64  `json:"predicted_open_rate"`
	Category          string   `json:"category"`           // urgency, curiosity, benefit, personalized, question
	Reasoning         string   `json:"reasoning"`
	CharacterCount    int      `json:"character_count"`
}

// SubjectSuggestionResponse contains all generated suggestions
type SubjectSuggestionResponse struct {
	Suggestions []SubjectSuggestion `json:"suggestions"`
	Context     map[string]string   `json:"context"`
	GeneratedAt string              `json:"generated_at"`
}

// HandleGenerateSubjectSuggestions generates AI-powered subject line suggestions
func (s *AISubjectSuggestionService) HandleGenerateSubjectSuggestions(w http.ResponseWriter, r *http.Request) {
	var req SubjectSuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Defaults
	if req.Count == 0 {
		req.Count = 5
	}
	if req.MaxLength == 0 {
		req.MaxLength = 60
	}
	if req.Tone == "" {
		req.Tone = "professional"
	}

	// Generate suggestions
	ctx := r.Context()
	suggestions, err := s.generateSubjectSuggestions(ctx, &req)
	if err != nil {
		log.Printf("Error generating subject suggestions: %v", err)
		// Return fallback suggestions if AI fails
		suggestions = s.getFallbackSuggestions(&req)
	}

	response := SubjectSuggestionResponse{
		Suggestions: suggestions,
		Context: map[string]string{
			"tone":          req.Tone,
			"campaign_type": req.CampaignType,
			"max_length":    fmt.Sprintf("%d", req.MaxLength),
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// generateSubjectSuggestions calls OpenAI to generate subject suggestions
func (s *AISubjectSuggestionService) generateSubjectSuggestions(ctx context.Context, req *SubjectSuggestionRequest) ([]SubjectSuggestion, error) {
	if s.openAIKey == "" {
		log.Println("AI Subject Suggestions: OpenAI API key not configured, using fallback suggestions")
		return nil, fmt.Errorf("OpenAI API key not configured")
	}
	
	log.Printf("AI Subject Suggestions: Generating %d suggestions with model %s", req.Count, s.model)

	// Extract content summary from HTML
	contentSummary := extractContentSummary(req.HTMLContent, 500)

	// Fetch real performance data to inform AI
	performanceData := s.fetchPerformanceInsights(ctx)
	log.Printf("AI Subject Suggestions: Loaded %d top subjects, %d top offers for context", 
		len(performanceData.TopSubjects), len(performanceData.BestOffers))

	// Build the prompt with real data
	prompt := s.buildSubjectPromptWithData(req, contentSummary, performanceData)

	// Call OpenAI
	completion, err := s.callOpenAI(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// Parse the response
	return s.parseSubjectSuggestions(completion)
}

// fetchPerformanceInsights retrieves real performance data from the database and analytics
func (s *AISubjectSuggestionService) fetchPerformanceInsights(ctx context.Context) PerformanceInsight {
	insights := PerformanceInsight{
		TopSubjects:      []TopPerformingSubject{},
		BestOffers:       []OfferPerformance{},
		AudienceInsights: []string{},
		Recommendations:  []string{},
	}

	// 1. Fetch top-performing subject lines from campaigns (last 30 days)
	topSubjectsQuery := `
		SELECT 
			subject,
			name as campaign_name,
			CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END as open_rate,
			CASE WHEN sent_count > 0 THEN (click_count::float / sent_count::float) * 100 ELSE 0 END as click_rate
		FROM mailing_campaigns
		WHERE status = 'completed' 
			AND sent_count >= 100
			AND created_at > NOW() - INTERVAL '30 days'
			AND subject IS NOT NULL
			AND subject != ''
		ORDER BY 
			(open_count::float / NULLIF(sent_count, 0)) DESC,
			(click_count::float / NULLIF(sent_count, 0)) DESC
		LIMIT 10
	`
	rows, err := s.db.QueryContext(ctx, topSubjectsQuery)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ts TopPerformingSubject
			if err := rows.Scan(&ts.Subject, &ts.CampaignName, &ts.OpenRate, &ts.ClickRate); err == nil {
				insights.TopSubjects = append(insights.TopSubjects, ts)
			}
		}
	} else {
		log.Printf("AI Subject Suggestions: Error fetching top subjects: %v", err)
	}

	// 2. Fetch top-performing offers
	topOffersQuery := `
		SELECT 
			name as offer_name,
			CASE WHEN total_sends > 0 THEN (total_conversions::float / total_sends::float) * 100 ELSE 0 END as conversion_rate,
			total_revenue,
			CASE WHEN total_sends > 0 THEN (total_revenue / total_sends) * 1000 ELSE 0 END as ecpm
		FROM mailing_offers
		WHERE status = 'active' 
			AND total_sends >= 100
		ORDER BY ecpm DESC, conversion_rate DESC
		LIMIT 10
	`
	rows2, err := s.db.QueryContext(ctx, topOffersQuery)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var op OfferPerformance
			if err := rows2.Scan(&op.OfferName, &op.ConversionRate, &op.Revenue, &op.ECPM); err == nil {
				insights.BestOffers = append(insights.BestOffers, op)
			}
		}
	} else {
		log.Printf("AI Subject Suggestions: Error fetching top offers: %v", err)
	}

	// 3. Fetch audience engagement insights
	audienceQuery := `
		SELECT 
			CASE 
				WHEN engagement_score >= 70 THEN 'High engagement subscribers respond best to exclusive/VIP messaging'
				WHEN engagement_score >= 40 THEN 'Medium engagement subscribers respond to benefit-focused subject lines'
				ELSE 'Low engagement subscribers need urgency and curiosity triggers'
			END as insight,
			COUNT(*) as count
		FROM mailing_subscribers
		WHERE status = 'confirmed'
		GROUP BY 1
		ORDER BY count DESC
		LIMIT 3
	`
	rows3, err := s.db.QueryContext(ctx, audienceQuery)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var insight string
			var count int
			if err := rows3.Scan(&insight, &count); err == nil {
				insights.AudienceInsights = append(insights.AudienceInsights, insight)
			}
		}
	}

	// 4. Fetch recent patterns from campaign performance
	patternQuery := `
		SELECT 
			CASE 
				WHEN subject ILIKE '%exclusive%' OR subject ILIKE '%vip%' THEN 'Exclusive/VIP messaging'
				WHEN subject ILIKE '%save%' OR subject ILIKE '%%off%' THEN 'Discount/savings messaging'
				WHEN subject ILIKE '%free%' THEN 'Free offer messaging'
				WHEN subject ILIKE '%last%' OR subject ILIKE '%hurry%' OR subject ILIKE '%ending%' THEN 'Urgency messaging'
				WHEN subject ILIKE '%?%' THEN 'Question-based subject lines'
				ELSE 'Standard messaging'
			END as pattern_type,
			AVG(CASE WHEN sent_count > 0 THEN (open_count::float / sent_count::float) * 100 ELSE 0 END) as avg_open_rate,
			COUNT(*) as sample_size
		FROM mailing_campaigns
		WHERE status = 'completed' 
			AND sent_count >= 100
			AND created_at > NOW() - INTERVAL '30 days'
		GROUP BY 1
		HAVING COUNT(*) >= 3
		ORDER BY avg_open_rate DESC
		LIMIT 5
	`
	rows4, err := s.db.QueryContext(ctx, patternQuery)
	if err == nil {
		defer rows4.Close()
		for rows4.Next() {
			var patternType string
			var avgOpenRate float64
			var sampleSize int
			if err := rows4.Scan(&patternType, &avgOpenRate, &sampleSize); err == nil {
				rec := fmt.Sprintf("%s performs at %.1f%% open rate (based on %d campaigns)", patternType, avgOpenRate, sampleSize)
				insights.Recommendations = append(insights.Recommendations, rec)
			}
		}
	}

	return insights
}

// buildSubjectPromptWithData creates a prompt enriched with real performance data
func (s *AISubjectSuggestionService) buildSubjectPromptWithData(req *SubjectSuggestionRequest, contentSummary string, data PerformanceInsight) string {
	var sb strings.Builder

	sb.WriteString(`You are an expert email marketing copywriter with access to REAL PERFORMANCE DATA from our ESP and analytics platform.
Generate ` + fmt.Sprintf("%d", req.Count) + ` unique email subject line suggestions INFORMED BY THE DATA below.

=== REAL PERFORMANCE DATA FROM OUR SYSTEM ===

`)

	// Add top-performing subject lines
	if len(data.TopSubjects) > 0 {
		sb.WriteString("TOP-PERFORMING SUBJECT LINES (Last 30 Days):\n")
		for i, ts := range data.TopSubjects {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("  - \"%s\" (Open: %.1f%%, Click: %.1f%%)\n", ts.Subject, ts.OpenRate, ts.ClickRate))
		}
		sb.WriteString("\n")
	}

	// Add top-performing offers
	if len(data.BestOffers) > 0 {
		sb.WriteString("TOP-PERFORMING OFFERS (By Revenue):\n")
		for i, op := range data.BestOffers {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("  - %s (Conv: %.2f%%, ECPM: $%.2f)\n", op.OfferName, op.ConversionRate, op.ECPM))
		}
		sb.WriteString("\n")
	}

	// Add audience insights
	if len(data.AudienceInsights) > 0 {
		sb.WriteString("AUDIENCE INSIGHTS:\n")
		for _, insight := range data.AudienceInsights {
			sb.WriteString(fmt.Sprintf("  - %s\n", insight))
		}
		sb.WriteString("\n")
	}

	// Add pattern recommendations
	if len(data.Recommendations) > 0 {
		sb.WriteString("WHAT'S WORKING (Pattern Analysis):\n")
		for _, rec := range data.Recommendations {
			sb.WriteString(fmt.Sprintf("  - %s\n", rec))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(`=== END PERFORMANCE DATA ===

CRITICAL REQUIREMENTS:
1. USE THE PERFORMANCE DATA ABOVE to inform your suggestions
2. Incorporate elements from high-performing subject lines
3. Reference successful offer themes when relevant
4. Each subject line MUST include at least one personalization merge tag using Liquid syntax
5. Available merge tags:
   - {{ first_name }} - Subscriber's first name
   - {{ first_name | default: "there" }} - First name with fallback
   - {{ custom.company }} - Company name
   - {{ custom.city }} - City
6. Subject lines should be under ` + fmt.Sprintf("%d", req.MaxLength) + ` characters

`)

	if req.Tone != "" {
		sb.WriteString(fmt.Sprintf("TONE: %s\n", req.Tone))
	}

	if req.CampaignType != "" {
		sb.WriteString(fmt.Sprintf("CAMPAIGN TYPE: %s\n", req.CampaignType))
	}

	if req.Industry != "" {
		sb.WriteString(fmt.Sprintf("INDUSTRY: %s\n", req.Industry))
	}

	if req.AudienceType != "" {
		sb.WriteString(fmt.Sprintf("AUDIENCE: %s\n", req.AudienceType))
	}

	if req.CurrentSubject != "" {
		sb.WriteString(fmt.Sprintf("\nCURRENT SUBJECT (improve this): %s\n", req.CurrentSubject))
	}

	if contentSummary != "" {
		sb.WriteString(fmt.Sprintf("\nEMAIL CONTENT SUMMARY:\n%s\n", contentSummary))
	}

	if req.IncludeEmoji {
		sb.WriteString("\nINCLUDE: Use relevant emojis strategically (1-2 per subject line)\n")
	}

	sb.WriteString(`
RESPONSE FORMAT (JSON array):
[
  {
    "subject": "{{ first_name }}, your exclusive offer expires tonight",
    "category": "urgency",
    "reasoning": "Based on our data showing urgency messaging achieves 23% open rates",
    "predicted_open_rate": 23.0
  }
]

Categories: urgency, curiosity, benefit, personalized, question, social_proof, exclusive

Generate data-driven suggestions now:`)

	return sb.String()
}

// buildSubjectPrompt is a legacy method that redirects to data-driven version
func (s *AISubjectSuggestionService) buildSubjectPrompt(req *SubjectSuggestionRequest, contentSummary string) string {
	// Redirect to data-driven version with empty performance data
	return s.buildSubjectPromptWithData(req, contentSummary, PerformanceInsight{})
}

func (s *AISubjectSuggestionService) callOpenAI(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]interface{}{
		"model": s.model,
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
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+s.openAIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenAI request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return "", fmt.Errorf("failed to parse OpenAI response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in OpenAI response")
	}

	return openAIResp.Choices[0].Message.Content, nil
}

func (s *AISubjectSuggestionService) parseSubjectSuggestions(completion string) ([]SubjectSuggestion, error) {
	// Extract JSON array from response
	completion = strings.TrimSpace(completion)
	
	// Handle markdown code blocks
	if strings.HasPrefix(completion, "```json") {
		completion = strings.TrimPrefix(completion, "```json")
		completion = strings.TrimSuffix(completion, "```")
		completion = strings.TrimSpace(completion)
	} else if strings.HasPrefix(completion, "```") {
		completion = strings.TrimPrefix(completion, "```")
		completion = strings.TrimSuffix(completion, "```")
		completion = strings.TrimSpace(completion)
	}

	var rawSuggestions []struct {
		Subject           string  `json:"subject"`
		Category          string  `json:"category"`
		Reasoning         string  `json:"reasoning"`
		PredictedOpenRate float64 `json:"predicted_open_rate"`
	}

	if err := json.Unmarshal([]byte(completion), &rawSuggestions); err != nil {
		return nil, fmt.Errorf("failed to parse suggestions: %w", err)
	}

	suggestions := make([]SubjectSuggestion, 0, len(rawSuggestions))
	for _, raw := range rawSuggestions {
		// Extract personalization tags
		tags := extractMergeTags(raw.Subject)
		
		// Create plain version for preview
		plain := stripMergeTags(raw.Subject)

		suggestions = append(suggestions, SubjectSuggestion{
			Subject:             raw.Subject,
			PlainSubject:        plain,
			PersonalizationTags: tags,
			PredictedOpenRate:   raw.PredictedOpenRate,
			Category:            raw.Category,
			Reasoning:           raw.Reasoning,
			CharacterCount:      len(raw.Subject),
		})
	}

	return suggestions, nil
}

func (s *AISubjectSuggestionService) getFallbackSuggestions(req *SubjectSuggestionRequest) []SubjectSuggestion {
	// Return pre-defined suggestions if AI fails
	suggestions := []SubjectSuggestion{
		{
			Subject:             "{{ first_name | default: \"Hey\" }}, check this out",
			PlainSubject:        "Hey, check this out",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   18.0,
			Category:            "personalized",
			Reasoning:           "Simple personalization with casual tone",
			CharacterCount:      42,
		},
		{
			Subject:             "{{ first_name }}, your exclusive update is here",
			PlainSubject:        "Your exclusive update is here",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   19.5,
			Category:            "exclusive",
			Reasoning:           "Creates sense of exclusivity with personalization",
			CharacterCount:      47,
		},
		{
			Subject:             "Quick question, {{ first_name }}...",
			PlainSubject:        "Quick question...",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   21.0,
			Category:            "curiosity",
			Reasoning:           "Questions drive curiosity and engagement",
			CharacterCount:      33,
		},
		{
			Subject:             "{{ first_name }}, don't miss this opportunity",
			PlainSubject:        "Don't miss this opportunity",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   17.5,
			Category:            "urgency",
			Reasoning:           "Creates urgency to drive immediate opens",
			CharacterCount:      44,
		},
		{
			Subject:             "We thought you'd want to know, {{ first_name }}",
			PlainSubject:        "We thought you'd want to know",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   18.5,
			Category:            "personalized",
			Reasoning:           "Builds trust through personalized tone",
			CharacterCount:      48,
		},
	}

	if req.IncludeEmoji {
		suggestions[0].Subject = "👋 " + suggestions[0].Subject
		suggestions[1].Subject = "🎁 " + suggestions[1].Subject
		suggestions[2].Subject = "🤔 " + suggestions[2].Subject
		suggestions[3].Subject = "⏰ " + suggestions[3].Subject
		suggestions[4].Subject = "💡 " + suggestions[4].Subject
	}

	return suggestions
}

// PreheaderSuggestionRequest represents the request for preheader suggestions
type PreheaderSuggestionRequest struct {
	Subject      string `json:"subject"`
	HTMLContent  string `json:"html_content,omitempty"`
	Tone         string `json:"tone,omitempty"`
	Count        int    `json:"count,omitempty"`
	IncludeEmoji bool   `json:"include_emoji,omitempty"`
}

// HandleGeneratePreheaderSuggestions generates AI-powered preheader suggestions
func (s *AISubjectSuggestionService) HandleGeneratePreheaderSuggestions(w http.ResponseWriter, r *http.Request) {
	var req PreheaderSuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Defaults
	if req.Count == 0 {
		req.Count = 5
	}
	if req.Tone == "" {
		req.Tone = "professional"
	}

	// Generate preheader suggestions
	ctx := r.Context()
	suggestions, err := s.generatePreheaderSuggestionsAI(ctx, &req)
	if err != nil {
		log.Printf("Error generating preheader suggestions: %v", err)
		// Return fallback suggestions if AI fails
		suggestions = s.getFallbackPreheaderSuggestions(&req)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suggestions":  suggestions,
		"subject":      req.Subject,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// generatePreheaderSuggestionsAI calls OpenAI to generate preheader suggestions
func (s *AISubjectSuggestionService) generatePreheaderSuggestionsAI(ctx context.Context, req *PreheaderSuggestionRequest) ([]SubjectSuggestion, error) {
	if s.openAIKey == "" {
		log.Println("AI Preheader Suggestions: OpenAI API key not configured, using fallback suggestions")
		return nil, fmt.Errorf("OpenAI API key not configured")
	}

	log.Printf("AI Preheader Suggestions: Generating %d suggestions with model %s", req.Count, s.model)

	// Fetch real performance data to inform AI
	performanceData := s.fetchPerformanceInsights(ctx)

	// Build the prompt with performance data
	prompt := s.buildPreheaderPromptWithData(req, performanceData)

	// Call OpenAI
	completion, err := s.callOpenAI(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// Parse the response (same format as subject suggestions)
	return s.parseSubjectSuggestions(completion)
}

func (s *AISubjectSuggestionService) buildPreheaderPrompt(req *PreheaderSuggestionRequest) string {
	// Legacy method - redirect to data-driven version with empty data
	return s.buildPreheaderPromptWithData(req, PerformanceInsight{})
}

func (s *AISubjectSuggestionService) buildPreheaderPromptWithData(req *PreheaderSuggestionRequest, data PerformanceInsight) string {
	var sb strings.Builder

	sb.WriteString(`You are an expert email marketing copywriter with access to REAL PERFORMANCE DATA from our ESP and analytics platform.
Generate ` + fmt.Sprintf("%d", req.Count) + ` unique email preheader suggestions that COMPLEMENT the subject line.

`)

	// Add performance data context
	if len(data.TopSubjects) > 0 || len(data.BestOffers) > 0 {
		sb.WriteString("=== REAL PERFORMANCE DATA ===\n\n")
		
		if len(data.TopSubjects) > 0 {
			sb.WriteString("TOP-PERFORMING CAMPAIGNS (What's Working):\n")
			for i, ts := range data.TopSubjects {
				if i >= 3 {
					break
				}
				sb.WriteString(fmt.Sprintf("  - \"%s\" (%.1f%% opens)\n", ts.Subject, ts.OpenRate))
			}
			sb.WriteString("\n")
		}

		if len(data.BestOffers) > 0 {
			sb.WriteString("BEST OFFERS (By Performance):\n")
			for i, op := range data.BestOffers {
				if i >= 3 {
					break
				}
				sb.WriteString(fmt.Sprintf("  - %s (ECPM: $%.2f)\n", op.OfferName, op.ECPM))
			}
			sb.WriteString("\n")
		}

		if len(data.Recommendations) > 0 {
			sb.WriteString("PATTERN INSIGHTS:\n")
			for i, rec := range data.Recommendations {
				if i >= 3 {
					break
				}
				sb.WriteString(fmt.Sprintf("  - %s\n", rec))
			}
			sb.WriteString("\n")
		}

		sb.WriteString("=== END PERFORMANCE DATA ===\n\n")
	}

	sb.WriteString(`CRITICAL REQUIREMENTS:
1. Preheaders should ADD VALUE to the subject line, not repeat it
2. USE THE PERFORMANCE DATA to inform your messaging approach
3. Each preheader MUST include at least one personalization merge tag using Liquid syntax
4. Available merge tags:
   - {{ first_name }} - Subscriber's first name
   - {{ first_name | default: "there" }} - First name with fallback
   - {{ custom.company }} - Company name
5. Preheaders should be 40-100 characters (shown after subject in inbox)
6. Create curiosity gap - tease content without revealing everything

PREHEADER BEST PRACTICES:
- Extend the subject line's message
- Add a call-to-action hint
- Create urgency or exclusivity
- Use personalization to increase relevance

`)

	sb.WriteString(fmt.Sprintf("TONE: %s\n", req.Tone))

	if req.Subject != "" {
		sb.WriteString(fmt.Sprintf("\nSUBJECT LINE TO COMPLEMENT: %s\n", req.Subject))
		sb.WriteString("The preheader must work WITH this subject line - extend, complement, or add context.\n")
	}

	// Extract content summary from HTML
	if req.HTMLContent != "" {
		contentSummary := extractContentSummary(req.HTMLContent, 300)
		if contentSummary != "" {
			sb.WriteString(fmt.Sprintf("\nEMAIL CONTENT SUMMARY:\n%s\n", contentSummary))
		}
	}

	if req.IncludeEmoji {
		sb.WriteString("\nINCLUDE: Use relevant emojis strategically (1-2 per preheader)\n")
	}

	sb.WriteString(`
RESPONSE FORMAT (JSON array):
[
  {
    "subject": "{{ first_name }}, see what's inside waiting for you...",
    "category": "curiosity",
    "reasoning": "Based on our top-performing campaigns showing curiosity drives engagement",
    "predicted_open_rate": 22.5
  }
]

Categories: curiosity, urgency, benefit, personalized, call_to_action, exclusive

Generate data-driven preheader suggestions now:`)

	return sb.String()
}

func (s *AISubjectSuggestionService) getFallbackPreheaderSuggestions(req *PreheaderSuggestionRequest) []SubjectSuggestion {
	// Smart fallback suggestions based on subject line content
	suggestions := []SubjectSuggestion{}

	// Analyze subject line to provide contextual suggestions
	subjectLower := strings.ToLower(req.Subject)
	hasQuestion := strings.Contains(subjectLower, "?")
	hasUrgency := strings.Contains(subjectLower, "miss") || strings.Contains(subjectLower, "hurry") || strings.Contains(subjectLower, "last") || strings.Contains(subjectLower, "now")
	hasExclusive := strings.Contains(subjectLower, "exclusive") || strings.Contains(subjectLower, "vip") || strings.Contains(subjectLower, "special")

	if hasQuestion {
		suggestions = append(suggestions, SubjectSuggestion{
			Subject:             "{{ first_name }}, the answer might surprise you...",
			PlainSubject:        "The answer might surprise you...",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   21.5,
			Category:            "curiosity",
			Reasoning:           "Builds on the question in your subject line",
			CharacterCount:      47,
		})
	}

	if hasUrgency {
		suggestions = append(suggestions, SubjectSuggestion{
			Subject:             "{{ first_name }}, this won't last long. Open now →",
			PlainSubject:        "This won't last long. Open now →",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   23.0,
			Category:            "urgency",
			Reasoning:           "Reinforces the urgency from your subject line",
			CharacterCount:      50,
		})
	}

	if hasExclusive {
		suggestions = append(suggestions, SubjectSuggestion{
			Subject:             "{{ first_name }}, you're one of the few to see this first",
			PlainSubject:        "You're one of the few to see this first",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   22.0,
			Category:            "exclusive",
			Reasoning:           "Amplifies the exclusivity message",
			CharacterCount:      56,
		})
	}

	// Always include these versatile options
	defaultSuggestions := []SubjectSuggestion{
		{
			Subject:             "{{ first_name }}, we put this together just for you...",
			PlainSubject:        "We put this together just for you...",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   20.5,
			Category:            "personalized",
			Reasoning:           "Personal touch that complements any subject line",
			CharacterCount:      52,
		},
		{
			Subject:             "Open to see what's waiting inside, {{ first_name }}",
			PlainSubject:        "Open to see what's waiting inside",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   19.5,
			Category:            "curiosity",
			Reasoning:           "Creates curiosity without spoiling content",
			CharacterCount:      51,
		},
		{
			Subject:             "{{ first_name }}, you'll want to see this →",
			PlainSubject:        "You'll want to see this →",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   20.0,
			Category:            "call_to_action",
			Reasoning:           "Direct call-to-action with curiosity",
			CharacterCount:      43,
		},
		{
			Subject:             "Here's what we've been working on, {{ first_name }}",
			PlainSubject:        "Here's what we've been working on",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   18.5,
			Category:            "personalized",
			Reasoning:           "Behind-the-scenes feel builds connection",
			CharacterCount:      52,
		},
		{
			Subject:             "{{ first_name }}, this is worth your time. Trust us.",
			PlainSubject:        "This is worth your time. Trust us.",
			PersonalizationTags: []string{"first_name"},
			PredictedOpenRate:   19.0,
			Category:            "benefit",
			Reasoning:           "Direct value promise builds trust",
			CharacterCount:      51,
		},
	}

	suggestions = append(suggestions, defaultSuggestions...)

	// Apply emoji if requested
	if req.IncludeEmoji && len(suggestions) > 0 {
		emojis := []string{"👀 ", "✨ ", "🎯 ", "💫 ", "🚀 "}
		for i := range suggestions {
			if i < len(emojis) {
				suggestions[i].Subject = emojis[i] + suggestions[i].Subject
				suggestions[i].CharacterCount += 2
			}
		}
	}

	// Limit to requested count
	if req.Count < len(suggestions) {
		return suggestions[:req.Count]
	}
	return suggestions
}

// Helper functions

func extractContentSummary(html string, maxLen int) string {
	if html == "" {
		return ""
	}

	// Strip HTML tags (simple approach)
	text := html
	for {
		start := strings.Index(text, "<")
		if start == -1 {
			break
		}
		end := strings.Index(text[start:], ">")
		if end == -1 {
			break
		}
		text = text[:start] + " " + text[start+end+1:]
	}

	// Clean up whitespace
	text = strings.Join(strings.Fields(text), " ")

	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}

	return text
}

func extractMergeTags(text string) []string {
	tags := []string{}
	seen := make(map[string]bool)

	// Find all {{ ... }} patterns
	remaining := text
	for {
		start := strings.Index(remaining, "{{")
		if start == -1 {
			break
		}
		end := strings.Index(remaining[start:], "}}")
		if end == -1 {
			break
		}
		
		tag := remaining[start : start+end+2]
		// Extract the variable name
		inner := strings.Trim(tag, "{ }")
		parts := strings.Split(inner, "|")
		varName := strings.TrimSpace(parts[0])
		
		if !seen[varName] {
			tags = append(tags, varName)
			seen[varName] = true
		}
		
		remaining = remaining[start+end+2:]
	}

	return tags
}

func stripMergeTags(text string) string {
	result := text
	
	// Replace common patterns with sample values
	replacements := map[string]string{
		"{{ first_name | default: \"there\" }}": "there",
		"{{ first_name | default: \"Hey\" }}":   "Hey",
		"{{ first_name | default: \"you\" }}":   "you",
		"{{ first_name }}":                       "John",
		"{{ last_name }}":                        "Doe",
		"{{ email }}":                            "john@example.com",
		"{{ custom.company }}":                   "Acme Inc",
		"{{ custom.city }}":                      "San Francisco",
	}

	for pattern, replacement := range replacements {
		result = strings.ReplaceAll(result, pattern, replacement)
	}

	// Handle any remaining tags
	for {
		start := strings.Index(result, "{{")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "}}")
		if end == -1 {
			break
		}
		result = result[:start] + "..." + result[start+end+2:]
	}

	return result
}

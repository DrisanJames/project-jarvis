package mailing

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAIContentService_ExtractTextFromHTML(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name     string
		html     string
		expected string
	}{
		{
			name:     "simple text",
			html:     "<p>Hello World</p>",
			expected: "Hello World",
		},
		{
			name:     "nested tags",
			html:     "<div><p>Hello</p><p>World</p></div>",
			expected: "Hello World",
		},
		{
			name:     "with script",
			html:     "<p>Hello</p><script>alert('x')</script><p>World</p>",
			expected: "Hello World",
		},
		{
			name:     "with style",
			html:     "<style>.test{color:red}</style><p>Hello World</p>",
			expected: "Hello World",
		},
		{
			name:     "html entities",
			html:     "<p>Hello &amp; World &lt;test&gt;</p>",
			expected: "Hello & World <test>",
		},
		{
			name:     "empty",
			html:     "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.extractTextFromHTML(tt.html)
			if result != tt.expected {
				t.Errorf("extractTextFromHTML() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestAIContentService_CountWords(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name     string
		text     string
		expected int
	}{
		{"empty", "", 0},
		{"single word", "Hello", 1},
		{"multiple words", "Hello World Test", 3},
		{"with punctuation", "Hello, World! Test.", 3},
		{"with extra whitespace", "  Hello   World  ", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.countWords(tt.text)
			if result != tt.expected {
				t.Errorf("countWords() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestAIContentService_CountSyllables(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		word     string
		expected int
	}{
		{"the", 1},
		{"hello", 2},
		{"beautiful", 3},
		{"programming", 3},
		{"a", 1},
		{"extraordinary", 5},
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			result := svc.countSyllables(tt.word)
			// Allow some variance as syllable counting is approximate
			if result < tt.expected-1 || result > tt.expected+1 {
				t.Errorf("countSyllables(%q) = %d, want approximately %d", tt.word, result, tt.expected)
			}
		})
	}
}

func TestAIContentService_CalculateReadability(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name    string
		text    string
		minScore float64
		maxScore float64
	}{
		{
			name:     "simple text",
			text:     "This is a simple test. It has short words. Easy to read.",
			minScore: 70,
			maxScore: 100,
		},
		{
			name:     "complex text",
			text:     "The implementation of sophisticated algorithmic methodologies necessitates comprehensive understanding of computational complexity theory and associated mathematical constructs.",
			minScore: 0,
			maxScore: 40,
		},
		{
			name:     "empty",
			text:     "",
			minScore: 50,
			maxScore: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.calculateReadability(tt.text)
			if result < tt.minScore || result > tt.maxScore {
				t.Errorf("calculateReadability() = %.2f, want between %.2f and %.2f", result, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestAIContentService_DetectSpamTriggers(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name           string
		text           string
		expectedCount  int
		containsTrigger string
	}{
		{
			name:           "no spam",
			text:           "Hello, check out our new product line.",
			expectedCount:  0,
			containsTrigger: "",
		},
		{
			name:           "free trigger",
			text:           "Get your FREE gift today!",
			expectedCount:  1,
			containsTrigger: "free",
		},
		{
			name:           "multiple triggers",
			text:           "Act now! This is urgent! Win cash prizes!",
			expectedCount:  3,
			containsTrigger: "urgent",
		},
		{
			name:           "excessive caps",
			text:           "THIS IS IMPORTANT REALLY IMPORTANT CHECK THIS",
			expectedCount:  1,
			containsTrigger: "EXCESSIVE CAPS",
		},
		{
			name:           "excessive exclamation",
			text:           "Amazing! Incredible! Unbelievable! Wow! Great!",
			expectedCount:  2, // triggers + excessive exclamation
			containsTrigger: "excessive exclamation marks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.detectSpamTriggers(tt.text)
			if tt.containsTrigger != "" {
				found := false
				for _, trigger := range result {
					if strings.Contains(strings.ToLower(trigger), strings.ToLower(tt.containsTrigger)) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("detectSpamTriggers() should contain %q, got %v", tt.containsTrigger, result)
				}
			}
		})
	}
}

func TestAIContentService_CalculateSpamScore(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name       string
		html       string
		triggers   []string
		minScore   float64
		maxScore   float64
	}{
		{
			name:     "clean email",
			html:     "<p>Hello, here is your update.</p>",
			triggers: []string{},
			minScore: 0,
			maxScore: 10,
		},
		{
			name:     "with spam triggers",
			html:     "<p>FREE MONEY NOW!</p>",
			triggers: []string{"free", "money", "now"},
			minScore: 10,
			maxScore: 30,
		},
		{
			name:     "hidden text",
			html:     "<p style='display:none'>Hidden content</p><p>Visible content</p>",
			triggers: []string{},
			minScore: 10,
			maxScore: 30,
		},
		{
			name:     "with javascript",
			html:     "<p>Hello</p><script>alert('x')</script>",
			triggers: []string{},
			minScore: 20,
			maxScore: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.calculateSpamScore(tt.html, tt.triggers)
			if result < tt.minScore || result > tt.maxScore {
				t.Errorf("calculateSpamScore() = %.2f, want between %.2f and %.2f", result, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestAIContentService_CalculateImageTextRatio(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name      string
		html      string
		wordCount int
		minRatio  float64
		maxRatio  float64
	}{
		{
			name:      "no images",
			html:      "<p>Hello World</p>",
			wordCount: 100,
			minRatio:  0,
			maxRatio:  0,
		},
		{
			name:      "one image",
			html:      "<p>Hello</p><img src='test.jpg'>",
			wordCount: 50,
			minRatio:  0.4,
			maxRatio:  0.6,
		},
		{
			name:      "image only",
			html:      "<img src='test.jpg'>",
			wordCount: 0,
			minRatio:  1.0,
			maxRatio:  1.0,
		},
		{
			name:      "many images",
			html:      "<img src='1.jpg'><img src='2.jpg'><img src='3.jpg'>",
			wordCount: 30,
			minRatio:  0.7,
			maxRatio:  1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.calculateImageTextRatio(tt.html, tt.wordCount)
			if result < tt.minRatio || result > tt.maxRatio {
				t.Errorf("calculateImageTextRatio() = %.2f, want between %.2f and %.2f", result, tt.minRatio, tt.maxRatio)
			}
		})
	}
}

func TestAIContentService_GenerateContentSuggestions(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name         string
		analysis     *ContentAnalysis
		minSuggestions int
	}{
		{
			name: "high spam score",
			analysis: &ContentAnalysis{
				SpamScore:        60,
				ReadabilityScore: 70,
				WordCount:        100,
				ImageTextRatio:   0.3,
				SpamTriggers:     []string{"free", "urgent"},
			},
			minSuggestions: 1,
		},
		{
			name: "low readability",
			analysis: &ContentAnalysis{
				SpamScore:        10,
				ReadabilityScore: 30,
				WordCount:        100,
				ImageTextRatio:   0.3,
				SpamTriggers:     []string{},
			},
			minSuggestions: 1,
		},
		{
			name: "short content",
			analysis: &ContentAnalysis{
				SpamScore:        10,
				ReadabilityScore: 70,
				WordCount:        20,
				ImageTextRatio:   0.3,
				SpamTriggers:     []string{},
			},
			minSuggestions: 1,
		},
		{
			name: "good email",
			analysis: &ContentAnalysis{
				SpamScore:        10,
				ReadabilityScore: 70,
				WordCount:        150,
				ImageTextRatio:   0.3,
				SentimentScore:   0.5,
				SpamTriggers:     []string{},
			},
			minSuggestions: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.generateContentSuggestions(tt.analysis)
			if len(result) < tt.minSuggestions {
				t.Errorf("generateContentSuggestions() returned %d suggestions, want at least %d", len(result), tt.minSuggestions)
			}
		})
	}
}

func TestAIContentService_AnalyzeContent(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name         string
		html         string
		wantSpamMin  float64
		wantSpamMax  float64
		wantReadMin  float64
		wantReadMax  float64
	}{
		{
			name:        "clean marketing email",
			html:        "<p>Hello! Check out our latest products. We have great options for you.</p>",
			wantSpamMin: 0,
			wantSpamMax: 20,
			wantReadMin: 50,
			wantReadMax: 100,
		},
		{
			name:        "spammy email",
			html:        "<p>FREE CASH! ACT NOW! LIMITED TIME OFFER! CLICK HERE!</p>",
			wantSpamMin: 15,
			wantSpamMax: 100,
			wantReadMin: 40,
			wantReadMax: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			result, err := svc.AnalyzeContent(ctx, tt.html)
			if err != nil {
				t.Fatalf("AnalyzeContent() error = %v", err)
			}

			if result.SpamScore < tt.wantSpamMin || result.SpamScore > tt.wantSpamMax {
				t.Errorf("SpamScore = %.2f, want between %.2f and %.2f", result.SpamScore, tt.wantSpamMin, tt.wantSpamMax)
			}

			if result.ReadabilityScore < tt.wantReadMin || result.ReadabilityScore > tt.wantReadMax {
				t.Errorf("ReadabilityScore = %.2f, want between %.2f and %.2f", result.ReadabilityScore, tt.wantReadMin, tt.wantReadMax)
			}

			if result.WordCount == 0 {
				t.Error("WordCount should not be 0 for non-empty HTML")
			}
		})
	}
}

func TestAIContentService_GetFallbackSubjectSuggestions(t *testing.T) {
	svc := &AIContentService{}

	tests := []struct {
		name     string
		params   SubjectParams
		wantLen  int
		hasEmoji bool
	}{
		{
			name:    "default",
			params:  SubjectParams{Count: 5},
			wantLen: 5,
		},
		{
			name:    "limited count",
			params:  SubjectParams{Count: 3},
			wantLen: 3,
		},
		{
			name:     "with emoji",
			params:   SubjectParams{Count: 5, IncludeEmoji: true},
			wantLen:  5,
			hasEmoji: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.getFallbackSubjectSuggestions(tt.params)
			
			if len(result) != tt.wantLen {
				t.Errorf("getFallbackSubjectSuggestions() returned %d suggestions, want %d", len(result), tt.wantLen)
			}

			// Verify all suggestions have required fields
			for _, s := range result {
				if s.Subject == "" {
					t.Error("Subject should not be empty")
				}
				if s.PredictedOpenRate <= 0 {
					t.Error("PredictedOpenRate should be positive")
				}
				if s.Tone == "" {
					t.Error("Tone should not be empty")
				}
				if s.Reasoning == "" {
					t.Error("Reasoning should not be empty")
				}
			}

			if tt.hasEmoji && len(result) > 0 {
				// Check that at least some subjects have emojis
				hasEmojiInSubject := false
				for _, s := range result {
					if strings.ContainsAny(s.Subject, "✨🔥👋🤔📬") {
						hasEmojiInSubject = true
						break
					}
				}
				if !hasEmojiInSubject {
					t.Error("Expected emoji in subjects when IncludeEmoji is true")
				}
			}
		})
	}
}

func TestSubjectSuggestion_Struct(t *testing.T) {
	suggestion := SubjectSuggestion{
		Subject:           "Test subject",
		PredictedOpenRate: 18.5,
		Tone:              "professional",
		Reasoning:         "Test reasoning",
	}

	if suggestion.Subject != "Test subject" {
		t.Error("Subject field not set correctly")
	}
	if suggestion.PredictedOpenRate != 18.5 {
		t.Error("PredictedOpenRate field not set correctly")
	}
	if suggestion.Tone != "professional" {
		t.Error("Tone field not set correctly")
	}
	if suggestion.Reasoning != "Test reasoning" {
		t.Error("Reasoning field not set correctly")
	}
}

func TestContentAnalysis_Struct(t *testing.T) {
	analysis := ContentAnalysis{
		SpamScore:        25.5,
		ReadabilityScore: 65.0,
		SentimentScore:   0.3,
		SpamTriggers:     []string{"free", "urgent"},
		Suggestions:      []string{"Remove spam words"},
		WordCount:        150,
		ImageTextRatio:   0.25,
	}

	if analysis.SpamScore != 25.5 {
		t.Error("SpamScore field not set correctly")
	}
	if analysis.ReadabilityScore != 65.0 {
		t.Error("ReadabilityScore field not set correctly")
	}
	if len(analysis.SpamTriggers) != 2 {
		t.Error("SpamTriggers field not set correctly")
	}
}

func TestPerformancePrediction_Struct(t *testing.T) {
	prediction := PerformancePrediction{
		OpenRate:        15.5,
		ClickRate:       2.3,
		UnsubscribeRate: 0.5,
		Confidence:      0.85,
		Factors: []PredictionFactor{
			{
				Factor:      "personalization",
				Impact:      0.15,
				Description: "Personalization detected",
			},
		},
	}

	if prediction.OpenRate != 15.5 {
		t.Error("OpenRate field not set correctly")
	}
	if len(prediction.Factors) != 1 {
		t.Error("Factors field not set correctly")
	}
	if prediction.Factors[0].Factor != "personalization" {
		t.Error("Factor field not set correctly")
	}
}

func TestSegmentRecommendation_Struct(t *testing.T) {
	rec := SegmentRecommendation{
		SegmentName:    "High-Value Engaged",
		EstimatedSize:  5000,
		PredictedValue: 0.025,
		Criteria:       []string{"engagement_score >= 70"},
		Reasoning:      "High engagement subscribers have better conversion",
	}

	if rec.SegmentName != "High-Value Engaged" {
		t.Error("SegmentName field not set correctly")
	}
	if rec.EstimatedSize != 5000 {
		t.Error("EstimatedSize field not set correctly")
	}
	if len(rec.Criteria) != 1 {
		t.Error("Criteria field not set correctly")
	}
}

func TestPredictionFactor_Struct(t *testing.T) {
	factor := PredictionFactor{
		Factor:      "subject_length",
		Impact:      0.05,
		Description: "Optimal subject line length",
	}

	if factor.Factor != "subject_length" {
		t.Error("Factor field not set correctly")
	}
	if factor.Impact != 0.05 {
		t.Error("Impact field not set correctly")
	}
}

func TestSpamTriggerWords(t *testing.T) {
	// Verify that spam trigger words list is populated
	if len(spamTriggerWords) == 0 {
		t.Error("spamTriggerWords should not be empty")
	}

	// Verify common spam words are included
	expectedWords := []string{"free", "winner", "urgent", "act now", "click here"}
	for _, word := range expectedWords {
		found := false
		for _, trigger := range spamTriggerWords {
			if strings.EqualFold(trigger, word) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected spam trigger word %q not found in spamTriggerWords", word)
		}
	}
}

func TestIsAlpha(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"hello", true},
		{"HELLO", true},
		{"Hello", true},
		{"hello123", false},
		{"hello!", false},
		{"", true}, // empty string has no non-alpha chars
		{"HelloWorld", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := isAlpha(tt.input)
			if result != tt.expected {
				t.Errorf("isAlpha(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSubjectParams_Defaults(t *testing.T) {
	params := SubjectParams{}

	// Verify default values should be handled in GenerateSubjectLines
	if params.Count != 0 {
		t.Error("Default Count should be 0 (handled by service)")
	}
	if params.MaxLength != 0 {
		t.Error("Default MaxLength should be 0 (handled by service)")
	}
}

// BenchmarkExtractTextFromHTML benchmarks HTML text extraction
func BenchmarkExtractTextFromHTML(b *testing.B) {
	svc := &AIContentService{}
	html := `
		<html>
		<head><title>Test</title></head>
		<body>
			<div class="header">
				<h1>Welcome to Our Newsletter</h1>
			</div>
			<div class="content">
				<p>This is a test email with <strong>bold</strong> and <em>italic</em> text.</p>
				<p>Check out our <a href="http://example.com">website</a> for more info.</p>
				<ul>
					<li>Item 1</li>
					<li>Item 2</li>
					<li>Item 3</li>
				</ul>
			</div>
			<script>alert('x')</script>
			<style>.test{color:red}</style>
		</body>
		</html>
	`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.extractTextFromHTML(html)
	}
}

// BenchmarkCalculateReadability benchmarks readability calculation
func BenchmarkCalculateReadability(b *testing.B) {
	svc := &AIContentService{}
	text := "The quick brown fox jumps over the lazy dog. This is a test sentence with multiple words and various lengths. Simple words are easy to read while complex vocabulary requires more effort."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.calculateReadability(text)
	}
}

// BenchmarkDetectSpamTriggers benchmarks spam trigger detection
func BenchmarkDetectSpamTriggers(b *testing.B) {
	svc := &AIContentService{}
	text := "Get your FREE gift today! Act now for this limited time offer. Winner of our exclusive prize! Click here to claim your cash bonus."

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.detectSpamTriggers(text)
	}
}

// Test for UUID helper used in tests
func TestUUIDPtr(t *testing.T) {
	id := uuid.New()
	ptr := uuidPtr(id)

	if ptr == nil {
		t.Error("uuidPtr should not return nil")
	}
	if *ptr != id {
		t.Error("uuidPtr should return pointer to same UUID")
	}
}

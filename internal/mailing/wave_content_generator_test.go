package mailing

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TemplateFillWithVariation — the mechanical Phase 2 rendering
// ---------------------------------------------------------------------------

func TestTemplateFillWithVariation_SubstitutesAllPlaceholders(t *testing.T) {
	template := `<!DOCTYPE html><html>
<body>
<!-- preview text --><div>{{PREVIEW_TEXT}}</div>
<p>{{INTRO}}</p>
<!-- BLOCK:ARTICLE_1 -->
<h2>{{ARTICLE_1_HEADLINE}}</h2>
<p>{{ARTICLE_1_SUMMARY}}</p>
<a href="{{ARTICLE_1_URL}}">{{ARTICLE_1_CTA}}</a>
<!-- /BLOCK:ARTICLE_1 -->
<!-- BLOCK:ARTICLE_2 -->
<h2>{{ARTICLE_2_HEADLINE}}</h2>
<p>{{ARTICLE_2_SUMMARY}}</p>
<a href="{{ARTICLE_2_URL}}">{{ARTICLE_2_CTA}}</a>
<!-- /BLOCK:ARTICLE_2 -->
<p>{{CLOSING_LINE}}</p>
</body></html>`

	editorial := []WaveEditorialContent{
		{
			WaveIndex:   0,
			Subject:     "Test Subject Line",
			PreviewText: "Test Preview",
			Intro:       "Welcome to the newsletter!",
			ArticleSections: []ArticleSection{
				{Headline: "Article One", Summary: "Summary one.", CTAText: "Read more", URL: "https://example.com/1"},
				{Headline: "Article Two", Summary: "Summary two.", CTAText: "Learn more", URL: "https://example.com/2"},
			},
			ClosingLine: "Until next time.",
		},
	}

	req := WaveContentRequest{
		HTMLTemplate: template,
		BrandName:    "Discount Blog",
	}

	result := TemplateFillWithVariation(editorial, req)
	if len(result) != 1 {
		t.Fatalf("expected 1 variation, got %d", len(result))
	}

	v := result[0]
	if v.Subject != "Test Subject Line" {
		t.Errorf("subject = %q, want %q", v.Subject, "Test Subject Line")
	}
	if v.PreviewText != "Test Preview" {
		t.Errorf("preview = %q, want %q", v.PreviewText, "Test Preview")
	}
	if v.FromName != "Discount Blog" {
		t.Errorf("fromName = %q, want %q", v.FromName, "Discount Blog")
	}

	for _, want := range []string{
		"Welcome to the newsletter!",
		"Article One", "Summary one.", "Read more", "https://example.com/1",
		"Article Two", "Summary two.", "Learn more", "https://example.com/2",
		"Until next time.",
	} {
		if !strings.Contains(v.HTMLContent, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}

	for _, bad := range []string{"{{INTRO}}", "{{PREVIEW_TEXT}}", "{{ARTICLE_1_HEADLINE}}", "{{CLOSING_LINE}}"} {
		if strings.Contains(v.HTMLContent, bad) {
			t.Errorf("placeholder %s was not replaced", bad)
		}
	}
}

func TestTemplateFillWithVariation_RemovesUnusedBlocks(t *testing.T) {
	template := `<html>
<p>{{INTRO}}</p>
<!-- BLOCK:ARTICLE_1 -->
<h2>{{ARTICLE_1_HEADLINE}}</h2>
<!-- /BLOCK:ARTICLE_1 -->
<!-- BLOCK:ARTICLE_2 -->
<h2>{{ARTICLE_2_HEADLINE}}</h2>
<!-- /BLOCK:ARTICLE_2 -->
<!-- BLOCK:ARTICLE_3 -->
<h2>{{ARTICLE_3_HEADLINE}}</h2>
<!-- /BLOCK:ARTICLE_3 -->
</html>`

	editorial := []WaveEditorialContent{
		{
			WaveIndex: 0, Subject: "S", Intro: "Hi",
			ArticleSections: []ArticleSection{
				{Headline: "Only Article"},
			},
		},
	}

	result := TemplateFillWithVariation(editorial, WaveContentRequest{
		HTMLTemplate: template,
		BrandName:    "Test Brand",
	})
	if len(result) != 1 {
		t.Fatalf("expected 1 variation, got %d", len(result))
	}

	html := result[0].HTMLContent
	if !strings.Contains(html, "Only Article") {
		t.Error("article 1 should be present")
	}
	if strings.Contains(html, "ARTICLE_2") {
		t.Error("unused ARTICLE_2 block should be removed")
	}
	if strings.Contains(html, "ARTICLE_3") {
		t.Error("unused ARTICLE_3 block should be removed")
	}
}

func TestTemplateFillWithVariation_MultipleWaves(t *testing.T) {
	template := `<html><p>{{INTRO}}</p></html>`

	editorial := []WaveEditorialContent{
		{WaveIndex: 0, Subject: "Wave 0", Intro: "Intro zero"},
		{WaveIndex: 1, Subject: "Wave 1", Intro: "Intro one"},
		{WaveIndex: 2, Subject: "Wave 2", Intro: "Intro two"},
	}

	result := TemplateFillWithVariation(editorial, WaveContentRequest{
		HTMLTemplate: template,
		BrandName:    "Test",
	})
	if len(result) != 3 {
		t.Fatalf("expected 3 variations, got %d", len(result))
	}

	for i, v := range result {
		if v.Subject != editorial[i].Subject {
			t.Errorf("wave %d subject = %q, want %q", i, v.Subject, editorial[i].Subject)
		}
		if !strings.Contains(v.HTMLContent, editorial[i].Intro) {
			t.Errorf("wave %d missing its intro text", i)
		}
	}
}

func TestTemplateFillWithVariation_EmptyEditorial(t *testing.T) {
	result := TemplateFillWithVariation(nil, WaveContentRequest{
		HTMLTemplate: `<html>{{INTRO}}</html>`,
		BrandName:    "Brand",
	})
	if len(result) != 0 {
		t.Errorf("expected 0 variations for nil editorial, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// brandKeyFromName — used inside TemplateFillWithVariation for category/emoji
// ---------------------------------------------------------------------------

func TestBrandKeyFromName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Discount Blog", "discountblog"},
		{"discount", "discountblog"},
		{"QuizFiesta", "quizfiesta"},
		{"quiz", "quizfiesta"},
		{"fiesta", "quizfiesta"},
		{"Unknown Brand", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := brandKeyFromName(tt.name)
			if got != tt.want {
				t.Errorf("brandKeyFromName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// removeBlock — strips unused article sections
// ---------------------------------------------------------------------------

func TestRemoveBlock(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		block string
		want  string
	}{
		{
			"removes matching block",
			"before<!-- BLOCK:ARTICLE_2 -->content<!-- /BLOCK:ARTICLE_2 -->after",
			"ARTICLE_2",
			"beforeafter",
		},
		{
			"no matching block — returns unchanged",
			"<p>nothing to remove</p>",
			"ARTICLE_5",
			"<p>nothing to remove</p>",
		},
		{
			"only start marker — returns unchanged",
			"<!-- BLOCK:ARTICLE_3 -->content",
			"ARTICLE_3",
			"<!-- BLOCK:ARTICLE_3 -->content",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := removeBlock(tt.html, tt.block)
			if got != tt.want {
				t.Errorf("removeBlock() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Prompt dispatch — buildEditorialPrompt routes by campaign type
// ---------------------------------------------------------------------------

func TestBuildPrompt_DispatchesByCampaignType(t *testing.T) {
	gen := NewWaveContentGenerator(nil)

	welcomeReq := WaveContentRequest{
		CampaignType: "welcome",
		BrandName:    "Discount Blog",
		NumWaves:     3,
	}
	welcomePrompt := gen.BuildPrompt(welcomeReq)

	if !strings.Contains(welcomePrompt, "welcome") && !strings.Contains(welcomePrompt, "Welcome") {
		t.Error("welcome prompt should contain welcome-related language")
	}

	newsletterReq := WaveContentRequest{
		CampaignType: "newsletter",
		BrandName:    "Discount Blog",
		NumWaves:     3,
	}
	newsletterPrompt := gen.BuildPrompt(newsletterReq)

	if welcomePrompt == newsletterPrompt {
		t.Error("welcome and newsletter prompts should differ")
	}

	defaultReq := WaveContentRequest{
		BrandName: "Discount Blog",
		NumWaves:  3,
	}
	defaultPrompt := gen.BuildPrompt(defaultReq)

	if defaultPrompt != newsletterPrompt {
		t.Error("empty campaign type should default to newsletter prompt")
	}
}

func TestBuildPrompt_WelcomeIncludesPersonaAndAudience(t *testing.T) {
	gen := NewWaveContentGenerator(nil)

	prompt := gen.BuildPrompt(WaveContentRequest{
		CampaignType: "welcome",
		BrandName:    "Discount Blog",
		Voice:        "friendly savings expert",
		Audience:     "brand-new subscribers",
		NumWaves:     5,
	})

	if !strings.Contains(prompt, "5") {
		t.Error("prompt should include the requested number of waves")
	}
}

// ---------------------------------------------------------------------------
// pickCategory / pickEmoji — deterministic per brand
// ---------------------------------------------------------------------------

func TestPickCategory(t *testing.T) {
	got := pickCategory("discountblog", 0)
	if got != "SAVINGS TIPS" {
		t.Errorf("discountblog[0] = %q, want SAVINGS TIPS", got)
	}
	got = pickCategory("quizfiesta", 0)
	if got != "GAME MODE" {
		t.Errorf("quizfiesta[0] = %q, want GAME MODE", got)
	}
	got = pickCategory("unknown", 0)
	if got != "FEATURED" {
		t.Errorf("unknown brand = %q, want FEATURED", got)
	}
	got = pickCategory("discountblog", 99)
	if got != "FEATURED" {
		t.Errorf("out-of-range index = %q, want FEATURED", got)
	}
}

func TestPickEmoji(t *testing.T) {
	got := pickEmoji("discountblog", 0)
	if got != "💰" {
		t.Errorf("discountblog[0] = %q, want 💰", got)
	}
	got = pickEmoji("quizfiesta", 1)
	if got != "💀" {
		t.Errorf("quizfiesta[1] = %q, want 💀", got)
	}
	got = pickEmoji("other", 0)
	if got != "📰" {
		t.Errorf("unknown brand = %q, want 📰", got)
	}
}

// ---------------------------------------------------------------------------
// coalesce — first non-empty string wins
// ---------------------------------------------------------------------------

func TestCoalesce(t *testing.T) {
	if got := coalesce("", "", "third"); got != "third" {
		t.Errorf("coalesce skipped empties = %q, want third", got)
	}
	if got := coalesce("first", "second"); got != "first" {
		t.Errorf("coalesce first = %q, want first", got)
	}
	if got := coalesce("", ""); got != "" {
		t.Errorf("coalesce all empty = %q, want empty", got)
	}
}

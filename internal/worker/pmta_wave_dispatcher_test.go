package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

func TestDeriveBrandKey(t *testing.T) {
	tests := []struct {
		fromEmail string
		want      string
	}{
		{"hello@em.discountblog.com", "discountblog"},
		{"hello@em.quizfiesta.com", "quizfiesta"},
		{"hello@m.discountblog.com", "discountblog"},
		{"noreply@discountblog.com", "discountblog"},
		{"hello@em.DISCOUNTBLOG.COM", "discountblog"},
		{"", ""},
		{"noemail", ""},
		{"user@sub.em.example.com", "sub"},
	}
	for _, tt := range tests {
		t.Run(tt.fromEmail, func(t *testing.T) {
			got := deriveBrandKey(tt.fromEmail)
			if got != tt.want {
				t.Errorf("deriveBrandKey(%q) = %q, want %q", tt.fromEmail, got, tt.want)
			}
		})
	}
}

func TestCoalesceWaveValue(t *testing.T) {
	tests := []struct {
		value, fallback, want string
	}{
		{"cached subject", "default subject", "cached subject"},
		{"", "default subject", "default subject"},
		{"cached", "", "cached"},
		{"", "", ""},
	}
	for _, tt := range tests {
		got := coalesceWaveValue(tt.value, tt.fallback)
		if got != tt.want {
			t.Errorf("coalesceWaveValue(%q, %q) = %q, want %q", tt.value, tt.fallback, got, tt.want)
		}
	}
}

func TestDetectCampaignType(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Mar 16 Welcome", "welcome"},
		{"em.discountblog.com — Mar 15 Welcome", "welcome"},
		{"WELCOME new subscribers", "welcome"},
		{"Sunday Savings!", "newsletter"},
		{"em.quizfiesta.com — Sunday Trivia!", "trivia"},
		{"Quiz Night Special", "trivia"},
		{"QuizFiesta Weekly", "trivia"},
		{"Random Campaign", "newsletter"},
		{"", "newsletter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectCampaignType(tt.name)
			if got != tt.want {
				t.Errorf("detectCampaignType(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestHasPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		html string
		want bool
	}{
		{"intro placeholder", `<p>{{INTRO}}</p>`, true},
		{"article placeholder", `<h2>{{ARTICLE_1_HEADLINE}}</h2>`, true},
		{"both placeholders", `{{INTRO}} ... {{ARTICLE_1_HEADLINE}}`, true},
		{"no placeholders", `<p>Hello world, this is static content.</p>`, false},
		{"empty string", "", false},
		{"similar but wrong", `{{INTRODUCTION}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasPlaceholders(tt.html)
			if got != tt.want {
				t.Errorf("hasPlaceholders() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildVariantFromEditorial_MergesIntoTemplate(t *testing.T) {
	editorial := mailing.WaveEditorialContent{
		WaveIndex:   0,
		Subject:     "AI-Generated Subject",
		PreviewText: "AI Preview",
		Intro:       "Fresh intro paragraph from AI.",
		ArticleSections: []mailing.ArticleSection{
			{Headline: "Top Savings Tip", Summary: "Save $500 monthly.", CTAText: "See the plan", URL: "https://discountblog.com/savings"},
		},
		ClosingLine: "Cheers from the team!",
	}
	editJSON, _ := json.Marshal(editorial)

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
<!-- /BLOCK:ARTICLE_2 -->
<p>{{CLOSING_LINE}}</p>
</body></html>`

	cached := &CachedWaveContent{
		Variation:     mailing.WaveVariation{WaveIndex: 0, Subject: "AI-Generated Subject", HTMLContent: "<old-rendered>"},
		EditorialJSON: editJSON,
	}

	v := buildVariantFromEditorial(cached, "Fallback Name", "Fallback Subject", template, "discountblog")

	if v.Subject != "AI-Generated Subject" {
		t.Errorf("subject = %q, want AI-Generated Subject", v.Subject)
	}
	if !strings.Contains(v.HTMLContent, "Fresh intro paragraph from AI.") {
		t.Error("expected editorial intro to be merged into template")
	}
	if !strings.Contains(v.HTMLContent, "Top Savings Tip") {
		t.Error("expected article headline to be merged into template")
	}
	if !strings.Contains(v.HTMLContent, "Save $500 monthly.") {
		t.Error("expected article summary to be merged")
	}
	if !strings.Contains(v.HTMLContent, "https://discountblog.com/savings") {
		t.Error("expected article URL to be merged")
	}
	if strings.Contains(v.HTMLContent, "{{INTRO}}") {
		t.Error("placeholder {{INTRO}} should have been replaced")
	}
	if strings.Contains(v.HTMLContent, "ARTICLE_2_HEADLINE") {
		t.Error("unused article block should have been removed")
	}
	if !strings.Contains(v.HTMLContent, "Cheers from the team!") {
		t.Error("expected closing line to be merged")
	}
}

func TestBuildVariantFromEditorial_NoPlaceholders_UsesHTMLAsIs(t *testing.T) {
	editorial := mailing.WaveEditorialContent{Subject: "AI Subject", Intro: "Fresh content"}
	editJSON, _ := json.Marshal(editorial)

	staticHTML := `<html><body><p>This is a fully authored email with no placeholders.</p></body></html>`

	cached := &CachedWaveContent{
		Variation:     mailing.WaveVariation{WaveIndex: 0, Subject: "AI Subject", HTMLContent: "<cached-rendered>"},
		EditorialJSON: editJSON,
	}

	v := buildVariantFromEditorial(cached, "Default Name", "Default Subject", staticHTML, "discountblog")

	if v.HTMLContent != staticHTML {
		t.Error("when template has no placeholders, campaign HTML should be used as-is")
	}
	if v.Subject != "AI Subject" {
		t.Errorf("subject should still be varied, got %q", v.Subject)
	}
}

func TestBuildVariantFromEditorial_NoEditorialJSON_FallsBackToCachedHTML(t *testing.T) {
	templateHTML := `<html>{{INTRO}} {{ARTICLE_1_HEADLINE}}</html>`
	cachedRendered := `<html>Previously rendered content</html>`

	cached := &CachedWaveContent{
		Variation:     mailing.WaveVariation{WaveIndex: 0, Subject: "Cached Subject", HTMLContent: cachedRendered},
		EditorialJSON: nil,
	}

	v := buildVariantFromEditorial(cached, "Name", "Fallback Subject", templateHTML, "discountblog")

	if v.HTMLContent != cachedRendered {
		t.Error("when no editorial JSON exists, should fall back to cached rendered HTML")
	}
}

func TestBuildVariantFromEditorial_MalformedJSON_FallsBack(t *testing.T) {
	templateHTML := `<html>{{INTRO}} {{ARTICLE_1_HEADLINE}}</html>`

	cached := &CachedWaveContent{
		Variation:     mailing.WaveVariation{WaveIndex: 0, Subject: "Sub", HTMLContent: "<cached>"},
		EditorialJSON: []byte(`{not valid json`),
	}

	v := buildVariantFromEditorial(cached, "Name", "Fallback", templateHTML, "discountblog")

	if v.HTMLContent == "" {
		t.Error("should not produce empty HTML on malformed JSON")
	}
}

func TestBuildVariantFromEditorial_SubjectFallback(t *testing.T) {
	cached := &CachedWaveContent{
		Variation:     mailing.WaveVariation{WaveIndex: 0, Subject: "", HTMLContent: "<html>ok</html>"},
		EditorialJSON: nil,
	}

	v := buildVariantFromEditorial(cached, "Name", "Fallback Subject", "<p>no placeholders</p>", "discountblog")

	if v.Subject != "Fallback Subject" {
		t.Errorf("empty cached subject should fall back, got %q", v.Subject)
	}
}

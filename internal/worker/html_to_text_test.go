package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateTextFromHTML_EmptyInput(t *testing.T) {
	assert.Equal(t, "", GenerateTextFromHTML(""))
}

func TestGenerateTextFromHTML_StripHiddenDivs(t *testing.T) {
	html := `<body>
		<div style="display:none;font-size:1px;">Hidden preheader content &#847;&#847;</div>
		<p>Visible content here</p>
	</body>`

	text := GenerateTextFromHTML(html)
	assert.NotContains(t, text, "Hidden preheader")
	assert.NotContains(t, text, "&#847;")
	assert.Contains(t, text, "Visible content here")
}

func TestGenerateTextFromHTML_PreserveLinks(t *testing.T) {
	html := `<p>Click <a href="https://example.com/deals">here for deals</a> today.</p>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "here for deals (https://example.com/deals)")
}

func TestGenerateTextFromHTML_ConvertListItems(t *testing.T) {
	html := `<ul><li>First item</li><li>Second item</li></ul>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "- First item")
	assert.Contains(t, text, "- Second item")
}

func TestGenerateTextFromHTML_BlockElements(t *testing.T) {
	html := `<p>Paragraph one</p><p>Paragraph two</p><div>A div</div>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "Paragraph one")
	assert.Contains(t, text, "Paragraph two")
	assert.Contains(t, text, "A div")
	lines := strings.Split(text, "\n")
	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	assert.GreaterOrEqual(t, nonEmpty, 3, "block elements should produce separate lines")
}

func TestGenerateTextFromHTML_DecodeEntities(t *testing.T) {
	html := `<p>Save 50&amp; more — don&apos;t miss out!</p>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "Save 50& more")
}

func TestGenerateTextFromHTML_CollapseWhitespace(t *testing.T) {
	html := `<p>   Too   many    spaces   </p>
	
	
	
	<p>After many blank lines</p>`

	text := GenerateTextFromHTML(html)
	assert.NotContains(t, text, "   ")
	// Should not have more than 2 consecutive newlines
	assert.NotContains(t, text, "\n\n\n")
}

func TestGenerateTextFromHTML_FullEmail(t *testing.T) {
	html := `<html><body>
		<div style="display:none;max-height:0px;overflow:hidden;">Preview text here</div>
		<h1>Welcome to Discount Blog!</h1>
		<p>Hi {{ first_name | default: "there" }},</p>
		<p>Check out our <a href="https://discountblog.com/deals">latest deals</a>.</p>
		<div style="text-align:center;padding:16px;">
			<a href="{{ system.unsubscribe_url }}" style="color:#999;">Unsubscribe</a>
		</div>
	</body></html>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "Welcome to Discount Blog!")
	assert.Contains(t, text, "latest deals (https://discountblog.com/deals)")
	assert.Contains(t, text, "Unsubscribe")
	assert.NotContains(t, text, "Preview text here")
	assert.NotContains(t, text, "<")
}

func TestGenerateTextFromHTML_UnsubscribePreserved(t *testing.T) {
	html := `<body>
		<p>Content</p>
		<a href="https://trk.em.discountblog.com/track/unsubscribe/abc123/sig456">Unsubscribe</a>
	</body>`

	text := GenerateTextFromHTML(html)
	assert.Contains(t, text, "Unsubscribe (https://trk.em.discountblog.com/track/unsubscribe/abc123/sig456)")
}

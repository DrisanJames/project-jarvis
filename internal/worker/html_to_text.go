package worker

import (
	"html"
	"regexp"
	"strings"
)

var (
	// Comments must go FIRST with a proper `-->` terminator: reTxtAllTags'
	// `<[^>]*>` ends a "tag" at the first `>`, so a comment containing `>`
	// mid-body (e.g. "Lane: x -> brand fc") was cut short and its remainder
	// shipped as visible text in the text/plain part (seed test 2026-08-14,
	// Auto Policy Bridge — the design comment leaked into the inbox).
	reTxtComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTxtHiddenDiv  = regexp.MustCompile(`(?is)<div[^>]*display\s*:\s*none[^>]*>.*?</div>`)
	reTxtAnchor     = regexp.MustCompile(`(?is)<a\s[^>]*href=["']([^"']*)["'][^>]*>(.*?)</a>`)
	reTxtListItem   = regexp.MustCompile(`(?i)<li[^>]*>`)
	reTxtBlockOpen  = regexp.MustCompile(`(?i)<\s*(?:br|p|div|tr|h[1-6])[^>]*/?\s*>`)
	reTxtBlockClose = regexp.MustCompile(`(?i)</\s*(?:p|div|tr|h[1-6])\s*>`)
	reTxtAllTags    = regexp.MustCompile(`<[^>]*>`)
	reTxtMultiNL    = regexp.MustCompile(`\n{3,}`)
	reTxtMultiSP    = regexp.MustCompile(`[ \t]+`)
)

// GenerateTextFromHTML converts final rendered HTML into a meaningful
// text/plain alternative. It strips hidden divs (display:none) before
// conversion so preheader filler and tracking pixels don't leak into
// the text part.
func GenerateTextFromHTML(finalHTML string) string {
	if finalHTML == "" {
		return ""
	}

	s := finalHTML

	// Strip HTML comments before anything else — see reTxtComment. An
	// unterminated comment (no closing -->) is dropped to end-of-input by the
	// second pass rather than left to leak.
	s = reTxtComment.ReplaceAllString(s, "")
	if i := strings.Index(s, "<!--"); i >= 0 {
		s = s[:i]
	}

	// Strip hidden divs (preheader, tracking pixel containers)
	s = reTxtHiddenDiv.ReplaceAllString(s, "")

	// Convert <a href="URL">text</a> to "text (URL)"
	s = reTxtAnchor.ReplaceAllString(s, "$2 ($1)")

	// Convert <li> to bullet points
	s = reTxtListItem.ReplaceAllString(s, "\n- ")

	// Convert block-level tags to newlines
	s = reTxtBlockOpen.ReplaceAllString(s, "\n")
	s = reTxtBlockClose.ReplaceAllString(s, "\n")

	// Strip all remaining HTML tags
	s = reTxtAllTags.ReplaceAllString(s, "")

	// Decode HTML entities
	s = html.UnescapeString(s)

	// Collapse multiple spaces on each line
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(reTxtMultiSP.ReplaceAllString(line, " "))
	}
	s = strings.Join(lines, "\n")

	// Collapse excessive blank lines
	s = reTxtMultiNL.ReplaceAllString(s, "\n\n")

	return strings.TrimSpace(s)
}

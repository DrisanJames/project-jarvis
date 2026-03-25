package worker

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

func computeMutationSeed(subscriberID uuid.UUID, waveID string) int64 {
	h := sha256.New()
	h.Write(subscriberID[:])
	h.Write([]byte(waveID))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// mutateHTMLHash applies deterministic, visually-invisible mutations to HTML
// so each recipient gets a unique fingerprint. The rendered output is identical.
func mutateHTMLHash(html string, seed int64) string {
	rng := rand.New(rand.NewSource(seed))
	html = injectHTMLComments(html, rng)
	html = injectDataAttributes(html, rng)
	html = reorderInlineStyles(html, rng)
	html = varyWhitespace(html, rng)
	return html
}

// ---------------------------------------------------------------------------
// Layer 1: HTML mutations
// ---------------------------------------------------------------------------

var reBlockClose = regexp.MustCompile(`</(?:td|tr|div|table|p|th)>`)

func injectHTMLComments(html string, rng *rand.Rand) string {
	matches := reBlockClose.FindAllStringIndex(html, -1)
	if len(matches) == 0 {
		return html
	}

	count := 3 + rng.Intn(3)
	if count > len(matches) {
		count = len(matches)
	}

	chosen := rng.Perm(len(matches))[:count]
	sort.Sort(sort.Reverse(sort.IntSlice(chosen)))

	for _, ci := range chosen {
		pos := matches[ci][1]
		comment := fmt.Sprintf("<!--%x-->", rng.Uint32())
		html = html[:pos] + comment + html[pos:]
	}
	return html
}

var reOpenTag = regexp.MustCompile(`<(table|tr|td|div|span|th)(\s[^>]*)?>`)

func injectDataAttributes(html string, rng *rand.Rand) string {
	return reOpenTag.ReplaceAllStringFunc(html, func(match string) string {
		if rng.Intn(5) != 0 {
			return match
		}
		token := fmt.Sprintf("%x", rng.Uint32())
		cb := strings.LastIndex(match, ">")
		if cb < 0 {
			return match
		}
		return match[:cb] + fmt.Sprintf(` data-m="%s"`, token) + match[cb:]
	})
}

var reStyleAttr = regexp.MustCompile(`style="([^"]*)"`)

func reorderInlineStyles(html string, rng *rand.Rand) string {
	return reStyleAttr.ReplaceAllStringFunc(html, func(match string) string {
		if rng.Intn(5) > 1 {
			return match
		}
		inner := match[7 : len(match)-1]
		parts := strings.Split(inner, ";")
		var clean []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				clean = append(clean, p)
			}
		}
		if len(clean) < 2 {
			return match
		}
		for i := len(clean) - 1; i > 0; i-- {
			j := rng.Intn(i + 1)
			clean[i], clean[j] = clean[j], clean[i]
		}
		return `style="` + strings.Join(clean, "; ") + `"`
	})
}

var reTagGap = regexp.MustCompile(`>([\s\n]+)<`)

func varyWhitespace(html string, rng *rand.Rand) string {
	return reTagGap.ReplaceAllStringFunc(html, func(match string) string {
		if rng.Intn(8) != 0 {
			return match
		}
		extra := strings.Repeat(" ", rng.Intn(4)+1)
		return ">\n" + extra + "<"
	})
}

// ---------------------------------------------------------------------------
// Layer 2: Subject / preheader light variance
// ---------------------------------------------------------------------------

var synonymPairs = []struct{ A, B string }{
	{"save", "get"},
	{"deals", "offers"},
	{"don't", "won't"},
	{"plus", "and"},
	{"check out", "discover"},
	{"tips", "tricks"},
	{"best", "top"},
	{"new", "latest"},
	{"amazing", "incredible"},
	{"easy", "simple"},
	{"great", "awesome"},
	{"find", "discover"},
	{"ways", "methods"},
	{"guide", "handbook"},
	{"learn", "find out"},
	{"secrets", "insider tips"},
	{"huge", "massive"},
	{"quick", "fast"},
	{"smart", "clever"},
	{"essential", "must-have"},
}

var brandEmojiPools = map[string][]string{
	"discountblog":    {"💰", "🛒", "🔥", "✨", "💵", "🎯", "💸", "🏷️"},
	"historythinking": {"📜", "🏛️", "⚔️", "🗺️", "🔍", "🏺", "📚", "🎭"},
	"quizfiesta":     {"🧠", "🎯", "❓", "🏆", "⭐", "🎪", "🤔", "💡"},
	"projectjarvis":  {"🚀", "⚡", "🔧", "💻", "🤖", "✨", "🎯", "📊"},
}

func mutateSubjectLine(subject string, seed int64, brandKey string) string {
	rng := rand.New(rand.NewSource(seed))
	result := subject
	result = applySynonymSwap(result, rng)
	result = rotatePunctuation(result, rng)
	result = microShiftCaps(result, rng)
	result = applyEmojiBracket(result, rng, brandKey)
	return result
}

func mutatePreheader(preheader string, seed int64) string {
	rng := rand.New(rand.NewSource(seed))
	result := preheader
	result = applySynonymSwap(result, rng)
	result = rotatePunctuation(result, rng)
	return result
}

func applySynonymSwap(text string, rng *rand.Rand) string {
	order := rng.Perm(len(synonymPairs))
	lower := strings.ToLower(text)

	for _, idx := range order {
		pair := synonymPairs[idx]
		if pos := findWholeWord(lower, pair.A); pos >= 0 {
			repl := matchCase(text[pos:pos+len(pair.A)], pair.B)
			return text[:pos] + repl + text[pos+len(pair.A):]
		}
		if pos := findWholeWord(lower, pair.B); pos >= 0 {
			repl := matchCase(text[pos:pos+len(pair.B)], pair.A)
			return text[:pos] + repl + text[pos+len(pair.B):]
		}
	}
	return text
}

func findWholeWord(text, word string) int {
	pos := 0
	for {
		idx := strings.Index(text[pos:], word)
		if idx < 0 {
			return -1
		}
		abs := pos + idx
		before := abs == 0 || !isWordChar(text[abs-1])
		after := abs+len(word) >= len(text) || !isWordChar(text[abs+len(word)])
		if before && after {
			return abs
		}
		pos = abs + 1
	}
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '\''
}

func matchCase(original, replacement string) string {
	if len(original) == 0 || len(replacement) == 0 {
		return replacement
	}
	if original[0] >= 'A' && original[0] <= 'Z' {
		return strings.ToUpper(replacement[:1]) + replacement[1:]
	}
	return replacement
}

func rotatePunctuation(text string, rng *rand.Rand) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	base := strings.TrimRight(text, ".!…")
	options := []string{"", ".", "!", "…"}
	return base + options[rng.Intn(len(options))]
}

func microShiftCaps(text string, rng *rand.Rand) string {
	words := strings.Fields(text)
	if len(words) < 3 {
		return text
	}
	idx := 1 + rng.Intn(len(words)-1)
	w := words[idx]
	if len(w) < 3 {
		return text
	}
	first := w[0]
	if first >= 'A' && first <= 'Z' {
		words[idx] = strings.ToLower(w[:1]) + w[1:]
	} else if first >= 'a' && first <= 'z' {
		words[idx] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func applyEmojiBracket(text string, rng *rand.Rand, brandKey string) string {
	pool, ok := brandEmojiPools[brandKey]
	if !ok || len(pool) == 0 {
		return text
	}
	roll := rng.Intn(10)
	if roll < 4 {
		return text
	}
	emoji := pool[rng.Intn(len(pool))]
	if roll < 7 {
		return emoji + " " + text
	}
	return text + " " + emoji
}

// ---------------------------------------------------------------------------
// Bot-trap honeypot link
// ---------------------------------------------------------------------------

const botTrapBaseURL = "https://projectjarvis.io/track/verify/"

// injectHoneypotLink embeds a hidden link that only automated scanners will
// follow. If clicked, the backend flags the subscriber as a bot. The link is
// invisible to humans: off-screen, zero-opacity, zero font size, aria-hidden.
// Uses two path segments (/track/verify/{token}/{nonce}) to match production
// ALB routing requirements.
func injectHoneypotLink(html, subscriberID string) string {
	token := base64.URLEncoding.EncodeToString([]byte(subscriberID))
	h := sha256.Sum256([]byte(subscriberID))
	nonce := fmt.Sprintf("%x", h[:4])
	honeypot := fmt.Sprintf(
		`<div style="position:absolute;left:-9999px;top:-9999px;opacity:0;font-size:0;line-height:0;max-height:0;overflow:hidden;mso-hide:all;" aria-hidden="true">`+
			`<a href="%s%s/%s" style="color:transparent;text-decoration:none;font-size:0;line-height:0;" tabindex="-1">.</a>`+
			`</div>`,
		botTrapBaseURL, token, nonce,
	)

	bodyClose := strings.LastIndex(strings.ToLower(html), "</body>")
	if bodyClose >= 0 {
		return html[:bodyClose] + honeypot + html[bodyClose:]
	}
	return html + honeypot
}

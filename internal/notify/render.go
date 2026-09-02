package notify

import (
	"flag"
	"log"
	"os"
	"strings"
)

// render.go — the Go side of the platform Slack message standard
// (docs/SLACK_MESSAGE_STANDARD.md, operator ruling 2026-09-01: "Normalize our
// messages. All of them. Standardize them. Direct and to the point.").
//
// The standard is enforced HERE, structurally, not by convention at the call
// sites:
//
//   - Scope is a fixed noun from scopeVocabulary. An unknown scope panics under
//     `go test` and is logged + coerced to the nearest vocabulary word in prod.
//   - Tier maps to exactly one prefix: "🚨 ALERT", "⚠️ WARN", "✅ OK", none for
//     EVENT. The tier word appears once, as the first token of the headline.
//   - Headline renders as `<mark> · <Scope> · <what happened> · <the number>`.
//   - Body is hard-capped at bodyLineLimit lines (truncated with "…" and logged).
//   - Context is a single line (and is omitted by the call sites when the time
//     is "now" — an "as of <now>" line is an anti-pattern, standard §4).
//   - Banned phrases (standard "Anti-patterns") are stripped from every field.
//
// Channel-message constraint (verified 2026-06-20 against the live workspace):
// the native `alert` block is REJECTED by chat.postMessage in a channel
// ("Unsupported block type: alert") — it is modal-only. So severity is carried
// by a leading emoji-prefixed mrkdwn `section`, exactly like slackkit.

// Tier is the single severity of a message (standard §"Tiers").
type Tier string

const (
	TierEvent Tier = "EVENT" // something happened, no action — no prefix
	TierWarn  Tier = "WARN"  // degraded, self-healing or watch
	TierAlert Tier = "ALERT" // needs a human now
	TierOK    Tier = "OK"    // a prior WARN/ALERT cleared

	// Deprecated: pre-2026-09-01 tier names, kept so an unconverted call site
	// still compiles. They resolve to the four standard tiers above.
	TierWatch    = TierWarn
	TierDigest   = TierEvent
	TierResolved = TierOK
)

// tierPrefix is the ONLY severity decoration a message may carry.
var tierPrefix = map[Tier]string{
	TierEvent: "",
	TierWarn:  "\u26A0\uFE0F WARN", // WARN
	TierAlert: "\U0001F6A8 ALERT",  // ALERT
	TierOK:    "\u2705 OK",         // OK
}

// The fixed scope vocabulary (standard §"Scopes"). A new scope needs a doc edit
// in docs/SLACK_MESSAGE_STANDARD.md, not an ad-hoc word.
const (
	ScopeConversion = "Conversion"
	ScopeSend       = "Send"
	ScopeBoard      = "Board"
	ScopeDrip       = "Drip"
	ScopeKumo       = "Kumo"
	ScopeDeploy     = "Deploy"
	ScopeCron       = "Cron"
	ScopeDB         = "DB"
	ScopeLake       = "Lake"
	ScopeIngest     = "Ingest"
	ScopeSegment    = "Segment"
	ScopeReport     = "Report"
)

// ScopeVocabulary is the closed set, in doc order. Exported so tests (and the
// repo-wide literal sweep) can assert against exactly this list.
var ScopeVocabulary = []string{
	ScopeConversion, ScopeSend, ScopeBoard, ScopeDrip, ScopeKumo, ScopeDeploy,
	ScopeCron, ScopeDB, ScopeLake, ScopeIngest, ScopeSegment, ScopeReport,
}

// scopeAliases maps the pre-standard subsystem labels that older call sites and
// their tests still pass (e.g. worker.NewSlackAlerter("Campaign lateness")) onto
// the vocabulary. An alias is a known name, so it never panics.
var scopeAliases = map[string]string{
	"outbox":               ScopeSend,
	"outbox self-check":    ScopeSend,
	"campaign lateness":    ScopeSend,
	"send liveness":        ScopeSend,
	"storage":              ScopeDB,
	"storage guard":        ScopeDB,
	"worker stall":         ScopeCron,
	"sam's club drip":      ScopeDrip,
	"drip digest":          ScopeDrip,
	"project jarvis alert": ScopeSend,
}

// BannedPhrases are the standard's anti-patterns. They are stripped from every
// rendered field, and TestNoBannedPhraseInCallSites fails the build if any
// notify.Message literal in the repo contains one.
var BannedPhrases = []string{
	"hi team",
	"heads up",
	"just a heads up",
	"fyi",
	"please note",
	"note that",
	"successfully",
	"as of now",
}

// bodyLineLimit is the hard cap from the standard: body ≤ 6 lines.
const bodyLineLimit = 6

const textLimit = 3000

// strictStandard is true when this binary is a `go test` binary. Under test an
// off-vocabulary scope is a hard failure (panic); in prod it is logged and
// coerced so a bad label can never silence an operational alert.
var strictStandard = isTestBinary()

func isTestBinary() bool {
	if flag.Lookup("test.v") != nil {
		return true
	}
	return len(os.Args) > 0 && strings.HasSuffix(os.Args[0], ".test")
}

// NormalizeScope maps s onto the fixed vocabulary. Exact (case-insensitive)
// match wins, then the alias table, then — for an unknown word — panic under
// test / log + nearest-match in prod.
func NormalizeScope(s string) string {
	t := strings.TrimSpace(s)
	lower := strings.ToLower(t)
	for _, v := range ScopeVocabulary {
		if strings.EqualFold(v, t) {
			return v
		}
	}
	if v, ok := scopeAliases[lower]; ok {
		return v
	}
	if strictStandard {
		panic("notify: scope " + quote(t) + " is not in the standard vocabulary " +
			strings.Join(ScopeVocabulary, "|") + " — see docs/SLACK_MESSAGE_STANDARD.md")
	}
	near := nearestScope(lower)
	log.Printf("[notify] non-standard scope %q coerced to %q (docs/SLACK_MESSAGE_STANDARD.md)", t, near)
	return near
}

func quote(s string) string { return "\"" + s + "\"" }

// nearestScope picks the vocabulary word that shares a substring with the input,
// falling back to Report (the catch-all for "a thing was posted").
func nearestScope(lower string) string {
	if lower == "" {
		return ScopeReport
	}
	for _, v := range ScopeVocabulary {
		lv := strings.ToLower(v)
		if strings.Contains(lower, lv) || strings.Contains(lv, lower) {
			return v
		}
	}
	return ScopeReport
}

// stripBanned removes every banned phrase (case-insensitive) and collapses the
// whitespace it leaves behind. Per-line so a table body keeps its shape.
func stripBanned(s string) string {
	if s == "" {
		return ""
	}
	out := s
	for _, p := range BannedPhrases {
		out = removeFold(out, p)
	}
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(strings.Join(strings.Fields(ln), " "), " ")
		if lines[i] == "" && strings.TrimSpace(ln) == "" {
			lines[i] = ""
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// removeFold deletes every case-insensitive occurrence of sub from s.
func removeFold(s, sub string) string {
	if sub == "" {
		return s
	}
	ls, lsub := strings.ToLower(s), strings.ToLower(sub)
	var b strings.Builder
	for {
		i := strings.Index(ls, lsub)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s, ls = s[i+len(sub):], ls[i+len(sub):]
	}
}

// oneLine collapses a field to its first non-empty line (standard: Context and
// the next-action line are one line each).
func oneLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// capBody enforces the ≤6-line body: the first 5 lines are kept and the sixth
// becomes "…". Truncation is logged so a body that keeps overflowing is visible.
func capBody(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= bodyLineLimit {
		return strings.Join(lines, "\n")
	}
	log.Printf("[notify] body truncated: %d lines > %d (docs/SLACK_MESSAGE_STANDARD.md)", len(lines), bodyLineLimit)
	kept := append([]string{}, lines[:bodyLineLimit-1]...)
	kept = append(kept, "…")
	return strings.Join(kept, "\n")
}

// Message is the standard anatomy for a channel message. Headline carries Slack
// mrkdwn (single-asterisk *bold* on the one number that decides the action).
type Message struct {
	Tier     Tier
	Scope    string // one of ScopeVocabulary
	Headline string // "<what happened> · <the one number>"
	Context  string // one line, ONLY when the time is not "now"
	Body     string // ≤ 6 lines of "Label: value"
	Action   string // one line, ONLY when there is a real next action: "Run: …" / "Decide: …"
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func sectionBlock(md string) map[string]any {
	return map[string]any{"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": clipRunes(md, textLimit)}}
}

func contextBlock(md string) map[string]any {
	return map[string]any{"type": "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": clipRunes(md, textLimit)}}}
}

func markdownBlock(md string) map[string]any {
	return map[string]any{"type": "markdown", "text": clipRunes(md, textLimit)}
}

// HeadlineLine builds the one headline every message leads with:
//
//	EVENT : "<Scope> · <headline>"
//	other : "<mark> · <Scope> · <headline>"   (mark = 🚨 ALERT / ⚠️ WARN / ✅ OK)
func HeadlineLine(m Message) string {
	parts := make([]string, 0, 3)
	if p := tierPrefix[m.Tier]; p != "" {
		parts = append(parts, p)
	}
	parts = append(parts, NormalizeScope(m.Scope))
	if h := oneLine(stripBanned(m.Headline)); h != "" {
		parts = append(parts, h)
	}
	return strings.Join(parts, " · ")
}

// SeverityLine is the channel-safe severity headline (the `alert`-block
// replacement): an emoji-prefixed mrkdwn section.
func SeverityLine(tier Tier, text string) map[string]any {
	return sectionBlock(strings.TrimSpace(strings.TrimPrefix(tierPrefix[tier]+" · "+text, " · ")))
}

// Render builds the standard anatomy into a Block Kit blocks slice.
func Render(m Message) []any {
	blocks := []any{sectionBlock(HeadlineLine(m))}
	if c := oneLine(stripBanned(m.Context)); c != "" {
		blocks = append(blocks, contextBlock(c))
	}
	if b := capBody(stripBanned(m.Body)); b != "" {
		blocks = append(blocks, markdownBlock(b))
	}
	if a := oneLine(stripBanned(m.Action)); a != "" {
		blocks = append(blocks, markdownBlock(a))
	}
	return blocks
}

// Fallback is the plain-text notification mirror (and the body used when the
// transport can't send blocks — e.g. NoopNotifier).
func Fallback(m Message) string {
	return HeadlineLine(m)
}

// legacyTitleBody flattens a Message into the old title/body shape for
// transports that only implement Notify(title, body).
func (m Message) legacyTitleBody() (string, string) {
	parts := make([]string, 0, 3)
	if c := oneLine(stripBanned(m.Context)); c != "" {
		parts = append(parts, c)
	}
	if b := capBody(stripBanned(m.Body)); b != "" {
		parts = append(parts, b)
	}
	if a := oneLine(stripBanned(m.Action)); a != "" {
		parts = append(parts, a)
	}
	return HeadlineLine(m), strings.Join(parts, "\n")
}

// Deliver sends a standard Message: as Block Kit blocks when the notifier
// supports them, otherwise via the legacy Notify(title, body) path.
func Deliver(n Notifier, m Message) error {
	if bn, ok := n.(BlockNotifier); ok {
		return bn.NotifyBlocks(Render(m), Fallback(m))
	}
	title, body := m.legacyTitleBody()
	return n.Notify(title, body)
}

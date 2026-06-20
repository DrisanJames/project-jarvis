package notify

import (
	"strings"
)

// render.go — the Go side of the platform Slack house style
// (docs/SLACK_COMMS_STANDARD.md). It mirrors agents/slackkit so the Go backend
// alerts read identically to the Python posters.
//
// Channel-message constraint (verified 2026-06-20 against the live workspace):
// the native `alert` block is REJECTED by chat.postMessage in a channel
// ("Unsupported block type: alert") — it is modal-only. So severity is carried
// by a leading emoji-prefixed mrkdwn `section`, exactly like slackkit. The rich
// blocks data_table / data_visualization / card / markdown ARE channel-valid.

// Tier is the single severity of a message (standard §1).
type Tier string

const (
	TierAlert    Tier = "ALERT"    // 🔴 needs action now
	TierWatch    Tier = "WATCH"    // 🟡 degraded / approaching threshold
	TierDigest   Tier = "DIGEST"   // 🟦 routine rollup
	TierEvent    Tier = "EVENT"    // ⚪ lifecycle milestone
	TierResolved Tier = "RESOLVED" // ✅ a prior alert cleared
)

var tierEmoji = map[Tier]string{
	TierAlert:    "\U0001F534", // 🔴
	TierWatch:    "\U0001F7E1", // 🟡
	TierDigest:   "\U0001F7E6", // 🟦
	TierEvent:    "⚪",     // ⚪
	TierResolved: "✅",     // ✅
}

// Message is the standard §2 anatomy for a channel message. Headline carries
// Slack mrkdwn (single-asterisk *bold*). Scope is the stable subsystem label.
type Message struct {
	Tier     Tier
	Scope    string // OUTBOX, STORAGE, "jun20 send", …
	Headline string // the one fact, with the number, *bolded*
	Context  string // "as of HH:MM UTC · source · grain"
	Body     string // optional detail (mrkdwn)
	Action   string // optional "→ <link|label> · runbook: …"
	Footer   string // optional standard caveat
}

const textLimit = 3000

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

// SeverityLine is the channel-safe severity headline (the `alert`-block
// replacement): an emoji-prefixed mrkdwn section.
func SeverityLine(tier Tier, text string) map[string]any {
	return sectionBlock(strings.TrimSpace(tierEmoji[tier] + " " + text))
}

// Render builds the standard anatomy into a Block Kit blocks slice.
func Render(m Message) []any {
	head := m.Headline
	if m.Scope != "" {
		head = "*" + m.Scope + "* — " + m.Headline
	}
	blocks := []any{SeverityLine(m.Tier, head)}
	if m.Context != "" {
		blocks = append(blocks, contextBlock(m.Context))
	}
	if m.Body != "" {
		blocks = append(blocks, markdownBlock(m.Body))
	}
	if m.Action != "" {
		blocks = append(blocks, markdownBlock(m.Action))
	}
	if m.Footer != "" {
		blocks = append(blocks, contextBlock(m.Footer))
	}
	return blocks
}

// Fallback is the plain-text notification mirror (and the body used when the
// transport can't send blocks — e.g. NoopNotifier).
func Fallback(m Message) string {
	head := m.Headline
	if m.Scope != "" {
		head = m.Scope + " — " + m.Headline
	}
	return strings.TrimSpace(tierEmoji[m.Tier] + " " + head)
}

// legacyBody flattens a Message into the old title/body shape for transports
// that only implement Notify(title, body).
func (m Message) legacyTitleBody() (string, string) {
	title := m.Scope
	if title != "" {
		title += " — "
	}
	title += m.Headline
	parts := make([]string, 0, 4)
	for _, p := range []string{m.Context, m.Body, m.Action, m.Footer} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return title, strings.Join(parts, "\n")
}

// Deliver sends a house-style Message: as Block Kit blocks when the notifier
// supports them, otherwise via the legacy Notify(title, body) path. This keeps
// every existing Notifier working while upgrading the rich transports.
func Deliver(n Notifier, m Message) error {
	if bn, ok := n.(BlockNotifier); ok {
		return bn.NotifyBlocks(Render(m), Fallback(m))
	}
	title, body := m.legacyTitleBody()
	return n.Notify(title, body)
}

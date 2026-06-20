package notify

import (
	"errors"
	"testing"
)

func blockTypes(blocks []any) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			out = append(out, m["type"].(string))
		}
	}
	return out
}

func TestRenderAnatomyAndNoAlertBlock(t *testing.T) {
	m := Message{
		Tier: TierAlert, Scope: "OUTBOX",
		Headline: "queued backlog *214,003* (threshold 150k)",
		Context:  "as of 14:05 UTC · prod · live queue depth",
		Body:     "Send workers may be stalled.",
		Action:   "→ <https://x/api/outbox/summary|Outbox summary>",
		Footer:   "live prod state",
	}
	blocks := Render(m)
	types := blockTypes(blocks)
	// Severity headline is a section (the modal-only alert block must never appear).
	if types[0] != "section" {
		t.Fatalf("first block = %q, want section", types[0])
	}
	for _, ty := range types {
		if ty == "alert" {
			t.Fatal("Render emitted the modal-only alert block — rejected by chat.postMessage")
		}
	}
	if types[len(types)-1] != "context" { // footer last
		t.Fatalf("last block = %q, want context", types[len(types)-1])
	}
	// severity emoji present on the headline
	head := blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	if got := []rune(head)[0]; got != '\U0001F534' {
		t.Fatalf("headline missing 🔴 severity emoji, got %q", string(got))
	}
}

func TestFallbackHasEmoji(t *testing.T) {
	if got := Fallback(Message{Tier: TierResolved, Scope: "OUTBOX", Headline: "cleared"}); []rune(got)[0] != '✅' {
		t.Fatalf("fallback = %q, want leading ✅", got)
	}
}

// fakeBlockNotifier records the last blocks call.
type fakeBlockNotifier struct {
	blocks   []any
	fallback string
	err      error
}

func (f *fakeBlockNotifier) Notify(title, body string) error { return errors.New("should not be called") }
func (f *fakeBlockNotifier) Name() string                    { return "fake" }
func (f *fakeBlockNotifier) NotifyBlocks(b []any, fb string) error {
	f.blocks, f.fallback = b, fb
	return f.err
}

func TestDeliverUsesBlocksWhenSupported(t *testing.T) {
	f := &fakeBlockNotifier{}
	if err := Deliver(f, Message{Tier: TierWatch, Scope: "STORAGE", Headline: "WAL ~12 GB"}); err != nil {
		t.Fatal(err)
	}
	if len(f.blocks) == 0 || f.fallback == "" {
		t.Fatal("Deliver did not route through NotifyBlocks")
	}
}

func TestDeliverFallsBackToNotify(t *testing.T) {
	// NoopNotifier implements Notify but not BlockNotifier -> legacy path, no panic.
	if err := Deliver(NoopNotifier{}, Message{Tier: TierEvent, Scope: "x", Headline: "y"}); err != nil {
		t.Fatal(err)
	}
}

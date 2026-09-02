package notify

import (
	"errors"
	"strings"
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

func headText(blocks []any) string {
	return blocks[0].(map[string]any)["text"].(map[string]any)["text"].(string)
}

func TestRenderAnatomyAndNoAlertBlock(t *testing.T) {
	m := Message{
		Tier: TierAlert, Scope: ScopeSend,
		Headline: "queued backlog *214,003* rows",
		Context:  "12:05 MT · prod",
		Body:     "Threshold: 150,000",
		Action:   "Run: `curl -s $HOST/api/outbox/summary`",
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
	if got, want := len(types), 4; got != want {
		t.Fatalf("block count = %d (%v), want %d", got, types, want)
	}
	if head := headText(blocks); !strings.HasPrefix(head, "\U0001F6A8 ALERT · Send · ") {
		t.Fatalf("headline = %q, want 🚨 ALERT · Send · … prefix", head)
	}
}

func TestFallbackCarriesTierMark(t *testing.T) {
	got := Fallback(Message{Tier: TierOK, Scope: ScopeSend, Headline: "queued backlog cleared"})
	if want := "✅ OK · Send · queued backlog cleared"; got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

func TestEventTierHasNoMark(t *testing.T) {
	got := Fallback(Message{Tier: TierEvent, Scope: ScopeConversion, Headline: "Sam's Club (EF 8241) · payout *$27.50*"})
	if want := "Conversion · Sam's Club (EF 8241) · payout *$27.50*"; got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
}

// fakeBlockNotifier records the last blocks call. Transport is faked: these
// tests never touch the network.
type fakeBlockNotifier struct {
	blocks   []any
	fallback string
	err      error
}

func (f *fakeBlockNotifier) Notify(title, body string) error {
	return errors.New("should not be called")
}
func (f *fakeBlockNotifier) Name() string { return "fake" }
func (f *fakeBlockNotifier) NotifyBlocks(b []any, fb string) error {
	f.blocks, f.fallback = b, fb
	return f.err
}

func TestDeliverUsesBlocksWhenSupported(t *testing.T) {
	f := &fakeBlockNotifier{}
	if err := Deliver(f, Message{Tier: TierWarn, Scope: ScopeDB, Headline: "retained WAL *12.4 GB*"}); err != nil {
		t.Fatal(err)
	}
	if len(f.blocks) == 0 || f.fallback == "" {
		t.Fatal("Deliver did not route through NotifyBlocks")
	}
}

func TestDeliverFallsBackToNotify(t *testing.T) {
	// NoopNotifier implements Notify but not BlockNotifier -> legacy path, no panic.
	if err := Deliver(NoopNotifier{}, Message{Tier: TierEvent, Scope: ScopeReport, Headline: "y"}); err != nil {
		t.Fatal(err)
	}
}

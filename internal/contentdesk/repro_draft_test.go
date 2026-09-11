//go:build repro

package contentdesk

// Live-API reproduction of one draft call, byte-for-byte as the pipeline sends
// it. Never runs in CI (build tag). Usage:
//   REPRO_FIXTURE=fixture.json REPRO_OUT=out.txt go test -tags repro -run TestReproDraft ./internal/contentdesk/

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestReproDraft(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("REPRO_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Brief  Brief   `json:"brief"`
		Site   Site    `json:"site"`
		Claims []Claim `json:"claims"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	in := PipelineInput{OrgID: "repro", Brief: fx.Brief, Site: fx.Site}
	l := NewAnthropicLLM(nil)
	model := l.Models.For(TierWrite)
	params, err := l.buildParams(GenerateRequest{OrgID: "repro", Tier: TierWrite, System: draftSystem,
		Prompt: draftPrompt(in, fx.Claims), Schema: draftSchema(), MaxTokens: draftMaxTokens}, model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	stream := l.client.Messages.NewStreaming(ctx, params)
	msg := anthropic.Message{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	var text string
	for _, b := range msg.Content {
		if tb, ok := b.AsAny().(anthropic.TextBlock); ok {
			text += tb.Text
		}
	}
	if err := os.WriteFile(os.Getenv("REPRO_OUT"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s stop=%s in=%d out=%d chars=%d claims=%d prompt_chars=%d", model, msg.StopReason,
		msg.Usage.InputTokens, msg.Usage.OutputTokens, len(text), len(fx.Claims), len(draftPrompt(in, fx.Claims)))
}

package contentdesk

import "testing"

func TestContentDeskAPIKey_PrefersDedicatedVar(t *testing.T) {
	t.Setenv("CONTENT_DESK_ANTHROPIC_API_KEY", "  desk-key  ")
	if got := ContentDeskAPIKey(); got != "desk-key" {
		t.Fatalf("ContentDeskAPIKey() = %q, want trimmed dedicated key", got)
	}
}

func TestContentDeskAPIKey_EmptyMeansNoOverride(t *testing.T) {
	t.Setenv("CONTENT_DESK_ANTHROPIC_API_KEY", "")
	if got := ContentDeskAPIKey(); got != "" {
		t.Fatalf("ContentDeskAPIKey() = %q, want empty (SDK falls back to ANTHROPIC_API_KEY)", got)
	}
	t.Setenv("CONTENT_DESK_ANTHROPIC_API_KEY", "   ")
	if got := ContentDeskAPIKey(); got != "" {
		t.Fatalf("whitespace-only key must not override: got %q", got)
	}
}

func TestNewAnthropicLLM_BuildsWithDedicatedKey(t *testing.T) {
	t.Setenv("CONTENT_DESK_ANTHROPIC_API_KEY", "desk-key")
	if l := NewAnthropicLLM(nil); l == nil {
		t.Fatal("NewAnthropicLLM returned nil")
	}
}

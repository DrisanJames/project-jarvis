package contentdesk

import (
	"strings"
	"testing"
)

// Live 2026-09-11: an unbounded draft prompt ran a draft past 16000 output
// tokens ("model output truncated at max_tokens").
func TestDraftIsLengthBoundedWithHeadroom(t *testing.T) {
	if !strings.Contains(draftSystem, "Length: 700–1,400 words") {
		t.Fatal("draftSystem must bound article length")
	}
	// ~1,400 words of block JSON with claim_refs is well under 12k tokens;
	// keep 2x headroom but stay far inside the 10-minute request timeout.
	if draftMaxTokens < 24000 || draftMaxTokens > 40000 {
		t.Fatalf("draftMaxTokens = %d, want 24000..40000", draftMaxTokens)
	}
}

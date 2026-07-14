package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestApplyYahooNewsletterFollowupCaps pins the yahoo-newsletter-only drip gate
// (operator 2026-07-14). The gate lives on the follow-up per-ISP caps: it must
// remove yahoo from the OFFER claim and confine the NEWSLETTER claim to yahoo —
// and it must be a strict no-op when the flag is off (the kill-switch contract).
func TestApplyYahooNewsletterFollowupCaps(t *testing.T) {
	base := func() map[string]int {
		return map[string]int{"gmail": 150, "yahoo": 20, "apple": 100, "other": 100}
	}

	t.Run("flag off, offer pass: caps unchanged (yahoo still receives offers)", func(t *testing.T) {
		out := applyYahooNewsletterFollowupCaps(base(), false, false)
		assert.Equal(t, 20, out["yahoo"], "flag off must not touch yahoo — legacy behavior")
		assert.Equal(t, base(), out)
	})

	t.Run("flag on, offer pass: yahoo removed from the offer claim", func(t *testing.T) {
		out := applyYahooNewsletterFollowupCaps(base(), true, false)
		assert.Equal(t, 0, out["yahoo"], "offer pass must exclude yahoo when flag on")
		// Every other ISP is untouched — the ban is yahoo-only.
		assert.Equal(t, 150, out["gmail"])
		assert.Equal(t, 100, out["apple"])
		assert.Equal(t, 100, out["other"])
	})

	t.Run("newsletter pass: claim confined to yahoo ONLY", func(t *testing.T) {
		// The yahoo-newsletter pass fires regardless of the flag value at this
		// layer (the flag gates whether the pass is INVOKED, in tickFollowups).
		for _, flag := range []bool{true, false} {
			out := applyYahooNewsletterFollowupCaps(base(), flag, true)
			assert.Equal(t, 20, out["yahoo"], "newsletter pass keeps the yahoo cap")
			assert.Equal(t, 0, out["gmail"], "newsletter pass zeros every non-yahoo ISP")
			assert.Equal(t, 0, out["apple"])
			assert.Equal(t, 0, out["other"])
		}
	})

	t.Run("does not mutate the input map", func(t *testing.T) {
		in := base()
		_ = applyYahooNewsletterFollowupCaps(in, true, false)
		assert.Equal(t, 20, in["yahoo"], "input caps map must not be mutated")
		_ = applyYahooNewsletterFollowupCaps(in, true, true)
		assert.Equal(t, 150, in["gmail"], "input caps map must not be mutated")
	})
}

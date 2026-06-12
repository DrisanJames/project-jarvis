package worker

// Unit tests for the click-drip routing decision (isClickDripEnrollment)
// and the delay-acceleration test knob (scaleJourneyDelay).
//
// These are the two pure-ish branches added when the JourneyExecutor and
// JourneyEventEnroller were wired in-process (2026-06-01). The send itself
// (JourneyClickDripSender.Send → PMTA) is verified end-to-end in production
// per the quality-gate rule; here we lock down the decision logic that
// decides WHETHER to take the PMTA path and HOW LONG to wait.

import (
	"strings"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsClickDripEnrollment(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]interface{}
		want bool
	}{
		{
			name: "nil metadata is not click-drip",
			meta: nil,
			want: false,
		},
		{
			name: "empty metadata is not click-drip",
			meta: map[string]interface{}{},
			want: false,
		},
		{
			name: "enrolled_via=click_postback wins",
			meta: map[string]interface{}{"enrolled_via": "click_postback"},
			want: true,
		},
		{
			name: "source=click_postback wins",
			meta: map[string]interface{}{"source": "click_postback"},
			want: true,
		},
		{
			name: "sending_profile_id presence is sufficient",
			meta: map[string]interface{}{"sending_profile_id": "eeeeeeee-1111-2222-3333-444444444444"},
			want: true,
		},
		{
			name: "empty sending_profile_id does not count",
			meta: map[string]interface{}{"sending_profile_id": ""},
			want: false,
		},
		{
			name: "segment-triggered (Welcome) enrollment is NOT click-drip",
			meta: map[string]interface{}{"enrolled_via": "segment", "segment_id": "abc"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isClickDripEnrollment(Enrollment{Metadata: tc.meta})
			require.Equal(t, tc.want, got)
		})
	}
}

func TestScaleJourneyDelay(t *testing.T) {
	const oneHour = time.Hour

	t.Run("unset env returns delay unchanged", func(t *testing.T) {
		os.Unsetenv("JOURNEY_DELAY_SCALE")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("invalid env returns delay unchanged", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "not-a-number")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("zero or negative scale returns delay unchanged", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "0")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
		t.Setenv("JOURNEY_DELAY_SCALE", "-1")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("scale shrinks the delay", func(t *testing.T) {
		// 0.5 of 1h = 30m, comfortably above the 5s floor.
		t.Setenv("JOURNEY_DELAY_SCALE", "0.5")
		require.Equal(t, 30*time.Minute, scaleJourneyDelay(oneHour))
	})

	t.Run("aggressive scale is floored at 5s", func(t *testing.T) {
		// 0.000001 of 1h = 3.6ms → clamped to the 5s floor so the
		// executor's wait scheduling never collapses to zero.
		t.Setenv("JOURNEY_DELAY_SCALE", "0.000001")
		require.Equal(t, 5*time.Second, scaleJourneyDelay(oneHour))
	})

	t.Run("scale up is honored", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "2")
		require.Equal(t, 2*oneHour, scaleJourneyDelay(oneHour))
	})
}

// TestShadowCampaignID locks the deterministic shadow-campaign id contract:
// the same offer always maps to the same id (so ensureShadowCampaign can do a
// primary-key lookup instead of a name seq scan), different offers map to
// different ids, and the value is a valid UUID. Changing the namespace or the
// derivation string would orphan every existing shadow campaign, so this test
// guards against an accidental change.
func TestShadowCampaignID(t *testing.T) {
	a1 := shadowCampaignID("9539")
	a2 := shadowCampaignID("9539")
	b := shadowCampaignID("7667")

	require.Equal(t, a1, a2, "same offer must yield a stable id")
	require.NotEqual(t, a1, b, "different offers must yield different ids")
	require.Len(t, a1, 36, "must be a canonical UUID string")
	// Pin the exact value so a namespace/derivation change is caught.
	require.Equal(t, "55f62e3e-dccc-5181-812c-c5459661d5ef", a1)
}

func TestReplaceMoneyMergeTags(t *testing.T) {
	html := `<a href="https://www.eos57ytf.com/K4C5ZLC/PS8241/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}">x</a>` +
		`<a href="https://x.com/?s=%7B%7Bsubscriber.id%7D%7D">y</a>` +
		`<span>{{ subscriber.id }}</span>`
	out := replaceMoneyMergeTags(html, "abc-123", "deals@em.myownhealth.net")
	for _, bad := range []string{"{{subscriber.id}}", "{{ subscriber.id }}", "{{brand.domain}}", "%7B%7Bsubscriber.id%7D%7D"} {
		if strings.Contains(out, bad) {
			t.Fatalf("unrendered tag %q survived: %s", bad, out)
		}
	}
	if !strings.Contains(out, "sub1=abc-123") || !strings.Contains(out, "sub2=myownhealth.net") {
		t.Fatalf("substitution wrong: %s", out)
	}
	// em. prefix stripping must not mangle apexes that merely start with 'm'
	out2 := replaceMoneyMergeTags(`{{brand.domain}}`, "x", "a@em.myrepairdiy.com")
	if out2 != "myrepairdiy.com" {
		t.Fatalf("brand derivation wrong: %s", out2)
	}
}

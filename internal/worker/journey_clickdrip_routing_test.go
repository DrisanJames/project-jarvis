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

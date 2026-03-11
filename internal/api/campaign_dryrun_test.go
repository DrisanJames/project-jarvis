package api

import (
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDryRun_NormalizationAndWaveSpecs(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	startAt := now.Add(1 * time.Hour)

	input := engine.PMTACampaignInput{
		Name:          "dry-run-test",
		SendingDomain: "example.com",
		TargetISPs:    []engine.ISP{"gmail", "yahoo", "microsoft"},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 1000, TimeSpans: []engine.PMTATimeSpanInput{{
				StartAt: &startAt,
			}}},
			{ISP: "yahoo", Quota: 500, TimeSpans: []engine.PMTATimeSpanInput{{
				StartAt: &startAt,
			}}},
			{ISP: "microsoft", Quota: 300, TimeSpans: []engine.PMTATimeSpanInput{{
				StartAt: &startAt,
			}}},
		},
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Test",
			HTMLContent: "<p>test</p>",
		}},
	}

	normalized, err := normalizePMTACampaignInput(input)
	require.NoError(t, err)
	assert.Len(t, normalized.Plans, 3)

	for _, plan := range normalized.Plans {
		waves := buildPMTAWaveSpecs("dry-run-test", plan, plan.Quota)
		assert.GreaterOrEqual(t, len(waves), minWavesPerISP,
			"ISP %s should have at least %d waves", plan.ISP, minWavesPerISP)

		if len(waves) >= 2 {
			first := waves[0].ScheduledAt
			last := waves[len(waves)-1].ScheduledAt
			span := last.Sub(first)
			assert.GreaterOrEqual(t, span, defaultThrottleDuration-15*time.Minute,
				"ISP %s wave span %v should be at least %v", plan.ISP, span, defaultThrottleDuration)
		}

		total := 0
		for _, w := range waves {
			assert.Greater(t, w.PlannedRecipients, 0)
			total += w.PlannedRecipients
		}
		assert.Equal(t, plan.Quota, total,
			"ISP %s total planned (%d) should equal quota (%d)", plan.ISP, total, plan.Quota)
	}
}

func TestDryRun_WaveSanityCheck(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	startAt := now.Add(1 * time.Hour)

	input := engine.PMTACampaignInput{
		Name:          "sanity-check-test",
		SendingDomain: "example.com",
		TargetISPs:    []engine.ISP{"gmail"},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 100, TimeSpans: []engine.PMTATimeSpanInput{{
				StartAt: &startAt,
			}}},
		},
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Test",
			HTMLContent: "<p>test</p>",
		}},
	}

	normalized, err := normalizePMTACampaignInput(input)
	require.NoError(t, err)

	wavesByISP := make(map[string][]pmtaWaveSpec)
	for _, plan := range normalized.Plans {
		count := 100
		if plan.Quota > 0 {
			count = plan.Quota
		}
		wavesByISP[plan.ISP] = buildPMTAWaveSpecs("sanity-test", plan, count)
	}

	err = waveSanityCheck(normalized.Plans, wavesByISP)
	assert.NoError(t, err, "valid campaign should pass sanity check")
}

func TestDryRun_SmallAudienceStillGetsMinWaves(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	startAt := now.Add(1 * time.Hour)

	input := engine.PMTACampaignInput{
		Name:          "small-audience",
		SendingDomain: "example.com",
		TargetISPs:    []engine.ISP{"gmail"},
		ISPPlans: []engine.PMTAISPScheduleInput{
			{ISP: "gmail", Quota: 3, TimeSpans: []engine.PMTATimeSpanInput{{
				StartAt: &startAt,
			}}},
		},
		Variants: []engine.ContentVariant{{
			VariantName: "A",
			Subject:     "Test",
			HTMLContent: "<p>test</p>",
		}},
	}

	normalized, err := normalizePMTACampaignInput(input)
	require.NoError(t, err)

	for _, plan := range normalized.Plans {
		waves := buildPMTAWaveSpecs("small-test", plan, 3)
		total := 0
		for _, w := range waves {
			total += w.PlannedRecipients
		}
		assert.Equal(t, 3, total, "all 3 recipients should be planned")
		assert.GreaterOrEqual(t, len(waves), 1, "at least 1 wave for small audience")
	}
}

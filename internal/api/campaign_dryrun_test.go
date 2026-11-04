package api

import (
	"fmt"
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

		if len(waves) >= 2 && plan.Quota >= 500 {
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

// WP-E (2026-11-04): PMTA_MIN_WAVE_RECIPIENTS consolidates auto-sized cells
// whose waves average below the minimum; large cells and the disabled state
// must be byte-identical to the unconsolidated (pre-change) output.
func TestBuildPMTAWaveSpecs_MinWaveRecipients(t *testing.T) {
	anchor := time.Date(2026, 11, 5, 8, 1, 0, 0, time.UTC)
	mkPlan := func(mode string, batch int, spans ...[2]time.Duration) pmtaNormalizedPlan {
		p := pmtaNormalizedPlan{
			ISP:     "microsoft",
			Cadence: engine.PMTACadenceInput{Mode: mode, EveryMinutes: 15, BatchSize: batch},
		}
		for _, s := range spans {
			p.TimeSpans = append(p.TimeSpans, pmtaNormalizedTimeSpan{
				StartAt: anchor.Add(s[0]), EndAt: anchor.Add(s[1]), Source: "duration-calc",
			})
		}
		return p
	}
	twelveH := [2]time.Duration{0, 12 * time.Hour}
	build := func(env string, p pmtaNormalizedPlan, n int) []pmtaWaveSpec {
		t.Setenv("PMTA_MIN_WAVE_RECIPIENTS", env)
		// copy spans: buildPMTAWaveSpecs may rewrite TimeSpans[0].EndAt
		p.TimeSpans = append([]pmtaNormalizedTimeSpan(nil), p.TimeSpans...)
		return buildPMTAWaveSpecs("c1", p, n)
	}
	sum := func(ws []pmtaWaveSpec) int {
		s := 0
		for _, w := range ws {
			s += w.PlannedRecipients
		}
		return s
	}

	t.Run("16 recips 12h 15min -> 1 wave at anchor", func(t *testing.T) {
		old := build("0", mkPlan("interval", 0, twelveH), 16)
		require.Len(t, old, 16) // pre-change: 16 one-recipient waves
		ws := build("", mkPlan("interval", 0, twelveH), 16)
		require.Len(t, ws, 1)
		assert.Equal(t, anchor, ws[0].ScheduledAt)
		assert.Equal(t, 16, ws[0].PlannedRecipients)
		assert.Equal(t, 16, ws[0].BatchSize)
		assert.Equal(t, "c1:microsoft:1", ws[0].IdempotencyKey)
		assert.Equal(t, old[0].WindowStartAt, ws[0].WindowStartAt)
		assert.Equal(t, old[0].WindowEndAt, ws[0].WindowEndAt)
	})

	t.Run("763 recips -> <=16 waves evenly spaced within window", func(t *testing.T) {
		old := build("0", mkPlan("interval", 0, twelveH), 763)
		require.Len(t, old, 48) // pre-change: 48 waves of <=16
		ws := build("50", mkPlan("interval", 0, twelveH), 763)
		require.LessOrEqual(t, len(ws), 16)
		require.Equal(t, 16, len(ws))
		assert.Equal(t, 763, sum(ws))
		assert.Equal(t, anchor, ws[0].ScheduledAt)
		assert.False(t, ws[len(ws)-1].ScheduledAt.After(old[len(old)-1].ScheduledAt), "last wave must not move later")
		assert.Equal(t, old[len(old)-1].ScheduledAt, ws[len(ws)-1].ScheduledAt)
		minGap, maxGap := 24*time.Hour, time.Duration(0)
		for i, w := range ws {
			assert.Equal(t, i+1, w.WaveNumber)
			assert.Greater(t, w.PlannedRecipients, 0)
			if i < len(ws)-1 { // same allocation as before: full batches, remainder on the last wave
				assert.Equal(t, 48, w.PlannedRecipients)
			} else {
				assert.Equal(t, 763-15*48, w.PlannedRecipients)
			}
			assert.Equal(t, 48, w.BatchSize)
			assert.Equal(t, fmt.Sprintf("c1:microsoft:%d", i+1), w.IdempotencyKey)
			assert.False(t, w.ScheduledAt.Before(w.WindowStartAt) || w.ScheduledAt.After(w.WindowEndAt))
			if i > 0 {
				g := w.ScheduledAt.Sub(ws[i-1].ScheduledAt)
				if g < minGap {
					minGap = g
				}
				if g > maxGap {
					maxGap = g
				}
			}
		}
		assert.LessOrEqual(t, maxGap-minGap, 15*time.Minute, "even spacing within one cadence slot")
		require.NoError(t, waveSanityCheck([]pmtaNormalizedPlan{mkPlan("interval", 0, twelveH)},
			map[string][]pmtaWaveSpec{"microsoft": ws}))
		t.Logf("763: %d waves, gaps %v..%v, last %s", len(ws), minGap, maxGap, ws[len(ws)-1].ScheduledAt.Format(time.RFC3339))
	})

	t.Run("50000 recips byte-equal to pre-change", func(t *testing.T) {
		golden := fmt.Sprintf("%#v", build("0", mkPlan("interval", 0, twelveH), 50000))
		assert.Equal(t, golden, fmt.Sprintf("%#v", build("", mkPlan("interval", 0, twelveH), 50000)))
		assert.Equal(t, golden, fmt.Sprintf("%#v", build("50", mkPlan("interval", 0, twelveH), 50000)))
	})

	t.Run("explicit batch_size untouched", func(t *testing.T) {
		p := mkPlan("interval", 10, twelveH)
		assert.Equal(t, fmt.Sprintf("%#v", build("0", p, 763)), fmt.Sprintf("%#v", build("50", p, 763)))
	})

	perSpan := func(ws []pmtaWaveSpec) map[time.Time][]pmtaWaveSpec {
		m := map[time.Time][]pmtaWaveSpec{}
		for _, w := range ws {
			m[w.WindowStartAt] = append(m[w.WindowStartAt], w)
		}
		return m
	}
	// checkInvariants: totals, per-span share/anchor/last, wave >= 1, sanity.
	checkInvariants := func(t *testing.T, p pmtaNormalizedPlan, old, ws []pmtaWaveSpec) {
		t.Helper()
		require.Equal(t, sum(old), sum(ws))
		po, pn := perSpan(old), perSpan(ws)
		require.Equal(t, len(po), len(pn), "every span keeps >=1 wave")
		for k, o := range po {
			n := pn[k]
			require.NotEmpty(t, n)
			assert.Equal(t, sum(o), sum(n), "span %s share", k)
			assert.Equal(t, o[0].ScheduledAt, n[0].ScheduledAt, "span anchor")
			assert.False(t, n[len(n)-1].ScheduledAt.After(o[len(o)-1].ScheduledAt), "span last not later")
		}
		for i, w := range ws {
			assert.Equal(t, i+1, w.WaveNumber)
			assert.Equal(t, fmt.Sprintf("c1:microsoft:%d", i+1), w.IdempotencyKey)
			assert.GreaterOrEqual(t, w.PlannedRecipients, 1)
			assert.LessOrEqual(t, w.PlannedRecipients, w.BatchSize)
			assert.False(t, w.ScheduledAt.Before(w.WindowStartAt) || w.ScheduledAt.After(w.WindowEndAt))
		}
		plans := []pmtaNormalizedPlan{p}
		if waveSanityCheck(plans, map[string][]pmtaWaveSpec{"microsoft": old}) == nil {
			assert.NoError(t, waveSanityCheck(plans, map[string][]pmtaWaveSpec{"microsoft": ws}))
		}
	}

	t.Run("two spans n=40 -> one wave per span, share kept", func(t *testing.T) {
		// 08:01-11:01 + 19:01-22:01
		p := mkPlan("interval", 0, [2]time.Duration{0, 3 * time.Hour}, [2]time.Duration{11 * time.Hour, 14 * time.Hour})
		old := build("0", p, 40)
		ws := build("50", p, 40)
		require.Len(t, ws, 2)
		po, pn := perSpan(old), perSpan(ws)
		s1, s2 := anchor, anchor.Add(11*time.Hour)
		require.Len(t, pn[s1], 1)
		require.Len(t, pn[s2], 1)
		assert.InDelta(t, sum(po[s1]), sum(pn[s1]), 1)
		assert.InDelta(t, sum(po[s2]), sum(pn[s2]), 1)
		assert.Equal(t, s1, pn[s1][0].ScheduledAt)
		assert.Equal(t, s2, pn[s2][0].ScheduledAt)
		checkInvariants(t, p, old, ws)
		t.Logf("two-span n=40: old {%d:%d, %d:%d} new {%d:%d, %d:%d}", len(po[s1]), sum(po[s1]), len(po[s2]), sum(po[s2]),
			len(pn[s1]), sum(pn[s1]), len(pn[s2]), sum(pn[s2]))
	})

	t.Run("high env never fails sanity: env=200 n=550", func(t *testing.T) {
		p := mkPlan("interval", 0, twelveH)
		ws := build("200", p, 550)
		assert.GreaterOrEqual(t, len(ws), minWavesPerISP)
		assert.NoError(t, waveSanityCheck([]pmtaNormalizedPlan{p}, map[string][]pmtaWaveSpec{"microsoft": ws}))
		checkInvariants(t, p, build("0", p, 550), ws)
	})

	t.Run("high env never fails sanity: env=10000 n=5000", func(t *testing.T) {
		p := mkPlan("interval", 0, twelveH)
		ws := build("10000", p, 5000)
		assert.GreaterOrEqual(t, len(ws), minWavesPerISP)
		assert.NoError(t, waveSanityCheck([]pmtaNormalizedPlan{p}, map[string][]pmtaWaveSpec{"microsoft": ws}))
		checkInvariants(t, p, build("0", p, 5000), ws)
	})

	t.Run("invariant sweep any env any n", func(t *testing.T) {
		shapes := [][][2]time.Duration{
			{twelveH},
			{{0, 2 * time.Hour}, {6 * time.Hour, 8 * time.Hour}},
			{{0, 3 * time.Hour}, {11 * time.Hour, 14 * time.Hour}},
			{{0, 1 * time.Hour}, {2 * time.Hour, 3 * time.Hour}, {5 * time.Hour, 9 * time.Hour}},
		}
		for _, env := range []string{"1", "10", "50", "200", "499", "1000", "10000"} {
			for _, n := range []int{1, 2, 7, 16, 40, 99, 300, 499, 500, 501, 550, 763, 2000, 5000, 50000} {
				for _, mode := range []string{"interval", "single"} {
					for _, sh := range shapes {
						p := mkPlan(mode, 0, sh...)
						checkInvariants(t, p, build("0", p, n), build(env, p, n))
					}
				}
			}
		}
	})

	t.Run("env 0 and invalid env", func(t *testing.T) {
		for _, n := range []int{1, 16, 499, 763, 5000, 50000} {
			for _, mode := range []string{"interval", "single"} {
				off := build("0", mkPlan(mode, 0, twelveH), n)
				assert.Equal(t, fmt.Sprintf("%#v", off),
					fmt.Sprintf("%#v", consolidatePMTASmallWaves(off, "c1", "microsoft", 0)), "n=%d mode=%s", n, mode)
				assert.Equal(t, n, sum(off))
			}
		}
		t.Setenv("PMTA_MIN_WAVE_RECIPIENTS", "abc")
		assert.Equal(t, 50, pmtaMinWaveRecipients())
		t.Setenv("PMTA_MIN_WAVE_RECIPIENTS", "-3")
		assert.Equal(t, 50, pmtaMinWaveRecipients())
		t.Setenv("PMTA_MIN_WAVE_RECIPIENTS", "0")
		assert.Equal(t, 0, pmtaMinWaveRecipients())
	})
}

package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// RecallSynthesis is the aggregated insight produced by querying an agent's
// conviction memory for a given scenario. Pattern recognition happens here,
// at query time — never at storage time.
type RecallSynthesis struct {
	TotalRelevant   int      `json:"total_relevant"`
	WillCount       int      `json:"will_count"`
	WontCount       int      `json:"wont_count"`
	DominantVerdict Verdict  `json:"dominant_verdict"`
	Confidence      float64  `json:"confidence"`
	Summary         string   `json:"summary"`
	KeyObservations []string `json:"key_observations"`
	RateRange       [2]int   `json:"rate_range"`
	AvgRecoveryMin  float64  `json:"avg_recovery_min"`
	CommonDSNCodes  []string `json:"common_dsn_codes"`
	TimePatterns    []string `json:"time_patterns"`
}

// SynthesizeRecall takes matched convictions from RecallSimilar and produces
// an aggregated analysis grounded in specific micro-observations.
func SynthesizeRecall(convictions []ScoredConviction, query MicroContext) RecallSynthesis {
	s := RecallSynthesis{}
	if len(convictions) == 0 {
		s.Summary = "No relevant memories found for this scenario. The agent has not encountered a similar situation yet."
		return s
	}

	s.TotalRelevant = len(convictions)

	// Verdict counts
	for _, sc := range convictions {
		switch sc.Conviction.Verdict {
		case VerdictWill:
			s.WillCount++
		case VerdictWont:
			s.WontCount++
		}
	}

	if s.WillCount >= s.WontCount {
		s.DominantVerdict = VerdictWill
	} else {
		s.DominantVerdict = VerdictWont
	}

	// Confidence: ratio of dominant verdict scaled by average similarity
	dominant := math.Max(float64(s.WillCount), float64(s.WontCount))
	verdictRatio := dominant / float64(s.TotalRelevant)
	avgSim := 0.0
	for _, sc := range convictions {
		avgSim += sc.Similarity
	}
	avgSim /= float64(len(convictions))
	s.Confidence = math.Min(verdictRatio*avgSim*1.2, 1.0)

	// Rate range from convictions that have an EffectiveRate
	minRate, maxRate := math.MaxInt32, 0
	rateCount := 0
	for _, sc := range convictions {
		r := sc.Conviction.Context.EffectiveRate
		if r > 0 {
			if r < minRate {
				minRate = r
			}
			if r > maxRate {
				maxRate = r
			}
			rateCount++
		}
	}
	if rateCount > 0 {
		s.RateRange = [2]int{minRate, maxRate}
	}

	// Average recovery time
	recoverySum := 0.0
	recoveryCount := 0
	for _, sc := range convictions {
		if sc.Conviction.Context.RecoveryTimeMin > 0 {
			recoverySum += sc.Conviction.Context.RecoveryTimeMin
			recoveryCount++
		}
	}
	if recoveryCount > 0 {
		s.AvgRecoveryMin = recoverySum / float64(recoveryCount)
	}

	// DSN code frequency
	dsnFreq := make(map[string]int)
	for _, sc := range convictions {
		for _, code := range sc.Conviction.Context.DSNCodes {
			dsnFreq[code]++
		}
	}
	type kv struct {
		Code  string
		Count int
	}
	var dsnPairs []kv
	for code, count := range dsnFreq {
		dsnPairs = append(dsnPairs, kv{code, count})
	}
	sort.Slice(dsnPairs, func(i, j int) bool { return dsnPairs[i].Count > dsnPairs[j].Count })
	for i, p := range dsnPairs {
		if i >= 5 {
			break
		}
		s.CommonDSNCodes = append(s.CommonDSNCodes, p.Code)
	}

	// Time patterns: day-of-week and hour distribution
	dayFreq := make(map[string]int)
	hourBuckets := make(map[string]int) // "morning", "afternoon", "evening", "night"
	for _, sc := range convictions {
		if sc.Conviction.Context.DayOfWeek != "" {
			dayFreq[sc.Conviction.Context.DayOfWeek]++
		}
		h := sc.Conviction.Context.HourUTC
		switch {
		case h >= 6 && h < 12:
			hourBuckets["morning (06-12 UTC)"]++
		case h >= 12 && h < 18:
			hourBuckets["afternoon (12-18 UTC)"]++
		case h >= 18 && h < 24:
			hourBuckets["evening (18-00 UTC)"]++
		default:
			hourBuckets["night (00-06 UTC)"]++
		}
	}
	for day, count := range dayFreq {
		if count >= 2 {
			pct := float64(count) / float64(s.TotalRelevant) * 100
			s.TimePatterns = append(s.TimePatterns, fmt.Sprintf(
				"%d of %d observations (%.0f%%) occurred on %s",
				count, s.TotalRelevant, pct, day,
			))
		}
	}
	for bucket, count := range hourBuckets {
		if count >= 2 {
			pct := float64(count) / float64(s.TotalRelevant) * 100
			s.TimePatterns = append(s.TimePatterns, fmt.Sprintf(
				"%d of %d observations (%.0f%%) occurred during %s",
				count, s.TotalRelevant, pct, bucket,
			))
		}
	}

	// Key observations
	s.KeyObservations = buildKeyObservations(convictions, s)

	// Summary
	s.Summary = buildSummary(convictions, query, s)

	return s
}

func buildKeyObservations(convictions []ScoredConviction, s RecallSynthesis) []string {
	var obs []string

	if s.WontCount > 0 && s.WillCount > 0 {
		obs = append(obs, fmt.Sprintf(
			"Agent decided WONT %d times and WILL %d times in similar scenarios",
			s.WontCount, s.WillCount,
		))
	} else if s.WontCount > 0 {
		obs = append(obs, fmt.Sprintf(
			"Agent has ALWAYS decided WONT in all %d similar scenarios",
			s.WontCount,
		))
	} else {
		obs = append(obs, fmt.Sprintf(
			"Agent has ALWAYS decided WILL in all %d similar scenarios",
			s.WillCount,
		))
	}

	if s.RateRange[0] > 0 && s.RateRange[1] > 0 {
		if s.RateRange[0] == s.RateRange[1] {
			obs = append(obs, fmt.Sprintf(
				"Effective rate in similar situations was consistently %d/hr",
				s.RateRange[0],
			))
		} else {
			obs = append(obs, fmt.Sprintf(
				"Effective rate ranged from %d/hr to %d/hr across similar situations",
				s.RateRange[0], s.RateRange[1],
			))
		}
	}

	if s.AvgRecoveryMin > 0 {
		obs = append(obs, fmt.Sprintf(
			"Average recovery time was %.0f minutes",
			s.AvgRecoveryMin,
		))
	}

	if len(s.CommonDSNCodes) > 0 {
		obs = append(obs, fmt.Sprintf(
			"Most common DSN codes: %s",
			strings.Join(s.CommonDSNCodes, ", "),
		))
	}

	// Deferral rate analysis from WONT convictions
	var wontDeferralRates []float64
	for _, sc := range convictions {
		if sc.Conviction.Verdict == VerdictWont && sc.Conviction.Context.DeferralRate > 0 {
			wontDeferralRates = append(wontDeferralRates, sc.Conviction.Context.DeferralRate)
		}
	}
	if len(wontDeferralRates) >= 2 {
		sort.Float64s(wontDeferralRates)
		obs = append(obs, fmt.Sprintf(
			"Backoff was triggered at deferral rates between %.1f%% and %.1f%%",
			wontDeferralRates[0], wontDeferralRates[len(wontDeferralRates)-1],
		))
	}

	// Acceptance rate from WILL convictions
	var willAcceptRates []float64
	for _, sc := range convictions {
		if sc.Conviction.Verdict == VerdictWill && sc.Conviction.Context.AcceptanceRate > 0 {
			willAcceptRates = append(willAcceptRates, sc.Conviction.Context.AcceptanceRate)
		}
	}
	if len(willAcceptRates) >= 2 {
		sort.Float64s(willAcceptRates)
		obs = append(obs, fmt.Sprintf(
			"When conditions were favorable, acceptance rate ranged from %.1f%% to %.1f%%",
			willAcceptRates[0], willAcceptRates[len(willAcceptRates)-1],
		))
	}

	return obs
}

func buildSummary(convictions []ScoredConviction, query MicroContext, s RecallSynthesis) string {
	var parts []string

	parts = append(parts, fmt.Sprintf(
		"Based on %d similar observations:", s.TotalRelevant,
	))

	if s.DominantVerdict == VerdictWont {
		parts = append(parts, fmt.Sprintf(
			"The agent has decided WONT %d out of %d times (%.0f%% confidence).",
			s.WontCount, s.TotalRelevant, s.Confidence*100,
		))
	} else {
		parts = append(parts, fmt.Sprintf(
			"The agent has decided WILL %d out of %d times (%.0f%% confidence).",
			s.WillCount, s.TotalRelevant, s.Confidence*100,
		))
	}

	if len(query.DSNCodes) > 0 {
		parts = append(parts, fmt.Sprintf(
			"When encountering %s deferrals,",
			strings.Join(query.DSNCodes, "/"),
		))
		if s.WontCount > s.WillCount {
			parts = append(parts, "the agent typically reduces rate.")
		} else {
			parts = append(parts, "the agent has generally maintained or increased rate.")
		}
	}

	if s.RateRange[0] > 0 {
		parts = append(parts, fmt.Sprintf(
			"The effective rate that worked ranged from %d/hr to %d/hr.",
			s.RateRange[0], s.RateRange[1],
		))
	}

	if s.AvgRecoveryMin > 0 {
		parts = append(parts, fmt.Sprintf(
			"Recovery typically took %.0f minutes.",
			s.AvgRecoveryMin,
		))
	}

	return strings.Join(parts, " ")
}

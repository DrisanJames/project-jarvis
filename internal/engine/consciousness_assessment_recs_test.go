package engine

// Decision-table tests for deferral-storm attribution in
// buildAssessmentRecommendations (baseline-aware classification, 2026-06).

import (
	"strings"
	"testing"
	"time"
)

func stormPipe(ispName, vmta string, attempted, deferred int64) AssessmentPipe {
	p := AssessmentPipe{ISP: ispName, VMTA: vmta, Pool: "db-" + ispName + "-pool", Attempted: attempted, Deferred: deferred}
	if attempted > 0 {
		p.DeferralPct = round1(100 * float64(deferred) / float64(attempted))
	}
	return p
}

func recsForISP(recs []AssessmentRecommendation, ispName string) []AssessmentRecommendation {
	var out []AssessmentRecommendation
	for _, r := range recs {
		if r.ISP == ispName {
			out = append(out, r)
		}
	}
	return out
}

func TestStormAttribution_PacingWhenOverBaseline(t *testing.T) {
	// AOL case from the operator: 1082 sent today vs 471/day baseline (230%)
	// with a family-wide deferral storm ⇒ pacing (warmup), NOT reputation.
	pipes := []AssessmentPipe{stormPipe("aol", "mta-db-ao1", 1000, 700)}
	famCtx := map[string]familyVolumeContext{
		"aol": {BaselineDailySends: 471, HasBaseline: true, TodaySends: 1082,
			RecentOpenRate: 0.10, PriorOpenRate: 0.11, OpenQuality: openQualityHeld},
	}
	// Trailing-window baseline small enough to ALSO trip the generic spike
	// check — the pacing rec must still be the only rec for the family.
	baseline := map[string]float64{"aol": 100}

	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, nil, baseline, famCtx)
	aol := recsForISP(recs, "aol")
	if len(aol) != 1 {
		t.Fatalf("expected exactly 1 aol rec (pacing, deduped), got %d: %+v", len(aol), aol)
	}
	r := aol[0]
	if r.Type != RecommendationWarmup {
		t.Errorf("type = %q, want warmup (pacing)", r.Type)
	}
	if r.Severity != "critical" {
		t.Errorf("severity = %q, want critical", r.Severity)
	}
	if !strings.Contains(r.Action, "return to baseline pace") {
		t.Errorf("action missing pacing wording: %q", r.Action)
	}
	if strings.Contains(r.Action, "reputation") {
		t.Errorf("pacing rec must not blame reputation: %q", r.Action)
	}
	// Evidence must carry today-vs-baseline volume, deferral %, open quality.
	for _, want := range []string{"1082", "471", "deferred", "open quality: held"} {
		if !strings.Contains(r.Evidence, want) {
			t.Errorf("evidence missing %q: %q", want, r.Evidence)
		}
	}
}

func TestStormAttribution_RateLimitWhenWithinBaselineOpensHeld(t *testing.T) {
	pipes := []AssessmentPipe{stormPipe("aol", "mta-db-ao1", 1000, 700)}
	famCtx := map[string]familyVolumeContext{
		"aol": {BaselineDailySends: 1100, HasBaseline: true, TodaySends: 1000,
			RecentOpenRate: 0.09, PriorOpenRate: 0.10, OpenQuality: openQualityHeld},
	}
	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, nil, nil, famCtx)
	aol := recsForISP(recs, "aol")
	if len(aol) != 1 {
		t.Fatalf("expected 1 aol rec, got %d: %+v", len(aol), aol)
	}
	r := aol[0]
	if r.Type != RecommendationThrottle || r.Severity != "warn" {
		t.Errorf("got type=%q severity=%q, want throttle/warn", r.Type, r.Severity)
	}
	if !strings.Contains(r.Action, "hold aol volume flat, do not grow") {
		t.Errorf("action missing soft rate-limit wording: %q", r.Action)
	}
	if strings.Contains(r.Action, "reputation") {
		t.Errorf("rate-limit rec must not blame reputation: %q", r.Action)
	}
}

func TestStormAttribution_ReputationWhenOpensCollapsed(t *testing.T) {
	pipes := []AssessmentPipe{stormPipe("aol", "mta-db-ao1", 1000, 700)}
	famCtx := map[string]familyVolumeContext{
		"aol": {BaselineDailySends: 1100, HasBaseline: true, TodaySends: 1000,
			RecentOpenRate: 0.02, PriorOpenRate: 0.10, OpenQuality: openQualityCollapsed},
	}
	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, nil, nil, famCtx)
	aol := recsForISP(recs, "aol")
	if len(aol) != 1 {
		t.Fatalf("expected 1 aol rec, got %d: %+v", len(aol), aol)
	}
	r := aol[0]
	if r.Type != RecommendationThrottle || r.Severity != "critical" {
		t.Errorf("got type=%q severity=%q, want throttle/critical", r.Type, r.Severity)
	}
	if !strings.Contains(r.Action, "this is reputation, not routing") {
		t.Errorf("action missing reputation wording: %q", r.Action)
	}
	if !strings.Contains(r.Evidence, "open quality: collapsed") {
		t.Errorf("evidence missing open-quality status: %q", r.Evidence)
	}
}

func TestStormAttribution_NoBaselineFallsBackToReputation(t *testing.T) {
	pipes := []AssessmentPipe{stormPipe("aol", "mta-db-ao1", 1000, 700)}
	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, nil, nil, nil)
	aol := recsForISP(recs, "aol")
	if len(aol) != 1 {
		t.Fatalf("expected 1 aol rec, got %d: %+v", len(aol), aol)
	}
	r := aol[0]
	if r.Type != RecommendationThrottle || r.Severity != "critical" {
		t.Errorf("got type=%q severity=%q, want throttle/critical", r.Type, r.Severity)
	}
	if !strings.Contains(r.Evidence, "unavailable") || !strings.Contains(r.Evidence, "open quality: unknown") {
		t.Errorf("evidence must flag missing baseline + unknown opens: %q", r.Evidence)
	}
}

func TestStormAttribution_DomainScopedStormSuppressedByFamilyAttribution(t *testing.T) {
	pipes := []AssessmentPipe{stormPipe("aol", "mta-db-ao1", 1000, 700)}
	famCtx := map[string]familyVolumeContext{
		"aol": {BaselineDailySends: 471, HasBaseline: true, TodaySends: 1082,
			RecentOpenRate: 0.10, PriorOpenRate: 0.11, OpenQuality: openQualityHeld},
	}
	domainStats := []domainDeferralStat{
		{ISP: "aol", Domain: "em.discountblog.com", Attempted: 500, Deferred: 400, DeferralPct: 80},
	}
	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, domainStats, nil, famCtx)
	for _, r := range recsForISP(recs, "aol") {
		if r.Domain != "" {
			t.Errorf("domain-scoped storm rec should be suppressed when the family already has an attribution: %+v", r)
		}
	}
}

func TestStormAttribution_RerouteUnaffected(t *testing.T) {
	// One bad pipe + one healthy sibling ⇒ routing problem; still a reroute,
	// never a family storm attribution.
	pipes := []AssessmentPipe{
		stormPipe("gmail", "mta-db-gm1", 1000, 700), // 70% deferral
		stormPipe("gmail", "mta-qf-gm1", 1000, 50),  // 5% deferral
	}
	famCtx := map[string]familyVolumeContext{
		"gmail": {BaselineDailySends: 500, HasBaseline: true, TodaySends: 2000, OpenQuality: openQualityHeld},
	}
	recs := buildAssessmentRecommendations(time.Now(), pipes, nil, nil, nil, famCtx)
	var sawReroute bool
	for _, r := range recsForISP(recs, "gmail") {
		switch r.Type {
		case RecommendationReroute:
			sawReroute = true
			if r.TargetVMTA != "mta-qf-gm1" {
				t.Errorf("reroute target = %q, want healthy sibling mta-qf-gm1", r.TargetVMTA)
			}
		case RecommendationThrottle:
			t.Errorf("asymmetric pipes must not produce a family throttle: %+v", r)
		}
	}
	if !sawReroute {
		t.Fatal("expected a reroute rec for the bad gmail pipe")
	}
}

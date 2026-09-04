package api

// Tests for the per-(sending domain × ISP) touch policy.
//
// The behaviour that matters to the operator: seeding ONE Gmail row caps Gmail
// and leaves every other ISP untouched, and it does so while
// DISABLE_SDS_FREQUENCY_CAP is "true" (its production value), because the
// policy is deliberately independent of that switch.

import (
	"strconv"
	"strings"
	"testing"
)

func TestTouchGapWhereIsEmptyWithoutPolicy(t *testing.T) {
	if got := (ispTouchPolicy{}).touchGapWhere("s", "sds_filter"); got != "" {
		t.Fatalf("no policy must render no SQL, got %q", got)
	}
	var nilPolicy ispTouchPolicy
	if got := nilPolicy.touchGapWhere("s", "sds_filter"); got != "" {
		t.Fatalf("nil policy must render no SQL, got %q", got)
	}
}

// A Gmail row must name the Gmail domains and no others, so a Yahoo or
// Microsoft recipient can never match the predicate.
func TestTouchGapWhereScopesToTheNamedISPOnly(t *testing.T) {
	sql := ispTouchPolicy{"gmail": 20}.touchGapWhere("s", "sds_filter")
	for _, want := range []string{"'gmail.com'", "'googlemail.com'", "INTERVAL '20 hours'", "sds_filter.last_mailed_at"} {
		if !strings.Contains(sql, want) {
			t.Errorf("gmail policy SQL missing %s\ngot: %s", want, sql)
		}
	}
	for _, unwanted := range []string{"yahoo.com", "hotmail.com", "outlook.com", "aol.com", "icloud.com"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("gmail-only policy must not mention %s\ngot: %s", unwanted, sql)
		}
	}
	// Must splice after an existing WHERE.
	if !strings.HasPrefix(strings.TrimSpace(sql), "AND (") {
		t.Errorf("fragment must start with AND (, got %q", sql)
	}
}

// The gap is the dial the operator turns to raise touch points.
func TestTouchGapHoursIsTheDial(t *testing.T) {
	for _, gap := range []int{20, 11, 7} {
		sql := ispTouchPolicy{"gmail": gap}.touchGapWhere("s", "sds_filter")
		if !strings.Contains(sql, "INTERVAL '"+strconv.Itoa(gap)+" hours'") {
			t.Errorf("gap %d not rendered: %s", gap, sql)
		}
	}
}

// THE regression: production runs DISABLE_SDS_FREQUENCY_CAP=true. The policy
// must still bind, and must still bring the JOIN it needs to read
// last_mailed_at.
func TestPolicyBindsEvenWhenTheKillSwitchIsOn(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "true")

	none := buildSDSEligibilityClause("em.quizfiesta.com", "s", 0, nil)
	if none.Join != "" || none.Where != "" {
		t.Fatalf("switch on + no policy must be a no-op, got join=%q where=%q", none.Join, none.Where)
	}

	capped := buildSDSEligibilityClause("em.quizfiesta.com", "s", 0, ispTouchPolicy{"gmail": 20})
	if !strings.Contains(capped.Join, "mailing_subscriber_domain_state") {
		t.Errorf("policy needs the SDS join to read last_mailed_at, got %q", capped.Join)
	}
	if !strings.Contains(capped.Where, "gmail.com") || !strings.Contains(capped.Where, "INTERVAL '20 hours'") {
		t.Errorf("policy where not applied: %q", capped.Where)
	}
	// The kill switch still suppresses the SA-2 state exclusion and reordering.
	if strings.Contains(capped.Where, "sds_filter.state") {
		t.Errorf("kill switch must still suppress the state exclusion: %q", capped.Where)
	}
	if len(capped.BindArgs) != 1 || capped.BindArgs[0] != "em.quizfiesta.com" {
		t.Errorf("sending domain must be bound once, got %v", capped.BindArgs)
	}
}

// With the switch off, the policy is additive to the existing SA-2 clause
// rather than replacing it.
func TestPolicyIsAdditiveWhenTheKillSwitchIsOff(t *testing.T) {
	t.Setenv("DISABLE_SDS_FREQUENCY_CAP", "false")
	c := buildSDSEligibilityClause("em.historythinking.com", "s", 0, ispTouchPolicy{"gmail": 7})
	if !strings.Contains(c.Where, "sds_filter.state") {
		t.Errorf("SA-2 state exclusion should be present: %q", c.Where)
	}
	if !strings.Contains(c.Where, "INTERVAL '20 hours'") {
		t.Errorf("SA-2 20h window should be present: %q", c.Where)
	}
	if !strings.Contains(c.Where, "INTERVAL '7 hours'") || !strings.Contains(c.Where, "gmail.com") {
		t.Errorf("policy window should be appended: %q", c.Where)
	}
}

// An unknown ISP name cannot be matched in SQL and must be skipped rather than
// rendering a predicate that matches everything.
func TestUnknownISPRendersNothing(t *testing.T) {
	if got := (ispTouchPolicy{"not-an-isp": 20}).touchGapWhere("s", "sds_filter"); got != "" {
		t.Fatalf("unknown ISP must render no SQL, got %q", got)
	}
}

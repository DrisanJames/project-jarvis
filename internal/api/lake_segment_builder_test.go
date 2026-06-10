package api

import (
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// --- delta guard arithmetic ---

func TestLakeDeltaBlocked(t *testing.T) {
	cases := []struct {
		name     string
		prior    int64
		newCount int64
		force    bool
		want     bool
	}{
		{"no prior — never blocked", 0, 500000, false, false},
		{"small prior at threshold — guard not armed", 10000, 0, false, false},
		{"small prior below threshold — guard not armed", 9999, 100000, false, false},
		{"armed, +60% — blocked", 20000, 32000, false, true},
		{"armed, -60% — blocked", 20000, 8000, false, true},
		{"armed, wiped to zero — blocked", 20000, 0, false, true},
		{"armed, exactly +50% — allowed (strict >)", 20000, 30000, false, false},
		{"armed, exactly -50% — allowed (strict >)", 20000, 10000, false, false},
		{"armed, +10% — allowed", 20000, 22000, false, false},
		{"armed, +60% but forced — allowed", 20000, 32000, true, false},
		{"armed, wiped but forced — allowed", 20000, 0, true, false},
	}
	for _, tc := range cases {
		if got := lakeDeltaBlocked(tc.prior, tc.newCount, tc.force); got != tc.want {
			t.Errorf("%s: lakeDeltaBlocked(%d, %d, %v) = %v, want %v",
				tc.name, tc.prior, tc.newCount, tc.force, got, tc.want)
		}
	}
}

// --- empty-result rail ---

// BuildLakeSegment must never overwrite a segment's membership with an empty
// build unless the operator forces it (the Go mirror of the Python job's
// --allow-empty rail): status 'blocked_delta' is recorded and members are
// untouched. The rail applies regardless of prior size, unlike the delta
// guard, which only arms above lakeDeltaGuardMinPrior.
func TestLakeEmptyBlocked(t *testing.T) {
	cases := []struct {
		name     string
		newCount int64
		force    bool
		want     bool
	}{
		{"zero members, unforced — blocked", 0, false, true},
		{"zero members, forced — allowed", 0, true, false},
		{"one member — allowed", 1, false, false},
		{"large build — allowed", 250000, false, false},
	}
	for _, tc := range cases {
		if got := lakeEmptyBlocked(tc.newCount, tc.force); got != tc.want {
			t.Errorf("%s: lakeEmptyBlocked(%d, %v) = %v, want %v",
				tc.name, tc.newCount, tc.force, got, tc.want)
		}
	}
}

// --- single build slot ---

func TestLakeBuildSlotSecondAcquireRejected(t *testing.T) {
	if !tryAcquireLakeBuildSlot() {
		t.Fatal("first acquire should succeed")
	}
	defer releaseLakeBuildSlot()

	if tryAcquireLakeBuildSlot() {
		releaseLakeBuildSlot()
		t.Fatal("second acquire should fail while the slot is held (handler answers 409)")
	}
}

func TestLakeBuildSlotReleaseMakesSlotAvailable(t *testing.T) {
	if !tryAcquireLakeBuildSlot() {
		t.Fatal("first acquire should succeed")
	}
	releaseLakeBuildSlot()
	if !tryAcquireLakeBuildSlot() {
		t.Fatal("acquire after release should succeed")
	}
	releaseLakeBuildSlot()
}

func TestLakeBuildSlotDoubleReleaseDoesNotBlockOrPanic(t *testing.T) {
	if !tryAcquireLakeBuildSlot() {
		t.Fatal("acquire should succeed")
	}
	releaseLakeBuildSlot()
	releaseLakeBuildSlot() // defensive no-op
	if !tryAcquireLakeBuildSlot() {
		t.Fatal("slot should be acquirable after double release")
	}
	releaseLakeBuildSlot()
}

// --- lake-spec detection in mailing_segments.conditions ---

func TestParseLakeSpecObjectForm(t *testing.T) {
	raw := `{"lake_spec":{"event":"click","window_days":14,"scope":"brand","brand_apex":"discountblog.com","exclude_seeds":true}}`
	spec := parseLakeSpec(raw)
	if spec == nil {
		t.Fatal("expected a lake spec")
	}
	if spec.Event != "click" || spec.WindowDays != 14 || spec.Scope != "brand" ||
		spec.BrandApex != "discountblog.com" || !spec.ExcludeSeeds || spec.ISP != "" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	if len(spec.SeedListIDs) != 0 {
		t.Errorf("seed list ids must not come from persisted conditions, got %v", spec.SeedListIDs)
	}
}

func TestParseLakeSpecNonLakeConditions(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy condition array":   `[{"field":"email_opened","operator":"in_last_days","value":"30"}]`,
		"empty array":              `[]`,
		"object without lake_spec": `{"logic_operator":"AND","conditions":[]}`,
		"empty object":             `{}`,
		"malformed JSON":           `{not json`,
		"empty string":             ``,
	} {
		if spec := parseLakeSpec(raw); spec != nil {
			t.Errorf("%s: expected nil, got %+v", name, spec)
		}
	}
}

// --- name + category generation ---

func TestLakeSegmentNameDefaults(t *testing.T) {
	cases := []struct {
		spec analytics.SegmentSpec
		want string
	}{
		{analytics.SegmentSpec{Event: "open", WindowDays: 14, Scope: "global"}, "Lake-Open-14d-Global"},
		{analytics.SegmentSpec{Event: "click", WindowDays: 7, Scope: "brand", BrandApex: "discountblog.com"}, "Lake-Click-7d-discountblog.com"},
		{analytics.SegmentSpec{Event: "open", WindowDays: 30, Scope: "isp", ISP: "gmail"}, "Lake-Open-30d-gmail"},
	}
	for _, tc := range cases {
		if got := lakeSegmentName("", tc.spec); got != tc.want {
			t.Errorf("lakeSegmentName(\"\", %+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

func TestLakeSegmentNameOperatorBase(t *testing.T) {
	spec := analytics.SegmentSpec{Event: "open", WindowDays: 7, Scope: "global"}
	if got := lakeSegmentName("June-Promo-Openers", spec); got != "June-Promo-Openers-7d" {
		t.Errorf("got %q, want %q", got, "June-Promo-Openers-7d")
	}
}

func TestLakeCategoryForScopeStaysInCanonicalEnum(t *testing.T) {
	for scope, category := range lakeCategoryForScope {
		if !validSegmentCategories[category] {
			t.Errorf("scope %q maps to category %q which is not in validSegmentCategories", scope, category)
		}
	}
	for _, scope := range []string{analytics.SegmentScopeGlobal, analytics.SegmentScopeBrand, analytics.SegmentScopeISP} {
		if _, ok := lakeCategoryForScope[scope]; !ok {
			t.Errorf("scope %q has no category mapping", scope)
		}
	}
}

package worker

import (
	"strings"
	"testing"
)

// dictionary mirrors the production seed (slug → everflow offer id).
func testSlugDict() map[string]string {
	return map[string]string{
		"KW3Q1DJ":  "9539",   // Get Metal Roofing
		"K62P438":  "9135",   // Affordable Windows
		"CL38PFR":  "5990",   // CarShield
		"J876SLX":  "8614",   // AmeriSave HELOC
		"J345SSD":  "8511",   // Optima Tax
		"93W8N2N":  "4575",   // Quicken Loans
		"GK847MZ":  "7667",   // NDR legacy cratoolpro slug (mapped to journey 7667)
		"7N8NS1K":  "3776",   // Renewal by Andersen
		"PS8241":   "420",    // Sam's Club Membership — PS8241 is affiliate URL slug; Everflow offer 420
		"2HH43PB":  "7667",   // NDR eos57ytf slug (migrated off cratoolpro 2026-06-11)
		"J78S2MD":  "9539",   // Metal Roofing new cratoolpro slug (2026-06-11)
		"K86F3PC":  "9178",   // SBLI Quick Quote
		"XF1SR2CS": "417791", // Empire Today Flooring — xnonu network
		"XLRZDZ8K": "421060", // Home Repairly Roofing — muqes network
	}
}

func TestResolveOfferFromLink(t *testing.T) {
	dict := testSlugDict()
	cases := []struct {
		name   string
		link   string
		wantID string
		wantOK bool
	}{
		{
			name:   "metal roofing with full tracking suffix",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/?creative_id=643104&source_id=email&sub1=abc&sub2=quizfiesta.com",
			wantID: "9539", wantOK: true,
		},
		{
			name:   "affordable windows (operator example)",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/K62P438/",
			wantID: "9135", wantOK: true,
		},
		{
			name:   "ndr shared slug maps to journey 7667",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/GK847MZ/?source_id=email",
			wantID: "7667", wantOK: true,
		},
		{
			name:   "lowercase host + slug still resolves (case-insensitive)",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/cl38pfr/?source_id=email",
			wantID: "5990", wantOK: true,
		},
		{
			name:   "slug not in dictionary is skipped",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email", // Capital Wallet, no journey
			wantOK: false,
		},
		{
			name:   "non-cratoolpro link is skipped",
			link:   "https://discountblog.com/some-article?utm=1",
			wantOK: false,
		},
		{
			name:   "asset / font noise (no money slug) is skipped",
			link:   "https://img.projectjarvis.io/fonts/roboto.woff2",
			wantOK: false,
		},
		{
			name:   "empty link is skipped",
			link:   "",
			wantOK: false,
		},
		{
			name:   "wrong publisher path prefix is skipped",
			link:   "https://www.cratoolpro.com/OTHERPUB/KW3Q1DJ/", // not BJB4Q5BF
			wantOK: false,
		},
		{
			name:   "sams club eos57ytf affiliate with full tracking suffix",
			link:   "https://www.eos57ytf.com/K4C5ZLC/PS8241/?creative_id=4989&source_id=email&sub1=abc&sub2=quizfiesta.com",
			wantID: "420", wantOK: true,
		},
		{
			name:   "sams club eos57ytf affiliate trailing slash only",
			link:   "https://www.eos57ytf.com/K4C5ZLC/PS8241/",
			wantID: "420", wantOK: true,
		},
		{
			name:   "lowercase ps8241 slug still resolves",
			link:   "https://www.eos57ytf.com/K4C5ZLC/ps8241/?source_id=email",
			wantID: "420", wantOK: true,
		},
		{
			name:   "unmapped affiliate ps slug is skipped",
			link:   "https://www.eos57ytf.com/K4C5ZLC/PS9999/?source_id=email",
			wantOK: false,
		},
		{
			name:   "ndr eos57ytf alphanumeric slug resolves (2026-06-11 migration)",
			link:   "https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?source_id=email&sub1=abc&sub2=discountblog.com",
			wantID: "7667", wantOK: true,
		},
		{
			name:   "metal roofing new cratoolpro slug resolves",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/J78S2MD/?source_id=email",
			wantID: "9539", wantOK: true,
		},
		{
			name:   "sbli cratoolpro slug resolves",
			link:   "https://www.cratoolpro.com/BJB4Q5BF/K86F3PC/?source_id=email&sub1=abc",
			wantID: "9178", wantOK: true,
		},
		{
			name:   "empire today xnonu slug resolves",
			link:   "https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?source_id=email&sub1=abc&sub2=quizfiesta.com",
			wantID: "417791", wantOK: true,
		},
		{
			name:   "lowercase xnonu slug still resolves",
			link:   "https://www.xnonu.com/TQ5MX18J/xf1sr2cs/",
			wantID: "417791", wantOK: true,
		},
		{
			name:   "unmapped eos57ytf alphanumeric slug is skipped",
			link:   "https://www.eos57ytf.com/K4C5ZLC/ZZZ999/?source_id=email",
			wantOK: false,
		},
		{
			name:   "wrong xnonu publisher path prefix is skipped",
			link:   "https://www.xnonu.com/OTHERPUB/XF1SR2CS/",
			wantOK: false,
		},
		{
			name:   "home repairly muqes slug resolves",
			link:   "https://www.muqes.com/TQ5MX18J/XLRZDZ8K/?source_id=email&sub1=abc&sub2=discountblog.com",
			wantID: "421060", wantOK: true,
		},
		{
			name:   "lowercase muqes slug still resolves",
			link:   "https://www.muqes.com/TQ5MX18J/xlrzdz8k/",
			wantID: "421060", wantOK: true,
		},
		{
			name:   "wrong muqes publisher path prefix is skipped",
			link:   "https://www.muqes.com/OTHERPUB/XLRZDZ8K/",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := resolveOfferFromLink(tc.link, dict)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (link=%q)", gotOK, tc.wantOK, tc.link)
			}
			if gotOK && gotID != tc.wantID {
				t.Fatalf("offerID = %q, want %q (link=%q)", gotID, tc.wantID, tc.link)
			}
		})
	}
}

func TestNormalizeSlug(t *testing.T) {
	cases := map[string]string{
		"kw3q1dj":    "KW3Q1DJ",
		"  K62P438 ": "K62P438",
		"GK847MZ":    "GK847MZ",
	}
	for in, want := range cases {
		if got := normalizeSlug(in); got != want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveOfferFromLink_EmptyDict ensures we never panic / false-positive
// when the dictionary hasn't loaded yet.
func TestResolveOfferFromLink_EmptyDict(t *testing.T) {
	if _, ok := resolveOfferFromLink("https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/", map[string]string{}); ok {
		t.Fatal("expected ok=false against empty dictionary")
	}
}

// TestExtractMoneySlug verifies the slug extractor returns the normalized key
// used for both the offer dictionary and the consumer-signal redirect map.
func TestExtractMoneySlug(t *testing.T) {
	cases := []struct {
		name     string
		link     string
		wantSlug string
		wantOK   bool
	}{
		{"warby cratoolpro", "https://www.cratoolpro.com/BJB4Q5BF/K5C8PQQ/?source_id=email&sub1=x&sub2=quizfiesta.com", "K5C8PQQ", true},
		{"trugreen cratoolpro lowercase", "https://www.cratoolpro.com/BJB4Q5BF/bxpft55/?creative_id=643433", "BXPFT55", true},
		{"sams club affiliate PS slug", "https://www.eos57ytf.com/K4C5ZLC/PS8241/?source_id=email", "PS8241", true},
		{"ndr eos57ytf alphanumeric slug", "https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?source_id=email", "2HH43PB", true},
		{"empire xnonu slug", "https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?source_id=email", "XF1SR2CS", true},
		{"home repairly muqes slug", "https://www.muqes.com/TQ5MX18J/XLRZDZ8K/?source_id=email", "XLRZDZ8K", true},
		{"non-money link", "https://discountblog.com/article", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractMoneySlug(tc.link)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (link=%q)", ok, tc.wantOK, tc.link)
			}
			if ok && got != tc.wantSlug {
				t.Fatalf("slug = %q, want %q (link=%q)", got, tc.wantSlug, tc.link)
			}
		})
	}
}

// TestConsumerSignalRedirect mirrors the in-loop resolution: a Warby/TruGreen
// slug redirects to the Sam's Club target offer; any other slug falls through
// to the normal offer dictionary. This guards the core wiring of the
// 2026-06-06 "consumer signal → Sam's Club drip" directive.
func TestConsumerSignalRedirect(t *testing.T) {
	signals := map[string]consumerSignal{
		"K5C8PQQ": {targetOffer: "420", namePattern: "%sam%"}, // Warby Parker
		"BXPFT55": {targetOffer: "420", namePattern: "%sam%"}, // TruGreen
	}
	dict := testSlugDict()

	// resolve replicates the loop's offer-resolution precedence (signal first,
	// then the normal slug dictionary).
	resolve := func(link string) (offer string, redirect bool, ok bool) {
		slug, hasSlug := extractMoneySlug(link)
		if sig, isSignal := signals[slug]; hasSlug && isSignal {
			return sig.targetOffer, true, true
		}
		if o, found := resolveOfferFromLink(link, dict); found {
			return o, false, true
		}
		return "", false, false
	}

	cases := []struct {
		name         string
		link         string
		wantOffer    string
		wantRedirect bool
		wantOK       bool
	}{
		{"warby click redirects to sams 420", "https://www.cratoolpro.com/BJB4Q5BF/K5C8PQQ/?sub2=quizfiesta.com", "420", true, true},
		{"trugreen click redirects to sams 420", "https://www.cratoolpro.com/BJB4Q5BF/BXPFT55/?creative_id=643433", "420", true, true},
		{"metal roofing click is NOT redirected", "https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/", "9539", false, true},
		{"direct sams affiliate click maps normally (no redirect)", "https://www.eos57ytf.com/K4C5ZLC/PS8241/", "420", false, true},
		{"unknown slug is skipped", "https://www.cratoolpro.com/BJB4Q5BF/ZZZZZZZ/", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offer, redirect, ok := resolve(tc.link)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (offer != tc.wantOffer || redirect != tc.wantRedirect) {
				t.Fatalf("got (offer=%q, redirect=%v), want (offer=%q, redirect=%v)", offer, redirect, tc.wantOffer, tc.wantRedirect)
			}
		})
	}
}

// TestClickTrackingScanQuery_VerdictGate asserts the pre-enrollment gate is
// SIGNAL-GRADED (operator 2026-07-18): a money-link click is GOLD, so by
// default the scan excludes ONLY the seed-farm ('farm' verdict), never the
// whole machine/unknown bucket — and it must NOT re-introduce the old
// human-verdict gate that dropped ~20% of real converters. When
// CLICKDRIP_PREVERDICT_DISABLED is truthy even the farm predicate is omitted.
// The .scratch/journey_machine_click_gate.py cron applies the same farm-only
// predicate retroactively as the backstop.
func TestClickTrackingScanQuery_VerdictGate(t *testing.T) {
	const farmPred = "COALESCE(t.click_verdict, ignite_event_verdict(t.user_agent, t.ip_address)) <> 'farm'"

	q := clickTrackingScanQuery(false)
	if !strings.Contains(q, farmPred) {
		t.Fatalf("default scan query must carry the farm-only exclusion; got:\n%s", q)
	}
	// The old human-verdict gate must NOT come back — it over-excluded real humans.
	if strings.Contains(q, "ignite_verdict_is_human") {
		t.Fatalf("scan query must not gate on ignite_verdict_is_human (signal grading); got:\n%s", q)
	}

	t.Setenv("CLICKDRIP_PREVERDICT_DISABLED", "true")
	q = clickTrackingScanQuery(false)
	if strings.Contains(q, "<> 'farm'") || strings.Contains(q, "ignite_verdict_is_human") {
		t.Fatalf("CLICKDRIP_PREVERDICT_DISABLED=true must omit the verdict predicate entirely; got:\n%s", q)
	}

	// The money-URL ILIKE list and cursor bounds survive both variants.
	for _, frag := range []string{
		"cratoolpro.com/BJB4Q5BF/",
		"eos57ytf.com/K4C5ZLC/",
		"xnonu.com/TQ5MX18J/",
		"muqes.com/TQ5MX18J/",
		"k8k0hfdt.com/3QJ6DW/",
		"t.event_at > $1 AND t.event_at <= $2",
		"LIMIT $3",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("scan query lost fragment %q; got:\n%s", frag, q)
		}
	}
}

// TestPatternForTargetOffer covers the lane-governor redirect pattern
// resolution: the pattern comes from the consumer-signal rows targeting the
// redirect offer; no matching signal (or empty target) means no pattern and
// the caller skips.
func TestPatternForTargetOffer(t *testing.T) {
	signals := map[string]consumerSignal{
		"K5C8PQQ": {targetOffer: "420", namePattern: "%sam%"},
		"BXPFT55": {targetOffer: "420", namePattern: "%sam%"},
	}
	if got := patternForTargetOffer(signals, "420"); got != "%sam%" {
		t.Fatalf("pattern for 420 = %q, want %%sam%%", got)
	}
	if got := patternForTargetOffer(signals, "9999"); got != "" {
		t.Fatalf("pattern for unmapped target = %q, want empty", got)
	}
	if got := patternForTargetOffer(signals, ""); got != "" {
		t.Fatalf("pattern for empty target = %q, want empty", got)
	}
}

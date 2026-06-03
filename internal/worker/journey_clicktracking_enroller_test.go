package worker

import "testing"

// dictionary mirrors the production seed (slug → everflow offer id).
func testSlugDict() map[string]string {
	return map[string]string{
		"KW3Q1DJ": "9539", // Get Metal Roofing
		"K62P438": "9135", // Affordable Windows
		"CL38PFR": "5990", // CarShield
		"J876SLX": "8614", // AmeriSave HELOC
		"J345SSD": "8511", // Optima Tax
		"93W8N2N": "4575", // Quicken Loans
		"GK847MZ": "7667", // NDR (mapped to journey 7667)
		"7N8NS1K": "3776", // Renewal by Andersen
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

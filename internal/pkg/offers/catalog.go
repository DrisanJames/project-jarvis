// Package offers is the canonical catalog of cratoolpro offer slugs that
// the Analytics Center uses for attribution.
//
// Why slug-based:
//   `mailing_campaigns.offer_id` and `mailing_tracking_events.offer_id`
//   are 100% NULL across the last 14 days of production data because no
//   deploy script populates them. The propagation chain from queue →
//   send-worker → tracking-event is broken (per the INTENT segmentation
//   enterprise plan, Part II Findings 1-3).
//
//   Real intent IS in the data — encoded in `mailing_tracking_events.link_url`
//   via the cratoolpro slug pattern:
//
//     https://www.cratoolpro.com/BJB4Q5BF/{SLUG}/?source_id=email&sub1={subscriber.uuid}&sub2={brand_root}
//
//   Until INTENT Phase 3 (the Go-side fix that populates offer_id all the
//   way through the pipeline) ships, the Analytics Center derives offers
//   by matching `link_url ILIKE '%/{slug}/%'`. This package owns the
//   verified slug list.
//
// Adding a new offer:
//   1. Operator confirms the slug from the cratoolpro tracking link.
//   2. Add an entry to SlugCatalog with Name + Vertical.
//   3. Deploy.
//
//   No SQL or schema change required — this map is queried at request
//   time and surfaced in /api/mailing/analytics/offer-performance.
package offers

import "strings"

// Offer is the canonical metadata for a cratoolpro slug.
type Offer struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Vertical string `json:"vertical"`
}

// SlugCatalog is the verified slug → offer mapping.
//
// Verified entries (per INTENT plan Part II Finding 4):
//
//   BXPFT55  → TruGreen           (Lawn)
//   93W8N2N  → Quicken HELOC      (Finance)
//   J876SLX  → AmeriSave HELOC    (Finance)
//   K5C8PQQ  → Warby Parker       (Newsletter)
//   K3TL7NJ  → Jacuzzi            (Home Improvement)
//
// Unknown / pending verification:
//   KFSPRLK, KG53427  — observed in link_url but not yet attributed.
//   These are surfaced under name="UNKNOWN" so HandleSlugCoverage
//   surfaces them in the audit panel and the operator can add them.
var SlugCatalog = map[string]Offer{
	"BXPFT55": {Slug: "BXPFT55", Name: "TruGreen", Vertical: "Lawn"},
	"93W8N2N": {Slug: "93W8N2N", Name: "Quicken HELOC", Vertical: "Finance"},
	"J876SLX": {Slug: "J876SLX", Name: "AmeriSave HELOC", Vertical: "Finance"},
	"K5C8PQQ": {Slug: "K5C8PQQ", Name: "Warby Parker", Vertical: "Newsletter"},
	"K3TL7NJ": {Slug: "K3TL7NJ", Name: "Jacuzzi", Vertical: "Home Improvement"},
	"KFSPRLK": {Slug: "KFSPRLK", Name: "UNKNOWN", Vertical: "Unattributed"},
	"KG53427": {Slug: "KG53427", Name: "UNKNOWN", Vertical: "Unattributed"},
}

// AllSlugs returns the catalog slugs as a slice (deterministic call site
// helper — order is not stable since maps are unordered, callers that
// need stable order must sort).
func AllSlugs() []string {
	out := make([]string, 0, len(SlugCatalog))
	for s := range SlugCatalog {
		out = append(out, s)
	}
	return out
}

// Lookup returns the catalog entry for a slug, with `ok=false` when the
// slug is not in the catalog (in which case Name/Vertical are empty and
// the operator should consider adding it).
func Lookup(slug string) (Offer, bool) {
	o, ok := SlugCatalog[strings.ToUpper(strings.TrimSpace(slug))]
	return o, ok
}

// LinkURLContainsSlug returns the SQL fragment that matches a link_url
// containing the slug pattern `/SLUG/`. Used by analytics handlers to
// scan mailing_tracking_events without having to add an offer_id column
// (the INTENT Phase 3 long-term fix).
//
// Caller wraps the result with the appropriate column reference, e.g.:
//
//   WHERE t.link_url ILIKE LinkURLPattern("BXPFT55")
//
// The pattern uses the canonical cratoolpro path segment `/SLUG/` so
// it never matches a slug that appears elsewhere in the URL (query
// strings, hash fragments, etc.).
func LinkURLPattern(slug string) string {
	return "%/" + strings.ToUpper(strings.TrimSpace(slug)) + "/%"
}

// TrackingSuffixSubstrings returns the substrings every cratoolpro link
// MUST contain to be Everflow-attributable. Used by HandleSlugCoverage
// to flag campaigns whose creative HTML lacks the suffix.
func TrackingSuffixSubstrings() []string {
	return []string{"source_id=email", "sub1=", "sub2="}
}

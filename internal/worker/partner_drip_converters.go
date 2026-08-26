package worker

// CONVERTER CROSS-SELL LANES (operator 2026-08-25/26): proven converters get
// their own low-cadence, home-domain-pinned cross-sell sequence.
//
// Three mechanics, all scoped to verticals prefixed "converters_":
//
//  1. WEEKLY CADENCE — every other lane waits followupTouchGapHours (24h)
//     between touches; converter lanes wait 168h. "These people gave money;
//     one complaint from them costs more than a hundred cold sends earn."
//     Override: PARTNER_DRIP_TOUCH_GAP_BY_PREFIX="converters_=168,..."
//     (prefix=hours pairs; unknown lanes fall back to the 24h constant).
//
//  2. HOME-DOMAIN PINNING — a converter is mailed ONLY by the sending domain
//     that converted them (extra_metadata->>'home_brand', stamped at import).
//     The claim passes a pin: rows whose home_brand is set may be claimed only
//     by that brand's wave; rows without one are claimable by any brand.
//
//  3. NEVER EXIT ON CLICK — same continuation rule as the internal feeds:
//     a converter who clicks the cross-sell keeps receiving the sequence
//     (they are the best cohort we have). Implemented by including the
//     "converters_" prefix in the no-exit scope (engagedExitSQL).
//
// Kill switch: PARTNER_DRIP_CONVERTERS_PIN_DISABLED=1 removes the pin
// (rotation applies); the cadence override is env-driven and reversible.

import (
	"os"
	"strconv"
	"strings"
)

const convertersPrefix = "converters_"

// touchGapHoursFor returns the inter-touch gap for a vertical. Default is the
// estate-wide followupTouchGapHours; prefixes listed in
// PARTNER_DRIP_TOUCH_GAP_BY_PREFIX override, and the converters prefix carries
// a built-in 168h default so an unset env never silently runs converters at
// the daily cadence.
func touchGapHoursFor(vertical string) int {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	raw := os.Getenv("PARTNER_DRIP_TOUCH_GAP_BY_PREFIX")
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			continue
		}
		p := strings.ToLower(strings.TrimSpace(kv[0]))
		if p == "" || !strings.HasPrefix(lv, p) {
			continue
		}
		if h, err := strconv.Atoi(strings.TrimSpace(kv[1])); err == nil && h > 0 {
			return h
		}
	}
	if strings.HasPrefix(lv, convertersPrefix) {
		return 168 // weekly
	}
	return followupTouchGapHours
}

func convertersPinDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED")
	return v == "1" || v == "true"
}

// homeBrandPinSQL returns the claim predicate binding pinned records to their
// converting domain, or "" for non-converter verticals (byte-identical legacy
// SQL). `alias` qualifies the column; brand is inlined as a quoted literal
// (orchestrator brand codes, never user input).
func homeBrandPinSQL(vertical, brand, alias string) string {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	if convertersPinDisabled() || !strings.HasPrefix(lv, convertersPrefix) {
		return ""
	}
	q := ""
	if alias != "" {
		q = alias + "."
	}
	b := quoteSQLLiteral(strings.ToLower(strings.TrimSpace(brand)))
	return "\n\t\t\t  AND (COALESCE(" + q + "extra_metadata->>'home_brand','') = '' OR lower(" + q + "extra_metadata->>'home_brand') = " + b + ")"
}

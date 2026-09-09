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

// brandsPerTickFor returns how many brands' waves a vertical fires per tick.
//
// WHY THIS EXISTS. A cell's token bucket mints Effective/ActiveIntervals every
// interval, and a cell can only SPEND when its brand's turn comes round. With the
// estate default of 4 brands per tick and a 15-brand roster a brand turns roughly
// hourly, so at any moment ~4 intervals of minted allowance sits unspent waiting for
// a turn — and whatever is still unspent at the day boundary is destroyed by the
// day-roll, not carried. Measured on yahoo_family 2026-09-09 with the burst clamp
// already removed: spend against mint was a UNIFORM 83% across yahoo, aol, att,
// comcast and sbcglobal (cox 60%, supply-bound). Uniform across ISPs is the
// signature of turn cadence, not of supply or of any per-ISP ceiling.
//
// Raising the turn rate shrinks the unspent balance a lane is carrying when the day
// rolls. Firing the whole roster every tick means each turn spends roughly the one
// interval that just minted, so almost nothing is left to expire.
//
// It CANNOT raise the daily total: the contract still bounds spend through domain
// Headroom (Effective - Reserved - Committed, with committed monotone for the day),
// and per-wave ISP caps still bound each wave. It only changes how often a lane is
// allowed to collect what it is already owed.
//
// Estate default stays 4. Override per lane prefix:
//
//	PARTNER_DRIP_BRANDS_PER_TICK_BY_PREFIX="yahoo_family=15"
func brandsPerTickFor(vertical string, dflt int) int {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	raw := os.Getenv("PARTNER_DRIP_BRANDS_PER_TICK_BY_PREFIX")
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			continue
		}
		pfx := strings.ToLower(strings.TrimSpace(kv[0]))
		if pfx == "" || !strings.HasPrefix(lv, pfx) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(kv[1])); err == nil && n > 0 {
			return n
		}
	}
	return dflt
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
	// The family lane pins too: all five newsletter touches come from ONE
	// sending domain so the per-domain ladder accounting is exact.
	if convertersPinDisabled() || (!strings.HasPrefix(lv, convertersPrefix) && !IsFamilyLane(lv)) {
		return ""
	}
	q := ""
	if alias != "" {
		q = alias + "."
	}
	b := quoteSQLLiteral(strings.ToLower(strings.TrimSpace(brand)))
	return "\n\t\t\t  AND (COALESCE(" + q + "extra_metadata->>'home_brand','') = '' OR lower(" + q + "extra_metadata->>'home_brand') = " + b + ")"
}

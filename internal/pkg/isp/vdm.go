package isp

// SES VDM (Virtual Deliverability Manager) raw-ISP-name mapping + the static
// Property Ledger ISP authority (Vector A plan rev4, Step 11; invariant I-6 in
// internal/worker/property_ledger_doc.go).
//
// AWS VDM reports metrics under ITS OWN ISP names ("Hotmail", "Icloud", …).
// vdmToGroup maps each raw VDM name to the platform's canonical ISP group
// constant. Two raw names may map to one canonical group; consumers MUST sum
// raw results per canonical group (alias-collision rule, Step 16) — never
// treat a raw name as a storage key.

import (
	"sort"
	"strings"
)

// vdmToGroup maps AWS VDM raw ISP names → canonical platform ISP groups.
// Keys are the exact names AWS uses (config.go DefaultISPs). "WP" (Web.de/
// Wp.pl family) is intentionally absent: it has no canonical group here and
// would fall through GroupFromVDMISP as unmapped.
var vdmToGroup = map[string]string{
	"Gmail":   Gmail,
	"Yahoo":   Yahoo,
	"Aol":     Aol,
	"Hotmail": Microsoft,
	"Icloud":  Apple,
	"Att":     ATT,
	"Cox":     Cox,
}

// GroupFromVDMISP maps a raw AWS VDM ISP name to its canonical platform
// group. Unknown names return (lower(name), false) so callers can still key
// telemetry rows deterministically while knowing the name is unmapped —
// unmapped names must NEVER be auto-promoted into control dimensions (I-6).
func GroupFromVDMISP(name string) (string, bool) {
	if g, ok := vdmToGroup[name]; ok {
		return g, true
	}
	return strings.ToLower(strings.TrimSpace(name)), false
}

// VDMISPs returns the raw AWS VDM ISP names this platform queries, sorted for
// deterministic iteration.
func VDMISPs() []string {
	out := make([]string, 0, len(vdmToGroup))
	for k := range vdmToGroup {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LedgerGroups is THE static ledger/seed/API authority (I-6):
// AllGroups() ∪ {Other} — 14 reviewed values. KnownGroups() is NOT sufficient:
// partner_slicer writes isp_family from GroupFromDomain, whose range includes
// verizon/protonmail/zoho (isp.go AllGroups) — and 'other' is the COALESCE key
// for blank isp_family in the drip spend queries. Observed production strings
// never auto-promote into this list; the seed preflight ABORTS on strangers.
func LedgerGroups() []string {
	return append(AllGroups(), Other)
}

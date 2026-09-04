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
	"fmt"
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

// ---------------------------------------------------------------------------
// VDM COMPARISON BUCKETS (the reconciliation rollup)
// ---------------------------------------------------------------------------
//
// The map above answers "AWS said 'Hotmail' — what group is that?". This
// section answers the INVERSE question, which is what reconciliation needs:
// "we classified this recipient 'sbcglobal' — which VDM bucket would AWS have
// counted it in?".
//
// Two facts make this necessary, both MEASURED over source='ses',
// dt 2026-09-01..03 (ses_vdm_daily vs ignite_analytics.email_events):
//
//  1. VDM has NO sbcglobal bucket. AWS counts the 8 legacy AT&T domains
//     (sbcglobal.net, bellsouth.net, …) inside "Att", because it buckets by
//     the receiving MX, not the domain string. Comparing our att alone against
//     VDM's Att reads -60.4% (71,637 vs 180,743); folding sbcglobal in reads
//     -0.4% (180,105 vs 180,743). The fold is the fix.
//
//  2. VDM reports SEVEN ISPs and nothing else. Our charter, comcast, verizon,
//     protonmail, zoho and other lanes have NO VDM counterpart — 442,949 sends
//     in that window. They are not "missing from VDM"; they are out of VDM's
//     scope. A reconciliation that leaves them in the denominator invents a
//     gap that does not exist.
//
// DELIBERATELY NOT a reclassification. isp_group remains the operating
// classification that routes mail, sizes lanes and gates audience; this is a
// SEPARATE reporting dimension layered on top of it. Rolling charter/comcast
// into a VDM bucket, or reclassifying Google-hosted custom domains into gmail,
// would subject those recipients to every gmail-scoped gate (the 8-brand gmail
// ban, zero daily caps, the nightly wave-cancel sweep) and silently remove
// audience that is mailed today. Operator directive 2026-09-04: over-count
// rather than under-count; never narrow audience as a side effect of a
// measurement change.
var groupToVDM = map[string]string{
	Gmail:     "Gmail",
	Yahoo:     "Yahoo",
	Aol:       "Aol",
	Microsoft: "Hotmail",
	Apple:     "Icloud",
	ATT:       "Att",
	Sbcglobal: "Att", // VDM has no sbcglobal bucket — AWS counts these in Att.
	Cox:       "Cox",
}

// VDMBucket returns the AWS VDM bucket a canonical ISP group is counted in,
// and whether VDM reports that group at all.
//
// ok==false means VDM has no counterpart for the group (charter, comcast,
// verizon, protonmail, zoho, other). Callers comparing platform numbers to
// ses_vdm_daily MUST drop those rows from BOTH sides rather than treat them as
// a shortfall — see the block comment above.
func VDMBucket(group string) (string, bool) {
	b, ok := groupToVDM[group]
	return b, ok
}

// VDMComparable reports whether a canonical ISP group has any VDM counterpart.
func VDMComparable(group string) bool {
	_, ok := groupToVDM[group]
	return ok
}

// VDMComparableGroups returns every canonical ISP group that VDM reports on,
// sorted. This is the correct scope filter for a VDM reconciliation query.
func VDMComparableGroups() []string {
	out := make([]string, 0, len(groupToVDM))
	for g := range groupToVDM {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// SQLCaseVDMBucketFromGroup returns a SQL CASE that rolls a STORED canonical
// isp_group column up into its VDM bucket, emitting NULL for groups VDM does
// not report.
//
// It keys on the already-classified group, never on the email/domain — so a
// reconciliation view can never re-derive (and therefore never disagree with)
// the operating classification written at ingest.
//
// groupExpr must be trusted SQL, not user input; this function does not escape
// it. Generated from groupToVDM so the SQL and Go rollups cannot drift.
func SQLCaseVDMBucketFromGroup(groupExpr string) string {
	byBucket := make(map[string][]string, len(groupToVDM))
	for g, b := range groupToVDM {
		byBucket[b] = append(byBucket[b], g)
	}
	buckets := make([]string, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)

	var sb strings.Builder
	sb.WriteString("CASE\n")
	for _, b := range buckets {
		groups := byBucket[b]
		sort.Strings(groups)
		quoted := make([]string, 0, len(groups))
		for _, g := range groups {
			quoted = append(quoted, "'"+g+"'")
		}
		fmt.Fprintf(&sb, "    WHEN %s IN (%s) THEN '%s'\n",
			groupExpr, strings.Join(quoted, ","), b)
	}
	sb.WriteString("    ELSE NULL\nEND")
	return sb.String()
}

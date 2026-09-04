package api

// Per-(sending domain × ISP) touch spacing — the configurable frequency cap.
//
// Operator 2026-09-04: "keep it to one message per day for gmail only", and
// "make this software to where we can increase the touch points per recipient
// per ISP per sending domain".
//
// MEASURED PROBLEM (lake, 2026-09-02/03): a Gmail recipient received an average
// of 5.0 sends per day from ONE sending domain, max 30; ~21,000 people/day took
// six or more. Average distinct domains per person was 1.03, so this is one
// domain hitting one person repeatedly. Gmail answers with
// `550-5.7.1 ... unsolicited mail`: quizfiesta 82,538 rejections over two days
// at 10.2% delivery against historythinking's 100% — and historythinking sends
// the FEWEST per person.
//
// WHY NOT THE EXISTING KILL SWITCH: audience_finalizer_sds.go already carries a
// 20h per-(subscriber, sending_domain) window, but it is bundled with a
// cold/suppressed state exclusion and a priority ORDER BY behind one flag
// (DISABLE_SDS_FREQUENCY_CAP, currently "true" in prod). Flipping that flag
// would change three behaviours at once, for every ISP and every domain. This
// file adds the frequency window ALONE, driven by data, additive, and
// independent of that flag — so seeding a single Gmail row caps Gmail and
// touches nothing else.
//
// WHY A GAP AND NOT A DAILY COUNT: the underlying signal is
// mailing_subscriber_domain_state.last_mailed_at, a timestamp, not a counter.
// A minimum gap is the honest knob over that data and it also spreads touches
// instead of letting three arrive in one hour. Raising touch points is one
// UPDATE: 20h ≈ 1/day, 11h ≈ 2/day, 7h ≈ 3/day, delete the row for no cap.
//
// WHY THE PREDICATE IS ON THE EMAIL AND NOT AN ISP ARGUMENT: planPMTAAudience
// selects candidates per inclusion list and buckets them by ISP AFTERWARDS
// (RecipientsByISP), so the ISP of a given row is not known at selection time.
// The ISP is therefore derived in SQL from the row's own email domain, using
// the canonical isp package mapping so this can never drift from Group().

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// ispTouchPolicy is the resolved policy for ONE sending domain: ISP group name
// -> minimum hours between sends to the same recipient. Absent ISP = no cap.
type ispTouchPolicy map[string]int

// loadISPTouchPolicy resolves the policy for a sending domain. Rows with
// sending_domain '*' are the estate default; an exact-domain row overrides the
// default for that ISP. A missing table or any error yields an EMPTY policy —
// fail OPEN, because this governs how much mail goes out and a policy-store
// outage must not silently stop the estate.
func loadISPTouchPolicy(ctx context.Context, db dbQuerier, sendingDomain string) ispTouchPolicy {
	out := ispTouchPolicy{}
	domain := strings.ToLower(strings.TrimSpace(sendingDomain))
	if db == nil {
		return out
	}
	rows, err := db.QueryContext(ctx, `
		SELECT sending_domain, lower(isp), min_gap_hours
		FROM mailing_isp_touch_policy
		WHERE min_gap_hours > 0 AND (sending_domain = '*' OR lower(sending_domain) = $1)
		ORDER BY CASE WHEN sending_domain = '*' THEN 0 ELSE 1 END
	`, domain)
	if err != nil {
		log.Printf("[ISPTouchPolicy] load for %q: %v — no cap applied this run", domain, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sd, ispName string
		var gap int
		if err := rows.Scan(&sd, &ispName, &gap); err != nil {
			log.Printf("[ISPTouchPolicy] scan: %v", err)
			return ispTouchPolicy{}
		}
		// '*' rows come first, so an exact-domain row overwrites the default.
		out[ispName] = gap
	}
	if err := rows.Err(); err != nil {
		log.Printf("[ISPTouchPolicy] rows: %v — no cap applied this run", err)
		return ispTouchPolicy{}
	}
	return out
}

// touchGapWhere renders the additive WHERE fragment for a policy.
//
// For each capped ISP it emits: "this row is not that ISP, OR it has not been
// mailed by this domain inside the gap". A recipient of any other ISP is
// unaffected, which is what makes a Gmail-only seed Gmail-only in effect.
//
// subscriberAlias is the mailing_subscribers alias; sdsAlias is the LEFT JOIN
// alias carrying last_mailed_at. Returns "" when nothing is capped.
func (p ispTouchPolicy) touchGapWhere(subscriberAlias, sdsAlias string) string {
	if len(p) == 0 {
		return ""
	}
	var b strings.Builder
	for _, group := range sortedPolicyGroups(p) {
		domains := isp.DomainsForGroup(group)
		if len(domains) == 0 {
			// An ISP the canonical mapping does not know cannot be matched in
			// SQL; skipping is correct and must be visible.
			log.Printf("[ISPTouchPolicy] ISP %q has no domains in the canonical mapping — cap ignored", group)
			continue
		}
		quoted := make([]string, 0, len(domains))
		for _, d := range domains {
			quoted = append(quoted, "'"+strings.ReplaceAll(strings.ToLower(d), "'", "''")+"'")
		}
		b.WriteString(fmt.Sprintf(
			" AND (lower(split_part(%s.email, '@', 2)) NOT IN (%s)"+
				" OR %s.last_mailed_at IS NULL"+
				" OR %s.last_mailed_at < NOW() - INTERVAL '%d hours')",
			subscriberAlias, strings.Join(quoted, ","), sdsAlias, sdsAlias, p[group]))
	}
	return b.String()
}

// sortedPolicyGroups keeps the rendered SQL deterministic so it is diffable in
// logs and stable across runs.
func sortedPolicyGroups(p ispTouchPolicy) []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

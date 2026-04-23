package isp

import (
	"fmt"
	"sort"
	"strings"
)

// SQLCaseFromEmail returns a PostgreSQL CASE expression that classifies the
// domain portion of an email column into an ISP group name.
//
// The expression is generated directly from the canonical domainToISP map,
// so the SQL classifier and the Go GroupFromDomain classifier can never
// drift. Callers MUST use this function instead of hand-writing SQL CASE
// statements for ISP classification.
//
// Parameters:
//
//	emailExpr — a SQL expression that evaluates to an email string column or
//	            literal (e.g. "sub.email" or "t.email"). The caller is
//	            responsible for ensuring emailExpr is trusted SQL and not
//	            user input; this function does not escape it.
//
// Returned expression yields one of the ISP group constants (Gmail, Yahoo,
// Aol, Microsoft, Apple, Comcast, Charter, ATT, Sbcglobal, Cox, Verizon,
// Protonmail, Zoho) or Other for unrecognized domains. The wildcard rule
// for Charter/Spectrum regional subdomains (*.rr.com) is also emitted.
//
// Shape:
//
//	CASE
//	    WHEN lower(split_part({emailExpr}, '@', 2)) IN ('gmail.com','googlemail.com') THEN 'gmail'
//	    WHEN lower(split_part({emailExpr}, '@', 2)) IN ('yahoo.com', ...)           THEN 'yahoo'
//	    ...
//	    WHEN lower(split_part({emailExpr}, '@', 2)) LIKE '%.rr.com'                 THEN 'charter'
//	    ELSE 'other'
//	END
func SQLCaseFromEmail(emailExpr string) string {
	domainExpr := fmt.Sprintf("lower(split_part(%s, '@', 2))", emailExpr)
	return sqlCaseFromDomain(domainExpr)
}

// SQLCaseFromDomain returns the same classification CASE, but keyed on a
// domain expression (no '@' splitting). Used when the caller already has a
// bare domain (e.g. mailing_tracking_events.recipient_domain).
func SQLCaseFromDomain(domainExpr string) string {
	return sqlCaseFromDomain(fmt.Sprintf("lower(%s)", domainExpr))
}

func sqlCaseFromDomain(domainExpr string) string {
	// Group domains by ISP so the generated CASE has one WHEN per group.
	// Stable ordering: iterate known groups (KnownGroups + minor groups) in
	// declared order, then emit remaining groups alphabetically. Within each
	// group, domains are sorted alphabetically so the output is deterministic
	// for diffing and snapshot tests.
	byGroup := make(map[string][]string, 16)
	for domain, group := range domainToISP {
		byGroup[group] = append(byGroup[group], domain)
	}
	for _, v := range byGroup {
		sort.Strings(v)
	}

	var b strings.Builder
	b.WriteString("CASE\n")
	for _, group := range allGroupsForSQL() {
		domains, ok := byGroup[group]
		if !ok || len(domains) == 0 {
			continue
		}
		quoted := make([]string, 0, len(domains))
		for _, d := range domains {
			quoted = append(quoted, "'"+d+"'")
		}
		fmt.Fprintf(&b, "    WHEN %s IN (%s) THEN '%s'\n",
			domainExpr, strings.Join(quoted, ","), group)
	}
	// Wildcard rule for Charter regional subdomains (*.rr.com). Kept in
	// lockstep with the Go classifier's HasSuffix(".rr.com") rule.
	fmt.Fprintf(&b, "    WHEN %s LIKE '%%.rr.com' THEN '%s'\n", domainExpr, Charter)
	fmt.Fprintf(&b, "    ELSE '%s'\n", Other)
	b.WriteString("END")
	return b.String()
}

// allGroupsForSQL returns every ISP group known to domainToISP, in the order
// we want them to appear in the generated CASE expression. Major ISPs (with
// dedicated pools) come first so Postgres short-circuits the common path;
// minor ISPs (general pool) come after.
func allGroupsForSQL() []string {
	// Start with KnownGroups (the 10 ISPs with dedicated pools) in declared
	// order, then append minor groups in AllGroups order.
	known := KnownGroups()
	seen := make(map[string]bool, len(known))
	for _, g := range known {
		seen[g] = true
	}
	out := append([]string{}, known...)
	for _, g := range AllGroups() {
		if seen[g] {
			continue
		}
		out = append(out, g)
		seen[g] = true
	}
	return out
}

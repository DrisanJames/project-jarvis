package isp

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestSQLCaseFromEmail_ContainsAllDomains verifies that every domain in the
// canonical domainToISP map is present in the generated SQL CASE expression,
// associated with the correct group. This is the parity invariant — if the
// Go classifier recognizes "gmail.com" as "gmail", the SQL classifier must
// also recognize it.
func TestSQLCaseFromEmail_ContainsAllDomains(t *testing.T) {
	sql := SQLCaseFromEmail("sub.email")
	for domain, expectedGroup := range domainToISP {
		domainQuoted := "'" + domain + "'"
		if !strings.Contains(sql, domainQuoted) {
			t.Errorf("SQL CASE missing domain %q (should map to %q)", domain, expectedGroup)
			continue
		}
		// Ensure the domain appears in the WHEN clause that terminates in
		// the correct group. Regex searches for the domain token inside an
		// IN (...) list that resolves to `THEN '<expectedGroup>'`.
		pattern := fmt.Sprintf(`IN \([^)]*%s[^)]*\) THEN '%s'`,
			regexp.QuoteMeta(domainQuoted), regexp.QuoteMeta(expectedGroup))
		matched, err := regexp.MatchString(pattern, sql)
		if err != nil {
			t.Fatalf("regex compile: %v", err)
		}
		if !matched {
			t.Errorf("domain %q not mapped to %q in generated SQL\nSQL was:\n%s",
				domain, expectedGroup, sql)
		}
	}
}

// TestSQLCaseFromEmail_EmitsWildcardForRRCom verifies that the Charter
// wildcard rule (*.rr.com) is preserved in the SQL output — the Go
// classifier has a HasSuffix check for this, and SQL must mirror it.
func TestSQLCaseFromEmail_EmitsWildcardForRRCom(t *testing.T) {
	sql := SQLCaseFromEmail("sub.email")
	if !strings.Contains(sql, "LIKE '%.rr.com'") {
		t.Errorf("SQL CASE missing wildcard rule for *.rr.com\nSQL was:\n%s", sql)
	}
	// And it must map to Charter.
	pattern := regexp.MustCompile(`LIKE '%\.rr\.com' THEN '` + regexp.QuoteMeta(Charter) + `'`)
	if !pattern.MatchString(sql) {
		t.Errorf("*.rr.com wildcard does not resolve to %q\nSQL was:\n%s", Charter, sql)
	}
}

// TestSQLCaseFromEmail_OtherFallback verifies the ELSE branch returns Other.
func TestSQLCaseFromEmail_OtherFallback(t *testing.T) {
	sql := SQLCaseFromEmail("sub.email")
	expectedElse := fmt.Sprintf("ELSE '%s'", Other)
	if !strings.Contains(sql, expectedElse) {
		t.Errorf("SQL CASE missing ELSE branch %q\nSQL was:\n%s", expectedElse, sql)
	}
}

// TestSQLCaseFromEmail_EmailExprEmbeddedLiterally verifies the caller's
// emailExpr is substituted into the output without escaping. This is the
// documented contract — callers are responsible for trusting emailExpr.
func TestSQLCaseFromEmail_EmailExprEmbeddedLiterally(t *testing.T) {
	sql := SQLCaseFromEmail("tbl.custom_email")
	if !strings.Contains(sql, "split_part(tbl.custom_email, '@', 2)") {
		t.Errorf("caller's emailExpr not embedded as-is\nSQL was:\n%s", sql)
	}
}

// TestSQLCaseFromDomain_EmitsLowerWrap verifies that when the caller
// provides a bare domain column, the generated CASE still lower-cases it
// so matching is case-insensitive (mirrors the Go classifier's ToLower).
func TestSQLCaseFromDomain_EmitsLowerWrap(t *testing.T) {
	sql := SQLCaseFromDomain("t.recipient_domain")
	if !strings.Contains(sql, "lower(t.recipient_domain)") {
		t.Errorf("SQLCaseFromDomain missing lower() wrap\nSQL was:\n%s", sql)
	}
}

// TestAllGroupsForSQL_IncludesEveryGroupOnce ensures the iteration order
// contains every group present in domainToISP exactly once, so no group
// is silently dropped from the generated CASE.
func TestAllGroupsForSQL_IncludesEveryGroupOnce(t *testing.T) {
	groups := allGroupsForSQL()
	seen := make(map[string]bool)
	for _, g := range groups {
		if seen[g] {
			t.Errorf("group %q appears twice in allGroupsForSQL", g)
		}
		seen[g] = true
	}
	// Every group present in domainToISP must be emitted.
	for _, group := range domainToISP {
		if !seen[group] {
			t.Errorf("group %q present in domainToISP but missing from allGroupsForSQL", group)
		}
	}
}

// TestSQLCaseFromEmail_Deterministic verifies the output is stable across
// calls — important because the CASE appears in migration SQL and must be
// reproducible across deploys and environments.
func TestSQLCaseFromEmail_Deterministic(t *testing.T) {
	a := SQLCaseFromEmail("sub.email")
	b := SQLCaseFromEmail("sub.email")
	if a != b {
		t.Errorf("SQLCaseFromEmail is not deterministic")
	}
}

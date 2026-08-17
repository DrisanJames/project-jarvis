package main

// Drip Observatory P1 schema unit gates (Vector B plan rev4 §5.0/§5.9).
// DDL↔code parity for the GENERATED vocabulary CHECK: the isp list in the
// fact tables' CHECK must be exactly isp.AllGroups() + '' — never a frozen
// copy that can drift from the platform ISP map.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

var quotedTokenRE = regexp.MustCompile(`'([a-z]*)'`)

// TestObservatoryISPCheckMatchesAllGroups (plan §5.0) asserts the generated
// vocabulary SQL carries exactly isp.AllGroups() + ” — no member missing,
// no extra member — for BOTH fact tables' constraints.
func TestObservatoryISPCheckMatchesAllGroups(t *testing.T) {
	want := map[string]bool{"": true}
	for _, g := range isp.AllGroups() {
		want[g] = true
	}

	for _, tc := range []struct{ table, constraint string }{
		{"partner_drip_send_cohort_daily", "dob_cohort_isp_vocab"},
		{"partner_drip_event_daily", "dob_event_isp_vocab"},
	} {
		sql := observatoryISPVocabSQL(tc.table, tc.constraint)
		if !strings.Contains(sql, "ALTER TABLE "+tc.table+" ADD CONSTRAINT "+tc.constraint) {
			t.Fatalf("%s: generated SQL does not target table+constraint:\n%s", tc.constraint, sql)
		}
		if !strings.Contains(sql, "pg_constraint WHERE conname='"+tc.constraint+"'") {
			t.Errorf("%s: generated SQL is not idempotent (missing pg_constraint probe)", tc.constraint)
		}
		inList := sql[strings.Index(sql, "isp IN ("):]
		got := map[string]bool{}
		for _, m := range quotedTokenRE.FindAllStringSubmatch(inList, -1) {
			got[m[1]] = true
		}
		for g := range want {
			if !got[g] {
				t.Errorf("%s: generated CHECK missing %q", tc.constraint, g)
			}
		}
		for g := range got {
			if !want[g] {
				t.Errorf("%s: generated CHECK carries %q which is not in isp.AllGroups()+''", tc.constraint, g)
			}
		}
	}
}

// TestObservatoryBrandCodesSeedIsCanonical pins that the migration entry's
// seed SQL is generated from the brandident literal (27 rows) — the same
// one-source-of-truth guarantee as ownedDomainsSeedSQL.
func TestObservatoryBrandCodesSeedIsCanonical(t *testing.T) {
	sql := brandident.SeedSQL()
	pairs := brandident.Canonical()
	if len(pairs) != 27 {
		t.Fatalf("brandident literal must carry 27 brands, got %d", len(pairs))
	}
	for _, p := range pairs {
		if !strings.Contains(sql, "('"+p.Code+"', '"+p.Apex+"', 'seed')") {
			t.Errorf("seed SQL missing (%s, %s)", p.Code, p.Apex)
		}
	}
}

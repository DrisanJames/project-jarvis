package main

// Drip Observatory schema unit gates (Vector B plan rev 4.1 §5.0/§5.9).
// DDL↔code parity for the GENERATED vocabulary CHECKs: the isp list in every
// vocab-producing migration constant must be exactly
// append(isp.AllGroups(), isp.Other) — neither wider nor narrower — plus the
// empty-string sentinel on lane-scope rows (rev-4.1 STOP-2 ruling:
// GroupFromDomain returns "other" for the long tail while AllGroups()
// excludes it).

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

var (
	ispInListRE   = regexp.MustCompile(`isp IN\s*\(([^)]*)\)`)
	quotedTokenRE = regexp.MustCompile(`'([a-z]*)'`)
)

// assertVocabSQL parses every `isp IN (...)` group in sql and asserts its
// token set equals append(isp.AllGroups(), isp.Other) exactly, and that the
// lane-scope empty-string branch is present.
func assertVocabSQL(t *testing.T, label, sql string) {
	t.Helper()
	want := map[string]bool{}
	for _, g := range append(isp.AllGroups(), isp.Other) {
		want[g] = true
	}
	groups := ispInListRE.FindAllStringSubmatch(sql, -1)
	if len(groups) == 0 {
		t.Fatalf("%s: no `isp IN (...)` vocabulary group found:\n%s", label, sql)
	}
	for _, grp := range groups {
		got := map[string]bool{}
		for _, m := range quotedTokenRE.FindAllStringSubmatch(grp[1], -1) {
			got[m[1]] = true
		}
		for g := range want {
			if !got[g] {
				t.Errorf("%s: vocabulary missing %q", label, g)
			}
		}
		for g := range got {
			if !want[g] {
				t.Errorf("%s: vocabulary carries %q not in append(isp.AllGroups(), isp.Other)", label, g)
			}
		}
	}
	if !strings.Contains(sql, `isp = ''`) {
		t.Errorf("%s: lane-scope empty-string branch (isp = '') missing", label)
	}
}

// TestObservatoryISPCheckMatchesAllGroups (plan §5.0, rev 4.1) asserts every
// vocab-CHECK migration constant — the fresh-DB generator for BOTH fact
// tables AND the §5.0b widening entry — carries exactly
// append(isp.AllGroups(), isp.Other), so a fresh DB and a widened
// already-shipped DB converge on the identical constraint.
func TestObservatoryISPCheckMatchesAllGroups(t *testing.T) {
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
		assertVocabSQL(t, "generator "+tc.constraint, sql)
	}

	// The §5.0b widening constant (pinned literal) must agree with the code.
	assertVocabSQL(t, "dob_widen_isp_vocab_other", dobWidenISPVocabOtherSQL)
	for _, conname := range []string{"dob_cohort_isp_vocab", "dob_event_isp_vocab"} {
		if !strings.Contains(dobWidenISPVocabOtherSQL, conname) {
			t.Errorf("widening constant does not handle %s", conname)
		}
	}
	// Drop-if-narrow guard: the widen must detect the pre-ruling constraint.
	if !strings.Contains(dobWidenISPVocabOtherSQL, `NOT LIKE '%''other''%'`) {
		t.Error("widening constant missing the drop-if-narrow guard")
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

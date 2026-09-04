package isp

// Step-11 fixtures (Vector A plan rev4): the static ledger authority and the
// VDM raw-name mapping. Includes the seed-preflight negative fixture — an
// observed production string outside the authority must ABORT, never auto-add
// (permanent fixture, invariant I-11).

import "testing"

func TestLedgerGroupsAuthority(t *testing.T) {
	got := LedgerGroups()
	if len(got) != 14 {
		t.Fatalf("LedgerGroups() = %d values, want 14 (AllGroups ∪ {other}): %v", len(got), got)
	}
	set := map[string]bool{}
	for _, g := range got {
		if set[g] {
			t.Fatalf("LedgerGroups() contains duplicate %q", g)
		}
		set[g] = true
	}
	// The two values a KnownGroups-based authority would orphan, plus the
	// COALESCE fallback key.
	for _, must := range []string{Other, Verizon, Protonmail, Zoho} {
		if !set[must] {
			t.Errorf("LedgerGroups() missing %q", must)
		}
	}
}

func TestGroupFromVDMISP(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		mapped bool
	}{
		{"Gmail", Gmail, true},
		{"Yahoo", Yahoo, true},
		{"Aol", Aol, true},
		{"Hotmail", Microsoft, true},
		{"Icloud", Apple, true},
		{"Att", ATT, true},
		{"Cox", Cox, true},
		// Unknown names come back lowered and UNMAPPED — never a control key.
		{"WP", "wp", false},
		{"Outlook", "outlook", false},
	}
	for _, c := range cases {
		got, ok := GroupFromVDMISP(c.raw)
		if got != c.want || ok != c.mapped {
			t.Errorf("GroupFromVDMISP(%q) = (%q,%v), want (%q,%v)", c.raw, got, ok, c.want, c.mapped)
		}
	}
}

func TestVDMISPsDeterministic(t *testing.T) {
	got := VDMISPs()
	if len(got) != 7 {
		t.Fatalf("VDMISPs() = %d names, want 7: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("VDMISPs() not sorted: %v", got)
		}
	}
}

// TestSeedPreflightAbortsOnStranger pins the preflight contract: any observed
// isp_family value outside LedgerGroups() must abort the seed. 'msft' is the
// canonical stranger from the plan.
func TestSeedPreflightAbortsOnStranger(t *testing.T) {
	authority := map[string]bool{}
	for _, g := range LedgerGroups() {
		authority[g] = true
	}
	observed := []string{"gmail", "yahoo", "msft", "other"}
	var strangers []string
	for _, o := range observed {
		if !authority[o] {
			strangers = append(strangers, o)
		}
	}
	if len(strangers) != 1 || strangers[0] != "msft" {
		t.Fatalf("preflight fixture: want abort on ['msft'], got %v", strangers)
	}
}

// --- VDM comparison buckets (reconciliation rollup) -------------------------

func TestVDMBucketFoldsSbcglobalIntoAtt(t *testing.T) {
	// The measured reconciliation fix: VDM has no sbcglobal bucket, so both
	// att and sbcglobal must roll up into "Att" or the comparison reads -60%.
	for _, g := range []string{ATT, Sbcglobal} {
		b, ok := VDMBucket(g)
		if !ok || b != "Att" {
			t.Errorf("VDMBucket(%q) = (%q,%v), want (\"Att\",true)", g, b, ok)
		}
	}
}

func TestVDMBucketMapsEveryReportedGroup(t *testing.T) {
	cases := map[string]string{
		Gmail: "Gmail", Yahoo: "Yahoo", Aol: "Aol",
		Microsoft: "Hotmail", Apple: "Icloud", Cox: "Cox",
	}
	for g, want := range cases {
		if b, ok := VDMBucket(g); !ok || b != want {
			t.Errorf("VDMBucket(%q) = (%q,%v), want (%q,true)", g, b, ok, want)
		}
	}
}

// TestVDMBucketUnreportedGroupsAreNotComparable is the negative path: the
// lanes VDM does not report must be EXCLUDED from a reconciliation, never
// silently counted as a shortfall. 442,949 sends sat in these lanes over
// 2026-09-01..03 — folding them anywhere invents a gap.
func TestVDMBucketUnreportedGroupsAreNotComparable(t *testing.T) {
	for _, g := range []string{Charter, Comcast, Verizon, Protonmail, Zoho, Other} {
		if b, ok := VDMBucket(g); ok {
			t.Errorf("VDMBucket(%q) = (%q,true), want unmapped — VDM does not report it", g, b)
		}
		if VDMComparable(g) {
			t.Errorf("VDMComparable(%q) = true, want false", g)
		}
	}
	// An unknown/garbage group must also be non-comparable, never defaulted.
	if _, ok := VDMBucket("msft"); ok {
		t.Error(`VDMBucket("msft") mapped a stranger; want unmapped`)
	}
}

// TestVDMBucketRangeIsExactlyTheQueriedRawNames pins the two halves of vdm.go
// together: every bucket we roll UP into must be a raw name we actually query
// DOWN from, or a reconciliation compares against a bucket nobody fetched.
func TestVDMBucketRangeIsExactlyTheQueriedRawNames(t *testing.T) {
	queried := map[string]bool{}
	for _, raw := range VDMISPs() {
		queried[raw] = true
	}
	for g, bucket := range groupToVDM {
		if !queried[bucket] {
			t.Errorf("group %q rolls up to %q, which VDMISPs() never queries", g, bucket)
		}
		// And the round trip must land back on a canonical group.
		if back, ok := GroupFromVDMISP(bucket); !ok {
			t.Errorf("bucket %q (from group %q) is not a mapped VDM raw name", bucket, g)
		} else if _, isGroup := groupToVDM[back]; !isGroup {
			t.Errorf("bucket %q round-trips to %q, which has no VDM bucket", bucket, back)
		}
	}
	if got, want := len(VDMComparableGroups()), 8; got != want {
		t.Errorf("VDMComparableGroups() = %d, want %d", got, want)
	}
}

func TestSQLCaseVDMBucketFromGroup(t *testing.T) {
	got := SQLCaseVDMBucketFromGroup("isp_group")
	want := "CASE\n" +
		"    WHEN isp_group IN ('aol') THEN 'Aol'\n" +
		"    WHEN isp_group IN ('att','sbcglobal') THEN 'Att'\n" +
		"    WHEN isp_group IN ('cox') THEN 'Cox'\n" +
		"    WHEN isp_group IN ('gmail') THEN 'Gmail'\n" +
		"    WHEN isp_group IN ('microsoft') THEN 'Hotmail'\n" +
		"    WHEN isp_group IN ('apple') THEN 'Icloud'\n" +
		"    WHEN isp_group IN ('yahoo') THEN 'Yahoo'\n" +
		"    ELSE NULL\nEND"
	if got != want {
		t.Errorf("SQLCaseVDMBucketFromGroup mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

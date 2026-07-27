package worker

import (
	"strings"
	"testing"
)

// dedupeByMD5 exists solely to make the re-push ON CONFLICT DO UPDATE in
// bulkInsertSurvivors (partner_slicer.go:418) legal: Postgres aborts a statement
// whose VALUES list would update the same conflicting row twice ("ON CONFLICT DO
// UPDATE command cannot affect row a second time"). Partner NDJSON batches DO
// carry the same email more than once, so without this collapse the entire
// insert — every survivor in the batch, not just the duplicate — errors out and
// the batch is lost. These tests pin the observable contract of that collapse.

// md5s is a readability helper: the surviving MD5s, in order.
func md5s(recs []partnerRawRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.MD5)
	}
	return out
}

func eqStrs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDedupeByMD5(t *testing.T) {
	cases := []struct {
		name string
		in   []partnerRawRecord
		want []string
	}{
		{
			// The case that breaks the INSERT: same md5 twice in one batch.
			name: "duplicates_collapse",
			in: []partnerRawRecord{
				{Email: "a@x.com", MD5: "aaa"},
				{Email: "b@x.com", MD5: "bbb"},
				{Email: "a2@x.com", MD5: "aaa"},
			},
			want: []string{"aaa", "bbb"},
		},
		{
			// Order is load-bearing: the caller builds positional placeholders
			// ($1..$9 per record) against this exact slice, so a reshuffle would
			// bind the wrong email to the wrong md5/isp.
			name: "order_preserved",
			in: []partnerRawRecord{
				{MD5: "ccc"}, {MD5: "aaa"}, {MD5: "bbb"},
				{MD5: "aaa"}, {MD5: "ccc"},
			},
			want: []string{"ccc", "aaa", "bbb"},
		},
		{
			name: "no_duplicates_unchanged",
			in: []partnerRawRecord{
				{MD5: "aaa"}, {MD5: "bbb"}, {MD5: "ccc"},
			},
			want: []string{"aaa", "bbb", "ccc"},
		},
		{
			name: "empty_slice",
			in:   []partnerRawRecord{},
			want: []string{},
		},
		{
			name: "nil_slice",
			in:   nil,
			want: []string{},
		},
		{
			name: "all_identical_collapses_to_one",
			in: []partnerRawRecord{
				{MD5: "aaa"}, {MD5: "aaa"}, {MD5: "aaa"}, {MD5: "aaa"},
			},
			want: []string{"aaa"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeByMD5(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), md5s(got))
			}
			if !eqStrs(md5s(got), tc.want) {
				t.Fatalf("md5s = %v, want %v", md5s(got), tc.want)
			}
		})
	}
}

// TestDedupeByMD5KeepsFirstOccurrence: which record survives is not arbitrary —
// the batch is ordered as the partner pushed it, and the first occurrence is the
// one the rest of the pipeline (suppression check, extra_metadata payload) was
// built from. Distinguished by Email because MD5 alone cannot show which of the
// duplicates won.
func TestDedupeByMD5KeepsFirstOccurrence(t *testing.T) {
	got := dedupeByMD5([]partnerRawRecord{
		{Email: "first@x.com", MD5: "dup", ISPFamily: "gmail"},
		{Email: "second@x.com", MD5: "dup", ISPFamily: "yahoo"},
	})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Email != "first@x.com" {
		t.Errorf("Email = %q, want %q (first occurrence must win)", got[0].Email, "first@x.com")
	}
	if got[0].ISPFamily != "gmail" {
		t.Errorf("ISPFamily = %q, want %q — the whole first record survives, not a merge",
			got[0].ISPFamily, "gmail")
	}
}

// TestDedupeByMD5DoesNotMutateCaller: the helper must not write through the
// caller's backing array. bulkInsertSuppressed (partner_slicer.go:451) diffs the
// ORIGINAL batch against the survivors to decide what to record as
// suppressed_global; an in-place compaction here would silently corrupt that
// diff and mislabel live records as globally suppressed.
func TestDedupeByMD5DoesNotMutateCaller(t *testing.T) {
	in := []partnerRawRecord{
		{Email: "a@x.com", MD5: "aaa"},
		{Email: "b@x.com", MD5: "aaa"},
		{Email: "c@x.com", MD5: "ccc"},
	}
	_ = dedupeByMD5(in)
	if len(in) != 3 {
		t.Fatalf("caller slice len = %d, want 3", len(in))
	}
	want := []string{"a@x.com", "b@x.com", "c@x.com"}
	for i, w := range want {
		if in[i].Email != w {
			t.Fatalf("in[%d].Email = %q, want %q — caller slice was mutated", i, in[i].Email, w)
		}
	}
}

// --- QA SEV-3: pin the revive semantics of the re-push upsert (2026-07-26) ---
// The CASE decides which records a partner re-push may return to circulation.
// These assertions fail loudly if anyone widens or narrows the revive gate.

func TestPcqRepushUpsertClause_ReviveSemantics(t *testing.T) {
	c := pcqRepushUpsertClause

	// Signal capture always happens on conflict.
	for _, want := range []string{
		"ON CONFLICT (vertical, email_md5) DO UPDATE",
		"last_pushed_at = now()",
		"push_count     = partner_clean_queue.push_count + 1",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("upsert clause missing %q", want)
		}
	}

	// Revive gate: ONLY mailed + EO-clean may return to 'ready'.
	if !strings.Contains(c, "WHEN partner_clean_queue.status = 'mailed'") {
		t.Fatal("revive gate must require status='mailed' — nothing else is revivable")
	}
	if !strings.Contains(c, "eo_result IN ('Verified','Complainer')") {
		t.Fatal("revive gate must require EO Verified/Complainer")
	}
	if !strings.Contains(c, "ELSE partner_clean_queue.status") {
		t.Fatal("CASE must fall through to the EXISTING status for every non-revivable row")
	}

	// Negative pins: no other status may appear as a revive condition. A future
	// edit adding e.g. dead_letter or suppressed_eo to the WHEN leg must trip this.
	caseBody := c[strings.Index(c, "CASE"):]
	whenLeg := caseBody[:strings.Index(caseBody, "ELSE")]
	for _, banned := range []string{"dead_letter", "suppressed", "pending_eo", "eo_in_flight", "claimed", "held"} {
		if strings.Contains(whenLeg, banned) {
			t.Fatalf("revive WHEN leg must never mention %q", banned)
		}
	}
	// And the only assignment target of the WHEN leg is 'ready'.
	if !strings.Contains(whenLeg, "THEN 'ready'") {
		t.Fatal("revive outcome must be exactly 'ready'")
	}
}

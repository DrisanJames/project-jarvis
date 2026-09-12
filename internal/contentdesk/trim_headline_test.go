package contentdesk

import (
	"reflect"
	"testing"
)

const (
	ledeA = "That column of percentages on a label can look like a mystery."
	ledeB = "It is a simple calculation that the FDA explains on its own site."
)

// Live :1144: myownhealth's only excerpt sentence stayed overstated through 5
// package revisions. Trim replaces it with the lede's first sentence that
// carries no hard finding, and that sentence's references move with it.
func TestTrimFlagged_OverstatedExcerptFallsBackToTheLede(t *testing.T) {
	a := assessment{
		pkg: Package{Title: "T", Excerpt: "The formula applies to every nutrient on the label.",
			Blocks: []Block{{ID: "lede-1", Type: "lede", Text: ledeA + " " + ledeB}}},
		refs: []ClaimRef{ref(UnitExcerpt, 0, cU1, 1), ref("lede-1", 1, cU2, 3), ref(UnitTitle, 0, cU1, 1)},
		judgment: []JudgmentItem{
			{ID: "j5", Kind: "claim", BlockID: UnitExcerpt, SentenceIdx: 0, Verdict: "overstated"},
			{ID: "j6", Kind: "unreferenced_claim", BlockID: "lede-1", SentenceIdx: 0, Verdict: "flag"}, // lede A is cut, not used
		},
	}
	d, pk, n := trimFlagged(a)
	if pk.Excerpt != ledeB || d.Blocks[0].Text != ledeB || n != 2 {
		t.Fatalf("excerpt must fall back to the lede's surviving sentence: excerpt=%q lede=%q n=%d", pk.Excerpt, d.Blocks[0].Text, n)
	}
	want := []ClaimRef{ref(UnitExcerpt, 0, cU2, 3), ref(UnitTitle, 0, cU1, 1)}
	if !reflect.DeepEqual(pk.ClaimRefs, want) {
		t.Fatalf("the fallback carries the lede sentence's refs, the title keeps its own: %+v", pk.ClaimRefs)
	}
}

// A flagged meta description sentence is cut when another survives; the
// survivor's reference is re-indexed.
func TestTrimFlagged_MetaDescriptionSentence(t *testing.T) {
	a := assessment{
		pkg:      Package{MetaDescription: "Learn how the value is calculated. It fixes every diet.", Blocks: []Block{{ID: "lede-1", Type: "lede", Text: ledeB}}},
		refs:     []ClaimRef{ref(UnitMetaDescription, 0, cU1, 1), ref(UnitMetaDescription, 1, cU2, 3)},
		judgment: []JudgmentItem{{ID: "j9", Kind: "claim", BlockID: UnitMetaDescription, SentenceIdx: 1, Verdict: "unsupported"}},
	}
	_, pk, n := trimFlagged(a)
	if pk.MetaDescription != "Learn how the value is calculated." || n != 1 || !reflect.DeepEqual(pk.ClaimRefs, []ClaimRef{ref(UnitMetaDescription, 0, cU1, 1)}) {
		t.Fatalf("meta=%q n=%d refs=%+v", pk.MetaDescription, n, pk.ClaimRefs)
	}
}

// With no usable lede sentence the excerpt is left as is (never emptied).
func TestTrimFlagged_ExcerptWithoutFallbackStays(t *testing.T) {
	a := assessment{
		pkg:      Package{Excerpt: "The formula applies to every nutrient on the label.", Blocks: []Block{{ID: "s1", Type: "section", Text: ledeB}}},
		refs:     []ClaimRef{ref(UnitExcerpt, 0, cU1, 1)},
		judgment: []JudgmentItem{{ID: "j5", Kind: "claim", BlockID: UnitExcerpt, SentenceIdx: 0, Verdict: "overstated"}},
	}
	_, pk, n := trimFlagged(a)
	if pk.Excerpt != a.pkg.Excerpt || n != 0 || !reflect.DeepEqual(pk.ClaimRefs, a.refs) {
		t.Fatalf("excerpt must stay: %q n=%d refs=%+v", pk.Excerpt, n, pk.ClaimRefs)
	}
}

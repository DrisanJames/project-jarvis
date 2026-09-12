package contentdesk

import (
	"reflect"
	"testing"
)

// Live :1140: "Robert R. Livingston" split into two sentences; the citation
// marker at the end landed on the second half and the uncited first half was
// the history pilot's last hard blocker. Initials, dotted abbreviations and
// listed titles never end a sentence; real boundaries still do.
func TestSplitSentences_InitialsAndAbbreviations(t *testing.T) {
	cases := map[string][]string{
		"John Dickinson and Robert R. Livingston never signed. Others did.": {
			"John Dickinson and Robert R. Livingston never signed.", "Others did."},
		"Dr. Smith agreed. St. Louis hosted it.":       {"Dr. Smith agreed.", "St. Louis hosted it."},
		"In the U.S. Congress, e.g. the House, voted.": {"In the U.S. Congress, e.g. the House, voted."},
		"It rose 3.5% in 2024. Then it fell.":          {"It rose 3.5% in 2024.", "Then it fell."},
		"Signed on August 2. A few signed later!":      {"Signed on August 2.", "A few signed later!"},
		// A one-letter token reads as an initial, so "Plan B." stays with the
		// next sentence — the accepted cost of never splitting a name.
		"Is that so? Yes. Plan B. Next.":    {"Is that so?", "Yes.", "Plan B. Next."},
		"He said (J. Adams wrote it). End.": {"He said (J. Adams wrote it).", "End."},
	}
	for in, want := range cases {
		got := SplitSentences(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
		// The marker splitter must agree on every input.
		spans := sentenceSpans(in)
		rs := []rune(in)
		var viaSpans []string
		for _, sp := range spans {
			viaSpans = append(viaSpans, string(rs[sp.start:sp.end]))
		}
		if !reflect.DeepEqual(viaSpans, got) {
			t.Errorf("%q: spans %q disagree with SplitSentences %q", in, viaSpans, got)
		}
	}
}

// The marker after "R. Livingston … signed" attaches to the whole sentence.
func TestExtractBlockRefs_InitialKeepsTheMarkerOnItsSentence(t *testing.T) {
	b := Block{ID: "k", Type: "lede", Text: "Robert R. Livingston never signed [[c:" + cU1 + "@1]]. Next."}
	refs := extractBlockRefs(&b, map[string]int{cU1: 1})
	if len(refs) != 1 || refs[0].SentenceIdx != 0 || len(SplitSentences(b.Text)) != 2 {
		t.Fatalf("refs %+v, sentences %q", refs, SplitSentences(b.Text))
	}
}

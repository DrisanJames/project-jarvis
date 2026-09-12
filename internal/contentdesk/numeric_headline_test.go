package contentdesk

import (
	"strings"
	"testing"
)

// Live :1137/:1138: the history pilot (49/49 references supported) was held
// on a heading and a preheader that restate cited body dates. A headline line
// whose every number is in a cited body sentence passes; a headline number
// found in no cited sentence still fails.
func TestNumericCheck_HeadlineRestatingACitedNumberPasses(t *testing.T) {
	pkg := Package{Title: "Signed August 2", Preheaders: []string{"July 2 vote, July 4 text, August 2 signing"},
		Blocks: []Block{
			{ID: "s1", Type: "section", Heading: "August 2: The parchment is signed", Text: "Most delegates signed on August 2, 1776. Congress voted on July 2."},
			{ID: "s2", Type: "section", Heading: "In 1999 a copy sold", Text: "The text was adopted on July 4."},
		}}
	refs := []ClaimRef{
		{BlockID: "s1", SentenceIdx: 1, ClaimID: cU1, Version: 1},
		{BlockID: "s1", SentenceIdx: 2, ClaimID: cU1, Version: 1},
		{BlockID: "s2", SentenceIdx: 1, ClaimID: cU2, Version: 3},
	}
	r := checkNumericSentencesReferenced(CheckInput{Package: pkg, Refs: refs})
	details := strings.Join(r.Details, "\n")
	for _, unit := range []string{"s1#0", "preheader:0#0", "title#0"} {
		if strings.Contains(details, unit) {
			t.Fatalf("%s restates cited numbers and must pass:\n%s", unit, details)
		}
	}
	if r.Passed || !strings.Contains(details, "s2#0") {
		t.Fatalf("a heading number found in no cited sentence must still fail:\n%s", details)
	}
}

// Live :1139: an FAQ question restating a cited date ("Did anyone sign the
// Declaration after August 2, 1776?") passes like a heading; the answer is
// still a body sentence and still needs its own reference.
func TestNumericCheck_FAQQuestionRestatingACitedNumberPasses(t *testing.T) {
	pkg := Package{Blocks: []Block{
		{ID: "l1", Type: "lede", Text: "Most delegates signed on August 2, 1776."},
		{ID: "faq-1", Type: "faq", Items: []string{
			"Did anyone sign after August 2, 1776?", "Yes, a few signed later in 1776.",
			"What happened in 1999?", "Nothing relevant.",
		}},
	}}
	refs := []ClaimRef{{BlockID: "l1", SentenceIdx: 0, ClaimID: cU1, Version: 1}}
	r := checkNumericSentencesReferenced(CheckInput{Package: pkg, Refs: refs})
	details := strings.Join(r.Details, "\n")
	if strings.Contains(details, "faq-1#0") {
		t.Fatalf("a question restating a cited date must pass:\n%s", details)
	}
	if !strings.Contains(details, "faq-1#1") {
		t.Fatalf("an uncited answer with a number must still fail:\n%s", details)
	}
	if !strings.Contains(details, "faq-1#2") {
		t.Fatalf("a question with a number found in no cited sentence must still fail:\n%s", details)
	}
}

// Body sentences are unchanged: an uncited number still fails.
func TestNumericCheck_UncitedBodyNumberStillFails(t *testing.T) {
	pkg := Package{Blocks: []Block{{ID: "l1", Type: "lede", Text: "Rates rose 5% in 2024. Fees apply."}}}
	r := checkNumericSentencesReferenced(CheckInput{Package: pkg})
	if r.Passed || !strings.Contains(strings.Join(r.Details, " "), "l1#0") {
		t.Fatalf("an uncited body number must fail: %+v", r)
	}
}

// A headline cannot borrow a number from an uncited body sentence.
func TestNumericCheck_HeadlineNeedsTheNumberCited(t *testing.T) {
	pkg := Package{Subjects: []string{"37% of borrowers qualify"},
		Blocks: []Block{{ID: "l1", Type: "lede", Text: "About 37% of borrowers qualify."}}}
	r := checkNumericSentencesReferenced(CheckInput{Package: pkg})
	if !strings.Contains(strings.Join(r.Details, " "), "subject:0#0") {
		t.Fatalf("a subject restating an UNCITED number must fail: %+v", r.Details)
	}
}

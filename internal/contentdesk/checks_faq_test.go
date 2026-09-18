package contentdesk

import (
	"strings"
	"testing"
)

func faqPackage(items ...string) Package {
	return Package{Title: "T", Excerpt: "E.", MetaTitle: "M", MetaDescription: "D.",
		Subjects: []string{"S"}, Preheaders: []string{"P"},
		Blocks: []Block{{ID: "l1", Type: "lede", Text: "A holds."}, {ID: "faq-1", Type: "faq", Items: items}}}
}

// Live 2026-09-15..18: four articles were approved with an odd faq list and
// could never be published — the publishers refuse to render question/answer
// pairs that do not pair ("faq items must alternate question, answer (got
// 11)"). An odd faq is an S1 failure, so the block is written again.
func TestCheckSchema_FAQNeedsPairs(t *testing.T) {
	odd := checkSchema(faqPackage("Q1?", "A1.", "Q2?"))
	if odd.Passed {
		t.Fatal("an odd faq list must fail the schema check")
	}
	if !strings.Contains(strings.Join(odd.Details, " "), "got 3 items") || odd.Severity != "S1" {
		t.Fatalf("the failure must name the count and be S1: %+v", odd)
	}
	if even := checkSchema(faqPackage("Q1?", "A1.", "Q2?", "A2.")); !even.Passed {
		t.Fatalf("paired faq items must pass: %+v", even)
	}
	if empty := checkSchema(faqPackage()); empty.Passed {
		t.Fatal("a faq with no items must still fail")
	}
}

// The publishers pair items by position, so this check is what keeps an
// approved article publishable; the version bump re-checks cached articles.
func TestCodeChecksVersion_BumpedForFAQPairs(t *testing.T) {
	if codeChecksVersion != "2026-09-18.faq-pairs" {
		t.Fatalf("codeChecksVersion = %q", codeChecksVersion)
	}
}

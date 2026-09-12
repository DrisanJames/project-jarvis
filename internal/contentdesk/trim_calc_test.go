package contentdesk

import "testing"

// Live :1145: bestcreditcare's worked_example had no calc; schema (S1) held
// it through 5 revise rounds. Trim keeps the prose as a section, with every
// reference still on its sentence.
func TestTrimFlagged_DemotesACalclessWorkedExample(t *testing.T) {
	a := assessment{
		pkg:  Package{Blocks: []Block{{ID: "we", Type: "worked_example", Heading: "Example", Text: "A card is billed $75. The letter goes out the same week."}}},
		refs: []ClaimRef{ref("we", 1, cU1, 1)},
	}
	d, _, n := trimFlagged(a)
	if n != 1 || d.Blocks[0].Type != "section" || d.Blocks[0].Text != a.pkg.Blocks[0].Text {
		t.Fatalf("calc-less worked_example must become a section, text intact: n=%d %+v", n, d.Blocks[0])
	}
	assertRefsLand(t, d, map[ClaimRef]string{ref("we", 1, cU1, 1): "A card is billed $75."})
	if r := checkSchema(Package{Title: "T", Excerpt: "E.", MetaTitle: "M", MetaDescription: "D.", Subjects: []string{"S"}, Preheaders: []string{"P"}, Blocks: d.Blocks}); !r.Passed {
		t.Fatalf("schema must pass after the demotion: %+v", r)
	}
}

func TestTrimFlagged_KeepsAWorkedExampleWithCalcOrWithoutProse(t *testing.T) {
	a := assessment{pkg: Package{Blocks: []Block{
		{ID: "we1", Type: "worked_example", Text: "Two plus two.", Calc: &Calc{Inputs: []CalcInput{{Name: "a", Value: 2}}, Formula: "a+2", Result: 4}},
		{ID: "we2", Type: "worked_example"}, // no prose: a section would be empty
	}}}
	d, _, n := trimFlagged(a)
	if n != 0 || d.Blocks[0].Type != "worked_example" || d.Blocks[1].Type != "worked_example" {
		t.Fatalf("n=%d types=%s,%s", n, d.Blocks[0].Type, d.Blocks[1].Type)
	}
}

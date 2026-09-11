package contentdesk

import "testing"

// Money-decision content (APR, loans, refinance) must take the two-reviewer
// path like health, tax, insurance, benefits and DIY.
func TestConsequentialCategories_IncludeFinance(t *testing.T) {
	for _, c := range []string{CatFinance, CatHealth, CatBenefits, CatInsurance, CatTax, CatDIY} {
		if !ConsequentialCategories[c] {
			t.Errorf("category %q must force consequential=true", c)
		}
	}
	if ConsequentialCategories[CatHistory] {
		t.Error("history is not consequential")
	}
}

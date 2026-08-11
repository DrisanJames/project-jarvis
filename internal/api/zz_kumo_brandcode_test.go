package api

import "testing"

func TestKumoBrandCodesResolveASendingDomain(t *testing.T) {
	want := map[string]string{
		"FTH": "em.firsttimebuyerhomeloan.com", "BCC": "em.bestcreditcare.com",
		"USF": "em.us-finance.com", "HLJ": "em.homeloansbyjaime.com",
		"HTM": "em.hometracmortgage.com", "YFB": "em.yourfinancialblog.com",
		"MPF": "em.mypersonalfinancial.com", "PMD": "em.paymydebit.com",
		"TRB": "em.theretirementblog.com", "AAD": "em.aadwd.com", "HFC": "em.hfcl.net",
		"MP": "em.mypersonalfinancial.com", "DB": "em.discountblog.com",
	}
	for code, exp := range want {
		if got := sendingDomainFromBrandCode(code); got != exp {
			t.Errorf("sendingDomainFromBrandCode(%q) = %q, want %q", code, got, exp)
		}
	}
	// the apex form must ALSO resolve — that is the no-deploy path in use today
	if got := sendingDomainFromBrandCode("firsttimebuyerhomeloan.com"); got != "em.firsttimebuyerhomeloan.com" {
		t.Errorf("apex form = %q", got)
	}
}

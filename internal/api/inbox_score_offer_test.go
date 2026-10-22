package api

import "testing"

func TestInboxScoreKind(t *testing.T) {
	cases := map[string]string{
		"10222026 - BWP - NL-GM-NEWSLETTER":                              "newsletter",
		"10222026 - NX-aa.discountblog.com - NL-ENG-NEWSLETTER-D32":      "newsletter",
		"10222026 - BWP - NL-FRESH-NEWSLETTER-JVCMS1-D31":                "newsletter",
		"10222026 - BWP - Liberty":                                       "offer",
		"10222026 - NX-rc.financialcalculate.com - OFFER-ENG-InsHome":    "offer",
		"10222026 - aad - KUMO-WARM - OFFER-YH-eHAdv-D17":                "offer",
		"10222026 - YIH - AUTO-RELEASE-R1-W2":                            "offer",
		"10222026 - DB - OFFER-YF-EHSAVINGS-RAMP-D43":                    "offer",
		"10222026 - LPL - APPLE-WINBACK-eHealth-Switchers-CLK":           "offer",
		"10222026 - NX-ad.quizfiesta.com - NL-COLD-NEWSLETTER-D32-APPLE": "newsletter",
	}
	for name, want := range cases {
		if got := inboxScoreKind(name); got != want {
			t.Errorf("kind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInboxScoreOffer(t *testing.T) {
	cases := []struct{ name, key, want string }{
		{"10222026 - BWP - Liberty", "liberty-mutual", "Liberty Mutual"},
		{"10222026 - BWP - InsHome-Apple-1401", "insurhomev1", "Insurify Home"},
		{"10222026 - BWP - EmpireToday", "empireflooringcpm70offv1", "Empire Today"},
		{"oct22 - WCL HELOC - ABANDON-RECOVERY - 0913", "iwchelocv4", "WCL HELOC"},
		{"10222026 - X - Anything", "brand-new-key", "brand-new-key"},
		{"10222026 - NX-rc.financialcalculate.com - OFFER-ENG-InsHome", "", "Insurify Home"},
		{"10222026 - NX-ad.quizfiesta.com - OFFER-ENG-InsHome-APPLE", "", "Insurify Home"},
		{"10222026 - NX-ox.hfcl.net - OFFER-ENG-AEPSav-YF", "", "AEP Savings"},
		{"10222026 - NX-py.paymydebit.com - OFFER-ENG-AEPSw-YF", "", "AEP Switchers"},
		{"10232026 - NX-mb.quizfiesta.com - OFFER-ENG-eHSw-YF", "", "eHealth Switchers"},
		{"10222026 - aad - KUMO-WARM - OFFER-YH-eHAdv-D17", "", "eHealth Advisor"},
		{"10222026 - hfc - KUMO-WARM - OFFER-YH-eHSav-D17", "", "eHealth Savings"},
		{"10222026 - DB - OFFER-YF-EHSAVINGS-RAMP-D43", "", "eHealth Savings"},
		{"10222026 - RB - NYL-Apple-0901", "", "New York Life"},
		{"10232026 - WFY - eHealth-Switchers", "", "eHealth Switchers"},
		{"10222026 - CP - eHealth-Savings-Apple-0401", "", "eHealth Savings"},
		{"10222026 - LPL - APPLE-WINBACK-eHealth-Switchers-CLK", "", "eHealth Switchers"},
		{"10222026 - RRU - HealthCare-Apple-0901", "", "HealthCare"},
		{"10222026 - YIH - AUTO-RELEASE-R1-W2", "", "Internal Auto"},
		{"10222026 - RRU - SRC2-RELEASE-R2-W3", "", "SRC2 release"},
		{"10222026 - BWP - SomethingNew-Apple-0401", "", "SomethingNew"},
		{"10232026 - aad - KUMO-WARM - OFFER-HealthCare-D18", "", "HealthCare"},
		{"10232026 - hfc - KUMO-WARM - OFFER-InsHome-D18", "", "Insurify Home"},
	}
	for _, c := range cases {
		if got := inboxScoreOffer(c.name, c.key); got != c.want {
			t.Errorf("offer(%q, %q) = %q, want %q", c.name, c.key, got, c.want)
		}
	}
}

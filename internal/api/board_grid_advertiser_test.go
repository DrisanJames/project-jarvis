package api

import "testing"

func TestAdvertiserToken(t *testing.T) {
	cases := map[string]string{
		"Optima Tax Relief v2": "optima",
		"Optima - Fresh Start": "optima",
		"ADT Home Security":    "adt",
		"":                     "",
		"A1":                   "",
	}
	for in, want := range cases {
		if got := advertiserToken(in); got != want {
			t.Fatalf("advertiserToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two distinct offer rows, same advertiser token, one property -> warn (not
// blocker); same offer_id twice stays the REPEAT_OFFER blocker and does NOT
// also warn (single distinct id).
func TestBoardGrid_AdvertiserRepeatWarn(t *testing.T) {
	s := &BoardGridService{}
	cells := []BoardCell{
		{Property: "FC", Slot: "01:01", OfferID: "id-1", OfferName: "Optima Tax Relief", Name: "08222026 - FC - Optima", BrandRoot: "financialcalculate.com", Subject: "x"},
		{Property: "FC", Slot: "06:01", OfferID: "id-2", OfferName: "Optima Fresh Start", Name: "08222026 - FC - Optima FS", BrandRoot: "financialcalculate.com", Subject: "x"},
	}
	f := s.runGates(nil, "2026-08-22", cells)
	foundWarn := false
	for _, x := range f {
		if x.Code == "ADVERTISER_REPEAT" {
			foundWarn = true
			if x.Level != "warn" {
				t.Fatalf("ADVERTISER_REPEAT must be warn, got %s", x.Level)
			}
		}
		if x.Code == "REPEAT_OFFER" {
			t.Fatalf("distinct offer ids must not trip REPEAT_OFFER")
		}
	}
	if !foundWarn {
		t.Fatalf("expected ADVERTISER_REPEAT warn, findings: %+v", f)
	}
}

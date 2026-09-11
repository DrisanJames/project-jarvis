package tracking

import (
	"context"
	"testing"
)

// Real-shaped /track/click destinations (2026-09-11 prod triage: first shadow
// window reported click.dest.unknown for every click).
func realShapeDict(t *testing.T) *SmartLinkDictionary {
	t.Helper()
	d := &SmartLinkDictionary{}
	d.loadFn = func(context.Context) (map[string]smartLinkEntry, error) {
		return map[string]smartLinkEntry{
			"crt001": {Destination: "https://www.cratoolpro.com/BJB4Q5BF/J78S2MD/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"},
			"c2f001": {Destination: "https://www.codefortwo.com/X7LBB6/2CTPL/"},
		}, nil
	}
	if err := d.reloadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestClassifyDestination_RealShapes(t *testing.T) {
	d := realShapeDict(t)
	cases := map[string]string{
		"https://www.discountblog.com/blog/x":                                                 DestOwned,
		"https://www.financialcalculate.com/reviews/y":                                        DestOwned,
		"https://WWW.DiscountBlog.com:443/blog/x":                                             DestOwned,
		"https://www.discountblog.com/x?a=1&amp;b=2":                                          DestOwned,
		"https://t.em.discountblog.com/o/discountblog.com/" + subUUID + "/abc123/" + campUUID: DestOwned,
		"https://www.cratoolpro.com/BJB4Q5BF/J78S2MD/?source_id=email&sub1=" + subUUID:        DestMoney,
		"https://www.codefortwo.com/X7LBB6/2CTPL/?sub1=" + subUUID:                            DestMoney,
		"https://codefortwo.com/X7LBB6/2CTPL/":                                                DestMoney,
		"https://fonts.googleapis.com/css2?family=Inter:wght@400;700&display=swap":            DestUnknown,
	}
	for in, want := range cases {
		if got := classifyDestination(in, d); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

// Same shapes through the REAL handler path (token -> decode -> remap -> classify).
func TestClick_DestCountersThroughHandler(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "")
	h := NewHandler(&capturePublisher{}, realShapeDict(t))
	h.sigKeys = ParseSigKeys("k1")
	for _, dest := range []string{
		"https://www.discountblog.com/blog/x",
		"https://www.codefortwo.com/X7LBB6/2CTPL/?sub1=" + subUUID,
		"https://fonts.googleapis.com/css2?family=Inter",
	} {
		enc, sig := liveClickToken("k1", dest)
		doClick(h, enc, sig)
	}
	c := sigCounters.snapshot()
	if c["click.dest.owned"] != 1 || c["click.dest.money"] != 1 || c["click.dest.unknown"] != 1 {
		t.Fatalf("dest counters: %v", c)
	}
}

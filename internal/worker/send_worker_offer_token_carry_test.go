package worker

import (
	"strings"
	"testing"
)

// offerTokenFromBareURL is what lets a board remail's tokenid survive the
// emitter's bare->/o/ rewrite. Pins: value carried; absent/empty -> "";
// hostile -> "" (gateway alphabet); &amp;-escaped hrefs still parse.
func TestOfferTokenFromBareURL_CarriesValue(t *testing.T) {
	got := offerTokenFromBareURL("https://autocoveragepoint.com/coverage-match?id=ff2007&s4=7552&channel=Rev&tokenid=0f1c9e2a-11aa-4bbb-8ccc-0123456789ab")
	if got != "0f1c9e2a-11aa-4bbb-8ccc-0123456789ab" {
		t.Fatalf("got %q", got)
	}
}

func TestOfferTokenFromBareURL_NegativeControl_MissingOrEmptyIsEmpty(t *testing.T) {
	for _, u := range []string{
		"https://autocoveragepoint.com/coverage-match?id=ff2007&s4=7552&channel=Rev&tokenid=",
		"https://autocoveragepoint.com/coverage-match?id=ff2007&s4=7552&channel=Rev",
		"",
	} {
		if got := offerTokenFromBareURL(u); got != "" {
			t.Fatalf("%q: want empty, got %q", u, got)
		}
	}
}

func TestOfferTokenFromBareURL_NegativeControl_HostileIsDropped(t *testing.T) {
	for _, tok := range []string{"a b", "<script>", "x/y", "q=1", strings.Repeat("a", 257), "%7B%7Btoken%7D%7D"} {
		u := "https://autocoveragepoint.com/coverage-match?tokenid=" + tok
		if got := offerTokenFromBareURL(u); got != "" {
			t.Fatalf("hostile %q leaked as %q", tok, got)
		}
	}
}

func TestOfferTokenFromBareURL_AmpEscapedHrefStillParses(t *testing.T) {
	got := offerTokenFromBareURL("https://vehiclecoverageline.com/coverage-match?id=dc2de5&amp;s4=atr234&amp;tokenid=seedtest-v8")
	if got != "seedtest-v8" {
		t.Fatalf("got %q", got)
	}
}

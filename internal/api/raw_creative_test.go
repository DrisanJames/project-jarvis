package api

import (
	"strings"
	"testing"
)

// raw_creative bypass (operator 2026-07-25, wcl-heloc): strip must exactly
// invert append, and be a no-op without the marker.
func TestStripUnsubDisclaimerRoundTrip(t *testing.T) {
	orig := `<html><body><h1>WCL HELOC</h1><p>Rates from 6.5%</p></body></html>`
	withFooter := appendUnsubDisclaimer(orig, "wcl-heloc.com", "")
	if !strings.Contains(withFooter, unsubDisclaimerMarker) {
		t.Fatal("append did not inject the marker")
	}
	stripped := stripUnsubDisclaimer(withFooter)
	if strings.Contains(stripped, unsubDisclaimerMarker) {
		t.Fatal("strip left the marker behind")
	}
	if strings.Contains(stripped, "not monitored") {
		t.Fatal("strip left the disclaimer copy behind")
	}
	if stripped != orig {
		t.Fatalf("strip is not the exact inverse of append:\n got %q\nwant %q", stripped, orig)
	}
}

func TestStripUnsubDisclaimerNoopWithoutMarker(t *testing.T) {
	orig := `<html><body><p>as-is creative</p></body></html>`
	if got := stripUnsubDisclaimer(orig); got != orig {
		t.Fatalf("no-marker strip mutated the html: %q", got)
	}
}

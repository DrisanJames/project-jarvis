package api

import (
	"strings"
	"testing"
)

// ─── Regression guards for the two bugs the operator hit on 2026-08-01 ──────

// BUG 1: the CPM classifier was scoped to campaigns CREATED inside the display
// window, so in the 7-day view six live offers (kqckq7, sams-club,
// metal-roofing, tahiti-village, s6wff5, liberty-mutual) reported "No CPM deal"
// while liberty-mutual alone delivered 1.5M in that window — campaigns created
// weeks earlier keep sending. Deal membership is a property of the OFFER.
func TestNonCpmDealMapIsNotWindowedByCreatedAt(t *testing.T) {
	q := nonCpmDealMapSQL()
	if strings.Contains(q, "CURRENT_DATE -") {
		t.Errorf("deal map must not window campaigns by created_at:\n%s", q)
	}
	// The dm CTE legitimately carries `c.created_at >= d.start_date` (deal
	// membership floor). What must NOT appear is a floor tied to the
	// REPORTING window, which is what CURRENT_DATE - N encodes.
	if !strings.Contains(q, "c.created_at >= d.start_date") {
		t.Errorf("deal membership floor (start_date) should still be present:\n%s", q)
	}
}

// BUG 2: conversions read 0 because they were resolved
// offer_key -> mailing_offer_slug_map -> everflow_offer_id -> ledger, and that
// dictionary has no row for iwchelocv1..4 / liberty-mutual / metal-roofing /
// tahiti-village / sams-club / newsletter / fidelity, and disagrees where it
// does exist (kqckq7 -> 1090, but Liberty's conversions are filed under 338).
func TestNonCpmConversionsDoNotUseTheSlugMap(t *testing.T) {
	for name, q := range map[string]string{
		"ledger count": nonCpmConversionCountSQL(),
		"payout":       nonCpmConversionPayoutSQL(),
	} {
		if strings.Contains(q, "mailing_offer_slug_map") {
			t.Errorf("%s must not resolve conversions through the slug map:\n%s", name, q)
		}
	}
	// Revenue must come from the per-conversion supplied payout, never
	// mailing_offers.payout (0.00 on 27 of 34 offers).
	pq := nonCpmConversionPayoutSQL()
	if !strings.Contains(pq, "mailing_everflow_conversions") || !strings.Contains(pq, "ec.payout") {
		t.Errorf("payout must come from mailing_everflow_conversions.payout:\n%s", pq)
	}
}

// BUG 3 (caught pre-ship): resolving the everflow offer id with a LEFT JOIN
// multiplied revenue. everflow_offer_id is NOT unique in mailing_offers — id
// 162 (West Capital) carries FOUR rows, one per creative variant — so the join
// fanned 2 August conversions into 8 and 100 USD of payout into 400 USD. The
// fallback must be a SCALAR subquery, which cannot add rows.
func TestNonCpmPayoutDoesNotFanOutOnEverflowID(t *testing.T) {
	q := nonCpmConversionPayoutSQL()
	if strings.Contains(q, "JOIN mailing_offers eo") {
		t.Errorf("everflow-id fallback must not be a JOIN — it multiplies revenue:\n%s", q)
	}
	if !strings.Contains(q, "SELECT MIN(eo.name) FROM mailing_offers eo") {
		t.Errorf("everflow-id fallback must be a scalar subquery:\n%s", q)
	}
	// Exactly one row per conversion: the only tables joined are the campaign
	// and its offer, both by primary key.
	if n := strings.Count(q, "LEFT JOIN"); n != 2 {
		t.Errorf("expected exactly 2 primary-key LEFT JOINs, got %d:\n%s", n, q)
	}
}

// Every metric must key on the SAME identity cascade, or the volume, click and
// conversion rows silently fail to line up on one offer.
func TestNonCpmIdentityCascadeIsShared(t *testing.T) {
	if !strings.Contains(nonCpmIdentityExpr, "offer_key") ||
		!strings.Contains(nonCpmIdentityExpr, "o.name") ||
		!strings.Contains(nonCpmIdentityExpr, nonCpmUnattributed) {
		t.Fatalf("identity cascade must be offer_key -> offer name -> %s: %s", nonCpmUnattributed, nonCpmIdentityExpr)
	}
	for name, q := range map[string]string{
		"clicks":   nonCpmClicksSQL(),
		"deal map": nonCpmDealMapSQL(),
		"rollup":   nonCpmRollupInsertSQL(),
	} {
		if !strings.Contains(q, nonCpmIdentityExpr) {
			t.Errorf("%s does not use the shared identity cascade:\n%s", name, q)
		}
	}
}

// The click predicate is what separates engagement from vendor telemetry.
func TestNonCpmClickPredicateExcludesNonNavigational(t *testing.T) {
	q := nonCpmClicksSQL()
	for _, want := range []string{
		`css|js|woff2?`,     // asset extensions
		`fonts\.g|cdn\.`,    // asset hosts
		`unsub|optout`,      // compliance
		`^everflow-import:`, // importer marker
		`COUNT(DISTINCT te.subscriber_id)`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("click query missing %q:\n%s", want, q)
		}
	}
	// t.em: only the /track/ family is unproven; /o/ is the smart-link offer
	// redirect (the money click) and must survive.
	if !strings.Contains(q, `^https?://t\.em\.[^/]+/track/`) {
		t.Errorf("must exclude only the t.em /track/ family:\n%s", q)
	}
}

func TestNonCpmDiv(t *testing.T) {
	cases := []struct{ a, b, want float64 }{
		{100, 0, 0}, // zero denominator must not produce Inf
		{0, 100, 0}, //
		{50, 200, 0.25},
	}
	for _, c := range cases {
		if got := nonCpmDiv(c.a, c.b); got != c.want {
			t.Errorf("nonCpmDiv(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// deriveRates must compute eCPM from revenue and delivered, and avg payout from
// the COVERED conversions (not all conversions) — otherwise an offer whose
// payout is only partially supplied reports an average that is silently low.
func TestNonCpmDeriveRates(t *testing.T) {
	r := nonCpmOfferRow{Delivered: 1_000_000, Clickers: 500, Conversions: 10, PayoutCoverage: 4, Revenue: 200}
	r.deriveRates()
	if r.Ecpm != 0.2 {
		t.Errorf("eCPM = %v, want 0.2 ($200 over 1M delivered)", r.Ecpm)
	}
	if r.AvgPayout != 50 {
		t.Errorf("avg payout = %v, want 50 ($200 over the 4 COVERED conversions)", r.AvgPayout)
	}
	if r.ClickerRate != 0.0005 {
		t.Errorf("clicker rate = %v, want 0.0005", r.ClickerRate)
	}
}

package api

import (
	"context"
	"encoding/json"
	"testing"
)

// TestDeploymentAuditMoneyLinkExtraction proves the JSONB money_links payload is
// derived from extractMoneyHrefs + slugFromMoneyURL over a sample creative.
func TestDeploymentAuditMoneyLinkExtraction(t *testing.T) {
	html := `<html><body>
		<a data-everflow="true" href="https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?source_id=email&sub1={{subscriber.id}}&sub2=discountblog.com&sub3={{campaign.id}}">Deal</a>
		<a href="https://eos57ytf.com/K4C5ZLC/PS8241/?source_id=email">Other</a>
		<a href="https://example.com/not-money">Ignore</a>
	</body></html>`

	links := make([]moneyLinkAudit, 0)
	for _, u := range extractMoneyHrefs(html) {
		links = append(links, moneyLinkAudit{URL: u, Slug: slugFromMoneyURL(u)})
	}

	if len(links) != 2 {
		t.Fatalf("expected 2 money links, got %d: %+v", len(links), links)
	}
	if links[0].Slug != "93W8N2N" {
		t.Errorf("link[0] slug: want 93W8N2N, got %q (url=%s)", links[0].Slug, links[0].URL)
	}
	if links[1].Slug != "PS8241" {
		t.Errorf("link[1] slug: want PS8241, got %q (url=%s)", links[1].Slug, links[1].URL)
	}

	// The payload must marshal to a JSONB array of {url,slug} objects.
	raw, err := json.Marshal(links)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt []map[string]string
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt[0]["url"] == "" || rt[0]["slug"] != "93W8N2N" {
		t.Errorf("round-tripped JSONB wrong: %+v", rt[0])
	}
}

// TestWriteDeploymentAuditNilDBNoPanic proves the FAIL-SAFE contract: a nil db
// handle must not panic and must return cleanly (no deploy can be broken by it).
func TestWriteDeploymentAuditNilDBNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeDeploymentAudit panicked with nil db: %v", r)
		}
	}()
	writeDeploymentAudit(
		context.Background(),
		nil, // nil db must be a no-op, not a panic
		"00000000-0000-0000-0000-000000000001",
		"camp-123",
		"offer_key",
		`<a href="https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/">x</a>`,
		"em.discountblog.com",
		1000,
	)
}

// TestSendingDomainFromBrandCode proves brand_code -> em.<apex> resolution reuses
// brandRootFromCode (covers canonical codes and Creative Studio slugs).
func TestSendingDomainFromBrandCode(t *testing.T) {
	cases := map[string]string{
		"DB":            "em.discountblog.com",
		"discount-blog": "em.discountblog.com",
		"":              "",
		"em.quizfiesta.com": "em.quizfiesta.com",
	}
	for in, want := range cases {
		if got := sendingDomainFromBrandCode(in); got != want {
			t.Errorf("sendingDomainFromBrandCode(%q) = %q, want %q", in, got, want)
		}
	}
}

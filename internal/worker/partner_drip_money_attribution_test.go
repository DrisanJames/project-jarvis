package worker

import (
	"strings"
	"testing"
)

// The 2026-07-09/10 Empire Today conversions arrived at Everflow with empty
// sub1/sub2/sub3 because the creative's money links carried only creative_id —
// permanently unattributable to a subscriber or source file. These tests pin
// the guard that decorates every money-network link at creative-resolve time.

func TestEnsureMoneyLinkAttribution_BareLink(t *testing.T) {
	in := `<a href="https://www.xnonu.com/TQ5MX18J/XF1SR2CS/">book now</a>`
	out := ensureMoneyLinkAttribution(in)
	want := `https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?source_id=email&sub1={{SUBSCRIBER_ID}}&sub3={{CAMPAIGN_ID}}`
	if !strings.Contains(out, want) {
		t.Fatalf("bare link not decorated:\n%s", out)
	}
}

func TestEnsureMoneyLinkAttribution_ExistingQuery(t *testing.T) {
	in := `<a href="https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?creative_id=57465">go</a>`
	out := ensureMoneyLinkAttribution(in)
	want := `?creative_id=57465&source_id=email&sub1={{SUBSCRIBER_ID}}&sub3={{CAMPAIGN_ID}}`
	if !strings.Contains(out, want) {
		t.Fatalf("query link not decorated:\n%s", out)
	}
}

func TestEnsureMoneyLinkAttribution_Idempotent(t *testing.T) {
	in := `<a href="https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?creative_id=1">x</a>`
	once := ensureMoneyLinkAttribution(in)
	twice := ensureMoneyLinkAttribution(once)
	if once != twice {
		t.Fatalf("not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
	if strings.Count(twice, "sub1={{SUBSCRIBER_ID}}") != 1 {
		t.Fatalf("suffix duplicated: %s", twice)
	}
}

func TestEnsureMoneyLinkAttribution_AllEnrollerHosts(t *testing.T) {
	for _, host := range []string{
		"https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/",
		"https://www.eos57ytf.com/K4C5ZLC/2HH43PB/",
		"https://www.k8k0hfdt.com/3QJ6DW/3MZNPR/",
		"https://www.muqes.com/TQ5MX18J/XLRZDZ8K/",
		"https://www.codefortwo.com/K4C5ZLC/S6WFF5/",
	} {
		out := ensureMoneyLinkAttribution(`<a href="` + host + `">x</a>`)
		if !strings.Contains(out, "sub1={{SUBSCRIBER_ID}}") {
			t.Errorf("host not decorated: %s", host)
		}
	}
}

func TestEnsureMoneyLinkAttribution_SkipsUnsubAndNonMoney(t *testing.T) {
	unsub := `<a href="https://www.cratoolpro.com/integration/unsub1/?_redir=abc123">unsub</a>`
	if out := ensureMoneyLinkAttribution(unsub); out != unsub {
		t.Fatalf("unsub URL mutated:\n%s", out)
	}
	plain := `<a href="https://www.example.com/page?x=1">site</a>`
	if out := ensureMoneyLinkAttribution(plain); out != plain {
		t.Fatalf("non-money URL mutated:\n%s", out)
	}
}

package moneylink

import "testing"

const (
	insAuto = "https://protectedoptions.com/?a=1512&c=18903&s1={{subscriber.id}}&s2={{brand.domain}}"
	insHome = "https://protectedoptions.com/?a=1512&c=20906&s1={{subscriber.id}}&s2={{brand.domain}}"
)

func insIndex() *Index {
	return NewIndex(map[string]string{MatchKey(insAuto): "auto-hash", MatchKey(insHome): "home-hash"})
}

func TestMatchKey_KeepsFixedParamsDropsPlaceholders(t *testing.T) {
	if got, want := MatchKey(insAuto), "https://protectedoptions.com?a=1512&c=18903"; got != want {
		t.Fatalf("MatchKey(auto) = %q, want %q", got, want)
	}
	if MatchKey(insAuto) == MatchKey(insHome) {
		t.Fatal("Auto and Home must not share a key (the 2026-10-06 collision)")
	}
	if got := MatchKey("https://www.cratoolpro.com/X/Y/"); got != Normalize("https://www.cratoolpro.com/X/Y/") {
		t.Fatalf("no-query template must key exactly as Normalize, got %q", got)
	}
}

// The 2026-10-06..10-12 defect: a rendered Insurify AUTO href resolved to the HOME hash.
func TestIndex_InsurifyAutoAndHomeResolveToTheirOwnHash(t *testing.T) {
	ix := insIndex()
	cases := map[string]string{
		"https://protectedoptions.com/?a=1512&c=18903&s1=4bbb5eba-51f1&s2=discountblog.com":           "auto-hash",
		"https://protectedoptions.com/?a=1512&amp;c=20906&amp;s1=4bbb5eba&amp;s2=historythinking.com": "home-hash",
		"https://protectedoptions.com/?c=18903&a=1512&s1=x":                                           "auto-hash",
	}
	for href, want := range cases {
		if got, ok := ix.Lookup(href); !ok || got != want {
			t.Errorf("Lookup(%q) = (%q,%v), want (%q,true)", href, got, ok, want)
		}
	}
}

// Ambiguous or unmatched → MISS, so the caller wraps the literal href (right destination).
func TestIndex_UnmatchedCollisionMisses(t *testing.T) {
	ix := insIndex()
	for _, href := range []string{
		"https://protectedoptions.com/?a=1512&c=19029&s1=x", // Insv2 creative id: neither row
		"https://protectedoptions.com/?a=1512&s1=x",         // no c at all
	} {
		if h, ok := ix.Lookup(href); ok {
			t.Errorf("Lookup(%q) = %q, want miss", href, h)
		}
	}
}

// Backward compatibility: a single row on a base resolves whatever the href's params are.
func TestIndex_SingleCandidateUnchanged(t *testing.T) {
	tmpl := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}"
	ix := NewIndex(map[string]string{MatchKey(tmpl): "h"})
	for _, href := range []string{
		"https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=s&sub2=d",
		"https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=other",
		"https://www.cratoolpro.com/BJB4Q5BF/ABC123/",
	} {
		if h, ok := ix.Lookup(href); !ok || h != "h" {
			t.Errorf("Lookup(%q) = (%q,%v), want (h,true)", href, h, ok)
		}
	}
	if _, ok := ix.Lookup("https://www.cratoolpro.com/OTHER/"); ok {
		t.Error("an unseeded base must miss")
	}
}

// A generic row (no fixed params) and a specific row on one base: the specific one wins when it matches.
func TestIndex_MostSpecificWins(t *testing.T) {
	gen := "https://n.example.com/go?s1={{subscriber.id}}"
	spec := "https://n.example.com/go?c=7&s1={{subscriber.id}}"
	ix := NewIndex(map[string]string{MatchKey(gen): "gen", MatchKey(spec): "spec"})
	if h, _ := ix.Lookup("https://n.example.com/go?c=7&s1=a"); h != "spec" {
		t.Errorf("specific href got %q, want spec", h)
	}
	if h, _ := ix.Lookup("https://n.example.com/go?c=8&s1=a"); h != "gen" {
		t.Errorf("non-matching href got %q, want gen", h)
	}
}

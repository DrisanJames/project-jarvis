package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// normalizeOfferURL
// ---------------------------------------------------------------------------

func TestNormalizeOfferURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strip query", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip mustache query", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip trailing slash", "https://www.cratoolpro.com/BJB4Q5BF/ABC123/", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"no trailing slash", "https://www.cratoolpro.com/BJB4Q5BF/ABC123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"lowercase host only, keep path case", "https://WWW.CratoolPro.COM/BJB4Q5BF/ABC123", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"strip fragment", "https://www.cratoolpro.com/BJB4Q5BF/ABC123#frag", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"whitespace trimmed", "  https://www.cratoolpro.com/BJB4Q5BF/ABC123/  ", "https://www.cratoolpro.com/BJB4Q5BF/ABC123"},
		{"http scheme lowercased", "HTTP://www.cratoolpro.com/A/B", "http://www.cratoolpro.com/A/B"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeOfferURL(c.in); got != c.want {
				t.Errorf("normalizeOfferURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The load-bearing property: a rendered creative href (concrete params) and the
// stored template ({{...}} params) reduce to the SAME key. This is what makes
// the inverse dictionary resolve a live send's href to its seeded hash.
func TestNormalizeOfferURL_RenderedAndTemplateAgree(t *testing.T) {
	template := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"
	rendered := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=sub-999&sub2=discountblog.com"
	if normalizeOfferURL(template) != normalizeOfferURL(rendered) {
		t.Fatalf("template and rendered href must normalize to the same key:\n template=%q\n rendered=%q",
			normalizeOfferURL(template), normalizeOfferURL(rendered))
	}
}

// ---------------------------------------------------------------------------
// SmartLinkEmitter — disabled modes start no goroutine and always miss
// ---------------------------------------------------------------------------

func TestSmartLinkEmitter_DisabledByFlag(t *testing.T) {
	// enabled=false with a non-nil-ish db value is impossible to construct
	// without a real *sql.DB; nil db exercises the same disabled path.
	e := NewSmartLinkEmitter(nil, time.Second, false)
	if e.Enabled() {
		t.Fatal("emitter with enabled=false must report Enabled()==false")
	}
	if _, ok := e.Lookup("https://www.cratoolpro.com/A/B"); ok {
		t.Fatal("disabled emitter must always miss")
	}
	if e.cancel != nil {
		t.Fatal("disabled emitter must not start a background goroutine (cancel is nil)")
	}
}

func TestSmartLinkEmitter_DisabledByNilDB(t *testing.T) {
	// enabled=true but db==nil must still be disabled (no goroutine, misses).
	e := NewSmartLinkEmitter(nil, time.Second, true)
	if e.Enabled() {
		t.Fatal("emitter with nil db must report Enabled()==false")
	}
	if e.cancel != nil {
		t.Fatal("nil-db emitter must not start a background goroutine")
	}
}

func TestSmartLinkEmitter_NilReceiverSafe(t *testing.T) {
	var e *SmartLinkEmitter
	if e.Enabled() {
		t.Fatal("nil emitter Enabled() must be false")
	}
	if _, ok := e.Lookup("https://www.cratoolpro.com/A/B"); ok {
		t.Fatal("nil emitter Lookup must miss")
	}
	if e.Len() != 0 {
		t.Fatal("nil emitter Len must be 0")
	}
	e.Close() // must not panic
}

// ---------------------------------------------------------------------------
// SmartLinkEmitter — enabled Lookup hit/miss via injected loader
// ---------------------------------------------------------------------------

// newTestEmitter builds an ENABLED emitter without a live DB by injecting a
// loadFn, then running one synchronous reload. No goroutine is started.
func newTestEmitter(t *testing.T, load func(context.Context) (map[string]string, error)) *SmartLinkEmitter {
	t.Helper()
	e := &SmartLinkEmitter{
		enabled: true,
		refresh: time.Minute,
		entries: make(map[string]string),
	}
	e.loadFn = load
	if err := e.reloadOnce(context.Background()); err != nil {
		t.Fatalf("initial reload failed: %v", err)
	}
	return e
}

func TestSmartLinkEmitter_LookupHitMiss(t *testing.T) {
	template := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1={{subscriber.id}}"
	load := func(context.Context) (map[string]string, error) {
		return map[string]string{normalizeOfferURL(template): "hash-abc"}, nil
	}
	e := newTestEmitter(t, load)

	// Hit: a rendered href with concrete params resolves to the seeded hash.
	rendered := "https://www.cratoolpro.com/BJB4Q5BF/ABC123/?source_id=email&sub1=sub-1&sub2=discountblog.com"
	if h, ok := e.Lookup(rendered); !ok || h != "hash-abc" {
		t.Fatalf("Lookup(rendered) = (%q,%v), want (hash-abc,true)", h, ok)
	}
	// Miss: a different offer path.
	if _, ok := e.Lookup("https://www.cratoolpro.com/BJB4Q5BF/OTHER/?source_id=email"); ok {
		t.Fatal("Lookup of an unseeded offer must miss")
	}
}

func TestSmartLinkEmitter_ReloadErrorKeepsPriorSnapshot(t *testing.T) {
	template := "https://www.cratoolpro.com/BJB4Q5BF/ABC123"
	good := map[string]string{normalizeOfferURL(template): "hash-abc"}

	calls := 0
	load := func(context.Context) (map[string]string, error) {
		calls++
		if calls == 1 {
			return good, nil
		}
		return nil, errors.New("simulated DB failure")
	}
	e := newTestEmitter(t, load) // call 1: good snapshot loaded

	if e.Len() != 1 {
		t.Fatalf("after good load, Len=%d want 1", e.Len())
	}
	// Second reload errors — must return the error AND keep the prior snapshot.
	if err := e.reloadOnce(context.Background()); err == nil {
		t.Fatal("reloadOnce should have returned the injected error")
	}
	if e.Len() != 1 {
		t.Fatalf("after failed reload, Len=%d want 1 (prior snapshot must survive)", e.Len())
	}
	if h, ok := e.Lookup(template); !ok || h != "hash-abc" {
		t.Fatalf("prior snapshot lost after failed reload: (%q,%v)", h, ok)
	}
}

package tracking

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// --- renderOfferDestination -------------------------------------------------

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func TestRenderOfferDestination_AppendsAttributionWhenAbsent(t *testing.T) {
	got := renderOfferDestination("https://cratoolpro.com/ABC/DEF", "sub-1", "discountblog.com", "camp-9", "")
	q := mustQuery(t, got)
	if q.Get("source_id") != "email" {
		t.Errorf("source_id = %q, want email", q.Get("source_id"))
	}
	if q.Get("sub1") != "sub-1" {
		t.Errorf("sub1 = %q, want sub-1", q.Get("sub1"))
	}
	if q.Get("sub2") != "discountblog.com" {
		t.Errorf("sub2 = %q, want discountblog.com", q.Get("sub2"))
	}
	if q.Get("sub3") != "camp-9" {
		t.Errorf("sub3 = %q, want camp-9", q.Get("sub3"))
	}
}

func TestRenderOfferDestination_SubstitutesMustache(t *testing.T) {
	tmpl := "https://ex.com/go?u={{subscriber.id}}&b={{brand.domain}}&c={{campaign}}"
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "")
	q := mustQuery(t, got)
	if q.Get("u") != "sub-1" {
		t.Errorf("{{subscriber.id}} -> u = %q, want sub-1", q.Get("u"))
	}
	if q.Get("b") != "discountblog.com" {
		t.Errorf("{{brand.domain}} -> b = %q, want discountblog.com", q.Get("b"))
	}
	if q.Get("c") != "camp-9" {
		t.Errorf("{{campaign}} -> c = %q, want camp-9", q.Get("c"))
	}
}

func TestRenderOfferDestination_DoesNotDuplicateExisting(t *testing.T) {
	// Template already carries sub1 and source_id — operator's values must win
	// and must not be duplicated.
	tmpl := "https://ex.com/go?source_id=push&sub1=preset"
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if len(q["sub1"]) != 1 || q.Get("sub1") != "preset" {
		t.Errorf("sub1 should be preserved single value, got %v", q["sub1"])
	}
	if len(q["source_id"]) != 1 || q.Get("source_id") != "push" {
		t.Errorf("source_id should be preserved single value, got %v", q["source_id"])
	}
	// The absent ones are still appended.
	if q.Get("sub2") != "discountblog.com" || q.Get("sub3") != "camp-9" {
		t.Errorf("absent attribution params not appended: %v", q)
	}
}

func TestRenderOfferDestination_PreservesOtherParamsAndEncodes(t *testing.T) {
	tmpl := "https://ex.com/go?keep=1&other=a%20b"
	got := renderOfferDestination(tmpl, "sub id/1", "discountblog.com", "camp-9", "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("keep") != "1" {
		t.Errorf("keep param dropped: %v", q)
	}
	if q.Get("other") != "a b" {
		t.Errorf("other param mangled: %q", q.Get("other"))
	}
	// Value with a space/slash must round-trip through decoding intact,
	// proving it was URL-encoded on the way out.
	if q.Get("sub1") != "sub id/1" {
		t.Errorf("sub1 not correctly encoded/decoded: %q", q.Get("sub1"))
	}
}

func TestRenderOfferDestination_MustacheValuesEncoded(t *testing.T) {
	// A value with characters that MUST be percent-encoded when placed in the
	// query string.
	tmpl := "https://ex.com/go?u={{subscriber.id}}"
	got := renderOfferDestination(tmpl, "a&b=c", "discountblog.com", "camp-9", "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("u") != "a&b=c" {
		t.Errorf("mustache value not safely encoded, decoded to %q", u.Query().Get("u"))
	}
}

// --- {{token}} opaque passthrough ------------------------------------------

// The whole point: an advertiser's per-recipient id survives the gateway.
func TestRenderOfferDestination_TokenPassthrough(t *testing.T) {
	tmpl := "https://autocoveragepoint.com/coverage-match?id=ff2007&s4=7552&channel=Rev&tokenid={{token}}"
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "abc123XYZ_-.~")
	q := mustQuery(t, got)
	if q.Get("tokenid") != "abc123XYZ_-.~" {
		t.Errorf("tokenid = %q, want abc123XYZ_-.~ (full URL: %s)", q.Get("tokenid"), got)
	}
	// The advertiser's own params must be untouched.
	if q.Get("id") != "ff2007" || q.Get("s4") != "7552" || q.Get("channel") != "Rev" {
		t.Errorf("advertiser params mangled: %v", q)
	}
}

// renderOfferDestination escapes the token itself, independently of the
// handler-side sanitizer — defense in depth for any future caller.
func TestRenderOfferDestination_TokenIsURLEscaped(t *testing.T) {
	tmpl := "https://ex.com/go?tokenid={{token}}"
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "a&b=c#d /e")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Host != "ex.com" {
		t.Errorf("host altered by token: %q", u.Host)
	}
	if u.Query().Get("tokenid") != "a&b=c#d /e" {
		t.Errorf("token not safely encoded, decoded to %q (full URL: %s)", u.Query().Get("tokenid"), got)
	}
	if _, injected := u.Query()["b"]; injected {
		t.Errorf("token injected a parameter: %v", u.Query())
	}
}

// A recipient with no tid must still reach the offer: {{token}} renders empty
// and the URL stays valid and structurally intact.
func TestRenderOfferDestination_EmptyTokenStillValidURL(t *testing.T) {
	tmpl := "https://autocoveragepoint.com/coverage-match?id=ff2007&tokenid={{token}}"
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("empty token produced an unparseable URL %q: %v", got, err)
	}
	if u.Scheme != "https" || u.Host != "autocoveragepoint.com" || u.Path != "/coverage-match" {
		t.Errorf("empty token altered the destination structure: %q", got)
	}
	if _, ok := u.Query()["tokenid"]; !ok {
		t.Errorf("tokenid key should survive as empty, got %q", got)
	}
	if u.Query().Get("tokenid") != "" {
		t.Errorf("tokenid should be empty, got %q", u.Query().Get("tokenid"))
	}
	// Attribution still applied.
	if u.Query().Get("sub1") != "sub-1" || u.Query().Get("sub2") != "discountblog.com" {
		t.Errorf("attribution lost on the empty-token path: %q", got)
	}
}

// liveCratoolproTemplates are the DISTINCT active offer_url_template values
// carrying cratoolpro money links, read from prod mailing_smart_links
// (status='active') on 2026-08-20: 59 active links, 32 cratoolpro, 21 distinct
// shapes, and ZERO of them contain {{token}}. This is the backward-compat
// contract: adding the token parameter must not move a single byte for any
// link already in an inbox.
var liveCratoolproTemplates = []string{
	"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/",
	"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?creative_id=547301&source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/BXPFT55/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/CL38PFR/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/CMHJBRD/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/CTCDKM2/",
	"https://www.cratoolpro.com/BJB4Q5BF/J876SLX/",
	"https://www.cratoolpro.com/BJB4Q5BF/J876SLX/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/JGCDCW7/",
	"https://www.cratoolpro.com/BJB4Q5BF/JJRFMXZ/",
	"https://www.cratoolpro.com/BJB4Q5BF/K3TL7NJ/",
	"https://www.cratoolpro.com/BJB4Q5BF/K435QLZ/",
	"https://www.cratoolpro.com/BJB4Q5BF/K5C8PQQ/",
	"https://www.cratoolpro.com/BJB4Q5BF/K5C8PQQ/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/K62P438/?creative_id=153833&source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/K86F3PC/",
	"https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/",
	"https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/KG53427/",
	"https://www.cratoolpro.com/BJB4Q5BF/KM15N5P/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
	"https://www.cratoolpro.com/BJB4Q5BF/KQ4MBHZ/",
}

// REGRESSION GUARD: for every live template (none of which has {{token}}), the
// rendered destination must be INDEPENDENT of the token argument — including a
// hostile one. If this ever fails, a link already sitting in someone's inbox
// changed.
func TestRenderOfferDestination_NoTokenPlaceholder_UnaffectedByToken(t *testing.T) {
	tokens := []string{"", "abc123", "x&foo=bar", "../", "\r\nX: y", strings.Repeat("A", 10240)}
	for _, tmpl := range liveCratoolproTemplates {
		base := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", "")
		for _, tok := range tokens {
			got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9", tok)
			if got != base {
				t.Errorf("template %q moved with token %q:\n got  %q\n want %q", tmpl, tok, got, base)
			}
		}
	}
}

// Byte-for-byte goldens for the three live template SHAPES, derived from the
// pre-token behavior (mustache substitution + setIfAbsent + url.Values.Encode,
// which sorts keys). Pins the exact output string, not just its stability.
func TestRenderOfferDestination_LiveTemplateGoldens(t *testing.T) {
	const sub = "11111111-1111-1111-1111-111111111111"
	const camp = "22222222-2222-2222-2222-222222222222"
	cases := []struct{ tmpl, want string }{
		{
			// bare path, no query at all
			"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/",
			"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?source_id=email&sub1=" + sub + "&sub2=discountblog.com&sub3=" + camp,
		},
		{
			// the standard minted shape
			"https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
			"https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email&sub1=" + sub + "&sub2=discountblog.com&sub3=" + camp,
		},
		{
			// with a baked-in creative_id
			"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?creative_id=547301&source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}",
			"https://www.cratoolpro.com/BJB4Q5BF/93W8N2N/?creative_id=547301&source_id=email&sub1=" + sub + "&sub2=discountblog.com&sub3=" + camp,
		},
	}
	for _, c := range cases {
		if got := renderOfferDestination(c.tmpl, sub, "discountblog.com", camp, ""); got != c.want {
			t.Errorf("golden mismatch for %q:\n got  %q\n want %q", c.tmpl, got, c.want)
		}
		// And with a token present, since none of these carry {{token}}.
		if got := renderOfferDestination(c.tmpl, sub, "discountblog.com", camp, "tok999"); got != c.want {
			t.Errorf("token leaked into a no-{{token}} template %q: %q", c.tmpl, got)
		}
	}
}

// --- SmartLinkDictionary ----------------------------------------------------

func TestDictionary_NilDBIsSafeNoOp(t *testing.T) {
	d := NewSmartLinkDictionary(nil, time.Minute)
	if d == nil {
		t.Fatal("nil-db dictionary must still be a usable value")
	}
	if _, ok := d.Lookup("anything"); ok {
		t.Error("nil-db dictionary should always miss")
	}
	if d.Len() != 0 {
		t.Errorf("nil-db dictionary Len = %d, want 0", d.Len())
	}
	// Close must be safe even though no goroutine was started.
	d.Close()
}

func TestDictionary_NilReceiverLookupSafe(t *testing.T) {
	var d *SmartLinkDictionary
	if _, ok := d.Lookup("x"); ok {
		t.Error("nil-receiver Lookup should miss, not panic")
	}
	d.Close() // must not panic
}

func TestDictionary_LookupHitMiss(t *testing.T) {
	d := &SmartLinkDictionary{entries: map[string]smartLinkEntry{
		"abc123": {Destination: "https://ex.com/x", RiskProfile: "low", BrandRoot: "discountblog.com"},
	}}
	e, ok := d.Lookup("abc123")
	if !ok || e.Destination != "https://ex.com/x" {
		t.Errorf("hit failed: %+v ok=%v", e, ok)
	}
	if _, ok := d.Lookup("nope"); ok {
		t.Error("miss should return ok=false")
	}
}

func TestDictionary_ReloadErrorKeepsPriorSnapshot(t *testing.T) {
	d := &SmartLinkDictionary{
		entries: map[string]smartLinkEntry{
			"good01": {Destination: "https://ex.com/good", RiskProfile: "low"},
		},
	}
	// Inject a loader that always fails — reloadOnce must keep the prior map.
	d.loadFn = func(ctx context.Context) (map[string]smartLinkEntry, error) {
		return nil, errors.New("transient db error")
	}
	if err := d.reloadOnce(context.Background()); err == nil {
		t.Fatal("expected reload error")
	}
	if e, ok := d.Lookup("good01"); !ok || e.Destination != "https://ex.com/good" {
		t.Errorf("prior snapshot must survive a failed reload, got %+v ok=%v", e, ok)
	}
}

func TestDictionary_ReloadSuccessSwapsSnapshot(t *testing.T) {
	d := &SmartLinkDictionary{entries: map[string]smartLinkEntry{
		"old001": {Destination: "https://ex.com/old"},
	}}
	d.loadFn = func(ctx context.Context) (map[string]smartLinkEntry, error) {
		return map[string]smartLinkEntry{
			"new001": {Destination: "https://ex.com/new"},
		}, nil
	}
	if err := d.reloadOnce(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := d.Lookup("old001"); ok {
		t.Error("old entry should be gone after successful swap")
	}
	if _, ok := d.Lookup("new001"); !ok {
		t.Error("new entry should be present after successful swap")
	}
}

func TestDictionary_QueryDBParsesRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"hash", "offer_url_template", "risk_profile", "brand_root"}).
		AddRow("abc123", "https://ex.com/a?u={{subscriber.id}}", "high", "discountblog.com").
		AddRow("def456", "https://ex.com/b", "low", "quizfiesta.com").
		AddRow("", "https://ex.com/skip", "low", "x.com"). // empty hash -> skipped
		AddRow("ghi789", "", "low", "y.com")               // empty template -> skipped
	mock.ExpectQuery("SELECT hash, offer_url_template").WillReturnRows(rows)

	d := &SmartLinkDictionary{db: db, entries: map[string]smartLinkEntry{}}
	got, err := d.queryDB(context.Background())
	if err != nil {
		t.Fatalf("queryDB: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 usable rows, got %d: %+v", len(got), got)
	}
	if got["abc123"].RiskProfile != "high" || got["abc123"].BrandRoot != "discountblog.com" {
		t.Errorf("row abc123 mis-parsed: %+v", got["abc123"])
	}
	if _, ok := got["ghi789"]; ok {
		t.Error("empty-template row should have been skipped")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestDictionary_QueryDBErrorPropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT hash, offer_url_template").WillReturnError(errors.New("boom"))

	d := &SmartLinkDictionary{db: db, entries: map[string]smartLinkEntry{}}
	if _, err := d.queryDB(context.Background()); err == nil {
		t.Error("queryDB should propagate the DB error so reloadOnce keeps the prior snapshot")
	}
}

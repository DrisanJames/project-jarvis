package tracking

import (
	"context"
	"errors"
	"net/url"
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
	got := renderOfferDestination("https://cratoolpro.com/ABC/DEF", "sub-1", "discountblog.com", "camp-9")
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
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9")
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
	got := renderOfferDestination(tmpl, "sub-1", "discountblog.com", "camp-9")
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
	got := renderOfferDestination(tmpl, "sub id/1", "discountblog.com", "camp-9")
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
	got := renderOfferDestination(tmpl, "a&b=c", "discountblog.com", "camp-9")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Query().Get("u") != "a&b=c" {
		t.Errorf("mustache value not safely encoded, decoded to %q", u.Query().Get("u"))
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

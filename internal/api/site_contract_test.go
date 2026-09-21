package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

const testSiteKey = "sk_abcdefghijklmnopqrstuvwxyz012345"

type fakeSiteStore struct {
	key       siteKey
	seen      map[string]bool
	forgotten []string
	added     []string
	prefs     []preferences.Preference
	unsubbed  []string
	addErr    error
}

func (f *fakeSiteStore) ResolveKey(_ context.Context, hash string) (siteKey, error) {
	if hash != HashPartnerKey(testSiteKey) {
		return siteKey{}, errSiteKeyNotFound
	}
	return f.key, nil
}
func (f *fakeSiteStore) RecordEvent(_ context.Context, _ siteKey, ev SiteEvent, _ string) (bool, error) {
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[ev.EventID] {
		return false, nil
	}
	f.seen[ev.EventID] = true
	return true, nil
}
func (f *fakeSiteStore) ForgetEvent(_ context.Context, _ siteKey, id string) {
	delete(f.seen, id)
	f.forgotten = append(f.forgotten, id)
}
func (f *fakeSiteStore) SaveResult(context.Context, siteKey, string, map[string]any) {}
func (f *fakeSiteStore) AddSubscriber(_ context.Context, _ siteKey, email, _, _ string) (bool, error) {
	if f.addErr != nil {
		return false, f.addErr
	}
	f.added = append(f.added, email)
	return true, nil
}
func (f *fakeSiteStore) MarkUnsubscribed(_ context.Context, _, email string) error {
	f.unsubbed = append(f.unsubbed, email)
	return nil
}
func (f *fakeSiteStore) UpsertPreference(_ context.Context, p preferences.Preference) error {
	f.prefs = append(f.prefs, p)
	return nil
}

type fakeSiteHub struct {
	suppressed map[string]bool
	scoped     []string
	global     []string
}

func (h *fakeSiteHub) IsSuppressed(email string) bool { return h.suppressed[email] }
func (h *fakeSiteHub) IsSuppressedForBrand(email, brandRoot string) bool {
	return h.suppressed[email+"|"+brandRoot]
}
func (h *fakeSiteHub) Suppress(_ context.Context, email, _, _, _, _, _, _, _ string) (bool, error) {
	h.global = append(h.global, email)
	return true, nil
}
func (h *fakeSiteHub) SuppressScoped(_ context.Context, email, brandRoot, _, _, _, _, _ string) error {
	h.scoped = append(h.scoped, email+"|"+brandRoot)
	return nil
}
func (h *fakeSiteHub) RemoveScoped(context.Context, string, string) error { return nil }

var siteNow = time.Date(2026, 9, 12, 18, 0, 0, 0, time.UTC)

func newTestSiteHandler() (*SiteContractHandler, *fakeSiteStore, *fakeSiteHub) {
	st := &fakeSiteStore{key: siteKey{ID: "k1", OrgID: "00000000-0000-0000-0000-000000000001", Site: "bestcreditcare.com", ListID: "a0617f9e-40ba-48c1-a2d9-09d3eedc3d84"}}
	hub := &fakeSiteHub{suppressed: map[string]bool{}}
	return &SiteContractHandler{store: st, hub: hub, now: func() time.Time { return siteNow }}, st, hub
}

func postSite(h *SiteContractHandler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/sites/v1/events", strings.NewReader(body))
	if key != "" {
		req.Header.Set(siteKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	h.HandleEvent(rec, req)
	return rec
}

func TestSiteContract_AuthAndSiteBinding(t *testing.T) {
	h, st, _ := newTestSiteHandler()
	body := `{"event_id":"e1","type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"}}`
	if rec := postSite(h, "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d", rec.Code)
	}
	if rec := postSite(h, "sk_wrongwrongwrongwrongwrongwrong", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", rec.Code)
	}
	other := strings.Replace(body, "bestcreditcare.com", "discountblog.com", 1)
	if rec := postSite(h, testSiteKey, other); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "site_mismatch") {
		t.Fatalf("a key must not write for another site: %d %s", rec.Code, rec.Body)
	}
	if len(st.added) != 0 {
		t.Fatal("nothing may be written without a valid key for the site")
	}
	www := strings.Replace(body, `"site":"bestcreditcare.com"`, `"site":"www.bestcreditcare.com"`, 1)
	if rec := postSite(h, testSiteKey, www); rec.Code != http.StatusAccepted {
		t.Fatalf("www. form of the key's site is the same site: %d %s", rec.Code, rec.Body)
	}
}

func TestSiteContract_SubscribeAddsUnlessSuppressed(t *testing.T) {
	h, st, hub := newTestSiteHandler()
	rec := postSite(h, testSiteKey, `{"event_id":"e1","type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":" Reader@Example.com "}}`)
	if rec.Code != http.StatusAccepted || len(st.added) != 1 || st.added[0] != "reader@example.com" {
		t.Fatalf("confirmed signup must be added (normalized): %d %v %s", rec.Code, st.added, rec.Body)
	}
	hub.suppressed["gone@example.com|bestcreditcare.com"] = true
	rec = postSite(h, testSiteKey, `{"event_id":"e2","type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":"gone@example.com"}}`)
	if rec.Code != http.StatusAccepted || len(st.added) != 1 || !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Fatalf("a suppressed address must never be re-added: %d %v %s", rec.Code, st.added, rec.Body)
	}
}

func TestSiteContract_PreferencesAreBrandScopedAndValidated(t *testing.T) {
	h, st, _ := newTestSiteHandler()
	rec := postSite(h, testSiteKey, `{"event_id":"p1","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"},"preferences":{"frequency":"weekly","paused_until":"2026-10-01T00:00:00Z","topics":{"credit_scores":true,"cards":false}}}`)
	if rec.Code != http.StatusAccepted || len(st.prefs) != 1 {
		t.Fatalf("preferences must be saved: %d %s", rec.Code, rec.Body)
	}
	p := st.prefs[0]
	if p.Frequency != "weekly" || p.BrandRoot != "bestcreditcare.com" || p.EmailHash != preferences.EmailHash("reader@example.com") ||
		p.PausedUntil == nil || p.Topics["cards"] || !p.Topics["credit_scores"] || p.Source != "site:bestcreditcare.com" {
		t.Fatalf("wrong preference row: %+v", p)
	}
	for _, bad := range []string{
		`{"event_id":"p2","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"},"preferences":{"frequency":"daily"}}`,
		`{"event_id":"p3","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"},"preferences":{"paused_until":"2025-01-01T00:00:00Z"}}`,
		`{"event_id":"p4","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"}}`,
		`{"event_id":"p5","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"},"preferences":{"topics":{"Bad Topic":true}}}`,
		`{"event_id":"p6","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"not-an-email"},"preferences":{}}`,
		`{"event_id":"p7","type":"preferences.updated","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"},"preferences":{},"extra":1}`,
	} {
		if rec := postSite(h, testSiteKey, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("must be refused (400): %d %s for %s", rec.Code, rec.Body, bad)
		}
	}
	if len(st.prefs) != 1 {
		t.Fatalf("invalid payloads must not write: %d rows", len(st.prefs))
	}
}

func TestSiteContract_UnsubscribeBrandAndAll(t *testing.T) {
	h, st, hub := newTestSiteHandler()
	postSite(h, testSiteKey, `{"event_id":"u1","type":"unsubscribe","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"}}`)
	postSite(h, testSiteKey, `{"event_id":"u2","type":"unsubscribe","site":"bestcreditcare.com","subscriber":{"email":"all@example.com"},"unsubscribe":{"scope":"all"}}`)
	if len(hub.scoped) != 1 || hub.scoped[0] != "reader@example.com|bestcreditcare.com" {
		t.Fatalf("default scope is the site's brand: %v", hub.scoped)
	}
	if len(hub.global) != 1 || hub.global[0] != "all@example.com" || len(st.unsubbed) != 1 {
		t.Fatalf("scope all = global suppression + subscriber status: %v %v", hub.global, st.unsubbed)
	}
	if rec := postSite(h, testSiteKey, `{"event_id":"u3","type":"unsubscribe","site":"bestcreditcare.com","subscriber":{"email":"x@example.com"},"unsubscribe":{"scope":"everything"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope must be refused: %d", rec.Code)
	}
}

func TestSiteContract_IdempotentDryRunAndRetryAfterFailure(t *testing.T) {
	h, st, _ := newTestSiteHandler()
	body := `{"event_id":"e1","type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":"reader@example.com"}}`
	postSite(h, testSiteKey, body)
	rec := postSite(h, testSiteKey, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) || len(st.added) != 1 {
		t.Fatalf("a repeated event_id changes nothing: %d %s %v", rec.Code, rec.Body, st.added)
	}
	dry := `{"event_id":"d1","dry_run":true,"type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":"new@example.com"}}`
	if rec := postSite(h, testSiteKey, dry); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plan"`) || len(st.added) != 1 || st.seen["d1"] {
		t.Fatalf("dry_run writes nothing and returns the plan: %d %s", rec.Code, rec.Body)
	}
	st.addErr = errors.New("db down")
	fail := `{"event_id":"f1","type":"subscribe.confirmed","site":"bestcreditcare.com","subscriber":{"email":"retry@example.com"}}`
	if rec := postSite(h, testSiteKey, fail); rec.Code != http.StatusInternalServerError || len(st.forgotten) != 1 {
		t.Fatalf("a failed apply must be forgotten so the site can retry: %d %v", rec.Code, st.forgotten)
	}
	st.addErr = nil
	if rec := postSite(h, testSiteKey, fail); rec.Code != http.StatusAccepted {
		t.Fatalf("the retry with the same event_id applies: %d %s", rec.Code, rec.Body)
	}
}

func TestGenerateSiteKey_FormatAndHash(t *testing.T) {
	raw, prefix, hash, err := generateSiteKey()
	if err != nil || !strings.HasPrefix(raw, "sk_") || len(raw) != 35 || prefix != raw[:8] || hash != HashPartnerKey(raw) {
		t.Fatalf("raw=%q prefix=%q err=%v", raw, prefix, err)
	}
}

// TestPGSiteContractStore_AddSubscriberStampsProvenance pins the real Postgres
// store (the fake above cannot see SQL). source / source_detail were ABSENT
// from this INSERT: a site signup landed with NULL provenance, indistinguishable
// from a CSV import in every downstream read. The site event it emits is the
// dashboard's DYNAMIC-supply counter for that property.
func TestPGSiteContractStore_AddSubscriberStampsProvenance(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	k := siteKey{
		ID:     "key-1",
		OrgID:  "00000000-0000-0000-0000-000000000001",
		Site:   "discountblog.com",
		ListID: "44444444-4444-4444-4444-444444444444",
	}
	mock.ExpectQuery(`INSERT INTO mailing_subscribers \(id, organization_id, list_id, email, email_hash, first_name, last_name, status, source, source_detail`).
		WithArgs(k.OrgID, k.ListID, "reader@icloud.com", sqlmock.AnyArg(), "A", "B", k.Site).
		WillReturnRows(sqlmock.NewRows([]string{"inserted"}).AddRow(true))
	mock.ExpectExec(`UPDATE mailing_lists SET subscriber_count`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mailing_inbox_profiles`).WillReturnResult(sqlmock.NewResult(0, 1))

	var got []dataingest.Event
	restore := dataingest.SetTestSink(func(ev dataingest.Event) { got = append(got, ev) })
	defer dataingest.SetTestSink(restore)

	store := &pgSiteContractStore{db: db}
	inserted, err := store.AddSubscriber(context.Background(), k, "reader@icloud.com", "A", "B")
	if err != nil {
		t.Fatalf("AddSubscriber: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true (xmax = 0)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("source/source_detail not stamped on the INSERT: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("emitted %d ingest events, want exactly 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.Transition != dataingest.TransitionSiteEvent || ev.Source != dataingest.SourceSiteEvent {
		t.Errorf("event = %s/%s, want site_event/site_event", ev.Transition, ev.Source)
	}
	if ev.Class != dataingest.ClassDynamic {
		t.Errorf("supply_class = %q, want dynamic (a site event is live supply, brain #3589)", ev.Class)
	}
	if ev.N != 1 || ev.ByISP["apple"] != 1 {
		t.Errorf("n=%d by_isp=%v, want n=1 apple:1 (classified from the EMAIL)", ev.N, ev.ByISP)
	}
	if ev.Lane != k.Site {
		t.Errorf("lane = %q, want the property %q", ev.Lane, k.Site)
	}
}

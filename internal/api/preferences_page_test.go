package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

const (
	pOrg    = "00000000-0000-0000-0000-000000000001"
	pSub    = "55555555-5555-5555-5555-555555555555"
	pSecret = "test-secret"
	pEmail  = "human@example.com"
	pBrand  = "discountblog.com"
)

type fakePrefHub struct {
	mu     sync.Mutex
	global map[string]bool
	brand  map[string]bool // "email|brand"
	calls  []string
}

func newFakePrefHub() *fakePrefHub {
	return &fakePrefHub{global: map[string]bool{}, brand: map[string]bool{}}
}
func (f *fakePrefHub) IsSuppressed(e string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.global[e]
}
func (f *fakePrefHub) IsSuppressedForBrand(e, b string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.global[e] || f.brand[e+"|"+b]
}
func (f *fakePrefHub) Suppress(_ context.Context, e, reason, source, isp, _, _, _, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.global[e] = true
	f.calls = append(f.calls, "global:"+source)
	return true, nil
}
func (f *fakePrefHub) SuppressScoped(_ context.Context, e, b, reason, source, isp, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.brand[e+"|"+b] = true
	f.calls = append(f.calls, "brand:"+b+":"+source)
	return nil
}
func (f *fakePrefHub) RemoveScoped(_ context.Context, e, b string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.brand, e+"|"+b)
	f.calls = append(f.calls, "remove:"+b)
	return nil
}

var pNow = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func newPrefsHarness(t *testing.T) (*PreferencesHandler, sqlmock.Sqlmock, *fakePrefHub, *preferences.Hub) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	hub := newFakePrefHub()
	ph := preferences.NewHub(nil, pOrg)
	ph.SetModeFunc(func() preferences.Mode { return preferences.ModeEnforce })
	h := &PreferencesHandler{
		db: db, hub: hub,
		prefs:   func() *preferences.Hub { return ph },
		secret:  func() string { return pSecret },
		now:     func() time.Time { return pNow },
		limiter: newFreshLinkLimiter(100),
	}
	return h, mock, hub, ph
}

func pToken(scope preferences.Scope, now time.Time) string {
	return preferences.Mint(pOrg, pSub, pBrand, scope, now, pSecret)
}

func expectEmailLookup(mock sqlmock.Sqlmock, email string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id = $1 AND organization_id = $2`)).
		WithArgs(pSub, pOrg).WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(email))
}

func expectPrefsGet(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscriber_preferences`)).
		WillReturnRows(sqlmock.NewRows([]string{"brand_root", "paused_until", "frequency", "topics", "updated_at", "source"}))
}

func postForm(target string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// ── RFC 8058 one-click ──────────────────────────────────────────────────────

func TestOneClick_ValidTokenWithoutCookiesSucceeds(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	db, mock, _ := sqlmock.New()
	defer db.Close()
	hub := newFakePrefHub()
	tok := pToken(preferences.ScopeBrand, time.Now())
	expectEmailLookup(mock, pEmail)

	req := postForm("/unsubscribe/one-click?t="+url.QueryEscape(tok), url.Values{"List-Unsubscribe": {"One-Click"}})
	require.Empty(t, req.Cookies())
	rec := httptest.NewRecorder()
	serveTokenOneClick(rec, req, db, hub)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"brand:" + pBrand + ":rfc8058_one_click"}, hub.calls, "brand token must suppress the brand only")
	require.True(t, hub.IsSuppressedForBrand(pEmail, pBrand))
	require.False(t, hub.IsSuppressed(pEmail), "brand scope must not widen to global")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOneClick_AllScopeTokenSuppressesGlobally(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	db, mock, _ := sqlmock.New()
	defer db.Close()
	hub := newFakePrefHub()
	expectEmailLookup(mock, pEmail)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE mailing_subscribers SET status = 'unsubscribed'`)).
		WithArgs(pSub).WillReturnResult(sqlmock.NewResult(0, 1))

	rec := httptest.NewRecorder()
	serveTokenOneClick(rec, postForm("/unsubscribe/one-click?t="+url.QueryEscape(pToken(preferences.ScopeAll, time.Now())),
		url.Values{"List-Unsubscribe": {"One-Click"}}), db, hub)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, hub.IsSuppressed(pEmail))
	require.NoError(t, mock.ExpectationsWereMet())
}

// The pre-fix handler suppressed any `email` (or `token`) form value.
func TestOneClick_BareEmailRejected(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	db, _, _ := sqlmock.New()
	defer db.Close()
	for _, form := range []url.Values{
		{"email": {"victim@example.com"}},
		{"token": {"victim@example.com"}},
		{"List-Unsubscribe": {"One-Click"}, "email": {"victim@example.com"}},
	} {
		hub := newFakePrefHub()
		rec := httptest.NewRecorder()
		serveTokenOneClick(rec, postForm("/unsubscribe/one-click", form), db, hub)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, hub.calls, "bare email must never suppress (form %v)", form)
	}
	// Same through the SuppressionService wrapper (the route's handler).
	rec := httptest.NewRecorder()
	(&SuppressionService{db: db}).HandleOneClickUnsubscribe(rec, postForm("/unsubscribe/one-click", url.Values{"email": {"victim@example.com"}}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestOneClick_ModifiedTokenRejected(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	db, _, _ := sqlmock.New()
	defer db.Close()
	tok := pToken(preferences.ScopeBrand, time.Now())
	dot := strings.LastIndexByte(tok, '.')
	raw, _ := base64.RawURLEncoding.DecodeString(tok[:dot])
	widened := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "|brand|", "|all|", 1))) + tok[dot:]
	flipped := tok[:len(tok)-1] + "0"
	if tok[len(tok)-1] == '0' {
		flipped = tok[:len(tok)-1] + "1"
	}
	for _, bad := range []string{widened, flipped, tok + "x", "v1.garbage"} {
		hub := newFakePrefHub()
		rec := httptest.NewRecorder()
		serveTokenOneClick(rec, postForm("/unsubscribe/one-click?t="+url.QueryEscape(bad), url.Values{"List-Unsubscribe": {"One-Click"}}), db, hub)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, hub.calls)
	}
}

// Without a hub the one-click must NOT claim success (the ISP retries a 503).
func TestOneClick_NoHubIs503NotSilentSuccess(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	db, mock, _ := sqlmock.New()
	defer db.Close()
	expectEmailLookup(mock, pEmail)
	rec := httptest.NewRecorder()
	(&SuppressionService{db: db}).HandleOneClickUnsubscribe(rec,
		postForm("/unsubscribe/one-click?t="+url.QueryEscape(pToken(preferences.ScopeBrand, time.Now())), url.Values{"List-Unsubscribe": {"One-Click"}}))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ── page GET ────────────────────────────────────────────────────────────────

func TestPreferencesGet_ValidTokenRendersState(t *testing.T) {
	h, mock, hub, _ := newPrefsHarness(t)
	hub.brand[pEmail+"|quizfiesta.com"] = true // another brand's suppression must not show here
	tok := pToken(preferences.ScopeBrand, pNow)
	expectEmailLookup(mock, pEmail)
	expectPrefsGet(mock)

	rec := httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/preferences?t="+url.QueryEscape(tok), nil))
	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, "Discount Blog")
	require.Contains(t, body, "Unsubscribe from Discount Blog")
	require.Contains(t, body, "Unsubscribe from all")
	require.Contains(t, body, "h•••@example.com", "email is masked")
	require.NotContains(t, body, pEmail)
	require.Contains(t, body, `name="n" value="`+preferences.Nonce(tok, pSecret, pNow)+`"`)
	require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPreferencesGet_XSSPayloadsEscaped(t *testing.T) {
	const xss = `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	// 1) invalid token + hostile params -> neutral page, nothing reflected
	h, mock, _, _ := newPrefsHarness(t)
	rec := httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/preferences?t="+url.QueryEscape(xss)+"&sid="+url.QueryEscape(xss)+"&done="+url.QueryEscape(xss), nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotContains(t, rec.Body.String(), "<script>alert")
	require.NotContains(t, rec.Body.String(), "onerror=alert")

	// 2) valid token, hostile address in the DB + hostile params -> escaped
	tok := pToken(preferences.ScopeBrand, pNow)
	expectEmailLookup(mock, `a@x.com`+xss)
	expectPrefsGet(mock)
	rec = httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/preferences?t="+url.QueryEscape(tok)+"&done="+url.QueryEscape(xss)+"&intent="+url.QueryEscape(xss), nil))
	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, body, "<script>alert")
	require.NotContains(t, body, "<img src=x")
	require.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", "hostile DB value rendered escaped")

	// 3) legacy /unsubscribe with hostile sid -> neutral, not reflected
	rec = httptest.NewRecorder()
	handleUnsubscribeLanding(rec, httptest.NewRequest(http.MethodGet, "/unsubscribe?sid="+url.QueryEscape(xss)+"&cid=1&token=abc", nil))
	require.NotContains(t, rec.Body.String(), "<script>alert")
}

func TestPreferencesGet_ExpiredAndMissingAreNeutral(t *testing.T) {
	h, _, _, _ := newPrefsHarness(t)
	old := pToken(preferences.ScopeBrand, pNow.Add(-preferences.TokenTTL-preferences.ExpiryGrace-time.Hour))
	rec := httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/preferences?t="+url.QueryEscape(old), nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "This link has expired")
	require.NotContains(t, rec.Body.String(), "fresh-link", "fresh-link form is off unless PREFERENCES_FRESH_LINK_EMAIL is set and a sender exists")

	rec = httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/preferences", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Link not recognised")
}

// ── legacy /unsubscribe ─────────────────────────────────────────────────────

func TestUnsubscribeLanding_LegacyMD5AuthorizesNothing(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	rec := httptest.NewRecorder()
	handleUnsubscribeLanding(rec, httptest.NewRequest(http.MethodGet,
		"/unsubscribe?sid="+pSub+"&cid=22222222-2222-2222-2222-222222222222&token=0123456789abcdef", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Location"), "legacy link must not redirect into /track/unsubscribe")
	require.Contains(t, rec.Body.String(), "no longer supported")

	tok := pToken(preferences.ScopeBrand, time.Now())
	rec = httptest.NewRecorder()
	handleUnsubscribeLanding(rec, httptest.NewRequest(http.MethodGet, "/unsubscribe?t="+url.QueryEscape(tok), nil))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/preferences?t="+url.QueryEscape(tok)+"&intent=unsubscribe", rec.Header().Get("Location"))
}

// ── page POST ───────────────────────────────────────────────────────────────

func TestPreferencesPost_UnsubscribeBrand(t *testing.T) {
	h, mock, hub, _ := newPrefsHarness(t)
	tok := pToken(preferences.ScopeBrand, pNow)
	expectEmailLookup(mock, pEmail)
	rec := httptest.NewRecorder()
	h.HandlePost(rec, postForm("/preferences", url.Values{"t": {tok}, "n": {preferences.Nonce(tok, pSecret, pNow)}, "action": {"unsub_brand"}}))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Location"), "done=unsub_brand")
	require.Equal(t, []string{"brand:" + pBrand + ":" + prefsPageSource}, hub.calls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPreferencesPost_CSRFFailures(t *testing.T) {
	h, _, hub, _ := newPrefsHarness(t)
	tok := pToken(preferences.ScopeBrand, pNow)
	other := pToken(preferences.ScopeAll, pNow)
	for name, n := range map[string]string{
		"missing":      "",
		"garbage":      "deadbeef",
		"other token":  preferences.Nonce(other, pSecret, pNow),
		"stale (3h)":   preferences.Nonce(tok, pSecret, pNow.Add(-3*time.Hour)),
		"wrong secret": preferences.Nonce(tok, "other", pNow),
	} {
		rec := httptest.NewRecorder()
		h.HandlePost(rec, postForm("/preferences", url.Values{"t": {tok}, "n": {n}, "action": {"unsub_all"}}))
		require.Equal(t, http.StatusForbidden, rec.Code, name)
		require.Empty(t, hub.calls, "%s: CSRF failure must change nothing", name)
	}
}

func TestPreferencesPost_PauseWritesStoreAndHub(t *testing.T) {
	h, mock, _, ph := newPrefsHarness(t)
	tok := pToken(preferences.ScopeBrand, pNow)
	expectEmailLookup(mock, pEmail)
	expectPrefsGet(mock)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_subscriber_preferences`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	rec := httptest.NewRecorder()
	h.HandlePost(rec, postForm("/preferences", url.Values{"t": {tok}, "n": {preferences.Nonce(tok, pSecret, pNow)}, "action": {"pause_30"}}))
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
	ok, reason := ph.ShouldSend(pEmail, "quizfiesta.com", "", time.Now())
	require.False(t, ok, "pause applies to all brands and refreshes this task's hub on write")
	require.Equal(t, "paused", reason)
}

func TestPreferencesPost_InvalidFrequencyRejected(t *testing.T) {
	h, mock, _, _ := newPrefsHarness(t)
	tok := pToken(preferences.ScopeBrand, pNow)
	expectEmailLookup(mock, pEmail)
	rec := httptest.NewRecorder()
	h.HandlePost(rec, postForm("/preferences", url.Values{"t": {tok}, "n": {preferences.Nonce(tok, pSecret, pNow)}, "action": {"frequency"}, "frequency": {"hourly"}}))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// Resubscribe removes ONLY a brand suppression the recipient made on the page.
func TestPreferencesPost_ResubscribeOnlyOwnSuppression(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   int
	}{{"tracking_link", http.StatusConflict}, {prefsPageSource, http.StatusSeeOther}} {
		h, mock, hub, _ := newPrefsHarness(t)
		hub.brand[pEmail+"|"+pBrand] = true
		tok := pToken(preferences.ScopeBrand, pNow)
		expectEmailLookup(mock, pEmail)
		mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_domain_suppressions`)).
			WillReturnRows(sqlmock.NewRows([]string{"source"}).AddRow(tc.source))
		rec := httptest.NewRecorder()
		h.HandlePost(rec, postForm("/preferences", url.Values{"t": {tok}, "n": {preferences.Nonce(tok, pSecret, pNow)}, "action": {"resubscribe_brand"}}))
		require.Equal(t, tc.want, rec.Code, "source=%s", tc.source)
		if tc.want == http.StatusSeeOther {
			require.Equal(t, []string{"remove:" + pBrand}, hub.calls)
		} else {
			require.Empty(t, hub.calls, "must not remove a suppression the page did not create")
		}
	}
}

func TestFreshLinkLimiter(t *testing.T) {
	l := newFreshLinkLimiter(100)
	require.True(t, l.allow("s1", "ip", pNow))
	require.False(t, l.allow("s1", "ip2", pNow.Add(time.Hour)), "one per subscriber per 24h")
	require.True(t, l.allow("s1", "ip2", pNow.Add(25*time.Hour)))
}

// ── mailto webhook ──────────────────────────────────────────────────────────

func mailtoRequest(recipient string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/mailing/webhooks/unsub-inbound",
		strings.NewReader(`{"recipient":"`+recipient+`"}`))
}

func newMailtoService(t *testing.T) (*MailingService, sqlmock.Sqlmock, *engine.GlobalSuppressionHub) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	mock.MatchExpectationsInOrder(false) // side-effect writes are fire-and-forget
	hub := engine.NewGlobalSuppressionHub(db, pOrg, "")
	return &MailingService{db: db, signingKey: pSecret, globalHub: hub, throttler: NewMailingThrottler()}, mock, hub
}

const mailtoCamp = "22222222-2222-2222-2222-222222222222"

func mailtoPayload() string {
	return base64.URLEncoding.EncodeToString([]byte(pOrg + "|" + mailtoCamp + "|" + pSub))
}

// Written through the hub: enforced by this process immediately, no restart.
func TestMailtoUnsub_SignedWritesThroughHub(t *testing.T) {
	svc, mock, hub := newMailtoService(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(pEmail))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_global_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	require.False(t, hub.IsSuppressed(pEmail))

	tok := worker.SignedMailtoToken(mailtoPayload(), pSecret)
	rec := httptest.NewRecorder()
	svc.HandleInboundMailtoUnsubscribe(rec, mailtoRequest("unsub+"+tok+"@em.discountblog.com"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, hub.IsSuppressed(pEmail), "mailto unsubscribe must be visible to the send path without a restart")
	require.NoError(t, mock.ExpectationsWereMet())
}

// Legacy unsigned tokens (already in inboxes) are still honoured.
func TestMailtoUnsub_LegacyUnsignedStillHonoured(t *testing.T) {
	svc, mock, hub := newMailtoService(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT email FROM mailing_subscribers WHERE id = $1`)).
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow(pEmail))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_global_suppressions`)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	rec := httptest.NewRecorder()
	svc.HandleInboundMailtoUnsubscribe(rec, mailtoRequest("unsub+"+mailtoPayload()+"@em.discountblog.com"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, hub.IsSuppressed(pEmail))
}

func TestMailtoUnsub_ForgedSignatureIgnored(t *testing.T) {
	svc, _, hub := newMailtoService(t)
	rec := httptest.NewRecorder()
	svc.HandleInboundMailtoUnsubscribe(rec, mailtoRequest("unsub+"+mailtoPayload()+".0000000000000000@em.discountblog.com"))
	require.Equal(t, http.StatusOK, rec.Code, "200 so SNS does not retry a forgery")
	require.False(t, hub.IsSuppressed(pEmail))
}

// ── transactional ───────────────────────────────────────────────────────────

func TestBuildTxnUnsubLinks(t *testing.T) {
	t.Setenv("TRACKING_SECRET", pSecret)
	t.Setenv("PREFERENCES_BASE_URL", "")
	none := buildTxnUnsubLinks(pOrg, "txn", "", "a@em.discountblog.com", "https://t.em.discountblog.com", pSecret, time.Now())
	require.Empty(t, none.PrefsURL, "no subscriber row: no unsubscribe link")
	require.Empty(t, none.Header.Header)

	got := buildTxnUnsubLinks(pOrg, "33333333-3333-3333-3333-333333333333", pSub, "a@em.discountblog.com", "https://t.em.discountblog.com", pSecret, time.Now())
	require.True(t, strings.HasPrefix(got.PrefsURL, "https://t.em.discountblog.com/track/preferences?t="), got.PrefsURL)
	require.Contains(t, got.Header.Header, "<https://projectjarvis.io/unsubscribe/one-click?t=")
	require.Contains(t, got.Header.Header, "@em.discountblog.com?subject=unsubscribe>")
	require.NotContains(t, got.Header.Header, "33333333-3333-3333-3333-333333333333|", "payload is encoded, not raw")
	tokStart := strings.Index(got.PrefsURL, "t=") + 2
	c, err := preferences.Verify(got.PrefsURL[tokStart:], pSecret, time.Now())
	require.NoError(t, err)
	require.Equal(t, pSub, c.SubscriberID, "the REAL subscriber, never the txn id")
}

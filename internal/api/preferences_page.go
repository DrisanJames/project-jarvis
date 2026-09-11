package api

// Recipient-facing email preferences + token-authorized unsubscribe.
//
//	GET  /preferences?t=<v1 token>   server-rendered page (no JS)
//	POST /preferences                token + token-bound nonce (CSRF)
//	POST /preferences/fresh-link     re-mail a link for an EXPIRED token
//	GET  /unsubscribe                v1 token -> page; legacy md5 -> neutral
//	POST /unsubscribe/one-click?t=   RFC 8058 (suppression_extras_handlers.go)
//
// Every mutation is authorized by a signed v1 token (internal/preferences).
// A bare email, a subscriber UUID, or a legacy md5 token authorizes nothing.
// Unsubscribes write through the global suppression hub (DB + this task's
// memory in one call); pause/frequency write mailing_subscriber_preferences
// and refresh the preference hub.

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
)

// preferenceSuppressor is the slice of *engine.GlobalSuppressionHub the page
// and the token unsubscribe paths need.
type preferenceSuppressor interface {
	IsSuppressed(email string) bool
	IsSuppressedForBrand(email, brandRoot string) bool
	Suppress(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) (bool, error)
	SuppressScoped(ctx context.Context, email, brandRoot, reason, source, isp, sourceIP, campaign string) error
	RemoveScoped(ctx context.Context, email, brandRoot string) error
}

// prefsPageSource is the suppression `source` for writes made from the page.
// It is also the resubscribe guard: only a brand suppression carrying this
// source (i.e. one the recipient made here) may be removed from the page.
const prefsPageSource = "preferences_page"

var errPrefsHubNotWired = errors.New("suppression hub not wired")

// preferencesSecret is TRACKING_SECRET — the key the send worker signs
// tracking URLs with. Read per call and deliberately WITHOUT the dev default
// NewMailingService applies: an unset secret must verify nothing.
func preferencesSecret() string { return os.Getenv("TRACKING_SECRET") }

// PreferencesBaseURL is where token links that must reach the API host point
// (fresh-link mails, the transactional one-click leg). Default production host.
func PreferencesBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("PREFERENCES_BASE_URL")), "/"); v != "" {
		return v
	}
	return "https://projectjarvis.io"
}

// freshLinkEnabled gates the "email me a fresh link" mail. Default OFF.
func freshLinkEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PREFERENCES_FRESH_LINK_EMAIL")))
	return v == "1" || v == "true"
}

// freshLinkSender mails link to email from brandRoot's sending profile.
type freshLinkSender func(ctx context.Context, email, brandRoot, link string) error

// preferencesPageCounters surface on /health.preferences_page.
var preferencesPageCounters struct {
	oneClickOK, oneClickRejected, legacyUnsub, pageViews, posts, csrfRejected, freshLinkSent atomic.Int64
}

// PreferencesPageStatus is the /health "preferences_page" block.
func PreferencesPageStatus() map[string]interface{} {
	c := &preferencesPageCounters
	return map[string]interface{}{
		"wired":              preferencesHandler.Load() != nil,
		"one_click_ok":       c.oneClickOK.Load(),
		"one_click_rejected": c.oneClickRejected.Load(),
		"legacy_unsub_links": c.legacyUnsub.Load(),
		"page_views":         c.pageViews.Load(),
		"posts":              c.posts.Load(),
		"csrf_rejected":      c.csrfRejected.Load(),
		"fresh_link_sent":    c.freshLinkSent.Load(),
		"fresh_link_enabled": freshLinkEnabled(),
	}
}

// preferencesHandler is published once the suppression hub exists (the page
// cannot report or change suppression state without it). The routes are
// registered at construction and answer 503 until then.
var preferencesHandler atomic.Pointer[PreferencesHandler]

// PreferencesHandler serves the preference page.
type PreferencesHandler struct {
	db        *sql.DB
	hub       preferenceSuppressor
	prefs     func() *preferences.Hub
	secret    func() string
	now       func() time.Time
	freshLink freshLinkSender
	limiter   *freshLinkLimiter
}

// NewPreferencesHandler wires the page. fresh may be nil (no fresh-link mail).
func NewPreferencesHandler(db *sql.DB, hub preferenceSuppressor, fresh freshLinkSender) *PreferencesHandler {
	return &PreferencesHandler{
		db:        db,
		hub:       hub,
		prefs:     preferences.Current,
		secret:    preferencesSecret,
		now:       time.Now,
		freshLink: fresh,
		limiter:   newFreshLinkLimiter(10_000),
	}
}

// ── token unsubscribe (shared with the RFC 8058 one-click handler) ─────────

func lookupSubscriberEmail(ctx context.Context, db *sql.DB, c preferences.Claims) (string, error) {
	var email string
	err := db.QueryRowContext(ctx,
		`SELECT email FROM mailing_subscribers WHERE id = $1 AND organization_id = $2`,
		c.SubscriberID, c.OrgID).Scan(&email)
	return strings.TrimSpace(email), err
}

// applyTokenUnsubscribe honours the scope carried by a verified token:
// brand -> brand-scoped suppression; all -> global suppression + subscriber
// status. Both go through the hub so this task enforces them without a restart.
func applyTokenUnsubscribe(ctx context.Context, db *sql.DB, hub preferenceSuppressor, c preferences.Claims, email, source, ip string) error {
	if hub == nil {
		return errPrefsHubNotWired
	}
	isp := extractISP(email)
	if c.Scope == preferences.ScopeBrand && c.BrandRoot != "" {
		return hub.SuppressScoped(ctx, email, c.BrandRoot, "user_unsubscribe", source, isp, ip, "")
	}
	if _, err := hub.Suppress(ctx, email, "unsubscribe", source, isp, "", "", ip, ""); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mailing_subscribers SET status = 'unsubscribed', updated_at = NOW() WHERE id = $1`,
		c.SubscriberID); err != nil {
		log.Printf("[preferences] subscriber status update failed (suppression already written) sub=%s: %v", c.SubscriberID, err)
	}
	return nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ── page ────────────────────────────────────────────────────────────────────

func setPrefsPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// The token rides in the URL: never leak it to another origin.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

type neutralKind int

const (
	neutralInvalid neutralKind = iota
	neutralExpired
	neutralLegacy
	neutralUnavailable
	neutralFormExpired
	neutralSaveFailed
	neutralFreshLink
)

type prefsNeutralView struct {
	Title, Message string
	ExpiredToken   string // non-empty -> offer the fresh-link form
	RetryURL       string
}

var neutralCopy = map[neutralKind][2]string{
	neutralInvalid:     {"Link not recognised", "This link isn't valid. Please use the \"manage preferences\" or \"unsubscribe\" link in any recent email from us."},
	neutralExpired:     {"This link has expired", "For your security, preference links expire. Please use the link in a more recent email from us."},
	neutralLegacy:      {"This link has expired", "This unsubscribe link is no longer supported. Please use the \"unsubscribe\" link in a recent email from us, or reply to it with \"unsubscribe\"."},
	neutralUnavailable: {"Temporarily unavailable", "We can't load your preferences right now. Please try again in a few minutes."},
	neutralFormExpired: {"Please try again", "This form expired before it was sent. Reload the page and try again."},
	neutralSaveFailed:  {"We couldn't save that", "Nothing was changed. Please try again in a few minutes."},
	neutralFreshLink:   {"Check your inbox", "If this address is still on our lists, we've sent a new preferences link. It can take a few minutes to arrive."},
}

func renderPrefsNeutral(w http.ResponseWriter, status int, kind neutralKind, expiredToken, retryURL string) {
	setPrefsPageHeaders(w)
	c := neutralCopy[kind]
	w.WriteHeader(status)
	if err := prefsNeutralTmpl.Execute(w, prefsNeutralView{Title: c[0], Message: c[1], ExpiredToken: expiredToken, RetryURL: retryURL}); err != nil {
		log.Printf("[preferences] neutral render: %v", err)
	}
}

type prefsPageView struct {
	BrandLabel, MaskedEmail, Token, Nonce string
	HasBrand, Intent                      bool
	BrandSubscribed, AllSubscribed        bool
	CanResubscribe                        bool
	PausedUntil                           string
	ShowPause                             bool
	Frequency                             string
	Done                                  string
}

var doneCopy = map[string]string{
	// No "takes effect immediately": the write lands in the DB and THIS ECS
	// task's hub at once, but the sibling task's hub only reloads brand rows
	// on restart or a global-count divergence (engine ReconcileNow).
	"unsub_brand":  "You're unsubscribed from %s.",
	"unsub_all":    "You're unsubscribed from all of our emails.",
	"paused":       "Your emails are paused.",
	"resumed":      "Your emails are resumed.",
	"frequency":    "Saved your preferred frequency.",
	"resubscribed": "You're resubscribed to %s. It can take up to a day to fully take effect.",
}

func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "your address"
	}
	return email[:1] + "•••" + email[at:]
}

// HandleGet renders the page for a valid token.
func (h *PreferencesHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	preferencesPageCounters.pageViews.Add(1)
	tok := r.URL.Query().Get("t")
	c, ok := h.verifyOrNeutral(w, tok)
	if !ok {
		return
	}
	ctx := r.Context()
	email, err := lookupSubscriberEmail(ctx, h.db, c)
	if err != nil || email == "" {
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
		return
	}
	view := h.buildView(ctx, c, tok, email, r.URL.Query())
	setPrefsPageHeaders(w)
	if err := prefsPageTmpl.Execute(w, view); err != nil {
		log.Printf("[preferences] page render: %v", err)
	}
}

// verifyOrNeutral renders the neutral page (and returns ok=false) for a
// missing, invalid or expired token.
func (h *PreferencesHandler) verifyOrNeutral(w http.ResponseWriter, tok string) (preferences.Claims, bool) {
	if tok == "" {
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
		return preferences.Claims{}, false
	}
	c, err := preferences.Verify(tok, h.secret(), h.now())
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, preferences.ErrExpired):
		offer := ""
		if h.freshLink != nil && freshLinkEnabled() {
			offer = tok
		}
		renderPrefsNeutral(w, http.StatusBadRequest, neutralExpired, offer, "")
	default:
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
	}
	return preferences.Claims{}, false
}

func (h *PreferencesHandler) buildView(ctx context.Context, c preferences.Claims, tok, email string, q url.Values) prefsPageView {
	v := prefsPageView{
		MaskedEmail: maskEmail(email),
		Token:       tok,
		Nonce:       preferences.Nonce(tok, h.secret(), h.now()),
		HasBrand:    c.BrandRoot != "",
		Intent:      q.Get("intent") == "unsubscribe",
		Frequency:   preferences.FrequencyNormal,
		// A pause we would not honour is a promise we would not keep: in shadow
		// mode the control is shown only on explicit operator preview.
		ShowPause: preferences.ModeFromEnv() == preferences.ModeEnforce || q.Get("preview") == "1",
	}
	v.BrandLabel = "our emails"
	if v.HasBrand {
		v.BrandLabel = brand.Label(c.BrandRoot)
	}
	global := h.hub.IsSuppressed(email)
	v.AllSubscribed = !global
	v.BrandSubscribed = !global && !(v.HasBrand && h.hub.IsSuppressedForBrand(email, c.BrandRoot))
	if v.HasBrand && !global && !v.BrandSubscribed {
		v.CanResubscribe = h.brandSuppressionSource(ctx, c, email) == prefsPageSource
	}
	if rows, err := preferences.Get(ctx, h.db, c.OrgID, preferences.EmailHash(email)); err == nil {
		for _, p := range rows {
			if p.BrandRoot != "" {
				continue
			}
			if preferences.ValidFrequency(p.Frequency) {
				v.Frequency = p.Frequency
			}
			if p.PausedUntil != nil && h.now().Before(*p.PausedUntil) {
				v.PausedUntil = p.PausedUntil.Format("January 2, 2006")
			}
		}
	}
	if msg, ok := doneCopy[q.Get("done")]; ok {
		if strings.Contains(msg, "%s") {
			msg = strings.Replace(msg, "%s", v.BrandLabel, 1)
		}
		v.Done = msg
	}
	return v
}

// brandSuppressionSource returns the source of the brand-scoped suppression
// row for (org, email, brand), or "" when none / on error.
func (h *PreferencesHandler) brandSuppressionSource(ctx context.Context, c preferences.Claims, email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	var src sql.NullString
	if err := h.db.QueryRowContext(ctx,
		`SELECT source FROM mailing_domain_suppressions WHERE organization_id = $1 AND email_hash = $2 AND brand_root = $3`,
		c.OrgID, hex.EncodeToString(sum[:]), c.BrandRoot).Scan(&src); err != nil {
		return ""
	}
	return src.String
}

// HandlePost applies one action. CSRF: the form nonce must be bound to the
// same token and be <2h old.
func (h *PreferencesHandler) HandlePost(w http.ResponseWriter, r *http.Request) {
	preferencesPageCounters.posts.Add(1)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
		return
	}
	tok := r.PostFormValue("t")
	c, ok := h.verifyOrNeutral(w, tok)
	if !ok {
		return
	}
	back := "/preferences?t=" + url.QueryEscape(tok)
	if !preferences.VerifyNonce(tok, r.PostFormValue("n"), h.secret(), h.now()) {
		preferencesPageCounters.csrfRejected.Add(1)
		renderPrefsNeutral(w, http.StatusForbidden, neutralFormExpired, "", back)
		return
	}
	ctx := r.Context()
	email, err := lookupSubscriberEmail(ctx, h.db, c)
	if err != nil || email == "" {
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
		return
	}
	ip := clientIP(r)
	action := r.PostFormValue("action")
	var done string
	switch action {
	case "unsub_brand":
		if c.BrandRoot == "" {
			renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
			return
		}
		err = h.hub.SuppressScoped(ctx, email, c.BrandRoot, "user_unsubscribe", prefsPageSource, extractISP(email), ip, "")
		done = "unsub_brand"
	case "unsub_all":
		all := c
		all.Scope = preferences.ScopeAll
		err = applyTokenUnsubscribe(ctx, h.db, h.hub, all, email, prefsPageSource, ip)
		done = "unsub_all"
	case "pause_30", "pause_90":
		days := 30
		if action == "pause_90" {
			days = 90
		}
		until := h.now().AddDate(0, 0, days)
		err = h.writeAllBrands(ctx, c, email, func(p *preferences.Preference) { p.PausedUntil = &until })
		done = "paused"
	case "resume":
		err = h.writeAllBrands(ctx, c, email, func(p *preferences.Preference) { p.PausedUntil = nil })
		done = "resumed"
	case "frequency":
		f := r.PostFormValue("frequency")
		if !preferences.ValidFrequency(f) {
			renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
			return
		}
		err = h.writeAllBrands(ctx, c, email, func(p *preferences.Preference) { p.Frequency = f })
		done = "frequency"
	case "resubscribe_brand":
		// Only undo a brand suppression the recipient made on this page, and
		// never while a global suppression stands. The hub is add-only per
		// ECS task on reload: RemoveScoped clears the row and THIS task's
		// memory; the sibling task keeps suppressing until its next restart
		// (fails safe — the page copy says "up to a day").
		if c.BrandRoot == "" || h.hub.IsSuppressed(email) || h.brandSuppressionSource(ctx, c, email) != prefsPageSource {
			renderPrefsNeutral(w, http.StatusConflict, neutralInvalid, "", back)
			return
		}
		err = h.hub.RemoveScoped(ctx, email, c.BrandRoot)
		done = "resubscribed"
	default:
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", back)
		return
	}
	if err != nil {
		log.Printf("[preferences] action=%s sub=%s email=%s failed: %v", action, c.SubscriberID, logger.RedactEmail(email), err)
		renderPrefsNeutral(w, http.StatusServiceUnavailable, neutralSaveFailed, "", back)
		return
	}
	log.Printf("[preferences] action=%s sub=%s brand=%s email=%s", action, c.SubscriberID, c.BrandRoot, logger.RedactEmail(email))
	http.Redirect(w, r, back+"&done="+done, http.StatusSeeOther)
}

// writeAllBrands read-modify-writes the all-brands preference row.
func (h *PreferencesHandler) writeAllBrands(ctx context.Context, c preferences.Claims, email string, mutate func(*preferences.Preference)) error {
	hash := preferences.EmailHash(email)
	p := preferences.Preference{OrgID: c.OrgID, EmailHash: hash, Frequency: preferences.FrequencyNormal}
	rows, err := preferences.Get(ctx, h.db, c.OrgID, hash)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.BrandRoot == "" {
			p = r
		}
	}
	mutate(&p)
	p.Source = prefsPageSource
	return preferences.Upsert(ctx, h.db, h.prefs(), p)
}

// HandleFreshLink re-mails a preferences link for an EXPIRED (but genuinely
// signed) token, to the subscriber address on file only — never to an
// address typed into the form, so it cannot be aimed at a third party.
func (h *PreferencesHandler) HandleFreshLink(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	_ = r.ParseForm()
	tok := r.PostFormValue("t")
	c, err := preferences.Verify(tok, h.secret(), h.now())
	if err == nil {
		http.Redirect(w, r, "/preferences?t="+url.QueryEscape(tok), http.StatusSeeOther)
		return
	}
	if !errors.Is(err, preferences.ErrExpired) || h.freshLink == nil || !freshLinkEnabled() {
		renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
		return
	}
	// Same response whether we send, rate-limit, or find nobody.
	defer renderPrefsNeutral(w, http.StatusOK, neutralFreshLink, "", "")
	if !h.limiter.allow(c.SubscriberID, clientIP(r), h.now()) {
		return
	}
	email, lerr := lookupSubscriberEmail(r.Context(), h.db, c)
	if lerr != nil || email == "" {
		return
	}
	fresh := preferences.Mint(c.OrgID, c.SubscriberID, c.BrandRoot, c.Scope, h.now(), h.secret())
	if fresh == "" {
		return
	}
	if serr := h.freshLink(r.Context(), email, c.BrandRoot, PreferencesBaseURL()+"/preferences?t="+url.QueryEscape(fresh)); serr != nil {
		log.Printf("[preferences] fresh-link send failed sub=%s: %v", c.SubscriberID, serr)
		return
	}
	preferencesPageCounters.freshLinkSent.Add(1)
}

// handleUnsubscribeLanding serves GET /unsubscribe. A v1 token goes to the
// page (GET never mutates — mailbox scanners prefetch links). The legacy
// ?sid=&cid=&token=<md5> shape from context_builder authorizes NOTHING.
func handleUnsubscribeLanding(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if t := q.Get("t"); t != "" {
		if _, err := preferences.Verify(t, preferencesSecret(), time.Now()); err == nil || errors.Is(err, preferences.ErrExpired) {
			http.Redirect(w, r, "/preferences?t="+url.QueryEscape(t)+"&intent=unsubscribe", http.StatusSeeOther)
			return
		}
	}
	if q.Get("sid") != "" || q.Get("token") != "" {
		preferencesPageCounters.legacyUnsub.Add(1)
		renderPrefsNeutral(w, http.StatusBadRequest, neutralLegacy, "", "")
		return
	}
	renderPrefsNeutral(w, http.StatusBadRequest, neutralInvalid, "", "")
}

// serveTokenOneClick is the RFC 8058 one-click body (see
// SuppressionService.HandleOneClickUnsubscribe). Token from the URL query (the
// List-Unsubscribe URL) or a `t` form field.
func serveTokenOneClick(w http.ResponseWriter, r *http.Request, db *sql.DB, hub preferenceSuppressor) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	_ = r.ParseForm() // body is List-Unsubscribe=One-Click; the token decides
	tok := r.URL.Query().Get("t")
	if tok == "" {
		tok = r.PostFormValue("t")
	}
	c, err := preferences.Verify(tok, preferencesSecret(), time.Now())
	if err != nil {
		preferencesPageCounters.oneClickRejected.Add(1)
		http.Error(w, "Invalid or expired unsubscribe link", http.StatusBadRequest)
		return
	}
	email, err := lookupSubscriberEmail(r.Context(), db, c)
	if err != nil || email == "" {
		preferencesPageCounters.oneClickRejected.Add(1)
		http.Error(w, "Invalid or expired unsubscribe link", http.StatusBadRequest)
		return
	}
	if err := applyTokenUnsubscribe(r.Context(), db, hub, c, email, "rfc8058_one_click", clientIP(r)); err != nil {
		log.Printf("[preferences] one-click unsubscribe failed sub=%s scope=%s: %v", c.SubscriberID, c.Scope, err)
		http.Error(w, "Temporarily unavailable", http.StatusServiceUnavailable) // mailbox provider retries
		return
	}
	preferencesPageCounters.oneClickOK.Add(1)
	log.Printf("One-click unsubscribe (token scope=%s brand=%s) → %s", c.Scope, c.BrandRoot, logger.RedactEmail(email))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Unsubscribed successfully"))
}

// planPreferenceGate is the planner's preference check (shadow by default —
// counts "planner.<reason>", denies only under PREFERENCES_MODE=enforce).
func planPreferenceGate(email, brandRoot string) (bool, string) {
	return preferences.Current().Gate("planner", email, brandRoot, "", time.Now())
}

func preferencesHealth() preferences.Status { return preferences.CurrentStatus() }

// ── fresh-link rate limiter (per task, bounded) ─────────────────────────────

type freshLinkLimiter struct {
	mu         sync.Mutex
	maxEntries int
	bySub      map[string]time.Time
	byIP       map[string][]time.Time
}

func newFreshLinkLimiter(maxEntries int) *freshLinkLimiter {
	return &freshLinkLimiter{maxEntries: maxEntries, bySub: map[string]time.Time{}, byIP: map[string][]time.Time{}}
}

// allow: one link per subscriber per 24h, ten per client IP per hour.
func (l *freshLinkLimiter) allow(sub, ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, t := range l.bySub {
		if now.Sub(t) >= 24*time.Hour {
			delete(l.bySub, k)
		}
	}
	for k, ts := range l.byIP {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Hour {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.byIP, k)
		} else {
			l.byIP[k] = kept
		}
	}
	if _, seen := l.bySub[sub]; seen || len(l.byIP[ip]) >= 10 {
		return false
	}
	if len(l.bySub) >= l.maxEntries || len(l.byIP) >= l.maxEntries {
		return false
	}
	l.bySub[sub] = now
	l.byIP[ip] = append(l.byIP[ip], now)
	return true
}

// ── templates (html/template escapes every interpolation) ───────────────────

const prefsPageCSS = `body{margin:0;background:#f4f5f7;font:15px/1.5 -apple-system,Segoe UI,Roboto,Arial,sans-serif;color:#1f2937}
.wrap{max-width:520px;margin:40px auto;padding:0 16px}.card{background:#fff;border-radius:10px;padding:28px;box-shadow:0 1px 6px rgba(0,0,0,.08)}
h1{font-size:21px;margin:0 0 4px}p{margin:8px 0}.muted{color:#6b7280;font-size:13px}.ok{background:#ecfdf5;border:1px solid #a7f3d0;color:#065f46;padding:10px 12px;border-radius:6px;margin:12px 0}
.row{display:flex;justify-content:space-between;align-items:center;padding:12px 0;border-top:1px solid #eef0f3}.st{font-size:13px;font-weight:600}.on{color:#047857}.off{color:#b91c1c}
form{margin:0}button{font:inherit;padding:8px 14px;border-radius:6px;border:1px solid #d1d5db;background:#fff;cursor:pointer}button.primary{background:#1f2937;color:#fff;border-color:#1f2937}
select{font:inherit;padding:7px;border-radius:6px;border:1px solid #d1d5db}.act{display:flex;gap:8px;flex-wrap:wrap;margin-top:8px}`

var prefsPageTmpl = template.Must(template.New("prefs").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Email preferences — {{.BrandLabel}}</title><style>` + prefsPageCSS + `</style></head><body><div class="wrap"><div class="card">
<h1>Email preferences</h1><p class="muted">{{.BrandLabel}} · {{.MaskedEmail}}</p>
{{if .Done}}<div class="ok" role="status">{{.Done}}</div>{{end}}
{{if .Intent}}<p><strong>Unsubscribe?</strong> Choose below — nothing changes until you click a button.</p>{{end}}
{{if .HasBrand}}<div class="row"><div>{{.BrandLabel}}<br><span class="st {{if .BrandSubscribed}}on">Subscribed{{else}}off">Unsubscribed{{end}}</span></div>
{{if .BrandSubscribed}}<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="unsub_brand"><button class="primary" type="submit">Unsubscribe from {{.BrandLabel}}</button></form>
{{else if .CanResubscribe}}<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="resubscribe_brand"><button type="submit">Resubscribe</button></form>{{end}}</div>{{end}}
<div class="row"><div>All of our brands<br><span class="st {{if .AllSubscribed}}on">Subscribed{{else}}off">Unsubscribed from all{{end}}</span></div>
{{if .AllSubscribed}}<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="unsub_all"><button type="submit">Unsubscribe from all</button></form>{{end}}</div>
{{if and .AllSubscribed .ShowPause}}<div class="row"><div>Pause emails<br><span class="st">{{if .PausedUntil}}Paused until {{.PausedUntil}}{{else}}Not paused{{end}}</span></div></div>
<div class="act">{{if .PausedUntil}}<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="resume"><button type="submit">Resume now</button></form>{{end}}
<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="pause_30"><button type="submit">Pause 30 days</button></form>
<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="pause_90"><button type="submit">Pause 90 days</button></form></div>{{end}}
{{if .AllSubscribed}}<div class="row"><div>Preferred frequency</div>
<form method="post" action="/preferences"><input type="hidden" name="t" value="{{.Token}}"><input type="hidden" name="n" value="{{.Nonce}}"><input type="hidden" name="action" value="frequency">
<select name="frequency" aria-label="Preferred frequency"><option value="normal"{{if eq .Frequency "normal"}} selected{{end}}>Normal</option><option value="reduced"{{if eq .Frequency "reduced"}} selected{{end}}>Fewer emails</option><option value="weekly"{{if eq .Frequency "weekly"}} selected{{end}}>About weekly</option></select>
<button type="submit">Save</button></form></div>{{end}}
</div></div></body></html>`))

var prefsNeutralTmpl = template.Must(template.New("neutral").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + prefsPageCSS + `</style></head><body><div class="wrap"><div class="card">
<h1>{{.Title}}</h1><p>{{.Message}}</p>
{{if .ExpiredToken}}<form method="post" action="/preferences/fresh-link"><input type="hidden" name="t" value="{{.ExpiredToken}}"><button class="primary" type="submit">Email me a new link</button></form>{{end}}
{{if .RetryURL}}<p><a href="{{.RetryURL}}">Back to your preferences</a></p>{{end}}
</div></div></body></html>`))

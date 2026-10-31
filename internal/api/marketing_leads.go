package api

// Public marketing-site lead capture.
//
//	GET  /api/public/form-token   → {"token": "..."}
//	POST /api/public/leads        → early_access | demo | contact | api_access
//	POST /api/public/onboarding   → new-customer form; needs a Google-derived token
//	GET  /api/admin/marketing-leads (X-Admin-Key)
//
// Bot layers, in the same shape as the brand-site forms: signed form token
// (min age 3s, max age 2h), honeypot, Origin check, durable per-IP and global
// hourly limits, body cap, strict field validation.
//
// A caught bot gets the same 200 as a real submission, with no write and no
// mail. A person must never get that: a stale tab answers 409 so the page can
// fetch a new token, and the limit answers 429 so the page shows its error.
//
// Leads are saved first. The operator's notification is claimed from the row
// by a sweeper, so a failed send retries and two ECS tasks never double-send.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ignite/sparkpost-monitor/internal/auth"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

const (
	leadBodyLimit      = 8 << 10
	leadTokenMinAge    = 3 * time.Second
	leadTokenMaxAge    = 2 * time.Hour
	leadPerIPHourly    = 6
	leadGlobalHourly   = 60
	leadNotifyAttempts = 6
	leadNotifyTo       = "drisan@jamesventurescorp.com"
	// Sent straight through the SES API, not a sending profile, so it never
	// queues behind campaign mail. The identity is verified in SES_REGION.
	leadNotifyFrom = "Jarvis Leads <notify@em.discountblog.com>"
)

// leadSubjects are fixed. Nothing a visitor typed reaches a mail header.
var leadSubjects = map[string]string{
	"early_access": "Jarvis site: early access request",
	"demo":         "Jarvis site: demo request",
	"contact":      "Jarvis site: contact request",
	"api_access":   "Jarvis site: API access request",
	"onboarding":   "Jarvis site: new customer onboarding",
}

var leadEmailRe = regexp.MustCompile(`^[^\s@<>"',;:\\]{1,64}@[a-zA-Z0-9](?:[a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?\.[a-zA-Z]{2,}$`)

// leadNotifier delivers one notification; tests record instead of sending.
type leadNotifier interface {
	Send(ctx context.Context, subject, body string) error
}

type verifyOnboardingFn func(token string, now time.Time) (*auth.OnboardingClaims, error)

type marketingLeads struct {
	db               atomic.Pointer[sql.DB]
	formKey          []byte
	ipKey            []byte
	verifyOnboarding verifyOnboardingFn
	notifier         leadNotifier
	origins          map[string]bool
	now              func() time.Time
	wake             chan struct{}
}

// leads is package-level because the routes exist before the DB does, the same
// reason serveSESEventsWhenReady is.
var leads = &marketingLeads{now: time.Now, wake: make(chan struct{}, 1)}

// setAuth derives the keys. With no auth manager (dev) the forms answer 503.
func (m *marketingLeads) setAuth(am *auth.AuthManager) {
	if am == nil {
		return
	}
	m.formKey = am.DerivedKey("marketing-form-token")
	m.ipKey = am.DerivedKey("marketing-ip-hash")
	m.verifyOnboarding = am.VerifyOnboardingToken
	m.origins = leadOrigins(os.Getenv("AUTH_BASE_URL"), os.Getenv("DEV_MODE") == "true" || os.Getenv("ENVIRONMENT") == "development")
}

func leadOrigins(base string, dev bool) map[string]bool {
	out := map[string]bool{"https://projectjarvis.io": true, "https://www.projectjarvis.io": true}
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
		out[u.Scheme+"://"+u.Host] = true
	}
	if dev {
		out["http://localhost:5173"] = true
		out["http://localhost:8080"] = true
	}
	return out
}

// bindDB enables the forms and starts the notification sweeper.
func (m *marketingLeads) bindDB(db *sql.DB) {
	if db == nil || !m.db.CompareAndSwap(nil, db) {
		return
	}
	if m.notifier == nil {
		m.notifier = &sesLeadNotifier{}
	}
	go m.runSweeper(context.Background(), 2*time.Minute)
}

func writeLeadJSON(w http.ResponseWriter, status int, body map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeLeadOK(w http.ResponseWriter) {
	writeLeadJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ── Form token ──────────────────────────────────────────────────────────────

func (m *marketingLeads) tokenSig(ts, nonce string) string {
	h := hmac.New(sha256.New, m.formKey)
	h.Write([]byte(ts + "." + nonce))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *marketingLeads) issueFormToken(now time.Time) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := hex.EncodeToString(b)
	return ts + "." + nonce + "." + m.tokenSig(ts, nonce), nil
}

type formTokenState int

const (
	formTokenOK formTokenState = iota
	formTokenBad
	formTokenFast
	formTokenStale
)

func (m *marketingLeads) checkFormToken(tok string, now time.Time) formTokenState {
	p := strings.Split(tok, ".")
	if len(p) != 3 || subtle.ConstantTimeCompare([]byte(p[2]), []byte(m.tokenSig(p[0], p[1]))) != 1 {
		return formTokenBad
	}
	ts, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil {
		return formTokenBad
	}
	age := now.Sub(time.Unix(ts, 0))
	switch {
	case age < leadTokenMinAge:
		return formTokenFast
	case age > leadTokenMaxAge:
		return formTokenStale
	}
	return formTokenOK
}

// HandleFormToken issues the token the page holds while the visitor types.
func (m *marketingLeads) HandleFormToken(w http.ResponseWriter, r *http.Request) {
	if len(m.formKey) == 0 {
		writeLeadJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "forms unavailable"})
		return
	}
	tok, err := m.issueFormToken(m.now())
	if err != nil {
		writeLeadJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "forms unavailable"})
		return
	}
	writeLeadJSON(w, http.StatusOK, map[string]interface{}{"token": tok})
}

// ── Validation ──────────────────────────────────────────────────────────────

type leadRequest struct {
	FormToken       string `json:"form_token"`
	OnboardingToken string `json:"onboarding_token"`
	Honeypot        string `json:"website"`
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	Company         string `json:"company"`
	Role            string `json:"role"`
	WebsiteURL      string `json:"company_site"`
	Volume          string `json:"volume"`
	UseCase         string `json:"use_case"`
	CurrentPlatform string `json:"current_platform"`
	Notes           string `json:"notes"`
	SourcePage      string `json:"source_page"`
	MarketingOptIn  bool   `json:"marketing_opt_in"`
}

// singleLine trims and refuses control characters, so no field can carry a
// line break into the plain-text notification and fake another field.
func singleLine(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		return "", false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return s, true
}

func multiLine(s string, max int) (string, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len([]rune(s)) > max {
		return "", false
	}
	for _, c := range s {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return "", false
		}
	}
	return s, true
}

func validLeadEmail(s string) bool {
	if len(s) > 254 || !leadEmailRe.MatchString(s) {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s
}

// clean normalizes every field and enforces what each kind requires.
func (q *leadRequest) clean() error {
	var ok bool
	for _, f := range []struct {
		v   *string
		max int
	}{
		{&q.Name, 120}, {&q.Company, 160}, {&q.Role, 120}, {&q.WebsiteURL, 200},
		{&q.Volume, 60}, {&q.UseCase, 120}, {&q.CurrentPlatform, 120}, {&q.SourcePage, 120},
	} {
		if *f.v, ok = singleLine(*f.v, f.max); !ok {
			return errors.New("a field is too long or has invalid characters")
		}
	}
	if q.Notes, ok = multiLine(q.Notes, 2000); !ok {
		return errors.New("your message is too long")
	}
	q.Email = strings.ToLower(strings.TrimSpace(q.Email))
	if !validLeadEmail(q.Email) {
		return errors.New("enter a valid email address")
	}
	if q.SourcePage != "" && !strings.HasPrefix(q.SourcePage, "/") {
		return errors.New("invalid source page")
	}
	need := func(vals ...string) error {
		for _, v := range vals {
			if v == "" {
				return errors.New("fill in the required fields")
			}
		}
		return nil
	}
	switch q.Kind {
	case "early_access":
		return need(q.UseCase)
	case "demo":
		return need(q.Name, q.Company, q.Volume, q.UseCase)
	case "contact":
		return need(q.Name, q.UseCase, q.Notes)
	case "api_access":
		return need(q.Name, q.Company, q.UseCase)
	case "onboarding":
		return need(q.Name, q.Company, q.Volume, q.UseCase)
	}
	return errors.New("unknown request type")
}

// ── Submission ──────────────────────────────────────────────────────────────

// leadClientIP is the right-most X-Forwarded-For entry: the one the load balancer
// appended. chi's RealIP trusts the left-most, which the client controls, so
// it would let a bot walk around the per-IP limit.
func leadClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (m *marketingLeads) ipHash(ip string) string {
	h := hmac.New(sha256.New, m.ipKey)
	h.Write([]byte(ip))
	return hex.EncodeToString(h.Sum(nil))
}

// bump counts a hit in a shared hourly bucket; Postgres keeps the two ECS
// tasks on one count.
func bumpLeadRate(ctx context.Context, db *sql.DB, bucket string) (int, error) {
	var hits int
	err := db.QueryRowContext(ctx, `
		INSERT INTO marketing_lead_rate (bucket, window_start, hits)
		VALUES ($1, date_trunc('hour', NOW()), 1)
		ON CONFLICT (bucket, window_start) DO UPDATE SET hits = marketing_lead_rate.hits + 1
		RETURNING hits`, bucket).Scan(&hits)
	return hits, err
}

var errLeadRateLimited = errors.New("rate limited")

func (m *marketingLeads) checkRate(ctx context.Context, db *sql.DB, ipHash string) error {
	n, err := bumpLeadRate(ctx, db, "ip:"+ipHash)
	if err != nil {
		return err
	}
	if n > leadPerIPHourly {
		return errLeadRateLimited
	}
	n, err = bumpLeadRate(ctx, db, "global")
	if err != nil {
		return err
	}
	if n > leadGlobalHourly {
		return errLeadRateLimited
	}
	return nil
}

func (m *marketingLeads) saveLead(ctx context.Context, db *sql.DB, q *leadRequest, ipHash, ua, subject, dedupe string) error {
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO marketing_leads
			(kind, email, name, company, role, website_url, volume, use_case, current_platform,
			 notes, source_page, marketing_opt_in, google_subject, ip_hash, user_agent, dedupe_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		q.Kind, q.Email, q.Name, q.Company, q.Role, q.WebsiteURL, q.Volume, q.UseCase, q.CurrentPlatform,
		q.Notes, q.SourcePage, q.MarketingOptIn, subject, ipHash, ua, dedupe)
	return err
}

func (m *marketingLeads) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// admit runs the checks shared by both forms. It returns true only when the
// handler should go on to validate and save; otherwise the response is written.
func (m *marketingLeads) admit(w http.ResponseWriter, r *http.Request, q *leadRequest) bool {
	if !m.origins[r.Header.Get("Origin")] {
		log.Printf("[leads] dropped reason=origin")
		writeLeadOK(w)
		return false
	}
	if q.Honeypot != "" {
		log.Printf("[leads] dropped reason=honeypot")
		writeLeadOK(w)
		return false
	}
	switch m.checkFormToken(q.FormToken, m.now()) {
	case formTokenBad:
		log.Printf("[leads] dropped reason=bad_token")
		writeLeadOK(w)
		return false
	case formTokenFast:
		log.Printf("[leads] dropped reason=too_fast")
		writeLeadOK(w)
		return false
	case formTokenStale:
		writeLeadJSON(w, http.StatusConflict, map[string]interface{}{"error": "token_expired"})
		return false
	}
	return true
}

func (m *marketingLeads) decode(w http.ResponseWriter, r *http.Request) (*leadRequest, *sql.DB) {
	db := m.db.Load()
	if db == nil || len(m.formKey) == 0 {
		writeLeadJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "forms unavailable"})
		return nil, nil
	}
	var q leadRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, leadBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		writeLeadJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid request"})
		return nil, nil
	}
	return &q, db
}

// store rate-limits and saves. Every failure is an error response: success is
// only ever reported for a row that is in the table.
func (m *marketingLeads) store(w http.ResponseWriter, r *http.Request, db *sql.DB, q *leadRequest, subject, dedupe string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	ipHash := m.ipHash(leadClientIP(r))
	if err := m.checkRate(ctx, db, ipHash); err != nil {
		if errors.Is(err, errLeadRateLimited) {
			log.Printf("[leads] rate limited kind=%s", q.Kind)
			writeLeadJSON(w, http.StatusTooManyRequests, map[string]interface{}{"error": "too many requests"})
			return
		}
		log.Printf("[leads] rate check failed: %v", err)
		writeLeadJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "could not save"})
		return
	}
	if err := m.saveLead(ctx, db, q, ipHash, r.UserAgent(), subject, dedupe); err != nil {
		log.Printf("[leads] save failed: %v", err)
		writeLeadJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "could not save"})
		return
	}
	m.kick()
	writeLeadOK(w)
}

// HandleLead takes the four public forms. The same email and kind on the same
// day is one lead, so a bot cycling tokens cannot flood the operator's inbox.
func (m *marketingLeads) HandleLead(w http.ResponseWriter, r *http.Request) {
	q, db := m.decode(w, r)
	if q == nil || !m.admit(w, r, q) {
		return
	}
	if q.Kind == "onboarding" {
		writeLeadJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "unknown request type"})
		return
	}
	if err := q.clean(); err != nil {
		writeLeadJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	dedupe := q.Kind + "|" + q.Email + "|" + m.now().UTC().Format("2006-01-02")
	m.store(w, r, db, q, "", dedupe)
}

// HandleOnboarding takes the new-customer form. The Google-derived token
// supplies the email, so a bot needs a real verified account; a valid email
// can submit once, ever. Nothing here grants access to anything.
func (m *marketingLeads) HandleOnboarding(w http.ResponseWriter, r *http.Request) {
	q, db := m.decode(w, r)
	if q == nil {
		return
	}
	if m.verifyOnboarding == nil {
		writeLeadJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "forms unavailable"})
		return
	}
	claims, err := m.verifyOnboarding(q.OnboardingToken, m.now())
	if err != nil {
		writeLeadJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "unauthorized"})
		return
	}
	if !m.admit(w, r, q) {
		return
	}
	q.Kind = "onboarding"
	q.Email = claims.Email
	if err := q.clean(); err != nil {
		writeLeadJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	m.store(w, r, db, q, claims.Subject, "onboarding|"+q.Email)
}

// ── Notification ────────────────────────────────────────────────────────────

type pendingLead struct {
	ID, Kind, Email, Name, Company, Role, WebsiteURL, Volume, UseCase string
	CurrentPlatform, Notes, SourcePage                                string
	OptIn                                                             bool
	CreatedAt                                                         time.Time
}

func (p *pendingLead) body() string {
	var b strings.Builder
	line := func(label, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, v)
		}
	}
	line("Name", p.Name)
	line("Email", p.Email)
	line("Company", p.Company)
	line("Role", p.Role)
	line("Website", p.WebsiteURL)
	line("Monthly email volume", p.Volume)
	line("Primary use", p.UseCase)
	line("Current platform", p.CurrentPlatform)
	line("Product updates", map[bool]string{true: "yes", false: ""}[p.OptIn])
	line("Page", p.SourcePage)
	line("Received", p.CreatedAt.UTC().Format(time.RFC3339))
	if p.Notes != "" {
		b.WriteString("\nNotes:\n" + p.Notes + "\n")
	}
	return b.String()
}

// flush claims unsent leads and sends each. The claim survives a crash: after
// five minutes the row is eligible again, up to leadNotifyAttempts.
func (m *marketingLeads) flush(ctx context.Context) error {
	db := m.db.Load()
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		UPDATE marketing_leads SET notify_attempts = notify_attempts + 1, notify_claimed_at = NOW()
		WHERE id IN (
			SELECT id FROM marketing_leads
			WHERE notified_at IS NULL AND notify_attempts < $1
			  AND (notify_claimed_at IS NULL OR notify_claimed_at < NOW() - INTERVAL '5 minutes')
			ORDER BY created_at LIMIT 10 FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, email, name, company, role, website_url, volume, use_case,
		          current_platform, notes, source_page, marketing_opt_in, created_at`, leadNotifyAttempts)
	if err != nil {
		return err
	}
	var pending []pendingLead
	for rows.Next() {
		var p pendingLead
		if err := rows.Scan(&p.ID, &p.Kind, &p.Email, &p.Name, &p.Company, &p.Role, &p.WebsiteURL, &p.Volume,
			&p.UseCase, &p.CurrentPlatform, &p.Notes, &p.SourcePage, &p.OptIn, &p.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range pending {
		p := &pending[i]
		sendErr := m.notifier.Send(ctx, leadSubjects[p.Kind], p.body())
		if sendErr != nil {
			log.Printf("[leads] notification failed id=%s: %v", p.ID, sendErr)
			msg := sendErr.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
			if _, err := db.ExecContext(ctx, `UPDATE marketing_leads SET notify_error = $2 WHERE id = $1`, p.ID, msg); err != nil {
				log.Printf("[leads] could not record failure id=%s: %v", p.ID, err)
			}
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE marketing_leads SET notified_at = NOW(), notify_error = '' WHERE id = $1`, p.ID); err != nil {
			log.Printf("[leads] could not mark sent id=%s: %v", p.ID, err)
		}
	}
	return nil
}

func (m *marketingLeads) runSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-t.C:
			if db := m.db.Load(); db != nil {
				if _, err := db.ExecContext(ctx, `DELETE FROM marketing_lead_rate WHERE window_start < NOW() - INTERVAL '2 days'`); err != nil {
					log.Printf("[leads] rate cleanup failed: %v", err)
				}
			}
		}
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := m.flush(fctx); err != nil {
			log.Printf("[leads] sweep failed: %v", err)
		}
		cancel()
	}
}

// sesLeadNotifier sends plain text through the SES API.
type sesLeadNotifier struct {
	client atomic.Pointer[sesv2.Client]
}

func (n *sesLeadNotifier) Send(ctx context.Context, subject, body string) error {
	c := n.client.Load()
	if c == nil {
		region := os.Getenv("SES_REGION")
		if region == "" {
			region = "us-west-1"
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return err
		}
		c = sesv2.NewFromConfig(cfg)
		n.client.Store(c)
	}
	_, err := c.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(leadNotifyFrom),
		Destination:      &sestypes.Destination{ToAddresses: []string{leadNotifyTo}},
		Content: &sestypes.EmailContent{Simple: &sestypes.Message{
			Subject: &sestypes.Content{Data: aws.String(subject), Charset: aws.String("UTF-8")},
			Body:    &sestypes.Body{Text: &sestypes.Content{Data: aws.String(body), Charset: aws.String("UTF-8")}},
		}},
	})
	return err
}

// ── Admin ───────────────────────────────────────────────────────────────────

// HandleAdminList shows recent leads, including any whose notification never
// arrived. Admin key only; the IP hash and user agent stay in the table.
func (m *marketingLeads) HandleAdminList(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("ADMIN_API_KEY")
	if key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(key)) != 1 {
		writeLeadJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "unauthorized"})
		return
	}
	db := m.db.Load()
	if db == nil {
		writeLeadJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "forms unavailable"})
		return
	}
	rows, err := db.QueryContext(r.Context(), `
		SELECT id, kind, email, name, company, volume, use_case, notes, source_page,
		       created_at, notified_at, notify_attempts, notify_error
		FROM marketing_leads ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		writeLeadJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "could not read leads"})
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, kind, email, name, company, volume, useCase, notes, page, notifyErr string
		var created time.Time
		var notified sql.NullTime
		var attempts int
		if err := rows.Scan(&id, &kind, &email, &name, &company, &volume, &useCase, &notes, &page,
			&created, &notified, &attempts, &notifyErr); err != nil {
			writeLeadJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "could not read leads"})
			return
		}
		row := map[string]interface{}{
			"id": id, "kind": kind, "email": email, "name": name, "company": company, "volume": volume,
			"use_case": useCase, "notes": notes, "source_page": page, "created_at": created,
			"notify_attempts": attempts, "notify_error": notifyErr,
		}
		if notified.Valid {
			row["notified_at"] = notified.Time
		}
		out = append(out, row)
	}
	writeLeadJSON(w, http.StatusOK, map[string]interface{}{"leads": out})
}

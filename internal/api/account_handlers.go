package api

// Account — the portal's profile, company, subscription, users and provider-key
// screen, all under /api/mailing/account.
//
//	GET    /me                  signed-in user + role; fills the roster
//	GET    /company             company details
//	PUT    /company             rename (owner/admin)
//	GET    /subscription        plan, limits, this month's sends
//	GET    /users               roster
//	POST   /users               add a user (owner/admin)
//	PATCH  /users/{id}          change role or status (owner/admin)
//	GET    /keys                provider keys, tail only
//	PUT    /keys/{name}         replace a provider key (owner/admin)
//	DELETE /keys/{name}         go back to the deployment setting (owner/admin)
//
// IDENTITY comes from the server-side session, never from X-User-Email: the
// /api middleware rewrites that header on the session branch but passes it
// through on the admin-key branch (routes.go apiAuthMiddleware), so here it
// would let an admin-key caller name any user. An admin-key request has no
// session and gets 401 on every route.
//
// ROLES are read from `users` per organization on every write, so naming
// another organization in X-Organization-ID grants nothing. They govern this
// screen only; the rest of the portal has no role enforcement.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/auth"
	"github.com/lib/pq"
)

const (
	accountRoleOwner  = "owner"
	accountRoleAdmin  = "admin"
	accountRoleUser   = "user"
	accountRoleViewer = "viewer"

	accountStatusActive   = "active"
	accountStatusDisabled = "disabled"

	accountMaxBody = 16 << 10
	accountTimeout = 5 * time.Second
)

// AccountService owns the Account screen.
type AccountService struct {
	db           *sql.DB
	session      func(*http.Request) *auth.Session
	emailAllowed func(string) bool
	now          func() time.Time
	access       *accessCache

	// keyMu serializes key saves against the background refresh, so a refresh
	// that read the table before a save cannot put the old value back.
	keyMu   sync.Mutex
	keyEnc  []byte
	applier *providerKeyApplier
}

// NewAccountService wires the service. am may be nil when auth is disabled:
// every route then answers 401 and keys cannot be saved.
func NewAccountService(db *sql.DB, am *auth.AuthManager) *AccountService {
	s := &AccountService{
		db:           db,
		session:      func(*http.Request) *auth.Session { return nil },
		emailAllowed: func(string) bool { return false },
		now:          time.Now,
		access:       newAccessCache(),
	}
	if am != nil {
		s.session = am.GetSession
		s.emailAllowed = am.EmailAllowed
		s.keyEnc = am.DerivedKey(providerKeyPurpose)
	}
	s.applier = newProviderKeyApplier(db, s.keyEnc, platformOrgID())
	return s
}

// RegisterRoutes mounts the account routes on the /api/mailing group.
func (s *AccountService) RegisterRoutes(r chi.Router) {
	r.Route("/account", func(r chi.Router) {
		r.Get("/me", s.HandleMe)
		r.Get("/company", s.HandleGetCompany)
		r.Put("/company", s.HandleUpdateCompany)
		r.Get("/subscription", s.HandleSubscription)
		r.Get("/users", s.HandleListUsers)
		r.Post("/users", s.HandleCreateUser)
		r.Patch("/users/{id}", s.HandleUpdateUser)
		r.Get("/keys", s.HandleListKeys)
		r.Put("/keys/{name}", s.HandleSaveKey)
		r.Delete("/keys/{name}", s.HandleDeleteKey)
	})
}

// accountCaller is the signed-in user behind a request.
type accountCaller struct {
	OrgID   uuid.UUID
	Session *auth.Session
	Email   string
	Role    string
	Status  string
}

func (c *accountCaller) canManage() bool {
	return c.Status == accountStatusActive && (c.Role == accountRoleOwner || c.Role == accountRoleAdmin)
}

func normalizeAccountEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validAccountRole(role string) bool {
	switch role {
	case accountRoleOwner, accountRoleAdmin, accountRoleUser, accountRoleViewer:
		return true
	}
	return false
}

// caller resolves session and organization, answering 401/400 itself.
func (s *AccountService) caller(w http.ResponseWriter, r *http.Request) (*accountCaller, bool) {
	session := s.session(r)
	if session == nil || session.Email == "" {
		respondError(w, http.StatusUnauthorized, "Please sign in again.")
		return nil, false
	}
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "No organization found for this request.")
		return nil, false
	}
	return &accountCaller{OrgID: orgID, Session: session, Email: normalizeAccountEmail(session.Email)}, true
}

// loadRole fills in the caller's role. A user with no row is a plain user.
func (s *AccountService) loadRole(ctx context.Context, c *accountCaller) error {
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(role, 'user'), COALESCE(status, 'active')
		  FROM users WHERE organization_id = $1 AND email = $2`,
		c.OrgID, c.Email).Scan(&c.Role, &c.Status)
	if errors.Is(err, sql.ErrNoRows) {
		c.Role, c.Status = accountRoleUser, accountStatusActive
		return nil
	}
	return err
}

// requireManager admits an active owner or admin.
func (s *AccountService) requireManager(w http.ResponseWriter, r *http.Request) (*accountCaller, bool) {
	c, ok := s.caller(w, r)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	if err := s.loadRole(ctx, c); err != nil {
		log.Printf("[Account] role lookup failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't check your role. Please try again.")
		return nil, false
	}
	if !c.canManage() {
		respondError(w, http.StatusForbidden, "Only an owner or admin can do this.")
		return nil, false
	}
	return c, true
}

func decodeAccountBody(w http.ResponseWriter, r *http.Request, into interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, accountMaxBody)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		respondError(w, http.StatusBadRequest, "That request couldn't be read.")
		return false
	}
	return true
}

// ─── Profile ────────────────────────────────────────────────────────────────

type accountProfile struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Picture    string     `json:"picture"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	LastSignIn *time.Time `json:"last_sign_in"`
	CanManage  bool       `json:"can_manage"`
}

// HandleMe returns the session user and records them on the roster. Last
// sign-in is when the session was created, not this request.
func (s *AccountService) HandleMe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()

	var (
		p          accountProfile
		name, pic  sql.NullString
		lastSignIn sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (organization_id, email, name, google_id, avatar_url, last_login_at, role, status)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, 'user', 'active')
		ON CONFLICT (organization_id, email) DO UPDATE SET
			name          = COALESCE(EXCLUDED.name, users.name),
			google_id     = COALESCE(EXCLUDED.google_id, users.google_id),
			avatar_url    = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
			last_login_at = GREATEST(users.last_login_at, EXCLUDED.last_login_at),
			updated_at    = NOW()
		RETURNING id::text, email, name, avatar_url, COALESCE(role, 'user'), COALESCE(status, 'active'), last_login_at`,
		c.OrgID, c.Email, c.Session.Name, c.Session.UserID, c.Session.Picture, c.Session.CreatedAt,
	).Scan(&p.ID, &p.Email, &name, &pic, &p.Role, &p.Status, &lastSignIn)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23503" {
			respondError(w, http.StatusNotFound, "Organization not found.")
			return
		}
		log.Printf("[Account] profile upsert failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load your profile.")
		return
	}
	p.Name, p.Picture = name.String, pic.String
	if lastSignIn.Valid {
		p.LastSignIn = &lastSignIn.Time
	}
	c.Role, c.Status = p.Role, p.Status
	p.CanManage = c.canManage()
	respondJSON(w, http.StatusOK, p)
}

// ─── Company ────────────────────────────────────────────────────────────────

type accountCompany struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Plan         string `json:"plan"`
	Status       string `json:"status"`
	DailyLimit   int64  `json:"daily_limit"`
	MonthlyLimit int64  `json:"monthly_limit"`
}

func (s *AccountService) loadCompany(ctx context.Context, orgID uuid.UUID) (*accountCompany, error) {
	var c accountCompany
	err := s.db.QueryRowContext(ctx, `
		SELECT name, slug, COALESCE(plan, ''), COALESCE(status, ''),
		       COALESCE(daily_email_limit, 0), COALESCE(monthly_email_limit, 0)
		  FROM organizations WHERE id = $1`, orgID).
		Scan(&c.Name, &c.Slug, &c.Plan, &c.Status, &c.DailyLimit, &c.MonthlyLimit)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// HandleGetCompany returns the caller's organization.
func (s *AccountService) HandleGetCompany(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	company, err := s.loadCompany(ctx, c.OrgID)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "Organization not found.")
		return
	}
	if err != nil {
		log.Printf("[Account] company read failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load the company.")
		return
	}
	respondJSON(w, http.StatusOK, company)
}

// HandleUpdateCompany renames the organization. Nothing else on it is editable.
func (s *AccountService) HandleUpdateCompany(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireManager(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeAccountBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 255 {
		respondError(w, http.StatusBadRequest, "Company name must be between 1 and 255 characters.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE organizations SET name = $1, updated_at = NOW() WHERE id = $2`, name, c.OrgID)
	if err != nil {
		log.Printf("[Account] company update failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't save the company name.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "Organization not found.")
		return
	}
	log.Printf("[Account] company renamed by %s", c.Email)
	company, err := s.loadCompany(ctx, c.OrgID)
	if err != nil {
		log.Printf("[Account] company reread failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Saved, but couldn't reload the company.")
		return
	}
	respondJSON(w, http.StatusOK, company)
}

// ─── Subscription ───────────────────────────────────────────────────────────

type accountSubscription struct {
	Plan             string     `json:"plan"`
	Status           string     `json:"status"`
	DailyLimit       int64      `json:"daily_limit"`
	MonthlyLimit     int64      `json:"monthly_limit"`
	Month            string     `json:"month"`
	SentThisMonth    *int64     `json:"sent_this_month"`
	SentAsOf         *time.Time `json:"sent_as_of"`
	BillingConnected bool       `json:"billing_connected"`
}

// HandleSubscription returns the plan and this month's sends. Sent is attempted
// mail as METRIC_CONTRACT derives it (delivered + hard + soft) from
// mailing_growth_daily, a bounded indexed read by day on a table that is not
// split by organization (one tenant). It is null, not zero, if the read fails.
// There is no billing system, so no price, renewal date or payment method.
func (s *AccountService) HandleSubscription(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	company, err := s.loadCompany(ctx, c.OrgID)
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusNotFound, "Organization not found.")
		return
	}
	if err != nil {
		log.Printf("[Account] subscription read failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load the subscription.")
		return
	}

	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	now := s.now().In(loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)

	out := accountSubscription{
		Plan: company.Plan, Status: company.Status,
		DailyLimit: company.DailyLimit, MonthlyLimit: company.MonthlyLimit,
		Month: monthStart.Format("2006-01"),
	}
	var (
		sent int64
		asOf sql.NullTime
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(delivered + hard_bounce + soft_bounce), 0), MAX(computed_at)
		  FROM mailing_growth_daily
		 WHERE day >= $1::date AND day <= $2::date`,
		monthStart.Format("2006-01-02"), now.Format("2006-01-02")).Scan(&sent, &asOf)
	if err != nil {
		log.Printf("[Account] monthly usage read failed: %v", err)
	} else {
		out.SentThisMonth = &sent
		if asOf.Valid {
			out.SentAsOf = &asOf.Time
		}
	}
	respondJSON(w, http.StatusOK, out)
}

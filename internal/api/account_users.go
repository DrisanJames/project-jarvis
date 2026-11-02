package api

// Account users — the roster behind Account > Users. A row here does not grant
// sign-in (the domain rule does that); it carries the role and, through the
// access gate, can take sign-in away.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type accountUser struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Picture    string     `json:"picture"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	LastSignIn *time.Time `json:"last_sign_in"`
	CreatedAt  *time.Time `json:"created_at"`
}

const accountUserColumns = `id::text, email, name, avatar_url, COALESCE(role, 'user'),
	COALESCE(status, 'active'), last_login_at, created_at`

func scanAccountUser(row interface{ Scan(...interface{}) error }) (accountUser, error) {
	var (
		u             accountUser
		name, pic     sql.NullString
		last, created sql.NullTime
	)
	if err := row.Scan(&u.ID, &u.Email, &name, &pic, &u.Role, &u.Status, &last, &created); err != nil {
		return u, err
	}
	u.Name, u.Picture = name.String, pic.String
	if last.Valid {
		u.LastSignIn = &last.Time
	}
	if created.Valid {
		u.CreatedAt = &created.Time
	}
	return u, nil
}

// HandleListUsers returns the organization's roster.
func (s *AccountService) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+accountUserColumns+`
		  FROM users WHERE organization_id = $1
		 ORDER BY email LIMIT 500`, c.OrgID)
	if err != nil {
		log.Printf("[Account] users read failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load users.")
		return
	}
	defer rows.Close()
	users := []accountUser{}
	for rows.Next() {
		u, err := scanAccountUser(rows)
		if err != nil {
			log.Printf("[Account] users scan failed: %v", err)
			respondError(w, http.StatusInternalServerError, "Couldn't load users.")
			return
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Account] users read failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't load users.")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

// HandleCreateUser adds a user. The address must pass the sign-in domain rule.
func (s *AccountService) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireManager(w, r)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if !decodeAccountBody(w, r, &body) {
		return
	}
	email := normalizeAccountEmail(body.Email)
	name := strings.TrimSpace(body.Name)
	role := strings.TrimSpace(body.Role)
	if role == "" {
		role = accountRoleUser
	}
	if !validAccountRole(role) {
		respondError(w, http.StatusBadRequest, "Choose owner, admin, user or viewer.")
		return
	}
	if len(email) > 255 || strings.ContainsAny(email, " \t\r\n") || !s.emailAllowed(email) {
		respondError(w, http.StatusBadRequest, "That address can't sign in here. Use a company email address.")
		return
	}
	if utf8.RuneCountInString(name) > 255 {
		respondError(w, http.StatusBadRequest, "Name must be 255 characters or fewer.")
		return
	}
	if role == accountRoleOwner && c.Role != accountRoleOwner {
		respondError(w, http.StatusForbidden, "Only an owner can add another owner.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	u, err := scanAccountUser(s.db.QueryRowContext(ctx, `
		INSERT INTO users (organization_id, email, name, role, status)
		VALUES ($1, $2, NULLIF($3, ''), $4, 'active')
		ON CONFLICT (organization_id, email) DO NOTHING
		RETURNING `+accountUserColumns, c.OrgID, email, name, role))
	if errors.Is(err, sql.ErrNoRows) {
		respondError(w, http.StatusConflict, "That user is already on the list.")
		return
	}
	if err != nil {
		log.Printf("[Account] user create failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't add that user.")
		return
	}
	log.Printf("[Account] user %s added as %s by %s", email, role, c.Email)
	respondJSON(w, http.StatusCreated, u)
}

// errAccountUserRefused carries a refusal the handler answers verbatim.
type errAccountUserRefused struct {
	status  int
	message string
}

func (e *errAccountUserRefused) Error() string { return e.message }

// HandleUpdateUser changes a user's role or status. Only an owner can touch an
// owner, or an admin could promote themselves past every owner check here. The
// active-owner rows stay locked for the transaction, so two simultaneous
// demotions cannot leave the organization with no owner.
func (s *AccountService) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireManager(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusNotFound, "User not found.")
		return
	}
	var body struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
	}
	if !decodeAccountBody(w, r, &body) {
		return
	}
	if body.Role == nil && body.Status == nil {
		respondError(w, http.StatusBadRequest, "Nothing to change.")
		return
	}
	if body.Role != nil && !validAccountRole(*body.Role) {
		respondError(w, http.StatusBadRequest, "Choose owner, admin, user or viewer.")
		return
	}
	if body.Status != nil && *body.Status != accountStatusActive && *body.Status != accountStatusDisabled {
		respondError(w, http.StatusBadRequest, "Status must be active or disabled.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	u, err := s.updateUser(ctx, c, id, body.Role, body.Status)
	var refused *errAccountUserRefused
	if errors.As(err, &refused) {
		respondError(w, refused.status, refused.message)
		return
	}
	if err != nil {
		log.Printf("[Account] user update failed: %v", err)
		respondError(w, http.StatusInternalServerError, "Couldn't save that change.")
		return
	}
	s.access.forget(u.Email)
	log.Printf("[Account] user %s set to role=%s status=%s by %s", u.Email, u.Role, u.Status, c.Email)
	respondJSON(w, http.StatusOK, u)
}

func (s *AccountService) updateUser(ctx context.Context, c *accountCaller, id uuid.UUID, role, status *string) (accountUser, error) {
	var u accountUser
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return u, err
	}
	defer tx.Rollback()

	owners, err := tx.QueryContext(ctx, `
		SELECT id FROM users
		 WHERE organization_id = $1 AND role = 'owner' AND COALESCE(status, 'active') = 'active'
		 ORDER BY id FOR UPDATE`, c.OrgID)
	if err != nil {
		return u, err
	}
	activeOwners := map[uuid.UUID]bool{}
	for owners.Next() {
		var ownerID uuid.UUID
		if err := owners.Scan(&ownerID); err != nil {
			owners.Close()
			return u, err
		}
		activeOwners[ownerID] = true
	}
	if err := owners.Close(); err != nil {
		return u, err
	}
	if err := owners.Err(); err != nil {
		return u, err
	}

	var curRole, curStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(role, 'user'), COALESCE(status, 'active')
		  FROM users WHERE id = $1 AND organization_id = $2 FOR UPDATE`, id, c.OrgID).
		Scan(&curRole, &curStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return u, &errAccountUserRefused{http.StatusNotFound, "User not found."}
	}
	if err != nil {
		return u, err
	}

	newRole, newStatus := curRole, curStatus
	if role != nil {
		newRole = *role
	}
	if status != nil {
		newStatus = *status
	}
	if (curRole == accountRoleOwner || newRole == accountRoleOwner) &&
		c.Role != accountRoleOwner && (newRole != curRole || newStatus != curStatus) {
		return u, &errAccountUserRefused{http.StatusForbidden, "Only an owner can change an owner."}
	}
	losesOwner := activeOwners[id] && (newRole != accountRoleOwner || newStatus != accountStatusActive)
	if losesOwner && len(activeOwners) <= 1 {
		return u, &errAccountUserRefused{http.StatusConflict,
			"This is the last active owner. Make someone else an owner first."}
	}

	u, err = scanAccountUser(tx.QueryRowContext(ctx, `
		UPDATE users SET role = $1, status = $2, updated_at = NOW()
		 WHERE id = $3 AND organization_id = $4
		RETURNING `+accountUserColumns, newRole, newStatus, id, c.OrgID))
	if err != nil {
		return u, err
	}
	return u, tx.Commit()
}

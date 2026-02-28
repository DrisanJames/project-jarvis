package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleGetLists returns all lists
func (svc *MailingService) HandleGetLists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := svc.db.QueryContext(ctx, `
		SELECT id, name, description, subscriber_count, status, created_at 
		FROM mailing_lists ORDER BY created_at DESC
	`)
	defer rows.Close()

	var lists []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, desc, status string
		var subCount int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &subCount, &status, &createdAt)
		lists = append(lists, map[string]interface{}{
			"id": id.String(), "name": name, "description": desc,
			"subscriber_count": subCount, "status": status, "created_at": createdAt,
		})
	}
	if lists == nil {
		lists = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"lists": lists, "total": len(lists)})
}

// HandleCreateList creates a new list
func (svc *MailingService) HandleCreateList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	id := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}

	_, err = svc.db.ExecContext(ctx, `
		INSERT INTO mailing_lists (id, organization_id, name, description, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'active', NOW(), NOW())
	`, id, orgID, input.Name, input.Description)

	if err != nil {
		http.Error(w, `{"error":"failed to create list"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "name": input.Name, "description": input.Description, "status": "active",
	})
}

// HandleGetSubscribers returns subscribers for a list with pagination
func (svc *MailingService) HandleGetSubscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	pag := ParsePagination(r, 50, 200)

	// Get total count for this list
	var total int64
	if err := svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1
	`, listID).Scan(&total); err != nil {
		http.Error(w, `{"error":"failed to count subscribers"}`, http.StatusInternalServerError)
		return
	}

	rows, err := svc.db.QueryContext(ctx, `
		SELECT id, email, first_name, last_name, status, engagement_score, created_at
		FROM mailing_subscribers WHERE list_id = $1 ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, listID, pag.Limit, pag.Offset)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch subscribers"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var subs []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var email, fname, lname, status string
		var score float64
		var createdAt time.Time
		rows.Scan(&id, &email, &fname, &lname, &status, &score, &createdAt)
		subs = append(subs, map[string]interface{}{
			"id": id.String(), "email": email, "first_name": fname, "last_name": lname,
			"status": status, "engagement_score": score, "created_at": createdAt,
		})
	}
	if subs == nil {
		subs = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(NewPaginatedResponse(subs, pag, total))
}

// HandleAddSubscriber adds a subscriber to a list
func (svc *MailingService) HandleAddSubscriber(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")

	var input struct {
		Email     string `json:"email"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	email := strings.ToLower(strings.TrimSpace(input.Email))

	// Check suppression
	var suppressed bool
	svc.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)",
		email).Scan(&suppressed)

	if suppressed {
		http.Error(w, `{"error":"email is suppressed"}`, http.StatusBadRequest)
		return
	}

	id := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	listUUID, _ := uuid.Parse(listID)
	
	// Generate email hash
	h := sha256.Sum256([]byte(email))
	emailHash := hex.EncodeToString(h[:])

	_, err = svc.db.ExecContext(ctx, `
		INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash, first_name, last_name, status, engagement_score, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'confirmed', 50.0, NOW(), NOW())
		ON CONFLICT (list_id, email) DO UPDATE SET first_name = $6, last_name = $7, status = 'confirmed', updated_at = NOW()
	`, id, orgID, listUUID, email, emailHash, input.FirstName, input.LastName)

	if err != nil {
		log.Printf("Error adding subscriber: %v", err)
		http.Error(w, `{"error":"failed to add subscriber"}`, http.StatusInternalServerError)
		return
	}

	// Update list count
	svc.db.ExecContext(ctx, "UPDATE mailing_lists SET subscriber_count = subscriber_count + 1 WHERE id = $1", listUUID)

	// Create inbox profile
	parts := strings.Split(email, "@")
	domain := ""
	if len(parts) == 2 {
		domain = parts[1]
	}
	svc.db.ExecContext(ctx, `
		INSERT INTO mailing_inbox_profiles (id, email, domain, engagement_score, created_at, updated_at)
		VALUES ($1, $2, $3, 50.0, NOW(), NOW())
		ON CONFLICT (email) DO NOTHING
	`, uuid.New(), email, domain)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "email": email, "status": "confirmed",
	})
}

// HandleGetSuppressions returns suppression list
func (svc *MailingService) HandleGetSuppressions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := svc.db.QueryContext(ctx, `
		SELECT id, email, reason, source, created_at FROM mailing_suppressions WHERE active = true ORDER BY created_at DESC
	`)
	defer rows.Close()

	var supps []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var email, reason, source string
		var createdAt time.Time
		rows.Scan(&id, &email, &reason, &source, &createdAt)
		supps = append(supps, map[string]interface{}{
			"id": id.String(), "email": email, "reason": reason, "source": source, "created_at": createdAt,
		})
	}
	if supps == nil {
		supps = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"suppressions": supps, "total": len(supps)})
}

// HandleAddSuppression adds email to suppression
func (svc *MailingService) HandleAddSuppression(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Email  string `json:"email"`
		Reason string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	id := uuid.New()
	_, err := svc.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at)
		VALUES ($1, $2, $3, 'manual', true, NOW())
		ON CONFLICT (email) DO UPDATE SET reason = $3, active = true, updated_at = NOW()
	`, id, strings.ToLower(input.Email), input.Reason)

	if err != nil {
		http.Error(w, `{"error":"failed to add suppression"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "email": input.Email, "reason": input.Reason, "status": "suppressed",
	})
}

// HandleRemoveSuppression removes email from suppression
func (svc *MailingService) HandleRemoveSuppression(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := chi.URLParam(r, "email")

	svc.db.ExecContext(ctx, "UPDATE mailing_suppressions SET active = false, updated_at = NOW() WHERE email = $1", strings.ToLower(email))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"removed": email})
}

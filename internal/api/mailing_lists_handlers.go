package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleGetLists returns all lists with per-list mailing stats
func (svc *MailingService) HandleGetLists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := svc.db.QueryContext(ctx, `
		WITH list_sends AS (
			SELECT s.list_id, COUNT(DISTINCT q.subscriber_id) as mailed_count
			FROM mailing_campaign_queue q
			JOIN mailing_subscribers s ON q.subscriber_id = s.id
			WHERE q.status = 'sent'
			GROUP BY s.list_id
		),
		list_events AS (
			SELECT s.list_id,
				COUNT(DISTINCT CASE WHEN te.event_type = 'open' THEN te.subscriber_id END) as unique_opens,
				COUNT(DISTINCT CASE WHEN te.event_type = 'click' THEN te.subscriber_id END) as unique_clicks,
				COUNT(DISTINCT CASE WHEN te.event_type = 'complaint' THEN te.subscriber_id END) as unique_complaints
			FROM mailing_tracking_events te
			JOIN mailing_subscribers s ON te.subscriber_id = s.id
			GROUP BY s.list_id
		)
		SELECT l.id, l.name, l.description, l.subscriber_count,
			(SELECT COUNT(*) FROM mailing_subscribers sub
			 WHERE sub.list_id = l.id AND sub.status = 'confirmed') as active_count,
			l.status, l.created_at,
			COALESCE(ls.mailed_count, 0) as mailed_to_count,
			COALESCE(le.unique_opens, 0) as unique_opens,
			COALESCE(le.unique_clicks, 0) as unique_clicks,
			COALESCE(le.unique_complaints, 0) as unique_complaints
		FROM mailing_lists l
		LEFT JOIN list_sends ls ON ls.list_id = l.id
		LEFT JOIN list_events le ON le.list_id = l.id
		ORDER BY l.created_at DESC
	`)
	if err != nil {
		log.Printf("[HandleGetLists] query error: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "database temporarily unavailable"})
		return
	}
	defer rows.Close()

	var lists []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var name, desc, status string
		var subCount, activeCount, mailedTo, uniqueOpens, uniqueClicks, uniqueComplaints int
		var createdAt time.Time
		rows.Scan(&id, &name, &desc, &subCount, &activeCount, &status, &createdAt,
			&mailedTo, &uniqueOpens, &uniqueClicks, &uniqueComplaints)

		openPct := 0.0
		clickPct := 0.0
		complaintPct := 0.0
		if mailedTo > 0 {
			openPct = math.Round(float64(uniqueOpens)/float64(mailedTo)*10000) / 100
			clickPct = math.Round(float64(uniqueClicks)/float64(mailedTo)*10000) / 100
			complaintPct = math.Round(float64(uniqueComplaints)/float64(mailedTo)*10000) / 100
		}

		lists = append(lists, map[string]interface{}{
			"id": id.String(), "name": name, "description": desc,
			"subscriber_count": subCount, "active_count": activeCount,
			"status": status, "created_at": createdAt,
			"mailed_to_count": mailedTo,
			"open_pct":        openPct,
			"click_pct":       clickPct,
			"complaint_pct":   complaintPct,
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
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to add subscriber: " + err.Error()})
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
		INSERT INTO mailing_inbox_profiles (id, email_hash, email, domain, engagement_score, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0.50, NOW(), NOW())
		ON CONFLICT (email_hash) DO UPDATE SET email = EXCLUDED.email
	`, uuid.New(), emailHash, email, domain)

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

// HandleListActivity returns subscriber activity stats using lightweight queries.
// Uses data_import_log for recent import counts to avoid scanning mailing_subscribers.
func (svc *MailingService) HandleListActivity(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var new24h, new7d int
	// Use import log for recent subscriber counts (avoids full table scan)
	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(record_count), 0)::int FROM data_import_log
		WHERE status = 'completed' AND processed_at > NOW() - INTERVAL '24 hours'
	`).Scan(&new24h)

	svc.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(record_count), 0)::int FROM data_import_log
		WHERE status = 'completed' AND processed_at > NOW() - INTERVAL '7 days'
	`).Scan(&new7d)

	// Unsubscribes from tracking events (lightweight)
	var unsubs7d int
	svc.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_tracking_events
		WHERE event_type = 'unsubscribe' AND created_at > NOW() - INTERVAL '7 days'
	`).Scan(&unsubs7d)

	type activityItem struct {
		Action    string    `json:"action"`
		Details   string    `json:"details"`
		Timestamp time.Time `json:"timestamp"`
	}
	var activity []activityItem

	// Recent imports as activity (fast — small table)
	rows, err := svc.db.QueryContext(ctx, `
		SELECT classification, original_key, record_count, COALESCE(processed_at, started_at, created_at)
		FROM data_import_log
		ORDER BY created_at DESC LIMIT 10
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var class, key string
			var count int
			var ts time.Time
			rows.Scan(&class, &key, &count, &ts)
			activity = append(activity, activityItem{
				Action:    "import_" + class,
				Details:   fmt.Sprintf("%s (%d records)", key, count),
				Timestamp: ts,
			})
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"new_subscribers_24h": new24h,
		"new_subscribers_7d":  new7d,
		"unsubscribes_7d":    unsubs7d,
		"recent_activity":    activity,
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

// HandlePatchSubscriber updates a subscriber's custom fields by email.
func (svc *MailingService) HandlePatchSubscriber(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	emailParam := chi.URLParam(r, "email")
	email := strings.ToLower(strings.TrimSpace(emailParam))

	var input struct {
		Name         string                 `json:"name"`
		FirstName    string                 `json:"first_name"`
		LastName     string                 `json:"last_name"`
		Status       string                 `json:"status"`
		CustomFields map[string]interface{} `json:"custom_fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	if input.FirstName == "" && input.Name != "" {
		parts := strings.SplitN(input.Name, " ", 2)
		input.FirstName = parts[0]
		if len(parts) > 1 {
			input.LastName = parts[1]
		}
	}

	if input.FirstName != "" {
		svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET first_name = $1, updated_at = NOW() WHERE list_id = $2 AND LOWER(email) = $3`,
			input.FirstName, listID, email)
	}
	if input.LastName != "" {
		svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET last_name = $1, updated_at = NOW() WHERE list_id = $2 AND LOWER(email) = $3`,
			input.LastName, listID, email)
	}
	if input.Status != "" {
		svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET status = $1, updated_at = NOW() WHERE list_id = $2 AND LOWER(email) = $3`,
			input.Status, listID, email)
	}
	if len(input.CustomFields) > 0 {
		cfJSON, _ := json.Marshal(input.CustomFields)
		svc.db.ExecContext(ctx, `UPDATE mailing_subscribers SET custom_fields = COALESCE(custom_fields, '{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE list_id = $2 AND LOWER(email) = $3`,
			string(cfJSON), listID, email)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email})
}

// HandleDeleteSubscriber removes a subscriber by email from a list.
func (svc *MailingService) HandleDeleteSubscriber(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	emailParam := chi.URLParam(r, "email")
	email := strings.ToLower(strings.TrimSpace(emailParam))

	result, err := svc.db.ExecContext(ctx, `DELETE FROM mailing_subscribers WHERE list_id = $1 AND LOWER(email) = $2`, listID, email)
	if err != nil {
		http.Error(w, `{"error":"failed to delete subscriber"}`, http.StatusInternalServerError)
		return
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		svc.db.ExecContext(ctx, "UPDATE mailing_lists SET subscriber_count = GREATEST(subscriber_count - 1, 0) WHERE id = $1", listID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "email": email, "deleted": rows > 0})
}

package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

// =============================================================================
// MANUAL OFFER SUPPRESSION UPLOAD
// Operator-facing endpoints to add/list/remove individual addresses on an
// offer's suppression list (mailing_offer_suppressions). Complements the
// automated paths (Optizmo scrub, Everflow conversion postback, fatigue
// worker) which were previously the only writers to that table.
//
// Enforcement: the send worker always verifies mailing_offer_suppressions in
// the DB (send_worker.go), so a DB insert here is durably enforced at send
// time. For planning-time exclusion we also push the hashes into the offer's
// in-memory Bloom filter when one is loaded.
// =============================================================================

// maxManualOfferSuppressions caps a single request; the frontend batches
// larger uploads.
const maxManualOfferSuppressions = 50000

type OfferSuppressionUploadHandlers struct {
	db      *sql.DB
	suppMgr *OfferSuppressionManager
}

// HandleAddOfferSuppressions adds one or more email addresses to an offer's
// suppression list. Emails are matched against mailing_subscribers (the only
// population that can ever be mailed); unmatched emails are reported back.
// POST /offer-center/offers/{id}/suppressions
// Body: {"emails": ["a@b.com", ...], "reason": "manual", "source": "manual_upload"}
func (h *OfferSuppressionUploadHandlers) HandleAddOfferSuppressions(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")

	var input struct {
		Emails []string `json:"emails"`
		Reason string   `json:"reason"`
		Source string   `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if input.Reason == "" {
		input.Reason = "manual"
	}
	if input.Source == "" {
		input.Source = "manual_upload"
	}

	// Normalize + dedupe.
	seen := make(map[string]bool, len(input.Emails))
	emails := make([]string, 0, len(input.Emails))
	invalid := 0
	for _, e := range input.Emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		if !strings.Contains(e, "@") || strings.ContainsAny(e, " \t") {
			invalid++
			continue
		}
		seen[e] = true
		emails = append(emails, e)
	}
	if len(emails) == 0 {
		http.Error(w, `{"error":"no valid emails provided"}`, http.StatusBadRequest)
		return
	}
	if len(emails) > maxManualOfferSuppressions {
		http.Error(w, `{"error":"too many emails in one request (max 50000) — split into batches"}`, http.StatusBadRequest)
		return
	}

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"could not resolve organization"}`, http.StatusUnauthorized)
		return
	}

	var offerExists bool
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM mailing_offers WHERE id = $1)`, offerID).Scan(&offerExists); err != nil || !offerExists {
		http.Error(w, `{"error":"offer not found"}`, http.StatusNotFound)
		return
	}

	// Detached context: until idx_subscribers_lower_email is built this match
	// is a seq scan that can outlive both the 30s connection-level
	// statement_timeout and the load balancer — raise the budget for this
	// transaction only and don't let a client disconnect roll back the work.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, `{"error":"begin tx failed"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '280s'`); err != nil {
		log.Printf("[OfferSupp] could not raise statement_timeout: %v", err)
	}

	// Single pass over mailing_subscribers: resolve matches and insert in one
	// statement. Already-suppressed pairs are no-ops via the
	// (offer_id, subscriber_id) unique index.
	var added int64
	var matchedEmails []string
	err = tx.QueryRowContext(ctx,
		`WITH matched AS (
			SELECT s.id, LOWER(TRIM(s.email)) AS email
			FROM mailing_subscribers s
			WHERE s.organization_id = $1 AND LOWER(TRIM(s.email)) = ANY($5)
		), ins AS (
			INSERT INTO mailing_offer_suppressions
				(organization_id, offer_id, subscriber_id, email_hash, reason, source, suppressed_at)
			SELECT $1, $2, m.id, MD5(m.email), $3, $4, NOW()
			FROM matched m
			ON CONFLICT (offer_id, subscriber_id) DO NOTHING
			RETURNING 1
		)
		SELECT (SELECT COUNT(*) FROM ins),
		       COALESCE((SELECT array_agg(DISTINCT email) FROM matched), '{}')`,
		orgID, offerID, input.Reason, input.Source, pq.Array(emails)).Scan(&added, pq.Array(&matchedEmails))
	if err != nil {
		log.Printf("[OfferSupp] manual add failed for offer %s: %v", offerID, err)
		http.Error(w, `{"error":"insert failed"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[OfferSupp] manual add commit failed for offer %s: %v", offerID, err)
		http.Error(w, `{"error":"commit failed"}`, http.StatusInternalServerError)
		return
	}

	matched := make(map[string]bool, len(matchedEmails))
	for _, e := range matchedEmails {
		matched[e] = true
	}
	notFound := make([]string, 0)
	for _, e := range emails {
		if !matched[e] {
			notFound = append(notFound, e)
		}
	}

	// Push matched hashes into the live Bloom filter (if one is loaded for
	// this offer) so the campaign planner excludes them immediately. The DB
	// rows remain the durable source of truth either way.
	bloomUpdated := false
	if h.suppMgr != nil && len(matched) > 0 {
		hashes := make([]string, 0, len(matched))
		for e := range matched {
			sum := md5.Sum([]byte(e))
			hashes = append(hashes, hex.EncodeToString(sum[:]))
		}
		bloomUpdated = h.suppMgr.AddHashesToBloom(offerID, hashes)
	}

	log.Printf("[OfferSupp] manual add for offer %s: %d requested, %d matched subscribers, %d new rows, %d not found, bloom_updated=%v",
		offerID, len(emails), len(matched), added, len(notFound), bloomUpdated)

	// Cap the echoed not-found list so huge uploads don't balloon the response.
	notFoundEcho := notFound
	if len(notFoundEcho) > 500 {
		notFoundEcho = notFoundEcho[:500]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"requested":          len(emails),
		"invalid":            invalid,
		"matched":            len(matched),
		"added":              added,
		"already_suppressed": int64(len(matched)) - added,
		"not_found_count":    len(notFound),
		"not_found":          notFoundEcho,
		"bloom_updated":      bloomUpdated,
	})
}

// HandleListOfferSuppressions returns paginated suppression entries for an
// offer, with resolved subscriber emails where available.
// GET /offer-center/offers/{id}/suppressions?limit=50&offset=0&q=foo
func (h *OfferSuppressionUploadHandlers) HandleListOfferSuppressions(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	var total int
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM mailing_offer_suppressions WHERE offer_id = $1`, offerID).Scan(&total); err != nil {
		http.Error(w, `{"error":"count failed"}`, http.StatusInternalServerError)
		return
	}

	query := `
		SELECT os.id, COALESCE(LOWER(TRIM(s.email)), ''), os.email_hash, os.reason, os.source, os.suppressed_at
		FROM mailing_offer_suppressions os
		LEFT JOIN mailing_subscribers s ON s.id = os.subscriber_id
		WHERE os.offer_id = $1`
	args := []interface{}{offerID}
	if q != "" {
		query += ` AND LOWER(TRIM(s.email)) LIKE $2`
		args = append(args, "%"+q+"%")
	}
	query += ` ORDER BY os.suppressed_at DESC LIMIT ` + strconv.Itoa(limit) + ` OFFSET ` + strconv.Itoa(offset)

	rows, err := h.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type entry struct {
		ID           string `json:"id"`
		Email        string `json:"email"`
		EmailHash    string `json:"email_hash"`
		Reason       string `json:"reason"`
		Source       string `json:"source"`
		SuppressedAt string `json:"suppressed_at"`
	}
	entries := make([]entry, 0, limit)
	for rows.Next() {
		var e entry
		if rows.Scan(&e.ID, &e.Email, &e.EmailHash, &e.Reason, &e.Source, &e.SuppressedAt) == nil {
			entries = append(entries, e)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":   total,
		"entries": entries,
	})
}

// HandleRemoveOfferSuppression removes a single email from an offer's
// suppression list. Note: a loaded Bloom filter cannot un-learn the hash, so
// the planner may keep excluding the address until the next Bloom rebuild —
// but the send worker confirms positives against the DB, so this removal is
// effective for actual sending.
// DELETE /offer-center/offers/{id}/suppressions?email=a@b.com
func (h *OfferSuppressionUploadHandlers) HandleRemoveOfferSuppression(w http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" {
		http.Error(w, `{"error":"email query parameter required"}`, http.StatusBadRequest)
		return
	}

	sum := md5.Sum([]byte(email))
	emailHash := hex.EncodeToString(sum[:])

	// Same seq-scan caveat as the add path until idx_subscribers_lower_email
	// is built — raise the timeout for this transaction only.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, `{"error":"begin tx failed"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '280s'`); err != nil {
		log.Printf("[OfferSupp] could not raise statement_timeout: %v", err)
	}

	// email_hash is '' on some legacy rows (fatigue/conversion writers), so
	// also match through the subscriber record.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM mailing_offer_suppressions
		 WHERE offer_id = $1
		   AND (email_hash = $2 OR subscriber_id IN (
		       SELECT id FROM mailing_subscribers WHERE LOWER(TRIM(email)) = $3))`,
		offerID, emailHash, email)
	if err != nil {
		log.Printf("[OfferSupp] manual remove failed for offer %s: %v", offerID, err)
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	removed, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"commit failed"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("[OfferSupp] manual remove for offer %s: email=%s removed=%d", offerID, email, removed)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"removed": removed,
	})
}

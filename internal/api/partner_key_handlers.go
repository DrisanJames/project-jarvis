package api

// Partner API key lifecycle — list / rotate / revoke — plus the allowed
// vertical list. Until now the ONLY mint path was dataset creation
// (HandleCreateDataset, partner_admin_handlers.go): no way to see a
// dataset's keys, rotate a leaked one, or revoke without touching the DB by
// hand. These endpoints close that gap.
//
// Rules:
//   - the key HASH never leaves the DB — list returns key_prefix only;
//   - the RAW key is returned exactly once, at mint (rotate), with the same
//     api_key_warning wording dataset creation uses;
//   - rotate is ONE transaction: the new active row lands iff every other
//     active row for the dataset flips to revoked — a failure rolls both
//     back (no window with zero or two live keys);
//   - the middleware (partner_api_key_middleware.go) rejects status !=
//     'active', so a revoke takes effect on the partner's next request;
//   - every action is writeAuditLog'd (partner_admin_audit_log), mirroring
//     the existing partner-admin pattern.
//
// Mounted inside the authenticated /api router (session / X-Admin-Key), same
// as the rest of /api/mailing/data-partners.

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// ============ GET /api/mailing/data-partners/datasets/{id}/keys ============

func (h *PartnerAdminHandler) HandleListDatasetKeys(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT id, COALESCE(key_prefix, ''), COALESCE(status, 'active'),
		       last_used_at, created_at, revoked_at
		FROM partner_api_keys
		WHERE dataset_id = $1
		ORDER BY created_at DESC
	`, datasetID)
	if err != nil {
		writeJSONError(w, "list_keys_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var (
			id, prefix, status    string
			lastUsedAt, revokedAt sql.NullTime
			createdAt             time.Time
		)
		if err := rows.Scan(&id, &prefix, &status, &lastUsedAt, &createdAt, &revokedAt); err != nil {
			writeJSONError(w, "list_keys_scan_failed", http.StatusInternalServerError)
			return
		}
		out = append(out, map[string]interface{}{
			"id":           id,
			"key_prefix":   prefix,
			"status":       status,
			"last_used_at": formatNullTime(lastUsedAt),
			"created_at":   createdAt.Format(time.RFC3339),
			"revoked_at":   formatNullTime(revokedAt),
		})
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, "list_keys_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dataset_id": datasetID,
		"keys":       out,
	})
}

// ============ POST /api/mailing/data-partners/datasets/{id}/rotate-key ============

// HandleRotateDatasetKey mints a fresh key and revokes every other active
// key for the dataset, atomically. The raw key is shown ONCE.
func (h *PartnerAdminHandler) HandleRotateDatasetKey(w http.ResponseWriter, r *http.Request) {
	datasetID := chi.URLParam(r, "id")
	if !isValidUUID(datasetID) {
		writeJSONError(w, "invalid dataset id", http.StatusBadRequest)
		return
	}

	var partnerID string
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT partner_id FROM partner_datasets WHERE id = $1`, datasetID).
		Scan(&partnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, "dataset not found", http.StatusNotFound)
			return
		}
		writeJSONError(w, "dataset_lookup_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rawKey, prefix, hash, kerr := GeneratePartnerKey()
	if kerr != nil {
		writeJSONError(w, "key_generation_failed: "+kerr.Error(), http.StatusInternalServerError)
		return
	}
	keyID := uuid.New().String()

	// ONE tx: insert-new + revoke-others commit together or not at all.
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, "tx begin failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO partner_api_keys (id, partner_id, dataset_id, key_hash, key_prefix, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
	`, keyID, partnerID, datasetID, hash, prefix); err != nil {
		writeJSONError(w, "insert_api_key_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := tx.ExecContext(r.Context(), `
		UPDATE partner_api_keys
		SET status = 'revoked', revoked_at = NOW()
		WHERE dataset_id = $1 AND status = 'active' AND id <> $2
	`, datasetID, keyID)
	if err != nil {
		writeJSONError(w, "revoke_previous_keys_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	revokedCount, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		writeJSONError(w, "tx commit failed", http.StatusInternalServerError)
		return
	}

	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "rotate_dataset_key", "partner_api_key", keyID, nil, map[string]interface{}{
		"dataset_id": datasetID, "partner_id": partnerID,
		"key_prefix": prefix, "revoked_previous": revokedCount,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dataset_id":       datasetID,
		"key_id":           keyID,
		"api_key":          rawKey,
		"api_key_prefix":   prefix,
		"revoked_previous": revokedCount,
		"api_key_warning":  "Show this key to the partner ONCE — it cannot be retrieved later. Only the prefix is stored.",
	})
}

// ============ POST /api/mailing/data-partners/keys/{keyId}/revoke ============

// HandleRevokeKey revokes a single key. 404 when the key does not exist,
// 409 when it is already revoked (idempotency signalled, nothing rewritten —
// the original revoked_at is preserved).
func (h *PartnerAdminHandler) HandleRevokeKey(w http.ResponseWriter, r *http.Request) {
	keyID := chi.URLParam(r, "keyId")
	if !isValidUUID(keyID) {
		writeJSONError(w, "invalid key id", http.StatusBadRequest)
		return
	}

	var datasetID, prefix string
	var revokedAt time.Time
	err := h.db.QueryRowContext(r.Context(), `
		UPDATE partner_api_keys
		SET status = 'revoked', revoked_at = NOW()
		WHERE id = $1 AND status = 'active'
		RETURNING dataset_id, COALESCE(key_prefix, ''), revoked_at
	`, keyID).Scan(&datasetID, &prefix, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing flipped: absent, or already revoked. Distinguish for the
		// caller — a re-revoke is a 409 no-op, not a 404.
		var status string
		lookupErr := h.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(status, 'active') FROM partner_api_keys WHERE id = $1`, keyID).
			Scan(&status)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			writeJSONError(w, "key not found", http.StatusNotFound)
			return
		}
		if lookupErr != nil {
			writeJSONError(w, "revoke_lookup_failed: "+lookupErr.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONError(w, "key already revoked", http.StatusConflict)
		return
	}
	if err != nil {
		writeJSONError(w, "revoke_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeAuditLog(r.Context(), h.db, actorFromRequest(r), "revoke_partner_key", "partner_api_key", keyID, nil, map[string]interface{}{
		"dataset_id": datasetID, "key_prefix": prefix,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key_id":     keyID,
		"dataset_id": datasetID,
		"key_prefix": prefix,
		"status":     "revoked",
		"revoked_at": revokedAt.Format(time.RFC3339),
	})
}

// ============ GET /api/mailing/data-partners/verticals ============

// HandleListVerticals returns the allowed partner_datasets verticals, sourced
// from PartnerVerticals (the ONE Go source, partner_admin_handlers.go). The
// DB CHECK constraint partner_datasets_vertical_check (cmd/server/main.go) is
// the enforcement — keep them in lockstep. This exists so the frontend wizard
// and GET /api/partner-ingest/v1/schema stop hardcoding the 4 legacy
// verticals.
func (h *PartnerAdminHandler) HandleListVerticals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"verticals": append([]string{}, PartnerVerticals...),
		"note":      "sourced from api.PartnerVerticals; enforced by the partner_datasets_vertical_check CHECK constraint (cmd/server/main.go migration aug14_partner_datasets_vertical_internal_auto_v3_v7)",
	})
}

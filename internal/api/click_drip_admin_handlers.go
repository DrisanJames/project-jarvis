package api

// Click-Drip admin handlers — CRUD over two operator-managed tables that
// drive the click-drip journey feature wired in Phase 1 (2026-06-01):
//
//   * mailing_offer_reminder_subjects — per-offer subject/preheader/from-name
//     overrides for each step of a click-drip journey. JourneyExecutor reads
//     the row keyed on (everflow_offer_id, sequence_index) at email-node
//     execution time.
//
//   * mailing_offer_journey_map — pairs an Everflow offer_id to a journey id
//     plus the payout_type and an `enabled` kill-switch. The click postback
//     handler (everflow_click_postback_handler.go) re-reads this on every
//     postback, so flipping `enabled=false` here halts new enrollments
//     instantly without any cache reload.
//
// These endpoints feed Phase 4 of the click-drip build — the operator UI
// that lets ops edit subjects and toggle offers without a deploy. They are
// mounted inside the authenticated /api router via server_routes_mailing.go,
// inheriting session / X-Admin-Key auth.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// ClickDripAdminHandlers groups the CRUD endpoints. Constructed once at
// server-route setup time with a *sql.DB and registered via the helper in
// server_routes_mailing.go.
type ClickDripAdminHandlers struct {
	db *sql.DB
}

// NewClickDripAdminHandlers wires the handler with the mailing *sql.DB.
func NewClickDripAdminHandlers(db *sql.DB) *ClickDripAdminHandlers {
	return &ClickDripAdminHandlers{db: db}
}

// ─── shared types ──────────────────────────────────────────────────────────

type reminderSubjectRow struct {
	EverflowOfferID  string    `json:"everflow_offer_id"`
	SequenceIndex    int       `json:"sequence_index"`
	Subject          string    `json:"subject"`
	Preheader        string    `json:"preheader"`
	FromNameOverride string    `json:"from_name_override"`
	Enabled          bool      `json:"enabled"`
	Notes            string    `json:"notes"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type offerJourneyMapRow struct {
	EverflowOfferID string    `json:"everflow_offer_id"`
	ClickJourneyID  string    `json:"click_journey_id"`
	PayoutType      string    `json:"payout_type"`
	Enabled         bool      `json:"enabled"`
	Notes           string    `json:"notes"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// payoutTypeAllowed mirrors the CHECK constraint on mailing_offer_journey_map.
// Keep this in lockstep with the migration in cmd/server/main.go
// (jun01_click_drip_offer_journey_map_payout_chk) — any drift here lets
// invalid values through the API but the DB will reject them, producing a
// 500 instead of a 400.
func payoutTypeAllowed(t string) bool {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "CPM", "ECPM", "CPA", "CPL", "CPC", "IO", "PRV", "UNKNOWN":
		return true
	}
	return false
}

// canonicalPayoutType returns the storage-form (preserving the lone lowercase
// 'e' in eCPM that the CHECK constraint allows). Every other value is
// uppercased.
func canonicalPayoutType(t string) string {
	t = strings.TrimSpace(t)
	if strings.EqualFold(t, "eCPM") {
		return "eCPM"
	}
	return strings.ToUpper(t)
}

// writeClickDripError emits {"error": msg} with the supplied status code
// and the JSON content-type. The handler-suite-wide writeJSONError helper
// in custom_fields_handlers.go has the same shape but using a local
// wrapper keeps grep-for-callsite tight inside this file.
func writeClickDripError(w http.ResponseWriter, msg string, status int) {
	writeJSONError(w, msg, status)
}

// parseSequenceIndex extracts and validates the {sequence_index} URL param.
// Returns (idx, true) on success; on failure it writes a 400 and returns
// (_, false). Range [0, 99] matches the operator-locked maximum drip
// length — the seeded journey is 4 steps and we don't expect anyone to
// configure more than ~10 in practice.
func parseSequenceIndex(w http.ResponseWriter, raw string) (int, bool) {
	idx, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		writeClickDripError(w, "sequence_index must be an integer", http.StatusBadRequest)
		return 0, false
	}
	if idx < 0 || idx > 99 {
		writeClickDripError(w, "sequence_index must be in [0, 99]", http.StatusBadRequest)
		return 0, false
	}
	return idx, true
}

// ─── reminder subjects: list ───────────────────────────────────────────────

// ListReminderSubjects — GET /api/mailing/offer-reminder-subjects
//
// Optional filter ?everflow_offer_id={id} narrows results to a single offer.
// Without the filter, every row is returned. Results are ordered by
// (everflow_offer_id ASC, sequence_index ASC) so consumers can group on the
// fly. The "everflow_offer_id ASC" first key is harmless when filtering.
func (h *ClickDripAdminHandlers) ListReminderSubjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerFilter := strings.TrimSpace(r.URL.Query().Get("everflow_offer_id"))

	rows, err := h.db.QueryContext(ctx, `
		SELECT everflow_offer_id, sequence_index, subject,
		       COALESCE(preheader, ''), COALESCE(from_name_override, ''),
		       enabled, COALESCE(notes, ''), updated_at
		FROM mailing_offer_reminder_subjects
		WHERE ($1 = '' OR everflow_offer_id = $1)
		ORDER BY everflow_offer_id ASC, sequence_index ASC
	`, offerFilter)
	if err != nil {
		writeClickDripError(w, "list_reminder_subjects_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]reminderSubjectRow, 0)
	for rows.Next() {
		var row reminderSubjectRow
		if err := rows.Scan(
			&row.EverflowOfferID, &row.SequenceIndex, &row.Subject,
			&row.Preheader, &row.FromNameOverride, &row.Enabled,
			&row.Notes, &row.UpdatedAt,
		); err != nil {
			writeClickDripError(w, "scan_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeClickDripError(w, "rows_err: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": out})
}

// ─── reminder subjects: get one ────────────────────────────────────────────

// GetReminderSubject — GET /api/mailing/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}
func (h *ClickDripAdminHandlers) GetReminderSubject(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "everflow_offer_id"))
	if offerID == "" {
		writeClickDripError(w, "everflow_offer_id is required", http.StatusBadRequest)
		return
	}
	seqIdx, ok := parseSequenceIndex(w, chi.URLParam(r, "sequence_index"))
	if !ok {
		return
	}

	var row reminderSubjectRow
	err := h.db.QueryRowContext(r.Context(), `
		SELECT everflow_offer_id, sequence_index, subject,
		       COALESCE(preheader, ''), COALESCE(from_name_override, ''),
		       enabled, COALESCE(notes, ''), updated_at
		FROM mailing_offer_reminder_subjects
		WHERE everflow_offer_id = $1 AND sequence_index = $2
	`, offerID, seqIdx).Scan(
		&row.EverflowOfferID, &row.SequenceIndex, &row.Subject,
		&row.Preheader, &row.FromNameOverride, &row.Enabled,
		&row.Notes, &row.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeClickDripError(w, "reminder subject not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeClickDripError(w, "get_reminder_subject_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": row})
}

// ─── reminder subjects: upsert ─────────────────────────────────────────────

type upsertReminderSubjectRequest struct {
	Subject          string `json:"subject"`
	Preheader        string `json:"preheader"`
	FromNameOverride string `json:"from_name_override"`
	Enabled          *bool  `json:"enabled"`
	Notes            string `json:"notes"`
}

// UpsertReminderSubject — PUT /api/mailing/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}
//
// Body is the new value for every editable column. Subject is required —
// the table's NOT NULL DEFAULT '' would otherwise let us write an empty
// string, but the operator UI considers an empty subject a usability bug
// (it would ship a blank-subject email). preheader / from_name_override /
// notes default to empty; enabled defaults to true.
func (h *ClickDripAdminHandlers) UpsertReminderSubject(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "everflow_offer_id"))
	if offerID == "" {
		writeClickDripError(w, "everflow_offer_id is required", http.StatusBadRequest)
		return
	}
	seqIdx, ok := parseSequenceIndex(w, chi.URLParam(r, "sequence_index"))
	if !ok {
		return
	}

	var req upsertReminderSubjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeClickDripError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	if req.Subject == "" {
		writeClickDripError(w, "subject is required", http.StatusBadRequest)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	var row reminderSubjectRow
	err := h.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_offer_reminder_subjects (
			everflow_offer_id, sequence_index, subject, preheader,
			from_name_override, enabled, notes, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (everflow_offer_id, sequence_index) DO UPDATE SET
			subject = EXCLUDED.subject,
			preheader = EXCLUDED.preheader,
			from_name_override = EXCLUDED.from_name_override,
			enabled = EXCLUDED.enabled,
			notes = EXCLUDED.notes,
			updated_at = NOW()
		RETURNING everflow_offer_id, sequence_index, subject,
		          COALESCE(preheader, ''), COALESCE(from_name_override, ''),
		          enabled, COALESCE(notes, ''), updated_at
	`,
		offerID, seqIdx, req.Subject, req.Preheader,
		req.FromNameOverride, enabled, req.Notes,
	).Scan(
		&row.EverflowOfferID, &row.SequenceIndex, &row.Subject,
		&row.Preheader, &row.FromNameOverride, &row.Enabled,
		&row.Notes, &row.UpdatedAt,
	)
	if err != nil {
		writeClickDripError(w, "upsert_reminder_subject_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": row})
}

// ─── reminder subjects: delete ─────────────────────────────────────────────

// DeleteReminderSubject — DELETE /api/mailing/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}
//
// Hard delete. The intent is that an operator removing a step considers
// it gone; the journey config (cmd/server/main.go seed) is what drives
// step count, so a missing row at email-node execution falls back to the
// in-creative <title> per JourneyExecutor.resolveReminderSubject.
func (h *ClickDripAdminHandlers) DeleteReminderSubject(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "everflow_offer_id"))
	if offerID == "" {
		writeClickDripError(w, "everflow_offer_id is required", http.StatusBadRequest)
		return
	}
	seqIdx, ok := parseSequenceIndex(w, chi.URLParam(r, "sequence_index"))
	if !ok {
		return
	}

	res, err := h.db.ExecContext(r.Context(), `
		DELETE FROM mailing_offer_reminder_subjects
		WHERE everflow_offer_id = $1 AND sequence_index = $2
	`, offerID, seqIdx)
	if err != nil {
		writeClickDripError(w, "delete_reminder_subject_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeClickDripError(w, "reminder subject not found", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// ─── offer journey map: list ───────────────────────────────────────────────

// ListOfferJourneyMap — GET /api/mailing/offer-journey-map
//
// Returns every row, ordered by everflow_offer_id ASC. The table is tiny
// (one row per live offer, currently 2 in production) so no pagination /
// filter parameters are needed. If the table grows we can layer a
// ?enabled= filter on later without breaking response shape.
func (h *ClickDripAdminHandlers) ListOfferJourneyMap(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT everflow_offer_id, COALESCE(click_journey_id, ''),
		       COALESCE(payout_type, 'UNKNOWN'), enabled,
		       COALESCE(notes, ''), created_at, updated_at
		FROM mailing_offer_journey_map
		ORDER BY everflow_offer_id ASC
	`)
	if err != nil {
		writeClickDripError(w, "list_offer_journey_map_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]offerJourneyMapRow, 0)
	for rows.Next() {
		var row offerJourneyMapRow
		if err := rows.Scan(
			&row.EverflowOfferID, &row.ClickJourneyID, &row.PayoutType,
			&row.Enabled, &row.Notes, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			writeClickDripError(w, "scan_failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeClickDripError(w, "rows_err: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": out})
}

// ─── offer journey map: upsert ─────────────────────────────────────────────

type upsertOfferJourneyMapRequest struct {
	ClickJourneyID string `json:"click_journey_id"`
	PayoutType     string `json:"payout_type"`
	Enabled        *bool  `json:"enabled"`
	Notes          string `json:"notes"`
}

// UpsertOfferJourneyMap — PUT /api/mailing/offer-journey-map/{everflow_offer_id}
//
// click_journey_id may be empty (operator pre-creating a row to flip on
// later). payout_type must be one of the CHECK-constrained values:
// CPM, eCPM, CPA, CPL, CPC, IO, PRV, UNKNOWN. enabled defaults to false
// when omitted — newly added offers should ship paused so an operator
// can audit before going live (mirrors the NDR seed in main.go).
func (h *ClickDripAdminHandlers) UpsertOfferJourneyMap(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "everflow_offer_id"))
	if offerID == "" {
		writeClickDripError(w, "everflow_offer_id is required", http.StatusBadRequest)
		return
	}

	var req upsertOfferJourneyMapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeClickDripError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.PayoutType = strings.TrimSpace(req.PayoutType)
	if req.PayoutType == "" {
		req.PayoutType = "UNKNOWN"
	}
	if !payoutTypeAllowed(req.PayoutType) {
		writeClickDripError(w,
			"payout_type must be one of: CPM, eCPM, CPA, CPL, CPC, IO, PRV, UNKNOWN",
			http.StatusBadRequest)
		return
	}
	req.PayoutType = canonicalPayoutType(req.PayoutType)

	enabled := false
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	var row offerJourneyMapRow
	err := h.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_offer_journey_map (
			everflow_offer_id, click_journey_id, payout_type, enabled, notes,
			created_at, updated_at
		) VALUES ($1, NULLIF($2, ''), $3, $4, $5, NOW(), NOW())
		ON CONFLICT (everflow_offer_id) DO UPDATE SET
			click_journey_id = EXCLUDED.click_journey_id,
			payout_type = EXCLUDED.payout_type,
			enabled = EXCLUDED.enabled,
			notes = EXCLUDED.notes,
			updated_at = NOW()
		RETURNING everflow_offer_id, COALESCE(click_journey_id, ''),
		          COALESCE(payout_type, 'UNKNOWN'), enabled,
		          COALESCE(notes, ''), created_at, updated_at
	`,
		offerID, req.ClickJourneyID, req.PayoutType, enabled, req.Notes,
	).Scan(
		&row.EverflowOfferID, &row.ClickJourneyID, &row.PayoutType,
		&row.Enabled, &row.Notes, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		writeClickDripError(w, "upsert_offer_journey_map_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": row})
}

// ─── offer journey map: delete ─────────────────────────────────────────────

// DeleteOfferJourneyMap — DELETE /api/mailing/offer-journey-map/{everflow_offer_id}
//
// Hard delete. The click postback handler treats a missing row as
// offer_not_mapped → skip, so removing a row is the strongest pause
// (flip enabled=false is the soft pause). Existing in-flight enrollments
// for the offer continue to run — the click_journey_id on the enrollment
// is already frozen.
func (h *ClickDripAdminHandlers) DeleteOfferJourneyMap(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "everflow_offer_id"))
	if offerID == "" {
		writeClickDripError(w, "everflow_offer_id is required", http.StatusBadRequest)
		return
	}

	res, err := h.db.ExecContext(r.Context(), `
		DELETE FROM mailing_offer_journey_map WHERE everflow_offer_id = $1
	`, offerID)
	if err != nil {
		writeClickDripError(w, "delete_offer_journey_map_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeClickDripError(w, "offer journey map not found", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// RegisterClickDripAdminRoutes mounts every endpoint on the supplied chi
// router. Caller is responsible for routing it under /api/mailing — see
// server_routes_mailing.go.
func RegisterClickDripAdminRoutes(r chi.Router, db *sql.DB) {
	h := NewClickDripAdminHandlers(db)
	r.Get("/offer-reminder-subjects", h.ListReminderSubjects)
	r.Get("/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}", h.GetReminderSubject)
	r.Put("/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}", h.UpsertReminderSubject)
	r.Delete("/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}", h.DeleteReminderSubject)
	r.Get("/offer-journey-map", h.ListOfferJourneyMap)
	r.Put("/offer-journey-map/{everflow_offer_id}", h.UpsertOfferJourneyMap)
	r.Delete("/offer-journey-map/{everflow_offer_id}", h.DeleteOfferJourneyMap)
}

package api

// =============================================================================
// SEGMENT-FAMILY OWNERSHIP REGISTRY — Platform Coalition WS2, REQ-C15
// =============================================================================
// mailing_segment_registry holds one row per segment-family NAME PATTERN with
// its owner, refresh cadence, freshness SLA, and keep_policy. It exists because
// segment families had no machine-readable ownership: SegmentCleanupWorker
// hard-deleted the doctrine-ring and 60D-Human families (Lane 2 F3) with
// nowhere to ask "does anything own this?". Consumers:
//   - SegmentCleanupWorker consent gate (internal/worker/segment_cleanup.go,
//     REQ-C16): closed-by-default — only keep_policy='purgeable' families may
//     be hard-deleted.
//   - The WS3 ops console (registry list + family-freshness panels).
//   - Invariant I9 (no scheduled campaign references an unowned family).
// Schema + seeds land via runStartupMigrations (cmd/server/main.go,
// create_segment_registry / seed_segment_registry_families). JSON contract:
// tasks/eng-team/coalition/SCHEMA-CONTRACTS.md §1.
//
// Rows are never hard-deleted (charter rule 2) — disable via active=false.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// SegmentRegistryRow is one registered segment family (JSON shape is the WS3
// contract — do not rename fields without a SCHEMA-CONTRACTS.md revision).
type SegmentRegistryRow struct {
	ID               uuid.UUID `json:"id"`
	FamilyKey        string    `json:"family_key"`
	FamilyPattern    string    `json:"family_pattern"`
	DefinitionSource string    `json:"definition_source"`
	Owner            string    `json:"owner"`
	Cadence          string    `json:"cadence"`
	SLAHours         int       `json:"sla_hours"`
	KeepPolicy       string    `json:"keep_policy"`
	HeartbeatWorker  string    `json:"heartbeat_worker"`
	Notes            string    `json:"notes"`
	Active           bool      `json:"active"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// segmentRegistryHeartbeat is the joined liveness snapshot for a family whose
// heartbeat_worker is set (nil when unset or the worker never beat).
type segmentRegistryHeartbeat struct {
	LastBeatAt time.Time `json:"last_beat_at"`
	LastStatus string    `json:"last_status"`
	Stalled    bool      `json:"stalled"`
}

// SegmentRegistryFreshnessRow is one family's freshness snapshot for the ops
// console ("segment-family freshness vs SLA" panel).
type SegmentRegistryFreshnessRow struct {
	ID              uuid.UUID                 `json:"id"`
	FamilyKey       string                    `json:"family_key"`
	FamilyPattern   string                    `json:"family_pattern"`
	Owner           string                    `json:"owner"`
	Cadence         string                    `json:"cadence"`
	SLAHours        int                       `json:"sla_hours"`
	KeepPolicy      string                    `json:"keep_policy"`
	Active          bool                      `json:"active"`
	SegmentsCount   int                       `json:"segments_count"`
	MembersEstimate int64                     `json:"members_estimate"`
	// NewestRefreshAt nil = the family has NEVER been built/refreshed — the UI
	// must render that distinctly from "built empty" (AS-6.1).
	NewestRefreshAt *time.Time                `json:"newest_refresh_at"`
	OldestRefreshAt *time.Time                `json:"oldest_refresh_at"`
	HeartbeatWorker string                    `json:"heartbeat_worker"`
	Heartbeat       *segmentRegistryHeartbeat `json:"heartbeat"`
	SLAState        string                    `json:"sla_state"` // ok | breached | no_sla
}

// validDefinitionSources / validKeepPolicies mirror the SCHEMA-CONTRACTS.md §1
// enums; writes outside them are rejected at the handler.
var validDefinitionSources = map[string]bool{"conditions": true, "lake_spec": true, "slot_ledger": true, "script": true}
var validKeepPolicies = map[string]bool{"protect": true, "purgeable": true}

// SegmentRegistryStore is the data access layer (AS-5.2: no raw SQL in
// handlers). All methods are org-scoped by parameter.
type SegmentRegistryStore struct {
	db *sql.DB
}

func NewSegmentRegistryStore(db *sql.DB) *SegmentRegistryStore {
	return &SegmentRegistryStore{db: db}
}

const segmentRegistryCols = `id, family_key, family_pattern, definition_source, owner,
	cadence, sla_hours, keep_policy, heartbeat_worker, notes, active, created_at, updated_at`

func scanSegmentRegistryRow(sc interface{ Scan(...any) error }) (SegmentRegistryRow, error) {
	var row SegmentRegistryRow
	err := sc.Scan(&row.ID, &row.FamilyKey, &row.FamilyPattern, &row.DefinitionSource,
		&row.Owner, &row.Cadence, &row.SLAHours, &row.KeepPolicy, &row.HeartbeatWorker,
		&row.Notes, &row.Active, &row.CreatedAt, &row.UpdatedAt)
	return row, err
}

// List returns every registry row for the org (including active=false, so the
// console can show disabled families), ordered stably.
func (s *SegmentRegistryStore) List(r *http.Request, orgID uuid.UUID) ([]SegmentRegistryRow, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT `+segmentRegistryCols+`
		FROM mailing_segment_registry
		WHERE organization_id = $1
		ORDER BY family_key, family_pattern
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SegmentRegistryRow, 0, 16)
	for rows.Next() {
		row, err := scanSegmentRegistryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Create inserts a new family row. The (org, family_pattern) unique constraint
// surfaces duplicates as an error rather than silently shadowing a family.
func (s *SegmentRegistryStore) Create(r *http.Request, orgID uuid.UUID, in SegmentRegistryRow) (SegmentRegistryRow, error) {
	row := s.db.QueryRowContext(r.Context(), `
		INSERT INTO mailing_segment_registry (
			organization_id, family_key, family_pattern, definition_source,
			owner, cadence, sla_hours, keep_policy, heartbeat_worker, notes, active
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+segmentRegistryCols+`
	`, orgID, in.FamilyKey, in.FamilyPattern, in.DefinitionSource, in.Owner,
		in.Cadence, in.SLAHours, in.KeepPolicy, in.HeartbeatWorker, in.Notes, in.Active)
	return scanSegmentRegistryRow(row)
}

// Update rewrites the mutable fields of one row (org-scoped by WHERE — a row
// belonging to another org is simply not found).
func (s *SegmentRegistryStore) Update(r *http.Request, orgID, id uuid.UUID, in SegmentRegistryRow) (SegmentRegistryRow, error) {
	row := s.db.QueryRowContext(r.Context(), `
		UPDATE mailing_segment_registry SET
			family_key = $3, family_pattern = $4, definition_source = $5,
			owner = $6, cadence = $7, sla_hours = $8, keep_policy = $9,
			heartbeat_worker = $10, notes = $11, active = $12, updated_at = NOW()
		WHERE organization_id = $1 AND id = $2
		RETURNING `+segmentRegistryCols+`
	`, orgID, id, in.FamilyKey, in.FamilyPattern, in.DefinitionSource, in.Owner,
		in.Cadence, in.SLAHours, in.KeepPolicy, in.HeartbeatWorker, in.Notes, in.Active)
	return scanSegmentRegistryRow(row)
}

// Freshness joins each active family against its segments (count, member
// estimate, newest/oldest refresh) and its heartbeat row. One query, bounded
// by registry size (tens of rows) × an indexed-name LIKE scan per family over
// mailing_segments (thousands of rows) — well inside the 30s handler budget.
func (s *SegmentRegistryStore) Freshness(r *http.Request, orgID uuid.UUID) ([]SegmentRegistryFreshnessRow, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT reg.id, reg.family_key, reg.family_pattern, reg.owner, reg.cadence,
		       reg.sla_hours, reg.keep_policy, reg.active,
		       COALESCE(seg.segments_count, 0),
		       COALESCE(seg.members_estimate, 0),
		       seg.newest_refresh_at, seg.oldest_refresh_at,
		       reg.heartbeat_worker,
		       hb.last_beat_at, hb.last_status, hb.stalled
		FROM mailing_segment_registry reg
		LEFT JOIN LATERAL (
			SELECT COUNT(*)                        AS segments_count,
			       COALESCE(SUM(s.subscriber_count), 0)::bigint AS members_estimate,
			       MAX(s.last_calculated_at)       AS newest_refresh_at,
			       MIN(s.last_calculated_at)       AS oldest_refresh_at
			FROM mailing_segments s
			WHERE s.organization_id = reg.organization_id
			  AND s.name LIKE reg.family_pattern
			  AND s.archived_at IS NULL
		) seg ON TRUE
		LEFT JOIN mailing_worker_heartbeats hb ON hb.worker_name = reg.heartbeat_worker
		WHERE reg.organization_id = $1
		ORDER BY reg.family_key, reg.family_pattern
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SegmentRegistryFreshnessRow, 0, 16)
	for rows.Next() {
		var f SegmentRegistryFreshnessRow
		var newest, oldest, hbBeat sql.NullTime
		var hbStatus sql.NullString
		var hbStalled sql.NullBool
		if err := rows.Scan(&f.ID, &f.FamilyKey, &f.FamilyPattern, &f.Owner, &f.Cadence,
			&f.SLAHours, &f.KeepPolicy, &f.Active,
			&f.SegmentsCount, &f.MembersEstimate, &newest, &oldest,
			&f.HeartbeatWorker, &hbBeat, &hbStatus, &hbStalled); err != nil {
			return nil, err
		}
		if newest.Valid {
			t := newest.Time
			f.NewestRefreshAt = &t
		}
		if oldest.Valid {
			t := oldest.Time
			f.OldestRefreshAt = &t
		}
		if hbBeat.Valid {
			f.Heartbeat = &segmentRegistryHeartbeat{
				LastBeatAt: hbBeat.Time,
				LastStatus: hbStatus.String,
				Stalled:    hbStalled.Valid && hbStalled.Bool,
			}
		}
		switch {
		case f.SLAHours <= 0:
			f.SLAState = "no_sla"
		case f.NewestRefreshAt == nil,
			time.Since(*f.NewestRefreshAt) > time.Duration(f.SLAHours)*time.Hour:
			f.SLAState = "breached"
		default:
			f.SLAState = "ok"
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SegmentRegistryService exposes the registry over HTTP (org-scoped CRUD +
// freshness; no DELETE — charter rule 2, disable via active=false).
type SegmentRegistryService struct {
	store *SegmentRegistryStore
}

func NewSegmentRegistryService(db *sql.DB) *SegmentRegistryService {
	return &SegmentRegistryService{store: NewSegmentRegistryStore(db)}
}

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *SegmentRegistryService) RegisterRoutes(r chi.Router) {
	r.Route("/segment-registry", func(r chi.Router) {
		r.Get("/", s.HandleList)
		r.Post("/", s.HandleCreate)
		r.Put("/{id}", s.HandleUpdate)
		r.Get("/freshness", s.HandleFreshness)
	})
}

func (s *SegmentRegistryService) HandleList(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	rows, err := s.store.List(r, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "list segment registry: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"families": rows})
}

// validateRegistryWrite normalizes and validates a write payload in place.
func validateRegistryWrite(in *SegmentRegistryRow) string {
	in.FamilyKey = strings.TrimSpace(in.FamilyKey)
	in.FamilyPattern = strings.TrimSpace(in.FamilyPattern)
	in.DefinitionSource = strings.TrimSpace(in.DefinitionSource)
	in.KeepPolicy = strings.TrimSpace(in.KeepPolicy)
	if in.FamilyKey == "" {
		return "family_key is required"
	}
	if in.FamilyPattern == "" {
		return "family_pattern is required"
	}
	if in.DefinitionSource == "" {
		in.DefinitionSource = "script"
	}
	if !validDefinitionSources[in.DefinitionSource] {
		return "definition_source must be one of conditions|lake_spec|slot_ledger|script"
	}
	if in.KeepPolicy == "" {
		in.KeepPolicy = "protect"
	}
	if !validKeepPolicies[in.KeepPolicy] {
		return "keep_policy must be protect|purgeable"
	}
	if in.SLAHours < 0 {
		return "sla_hours must be >= 0"
	}
	if in.Owner == "" {
		return "owner is required (an unowned family is the defect this registry exists to kill)"
	}
	return ""
}

func (s *SegmentRegistryService) HandleCreate(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	var in SegmentRegistryRow
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in.Active = true
	if msg := validateRegistryWrite(&in); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	row, err := s.store.Create(r, orgID, in)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			respondError(w, http.StatusConflict, "a registry row with this family_pattern already exists")
			return
		}
		respondError(w, http.StatusInternalServerError, "create segment registry row: "+err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, row)
}

func (s *SegmentRegistryService) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in SegmentRegistryRow
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if msg := validateRegistryWrite(&in); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	row, err := s.store.Update(r, orgID, id, in)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "registry row not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "update segment registry row: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, row)
}

func (s *SegmentRegistryService) HandleFreshness(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	rows, err := s.store.Freshness(r, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "segment registry freshness: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"families":     rows,
	})
}

package api

// Segment-grid freshness + on-demand refresh — the API contract behind the
// portal's segment-freshness screen (frontend builds against EXACTLY this
// shape; bump VersionSegmentFreshnessAPI on any change).
//
//	GET  /api/mailing/segments/freshness  — the per-sending-domain engagement
//	     grid ('<CODE> <N>D (Openers|Clickers)') with build-ledger truth,
//	     an efficient real member-stamp read, and worker liveness.
//	POST /api/mailing/segments/refresh    — queue on-demand rebuilds into
//	     mailing_segment_refresh_requests (drained by worker.SegmentGridWorker
//	     within its tick). Idempotent: a segment already queued/running is
//	     reported as such, never double-inserted (partial unique index
//	     uq_seg_refresh_req_open is the rail).
//
// HONESTY RULES (repo-wide): member_count comes from the build LEDGER and is
// null until a VERIFIED successful build exists — absence of measurement is
// never rendered as zero. freshness is derived ONLY from the ledger's
// last_built_at (advanced exclusively by successful builds): 'unknown' when
// there is no verified measurement, and 'unknown' is never conflated with
// 'fresh'. members_stamped_at is a REAL read (max materialized_at per
// segment via the idx_segment_members_seg_stamp indexed path — never a full
// member-table scan); when that bounded read fails it degrades to null, not
// to a guess.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/lib/pq"
)

// VersionSegmentFreshnessAPI is bumped on every change to these handlers'
// response shapes so API consumers can detect drift.
const VersionSegmentFreshnessAPI = "1.0"

// Freshness bands (contract-fixed): fresh <26h, aging 26–48h, stale >48h,
// unknown = no verified measurement.
const (
	segmentFreshnessFreshMax = 26 * time.Hour
	segmentFreshnessAgingMax = 48 * time.Hour
)

// segFreshnessNameRe mirrors worker.SegmentGridWorker's grid selection.
var segFreshnessNameRe = regexp.MustCompile(`^([A-Z]{2,4}) (7|14|30|60)D (Openers|Clickers)$`)

// computeSegmentFreshness maps a verified last-built time to the contract's
// freshness band. lastBuiltAt == nil means no verified measurement exists —
// 'unknown', which callers must never render as fresh OR as zero.
func computeSegmentFreshness(lastBuiltAt *time.Time, now time.Time) string {
	if lastBuiltAt == nil {
		return "unknown"
	}
	age := now.Sub(*lastBuiltAt)
	switch {
	case age < segmentFreshnessFreshMax:
		return "fresh"
	case age <= segmentFreshnessAgingMax:
		return "aging"
	default:
		return "stale"
	}
}

// SegmentFreshnessService owns the two endpoints.
type SegmentFreshnessService struct {
	db *sql.DB
}

func NewSegmentFreshnessService(db *sql.DB) *SegmentFreshnessService {
	return &SegmentFreshnessService{db: db}
}

// RegisterRoutes mounts under the /api/mailing group. Static paths win over
// the existing /segments/{segmentId} params in chi, so these coexist with
// the legacy segment CRUD routes.
func (s *SegmentFreshnessService) RegisterRoutes(r chi.Router) {
	r.Get("/segments/freshness", s.HandleSegmentFreshness)
	r.Post("/segments/refresh", s.HandleSegmentRefreshRequest)
}

type segmentFreshnessWorker struct {
	Running         bool    `json:"running"`
	LastPassAt      *string `json:"last_pass_at"`
	LastPassOutcome string  `json:"last_pass_outcome"`
	Degraded        bool    `json:"degraded"`
	Leader          bool    `json:"leader"`
}

type segmentFreshnessRow struct {
	SegmentID        string  `json:"segment_id"`
	Name             string  `json:"name"`
	Brand            string  `json:"brand"`
	WindowDays       int     `json:"window_days"`
	Kind             string  `json:"kind"` // "openers" | "clickers"
	Status           string  `json:"status"`
	MemberCount      *int64  `json:"member_count"`
	MembersStampedAt *string `json:"members_stamped_at"`
	LastBuiltAt      *string `json:"last_built_at"`
	BuildSource      string  `json:"build_source"`
	LastBuildStatus  string  `json:"last_build_status"`
	LastError        string  `json:"last_error"`
	Freshness        string  `json:"freshness"`
	RefreshState     *string `json:"refresh_state"` // null | "queued" | "running"
}

type segmentFreshnessResponse struct {
	APIVersion  string                 `json:"api_version"`
	GeneratedAt string                 `json:"generated_at"`
	Worker      segmentFreshnessWorker `json:"worker"`
	Rows        []segmentFreshnessRow  `json:"rows"`
}

// HandleSegmentFreshness — GET /api/mailing/segments/freshness.
func (s *SegmentFreshnessService) HandleSegmentFreshness(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()

	resp := segmentFreshnessResponse{
		APIVersion:  VersionSegmentFreshnessAPI,
		GeneratedAt: now.Format(time.RFC3339),
		Rows:        []segmentFreshnessRow{},
	}

	// Grid rows + ledger in one read. Segment enumeration is by NAME +
	// status='active' (never ledger-only — a ledger-only enumeration counts
	// archived corpses; 2026-08-21 finding).
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id::text, s.name, s.status,
		       l.subscriber_count, l.last_built_at,
		       COALESCE(l.build_source, ''), COALESCE(l.last_build_status, ''),
		       COALESCE(l.last_error, ''), l.updated_at
		FROM mailing_segments s
		LEFT JOIN mailing_segment_build_ledger l ON l.segment_id = s.id
		WHERE s.organization_id = $1
		  AND s.status = 'active'
		  AND s.name ~ '^[A-Z]{2,4} (7|14|30|60)D (Openers|Clickers)$'
		ORDER BY s.name
	`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "freshness query failed: "+err.Error())
		return
	}
	type rawRow struct {
		row           segmentFreshnessRow
		ledgerCount   sql.NullInt64
		builtAt       sql.NullTime
		ledgerUpdated sql.NullTime
	}
	var raws []rawRow
	for rows.Next() {
		var rr rawRow
		if err := rows.Scan(&rr.row.SegmentID, &rr.row.Name, &rr.row.Status,
			&rr.ledgerCount, &rr.builtAt, &rr.row.BuildSource,
			&rr.row.LastBuildStatus, &rr.row.LastError, &rr.ledgerUpdated); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "freshness scan failed: "+err.Error())
			return
		}
		raws = append(raws, rr)
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		// A truncated listing is a WRONG listing — refuse rather than render
		// a partial grid as the whole grid.
		respondError(w, http.StatusInternalServerError, "freshness read truncated: "+readErr.Error())
		return
	}

	ids := make([]string, len(raws))
	for i := range raws {
		ids[i] = raws[i].row.SegmentID
	}

	stamps := s.loadMemberStamps(r, ids)
	refreshStates := s.loadOpenRefreshStates(r, ids)

	for _, rr := range raws {
		row := rr.row
		if m := segFreshnessNameRe.FindStringSubmatch(row.Name); m != nil {
			row.Brand = m[1]
			row.WindowDays, _ = strconv.Atoi(m[2])
			row.Kind = strings.ToLower(m[3])
		}
		// A stale 'running' ledger row is a crashed build (same coercion the
		// v2 list applies) — display it as failed, not perpetually running.
		if row.LastBuildStatus == "running" && rr.ledgerUpdated.Valid {
			row.LastBuildStatus = coerceLedgerStatus(row.LastBuildStatus, rr.ledgerUpdated.Time, now)
		}
		// member_count / last_built_at / freshness only from a VERIFIED
		// successful build. No verified build → null count + 'unknown';
		// unknown is never rendered as zero or as fresh.
		if rr.builtAt.Valid {
			t := rr.builtAt.Time.UTC()
			ts := t.Format(time.RFC3339)
			row.LastBuiltAt = &ts
			if rr.ledgerCount.Valid {
				c := rr.ledgerCount.Int64
				row.MemberCount = &c
			}
			row.Freshness = computeSegmentFreshness(&t, now)
		} else {
			row.Freshness = computeSegmentFreshness(nil, now)
		}
		if ts, ok := stamps[row.SegmentID]; ok {
			v := ts.UTC().Format(time.RFC3339)
			row.MembersStampedAt = &v
		}
		if st, ok := refreshStates[row.SegmentID]; ok {
			row.RefreshState = &st
		}
		resp.Rows = append(resp.Rows, row)
	}

	resp.Worker = s.loadWorkerStatus(r)
	respondJSON(w, http.StatusOK, resp)
}

// loadMemberStamps reads max(materialized_at) per segment — the REAL member
// stamp, cross-checking the ledger. Served by idx_segment_members_seg_stamp
// (segment_id, materialized_at DESC — concurrentIndexSpecs, cmd/server/
// main.go): one backward index probe per segment, never a member-table scan.
// Bounded to 10s; any failure degrades to "no stamps" (nulls), never a guess.
func (s *SegmentFreshnessService) loadMemberStamps(r *http.Request, ids []string) map[string]time.Time {
	out := make(map[string]time.Time, len(ids))
	if len(ids) == 0 {
		return out
	}
	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT segment_id::text, MAX(materialized_at)
		FROM mailing_segment_members
		WHERE segment_id = ANY($1::uuid[])
		GROUP BY segment_id
	`, pq.Array(ids))
	if err != nil {
		log.Printf("[segment-freshness] member-stamp read failed (degrading to null): %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ts sql.NullTime
		if err := rows.Scan(&id, &ts); err != nil {
			continue
		}
		if ts.Valid {
			out[id] = ts.Time
		}
	}
	if err := rows.Err(); err != nil {
		// Partial stamps are individually true (each row is a real MAX), so
		// keep what arrived; missing segments simply render null.
		log.Printf("[segment-freshness] member-stamp read truncated: %v", err)
	}
	return out
}

// loadOpenRefreshStates returns the open (queued/running) request state per
// segment. The uq_seg_refresh_req_open unique index guarantees at most one.
func (s *SegmentFreshnessService) loadOpenRefreshStates(r *http.Request, ids []string) map[string]string {
	out := make(map[string]string, 4)
	if len(ids) == 0 {
		return out
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT segment_id::text, status
		FROM mailing_segment_refresh_requests
		WHERE status IN ('queued','running') AND segment_id = ANY($1::uuid[])
	`, pq.Array(ids))
	if err != nil {
		// Table may not exist until startup migrations land — degrade quietly.
		if pqErr, ok := err.(*pq.Error); !ok || pqErr.Code != "42P01" {
			log.Printf("[segment-freshness] refresh-state read failed: %v", err)
		}
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, st string
		if rows.Scan(&id, &st) == nil {
			out[id] = st
		}
	}
	return out
}

// loadWorkerStatus merges the in-process worker state (running/leader/
// degraded on THIS instance) with the cluster-wide run history
// (mailing_worker_runs is written by whichever instance holds the lock).
func (s *SegmentFreshnessService) loadWorkerStatus(r *http.Request) segmentFreshnessWorker {
	st := worker.SegmentGridState()
	out := segmentFreshnessWorker{
		Running:  st.Running,
		Leader:   st.Leader,
		Degraded: st.Degraded,
	}
	if !st.LastPassAt.IsZero() {
		ts := st.LastPassAt.UTC().Format(time.RFC3339)
		out.LastPassAt = &ts
		out.LastPassOutcome = st.LastPassOutcome
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()
	var startedAt sql.NullTime
	var status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT started_at, status FROM mailing_worker_runs
		WHERE worker_name = 'segment_grid'
		ORDER BY started_at DESC LIMIT 1
	`).Scan(&startedAt, &status)
	if err == nil && startedAt.Valid {
		// Cluster-wide truth wins when it is newer than this instance's view.
		if st.LastPassAt.IsZero() || startedAt.Time.After(st.LastPassAt) {
			ts := startedAt.Time.UTC().Format(time.RFC3339)
			out.LastPassAt = &ts
			out.LastPassOutcome = status.String
		}
	} else if err != nil && err != sql.ErrNoRows {
		if pqErr, ok := err.(*pq.Error); !ok || pqErr.Code != "42P01" {
			log.Printf("[segment-freshness] worker-run read failed: %v", err)
		}
	}
	var hbStatus sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT last_status FROM mailing_worker_heartbeats WHERE worker_name = 'segment_grid'
	`).Scan(&hbStatus); err == nil && hbStatus.String == "degraded" {
		out.Degraded = true
	}
	return out
}

// contextWithTimeout derives a bounded child of the request context.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// ---------------------------------------------------------------------------
// POST /api/mailing/segments/refresh
// ---------------------------------------------------------------------------

type segmentRefreshRequestBody struct {
	SegmentIDs []string `json:"segment_ids"`
	Family     string   `json:"family"`
}

type segmentRefreshQueued struct {
	SegmentID string `json:"segment_id"`
	RequestID string `json:"request_id"`
}

type segmentRefreshSkipped struct {
	SegmentID string `json:"segment_id"`
	Reason    string `json:"reason"`
}

type segmentRefreshResponse struct {
	APIVersion     string                  `json:"api_version"`
	Queued         []segmentRefreshQueued  `json:"queued"`
	AlreadyPending []string                `json:"already_pending"`
	Skipped        []segmentRefreshSkipped `json:"skipped"`
}

// segFamilyRe validates the family code ('DB', 'BCC', …).
var segFamilyRe = regexp.MustCompile(`^[A-Z]{2,4}$`)

// maxRefreshBatch bounds one request's fan-out.
const maxRefreshBatch = 250

// HandleSegmentRefreshRequest — POST /api/mailing/segments/refresh
// {"segment_ids":[...]} or {"family":"<CODE>"} → 202 with what was queued.
// Idempotent: an already queued/running segment is reported in
// already_pending (ON CONFLICT DO NOTHING against uq_seg_refresh_req_open),
// never duplicated.
func (s *SegmentFreshnessService) HandleSegmentRefreshRequest(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return
	}
	var body segmentRefreshRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.SegmentIDs) == 0 && strings.TrimSpace(body.Family) == "" {
		respondError(w, http.StatusBadRequest, "segment_ids or family is required")
		return
	}
	ctx := r.Context()

	// Resolve targets to org-owned ACTIVE segments.
	type target struct{ id, name string }
	var targets []target
	resp := segmentRefreshResponse{
		APIVersion:     VersionSegmentFreshnessAPI,
		Queued:         []segmentRefreshQueued{},
		AlreadyPending: []string{},
		Skipped:        []segmentRefreshSkipped{},
	}

	if fam := strings.TrimSpace(body.Family); fam != "" {
		if !segFamilyRe.MatchString(fam) {
			respondError(w, http.StatusBadRequest, "family must be a 2-4 letter uppercase brand code")
			return
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT id::text, name FROM mailing_segments
			WHERE organization_id = $1 AND status = 'active'
			  AND name ~ ('^' || $2 || ' (7|14|30|60)D (Openers|Clickers)$')
			ORDER BY name
		`, orgID, fam)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "family lookup failed: "+err.Error())
			return
		}
		for rows.Next() {
			var t target
			if rows.Scan(&t.id, &t.name) == nil {
				targets = append(targets, t)
			}
		}
		familyReadErr := rows.Err()
		rows.Close()
		if familyReadErr != nil {
			respondError(w, http.StatusInternalServerError, "family lookup truncated: "+familyReadErr.Error())
			return
		}
		if len(targets) == 0 {
			respondError(w, http.StatusNotFound, "no active grid segments for family "+fam)
			return
		}
	}

	if len(body.SegmentIDs) > 0 {
		if len(body.SegmentIDs) > maxRefreshBatch {
			respondError(w, http.StatusBadRequest, "too many segment_ids (max 250)")
			return
		}
		valid := make([]string, 0, len(body.SegmentIDs))
		for _, raw := range body.SegmentIDs {
			if _, perr := uuid.Parse(raw); perr != nil {
				resp.Skipped = append(resp.Skipped, segmentRefreshSkipped{SegmentID: raw, Reason: "invalid uuid"})
				continue
			}
			valid = append(valid, raw)
		}
		if len(valid) > 0 {
			rows, err := s.db.QueryContext(ctx, `
				SELECT id::text, name, status FROM mailing_segments
				WHERE organization_id = $1 AND id = ANY($2::uuid[])
			`, orgID, pq.Array(valid))
			if err != nil {
				respondError(w, http.StatusInternalServerError, "segment lookup failed: "+err.Error())
				return
			}
			found := map[string]bool{}
			for rows.Next() {
				var t target
				var status string
				if rows.Scan(&t.id, &t.name, &status) != nil {
					continue
				}
				found[t.id] = true
				if status != "active" {
					resp.Skipped = append(resp.Skipped, segmentRefreshSkipped{SegmentID: t.id, Reason: "segment is not active"})
					continue
				}
				targets = append(targets, t)
			}
			idReadErr := rows.Err()
			rows.Close()
			if idReadErr != nil {
				respondError(w, http.StatusInternalServerError, "segment lookup truncated: "+idReadErr.Error())
				return
			}
			for _, id := range valid {
				if !found[id] {
					resp.Skipped = append(resp.Skipped, segmentRefreshSkipped{SegmentID: id, Reason: "not found in this organization"})
				}
			}
		}
	}

	// Queue each target. ON CONFLICT against the open-slot partial unique
	// index makes duplicates report as already_pending (0 rows inserted).
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.id] {
			continue
		}
		seen[t.id] = true
		reqID := uuid.New().String()
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO mailing_segment_refresh_requests
				(id, segment_id, organization_id, source, status, requested_at)
			VALUES ($1::uuid, $2::uuid, $3, 'ui', 'queued', NOW())
			ON CONFLICT (segment_id) WHERE status IN ('queued','running') DO NOTHING
		`, reqID, t.id, orgID)
		if err != nil {
			resp.Skipped = append(resp.Skipped, segmentRefreshSkipped{SegmentID: t.id, Reason: "queue insert failed: " + err.Error()})
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			resp.AlreadyPending = append(resp.AlreadyPending, t.id)
			continue
		}
		resp.Queued = append(resp.Queued, segmentRefreshQueued{SegmentID: t.id, RequestID: reqID})
	}

	respondJSON(w, http.StatusAccepted, resp)
}

package api

// Property Ledger — lane ROSTER (which sending domain mails which drip).
//
// partner_drip_vertical_roster is the LIVE binding between a drip vertical and
// a sending-domain brand code. The orchestrator refreshes it once per tick and
// BOTH passes consult it:
//   - loadVerticalRosters   internal/worker/partner_drip_orchestrator.go:2476-2495
//     (SELECT ... GREATEST(1, LEAST(COALESCE(weight,1), 20)) ... WHERE active)
//   - dynamicRosterFor      internal/worker/partner_drip_orchestrator.go:131-143
//     (welcome pass via brandRosterFor + follow-up pass via pickNextFollowupBrand)
// So a row written here changes live sending behavior on the NEXT TICK with no
// deploy. That is the whole point of the surface — and the reason every write
// is gated (see the env flag below) and audit-logged.
//
// ── STANDING OPERATOR CONSTRAINT: ADDITIVE ONLY, NO MAJOR WRITES OR DELETES ──
// Unassign is a SOFT DISABLE: it sets active=false and NEVER deletes the row.
// The row is the historical record of the binding (updated_by/updated_at carry
// who unassigned it and when), and the orchestrator already filters on
// `WHERE active`, so flipping the flag is sufficient to stop the lane. A DELETE
// would destroy that history and is forbidden in this file — the test suite
// asserts the unassign SQL contains no DELETE.
//
// ── Normalization ────────────────────────────────────────────────────────────
// The orchestrator reads `lower(btrim(vertical))` / `lower(btrim(brand))`, so
// every predicate here matches the SAME way and every write stores the already
// lower/trimmed form. A row written as "Consumer " would otherwise be invisible
// to a lookup for "consumer" while still being loaded by the orchestrator.
//
// ── Org scoping ──────────────────────────────────────────────────────────────
// getOrgID(r) is resolved on every handler and echoed in the response + the
// audit after-state. It is NOT a SQL predicate here because the table carries
// no organization column (verified against information_schema: vertical, brand,
// sort_order, active, updated_by, updated_at, weight). The whole partner-drip
// estate is single-tenant on the fallback org; adding a phantom org filter
// would silently return zero rows.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// laneRosterWriteFlagEnv gates the roster EDIT surface, mirroring
// laneThrottleWriteFlagEnv (property_lane_supply.go:476-481) exactly: ships
// UNSET = read-only panel with an honest banner naming the env var; setting it
// to "1" is the one-move enable and unsetting it is the one-move rollback.
// A dedicated flag (not the throttle one) so roster edits can be enabled or
// rolled back without touching the throttle posture.
const laneRosterWriteFlagEnv = "PROPERTY_LEDGER_ROSTER_WRITE_ENABLED"

func laneRosterWriteEnabled() bool {
	return os.Getenv(laneRosterWriteFlagEnv) == "1"
}

// Weight clamp — mirrors the orchestrator's GREATEST(1, LEAST(COALESCE(weight,1), 20))
// (partner_drip_orchestrator.go:2478). Clamping SERVER-SIDE means the stored
// value equals the value the orchestrator will actually honor; storing 999 and
// silently sending 20 is the "column written, never read as written" defect.
const (
	laneRosterWeightMin = 1
	laneRosterWeightMax = 20
)

func clampLaneRosterWeight(w int) int {
	if w < laneRosterWeightMin {
		return laneRosterWeightMin
	}
	if w > laneRosterWeightMax {
		return laneRosterWeightMax
	}
	return w
}

type laneRosterRow struct {
	Vertical string `json:"vertical"`
	// DisplayName is the operator label from partner_drip_lane_labels
	// (PUT …/property-ledger/lane-label). BEST-EFFORT: empty when unlabeled
	// or when the table doesn't exist yet (overseer-owned DDL) — the
	// vertical key is always the identity.
	DisplayName string    `json:"display_name,omitempty"`
	Brand       string    `json:"brand"`
	Weight      int       `json:"weight"`
	Active      bool      `json:"active"`
	SortOrder   int       `json:"sort_order"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

// laneRosterListSQL returns EVERY matching row including active=false ones —
// the UI has to show what is currently unassigned in order to offer re-assign,
// and an inactive row is the soft-disable tombstone, not a deleted binding.
// Weight is clamped in SQL with the orchestrator's exact expression so the
// screen shows the EFFECTIVE weight, not a stored value that never applies.
const laneRosterListSQL = `
	SELECT lower(btrim(vertical)), lower(btrim(brand)),
	       GREATEST(1, LEAST(COALESCE(weight, 1), 20)),
	       active, COALESCE(sort_order, 0), updated_at, COALESCE(updated_by, '')
	FROM partner_drip_vertical_roster
	WHERE ($1 = '' OR lower(btrim(brand)) = $1)
	  AND ($2 = '' OR lower(btrim(vertical)) = $2)
	ORDER BY vertical, sort_order, brand`

// laneRosterAssignPreflightSQL answers both write preconditions in ONE row:
//   - vertical_known: the vertical exists in the roster OR in partner_datasets.
//     Rejecting an unknown vertical is what stops a typo from creating a
//     PHANTOM LANE — a roster row for a vertical no dataset feeds is invisible
//     in every ledger surface yet loaded by the orchestrator every tick.
//   - cur.*: the current row (NULL when none) for the audit before-state.
//
// The LEFT JOIN off a one-row source guarantees exactly one result row whether
// or not the (vertical, brand) binding already exists.
const laneRosterAssignPreflightSQL = `
	SELECT (EXISTS (SELECT 1 FROM partner_drip_vertical_roster r
	                 WHERE lower(btrim(r.vertical)) = $1)
	        OR EXISTS (SELECT 1 FROM partner_datasets d
	                    WHERE lower(btrim(d.vertical)) = $1)) AS vertical_known,
	       cur.weight, cur.active, cur.sort_order
	FROM (SELECT 1) AS one
	LEFT JOIN partner_drip_vertical_roster cur
	       ON lower(btrim(cur.vertical)) = $1
	      AND lower(btrim(cur.brand)) = $2`

// laneRosterUpsertSQL — conflict target VERIFIED against the live schema, not
// assumed: partner_drip_vertical_roster_pkey is
// `PRIMARY KEY (vertical, brand)` (pg_constraint / pg_indexes, us-west-2 prod,
// 2026-08-19), so ON CONFLICT (vertical, brand) resolves against the PK and no
// UPDATE-then-INSERT transaction fallback is needed.
// active is forced back to TRUE: re-assigning a soft-disabled lane is the
// documented way to bring it back, and it is the exact inverse of unassign.
const laneRosterUpsertSQL = `
	INSERT INTO partner_drip_vertical_roster
	       (vertical, brand, weight, sort_order, active, updated_by, updated_at)
	VALUES ($1, $2, $3, $4, true, $5, NOW())
	ON CONFLICT (vertical, brand) DO UPDATE SET
	       active     = true,
	       weight     = EXCLUDED.weight,
	       sort_order = EXCLUDED.sort_order,
	       updated_by = EXCLUDED.updated_by,
	       updated_at = NOW()
	RETURNING weight, active, COALESCE(sort_order, 0), updated_at, COALESCE(updated_by, '')`

// laneRosterUnassignSQL — SOFT DISABLE ONLY.
//
// *** THIS STATEMENT MUST NEVER BECOME A DELETE. ***
// Operator standing constraint for this whole body of work: additive only, no
// major writes or deletes. The orchestrator filters `WHERE active`
// (partner_drip_orchestrator.go:2482), so active=false fully stops the lane on
// the next tick while preserving the binding's history and its updated_by/
// updated_at provenance. property_lane_roster_test.go asserts this constant
// contains no DELETE.
const laneRosterUnassignSQL = `
	UPDATE partner_drip_vertical_roster
	SET active = false, updated_by = $3, updated_at = NOW()
	WHERE lower(btrim(vertical)) = $1 AND lower(btrim(brand)) = $2
	RETURNING GREATEST(1, LEAST(COALESCE(weight, 1), 20)), COALESCE(sort_order, 0), updated_at`

// laneRosterWriteGate returns false and writes the 403 when the env flag is not
// armed. Called as the FIRST statement of every write handler so that a gated
// request executes ZERO SQL (proved by sqlmock ExpectationsWereMet in the
// negative test) — the gate is a real gate, not a late-stage refusal after the
// row has already moved.
func laneRosterWriteGate(w http.ResponseWriter) bool {
	if laneRosterWriteEnabled() {
		return true
	}
	respondError(w, http.StatusForbidden,
		"roster writes are disabled: set the server env "+laneRosterWriteFlagEnv+
			"=1 to enable (unsetting it is the one-move rollback)")
	return false
}

// HandleLaneRosterList GET …/property-ledger/lane-roster?brand=<code>&vertical=<v>
//
// At least one of brand / vertical is required — an unfiltered dump of every
// binding is not a screen this surface serves and would grow unbounded.
// Returns INACTIVE rows too (see laneRosterListSQL).
//
// The vertical filter is NOT existence-checked here: a typo on a READ returns
// an empty list, which is harmless. Existence is enforced on the WRITE path,
// where a typo would mint a phantom lane.
func (s *PMTACampaignService) HandleLaneRosterList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)
	brand := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand")))
	vertical := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("vertical")))

	if brand == "" && vertical == "" {
		respondError(w, http.StatusBadRequest, "brand or vertical required")
		return
	}
	if brand != "" && !propertyLedgerValidLaneBrand(r.Context(), s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}

	rows, err := s.db.QueryContext(ctx, laneRosterListSQL, brand, vertical)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "roster query failed")
		return
	}
	defer rows.Close()

	out := []laneRosterRow{}
	for rows.Next() {
		var lr laneRosterRow
		if err := rows.Scan(&lr.Vertical, &lr.Brand, &lr.Weight, &lr.Active,
			&lr.SortOrder, &lr.UpdatedAt, &lr.UpdatedBy); err != nil {
			// Never a silent partial-200: a half-scanned roster reads as
			// "this brand is not on that drip", which is a sending decision.
			respondError(w, http.StatusInternalServerError, "roster scan failed")
			return
		}
		out = append(out, lr)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "roster query failed")
		return
	}

	// Operator lane labels — best-effort (laneLabelMap degrades to empty on
	// any error, incl. the table not existing yet), never fails the roster.
	labels := laneLabelMap(ctx, s.db)
	for i := range out {
		out[i].DisplayName = labels[out[i].Vertical]
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"rows":            out,
		"brand":           brand,
		"vertical":        vertical,
		"organization_id": orgID,
		// The LIST stays live with the flag unset; write_enabled is what the UI
		// reads to decide whether to render the edit surface at all.
		"write_enabled":  laneRosterWriteEnabled(),
		"write_flag_env": laneRosterWriteFlagEnv,
	})
}

// HandleLaneRosterAssign POST …/property-ledger/lane-roster/assign
// body {"vertical":"...","brand":"...","weight":1,"sort_order":0}
// UPSERT on the (vertical, brand) primary key; re-activates a soft-disabled row.
func (s *PMTACampaignService) HandleLaneRosterAssign(w http.ResponseWriter, r *http.Request) {
	if !laneRosterWriteGate(w) {
		return
	}
	ctx := r.Context()
	orgID := getOrgID(r)

	var req struct {
		Vertical  string `json:"vertical"`
		Brand     string `json:"brand"`
		Weight    int    `json:"weight"`
		SortOrder int    `json:"sort_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	if !propertyLedgerValidLaneBrand(r.Context(), s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	if vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required")
		return
	}
	if req.SortOrder < 0 {
		respondError(w, http.StatusBadRequest, "sort_order must be >= 0")
		return
	}
	weight := clampLaneRosterWeight(req.Weight)
	actor := actorFromRequest(r)

	var (
		verticalKnown bool
		curWeight     sql.NullInt64
		curActive     sql.NullBool
		curSortOrder  sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, laneRosterAssignPreflightSQL, vertical, brand).
		Scan(&verticalKnown, &curWeight, &curActive, &curSortOrder); err != nil {
		respondError(w, http.StatusInternalServerError, "roster preflight failed")
		return
	}
	if !verticalKnown {
		respondError(w, http.StatusBadRequest,
			"unknown vertical: no partner_drip_vertical_roster row and no partner_datasets feed uses it — "+
				"a typo here would create a phantom lane the orchestrator loads every tick")
		return
	}

	var after laneRosterRow
	after.Vertical = vertical
	after.Brand = brand
	if err := s.db.QueryRowContext(ctx, laneRosterUpsertSQL,
		vertical, brand, weight, req.SortOrder, actor).
		Scan(&after.Weight, &after.Active, &after.SortOrder, &after.UpdatedAt, &after.UpdatedBy); err != nil {
		respondError(w, http.StatusInternalServerError, "roster upsert failed")
		return
	}

	before := map[string]interface{}{"exists": curWeight.Valid}
	if curWeight.Valid {
		before["weight"] = curWeight.Int64
		before["active"] = curActive.Bool
		before["sort_order"] = curSortOrder.Int64
	}
	writeAuditLog(ctx, s.db, actor, "property_lane_roster_assign",
		"partner_drip_vertical_roster", vertical+"/"+brand,
		before,
		map[string]interface{}{
			"weight": after.Weight, "active": after.Active,
			"sort_order": after.SortOrder, "weight_requested": req.Weight,
			"organization_id": orgID,
		})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":  true,
		"row": after,
		// Echoed so the UI can show "you asked for 999, the lane runs at 20"
		// instead of silently disagreeing with the orchestrator.
		"weight_requested": req.Weight,
		"weight_clamped":   after.Weight != req.Weight,
		"organization_id":  orgID,
	})
}

// HandleLaneRosterUnassign POST …/property-ledger/lane-roster/unassign
// body {"vertical":"...","brand":"..."}
//
// SOFT-DISABLE ONLY — sets active=false, NEVER deletes the row (operator
// standing constraint: additive only, no major writes or deletes). See
// laneRosterUnassignSQL.
func (s *PMTACampaignService) HandleLaneRosterUnassign(w http.ResponseWriter, r *http.Request) {
	if !laneRosterWriteGate(w) {
		return
	}
	ctx := r.Context()
	orgID := getOrgID(r)

	var req struct {
		Vertical string `json:"vertical"`
		Brand    string `json:"brand"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	if !propertyLedgerValidLaneBrand(r.Context(), s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	if vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required")
		return
	}
	actor := actorFromRequest(r)

	var row laneRosterRow
	row.Vertical = vertical
	row.Brand = brand
	row.Active = false
	err := s.db.QueryRowContext(ctx, laneRosterUnassignSQL, vertical, brand, actor).
		Scan(&row.Weight, &row.SortOrder, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "no roster row for (vertical, brand)")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "roster unassign failed")
		return
	}
	row.UpdatedBy = actor

	writeAuditLog(ctx, s.db, actor, "property_lane_roster_unassign",
		"partner_drip_vertical_roster", vertical+"/"+brand,
		map[string]interface{}{"active": true},
		map[string]interface{}{
			"active": false, "soft_disable": true, "deleted": false,
			"organization_id": orgID,
		})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":  true,
		"row": row,
		// Explicit so the UI (and any reader of this response) can see the row
		// still exists — this is a disable, not a delete.
		"soft_disabled":   true,
		"deleted":         false,
		"organization_id": orgID,
	})
}

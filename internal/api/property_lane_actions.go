package api

// Property Ledger — lane ACTIONS (journey advance + lane-level pause/resume).
//
// Two operator levers over the partner-drip ladder, both scoped to a VERTICAL
// (a drip lane) and resolved to its active partner_datasets rows first —
// partner_clean_queue.vertical is UNINDEXED at ~11.2M rows (docs/JAOS/
// drip-lanes.md §7: "resolve to dataset_id and filter on that; loop per
// dataset"), so no statement here ever filters pcq by vertical directly.
//
//   1. journey/advance — make up to `limit` WAITING follow-up rows due NOW by
//      rewinding next_touch_at. The orchestrator's follow-up pass claims rows
//      where next_touch_at <= NOW() (partner_drip_orchestrator.go:4366-4367),
//      so advanced rows are picked up on its next tick (TickInterval default
//      15 minutes — :373). This endpoint never sends anything itself.
//   2. lane-pause — loop the vertical's active datasets applying the SAME
//      UPDATE the per-dataset emergency stop applies
//      (partner_admin_handlers.go HandleEmergencyStopDataset :336 /
//      HandleResumeDataset :365).
//
// ── What paused_emergency actually stops (verified 2026-08-22) ──────────────
// The flag is consulted at THREE claim points, so a lane pause stops the whole
// pipeline for the dataset — including follow-up touches already laddered:
//   - ingest slicing: partner_slicer.go:224 claimNextBatch requires
//     `d.paused_emergency = false` (and isDatasetPaused :589 re-checks
//     mid-batch), so no new records are sliced into the queue;
//   - welcome (first-touch) claims: datasetNotEmergencyPausedSQL
//     (partner_drip_orchestrator.go:2149-2155) is appended to the
//     status='ready' claim in claimRecords;
//   - FOLLOW-UP claims: the same predicate is appended to the status='mailed'
//     follow-up claim (pinned by internal/worker/
//     partner_drip_emergency_stop_test.go:119) — so YES, pause stops
//     follow-ups too; a ladder already in motion halts until resume.
//
// Both writes are gated behind the SAME env flag as the roster edits
// (PROPERTY_LEDGER_ROSTER_WRITE_ENABLED — laneRosterWriteGate,
// property_lane_roster.go): they change live sending behavior with no deploy,
// exactly like a roster write.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneActionDatasetsSQL resolves a vertical to its ACTIVE dataset ids — the
// mandatory first step before touching partner_clean_queue (unindexed
// vertical column, see file header). Deterministic order so a limited advance
// consumes datasets in a stable sequence.
const laneActionDatasetsSQL = `
	SELECT id::text
	FROM partner_datasets
	WHERE lower(btrim(vertical)) = $1 AND status = 'active'
	ORDER BY created_at, id`

// laneJourneyAdvanceSQL rewinds up to $3 WAITING rows of one dataset to due-now.
// Predicates mirror the orchestrator's follow-up claim
// (partner_drip_orchestrator.go:4244: status='mailed', engaged_at IS NULL,
// terminal_reason IS NULL, next_touch_at <= NOW()) with next_touch_at > NOW()
// selecting only rows still WAITING — a row already due is already the
// orchestrator's to claim, and re-stamping it would reorder its queue position.
// FOR UPDATE SKIP LOCKED so an advance never blocks on (or double-touches) rows
// a concurrent orchestrator claim is holding.
const laneJourneyAdvanceSQL = `
	UPDATE partner_clean_queue
	SET next_touch_at = NOW()
	WHERE id IN (
		SELECT id
		FROM partner_clean_queue
		WHERE dataset_id = $1::uuid
		  AND status = 'mailed'
		  AND touch_count = $2
		  AND next_touch_at > NOW()
		  AND engaged_at IS NULL
		  AND terminal_reason IS NULL
		ORDER BY next_touch_at ASC
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)`

// laneJourneyAdvanceBrandSQL is the brand-filtered variant. The brand
// expression matches the orchestrator's follow-up brand identity exactly
// (COALESCE(NULLIF(last_touch_brand,''), mailed_brand) —
// partner_drip_orchestrator.go:4360): last_touch_brand once a follow-up has
// fired, mailed_brand for rows still waiting on touch 2.
const laneJourneyAdvanceBrandSQL = `
	UPDATE partner_clean_queue
	SET next_touch_at = NOW()
	WHERE id IN (
		SELECT id
		FROM partner_clean_queue
		WHERE dataset_id = $1::uuid
		  AND status = 'mailed'
		  AND touch_count = $2
		  AND next_touch_at > NOW()
		  AND engaged_at IS NULL
		  AND terminal_reason IS NULL
		  AND COALESCE(NULLIF(last_touch_brand, ''), mailed_brand) = $4
		ORDER BY next_touch_at ASC
		LIMIT $3
		FOR UPDATE SKIP LOCKED
	)`

// Advance bounds. Touch is the touch_count the rows currently carry: rows at
// MaxTouchCount (5) have no next touch (next_touch_at is NULL — the ladder
// ejects them), so only 1..MaxTouchCount-1 are advanceable.
const (
	laneJourneyAdvanceMaxTouch = worker.MaxTouchCount - 1 // 4
	laneJourneyAdvanceMaxLimit = 50000
)

const laneJourneyAdvanceNote = "Rows are now due (next_touch_at = NOW()); nothing was sent by this call. " +
	"The drip orchestrator's follow-up pass claims due rows on its next tick (default every 15 minutes)."

// lanePauseSQL / laneResumeSQL — byte-for-byte the semantics of
// HandleEmergencyStopDataset / HandleResumeDataset (partner_admin_handlers.go
// :336 / :365), applied per dataset id. See the file header for exactly what
// this flag stops (ingest slicing + welcome claims + follow-up claims).
const lanePauseSQL = `
	UPDATE partner_datasets
	SET paused_emergency = true,
	    paused_reason = $2,
	    paused_at = NOW(),
	    updated_at = NOW()
	WHERE id = $1::uuid`

const laneResumeSQL = `
	UPDATE partner_datasets
	SET paused_emergency = false,
	    paused_reason = NULL,
	    paused_at = NULL,
	    updated_at = NOW()
	WHERE id = $1::uuid`

// laneActionResolveDatasets returns the vertical's active dataset ids in
// deterministic order. Vertical must already be lower/trimmed.
func (s *PMTACampaignService) laneActionResolveDatasets(r *http.Request, vertical string) ([]string, error) {
	rows, err := s.db.QueryContext(r.Context(), laneActionDatasetsSQL, vertical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type laneJourneyAdvanceByDataset struct {
	DatasetID string `json:"dataset_id"`
	Count     int64  `json:"count"`
}

// HandleJourneyAdvance POST …/property-ledger/journey/advance
// body {"vertical":"...","touch":1..4,"limit":1..50000,"brand":"(optional)"}
//
// Makes up to `limit` WAITING rows (status='mailed', touch_count=touch,
// next_touch_at in the future, not engaged, not terminal) due now, looping the
// vertical's active datasets until the limit is consumed.
func (s *PMTACampaignService) HandleJourneyAdvance(w http.ResponseWriter, r *http.Request) {
	if !laneRosterWriteGate(w) {
		return
	}
	ctx := r.Context()
	orgID := getOrgID(r)

	var req struct {
		Vertical string `json:"vertical"`
		Touch    int    `json:"touch"`
		Limit    int    `json:"limit"`
		Brand    string `json:"brand"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	if !isValidVertical(vertical) {
		respondError(w, http.StatusBadRequest, "unknown vertical: must be one of the PartnerVerticals slugs")
		return
	}
	if req.Touch < 1 || req.Touch > laneJourneyAdvanceMaxTouch {
		respondError(w, http.StatusBadRequest, "touch must be 1..4 (the touch_count the waiting rows currently carry)")
		return
	}
	if req.Limit < 1 || req.Limit > laneJourneyAdvanceMaxLimit {
		respondError(w, http.StatusBadRequest, "limit is required and must be 1..50000")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	if brand != "" && !propertyLedgerValidLaneBrand(ctx, s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the drip roster codes)")
		return
	}

	datasetIDs, err := s.laneActionResolveDatasets(r, vertical)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "dataset resolution failed")
		return
	}
	if len(datasetIDs) == 0 {
		respondError(w, http.StatusNotFound, "vertical has no active datasets")
		return
	}

	var advanced int64
	remaining := int64(req.Limit)
	byDataset := []laneJourneyAdvanceByDataset{}
	for _, dsID := range datasetIDs {
		if remaining <= 0 {
			break
		}
		var res sql.Result
		var execErr error
		if brand == "" {
			res, execErr = s.db.ExecContext(ctx, laneJourneyAdvanceSQL, dsID, req.Touch, remaining)
		} else {
			res, execErr = s.db.ExecContext(ctx, laneJourneyAdvanceBrandSQL, dsID, req.Touch, remaining, brand)
		}
		if execErr != nil {
			respondError(w, http.StatusInternalServerError, "journey advance failed on dataset "+dsID)
			return
		}
		n, _ := res.RowsAffected()
		byDataset = append(byDataset, laneJourneyAdvanceByDataset{DatasetID: dsID, Count: n})
		advanced += n
		remaining -= n
	}

	actor := actorFromRequest(r)
	writeAuditLog(ctx, s.db, actor, "journey_advance",
		"partner_clean_queue", vertical,
		nil,
		map[string]interface{}{
			"vertical": vertical, "touch": req.Touch, "limit": req.Limit,
			"brand": brand, "advanced": advanced, "by_dataset": byDataset,
			"organization_id": orgID,
		})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"advanced":        advanced,
		"by_dataset":      byDataset,
		"vertical":        vertical,
		"touch":           req.Touch,
		"limit":           req.Limit,
		"brand":           brand,
		"note":            laneJourneyAdvanceNote,
		"organization_id": orgID,
	})
}

// HandleLanePause POST …/property-ledger/lane-pause
// body {"vertical":"...","pause":true|false,"reason":"(required when pausing)"}
//
// Lane-level convenience over the per-dataset emergency stop: loops the
// vertical's active datasets applying the exact HandleEmergencyStopDataset /
// HandleResumeDataset UPDATE. paused_emergency blocks the ingest slicer's
// batch claim AND both drip claim passes — welcome and follow-ups — so a
// paused lane's ladders stop mid-flight too (see file header for the three
// consultation points, with file:line).
func (s *PMTACampaignService) HandleLanePause(w http.ResponseWriter, r *http.Request) {
	if !laneRosterWriteGate(w) {
		return
	}
	ctx := r.Context()
	orgID := getOrgID(r)

	var req struct {
		Vertical string `json:"vertical"`
		Pause    *bool  `json:"pause"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	if !isValidVertical(vertical) {
		respondError(w, http.StatusBadRequest, "unknown vertical: must be one of the PartnerVerticals slugs")
		return
	}
	if req.Pause == nil {
		respondError(w, http.StatusBadRequest, "pause is required (true to pause, false to resume)")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if *req.Pause && reason == "" {
		respondError(w, http.StatusBadRequest, "reason is required when pausing")
		return
	}

	datasetIDs, err := s.laneActionResolveDatasets(r, vertical)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "dataset resolution failed")
		return
	}
	if len(datasetIDs) == 0 {
		respondError(w, http.StatusNotFound, "vertical has no active datasets")
		return
	}

	var affected int64
	for _, dsID := range datasetIDs {
		var execErr error
		if *req.Pause {
			_, execErr = s.db.ExecContext(ctx, lanePauseSQL, dsID, reason)
		} else {
			_, execErr = s.db.ExecContext(ctx, laneResumeSQL, dsID)
		}
		if execErr != nil {
			respondError(w, http.StatusInternalServerError, "lane pause update failed on dataset "+dsID)
			return
		}
		affected++
	}

	action := "lane_resume"
	if *req.Pause {
		action = "lane_pause"
	}
	actor := actorFromRequest(r)
	writeAuditLog(ctx, s.db, actor, action,
		"partner_dataset", vertical,
		nil,
		map[string]interface{}{
			"vertical": vertical, "paused_emergency": *req.Pause, "reason": reason,
			"dataset_ids": datasetIDs, "organization_id": orgID,
		})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":                true,
		"vertical":          vertical,
		"paused_emergency":  *req.Pause,
		"reason":            reason,
		"datasets_affected": affected,
		"dataset_ids":       datasetIDs,
		// Honest semantics: this stops ingest slicing AND both drip claim
		// passes (welcome + follow-ups) for every dataset of the lane.
		"scope_note":      "paused_emergency blocks the slicer batch claim and BOTH drip claim passes (welcome and follow-up) — ladders already in motion stop until resume",
		"organization_id": orgID,
	})
}

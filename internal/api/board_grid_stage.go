package api

// Board Grid STAGE — turn an edited grid proposal into scheduled campaigns.
//
//	POST /api/mailing/board-grid/stage
//	{ "date": "YYYY-MM-DD", "confirmed": bool,
//	  "slot_times"?: { "01:01": "02:00", ... },   // BOARD-WIDE column remap
//	  "throttle_strategy"?: "gentle|auto|even",   // BOARD-WIDE
//	  "window_hours"?: 1..16,                     // BOARD-WIDE
//	  "cells": [ { "property", "slot", "name", "source_campaign_id",
//	               "offer_id"?, "proof_id"?, "inclusion_segments"?: [uuid] } ] }
//
// TIMING + THROTTLE ARE BOARD-LEVEL BY OPERATOR RULING (2026-08-23): they
// apply to the ENTIRE board, never per cell. slot_times remaps a slot COLUMN's
// Denver time for every cell in that column; throttle_strategy overrides the
// input's (and every plan's) strategy; window_hours replaces each plan's
// absolute spans with ONE [target, target+window] span whose source is
// 'duration-calc' — the duration changed, so preserving the old source value
// would lie to the wave sanity contract. AUDIENCE is the one per-cell
// override: inclusion_segments replaces the source blob's audience program.
//
// This is the missing half of the clone/proposal loop (operator defect
// 2026-08-22: "Re-run Gates gives no feedback and nothing schedules"). The
// grid's gates AUDIT a proposal; this endpoint EXECUTES it — each cell becomes
// a real campaign, deployed id-less through the FULL existing gated path.
//
// Per cell, sequentially:
//
//	(a) load the SOURCE campaign's pmta_config->'campaign_input' blob — the
//	    byte-faithful deploy payload (same doctrine as day_cards_rebuild.go:
//	    a missing blob is a hard per-cell failure, never a degraded rebuild
//	    from DB columns, which loses audience/priority/cadence fields).
//	(b) apply the cell's overrides: offer_id onto the input; proof_id through
//	    applyDayCardsCreativeOverride (approved+active ENFORCED — the proof's
//	    html becomes the creative and its variants[0] subject/preheader and
//	    from_names[0] ride along, day_cards_rebuild.go).
//	(c) set the cell's proposed name and retime to the CELL's slot (HH:MM
//	    Denver) on the target date — rebaseTimeSpans shifts every span
//	    preserving duration + Source verbatim. The slot column the operator
//	    sees on the grid is the dispatch truth: a clone cell passes the
//	    source's slot naturally, an empty-cell "new campaign" passes the
//	    clicked column — NEVER the source campaign's own time. Targets less
//	    than now+10min are refused per cell (the planner silently converts
//	    near-past schedules into an IMMEDIATE send).
//	(d) confirmed:false → a dry per-cell summary, ZERO writes.
//	(e) confirmed:true → deploy id-less via deployFromInput (preflight,
//	    template render gate, offer gate, segment-ownership gate, by-(org,
//	    name) idempotency). Re-posting the same proposal converges: an
//	    existing live (org,name) match reports "already_existed", not an
//	    error.
//
// CREATE-ONLY BY CONSTRUCTION: staging a proposal never cancels, mutates, or
// deletes anything — the only writes are the deploys themselves (behind
// confirmed:true) and the audit-log row. One failed cell reports and the
// batch CONTINUES; there is no cross-cell transaction, matching the
// by-name idempotent re-post as the recovery path.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// boardGridStageMinLead is the per-cell floor for the target schedule time.
// Wider than dayCardsRebuildMinLead (5m): a whole board staging sequentially
// can take minutes, so a cell that was barely-future at request time must not
// drift into the planner's silent now+2min IMMEDIATE conversion mid-batch.
const boardGridStageMinLead = 10 * time.Minute

// boardGridStageMaxCells bounds one request; a board day is ~50 cells.
const boardGridStageMaxCells = 200

// SetCampaignService wires the deploy dependency (route wiring,
// server_routes_mailing.go). Without it POST /stage answers 503.
func (s *BoardGridService) SetCampaignService(c *PMTACampaignService) {
	s.campaigns = c
}

type boardGridStageCell struct {
	Property         string `json:"property"`
	Slot             string `json:"slot"` // HH:MM Denver — the grid column this cell fires in
	Name             string `json:"name"`
	SourceCampaignID string `json:"source_campaign_id"`
	OfferID          string `json:"offer_id,omitempty"`
	ProofID          string `json:"proof_id,omitempty"`
	// InclusionSegments, when non-empty, REPLACES the source blob's audience
	// program for this cell (segments + send_priority + segment_reserves).
	// Empty/absent = the source audience rides along untouched.
	InclusionSegments []string `json:"inclusion_segments,omitempty"`
	// Fine-grain ISP controls (operator 2026-08-24 — the per-ISP framework's
	// levers, grid-native). ExcludeISPs removes those ISPs' plans/quotas from
	// the cell entirely ("stop gmail from HWS/MR/BWP/FC"); ISPCaps sets a hard
	// per-ISP volume cap onto the plan quota AND isp_quotas ("hold CI yahoo at
	// 12,000 while filters catch up"). Both use canonical lowercase ISP class
	// names; unknown names fail the cell at the door rather than silently
	// no-opping.
	ExcludeISPs []string       `json:"exclude_isps,omitempty"`
	ISPCaps     map[string]int `json:"isp_caps,omitempty"`
}

// boardGridCanonicalISPs mirrors internal/pkg/isp's class roster (+ 'other').
// A typo'd ISP name in a control must refuse loudly — a silently ignored
// exclude is a policy violation that LOOKS applied.
var boardGridCanonicalISPs = map[string]bool{
	"microsoft": true, "gmail": true, "yahoo": true, "apple": true,
	"comcast": true, "aol": true, "att": true, "sbcglobal": true,
	"cox": true, "charter": true, "verizon": true, "other": true,
}

// applyCellISPControls applies exclude/cap controls onto the deploy input.
// Exclusion removes the ISP from ISPPlans, ISPQuotas and TargetISPs; a cap
// clamps the plan Quota and the ISPQuotas volume (inserting a quota row when
// the blob had none — a cap the planner cannot see is not a cap).
func applyCellISPControls(input *engine.PMTACampaignInput, excludes []string, caps map[string]int) error {
	ex := map[string]bool{}
	for _, e := range excludes {
		e = strings.ToLower(strings.TrimSpace(e))
		if !boardGridCanonicalISPs[e] {
			return fmt.Errorf("exclude_isps: unknown ISP class %q", e)
		}
		ex[e] = true
	}
	for k, v := range caps {
		if !boardGridCanonicalISPs[strings.ToLower(strings.TrimSpace(k))] {
			return fmt.Errorf("isp_caps: unknown ISP class %q", k)
		}
		if v < 0 {
			return fmt.Errorf("isp_caps[%s]: cap must be >= 0", k)
		}
	}
	capFor := func(isp string) (int, bool) {
		for k, v := range caps {
			if strings.EqualFold(strings.TrimSpace(k), isp) {
				return v, true
			}
		}
		return 0, false
	}

	kept := input.ISPPlans[:0]
	for _, p := range input.ISPPlans {
		isp := strings.ToLower(strings.TrimSpace(p.ISP))
		if ex[isp] {
			continue
		}
		if c, ok := capFor(isp); ok && (p.Quota == 0 || p.Quota > c) {
			p.Quota = c
		}
		kept = append(kept, p)
	}
	if len(input.ISPPlans) > 0 && len(kept) == 0 {
		return fmt.Errorf("exclude_isps removes every ISP plan — nothing would send")
	}
	input.ISPPlans = kept

	seen := map[string]bool{}
	keptQ := input.ISPQuotas[:0]
	for _, q := range input.ISPQuotas {
		isp := strings.ToLower(strings.TrimSpace(q.ISP))
		if ex[isp] {
			continue
		}
		if c, ok := capFor(isp); ok && (q.Volume == 0 || q.Volume > c) {
			q.Volume = c
		}
		seen[isp] = true
		keptQ = append(keptQ, q)
	}
	input.ISPQuotas = keptQ
	for k, v := range caps {
		isp := strings.ToLower(strings.TrimSpace(k))
		if !seen[isp] && !ex[isp] {
			input.ISPQuotas = append(input.ISPQuotas, engine.ISPQuota{ISP: isp, Volume: v})
		}
	}

	if len(input.TargetISPs) > 0 {
		keptT := input.TargetISPs[:0]
		for _, t := range input.TargetISPs {
			if !ex[strings.ToLower(strings.TrimSpace(string(t)))] {
				keptT = append(keptT, t)
			}
		}
		input.TargetISPs = keptT
	}
	return nil
}

// boardGridStageBoard carries the BOARD-WIDE timing/throttle overrides
// (operator ruling 2026-08-23 — these are never per-cell).
type boardGridStageBoard struct {
	// slotTimes remaps a slot column ("01:01") to a new Denver HH:MM for
	// every cell in that column. Values are pre-validated at the door.
	slotTimes map[string]string
	// throttleStrategy, when non-empty, overrides input.ThrottleStrategy AND
	// every plan's own strategy (plans override the default otherwise).
	throttleStrategy string
	// windowHours, when > 0, replaces each plan's absolute spans with one
	// [target, target+windowHours] span, source='duration-calc'.
	windowHours int
}

// boardGridThrottleStrategies is the allowlist the UI offers. "auto" is the
// planner's default (pmta_campaign_planner.go:348), "gentle" is the doctrine
// default (§6) used by fresh_broadcast_runner.go; "even" is accepted as the
// operator-named alias. The field is stored plan metadata — pacing truth is
// the spans + cadence, which window_hours controls.
var boardGridThrottleStrategies = map[string]bool{"gentle": true, "auto": true, "even": true}

type boardGridStageResult struct {
	Property string `json:"property"`
	Slot     string `json:"slot"`
	Name     string `json:"name"`
	// Status: "dry" (unconfirmed summary) | "deployed" | "already_existed" |
	// "failed". already_existed is the by-(org,name) idempotency guard
	// converging a re-post — reported, never fatal.
	Status      string     `json:"status"`
	CampaignID  string     `json:"campaign_id,omitempty"`
	Offer       string     `json:"offer,omitempty"`
	ProofName   string     `json:"proof_name,omitempty"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// Audience: "source" (blob's audience rides along) or
	// "N segments override".
	Audience string `json:"audience,omitempty"`
	// ISPControls echoes the applied per-ISP excludes/caps ("exclude gmail ·
	// cap yahoo=12000") so the stage result proves the policy landed.
	ISPControls string `json:"isp_controls,omitempty"`
	// RecipientsEstimate is intentionally not a number: the audience is
	// planned by the AudienceFinalizationWorker AFTER deploy.
	RecipientsEstimate string `json:"recipients_estimate,omitempty"`
	Code               int    `json:"code,omitempty"` // per-cell HTTP-style status on failure
	Error              string `json:"error,omitempty"`
}

func (s *BoardGridService) HandleStageGrid(w http.ResponseWriter, r *http.Request) {
	if s.campaigns == nil {
		respondError(w, http.StatusServiceUnavailable,
			"board-grid staging unavailable — campaign service not wired")
		return
	}
	var body struct {
		Date      string               `json:"date"`
		Cells     []boardGridStageCell `json:"cells"`
		Confirmed bool                 `json:"confirmed"`
		// Board-wide (operator ruling 2026-08-23): timing/throttle apply to
		// the ENTIRE board — see the header comment.
		SlotTimes        map[string]string `json:"slot_times,omitempty"`
		ThrottleStrategy string            `json:"throttle_strategy,omitempty"`
		WindowHours      int               `json:"window_hours,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Date == "" {
		respondError(w, http.StatusBadRequest, "date is required (YYYY-MM-DD)")
		return
	}
	if _, err := time.Parse("2006-01-02", body.Date); err != nil {
		respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	if len(body.Cells) == 0 {
		respondError(w, http.StatusBadRequest, "cells is empty — nothing to stage")
		return
	}
	if len(body.Cells) > boardGridStageMaxCells {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("too many cells (%d > %d)", len(body.Cells), boardGridStageMaxCells))
		return
	}
	// Board-wide override validation — at the door, so a typo fails the whole
	// request instead of failing every cell one by one.
	board := boardGridStageBoard{slotTimes: map[string]string{}}
	for col, at := range body.SlotTimes {
		at = strings.TrimSpace(at)
		if _, terr := time.Parse("15:04", at); terr != nil {
			respondError(w, http.StatusBadRequest,
				fmt.Sprintf("slot_times[%q] must be HH:MM (Denver), got %q", col, at))
			return
		}
		board.slotTimes[strings.TrimSpace(col)] = at
	}
	if v := strings.TrimSpace(body.ThrottleStrategy); v != "" {
		if !boardGridThrottleStrategies[v] {
			respondError(w, http.StatusBadRequest,
				"throttle_strategy must be one of: gentle, auto, even")
			return
		}
		board.throttleStrategy = v
	}
	if body.WindowHours != 0 && (body.WindowHours < 1 || body.WindowHours > 16) {
		respondError(w, http.StatusBadRequest, "window_hours must be 1..16")
		return
	}
	board.windowHours = body.WindowHours

	// dayStart carries the Denver calendar day AND the Denver location —
	// per-cell targets are built from its Y/M/D + the cell's HH:MM wall
	// clock, which is DST-correct where AddDate/Add arithmetic is not.
	dayStart, _, err := denverDayBounds(body.Date)
	if err != nil {
		respondError(w, http.StatusBadRequest, "board grid day bounds: "+err.Error())
		return
	}

	ctx := r.Context()
	orgID := getOrgID(r)

	results := make([]boardGridStageResult, 0, len(body.Cells))
	totals := map[string]int{"cells": len(body.Cells)}
	for _, cell := range body.Cells {
		res := s.stageOneCell(ctx, orgID, dayStart, board, cell, body.Confirmed)
		results = append(results, res)
		totals[res.Status]++
	}

	if body.Confirmed {
		writeAuditLog(ctx, s.db, actorFromRequest(r), "board_grid_stage", "board_grid", body.Date,
			nil,
			map[string]interface{}{
				"date":              body.Date,
				"cells":             len(body.Cells),
				"deployed":          totals["deployed"],
				"already_existed":   totals["already_existed"],
				"failed":            totals["failed"],
				"slot_times":        body.SlotTimes,
				"throttle_strategy": board.throttleStrategy,
				"window_hours":      board.windowHours,
			})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"date":      body.Date,
		"confirmed": body.Confirmed,
		"results":   results,
		"totals":    totals,
	})
}

// stageOneCell runs steps (a)–(e) for one cell. Every refusal is a per-cell
// result (the batch continues); nothing here cancels or mutates existing rows.
func (s *BoardGridService) stageOneCell(ctx context.Context, orgID string, dayStart time.Time, board boardGridStageBoard, cell boardGridStageCell, confirmed bool) boardGridStageResult {
	res := boardGridStageResult{
		Property: cell.Property,
		Slot:     strings.TrimSpace(cell.Slot),
		Name:     strings.TrimSpace(cell.Name),
	}
	fail := func(code int, msg string) boardGridStageResult {
		res.Status, res.Code, res.Error = "failed", code, msg
		return res
	}

	if res.Name == "" {
		return fail(http.StatusBadRequest, "name is required")
	}
	// Board-wide column remap first: the cell keeps its column identity
	// (res.Slot) while its fire time follows the remapped column.
	effectiveSlot := res.Slot
	if v, ok := board.slotTimes[res.Slot]; ok {
		effectiveSlot = v
	}
	slotTod, terr := time.Parse("15:04", effectiveSlot)
	if terr != nil {
		return fail(http.StatusBadRequest, "slot must be HH:MM (Denver): "+effectiveSlot)
	}
	srcID := strings.TrimSpace(cell.SourceCampaignID)
	if _, err := uuid.Parse(srcID); err != nil {
		return fail(http.StatusBadRequest,
			"source_campaign_id must be a campaign UUID — clone the grid (or pick a clone candidate) to carry it")
	}

	// (a) the SOURCE deploy blob — org-scoped, same shape loadCampaignData /
	// day_cards_rebuild read. Missing blob = per-cell 400, batch continues.
	var srcName string
	var configJSON sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(name,''), COALESCE(pmta_config::text,'')
		FROM mailing_campaigns
		WHERE id = $1 AND organization_id = $2
	`, srcID, orgID).Scan(&srcName, &configJSON); err != nil {
		return fail(http.StatusBadRequest, "source campaign not found: "+srcID)
	}
	var cfg struct {
		CampaignInput json.RawMessage `json:"campaign_input"`
	}
	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		_ = json.Unmarshal([]byte(configJSON.String), &cfg)
	}
	if len(cfg.CampaignInput) <= 2 {
		return fail(http.StatusBadRequest, fmt.Sprintf(
			"source campaign %q has no pmta_config campaign_input blob — cannot stage from it (the DB fallback loses audience/priority/cadence fields)",
			srcName))
	}
	var input engine.PMTACampaignInput
	if err := json.Unmarshal(cfg.CampaignInput, &input); err != nil {
		return fail(http.StatusBadRequest, "stored campaign_input blob is unreadable: "+err.Error())
	}
	// Id-less: identity comes from the by-(org,name) guard, never the source row.
	input.CampaignID = ""

	// (b) overrides — offer first, then the proof (whose copy rides along).
	if v := strings.TrimSpace(cell.OfferID); v != "" {
		if _, perr := uuid.Parse(v); perr != nil {
			return fail(http.StatusBadRequest, "offer_id must be a mailing_offers UUID")
		}
		var offerName string
		if oerr := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(name,'') FROM mailing_offers WHERE id = $1 AND organization_id = $2
		`, v, orgID).Scan(&offerName); oerr != nil {
			return fail(http.StatusNotFound, "offer not found: "+v)
		}
		input.OfferID = v
		res.Offer = offerName
	}
	if v := strings.TrimSpace(cell.ProofID); v != "" {
		var proofName string
		if perr := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(name,'') FROM mailing_offer_proofs WHERE id = $1 AND organization_id = $2
		`, v, orgID).Scan(&proofName); perr != nil {
			return fail(http.StatusNotFound, "offer proof not found: "+v)
		}
		// approved+active enforced here; the proof's subject/preheader/
		// from-name fill the variants (no explicit overrides given).
		ov := dayCardsRebuildOverrides{ProofID: v}
		if _, code, cerr := s.campaigns.applyDayCardsCreativeOverride(ctx, orgID, &ov, &input); cerr != nil {
			return fail(code, cerr.Error())
		}
		res.ProofName = proofName
	}

	// (b2) per-cell AUDIENCE override: an explicit segment list REPLACES the
	// source's audience program. SendPriority and SegmentReserves are cleared
	// with it — they order/reserve the SOURCE's segments, and carrying them
	// against a different inclusion set would silently mis-order the draw and
	// reserve against segments that are no longer in the audience.
	if len(cell.InclusionSegments) > 0 {
		for _, sid := range cell.InclusionSegments {
			if _, perr := uuid.Parse(strings.TrimSpace(sid)); perr != nil {
				return fail(http.StatusBadRequest, "inclusion_segments must be segment UUIDs, got "+sid)
			}
		}
		input.InclusionSegments = cell.InclusionSegments
		input.SendPriority = nil
		input.SegmentReserves = nil
		res.Audience = fmt.Sprintf("%d segments override", len(cell.InclusionSegments))
	} else {
		res.Audience = "source"
	}

	// (b3) fine-grain ISP controls — excludes and per-ISP caps land on the
	// deploy input itself so the planner/wave path enforce them; a bad ISP
	// name refuses the cell instead of silently no-opping.
	if len(cell.ExcludeISPs) > 0 || len(cell.ISPCaps) > 0 {
		if err := applyCellISPControls(&input, cell.ExcludeISPs, cell.ISPCaps); err != nil {
			return fail(http.StatusBadRequest, err.Error())
		}
		parts := []string{}
		if len(cell.ExcludeISPs) > 0 {
			parts = append(parts, "exclude "+strings.Join(cell.ExcludeISPs, ","))
		}
		for k, v := range cell.ISPCaps {
			parts = append(parts, fmt.Sprintf("cap %s=%d", k, v))
		}
		res.ISPControls = strings.Join(parts, " · ")
	}

	// (c) the proposed name + the CELL's (possibly column-remapped) slot on
	// the target Denver day — never the source campaign's own time.
	input.Name = res.Name
	loc := dayStart.Location()
	target := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(),
		slotTod.Hour(), slotTod.Minute(), 0, 0, loc)
	if !target.After(time.Now().Add(boardGridStageMinLead)) {
		return fail(http.StatusUnprocessableEntity, fmt.Sprintf(
			"target %s (%s %s Denver) is less than 10 minutes away — the planner would silently convert it into an IMMEDIATE send; pick a later slot or date",
			target.UTC().Format(time.RFC3339), dayStart.Format("2006-01-02"), effectiveSlot))
	}
	rebaseTimeSpans(&input, target.UTC())

	// (c2) BOARD-WIDE throttle + window (operator ruling 2026-08-23).
	if board.throttleStrategy != "" {
		// Plans carry their own strategy which overrides the input default
		// (pmta_campaign_planner.go:496) — a board-wide override must land on
		// both or the plans would silently keep the source's value.
		input.ThrottleStrategy = board.throttleStrategy
		for pi := range input.ISPPlans {
			input.ISPPlans[pi].ThrottleStrategy = board.throttleStrategy
		}
	}
	if board.windowHours > 0 {
		// Replace each plan's absolute spans with ONE [target, target+window]
		// span. The duration changed, so source MUST become 'duration-calc' —
		// preserving the old value would misrepresent the span to the wave
		// sanity check (the time_spans[*].source contract, §6 footgun #1).
		windowEnd := target.UTC().Add(time.Duration(board.windowHours) * time.Hour)
		for pi := range input.ISPPlans {
			kept := input.ISPPlans[pi].TimeSpans[:0]
			replaced := false
			for _, sp := range input.ISPPlans[pi].TimeSpans {
				if sp.StartAt == nil {
					kept = append(kept, sp) // weekly/relative spans untouched
					continue
				}
				replaced = true
			}
			if replaced {
				st, en := target.UTC(), windowEnd
				kept = append(kept, engine.PMTATimeSpanInput{
					Type: "absolute", StartAt: &st, EndAt: &en, Source: "duration-calc",
				})
			}
			input.ISPPlans[pi].TimeSpans = kept
		}
	}
	res.ScheduledAt = input.ScheduledAt

	// (d) dry: summarize, write NOTHING.
	if !confirmed {
		res.Status = "dry"
		res.RecipientsEstimate = "planned at deploy"
		if res.Offer == "" && input.OfferID != "" {
			res.Offer = input.OfferID // source blob's own offer, not re-read
		}
		return res
	}

	// (e) deploy id-less through the FULL gated path. dayCardsDeployFn is the
	// shared test seam (day_cards_rebuild.go precedent).
	deployFn := s.campaigns.dayCardsDeployFn
	if deployFn == nil {
		deployFn = s.campaigns.deployFromInput
	}
	newID, _, alreadyExisted, derr := deployFn(ctx, orgID, input)
	if derr != nil {
		code := http.StatusInternalServerError
		var inputErr *deployInputError
		var gateErr *offerGateError
		if errors.As(derr, &inputErr) {
			code = http.StatusBadRequest
		} else if errors.As(derr, &gateErr) {
			code = http.StatusUnprocessableEntity
		}
		return fail(code, "deploy failed: "+derr.Error())
	}
	res.CampaignID = newID
	if alreadyExisted {
		// The by-(org,name) guard matched a live campaign: the proposal has
		// already converged — report it, don't error (idempotent re-post).
		res.Status = "already_existed"
		return res
	}
	res.Status = "deployed"
	return res
}

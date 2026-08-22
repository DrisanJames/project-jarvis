package api

// Day Cards REBUILD — cancel + redeploy-sibling for a single campaign.
//
// Doctrine (§6): once a campaign is 'scheduled', audience/creative/timing are
// FROZEN — the sanctioned recovery is cancel + redeploy a sibling, never
// in-place mutation. This endpoint is that recovery as software:
//
//	POST /api/mailing/pmta-campaign/day-cards/rebuild
//	{ campaign_id, confirmed, overrides:{...} }
//
// The rebuild input is the campaign's OWN pmta_config->'campaign_input' blob
// — the byte-faithful deploy payload (pmta_campaign_persistence.go). The
// DB-fallback clone path loses inclusion_segments / send_priority /
// segment_reserves / min_remail_hours / sending_profile_id /
// use_master_selection / per-plan cadence+time_spans, so a missing blob is a
// hard 400 (precedent: HandleRetryCampaign), never a degraded rebuild.
//
// Deploys go through s.deployFromInput ONLY — preflight, template render
// gate, segment-ownership gate, master-selection coercion, by-name guard.
// Never normalizePMTACampaignInput + createPMTAWaveCampaign directly.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// dayCardsRebuildMinLead is the floor for an overridden scheduled_at. The
// planner silently flips anything closer than now+5min to an IMMEDIATE send
// at now+2min (pmta_campaign_planner.go:356-368) — a rebuild must refuse
// rather than arm that trap.
const dayCardsRebuildMinLead = 5 * time.Minute

type dayCardsRebuildOverrides struct {
	Name        string `json:"name,omitempty"`
	Subject     string `json:"subject,omitempty"`
	PreviewText string `json:"preview_text,omitempty"`
	FromName    string `json:"from_name,omitempty"`
	// Exactly one creative source may be given.
	ProofID     string `json:"proof_id,omitempty"`
	CreativeID  string `json:"creative_id,omitempty"`
	HTMLContent string `json:"html_content,omitempty"`

	InclusionSegments *[]string `json:"inclusion_segments,omitempty"`
	ExclusionSegments *[]string `json:"exclusion_segments,omitempty"`
	ExclusionLists    *[]string `json:"exclusion_lists,omitempty"`
	MinRemailHours    *int      `json:"min_remail_hours,omitempty"`
	ScheduledAt       string    `json:"scheduled_at,omitempty"` // RFC3339
}

type dayCardsRebuildRequest struct {
	CampaignID string                   `json:"campaign_id"`
	Confirmed  bool                     `json:"confirmed"`
	Overrides  dayCardsRebuildOverrides `json:"overrides"`
}

// rebaseTimeSpans moves a campaign's schedule to newAt, preserving structure:
// every absolute isp_plans[*].time_spans[*] span is shifted by the same delta
// (newAt - old anchor) and keeps its ORIGINAL duration (end-start) and its
// Source value verbatim — source ∈ {duration-calc, manual} is what skips the
// wave sanity check (planner.go isUserExplicitSpan); rewriting it would
// re-arm or disarm gates. Weekly spans are untouched (they are relative
// already). Pure function so the rewrite is unit-testable.
func rebaseTimeSpans(input *engine.PMTACampaignInput, newAt time.Time) {
	// Anchor: the stored scheduled_at when present, else the earliest
	// absolute span start. mailing_campaigns.scheduled_at is DERIVED
	// (EarliestStart), so the blob's anchor is the deploy truth.
	var anchor *time.Time
	if input.ScheduledAt != nil {
		t := *input.ScheduledAt
		anchor = &t
	} else {
		for _, p := range input.ISPPlans {
			for _, sp := range p.TimeSpans {
				if sp.StartAt != nil && (anchor == nil || sp.StartAt.Before(*anchor)) {
					t := *sp.StartAt
					anchor = &t
				}
			}
		}
	}

	nt := newAt
	input.SendMode = "scheduled"
	input.ScheduledAt = &nt

	if anchor == nil {
		return // nothing to rebase — no absolute spans, no prior anchor
	}
	delta := newAt.Sub(*anchor)
	for pi := range input.ISPPlans {
		for si := range input.ISPPlans[pi].TimeSpans {
			span := &input.ISPPlans[pi].TimeSpans[si]
			if span.StartAt == nil {
				continue
			}
			newStart := span.StartAt.Add(delta)
			if span.EndAt != nil {
				dur := span.EndAt.Sub(*span.StartAt) // preserve the span's own duration
				newEnd := newStart.Add(dur)
				span.EndAt = &newEnd
			}
			span.StartAt = &newStart
			// span.Source deliberately untouched.
		}
	}
}

// HandleDayCardsRebuild implements the cancel-first rebuild.
func (s *PMTACampaignService) HandleDayCardsRebuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	var req dayCardsRebuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := uuid.Parse(strings.TrimSpace(req.CampaignID)); err != nil {
		respondError(w, http.StatusBadRequest, "campaign_id must be a UUID")
		return
	}
	campaignID := strings.TrimSpace(req.CampaignID)

	// ── (a) load the deploy blob via the loadCampaignData path ──────────
	// (same SELECT shape; org-scoped). A missing blob is a hard 400: the
	// DB-fallback reconstruction loses audience/priority/cadence fields.
	var name, status string
	var sentCount int
	var configJSON sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(name,''), status, COALESCE(sent_count,0), COALESCE(pmta_config::text,'')
		FROM mailing_campaigns
		WHERE id = $1 AND organization_id = $2
	`, campaignID, orgID).Scan(&name, &status, &sentCount, &configJSON)
	if err != nil {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}

	// ── (b) status gates ────────────────────────────────────────────────
	if status == "cancelled" || status == "deleted" {
		respondJSON(w, http.StatusConflict, map[string]string{
			"error":  "campaign is " + status + " — rebuild a live sibling instead, or deploy fresh",
			"status": status,
		})
		return
	}
	// Partial-send guard: anything already mailed (or in a mailing/terminal
	// status) means the sibling re-mails recipients the original reached —
	// they will be re-eligible. That needs an explicit confirmed:true.
	partialSend := sentCount > 0 ||
		status == "sending" || status == "sent" ||
		status == "completed" || status == "completed_with_errors"

	var cfg struct {
		CampaignInput json.RawMessage `json:"campaign_input"`
	}
	if configJSON.Valid && configJSON.String != "" && configJSON.String != "{}" {
		_ = json.Unmarshal([]byte(configJSON.String), &cfg)
	}
	if len(cfg.CampaignInput) <= 2 {
		respondError(w, http.StatusBadRequest,
			"campaign has no pmta_config campaign_input blob — cannot rebuild (the DB fallback loses audience/priority/cadence fields)")
		return
	}
	var input engine.PMTACampaignInput
	if err := json.Unmarshal(cfg.CampaignInput, &input); err != nil {
		respondError(w, http.StatusBadRequest, "stored campaign_input blob is unreadable: "+err.Error())
		return
	}
	// The sibling is an id-less deploy — identity comes from cancel-first +
	// the by-(org,name) guard, never from reusing the old row.
	input.CampaignID = ""
	if input.Name == "" {
		input.Name = name
	}

	// ── (c) apply overrides ─────────────────────────────────────────────
	creativeSource, code, cerr := s.applyDayCardsCreativeOverride(ctx, orgID, &req.Overrides, &input)
	if cerr != nil {
		respondError(w, code, cerr.Error())
		return
	}
	if v := strings.TrimSpace(req.Overrides.Name); v != "" {
		input.Name = v
	}
	if v := strings.TrimSpace(req.Overrides.Subject); v != "" {
		for i := range input.Variants {
			input.Variants[i].Subject = v
		}
	}
	if v := strings.TrimSpace(req.Overrides.PreviewText); v != "" {
		for i := range input.Variants {
			input.Variants[i].PreviewText = v
		}
	}
	if v := strings.TrimSpace(req.Overrides.FromName); v != "" {
		for i := range input.Variants {
			input.Variants[i].FromName = v
		}
	}
	if req.Overrides.InclusionSegments != nil {
		input.InclusionSegments = *req.Overrides.InclusionSegments
	}
	if req.Overrides.ExclusionSegments != nil {
		input.ExclusionSegments = *req.Overrides.ExclusionSegments
	}
	if req.Overrides.ExclusionLists != nil {
		input.ExclusionLists = *req.Overrides.ExclusionLists
	}
	if req.Overrides.MinRemailHours != nil {
		// NB: MinRemailHours only fires when sourceType=="list" (§6 footgun
		// #3) — we set it faithfully and leave the semantics to the planner.
		input.MinRemailHours = *req.Overrides.MinRemailHours
	}
	if v := strings.TrimSpace(req.Overrides.ScheduledAt); v != "" {
		newAt, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			respondError(w, http.StatusUnprocessableEntity, "overrides.scheduled_at must be RFC3339: "+perr.Error())
			return
		}
		// Planner trap: anything < now+5min is SILENTLY recovered as an
		// immediate send at now+2min. Refuse instead of arming it.
		if !newAt.After(time.Now().Add(dayCardsRebuildMinLead)) {
			respondError(w, http.StatusUnprocessableEntity,
				"overrides.scheduled_at must be more than 5 minutes in the future — the planner silently converts anything closer into an IMMEDIATE send at now+2min")
			return
		}
		rebaseTimeSpans(&input, newAt.UTC())
	}

	// ── (d) unconfirmed = dry preview, ZERO writes ──────────────────────
	spanCount := 0
	for _, p := range input.ISPPlans {
		spanCount += len(p.TimeSpans)
	}
	summary := map[string]interface{}{
		"name":               input.Name,
		"scheduled_at":       input.ScheduledAt,
		"send_mode":          input.SendMode,
		"isp_plans":          len(input.ISPPlans),
		"time_spans":         spanCount,
		"creative_source":    creativeSource,
		"variants":           len(input.Variants),
		"inclusion_segments": len(input.InclusionSegments),
		"inclusion_lists":    len(input.InclusionLists),
		"exclusion_segments": len(input.ExclusionSegments),
		"exclusion_lists":    len(input.ExclusionLists),
		"min_remail_hours":   input.MinRemailHours,
	}
	if !req.Confirmed {
		resp := map[string]interface{}{
			"dry_run": true,
			"would_cancel": map[string]interface{}{
				"campaign_id": campaignID,
				"name":        name,
				"status":      status,
				"sent_count":  sentCount,
			},
			"new_input_summary": summary,
		}
		if partialSend {
			resp["partial_send_warning"] = fmt.Sprintf(
				"%d recipients already mailed by %q (status %s) will be RE-ELIGIBLE in the rebuilt sibling",
				sentCount, name, status)
		}
		respondJSON(w, http.StatusOK, resp)
		return
	}

	// ── (e) confirmed: cancel the original ──────────────────────────────
	// Inline the sanctioned cancel UPDATEs (campaign_builder_actions.go
	// HandleCancelCampaign): status→cancelled + queue rows queued/paused→
	// cancelled; waves die via the dispatcher's status filter + janitor.
	// sent_before_cancel is the pre-cancel sent_count. The guard mirrors the
	// sanctioned one: terminal-complete rows can't be cancelled — for those
	// the by-name guard does NOT free the name, so a sibling needs a new one.
	terminalComplete := status == "sent" || status == "completed" || status == "completed_with_errors" || status == "failed"
	if !terminalComplete {
		res, uerr := s.db.ExecContext(ctx, `
			UPDATE mailing_campaigns
			SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND organization_id = $2
			  AND status NOT IN ('sent','completed','completed_with_errors','cancelled','failed','deleted')
		`, campaignID, orgID)
		if uerr != nil {
			respondError(w, http.StatusInternalServerError, "cancel original campaign: "+uerr.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			respondJSON(w, http.StatusConflict, map[string]string{
				"error": "campaign moved to a terminal status mid-request — reload and retry"})
			return
		}
		if _, uerr := s.db.ExecContext(ctx, `
			UPDATE mailing_campaign_queue
			SET status = 'cancelled', updated_at = NOW()
			WHERE campaign_id = $1 AND status IN ('queued','paused')
		`, campaignID); uerr != nil {
			// Non-fatal by sanctioned precedent, but say so.
			log.Printf("[day-cards-rebuild] queue-row cancel for %s failed: %v", campaignID, uerr)
		}
	} else if strings.TrimSpace(req.Overrides.Name) == "" && input.Name == name {
		// A completed row HOLDS its name (the by-(org,name) guard frees only
		// cancelled/failed/deleted... failed IS freed, but sent/completed are
		// not) — an id-less same-name deploy would converge on the old row
		// instead of minting the sibling. Derive a sibling name.
		if status != "failed" {
			input.Name = name + " (rebuild)"
		}
	}

	// ── (f) deploy the sibling id-less through the FULL deploy path ─────
	// Cancel-first freed the name (the by-(org,name) guard frees names for
	// cancelled/failed/deleted), so the same name is reusable.
	deployFn := s.dayCardsDeployFn
	if deployFn == nil {
		deployFn = s.deployFromInput
	}
	newID, newStatus, alreadyExisted, derr := deployFn(ctx, orgID, input)
	if derr != nil {
		// Never hide the half-state: the original IS cancelled (unless it
		// was terminal-complete), the sibling does NOT exist.
		respondJSON(w, http.StatusBadGateway, map[string]interface{}{
			"error":                 "sibling deploy failed AFTER the original was cancelled",
			"cancelled_campaign_id": campaignID,
			"original_cancelled":    !terminalComplete,
			"sent_before_cancel":    sentCount,
			"deploy_error":          derr.Error(),
			"recovery":              "retry the rebuild — the campaign_input blob is still on the cancelled row",
		})
		return
	}
	if alreadyExisted {
		respondJSON(w, http.StatusBadGateway, map[string]interface{}{
			"error":                 fmt.Sprintf("a live campaign already holds the name %q — no sibling was created", input.Name),
			"cancelled_campaign_id": campaignID,
			"original_cancelled":    !terminalComplete,
			"existing_campaign_id":  newID,
			"existing_status":       newStatus,
			"recovery":              "retry the rebuild with overrides.name set to a fresh name",
		})
		return
	}

	// Stamp lineage on the NEW row so the day-cards panel can surface it.
	// Top-level key next to campaign_input; single jsonb_set UPDATE.
	if _, uerr := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET pmta_config = jsonb_set(COALESCE(pmta_config, '{}'::jsonb), '{rebuilt_from}', to_jsonb($2::text), true),
		    updated_at = NOW()
		WHERE id = $1 AND organization_id = $3
	`, newID, campaignID, orgID); uerr != nil {
		log.Printf("[day-cards-rebuild] rebuilt_from stamp on %s failed: %v", newID, uerr)
	}

	// ── (h) audit ───────────────────────────────────────────────────────
	writeAuditLog(ctx, s.db, actorFromRequest(r), "day_cards_rebuild", "mailing_campaign", campaignID,
		map[string]interface{}{
			"campaign_id": campaignID,
			"name":        name,
			"status":      status,
			"sent_count":  sentCount,
		},
		map[string]interface{}{
			"new_campaign_id": newID,
			"new_status":      newStatus,
			"summary":         summary,
			"creative_source": creativeSource,
		})

	// ── (g) response ────────────────────────────────────────────────────
	note := fmt.Sprintf("original %q cancelled; sibling %q deployed as %s", name, input.Name, newStatus)
	if terminalComplete {
		note = fmt.Sprintf("original %q was already %s (not cancellable); sibling %q deployed as %s", name, status, input.Name, newStatus)
	}
	resp := map[string]interface{}{
		"cancelled_campaign_id": campaignID,
		"new_campaign_id":       newID,
		"new_status":            newStatus,
		"sent_before_cancel":    sentCount,
		"note":                  note,
	}
	if partialSend {
		resp["partial_send_warning"] = fmt.Sprintf(
			"%d recipients already mailed by the original are re-eligible in the sibling", sentCount)
	}
	respondJSON(w, http.StatusOK, resp)
}

// applyDayCardsCreativeOverride resolves the creative override (exactly one of
// proof_id / creative_id / html_content) onto the input's variants. Returns
// the creative source label for the audit/summary ("unchanged", "proof:<id>",
// "creative:<id>", "raw") and an HTTP status + error on refusal.
func (s *PMTACampaignService) applyDayCardsCreativeOverride(ctx context.Context, orgID string, ov *dayCardsRebuildOverrides, input *engine.PMTACampaignInput) (string, int, error) {
	sources := 0
	if strings.TrimSpace(ov.ProofID) != "" {
		sources++
	}
	if strings.TrimSpace(ov.CreativeID) != "" {
		sources++
	}
	if strings.TrimSpace(ov.HTMLContent) != "" {
		sources++
	}
	if sources == 0 {
		return "unchanged", 0, nil
	}
	if sources > 1 {
		return "", http.StatusBadRequest, fmt.Errorf("exactly one creative source: proof_id OR creative_id OR html_content")
	}

	setHTML := func(html string) {
		if len(input.Variants) == 0 {
			input.Variants = []engine.ContentVariant{{VariantName: "A"}}
		}
		for i := range input.Variants {
			input.Variants[i].HTMLContent = html
		}
	}

	switch {
	case strings.TrimSpace(ov.ProofID) != "":
		id := strings.TrimSpace(ov.ProofID)
		var html, approval string
		var isActive bool
		var variantsB, fromNamesB []byte
		err := s.db.QueryRowContext(ctx, `
			SELECT html_content, approval_status, COALESCE(is_active, FALSE), variants, from_names
			FROM mailing_offer_proofs
			WHERE id = $1 AND organization_id = $2
		`, id, orgID).Scan(&html, &approval, &isActive, &variantsB, &fromNamesB)
		if err != nil {
			return "", http.StatusNotFound, fmt.Errorf("offer proof %s not found", id)
		}
		// Approved-pool doctrine (§6 COPY / §8): only an approved+active
		// proof may ship.
		if approval != "approved" || !isActive {
			return "", http.StatusUnprocessableEntity,
				fmt.Errorf("offer proof %s is not shippable (approval_status=%s, is_active=%t) — only approved+active proofs may mail", id, approval, isActive)
		}
		setHTML(html)
		var pvs []proofVariant
		_ = json.Unmarshal(variantsB, &pvs)
		if strings.TrimSpace(ov.Subject) == "" && len(pvs) > 0 && strings.TrimSpace(pvs[0].Subject) != "" {
			for i := range input.Variants {
				input.Variants[i].Subject = pvs[0].Subject
			}
			if strings.TrimSpace(ov.PreviewText) == "" && strings.TrimSpace(pvs[0].Preheader) != "" {
				for i := range input.Variants {
					input.Variants[i].PreviewText = pvs[0].Preheader
				}
			}
		}
		var fns []string
		_ = json.Unmarshal(fromNamesB, &fns)
		if strings.TrimSpace(ov.FromName) == "" && len(fns) > 0 && strings.TrimSpace(fns[0]) != "" {
			for i := range input.Variants {
				input.Variants[i].FromName = fns[0]
			}
		}
		return "proof:" + id, 0, nil

	case strings.TrimSpace(ov.CreativeID) != "":
		id := strings.TrimSpace(ov.CreativeID)
		// Body loaded exactly the way the registry preview handler does
		// (creatives_registry.go HandlePreview): the stored html_content.
		var html, subject, preheader, approval string
		err := s.db.QueryRowContext(ctx, `
			SELECT html_content, COALESCE(subject,''), COALESCE(preheader,''), approval_status
			FROM mailing_creatives
			WHERE id = $1 AND organization_id = $2
		`, id, orgID).Scan(&html, &subject, &preheader, &approval)
		if err != nil {
			return "", http.StatusNotFound, fmt.Errorf("creative %s not found", id)
		}
		if approval != "approved" {
			return "", http.StatusUnprocessableEntity,
				fmt.Errorf("creative %s is not approved (approval_status=%s) — only approved Creative Studio rows may mail", id, approval)
		}
		setHTML(html)
		if strings.TrimSpace(ov.Subject) == "" && strings.TrimSpace(subject) != "" {
			for i := range input.Variants {
				input.Variants[i].Subject = subject
			}
		}
		if strings.TrimSpace(ov.PreviewText) == "" && strings.TrimSpace(preheader) != "" {
			for i := range input.Variants {
				input.Variants[i].PreviewText = preheader
			}
		}
		return "creative:" + id, 0, nil

	default: // raw html_content — allowed, but audit-logged as 'raw'
		setHTML(ov.HTMLContent)
		return "raw", 0, nil
	}
}

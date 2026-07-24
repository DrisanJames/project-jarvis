package api

// =============================================================================
// PROMOTE POST-CONDITIONS VERIFY — Platform Coalition WS2, REQ-C18
// =============================================================================
// POST /api/mailing/pmta-campaign/verify — the read-only gate the stage/
// promote CLI (REQ-C04) calls after a deploy. It turns the silent-failure
// classes (scheduled-with-0-recipients, NULL offer_id, burst 200-wrong-id,
// zero waves, volume:0 in explicit mode, fresh-drew-zero) into deterministic
// PASS/FAIL/PENDING verdicts. Performs NO mutation — safe to poll.
//
// Verification is BY NAME, never by returned id (stage/deploy can 200 with a
// wrong id under burst), which requires campaign names unique per lane
// (AS-6.3). 'cancelled'/'deleted' siblings are excluded from matching (the
// sanctioned recovery path is cancel + redeploy a sibling, so a cancelled
// twin is routine) but their presence is reported in the check detail.
//
// JSON contract: tasks/eng-team/coalition/SCHEMA-CONTRACTS.md §4.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// pmtaVerifyCampaignRequest is one campaign's expectations.
type pmtaVerifyCampaignRequest struct {
	Name string `json:"name"`
	// ExpectOffer: offer wave → mailing_campaigns.offer_id must be non-NULL
	// (a NULL offer_id silently disables offer suppression — the
	// board-offer-id gap class).
	ExpectOffer bool `json:"expect_offer"`
	// ExplicitQuotas: explicit-cap board → any isp plan with quota=0 FAILS
	// (volume:0 = UNLIMITED in explicit mode — banned).
	ExplicitQuotas bool `json:"explicit_quotas"`
	// MinRecipients defaults to 1 (finalization must yield > 0).
	MinRecipients int `json:"min_recipients"`
	// ExpectWaves defaults to true; set false only for draft-stage checks.
	ExpectWaves *bool `json:"expect_waves"`
	// SegmentReserves: reserve floors to assert against plan-recipient
	// source counts (segment_id → minimum selected draw).
	SegmentReserves map[string]int `json:"segment_reserves"`
}

type pmtaVerifyRequest struct {
	Campaigns []pmtaVerifyCampaignRequest `json:"campaigns"`
}

type pmtaVerifyCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"` // PASS | FAIL | PENDING | SKIPPED
	Detail string `json:"detail"`
}

type pmtaVerifyWaves struct {
	Count             int        `json:"count"`
	FirstAt           *time.Time `json:"first_at"`
	LastAt            *time.Time `json:"last_at"`
	PlannedRecipients int        `json:"planned_recipients"`
}

type pmtaVerifyCampaignResult struct {
	Name            string            `json:"name"`
	CampaignID      *string           `json:"campaign_id"`
	Status          string            `json:"status"` // PASS | FAIL | PENDING
	CampaignStatus  string            `json:"campaign_status"`
	TotalRecipients int               `json:"total_recipients"`
	Checks          []pmtaVerifyCheck `json:"checks"`
	ReserveFill     map[string]int    `json:"reserve_fill"`
	Waves           pmtaVerifyWaves   `json:"waves"`
}

type pmtaVerifyResponse struct {
	Overall   string                     `json:"overall"`
	Campaigns []pmtaVerifyCampaignResult `json:"campaigns"`
}

// HandleVerifyCampaigns implements the 6-item post-conditions list.
func (s *PMTACampaignService) HandleVerifyCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	var req pmtaVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Campaigns) == 0 {
		respondError(w, http.StatusBadRequest, "campaigns is required (verify is by NAME)")
		return
	}
	if len(req.Campaigns) > 200 {
		respondError(w, http.StatusBadRequest, "at most 200 campaigns per verify call")
		return
	}

	resp := pmtaVerifyResponse{Overall: "PASS"}
	for _, c := range req.Campaigns {
		res := s.verifyOneCampaign(r, orgID, c)
		switch res.Status {
		case "FAIL":
			resp.Overall = "FAIL"
		case "PENDING":
			if resp.Overall != "FAIL" {
				resp.Overall = "PENDING"
			}
		}
		resp.Campaigns = append(resp.Campaigns, res)
	}
	respondJSON(w, http.StatusOK, resp)
}

func (s *PMTACampaignService) verifyOneCampaign(r *http.Request, orgID string, c pmtaVerifyCampaignRequest) pmtaVerifyCampaignResult {
	res := pmtaVerifyCampaignResult{Name: c.Name, Status: "PASS"}
	addCheck := func(check, status, detail string) {
		res.Checks = append(res.Checks, pmtaVerifyCheck{Check: check, Status: status, Detail: detail})
		switch status {
		case "FAIL":
			res.Status = "FAIL"
		case "PENDING":
			if res.Status != "FAIL" {
				res.Status = "PENDING"
			}
		}
	}

	name := strings.TrimSpace(c.Name)
	if name == "" {
		addCheck("by_name", "FAIL", "empty campaign name")
		return res
	}
	minRecipients := c.MinRecipients
	if minRecipients <= 0 {
		minRecipients = 1
	}
	expectWaves := c.ExpectWaves == nil || *c.ExpectWaves

	// ── 1. by-name verify ────────────────────────────────────────────────
	type matchRow struct {
		id, status       string
		totalRecipients  int
		offerID          sql.NullString
		sendingProfileID sql.NullString
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id::text, status, COALESCE(total_recipients, 0),
		       offer_id::text, sending_profile_id::text
		FROM mailing_campaigns
		WHERE organization_id = $1::uuid
		  AND name = $2
		  AND status NOT IN ('cancelled', 'deleted')
		ORDER BY created_at DESC
	`, orgID, name)
	if err != nil {
		addCheck("by_name", "FAIL", "campaign lookup error: "+err.Error())
		return res
	}
	var matches []matchRow
	for rows.Next() {
		var m matchRow
		if err := rows.Scan(&m.id, &m.status, &m.totalRecipients, &m.offerID, &m.sendingProfileID); err == nil {
			matches = append(matches, m)
		}
	}
	rows.Close()

	switch {
	case len(matches) == 0:
		addCheck("by_name", "FAIL", "no live campaign with this name (not deployed, or only cancelled/deleted siblings exist)")
		return res
	case len(matches) > 1:
		addCheck("by_name", "FAIL", fmt.Sprintf("%d live campaigns share this name — colliding names break by-name verification (AS-6.3)", len(matches)))
		return res
	}
	m := matches[0]
	res.CampaignID = &m.id
	res.CampaignStatus = m.status
	res.TotalRecipients = m.totalRecipients
	profileNote := ""
	if !m.sendingProfileID.Valid {
		// NULL sending_profile_id = "pending", never "missing" (jul13 lesson).
		profileNote = "; sending_profile_id NULL (pending)"
	}
	addCheck("by_name", "PASS", "1 campaign matched"+profileNote)

	// ── 2. finalization > 0 ─────────────────────────────────────────────
	switch m.status {
	case "failed":
		addCheck("finalized", "FAIL", fmt.Sprintf("status=failed recipients=%d (silent finalization failure class)", m.totalRecipients))
	case "draft", "finalizing_audience":
		// Caller WAITS and re-verifies — never re-POSTs the deploy.
		addCheck("finalized", "PENDING", fmt.Sprintf("status=%s — audience not finalized yet; wait, do not re-deploy", m.status))
	default:
		if m.totalRecipients < minRecipients {
			addCheck("finalized", "FAIL", fmt.Sprintf("status=%s recipients=%d < min %d", m.status, m.totalRecipients, minRecipients))
		} else {
			addCheck("finalized", "PASS", fmt.Sprintf("status=%s recipients=%d", m.status, m.totalRecipients))
		}
	}

	// ── 3. offer_id non-NULL for offer campaigns ────────────────────────
	if c.ExpectOffer {
		if !m.offerID.Valid || strings.TrimSpace(m.offerID.String) == "" {
			addCheck("offer_id", "FAIL", "offer wave with NULL offer_id — offer suppression will silently never fire")
		} else {
			addCheck("offer_id", "PASS", "offer_id="+m.offerID.String)
		}
	} else {
		addCheck("offer_id", "SKIPPED", "not an offer wave")
	}

	// ── 4. waves materialized + window spread ───────────────────────────
	var waveCount, wavePlanned int
	var firstAt, lastAt sql.NullTime
	err = s.db.QueryRowContext(r.Context(), `
		SELECT COUNT(*), COALESCE(SUM(planned_recipients), 0),
		       MIN(scheduled_at), MAX(scheduled_at)
		FROM mailing_campaign_waves
		WHERE campaign_id = $1::uuid
	`, m.id).Scan(&waveCount, &wavePlanned, &firstAt, &lastAt)
	if err != nil {
		addCheck("waves", "FAIL", "wave lookup error: "+err.Error())
	} else {
		res.Waves = pmtaVerifyWaves{Count: waveCount, PlannedRecipients: wavePlanned}
		if firstAt.Valid {
			t := firstAt.Time
			res.Waves.FirstAt = &t
		}
		if lastAt.Valid {
			t := lastAt.Time
			res.Waves.LastAt = &t
		}
		switch {
		case !expectWaves:
			addCheck("waves", "SKIPPED", "waves not expected at this stage")
		case waveCount == 0 && (m.status == "draft" || m.status == "finalizing_audience"):
			addCheck("waves", "PENDING", "no waves yet (finalization pending)")
		case waveCount == 0:
			addCheck("waves", "FAIL", fmt.Sprintf("status=%s but zero waves materialized", m.status))
		case waveCount > 1 && firstAt.Valid && lastAt.Valid && lastAt.Time.Equal(firstAt.Time):
			addCheck("waves", "FAIL", fmt.Sprintf("%d waves all scheduled at %s — window not spread", waveCount, firstAt.Time.UTC().Format(time.RFC3339)))
		default:
			spread := ""
			if firstAt.Valid && lastAt.Valid {
				spread = fmt.Sprintf(", spread %s → %s", firstAt.Time.UTC().Format(time.RFC3339), lastAt.Time.UTC().Format(time.RFC3339))
			}
			addCheck("waves", "PASS", fmt.Sprintf("%d waves, planned %d%s", waveCount, wavePlanned, spread))
		}
	}

	// ── 5. reserve-fill counts via plan recipients source_id ────────────
	fill, fillErr := s.loadReserveFill(r, m.id)
	if fillErr != nil {
		if len(c.SegmentReserves) > 0 {
			addCheck("reserve_fill", "FAIL", "plan-recipient source counts unavailable: "+fillErr.Error())
		} else {
			addCheck("reserve_fill", "SKIPPED", "no reserves asserted; source counts unavailable: "+fillErr.Error())
		}
	} else {
		res.ReserveFill = fill
		if len(c.SegmentReserves) == 0 {
			addCheck("reserve_fill", "SKIPPED", "no reserves asserted (source counts returned for observability)")
		} else {
			var failures []string
			segIDs := make([]string, 0, len(c.SegmentReserves))
			for segID := range c.SegmentReserves {
				segIDs = append(segIDs, segID)
			}
			sort.Strings(segIDs)
			for _, segID := range segIDs {
				floor := c.SegmentReserves[segID]
				if fill[segID] < floor {
					failures = append(failures, fmt.Sprintf("seg %s: %d/%d filled", segID, fill[segID], floor))
				}
			}
			if len(failures) > 0 {
				addCheck("reserve_fill", "FAIL", strings.Join(failures, "; "))
			} else {
				addCheck("reserve_fill", "PASS", fmt.Sprintf("%d reserve floor(s) met", len(c.SegmentReserves)))
			}
		}
	}

	// ── 6. explicit-quota sanity (incl. no volume:0) ────────────────────
	if !c.ExplicitQuotas {
		addCheck("quota_sanity", "SKIPPED", "explicit quotas not asserted")
	} else {
		qrows, err := s.db.QueryContext(r.Context(), `
			SELECT isp, COALESCE(quota, 0)
			FROM mailing_campaign_isp_plans
			WHERE campaign_id = $1::uuid
			ORDER BY isp
		`, m.id)
		if err != nil {
			addCheck("quota_sanity", "FAIL", "isp plan lookup error: "+err.Error())
		} else {
			var zeroISPs []string
			planCount, quotaSum := 0, 0
			for qrows.Next() {
				var isp string
				var quota int
				if qrows.Scan(&isp, &quota) == nil {
					planCount++
					quotaSum += quota
					if quota == 0 {
						zeroISPs = append(zeroISPs, isp)
					}
				}
			}
			qrows.Close()
			switch {
			case planCount == 0:
				addCheck("quota_sanity", "FAIL", "no isp plans materialized")
			case len(zeroISPs) > 0:
				addCheck("quota_sanity", "FAIL", fmt.Sprintf("quota=0 on %s — volume:0 means UNLIMITED in explicit mode (banned)", strings.Join(zeroISPs, ",")))
			case quotaSum <= 0:
				addCheck("quota_sanity", "FAIL", "explicit quotas sum to 0")
			default:
				addCheck("quota_sanity", "PASS", fmt.Sprintf("%d plans, no zero-quota, sum=%d", planCount, quotaSum))
			}
		}
	}

	return res
}

// loadReserveFill returns selected (non-reserve) plan-recipient counts per
// source segment — the measurable truth behind REQ-C17 reserve floors.
func (s *PMTACampaignService) loadReserveFill(r *http.Request, campaignID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT audience_source_id::text, COUNT(*)
		FROM mailing_campaign_plan_recipients
		WHERE campaign_id = $1::uuid
		  AND audience_source_type = 'segment'
		  AND audience_source_id IS NOT NULL
		  AND status <> 'reserve'
		GROUP BY audience_source_id
	`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fill := make(map[string]int)
	for rows.Next() {
		var segID string
		var n int
		if err := rows.Scan(&segID, &n); err == nil {
			fill[segID] = n
		}
	}
	return fill, rows.Err()
}

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/lib/pq"
)

// Engagement-tier resolution for the Campaign Manager.
//
// The send-day board picks its engaged audience by ENGAGEMENT RANGE — clickers
// and openers over a recency window — not by hunting segment UUIDs. This file
// exposes the same primitive to the portal so the operator can build an
// engagement campaign in the wizard the way the board builds one.
//
// The grid is data-driven, never a hardcoded name list: every row of
// mailing_segments with category='engagement_brand' carries its own conditions,
// and the window is read straight out of them. Verified against all 108 active
// rows in prod 2026-08-17 — three distinct shapes exist:
//
//   1. 98 rows  sending_domain=em.<apex> + email_opened|email_clicked/in_last_days
//   2.  9 rows  KUMO-ALLTIME-<CODE>-ENG: sending_domain=<apex> (bare), NO window
//   3.  1 row   EXCL Charter Family Domains: no sending_domain at all
//
// (1) becomes clickers[]/openers[]; (2) goes to other[]; (3) is skipped.

// engagementSegmentCondition is one entry of mailing_segments.conditions.
type engagementSegmentCondition struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// parseEngagementConditions extracts (sendingDomain, kind, windowDays) from a
// segment's raw conditions JSON.
//
//   - sendingDomain is "" when the segment is not domain-scoped — the caller
//     must skip such rows rather than attribute them to a property.
//   - kind is "clickers" (email_clicked), "openers" (email_opened) or "" when
//     the segment carries no recency window (the all-time KUMO pools).
//   - windowDays is 0 unless kind != "".
//
// Pure — no DB, no HTTP. Unit-tested against every shape above.
func parseEngagementConditions(raw []byte) (sendingDomain, kind string, windowDays int) {
	if len(raw) == 0 {
		return "", "", 0
	}
	var conds []engagementSegmentCondition
	if err := json.Unmarshal(raw, &conds); err != nil {
		return "", "", 0
	}
	for _, c := range conds {
		field := strings.ToLower(strings.TrimSpace(c.Field))
		switch field {
		case "sending_domain":
			if strings.EqualFold(strings.TrimSpace(c.Operator), "equals") && sendingDomain == "" {
				sendingDomain = strings.ToLower(strings.TrimSpace(c.Value))
			}
		case "email_clicked", "email_opened":
			// Only in_last_days carries a recency window. Any other operator
			// (e.g. a boolean "has ever") is not a range and must not be
			// presented as one.
			if kind != "" || !strings.EqualFold(strings.TrimSpace(c.Operator), "in_last_days") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(c.Value))
			if err != nil || n <= 0 {
				continue
			}
			if field == "email_clicked" {
				kind = "clickers"
			} else {
				kind = "openers"
			}
			windowDays = n
		}
	}
	return sendingDomain, kind, windowDays
}

// engagementTier is one selectable chip in the wizard's engagement panel.
type engagementTier struct {
	SegmentID  string `json:"segment_id"`
	Name       string `json:"name"`
	WindowDays int    `json:"window_days"`
	// Count is what will actually mail: the live row count in
	// mailing_segment_members, which is the table the audience planner
	// streams. It is NOT mailing_segments.subscriber_count — see
	// CounterCount below.
	Count int `json:"count"`
	// CounterCount is the cached mailing_segments.subscriber_count. When it
	// disagrees with Count the per-segment refresh is failing to write its
	// tally (observed in prod 2026-08-18: "DB 30D Clickers" counter=0 while
	// 1,534 members were materialized and a dry run planned 4,477 recipients
	// off it — SegmentRefreshWorker's member-count query was timing out under
	// load and persisting a zero). Surfaced so a broken counter reads as a
	// warning instead of an empty audience the operator skips.
	CounterCount    int  `json:"counter_count"`
	CounterMismatch bool `json:"counter_mismatch"`
	// CountIsLive is false when the live membership read could not be done
	// (timeout / error) and Count fell back to the cached counter.
	CountIsLive      bool   `json:"count_is_live"`
	LastCalculatedAt string `json:"last_calculated_at,omitempty"`
	Stale            bool   `json:"stale"`
}

type engagementTiersResponse struct {
	SendingDomain string           `json:"sending_domain"`
	BrandRoot     string           `json:"brand_root"`
	Clickers      []engagementTier `json:"clickers"`
	Openers       []engagementTier `json:"openers"`
	Other         []engagementTier `json:"other"`
}

// engagementStaleAfter is how old a dynamic engagement segment's last build may
// be before the UI flags it. SegmentRefreshWorker rebuilds these daily, so a
// row older than 24h means the refresh is not running and the counts on screen
// are not what would actually mail.
const engagementStaleAfter = 24 * time.Hour

// HandleEngagementTiers returns the live engagement grid (clickers/openers by
// recency window) for the property behind a sending domain.
//
// GET /api/mailing/pmta-campaign/engagement-tiers?sending_domain=m.discountblog.com
//
// Read-only. An empty grid is a 200 with empty arrays — a property with no
// engagement segments is a real, displayable state, not an error.
func (s *PMTACampaignService) HandleEngagementTiers(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	sendingDomain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain")))
	if sendingDomain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain is required"})
		return
	}

	// Engagement segments are scoped to the brand ROOT, not the literal sending
	// domain: the board mails m.discountblog.com while the segments are built
	// on em.discountblog.com history. brand.Root unions the compile-time list
	// with mailing_owned_domains, so both label forms resolve to the apex.
	root := brand.Root(sendingDomain)

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id::text, name, COALESCE(subscriber_count, 0),
		       last_calculated_at, COALESCE(conditions, '[]'::jsonb)::text
		FROM mailing_segments
		WHERE organization_id = $1
		  AND status = 'active'
		  AND category = 'engagement_brand'
		ORDER BY name
	`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	resp := engagementTiersResponse{
		SendingDomain: sendingDomain,
		BrandRoot:     root,
		Clickers:      []engagementTier{},
		Openers:       []engagementTier{},
		Other:         []engagementTier{},
	}
	now := time.Now().UTC()

	for rows.Next() {
		var (
			id, name, condJSON string
			count              int
			lastCalc           *time.Time
		)
		if err := rows.Scan(&id, &name, &count, &lastCalc, &condJSON); err != nil {
			continue
		}
		segDomain, kind, window := parseEngagementConditions([]byte(condJSON))
		if segDomain == "" {
			continue // not domain-scoped — cannot be attributed to a property
		}
		if segDomain != root && segDomain != "em."+root {
			continue
		}
		tier := engagementTier{
			SegmentID:    id,
			Name:         name,
			WindowDays:   window,
			Count:        count,
			CounterCount: count,
			Stale:        true,
		}
		if lastCalc != nil {
			tier.LastCalculatedAt = lastCalc.UTC().Format(time.RFC3339)
			tier.Stale = now.Sub(lastCalc.UTC()) > engagementStaleAfter
		}
		switch kind {
		case "clickers":
			resp.Clickers = append(resp.Clickers, tier)
		case "openers":
			resp.Openers = append(resp.Openers, tier)
		default:
			resp.Other = append(resp.Other, tier)
		}
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	sortTiers := func(t []engagementTier) {
		sort.SliceStable(t, func(i, j int) bool {
			if t[i].WindowDays != t[j].WindowDays {
				return t[i].WindowDays < t[j].WindowDays
			}
			return t[i].Name < t[j].Name
		})
	}
	sortTiers(resp.Clickers)
	sortTiers(resp.Openers)
	sortTiers(resp.Other)

	applyLiveMemberCounts(r.Context(), s.db, &resp)

	respondJSON(w, http.StatusOK, resp)
}

// coerceMasterSelectionForSegmentAudience forces use_master_selection=false on
// an audience-bound, segment-sourced payload. Returns true when it coerced.
//
// WHY THIS EXISTS. mailing_campaigns.use_master_selection defaults to TRUE in
// prod, so any deploy that omits the field gets the master-selection path. That
// path runs the operator's inclusion segments as a "hybrid prelude" and then
// TOPS THE AUDIENCE UP from mailing_subscriber_domain_state until every ISP
// quota is filled (pmta_campaign_planner.go:1589-1648). When every quota is 0 —
// the standing uncapped engaged-tier shape — allQuotasMet() can never return
// true (planner:862-881, "false if ALL ISPs are unlimited"), so the top-up has
// no stopping condition and streams the entire sending domain on top of the
// segments the operator actually chose.
//
// That combination is never intentional: an audience-bound send means "mail
// exactly these segments". So we coerce rather than reject — a 400 here would
// break live callers (blog-campaign, EDITH) for a payload we can make correct.
func coerceMasterSelectionForSegmentAudience(input *engine.PMTACampaignInput) bool {
	if input == nil {
		return false
	}
	if input.UseMasterSelection != nil && !*input.UseMasterSelection {
		return false // already explicitly segment-bound
	}
	if len(input.InclusionSegments) == 0 && len(input.InclusionLists) == 0 {
		return false // no segment audience to be bound to
	}
	if !deployInputIsUncapped(*input) {
		return false // a finite cap somewhere gives the top-up a stopping point
	}
	segmentBound := false
	input.UseMasterSelection = &segmentBound
	log.Printf("[PMTA] use_master_selection coerced to false for campaign %q — audience-bound payload "+
		"(all ISP quotas 0) with %d inclusion segment(s)/%d list(s); the master-selection top-up would "+
		"have had no quota ceiling and streamed the whole sending domain",
		input.Name, len(input.InclusionSegments), len(input.InclusionLists))
	return true
}

// applyLiveMemberCounts replaces each tier's Count with the live row count in
// mailing_segment_members — the table the audience planner actually streams.
//
// mailing_segments.subscriber_count is a cached tally written by
// SegmentRefreshWorker, and it lies when that worker's count query times out:
// prod 2026-08-18 had "DB 30D Clickers" at counter=0 with 1,534 members
// materialized, and a dry run over it planned 4,477 recipients. Showing the
// counter would have told the operator the audience was empty.
//
// Bounded and fail-soft: at most a few dozen segment ids, one grouped read on
// the segment_id index, 5s ceiling. On any error the cached counters stand and
// CountIsLive stays false, so the panel degrades to the old numbers instead of
// failing the request.
func applyLiveMemberCounts(ctx context.Context, db *sql.DB, resp *engagementTiersResponse) {
	groups := [][]engagementTier{resp.Clickers, resp.Openers, resp.Other}
	var ids []string
	for _, g := range groups {
		for _, t := range g {
			ids = append(ids, t.SegmentID)
		}
	}
	if len(ids) == 0 {
		return
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(qctx, `
		SELECT segment_id::text, COUNT(*)
		FROM mailing_segment_members
		WHERE segment_id = ANY($1::uuid[])
		GROUP BY segment_id
	`, pq.Array(ids))
	if err != nil {
		log.Printf("[engagement-tiers] live member count failed for %d segment(s), "+
			"falling back to cached subscriber_count: %v", len(ids), err)
		return
	}
	defer rows.Close()

	live := make(map[string]int, len(ids))
	for rows.Next() {
		var id string
		var n int
		if rows.Scan(&id, &n) == nil {
			live[id] = n
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[engagement-tiers] live member count read error, falling back to "+
			"cached subscriber_count: %v", err)
		return
	}

	for _, g := range groups {
		for i := range g {
			// A segment with zero materialized rows is absent from the GROUP BY
			// result; that is a genuine 0, not a missing read.
			n := live[g[i].SegmentID]
			g[i].Count = n
			g[i].CountIsLive = true
			g[i].CounterMismatch = n != g[i].CounterCount
		}
	}
}

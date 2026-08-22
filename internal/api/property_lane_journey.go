package api

// Property Ledger — lane JOURNEY view (read-only).
//
// The canvas rendering of one partner-drip lane: the touch ladder as NODES,
// the 24h follow-up delay as EDGES, and — on each edge — how many audience
// members are currently parked there waiting for their next touch, split by
// ISP family.
//
// Where each half of the picture comes from:
//
//   - NODES (touch content) mirror the orchestrator's OWN resolution:
//       touch 1  -> partner_drip_creatives (PK vertical+brand)
//                   = resolveCreative, partner_drip_orchestrator.go:1633-1640
//       touch 2+ -> partner_drip_followup_creatives, where a row carrying THIS
//                   vertical wins over a `vertical IS NULL` shared/global
//                   fallback row
//                   = resolveFollowupCreative, partner_drip_orchestrator.go:4845-4856
//     A touch number <= max_touches with no resolvable row renders
//     `configured: false` rather than being dropped — that is exactly how the
//     ladder retires early: processFollowup treats sql.ErrNoRows from the
//     lookup as "this touch is unconfigured -> retire terminal", not as an
//     error (partner_drip_orchestrator.go:4857-4859 + followupTouchConfigured).
//
//   - EDGES (waiting census) come from the live partner_clean_queue. A row's
//     `touch_count` is the number of touches already DELIVERED (markMailed
//     advances it, partner_drip_orchestrator.go:3933), so a waiting row with
//     touch_count = N sits on the edge N -> N+1.
//
// SCOPE HONESTY (same framing as the supply strip): partner_clean_queue rows
// carry NO brand until they are mailed (`mailed_brand` is stamped at claim —
// cmd/server/main.go:8120-8140), so the waiting census is a VERTICAL fact
// shared across every brand in that vertical's rotation. It is not this
// domain's private inventory, and the payload says so in `scope_note`.
//
// ORG SCOPING: none of the tables read here (partner_clean_queue,
// partner_drip_creatives, partner_drip_followup_creatives) has an
// organization_id column — see the CREATE TABLE at cmd/server/main.go:8120
// and 8159. There is therefore no org predicate to apply; `getOrgID(r)` is
// resolved and echoed back as `organization_id` so the caller can see which
// tenant context served the read, rather than the endpoint pretending to a
// scoping it cannot perform. (The sibling property-ledger handlers —
// property_ledger.go, property_lane_content.go, property_lane_supply.go —
// resolve no org at all for the same reason.)

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneJourneyDelayHours is the gap between consecutive touches.
//
// SOURCE OF TRUTH: `followupTouchGapHours = 24` in
// internal/worker/partner_drip_orchestrator.go:348 (used at :3925 to compute
// next_touch_at). That const is UNEXPORTED, so the value is restated here;
// grep `followupTouchGapHours` to check both sides when it moves.
const laneJourneyDelayHours = 24

// laneJourneyMaxTouches is the terminal touch number — read directly from the
// exported const in the worker package (partner_drip_orchestrator.go:361), so
// this one cannot drift.
const laneJourneyMaxTouches = worker.MaxTouchCount

// laneJourneyWaitingSQL — the waiting-per-edge census.
//
// ⚠️ DO NOT "SIMPLIFY" THE WHERE CLAUSE. Every clause is load-bearing: the
// statement is written to match the PARTIAL index `idx_pcq_followup_isp`
// (cmd/server/main.go:10321-10324):
//
//	CREATE INDEX CONCURRENTLY idx_pcq_followup_isp
//	  ON partner_clean_queue (vertical, touch_count, isp_family, next_touch_at)
//	  WHERE status = 'mailed' AND engaged_at IS NULL AND terminal_reason IS NULL
//
// Measured against the live queue (2026-08-18):
//
//	all four clauses present ......................................  1.26s
//	dropping `engaged_at IS NULL AND terminal_reason IS NULL` ..... 16.90s  (partial index unusable)
//	keying on dataset_id instead of vertical ...................... 56.00s  (wrong leading column)
//
// Prod runs a 30s statement_timeout, so the two "simplified" variants are a
// broken screen, not a slow one.
//
// The WHERE / GROUP BY / ORDER BY are the measured statement verbatim. The
// only addition is the `due_now` FILTER aggregate in the SELECT list, which
// is computed inside the SAME grouped scan — it changes neither the index
// predicate nor the number of passes.
const laneJourneyWaitingSQL = `
SELECT touch_count, isp_family, COUNT(*) AS waiting,
       COUNT(*) FILTER (WHERE next_touch_at <= NOW()) AS due_now,
       MIN(next_touch_at) AS soonest, MAX(next_touch_at) AS latest
  FROM partner_clean_queue
 WHERE vertical = $1
   AND status = 'mailed'
   AND engaged_at IS NULL
   AND terminal_reason IS NULL
   AND next_touch_at IS NOT NULL
 GROUP BY touch_count, isp_family
 ORDER BY touch_count, isp_family`

// laneJourneyFollowupSQL fetches every follow-up row that could serve THIS
// vertical, for every touch, in resolveFollowupCreative's precedence order
// (partner_drip_orchestrator.go:4849-4854): among rows that can actually
// serve, a vertical-specific row beats the `vertical IS NULL` global
// fallback. `active` is NOT filtered out here (the orchestrator's own lookup
// does filter it) so an INACTIVE row is still visible to the operator as a
// disabled node rather than vanishing into "unconfigured"; the `active DESC`
// leg of the ORDER BY keeps the row the orchestrator would actually pick
// first, so the resolution result is identical.
const laneJourneyFollowupSQL = `
	SELECT touch_number, COALESCE(vertical, ''), brand, creative_filename, subject_line,
	       COALESCE(preheader, ''), from_name, COALESCE(offer_id::text, ''), active
	FROM partner_drip_followup_creatives
	WHERE touch_number >= 2
	  AND (vertical = $1 OR vertical IS NULL)
	ORDER BY touch_number, active DESC, (vertical = $1) DESC NULLS LAST`

// laneJourneyISP is one ISP's share of an edge's waiting population.
type laneJourneyISP struct {
	ISP     string `json:"isp"`
	Waiting int64  `json:"waiting"`
}

// laneJourneyTouch is a NODE: what this lane mails at touch N.
type laneJourneyTouch struct {
	Touch            int    `json:"touch"`
	SubjectLine      string `json:"subject_line"`
	Preheader        string `json:"preheader"`
	FromName         string `json:"from_name"`
	CreativeFilename string `json:"creative_filename"` // "" = offer-center path
	OfferID          string `json:"offer_id,omitempty"`
	Active           bool   `json:"active"`
	// Configured: an ACTIVE row resolves for this (brand, vertical, touch).
	// False on a touch <= max_touches means the ladder RETIRES here.
	Configured bool `json:"configured"`
	// Source: "touch1" | "vertical" | "global-fallback" | "" (unconfigured).
	Source string `json:"source"`
	// ── Content resolution (operator 2026-08-21) ───────────────────────────
	// ContentSource: "file" | "offer" | "none". An EMPTY CreativeFilename is
	// the offer-center path, NOT "nothing bound" — see the resolution block
	// below for the orchestrator branches these mirror.
	ContentSource string `json:"content_source"`
	// Resolves: would the orchestrator actually produce a message for this
	// touch right now? false + Configured true = the touch is live in config
	// but WILL NOT SEND.
	Resolves bool `json:"resolves"`
	// ResolutionLabel is what the node should print instead of a bare
	// filename: the filename, "offer: <name>", or the unresolvable banner.
	ResolutionLabel   string `json:"resolution_label"`
	ResolutionDetail  string `json:"resolution_detail,omitempty"`
	ResolutionProblem string `json:"resolution_problem,omitempty"`
	// OfferSource: "dataset" (the vertical's dominant dataset binds the offer,
	// touch 1 only) or "touch-row" (this creative row's own offer_id).
	OfferSource  string            `json:"offer_source,omitempty"`
	OfferDataset string            `json:"offer_dataset,omitempty"`
	Offer        *laneJourneyOffer `json:"offer,omitempty"`
	// Perf7d: the lane's trailing-7-day wave counters for THIS touch number
	// (property_lane_journey_perf.go). nil = the perf read failed (named gap
	// via perf_7d_error) — never a zero row substituted for a failed read.
	// Counters are the denormalized mailing_campaigns columns: opens/clicks
	// undercount SES-routed engagement (see laneJourneyPerfNote).
	Perf7d *laneJourneyPerf `json:"perf_7d"`
	// Serving is the shelf's LEAD summary of what this touch actually
	// serves, derived from the resolution fields above (never a second read).
	Serving laneJourneyServing `json:"serving"`
	// VerticalConfigured mirrors followupTouchConfigured
	// (partner_drip_orchestrator.go), which tests the touch WITHOUT the brand
	// filter — it is the test the orchestrator uses to decide retirement.
	// Configured=false while VerticalConfigured=true is the stall state: the
	// record is NOT retired, but resolveFollowupCreative (which DOES filter by
	// brand) finds nothing for this brand.
	VerticalConfigured bool `json:"vertical_configured"`
}

// laneJourneyServing condenses a touch's content resolution into the shape
// the shelf leads with: WHAT ships (source + creative_label) and WHICH
// subject line(s) it ships under.
//
// subject_mode="rotating_pool" (the offer-center path): the subject is drawn
// from the offer's active subject pool at send time and ROTATION IS EVEN
// ACROSS THE POOL — there is no single "default" subject. `subject` is a
// SAMPLE (the pool's first active row, the orchestrator's ORDER BY id) and
// `pool_size` says how many rotate. subject_mode="fixed" (file path /
// unconfigured): the creative row's own subject_line, verbatim.
type laneJourneyServing struct {
	Source        string `json:"source"` // "offer_pool" | "file" | "none"
	CreativeLabel string `json:"creative_label"`
	SubjectMode   string `json:"subject_mode"` // "fixed" | "rotating_pool"
	Subject       string `json:"subject"`
	PoolSize      int64  `json:"pool_size,omitempty"`
}

// laneJourneyBuildServing derives the serving summary from a node's ALREADY
// RESOLVED fields (laneJourneyApplyResolution runs first) — no extra query.
func laneJourneyBuildServing(t *laneJourneyTouch) laneJourneyServing {
	switch t.ContentSource {
	case "file":
		return laneJourneyServing{
			Source:        "file",
			CreativeLabel: t.ResolutionLabel,
			SubjectMode:   "fixed",
			Subject:       t.SubjectLine,
		}
	case "offer":
		s := laneJourneyServing{
			Source:        "offer_pool",
			CreativeLabel: t.ResolutionLabel,
			// Rotation is even across the pool — no single 'default' subject.
			SubjectMode: "rotating_pool",
		}
		if t.Offer != nil {
			s.PoolSize = t.Offer.Subjects
			s.Subject = t.Offer.FirstSubject
		}
		return s
	default:
		return laneJourneyServing{
			Source:        "none",
			CreativeLabel: t.ResolutionLabel,
			SubjectMode:   "fixed",
			Subject:       t.SubjectLine,
		}
	}
}

// laneJourneyEdge is the delay between two touches and who is parked on it.
type laneJourneyEdge struct {
	FromTouch int              `json:"from_touch"`
	ToTouch   int              `json:"to_touch"`
	Waiting   int64            `json:"waiting"`
	DueNow    int64            `json:"due_now"`
	Soonest   *time.Time       `json:"soonest"`
	Latest    *time.Time       `json:"latest"`
	ByISP     []laneJourneyISP `json:"by_isp"`
}

type laneJourneyTotals struct {
	InFlight int64 `json:"in_flight"`
	DueNow   int64 `json:"due_now"`
}

const laneJourneyScopeNote = "waiting counts are a VERTICAL fact — partner_clean_queue rows carry no brand until they are mailed (mailed_brand is stamped at claim), so this population is shared across every brand in the vertical's rotation"

// laneJourneyEdgeAgg accumulates one edge while scanning the census.
type laneJourneyEdgeAgg struct {
	waiting, dueNow int64
	soonest, latest sql.NullTime
	byISP           map[string]int64
}

// HandleLaneJourney GET …/property-ledger/lane-journey?brand=<code>&vertical=<v>
func (s *PMTACampaignService) HandleLaneJourney(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	brand := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand")))
	vertical := strings.TrimSpace(r.URL.Query().Get("vertical"))

	if !propertyLedgerValidLaneBrand(r.Context(), s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	if vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required")
		return
	}
	if !laneJourneyValidVertical(vertical) {
		respondError(w, http.StatusBadRequest, "vertical must be a lowercase feed key (letters, digits, '_' or '-')")
		return
	}
	orgID := getOrgID(r)
	staleHours := laneJourneyStaleHours(r.URL.Query().Get("stale_hours"))
	sendingDomain, _ := worker.BrandSendingDomain(brand)

	// The vertical's DOMINANT dataset decides whether touch 1 resolves through
	// the offer-center path at all (refreshVerticalState phase 2). Read first
	// so the touch-1 node can be labeled correctly. sql.ErrNoRows = no ready
	// row in the queue: no dominant dataset, so no dataset-bound offer.
	var domDatasetID, domDatasetSlug, domOfferID string
	switch err := s.db.QueryRowContext(ctx, laneJourneyDominantOfferSQL, vertical).
		Scan(&domDatasetID, &domDatasetSlug, &domOfferID); err {
	case nil, sql.ErrNoRows:
	default:
		respondError(w, http.StatusInternalServerError, "dominant-dataset query failed")
		return
	}

	// ---- NODES ------------------------------------------------------------
	touches := make([]laneJourneyTouch, 0, laneJourneyMaxTouches)

	// Touch 1: partner_drip_creatives, PK (vertical, brand) — resolveCreative.
	t1 := laneJourneyTouch{Touch: 1}
	err := s.db.QueryRowContext(ctx, `
		SELECT creative_filename, subject_line, COALESCE(preheader, ''), from_name,
		       COALESCE(offer_id::text, ''), active
		FROM partner_drip_creatives
		WHERE vertical = $1 AND brand = $2`, vertical, brand).
		Scan(&t1.CreativeFilename, &t1.SubjectLine, &t1.Preheader, &t1.FromName,
			&t1.OfferID, &t1.Active)
	switch err {
	case nil:
		t1.Configured = t1.Active
		t1.VerticalConfigured = t1.Active
		if t1.Active {
			t1.Source = "touch1"
		}
	case sql.ErrNoRows:
		// No welcome row for (vertical, brand) — resolveCreative errors out
		// loud and the lane never mails. Rendered as an unconfigured node.
	default:
		respondError(w, http.StatusInternalServerError, "touch-1 creative query failed")
		return
	}
	touches = append(touches, t1)

	// Touches 2+: one scan, resolved in the orchestrator's precedence order.
	type followupRow struct {
		touch  int
		vert   string
		brand  string
		t      laneJourneyTouch
		active bool
	}
	followups := []followupRow{}
	err = func() error {
		rows, err := s.db.QueryContext(ctx, laneJourneyFollowupSQL, vertical)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f followupRow
			if err := rows.Scan(&f.touch, &f.vert, &f.brand, &f.t.CreativeFilename,
				&f.t.SubjectLine, &f.t.Preheader, &f.t.FromName, &f.t.OfferID, &f.active); err != nil {
				return err
			}
			f.t.Touch = f.touch
			f.t.Active = f.active
			followups = append(followups, f)
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "follow-up creative query failed")
		return
	}

	for touch := 2; touch <= laneJourneyMaxTouches; touch++ {
		// The SQL order (active DESC, vertical-specific DESC) means the FIRST
		// row matching this brand is exactly the one resolveFollowupCreative
		// would pick: active vertical-specific > active global fallback. If only
		// inactive rows exist we still render one, as a disabled node.
		node := laneJourneyTouch{Touch: touch}
		picked := false
		verticalConfigured := false
		for _, f := range followups {
			if f.touch != touch {
				continue
			}
			if f.active {
				verticalConfigured = true // followupTouchConfigured: no brand filter
			}
			if picked || f.brand != brand {
				continue
			}
			node = f.t
			node.Touch = touch
			node.Configured = f.active
			if f.active {
				if f.vert == "" {
					node.Source = "global-fallback"
				} else {
					node.Source = "vertical"
				}
			}
			picked = true
		}
		node.VerticalConfigured = verticalConfigured
		touches = append(touches, node)
	}

	// ---- CONTENT RESOLUTION ------------------------------------------------
	// One pool census for the ladder's whole distinct offer set, then the
	// orchestrator's branch order applied per node.
	offerIDs := []string{}
	seenOffer := map[string]bool{}
	addOffer := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seenOffer[id] {
			return
		}
		seenOffer[id] = true
		offerIDs = append(offerIDs, id)
	}
	if domOfferID != "" {
		addOffer(domOfferID)
	}
	for i := range touches {
		if touches[i].Configured && strings.TrimSpace(touches[i].CreativeFilename) == "" {
			addOffer(touches[i].OfferID)
		}
	}
	offers, err := s.laneJourneyResolveOffers(ctx, offerIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer-resolution query failed")
		return
	}
	for i := range touches {
		laneJourneyApplyResolution(&touches[i], domOfferID, domDatasetSlug, offers)
		touches[i].Serving = laneJourneyBuildServing(&touches[i])
	}

	// ---- EDGES ------------------------------------------------------------
	aggs := map[int]*laneJourneyEdgeAgg{}
	err = func() error {
		rows, err := s.db.QueryContext(ctx, laneJourneyWaitingSQL, vertical)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var touchCount int
			var isp string
			var waiting, dueNow int64
			var soonest, latest sql.NullTime
			if err := rows.Scan(&touchCount, &isp, &waiting, &dueNow, &soonest, &latest); err != nil {
				return err
			}
			a, ok := aggs[touchCount]
			if !ok {
				a = &laneJourneyEdgeAgg{byISP: map[string]int64{}}
				aggs[touchCount] = a
			}
			a.waiting += waiting
			a.dueNow += dueNow
			a.byISP[strings.ToLower(strings.TrimSpace(isp))] += waiting
			if soonest.Valid && (!a.soonest.Valid || soonest.Time.Before(a.soonest.Time)) {
				a.soonest = soonest
			}
			if latest.Valid && (!a.latest.Valid || latest.Time.After(a.latest.Time)) {
				a.latest = latest
			}
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "waiting-census query failed")
		return
	}

	// Edge keys = the structural ladder (1 -> 2 … max-1 -> max) UNION every
	// touch_count actually observed. The structural edges are emitted even at
	// zero so the canvas always draws the full ladder; an observed key outside
	// that range (e.g. touch_count 0, which markMailed should have advanced)
	// is surfaced rather than hidden.
	keySet := map[int]bool{}
	for t := 1; t < laneJourneyMaxTouches; t++ {
		keySet[t] = true
	}
	for k := range aggs {
		keySet[k] = true
	}
	keys := make([]int, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	edges := make([]laneJourneyEdge, 0, len(keys))
	totals := laneJourneyTotals{}
	for _, k := range keys {
		e := laneJourneyEdge{FromTouch: k, ToTouch: k + 1, ByISP: []laneJourneyISP{}}
		if a, ok := aggs[k]; ok {
			e.Waiting = a.waiting
			e.DueNow = a.dueNow
			if a.soonest.Valid {
				t := a.soonest.Time.UTC()
				e.Soonest = &t
			}
			if a.latest.Valid {
				t := a.latest.Time.UTC()
				e.Latest = &t
			}
			e.ByISP = laneJourneyOrderByISP(a.byISP)
		}
		totals.InFlight += e.Waiting
		totals.DueNow += e.DueNow
		edges = append(edges, e)
	}

	// ---- PER-TOUCH 7D PERFORMANCE (aux read — degrades, never fails the
	// canvas; property_lane_journey_perf.go). Runs LAST so its single query
	// sits after the core reads.
	perfErr := s.laneJourneyAttachPerf(ctx, orgID, vertical, brand, touches)

	resp := map[string]interface{}{
		"organization_id": orgID,
		"brand":           brand,
		"sending_domain":  sendingDomain,
		"vertical":        vertical,
		"delay_hours":     laneJourneyDelayHours,
		"max_touches":     laneJourneyMaxTouches,
		"touches":         touches,
		"perf_7d_note":    laneJourneyPerfNote,
		"edges":           edges,
		"totals":          totals,
		"send_activity":   s.laneJourneyActivity(ctx, orgID, vertical, brand, staleHours),
		// The dataset whose offer binding decides touch 1 (oldest ready row).
		// Empty = the vertical has no ready row, so no dataset-bound offer.
		"dominant_dataset": map[string]interface{}{
			"dataset_id": domDatasetID,
			"slug":       domDatasetSlug,
			"offer_id":   domOfferID,
			"note":       "the dataset owning the OLDEST status='ready' row — refreshVerticalState phase 2. When it carries an offer_id, touch 1 resolves through the offer centre and partner_drip_creatives is not consulted.",
		},
		"scope_note":   laneJourneyScopeNote,
		"generated_at": time.Now().UTC(),
	}
	if perfErr != "" {
		resp["perf_7d_error"] = perfErr
	}
	respondJSON(w, http.StatusOK, resp)
}

// laneJourneyValidVertical is a cheap charset gate — a feed key is lowercase
// letters/digits with '_' or '-' (e.g. "internal_auto_insurance"). It keeps
// junk out of the census query without paying for a roster lookup.
func laneJourneyValidVertical(v string) bool {
	if len(v) == 0 || len(v) > 64 {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// laneJourneyOrderByISP renders an edge's ISP split in the canonical ledger
// order, reusing the supply strip's ordering (isppkg.LedgerGroups first,
// strangers appended alphabetically rather than hidden) so the two screens
// never disagree on ISP order.
func laneJourneyOrderByISP(counts map[string]int64) []laneJourneyISP {
	ordered := laneSupplyOrderReadyByISP(counts)
	out := make([]laneJourneyISP, 0, len(ordered))
	for _, e := range ordered {
		out = append(out, laneJourneyISP{ISP: e.ISP, Waiting: e.Ready})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// SEND ACTIVITY (operator 2026-08-21: "it is very challenging to infer if mail
// is being sent or not … it makes it seem like this lane is static")
//
// The canvas's in-flight / due-now / waiting numbers are all STOCK. A stalled
// lane and a flowing lane render identically. This block is the FLOW half.
//
// ⚠️ THE COUNTING RULE THIS BLOCK EXISTS FOR — do not collapse the two lines.
// partner_drip_state.last_wave_at is written by updateDripState
// (partner_drip_orchestrator.go:1490, welcome pass ONLY — the single call
// site). The FOLLOW-UP pass rotates brands off a SEPARATE shared row
// (vertical='followup') via persistFollowupBrandIndex, which writes
// next_brand_index + updated_at and NEVER last_wave_at. So:
//
//   - a lane mailing only follow-ups has a stale/NULL per-vertical
//     last_wave_at while it is actively sending;
//   - the 'followup' row's own last_wave_at is NULL for every lane, always.
//
// Verified on prod 2026-08-22: partner_drip_state('internal_auto_insurance')
// last_wave_at = 04:45:25Z, partner_drip_state('followup') last_wave_at = NULL,
// while 37 follow-up waves shipped 2,655 messages in the same 30h window.
//
// A single merged verdict is therefore WRONG. Welcome and follow-up each get
// their own last-activity time, counts and verdict, and follow-up activity is
// derived from CAMPAIGN ROWS (the ` [tN]` name suffix appended by
// deployWaveGroups, partner_drip_orchestrator.go:4691/4740), never from
// partner_drip_state.

// laneJourneyActivitySQL — per-family wave + message counts for one
// (vertical, brand) lane.
//
// ACCESS PATH (copied from stampRecoveryWorklistSQL,
// partner_drip_stamp_recovery.go:490-503 — the proven shape): the driver is
// `idx_campaigns_org_status_created (organization_id, status, created_at DESC)`.
// mailing_campaigns has NO index on `name`, so the regex is a filter on the
// already-narrow recent slice, never the driver. Measured on prod 2026-08-22
// for (internal_auto_insurance, db) over a 30h window: 101 campaigns,
// 0.56–0.92s warm / 4.75s cold — inside the prod 30s statement_timeout.
//
// `sent` is COUNT(mailing_message_log) per campaign, which is the same
// authority RecoverUnstampedTouches uses for "was this actually sent"
// (partner_drip_stamp_recovery.go). It is deliberately NOT
// mailing_campaigns.sent_count: measured on the same window, sent_count read
// 682/2658 vs message_log 677/2655 because sent_count is stamped ahead of the
// physical send.
//
// $1 org, $2 window start (the earlier of the Denver day start and now-1h),
// $3 vertical, $4 brand, $5 hour cutoff, $6 Denver day start.
const laneJourneyActivitySQL = `
WITH lane AS (
	SELECT c.id, c.created_at,
	       (c.name ~ ' \[t[0-9]+\]$') AS is_followup
	FROM mailing_campaigns c
	WHERE c.organization_id = $1::uuid
	  AND c.status IN ('sending','sent','completed','completed_with_errors')
	  AND c.created_at >= $2
	  AND c.partner_drip_tag IS NOT NULL
	  AND c.name ~ ('^\[partner-drip\] ' || $3 || ' ' || $4 || ' ')
), counted AS (
	SELECT lane.is_followup, lane.created_at, COALESCE(ml.n, 0) AS sent
	FROM lane
	LEFT JOIN LATERAL (
		SELECT COUNT(*) AS n FROM mailing_message_log m WHERE m.campaign_id = lane.id
	) ml ON TRUE
)
SELECT is_followup,
       MAX(created_at)                                            AS last_wave_at,
       COUNT(*) FILTER (WHERE created_at >= $5)                    AS waves_1h,
       COALESCE(SUM(sent) FILTER (WHERE created_at >= $5), 0)      AS sent_1h,
       COUNT(*) FILTER (WHERE created_at >= $6)                    AS waves_today,
       COALESCE(SUM(sent) FILTER (WHERE created_at >= $6), 0)      AS sent_today
FROM counted
GROUP BY is_followup`

// laneJourneyStaleDefaultHours is the FLOWING/STALLED threshold. Overridable
// per request via ?stale_hours= (1..48) so a slow lane can be judged on its own
// cadence; the default matches the operator's stated 2h.
const laneJourneyStaleDefaultHours = 2

// laneJourneyActivityBudget caps the send-activity flow read with its OWN
// deadline. It is the heaviest statement on this endpoint (0.56–0.92s warm /
// 4.75s cold measured 2026-08-22 — and unbounded when RDS is IO-starved), and
// it is AUXILIARY: without a budget of its own a stalled activity read
// consumes the whole journey request. On expiry the read fails soft into the
// named Error gap (laneJourneyActivity never fails the response) and the
// canvas still draws.
const laneJourneyActivityBudget = 8 * time.Second

// laneJourneyActivityFamily is one flow line — WELCOME (touch 1) or FOLLOW-UP
// (touches 2+). Never merged.
type laneJourneyActivityFamily struct {
	Family string `json:"family"` // "welcome" | "followup"
	// LastWaveAt is the newest campaign row for this family on this
	// (vertical, brand). nil = no wave in the window — ABSENT, never "0".
	LastWaveAt  *time.Time `json:"last_wave_at"`
	AgeSeconds  *int64     `json:"age_seconds"`
	Waves1h     int64      `json:"waves_1h"`
	Sent1h      int64      `json:"sent_1h"`
	WavesToday  int64      `json:"waves_today"`
	SentToday   int64      `json:"sent_today"`
	Verdict     string     `json:"verdict"` // "flowing" | "stalled"
	VerdictNote string     `json:"verdict_note"`
}

// laneJourneyRotationState is partner_drip_state for the vertical, reported
// verbatim and labeled for what it actually is: a VERTICAL-wide rotation
// pointer written only by the welcome pass. It is NOT this brand's send
// record and it is NOT evidence about follow-ups.
type laneJourneyRotationState struct {
	Present    bool       `json:"present"`
	LastWaveAt *time.Time `json:"last_wave_at"`
	LastBrand  string     `json:"last_wave_brand,omitempty"`
	LastSize   *int64     `json:"last_wave_size,omitempty"`
	Note       string     `json:"note"`
}

type laneJourneySendActivity struct {
	StaleHours    int                         `json:"stale_hours"`
	DenverDay     string                      `json:"denver_day"`
	WindowStart   time.Time                   `json:"window_start"`
	Families      []laneJourneyActivityFamily `json:"families"`
	RotationState laneJourneyRotationState    `json:"rotation_state"`
	CountingNote  string                      `json:"counting_note"`
	// Error is a NAMED GAP: the activity read failed while the ladder read
	// succeeded. The strip renders the failure; the canvas still draws. A
	// zero row is never substituted for a failed read.
	Error string `json:"error,omitempty"`
}

const laneJourneyCountingNote = "WELCOME and FOLLOW-UP are counted separately and must not be merged. partner_drip_state.last_wave_at is written ONLY by the welcome pass (updateDripState); the follow-up pass rotates off a shared vertical='followup' row that never carries last_wave_at. Follow-up activity here is derived from campaign rows carrying the ` [tN]` touch suffix. `sent` counts mailing_message_log rows for each wave's campaign (the same authority the ladder-stamp recovery uses), not mailing_campaigns.sent_count."

const laneJourneyRotationNote = "partner_drip_state row for the VERTICAL: a rotation pointer written only by the welcome pass, shared across every brand in the vertical's rotation. It is not this brand's send record, and it says nothing about follow-ups."

// laneJourneyActivity runs the flow read. It NEVER fails the response: a query
// error comes back as a named gap on the strip (Error set, families empty), so
// a heavy auxiliary read cannot take the canvas down.
func (s *PMTACampaignService) laneJourneyActivity(ctx context.Context, orgID, vertical, brand string, staleHours int) laneJourneySendActivity {
	// Own short budget: this read must never consume the journey request's
	// whole deadline. A timeout lands in the same fail-soft Error path as any
	// other query error below.
	ctx, cancel := context.WithTimeout(ctx, laneJourneyActivityBudget)
	defer cancel()
	now := time.Now()
	dayStart, _ := laneSupplyDenverDayBoundsUTC(now)
	hourCut := now.Add(-time.Hour).UTC()
	windowStart := dayStart
	if hourCut.Before(windowStart) {
		// Just after Denver midnight the "last hour" reaches into yesterday.
		windowStart = hourCut
	}
	out := laneJourneySendActivity{
		StaleHours:   staleHours,
		DenverDay:    now.In(propertyLedgerLoc).Format("2006-01-02"),
		WindowStart:  windowStart,
		Families:     []laneJourneyActivityFamily{},
		CountingNote: laneJourneyCountingNote,
		RotationState: laneJourneyRotationState{
			Note: laneJourneyRotationNote,
		},
	}

	// The vertical's rotation pointer, reported as itself.
	var stateAt sql.NullTime
	var stateBrand sql.NullString
	var stateSize sql.NullInt64
	switch err := s.db.QueryRowContext(ctx, `
		SELECT last_wave_at, last_wave_brand, last_wave_size
		FROM partner_drip_state WHERE vertical = $1`, vertical).
		Scan(&stateAt, &stateBrand, &stateSize); err {
	case nil:
		out.RotationState.Present = true
		if stateAt.Valid {
			t := stateAt.Time.UTC()
			out.RotationState.LastWaveAt = &t
		}
		out.RotationState.LastBrand = strings.TrimSpace(stateBrand.String)
		if stateSize.Valid {
			v := stateSize.Int64
			out.RotationState.LastSize = &v
		}
	case sql.ErrNoRows:
		// No rotation row: the welcome pass has never fired for this vertical.
	default:
		out.Error = "rotation-state read failed"
		return out
	}

	agg := map[bool]*laneJourneyActivityFamily{}
	err := func() error {
		rows, err := s.db.QueryContext(ctx, laneJourneyActivitySQL,
			orgID, windowStart, vertical, brand, hourCut, dayStart)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var isFollowup bool
			var last sql.NullTime
			f := &laneJourneyActivityFamily{}
			if err := rows.Scan(&isFollowup, &last, &f.Waves1h, &f.Sent1h,
				&f.WavesToday, &f.SentToday); err != nil {
				return err
			}
			if last.Valid {
				t := last.Time.UTC()
				f.LastWaveAt = &t
				age := int64(time.Since(t).Seconds())
				if age < 0 {
					age = 0
				}
				f.AgeSeconds = &age
			}
			agg[isFollowup] = f
		}
		return rows.Err()
	}()
	if err != nil {
		out.Error = "send-activity read failed"
		return out
	}

	for _, spec := range []struct {
		key  bool
		name string
	}{{false, "welcome"}, {true, "followup"}} {
		f := agg[spec.key]
		if f == nil {
			f = &laneJourneyActivityFamily{}
		}
		f.Family = spec.name
		f.Verdict, f.VerdictNote = laneJourneyVerdict(spec.name, f.LastWaveAt, staleHours, windowStart)
		out.Families = append(out.Families, *f)
	}
	return out
}

// laneJourneyVerdict grades ONE family. FLOWING requires a wave inside the
// stale threshold; anything else is STALLED and says so plainly.
func laneJourneyVerdict(family string, last *time.Time, staleHours int, windowStart time.Time) (string, string) {
	label := "welcome (touch 1)"
	if family == "followup" {
		label = "follow-up (touches 2+)"
	}
	if last == nil {
		return "stalled", fmt.Sprintf("STALLED — no %s wave since %s. Nothing has been deployed on this lane in the read window.",
			label, windowStart.UTC().Format(time.RFC3339))
	}
	age := time.Since(*last)
	if age <= time.Duration(staleHours)*time.Hour {
		return "flowing", fmt.Sprintf("FLOWING — last %s wave %s ago (threshold %dh).",
			label, laneJourneyShortDur(age), staleHours)
	}
	return "stalled", fmt.Sprintf("STALLED — last %s wave was %s ago, past the %dh threshold.",
		label, laneJourneyShortDur(age), staleHours)
}

func laneJourneyShortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// laneJourneyStaleHours parses ?stale_hours=, clamped to 1..48.
func laneJourneyStaleHours(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > 48 {
		return laneJourneyStaleDefaultHours
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATIVE RESOLUTION (operator 2026-08-21: the canvas rendered
// `(no creative bound)` on every touch — "that is FALSE").
//
// An empty creative_filename does NOT mean nothing is bound. It means the
// creative resolves through the OFFER-CENTER path. Verified on prod
// 2026-08-22 for (db, internal_auto_insurance): all four configured touches
// carry creative_filename='' AND offer_id=48ddcdf4-… (offer
// "AutoCoveragePoint - Quote Ready"), i.e. every one of them was labeled
// "no creative bound" while shipping a real, approved offer creative.
//
// The resolution mirrored here is the orchestrator's OWN, verbatim:
//
//   TOUCH 1 — resolveCreativeForVertical (partner_drip_orchestrator.go:1577):
//     (a) if the vertical's DOMINANT dataset carries offer_id, that offer
//         drives the creative and partner_drip_creatives is NOT consulted at
//         all (v.offerID branch at :1581). "Dominant" = the dataset owning the
//         OLDEST status='ready' row (refreshVerticalState phase 2, :900-912) —
//         it is a live, moving fact, not a static config.
//         ⚠️ The brief for this change said "partner_datasets.offer_id"; the
//         source is narrower (dominant dataset only) and the payload says so.
//     (b) otherwise resolveCreative (:1654) reads partner_drip_creatives:
//           filename non-empty            -> DISK, docs/emails/<filename>
//           filename empty + offer_id set -> offer-center for the ROW's offer (:1669)
//           filename empty + no offer_id  -> os.ReadFile("") -> the wave ERRORS
//
//   TOUCH 2+ — resolveFollowupCreative (:4900): a configured
//     creative_filename WINS (disk read at :4931); the row's own offer_id is
//     used ONLY when the filename is empty (:4924).
//
// OFFER PATH FAILURE MODES (resolveOfferCreative, :1828). All three pools must
// be non-empty or the wave HARD-ERRORS and the claim is released:
//     creatives   status NOT IN ('archived','rejected') AND html_content <> ''
//     subjects    status NOT IN ('archived','rejected') AND subject_line <> ''
//     from_names  status NOT IN ('archived','rejected') AND from_name  <> ''
//                 -> "offer %s has no active from-names" (:1868, :1913)
// An offer whose pool is entirely archived is therefore UNRESOLVABLE, and the
// payload names WHICH pool is empty.

// laneJourneyDominantOfferSQL mirrors refreshVerticalState's phase-2 lookup
// (partner_drip_orchestrator.go:900-912) — the dataset owning the OLDEST ready
// row decides the vertical's offer binding.
const laneJourneyDominantOfferSQL = `
	SELECT COALESCE(d.id::text, ''), COALESCE(d.slug, ''), COALESCE(d.offer_id::text, '')
	FROM (
		SELECT dataset_id
		FROM partner_clean_queue
		WHERE vertical = $1 AND status = 'ready'
		ORDER BY ingested_at ASC
		LIMIT 1
	) AS dom
	LEFT JOIN partner_datasets d ON d.id = dom.dataset_id`

// laneJourneyOfferPoolSQL counts each offer's three pools with the EXACT
// predicates resolveOfferCreative / fetchOfferSubjects / fetchOfferFromNames
// filter on. One query for the whole ladder's distinct offer set. The last
// column is the pool's FIRST active subject (fetchOfferSubjects' ORDER BY
// id), served as the serving strip's SAMPLE — a scalar subquery on the same
// indexed offer_id path the COUNT beside it already walks, never a join.
const laneJourneyOfferPoolSQL = `
	SELECT o.id::text, COALESCE(o.name, ''), COALESCE(o.status, ''),
	       (SELECT COUNT(*) FROM mailing_offer_creatives c
	         WHERE c.offer_id = o.id
	           AND COALESCE(c.status, '') NOT IN ('archived','rejected')
	           AND COALESCE(c.html_content, '') <> ''),
	       (SELECT COUNT(*) FROM mailing_offer_subject_lines sl
	         WHERE sl.offer_id = o.id
	           AND COALESCE(sl.status, '') NOT IN ('archived','rejected')
	           AND COALESCE(sl.subject_line, '') <> ''),
	       (SELECT COUNT(*) FROM mailing_offer_from_names fn
	         WHERE fn.offer_id = o.id
	           AND COALESCE(fn.status, '') NOT IN ('archived','rejected')
	           AND COALESCE(fn.from_name, '') <> ''),
	       COALESCE((SELECT sl2.subject_line FROM mailing_offer_subject_lines sl2
	         WHERE sl2.offer_id = o.id
	           AND COALESCE(sl2.status, '') NOT IN ('archived','rejected')
	           AND COALESCE(sl2.subject_line, '') <> ''
	         ORDER BY sl2.id LIMIT 1), '')
	FROM mailing_offers o
	WHERE o.id::text = ANY($1)`

// laneJourneyOffer is one offer's resolvability, as the orchestrator would
// find it at send time.
type laneJourneyOffer struct {
	OfferID    string `json:"offer_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Found      bool   `json:"found"`
	Creatives  int64  `json:"active_creatives"`
	Subjects   int64  `json:"active_subjects"`
	FromNames  int64  `json:"active_from_names"`
	// FirstSubject is the pool's first active row (fetchOfferSubjects'
	// ORDER BY id) — a SAMPLE only: rotation is even across the pool and
	// there is no single 'default' subject.
	FirstSubject string `json:"first_active_subject,omitempty"`
	Resolvable   bool   `json:"resolvable"`
	Problem      string `json:"problem,omitempty"`
}

// laneJourneyResolveOffers loads the pool census for a set of offer ids.
func (s *PMTACampaignService) laneJourneyResolveOffers(ctx context.Context, ids []string) (map[string]*laneJourneyOffer, error) {
	out := map[string]*laneJourneyOffer{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		out[id] = &laneJourneyOffer{OfferID: id}
	}
	rows, err := s.db.QueryContext(ctx, laneJourneyOfferPoolSQL, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, status, firstSubject string
		var creatives, subjects, fromNames int64
		if err := rows.Scan(&id, &name, &status, &creatives, &subjects, &fromNames, &firstSubject); err != nil {
			return nil, err
		}
		o, ok := out[id]
		if !ok {
			o = &laneJourneyOffer{OfferID: id}
			out[id] = o
		}
		o.Found = true
		o.Name = name
		o.Status = status
		o.Creatives = creatives
		o.Subjects = subjects
		o.FromNames = fromNames
		o.FirstSubject = firstSubject
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, o := range out {
		o.Resolvable, o.Problem = laneJourneyOfferVerdict(o)
	}
	return out, nil
}

// laneJourneyOfferVerdict names the FIRST pool that would hard-error the send.
func laneJourneyOfferVerdict(o *laneJourneyOffer) (bool, string) {
	switch {
	case !o.Found:
		return false, "offer row not found in mailing_offers — the wave errors on lookup"
	case o.Creatives == 0:
		return false, "offer CREATIVE pool is empty (every mailing_offer_creatives row is archived/rejected or has no html_content) — the wave errors on lookup"
	case o.Subjects == 0:
		return false, "offer SUBJECT pool is empty (every mailing_offer_subject_lines row is archived/rejected) — the send hard-errors: \"offer has no active subjects\""
	case o.FromNames == 0:
		return false, "offer FROM-NAME pool is empty (every mailing_offer_from_names row is archived/rejected) — the send hard-errors: \"offer has no active from-names\""
	}
	return true, ""
}

// laneJourneyApplyResolution fills a node's content-path fields, mirroring the
// orchestrator's branch order exactly. datasetOffer is the dominant dataset's
// offer (touch 1 only); it is ignored for touches 2+.
func laneJourneyApplyResolution(t *laneJourneyTouch, datasetOffer, datasetSlug string, offers map[string]*laneJourneyOffer) {
	if !t.Configured {
		t.ContentSource = "none"
		t.Resolves = false
		t.ResolutionLabel = "no active row — the ladder retires here"
		return
	}

	offerID := ""
	switch {
	case t.Touch == 1 && datasetOffer != "":
		// resolveCreativeForVertical branch (a): the dataset's offer wins and
		// partner_drip_creatives is never read.
		offerID = datasetOffer
		t.OfferSource = "dataset"
		t.OfferDataset = datasetSlug
	case strings.TrimSpace(t.CreativeFilename) != "":
		// A configured filename WINS on every touch (disk read).
		t.ContentSource = "file"
		t.Resolves = true
		t.ResolutionLabel = t.CreativeFilename
		t.ResolutionDetail = "file — read from the orchestrator's creatives dir at send time"
		if t.OfferID != "" {
			t.ResolutionDetail += "; the row's offer_id is used for suppression/attribution only"
		}
		return
	case t.OfferID != "":
		offerID = t.OfferID
		t.OfferSource = "touch-row"
	default:
		t.ContentSource = "none"
		t.Resolves = false
		t.ResolutionLabel = "UNRESOLVABLE — touch will not send"
		t.ResolutionProblem = "the row has neither a creative_filename nor an offer_id: the resolver reads an empty path and the wave errors"
		return
	}

	t.ContentSource = "offer"
	o := offers[offerID]
	if o == nil {
		t.Resolves = false
		t.ResolutionLabel = "UNRESOLVABLE — touch will not send"
		t.ResolutionProblem = "offer " + offerID + " could not be read"
		return
	}
	t.Offer = o
	name := o.Name
	if name == "" {
		name = offerID
	}
	if o.Resolvable {
		t.Resolves = true
		t.ResolutionLabel = "offer: " + name
		t.ResolutionDetail = fmt.Sprintf("offer-center path — %d active creative(s), %d subject(s), %d from-name(s); the wave ships the newest approved creative",
			o.Creatives, o.Subjects, o.FromNames)
		return
	}
	t.Resolves = false
	t.ResolutionLabel = "UNRESOLVABLE — touch will not send"
	t.ResolutionProblem = "offer \"" + name + "\": " + o.Problem
}

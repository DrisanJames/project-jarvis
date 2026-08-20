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
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"

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
	// VerticalConfigured mirrors followupTouchConfigured
	// (partner_drip_orchestrator.go), which tests the touch WITHOUT the brand
	// filter — it is the test the orchestrator uses to decide retirement.
	// Configured=false while VerticalConfigured=true is the stall state: the
	// record is NOT retired, but resolveFollowupCreative (which DOES filter by
	// brand) finds nothing for this brand.
	VerticalConfigured bool `json:"vertical_configured"`
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

	if !propertyLedgerValidBrand(brand) {
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
	sendingDomain, _ := worker.BrandSendingDomain(brand)

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

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"organization_id": orgID,
		"brand":           brand,
		"sending_domain":  sendingDomain,
		"vertical":        vertical,
		"delay_hours":     laneJourneyDelayHours,
		"max_touches":     laneJourneyMaxTouches,
		"touches":         touches,
		"edges":           edges,
		"totals":          totals,
		"scope_note":      laneJourneyScopeNote,
		"generated_at":    time.Now().UTC(),
	})
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

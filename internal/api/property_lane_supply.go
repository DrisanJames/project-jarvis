package api

// Property Ledger — supply strip (Pipeline Cockpit plan P1, read-only).
//
// One card per FEED (dataset): the full tranche anatomy of the live
// partner_clean_queue — total, actively-cleaning (pending_eo + eo_in_flight),
// ready (headline + per-ISP split), the held reservoir, and the
// suppressed_eo / dead_letter tail. These are LIVE queue facts (PG,
// point-in-time, labeled "live queue") — never Observatory fact-table
// aggregates.
//
// Honest framing (plan §2): supply is a DATASET fact, shared across the
// rotation's brands — pcq rows have no brand until mailed (mailed_brand is
// stamped at claim). The endpoint therefore resolves the selected domain's
// FEEDS exactly as HandleLaneContent does (partner_drip_vertical_roster
// brand↔vertical → partner_datasets per vertical) and returns the
// shared-brand list on every feed so the UI can render "supply shared across
// N rotation brands" — never dataset supply as domain-owned inventory.
//
// "ready = EO Verified (1) + Complainer (7)" is BY CONSTRUCTION: the
// validator's callEOValidation maps EO ResultID 1 and 7 → outcomeReady →
// markReady (internal/worker/partner_validator.go), which sets
// status='ready'. The ready_semantics field states this verbatim.
//
// Denver windows: UTC bounds computed in Go and param-injected ($2/$3) —
// NEVER tz-cast in the WHERE clause (drip_lanes.go's
// `(mailed_at AT TIME ZONE 'America/Denver')::date` is the documented
// seq-scan footgun and must not be copied; pinned by regex in
// property_lane_supply_test.go).

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneSupplyAnatomySQL — one row per dataset: full tranche anatomy (live
// queue, point-in-time). Plan §3.1 verbatim, plus the pending_eo /
// eo_in_flight split the response contract requires (summed in the headline
// `cleaning` number, split in the tooltip). $1 = dataset_id; $2/$3 =
// Go-computed Denver-day UTC bounds.
const laneSupplyAnatomySQL = `
	SELECT dataset_id,
	       COUNT(*)                                                        AS tranche_total,
	       COUNT(*) FILTER (WHERE status IN ('pending_eo','eo_in_flight')) AS cleaning,
	       COUNT(*) FILTER (WHERE status = 'pending_eo')                   AS pending_eo,
	       COUNT(*) FILTER (WHERE status = 'eo_in_flight')                 AS eo_in_flight,
	       COUNT(*) FILTER (WHERE status = 'ready')                        AS ready_total,
	       COUNT(*) FILTER (WHERE status = 'held')                         AS held,
	       COUNT(*) FILTER (WHERE status = 'suppressed_eo')                AS suppressed,
	       COUNT(*) FILTER (WHERE status = 'dead_letter')                  AS dead_letter,
	       COUNT(*) FILTER (WHERE status = 'mailed')                       AS mailed_lifetime,
	       COUNT(*) FILTER (WHERE mailed_at >= $2 AND mailed_at < $3)      AS mailed_today
	FROM partner_clean_queue
	WHERE dataset_id = $1
	GROUP BY dataset_id`

// laneSupplyReadyByISPSQL — the operator's "measure of quantity by ISP".
// Rides idx_pcq_isp_family (dataset_id, isp_family, status) — cmd/server/
// main.go dp_idx_pcq_isp_family.
const laneSupplyReadyByISPSQL = `
	SELECT isp_family, COUNT(*)
	FROM partner_clean_queue
	WHERE dataset_id = $1 AND status = 'ready'
	GROUP BY isp_family`

const laneSupplyReadySemantics = "ready = EO Verified (1) + Complainer (7) — the validator's markReady path"

type laneSupplyISP struct {
	ISP   string `json:"isp"`
	Ready int64  `json:"ready"`
}

type laneSupplyFeed struct {
	DatasetID string `json:"dataset_id"`
	Name      string `json:"name"`
	Vertical  string `json:"vertical"`
	Status    string `json:"status"`
	// DailyCap is the SUPPLY-RELEASE budget (partner_datasets.daily_cap,
	// enforced by agents/jobs/drip_lane_release.py: at most daily_cap rows
	// 'ready', the rest parked 'held'). It is NOT the claim-side per-ISP cap
	// (partner_isp_distribution_overrides) — two distinct systems.
	DailyCap int  `json:"daily_cap"`
	Paused   bool `json:"paused_emergency"`
	// SharedBrands: the rotation brands this dataset's supply is shared
	// across (the vertical's active roster). Supply is never domain-owned.
	SharedBrands   []string        `json:"shared_brands"`
	TrancheTotal   int64           `json:"tranche_total"`
	Cleaning       int64           `json:"cleaning"`
	PendingEO      int64           `json:"pending_eo"`
	EOInFlight     int64           `json:"eo_in_flight"`
	ReadyTotal     int64           `json:"ready_total"`
	ReadyByISP     []laneSupplyISP `json:"ready_by_isp"`
	Held           int64           `json:"held"`
	Suppressed     int64           `json:"suppressed"`
	DeadLetter     int64           `json:"dead_letter"`
	MailedLifetime int64           `json:"mailed_lifetime"`
	MailedToday    int64           `json:"mailed_today"`
}

// laneSupplyDenverDayBoundsUTC computes the current Denver day as UTC
// instants for param injection — the SQL never tz-casts (DST-correct: the
// spring-forward day is 23h, the fall-back day 25h).
func laneSupplyDenverDayBoundsUTC(now time.Time) (time.Time, time.Time) {
	local := now.In(propertyLedgerLoc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, propertyLedgerLoc)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

// laneSupplyStripPrefix strips the sending-domain prefix to the apex
// ("em.discountblog.com" → "discountblog.com", "m.wcl-heloc.com" →
// "wcl-heloc.com").
func laneSupplyStripPrefix(d string) string {
	if strings.HasPrefix(d, "em.") {
		return strings.TrimPrefix(d, "em.")
	}
	return strings.TrimPrefix(d, "m.")
}

// laneSupplyBrandForDomain resolves ?domain= (apex or full sending domain)
// to a lane brand code. Ledger brands resolve through the orchestrator's
// canonical roster (DripIntroBrands + BrandSendingDomain, DB overlay
// included). Non-ledger cockpit domains (plan §1: wcl-heloc.com first-class)
// resolve through the mailing_brand_metadata overlay (brand_code →
// sending_domain) — the same chain the orchestrator's dynamic brand map
// loads from — and are flagged non_ledger so the UI renders budgets as
// "Not in ledger — budgets N/A", never zeros.
func (s *PMTACampaignService) laneSupplyBrandForDomain(ctx context.Context, domain string) (brand, sendingDomain string, nonLedger, ok bool) {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return "", "", false, false
	}
	apex := laneSupplyStripPrefix(d)
	for _, code := range worker.DripIntroBrands() {
		sd, found := worker.BrandSendingDomain(code)
		if found && (d == sd || apex == laneSupplyStripPrefix(sd)) {
			return code, sd, false, true
		}
	}
	var code, sd string
	err := s.db.QueryRowContext(ctx, `
		SELECT brand_code, sending_domain FROM mailing_brand_metadata
		WHERE status = 'active'
		  AND (sending_domain = $1 OR sending_domain = 'em.' || $2 OR sending_domain = 'm.' || $2 OR brand_root = $2)
		LIMIT 1`, d, apex).Scan(&code, &sd)
	if err != nil {
		return "", "", false, false
	}
	return strings.ToLower(strings.TrimSpace(code)), sd, true, true
}

// laneSupplyOrderReadyByISP orders the per-ISP split by the canonical ledger
// vocabulary (isppkg.LedgerGroups: AllGroups ∪ {'other'}); any stranger
// isp_family value is appended afterwards rather than hidden.
func laneSupplyOrderReadyByISP(counts map[string]int64) []laneSupplyISP {
	out := []laneSupplyISP{}
	seen := map[string]bool{}
	for _, g := range isppkg.LedgerGroups() {
		seen[g] = true
		if n, ok := counts[g]; ok && n > 0 {
			out = append(out, laneSupplyISP{ISP: g, Ready: n})
		}
	}
	strangers := []string{}
	for k := range counts {
		if !seen[k] && counts[k] > 0 {
			strangers = append(strangers, k)
		}
	}
	// deterministic order for the (never-expected) stranger tail
	for i := 0; i < len(strangers); i++ {
		for j := i + 1; j < len(strangers); j++ {
			if strangers[j] < strangers[i] {
				strangers[i], strangers[j] = strangers[j], strangers[i]
			}
		}
	}
	for _, k := range strangers {
		out = append(out, laneSupplyISP{ISP: k, Ready: counts[k]})
	}
	return out
}

// HandleLaneSupply GET …/property-ledger/supply?domain=<apex-or-sending-domain>
func (s *PMTACampaignService) HandleLaneSupply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain := r.URL.Query().Get("domain")
	brand, sendingDomain, nonLedger, ok := s.laneSupplyBrandForDomain(ctx, domain)
	if !ok {
		respondError(w, http.StatusBadRequest, "unknown domain — must be a drip roster sending domain or a registered mailing_brand_metadata domain")
		return
	}
	dayStart, dayEnd := laneSupplyDenverDayBoundsUTC(time.Now())

	// Every query failure fails the response — a silent partial-200 misleads
	// the operator (same robustness rule as HandleLaneContent).

	// 1. The brand's feeds: verticals off the roster (as HandleLaneContent).
	verticals := []string{}
	err := func() error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT vertical
			FROM partner_drip_vertical_roster
			WHERE brand = $1 AND active = true
			ORDER BY sort_order, vertical`, brand)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			verticals = append(verticals, v)
		}
		return rows.Err()
	}()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "roster query failed")
		return
	}

	// 2. Shared-brand list per vertical (the rotation the supply is shared
	// across), cached — verticals repeat across feeds.
	sharedCache := map[string][]string{}
	sharedBrands := func(vertical string) ([]string, error) {
		if b, ok := sharedCache[vertical]; ok {
			return b, nil
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT brand FROM partner_drip_vertical_roster
			WHERE vertical = $1 AND active = true
			ORDER BY brand`, vertical)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sharedCache[vertical] = out
		return out, nil
	}

	// 3. Active datasets per vertical, then the tranche anatomy per dataset.
	feeds := []laneSupplyFeed{}
	for _, vertical := range verticals {
		type dsTmp struct{ feed laneSupplyFeed }
		tmp := []dsTmp{}
		err := func() error {
			rows, err := s.db.QueryContext(ctx, `
				SELECT id::text, name, COALESCE(status, ''), COALESCE(daily_cap, 0),
				       COALESCE(paused_emergency, false)
				FROM partner_datasets
				WHERE vertical = $1 AND status = 'active'
				ORDER BY name`, vertical)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				d := dsTmp{feed: laneSupplyFeed{Vertical: vertical, ReadyByISP: []laneSupplyISP{}, SharedBrands: []string{}}}
				if err := rows.Scan(&d.feed.DatasetID, &d.feed.Name, &d.feed.Status,
					&d.feed.DailyCap, &d.feed.Paused); err != nil {
					return err
				}
				tmp = append(tmp, d)
			}
			return rows.Err()
		}()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "dataset query failed")
			return
		}
		for _, d := range tmp {
			f := d.feed
			shared, err := sharedBrands(vertical)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "shared-brand query failed")
				return
			}
			f.SharedBrands = shared

			var dsID string
			err = s.db.QueryRowContext(ctx, laneSupplyAnatomySQL, f.DatasetID, dayStart, dayEnd).
				Scan(&dsID, &f.TrancheTotal, &f.Cleaning, &f.PendingEO, &f.EOInFlight,
					&f.ReadyTotal, &f.Held, &f.Suppressed, &f.DeadLetter,
					&f.MailedLifetime, &f.MailedToday)
			if err != nil && err != sql.ErrNoRows {
				respondError(w, http.StatusInternalServerError, "supply anatomy query failed")
				return
			}
			// sql.ErrNoRows = a dataset with zero pcq rows: all-zero counts
			// are the true state of that queue, not an error.

			counts := map[string]int64{}
			err = func() error {
				rows, err := s.db.QueryContext(ctx, laneSupplyReadyByISPSQL, f.DatasetID)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var isp string
					var n int64
					if err := rows.Scan(&isp, &n); err != nil {
						return err
					}
					counts[isp] = n
				}
				return rows.Err()
			}()
			if err != nil {
				respondError(w, http.StatusInternalServerError, "ready-by-isp query failed")
				return
			}
			f.ReadyByISP = laneSupplyOrderReadyByISP(counts)
			feeds = append(feeds, f)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domain":          domain,
		"brand":           brand,
		"sending_domain":  sendingDomain,
		"non_ledger":      nonLedger,
		"as_of":           time.Now().UTC(),
		"denver_day":      time.Now().In(propertyLedgerLoc).Format("2006-01-02"),
		"ready_semantics": laneSupplyReadySemantics,
		"supply_note":     "Live queue facts (point-in-time). Supply is a DATASET fact shared across the rotation's brands — never domain-owned inventory.",
		"feeds":           feeds,
	})
}

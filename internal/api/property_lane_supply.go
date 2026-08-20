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
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
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
	// ComputedAt: when this dataset's anatomy was actually scanned — counts
	// are served from a per-dataset TTL cache (laneSupplyCacheTTL), so the
	// UI shows staleness honestly ("as of HH:MM:SS").
	ComputedAt time.Time `json:"computed_at"`
}

// laneSupplyCacheTTL bounds anatomy staleness. The aggregate is ~1.8s /
// ~550k shared buffers on the largest live dataset (measured 2026-08-17);
// with 5-minute UI polling plus concurrent viewers, uncached calls stack.
// Queue-level supply is a slow-moving fact — 2 minutes is honest.
const laneSupplyCacheTTL = 120 * time.Second

// laneSupplyCacheSlot is one dataset's cached anatomy. Misses collapse on
// mu: concurrent callers for the same dataset serialize, the first runs the
// scan, the rest read the fresh cache.
type laneSupplyCacheSlot struct {
	mu         sync.Mutex
	computedAt time.Time
	counts     laneSupplyCounts
	ready      []laneSupplyISP
}

type laneSupplyCounts struct {
	TrancheTotal, Cleaning, PendingEO, EOInFlight int64
	ReadyTotal, Held, Suppressed, DeadLetter      int64
	MailedLifetime, MailedToday                   int64
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
	// INDEX GATE (same fail-closed posture as the intro-rollup worker): the
	// anatomy aggregate is a measured 16.7s seq scan on prod WITHOUT
	// idx_pcq_dataset_status_mailed (covering IOS: 27.7ms). Refuse rather
	// than tax the heap until the CONCURRENTLY build lands and is valid.
	// Once observed valid it stays valid in-process (indexes don't un-build;
	// a REINDEX bounce is a redeploy-scale event) — no probe per poll.
	if !s.laneSupplyIndexOK.Load() {
		var indexValid bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(i.indisvalid, false)
			FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
			WHERE c.relname = 'idx_pcq_dataset_status_mailed'
		`).Scan(&indexValid); err != nil && err != sql.ErrNoRows {
			respondError(w, http.StatusInternalServerError, "index check failed")
			return
		}
		if !indexValid {
			respondError(w, http.StatusServiceUnavailable, "supply view warming up — idx_pcq_dataset_status_mailed is still building (calm-IO CONCURRENTLY); retry shortly")
			return
		}
		s.laneSupplyIndexOK.Store(true)
	}
	dayStart, dayEnd := laneSupplyDenverDayBoundsUTC(time.Now())

	// Every query failure fails the response — a silent partial-200 misleads
	// the operator (same robustness rule as HandleLaneContent).

	// 1–2. Feed identities + shared rotation (the single resolution path,
	// shared with the throttle read).
	feeds, err := s.laneSupplyResolveFeeds(ctx, brand)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 3. The tranche anatomy per dataset — through the per-dataset TTL
	// cache. A miss runs the scan while HOLDING the slot mutex, so
	// concurrent pollers for the same dataset collapse to one scan per TTL.
	for i := range feeds {
		f := &feeds[i]
		if err := s.laneSupplyFillAnatomy(ctx, f, dayStart, dayEnd); err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
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

// laneSupplyFillAnatomy fills one feed's counts + ready-by-ISP from the
// per-dataset TTL cache, scanning only on a cold/expired slot. Errors are
// operator-facing strings (never a silent partial-200). The Denver-day
// bounds ride the cache too: up to laneSupplyCacheTTL of staleness across
// the midnight boundary, surfaced honestly via computed_at.
func (s *PMTACampaignService) laneSupplyFillAnatomy(ctx context.Context, f *laneSupplyFeed, dayStart, dayEnd time.Time) error {
	slotI, _ := s.laneSupplyCache.LoadOrStore(f.DatasetID, &laneSupplyCacheSlot{})
	slot := slotI.(*laneSupplyCacheSlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.computedAt.IsZero() || time.Since(slot.computedAt) >= laneSupplyCacheTTL {
		var c laneSupplyCounts
		var dsID string
		err := s.db.QueryRowContext(ctx, laneSupplyAnatomySQL, f.DatasetID, dayStart, dayEnd).
			Scan(&dsID, &c.TrancheTotal, &c.Cleaning, &c.PendingEO, &c.EOInFlight,
				&c.ReadyTotal, &c.Held, &c.Suppressed, &c.DeadLetter,
				&c.MailedLifetime, &c.MailedToday)
		if err != nil && err != sql.ErrNoRows {
			return errors.New("supply anatomy query failed")
		}
		// sql.ErrNoRows = a dataset with zero pcq rows: all-zero counts are
		// the true state of that queue, not an error (and cacheable).

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
			return errors.New("ready-by-isp query failed")
		}
		slot.counts = c
		slot.ready = laneSupplyOrderReadyByISP(counts)
		slot.computedAt = time.Now()
	}

	f.TrancheTotal = slot.counts.TrancheTotal
	f.Cleaning = slot.counts.Cleaning
	f.PendingEO = slot.counts.PendingEO
	f.EOInFlight = slot.counts.EOInFlight
	f.ReadyTotal = slot.counts.ReadyTotal
	f.Held = slot.counts.Held
	f.Suppressed = slot.counts.Suppressed
	f.DeadLetter = slot.counts.DeadLetter
	f.MailedLifetime = slot.counts.MailedLifetime
	f.MailedToday = slot.counts.MailedToday
	f.ReadyByISP = append([]laneSupplyISP{}, slot.ready...)
	f.ComputedAt = slot.computedAt
	return nil
}

// laneSupplyResolveFeeds resolves a lane brand's feeds — roster verticals →
// active datasets per vertical, with the vertical's shared-brand rotation
// attached — the SINGLE resolution path shared by the supply and throttle
// reads. Identity/budget fields are populated; anatomy counts are the
// caller's. Error messages are operator-facing (the callers respond them
// verbatim); every failure is an error — never a silent partial result.
func (s *PMTACampaignService) laneSupplyResolveFeeds(ctx context.Context, brand string) ([]laneSupplyFeed, error) {
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
		return nil, errors.New("roster query failed")
	}

	// Shared-brand list per vertical (the rotation the supply is shared
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

	feeds := []laneSupplyFeed{}
	for _, vertical := range verticals {
		tmp := []laneSupplyFeed{}
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
				f := laneSupplyFeed{Vertical: vertical, ReadyByISP: []laneSupplyISP{}, SharedBrands: []string{}}
				if err := rows.Scan(&f.DatasetID, &f.Name, &f.Status,
					&f.DailyCap, &f.Paused); err != nil {
					return err
				}
				tmp = append(tmp, f)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, errors.New("dataset query failed")
		}
		for _, f := range tmp {
			shared, err := sharedBrands(vertical)
			if err != nil {
				return nil, errors.New("shared-brand query failed")
			}
			f.SharedBrands = shared
			feeds = append(feeds, f)
		}
	}
	return feeds, nil
}

// ── Throttle configuration — READ side (Pipeline Cockpit plan P2) ───────────
//
// Current partner_isp_distribution_overrides per feed dataset + the effective
// posture (which ISPs carry an override vs ride the global defaults),
// rendered beside the supply strip's per-ISP ready counts. The lane's
// supply-release cap (partner_datasets.daily_cap) is returned beside it,
// labeled as the DISTINCT system it is — the two cap systems the plan names
// as the #1 predictable operator confusion:
//   (1) supply_release_daily_cap — SUPPLY release budget (ready-vs-held,
//       agents/jobs/drip_lane_release.py);
//   (2) overrides[] — CLAIM-side per-ISP enforcement, read fresh by the drip
//       orchestrator (max_per_wave replaces the global per-wave claim cap for
//       this dataset's waves; daily_cap is the lane-owned per-ISP daily
//       budget: NULL = global default, 0 = hard-suppressed).
//
// WRITE side: the cockpit UI REUSES the existing
// PUT /api/mailing/data-partners/datasets/{id}/isp-distribution
// (HandleUpdateISPDistribution, partner_admin_handlers.go — delete-and-
// replace semantics, updated_by stamped, writeAuditLog'd). No second writer.
// The edit surface renders ONLY when this response reports
// write_enabled=true, i.e. the server env
// PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED=1 (HOLD-CRITICAL posture: ships
// unset = read-only panel with an honest banner naming the env var; flipping
// the env is the one-move enable AND the one-move rollback).

// laneThrottleWriteFlagEnv gates the cockpit's throttle EDIT surface. This is
// a server-reported flag (surfaced in the throttle read), not a UI-local one.
const laneThrottleWriteFlagEnv = "PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED"

func laneThrottleWriteEnabled() bool {
	return os.Getenv(laneThrottleWriteFlagEnv) == "1"
}

// laneThrottleOverridesSQL mirrors the admin read
// (partner_admin_handlers.go HandleGetFlushStatus overrides block) plus the
// updated_at/updated_by audit columns the cockpit displays.
const laneThrottleOverridesSQL = `
	SELECT isp, pct_override, COALESCE(max_per_wave, 0), daily_cap,
	       updated_at, COALESCE(updated_by, '')
	FROM partner_isp_distribution_overrides
	WHERE dataset_id = $1
	ORDER BY isp`

type laneThrottleOverride struct {
	ISP         string  `json:"isp"`
	PctOverride float64 `json:"pct_override"`
	MaxPerWave  int     `json:"max_per_wave"`
	// DailyCap: NULL (omitted) = lane rides the global per-brand default;
	// 0 = hard-suppressed for this lane (core.md §14 2026-08-06).
	DailyCap  *int64    `json:"daily_cap,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

type laneThrottleFeed struct {
	DatasetID string `json:"dataset_id"`
	Name      string `json:"name"`
	Vertical  string `json:"vertical"`
	Status    string `json:"status"`
	Paused    bool   `json:"paused_emergency"`
	// SupplyReleaseDailyCap is cap system (1) — partner_datasets.daily_cap,
	// the SUPPLY-side release budget. NOT a claim-side ISP cap.
	SupplyReleaseDailyCap int      `json:"supply_release_daily_cap"`
	SharedBrands          []string `json:"shared_brands"`
	// Overrides is cap system (2) — the live claim-side enforcement rows.
	Overrides []laneThrottleOverride `json:"overrides"`
	// DefaultISPs: ledger ISPs with NO override row — they ride the global
	// defaults (the effective posture's other half).
	DefaultISPs []string `json:"default_isps"`
}

// HandleLaneThrottle GET …/property-ledger/throttle?domain=<apex-or-sending-domain>
func (s *PMTACampaignService) HandleLaneThrottle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain := r.URL.Query().Get("domain")
	brand, sendingDomain, nonLedger, ok := s.laneSupplyBrandForDomain(ctx, domain)
	if !ok {
		respondError(w, http.StatusBadRequest, "unknown domain — must be a drip roster sending domain or a registered mailing_brand_metadata domain")
		return
	}
	feeds, err := s.laneSupplyResolveFeeds(ctx, brand)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]laneThrottleFeed, 0, len(feeds))
	for _, f := range feeds {
		tf := laneThrottleFeed{
			DatasetID:             f.DatasetID,
			Name:                  f.Name,
			Vertical:              f.Vertical,
			Status:                f.Status,
			Paused:                f.Paused,
			SupplyReleaseDailyCap: f.DailyCap,
			SharedBrands:          f.SharedBrands,
			Overrides:             []laneThrottleOverride{},
			DefaultISPs:           []string{},
		}
		overridden := map[string]bool{}
		err := func() error {
			rows, err := s.db.QueryContext(ctx, laneThrottleOverridesSQL, f.DatasetID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var ov laneThrottleOverride
				var dailyCap sql.NullInt64
				if err := rows.Scan(&ov.ISP, &ov.PctOverride, &ov.MaxPerWave,
					&dailyCap, &ov.UpdatedAt, &ov.UpdatedBy); err != nil {
					return err
				}
				if dailyCap.Valid {
					v := dailyCap.Int64
					ov.DailyCap = &v
				}
				overridden[strings.ToLower(strings.TrimSpace(ov.ISP))] = true
				tf.Overrides = append(tf.Overrides, ov)
			}
			return rows.Err()
		}()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "override query failed")
			return
		}
		for _, g := range isppkg.LedgerGroups() {
			if !overridden[g] {
				tf.DefaultISPs = append(tf.DefaultISPs, g)
			}
		}
		out = append(out, tf)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domain":         domain,
		"brand":          brand,
		"sending_domain": sendingDomain,
		"non_ledger":     nonLedger,
		"as_of":          time.Now().UTC(),
		// The server-side gate for the cockpit's throttle EDIT surface
		// (HOLD-CRITICAL write path): the UI renders read-only unless this
		// server reports true. Flipping the env is the one-move rollback.
		"write_enabled":  laneThrottleWriteEnabled(),
		"write_flag_env": laneThrottleWriteFlagEnv,
		// The one writer (REUSED, never duplicated): delete-and-replace.
		"write_endpoint": "/api/mailing/data-partners/datasets/{id}/isp-distribution",
		"replacement_note": "Writes REPLACE the lane's override set (delete-and-replace): ISPs omitted from a write fall back to the global defaults, and their lane daily budgets are removed with the row. An ISP included without daily_cap keeps its prior lane budget.",
		"enforcement_note": "LIVE enforcement input — the drip orchestrator reads these rows fresh each wave: max_per_wave replaces the global per-wave claim cap for this dataset; daily_cap is the lane-owned per-ISP daily budget for NEW RECORDS ONLY (first touch \u2014 counted per (dataset, ISP) across all of the lane\u0027s brands; NULL = global default, 0 = hard-suppresses new intake for that ISP). It does NOT bound follow-up touches: mailed_at is stamped once at first touch, so a ladder already in motion keeps flowing at daily_cap=0 \u2014 use max_per_wave to pace follow-ups. The brand-allow gate still applies on top and a lane override cannot bypass it. Changes apply on the next wave, no deploy.",
		"cap_systems_note": "Two distinct cap systems: supply_release_daily_cap = supply release cap (lane, ready-vs-held); overrides[] = claim cap (per ISP, orchestrator).",
		"feeds":            out,
	})
}

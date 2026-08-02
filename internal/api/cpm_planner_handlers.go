package api

// CPM Planner — the operator's living pricing/pacing screen for email CPM
// deals. A deal is "budget × eCPM goal × eCPA goal × avg campaign size";
// the planner derives the planned volume, conversions needed, and days to
// finish (the operator's spreadsheet math), persists the deal, and then maps
// LIVE platform delivery back onto it:
//
//	planned_volume     = budget / ecpm_goal * 1000
//	conversions_needed = ceil(budget / ecpa_goal)
//	days_to_finish     = ceil(planned_volume / avg_campaign_size)
//
// Ground truth sources (campaign counter columns are STALE — never used):
//   - delivery: mailing_tracking_events for campaigns whose offer_id matches
//     the deal's offer (hard/soft bounce split via HardBounceSQL)
//   - conversions: mailing_offer_suppressions reason='converted'
//     (everflow postbacks write these rows)
//   - payout: mailing_offers.payout
//
// Capacity risk compares the sum of active deals' required daily volume
// against the platform's 14-day average daily 'sent' trend.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

type CpmPlannerHandlers struct {
	db *sql.DB

	// eventCountCache holds each deal's lifetime tracking-event aggregate —
	// the ONLY slow slice of deal progress (post-backfill it scans tens of
	// millions of rows; measured >3m for the org, 2026-07-07). A background
	// goroutine refreshes it every cpmEventCacheRefresh (matching the UI's
	// 5-min poll); everything mutation-sensitive (conversions, manual sums,
	// budgets, pacing math) stays live-computed per request so operator
	// edits are never served stale. In-memory only: rebuilt after restart
	// by the prewarm goroutine; a request arriving before the first warm
	// completes falls back to a synchronous compute (once per boot).
	evMu      sync.Mutex
	evByOrg   map[string]map[string]*cpmDealEventCounts
	evWarmed  map[string]time.Time
	evRunning map[string]bool
	// gapByOrg caches the default-window attribution-gap rollup (the 30d
	// org-wide delivered scan blew the request-path statement_timeout →
	// HTTP 500, 2026-07-08). Recomputed by the same refresher cycle.
	gapByOrg map[string]*cpmAttributionGap
	// dealIdentity caches the non-CPM surface's offer-identity → CPM deal map
	// (nonCpmDealForIdentity); a ~16.5s campaign-wide pass, TTL'd rather than
	// recomputed per page view. Guarded by evMu.
	dealIdentity   map[string][2]string
	dealIdentityAt time.Time
	// nonCpmCache holds built non-CPM responses per (org, month). A full build
	// is a multi-minute job on a busy DB; only COMPLETE builds are cached.
	nonCpmCache map[string]nonCpmCached
}

// nonCpmCached is one cached non-CPM payload.
type nonCpmCached struct {
	payload map[string]interface{}
	at      time.Time
}

const cpmEventCacheRefresh = 4 * time.Minute

func NewCpmPlannerHandlers(db *sql.DB) *CpmPlannerHandlers {
	h := &CpmPlannerHandlers{
		db:        db,
		evByOrg:   map[string]map[string]*cpmDealEventCounts{},
		evWarmed:  map[string]time.Time{},
		evRunning: map[string]bool{},
		gapByOrg:  map[string]*cpmAttributionGap{},
	}
	h.ensureTables()
	go h.eventCacheLoop()
	// Daily delivered rollup behind the non-CPM funnel. Process-lifetime, same
	// posture as eventCacheLoop; each day is an idempotent DELETE+INSERT so a
	// server bounce mid-pass just recomputes that day next cycle.
	go h.nonCpmRollupLoop()
	return h
}

// eventCacheLoop prewarms the event-count cache for every org with deals at
// boot, then keeps each warm on a fixed cadence. Process-lifetime goroutine
// (same posture as the other portal snapshot workers); double-fire safe —
// refreshOrgEventCounts no-ops when a refresh for the org is already running.
func (h *CpmPlannerHandlers) eventCacheLoop() {
	for {
		rows, err := h.db.Query(`SELECT DISTINCT organization_id::text FROM mailing_cpm_deals`)
		if err != nil {
			log.Printf("[CpmPlanner] event-cache org scan: %v", err)
		} else {
			orgs := []string{}
			for rows.Next() {
				var o string
				if err := rows.Scan(&o); err == nil {
					orgs = append(orgs, o)
				}
			}
			rows.Close()
			for _, o := range orgs {
				h.refreshOrgEventCounts(o)
			}
		}
		time.Sleep(cpmEventCacheRefresh)
	}
}

// refreshOrgEventCounts recomputes one org's per-deal event aggregates (one
// org-wide events pass) and swaps them into the cache. Serialized per org.
func (h *CpmPlannerHandlers) refreshOrgEventCounts(orgID string) {
	h.evMu.Lock()
	if h.evRunning[orgID] {
		h.evMu.Unlock()
		return
	}
	h.evRunning[orgID] = true
	h.evMu.Unlock()
	defer func() {
		h.evMu.Lock()
		h.evRunning[orgID] = false
		h.evMu.Unlock()
	}()

	deals, _, err := h.loadDealsLite(orgID)
	if err != nil {
		log.Printf("[CpmPlanner] event-cache deals for org %s: %v", orgID, err)
		return
	}
	if len(deals) == 0 {
		return
	}
	started := time.Now()
	counts := h.loadAllDealEventCounts(orgID, deals)
	if counts == nil {
		return // logged inside; keep the previous cache
	}
	h.evMu.Lock()
	h.evByOrg[orgID] = counts
	h.evWarmed[orgID] = time.Now()
	h.evMu.Unlock()
	log.Printf("[CpmPlanner] event cache refreshed for org %s: %d deals in %s", orgID, len(deals), time.Since(started).Round(time.Millisecond))

	// Attribution-gap rollup rides the same cycle (default window only).
	if gap, err := h.computeAttributionGap(orgID, cpmAttributionGapDefaultDays); err != nil {
		log.Printf("[CpmPlanner] attribution-gap refresh for org %s: %v", orgID, err)
	} else {
		h.evMu.Lock()
		h.gapByOrg[orgID] = gap
		h.evMu.Unlock()
	}
}

// cachedEventCounts returns the org's cached per-deal event aggregates, or
// nil when the cache has never been warmed (caller decides the fallback).
func (h *CpmPlannerHandlers) cachedEventCounts(orgID string) map[string]*cpmDealEventCounts {
	h.evMu.Lock()
	defer h.evMu.Unlock()
	if _, ok := h.evWarmed[orgID]; !ok {
		return nil
	}
	return h.evByOrg[orgID]
}

func (h *CpmPlannerHandlers) ensureTables() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS mailing_cpm_deals (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			organization_id UUID NOT NULL,
			name TEXT NOT NULL,
			offer_id UUID,
			everflow_offer_id VARCHAR(64) DEFAULT '',
			budget NUMERIC(12,2) NOT NULL,
			ecpm_goal NUMERIC(8,4) NOT NULL,
			ecpa_goal NUMERIC(10,2),
			avg_campaign_size INTEGER NOT NULL DEFAULT 160000,
			start_date DATE DEFAULT CURRENT_DATE,
			status TEXT NOT NULL DEFAULT 'active',
			notes TEXT DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cpm_deals_org ON mailing_cpm_deals(organization_id, status)`,
		// Explicit campaign→deal association (operator 2026-06-13): lets an
		// operator earmark specific campaigns to a deal so delivery/volume-to-
		// goal counts that exact set, on top of the offer_id auto-match.
		`CREATE TABLE IF NOT EXISTS mailing_cpm_deal_campaigns (
			deal_id UUID NOT NULL,
			campaign_id UUID NOT NULL,
			added_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (deal_id, campaign_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cpm_deal_campaigns_deal ON mailing_cpm_deal_campaigns(deal_id)`,
		// Per-deal name match (operator 2026-06-27): broadcasts + daily-board sends
		// carry offer_id=NULL, so the offer_id auto-match misses them and they only
		// count if hand-earmarked. A name pattern (ILIKE) attributes them to the deal
		// DYNAMICALLY — no manual earmarking, no generator changes. Empty = off.
		`ALTER TABLE mailing_cpm_deals ADD COLUMN IF NOT EXISTS campaign_name_pattern TEXT DEFAULT ''`,
		// Operator-set conversion count of record (2026-06-28). Postback capture
		// can lag/miss (Everflow not caught up, conversions under offer-ids the
		// deal doesn't match), so the operator can pin the true count here; when
		// set (>0) it overrides the tracked+uploaded total for eCPA / pacing.
		// NULL = unset → fall back to the computed count.
		`ALTER TABLE mailing_cpm_deals ADD COLUMN IF NOT EXISTS conversions_override INTEGER`,
		// Seed the Sam's Club deal so its board/broadcast volume (offer_id=NULL)
		// counts toward pacing. The pattern must match the OFFER slug the board
		// name carries ("… - sams-club"), NOT the bare token "sam".
		//
		// '%sam%' was the original seed and it conflated the Sam's Club OFFER with
		// the Sam's Club AUDIENCE: the partner-drip lanes are named for their DATA
		// SOURCE ("[partner-drip] samsclub_internal …", "[partner-drip]
		// clickers_samsclub …") but mail OTHER advertisers' offers to that list.
		// Measured on prod 2026-08-01: of 53,742 campaigns matching '%sam%' since
		// the deal's Jun 11 start, 20,423 carried a FOREIGN offer_id (Fidelity Life,
		// Metal Roofing, National Debt Relief, Liberty Mutual, Tahiti Village,
		// 3 Day Blinds) — 54,569 delivered in July alone billed to Sam's Club, and
		// the Liberty slice was double-counted into both deals. '%sams-club%' keeps
		// the 113 offer_id=NULL board sends that actually need the pattern; every
		// other genuine Sam's campaign carries the offer_id and is attributed by
		// that branch regardless of its name.
		`UPDATE mailing_cpm_deals SET campaign_name_pattern = '%sams-club%'
		   WHERE lower(name) ~ 'sam' AND COALESCE(campaign_name_pattern,'') = ''`,
		// One-time corrective for deals already carrying the over-broad seed.
		// Scoped to the EXACT superseded value so it is a no-op on every boot
		// after the first and can never stomp a later operator edit.
		`UPDATE mailing_cpm_deals SET campaign_name_pattern = '%sams-club%', updated_at = NOW()
		   WHERE campaign_name_pattern = '%sam%'`,
		// Same fix for Liberty Mutual and Metal Roofing (operator 2026-07-02): both
		// deals mail entirely as board/broadcast sends that carry offer_id=NULL and
		// are NOT hand-earmarked, so with no name pattern they attributed ZERO —
		// pacing and dynamic metrics rendered empty while Sam's (which has a pattern)
		// worked. Every send carries the offer slug verbatim in the campaign name
		// ("… - liberty-mutual", "… - metal-roofing[-ses]"); both patterns are
		// exact — they match only their own offer's campaigns (verified: 2,493
		// liberty rows all end "liberty-mutual", metal all "metal-roofing").
		`UPDATE mailing_cpm_deals SET campaign_name_pattern = '%liberty%'
		   WHERE lower(name) ~ 'liberty' AND COALESCE(campaign_name_pattern,'') = ''`,
		`UPDATE mailing_cpm_deals SET campaign_name_pattern = '%metal-roofing%'
		   WHERE lower(name) ~ 'metal' AND COALESCE(campaign_name_pattern,'') = ''`,
		// Per-deal MONTHLY TARGETS (operator 2026-06-30): the operator commits a
		// budget/volume/eCPM/eCPA target for a calendar month, and next month's
		// actuals are measured against it. Only the TARGET is stored here —
		// actuals (delivered/revenue/conversions) are computed LIVE from PG per
		// month (mailing_tracking_events + suppressions + manual conversions), so
		// they self-correct when a deal's attribution is later fixed. `month` is
		// the 1st of the calendar month.
		`CREATE TABLE IF NOT EXISTS mailing_cpm_deal_monthly_targets (
			organization_id UUID NOT NULL,
			deal_id         UUID NOT NULL,
			month           DATE NOT NULL CHECK (month = date_trunc('month', month)::date),
			target_budget   NUMERIC(12,2),
			target_volume   BIGINT,
			target_ecpm     NUMERIC(8,4),
			target_ecpa     NUMERIC(10,2),
			notes           TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (deal_id, month)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cpm_deal_monthly_targets_org_month
			ON mailing_cpm_deal_monthly_targets(organization_id, month)`,
		// Supports the stamped-offer_key attribution branch (dealCampaignSetSubquery
		// branch 4 / dealCampaignMapCTE) — 146k campaigns carry offer_key after the
		// 2026-07-07 backfill. Partial: NULL rows (no offer identity) excluded.
		`CREATE INDEX IF NOT EXISTS idx_campaigns_org_offer_key
			ON mailing_campaigns(organization_id, offer_key) WHERE offer_key IS NOT NULL`,
		// Lake click columns HandleNonCpmPerformance selects. They are also in
		// runStartupMigrations next to the table's CREATE, but that runs in a
		// BACKGROUND GOROUTINE (cmd/server/main.go — `go func() { …
		// runStartupMigrations(mailingDB) … }()`), so on a cold boot the route
		// can be live before the columns land and the handler would 500 on
		// "column does not exist". ensureTables runs synchronously inside
		// NewCpmPlannerHandlers, which is constructed immediately before the
		// /cpm-planner routes are registered — so the columns cannot be missing
		// by the time anything can call the handler. Idempotent, and cheap
		// (constant DEFAULT ⇒ metadata-only on PG 11+, no table rewrite).
		`ALTER TABLE mailing_offer_alignment_snapshot
			ADD COLUMN IF NOT EXISTS lake_clickers BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE mailing_offer_alignment_snapshot
			ADD COLUMN IF NOT EXISTS lake_clicks BIGINT NOT NULL DEFAULT 0`,
	}
	// Non-CPM performance surface (day rollup + operator offer groups). Same
	// synchronous slot, same reason: its routes are registered moments later.
	stmts = append(stmts, nonCpmTableDDL()...)
	for _, s := range stmts {
		if _, err := h.db.Exec(s); err != nil {
			log.Printf("[CpmPlanner] ensure tables: %v", err)
		}
	}
	h.seedNonCpmGroups()
}

// ─── Types ──────────────────────────────────────────────────────────────────

type cpmDealProgress struct {
	Sent        int64 `json:"sent"`
	Delivered   int64 `json:"delivered"`
	Opened      int64 `json:"opened"`  // ALWAYS 0 since 2026-07-07 (unrendered; dropped from the scan — see loadProgress)
	Clicked     int64 `json:"clicked"` // ALWAYS 0 since 2026-07-07 (unrendered; dropped from the scan — see loadProgress)
	HardBounces int64 `json:"hard_bounces"`
	SoftBounces int64 `json:"soft_bounces"`
	// Conversions is the TOTAL (tracked + manual) — the field name predates
	// manual uploads, kept as the total for backward compat. The split is
	// exposed alongside it.
	Conversions        int64   `json:"conversions"`
	ConversionsTracked int64   `json:"conversions_tracked"` // everflow postbacks (countOfferConversions)
	ConversionsManual  int64   `json:"conversions_manual"`  // operator CSV uploads / quick-adds
	ManualRevenue      float64 `json:"manual_revenue"`      // raw revenue reported on manual rows
	Payout             float64 `json:"payout"`
	PctDelivered       float64 `json:"pct_volume_delivered"` // delivered / planned_volume (0..1+)
	Revenue            float64 `json:"revenue_earned"`
	ActualEcpm         float64 `json:"actual_ecpm"`
	ActualEcpa         float64 `json:"actual_ecpa"` // full budget / conversions (matches the eCPA goal basis); 0 when no conversions
	DaysElapsed        int64   `json:"days_elapsed"`
	RequiredDaily      float64 `json:"required_daily"`
	ActualDaily        float64 `json:"actual_daily"`
	OnPace             bool    `json:"on_pace"`
	// Deadline pacing (operator 2026-07-02): populated only when the deal has an
	// end_date. DaysToDeadline = Denver calendar days remaining, INCLUSIVE of
	// today and the deadline day (>=1; a today/past deadline = 1);
	// RequiredDailyToDeadline = remaining planned volume ÷ those days — the
	// "finish sooner" pace. Both nil when no end_date is set (JSON null), so
	// the UI shows the deadline row only for deals that have one.
	DaysToDeadline          *int64   `json:"days_to_deadline"`
	RequiredDailyToDeadline *float64 `json:"required_daily_to_deadline"`
}

type cpmDeal struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	OfferID         string  `json:"offer_id"` // effective offer id ('' when unmapped)
	OfferName       string  `json:"offer_name"`
	EverflowOfferID string  `json:"everflow_offer_id"`
	Budget          float64 `json:"budget"`
	EcpmGoal        float64 `json:"ecpm_goal"`
	EcpaGoal        float64 `json:"ecpa_goal"`
	AvgCampaignSize int     `json:"avg_campaign_size"`
	StartDate       string  `json:"start_date"` // YYYY-MM-DD
	EndDate         string  `json:"end_date"`   // YYYY-MM-DD, '' when no deadline set
	Status          string  `json:"status"`
	Notes           string  `json:"notes"`
	// CampaignNamePattern: ILIKE pattern that attributes offer_id=NULL broadcasts/
	// board sends to this deal (operator 2026-06-27). '' = off.
	CampaignNamePattern string `json:"campaign_name_pattern"`
	// ConversionsOverride: operator-pinned conversion count (Everflow truth).
	// nil = unset; when set (>0) it overrides the tracked+uploaded total.
	ConversionsOverride *int64    `json:"conversions_override"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`

	PlannedVolume     int64 `json:"planned_volume"`
	ConversionsNeeded int64 `json:"conversions_needed"`
	DaysToFinish      int64 `json:"days_to_finish"`

	Progress cpmDealProgress `json:"progress"`

	startDate  time.Time `json:"-"`
	endDate    time.Time `json:"-"` // zero when no deadline (see hasEndDate)
	hasEndDate bool      `json:"-"`
}

type cpmCapacity struct {
	PlatformDaily      float64 `json:"platform_daily"`       // 14-day avg daily 'sent'
	TotalRequiredDaily float64 `json:"total_required_daily"` // sum over active deals
	UtilizationPct     float64 `json:"utilization_pct"`      // required / platform (0..1+)
	Headroom           float64 `json:"headroom"`             // platform - required
	Risk               string  `json:"risk"`                 // HIGH | MODERATE | LOW
	ActiveDeals        int     `json:"active_deals"`
}

type cpmDealInput struct {
	Name                *string  `json:"name"`
	OfferID             *string  `json:"offer_id"`
	EverflowOfferID     *string  `json:"everflow_offer_id"`
	Budget              *float64 `json:"budget"`
	EcpmGoal            *float64 `json:"ecpm_goal"`
	EcpaGoal            *float64 `json:"ecpa_goal"`
	AvgCampaignSize     *int     `json:"avg_campaign_size"`
	StartDate           *string  `json:"start_date"`
	EndDate             *string  `json:"end_date"` // '' clears the deadline; YYYY-MM-DD sets it
	Status              *string  `json:"status"`
	Notes               *string  `json:"notes"`
	CampaignNamePattern *string  `json:"campaign_name_pattern"`
	ConversionsOverride *int64   `json:"conversions_override"`
}

// ─── Core math ──────────────────────────────────────────────────────────────

// cpmPlanNumbers derives the deal's plan from its pricing terms.
// Verified against the operator's reference: budget $2,000, eCPM $0.70,
// eCPA $38, avg campaign 160,000 → 2,857,143 planned / 53 conversions / 18 days.
func cpmPlanNumbers(budget, ecpmGoal, ecpaGoal float64, avgCampaignSize int) (planned, convNeeded, days int64) {
	if budget > 0 && ecpmGoal > 0 {
		planned = int64(math.Ceil(budget / ecpmGoal * 1000))
	}
	if budget > 0 && ecpaGoal > 0 {
		convNeeded = int64(math.Ceil(budget / ecpaGoal))
	}
	if planned > 0 && avgCampaignSize > 0 {
		days = int64(math.Ceil(float64(planned) / float64(avgCampaignSize)))
	}
	return
}

// cpmEffectiveConversions is the conversion count of record for a deal. When the
// operator has pinned a manual override (Everflow truth — postback capture can
// lag or miss conversions landing under offer-ids the deal doesn't match), that
// wins; otherwise fall back to the tracked + uploaded total. (operator 2026-06-28)
func cpmEffectiveConversions(tracked, manual int64, override *int64) int64 {
	if override != nil && *override > 0 {
		return *override
	}
	return tracked + manual
}

// cpmActualEcpa is the realized cost per acquisition: full committed budget /
// conversions. Matches how the eCPA GOAL is defined (budget / conversions
// needed) and — unlike the prior (budget × pct_delivered) form, in which the
// budget cancelled against planned_volume — actually responds to budget edits.
// Returns 0 when there are no conversions. (operator 2026-06-28)
func cpmActualEcpa(budget float64, conversions int64) float64 {
	if conversions <= 0 {
		return 0
	}
	return budget / float64(conversions)
}

// cpmDeadlinePace is the "finish sooner" lever (operator 2026-07-02): given a
// deal's end_date, how many recipients per day must go out to deliver the
// remaining planned volume by the deadline. days is INCLUSIVE of today in the
// operator's America/Denver calendar (a deadline of "today" = 1 day left, not
// 0), matching the current-month pacing math. An earlier end_date → fewer days
// → higher required/day; a today/past deadline → 1 day (all remaining today).
func cpmDeadlinePace(planned, delivered int64, endDate time.Time) (days int64, requiredDaily float64) {
	// Denver-day granularity — the operator's send-day calendar (CLAUDE.md §6);
	// UTC would flip "days left" a day early every Denver evening.
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc)
	todayD := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	end := endDate.In(loc)
	endD := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, loc)
	// +1 makes it inclusive of both today and the deadline day.
	days = int64(endD.Sub(todayD).Hours()/24) + 1
	if days < 1 {
		days = 1 // today or past → everything remaining is due today
	}
	remaining := planned - delivered
	if remaining < 0 {
		remaining = 0
	}
	requiredDaily = math.Ceil(float64(remaining) / float64(days))
	return days, requiredDaily
}

// ─── Deal loading + live progress ───────────────────────────────────────────

const cpmDealSelect = `
	SELECT d.id, d.name,
	       COALESCE(d.offer_id::text, o2.id::text, '') AS effective_offer_id,
	       COALESCE(o.name, o2.name, '') AS offer_name,
	       COALESCE(d.everflow_offer_id, ''),
	       d.budget, d.ecpm_goal, COALESCE(d.ecpa_goal, 0),
	       d.avg_campaign_size, d.start_date, d.end_date, d.status, COALESCE(d.notes, ''),
	       COALESCE(d.campaign_name_pattern, ''),
	       d.conversions_override,
	       d.created_at, d.updated_at,
	       COALESCE(o.payout, o2.payout, 0) AS payout
	FROM mailing_cpm_deals d
	LEFT JOIN mailing_offers o  ON o.id = d.offer_id
	LEFT JOIN mailing_offers o2 ON d.offer_id IS NULL
	      AND COALESCE(d.everflow_offer_id,'') <> ''
	      AND o2.everflow_offer_id = d.everflow_offer_id
	      AND o2.organization_id = d.organization_id
	WHERE d.organization_id = $1`

func scanCpmDeal(rows interface{ Scan(...interface{}) error }) (cpmDeal, float64, error) {
	var d cpmDeal
	var payout float64
	var start time.Time
	var end sql.NullTime
	var convOverride sql.NullInt64
	err := rows.Scan(&d.ID, &d.Name, &d.OfferID, &d.OfferName, &d.EverflowOfferID,
		&d.Budget, &d.EcpmGoal, &d.EcpaGoal, &d.AvgCampaignSize, &start, &end,
		&d.Status, &d.Notes, &d.CampaignNamePattern, &convOverride, &d.CreatedAt, &d.UpdatedAt, &payout)
	if err != nil {
		return d, 0, err
	}
	if convOverride.Valid {
		v := convOverride.Int64
		d.ConversionsOverride = &v
	}
	d.startDate = start
	d.StartDate = start.Format("2006-01-02")
	if end.Valid {
		d.endDate = end.Time
		d.hasEndDate = true
		d.EndDate = end.Time.Format("2006-01-02")
	}
	d.PlannedVolume, d.ConversionsNeeded, d.DaysToFinish = cpmPlanNumbers(d.Budget, d.EcpmGoal, d.EcpaGoal, d.AvgCampaignSize)
	return d, payout, nil
}

func (h *CpmPlannerHandlers) loadDeals(orgID, dealID string) ([]cpmDeal, error) {
	q := cpmDealSelect
	args := []interface{}{orgID}
	if dealID != "" {
		q += " AND d.id = $2"
		args = append(args, dealID)
	}
	q += " ORDER BY d.created_at DESC"
	rows, err := h.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deals := []cpmDeal{}
	payouts := []float64{}
	for rows.Next() {
		d, payout, err := scanCpmDeal(rows)
		if err != nil {
			return nil, err
		}
		deals = append(deals, d)
		payouts = append(payouts, payout)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// LIST mode serves event aggregates from the background cache (the
	// per-deal events aggregate ran serially per deal and, once the
	// attribution backfill widened each deal's campaign set, the full list
	// took ~2m45s on prod — 2026-07-07). Conversions/manual/math below are
	// live-computed per request, so operator edits are never stale. Before
	// the first warm (fresh boot) the pass runs synchronously once.
	// Single-deal mode keeps the live per-deal query.
	// Single-deal mode (insights/detail) reads the same cache when warm —
	// its live per-deal query also dies on the prod statement_timeout
	// post-backfill (2026-07-08). Cold cache falls through to the live query.
	var bulk map[string]*cpmDealEventCounts
	if len(deals) > 0 {
		bulk = h.cachedEventCounts(orgID)
		if bulk == nil && dealID == "" {
			bulk = h.loadAllDealEventCounts(orgID, deals)
			if bulk != nil {
				h.evMu.Lock()
				h.evByOrg[orgID] = bulk
				h.evWarmed[orgID] = time.Now()
				h.evMu.Unlock()
			}
		}
	}
	for i := range deals {
		var pre *cpmDealEventCounts
		if bulk != nil {
			// A deal absent from the cache (zero events, or created after the
			// last refresh) renders zeros until the next cache cycle.
			pre = bulk[deals[i].ID]
			if pre == nil {
				pre = &cpmDealEventCounts{}
			}
		}
		deals[i].Progress = h.loadProgress(orgID, &deals[i], payouts[i], pre)
	}
	return deals, nil
}

// cpmDealEventCounts is one deal's tracking-event aggregate (the expensive
// slice of loadProgress, bulk-computable for the whole org in one pass).
type cpmDealEventCounts struct {
	sent, delivered, hard, soft int64
}

// loadAllDealEventCounts computes every deal's event aggregate in ONE
// events scan joined to the unified deal→campaign map (dealCampaignMapCTE),
// mirroring the loadAllDealMonthlyActuals shape. The global min start_date
// bounds partitions; each deal's own start_date floors its rows exactly like
// the per-deal query. Best-effort: on error returns nil and the caller falls
// back to per-deal queries.
func (h *CpmPlannerHandlers) loadAllDealEventCounts(orgID string, deals []cpmDeal) map[string]*cpmDealEventCounts {
	minStart := time.Time{}
	for i := range deals {
		if minStart.IsZero() || deals[i].startDate.Before(minStart) {
			minStart = deals[i].startDate
		}
	}
	if minStart.IsZero() {
		return nil
	}
	// This aggregate legitimately exceeds the server's statement_timeout
	// (post-backfill campaign sets; both bulk and per-deal shapes were
	// killed on prod, 2026-07-08) — it runs ONLY here: in the background
	// refresher and the once-per-boot synchronous fallback. SET LOCAL
	// scopes the override to this transaction alone.
	tx, err := h.db.Begin()
	if err != nil {
		log.Printf("[CpmPlanner] bulk event counts begin: %v", err)
		return nil
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(`SET LOCAL statement_timeout = '600s'`); err != nil {
		log.Printf("[CpmPlanner] bulk event counts timeout override: %v", err)
		return nil
	}
	// opened/clicked are deliberately NOT aggregated: they were never
	// rendered anywhere (dead payload, 2026-07-07 review) and opens are the
	// single largest event class (~90% machine) — excluding them cuts the
	// scan materially. The JSON fields remain, as zeros.
	rows, err := tx.Query(dealCampaignMapCTE+`
		SELECT dm.deal_id::text,
			COUNT(*) FILTER (WHERE te.event_type = 'sent'),
			COUNT(*) FILTER (WHERE te.event_type = 'delivered'),
			COUNT(*) FILTER (WHERE te.event_type = 'bounced' AND `+HardBounceSQL("te")+`),
			COUNT(*) FILTER (WHERE te.event_type = 'bounced' AND NOT (`+HardBounceSQL("te")+`))
		FROM mailing_tracking_events te
		JOIN dm ON dm.campaign_id = te.campaign_id
		JOIN mailing_cpm_deals d ON d.id = dm.deal_id
		WHERE te.organization_id = $1
		  AND te.event_type IN ('sent','delivered','bounced')
		  AND te.event_at >= $2
		  AND te.event_at >= d.start_date
		GROUP BY 1`, orgID, minStart)
	if err != nil {
		log.Printf("[CpmPlanner] bulk event counts: %v (falling back to per-deal)", err)
		return nil
	}
	defer rows.Close()
	out := map[string]*cpmDealEventCounts{}
	for rows.Next() {
		var id string
		c := &cpmDealEventCounts{}
		if err := rows.Scan(&id, &c.sent, &c.delivered, &c.hard, &c.soft); err == nil {
			out[id] = c
		}
	}
	return out
}

// dealCampaignSetSubquery returns the parenthesized "(SELECT … UNION … UNION …)"
// subquery that yields the campaign_id set attributed to a deal via the four
// attribution paths: offer_id auto-match, explicit mailing_cpm_deal_campaigns
// earmarks (operator 2026-06-13), campaign_name_pattern ILIKE for
// offer_id=NULL broadcasts/board sends (operator 2026-06-27), and the stamped
// offer_key matched through the deal's everflow id → slug-map slug/offer-name
// set (Offer Alignment, operator 2026-07-07 — catches name-inferred/
// html-inferred/click-inferred campaigns whose offer_id could not resolve).
// It is the single source of truth for deal attribution — loadProgress,
// loadDailySeries, and the monthly aggregate all use it so they can never
// diverge.
//
// Callers MUST bind placeholders as:
//
//	$1 = organization_id, $2 = membership floor (created_at >= $2),
//	$3 = effective offer_id (text; '' when unmapped),
//	$4 = deal id, $5 = campaign_name_pattern ('' = off),
//	$6 = deal everflow_offer_id ('' = off).
func dealCampaignSetSubquery() string {
	return `(
		SELECT id FROM mailing_campaigns
		WHERE organization_id = $1 AND offer_id = NULLIF($3,'')::uuid AND created_at >= $2
		UNION
		SELECT campaign_id FROM mailing_cpm_deal_campaigns WHERE deal_id = $4
		UNION
		-- Name-pattern branch: captures offer_id=NULL broadcasts/board sends.
		-- Inert when the deal has no pattern set.
		SELECT id FROM mailing_campaigns
		WHERE organization_id = $1 AND NULLIF($5,'') IS NOT NULL
		  AND name ILIKE $5 AND created_at >= $2
		UNION
		-- Stamped offer_key branch: campaign_attribution.go stamps lowercased
		-- keys (slug-map cratoolpro_slug, board-name token = slug-map
		-- offer_name, or landing_page_slug); the deal's everflow id resolves
		-- the same dictionary. Inert when the deal has no everflow id.
		SELECT id FROM mailing_campaigns
		WHERE organization_id = $1 AND NULLIF($6,'') IS NOT NULL
		  AND created_at >= $2
		  AND offer_key IN (
			SELECT lower(cratoolpro_slug) FROM mailing_offer_slug_map WHERE everflow_offer_id = $6
			UNION
			SELECT lower(offer_name) FROM mailing_offer_slug_map
			WHERE everflow_offer_id = $6 AND COALESCE(offer_name,'') <> ''
		  )
	)`
}

// loadProgress maps live platform delivery + conversion ground truth onto a
// deal. Best-effort: a failed sub-query logs and leaves zeros rather than
// failing the whole list. preEvents, when non-nil, carries the deal's event
// aggregate from the bulk org-wide pass (loadAllDealEventCounts) and the
// per-deal events query is skipped — list mode; nil = single-deal mode.
func (h *CpmPlannerHandlers) loadProgress(orgID string, d *cpmDeal, payout float64, preEvents *cpmDealEventCounts) cpmDealProgress {
	p := cpmDealProgress{Payout: payout}

	// Delivery campaign set = offer_id auto-match (when mapped) ∪ explicitly
	// associated campaigns (operator 2026-06-13). Associated campaigns count
	// regardless of offer_id, so an operator can attribute any send to the deal.
	if preEvents != nil {
		p.Sent, p.Delivered = preEvents.sent, preEvents.delivered
		p.HardBounces, p.SoftBounces = preEvents.hard, preEvents.soft
	} else {
		// opened/clicked are no longer aggregated (2026-07-07): they were
		// never rendered on any surface, and opens are the largest event
		// class (~90% machine) — dropping them cuts the scan materially.
		// The JSON fields remain as zeros; if opens/clicks are ever wanted
		// here, add them as verdict-labeled companions per METRIC_CONTRACT §6.
		// The raw event_at >= $2 bound below is the partition-pruning predicate.
		evQ := `
			SELECT
				COUNT(*) FILTER (WHERE event_type = 'sent'),
				COUNT(*) FILTER (WHERE event_type = 'delivered'),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND ` + HardBounceSQL("") + `),
				COUNT(*) FILTER (WHERE event_type = 'bounced' AND NOT (` + HardBounceSQL("") + `))
			FROM mailing_tracking_events
			WHERE organization_id = $1
			  AND event_type IN ('sent','delivered','bounced')
			  AND event_at >= $2
			  AND campaign_id IN ` + dealCampaignSetSubquery()
		if err := h.db.QueryRow(evQ, orgID, d.startDate, d.OfferID, d.ID, d.CampaignNamePattern, d.EverflowOfferID).Scan(
			&p.Sent, &p.Delivered, &p.HardBounces, &p.SoftBounces); err != nil {
			log.Printf("[CpmPlanner] progress events for deal %s: %v", d.ID, err)
		}
	}
	if d.OfferID != "" {

		// Conversion ground truth: everflow postbacks → offer suppressions.
		// countOfferConversions (offer_center_handlers.go) is THE shared
		// attribution query — same implementation the Offers tab uses, so
		// the two surfaces can never disagree.
		if n, err := countOfferConversions(context.Background(), h.db, orgID, d.OfferID, d.startDate); err != nil {
			log.Printf("[CpmPlanner] progress conversions for deal %s: %v", d.ID, err)
		} else {
			p.ConversionsTracked = n
		}
	}

	// Revenue basis per conversion: offer payout, else eCPA goal as estimate
	// (same precedence the pre-manual revenue logic used).
	basis := payout
	if basis == 0 && d.EcpaGoal > 0 {
		basis = d.EcpaGoal
	}

	// Manual conversions (operator CSV uploads / quick-adds) — counted for
	// EVERY deal, mapped or not: offers without postback wiring are exactly
	// the manual-upload case. NO date floor (operator 2026-06-13): manual
	// rows are explicit, deal-scoped operator ground truth, so they count
	// toward the deal regardless of converted_at vs start_date — an operator
	// backfilling a conversion dated before the deal's nominal start still
	// sees it land. manualRevEffective values each row at its reported revenue
	// when present, else at the basis estimate.
	var manualRevEffective float64
	if err := h.db.QueryRow(`
		SELECT COALESCE(SUM(count), 0),
		       COALESCE(SUM(revenue), 0),
		       -- $3::numeric: bare $3 gets inferred as INTEGER from count*$3,
		       -- which rejects decimal bases ("9.34" — Tahiti, 2026-07-08).
		       COALESCE(SUM(CASE WHEN revenue > 0 THEN revenue ELSE count * $3::numeric END), 0)
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1 AND deal_id = $2`,
		orgID, d.ID, basis).Scan(&p.ConversionsManual, &p.ManualRevenue, &manualRevEffective); err != nil {
		log.Printf("[CpmPlanner] progress manual conversions for deal %s: %v", d.ID, err)
	}
	p.Conversions = cpmEffectiveConversions(p.ConversionsTracked, p.ConversionsManual, d.ConversionsOverride)

	// CPM volume is billed on DELIVERED (operator 2026-06-18: "base only on delivered").
	// Every volume-basis metric — progress %, eCPM (revenue per 1000 delivered), eCPA,
	// daily pace — uses Delivered, NOT Sent. Sent stays a raw count for the tiles only.
	if d.PlannedVolume > 0 {
		p.PctDelivered = float64(p.Delivered) / float64(d.PlannedVolume)
	}
	p.Revenue = float64(p.ConversionsTracked)*basis + manualRevEffective
	if p.Delivered > 0 {
		p.ActualEcpm = p.Revenue / float64(p.Delivered) * 1000
	}
	// eCPA Actual = full committed budget / conversions (operator 2026-06-28).
	p.ActualEcpa = cpmActualEcpa(d.Budget, p.Conversions)

	p.DaysElapsed = int64(time.Since(d.startDate).Hours() / 24)
	if p.DaysElapsed < 0 {
		p.DaysElapsed = 0
	}
	if d.DaysToFinish > 0 {
		p.RequiredDaily = float64(d.PlannedVolume) / float64(d.DaysToFinish)
	}
	elapsed := p.DaysElapsed
	if elapsed < 1 {
		elapsed = 1
	}
	p.ActualDaily = float64(p.Delivered) / float64(elapsed)
	p.OnPace = p.ActualDaily >= p.RequiredDaily

	// Deadline pacing — only when the deal carries an end_date (NULL-tolerant:
	// both fields stay nil → JSON null → the UI hides the deadline row).
	if d.hasEndDate {
		days, reqDaily := cpmDeadlinePace(d.PlannedVolume, p.Delivered, d.endDate)
		p.DaysToDeadline = &days
		p.RequiredDailyToDeadline = &reqDaily
	}
	return p
}

// ─── Capacity ───────────────────────────────────────────────────────────────

func (h *CpmPlannerHandlers) loadCapacity(orgID string) cpmCapacity {
	c := cpmCapacity{Risk: "LOW"}

	// 3-day average daily sends — from the daily domain-agent scorecard
	// rollup, NOT raw tracking events (an event scan per poll was a prod
	// hazard — QA finding C4, 2026-06-12). Window was 14d; operator 2026-07-01:
	// capacity/sending is evaluated on the last 3 days, not two weeks.
	trendQ := `
		SELECT COALESCE(AVG(cnt), 0) FROM (
			SELECT day, SUM(sends) AS cnt
			FROM mailing_domain_agent_scorecard
			WHERE organization_id = $1 AND day >= CURRENT_DATE - 3 AND day < CURRENT_DATE
			GROUP BY 1
		) t`
	if err := h.db.QueryRow(trendQ, orgID).Scan(&c.PlatformDaily); err != nil {
		log.Printf("[CpmPlanner] capacity trend: %v", err)
	}

	// Required daily across active deals.
	rows, err := h.db.Query(`
		SELECT budget, ecpm_goal, COALESCE(ecpa_goal, 0), avg_campaign_size
		FROM mailing_cpm_deals
		WHERE organization_id = $1 AND status = 'active'`, orgID)
	if err != nil {
		log.Printf("[CpmPlanner] capacity deals: %v", err)
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var budget, ecpm, ecpa float64
		var avgSize int
		if err := rows.Scan(&budget, &ecpm, &ecpa, &avgSize); err != nil {
			continue
		}
		planned, _, days := cpmPlanNumbers(budget, ecpm, ecpa, avgSize)
		if days > 0 {
			c.TotalRequiredDaily += float64(planned) / float64(days)
		}
		c.ActiveDeals++
	}

	c.Headroom = c.PlatformDaily - c.TotalRequiredDaily
	if c.PlatformDaily > 0 {
		c.UtilizationPct = c.TotalRequiredDaily / c.PlatformDaily
	} else if c.TotalRequiredDaily > 0 {
		c.UtilizationPct = 1 // demand with zero observed trend = fully at risk
	}
	switch {
	case c.UtilizationPct > 0.60:
		c.Risk = "HIGH"
	case c.UtilizationPct > 0.35:
		c.Risk = "MODERATE"
	}
	return c
}

// ─── Handlers ───────────────────────────────────────────────────────────────

// HandleListDeals GET /cpm-planner/deals
func (h *CpmPlannerHandlers) HandleListDeals(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	deals, err := h.loadDeals(orgID, "")
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list deals: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"deals": deals, "total": len(deals)})
}

// HandleCreateDeal POST /cpm-planner/deals
func (h *CpmPlannerHandlers) HandleCreateDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var in cpmDealInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}
	if in.Budget == nil || *in.Budget <= 0 {
		respondError(w, http.StatusBadRequest, "budget must be > 0")
		return
	}
	if in.EcpmGoal == nil || *in.EcpmGoal <= 0 {
		respondError(w, http.StatusBadRequest, "ecpm_goal must be > 0")
		return
	}
	var offerID interface{}
	if in.OfferID != nil && strings.TrimSpace(*in.OfferID) != "" {
		offerID = strings.TrimSpace(*in.OfferID)
	}
	var ecpaGoal interface{}
	if in.EcpaGoal != nil && *in.EcpaGoal > 0 {
		ecpaGoal = *in.EcpaGoal
	}
	everflowID := ""
	if in.EverflowOfferID != nil {
		everflowID = strings.TrimSpace(*in.EverflowOfferID)
	}
	avgSize := 160000
	if in.AvgCampaignSize != nil && *in.AvgCampaignSize > 0 {
		avgSize = *in.AvgCampaignSize
	}
	startDate := time.Now().Format("2006-01-02")
	if in.StartDate != nil && *in.StartDate != "" {
		if _, err := time.Parse("2006-01-02", *in.StartDate); err != nil {
			respondError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
			return
		}
		startDate = *in.StartDate
	}
	var endDate interface{} // NULL unless a valid deadline is supplied
	if in.EndDate != nil && strings.TrimSpace(*in.EndDate) != "" {
		if _, err := time.Parse("2006-01-02", *in.EndDate); err != nil {
			respondError(w, http.StatusBadRequest, "end_date must be YYYY-MM-DD")
			return
		}
		endDate = strings.TrimSpace(*in.EndDate)
	}
	notes := ""
	if in.Notes != nil {
		notes = *in.Notes
	}
	namePattern := ""
	if in.CampaignNamePattern != nil {
		namePattern = strings.TrimSpace(*in.CampaignNamePattern)
	}

	var id string
	err := h.db.QueryRow(`
		INSERT INTO mailing_cpm_deals
			(organization_id, name, offer_id, everflow_offer_id, budget, ecpm_goal,
			 ecpa_goal, avg_campaign_size, start_date, end_date, notes, campaign_name_pattern)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id`,
		orgID, strings.TrimSpace(*in.Name), offerID, everflowID, *in.Budget, *in.EcpmGoal,
		ecpaGoal, avgSize, startDate, endDate, notes, namePattern).Scan(&id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("create deal: %v", err))
		return
	}
	deals, err := h.loadDeals(orgID, id)
	if err != nil || len(deals) == 0 {
		respondJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	respondJSON(w, http.StatusCreated, deals[0])
}

// HandleUpdateDeal PUT /cpm-planner/deals/{id}
func (h *CpmPlannerHandlers) HandleUpdateDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	var in cpmDealInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	sets := []string{}
	args := []interface{}{}
	add := func(col string, val interface{}) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			respondError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		add("name", strings.TrimSpace(*in.Name))
	}
	if in.OfferID != nil {
		if strings.TrimSpace(*in.OfferID) == "" {
			add("offer_id", nil)
		} else {
			add("offer_id", strings.TrimSpace(*in.OfferID))
		}
	}
	if in.EverflowOfferID != nil {
		add("everflow_offer_id", strings.TrimSpace(*in.EverflowOfferID))
	}
	if in.Budget != nil {
		if *in.Budget <= 0 {
			respondError(w, http.StatusBadRequest, "budget must be > 0")
			return
		}
		add("budget", *in.Budget)
	}
	if in.EcpmGoal != nil {
		if *in.EcpmGoal <= 0 {
			respondError(w, http.StatusBadRequest, "ecpm_goal must be > 0")
			return
		}
		add("ecpm_goal", *in.EcpmGoal)
	}
	if in.EcpaGoal != nil {
		if *in.EcpaGoal > 0 {
			add("ecpa_goal", *in.EcpaGoal)
		} else {
			add("ecpa_goal", nil)
		}
	}
	if in.AvgCampaignSize != nil {
		if *in.AvgCampaignSize <= 0 {
			respondError(w, http.StatusBadRequest, "avg_campaign_size must be > 0")
			return
		}
		add("avg_campaign_size", *in.AvgCampaignSize)
	}
	if in.StartDate != nil {
		if _, err := time.Parse("2006-01-02", *in.StartDate); err != nil {
			respondError(w, http.StatusBadRequest, "start_date must be YYYY-MM-DD")
			return
		}
		add("start_date", *in.StartDate)
	}
	if in.EndDate != nil {
		// '' clears the deadline (NULL); a valid date sets it.
		if strings.TrimSpace(*in.EndDate) == "" {
			add("end_date", nil)
		} else if _, err := time.Parse("2006-01-02", *in.EndDate); err != nil {
			respondError(w, http.StatusBadRequest, "end_date must be YYYY-MM-DD")
			return
		} else {
			add("end_date", strings.TrimSpace(*in.EndDate))
		}
	}
	if in.Status != nil {
		s := strings.ToLower(strings.TrimSpace(*in.Status))
		if s != "active" && s != "paused" && s != "completed" {
			respondError(w, http.StatusBadRequest, "status must be active|paused|completed")
			return
		}
		add("status", s)
	}
	if in.Notes != nil {
		add("notes", *in.Notes)
	}
	if in.CampaignNamePattern != nil {
		add("campaign_name_pattern", strings.TrimSpace(*in.CampaignNamePattern))
	}
	if in.ConversionsOverride != nil {
		// >0 pins the count; <=0 clears the override (back to computed count).
		if *in.ConversionsOverride > 0 {
			add("conversions_override", *in.ConversionsOverride)
		} else {
			add("conversions_override", nil)
		}
	}
	if len(sets) == 0 {
		respondError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	args = append(args, id, orgID)
	q := fmt.Sprintf(`UPDATE mailing_cpm_deals SET %s, updated_at = NOW() WHERE id = $%d AND organization_id = $%d`,
		strings.Join(sets, ", "), len(args)-1, len(args))
	res, err := h.db.Exec(q, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("update deal: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	deals, err := h.loadDeals(orgID, id)
	if err != nil || len(deals) == 0 {
		respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
		return
	}
	respondJSON(w, http.StatusOK, deals[0])
}

// HandleDeleteDeal DELETE /cpm-planner/deals/{id}
func (h *CpmPlannerHandlers) HandleDeleteDeal(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	res, err := h.db.Exec(`DELETE FROM mailing_cpm_deals WHERE id = $1 AND organization_id = $2`, id, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("delete deal: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}

// HandleDealInsights GET /cpm-planner/deals/{id}/insights
func (h *CpmPlannerHandlers) HandleDealInsights(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	deals, err := h.loadDeals(orgID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if len(deals) == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	deal := deals[0]
	capacity := h.loadCapacity(orgID)
	daily := h.loadDailySeries(orgID, &deal)
	topDomains := h.loadTopConvertingDomains(orgID, &deal)
	recs := buildCpmRecommendations(&deal, capacity, topDomains)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal":            deal,
		"capacity":        capacity,
		"daily_series":    daily,
		"top_domains":     topDomains,
		"recommendations": recs,
	})
}

// HandleDealOfferPerformance GET /cpm-planner/deals/{id}/offer-performance?days=30
//
// Embeds the offer performance panel in the deal detail (doc §6.1/§6.3):
// resolves the deal's effective offer (offer_id, or everflow_offer_id
// mapping) and returns the SAME aggregation the Offers tab Performance view
// uses — loadOfferStats in offer_center_handlers.go (single implementation,
// two endpoints) — including human opens/clicks, hard/soft bounce split,
// conversions (shared attribution query), suppression total + DNM list size,
// daily sent/conversion series, recent campaigns, and the 8-week
// suppressed-count trend with audience size for ceiling awareness.
func (h *CpmPlannerHandlers) HandleDealOfferPerformance(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	id := chi.URLParam(r, "id")
	deals, err := h.loadDeals(orgID, id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if len(deals) == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	deal := deals[0]

	if deal.OfferID == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"deal_id":     deal.ID,
			"offer_id":    "",
			"offer_name":  "",
			"performance": nil,
			"note":        "deal is not mapped to a platform offer — set offer_id or everflow_offer_id to see live offer performance",
		})
		return
	}

	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 365 {
		days = d
	}

	perf, err := loadOfferStats(r.Context(), h.db, orgID, deal.OfferID, days)
	if err != nil {
		log.Printf("[CpmPlanner] offer performance for deal %s (offer %s): %v", deal.ID, deal.OfferID, err)
		respondError(w, http.StatusInternalServerError, "Failed to load offer performance")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal_id":     deal.ID,
		"offer_id":    deal.OfferID,
		"offer_name":  deal.OfferName,
		"performance": perf,
	})
}

// HandleCapacity GET /cpm-planner/capacity
func (h *CpmPlannerHandlers) HandleCapacity(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.loadCapacity(getOrgID(r)))
}

// HandleOffersLite GET /cpm-planner/offers-lite — minimal offers list for the deal form.
func (h *CpmPlannerHandlers) HandleOffersLite(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	rows, err := h.db.Query(`
		SELECT id, name, COALESCE(everflow_offer_id, ''), COALESCE(payout, 0)
		FROM mailing_offers
		WHERE organization_id = $1
		ORDER BY name`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list offers: %v", err))
		return
	}
	defer rows.Close()
	type offerLite struct {
		ID              string  `json:"id"`
		Name            string  `json:"name"`
		EverflowOfferID string  `json:"everflow_offer_id"`
		Payout          float64 `json:"payout"`
	}
	offers := []offerLite{}
	for rows.Next() {
		var o offerLite
		if err := rows.Scan(&o.ID, &o.Name, &o.EverflowOfferID, &o.Payout); err != nil {
			continue
		}
		offers = append(offers, o)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"offers": offers})
}

// ─── Manual conversions ─────────────────────────────────────────────────────
//
// Most CPM offers have NO everflow postback wiring, so tracked conversions
// (mailing_offer_suppressions reason='converted') under-count reality. The
// operator gets conversion truth two ways: an Everflow conversion-report CSV
// export, or just "N conversions happened on date D". Both land in
// mailing_cpm_manual_conversions and blend into deal pacing as
// conversions_manual (see loadProgress).

type cpmManualConvEntry struct {
	ID           string    `json:"id"`
	ConvertedAt  time.Time `json:"converted_at"`
	Count        int       `json:"count"`
	Revenue      float64   `json:"revenue"`
	Sub1         string    `json:"sub1"`
	Sub2         string    `json:"sub2"`
	ConversionID string    `json:"conversion_id"`
	Source       string    `json:"source"` // 'csv' | 'manual'
	Note         string    `json:"note"`
	CreatedAt    time.Time `json:"created_at"`
}

type cpmManualConvInput struct {
	Entries []struct {
		ConvertedAt string  `json:"converted_at"` // YYYY-MM-DD or RFC3339
		Count       int     `json:"count"`        // default 1
		Revenue     float64 `json:"revenue"`
		Note        string  `json:"note"`
	} `json:"entries"`
	CSV string `json:"csv"` // raw Everflow conversion-export text
}

// cpmManualConvRow is one row ready for insert (from either input shape).
type cpmManualConvRow struct {
	convertedAt  time.Time
	count        int
	revenue      float64
	sub1         string
	sub2         string
	conversionID string
	source       string
	note         string
}

// cpmConvDateLayouts: Everflow exports vary by report/timezone setting;
// tolerate the common shapes plus plain dates for quick-adds.
var cpmConvDateLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"01/02/2006 15:04:05",
	"01/02/2006 15:04",
	"01/02/2006",
	"1/2/2006 15:04",
	"1/2/2006",
}

func parseCpmConvDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range cpmConvDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	// Raw unix-seconds timestamps (some Everflow API-driven exports).
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 1_000_000_000 && n < 10_000_000_000 {
		return time.Unix(n, 0).UTC(), true
	}
	return time.Time{}, false
}

func parseCpmRevenue(s string) float64 {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "$", ""), ",", ""))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parseEverflowConversionsCSV parses a raw Everflow conversion-report export.
// Header-driven column lookup (extra/reordered columns tolerated): only
// 'date' is required; 'revenue', 'sub1', 'sub2', 'conversion_id' are used
// when present. One conversion per row (count=1). Ragged rows, lazy quotes
// and a UTF-8 BOM are tolerated; unparseable rows are counted as parse
// errors (first few reported back as samples) rather than failing the batch.
func parseEverflowConversionsCSV(raw string) (rows []cpmManualConvRow, parseErrors int, samples []string) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	r := csv.NewReader(strings.NewReader(raw))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err != nil {
		return nil, 1, []string{"unreadable CSV: " + err.Error()}
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	if _, ok := col["date"]; !ok {
		return nil, 1, []string{"CSV has no 'date' column — expected an Everflow conversions export"}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	line := 1
	for {
		rec, err := r.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			parseErrors++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("line %d: %v", line, err))
			}
			continue
		}
		blank := true
		for _, f := range rec {
			if strings.TrimSpace(f) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		when, ok := parseCpmConvDate(get(rec, "date"))
		if !ok {
			parseErrors++
			if len(samples) < 5 {
				samples = append(samples, fmt.Sprintf("line %d: unparseable date %q", line, get(rec, "date")))
			}
			continue
		}
		rows = append(rows, cpmManualConvRow{
			convertedAt:  when,
			count:        1,
			revenue:      parseCpmRevenue(get(rec, "revenue")),
			sub1:         get(rec, "sub1"),
			sub2:         get(rec, "sub2"),
			conversionID: get(rec, "conversion_id"),
			source:       "csv",
		})
	}
	return rows, parseErrors, samples
}

// dealExists scopes the conversions sub-resource to an org-owned deal.
func (h *CpmPlannerHandlers) dealExists(orgID, dealID string) (bool, error) {
	var one int
	err := h.db.QueryRow(
		`SELECT 1 FROM mailing_cpm_deals WHERE id = $1 AND organization_id = $2`,
		dealID, orgID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// HandleAddDealConversions POST /cpm-planner/deals/{id}/conversions
//
// Body is EITHER {entries:[{converted_at,count,revenue?,note?}]} (quick-add)
// OR {csv:"<raw everflow export>"}. CSV rows dedupe on (deal_id,
// conversion_id) via the partial unique index — re-uploading the same export
// is safe and reports duplicates instead of double-counting.
func (h *CpmPlannerHandlers) HandleAddDealConversions(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	ok, err := h.dealExists(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}

	var in cpmManualConvInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	var rows []cpmManualConvRow
	parseErrors := 0
	var samples []string
	switch {
	case strings.TrimSpace(in.CSV) != "":
		rows, parseErrors, samples = parseEverflowConversionsCSV(in.CSV)
	case len(in.Entries) > 0:
		for i, e := range in.Entries {
			when, ok := parseCpmConvDate(e.ConvertedAt)
			if !ok {
				respondError(w, http.StatusBadRequest,
					fmt.Sprintf("entries[%d].converted_at: use YYYY-MM-DD or RFC3339", i))
				return
			}
			if e.Revenue < 0 {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("entries[%d].revenue must be >= 0", i))
				return
			}
			count := e.Count
			if count <= 0 {
				count = 1
			}
			rows = append(rows, cpmManualConvRow{
				convertedAt: when,
				count:       count,
				revenue:     e.Revenue,
				source:      "manual",
				note:        strings.TrimSpace(e.Note),
			})
		}
	default:
		respondError(w, http.StatusBadRequest, "body must include entries[] or csv")
		return
	}
	if len(rows) > 50000 {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("too many rows (%d) — cap is 50,000 per upload", len(rows)))
		return
	}

	inserted, duplicates := 0, 0
	if len(rows) > 0 {
		tx, err := h.db.Begin()
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("begin: %v", err))
			return
		}
		defer tx.Rollback() //nolint:errcheck // no-op after commit
		stmt, err := tx.Prepare(`
			INSERT INTO mailing_cpm_manual_conversions
				(organization_id, deal_id, converted_at, count, revenue,
				 sub1, sub2, conversion_id, source, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (deal_id, conversion_id) WHERE conversion_id <> '' DO NOTHING`)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("prepare: %v", err))
			return
		}
		defer stmt.Close()
		for _, row := range rows {
			res, err := stmt.Exec(orgID, dealID, row.convertedAt, row.count, row.revenue,
				row.sub1, row.sub2, row.conversionID, row.source, row.note)
			if err != nil {
				respondError(w, http.StatusInternalServerError, fmt.Sprintf("insert conversion: %v", err))
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				duplicates++
			} else {
				inserted++
			}
		}
		if err := tx.Commit(); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("commit: %v", err))
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"inserted":     inserted,
		"duplicates":   duplicates,
		"parse_errors": parseErrors,
		"errors":       samples,
	})
}

// HandleListDealConversions GET /cpm-planner/deals/{id}/conversions?days=N
// Newest first, capped at 500 rows; totals are computed over the full
// filtered set (not the capped page).
func (h *CpmPlannerHandlers) HandleListDealConversions(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	ok, err := h.dealExists(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}

	where := `organization_id = $1 AND deal_id = $2`
	args := []interface{}{orgID, dealID}
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 3650 {
		args = append(args, time.Now().AddDate(0, 0, -d))
		where += fmt.Sprintf(" AND converted_at >= $%d", len(args))
	}

	var manualTotal int64
	var manualRevenue float64
	if err := h.db.QueryRow(
		`SELECT COALESCE(SUM(count), 0), COALESCE(SUM(revenue), 0)
		 FROM mailing_cpm_manual_conversions WHERE `+where, args...).Scan(&manualTotal, &manualRevenue); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("conversion totals: %v", err))
		return
	}

	rows, err := h.db.Query(
		`SELECT id, converted_at, count, revenue, sub1, sub2, conversion_id, source, note, created_at
		 FROM mailing_cpm_manual_conversions WHERE `+where+`
		 ORDER BY converted_at DESC, created_at DESC LIMIT 500`, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list conversions: %v", err))
		return
	}
	defer rows.Close()
	entries := []cpmManualConvEntry{}
	for rows.Next() {
		var e cpmManualConvEntry
		if err := rows.Scan(&e.ID, &e.ConvertedAt, &e.Count, &e.Revenue,
			&e.Sub1, &e.Sub2, &e.ConversionID, &e.Source, &e.Note, &e.CreatedAt); err != nil {
			continue
		}
		entries = append(entries, e)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"totals": map[string]interface{}{
			"manual_total":   manualTotal,
			"manual_revenue": manualRevenue,
		},
	})
}

// HandleDeleteDealConversion DELETE /cpm-planner/deals/{id}/conversions/{convID}
func (h *CpmPlannerHandlers) HandleDeleteDealConversion(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	convID := chi.URLParam(r, "convID")
	res, err := h.db.Exec(
		`DELETE FROM mailing_cpm_manual_conversions
		 WHERE id = $1 AND deal_id = $2 AND organization_id = $3`,
		convID, dealID, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("delete conversion: %v", err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		respondError(w, http.StatusNotFound, "conversion entry not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"id": convID, "status": "deleted"})
}

// ─── Insights helpers ───────────────────────────────────────────────────────

type cpmDailyPoint struct {
	Date        string `json:"date"`
	Sent        int64  `json:"sent"`
	Conversions int64  `json:"conversions"` // tracked (postback) + manual, by day
}

func (h *CpmPlannerHandlers) loadDailySeries(orgID string, d *cpmDeal) []cpmDailyPoint {
	byDay := map[string]*cpmDailyPoint{}
	point := func(day string) *cpmDailyPoint {
		if p, ok := byDay[day]; ok {
			return p
		}
		p := &cpmDailyPoint{Date: day}
		byDay[day] = p
		return p
	}

	// Sent by day across the FULL attributed campaign set (offer_id ∪ earmarks ∪
	// name-pattern) — identical set to loadProgress, so the pace chart agrees with
	// the headline Delivered. Runs for every deal, mapped or not. (Was offer_id-
	// only before 2026-06-30, which under-counted earmark/pattern deals like Sam's.)
	{
		rows, err := h.db.Query(`
			SELECT to_char(date_trunc('day', event_at), 'YYYY-MM-DD') AS day, COUNT(*)
			FROM mailing_tracking_events
			WHERE organization_id = $1 AND event_type = 'sent' AND event_at >= $2
			  AND campaign_id IN `+dealCampaignSetSubquery()+`
			GROUP BY 1`, orgID, d.startDate, d.OfferID, d.ID, d.CampaignNamePattern, d.EverflowOfferID)
		if err != nil {
			log.Printf("[CpmPlanner] daily series for deal %s: %v", d.ID, err)
		} else {
			func() {
				defer rows.Close()
				for rows.Next() {
					var day string
					var sent int64
					if err := rows.Scan(&day, &sent); err != nil {
						continue
					}
					point(day).Sent = sent
				}
			}()
		}
	}

	if d.OfferID != "" {
		// Tracked conversions by day (same source as countOfferConversions).
		convRows, err := h.db.Query(`
			SELECT to_char(date_trunc('day', suppressed_at), 'YYYY-MM-DD') AS day, COUNT(*)
			FROM mailing_offer_suppressions
			WHERE organization_id = $1 AND offer_id = $2
			  AND reason = 'converted' AND suppressed_at >= $3
			GROUP BY 1`, orgID, d.OfferID, d.startDate)
		if err != nil {
			log.Printf("[CpmPlanner] daily conversions for deal %s: %v", d.ID, err)
		} else {
			func() {
				defer convRows.Close()
				for convRows.Next() {
					var day string
					var n int64
					if err := convRows.Scan(&day, &n); err != nil {
						continue
					}
					point(day).Conversions += n
				}
			}()
		}
	}

	// Manual conversions by day — union'd into the same series (charts for
	// unmapped deals show manual conversions alone).
	manRows, err := h.db.Query(`
		SELECT to_char(date_trunc('day', converted_at), 'YYYY-MM-DD') AS day, SUM(count)
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1 AND deal_id = $2 AND converted_at >= $3
		GROUP BY 1`, orgID, d.ID, d.startDate)
	if err != nil {
		log.Printf("[CpmPlanner] daily manual conversions for deal %s: %v", d.ID, err)
	} else {
		func() {
			defer manRows.Close()
			for manRows.Next() {
				var day string
				var n int64
				if err := manRows.Scan(&day, &n); err != nil {
					continue
				}
				point(day).Conversions += n
			}
		}()
	}

	series := make([]cpmDailyPoint, 0, len(byDay))
	for _, p := range byDay {
		series = append(series, *p)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].Date < series[j].Date })
	return series
}

type cpmDomainConversions struct {
	Domain      string `json:"domain"`
	Conversions int64  `json:"conversions"`
}

func (h *CpmPlannerHandlers) loadTopConvertingDomains(orgID string, d *cpmDeal) []cpmDomainConversions {
	out := []cpmDomainConversions{}
	if d.OfferID == "" {
		return out
	}
	rows, err := h.db.Query(`
		SELECT LOWER(SPLIT_PART(sub.email, '@', 2)) AS dom, COUNT(*)
		FROM mailing_offer_suppressions s
		JOIN mailing_subscribers sub ON sub.id = s.subscriber_id
		WHERE s.organization_id = $1 AND s.offer_id = $2
		  AND s.reason = 'converted' AND s.suppressed_at >= $3
		GROUP BY 1 ORDER BY 2 DESC LIMIT 5`, orgID, d.OfferID, d.startDate)
	if err != nil {
		log.Printf("[CpmPlanner] top domains for deal %s: %v", d.ID, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var dc cpmDomainConversions
		if err := rows.Scan(&dc.Domain, &dc.Conversions); err != nil {
			continue
		}
		out = append(out, dc)
	}
	return out
}

func buildCpmRecommendations(d *cpmDeal, cap cpmCapacity, topDomains []cpmDomainConversions) []string {
	recs := []string{}
	p := d.Progress

	if d.OfferID == "" {
		recs = append(recs, "Deal is not mapped to a platform offer — set offer_id or everflow_offer_id so live delivery and conversions can be tracked against the plan.")
	}

	// 1. Pace.
	if d.Status == "active" && p.RequiredDaily > 0 {
		if !p.OnPace {
			gap := p.RequiredDaily - p.ActualDaily
			extraSlots := int64(math.Ceil(gap / float64(d.AvgCampaignSize)))
			if extraSlots < 1 {
				extraSlots = 1
			}
			remaining := float64(d.PlannedVolume) - float64(p.Delivered)
			if remaining < 0 {
				remaining = 0
			}
			extendedDays := d.DaysToFinish
			if p.ActualDaily > 0 {
				extendedDays = p.DaysElapsed + int64(math.Ceil(remaining/p.ActualDaily))
			}
			recs = append(recs, fmt.Sprintf(
				"Behind pace: delivering %s/day vs %s/day needed — add %d more campaign slot(s) of %s per day, or extend the window to %d days at the current rate.",
				fmtCpmInt(int64(p.ActualDaily)), fmtCpmInt(int64(p.RequiredDaily)),
				extraSlots, fmtCpmInt(int64(d.AvgCampaignSize)), extendedDays))
		} else if p.Delivered > 0 {
			recs = append(recs, fmt.Sprintf(
				"On pace: %s/day actual vs %s/day required (%.1f%% of planned volume delivered).",
				fmtCpmInt(int64(p.ActualDaily)), fmtCpmInt(int64(p.RequiredDaily)), p.PctDelivered*100))
		}
	}

	// 2. eCPM vs goal.
	if p.Delivered > 0 && p.ActualEcpm < d.EcpmGoal {
		crPerMillion := float64(p.Conversions) / float64(p.Delivered) * 1_000_000
		remainingVolume := d.PlannedVolume - p.Delivered
		if remainingVolume < 0 {
			remainingVolume = 0
		}
		neededConv := d.ConversionsNeeded - p.Conversions
		if neededConv < 0 {
			neededConv = 0
		}
		requiredCr := 0.0
		if remainingVolume > 0 {
			requiredCr = float64(neededConv) / float64(remainingVolume) * 1_000_000
		}
		msg := fmt.Sprintf(
			"Actual eCPM $%.2f vs $%.2f goal — converting %.1f per million delivered; you need %d more conversions over the remaining %s delivered (%.1f per million required). Consider concentrating volume on the ISPs/brands where this offer converts.",
			p.ActualEcpm, d.EcpmGoal, crPerMillion, neededConv, fmtCpmInt(remainingVolume), requiredCr)
		if len(topDomains) > 0 {
			parts := make([]string, 0, len(topDomains))
			for _, td := range topDomains {
				parts = append(parts, fmt.Sprintf("%s (%d)", td.Domain, td.Conversions))
			}
			msg += " Top converting domains: " + strings.Join(parts, ", ") + "."
		}
		recs = append(recs, msg)
	} else if p.Delivered > 0 && p.ActualEcpm >= d.EcpmGoal {
		recs = append(recs, fmt.Sprintf(
			"Earning above plan: actual eCPM $%.2f vs $%.2f goal — current mix is working, hold the volume allocation.",
			p.ActualEcpm, d.EcpmGoal))
	}

	// 3. Capacity risk.
	if cap.PlatformDaily > 0 {
		recs = append(recs, fmt.Sprintf(
			"Platform is sending %s emails/day (14-day avg). Active CPM deals require %s/day — %.0f%% of capacity. Risk: %s.",
			fmtCpmInt(int64(cap.PlatformDaily)), fmtCpmInt(int64(cap.TotalRequiredDaily)),
			cap.UtilizationPct*100, cap.Risk))
	}

	// 4. eCPA check.
	if d.EcpaGoal > 0 {
		if p.Conversions == 0 && p.PctDelivered > 0.20 {
			recs = append(recs, fmt.Sprintf(
				"Zero conversions after %.0f%% of planned volume (%s delivered) — verify the everflow postback wiring and the creative's money links before sending more budgeted volume.",
				p.PctDelivered*100, fmtCpmInt(p.Delivered)))
		} else if p.Conversions > 0 && p.ActualEcpa > d.EcpaGoal {
			recs = append(recs, fmt.Sprintf(
				"Actual eCPA $%.2f is above the $%.2f goal — conversions are costing more volume than planned; tighten the audience toward proven converters.",
				p.ActualEcpa, d.EcpaGoal))
		}
	}

	return recs
}

func fmtCpmInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ─── Campaign association (operator 2026-06-13) ──────────────────────────────
// Earmark specific campaigns to a deal so delivery/volume-to-goal counts that
// exact set (UNION-ed with the offer_id auto-match in loadProgress), and the
// operator can see remaining volume to the deal's planned_volume target.

type cpmDealCampaign struct {
	CampaignID string    `json:"campaign_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Delivered  int64     `json:"delivered"`
	AddedAt    time.Time `json:"added_at"`
}

// HandleListDealCampaigns GET /cpm-planner/deals/{id}/campaigns
// Associated campaigns with per-campaign delivered counts + a volume-to-goal
// rollup (planned_volume, delivered, remaining, days-to-goal at current rate).
func (h *CpmPlannerHandlers) HandleListDealCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	deals, err := h.loadDeals(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if len(deals) == 0 {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	d := deals[0]

	rows, err := h.db.Query(`
		SELECT dc.campaign_id, COALESCE(c.name,'(deleted)'), COALESCE(c.status,''),
		       dc.added_at,
		       COALESCE((SELECT COUNT(*) FROM mailing_tracking_events e
		                 WHERE e.campaign_id = dc.campaign_id AND e.event_type='delivered'), 0)
		FROM mailing_cpm_deal_campaigns dc
		LEFT JOIN mailing_campaigns c ON c.id = dc.campaign_id
		WHERE dc.deal_id = $1
		ORDER BY dc.added_at DESC`, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list campaigns: %v", err))
		return
	}
	defer rows.Close()
	out := []cpmDealCampaign{}
	var assocDelivered int64
	for rows.Next() {
		var c cpmDealCampaign
		if err := rows.Scan(&c.CampaignID, &c.Name, &c.Status, &c.AddedAt, &c.Delivered); err != nil {
			continue
		}
		assocDelivered += c.Delivered
		out = append(out, c)
	}

	// Volume-to-goal: planned_volume is the deal's CPM volume target. Use the
	// deal's full DELIVERED (attributed set, from progress) — CPM volume is
	// billed on delivered (operator 2026-06-18), and this keeps the panel in
	// agreement with the headline progress bar and the delivered-based
	// actual_daily rate. (Was Progress.Sent — mislabeled as delivered_total
	// and overstating progress-to-goal; fixed 2026-07-07.)
	remaining := d.PlannedVolume - d.Progress.Delivered
	if remaining < 0 {
		remaining = 0
	}
	var daysToGoal float64
	if d.Progress.ActualDaily > 0 {
		daysToGoal = float64(remaining) / d.Progress.ActualDaily
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"campaigns": out,
		"volume_to_goal": map[string]interface{}{
			"planned_volume":       d.PlannedVolume,
			"delivered_total":      d.Progress.Delivered, // attributed set (offer ∪ earmarks ∪ pattern ∪ offer_key)
			"delivered_associated": assocDelivered,       // associated campaigns only
			"remaining":            remaining,
			"actual_daily":         d.Progress.ActualDaily,
			"days_to_goal":         daysToGoal,
		},
	})
}

// HandleAssociateDealCampaigns POST /cpm-planner/deals/{id}/campaigns {"campaign_ids":[...]}
func (h *CpmPlannerHandlers) HandleAssociateDealCampaigns(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	ok, err := h.dealExists(orgID, dealID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deal: %v", err))
		return
	}
	if !ok {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	var req struct {
		CampaignIDs []string `json:"campaign_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid body")
		return
	}
	inserted := 0
	for _, cid := range req.CampaignIDs {
		cid = strings.TrimSpace(cid)
		if cid == "" {
			continue
		}
		// Only associate campaigns that exist in this org.
		res, err := h.db.Exec(`
			INSERT INTO mailing_cpm_deal_campaigns (deal_id, campaign_id)
			SELECT $1, $2 WHERE EXISTS (
				SELECT 1 FROM mailing_campaigns WHERE id = $2 AND organization_id = $3)
			ON CONFLICT (deal_id, campaign_id) DO NOTHING`, dealID, cid, orgID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("associate: %v", err))
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"associated": inserted, "requested": len(req.CampaignIDs)})
}

// HandleRemoveDealCampaign DELETE /cpm-planner/deals/{id}/campaigns/{campaignID}
func (h *CpmPlannerHandlers) HandleRemoveDealCampaign(w http.ResponseWriter, r *http.Request) {
	dealID := chi.URLParam(r, "id")
	cid := chi.URLParam(r, "campaignID")
	if _, err := h.db.Exec(
		`DELETE FROM mailing_cpm_deal_campaigns WHERE deal_id = $1 AND campaign_id = $2`,
		dealID, cid); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("remove: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"removed": true})
}

// HandleDealCampaignCandidates GET /cpm-planner/deals/{id}/campaign-candidates?q=&limit=
// Recent campaigns the operator can associate — offer-matched first, then a
// name search, excluding already-associated. Lightweight picker source.
func (h *CpmPlannerHandlers) HandleDealCampaignCandidates(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	args := []interface{}{orgID, dealID}
	nameFilter := ""
	if q != "" {
		args = append(args, "%"+q+"%")
		nameFilter = fmt.Sprintf(" AND c.name ILIKE $%d", len(args))
	}
	args = append(args, limit)
	rows, err := h.db.Query(`
		SELECT c.id, c.name, COALESCE(c.status,''), c.created_at
		FROM mailing_campaigns c
		WHERE c.organization_id = $1
		  AND c.created_at > NOW() - INTERVAL '45 days'
		  AND c.id NOT IN (SELECT campaign_id FROM mailing_cpm_deal_campaigns WHERE deal_id = $2)`+
		nameFilter+`
		ORDER BY c.created_at DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("candidates: %v", err))
		return
	}
	defer rows.Close()
	type cand struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	out := []cand{}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &c.CreatedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"candidates": out})
}

// ─── Monthly historics & planning ───────────────────────────────────────────
//
// Two month-anchored capabilities the rolling-window planner lacked:
//   1. Historics — a closed calendar month's delivered / revenue / conversions,
//      portfolio total + per-deal.
//   2. Planning — a per-deal TARGET (budget / volume / eCPM / eCPA) committed for
//      a coming month, against which next month's actuals are measured.
//
// Only the TARGET is stored (mailing_cpm_deal_monthly_targets). Actuals compute
// LIVE from Postgres per Denver calendar month — delivered from
// mailing_tracking_events over the deal's attributed campaign set
// (dealCampaignSetSubquery), conversions from suppressions + manual rows. PG is
// the source (NOT the Athena lake): verified 2026-06-30, PG holds more delivered
// and is 100% campaign-attributed, while the lake loses ~20% of PMTA delivered to
// an unresolved campaign_id. Live compute also self-corrects: fixing a deal's
// attribution retroactively corrects its historics. Revenue = delivered/1000 ×
// eCPM. The lifetime conversions_override is NOT applied per month.

type cpmMonthlyTargetInput struct {
	Budget *float64 `json:"budget"`
	Volume *int64   `json:"volume"`
	Ecpm   *float64 `json:"ecpm"`
	Ecpa   *float64 `json:"ecpa"`
	Notes  *string  `json:"notes"`
}

type cpmMonthlyTarget struct {
	Budget *float64 `json:"budget"`
	Volume *int64   `json:"volume"`
	Ecpm   *float64 `json:"ecpm"`
	Ecpa   *float64 `json:"ecpa"`
	Notes  string   `json:"notes"`
}

type cpmMonthDeal struct {
	DealID             string           `json:"deal_id"`
	Name               string           `json:"name"`
	OfferName          string           `json:"offer_name"`
	HasTarget          bool             `json:"has_target"`
	Target             cpmMonthlyTarget `json:"target"`
	Delivered          int64            `json:"delivered"`
	Revenue            float64          `json:"revenue"`
	Conversions        int64            `json:"conversions"`
	ConversionsTracked int64            `json:"conversions_tracked"`
	ConversionsManual  int64            `json:"conversions_manual"`
	Ecpm               float64          `json:"ecpm"` // effective eCPM used for revenue (target if set, else deal goal)
}

type cpmMonthPortfolio struct {
	Delivered    int64   `json:"delivered"`
	Revenue      float64 `json:"revenue"`
	Conversions  int64   `json:"conversions"`
	TargetBudget float64 `json:"target_budget"`
	TargetVolume int64   `json:"target_volume"`
}

type cpmMonthRow struct {
	Month     string            `json:"month"` // YYYY-MM
	IsCurrent bool              `json:"is_current"`
	IsFuture  bool              `json:"is_future"`
	Portfolio cpmMonthPortfolio `json:"portfolio"`
	Deals     []cpmMonthDeal    `json:"deals"`
}

// cpmMonthlyRevenue is the CPM-billable revenue for a month: delivered volume
// priced at the effective eCPM (revenue = delivered / 1000 × eCPM). This is the
// number the advertiser is billed, distinct from the deal card's conversion-based
// revenue_earned. Returns 0 for non-positive inputs.
func cpmMonthlyRevenue(delivered int64, ecpm float64) float64 {
	if delivered <= 0 || ecpm <= 0 {
		return 0
	}
	return float64(delivered) / 1000.0 * ecpm
}

// cpmMonthlyConversions is the per-month conversion count: tracked + manual for
// THAT month. Unlike the deal card's lifetime count, it deliberately takes no
// override — conversions_override is a lifetime pin and must NOT be applied to a
// single month (footgun, operator 2026-06-30).
func cpmMonthlyConversions(tracked, manual int64) int64 {
	return tracked + manual
}

// loadDealsLite loads deals WITHOUT the per-deal live progress sub-queries
// (loadProgress) — the monthly view computes its own per-month aggregates.
// The second return maps deal id → offer payout (the revenue basis; 0 when
// the offer has none) for callers that price conversions.
func (h *CpmPlannerHandlers) loadDealsLite(orgID string) ([]cpmDeal, map[string]float64, error) {
	rows, err := h.db.Query(cpmDealSelect+" ORDER BY d.created_at DESC", orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	deals := []cpmDeal{}
	payouts := map[string]float64{}
	for rows.Next() {
		d, payout, err := scanCpmDeal(rows)
		if err != nil {
			return nil, nil, err
		}
		deals = append(deals, d)
		payouts[d.ID] = payout
	}
	return deals, payouts, rows.Err()
}

// dealCampaignMapCTE is the unified deal→campaign attribution map (offer match
// ∪ earmarks ∪ name-pattern ∪ stamped offer_key) for ALL of an org's deals at
// once — the whole-org analogue of dealCampaignSetSubquery() (same four
// paths). $1 = org id; queries appending to it may use $2+ freely.
const dealCampaignMapCTE = `
	WITH dm AS (
		SELECT d.id AS deal_id, c.id AS campaign_id
		FROM mailing_cpm_deals d
		JOIN mailing_campaigns c ON c.organization_id = d.organization_id
			AND d.offer_id IS NOT NULL AND c.offer_id = d.offer_id AND c.created_at >= d.start_date
		WHERE d.organization_id = $1
		UNION
		SELECT dc.deal_id, dc.campaign_id
		FROM mailing_cpm_deal_campaigns dc
		JOIN mailing_cpm_deals d ON d.id = dc.deal_id AND d.organization_id = $1
		UNION
		SELECT d.id, c.id
		FROM mailing_cpm_deals d
		JOIN mailing_campaigns c ON c.organization_id = d.organization_id
			AND COALESCE(d.campaign_name_pattern,'') <> '' AND c.name ILIKE d.campaign_name_pattern
			AND c.created_at >= d.start_date
		WHERE d.organization_id = $1
		UNION
		-- Stamped offer_key branch (mirrors dealCampaignSetSubquery): the
		-- deal's everflow id → slug-map slug/offer-name set → campaigns
		-- stamped by campaign_attribution.go (deploy stamp or backfill).
		SELECT d.id, c.id
		FROM mailing_cpm_deals d
		JOIN mailing_offer_slug_map sm ON sm.everflow_offer_id = d.everflow_offer_id
		JOIN mailing_campaigns c ON c.organization_id = d.organization_id
			AND c.created_at >= d.start_date
			AND (c.offer_key = lower(sm.cratoolpro_slug)
			     OR (COALESCE(sm.offer_name,'') <> '' AND c.offer_key = lower(sm.offer_name)))
		WHERE d.organization_id = $1 AND COALESCE(d.everflow_offer_id,'') <> ''
	)`

// cpmDealMonthActuals holds a deal's per-Denver-month actuals (keyed "YYYY-MM").
type cpmDealMonthActuals struct {
	delivered map[string]int64
	tracked   map[string]int64
	manual    map[string]int64
}

// loadAllDealMonthlyActuals computes per-deal × per-month actuals for the whole
// org in THREE passes total (NOT 3×N): one events scan joined to the unified
// deal→campaign map, one suppressions scan, one manual-conversions scan. The
// earlier per-deal-loop version ran N full events scans (~65s for ~9 deals on
// prod); this single pruned pass returns the same numbers in a fraction of that.
// windowStart is "YYYY-MM-DD".
func (h *CpmPlannerHandlers) loadAllDealMonthlyActuals(orgID, windowStart string) map[string]*cpmDealMonthActuals {
	out := map[string]*cpmDealMonthActuals{}
	at := func(dealID string) *cpmDealMonthActuals {
		a := out[dealID]
		if a == nil {
			a = &cpmDealMonthActuals{delivered: map[string]int64{}, tracked: map[string]int64{}, manual: map[string]int64{}}
			out[dealID] = a
		}
		return a
	}
	scan := func(label, q string, set func(*cpmDealMonthActuals, string, int64), args ...interface{}) {
		rows, err := h.db.Query(q, args...)
		if err != nil {
			log.Printf("[CpmPlanner] monthly %s: %v", label, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var dealID, ym string
			var n int64
			if err := rows.Scan(&dealID, &ym, &n); err == nil {
				set(at(dealID), ym, n)
			}
		}
	}

	// Delivered — read from the per-day rollup, NOT a live month scan.
	//
	// This used to scan a whole month of mailing_tracking_events joined to the
	// deal map on every /pacing and /months request. Past the 30s
	// statement_timeout the scan is cancelled and the `scan` helper below just
	// logs and returns, leaving this map EMPTY — so the handler reported
	// mtd_delivered = 0 as though it were fact. Observed repeatedly on prod
	// 2026-08-01 ("[CpmPlanner] monthly delivered: pq: canceling statement due
	// to statement timeout") and reported by the operator as "Mailed MTD is not
	// updating". A silent zero on a budget-pacing screen is worse than an
	// error: it reads as "we mailed nothing" and invites over-sending to catch
	// up. The rollup makes this a cheap indexed SUM that cannot time out.
	scan("delivered", `
		SELECT deal_id::text, to_char(day, 'YYYY-MM') AS ym, SUM(delivered)
		FROM mailing_cpm_deal_day_rollup
		WHERE organization_id = $1 AND day >= $2::date
		GROUP BY 1, 2`,
		func(a *cpmDealMonthActuals, ym string, n int64) { a.delivered[ym] = n },
		orgID, windowStart)

	// Tracked conversions (everflow postback suppressions) by Denver month.
	scan("tracked", `
		SELECT d.id::text,
		       to_char(s.suppressed_at AT TIME ZONE 'America/Denver', 'YYYY-MM') AS ym,
		       COUNT(*)
		FROM mailing_cpm_deals d
		JOIN mailing_offer_suppressions s ON s.organization_id = d.organization_id
			AND d.offer_id IS NOT NULL AND s.offer_id = d.offer_id
			AND s.reason = 'converted'
			AND (s.suppressed_at AT TIME ZONE 'America/Denver')::date >= $2::date
		WHERE d.organization_id = $1
		GROUP BY 1, 2`,
		func(a *cpmDealMonthActuals, ym string, n int64) { a.tracked[ym] = n },
		orgID, windowStart)

	// Manual conversions by STORED (UTC) date — bare-date quick-adds are
	// timezone-naive and must NOT be reprojected through Denver (June 1 → May 31).
	scan("manual", `
		SELECT mc.deal_id::text,
		       to_char(mc.converted_at AT TIME ZONE 'UTC', 'YYYY-MM') AS ym,
		       COALESCE(SUM(mc.count), 0)
		FROM mailing_cpm_manual_conversions mc
		JOIN mailing_cpm_deals d ON d.id = mc.deal_id
		WHERE mc.organization_id = $1
		  AND (mc.converted_at AT TIME ZONE 'UTC')::date >= $2::date
		GROUP BY 1, 2`,
		func(a *cpmDealMonthActuals, ym string, n int64) { a.manual[ym] = n },
		orgID, windowStart)

	return out
}

// HandleMonths GET /cpm-planner/months?months=N
// Per calendar month: portfolio roll-up + per-deal TARGET vs LIVE actuals.
// Covers the last N months (default 6, clamp 1..24) plus any month carrying a
// target row (so a future planning month appears). Newest month first.
func (h *CpmPlannerHandlers) HandleMonths(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	n := 6
	if v := r.URL.Query().Get("months"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 1 && k <= 24 {
			n = k
		}
	}

	// Current Denver month from the DB (avoids a Go tzdata dependency).
	var curMonth time.Time
	if err := h.db.QueryRow(`SELECT (date_trunc('month', NOW() AT TIME ZONE 'America/Denver'))::date`).Scan(&curMonth); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("current month: %v", err))
		return
	}
	curMonth = time.Date(curMonth.Year(), curMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastNStart := curMonth.AddDate(0, -(n - 1), 0)

	// Expand the reported range to include any target month, bounded to
	// [now-24mo, now+12mo] so a stray far-future target can't blow up the range.
	floorM := curMonth.AddDate(0, -24, 0)
	ceilM := curMonth.AddDate(0, 12, 0)
	minMonth, maxMonth := lastNStart, curMonth
	if rows, err := h.db.Query(`
		SELECT DISTINCT month FROM mailing_cpm_deal_monthly_targets
		WHERE organization_id = $1 AND month >= $2 AND month <= $3`, orgID, floorM, ceilM); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var m time.Time
				if err := rows.Scan(&m); err != nil {
					continue
				}
				m = time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
				if m.Before(minMonth) {
					minMonth = m
				}
				if m.After(maxMonth) {
					maxMonth = m
				}
			}
		}()
	} else {
		log.Printf("[CpmPlanner] months target-range scan: %v", err)
	}

	deals, _, err := h.loadDealsLite(orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deals: %v", err))
		return
	}

	windowStart := minMonth.Format("2006-01-02")

	// targets[dealID][ym] = target row
	targets := map[string]map[string]cpmMonthlyTarget{}
	if rows, err := h.db.Query(`
		SELECT deal_id::text, to_char(month, 'YYYY-MM'),
		       target_budget, target_volume, target_ecpm, target_ecpa, COALESCE(notes, '')
		FROM mailing_cpm_deal_monthly_targets
		WHERE organization_id = $1 AND month >= $2`, orgID, minMonth); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var dealID, ym, notes string
				var budget, ecpm, ecpa sql.NullFloat64
				var volume sql.NullInt64
				if err := rows.Scan(&dealID, &ym, &budget, &volume, &ecpm, &ecpa, &notes); err != nil {
					continue
				}
				t := cpmMonthlyTarget{Notes: notes}
				if budget.Valid {
					v := budget.Float64
					t.Budget = &v
				}
				if volume.Valid {
					v := volume.Int64
					t.Volume = &v
				}
				if ecpm.Valid {
					v := ecpm.Float64
					t.Ecpm = &v
				}
				if ecpa.Valid {
					v := ecpa.Float64
					t.Ecpa = &v
				}
				if targets[dealID] == nil {
					targets[dealID] = map[string]cpmMonthlyTarget{}
				}
				targets[dealID][ym] = t
			}
		}()
	} else {
		log.Printf("[CpmPlanner] months target load: %v", err)
	}

	// Per-deal monthly actuals — computed for the whole org in 3 passes total.
	actuals := h.loadAllDealMonthlyActuals(orgID, windowStart)

	curYM := curMonth.Format("2006-01")
	out := []cpmMonthRow{}
	// Iterate newest month first.
	for m := maxMonth; !m.Before(minMonth); m = m.AddDate(0, -1, 0) {
		ym := m.Format("2006-01")
		row := cpmMonthRow{Month: ym, IsCurrent: ym == curYM, IsFuture: m.After(curMonth)}
		for di := range deals {
			d := &deals[di]
			tgt, hasTgt := targets[d.ID][ym]
			var delivered, tracked, manual int64
			if da := actuals[d.ID]; da != nil {
				delivered = da.delivered[ym]
				tracked = da.tracked[ym]
				manual = da.manual[ym]
			}
			conv := cpmMonthlyConversions(tracked, manual)
			// Include a deal in a month only if it has a target or real activity.
			if !hasTgt && delivered == 0 && conv == 0 {
				continue
			}
			ecpm := d.EcpmGoal
			if hasTgt && tgt.Ecpm != nil && *tgt.Ecpm > 0 {
				ecpm = *tgt.Ecpm
			}
			revenue := cpmMonthlyRevenue(delivered, ecpm)
			row.Deals = append(row.Deals, cpmMonthDeal{
				DealID: d.ID, Name: d.Name, OfferName: d.OfferName,
				HasTarget: hasTgt, Target: tgt,
				Delivered: delivered, Revenue: revenue, Conversions: conv,
				ConversionsTracked: tracked, ConversionsManual: manual, Ecpm: ecpm,
			})
			row.Portfolio.Delivered += delivered
			row.Portfolio.Revenue += revenue
			row.Portfolio.Conversions += conv
			if hasTgt {
				if tgt.Budget != nil {
					row.Portfolio.TargetBudget += *tgt.Budget
				}
				if tgt.Volume != nil {
					row.Portfolio.TargetVolume += *tgt.Volume
				}
			}
		}
		out = append(out, row)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"months": out, "current_month": curYM})
}

// parseMonthArg normalizes a {month} path param ("YYYY-MM" or "YYYY-MM-DD") to
// the first-of-month date (UTC midnight).
func parseMonthArg(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01", s); err == nil {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("invalid month %q (want YYYY-MM)", s)
}

// HandleUpsertMonthlyTarget PUT /cpm-planner/deals/{id}/monthly/{month}
// Org-scoped upsert of a deal's monthly target. Numeric fields are partial-merge
// (COALESCE keeps existing when omitted); notes is replaced.
func (h *CpmPlannerHandlers) HandleUpsertMonthlyTarget(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	month, err := parseMonthArg(chi.URLParam(r, "month"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var exists bool
	if err := h.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM mailing_cpm_deals WHERE id = $1 AND organization_id = $2)`,
		dealID, orgID).Scan(&exists); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("verify deal: %v", err))
		return
	}
	if !exists {
		respondError(w, http.StatusNotFound, "deal not found")
		return
	}
	var in cpmMonthlyTargetInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// notes is partial-merge like the numeric fields: omitted (nil) leaves the
	// stored note untouched; provided (even "") replaces it. Kept NULL when
	// omitted so the ON CONFLICT COALESCE preserves the existing value.
	var notes interface{}
	if in.Notes != nil {
		notes = strings.TrimSpace(*in.Notes)
	}
	var budget, volume, ecpm, ecpa interface{}
	if in.Budget != nil {
		budget = *in.Budget
	}
	if in.Volume != nil {
		volume = *in.Volume
	}
	if in.Ecpm != nil {
		ecpm = *in.Ecpm
	}
	if in.Ecpa != nil {
		ecpa = *in.Ecpa
	}
	if _, err := h.db.Exec(`
		INSERT INTO mailing_cpm_deal_monthly_targets
			(organization_id, deal_id, month, target_budget, target_volume, target_ecpm, target_ecpa, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, ''))
		ON CONFLICT (deal_id, month) DO UPDATE SET
			target_budget = COALESCE(EXCLUDED.target_budget, mailing_cpm_deal_monthly_targets.target_budget),
			target_volume = COALESCE(EXCLUDED.target_volume, mailing_cpm_deal_monthly_targets.target_volume),
			target_ecpm   = COALESCE(EXCLUDED.target_ecpm,   mailing_cpm_deal_monthly_targets.target_ecpm),
			target_ecpa   = COALESCE(EXCLUDED.target_ecpa,   mailing_cpm_deal_monthly_targets.target_ecpa),
			notes         = COALESCE($8, mailing_cpm_deal_monthly_targets.notes),
			updated_at    = NOW()`,
		orgID, dealID, month, budget, volume, ecpm, ecpa, notes); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("upsert target: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal_id": dealID, "month": month.Format("2006-01"), "status": "saved",
	})
}

// HandleDeleteMonthlyTarget DELETE /cpm-planner/deals/{id}/monthly/{month}
func (h *CpmPlannerHandlers) HandleDeleteMonthlyTarget(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	dealID := chi.URLParam(r, "id")
	month, err := parseMonthArg(chi.URLParam(r, "month"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.db.Exec(
		`DELETE FROM mailing_cpm_deal_monthly_targets WHERE organization_id = $1 AND deal_id = $2 AND month = $3`,
		orgID, dealID, month)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("delete target: %v", err))
		return
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		respondError(w, http.StatusNotFound, "target not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"deal_id": dealID, "month": month.Format("2006-01"), "status": "deleted",
	})
}

// ─── Attribution gap ─────────────────────────────────────────────────────────

type cpmAttributionGapCampaign struct {
	Name              string `json:"name"`
	OfferKey          string `json:"offer_key"`
	AttributionSource string `json:"attribution_source"`
	Delivered         int64  `json:"delivered"`
	HasOffer          bool   `json:"has_offer"`
}

type cpmAttributionGap struct {
	Days        int    `json:"days"`
	WindowStart string `json:"window_start"`
	// Delivered volume in the window, bucketed by attribution outcome:
	DealDelivered         int64 `json:"deal_delivered"`          // captured by ≥1 deal's attribution map
	OfferNoDealDelivered  int64 `json:"offer_no_deal_delivered"` // offer-identified campaign, but no deal claims it
	UnidentifiedDelivered int64 `json:"unidentified_delivered"`  // no offer identity at all
	DealCampaigns         int   `json:"deal_campaigns"`
	OfferNoDealCampaigns  int   `json:"offer_no_deal_campaigns"`
	UnidentifiedCampaigns int   `json:"unidentified_campaigns"`
	// Largest unattributed (either non-deal bucket) campaigns, delivered desc.
	TopUnattributed []cpmAttributionGapCampaign `json:"top_unattributed"`
}

const cpmAttributionGapDefaultDays = 30

// HandleAttributionGap GET /cpm-planner/attribution-gap?days=N
//
// The acceptance surface for offer→volume attribution (operator 2026-07-07:
// "offer volume isn't correct — one-off broadcasts aren't captured"): every
// delivered event in the window lands in exactly one bucket — captured by a
// deal, offer-identified but claimed by no deal, or carrying no offer identity
// at all. Under-attribution stops being silent low deal volume and becomes a
// number the operator can drive to zero (stamp/backfill/slug-map fixes).
// The default window serves from the refresher's cached rollup (the live
// org-wide delivered scan blew the request-path statement_timeout → 500,
// 2026-07-08); a non-default days= runs live under the long-tx override.
func (h *CpmPlannerHandlers) HandleAttributionGap(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	days := cpmAttributionGapDefaultDays
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n >= 1 && n <= 90 {
		days = n
	}
	if days == cpmAttributionGapDefaultDays {
		h.evMu.Lock()
		cached := h.gapByOrg[orgID]
		h.evMu.Unlock()
		if cached != nil {
			respondJSON(w, http.StatusOK, cached)
			return
		}
		// Cold cache (fresh boot, refresher not done yet): compute once,
		// synchronously, and prime the cache.
	}
	gap, err := h.computeAttributionGap(orgID, days)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("attribution gap: %v", err))
		return
	}
	if days == cpmAttributionGapDefaultDays {
		h.evMu.Lock()
		h.gapByOrg[orgID] = gap
		h.evMu.Unlock()
	}
	respondJSON(w, http.StatusOK, gap)
}

// computeAttributionGap runs the windowed delivered scan inside a
// long-statement transaction (same posture as loadAllDealEventCounts).
func (h *CpmPlannerHandlers) computeAttributionGap(orgID string, days int) (*cpmAttributionGap, error) {
	start := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

	tx, err := h.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	if _, err := tx.Exec(`SET LOCAL statement_timeout = '600s'`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(dealCampaignMapCTE+`
		, vol AS (
			SELECT te.campaign_id, COUNT(*) AS delivered
			FROM mailing_tracking_events te
			WHERE te.organization_id = $1
			  AND te.event_type = 'delivered'
			  AND te.event_at >= $2::date
			GROUP BY 1
		)
		SELECT COALESCE(c.name, '(deleted)'),
		       COALESCE(c.offer_key, ''),
		       COALESCE(c.attribution_source, ''),
		       (c.offer_key IS NOT NULL OR c.offer_id IS NOT NULL) AS has_offer,
		       EXISTS (SELECT 1 FROM dm WHERE dm.campaign_id = v.campaign_id) AS in_deal,
		       v.delivered
		FROM vol v
		LEFT JOIN mailing_campaigns c ON c.id = v.campaign_id
		ORDER BY v.delivered DESC`, orgID, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	gap := cpmAttributionGap{Days: days, WindowStart: start, TopUnattributed: []cpmAttributionGapCampaign{}}
	const topN = 15
	for rows.Next() {
		var c cpmAttributionGapCampaign
		var inDeal bool
		if err := rows.Scan(&c.Name, &c.OfferKey, &c.AttributionSource, &c.HasOffer, &inDeal, &c.Delivered); err != nil {
			continue
		}
		switch {
		case inDeal:
			gap.DealDelivered += c.Delivered
			gap.DealCampaigns++
		case c.HasOffer:
			gap.OfferNoDealDelivered += c.Delivered
			gap.OfferNoDealCampaigns++
		default:
			gap.UnidentifiedDelivered += c.Delivered
			gap.UnidentifiedCampaigns++
		}
		if !inDeal && len(gap.TopUnattributed) < topN {
			gap.TopUnattributed = append(gap.TopUnattributed, c)
		}
	}
	return &gap, rows.Err()
}

// ─── Current-month pacing ────────────────────────────────────────────────────

type cpmPacingDeal struct {
	DealID          string  `json:"deal_id"`
	Name            string  `json:"name"`
	OfferName       string  `json:"offer_name"`
	EverflowOfferID string  `json:"everflow_offer_id"`
	Status          string  `json:"status"`
	TargetVolume    int64   `json:"target_volume"`
	TargetBudget    float64 `json:"target_budget"`
	TargetSource    string  `json:"target_source"` // "monthly" | "deal" | "none"
	// Raw monthly-target fields (null when unset) — the pacing row is the
	// current month's EDIT surface, so the inputs need the stored values.
	HasMonthlyTarget bool     `json:"has_monthly_target"`
	MonthlyBudget    *float64 `json:"monthly_budget"`
	MonthlyEcpm      *float64 `json:"monthly_ecpm"`
	MonthlyEcpa      *float64 `json:"monthly_ecpa"`
	MonthlyNotes     string   `json:"monthly_notes"` // PUT always replaces notes — edit surface must echo it back
	Ecpm             float64  `json:"ecpm"`          // effective (monthly target if set, else deal goal)
	MtdDelivered     int64    `json:"mtd_delivered"`
	MtdConversions   int64    `json:"mtd_conversions"`
	// Month-scoped versions of the deal card's "strong metrics":
	ConversionsNeeded int64   `json:"conversions_needed"` // ⌈month budget ÷ eCPA goal⌉
	ActualEcpm        float64 `json:"actual_ecpm"`        // MTD conv revenue / MTD delivered × 1000
	ActualEcpa        float64 `json:"actual_ecpa"`        // month budget / MTD conversions
	Rate3d            float64 `json:"rate_3d"`            // avg delivered/day over the last 3 complete Denver days
	RequiredDaily     float64 `json:"required_daily"`
	Projected         int64   `json:"projected"`
	ProjectedPct      float64 `json:"projected_pct"`
	OnPace            bool    `json:"on_pace"`
	// OVER band (operator 2026-07-10): on_pace has no upper bound, so a deal
	// that already delivered PAST its month target still read green "ON PACE" —
	// indistinguishable from "keep sending". These flag target-met on MTD
	// ACTUALS (not projection) and size the overage so the operator can pull
	// the offer from the board. Delivery past target is beyond the committed
	// budget (unbilled).
	Over          bool    `json:"over"`            // mtd_delivered ≥ target_volume (target > 0)
	OverVolume    int64   `json:"over_volume"`     // delivered past the month target
	OverBudgetUSD float64 `json:"over_budget_usd"` // over_volume ÷ 1000 × effective eCPM
}

// cpmOnPaceThreshold: a month-end projection at or above this share of the
// target counts as ON PACE (operator 2026-07-08: "projected within 90% is on
// pace; 75% to 90% should display the percentages"). The 75% line is a
// DISPLAY band (amber vs red) owned by the UI; the on_pace flag encodes only
// the 90% rule.
const cpmOnPaceThreshold = 0.90

// cpmPacingMath derives the pacing numbers for one deal-month. dayOfMonth is
// today's Denver day (1-based); requiredDaily spreads the remaining volume over
// the days left INCLUDING today (today's sending can still count toward it);
// the projection adds the trailing 3-day rate over the days AFTER today only,
// since today's partial sends are already inside mtd.
func cpmPacingMath(target, mtd int64, rate3d float64, dayOfMonth, daysInMonth int) (requiredDaily float64, projected int64, projectedPct float64, onPace bool) {
	daysInclToday := daysInMonth - dayOfMonth + 1
	if daysInclToday < 1 {
		daysInclToday = 1
	}
	if target > mtd {
		requiredDaily = float64(target-mtd) / float64(daysInclToday)
	}
	daysAfterToday := daysInMonth - dayOfMonth
	if daysAfterToday < 0 {
		daysAfterToday = 0
	}
	projected = mtd + int64(math.Round(rate3d*float64(daysAfterToday)))
	if target > 0 {
		projectedPct = float64(projected) / float64(target)
		onPace = projectedPct >= cpmOnPaceThreshold
	}
	return
}

// cpmPacingOver: the month target is met/exceeded on MTD ACTUALS — a fact, not
// a forecast (onPace is projection-based and has no upper bound, so 113% and
// 95% both read "on pace"). overVolume is the delivery past target; overUSD
// prices it at the effective eCPM — CPM dollars sent beyond the committed
// budget, which the advertiser is not billed for. Zero-target deals are never
// "over" (nothing to exceed).
func cpmPacingOver(target, mtd int64, ecpm float64) (over bool, overVolume int64, overUSD float64) {
	if target <= 0 || mtd < target {
		return false, 0, 0
	}
	overVolume = mtd - target
	if ecpm > 0 {
		overUSD = float64(overVolume) / 1000.0 * ecpm
	}
	return true, overVolume, overUSD
}

// HandleCurrentMonthPacing GET /cpm-planner/pacing
// Per-deal pacing for the CURRENT Denver month: the month's target (monthly
// target row if set, else derived from the deal's own budget/eCPM plan), MTD
// actuals, the trailing-3-day delivered rate, and the month-end projection
// (operator 2026-07-01: pacing reads the current month; capacity/sending reads
// the last 3 days). Month-scoped events scan — serve on demand (mount /
// explicit refresh / after a target save), never on a poll timer (QA C4).
func (h *CpmPlannerHandlers) HandleCurrentMonthPacing(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	// Current Denver date from the DB (same tz source as HandleMonths).
	var curDate time.Time
	if err := h.db.QueryRow(`SELECT (NOW() AT TIME ZONE 'America/Denver')::date`).Scan(&curDate); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("current date: %v", err))
		return
	}
	curMonth := time.Date(curDate.Year(), curDate.Month(), 1, 0, 0, 0, 0, time.UTC)
	dayOfMonth := curDate.Day()
	daysInMonth := curMonth.AddDate(0, 1, -1).Day()
	curYM := curMonth.Format("2006-01")

	deals, payouts, err := h.loadDealsLite(orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("load deals: %v", err))
		return
	}

	// Current month's targets.
	type tgt struct {
		budget, ecpm, ecpa sql.NullFloat64
		volume             sql.NullInt64
		notes              string
	}
	targets := map[string]tgt{}
	if rows, err := h.db.Query(`
		SELECT deal_id::text, target_budget, target_volume, target_ecpm, target_ecpa, COALESCE(notes, '')
		FROM mailing_cpm_deal_monthly_targets
		WHERE organization_id = $1 AND month = $2`, orgID, curMonth); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				var t tgt
				if err := rows.Scan(&id, &t.budget, &t.volume, &t.ecpm, &t.ecpa, &t.notes); err == nil {
					targets[id] = t
				}
			}
		}()
	} else {
		log.Printf("[CpmPlanner] pacing targets: %v", err)
	}

	// MTD actuals — the shared monthly loader scoped to just this month.
	actuals := h.loadAllDealMonthlyActuals(orgID, curMonth.Format("2006-01-02"))

	// MTD manual-conversion revenue components (per deal): rows carrying real
	// revenue sum as-is; revenue-less rows are counted so they can be valued at
	// the deal's basis (offer payout, else eCPA goal) — the same effective-
	// revenue rule loadProgress applies (":403"). UTC month like the loader.
	type manualRev struct {
		posRevenue float64
		noRevCount int64
	}
	manualRevs := map[string]manualRev{}
	if rows, err := h.db.Query(`
		SELECT deal_id::text,
		       COALESCE(SUM(CASE WHEN revenue > 0 THEN revenue ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN revenue <= 0 THEN count ELSE 0 END), 0)
		FROM mailing_cpm_manual_conversions
		WHERE organization_id = $1
		  AND (converted_at AT TIME ZONE 'UTC')::date >= $2::date
		GROUP BY 1`, orgID, curMonth.Format("2006-01-02")); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				var mr manualRev
				if err := rows.Scan(&id, &mr.posRevenue, &mr.noRevCount); err == nil {
					manualRevs[id] = mr
				}
			}
		}()
	} else {
		log.Printf("[CpmPlanner] pacing manual revenue: %v", err)
	}

	// Trailing 3-day delivered per deal (last 3 COMPLETE Denver days — today's
	// partial day would understate the rate). Same pruning pattern as the
	// monthly loader: bare event_at bound for partitions, Denver ::date edges.
	// Same rollup, same reason: this scan was also being cancelled on prod
	// ("[CpmPlanner] pacing 3d rate: pq: canceling statement due to statement
	// timeout"), which zeroed the trailing rate and therefore the month-end
	// projection — a deal that was pacing fine rendered as projecting nothing.
	rate3d := map[string]float64{}
	if rows, err := h.db.Query(`
		SELECT deal_id::text, SUM(delivered)
		FROM mailing_cpm_deal_day_rollup
		WHERE organization_id = $1 AND day >= $2::date AND day < $3::date
		GROUP BY 1`,
		orgID, curDate.AddDate(0, 0, -3).Format("2006-01-02"), curDate.Format("2006-01-02")); err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var id string
				var n int64
				if err := rows.Scan(&id, &n); err == nil {
					rate3d[id] = float64(n) / 3.0
				}
			}
		}()
	} else {
		log.Printf("[CpmPlanner] pacing 3d rate: %v", err)
	}

	out := []cpmPacingDeal{}
	var pf struct {
		TargetVolume   int64   `json:"target_volume"`
		MtdDelivered   int64   `json:"mtd_delivered"`
		MtdConversions int64   `json:"mtd_conversions"`
		Projected      int64   `json:"projected"`
		RequiredDaily  float64 `json:"required_daily"`
	}
	for i := range deals {
		d := &deals[i]
		var mtd, tracked, manual int64
		if a := actuals[d.ID]; a != nil {
			mtd, tracked, manual = a.delivered[curYM], a.tracked[curYM], a.manual[curYM]
		}

		ecpm, ecpaGoal := d.EcpmGoal, d.EcpaGoal
		targetVolume, targetBudget := int64(0), 0.0
		source := "none"
		var hasTgt bool
		var mBudget, mEcpm, mEcpa *float64
		var mNotes string
		if t, ok := targets[d.ID]; ok {
			hasTgt = true
			mNotes = t.notes
			if t.budget.Valid {
				v := t.budget.Float64
				mBudget, targetBudget = &v, v
			}
			if t.ecpm.Valid {
				v := t.ecpm.Float64
				mEcpm = &v
				if v > 0 {
					ecpm = v
				}
			}
			if t.ecpa.Valid {
				v := t.ecpa.Float64
				mEcpa = &v
				if v > 0 {
					ecpaGoal = v
				}
			}
			switch {
			case t.volume.Valid && t.volume.Int64 > 0:
				targetVolume, source = t.volume.Int64, "monthly"
			case targetBudget > 0 && ecpm > 0:
				targetVolume, source = int64(math.Ceil(targetBudget/ecpm*1000)), "monthly"
			}
		}
		if source == "none" && d.Status == "active" {
			// No monthly target — fall back to the deal's own plan so an
			// untargeted active deal still paces against SOMETHING visible.
			if planned, _, _ := cpmPlanNumbers(d.Budget, d.EcpmGoal, d.EcpaGoal, d.AvgCampaignSize); planned > 0 {
				targetVolume, targetBudget, source = planned, d.Budget, "deal"
			}
		}

		conv := cpmMonthlyConversions(tracked, manual)
		if targetVolume == 0 && mtd == 0 && conv == 0 {
			continue // nothing to pace, nothing sent — skip the row
		}

		// Month-scoped strong metrics, same definitions as the deal card:
		// revenue basis = offer payout, else eCPA goal (loadProgress ":386").
		basis := payouts[d.ID]
		if basis == 0 && ecpaGoal > 0 {
			basis = ecpaGoal
		}
		mr := manualRevs[d.ID]
		mtdRevenue := float64(tracked)*basis + mr.posRevenue + float64(mr.noRevCount)*basis
		var actualEcpm float64
		if mtd > 0 {
			actualEcpm = mtdRevenue / float64(mtd) * 1000
		}
		var convNeeded int64
		if targetBudget > 0 && ecpaGoal > 0 {
			convNeeded = int64(math.Ceil(targetBudget / ecpaGoal))
		}

		requiredDaily, projected, projectedPct, onPace := cpmPacingMath(targetVolume, mtd, rate3d[d.ID], dayOfMonth, daysInMonth)
		over, overVolume, overUSD := cpmPacingOver(targetVolume, mtd, ecpm)
		out = append(out, cpmPacingDeal{
			DealID: d.ID, Name: d.Name, OfferName: d.OfferName, EverflowOfferID: d.EverflowOfferID,
			Status: d.Status, TargetVolume: targetVolume, TargetBudget: targetBudget, TargetSource: source,
			HasMonthlyTarget: hasTgt, MonthlyBudget: mBudget, MonthlyEcpm: mEcpm, MonthlyEcpa: mEcpa, MonthlyNotes: mNotes,
			Ecpm: ecpm, MtdDelivered: mtd, MtdConversions: conv,
			ConversionsNeeded: convNeeded, ActualEcpm: actualEcpm, ActualEcpa: cpmActualEcpa(targetBudget, conv),
			Rate3d: rate3d[d.ID], RequiredDaily: requiredDaily,
			Projected: projected, ProjectedPct: projectedPct, OnPace: onPace,
			Over: over, OverVolume: overVolume, OverBudgetUSD: overUSD,
		})
		pf.TargetVolume += targetVolume
		pf.MtdDelivered += mtd
		pf.MtdConversions += conv
		pf.Projected += projected
		pf.RequiredDaily += requiredDaily
	}

	// Rollup coverage for the month so far. MTD delivered is only as complete as
	// the days that have been rolled up; without this the screen cannot tell
	// "we mailed less" from "the rollup has not caught up", which is exactly the
	// ambiguity that made the old silent-zero so hard to spot.
	var daysRolled int
	if err := h.db.QueryRow(`
		SELECT COUNT(DISTINCT day) FROM mailing_cpm_deal_day_rollup
		WHERE organization_id = $1 AND day >= $2::date`,
		orgID, curMonth.Format("2006-01-02")).Scan(&daysRolled); err != nil {
		log.Printf("[CpmPlanner] pacing rollup coverage: %v", err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"month":          curYM,
		"day_of_month":   dayOfMonth,
		"days_in_month":  daysInMonth,
		"deals":          out,
		"portfolio":      pf,
		"days_rolled":    daysRolled,
		"volume_partial": daysRolled < dayOfMonth,
	})
}

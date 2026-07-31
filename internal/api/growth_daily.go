package api

// Growth time series — the platform home of the operator's growth spreadsheet.
//
//	GET /api/mailing/growth/daily?from=&to=&sending_domain=&isp=
//	GET /api/mailing/growth/filters
//
// Reads mailing_growth_daily (written by worker.GrowthRollupWorker — see that
// file for per-column semantics and sources) and returns ONE row per Denver
// day for the selected (sending domain × ISP) slice, with the day-over-day
// deltas the operator tracks.
//
// COLUMN CONTRACT — deliberately the operator's sheet, computed once here so
// the screen, the CSV export and any future consumer cannot disagree:
//
//	delivered        lake, real transports only (pmta/ses/kumo)
//	attempted        DERIVED = delivered + hard + soft (METRIC_CONTRACT §2)
//	open_rate        open_events / delivered — EVENTS, not unique mailboxes,
//	                 which is why it legitimately exceeds 100% (one scanner
//	                 opens one message many times). This is the operator's
//	                 long-standing convention; open_subs is carried alongside
//	                 for the "how many mailboxes" read.
//	click_rate       click_events / delivered (same convention)
//	action_click_rate action_click_subs / delivered — the GOVERNING cohort
//	                 (C_CLICK_ACTION): navigational links only, asset fetches
//	                 excluded. This is the number that means engagement.
//	complaint_rate   complaints / delivered
//	unsub_rate       unsubs / delivered
//	hard_rate/soft_rate  ALWAYS separate, never summed (bounce doctrine).
//	delta_*          day-over-day vs the PREVIOUS RETURNED ROW; percentage
//	                 deltas are relative-to-previous ((cur-prev)/prev).
//
// Rates are computed here and never stored, so a denominator change can never
// leave stale percentages in the table.
//
// COVERAGE HONESTY (rendered, never hidden): lake coverage starts 2026-04-06,
// and per-sending-domain attribution is only exact from 2026-07-02 — before
// that the emitters wrote a blank brand and SES rows carry no VMTA fallback,
// so that volume lands under "(unattributed)". Global (all-domain) totals are
// correct for the whole window either way.
//
// Org-scoped via GetOrgIDFromRequest for consistency with sibling analytics
// handlers; the rollup itself is not org-partitioned (single-tenant estate),
// which the response states rather than implying otherwise.
//
// READ ONLY. No writes anywhere.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// VersionGrowthDaily is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-07-29): initial — daily growth series + filters.
//	1.1 (2026-07-30): + /growth/day — single-day breakdown by ISP or by
//	    sending domain (the expand-a-day drill-down on the Growth tab).
const VersionGrowthDaily = "1.1"

const (
	growthQueryTimeout = 20 * time.Second
	// growthMaxRangeDays bounds one request. ~2 years of daily rows is far
	// more than any review needs and keeps the response small.
	growthMaxRangeDays = 800
	// growthLakeEpoch mirrors worker.growthLakeEpoch — the first Denver day
	// with lake coverage. Requests are clamped to it rather than silently
	// returning empty leading rows.
	growthLakeEpoch = "2026-04-06"
	// growthDomainAttributionFrom is the first day per-sending-domain
	// attribution is exact (measured 2026-07-29: brand resolvable 100% from
	// this day, 17%–92% before it).
	growthDomainAttributionFrom = "2026-07-02"
	// growthUnattributedLabel is how a blank sending_domain is displayed.
	growthUnattributedLabel = "(unattributed)"
)

// ── response contract ────────────────────────────────────────────────────────

type growthRow struct {
	Day string `json:"day"`

	Delivered       int64 `json:"delivered"`
	Attempted       int64 `json:"attempted"`
	HardBounce      int64 `json:"hard_bounce"`
	SoftBounce      int64 `json:"soft_bounce"`
	ReputationBlock int64 `json:"reputation_block"`
	Complaints      int64 `json:"complaints"`
	Unsubs          int64 `json:"unsubs"`

	OpenEvents      int64 `json:"open_events"`
	OpenSubs        int64 `json:"open_subs"`
	ClickEvents     int64 `json:"click_events"`
	ClickSubs       int64 `json:"click_subs"`
	ActionClickSubs int64 `json:"action_click_subs"`

	DeliveryPct    float64 `json:"delivery_pct"`
	OpenPct        float64 `json:"open_pct"`
	ClickPct       float64 `json:"click_pct"`
	ActionClickPct float64 `json:"action_click_pct"`
	ComplaintPct   float64 `json:"complaint_pct"`
	UnsubPct       float64 `json:"unsub_pct"`
	HardPct        float64 `json:"hard_pct"`
	SoftPct        float64 `json:"soft_pct"`

	// Day-over-day vs the previous returned row. Null on the first row.
	DeltaDelivered    *int64   `json:"delta_delivered"`
	DeltaDeliveredPct *float64 `json:"delta_delivered_pct"`
	DeltaOpenPct      *float64 `json:"delta_open_pct"`
	DeltaClickPct     *float64 `json:"delta_click_pct"`
}

type growthTotals struct {
	Delivered       int64   `json:"delivered"`
	Attempted       int64   `json:"attempted"`
	HardBounce      int64   `json:"hard_bounce"`
	SoftBounce      int64   `json:"soft_bounce"`
	ReputationBlock int64   `json:"reputation_block"`
	Complaints      int64   `json:"complaints"`
	Unsubs          int64   `json:"unsubs"`
	OpenEvents      int64   `json:"open_events"`
	ClickEvents     int64   `json:"click_events"`
	ActionClickSubs int64   `json:"action_click_subs"`
	OpenPct         float64 `json:"open_pct"`
	ClickPct        float64 `json:"click_pct"`
	ActionClickPct  float64 `json:"action_click_pct"`
	ComplaintPct    float64 `json:"complaint_pct"`
	UnsubPct        float64 `json:"unsub_pct"`
	Days            int     `json:"days"`
}

type growthResponse struct {
	Version       string       `json:"version"`
	From          string       `json:"from"`
	To            string       `json:"to"`
	SendingDomain string       `json:"sending_domain"`
	ISP           string       `json:"isp"`
	Rows          []growthRow  `json:"rows"`
	Totals        growthTotals `json:"totals"`
	Coverage      struct {
		LakeEpoch             string `json:"lake_epoch"`
		DomainAttributionFrom string `json:"domain_attribution_from"`
		LastComputedAt        string `json:"last_computed_at"`
	} `json:"coverage"`
	Notes []string `json:"notes"`
}

// growthBreakdownRow is one dimension value's slice of a single day. Same
// column contract as growthRow (rates computed here, never stored); no deltas —
// there is no "previous row" inside one day. SharePct is this slice's share of
// the day's delivered, so the sub-table shows where the volume went.
type growthBreakdownRow struct {
	Key string `json:"key"` // the ISP bucket or the sending-domain apex

	Delivered       int64 `json:"delivered"`
	Attempted       int64 `json:"attempted"`
	HardBounce      int64 `json:"hard_bounce"`
	SoftBounce      int64 `json:"soft_bounce"`
	ReputationBlock int64 `json:"reputation_block"`
	Complaints      int64 `json:"complaints"`
	Unsubs          int64 `json:"unsubs"`

	OpenEvents      int64 `json:"open_events"`
	OpenSubs        int64 `json:"open_subs"`
	ClickEvents     int64 `json:"click_events"`
	ClickSubs       int64 `json:"click_subs"`
	ActionClickSubs int64 `json:"action_click_subs"`

	SharePct       float64 `json:"share_pct"`
	DeliveryPct    float64 `json:"delivery_pct"`
	OpenPct        float64 `json:"open_pct"`
	ClickPct       float64 `json:"click_pct"`
	ActionClickPct float64 `json:"action_click_pct"`
	ComplaintPct   float64 `json:"complaint_pct"`
	UnsubPct       float64 `json:"unsub_pct"`
	HardPct        float64 `json:"hard_pct"`
	SoftPct        float64 `json:"soft_pct"`
}

type growthBreakdownResponse struct {
	Version       string               `json:"version"`
	Day           string               `json:"day"`
	By            string               `json:"by"`
	SendingDomain string               `json:"sending_domain"`
	ISP           string               `json:"isp"`
	Rows          []growthBreakdownRow `json:"rows"`
	Notes         []string             `json:"notes"`
}

type growthFiltersResponse struct {
	Version        string   `json:"version"`
	SendingDomains []string `json:"sending_domains"`
	ISPs           []string `json:"isps"`
	MinDay         string   `json:"min_day"`
	MaxDay         string   `json:"max_day"`
}

// ── service ──────────────────────────────────────────────────────────────────

// GrowthService serves the growth time series.
type GrowthService struct{ db *sql.DB }

// NewGrowthService builds the service.
func NewGrowthService(db *sql.DB) *GrowthService { return &GrowthService{db: db} }

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *GrowthService) RegisterRoutes(r chi.Router) {
	r.Get("/growth/daily", s.HandleDaily)
	r.Get("/growth/day", s.HandleDayBreakdown)
	r.Get("/growth/filters", s.HandleFilters)
}

// growthPct returns n/d as a percentage rounded to 2dp. Zero denominator → 0.
func growthPct(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(int64(float64(n)/float64(d)*10000.0+0.5)) / 100.0
}

// growthRelDelta returns (cur-prev)/prev as a percentage rounded to 2dp, or nil
// when prev is zero (an undefined ratio must not render as 0% growth — that is
// the #DIV/0! the spreadsheet showed honestly).
//
// math.Round, NOT the int64(x+0.5) idiom used by growthPct: deltas are signed,
// and +0.5-then-truncate rounds negatives toward zero (-47.4229 → -47.41),
// which understates every decline on the screen.
func growthRelDelta(cur, prev float64) *float64 {
	if prev == 0 {
		return nil
	}
	v := math.Round((cur-prev)/prev*10000.0) / 100.0
	return &v
}

// parseGrowthDay validates a YYYY-MM-DD query param, returning ok=false when
// it is empty or malformed so the caller can apply its default.
func parseGrowthDay(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// HandleDaily serves GET /api/mailing/growth/daily.
func (s *GrowthService) HandleDaily(w http.ResponseWriter, r *http.Request) {
	if _, err := GetOrgIDFromRequest(r); err != nil {
		respondError(w, http.StatusBadRequest, "organization not resolved")
		return
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), growthQueryTimeout)
	defer cancel()

	q := r.URL.Query()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc)

	to, ok := parseGrowthDay(q.Get("to"))
	if !ok {
		to = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	}
	from, ok := parseGrowthDay(q.Get("from"))
	if !ok {
		from = to.AddDate(0, 0, -29)
	}
	if from.After(to) {
		from, to = to, from
	}
	epoch, _ := time.Parse("2006-01-02", growthLakeEpoch)
	if from.Before(epoch) {
		from = epoch
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > growthMaxRangeDays {
		from = to.AddDate(0, 0, -(growthMaxRangeDays - 1))
	}

	domain := normalizeSendingDomain(q.Get("sending_domain"))
	if strings.TrimSpace(q.Get("sending_domain")) == "" {
		domain = "" // all domains
	}
	isp := strings.ToLower(strings.TrimSpace(q.Get("isp")))

	// One indexed aggregate over the slice. Filters are bound parameters; the
	// empty string means "all" for both dimensions.
	rows, err := s.db.QueryContext(ctx, `
		SELECT day,
		       COALESCE(SUM(delivered),0), COALESCE(SUM(hard_bounce),0),
		       COALESCE(SUM(soft_bounce),0), COALESCE(SUM(reputation_block),0),
		       COALESCE(SUM(complaints),0), COALESCE(SUM(unsubs),0),
		       COALESCE(SUM(open_events),0), COALESCE(SUM(open_subs),0),
		       COALESCE(SUM(click_events),0), COALESCE(SUM(click_subs),0),
		       COALESCE(SUM(action_click_subs),0),
		       MAX(computed_at)
		  FROM mailing_growth_daily
		 WHERE day >= $1::date AND day <= $2::date
		   AND ($3 = '' OR sending_domain = $3)
		   AND ($4 = '' OR isp = $4)
		 GROUP BY day
		 ORDER BY day
	`, from.Format("2006-01-02"), to.Format("2006-01-02"), domain, isp)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "growth query failed: "+err.Error())
		return
	}
	defer rows.Close()

	resp := growthResponse{
		Version:       VersionGrowthDaily,
		From:          from.Format("2006-01-02"),
		To:            to.Format("2006-01-02"),
		SendingDomain: domain,
		ISP:           isp,
		Rows:          []growthRow{},
	}
	resp.Coverage.LakeEpoch = growthLakeEpoch
	resp.Coverage.DomainAttributionFrom = growthDomainAttributionFrom

	var lastComputed time.Time
	var prev *growthRow
	for rows.Next() {
		var day time.Time
		var computed sql.NullTime
		var g growthRow
		if err := rows.Scan(&day,
			&g.Delivered, &g.HardBounce, &g.SoftBounce, &g.ReputationBlock,
			&g.Complaints, &g.Unsubs,
			&g.OpenEvents, &g.OpenSubs, &g.ClickEvents, &g.ClickSubs,
			&g.ActionClickSubs, &computed); err != nil {
			respondError(w, http.StatusInternalServerError, "growth scan failed: "+err.Error())
			return
		}
		g.Day = day.Format("2006-01-02")
		g.Attempted = g.Delivered + g.HardBounce + g.SoftBounce
		g.DeliveryPct = growthPct(g.Delivered, g.Attempted)
		g.OpenPct = growthPct(g.OpenEvents, g.Delivered)
		g.ClickPct = growthPct(g.ClickEvents, g.Delivered)
		g.ActionClickPct = growthPct(g.ActionClickSubs, g.Delivered)
		g.ComplaintPct = growthPct(g.Complaints, g.Delivered)
		g.UnsubPct = growthPct(g.Unsubs, g.Delivered)
		g.HardPct = growthPct(g.HardBounce, g.Attempted)
		g.SoftPct = growthPct(g.SoftBounce, g.Attempted)

		if prev != nil {
			d := g.Delivered - prev.Delivered
			g.DeltaDelivered = &d
			g.DeltaDeliveredPct = growthRelDelta(float64(g.Delivered), float64(prev.Delivered))
			g.DeltaOpenPct = growthRelDelta(g.OpenPct, prev.OpenPct)
			g.DeltaClickPct = growthRelDelta(g.ClickPct, prev.ClickPct)
		}

		if computed.Valid && computed.Time.After(lastComputed) {
			lastComputed = computed.Time
		}
		resp.Rows = append(resp.Rows, g)
		prev = &resp.Rows[len(resp.Rows)-1]
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "growth iteration failed: "+err.Error())
		return
	}

	for _, g := range resp.Rows {
		resp.Totals.Delivered += g.Delivered
		resp.Totals.Attempted += g.Attempted
		resp.Totals.HardBounce += g.HardBounce
		resp.Totals.SoftBounce += g.SoftBounce
		resp.Totals.ReputationBlock += g.ReputationBlock
		resp.Totals.Complaints += g.Complaints
		resp.Totals.Unsubs += g.Unsubs
		resp.Totals.OpenEvents += g.OpenEvents
		resp.Totals.ClickEvents += g.ClickEvents
		resp.Totals.ActionClickSubs += g.ActionClickSubs
	}
	resp.Totals.Days = len(resp.Rows)
	resp.Totals.OpenPct = growthPct(resp.Totals.OpenEvents, resp.Totals.Delivered)
	resp.Totals.ClickPct = growthPct(resp.Totals.ClickEvents, resp.Totals.Delivered)
	resp.Totals.ActionClickPct = growthPct(resp.Totals.ActionClickSubs, resp.Totals.Delivered)
	resp.Totals.ComplaintPct = growthPct(resp.Totals.Complaints, resp.Totals.Delivered)
	resp.Totals.UnsubPct = growthPct(resp.Totals.Unsubs, resp.Totals.Delivered)

	if !lastComputed.IsZero() {
		resp.Coverage.LastComputedAt = lastComputed.UTC().Format(time.RFC3339)
	}
	resp.Notes = growthNotes(from, domain, len(resp.Rows))
	respondJSON(w, http.StatusOK, resp)
}

// growthNotes builds the honest caveats for the requested slice.
func growthNotes(from time.Time, domain string, nRows int) []string {
	notes := []string{
		"Open/click rates are EVENTS ÷ delivered (the long-standing convention) — one scanner opening one message many times can push open rate above 100%. Action clicks are unique mailboxes on navigational links only.",
		"Delivered / bounces / complaints come from the event lake (real transports only); opens / clicks / unsubscribes from platform tracking. Hard and soft bounces are never summed.",
	}
	attrFrom, _ := time.Parse("2006-01-02", growthDomainAttributionFrom)
	if from.Before(attrFrom) {
		if domain == "" {
			notes = append(notes, fmt.Sprintf(
				"Before %s the sender was not stamped on every delivery row; that volume is grouped under %q. All-domain totals are unaffected.",
				growthDomainAttributionFrom, growthUnattributedLabel))
		} else {
			notes = append(notes, fmt.Sprintf(
				"Per-domain history is only exact from %s — earlier days under-count this domain because the sender was not stamped on every delivery row.",
				growthDomainAttributionFrom))
		}
	}
	if nRows == 0 {
		notes = append(notes, "No rows yet for this range — the growth rollup back-fills history in chunks every 2 hours.")
	}
	return notes
}

// HandleDayBreakdown serves GET /api/mailing/growth/day — one day of the same
// rollup, split by ISP (default) or by sending domain. This is the expand-arrow
// drill-down under a day row on the Growth tab: same source (mailing_growth_daily,
// delivery from the Athena lake via the rollup worker), same rate conventions,
// just a different GROUP BY — so the sub-table can never disagree with the row
// it expands.
func (s *GrowthService) HandleDayBreakdown(w http.ResponseWriter, r *http.Request) {
	if _, err := GetOrgIDFromRequest(r); err != nil {
		respondError(w, http.StatusBadRequest, "organization not resolved")
		return
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), growthQueryTimeout)
	defer cancel()

	q := r.URL.Query()
	day, ok := parseGrowthDay(q.Get("day"))
	if !ok {
		respondError(w, http.StatusBadRequest, "day is required (YYYY-MM-DD)")
		return
	}

	by := strings.ToLower(strings.TrimSpace(q.Get("by")))
	if by == "" {
		by = "isp"
	}
	// The dimension is interpolated into SQL, so it is a strict whitelist —
	// never the raw query value.
	var dimCol string
	switch by {
	case "isp":
		dimCol = "isp"
	case "sending_domain":
		dimCol = "sending_domain"
	default:
		respondError(w, http.StatusBadRequest, "by must be 'isp' or 'sending_domain'")
		return
	}

	domain := normalizeSendingDomain(q.Get("sending_domain"))
	if strings.TrimSpace(q.Get("sending_domain")) == "" {
		domain = "" // all domains
	}
	isp := strings.ToLower(strings.TrimSpace(q.Get("isp")))

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+dimCol+`,
		       COALESCE(SUM(delivered),0), COALESCE(SUM(hard_bounce),0),
		       COALESCE(SUM(soft_bounce),0), COALESCE(SUM(reputation_block),0),
		       COALESCE(SUM(complaints),0), COALESCE(SUM(unsubs),0),
		       COALESCE(SUM(open_events),0), COALESCE(SUM(open_subs),0),
		       COALESCE(SUM(click_events),0), COALESCE(SUM(click_subs),0),
		       COALESCE(SUM(action_click_subs),0)
		  FROM mailing_growth_daily
		 WHERE day = $1::date
		   AND ($2 = '' OR sending_domain = $2)
		   AND ($3 = '' OR isp = $3)
		 GROUP BY `+dimCol+`
		 ORDER BY 2 DESC, 1
	`, day.Format("2006-01-02"), domain, isp)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "growth breakdown query failed: "+err.Error())
		return
	}
	defer rows.Close()

	resp := growthBreakdownResponse{
		Version:       VersionGrowthDaily,
		Day:           day.Format("2006-01-02"),
		By:            by,
		SendingDomain: domain,
		ISP:           isp,
		Rows:          []growthBreakdownRow{},
	}

	var dayDelivered int64
	for rows.Next() {
		var g growthBreakdownRow
		if err := rows.Scan(&g.Key,
			&g.Delivered, &g.HardBounce, &g.SoftBounce, &g.ReputationBlock,
			&g.Complaints, &g.Unsubs,
			&g.OpenEvents, &g.OpenSubs, &g.ClickEvents, &g.ClickSubs,
			&g.ActionClickSubs); err != nil {
			respondError(w, http.StatusInternalServerError, "growth breakdown scan failed: "+err.Error())
			return
		}
		if g.Key == "" {
			g.Key = growthUnattributedLabel
		}
		g.Attempted = g.Delivered + g.HardBounce + g.SoftBounce
		g.DeliveryPct = growthPct(g.Delivered, g.Attempted)
		g.OpenPct = growthPct(g.OpenEvents, g.Delivered)
		g.ClickPct = growthPct(g.ClickEvents, g.Delivered)
		g.ActionClickPct = growthPct(g.ActionClickSubs, g.Delivered)
		g.ComplaintPct = growthPct(g.Complaints, g.Delivered)
		g.UnsubPct = growthPct(g.Unsubs, g.Delivered)
		g.HardPct = growthPct(g.HardBounce, g.Attempted)
		g.SoftPct = growthPct(g.SoftBounce, g.Attempted)
		dayDelivered += g.Delivered
		resp.Rows = append(resp.Rows, g)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "growth breakdown iteration failed: "+err.Error())
		return
	}
	for i := range resp.Rows {
		resp.Rows[i].SharePct = growthPct(resp.Rows[i].Delivered, dayDelivered)
	}

	resp.Notes = []string{
		"Same rollup rows as the day series (delivery from the event lake, engagement from platform tracking) — only the grouping differs, so these slices always sum to the day row.",
		"Open/click rates are EVENTS ÷ delivered; action clicks are unique mailboxes on navigational links only. Hard and soft bounces are never summed.",
	}
	attrFrom, _ := time.Parse("2006-01-02", growthDomainAttributionFrom)
	if by == "sending_domain" && day.Before(attrFrom) {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"Before %s the sender was not stamped on every delivery row; that volume is grouped under %q.",
			growthDomainAttributionFrom, growthUnattributedLabel))
	}
	respondJSON(w, http.StatusOK, resp)
}

// HandleFilters serves GET /api/mailing/growth/filters — the values actually
// present in the rollup, so the screen never offers a filter that returns
// nothing.
func (s *GrowthService) HandleFilters(w http.ResponseWriter, r *http.Request) {
	if _, err := GetOrgIDFromRequest(r); err != nil {
		respondError(w, http.StatusBadRequest, "organization not resolved")
		return
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), growthQueryTimeout)
	defer cancel()

	out := growthFiltersResponse{
		Version:        VersionGrowthDaily,
		SendingDomains: []string{},
		ISPs:           []string{},
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT sending_domain FROM mailing_growth_daily
		 WHERE delivered > 0 OR open_events > 0
		 ORDER BY sending_domain
	`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "growth filters failed: "+err.Error())
		return
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "growth filters scan failed: "+err.Error())
			return
		}
		out.SendingDomains = append(out.SendingDomains, d)
	}
	rows.Close()

	irows, err := s.db.QueryContext(ctx, `
		SELECT isp, SUM(delivered) d FROM mailing_growth_daily
		 GROUP BY isp ORDER BY d DESC
	`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "growth filters failed: "+err.Error())
		return
	}
	for irows.Next() {
		var isp string
		var d sql.NullInt64
		if err := irows.Scan(&isp, &d); err != nil {
			irows.Close()
			respondError(w, http.StatusInternalServerError, "growth filters scan failed: "+err.Error())
			return
		}
		out.ISPs = append(out.ISPs, isp)
	}
	irows.Close()

	var minDay, maxDay sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(day), MAX(day) FROM mailing_growth_daily`).Scan(&minDay, &maxDay); err == nil {
		if minDay.Valid {
			out.MinDay = minDay.Time.Format("2006-01-02")
		}
		if maxDay.Valid {
			out.MaxDay = maxDay.Time.Format("2006-01-02")
		}
	}
	respondJSON(w, http.StatusOK, out)
}

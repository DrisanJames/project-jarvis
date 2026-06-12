package api

// Send Baselines & Scorecards — volume-governance baselines per
// (sending_domain × ISP) plus historical domain × offer × ISP scorecards.
//
// Operator intent: "establish baselines for ALL sending domains as it relates
// to their ISP they are targeting. This baseline should judge if we increase
// or decrease or maintain volume. Score cards for post sending by sending
// domain by offer by ISP would be great to review. This can be historical."
//
// Data sources (campaign counters are stale — never used here):
//   - mailing_domain_agent_scorecard: daily (org, day, sending_domain, isp)
//     rollups maintained by domainagent.ScorecardWorker (today + 2 prior days
//     rebuilt every 6h). THE source for baselines.
//   - mailing_tracking_events: per-campaign per-event detail for the
//     domain × offer × ISP scorecards (campaign_id indexed; is_machine_open
//     splits human vs machine opens). recipient_domain is COALESCEd with a
//     subscriber-email join because the ingest-time column has gaps on older
//     rows.
//   - mailing_offer_suppressions reason='converted': conversions per offer/day,
//     ISP classified via the subscriber email domain.
//
// Endpoints (mounted under /api/mailing by RegisterRoutes):
//   GET /send-baselines?days=28        — baseline + INCREASE/MAINTAIN/DECREASE verdict per pair
//   GET /send-baselines/today          — today's sends vs baseline (live pace board)
//   GET /send-scorecards?days=14&domain=&offer=&isp= — historical day × domain × offer × ISP rows

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/lib/pq"
)

// SendBaselinesAPI exposes the send-baselines / send-scorecards surface.
type SendBaselinesAPI struct {
	db *sql.DB
}

func NewSendBaselinesAPI(db *sql.DB) *SendBaselinesAPI {
	return &SendBaselinesAPI{db: db}
}

// RegisterRoutes mounts the endpoints on the /api/mailing sub-router.
func (a *SendBaselinesAPI) RegisterRoutes(r chi.Router) {
	r.Get("/send-baselines", a.HandleSendBaselines)
	r.Get("/send-baselines/today", a.HandleSendBaselinesToday)
	r.Get("/send-scorecards", a.HandleSendScorecards)
}

// ── Verdicts ─────────────────────────────────────────────────────────────────

const (
	VerdictIncrease = "INCREASE"
	VerdictMaintain = "MAINTAIN"
	VerdictDecrease = "DECREASE"
)

// Volume-governance thresholds. Rates are fractions (0.02 == 2%).
const (
	hardBounceCeiling    = 0.02   // recent hard rate above this ⇒ DECREASE
	complaintCeiling     = 0.001  // recent complaint rate above this ⇒ DECREASE
	softBounceCeiling    = 0.08   // deferral-ish signal ⇒ DECREASE
	openCollapseFraction = 0.60   // recent human-open rate < 60% of prior ⇒ DECREASE
	hardBounceFloor      = 0.005  // recent hard rate must be below this for INCREASE
	complaintFloor       = 0.0005 // recent complaint rate must be below this for INCREASE
	stableSendsFraction  = 0.80   // recent daily avg ≥ 80% of prior ⇒ "stable/positive"
	recentWindowDays     = 7
)

// SendBaselineWindow aggregates one comparison window (recent 7d vs prior rest).
type SendBaselineWindow struct {
	Sends         int     `json:"sends"`
	Delivered     int     `json:"delivered"`
	HumanOpens    int     `json:"human_opens"`
	HumanClicks   int     `json:"human_clicks"`
	HardBounces   int     `json:"hard_bounces"`
	SoftBounces   int     `json:"soft_bounces"`
	Complaints    int     `json:"complaints"`
	DaysWithSends int     `json:"days_with_sends"`
	DailyAvgSends float64 `json:"daily_avg_sends"`
	HumanOpenRate float64 `json:"human_open_rate"`
	ClickRate     float64 `json:"click_rate"`
	HardRate      float64 `json:"hard_bounce_rate"`
	SoftRate      float64 `json:"soft_bounce_rate"`
	ComplaintRate float64 `json:"complaint_rate"`
}

func (w *SendBaselineWindow) finalize() {
	if w.DaysWithSends > 0 {
		w.DailyAvgSends = round1(float64(w.Sends) / float64(w.DaysWithSends))
	}
	if w.Sends > 0 {
		s := float64(w.Sends)
		w.HumanOpenRate = round4(float64(w.HumanOpens) / s)
		w.ClickRate = round4(float64(w.HumanClicks) / s)
		w.HardRate = round4(float64(w.HardBounces) / s)
		w.SoftRate = round4(float64(w.SoftBounces) / s)
		w.ComplaintRate = round4(float64(w.Complaints) / s)
	}
}

// SendBaselineRow is one (sending_domain × isp) baseline + verdict.
type SendBaselineRow struct {
	SendingDomain      string             `json:"sending_domain"`
	ISP                string             `json:"isp"`
	BaselineDailySends float64            `json:"baseline_daily_sends"`
	WindowDays         int                `json:"window_days"`
	ActiveDays         int                `json:"active_days"`
	Recent             SendBaselineWindow `json:"recent"`
	Prior              SendBaselineWindow `json:"prior"`
	Verdict            string             `json:"verdict"`
	Reasons            []string           `json:"reasons"`
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func pctStr(v float64) string { return fmt.Sprintf("%.2f%%", v*100) }

// verdictFor implements the volume-governance judgement for one pair.
func verdictFor(recent, prior SendBaselineWindow) (string, []string) {
	if recent.Sends == 0 {
		return VerdictMaintain, []string{"no sends in the last 7 days — nothing to judge; maintain plan volume"}
	}

	var decrease []string
	if recent.HardRate > hardBounceCeiling {
		decrease = append(decrease, fmt.Sprintf("hard bounce %s > %s ceiling", pctStr(recent.HardRate), pctStr(hardBounceCeiling)))
	}
	if recent.ComplaintRate > complaintCeiling {
		decrease = append(decrease, fmt.Sprintf("complaint %s > %s ceiling", pctStr(recent.ComplaintRate), pctStr(complaintCeiling)))
	}
	if prior.Sends > 0 && prior.HumanOpenRate > 0 && recent.HumanOpenRate < openCollapseFraction*prior.HumanOpenRate {
		decrease = append(decrease, fmt.Sprintf("human open rate collapsed: recent %s < 60%% of prior %s", pctStr(recent.HumanOpenRate), pctStr(prior.HumanOpenRate)))
	}
	if recent.SoftRate > softBounceCeiling {
		decrease = append(decrease, fmt.Sprintf("soft bounce/deferral %s > %s ceiling", pctStr(recent.SoftRate), pctStr(softBounceCeiling)))
	}
	if len(decrease) > 0 {
		return VerdictDecrease, decrease
	}

	var blockers []string
	if recent.HardRate >= hardBounceFloor {
		blockers = append(blockers, fmt.Sprintf("hard bounce %s not under %s increase floor", pctStr(recent.HardRate), pctStr(hardBounceFloor)))
	}
	if recent.ComplaintRate >= complaintFloor {
		blockers = append(blockers, fmt.Sprintf("complaint %s not under %s increase floor", pctStr(recent.ComplaintRate), pctStr(complaintFloor)))
	}
	if prior.Sends > 0 && recent.HumanOpenRate < prior.HumanOpenRate {
		blockers = append(blockers, fmt.Sprintf("human open rate slipped: recent %s < prior %s", pctStr(recent.HumanOpenRate), pctStr(prior.HumanOpenRate)))
	}
	if prior.DailyAvgSends > 0 && recent.DailyAvgSends < stableSendsFraction*prior.DailyAvgSends {
		blockers = append(blockers, fmt.Sprintf("sends not stable: recent %.0f/day < 80%% of prior %.0f/day", recent.DailyAvgSends, prior.DailyAvgSends))
	}
	if len(blockers) == 0 {
		reasons := []string{
			fmt.Sprintf("hard bounce %s < %s floor", pctStr(recent.HardRate), pctStr(hardBounceFloor)),
			fmt.Sprintf("complaint %s < %s floor", pctStr(recent.ComplaintRate), pctStr(complaintFloor)),
		}
		if prior.Sends > 0 {
			reasons = append(reasons,
				fmt.Sprintf("human open rate held: recent %s >= prior %s", pctStr(recent.HumanOpenRate), pctStr(prior.HumanOpenRate)),
				fmt.Sprintf("sends stable/positive: recent %.0f/day vs prior %.0f/day", recent.DailyAvgSends, prior.DailyAvgSends))
		} else {
			reasons = append(reasons, "no prior-window volume — clean recent window")
		}
		return VerdictIncrease, reasons
	}
	return VerdictMaintain, blockers
}

func verdictRank(v string) int {
	switch v {
	case VerdictDecrease:
		return 0
	case VerdictMaintain:
		return 1
	default:
		return 2
	}
}

// ── Baseline computation ─────────────────────────────────────────────────────

// computeBaselines reads mailing_domain_agent_scorecard for the trailing
// `days` full days (today excluded — partial day skews both the trimmed
// average and the recent-window rates) and produces one row per
// (sending_domain × isp) pair, sorted worst-first.
func (a *SendBaselinesAPI) computeBaselines(ctx context.Context, orgID string, days int) ([]*SendBaselineRow, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT day, sending_domain, isp, sends, delivered,
		       human_opens, human_clicks, hard_bounces, soft_bounces, complaints
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1
		  AND day >= CURRENT_DATE - $2::int
		  AND day <  CURRENT_DATE
		ORDER BY sending_domain, isp, day`, orgID, days)
	if err != nil {
		return nil, fmt.Errorf("baseline query: %w", err)
	}
	defer rows.Close()

	type pairKey struct{ domain, isp string }
	type daily struct {
		day                                                                          time.Time
		sends, delivered, humanOpens, humanClicks, hard, soft, complaints int
	}
	pairs := map[pairKey][]daily{}
	for rows.Next() {
		var d daily
		var k pairKey
		if err := rows.Scan(&d.day, &k.domain, &k.isp, &d.sends, &d.delivered,
			&d.humanOpens, &d.humanClicks, &d.hard, &d.soft, &d.complaints); err != nil {
			return nil, fmt.Errorf("baseline scan: %w", err)
		}
		pairs[k] = append(pairs[k], d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("baseline rows: %w", err)
	}

	recentCutoff := time.Now().UTC().Truncate(24 * time.Hour).AddDate(0, 0, -recentWindowDays)

	out := make([]*SendBaselineRow, 0, len(pairs))
	for k, ds := range pairs {
		row := &SendBaselineRow{SendingDomain: k.domain, ISP: k.isp, WindowDays: days}
		var activeSends []float64
		for _, d := range ds {
			w := &row.Prior
			if !d.day.Before(recentCutoff) {
				w = &row.Recent
			}
			w.Sends += d.sends
			w.Delivered += d.delivered
			w.HumanOpens += d.humanOpens
			w.HumanClicks += d.humanClicks
			w.HardBounces += d.hard
			w.SoftBounces += d.soft
			w.Complaints += d.complaints
			if d.sends > 0 {
				w.DaysWithSends++
				activeSends = append(activeSends, float64(d.sends))
			}
		}
		row.Recent.finalize()
		row.Prior.finalize()
		row.ActiveDays = len(activeSends)
		row.BaselineDailySends = trimmedMean(activeSends)
		row.Verdict, row.Reasons = verdictFor(row.Recent, row.Prior)
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool {
		ri, rj := verdictRank(out[i].Verdict), verdictRank(out[j].Verdict)
		if ri != rj {
			return ri < rj
		}
		if out[i].Recent.Sends != out[j].Recent.Sends {
			return out[i].Recent.Sends > out[j].Recent.Sends
		}
		if out[i].SendingDomain != out[j].SendingDomain {
			return out[i].SendingDomain < out[j].SendingDomain
		}
		return out[i].ISP < out[j].ISP
	})
	return out, nil
}

// trimmedMean: mean of days-with-sends, dropping the single min and max when
// there are at least 5 samples (robust against one blowout / one trickle day).
func trimmedMean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	if len(sorted) >= 5 {
		sorted = sorted[1 : len(sorted)-1]
	}
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return round1(sum / float64(len(sorted)))
}

// ── Endpoint 1: GET /api/mailing/send-baselines ──────────────────────────────

func (a *SendBaselinesAPI) HandleSendBaselines(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	days := parseIntParamBounded(r, "days", 28, 7, 90)

	baselines, err := a.computeBaselines(r.Context(), orgID, days)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	summary := map[string]int{"increase": 0, "maintain": 0, "decrease": 0}
	for _, b := range baselines {
		summary[strings.ToLower(b.Verdict)]++
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"window_days":  days,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"summary":      summary,
		"rows":         baselines,
	})
}

// ── Endpoint 3: GET /api/mailing/send-baselines/today ────────────────────────

// SendPaceRow is one live (sending_domain × isp) pace reading.
type SendPaceRow struct {
	SendingDomain      string  `json:"sending_domain"`
	ISP                string  `json:"isp"`
	TodaySends         int     `json:"today_sends"`
	BaselineDailySends float64 `json:"baseline_daily_sends"`
	PacePct            float64 `json:"pace_pct"` // today / baseline × 100; 0 when no baseline
	Verdict            string  `json:"verdict"`
	Alert              bool    `json:"alert"` // sending ABOVE baseline on a DECREASE verdict
}

func (a *SendBaselinesAPI) HandleSendBaselinesToday(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx := r.Context()

	baselines, err := a.computeBaselines(ctx, orgID, 28)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	type pairKey struct{ domain, isp string }
	baseByPair := make(map[pairKey]*SendBaselineRow, len(baselines))
	for _, b := range baselines {
		baseByPair[pairKey{b.SendingDomain, b.ISP}] = b
	}

	// Today's sends: take the MAX of (a) the scorecard worker's today row
	// (rebuilt every 6h — can lag) and (b) a live count from today's tracking
	// events (org+event_at index; sending_domain stamped at ingest). MAX
	// covers both worker staleness and routes that don't emit 'sent' events.
	today := map[pairKey]int{}
	scRows, err := a.db.QueryContext(ctx, `
		SELECT sending_domain, isp, sends
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND day = CURRENT_DATE`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today scorecard query: " + err.Error()})
		return
	}
	for scRows.Next() {
		var k pairKey
		var sends int
		if err := scRows.Scan(&k.domain, &k.isp, &sends); err != nil {
			scRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today scorecard scan: " + err.Error()})
			return
		}
		if sends > today[k] {
			today[k] = sends
		}
	}
	scErr := scRows.Err()
	scRows.Close()
	if scErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today scorecard rows: " + scErr.Error()})
		return
	}

	liveRows, err := a.db.QueryContext(ctx, `
		SELECT COALESCE(sending_domain, '') AS sdom,
		       COALESCE(LOWER(recipient_domain), '') AS rdom,
		       COUNT(*)
		FROM mailing_tracking_events
		WHERE organization_id = $1
		  AND event_at >= CURRENT_DATE
		  AND event_type = 'sent'
		GROUP BY 1, 2`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today live query: " + err.Error()})
		return
	}
	live := map[pairKey]int{}
	for liveRows.Next() {
		var sdom, rdom string
		var n int
		if err := liveRows.Scan(&sdom, &rdom, &n); err != nil {
			liveRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today live scan: " + err.Error()})
			return
		}
		if sdom == "" {
			continue
		}
		live[pairKey{sdom, isp.GroupFromDomain(rdom)}] += n
	}
	liveErr := liveRows.Err()
	liveRows.Close()
	if liveErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "today live rows: " + liveErr.Error()})
		return
	}
	for k, n := range live {
		if n > today[k] {
			today[k] = n
		}
	}

	// Union of pairs seen today and pairs with a baseline.
	seen := map[pairKey]bool{}
	out := []SendPaceRow{}
	addRow := func(k pairKey) {
		if seen[k] {
			return
		}
		seen[k] = true
		row := SendPaceRow{SendingDomain: k.domain, ISP: k.isp, TodaySends: today[k], Verdict: VerdictMaintain}
		if b, ok := baseByPair[k]; ok {
			row.BaselineDailySends = b.BaselineDailySends
			row.Verdict = b.Verdict
			if b.BaselineDailySends > 0 {
				row.PacePct = round1(float64(row.TodaySends) / b.BaselineDailySends * 100)
			}
		}
		row.Alert = row.Verdict == VerdictDecrease && row.BaselineDailySends > 0 && float64(row.TodaySends) > row.BaselineDailySends
		out = append(out, row)
	}
	for k := range today {
		addRow(k)
	}
	for k := range baseByPair {
		addRow(k)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Alert != out[j].Alert {
			return out[i].Alert
		}
		if out[i].TodaySends != out[j].TodaySends {
			return out[i].TodaySends > out[j].TodaySends
		}
		if out[i].SendingDomain != out[j].SendingDomain {
			return out[i].SendingDomain < out[j].SendingDomain
		}
		return out[i].ISP < out[j].ISP
	})

	now := time.Now().UTC()
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"day":          now.Format("2006-01-02"),
		"day_fraction": round4(float64(now.Hour()*3600+now.Minute()*60+now.Second()) / 86400.0),
		"generated_at": now.Format(time.RFC3339),
		"rows":         out,
	})
}

// ── Endpoint 2: GET /api/mailing/send-scorecards ─────────────────────────────

// SendScorecardRow is one historical day × sending_domain × offer × isp cell.
type SendScorecardRow struct {
	Day           string `json:"day"`
	SendingDomain string `json:"sending_domain"`
	OfferName     string `json:"offer_name"`
	ISP           string `json:"isp"`
	Sent          int    `json:"sent"`
	Delivered     int    `json:"delivered"`
	Opens         int    `json:"opens"`
	HumanOpens    int    `json:"human_opens"`
	Clicks        int    `json:"clicks"`
	HardBounces   int    `json:"hard_bounces"`
	SoftBounces   int    `json:"soft_bounces"`
	Conversions   int    `json:"conversions"`
}

const maxScorecardRows = 5000

func (a *SendBaselinesAPI) HandleSendScorecards(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx := r.Context()
	days := parseIntParamBounded(r, "days", 14, 1, 30)
	filterDomain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	filterOffer := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("offer")))
	filterISP := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("isp")))
	since := time.Now().UTC().AddDate(0, 0, -days).Truncate(24 * time.Hour)

	// Step 1 — bound the work: campaigns created in the window (indexed),
	// mapped to (sending_domain, offer). Sending domain resolution mirrors
	// domainagent: pmta_config wins, then the profile, then from_email.
	campRows, err := a.db.QueryContext(ctx, `
		SELECT c.id::text,
		       COALESCE(c.offer_id::text, ''),
		       COALESCE(NULLIF(o.name, ''), '(no offer)'),
		       LOWER(COALESCE(NULLIF(c.pmta_config->>'sending_domain', ''),
		                      NULLIF(sp.sending_domain, ''),
		                      SPLIT_PART(c.from_email, '@', 2), ''))
		FROM mailing_campaigns c
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.organization_id = $1 AND c.created_at >= $2
		LIMIT 5000`, orgID, since)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard campaigns query: " + err.Error()})
		return
	}
	type campMeta struct{ offerID, offerName, domain string }
	campaigns := map[string]campMeta{}
	offerNames := map[string]string{} // offer_id → name
	for campRows.Next() {
		var id string
		var m campMeta
		if err := campRows.Scan(&id, &m.offerID, &m.offerName, &m.domain); err != nil {
			campRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard campaigns scan: " + err.Error()})
			return
		}
		campaigns[id] = m
		if m.offerID != "" {
			offerNames[m.offerID] = m.offerName
		}
	}
	campErr := campRows.Err()
	campRows.Close()
	if campErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard campaigns rows: " + campErr.Error()})
		return
	}

	type cellKey struct{ day, domain, offer, ispName string }
	cells := map[cellKey]*SendScorecardRow{}
	cell := func(k cellKey) *SendScorecardRow {
		c, ok := cells[k]
		if !ok {
			c = &SendScorecardRow{Day: k.day, SendingDomain: k.domain, OfferName: k.offer, ISP: k.ispName}
			cells[k] = c
		}
		return c
	}

	if len(campaigns) > 0 {
		campaignIDs := make([]string, 0, len(campaigns))
		for id := range campaigns {
			campaignIDs = append(campaignIDs, id)
		}

		// Step 2 — one aggregate pass over tracking events for those
		// campaigns. recipient_domain (stamped at ingest) is coalesced with
		// the subscriber email domain for older rows; human opens =
		// opened AND NOT is_machine_open (idx_mte_machine_open).
		evtRows, err := a.db.QueryContext(ctx, `
			SELECT t.campaign_id::text,
			       DATE(t.event_at),
			       COALESCE(NULLIF(LOWER(t.recipient_domain), ''), LOWER(SPLIT_PART(s.email, '@', 2)), ''),
			       t.event_type,
			       COALESCE(t.is_machine_open, FALSE),
			       (t.event_type = 'bounced' AND `+HardBounceSQL("t")+`),
			       COUNT(*)
			FROM mailing_tracking_events t
			LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
			WHERE t.organization_id = $1
			  AND t.campaign_id = ANY($2::uuid[])
			  AND t.event_at >= $3
			  AND t.event_type IN ('sent','delivered','opened','clicked','bounced')
			GROUP BY 1, 2, 3, 4, 5, 6`, orgID, pq.Array(campaignIDs), since)
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard events query: " + err.Error()})
			return
		}
		for evtRows.Next() {
			var campaignID, rdom, eventType string
			var day time.Time
			var machineOpen, isHard bool
			var n int
			if err := evtRows.Scan(&campaignID, &day, &rdom, &eventType, &machineOpen, &isHard, &n); err != nil {
				evtRows.Close()
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard events scan: " + err.Error()})
				return
			}
			meta, ok := campaigns[campaignID]
			if !ok {
				continue
			}
			c := cell(cellKey{day.Format("2006-01-02"), meta.domain, meta.offerName, isp.GroupFromDomain(rdom)})
			switch eventType {
			case "sent":
				c.Sent += n
			case "delivered":
				c.Delivered += n
			case "opened":
				c.Opens += n
				if !machineOpen {
					c.HumanOpens += n
				}
			case "clicked":
				c.Clicks += n
			case "bounced":
				if isHard {
					c.HardBounces += n
				} else {
					c.SoftBounces += n
				}
			}
		}
		evtErr := evtRows.Err()
		evtRows.Close()
		if evtErr != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard events rows: " + evtErr.Error()})
			return
		}
	}

	// Step 3 — conversions: mailing_offer_suppressions reason='converted' per
	// offer/day, ISP via the subscriber email domain. Conversions are
	// offer-level (no sending_domain dimension), so each (day, offer, isp)
	// count is attributed to the cell with the most sends for that triple;
	// when no send cell matches, a domain-less "—" row is emitted so the
	// conversion is never silently dropped.
	convRows, err := a.db.QueryContext(ctx, `
		SELECT os.offer_id::text,
		       DATE(os.suppressed_at),
		       COALESCE(LOWER(SPLIT_PART(s.email, '@', 2)), ''),
		       COUNT(*)
		FROM mailing_offer_suppressions os
		LEFT JOIN mailing_subscribers s ON s.id = os.subscriber_id
		WHERE os.organization_id = $1
		  AND os.reason = 'converted'
		  AND os.suppressed_at >= $2
		GROUP BY 1, 2, 3`, orgID, since)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard conversions query: " + err.Error()})
		return
	}
	for convRows.Next() {
		var offerID, rdom string
		var day time.Time
		var n int
		if err := convRows.Scan(&offerID, &day, &rdom, &n); err != nil {
			convRows.Close()
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard conversions scan: " + err.Error()})
			return
		}
		offerName, ok := offerNames[offerID]
		if !ok {
			continue // offer had no campaign in the window — out of scope
		}
		dayStr := day.Format("2006-01-02")
		ispName := isp.GroupFromDomain(rdom)
		var best *SendScorecardRow
		for k, c := range cells {
			if k.day == dayStr && k.offer == offerName && k.ispName == ispName {
				if best == nil || c.Sent > best.Sent {
					best = c
				}
			}
		}
		if best == nil {
			best = cell(cellKey{dayStr, "—", offerName, ispName})
		}
		best.Conversions += n
	}
	convErr := convRows.Err()
	convRows.Close()
	if convErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "scorecard conversions rows: " + convErr.Error()})
		return
	}

	// Filters + sort (day desc, sent desc).
	out := make([]*SendScorecardRow, 0, len(cells))
	for _, c := range cells {
		if filterDomain != "" && !strings.Contains(strings.ToLower(c.SendingDomain), filterDomain) {
			continue
		}
		if filterOffer != "" && !strings.Contains(strings.ToLower(c.OfferName), filterOffer) {
			continue
		}
		if filterISP != "" && !strings.EqualFold(c.ISP, filterISP) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		if out[i].Sent != out[j].Sent {
			return out[i].Sent > out[j].Sent
		}
		if out[i].SendingDomain != out[j].SendingDomain {
			return out[i].SendingDomain < out[j].SendingDomain
		}
		if out[i].OfferName != out[j].OfferName {
			return out[i].OfferName < out[j].OfferName
		}
		return out[i].ISP < out[j].ISP
	})
	truncated := false
	if len(out) > maxScorecardRows {
		out = out[:maxScorecardRows]
		truncated = true
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"days":         days,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"total_rows":   len(out),
		"truncated":    truncated,
		"rows":         out,
	})
}

// parseIntParamBounded reads an integer query param with default + clamping.
func parseIntParamBounded(r *http.Request, name string, def, min, max int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

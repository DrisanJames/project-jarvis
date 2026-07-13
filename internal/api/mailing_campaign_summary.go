package api

// Campaign Summary — the fast, accurate replacement read-path for Campaign
// Center (Phase 0 of the analytics rebuild).
//
// Why this exists:
//   - The legacy Campaign Center list pulled full html_content (TOAST payloads)
//     for ~200 campaigns and aggregated metrics client-side, so it loaded
//     slowly, timed out, and frequently rendered zeros.
//   - "Delivered" was inflated for SES-routed mail because PMTA's "d" (relay
//     handoff to SES) was counted as a recipient delivery — the source of the
//     RRU 100% vs SES VDM ~72% discrepancy. That is now fixed upstream
//     (ingest records relayed_to_ses; SES DELIVERY webhook feeds delivered).
//
// This endpoint reads ONLY pre-aggregated counters / tracking rollups (never
// html_content), is process-cache-backed, carries a short server timeout, and
// returns every metric with an explicit denominator + freshness so the
// operator can trust the numbers and see how stale they are.
import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// mustJSON marshals v, returning an error JSON body on failure so the cache
// always stores a valid response.
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return b
}

// VersionCampaignSummary is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-06-04): initial. List from mailing_campaigns counters (no
//	html_content), detail from the terminal-state grain over
//	mailing_tracking_events, reconcile that explains PMTA-relay vs SES-delivery
//	vs system-counter divergence. Explicit denominators + freshness on every
//	metric.
//	1.1 (2026-06-04): detail + reconcile now attach the audience funnel
//	(targeted vs suppressed-by-reason) from mailing_campaign_audience_funnel.
//	1.2 (2026-06-04): list now ROLLS UP partner-drip campaigns. The
//	partner_drip_orchestrator ships hundreds-to-thousands of 15-minute
//	mini-campaigns/day (19,362 of the last 90 days' 23,320 campaigns =
//	83%, collapsing into only 5 partner_drip_tag values). Listed flat they
//	bury every real campaign past the limit. The list now aggregates each
//	partner_drip_tag into ONE synthetic rollup row (is_rollup=true,
//	wave_count=N), keeping real campaigns individual. `partner_tag=<tag>`
//	drills into a tag's underlying waves (flat); `rollup=0` disables
//	rollup entirely (legacy flat list escape hatch).
//	1.4 (2026-06-04): detail splits the bounced bucket THREE ways — TRUE hard
//	(list hygiene) vs reputation_block (sender-side ISP block of a valid
//	recipient: DSN 5.3/5.4/5.5/5.7 + policy/routing/connection categories) vs
//	soft — so a Charter/Spectrum "5.3.2 system not accepting network messages"
//	no longer inflates the hard-bounce rate or implies a list-hygiene problem.
//	Detail also carries content/sender metadata (subject, preheader, from,
//	sending profile/domain, IP pool, vendor, created_at).
const VersionCampaignSummary = "1.4"

const campaignSummaryListTTL = 30 * time.Second
const campaignSummaryHandlerTimeout = 12 * time.Second

// process-local list cache (per ECS task), keyed by limit|status.
var (
	campaignSummaryCacheMu  sync.RWMutex
	campaignSummaryCache    = map[string]campaignSummaryCacheEntry{}
)

type campaignSummaryCacheEntry struct {
	body     []byte
	storedAt time.Time
}

// rate returns numerator/denominator*100 rounded to 2dp, 0 when denom is 0.
func summaryRate(num, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	return math.Round(float64(num)/float64(denom)*10000) / 100
}

// deliveryDenominator returns a reliable denominator for the delivery and
// bounce/complaint rates. mailing_campaigns.sent_count UNDER-reports for
// SES-routed sends (the SES path does not reliably increment it), so a naive
// delivered/sent can exceed 100% — observed on partner-drip rollups where
// delivered (SES DELIVERY events) outran the injection counter. We fall back to
// the realized-disposition total (delivered + hard + soft bounces) whenever it
// exceeds sent. For accurate PMTA-direct campaigns sent already includes
// deferred/unresolved attempts and is >= the realized total, so this is a no-op
// there; it only repairs the under-counted SES case. NOTE: this is the display
// fix; the root-cause sent_count accounting fix for SES sends is a scheduled
// follow-up.
func deliveryDenominator(sent, delivered, hardBounce, softBounce int) int {
	processed := delivered + hardBounce + softBounce
	if sent > processed {
		return sent
	}
	return processed
}

// campaignSummaryRow is one campaign's headline numbers for the list view.
// Every rate names its denominator so the UI can label it unambiguously.
type campaignSummaryRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ScheduledAt   *time.Time `json:"scheduled_at,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	RouteType     string `json:"route_type"` // ses | pmta | unknown

	// Rollup markers. When IsRollup is true this row aggregates WaveCount
	// partner-drip mini-campaigns that share PartnerTag — the id is the
	// synthetic "drip-rollup:<tag>" (NOT a campaign uuid), so the UI must
	// drill (partner_tag=<tag>) rather than fetch a per-campaign detail.
	IsRollup   bool   `json:"is_rollup,omitempty"`
	WaveCount  int    `json:"wave_count,omitempty"`
	PartnerTag string `json:"partner_tag,omitempty"`

	// Funnel counts (numerators).
	Targeted   int `json:"targeted"`   // planned audience (total_recipients)
	Sent       int `json:"sent"`       // attempted/injected (sent_count)
	Delivered  int `json:"delivered"`  // delivered_count (SES DELIVERY for SES, PMTA d for direct)
	HardBounce int `json:"hard_bounce"`
	SoftBounce int `json:"soft_bounce"`
	Complaints int `json:"complaints"`
	Opens      int `json:"opens"`
	Clicks     int `json:"clicks"`
	Unsubs     int `json:"unsubscribes"`

	// Rates with explicit denominators. delivery/bounce/complaint rates use the
	// processed-aware denominator max(sent, delivered+hard+soft) so SES-routed
	// rows whose sent_count under-reports no longer exceed 100%.
	DeliveryRate   float64 `json:"delivery_rate"`    // delivered / max(sent, delivered+hard+soft)
	HardBounceRate float64 `json:"hard_bounce_rate"` // hard / max(sent, delivered+hard+soft)
	SoftBounceRate float64 `json:"soft_bounce_rate"` // soft / max(sent, delivered+hard+soft)
	ComplaintRate  float64 `json:"complaint_rate"`   // complaints / max(sent, delivered+hard+soft)
	OpenRate       float64 `json:"open_rate"`        // opens / delivered
	ClickRate      float64 `json:"click_rate"`       // clicks / delivered
	CTOR           float64 `json:"ctor"`             // clicks / opens
}

// HandleCampaignSummaryList returns lightweight, accurate per-campaign metrics
// for the most recent campaigns. Fast: reads pre-aggregated counters only.
// Query params: limit (default 50, max 200), status (optional filter).
func (s *AdvancedMailingService) HandleCampaignSummaryList(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	// rollup defaults ON. partner_drip campaigns (19k+ of the last 90 days'
	// 23k) otherwise flood the list and bury every real campaign. rollup=0
	// is the legacy flat-list escape hatch.
	rollup := true
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("rollup"))); v == "0" || v == "false" || v == "no" {
		rollup = false
	}
	// partner_tag drills into one tag's underlying waves (flat, real uuids).
	partnerTag := strings.TrimSpace(r.URL.Query().Get("partner_tag"))
	cacheKey := fmt.Sprintf("%d|%s|rollup=%t|tag=%s", limit, status, rollup, partnerTag)

	campaignSummaryCacheMu.RLock()
	if e, ok := campaignSummaryCache[cacheKey]; ok && time.Since(e.storedAt) < campaignSummaryListTTL {
		body := e.body
		age := time.Since(e.storedAt)
		campaignSummaryCacheMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-Cache-Age-Seconds", fmt.Sprintf("%.1f", age.Seconds()))
		_, _ = w.Write(body)
		return
	}
	campaignSummaryCacheMu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), campaignSummaryHandlerTimeout)
	defer cancel()

	// Build the WHERE for the per-campaign (individual-row) query.
	//   - default rollup mode: exclude partner-drip campaigns (they are
	//     aggregated separately below).
	//   - drill mode (partner_tag set): show ONLY that tag's waves, flat.
	//   - legacy flat mode (rollup=0): no partner filter.
	args := []interface{}{}
	where := "WHERE c.created_at > NOW() - INTERVAL '90 days'"
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND c.status = $%d", len(args))
	}
	if partnerTag != "" {
		args = append(args, partnerTag)
		where += fmt.Sprintf(" AND c.partner_drip_tag = $%d", len(args))
	} else if rollup {
		where += " AND c.partner_drip_tag IS NULL"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT c.id::text, COALESCE(c.name,''), COALESCE(c.status,''),
		       c.scheduled_at, c.updated_at,
		       COALESCE(c.total_recipients,0), COALESCE(c.sent_count,0),
		       COALESCE(c.delivered_count,0), COALESCE(c.hard_bounce_count,0),
		       COALESCE(c.soft_bounce_count,0), COALESCE(c.complaint_count,0),
		       COALESCE(c.open_count,0), COALESCE(c.click_count,0),
		       COALESCE(c.unsubscribe_count,0),
		       COALESCE(sp.via_ses, FALSE)
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		%s
		ORDER BY COALESCE(c.scheduled_at, c.created_at) DESC
		LIMIT $%d
	`, where, len(args))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		writeJSONResponse(w, map[string]interface{}{
			"api_version": VersionCampaignSummary,
			"error":       fmt.Sprintf("campaign-summary list query: %v", err),
			"campaigns":   []campaignSummaryRow{},
		})
		return
	}
	defer rows.Close()

	out := []campaignSummaryRow{}
	var dataAsOf time.Time
	for rows.Next() {
		var row campaignSummaryRow
		var sched, upd sql.NullTime
		var viaSES bool
		if err := rows.Scan(&row.ID, &row.Name, &row.Status, &sched, &upd,
			&row.Targeted, &row.Sent, &row.Delivered, &row.HardBounce,
			&row.SoftBounce, &row.Complaints, &row.Opens, &row.Clicks,
			&row.Unsubs, &viaSES); err != nil {
			continue
		}
		if sched.Valid {
			t := sched.Time.UTC()
			row.ScheduledAt = &t
		}
		if upd.Valid {
			t := upd.Time.UTC()
			row.UpdatedAt = &t
			if t.After(dataAsOf) {
				dataAsOf = t
			}
		}
		if viaSES {
			row.RouteType = "ses"
		} else {
			row.RouteType = "pmta"
		}
		fillSummaryRates(&row)
		out = append(out, row)
	}

	// Append partner-drip rollups (only in default rollup mode — not when
	// drilling into a single tag's waves, and not in legacy flat mode).
	if rollup && partnerTag == "" {
		rollups, rollupAsOf, rerr := s.partnerDripRollups(ctx, status)
		if rerr != nil {
			// Non-fatal: log and serve the real campaigns without rollups
			// rather than failing the whole list.
			log.Printf("[campaign-summary] partner-drip rollup query: %v", rerr)
		} else {
			out = append(out, rollups...)
			if rollupAsOf.After(dataAsOf) {
				dataAsOf = rollupAsOf
			}
			// Re-sort merged set newest-first so rollups interleave by
			// last activity rather than always trailing real campaigns.
			sort.SliceStable(out, func(i, j int) bool {
				return summaryRowActivity(out[i]).After(summaryRowActivity(out[j]))
			})
		}
	}

	notes := "delivered = SES DELIVERY for SES-routed campaigns (route_type=ses), PMTA delivery for direct (route_type=pmta); PMTA relay-to-SES handoffs are NOT counted as delivered."
	if rollup && partnerTag == "" {
		notes += " Partner-drip mini-campaigns are rolled up by partner feed (is_rollup=true, wave_count=N); open partner_tag to drill into the underlying waves. For SES-routed partner-drip rollups sent_count (an injection counter) under-reports, so delivery/bounce rates use a processed-aware denominator max(sent, delivered+hard+soft) to stay <=100% until the sent_count accounting fix ships."
	}
	resp := map[string]interface{}{
		"api_version": VersionCampaignSummary,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"data_as_of":   nullableTimeStr(dataAsOf),
		"count":        len(out),
		"campaigns":    out,
		"rolled_up":    rollup && partnerTag == "",
		"partner_tag":  partnerTag,
		"denominators": campaignSummaryDenominators(),
		"notes":        notes,
	}
	body := mustJSON(resp)

	campaignSummaryCacheMu.Lock()
	campaignSummaryCache[cacheKey] = campaignSummaryCacheEntry{body: body, storedAt: time.Now()}
	campaignSummaryCacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	_, _ = w.Write(body)
}

// fillSummaryRates computes every rate on a row from its raw counts so the
// numerator/denominator pairing is identical for individual rows and rollups.
func fillSummaryRates(row *campaignSummaryRow) {
	denom := deliveryDenominator(row.Sent, row.Delivered, row.HardBounce, row.SoftBounce)
	row.DeliveryRate = summaryRate(row.Delivered, denom)
	row.HardBounceRate = summaryRate(row.HardBounce, denom)
	row.SoftBounceRate = summaryRate(row.SoftBounce, denom)
	row.ComplaintRate = summaryRate(row.Complaints, denom)
	row.OpenRate = summaryRate(row.Opens, row.Delivered)
	row.ClickRate = summaryRate(row.Clicks, row.Delivered)
	row.CTOR = summaryRate(row.Clicks, row.Opens)
}

// summaryRowActivity is the sort key used to interleave rollups with real
// campaigns: most-recent scheduled time, else updated time, else zero.
func summaryRowActivity(row campaignSummaryRow) time.Time {
	if row.ScheduledAt != nil {
		return *row.ScheduledAt
	}
	if row.UpdatedAt != nil {
		return *row.UpdatedAt
	}
	return time.Time{}
}

// partnerDripRollupLabel turns a partner_drip_tag ("data_partner:<slug>/<vertical>")
// into a human label ("Partner Drip · <slug> · <vertical>").
func partnerDripRollupLabel(tag string) string {
	rest := strings.TrimPrefix(tag, "data_partner:")
	rest = strings.ReplaceAll(rest, "/", " · ")
	if rest == "" {
		return "Partner Drip"
	}
	return "Partner Drip · " + rest
}

// partnerDripRollups aggregates every partner-drip campaign (partner_drip_tag
// IS NOT NULL) in the 90-day window into one synthetic row per tag. Counts are
// summed from the campaign aggregate counters (the same columns the individual
// rows read), so a rollup is the exact sum of its waves. The synthetic id
// ("drip-rollup:<tag>") is intentionally not a uuid — the UI drills via
// partner_tag rather than fetching a per-campaign detail.
func (s *AdvancedMailingService) partnerDripRollups(ctx context.Context, status string) ([]campaignSummaryRow, time.Time, error) {
	args := []interface{}{}
	where := "WHERE c.partner_drip_tag IS NOT NULL AND c.created_at > NOW() - INTERVAL '90 days'"
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND c.status = $%d", len(args))
	}
	q := fmt.Sprintf(`
		SELECT c.partner_drip_tag,
		       COUNT(*) AS waves,
		       (array_agg(COALESCE(c.status,'') ORDER BY COALESCE(c.scheduled_at, c.created_at) DESC))[1] AS latest_status,
		       MAX(c.scheduled_at) AS last_scheduled,
		       MAX(c.updated_at) AS last_updated,
		       SUM(COALESCE(c.total_recipients,0)), SUM(COALESCE(c.sent_count,0)),
		       SUM(COALESCE(c.delivered_count,0)), SUM(COALESCE(c.hard_bounce_count,0)),
		       SUM(COALESCE(c.soft_bounce_count,0)), SUM(COALESCE(c.complaint_count,0)),
		       SUM(COALESCE(c.open_count,0)), SUM(COALESCE(c.click_count,0)),
		       SUM(COALESCE(c.unsubscribe_count,0)),
		       BOOL_OR(COALESCE(sp.via_ses, FALSE))
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		%s
		GROUP BY c.partner_drip_tag
		ORDER BY last_scheduled DESC NULLS LAST
	`, where)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	out := []campaignSummaryRow{}
	var dataAsOf time.Time
	for rows.Next() {
		var (
			tag, latestStatus string
			waves             int
			sched, upd        sql.NullTime
			viaSES            bool
			row               campaignSummaryRow
		)
		if err := rows.Scan(&tag, &waves, &latestStatus, &sched, &upd,
			&row.Targeted, &row.Sent, &row.Delivered, &row.HardBounce,
			&row.SoftBounce, &row.Complaints, &row.Opens, &row.Clicks,
			&row.Unsubs, &viaSES); err != nil {
			continue
		}
		row.ID = "drip-rollup:" + tag
		row.PartnerTag = tag
		row.Name = partnerDripRollupLabel(tag)
		row.Status = latestStatus
		row.IsRollup = true
		row.WaveCount = waves
		if sched.Valid {
			t := sched.Time.UTC()
			row.ScheduledAt = &t
		}
		if upd.Valid {
			t := upd.Time.UTC()
			row.UpdatedAt = &t
			if t.After(dataAsOf) {
				dataAsOf = t
			}
		}
		if viaSES {
			row.RouteType = "ses"
		} else {
			row.RouteType = "pmta"
		}
		fillSummaryRates(&row)
		out = append(out, row)
	}
	return out, dataAsOf, rows.Err()
}

// campaignSummaryDetail is the per-campaign deep view built from the terminal
// state grain plus engagement uniques.
type campaignSummaryDetail struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	RouteType   string     `json:"route_type"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`

	// Content + sender metadata so the operator can see exactly what shipped
	// and from where without opening another panel.
	Subject        string `json:"subject,omitempty"`
	Preheader      string `json:"preheader,omitempty"`
	FromName       string `json:"from_name,omitempty"`
	FromEmail      string `json:"from_email,omitempty"`
	CampaignType   string `json:"campaign_type,omitempty"`
	SendingProfile string `json:"sending_profile,omitempty"`
	SendingDomain  string `json:"sending_domain,omitempty"`
	IPPool         string `json:"ip_pool,omitempty"`
	Vendor         string `json:"vendor,omitempty"`

	// Funnel (each is a count; denominators documented in `denominators`).
	Targeted     int `json:"targeted"`      // planned audience (total_recipients)
	Sent         int `json:"sent"`          // attempted/injected (sent_count)
	RelayedToSES int `json:"relayed_to_ses"` // PMTA handoffs to SES (not delivered)
	Recipients   int `json:"recipients"`    // distinct (campaign,subscriber) with a tail event
	Delivered    int `json:"delivered"`     // terminal delivered (delivered wins)
	HardBounce   int `json:"hard_bounce"`   // TRUE list hygiene: invalid address / dead domain / disabled mailbox
	ReputationBlock int `json:"reputation_block"` // sender-side block of a VALID recipient (5.3/5.4/5.5/5.7, policy/routing). NOT list hygiene.
	SoftBounce   int `json:"soft_bounce"`
	DeferredOpen int `json:"deferred_open"` // only deferral events, no terminal
	SentOnly     int `json:"sent_only"`     // sent, no tail event yet
	UniqueOpens  int `json:"unique_opens"`
	TotalOpens   int `json:"total_opens"`
	UniqueClicks int `json:"unique_clicks"`
	TotalClicks  int `json:"total_clicks"`

	DeliveryRate        float64 `json:"delivery_rate"`         // delivered / processed
	HardBounceRate      float64 `json:"hard_bounce_rate"`      // hard / processed
	ReputationBlockRate float64 `json:"reputation_block_rate"` // reputation_block / processed
	SoftBounceRate      float64 `json:"soft_bounce_rate"`      // soft / processed
	OpenRate            float64 `json:"open_rate"`             // unique_opens / delivered
	ClickRate      float64 `json:"click_rate"`       // unique_clicks / delivered
	CTOR           float64 `json:"ctor"`             // unique_clicks / unique_opens

	MetricsSource string     `json:"metrics_source"` // tracking_events
	DataAsOf      string     `json:"data_as_of"`     // max event_at seen
	GeneratedAt   string     `json:"generated_at"`
}

// HandleCampaignSummaryByID returns the terminal-state detail for one campaign.
func (s *AdvancedMailingService) HandleCampaignSummaryByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		http.Error(w, `{"error":"missing campaign id"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), campaignSummaryHandlerTimeout)
	defer cancel()

	detail, err := s.computeCampaignSummaryDetail(ctx, id)
	if err != nil {
		writeJSONResponse(w, map[string]interface{}{
			"api_version": VersionCampaignSummary,
			"error":       err.Error(),
		})
		return
	}
	writeJSONResponse(w, map[string]interface{}{
		"api_version":  VersionCampaignSummary,
		"campaign":     detail,
		"audience_funnel": s.loadAudienceFunnel(ctx, id),
		"denominators": campaignSummaryDenominators(),
	})
}

func (s *AdvancedMailingService) computeCampaignSummaryDetail(ctx context.Context, id string) (*campaignSummaryDetail, error) {
	d := &campaignSummaryDetail{ID: id, MetricsSource: "tracking_events", GeneratedAt: time.Now().UTC().Format(time.RFC3339)}

	var sched, created sql.NullTime
	var viaSES sql.NullBool
	var subject, preheader, fromName, fromEmail, campaignType sql.NullString
	var profileName, sendingDomain, ipPool, vendor sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.name,''), COALESCE(c.status,''), c.scheduled_at, c.created_at,
		       COALESCE(c.total_recipients,0), COALESCE(c.sent_count,0),
		       c.subject, c.preview_text, c.from_name, c.from_email, c.campaign_type,
		       sp.name, sp.sending_domain, sp.ip_pool, sp.vendor_type, sp.via_ses
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.id = $1::uuid
	`, id).Scan(&d.Name, &d.Status, &sched, &created, &d.Targeted, &d.Sent,
		&subject, &preheader, &fromName, &fromEmail, &campaignType,
		&profileName, &sendingDomain, &ipPool, &vendor, &viaSES)
	if err != nil {
		return nil, fmt.Errorf("campaign lookup: %w", err)
	}
	if sched.Valid {
		t := sched.Time.UTC()
		d.ScheduledAt = &t
	}
	if created.Valid {
		t := created.Time.UTC()
		d.CreatedAt = &t
	}
	d.Subject = subject.String
	d.Preheader = preheader.String
	d.FromName = fromName.String
	d.FromEmail = fromEmail.String
	d.CampaignType = campaignType.String
	d.SendingProfile = profileName.String
	d.SendingDomain = sendingDomain.String
	d.IPPool = ipPool.String
	d.Vendor = vendor.String
	if viaSES.Valid && viaSES.Bool {
		d.RouteType = "ses"
	} else {
		d.RouteType = "pmta"
	}

	// Terminal-state grain over the campaign's tracking events. The bounced
	// bucket is split three ways using the authoritative DSN status (in
	// bounce_reason) so the operator sees TRUE list-hygiene hard bounces apart
	// from reputation/system blocks (e.g. Charter/Spectrum "5.3.2 system not
	// accepting network messages", which PMTA mislabels bad-mailbox). A
	// recipient is hard only if it has a genuine hard event and NO block event,
	// so a single valid address blocked by an ISP is never counted as hard.
	hb := HardBounceSQL("t")
	rb := ReputationBlockSQL("t")
	tq := fmt.Sprintf(`
		WITH per_recipient AS (
			SELECT t.subscriber_id,
				BOOL_OR(t.event_type = 'delivered')                       AS has_delivered,
				BOOL_OR(t.event_type = 'bounced' AND (%s) AND NOT (%s))   AS has_hard,
				BOOL_OR(t.event_type = 'bounced' AND (%s))                AS has_block,
				BOOL_OR(t.event_type = 'bounced' AND NOT (%s) AND NOT (%s)) AS has_soft,
				BOOL_OR(t.event_type IN ('deferred','deferral'))         AS has_deferral,
				BOOL_OR(t.event_type = 'sent')                           AS has_sent
			FROM mailing_tracking_events t
			WHERE t.campaign_id = $1::uuid
			  AND t.subscriber_id IS NOT NULL
			  AND t.event_type IN ('sent','delivered','bounced','deferred','deferral')
			GROUP BY t.subscriber_id
		)
		SELECT COUNT(*),
			COUNT(*) FILTER (WHERE has_delivered),
			COUNT(*) FILTER (WHERE NOT has_delivered AND has_hard),
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard AND has_block),
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard AND NOT has_block AND has_soft),
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard AND NOT has_block AND NOT has_soft AND has_deferral),
			COUNT(*) FILTER (WHERE NOT has_delivered AND NOT has_hard AND NOT has_block AND NOT has_soft AND NOT has_deferral AND has_sent)
		FROM per_recipient
	`, hb, rb, rb, hb, rb)
	if err := s.db.QueryRowContext(ctx, tq, id).Scan(
		&d.Recipients, &d.Delivered, &d.HardBounce, &d.ReputationBlock, &d.SoftBounce, &d.DeferredOpen, &d.SentOnly,
	); err != nil {
		return nil, fmt.Errorf("terminal-state query: %w", err)
	}

	// Engagement uniques + relayed_to_ses + freshness in one scan.
	var dataAsOf sql.NullTime
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened'),
			COUNT(*)                      FILTER (WHERE event_type = 'opened'),
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'clicked'),
			COUNT(*)                      FILTER (WHERE event_type = 'clicked'),
			COUNT(*)                      FILTER (WHERE event_type = 'relayed_to_ses'),
			MAX(event_at)
		FROM mailing_tracking_events
		WHERE campaign_id = $1::uuid
	`, id).Scan(&d.UniqueOpens, &d.TotalOpens, &d.UniqueClicks, &d.TotalClicks, &d.RelayedToSES, &dataAsOf); err != nil {
		return nil, fmt.Errorf("engagement query: %w", err)
	}
	if dataAsOf.Valid {
		d.DataAsOf = dataAsOf.Time.UTC().Format(time.RFC3339)
	}

	// processed includes reputation blocks so every realized disposition shares
	// one denominator and the rates sum coherently.
	denom := deliveryDenominator(d.Sent, d.Delivered, d.HardBounce+d.ReputationBlock, d.SoftBounce)
	d.DeliveryRate = summaryRate(d.Delivered, denom)
	d.HardBounceRate = summaryRate(d.HardBounce, denom)
	d.ReputationBlockRate = summaryRate(d.ReputationBlock, denom)
	d.SoftBounceRate = summaryRate(d.SoftBounce, denom)
	d.OpenRate = summaryRate(d.UniqueOpens, d.Delivered)
	d.ClickRate = summaryRate(d.UniqueClicks, d.Delivered)
	d.CTOR = summaryRate(d.UniqueClicks, d.UniqueOpens)
	return d, nil
}

// HandleCampaignSummaryReconcile explains the gap between what the system
// reports and the underlying signals — directly answering "why does RRU show
// 100% delivered when SES VDM shows ~72%?". It compares, for one campaign:
//   - sent (attempted/injected)
//   - PMTA relayed_to_ses (handoffs to SES)
//   - PMTA direct delivered (non-SES pool deliveries)
//   - SES delivered (authoritative for SES-routed mail)
//   - system delivered_count
func (s *AdvancedMailingService) HandleCampaignSummaryReconcile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		http.Error(w, `{"error":"missing campaign id"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), campaignSummaryHandlerTimeout)
	defer cancel()

	detail, err := s.computeCampaignSummaryDetail(ctx, id)
	if err != nil {
		writeJSONResponse(w, map[string]interface{}{"api_version": VersionCampaignSummary, "error": err.Error()})
		return
	}

	// PMTA accounting rollup (delivered = direct MX delivery; relayed_to_ses
	// = handoffs to SES; these are now separated by the summary builder).
	var pmtaDelivered, pmtaRelayed, pmtaHard, pmtaSoft int
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(delivered),0), COALESCE(SUM(relayed_to_ses),0),
		       COALESCE(SUM(hard_bounced),0), COALESCE(SUM(soft_bounced),0)
		FROM pmta_acct_daily_summary WHERE campaign_id = $1::uuid
	`, id).Scan(&pmtaDelivered, &pmtaRelayed, &pmtaHard, &pmtaSoft)

	// SES DELIVERY events persisted from the webhook (authoritative for SES).
	var sesDelivered, sesBounced int
	s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE event_type = 'delivered'),
			COUNT(*) FILTER (WHERE event_type = 'bounced')
		FROM mailing_tracking_events
		WHERE campaign_id = $1::uuid
		  AND event_at > NOW() - INTERVAL '60 days'
	`, id).Scan(&sesDelivered, &sesBounced)

	// system counter
	var systemDelivered int
	s.db.QueryRowContext(ctx, `SELECT COALESCE(delivered_count,0) FROM mailing_campaigns WHERE id = $1::uuid`, id).Scan(&systemDelivered)

	explanation := campaignReconcileExplanation(detail.RouteType, detail.Sent, pmtaRelayed, pmtaDelivered, sesDelivered, systemDelivered)

	writeJSONResponse(w, map[string]interface{}{
		"api_version":     VersionCampaignSummary,
		"campaign":        detail,
		"audience_funnel": s.loadAudienceFunnel(ctx, id),
		"reconciliation": map[string]interface{}{
			"route_type":             detail.RouteType,
			"sent_attempted":         detail.Sent,
			"pmta_relayed_to_ses":    pmtaRelayed,
			"pmta_direct_delivered":  pmtaDelivered,
			"ses_delivered":          sesDelivered,
			"tracking_terminal_delivered": detail.Delivered,
			"system_delivered_count": systemDelivered,
			"explanation":            explanation,
		},
		"note": "SES DELIVERY = accepted by recipient mail system (NOT inbox placement). Inbox placement requires VDM / seed / panel data.",
	})
}

// campaignReconcileExplanation produces a human-readable diagnosis string.
func campaignReconcileExplanation(route string, sent, relayed, pmtaDelivered, sesDelivered, systemDelivered int) string {
	if route == "ses" {
		if sesDelivered == 0 && relayed > 0 {
			return fmt.Sprintf("SES-routed: PMTA relayed %d messages to SES but no SES DELIVERY events have been persisted yet — delivered will populate as SES publishes events. PMTA relay success is NOT delivery, which is exactly why the old view over-reported.", relayed)
		}
		gap := relayed - sesDelivered
		return fmt.Sprintf("SES-routed: PMTA relayed %d to SES; SES confirms %d delivered (gap %d = bounces/deferrals/pending at the recipient mail system). Delivery rate is computed off SES truth, not PMTA relay success.", relayed, sesDelivered, gap)
	}
	return fmt.Sprintf("PMTA-direct: %d delivered to recipient MX out of %d attempted. No SES relay leg for this campaign.", pmtaDelivered, sent)
}

func campaignSummaryDenominators() map[string]string {
	return map[string]string{
		"delivery_rate":         "delivered / max(sent, delivered+hard_bounce+reputation_block+soft_bounce)",
		"hard_bounce_rate":      "hard_bounce / processed (TRUE list hygiene: invalid address / dead domain / disabled mailbox only)",
		"reputation_block_rate": "reputation_block / processed (sender-side ISP block of a VALID recipient: DSN 5.3/5.4/5.5/5.7, policy/routing/connection — recoverable, NOT list hygiene)",
		"soft_bounce_rate":      "soft_bounce / processed",
		"complaint_rate":        "complaints / processed",
		"open_rate":        "unique_opens / delivered",
		"click_rate":       "unique_clicks / delivered",
		"ctor":             "unique_clicks / unique_opens",
		"targeted":         "planned audience (mailing_campaigns.total_recipients)",
		"sent":             "attempted/injected (mailing_campaigns.sent_count)",
		"delivered":        "SES DELIVERY for route_type=ses; PMTA direct delivery for route_type=pmta; PMTA relay-to-SES excluded",
	}
}

func nullableTimeStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// persistAudienceFunnel writes one aggregate row capturing the campaign's
// audience funnel: total candidates seen, total targeted (selected), total
// suppressed, and a per-reason breakdown. This is the hot-DB-safe form of the
// recipient_targeted / recipient_suppressed{reason} signal — one row per
// campaign instead of hundreds of thousands of per-recipient events (which
// belong in the Phase 1 S3 event lake). Idempotent on campaign_id.
func persistAudienceFunnel(ctx context.Context, db *sql.DB, campaignID, orgID string, audience pmtaAudiencePlan) {
	if db == nil || campaignID == "" {
		return
	}
	suppressedTotal := 0
	for _, n := range audience.SuppressionReasons {
		suppressedTotal += n
	}
	reasonsJSON := mustJSON(audience.SuppressionReasons)
	// plan_warnings (REQ-043): structured per-segment warnings. A nil slice
	// must serialize as '[]' (not 'null') so JSONB readers can iterate
	// unconditionally. The upsert REPLACES the set on every finalize —
	// a re-fired finalization can never duplicate warnings.
	warns := audience.PlanWarnings
	if warns == nil {
		warns = []pmtaPlanWarning{}
	}
	warningsJSON := mustJSON(warns)

	var orgParam interface{}
	if strings.TrimSpace(orgID) != "" {
		orgParam = orgID
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_audience_funnel
			(campaign_id, organization_id, total_seen, targeted, reserve, suppressed_total, reason_breakdown, plan_warnings, computed_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::jsonb, $8::jsonb, NOW())
		ON CONFLICT (campaign_id) DO UPDATE SET
			total_seen = EXCLUDED.total_seen,
			targeted = EXCLUDED.targeted,
			reserve = EXCLUDED.reserve,
			suppressed_total = EXCLUDED.suppressed_total,
			reason_breakdown = EXCLUDED.reason_breakdown,
			plan_warnings = EXCLUDED.plan_warnings,
			computed_at = NOW()
	`, campaignID, orgParam, audience.TotalSeen, audience.SelectedTotal,
		audience.ReserveTotal, suppressedTotal, string(reasonsJSON), string(warningsJSON))
	if err != nil {
		log.Printf("[audience-funnel] persist campaign=%s error: %v", campaignID, err)
	}
}

// campaignAudienceFunnel is the funnel slice attached to the campaign-summary
// detail when a finalized row exists.
type campaignAudienceFunnel struct {
	TotalSeen       int            `json:"total_seen"`
	Targeted        int            `json:"targeted"`
	Reserve         int            `json:"reserve"`
	SuppressedTotal int            `json:"suppressed_total"`
	ReasonBreakdown map[string]int `json:"reason_breakdown"`
	// PlanWarnings surfaces the planner's structured warnings (REQ-043),
	// e.g. "segment X contributed 0 (never built)" — the Draft Board / QA
	// sweep signal for silent per-lane collapse.
	PlanWarnings []pmtaPlanWarning `json:"plan_warnings"`
	ComputedAt   string            `json:"computed_at"`
}

// loadAudienceFunnel returns the funnel for a campaign, or nil if none recorded.
func (s *AdvancedMailingService) loadAudienceFunnel(ctx context.Context, id string) *campaignAudienceFunnel {
	var f campaignAudienceFunnel
	var reasonsRaw, warningsRaw []byte
	var computed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT total_seen, targeted, reserve, suppressed_total, COALESCE(reason_breakdown,'{}'::jsonb), COALESCE(plan_warnings,'[]'::jsonb), computed_at
		FROM mailing_campaign_audience_funnel WHERE campaign_id = $1::uuid
	`, id).Scan(&f.TotalSeen, &f.Targeted, &f.Reserve, &f.SuppressedTotal, &reasonsRaw, &warningsRaw, &computed)
	if err != nil {
		return nil
	}
	f.ReasonBreakdown = map[string]int{}
	_ = json.Unmarshal(reasonsRaw, &f.ReasonBreakdown)
	f.PlanWarnings = []pmtaPlanWarning{}
	_ = json.Unmarshal(warningsRaw, &f.PlanWarnings)
	if computed.Valid {
		f.ComputedAt = computed.Time.UTC().Format(time.RFC3339)
	}
	return &f
}

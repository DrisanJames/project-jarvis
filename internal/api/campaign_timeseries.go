package api

// Campaign Center inline analytics (Round-3 requirement §1) — the read
// endpoints behind the list-view rebuild:
//
//   GET /api/mailing/campaigns/{id}/timeseries?bucket=1m|15m
//       Time-bucketed send-pace series (attempted / delivered / human opens /
//       clicks / unsubscribes / bounces) over the campaign's send window,
//       plus wave-start markers from mailing_campaign_waves.
//
//   GET /api/mailing/campaigns/{id}/detail
//       The inline-expand payload: KPI strip (tracking-event derived, hard vs
//       soft ALWAYS split), per-ISP-family mini table, top 5 clicked links,
//       content/sender/segment/throttle facts, wave list, and conversions
//       (mailing_offer_suppressions reason='converted') when the campaign's
//       offer is resolvable.
//
//   GET /api/mailing/campaigns/list-metrics?ids=...&tags=...&from=...&to=...
//       ONE grouped query per visible list page (≤50 campaign ids) returning
//       per-campaign tracking-event metrics + identity extras (subject,
//       sending domain, planned, wave window) the summary list lacks. `tags`
//       returns the same aggregates rolled up per partner_drip_tag for the
//       drip-visibility toggle.
//
// Doctrine:
//   - Metrics come from mailing_tracking_events ONLY — campaign counters
//     (sent_count/open_count/...) are NEVER read here (known staleness).
//   - Human opens = event_type='opened' AND NOT is_machine_open.
//   - Hard vs soft bounces split via the canonical HardBounceSQL helper.
//   - Every event query is bounded by campaign_id + an event_at window.
//   - Org-scoped via getOrgID(r); campaign ownership verified before any
//     event scan.
//   - v1 reads Postgres only (source="pg"). SES-routed campaigns have sparse
//     PG events (SES strips pixels / events arrive via webhook), so SES rows
//     carry an explicit note; lake (Athena) wiring is a follow-up.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lib/pq"
)

// VersionCampaignTimeseries is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-06-12): initial — timeseries + detail + list-metrics for the
//	Campaign Center list rebuild (Round-3 §1).
const VersionCampaignTimeseries = "1.0"

const (
	campaignTSHandlerTimeout = 15 * time.Second
	campaignTSMaxListIDs     = 50
	campaignTSMaxTags        = 25
	campaignTSMaxBuckets     = 2200 // hard cap on zero-filled series length
	// Engagement (opens/clicks) trails the send window; the detail KPI scan
	// extends this far past the window end (bounded, never unbounded).
	campaignTSEngagementTail = 14 * 24 * time.Hour
	campaignTSSeriesTail     = 12 * time.Hour
)

var campaignTSUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const campaignTSSESNote = "SES-routed campaign: Postgres tracking events are sparse on this route (SES strips tracking pixels; delivery/engagement arrive via webhook and may under-report). delivered here is the PG event count, not SES truth — treat as a lower bound. Lake (Athena) wiring is a follow-up; v1 is PG-only."

// ── shared identity / window resolution ──────────────────────────────────────

type campaignTSWave struct {
	WaveNumber        int       `json:"wave_number"`
	ScheduledAt       time.Time `json:"scheduled_at"`
	Status            string    `json:"status"`
	PlannedRecipients int       `json:"planned_recipients"`
}

type campaignTSIdentity struct {
	ID             string
	Name           string
	Status         string
	Subject        string
	Preheader      string
	FromName       string
	FromEmail      string
	CampaignType   string
	PartnerDripTag string
	RouteType      string // ses | pmta
	SendingDomain  string
	SendingProfile string
	OfferID        string
	ThrottleSpeed  string
	ThrottleRPM    int
	ThrottleHours  int
	Planned        int
	SegmentID      string
	ScheduledAt    *time.Time
	CreatedAt      *time.Time
	StartedAt      *time.Time
	CompletedAt    *time.Time

	Waves       []campaignTSWave
	WindowStart *time.Time
	WindowEnd   *time.Time
	FirstWaveAt *time.Time
}

// loadCampaignTSIdentity loads the org-scoped campaign row + its waves and
// derives the send window. Returns sql.ErrNoRows when the campaign does not
// exist in this org.
func (s *AdvancedMailingService) loadCampaignTSIdentity(ctx context.Context, orgID, id string) (*campaignTSIdentity, error) {
	c := &campaignTSIdentity{ID: id}
	var sched, created, started, completed sql.NullTime
	var subject, preheader, fromName, fromEmail, ctype, dripTag sql.NullString
	var domain, profile, offerID, throttle, segID sql.NullString
	var rpm, hours sql.NullInt64
	var viaSES sql.NullBool
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.name,''), COALESCE(c.status,''),
		       c.subject, c.preview_text, c.from_name, c.from_email,
		       c.campaign_type, c.partner_drip_tag,
		       sp.sending_domain, sp.name, c.offer_id::text,
		       c.throttle_speed, c.throttle_rate_per_minute, c.throttle_duration_hours,
		       COALESCE(c.total_recipients,0), c.segment_id::text,
		       c.scheduled_at, c.created_at, c.started_at, c.completed_at,
		       sp.via_ses
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.id = $1::uuid AND c.organization_id = $2::uuid
	`, id, orgID).Scan(&c.Name, &c.Status, &subject, &preheader, &fromName, &fromEmail,
		&ctype, &dripTag, &domain, &profile, &offerID,
		&throttle, &rpm, &hours, &c.Planned, &segID,
		&sched, &created, &started, &completed, &viaSES)
	if err != nil {
		return nil, err
	}
	c.Subject = subject.String
	c.Preheader = preheader.String
	c.FromName = fromName.String
	c.FromEmail = fromEmail.String
	c.CampaignType = ctype.String
	c.PartnerDripTag = dripTag.String
	c.SendingDomain = domain.String
	c.SendingProfile = profile.String
	c.OfferID = offerID.String
	c.ThrottleSpeed = throttle.String
	c.ThrottleRPM = int(rpm.Int64)
	c.ThrottleHours = int(hours.Int64)
	c.SegmentID = segID.String
	if viaSES.Valid && viaSES.Bool {
		c.RouteType = "ses"
	} else {
		c.RouteType = "pmta"
	}
	setT := func(dst **time.Time, v sql.NullTime) {
		if v.Valid {
			t := v.Time.UTC()
			*dst = &t
		}
	}
	setT(&c.ScheduledAt, sched)
	setT(&c.CreatedAt, created)
	setT(&c.StartedAt, started)
	setT(&c.CompletedAt, completed)

	// Waves: markers + the authoritative send window. The waves table has no
	// org column; ownership was just verified through the campaign row.
	rows, err := s.db.QueryContext(ctx, `
		SELECT wave_number, scheduled_at, COALESCE(status,''), COALESCE(planned_recipients,0),
		       window_start_at, window_end_at
		FROM mailing_campaign_waves
		WHERE campaign_id = $1::uuid
		ORDER BY scheduled_at ASC, wave_number ASC
	`, id)
	if err == nil {
		defer rows.Close()
		var wStart, wEnd *time.Time
		for rows.Next() {
			var w campaignTSWave
			var ws, we sql.NullTime
			if err := rows.Scan(&w.WaveNumber, &w.ScheduledAt, &w.Status, &w.PlannedRecipients, &ws, &we); err != nil {
				continue
			}
			w.ScheduledAt = w.ScheduledAt.UTC()
			c.Waves = append(c.Waves, w)
			if ws.Valid {
				t := ws.Time.UTC()
				if wStart == nil || t.Before(*wStart) {
					wStart = &t
				}
			}
			if we.Valid {
				t := we.Time.UTC()
				if wEnd == nil || t.After(*wEnd) {
					wEnd = &t
				}
			}
		}
		c.WindowStart, c.WindowEnd = wStart, wEnd
		if len(c.Waves) > 0 {
			t := c.Waves[0].ScheduledAt
			c.FirstWaveAt = &t
		}
	}

	// Fallbacks for non-wave (standard queue) campaigns.
	if c.WindowStart == nil {
		switch {
		case c.StartedAt != nil:
			c.WindowStart = c.StartedAt
		case c.ScheduledAt != nil:
			c.WindowStart = c.ScheduledAt
		case c.CreatedAt != nil:
			c.WindowStart = c.CreatedAt
		}
	}
	if c.WindowEnd == nil {
		if c.CompletedAt != nil {
			c.WindowEnd = c.CompletedAt
		} else if c.WindowStart != nil {
			t := c.WindowStart.Add(8 * time.Hour) // default throttle window
			c.WindowEnd = &t
		}
	}
	if c.FirstWaveAt == nil && c.ScheduledAt != nil {
		c.FirstWaveAt = c.ScheduledAt
	}
	return c, nil
}

// ── GET /campaigns/{id}/timeseries ───────────────────────────────────────────

type campaignTSBucket struct {
	T            time.Time `json:"t"`
	Attempted    int       `json:"attempted"`
	Delivered    int       `json:"delivered"`
	Opens        int       `json:"opens"` // HUMAN opens (machine/MPP excluded)
	Clicks       int       `json:"clicks"`
	Unsubscribes int       `json:"unsubscribes"`
	Bounces      int       `json:"bounces"`
}

// HandleCampaignTimeseries returns the bucketed send-pace series for one
// campaign. bucket=1m|15m (default: 1m when the send window started <6h ago,
// else 15m).
func (s *AdvancedMailingService) HandleCampaignTimeseries(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if !campaignTSUUIDRe.MatchString(id) {
		respondError(w, http.StatusBadRequest, "invalid campaign id")
		return
	}
	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), campaignTSHandlerTimeout)
	defer cancel()

	ident, err := s.loadCampaignTSIdentity(ctx, orgID, id)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("campaign lookup: %v", err))
		return
	}

	now := time.Now().UTC()
	if ident.WindowStart == nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"api_version": VersionCampaignTimeseries,
			"campaign_id": id,
			"source":      "pg",
			"bucket":      "",
			"series":      []campaignTSBucket{},
			"waves":       ident.Waves,
			"notes":       "campaign has no send window yet (draft / not scheduled)",
		})
		return
	}
	start := ident.WindowStart.UTC()
	end := now
	if ident.WindowEnd != nil {
		end = ident.WindowEnd.Add(campaignTSSeriesTail)
	}
	if end.After(now) {
		end = now
	}
	// Hard range cap so a stale window can never trigger an unbounded scan.
	if end.Sub(start) > 7*24*time.Hour {
		end = start.Add(7 * 24 * time.Hour)
	}
	if !end.After(start) {
		end = start.Add(time.Minute)
	}

	// Bucket selection: per-minute while the window is <6h old, else 15m.
	bucketParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("bucket")))
	bucketSec := 900
	if bucketParam == "" {
		if now.Sub(start) < 6*time.Hour {
			bucketSec = 60
		}
	} else if bucketParam == "1m" {
		bucketSec = 60
	} else if bucketParam != "15m" {
		respondError(w, http.StatusBadRequest, "bucket must be 1m or 15m")
		return
	}
	// Never emit a pathological number of buckets — widen instead.
	for int(end.Sub(start).Seconds())/bucketSec > campaignTSMaxBuckets {
		bucketSec *= 4
	}

	// Bounces in the series are total (hard+soft); the split lives in /detail.
	q := `
		SELECT to_timestamp(floor(extract(epoch FROM t.event_at) / $4) * $4) AS bucket_ts,
		       COUNT(*) FILTER (WHERE t.event_type = 'sent')                                                   AS attempted,
		       COUNT(*) FILTER (WHERE t.event_type = 'delivered')                                              AS delivered,
		       COUNT(*) FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false))       AS opens,
		       COUNT(*) FILTER (WHERE t.event_type = 'clicked')                                                AS clicks,
		       COUNT(*) FILTER (WHERE t.event_type = 'unsubscribed')                                           AS unsubs,
		       COUNT(*) FILTER (WHERE t.event_type = 'bounced')                                                AS bounces
		FROM mailing_tracking_events t
		WHERE t.campaign_id = $1::uuid
		  AND t.event_at >= $2 AND t.event_at < $3
		GROUP BY 1
		ORDER BY 1
	`
	rows, err := s.db.QueryContext(ctx, q, id, start, end, bucketSec)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("timeseries query: %v", err))
		return
	}
	defer rows.Close()

	byBucket := map[int64]campaignTSBucket{}
	for rows.Next() {
		var b campaignTSBucket
		if err := rows.Scan(&b.T, &b.Attempted, &b.Delivered, &b.Opens, &b.Clicks, &b.Unsubscribes, &b.Bounces); err != nil {
			continue
		}
		b.T = b.T.UTC()
		byBucket[b.T.Unix()] = b
	}

	// Zero-fill so the chart renders a continuous pace line.
	step := int64(bucketSec)
	first := (start.Unix() / step) * step
	last := (end.Unix() / step) * step
	series := make([]campaignTSBucket, 0, (last-first)/step+1)
	for ts := first; ts <= last; ts += step {
		if b, ok := byBucket[ts]; ok {
			series = append(series, b)
		} else {
			series = append(series, campaignTSBucket{T: time.Unix(ts, 0).UTC()})
		}
	}

	bucketLabel := "15m"
	switch bucketSec {
	case 60:
		bucketLabel = "1m"
	case 900:
		bucketLabel = "15m"
	default:
		bucketLabel = fmt.Sprintf("%ds", bucketSec)
	}
	notes := "series from mailing_tracking_events (PG); opens are HUMAN opens (is_machine_open excluded); bounces are total (hard/soft split in /detail)"
	if ident.RouteType == "ses" {
		notes += ". " + campaignTSSESNote
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":  VersionCampaignTimeseries,
		"campaign_id":  id,
		"source":       "pg",
		"route_type":   ident.RouteType,
		"bucket":       bucketLabel,
		"window_start": start.Format(time.RFC3339),
		"window_end":   end.Format(time.RFC3339),
		"series":       series,
		"waves":        ident.Waves,
		"generated_at": now.Format(time.RFC3339),
		"notes":        notes,
	})
}

// ── GET /campaigns/{id}/detail ───────────────────────────────────────────────

type campaignTSISPRow struct {
	ISP         string `json:"isp"`
	Delivered   int    `json:"delivered"`
	HumanOpens  int    `json:"human_opens"`
	HumanClicks int    `json:"human_clicks"`
	Hard        int    `json:"hard"`
	Soft        int    `json:"soft"`
	Deferrals   int    `json:"deferrals"`
}

type campaignTSLink struct {
	URL          string `json:"url"`
	Clicks       int    `json:"clicks"`
	UniqueClicks int    `json:"unique_clicks"`
}

// HandleCampaignInlineDetail returns everything the inline row expansion needs
// beyond the pace graph: KPI strip, per-ISP table, top links, content/segment/
// throttle facts, waves, and conversions when the offer is resolvable.
func (s *AdvancedMailingService) HandleCampaignInlineDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if !campaignTSUUIDRe.MatchString(id) {
		respondError(w, http.StatusBadRequest, "invalid campaign id")
		return
	}
	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), campaignTSHandlerTimeout)
	defer cancel()

	ident, err := s.loadCampaignTSIdentity(ctx, orgID, id)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("campaign lookup: %v", err))
		return
	}

	now := time.Now().UTC()
	// Bounded event window: send window start (minus 1h of clock skew) through
	// the engagement tail, never past now and never more than the tail length.
	evStart := now.Add(-campaignTSEngagementTail)
	if ident.WindowStart != nil {
		evStart = ident.WindowStart.Add(-1 * time.Hour)
	}
	evEnd := now
	if ident.WindowStart != nil {
		if cap := ident.WindowStart.Add(campaignTSEngagementTail); cap.Before(evEnd) {
			evEnd = cap
		}
	}
	if !evEnd.After(evStart) {
		evEnd = evStart.Add(time.Minute)
	}

	hb := HardBounceSQL("t")

	// One grouped scan for the KPI strip.
	var attempted, delivered, humanOpensU, humanOpensT, clicksU, clicksT, unsubs, hard, soft, complaints, deferrals int
	var dataAsOf sql.NullTime
	kq := fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE t.event_type = 'sent'),
			COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
			COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false)),
			COUNT(*)                        FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false)),
			COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked'),
			COUNT(*)                        FILTER (WHERE t.event_type = 'clicked'),
			COUNT(*) FILTER (WHERE t.event_type = 'unsubscribed'),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND (%s)),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)),
			COUNT(*) FILTER (WHERE t.event_type = 'complained'),
			COUNT(*) FILTER (WHERE t.event_type IN ('deferred','deferral')),
			MAX(t.event_at)
		FROM mailing_tracking_events t
		WHERE t.campaign_id = $1::uuid
		  AND t.event_at >= $2 AND t.event_at < $3
	`, hb, hb)
	if err := s.db.QueryRowContext(ctx, kq, id, evStart, evEnd).Scan(
		&attempted, &delivered, &humanOpensU, &humanOpensT, &clicksU, &clicksT,
		&unsubs, &hard, &soft, &complaints, &deferrals, &dataAsOf,
	); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("kpi query: %v", err))
		return
	}

	// Per-ISP-family mini table (recipient_domain → family; NULL → other).
	ispCase := ispFamilyCase(`LOWER(COALESCE(NULLIF(t.recipient_domain,''),''))`)
	ispRows := []campaignTSISPRow{}
	iq := fmt.Sprintf(`
		SELECT %s AS fam,
			COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
			COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false)),
			COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked'),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND (%s)),
			COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)),
			COUNT(*) FILTER (WHERE t.event_type IN ('deferred','deferral'))
		FROM mailing_tracking_events t
		WHERE t.campaign_id = $1::uuid
		  AND t.event_at >= $2 AND t.event_at < $3
		GROUP BY 1
		ORDER BY 2 DESC
	`, ispCase, hb, hb)
	if rows, err := s.db.QueryContext(ctx, iq, id, evStart, evEnd); err == nil {
		defer rows.Close()
		for rows.Next() {
			var ir campaignTSISPRow
			if err := rows.Scan(&ir.ISP, &ir.Delivered, &ir.HumanOpens, &ir.HumanClicks, &ir.Hard, &ir.Soft, &ir.Deferrals); err == nil {
				ispRows = append(ispRows, ir)
			}
		}
	}

	// Top 5 clicked links.
	links := []campaignTSLink{}
	if rows, err := s.db.QueryContext(ctx, `
		SELECT t.link_url, COUNT(*), COUNT(DISTINCT t.subscriber_id)
		FROM mailing_tracking_events t
		WHERE t.campaign_id = $1::uuid
		  AND t.event_at >= $2 AND t.event_at < $3
		  AND t.event_type = 'clicked'
		  AND COALESCE(t.link_url,'') <> ''
		GROUP BY 1
		ORDER BY 2 DESC
		LIMIT 5
	`, id, evStart, evEnd); err == nil {
		defer rows.Close()
		for rows.Next() {
			var l campaignTSLink
			if err := rows.Scan(&l.URL, &l.Clicks, &l.UniqueClicks); err == nil {
				links = append(links, l)
			}
		}
	}

	inclusion, exclusion := s.resolveCampaignSegmentNames(ctx, orgID, id, ident.SegmentID)

	// Conversions: offer suppressions reason='converted' within the campaign
	// window (+24h attribution lag) for the campaign's offer — omitted (null)
	// when the offer is not resolvable.
	var conversions *int
	var offerName string
	if offerID, name := s.resolveCampaignOffer(ctx, orgID, ident); offerID != "" {
		offerName = name
		if ident.WindowStart != nil {
			convEnd := evEnd
			if ident.WindowEnd != nil {
				convEnd = ident.WindowEnd.Add(24 * time.Hour)
				if convEnd.After(now) {
					convEnd = now
				}
			}
			var n int
			if err := s.db.QueryRowContext(ctx, `
				SELECT COUNT(*)
				FROM mailing_offer_suppressions
				WHERE offer_id = $1::uuid
				  AND organization_id = $2::uuid
				  AND reason = 'converted'
				  AND suppressed_at >= $3 AND suppressed_at < $4
			`, offerID, orgID, *ident.WindowStart, convEnd).Scan(&n); err == nil {
				conversions = &n
			}
		}
	}

	pct := func(num, den int) float64 { return summaryRate(num, den) }
	notes := "metrics from mailing_tracking_events (PG) — campaign counters never used; human opens exclude machine/MPP fetches; clicks are unique subscribers (machine-click flagging is not yet trustworthy)"
	if ident.RouteType == "ses" {
		notes += ". " + campaignTSSESNote
	}

	resp := map[string]interface{}{
		"api_version": VersionCampaignTimeseries,
		"source":      "pg",
		"campaign": map[string]interface{}{
			"id":              ident.ID,
			"name":            ident.Name,
			"status":          ident.Status,
			"route_type":      ident.RouteType,
			"campaign_type":   ident.CampaignType,
			"partner_drip_tag": ident.PartnerDripTag,
			"subject":         ident.Subject,
			"preheader":       ident.Preheader,
			"from_name":       ident.FromName,
			"from_email":      ident.FromEmail,
			"sending_domain":  ident.SendingDomain,
			"sending_profile": ident.SendingProfile,
			"offer_name":      offerName,
			"scheduled_at":    ident.ScheduledAt,
			"created_at":      ident.CreatedAt,
		},
		"window": map[string]interface{}{
			"first_wave_at":            ident.FirstWaveAt,
			"window_start":             ident.WindowStart,
			"window_end":               ident.WindowEnd,
			"wave_count":               len(ident.Waves),
			"throttle_speed":           ident.ThrottleSpeed,
			"throttle_rate_per_minute": ident.ThrottleRPM,
			"throttle_duration_hours":  ident.ThrottleHours,
		},
		"segments": map[string]interface{}{
			"inclusion": inclusion,
			"exclusion": exclusion,
		},
		"kpis": map[string]interface{}{
			"planned":            ident.Planned,
			"attempted":          attempted,
			"delivered":          delivered,
			"human_opens":        humanOpensU,
			"human_opens_total":  humanOpensT,
			"human_clicks":       clicksU,
			"clicks_total":       clicksT,
			"unsubscribes":       unsubs,
			"hard_bounces":       hard,
			"soft_bounces":       soft,
			"complaints":         complaints,
			"deferrals":          deferrals,
			"conversions":        conversions, // null when offer unresolvable
			"delivered_rate":     pct(delivered, maxInt(attempted, delivered+hard+soft)),
			"human_open_rate":    pct(humanOpensU, delivered),
			"human_click_rate":   pct(clicksU, delivered),
			"ctor":               pct(clicksU, humanOpensU),
			"hard_bounce_rate":   pct(hard, maxInt(attempted, delivered+hard+soft)),
			"soft_bounce_rate":   pct(soft, maxInt(attempted, delivered+hard+soft)),
			"data_as_of":         nullTimeStr(dataAsOf),
		},
		"isp_breakdown": ispRows,
		"top_links":     links,
		"waves":         ident.Waves,
		"generated_at":  now.Format(time.RFC3339),
		"notes":         notes,
	}
	respondJSON(w, http.StatusOK, resp)
}

func nullTimeStr(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// resolveCampaignSegmentNames resolves inclusion/exclusion segment NAMES for a
// campaign. Standard campaigns carry segment_id (+ suppression_segment_ids
// JSONB); PMTA wave campaigns embed inclusion_segments / exclusion_segments in
// pmta_config->'campaign_input'. All ids resolve through mailing_segments,
// org-scoped.
func (s *AdvancedMailingService) resolveCampaignSegmentNames(ctx context.Context, orgID, id, segmentID string) (inclusion []string, exclusion []string) {
	inclusion, exclusion = []string{}, []string{}

	collect := func(dst *[]string, q string) {
		rows, err := s.db.QueryContext(ctx, q, id, orgID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil && name != "" {
				*dst = append(*dst, name)
			}
		}
	}

	// Inclusion: pmta_config inclusion_segments ∪ segment_id.
	collect(&inclusion, `
		SELECT s.name
		FROM mailing_segments s
		WHERE s.organization_id = $2::uuid
		  AND s.id::text IN (
			SELECT jsonb_array_elements_text(COALESCE(c.pmta_config->'campaign_input'->'inclusion_segments','[]'::jsonb))
			FROM mailing_campaigns c WHERE c.id = $1::uuid AND c.organization_id = $2::uuid
		  )
		ORDER BY s.name
	`)
	if len(inclusion) == 0 && campaignTSUUIDRe.MatchString(segmentID) {
		var name string
		if err := s.db.QueryRowContext(ctx, `
			SELECT name FROM mailing_segments WHERE id = $1::uuid AND organization_id = $2::uuid
		`, segmentID, orgID).Scan(&name); err == nil && name != "" {
			inclusion = append(inclusion, name)
		}
	}

	// Exclusion: pmta_config exclusion_segments ∪ suppression_segment_ids.
	collect(&exclusion, `
		SELECT s.name
		FROM mailing_segments s
		WHERE s.organization_id = $2::uuid
		  AND s.id::text IN (
			SELECT jsonb_array_elements_text(COALESCE(c.pmta_config->'campaign_input'->'exclusion_segments','[]'::jsonb))
			FROM mailing_campaigns c WHERE c.id = $1::uuid AND c.organization_id = $2::uuid
			UNION
			SELECT jsonb_array_elements_text(COALESCE(c.suppression_segment_ids,'[]'::jsonb))
			FROM mailing_campaigns c WHERE c.id = $1::uuid AND c.organization_id = $2::uuid
		  )
		ORDER BY s.name
	`)
	return inclusion, exclusion
}

// resolveCampaignOffer resolves the campaign's offer (id + name): direct
// offer_id first, else an exact (case-insensitive) mailing_offers.name match
// on the campaign-name suffix after the last " - " (the operator naming
// convention, e.g. "DB Engager AM - Warby Parker").
func (s *AdvancedMailingService) resolveCampaignOffer(ctx context.Context, orgID string, ident *campaignTSIdentity) (string, string) {
	if campaignTSUUIDRe.MatchString(ident.OfferID) {
		var name string
		if err := s.db.QueryRowContext(ctx, `
			SELECT name FROM mailing_offers WHERE id = $1::uuid AND organization_id = $2::uuid
		`, ident.OfferID, orgID).Scan(&name); err == nil {
			return ident.OfferID, name
		}
		return ident.OfferID, ""
	}
	idx := strings.LastIndex(ident.Name, " - ")
	if idx <= 0 {
		return "", ""
	}
	suffix := strings.TrimSpace(ident.Name[idx+3:])
	if suffix == "" {
		return "", ""
	}
	var offerID, name string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id::text, name FROM mailing_offers
		WHERE organization_id = $1::uuid AND LOWER(name) = LOWER($2)
		LIMIT 1
	`, orgID, suffix).Scan(&offerID, &name); err == nil {
		return offerID, name
	}
	return "", ""
}

// ── GET /campaigns/list-metrics ──────────────────────────────────────────────

type campaignListMetrics struct {
	Attempted   int `json:"attempted"`
	Delivered   int `json:"delivered"`
	HumanOpens  int `json:"human_opens"`
	HumanClicks int `json:"human_clicks"`
	Unsubs      int `json:"unsubscribes"`
	Hard        int `json:"hard_bounces"`
	Soft        int `json:"soft_bounces"`
	Complaints  int `json:"complaints"`
}

type campaignListEntry struct {
	Subject       string              `json:"subject"`
	SendingDomain string              `json:"sending_domain"`
	RouteType     string              `json:"route_type"`
	Planned       int                 `json:"planned"`
	OfferID       string              `json:"offer_id,omitempty"`
	ScheduledAt   *time.Time          `json:"scheduled_at,omitempty"`
	FirstWaveAt   *time.Time          `json:"first_wave_at,omitempty"`
	WindowStart   *time.Time          `json:"window_start,omitempty"`
	WindowEnd     *time.Time          `json:"window_end,omitempty"`
	WaveCount     int                 `json:"wave_count"`
	Metrics       campaignListMetrics `json:"metrics"`
}

type campaignTagEntry struct {
	WaveCount   int                 `json:"wave_count"` // member campaigns in window
	Planned     int                 `json:"planned"`
	FirstWaveAt *time.Time          `json:"first_wave_at,omitempty"`
	LastWaveAt  *time.Time          `json:"last_wave_at,omitempty"`
	RouteType   string              `json:"route_type"`
	Metrics     campaignListMetrics `json:"metrics"`
}

// HandleCampaignListMetrics returns tracking-event metrics + identity extras
// for a PAGE of campaign ids (≤50) in ONE grouped event scan, plus optional
// per-partner-drip-tag rollups for the drip toggle. Never reads campaign
// counters.
//
// Query params:
//   - ids:  comma-separated campaign uuids (≤50)
//   - tags: comma-separated partner_drip_tag values (≤25)
//   - from/to: RFC3339 event window for tag rollups (and a floor for id
//     metrics); defaults: ids → min(campaign created_at)-1h, tags → last 24h.
func (s *AdvancedMailingService) HandleCampaignListMetrics(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), campaignTSHandlerTimeout)
	defer cancel()
	now := time.Now().UTC()

	ids := []string{}
	for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if !campaignTSUUIDRe.MatchString(v) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid campaign id %q", v))
			return
		}
		ids = append(ids, v)
	}
	if len(ids) > campaignTSMaxListIDs {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("too many ids (max %d per page)", campaignTSMaxListIDs))
		return
	}
	tags := []string{}
	for _, raw := range strings.Split(r.URL.Query().Get("tags"), ",") {
		if v := strings.TrimSpace(raw); v != "" {
			tags = append(tags, v)
		}
	}
	if len(tags) > campaignTSMaxTags {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("too many tags (max %d)", campaignTSMaxTags))
		return
	}
	if len(ids) == 0 && len(tags) == 0 {
		respondError(w, http.StatusBadRequest, "ids or tags required")
		return
	}

	parseT := func(key string, def time.Time) time.Time {
		if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.UTC()
			}
		}
		return def
	}
	from := parseT("from", time.Time{})
	to := parseT("to", now)
	if to.After(now) {
		to = now
	}

	campaigns := map[string]*campaignListEntry{}
	sesPresent := false

	if len(ids) > 0 {
		// 1) Identity extras + per-campaign window (one query, org-scoped).
		minCreated := now
		iq := `
			SELECT c.id::text, COALESCE(c.subject,''), COALESCE(sp.sending_domain,''),
			       COALESCE(sp.via_ses,false), COALESCE(c.total_recipients,0),
			       c.offer_id::text, c.scheduled_at, c.created_at,
			       w.first_wave, w.win_start, w.win_end, COALESCE(w.n,0)
			FROM mailing_campaigns c
			LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
			LEFT JOIN LATERAL (
				SELECT MIN(scheduled_at) AS first_wave, MIN(window_start_at) AS win_start,
				       MAX(window_end_at) AS win_end, COUNT(*) AS n
				FROM mailing_campaign_waves wv WHERE wv.campaign_id = c.id
			) w ON TRUE
			WHERE c.id = ANY($1::uuid[]) AND c.organization_id = $2::uuid
		`
		rows, err := s.db.QueryContext(ctx, iq, pq.Array(ids), orgID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("identity query: %v", err))
			return
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var cid string
				e := &campaignListEntry{}
				var viaSES bool
				var offerID sql.NullString
				var sched, created, fw, ws, we sql.NullTime
				if err := rows.Scan(&cid, &e.Subject, &e.SendingDomain, &viaSES, &e.Planned,
					&offerID, &sched, &created, &fw, &ws, &we, &e.WaveCount); err != nil {
					continue
				}
				if viaSES {
					e.RouteType = "ses"
					sesPresent = true
				} else {
					e.RouteType = "pmta"
				}
				e.OfferID = offerID.String
				setNT := func(dst **time.Time, v sql.NullTime) {
					if v.Valid {
						t := v.Time.UTC()
						*dst = &t
					}
				}
				setNT(&e.ScheduledAt, sched)
				setNT(&e.FirstWaveAt, fw)
				setNT(&e.WindowStart, ws)
				setNT(&e.WindowEnd, we)
				if created.Valid && created.Time.UTC().Before(minCreated) {
					minCreated = created.Time.UTC()
				}
				campaigns[cid] = e
			}
		}()

		// 2) ONE grouped event scan for the whole page, bounded by the page's
		//    oldest campaign created_at (NOT the caller's `from` — metrics
		//    columns reflect each campaign's whole life; `from` only scopes
		//    the tag rollups below).
		evFrom := minCreated.Add(-1 * time.Hour)
		mq := fmt.Sprintf(`
			SELECT t.campaign_id::text,
				COUNT(*) FILTER (WHERE t.event_type = 'sent'),
				COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
				COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false)),
				COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked'),
				COUNT(*) FILTER (WHERE t.event_type = 'unsubscribed'),
				COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND (%s)),
				COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)),
				COUNT(*) FILTER (WHERE t.event_type = 'complained')
			FROM mailing_tracking_events t
			WHERE t.campaign_id = ANY($1::uuid[])
			  AND t.event_at >= $2 AND t.event_at < $3
			GROUP BY t.campaign_id
		`, HardBounceSQL("t"), HardBounceSQL("t"))
		mrows, err := s.db.QueryContext(ctx, mq, pq.Array(ids), evFrom, to)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("metrics query: %v", err))
			return
		}
		func() {
			defer mrows.Close()
			for mrows.Next() {
				var cid string
				var m campaignListMetrics
				if err := mrows.Scan(&cid, &m.Attempted, &m.Delivered, &m.HumanOpens,
					&m.HumanClicks, &m.Unsubs, &m.Hard, &m.Soft, &m.Complaints); err != nil {
					continue
				}
				if e, ok := campaigns[cid]; ok {
					e.Metrics = m
				}
			}
		}()
	}

	// Tag rollups for the drip toggle: aggregates per partner_drip_tag,
	// bounded by [from,to] (default: last 24h).
	tagEntries := map[string]*campaignTagEntry{}
	if len(tags) > 0 {
		tagFrom := from
		if tagFrom.IsZero() {
			tagFrom = now.Add(-24 * time.Hour)
		}
		// Identity per tag: member campaigns whose activity falls in window.
		tq := `
			SELECT c.partner_drip_tag, COUNT(*), SUM(COALESCE(c.total_recipients,0)),
			       MIN(c.scheduled_at), MAX(c.scheduled_at), BOOL_OR(COALESCE(sp.via_ses,false))
			FROM mailing_campaigns c
			LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
			WHERE c.partner_drip_tag = ANY($1::text[])
			  AND c.organization_id = $2::uuid
			  AND COALESCE(c.scheduled_at, c.created_at) >= $3
			  AND COALESCE(c.scheduled_at, c.created_at) < $4
			GROUP BY c.partner_drip_tag
		`
		if rows, err := s.db.QueryContext(ctx, tq, pq.Array(tags), orgID, tagFrom, to); err == nil {
			func() {
				defer rows.Close()
				for rows.Next() {
					var tag string
					e := &campaignTagEntry{}
					var first, last sql.NullTime
					var viaSES bool
					if err := rows.Scan(&tag, &e.WaveCount, &e.Planned, &first, &last, &viaSES); err != nil {
						continue
					}
					if viaSES {
						e.RouteType = "ses"
						sesPresent = true
					} else {
						e.RouteType = "pmta"
					}
					if first.Valid {
						t := first.Time.UTC()
						e.FirstWaveAt = &t
					}
					if last.Valid {
						t := last.Time.UTC()
						e.LastWaveAt = &t
					}
					tagEntries[tag] = e
				}
			}()
		}
		// One grouped event scan across all requested tags, bounded by
		// event_at window (the per-campaign join is driven by the campaign
		// index; event rows hit (campaign_id, event_at)).
		tmq := fmt.Sprintf(`
			SELECT c.partner_drip_tag,
				COUNT(*) FILTER (WHERE t.event_type = 'sent'),
				COUNT(*) FILTER (WHERE t.event_type = 'delivered'),
				COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'opened' AND NOT COALESCE(t.is_machine_open,false)),
				COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked'),
				COUNT(*) FILTER (WHERE t.event_type = 'unsubscribed'),
				COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND (%s)),
				COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND NOT (%s)),
				COUNT(*) FILTER (WHERE t.event_type = 'complained')
			FROM mailing_tracking_events t
			JOIN mailing_campaigns c ON c.id = t.campaign_id
			WHERE c.partner_drip_tag = ANY($1::text[])
			  AND c.organization_id = $2::uuid
			  AND t.event_at >= $3 AND t.event_at < $4
			GROUP BY c.partner_drip_tag
		`, HardBounceSQL("t"), HardBounceSQL("t"))
		if rows, err := s.db.QueryContext(ctx, tmq, pq.Array(tags), orgID, tagFrom, to); err == nil {
			func() {
				defer rows.Close()
				for rows.Next() {
					var tag string
					var m campaignListMetrics
					if err := rows.Scan(&tag, &m.Attempted, &m.Delivered, &m.HumanOpens,
						&m.HumanClicks, &m.Unsubs, &m.Hard, &m.Soft, &m.Complaints); err != nil {
						continue
					}
					if e, ok := tagEntries[tag]; ok {
						e.Metrics = m
					} else {
						tagEntries[tag] = &campaignTagEntry{RouteType: "pmta", Metrics: m}
					}
				}
			}()
		}
	}

	notes := "metrics from mailing_tracking_events (PG) — campaign counters never used; human opens exclude machine/MPP; clicks are unique subscribers; hard/soft split via canonical HardBounceSQL"
	if sesPresent {
		notes += ". " + campaignTSSESNote
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":  VersionCampaignTimeseries,
		"source":       "pg",
		"generated_at": now.Format(time.RFC3339),
		"campaigns":    campaigns,
		"tags":         tagEntries,
		"notes":        notes,
	})
}

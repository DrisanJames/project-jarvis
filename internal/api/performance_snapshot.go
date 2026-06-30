package api

// Current-day (Denver/MT) performance snapshot — a READ-ONLY operator dashboard
// surface aggregated by sending DOMAIN or by OFFER.
//
//	GET /api/mailing/performance/snapshot?group_by=domain|offer
//
// Doctrine (mirrors the #customer-acquisition Slack digest so the portal
// reconciles with it — see .scratch/jun08_acq_hourly_light.py and
// .scratch/jun08_domain_funnel.py):
//
//   - Volume + bounces + unsubs come from the mailing_campaigns ROLLUP counters
//     (sent_count / delivered_count / hard_bounce_count / soft_bounce_count /
//     unsubscribe_count), bounded to TODAY-MT by scheduled_at.
//   - HUMAN opens/clicks come from mailing_tracking_events ONLY, as
//     COUNT(DISTINCT subscriber_id) FILTER (event_type AND NOT machine-flag),
//     bounded to TODAY-MT by event_at. Apple-MPP / scanner traffic is excluded
//     via is_machine_open / is_machine_click (both columns confirmed present:
//     migration 046 + cmd/server/main.go add_is_machine_click_col).
//   - "today-MT" = >= date_trunc('day', now() AT TIME ZONE 'America/Denver'),
//     re-localised back to TIMESTAMPTZ, exactly as the acq reporter does.
//   - Sending domain is normalised to its brand root (strip leading em./m.).
//   - Hard vs soft bounces are ALWAYS kept separate.
//   - Org-scoped via getOrgID(r) (the helper every sibling analytics handler
//     in this package uses); each org id is cast ::uuid in SQL.
//
// Offer view adds slug-anchored EXACT clicks from the money-URL link_url plus a
// BEST-EFFORT campaign->offer volume map (see the offerSnapshot section).
//
// Robustness: bounded to TODAY only, an ~8s context timeout, and FAIL-SOFT —
// any single query error degrades into a note rather than a 500.
//
// No writes anywhere.

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// VersionPerformanceSnapshot is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-06-21): initial — domain + offer current-day snapshot.
//	1.1 (2026-06-29): offer view falls back to the campaign name-derived offer
//	    (clicks + volume) when mailing_offer_slug_map is empty/sparse, so the
//	    Offer breakdown is populated instead of collapsing to (unattributed).
const VersionPerformanceSnapshot = "1.1"

const perfSnapshotTimeout = 8 * time.Second

// todayMT is the SQL expression for "start of the current Denver day" as a
// TIMESTAMPTZ — identical to the acq reporter's WHERE clause. Used as the
// floor for both scheduled_at (campaign rollups) and event_at (tracking).
const todayMT = `(date_trunc('day', now() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver')`

// ── slug + domain helpers (unit-tested) ──────────────────────────────────────

// moneyHostLabels enumerates the money-network host roots the platform sends to
// (the same set the click-drip enroller and money_link_check scan —
// cratoolpro/eos57ytf/k8k0hfdt/xnonu/muqes/jyqye). Anchoring on these avoids
// mis-classifying a non-money click (e.g. a t.em.<brand>/track/open redirect)
// as an offer click in the offer view.
var moneyHostLabels = []string{"cratoolpro", "eos57ytf", "k8k0hfdt", "xnonu", "muqes", "jyqye"}

// moneySlugRe extracts the offer slug from a money URL: the path segment AFTER
// the account id, i.e. <moneyhost>.com/<acct>/<slug>[/...]. Scheme-tolerant
// (the stored link_url may or may not carry https://) and host-anchored to the
// money networks above — NOT a loose any-host match.
//
//	cratoolpro.com/BJB4Q5BF/93W8N2N/?x            -> 93W8N2N
//	https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?s=1 -> 2HH43PB
//	xnonu.com/TQ5MX18J/XF1SR2CS/                  -> XF1SR2CS
//	t.em.discountblog.com/track/open?id=abc       -> "" (not a money host)
var moneySlugRe = regexp.MustCompile(
	`(?i)(?:https?://)?(?:www\.)?(?:` + strings.Join(moneyHostLabels, "|") +
		`)\.com/[A-Za-z0-9]+/([A-Za-z0-9_-]+)`)

// slugFromLinkURL returns the anchored money slug, or "" when the URL is not a
// recognisable money URL. Anchored (positional path segment on a money host),
// never a loose substring scan.
func slugFromLinkURL(u string) string {
	if u == "" {
		return ""
	}
	m := moneySlugRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// normalizeSendingDomain strips a leading em./m. so the sending domain collapses
// to its brand root — matching root() in the acq Python reporters.
//
//	em.discountblog.com -> discountblog.com
func normalizeSendingDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return "(none)"
	}
	for _, p := range []string{"em.", "m."} {
		if strings.HasPrefix(d, p) {
			return d[len(p):]
		}
	}
	return d
}

// ── response contract ────────────────────────────────────────────────────────

type perfSnapshotRow struct {
	Key             string  `json:"key"`
	Label           string  `json:"label"`
	Sent            int64   `json:"sent"`
	Delivered       int64   `json:"delivered"`
	Reached         int64   `json:"reached"`
	DeliveryPct     float64 `json:"delivery_pct"`
	HumanOpens      int64   `json:"human_opens"`
	OpenPct         float64 `json:"open_pct"`
	Clicks          int64   `json:"clicks"`
	Clickers        int64   `json:"clickers"`
	ClickPct        float64 `json:"click_pct"`
	CtorPct         float64 `json:"ctor_pct"`
	Hard            int64   `json:"hard"`
	HardPct         float64 `json:"hard_pct"`
	Soft            int64   `json:"soft"`
	SoftPct         float64 `json:"soft_pct"`
	Unattributed    bool    `json:"unattributed"`
	ClicksExact     bool    `json:"clicks_exact"`
	VolumeEstimated bool    `json:"volume_estimated"`
}

type perfSnapshotTotals struct {
	Sent       int64 `json:"sent"`
	Delivered  int64 `json:"delivered"`
	Reached    int64 `json:"reached"`
	HumanOpens int64 `json:"human_opens"`
	Clicks     int64 `json:"clicks"`
	Clickers   int64 `json:"clickers"`
	Hard       int64 `json:"hard"`
	Soft       int64 `json:"soft"`
	Unsub      int64 `json:"unsub"`
}

type perfSnapshotResponse struct {
	GroupBy    string             `json:"group_by"`
	DenverDate string             `json:"denver_date"`
	AsOf       string             `json:"as_of"`
	Totals     perfSnapshotTotals `json:"totals"`
	Rows       []perfSnapshotRow  `json:"rows"`
	Notes      []string           `json:"notes"`
}

func pct1(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(int64(float64(n)/float64(d)*1000.0+0.5)) / 10.0
}

// ── accumulators ─────────────────────────────────────────────────────────────

// volAgg holds the campaign-counter side of a row.
type volAgg struct {
	sent, delivered, hard, soft, unsub, humanOpens int64
}

// clickAgg holds the tracking-event click side of a row, keyed independently so
// a missing volume side still surfaces engagement.
type clickAgg struct {
	clicks, clickers int64
}

// offerMeta is a resolved offer's display identity (everflow key + label).
type offerMeta struct {
	key, label string
}

// ── HANDLER ──────────────────────────────────────────────────────────────────

// HandlePerformanceSnapshot serves the current-day performance snapshot.
func (s *AdvancedMailingService) HandlePerformanceSnapshot(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ctx, cancel := context.WithTimeout(r.Context(), perfSnapshotTimeout)
	defer cancel()

	groupBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_by")))
	if groupBy != "offer" {
		groupBy = "domain"
	}

	now := time.Now()
	denverDate := now.UTC().Format("2006-01-02")
	if loc, err := time.LoadLocation("America/Denver"); err == nil {
		denverDate = now.In(loc).Format("2006-01-02")
	}

	baseNotes := []string{
		"opens exclude machine traffic where flagged; flagging is strongest on PMTA routes — SES opens may still include machine/MPP traffic",
		"volume & bounces from campaign counters (can lag live in-flight sends); rates capped at 100%",
		"offer volume is best-effort campaign→offer; clicks are exact (slug-anchored)",
		"for per-ISP placement, deferral & bounce detail, see the Domain Agent scorecard",
	}

	var resp perfSnapshotResponse
	if groupBy == "offer" {
		resp = s.snapshotByOffer(ctx, orgID)
	} else {
		resp = s.snapshotByDomain(ctx, orgID)
	}
	resp.GroupBy = groupBy
	resp.DenverDate = denverDate
	resp.AsOf = now.UTC().Format(time.RFC3339)
	resp.Notes = append(baseNotes, resp.Notes...)
	if resp.Rows == nil {
		resp.Rows = []perfSnapshotRow{}
	}

	respondJSON(w, http.StatusOK, resp)
}

// finalizeRow derives the percentage fields from the raw counters. reached is
// set by the caller (no per-campaign SES-relayed counter exists, so
// reached == delivered — see the response note).
// capPct bounds a rate at 100.0. Volume (campaign counters) and engagement
// (tracking_events) come from different sources that can lag each other — e.g.
// delivered_count can briefly exceed sent_count on live in-flight sends — which
// would otherwise render a nonsensical >100% rate. Cap defensively.
func capPct(v float64) float64 {
	if v > 100.0 {
		return 100.0
	}
	return v
}

func finalizeRow(row *perfSnapshotRow) {
	row.DeliveryPct = capPct(pct1(row.Delivered, row.Sent))
	row.OpenPct = capPct(pct1(row.HumanOpens, row.Delivered))
	row.ClickPct = capPct(pct1(row.Clickers, row.Delivered))
	row.CtorPct = capPct(pct1(row.Clickers, row.HumanOpens))
	row.HardPct = capPct(pct1(row.Hard, row.Sent))
	row.SoftPct = capPct(pct1(row.Soft, row.Sent))
}

// ── DOMAIN VIEW ──────────────────────────────────────────────────────────────

// snapshotByDomain ports the acq SQL: volume/bounces from campaign counters,
// human opens/clicks from tracking_events, grouped by normalised sending domain.
// All domain rows are exact + attributed (clicks_exact=true, volume_estimated=
// false, unattributed=false). Fail-soft per query.
func (s *AdvancedMailingService) snapshotByDomain(ctx context.Context, orgID string) perfSnapshotResponse {
	var notes []string
	vol := map[string]*volAgg{}   // domain root -> counters
	clk := map[string]*clickAgg{} // domain root -> exact clicks

	// (1) Campaign rollups — volume / delivered / bounces / unsubs / human opens
	//     by sending domain. Human opens are pulled here too via a correlated
	//     DISTINCT-subscriber tracking subquery scoped to today-MT so the open
	//     number aligns 1:1 with the domain its campaigns sent under.
	rollupSQL := `
		SELECT COALESCE(sp.sending_domain, split_part(c.from_email,'@',2), '(none)') AS dom,
		       COALESCE(SUM(c.sent_count),0)        AS sent,
		       COALESCE(SUM(c.delivered_count),0)   AS delivered,
		       COALESCE(SUM(c.hard_bounce_count),0) AS hard,
		       COALESCE(SUM(c.soft_bounce_count),0) AS soft,
		       COALESCE(SUM(c.unsubscribe_count),0) AS unsub
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		WHERE c.organization_id = $1::uuid
		  AND c.scheduled_at >= ` + todayMT + `
		GROUP BY 1`
	if rows, err := s.db.QueryContext(ctx, rollupSQL, orgID); err != nil {
		notes = append(notes, "volume rollup unavailable: "+err.Error())
	} else {
		for rows.Next() {
			var dom string
			var a volAgg
			if err := rows.Scan(&dom, &a.sent, &a.delivered, &a.hard, &a.soft, &a.unsub); err != nil {
				continue
			}
			k := normalizeSendingDomain(dom)
			g := vol[k]
			if g == nil {
				g = &volAgg{}
				vol[k] = g
			}
			g.sent += a.sent
			g.delivered += a.delivered
			g.hard += a.hard
			g.soft += a.soft
			g.unsub += a.unsub
		}
		rows.Close()
	}

	// (2) Human opens + clicks from tracking_events, grouped by the event's
	//     sending_domain (the column the tracking rows carry directly).
	engSQL := `
		SELECT COALESCE(sending_domain,'(none)') AS dom,
		       COUNT(DISTINCT subscriber_id) FILTER (
		         WHERE event_type='opened'  AND NOT COALESCE(is_machine_open,false))  AS opens,
		       COUNT(*) FILTER (
		         WHERE event_type='clicked' AND NOT COALESCE(is_machine_click,false)) AS clicks,
		       COUNT(DISTINCT subscriber_id) FILTER (
		         WHERE event_type='clicked' AND NOT COALESCE(is_machine_click,false)) AS clickers
		FROM mailing_tracking_events
		WHERE organization_id = $1::uuid
		  AND event_at >= ` + todayMT + `
		  AND event_type IN ('opened','clicked')
		GROUP BY 1`
	if rows, err := s.db.QueryContext(ctx, engSQL, orgID); err != nil {
		notes = append(notes, "engagement unavailable: "+err.Error())
	} else {
		for rows.Next() {
			var dom string
			var opens, clicks, clickers int64
			if err := rows.Scan(&dom, &opens, &clicks, &clickers); err != nil {
				continue
			}
			k := normalizeSendingDomain(dom)
			g := vol[k]
			if g == nil {
				g = &volAgg{}
				vol[k] = g
			}
			g.humanOpens += opens
			c := clk[k]
			if c == nil {
				c = &clickAgg{}
				clk[k] = c
			}
			c.clicks += clicks
			c.clickers += clickers
		}
		rows.Close()
	}

	// Merge into rows.
	rowsOut := make([]perfSnapshotRow, 0, len(vol))
	for k, g := range vol {
		c := clk[k]
		row := perfSnapshotRow{
			Key:         k,
			Label:       k,
			Sent:        g.sent,
			Delivered:   g.delivered,
			Reached:     g.delivered, // no per-campaign SES-relayed counter; reached=delivered
			HumanOpens:  g.humanOpens,
			Hard:        g.hard,
			Soft:        g.soft,
			ClicksExact: true,
		}
		if c != nil {
			row.Clicks = c.clicks
			row.Clickers = c.clickers
		}
		finalizeRow(&row)
		rowsOut = append(rowsOut, row)
	}

	sort.Slice(rowsOut, func(i, j int) bool {
		if rowsOut[i].Sent != rowsOut[j].Sent {
			return rowsOut[i].Sent > rowsOut[j].Sent
		}
		return rowsOut[i].Key < rowsOut[j].Key
	})

	return perfSnapshotResponse{Totals: totalsOf(rowsOut, vol), Rows: rowsOut, Notes: notes}
}

// ── OFFER VIEW ───────────────────────────────────────────────────────────────

// snapshotByOffer merges EXACT slug-anchored clicks (tracking_events.link_url ->
// mailing_offer_slug_map) with BEST-EFFORT campaign->offer volume.
func (s *AdvancedMailingService) snapshotByOffer(ctx context.Context, orgID string) perfSnapshotResponse {
	var notes []string

	// slug -> offer label, loaded from the canonical dictionary.
	slugToOffer := map[string]offerMeta{} // cratoolpro_slug -> offer
	efToOffer := map[string]offerMeta{}   // everflow_offer_id -> offer
	if rows, err := s.db.QueryContext(ctx, `
		SELECT cratoolpro_slug, everflow_offer_id, COALESCE(offer_name,'')
		FROM mailing_offer_slug_map`); err != nil {
		notes = append(notes, "offer slug map unavailable: "+err.Error())
	} else {
		for rows.Next() {
			var slug, ef, name string
			if err := rows.Scan(&slug, &ef, &name); err != nil {
				continue
			}
			key := ef
			label := name
			if label == "" {
				label = "offer " + ef
			}
			om := offerMeta{key: key, label: label}
			slugToOffer[slug] = om
			if _, ok := efToOffer[ef]; !ok {
				efToOffer[ef] = om
			}
		}
		rows.Close()
	}

	const unattribKey = "(unattributed)"
	vol := map[string]*volAgg{}   // offer key -> counters
	clk := map[string]*clickAgg{} // offer key -> exact clicks
	label := map[string]string{}  // offer key -> display label
	label[unattribKey] = "(unattributed)"

	// campFallback maps a campaign -> an offer identity DERIVED FROM THE CAMPAIGN
	// NAME suffix (the operator " - <Offer>" convention). It is the graceful
	// degrade for deployments where mailing_offer_slug_map is empty/sparse: with
	// no slug dictionary the click + volume loops would otherwise collapse EVERY
	// row into (unattributed) and the Offer view would look broken. Resolving
	// both loops through the same name-derived key keeps clicks and volume on the
	// SAME offer row. Loaded once for today-MT campaigns; "name:" prefix keeps
	// these keys from colliding with everflow/drip keys.
	campFallback := map[string]offerMeta{}
	fbSQL := `
		SELECT c.id::text, COALESCE(c.name,'')
		FROM mailing_campaigns c
		WHERE c.organization_id = $1::uuid
		  AND c.scheduled_at >= ` + todayMT
	if rows, err := s.db.QueryContext(ctx, fbSQL, orgID); err != nil {
		notes = append(notes, "offer name fallback unavailable: "+err.Error())
	} else {
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				continue
			}
			if suffix := nameSuffixOffer(name); suffix != "" {
				campFallback[id] = offerMeta{key: "name:" + strings.ToLower(suffix), label: suffix}
			}
		}
		rows.Close()
	}

	// campaignToOffer maps a campaign -> the offer its clicks landed on, so the
	// VOLUME loop below can follow the SAME offer as the clicks for any campaign
	// that got >=1 mapped click. This closes the "clicks with no volume" gap
	// where a broadcast campaign with a NULL/zero everflow_offer_id but a money
	// slug in its creative had its clicks attributed to an offer while its volume
	// fell to (unattributed). Only mapped (non-unattributed) offers are tracked;
	// when a campaign clicked across several offer-slugs we keep per-offer counts
	// and pick the dominant one (ties -> first seen, preserved via firstSeen).
	campaignToOffer := map[string]string{}       // campaign_id -> winning offer key
	campOfferCnt := map[string]map[string]int{}  // campaign_id -> offer key -> clicks
	campOfferSeen := map[string]map[string]int{} // campaign_id -> offer key -> first-seen order

	// (1) EXACT clicks: scan today-MT clicked events, extract the slug from
	//     link_url in Go (anchored), resolve via the dictionary. Unmapped slugs
	//     and unrecognisable money URLs fold into (unattributed).
	clickSQL := `
		SELECT subscriber_id::text, campaign_id::text, COALESCE(link_url,'')
		FROM mailing_tracking_events
		WHERE organization_id = $1::uuid
		  AND event_at >= ` + todayMT + `
		  AND event_type = 'clicked'
		  AND NOT COALESCE(is_machine_click,false)`
	if rows, err := s.db.QueryContext(ctx, clickSQL, orgID); err != nil {
		notes = append(notes, "exact clicks unavailable: "+err.Error())
	} else {
		// clickers = distinct subscriber per offer.
		seen := map[string]map[string]struct{}{}
		seenOrder := 0
		for rows.Next() {
			var sub, camp, url string
			if err := rows.Scan(&sub, &camp, &url); err != nil {
				continue
			}
			slug := slugFromLinkURL(url)
			key := unattribKey
			if slug != "" {
				if om, ok := slugToOffer[slug]; ok {
					key = om.key
					if _, ok := label[key]; !ok {
						label[key] = om.label
					}
				}
			}
			// Graceful degrade: no slug-map hit (empty dictionary or unmapped
			// slug) — attribute the click to the campaign's name-derived offer so
			// it lands on a real offer row instead of (unattributed).
			if key == unattribKey {
				if om, ok := campFallback[camp]; ok {
					key = om.key
					if _, ok := label[key]; !ok {
						label[key] = om.label
					}
				}
			}
			c := clk[key]
			if c == nil {
				c = &clickAgg{}
				clk[key] = c
			}
			c.clicks++
			set := seen[key]
			if set == nil {
				set = map[string]struct{}{}
				seen[key] = set
			}
			if sub != "" {
				if _, ok := set[sub]; !ok {
					set[sub] = struct{}{}
					c.clickers++
				}
			} else {
				c.clickers++
			}
			// Track per-campaign offer click counts for the volume-follow map.
			// Only attribute volume-follow to a real (mapped) offer, never to
			// (unattributed) — an unmapped click must not pin a campaign's volume.
			if camp != "" && key != unattribKey {
				cnt := campOfferCnt[camp]
				if cnt == nil {
					cnt = map[string]int{}
					campOfferCnt[camp] = cnt
				}
				if _, ok := cnt[key]; !ok {
					ord := campOfferSeen[camp]
					if ord == nil {
						ord = map[string]int{}
						campOfferSeen[camp] = ord
					}
					ord[key] = seenOrder
					seenOrder++
				}
				cnt[key]++
			}
		}
		rows.Close()
	}

	// Resolve each campaign's winning offer: most clicks, ties -> first seen.
	for camp, cnt := range campOfferCnt {
		if best := dominantOffer(cnt, campOfferSeen[camp]); best != "" {
			campaignToOffer[camp] = best
		}
	}

	// (2) BEST-EFFORT volume per offer: map each campaign -> offer by the best
	//     available signal in PRIORITY order —
	//       (1) its own clicked-slug offer (campaignToOffer) so volume follows
	//           the SAME offer as its clicks,
	//       (2) everflow_offer_id, (3) partner_drip_tag,
	//       (4) the name " - " suffix (matched against the slug-map offer_name).
	//     Counters + human opens aggregate per offer; campaigns with no
	//     resolvable offer fold into (unattributed). Each campaign's volume goes
	//     to exactly ONE offer row.
	volSQL := `
		SELECT c.id::text AS campaign_id,
		       COALESCE(c.everflow_offer_id::text,'') AS ef,
		       COALESCE(c.partner_drip_tag,'')        AS drip,
		       COALESCE(c.name,'')                     AS name,
		       COALESCE(c.sent_count,0)        AS sent,
		       COALESCE(c.delivered_count,0)   AS delivered,
		       COALESCE(c.hard_bounce_count,0) AS hard,
		       COALESCE(c.soft_bounce_count,0) AS soft,
		       COALESCE(c.unsubscribe_count,0) AS unsub,
		       COALESCE((
		          SELECT COUNT(DISTINCT t.subscriber_id)
		          FROM mailing_tracking_events t
		          WHERE t.campaign_id = c.id
		            AND t.organization_id = c.organization_id
		            AND t.event_at >= ` + todayMT + `
		            AND t.event_type = 'opened'
		            AND NOT COALESCE(t.is_machine_open,false)
		       ),0) AS human_opens
		FROM mailing_campaigns c
		WHERE c.organization_id = $1::uuid
		  AND c.scheduled_at >= ` + todayMT
	if rows, err := s.db.QueryContext(ctx, volSQL, orgID); err != nil {
		notes = append(notes, "offer volume unavailable: "+err.Error())
	} else {
		for rows.Next() {
			var campID, ef, drip, name string
			var a volAgg
			if err := rows.Scan(&campID, &ef, &drip, &name, &a.sent, &a.delivered, &a.hard, &a.soft, &a.unsub, &a.humanOpens); err != nil {
				continue
			}
			key := unattribKey
			switch {
			case campaignToOffer[campID] != "":
				// (1) Volume follows the campaign's own clicked-slug offer.
				key = campaignToOffer[campID]
				// label was already set from slugToOffer during click bucketing.
			case ef != "" && ef != "0":
				if om, ok := efToOffer[ef]; ok {
					key = om.key
					if _, ok := label[key]; !ok {
						label[key] = om.label
					}
				} else {
					key = ef
					if _, ok := label[key]; !ok {
						label[key] = "offer " + ef
					}
				}
			case drip != "":
				key = "drip:" + drip
				if _, ok := label[key]; !ok {
					label[key] = drip
				}
			default:
				if suffix := nameSuffixOffer(name); suffix != "" {
					if k, lbl := matchOfferByName(efToOffer, suffix); k != "" {
						key = k
						if _, ok := label[key]; !ok {
							label[key] = lbl
						}
					} else if om, ok := campFallback[campID]; ok {
						// No slug-map / everflow label match — fall back to the
						// name-derived offer so this campaign's volume lands on the
						// same offer row its clicks did (keeps the Offer view
						// populated when mailing_offer_slug_map is empty).
						key = om.key
						if _, ok := label[key]; !ok {
							label[key] = om.label
						}
					}
				}
			}
			g := vol[key]
			if g == nil {
				g = &volAgg{}
				vol[key] = g
			}
			g.sent += a.sent
			g.delivered += a.delivered
			g.hard += a.hard
			g.soft += a.soft
			g.unsub += a.unsub
			g.humanOpens += a.humanOpens
		}
		rows.Close()
	}

	// Merge: every key present in either side becomes a row.
	keys := map[string]struct{}{}
	for k := range vol {
		keys[k] = struct{}{}
	}
	for k := range clk {
		keys[k] = struct{}{}
	}
	rowsOut := make([]perfSnapshotRow, 0, len(keys))
	for k := range keys {
		lbl := label[k]
		if lbl == "" {
			lbl = k
		}
		row := perfSnapshotRow{
			Key:             k,
			Label:           lbl,
			Unattributed:    k == unattribKey,
			ClicksExact:     true, // clicks are always exact slug-anchored
			VolumeEstimated: true, // offer-view volume is best-effort
		}
		if g := vol[k]; g != nil {
			row.Sent = g.sent
			row.Delivered = g.delivered
			row.Reached = g.delivered
			row.HumanOpens = g.humanOpens
			row.Hard = g.hard
			row.Soft = g.soft
		}
		if c := clk[k]; c != nil {
			row.Clicks = c.clicks
			row.Clickers = c.clickers
		}
		finalizeRow(&row)
		rowsOut = append(rowsOut, row)
	}

	// Order by clicks desc (then clickers, then key) — offer view is clicks-led.
	sort.Slice(rowsOut, func(i, j int) bool {
		if rowsOut[i].Clicks != rowsOut[j].Clicks {
			return rowsOut[i].Clicks > rowsOut[j].Clicks
		}
		if rowsOut[i].Clickers != rowsOut[j].Clickers {
			return rowsOut[i].Clickers > rowsOut[j].Clickers
		}
		return rowsOut[i].Key < rowsOut[j].Key
	})

	return perfSnapshotResponse{Totals: totalsOf(rowsOut, vol), Rows: rowsOut, Notes: notes}
}

// nameSuffixOffer returns the trimmed campaign-name suffix after the last " - "
// (the operator naming convention, e.g. "DB Engager AM - Quicken Loans").
func nameSuffixOffer(name string) string {
	idx := strings.LastIndex(name, " - ")
	if idx <= 0 {
		return ""
	}
	return strings.TrimSpace(name[idx+3:])
}

// matchOfferByName resolves a campaign-name suffix to an offer key by a
// case-insensitive match against the slug-map offer labels. Returns ("","")
// when no offer label matches.
func matchOfferByName(efToOffer map[string]offerMeta, suffix string) (string, string) {
	low := strings.ToLower(suffix)
	for _, om := range efToOffer {
		if strings.ToLower(om.label) == low {
			return om.key, om.label
		}
	}
	return "", ""
}

// dominantOffer picks the offer a campaign's clicks predominantly landed on:
// the offer key with the MOST clicks, ties broken by the smallest first-seen
// order (so ties resolve to the first offer encountered). cnt maps offer key ->
// click count; seenOrder maps offer key -> the order it was first seen. Returns
// "" when cnt is empty. This is the volume-follow tie-break used so a broadcast
// campaign's VOLUME tracks the SAME offer as its (exact, slug-anchored) clicks.
func dominantOffer(cnt map[string]int, seenOrder map[string]int) string {
	bestKey := ""
	bestCnt := -1
	bestSeen := int(^uint(0) >> 1) // max int
	for k, n := range cnt {
		ord := seenOrder[k]
		if n > bestCnt || (n == bestCnt && ord < bestSeen) {
			bestKey, bestCnt, bestSeen = k, n, ord
		}
	}
	return bestKey
}

// totalsOf sums the row-visible counters for the response totals strip. clicks/
// clickers come from the rows (already merged); the rest from the volume map so
// totals match the rollup even when a row's volume side is empty.
func totalsOf(rows []perfSnapshotRow, vol map[string]*volAgg) perfSnapshotTotals {
	var t perfSnapshotTotals
	for _, g := range vol {
		t.Sent += g.sent
		t.Delivered += g.delivered
		t.Reached += g.delivered
		t.HumanOpens += g.humanOpens
		t.Hard += g.hard
		t.Soft += g.soft
		t.Unsub += g.unsub
	}
	for i := range rows {
		t.Clicks += rows[i].Clicks
		t.Clickers += rows[i].Clickers
	}
	return t
}

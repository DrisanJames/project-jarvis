package api

// Creatives reporting — READ ONLY analytics over which creative × subject ×
// sending-domain combos drive money-link clicks & conversions for a chosen
// offer.
//
// Why slug-and-md5, not the identity FK columns: the identity FK columns on
// the tracking/campaign tables are inert (0% populated) in production. The real
// keys are (a) the Everflow money-slug embedded in
// mailing_tracking_events.link_url and (b) the campaign_id. Creative identity
// is md5(mailing_campaigns.html_content) — same bytes ⇒ same creative.
//
// MONEY-LINK DOMAINS ARE HOST-AGNOSTIC. Everflow rotates the money-link host
// across a family of domains (eos57ytf.com — the highest-volume, carries Sam's
// Club — plus cratoolpro.com, k8k0hfdt.com, xnonu.com, muqes.com, …). A
// cratoolpro-only filter silently dropped the MAJORITY of money clicks
// (including the top-converting offer), so the money-link marker is purely
// `source_id=email`, and the offer slug is the 2nd path segment of ANY host:
// substring(link_url FROM 'https?://[^/]+/[A-Za-z0-9]+/([A-Za-z0-9]+)/').
//
// Both handlers are READ ONLY (SELECT only), every table is org-scoped on
// organization_id = GetOrgIDFromRequest(r), and every value the caller controls
// (org id, date bounds, the resolved slug-pattern set, the offer's everflow
// ids) is bound as a $N parameter — there is NO string concatenation of user
// input. The slug set and everflow-id set ride through pq.Array.
//
// Date params: both handlers read start_date / end_date (YYYY-MM-DD) via the
// shared parseAnalyticsRange helper (mailing_analytics.go), which anchors the
// window to America/Denver and also understands RFC3339, range_type and days.
//
// Routes (wired in server_routes_mailing.go under /api/mailing):
//
//	GET /analytics/creatives/offers — offers that actually ran in range, by money_clicks
//	GET /analytics/creatives        — per (creative|subject) × sending-domain breakdown for one offer

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// creativesAnalyticsRowLimit caps the per-offer breakdown so a pathological
// fan-out can't return an unbounded result. Hitting it sets truncated=true.
const creativesAnalyticsRowLimit = 1000

// moneySlugExpr extracts the Everflow offer slug (2nd path segment) from a
// money link on ANY host. NULL when the link doesn't match the /SEG/SLUG/ shape
// (e.g. a direct advertiser link or a test URL) — those rows are dropped.
const moneySlugExpr = `substring(%s.link_url FROM 'https?://[^/]+/[A-Za-z0-9]+/([A-Za-z0-9]+)/')`

// HandleAnalyticsCreativeOffers lists the offers that actually ran in the
// window — derived from the real clicked Everflow money-slugs in
// mailing_tracking_events (any money host), LEFT JOINed to
// mailing_offer_slug_map for names. Unmapped slugs are kept with
// offer_name=null. Sorted by money_clicks desc.
//
// GET /api/mailing/analytics/creatives/offers?start_date=YYYY-MM-DD&end_date=YYYY-MM-DD
func (s *Server) HandleAnalyticsCreativeOffers(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	start, end := parseAnalyticsRange(r)

	// Slug = 2nd path segment of the money URL on any host. The money-link
	// marker is source_id=email; drop NULL slugs (links that don't match the
	// canonical /SEG/SLUG/ shape).
	const q = `
		WITH clicked AS (
			SELECT substring(t.link_url FROM 'https?://[^/]+/[A-Za-z0-9]+/([A-Za-z0-9]+)/') AS slug
			FROM mailing_tracking_events t
			WHERE t.organization_id = $1
			  AND t.event_at >= $2 AND t.event_at <= $3
			  AND t.event_type = 'clicked'
			  AND t.link_url ILIKE '%source_id=email%'
		)
		SELECT c.slug,
		       m.offer_name,
		       m.everflow_offer_id,
		       COUNT(*) AS money_clicks
		FROM clicked c
		LEFT JOIN mailing_offer_slug_map m ON upper(m.cratoolpro_slug) = upper(c.slug)
		WHERE c.slug IS NOT NULL
		GROUP BY c.slug, m.offer_name, m.everflow_offer_id
		ORDER BY money_clicks DESC
	`

	rows, err := s.mailingDB.QueryContext(r.Context(), q, orgID, start, end)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type offerRow struct {
		Slug            string  `json:"slug"`
		OfferName       *string `json:"offer_name"`
		EverflowOfferID *int64  `json:"everflow_offer_id"`
		MoneyClicks     int     `json:"money_clicks"`
	}
	out := []offerRow{}
	for rows.Next() {
		var (
			slug    string
			name    *string // NULL when slug is unmapped (LEFT JOIN miss)
			efIDStr *string // VARCHAR(64) column; numeric-parsed below
			clicks  int
		)
		if err := rows.Scan(&slug, &name, &efIDStr, &clicks); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, offerRow{
			Slug:            slug,
			OfferName:       name,
			EverflowOfferID: parseEverflowOfferID(efIDStr),
			MoneyClicks:     clicks,
		})
	}
	if err := rows.Err(); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"offers": out})
}

// HandleAnalyticsCreatives returns the per-creative (or per-subject) ×
// sending-domain breakdown for a single offer: delivered, clicks, clickers,
// click_rate, conversions, conv_rate, revenue.
//
// GET /api/mailing/analytics/creatives?offer=<slug|everflow_id|name>&start_date=&end_date=&view=creative|subject
func (s *Server) HandleAnalyticsCreatives(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	offer := strings.TrimSpace(r.URL.Query().Get("offer"))
	if offer == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "offer is required"})
		return
	}
	view := strings.TrimSpace(r.URL.Query().Get("view"))
	if view != "subject" {
		view = "creative"
	}
	start, end := parseAnalyticsRange(r)

	// Resolve the offer specifier to a SET of slugs AND the set of Everflow
	// offer ids it covers. The slugs become money-link ILIKE patterns
	// '%/'+slug+'/%' (host-agnostic). The everflow ids scope the conversion
	// ledger so "conversions" and "unattributed" pertain to THIS offer only —
	// without it, a single-offer view would miscount every other offer's
	// conversions as unattributed.
	slugs, efids, offerName, err := s.resolveOfferSlugs(r, offer)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(slugs) == 0 {
		// Unknown offer / no mapping: respond with an empty, well-formed body
		// rather than an error so the frontend renders cleanly.
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"view":                     view,
			"offer":                    offer,
			"from":                     start.Format("2006-01-02"),
			"to":                       end.Format("2006-01-02"),
			"delivered_source":         "pg_campaign_counters",
			"truncated":                false,
			"unattributed_conversions": 0,
			"offer_scoped":             len(efids) > 0,
			"rows":                     []interface{}{},
		})
		return
	}
	patterns := make([]string, len(slugs))
	for i, sl := range slugs {
		patterns[i] = "%/" + sl + "/%"
	}

	conversions, revenue, unattributed, truncated, scanErr := s.creativesBreakdown(
		r, orgID, start, end, patterns, efids, view, offerName,
	)
	if scanErr != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": scanErr.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"view":                     view,
		"offer":                    offer,
		"from":                     start.Format("2006-01-02"),
		"to":                       end.Format("2006-01-02"),
		"delivered_source":         "pg_campaign_counters",
		"truncated":                truncated,
		"unattributed_conversions": unattributed,
		// offer_scoped=false ⇒ the offer's slugs aren't in slug_map, so the
		// conversion ledger can't be tagged to it; unattributed is reported as 0
		// and rows show click-attributed conversions only.
		"offer_scoped": len(efids) > 0,
		"rows":         conversions,
	})
	_ = revenue // revenue is carried per-row inside conversions; no top-line aggregate
}

// resolveOfferSlugs maps the caller's offer specifier (a raw slug, an
// everflow_offer_id, or an offer_name fragment) to (a) the set of money-slugs it
// covers and (b) the set of Everflow offer ids it covers (for conversion-ledger
// scoping). Reads mailing_offer_slug_map (a global catalog, not org-partitioned);
// the click/conv queries downstream are org-scoped.
func (s *Server) resolveOfferSlugs(r *http.Request, offer string) (slugs []string, efids []string, offerName string, err error) {
	const q = `
		SELECT cratoolpro_slug, COALESCE(offer_name, ''), COALESCE(everflow_offer_id, '')
		FROM mailing_offer_slug_map
		WHERE enabled = TRUE
		  AND (
		        everflow_offer_id = $1
		     OR upper(cratoolpro_slug) = upper($1)
		     OR ($1 <> '' AND offer_name ILIKE '%' || $1 || '%')
		  )
	`
	rows, qerr := s.mailingDB.QueryContext(r.Context(), q, offer)
	if qerr != nil {
		return nil, nil, "", qerr
	}
	defer rows.Close()

	efSeen := map[string]bool{}
	for rows.Next() {
		var slug, name, efid string
		if scanErr := rows.Scan(&slug, &name, &efid); scanErr != nil {
			return nil, nil, "", scanErr
		}
		slugs = append(slugs, slug)
		if offerName == "" && name != "" {
			offerName = name
		}
		if efid != "" && !efSeen[efid] {
			efSeen[efid] = true
			efids = append(efids, efid)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, "", rerr
	}

	// If the specifier matched nothing in the map but looks like a bare slug
	// (alphanumeric, no path separators), treat it as a self-resolving slug so
	// callers can report on a slug that simply isn't catalogued yet. No everflow
	// id is known in that case, so conversion-scoping is skipped.
	if len(slugs) == 0 && isBareSlug(offer) {
		slugs = []string{strings.ToUpper(offer)}
	}
	return slugs, efids, offerName, nil
}

// creativesBreakdown runs the validated multi-CTE query: money clicks grouped
// by creative_key/subject/sending_domain, an approximate delivered roll-up from
// the source campaigns' PG counters, and a conversion ledger (UNION of
// offer-suppression 'converted' rows and CPM manual conversions) re-attributed
// to the creative via the subscriber's most-recent in-window money-link click.
// When the offer's everflow ids are known (efids non-empty) the conversion
// ledger is scoped to THIS offer (suppressions by offer_id, CPM by deal); the
// rows then reconcile with unattributedConversions as rows.conversions +
// unattributed = offer-tagged conversions.
//
// Every table is scoped on organization_id = $1. The slug patterns ride in via
// pq.Array($4); the everflow ids (when present) via pq.Array($7).
func (s *Server) creativesBreakdown(
	r *http.Request, orgID interface{}, start, end interface{}, patterns, efids []string, view, offerName string,
) ([]map[string]interface{}, float64, int, bool, error) {
	// creativeKeyExpr is md5(html_content) for view=creative, or '' (collapsed)
	// for view=subject so the GROUP BY / JOIN keys reduce to (subject, domain).
	creativeKeyExpr := "md5(c.html_content)"
	if view == "subject" {
		creativeKeyExpr = "''::text"
	}

	// Offer-tagged conversion scoping (only when the offer's everflow ids are
	// known). $7 = everflow id array. Without it the ledger stays broad and the
	// LATERAL slug-match still restricts attribution to this offer's creatives.
	suppScope, cpmScope := "", ""
	args := []interface{}{orgID, start, end, pq.Array(patterns), offerName, creativesAnalyticsRowLimit + 1}
	if len(efids) > 0 {
		suppScope = " AND s.offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($7))"
		cpmScope = " AND cm.deal_id IN (SELECT id FROM mailing_cpm_deals WHERE organization_id = $1 AND (everflow_offer_id = ANY($7) OR offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($7))))"
		args = append(args, pq.Array(efids))
	}

	// $1 org, $2 start, $3 end, $4 slug patterns, $5 offer label, $6 row limit,
	// $7 everflow ids (only when offer-scoped).
	q := `
		WITH money_clicks AS (
			SELECT t.campaign_id,
			       t.subscriber_id,
			       t.sending_domain,
			       t.event_at
			FROM mailing_tracking_events t
			WHERE t.organization_id = $1
			  AND t.event_at >= $2 AND t.event_at <= $3
			  AND t.event_type = 'clicked'
			  AND t.link_url ILIKE '%source_id=email%'
			  AND t.link_url ILIKE ANY($4)
		),
		clicks_grp AS (
			SELECT ` + creativeKeyExpr + ` AS creative_key,
			       c.subject,
			       mc.sending_domain,
			       COUNT(*)                          AS clicks,
			       COUNT(DISTINCT mc.subscriber_id)  AS clickers
			FROM money_clicks mc
			JOIN mailing_campaigns c
			  ON c.id = mc.campaign_id AND c.organization_id = $1
			GROUP BY 1, c.subject, mc.sending_domain
		),
		camp_dom AS (
			SELECT DISTINCT campaign_id, sending_domain FROM money_clicks
		),
		delivered_grp AS (
			SELECT ` + creativeKeyExpr + ` AS creative_key,
			       c.subject,
			       cd.sending_domain,
			       SUM(COALESCE(c.delivered_count, 0)) AS delivered
			FROM camp_dom cd
			JOIN mailing_campaigns c
			  ON c.id = cd.campaign_id AND c.organization_id = $1
			GROUP BY 1, c.subject, cd.sending_domain
		),
		conv AS (
			-- Offer-suppression conversions (postback truth); revenue 0.
			SELECT s.subscriber_id, s.suppressed_at AS converted_at, 0::numeric AS revenue
			FROM mailing_offer_suppressions s
			WHERE s.organization_id = $1
			  AND s.reason = 'converted'
			  AND s.suppressed_at >= $2 AND s.suppressed_at <= $3` + suppScope + `
			UNION ALL
			-- CPM manual conversions; sub1 is the subscriber UUID.
			SELECT cm.sub1::uuid AS subscriber_id, cm.converted_at, COALESCE(cm.revenue, 0)::numeric AS revenue
			FROM mailing_cpm_manual_conversions cm
			WHERE cm.organization_id = $1
			  AND cm.converted_at >= $2 AND cm.converted_at <= $3
			  AND cm.sub1 ~ '^[0-9a-f-]{36}$'` + cpmScope + `
		),
		conv_attr AS (
			SELECT cv.revenue,
			       att.creative_key,
			       att.subject,
			       att.sending_domain
			FROM conv cv
			LEFT JOIN LATERAL (
				SELECT ` + creativeKeyExpr + ` AS creative_key,
				       c.subject,
				       mc2.sending_domain
				FROM mailing_tracking_events mc2
				JOIN mailing_campaigns c
				  ON c.id = mc2.campaign_id AND c.organization_id = $1
				WHERE mc2.organization_id = $1
				  AND mc2.subscriber_id = cv.subscriber_id
				  AND mc2.event_type = 'clicked'
				  AND mc2.link_url ILIKE '%source_id=email%'
				  AND mc2.link_url ILIKE ANY($4)
				  AND mc2.event_at <= cv.converted_at
				  AND mc2.event_at >= cv.converted_at - INTERVAL '90 days'
				ORDER BY mc2.event_at DESC
				LIMIT 1
			) att ON TRUE
		),
		conv_grp AS (
			SELECT creative_key, subject, sending_domain,
			       COUNT(*)        AS conversions,
			       SUM(revenue)    AS revenue
			FROM conv_attr
			WHERE creative_key IS NOT NULL   -- attributed (LATERAL found a click)
			GROUP BY creative_key, subject, sending_domain
		)
		SELECT COALESCE(cg.creative_key, dg.creative_key, cv.creative_key)        AS creative_key,
		       COALESCE(cg.subject, dg.subject, cv.subject)                       AS subject,
		       COALESCE(cg.sending_domain, dg.sending_domain, cv.sending_domain)  AS sending_domain,
		       COALESCE(dg.delivered, 0)                                          AS delivered,
		       COALESCE(cg.clicks, 0)                                             AS clicks,
		       COALESCE(cg.clickers, 0)                                           AS clickers,
		       COALESCE(cv.conversions, 0)                                        AS conversions,
		       COALESCE(cv.revenue, 0)                                            AS revenue
		FROM clicks_grp cg
		FULL OUTER JOIN delivered_grp dg
		  ON dg.creative_key = cg.creative_key AND dg.subject = cg.subject AND dg.sending_domain = cg.sending_domain
		FULL OUTER JOIN conv_grp cv
		  ON cv.creative_key = COALESCE(cg.creative_key, dg.creative_key)
		 AND cv.subject = COALESCE(cg.subject, dg.subject)
		 AND cv.sending_domain = COALESCE(cg.sending_domain, dg.sending_domain)
		ORDER BY clicks DESC, conversions DESC
		LIMIT $6
	`

	rows, err := s.mailingDB.QueryContext(r.Context(), q, args...)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer rows.Close()

	out := []map[string]interface{}{}
	var totalRevenue float64
	for rows.Next() {
		var (
			creativeKey, subject, sendingDomain string
			delivered, clicks, clickers, convs  int
			revenue                             float64
		)
		if err := rows.Scan(&creativeKey, &subject, &sendingDomain, &delivered, &clicks, &clickers, &convs, &revenue); err != nil {
			return nil, 0, 0, false, err
		}
		totalRevenue += revenue

		row := map[string]interface{}{
			"subject":        subject,
			"sending_domain": sendingDomain,
			"delivered":      delivered,
			"clicks":         clicks,
			"clickers":       clickers,
			"click_rate":     rate(clicks, delivered),
			"conversions":    convs,
			"conv_rate":      rate(convs, clicks),
			"revenue":        revenue,
		}
		if view == "creative" {
			row["creative_key"] = creativeKey
			row["creative_label"] = creativeLabel(offerName, subject, creativeKey)
		} else {
			row["creative_key"] = nil
			row["creative_label"] = nil
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, false, err
	}

	truncated := false
	if len(out) > creativesAnalyticsRowLimit {
		out = out[:creativesAnalyticsRowLimit]
		truncated = true
	}

	// unattributed_conversions: offer-tagged conversions whose LATERAL found no
	// matching in-window money-link click for this offer's slugs. Skipped (0)
	// when the offer isn't slug-mapped (efids empty) — see unattributedConversions.
	unattributed, err := s.unattributedConversions(r, orgID, start, end, patterns, efids)
	if err != nil {
		return nil, 0, 0, false, err
	}

	return out, totalRevenue, unattributed, truncated, nil
}

// unattributedConversions counts conversions in the window — scoped to THIS
// offer via its everflow ids — for which the most-recent-click lookup found no
// money-link click matching the offer's slugs within the 90-day attribution
// window. With offer scoping, rows.conversions + unattributed = offer-tagged
// conversions, so the badge is meaningful ("N of this offer's conversions
// couldn't be tied to a creative").
//
// When efids is empty the offer isn't slug-mapped, so conversions can't be
// honestly tagged to it; we return 0 rather than counting every other offer's
// conversions as unattributed.
func (s *Server) unattributedConversions(
	r *http.Request, orgID interface{}, start, end interface{}, patterns, efids []string,
) (int, error) {
	if len(efids) == 0 {
		return 0, nil
	}
	const q = `
		WITH conv AS (
			SELECT s.subscriber_id, s.suppressed_at AS converted_at
			FROM mailing_offer_suppressions s
			WHERE s.organization_id = $1
			  AND s.reason = 'converted'
			  AND s.suppressed_at >= $2 AND s.suppressed_at <= $3
			  AND s.offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($5))
			UNION ALL
			SELECT cm.sub1::uuid AS subscriber_id, cm.converted_at
			FROM mailing_cpm_manual_conversions cm
			WHERE cm.organization_id = $1
			  AND cm.converted_at >= $2 AND cm.converted_at <= $3
			  AND cm.sub1 ~ '^[0-9a-f-]{36}$'
			  AND cm.deal_id IN (SELECT id FROM mailing_cpm_deals WHERE organization_id = $1 AND (everflow_offer_id = ANY($5) OR offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($5))))
		)
		SELECT COUNT(*)
		FROM conv cv
		WHERE NOT EXISTS (
			SELECT 1
			FROM mailing_tracking_events mc2
			WHERE mc2.organization_id = $1
			  AND mc2.subscriber_id = cv.subscriber_id
			  AND mc2.event_type = 'clicked'
			  AND mc2.link_url ILIKE '%source_id=email%'
			  AND mc2.link_url ILIKE ANY($4)
			  AND mc2.event_at <= cv.converted_at
			  AND mc2.event_at >= cv.converted_at - INTERVAL '90 days'
		)
	`
	var n int
	if err := s.mailingDB.QueryRowContext(r.Context(), q, orgID, start, end, pq.Array(patterns), pq.Array(efids)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ─── small helpers ──────────────────────────────────────────────────────────

// rate returns num/den as a float, or 0 when den is zero (NULLIF semantics).
func rate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// creativeLabel = offer_name · subject · left(creative_key,8).
func creativeLabel(offerName, subject, creativeKey string) string {
	key := creativeKey
	if len(key) > 8 {
		key = key[:8]
	}
	name := offerName
	if name == "" {
		name = "(unmapped offer)"
	}
	return name + " · " + subject + " · " + key
}

// parseEverflowOfferID converts the VARCHAR everflow_offer_id column to an int
// for the JSON contract ({"everflow_offer_id":123}). Non-numeric / empty / NULL
// values become null in the response.
func parseEverflowOfferID(s *string) *int64 {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// isBareSlug reports whether s looks like an Everflow money slug (alphanumeric,
// no path separators) so an uncatalogued slug still self-resolves.
func isBareSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

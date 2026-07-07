package api

// campaign_attribution_backfill.go — Offer Alignment PART B: retroactive
// attribution for campaigns deployed BEFORE the deploy-time stamp (PART A,
// campaign_attribution.go) existed. Operator-triggered via the admin router
// (X-Admin-Key), DRY-RUN BY DEFAULT — only an explicit dry_run=0 writes.
//
// Three phases, each touching only attribution_source IS NULL rows (idempotent;
// re-runs are no-ops for already-stamped campaigns):
//
//  1. html_inferred — extract the money-link slug set from the stored
//     html_content (moneyLinkSlugPattern, the same regex the Go stamp uses,
//     passed as a bind arg so the two can never diverge). Exactly one
//     distinct slug → stamp; multi-offer creatives are counted but skipped.
//  2. name_inferred (click-drip shadows) — 'Click-Drip Reminder · offer <N>'
//     campaigns carry their everflow id in the name (campaign_type=
//     'click_drip'); resolve it directly. These have no broadcast HTML.
//  3. click_inferred — campaigns still unstamped (no stored HTML / ambiguous)
//     whose CLICKED money links show one dominant slug (>=80% of money
//     clicks, >=3 clicks): the same read-time attribution the creatives
//     analytics screen trusts, frozen onto the row.
//
// One-move undo:
//
//	UPDATE mailing_campaigns SET offer_id=NULL, offer_key=NULL,
//	  attribution_source=NULL WHERE attribution_source IN
//	  ('html_inferred','click_inferred');
//
// (phase 2 rows share 'name_inferred' with deploy-time stamps; undo those by
// campaign_type='click_drip' if ever needed.)

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"
)

// attributionBackfillResult is the response body — counts per phase so the
// operator can eyeball a dry run before writing. Dry-run phase counts are
// computed independently (a campaign phase 1 would claim can also appear in
// the phase-3 count); on a real run phases execute in order and later phases
// skip rows earlier ones stamped.
type attributionBackfillResult struct {
	DryRun        bool   `json:"dry_run"`
	WindowDays    int    `json:"window_days"`
	WindowStart   string `json:"window_start"`
	HTMLStamped   int64  `json:"html_stamped"`
	HTMLAmbiguous int64  `json:"html_ambiguous_multi_offer"`
	ClickDrip     int64  `json:"clickdrip_stamped"`
	ClickStamped  int64  `json:"click_stamped"`
	RemainingNull int64  `json:"remaining_unattributed"`
	TookMs        int64  `json:"took_ms"`
}

// attributionBackfillHTMLCTE is the shared phase-1 candidate shape: per
// still-unstamped campaign, the distinct money-link slug set from stored HTML.
// Binds: $1 org, $2 window start, $3 moneyLinkSlugPattern.
const attributionBackfillHTMLCTE = `
	WITH cand AS (
		SELECT c.id, lower(m.slug[1]) AS slug
		FROM mailing_campaigns c
		CROSS JOIN LATERAL regexp_matches(c.html_content, $3, 'g') AS m(slug)
		WHERE c.organization_id = $1
		  AND c.attribution_source IS NULL
		  AND c.created_at >= $2
		  AND c.html_content LIKE '%source_id=email%'
	), per AS (
		SELECT id, COUNT(DISTINCT slug) AS n, MIN(slug) AS slug
		FROM cand GROUP BY id
	)`

// HandleAttributionBackfill POST /api/admin/attribution-backfill?days=N&dry_run=0|1
func HandleAttributionBackfill(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		started := time.Now()

		days := 90
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n >= 1 && n <= 365 {
			days = n
		}
		// Dry run unless explicitly disabled — this endpoint mutates campaign rows.
		dry := true
		if v := r.URL.Query().Get("dry_run"); v == "0" || v == "false" {
			dry = false
		}
		windowStart := time.Now().AddDate(0, 0, -days).Format("2006-01-02")

		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()

		res := attributionBackfillResult{DryRun: dry, WindowDays: days, WindowStart: windowStart}

		// ── Phase 1: html_inferred ─────────────────────────────────────────
		if dry {
			if err := db.QueryRowContext(ctx, attributionBackfillHTMLCTE+`
				SELECT COALESCE(COUNT(*) FILTER (WHERE n = 1), 0),
				       COALESCE(COUNT(*) FILTER (WHERE n > 1), 0)
				FROM per`, orgID, windowStart, moneyLinkSlugPattern).
				Scan(&res.HTMLStamped, &res.HTMLAmbiguous); err != nil {
				respondError(w, http.StatusInternalServerError, "backfill html scan: "+err.Error())
				return
			}
		} else {
			// Ambiguity count first (the UPDATE only reports stamped rows).
			if err := db.QueryRowContext(ctx, attributionBackfillHTMLCTE+`
				SELECT COALESCE(COUNT(*) FILTER (WHERE n > 1), 0) FROM per`,
				orgID, windowStart, moneyLinkSlugPattern).Scan(&res.HTMLAmbiguous); err != nil {
				respondError(w, http.StatusInternalServerError, "backfill html ambiguity scan: "+err.Error())
				return
			}
			out, err := db.ExecContext(ctx, attributionBackfillHTMLCTE+`
				UPDATE mailing_campaigns c
				SET offer_key = per.slug,
				    offer_id = COALESCE(ofr.id, c.offer_id),
				    attribution_source = 'html_inferred',
				    updated_at = NOW()
				FROM per
				LEFT JOIN LATERAL (
					SELECT o.id
					FROM mailing_offer_slug_map sm
					JOIN mailing_offers o ON o.organization_id = $1
					     AND o.everflow_offer_id = sm.everflow_offer_id
					WHERE upper(sm.cratoolpro_slug) = upper(per.slug)
					   OR upper(sm.offer_name) = upper(per.slug)
					ORDER BY o.id LIMIT 1
				) ofr ON TRUE
				WHERE c.id = per.id AND per.n = 1 AND c.attribution_source IS NULL`,
				orgID, windowStart, moneyLinkSlugPattern)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "backfill html stamp: "+err.Error())
				return
			}
			res.HTMLStamped, _ = out.RowsAffected()
		}

		// ── Phase 2: click-drip shadow campaigns (everflow id in the name) ─
		// No created_at floor: the shadow campaigns are created once per offer
		// and can predate the window while their touches are current. The two
		// lookups are INDEPENDENT scalar subqueries (not a required join): a
		// slug-map hit stamps offer_key even when mailing_offers has no row
		// for the everflow id, so the CPM deal slug-set branch still matches.
		const clickDripPred = `
			c.organization_id = $1
			AND c.attribution_source IS NULL
			AND c.campaign_type = 'click_drip'
			AND c.name ~ 'offer [0-9]+$'
			AND (
				EXISTS (SELECT 1 FROM mailing_offer_slug_map sm
				        WHERE sm.everflow_offer_id = substring(c.name FROM 'offer ([0-9]+)$'))
				OR EXISTS (SELECT 1 FROM mailing_offers o
				           WHERE o.organization_id = $1
				             AND o.everflow_offer_id = substring(c.name FROM 'offer ([0-9]+)$'))
			)`
		if dry {
			if err := db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM mailing_campaigns c WHERE `+clickDripPred, orgID).
				Scan(&res.ClickDrip); err != nil {
				respondError(w, http.StatusInternalServerError, "backfill click-drip scan: "+err.Error())
				return
			}
		} else {
			out, err := db.ExecContext(ctx, `
				UPDATE mailing_campaigns c
				SET offer_id = COALESCE((
				        SELECT o.id FROM mailing_offers o
				        WHERE o.organization_id = $1
				          AND o.everflow_offer_id = substring(c.name FROM 'offer ([0-9]+)$')
				        ORDER BY o.id LIMIT 1), c.offer_id),
				    offer_key = COALESCE((
				        SELECT lower(sm.cratoolpro_slug) FROM mailing_offer_slug_map sm
				        WHERE sm.everflow_offer_id = substring(c.name FROM 'offer ([0-9]+)$')
				        ORDER BY sm.cratoolpro_slug LIMIT 1), c.offer_key),
				    attribution_source = 'name_inferred',
				    updated_at = NOW()
				WHERE `+clickDripPred, orgID)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "backfill click-drip stamp: "+err.Error())
				return
			}
			res.ClickDrip, _ = out.RowsAffected()
		}

		// ── Phase 3: click_inferred (dominant clicked money slug) ──────────
		// The bare te.event_at bound is the partition-pruning predicate
		// (METRIC_CONTRACT §4); clicked rows are a small slice of the window.
		const clickCTE = `
			WITH clicks AS (
				SELECT te.campaign_id, lower(substring(te.link_url FROM $3)) AS slug, COUNT(*) AS n
				FROM mailing_tracking_events te
				JOIN mailing_campaigns c ON c.id = te.campaign_id
				     AND c.organization_id = $1 AND c.attribution_source IS NULL
				     AND c.created_at >= $2
				WHERE te.organization_id = $1
				  AND te.event_type = 'clicked'
				  AND te.event_at >= $2
				  AND te.link_url LIKE '%source_id=email%'
				GROUP BY 1, 2
			), scored AS (
				SELECT campaign_id, slug, n, SUM(n) OVER (PARTITION BY campaign_id) AS total
				FROM clicks
				WHERE slug IS NOT NULL AND slug <> ''
			), top AS (
				SELECT DISTINCT ON (campaign_id) campaign_id, slug, n, total
				FROM scored
				ORDER BY campaign_id, n DESC, slug
			), pick AS (
				SELECT campaign_id, slug FROM top
				WHERE n >= 3 AND n::float8 / NULLIF(total, 0) >= 0.8
			)`
		if dry {
			if err := db.QueryRowContext(ctx, clickCTE+`
				SELECT COUNT(*) FROM pick`, orgID, windowStart, moneyLinkSlugPattern).
				Scan(&res.ClickStamped); err != nil {
				respondError(w, http.StatusInternalServerError, "backfill click scan: "+err.Error())
				return
			}
		} else {
			out, err := db.ExecContext(ctx, clickCTE+`
				UPDATE mailing_campaigns c
				SET offer_key = pick.slug,
				    offer_id = COALESCE(ofr.id, c.offer_id),
				    attribution_source = 'click_inferred',
				    updated_at = NOW()
				FROM pick
				LEFT JOIN LATERAL (
					SELECT o.id
					FROM mailing_offer_slug_map sm
					JOIN mailing_offers o ON o.organization_id = $1
					     AND o.everflow_offer_id = sm.everflow_offer_id
					WHERE upper(sm.cratoolpro_slug) = upper(pick.slug)
					   OR upper(sm.offer_name) = upper(pick.slug)
					ORDER BY o.id LIMIT 1
				) ofr ON TRUE
				WHERE c.id = pick.campaign_id AND c.attribution_source IS NULL`,
				orgID, windowStart, moneyLinkSlugPattern)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "backfill click stamp: "+err.Error())
				return
			}
			res.ClickStamped, _ = out.RowsAffected()
		}

		// ── What's left ────────────────────────────────────────────────────
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_campaigns
			WHERE organization_id = $1 AND attribution_source IS NULL AND created_at >= $2`,
			orgID, windowStart).Scan(&res.RemainingNull); err != nil {
			respondError(w, http.StatusInternalServerError, "backfill remaining count: "+err.Error())
			return
		}

		res.TookMs = time.Since(started).Milliseconds()
		respondJSON(w, http.StatusOK, res)
	}
}

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
//     BATCHED ~150 campaigns per statement: the regex over months of stored
//     HTML in one statement blew prod's statement_timeout (verified
//     2026-07-07 — 14d window timed out), so the id list is collected
//     cheaply first and the expensive regex runs per small chunk.
//  2. name_inferred (click-drip shadows) — 'Click-Drip Reminder · offer <N>'
//     campaigns carry their everflow id in the name (campaign_type=
//     'click_drip'); resolve it directly. These have no broadcast HTML.
//  3. click_inferred — campaigns still unstamped (no stored HTML / ambiguous)
//     whose CLICKED money links show one dominant slug (>=80% of money
//     clicks, >=3 clicks): the same read-time attribution the creatives
//     analytics screen trusts, frozen onto the row. Per-campaign indexed
//     lookups (campaign_id + event_at partition bound), capped per run —
//     the cap and progress are reported, never silent.
//
// Phase failures are per-phase and non-fatal: one slow phase reports its
// error in `errors[]` while the others still run.
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
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/lib/pq"
)

// attributionBackfillResult is the response body — counts per phase so the
// operator can eyeball a dry run before writing. Dry-run phase counts are
// computed independently (a campaign phase 1 would claim can also appear in
// the phase-3 count); on a real run phases execute in order and later phases
// skip rows earlier ones stamped.
type attributionBackfillResult struct {
	DryRun        bool     `json:"dry_run"`
	WindowDays    int      `json:"window_days"`
	WindowStart   string   `json:"window_start"`
	HTMLStamped   int64    `json:"html_stamped"`
	HTMLAmbiguous int64    `json:"html_ambiguous_multi_offer"`
	HTMLBatches   int      `json:"html_batches"`
	ClickDrip     int64    `json:"clickdrip_stamped"`
	ClickStamped  int64    `json:"click_stamped"`
	ClickScanned  int      `json:"click_scanned"`
	ClickCapped   bool     `json:"click_capped"` // true = more NULL campaigns than the per-run cap; re-run to continue
	RemainingNull int64    `json:"remaining_unattributed"`
	Errors        []string `json:"errors"`
	TookMs        int64    `json:"took_ms"`
}

const (
	attributionHTMLBatchSize = 150
	attributionClickScanCap  = 2500
)

// attributionBackfillHTMLBatchCTE is the shared phase-1 candidate shape for
// one id chunk: per still-unstamped campaign, the distinct money-link slug
// set from stored HTML. Binds: $1 = uuid[] id chunk, $2 = moneyLinkSlugPattern.
const attributionBackfillHTMLBatchCTE = `
	WITH cand AS (
		SELECT c.id, lower(m.slug[1]) AS slug
		FROM mailing_campaigns c
		CROSS JOIN LATERAL regexp_matches(c.html_content, $2, 'g') AS m(slug)
		WHERE c.id = ANY($1::uuid[])
		  AND c.attribution_source IS NULL
		  AND c.html_content LIKE '%source_id=email%'
	), per AS (
		SELECT id, COUNT(DISTINCT slug) AS n, MIN(slug) AS slug
		FROM cand GROUP BY id
	)`

// unattributedCampaignIDs returns ids of campaigns in the window still
// carrying no attribution, ordered stably. Cheap: never touches html_content.
func unattributedCampaignIDs(ctx context.Context, db *sql.DB, orgID, windowStart string, limit int) ([]string, error) {
	q := `
		SELECT id::text FROM mailing_campaigns
		WHERE organization_id = $1 AND attribution_source IS NULL AND created_at >= $2
		ORDER BY id`
	args := []interface{}{orgID, windowStart}
	if limit > 0 {
		args = append(args, limit)
		q += ` LIMIT $3`
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

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

		res := attributionBackfillResult{DryRun: dry, WindowDays: days, WindowStart: windowStart, Errors: []string{}}
		fail := func(phase string, err error) {
			res.Errors = append(res.Errors, phase+": "+err.Error())
		}

		// ── Phase 1: html_inferred (batched) ───────────────────────────────
		if ids, err := unattributedCampaignIDs(ctx, db, orgID, windowStart, 0); err != nil {
			fail("html id scan", err)
		} else {
			for start := 0; start < len(ids) && ctx.Err() == nil; start += attributionHTMLBatchSize {
				end := start + attributionHTMLBatchSize
				if end > len(ids) {
					end = len(ids)
				}
				chunk := pq.Array(ids[start:end])
				res.HTMLBatches++
				if dry {
					var one, multi int64
					if err := db.QueryRowContext(ctx, attributionBackfillHTMLBatchCTE+`
						SELECT COALESCE(COUNT(*) FILTER (WHERE n = 1), 0),
						       COALESCE(COUNT(*) FILTER (WHERE n > 1), 0)
						FROM per`, chunk, moneyLinkSlugPattern).Scan(&one, &multi); err != nil {
						fail(fmt.Sprintf("html batch %d scan", res.HTMLBatches), err)
						break
					}
					res.HTMLStamped += one
					res.HTMLAmbiguous += multi
				} else {
					var multi int64
					if err := db.QueryRowContext(ctx, attributionBackfillHTMLBatchCTE+`
						SELECT COALESCE(COUNT(*) FILTER (WHERE n > 1), 0) FROM per`,
						chunk, moneyLinkSlugPattern).Scan(&multi); err != nil {
						fail(fmt.Sprintf("html batch %d ambiguity scan", res.HTMLBatches), err)
						break
					}
					res.HTMLAmbiguous += multi
					out, err := db.ExecContext(ctx, attributionBackfillHTMLBatchCTE+`
						UPDATE mailing_campaigns c
						SET offer_key = per.slug,
						    offer_id = COALESCE(ofr.id, c.offer_id),
						    attribution_source = 'html_inferred',
						    updated_at = NOW()
						FROM per
						LEFT JOIN LATERAL (
							SELECT o.id
							FROM mailing_offer_slug_map sm
							JOIN mailing_offers o ON o.organization_id = $3
							     AND o.everflow_offer_id = sm.everflow_offer_id
							WHERE upper(sm.cratoolpro_slug) = upper(per.slug)
							   OR upper(sm.offer_name) = upper(per.slug)
							ORDER BY o.id LIMIT 1
						) ofr ON TRUE
						WHERE c.id = per.id AND per.n = 1 AND c.attribution_source IS NULL`,
						chunk, moneyLinkSlugPattern, orgID)
					if err != nil {
						fail(fmt.Sprintf("html batch %d stamp", res.HTMLBatches), err)
						break
					}
					n, _ := out.RowsAffected()
					res.HTMLStamped += n
				}
			}
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
				fail("click-drip scan", err)
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
				fail("click-drip stamp", err)
			} else {
				res.ClickDrip, _ = out.RowsAffected()
			}
		}

		// ── Phase 3: click_inferred (dominant clicked money slug) ──────────
		// Per-campaign indexed lookups: campaign_id predicate + the bare
		// event_at bound for partition pruning (METRIC_CONTRACT §4). Capped
		// per run — ClickCapped=true means re-run to continue (never silent).
		if ids, err := unattributedCampaignIDs(ctx, db, orgID, windowStart, attributionClickScanCap+1); err != nil {
			fail("click id scan", err)
		} else {
			if len(ids) > attributionClickScanCap {
				ids = ids[:attributionClickScanCap]
				res.ClickCapped = true
			}
			for _, id := range ids {
				if ctx.Err() != nil {
					fail("click scan", ctx.Err())
					break
				}
				res.ClickScanned++
				rows, err := db.QueryContext(ctx, `
					SELECT lower(substring(link_url FROM $3)) AS slug, COUNT(*) AS n
					FROM mailing_tracking_events
					WHERE campaign_id = $1 AND organization_id = $2
					  AND event_type = 'clicked' AND event_at >= $4
					  AND link_url LIKE '%source_id=email%'
					GROUP BY 1`, id, orgID, moneyLinkSlugPattern, windowStart)
				if err != nil {
					fail("click lookup "+id, err)
					break
				}
				type sc struct {
					slug string
					n    int64
				}
				var total int64
				slugCounts := []sc{}
				for rows.Next() {
					var s sql.NullString
					var n int64
					if err := rows.Scan(&s, &n); err == nil {
						total += n
						if s.Valid && s.String != "" {
							slugCounts = append(slugCounts, sc{s.String, n})
						}
					}
				}
				rows.Close()
				if len(slugCounts) == 0 {
					continue
				}
				sort.Slice(slugCounts, func(i, j int) bool {
					if slugCounts[i].n != slugCounts[j].n {
						return slugCounts[i].n > slugCounts[j].n
					}
					return slugCounts[i].slug < slugCounts[j].slug
				})
				top := slugCounts[0]
				if top.n < 3 || float64(top.n)/float64(total) < 0.8 {
					continue
				}
				if dry {
					res.ClickStamped++
					continue
				}
				offerID := resolveOfferIDViaSlugMap(ctx, db, orgID, id, top.slug)
				out, err := db.ExecContext(ctx, `
					UPDATE mailing_campaigns
					SET offer_key = $2,
					    offer_id = COALESCE($3::uuid, offer_id),
					    attribution_source = 'click_inferred',
					    updated_at = NOW()
					WHERE id = $1 AND attribution_source IS NULL`, id, top.slug, offerID)
				if err != nil {
					fail("click stamp "+id, err)
					break
				}
				if n, _ := out.RowsAffected(); n > 0 {
					res.ClickStamped++
				}
			}
		}

		// ── What's left ────────────────────────────────────────────────────
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_campaigns
			WHERE organization_id = $1 AND attribution_source IS NULL AND created_at >= $2`,
			orgID, windowStart).Scan(&res.RemainingNull); err != nil {
			fail("remaining count", err)
		}

		res.TookMs = time.Since(started).Milliseconds()
		respondJSON(w, http.StatusOK, res)
	}
}

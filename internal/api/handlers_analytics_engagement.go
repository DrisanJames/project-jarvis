package api

// Engagement summary for the Range Overview KPIs — HUMAN opens/clicks sourced
// from Postgres mailing_tracking_events (the ONLY store of internal-tracking
// opens/clicks) and classified by the canonical ignite_event_verdict() verdict
// function.
//
//	GET /api/mailing/analytics/engagement?from=YYYY-MM-DD&to=YYYY-MM-DD
//
// Why this exists (the bug it fixes):
//   - The Athena lake (the Range Overview's other tiles) only carries
//     open/click events from the SES webhook (source='ses') plus a polluted
//     'app' stream; the lake's pmta/ses open/click slice the Overview card read
//     under-reports clicks by ~3 orders of magnitude (3 vs reality).
//   - The is_machine_open / is_machine_click columns are INERT in this DB
//     (verified 2026-06-24: COUNT(... NOT is_machine_click) == COUNT(*)), so
//     filtering on them is a no-op. ignite_event_verdict() is the ONLY working
//     human classifier — it is what human_engagement and the verdict crons use.
//
// Semantics:
//   - Denver-day window: (event_at AT TIME ZONE 'America/Denver')::date BETWEEN
//     from AND to — matches the lake breakdown's local_dt buckets exactly.
//   - "raw" clicks EXCLUDE asset-CDN hosts (fonts/css/js the link-rewriter wraps
//     and scanners prefetch) so a "click" means a real destination click; raw
//     opens are all opened events (machine traffic included, as labelled).
//   - human = ignite_verdict_is_human(ignite_event_verdict(user_agent,
//     ip_address)); evaluated ONCE per row in the CTE.
//   - Org-scoped via GetOrgIDFromRequest. READ ONLY (no writes, no send path).

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// VersionEngagementSummary is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-06-24): initial — PG+verdict human opens/clicks for the Range
//	    Overview KPIs (replaces the lake's inert is_machine_click read).
const VersionEngagementSummary = "1.0"

const engagementSummaryTimeout = 15 * time.Second

// engagementAssetExclude matches resource-CDN hosts the link rewriter wraps as
// trackable redirects; scanners prefetch them, inflating "clicks" ~20x (verified
// 2026-06-24: 7,974 of 8,366 click rows were these hosts). Excluded from every
// click count so "clicks" means a real destination click. POSIX regex, used with
// the case-insensitive !~* operator. Constant — never interpolated from input.
const engagementAssetExclude = `(fonts\.googleapis\.com|fonts\.gstatic\.com|cdnjs\.cloudflare\.com)`

// HandleEngagementSummary returns verdict-human opens/clicks (plus raw and
// distinct openers/clickers) for [from,to] Denver days.
func (s *Server) HandleEngagementSummary(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" || to == "" {
		now := time.Now().UTC()
		if to == "" {
			to = now.Format("2006-01-02")
		}
		if from == "" {
			from = now.AddDate(0, 0, -6).Format("2006-01-02")
		}
	}

	// Optional brand (sending-domain) filter — honors the Range Overview toolbar.
	// Contains-match is format-tolerant: the toolbar may pass a brand root
	// ("quizfiesta.com") while the stored sending_domain is prefixed
	// ("em.quizfiesta.com"). Empty -> all brands. Transport and ISP filters are
	// intentionally NOT applied: engagement is not a transport property, and the
	// ISP table is delivery-only.
	brand := strings.TrimSpace(r.URL.Query().Get("brand"))

	// One pass: classify each event once in the CTE (verdict fn is the only
	// working human filter — the is_machine_* columns are inert), then aggregate.
	const q = `
		WITH ev AS (
			SELECT
				event_type,
				subscriber_id,
				(event_type = 'clicked'
					AND link_url IS NOT NULL
					AND link_url !~* '` + engagementAssetExclude + `') AS real_click,
				ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address)) AS is_human
			FROM mailing_tracking_events
			WHERE organization_id = $1
			  AND (event_at AT TIME ZONE 'America/Denver')::date BETWEEN $2::date AND $3::date
			  AND event_type IN ('opened', 'clicked')
			  AND ($4 = '' OR sending_domain ILIKE '%' || $4 || '%')
		)
		SELECT
			COUNT(*) FILTER (WHERE event_type = 'opened')                                   AS raw_opens,
			COUNT(*) FILTER (WHERE event_type = 'opened' AND is_human)                       AS human_opens,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened' AND is_human)  AS human_openers,
			COUNT(*) FILTER (WHERE real_click)                                               AS raw_clicks,
			COUNT(*) FILTER (WHERE real_click AND is_human)                                  AS human_clicks,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE real_click AND is_human)             AS human_clickers
		FROM ev`

	ctx, cancel := context.WithTimeout(r.Context(), engagementSummaryTimeout)
	defer cancel()

	var rawOpens, humanOpens, humanOpeners, rawClicks, humanClicks, humanClickers int64
	// Scan order MUST match the final SELECT column order: raw_opens, human_opens,
	// human_openers, raw_clicks, human_clicks, human_clickers.
	if err := s.mailingDB.QueryRowContext(ctx, q, orgID, from, to, brand).Scan(
		&rawOpens, &humanOpens, &humanOpeners, &rawClicks, &humanClicks, &humanClickers,
	); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"version":        VersionEngagementSummary,
		"from":           from,
		"to":             to,
		"raw_opens":      rawOpens,
		"human_opens":    humanOpens,
		"human_openers":  humanOpeners,
		"raw_clicks":     rawClicks,
		"human_clicks":   humanClicks,
		"human_clickers": humanClickers,
	})
}

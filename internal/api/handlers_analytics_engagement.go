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
//   - "raw" opens/clicks are ALL recorded events (machine traffic included, as
//     labelled) — no asset-host exclusion. The verdict IS the click filter; per
//     operator there is no additional layer.
//   - human = ignite_verdict_is_human(ignite_event_verdict(user_agent,
//     ip_address)); evaluated ONCE per row in the CTE.
//   - Org-scoped via GetOrgIDFromRequest. READ ONLY (no writes, no send path).
//
// Performance — partition pruning (the bug v1.2 fixes):
//   mailing_tracking_events is RANGE-partitioned by month on event_at. Filtering
//   ONLY on (event_at AT TIME ZONE 'America/Denver')::date wraps the partition key
//   in a function, so the planner cannot prune — it scans ALL monthly partitions
//   (tens of millions of rows) before the verdict aggregate, blowing the 15s
//   budget once the rolling window grew past ~1M opened/clicked rows (the Range
//   Overview then showed "engagement unavailable"). Fix mirrors the lake reader
//   (buildBreakdownSQL): add a raw event_at timestamptz bound with a ±1-day margin
//   so the planner prunes to the touched month(s) + uses the event_at index, while
//   the precise Denver-date predicate still does the exact day selection. The
//   margin is non-excluding — a Denver day is always inside [from-1, to+2) UTC —
//   so counts are byte-identical (verified against the unbounded query).

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// engagementBrandRe admits domain-shaped brand filters only (letters, digits,
// dots, hyphens). Deliberately excludes '_' and '%': both are LIKE wildcards
// and neither is legal in a hostname.
var engagementBrandRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]{1,255}$`)

// VersionEngagementSummary is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-06-24): initial — PG+verdict human opens/clicks for the Range
//	    Overview KPIs (replaces the lake's inert is_machine_click read).
//	1.1 (2026-06-24): raw = ALL recorded events (machine incl.). Dropped the
//	    asset-host exclusion layer — the verdict IS the click filter; per
//	    operator, no additional layer.
//	1.2 (2026-06-25): partition-prune fix — bound event_at to a ±1-day UTC margin
//	    so the monthly partitions prune instead of a full-table scan. Fixes the
//	    15s timeout / "engagement unavailable" once the rolling set passed ~1M
//	    opened/clicked rows. Counts unchanged (margin is non-excluding).
//	1.3 (2026-07-01): brand filter anchored (exact or dot-suffix, was substring
//	    ILIKE) + validated domain-shaped — scope now matches the lake tiles'
//	    exact brand match beside it.
const VersionEngagementSummary = "1.3"

const engagementSummaryTimeout = 15 * time.Second

// engagementSummaryQuery — see the package doc above. The event_at >= / < bound
// is what enables partition pruning; do NOT remove it (a function-wrapped
// partition key defeats pruning and reintroduces the full-table-scan timeout).
const engagementSummaryQuery = `
		WITH ev AS (
			SELECT
				event_type,
				subscriber_id,
				ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address)) AS is_human
			FROM mailing_tracking_events
			WHERE organization_id = $1
			  AND event_at >= ($2::date - 1)::timestamptz
			  AND event_at <  ($3::date + 2)::timestamptz
			  AND (event_at AT TIME ZONE 'America/Denver')::date BETWEEN $2::date AND $3::date
			  AND event_type IN ('opened', 'clicked')
			  AND ($4 = '' OR lower(sending_domain) = lower($4)
			       OR lower(sending_domain) LIKE '%.' || lower($4))
		)
		SELECT
			COUNT(*) FILTER (WHERE event_type = 'opened')                                    AS raw_opens,
			COUNT(*) FILTER (WHERE event_type = 'opened' AND is_human)                        AS human_opens,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened' AND is_human)   AS human_openers,
			COUNT(*) FILTER (WHERE event_type = 'clicked')                                    AS raw_clicks,
			COUNT(*) FILTER (WHERE event_type = 'clicked' AND is_human)                       AS human_clicks,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'clicked' AND is_human)  AS human_clickers
		FROM ev`

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
	// Suffix-anchored match, format-tolerant: the toolbar may pass a brand root
	// ("quizfiesta.com") while the stored sending_domain is prefixed
	// ("em.quizfiesta.com") — exact OR dot-suffix. The old substring ILIKE
	// matched infixes (a brand apex inside ANOTHER brand's sending domain) and
	// treated %/_ as wildcards, so its scope could differ from the lake tiles'
	// exact brandExpr match beside it. Reject non-domain input outright (mirrors
	// the lake's dottedRe, minus '_' which is a LIKE wildcard and never legal in
	// a hostname). Empty -> all brands. Transport is intentionally NOT applied
	// (engagement is not a transport property).
	brand := strings.TrimSpace(r.URL.Query().Get("brand"))
	if brand != "" && !engagementBrandRe.MatchString(brand) {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid value for brand"})
		return
	}

	// One pass: classify each event once in the CTE (verdict fn is the only
	// working human filter — the is_machine_* columns are inert), then aggregate.
	// Query is a package const (engagementSummaryQuery) so the partition-prune
	// bound is covered by a regression test.
	ctx, cancel := context.WithTimeout(r.Context(), engagementSummaryTimeout)
	defer cancel()

	var rawOpens, humanOpens, humanOpeners, rawClicks, humanClicks, humanClickers int64
	// Scan order MUST match the final SELECT column order: raw_opens, human_opens,
	// human_openers, raw_clicks, human_clicks, human_clickers.
	if err := s.mailingDB.QueryRowContext(ctx, engagementSummaryQuery, orgID, from, to, brand).Scan(
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

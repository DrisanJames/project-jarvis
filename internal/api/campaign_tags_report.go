package api

// campaign_tags_report.go — audience unification Phase 1, W4b.
//
// Read side of mailing_campaign_tags: per-tag family rollups over the
// denormalized mailing_campaigns counters (the same columns every campaign
// screen reads — no new metric source, METRIC_CONTRACT-neutral).
//
//	GET /api/mailing/campaign-tags/report?tag=&from=&to=
//	    tag empty → one aggregate row per tag in the created_at window
//	    tag given → the same aggregate per Denver day for that tag
//	    from/to   → YYYY-MM-DD (inclusive); default last 30 days
//	GET /api/mailing/campaign-tags/{campaignID}
//	    the tags on one campaign (org-checked via the campaigns join)

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// campaignTagAggregate is one rollup row — keyed by tag (report mode) or by
// Denver day (single-tag mode).
type campaignTagAggregate struct {
	Tag             string `json:"tag,omitempty"`
	Day             string `json:"day,omitempty"`
	Campaigns       int64  `json:"campaigns"`
	TotalRecipients int64  `json:"total_recipients"`
	SentCount       int64  `json:"sent_count"`
	DeliveredCount  int64  `json:"delivered_count"`
	UniqueOpenCount int64  `json:"unique_open_count"`
	UniqueClickCnt  int64  `json:"unique_click_count"`
	HardBounceCount int64  `json:"hard_bounce_count"`
	SoftBounceCount int64  `json:"soft_bounce_count"`
}

// campaignTagSumCols is the shared aggregate column list — one definition so
// the per-tag and per-day modes can never disagree on how a number is summed.
const campaignTagSumCols = `
	COUNT(*),
	COALESCE(SUM(c.total_recipients), 0),
	COALESCE(SUM(c.sent_count), 0),
	COALESCE(SUM(c.delivered_count), 0),
	COALESCE(SUM(c.unique_open_count), 0),
	COALESCE(SUM(c.unique_click_count), 0),
	COALESCE(SUM(c.hard_bounce_count), 0),
	COALESCE(SUM(c.soft_bounce_count), 0)`

// HandleCampaignTagsReport GET /api/mailing/campaign-tags/report?tag=&from=&to=
func HandleCampaignTagsReport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		q := r.URL.Query()
		tag := strings.TrimSpace(q.Get("tag"))

		// Window: [from 00:00, to+1d 00:00) UTC, default trailing 30 days.
		now := time.Now().UTC()
		from := now.AddDate(0, 0, -30)
		to := now.AddDate(0, 0, 1).Truncate(24 * time.Hour)
		if v := strings.TrimSpace(q.Get("from")); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				respondError(w, http.StatusBadRequest, "invalid from date (YYYY-MM-DD)")
				return
			}
			from = t
		}
		if v := strings.TrimSpace(q.Get("to")); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				respondError(w, http.StatusBadRequest, "invalid to date (YYYY-MM-DD)")
				return
			}
			to = t.AddDate(0, 0, 1) // inclusive upper bound
		}

		var (
			rows *sql.Rows
			err  error
		)
		if tag == "" {
			rows, err = db.QueryContext(r.Context(), `
				SELECT t.tag, `+campaignTagSumCols+`
				FROM mailing_campaign_tags t
				JOIN mailing_campaigns c ON c.id = t.campaign_id
				WHERE c.organization_id = $1
				  AND c.created_at >= $2 AND c.created_at < $3
				GROUP BY t.tag
				ORDER BY 2 DESC, t.tag ASC`, orgID, from, to)
		} else {
			// Per Denver day — the send-day/plan calendar (METRIC_CONTRACT).
			rows, err = db.QueryContext(r.Context(), `
				SELECT (c.created_at AT TIME ZONE 'America/Denver')::date::text AS day, `+campaignTagSumCols+`
				FROM mailing_campaign_tags t
				JOIN mailing_campaigns c ON c.id = t.campaign_id
				WHERE c.organization_id = $1
				  AND c.created_at >= $2 AND c.created_at < $3
				  AND t.tag = $4
				GROUP BY 1
				ORDER BY 1 ASC`, orgID, from, to, tag)
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "tag report query failed: "+err.Error())
			return
		}
		defer rows.Close()

		out := []campaignTagAggregate{}
		for rows.Next() {
			var a campaignTagAggregate
			var key string
			if err := rows.Scan(&key, &a.Campaigns, &a.TotalRecipients, &a.SentCount,
				&a.DeliveredCount, &a.UniqueOpenCount, &a.UniqueClickCnt,
				&a.HardBounceCount, &a.SoftBounceCount); err != nil {
				respondError(w, http.StatusInternalServerError, "tag report scan failed: "+err.Error())
				return
			}
			if tag == "" {
				a.Tag = key
			} else {
				a.Day = key
			}
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "tag report rows failed: "+err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"tag":  tag,
			"from": from.Format("2006-01-02"),
			"to":   to.AddDate(0, 0, -1).Format("2006-01-02"),
			"rows": out,
		})
	}
}

// campaignTagRow is one tag on one campaign.
type campaignTagRow struct {
	Tag       string    `json:"tag"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// HandleCampaignTagsForCampaign GET /api/mailing/campaign-tags/{campaignID}
func HandleCampaignTagsForCampaign(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		campaignID := chi.URLParam(r, "campaignID")
		if _, err := uuid.Parse(campaignID); err != nil {
			respondError(w, http.StatusBadRequest, "invalid campaign id")
			return
		}

		rows, err := db.QueryContext(r.Context(), `
			SELECT t.tag, t.source, t.created_at
			FROM mailing_campaign_tags t
			JOIN mailing_campaigns c ON c.id = t.campaign_id
			WHERE t.campaign_id = $1 AND c.organization_id = $2
			ORDER BY t.tag ASC`, campaignID, orgID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "campaign tags query failed: "+err.Error())
			return
		}
		defer rows.Close()

		tags := []campaignTagRow{}
		for rows.Next() {
			var t campaignTagRow
			if err := rows.Scan(&t.Tag, &t.Source, &t.CreatedAt); err != nil {
				respondError(w, http.StatusInternalServerError, "campaign tags scan failed: "+err.Error())
				return
			}
			tags = append(tags, t)
		}
		if err := rows.Err(); err != nil {
			respondError(w, http.StatusInternalServerError, "campaign tags rows failed: "+err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"campaign_id": campaignID,
			"tags":        tags,
		})
	}
}
